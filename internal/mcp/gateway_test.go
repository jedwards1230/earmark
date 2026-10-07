package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
)

const testVirtualKey = "sk-earmark-SECRET-virtual-key"

// fakeLiteLLM is an httptest LiteLLM: readiness/liveliness, and /key/info
// that requires the virtual key and echoes key material the prober must drop.
type fakeLiteLLM struct {
	readiness   int // status for /health/readiness
	keyInfoCode int // status for /key/info (when the bearer is right)
	hits        atomic.Int32
	keyHits     atomic.Int32
	sawQuery    atomic.Bool
}

func (f *fakeLiteLLM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		switch r.URL.Path {
		case "/health/readiness":
			w.WriteHeader(f.readiness)
			_, _ = w.Write([]byte(`{"status":"healthy","db":"connected","litellm_version":"1.102.1","cache":null}`))
		case "/health/liveliness":
			_, _ = w.Write([]byte(`"I'm alive!"`))
		case "/key/info":
			f.keyHits.Add(1)
			if r.URL.RawQuery != "" {
				f.sawQuery.Store(true)
			}
			if r.Header.Get("Authorization") != "Bearer "+testVirtualKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.keyInfoCode != http.StatusOK {
				w.WriteHeader(f.keyInfoCode)
				_, _ = w.Write([]byte(`{"error":"` + testVirtualKey + `"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"key":%q,"info":{"token":"hashed-%s","key_name":"sk-...key","key_alias":"earmark",
				"models":["nomic-embed-text","anthropic/*"],"spend":12.25,"max_budget":50,"budget_duration":"30d",
				"budget_reset_at":"2026-10-18T00:00:00","expires":null,"blocked":false,"status":"active",
				"tpm_limit":100000,"rpm_limit":60,"model_spend":{}}}`, testVirtualKey, testVirtualKey)
		default:
			// Never a completion or embedding call.
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	})
}

func TestHTTPGatewayProber_ReadsOwnKeyInfo(t *testing.T) {
	f := &fakeLiteLLM{readiness: 200, keyInfoCode: 200}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newHTTPGatewayProber(2*time.Second, time.Hour)
	st := p.Probe(context.Background(), srv.URL+"/v1/", testVirtualKey)

	assert.True(t, st.Probed)
	assert.Equal(t, srv.URL, st.Base, "trailing /v1 stripped")
	assert.True(t, st.Reachable)
	assert.Equal(t, "healthy", st.Health)
	assert.Equal(t, "connected", st.DB)
	assert.Equal(t, "1.102.1", st.Version)
	require.True(t, st.KeyInfoOK, st.KeyInfoErr)
	assert.Equal(t, "earmark", st.Key.KeyAlias)
	assert.Equal(t, []string{"anthropic/*", "nomic-embed-text"}, st.Key.Models)
	assert.InDelta(t, 12.25, st.Key.Spend, 0.001)
	require.NotNil(t, st.Key.MaxBudget)
	require.NotNil(t, st.Key.RPMLimit)
	assert.False(t, f.sawQuery.Load(), "/key/info is called WITHOUT a key= query (the caller's own key)")

	// No key material anywhere in what the prober returns or the API emits.
	raw, _ := json.Marshal(st)
	api, _ := json.Marshal(apiGatewaysFrom([]gatewayStatus{st}, nil))
	for _, out := range []string{string(raw), fmt.Sprintf("%+v", st), string(api)} {
		assert.NotContains(t, out, "SECRET")
		assert.NotContains(t, out, "hashed-")
		assert.NotContains(t, out, "sk-")
	}

	// Allowlist matching through the wildcard.
	assert.True(t, *modelAllowed(st.Key.Models, "anthropic/claude-haiku-4-5-20251001"))
	assert.False(t, *modelAllowed(st.Key.Models, "earmark-judge"))
}

func TestHTTPGatewayProber_CachesPerBaseAndKey(t *testing.T) {
	f := &fakeLiteLLM{readiness: 200, keyInfoCode: 200}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	p := newHTTPGatewayProber(2*time.Second, time.Hour)
	_ = p.Probe(context.Background(), srv.URL+"/v1", testVirtualKey)
	_ = p.Probe(context.Background(), srv.URL, testVirtualKey) // same base after /v1 strip
	assert.EqualValues(t, 1, f.keyHits.Load(), "second probe within TTL must hit the cache")
	_ = p.Probe(context.Background(), srv.URL, "other-key")
	assert.EqualValues(t, 2, f.keyHits.Load(), "a different key is a different cache entry")
}

func TestHTTPGatewayProber_KeyInfoErrors(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		key     string
		wantErr string
	}{
		{"wrong key 401", 200, "not-the-key", "key info not readable by earmark's key (HTTP 401)"},
		{"forbidden 403", http.StatusForbidden, testVirtualKey, "key info not readable by earmark's key (HTTP 403)"},
		{"server error", http.StatusInternalServerError, testVirtualKey, "key info returned HTTP 500"},
		{"no key configured", 200, "", "no API key configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLiteLLM{readiness: 200, keyInfoCode: tc.code}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			st := newHTTPGatewayProber(2*time.Second, time.Hour).Probe(context.Background(), srv.URL+"/v1", tc.key)
			assert.True(t, st.Reachable)
			assert.False(t, st.KeyInfoOK)
			assert.Contains(t, st.KeyInfoErr, tc.wantErr)
			assert.NotContains(t, st.KeyInfoErr, "SECRET", "error text never echoes the response body")
		})
	}
}

func TestHTTPGatewayProber_LivelinessFallbackAndDown(t *testing.T) {
	f := &fakeLiteLLM{readiness: http.StatusServiceUnavailable, keyInfoCode: 200}
	srv := httptest.NewServer(f.handler())
	st := newHTTPGatewayProber(2*time.Second, time.Hour).Probe(context.Background(), srv.URL, testVirtualKey)
	assert.True(t, st.Reachable)
	assert.Equal(t, "alive", st.Health)
	srv.Close()

	down := newHTTPGatewayProber(500*time.Millisecond, time.Hour).Probe(context.Background(), srv.URL, testVirtualKey)
	assert.True(t, down.Probed)
	assert.False(t, down.Reachable)
	assert.Equal(t, "key info unreachable", down.KeyInfoErr)
}

func TestHTTPGatewayProber_NoRedirectsAndHTTPOnly(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redir.Close()
	st := newHTTPGatewayProber(2*time.Second, time.Hour).Probe(context.Background(), redir.URL, testVirtualKey)
	assert.False(t, followed.Load(), "the bearer must never follow a redirect")
	assert.False(t, st.Reachable)
	assert.False(t, st.KeyInfoOK)

	bad := newHTTPGatewayProber(time.Second, time.Hour).Probe(context.Background(), "file:///etc/passwd", testVirtualKey)
	assert.False(t, bad.Reachable)
	assert.Contains(t, bad.KeyInfoErr, "not http(s)")
}

func TestGatewayTargets(t *testing.T) {
	cfg := &config.Config{AIEndpoints: []config.AIEndpoint{
		{ID: "a", BaseURL: "http://litellm.lan:4000/v1", APIKey: "k1"},
		{ID: "b", BaseURL: "http://litellm.lan:4000", APIKey: "k1"},
		{ID: "c", BaseURL: "http://10.0.0.5:4000/v1", Gateway: "litellm", APIKey: "k2"},
		{ID: "d", BaseURL: "http://ollama:11434/v1"},
	}}
	ts := gatewayTargets(cfg)
	require.Len(t, ts, 2)
	assert.Equal(t, "http://litellm.lan:4000", ts[0].Base)
	assert.Len(t, ts[0].Endpoints, 2)
	assert.Equal(t, "http://10.0.0.5:4000", ts[1].Base)
}

func TestBuildGatewayViews(t *testing.T) {
	budget := 50.0
	st := gatewayStatus{Probed: true, Host: "gw:4000", Reachable: true, Health: "healthy", DB: "connected", KeyInfoOK: true,
		Key: gatewayKeyInfo{KeyAlias: "earmark", Models: []string{"nomic-embed-text"}, Spend: 12.25, MaxBudget: &budget,
			BudgetResetAt: testNow.Add(12 * 24 * time.Hour).Format("2006-01-02T15:04:05"), Expires: testNow.Add(-time.Hour).Format(time.RFC3339)}}
	targets := []gatewayTarget{{Endpoints: []config.AIEndpoint{{ID: "judge-ep"}}}}
	roles := []roleCard{{Title: "Judge", EndpointID: "judge-ep", Requested: "earmark-judge", AllowState: "denied"}}
	v := buildGatewayViews([]gatewayStatus{st}, targets, roles, testNow)
	require.Len(t, v, 1)
	assert.Equal(t, "✓ READY", v[0].StateLabel)
	assert.Equal(t, "state-busy", v[0].Class, "warnings turn a READY gateway amber")
	assert.Equal(t, "$12.25 of $50.00 budget, resets in 12d", v[0].SpendText)
	assert.True(t, strings.HasPrefix(v[0].ExpiresText, "EXPIRED"))
	require.Len(t, v[0].RoleModels, 1)
	assert.Equal(t, "✗ NOT ALLOWED", v[0].RoleModels[0].Mark)
	assert.Len(t, v[0].Warnings, 2) // expired key + judge not allowed

	down := buildGatewayViews([]gatewayStatus{{Probed: true, Host: "gw:4000"}}, nil, nil, testNow)
	assert.Equal(t, "✗ DOWN", down[0].StateLabel)
}
