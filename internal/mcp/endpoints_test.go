package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostOnly(t *testing.T) {
	assert.Equal(t, "ollama:11434", hostOnly("http://ollama:11434/v1"))
	assert.Equal(t, "gpu-host:8000", hostOnly("http://gpu-host:8000/v1"))
	assert.Equal(t, "example.com", hostOnly("https://example.com/v1"))
	// Unparseable → returned verbatim.
	assert.Equal(t, "not a url", hostOnly("not a url"))
}

func TestSortedOptions(t *testing.T) {
	assert.Nil(t, sortedOptions(nil))
	got := sortedOptions(map[string]string{"temperature": "0", "max_tokens": "256", "top_p": "1"})
	require.Len(t, got, 3)
	// Deterministic, key-sorted.
	assert.Equal(t, "max_tokens", got[0].Key)
	assert.Equal(t, "temperature", got[1].Key)
	assert.Equal(t, "top_p", got[2].Key)
}

// fakeEndpointProber returns a per-baseURL canned probe for view tests.
type fakeEndpointProber struct{ byURL map[string]endpointProbe }

func (f fakeEndpointProber) Probe(_ context.Context, baseURL, _ string) endpointProbe {
	return f.byURL[baseURL]
}

func TestBuildEndpointViews(t *testing.T) {
	cfg := &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, Backend: config.AIBackendOllama,
				BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "eval-1", Type: config.AIEndpointTypeChat, Backend: config.AIBackendVLLM,
				BaseURL: "http://gpu-host:8000/v1", Model: "Qwen2.5-7B-Instruct",
				Options: map[string]string{"temperature": "0"}},
			{ID: "spare", Type: config.AIEndpointTypeChat, Backend: config.AIBackendOpenAI,
				BaseURL: "http://spare:9000/v1", Model: "m"},
		},
		AIRoles: &config.AIRoles{Embeddings: "embed-1", Eval: "eval-1"},
	}
	probes := map[string]endpointProbe{
		"embed-1": {Probed: true, State: epStateReady},
		"eval-1":  {Probed: true, State: epStateModelMissing},
		// "spare" intentionally absent → UNKNOWN.
	}

	views := buildEndpointViews(cfg, probes)
	require.Len(t, views, 3)

	emb := views[0]
	assert.Equal(t, "embed-1", emb.ID)
	assert.Equal(t, "embeddings", emb.Type)
	assert.Equal(t, "ollama:11434", emb.HostOnly)
	assert.Equal(t, "embeddings", emb.Role)
	assert.Equal(t, "READY", emb.State.Label)
	assert.Equal(t, "ready", emb.StateToken)
	assert.True(t, emb.Probed)

	ev := views[1]
	assert.Equal(t, "eval", ev.Role)
	assert.Equal(t, "MODEL NOT LOADED", ev.State.Label)
	assert.Equal(t, "model_not_loaded", ev.StateToken)
	assert.Equal(t, "temperature=0", ev.optionsLine())
	assert.Equal(t, "role: eval (assigned)", ev.roleNote())

	sp := views[2]
	assert.Equal(t, "", sp.Role) // unbound
	assert.Equal(t, "", sp.roleNote())
	assert.Equal(t, "UNKNOWN", sp.State.Label)
	assert.Equal(t, "unknown", sp.StateToken)
	assert.False(t, sp.Probed)
}

func TestBuildEndpointViews_NilCfg(t *testing.T) {
	assert.Nil(t, buildEndpointViews(nil, nil))
}

func TestHTTPEndpointProber_Ready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[{"id":"nomic-embed-text"},{"id":"other"}]}`))
	}))
	defer srv.Close()

	p := newHTTPEndpointProber(2*time.Second, 5*time.Second)
	got := p.Probe(context.Background(), srv.URL+"/v1", "nomic-embed-text")
	assert.True(t, got.Probed)
	assert.Equal(t, epStateReady, got.State)
}

func TestHTTPEndpointProber_ModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"some-other-model"}]}`))
	}))
	defer srv.Close()

	p := newHTTPEndpointProber(2*time.Second, 5*time.Second)
	got := p.Probe(context.Background(), srv.URL+"/v1", "nomic-embed-text")
	assert.Equal(t, epStateModelMissing, got.State)
}

func TestHTTPEndpointProber_EmptyListIsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := newHTTPEndpointProber(2*time.Second, 5*time.Second)
	// Up + 200 but no model list → up-ness wins → ready.
	got := p.Probe(context.Background(), srv.URL+"/v1", "nomic-embed-text")
	assert.Equal(t, epStateReady, got.State)
}

func TestHTTPEndpointProber_Non200IsOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newHTTPEndpointProber(2*time.Second, 5*time.Second)
	got := p.Probe(context.Background(), srv.URL+"/v1", "m")
	assert.True(t, got.Probed)
	assert.Equal(t, epStateOffline, got.State)
}

func TestHTTPEndpointProber_UnreachableIsOffline(t *testing.T) {
	p := newHTTPEndpointProber(500*time.Millisecond, 5*time.Second)
	// Reserved TEST-NET-1 address; connection should fail fast.
	got := p.Probe(context.Background(), "http://192.0.2.1:9/v1", "m")
	assert.Equal(t, epStateOffline, got.State)
}

func TestHTTPEndpointProber_RejectsNonHTTPScheme(t *testing.T) {
	p := newHTTPEndpointProber(2*time.Second, 5*time.Second)
	got := p.Probe(context.Background(), "file:///etc/passwd", "m")
	assert.Equal(t, epStateOffline, got.State)
}

func TestHTTPEndpointProber_CachesPerURL(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()

	p := newHTTPEndpointProber(2*time.Second, 1*time.Hour) // long TTL
	_ = p.Probe(context.Background(), srv.URL+"/v1", "m")
	_ = p.Probe(context.Background(), srv.URL+"/v1", "m")
	assert.Equal(t, 1, hits, "second probe within TTL must hit the cache, not the server")
}

func TestAPIStatusEndpointsArray(t *testing.T) {
	cfg := &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, Backend: config.AIBackendOllama,
				BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "eval-1", Type: config.AIEndpointTypeChat, Backend: config.AIBackendVLLM,
				BaseURL: "http://gpu-host:8000/v1", Model: "Qwen2.5-7B-Instruct",
				Options: map[string]string{"temperature": "0", "max_tokens": "256"}},
		},
		AIRoles: &config.AIRoles{Embeddings: "embed-1", Eval: "eval-1"},
	}
	srv := NewMCPServer(&SimpleMockDB{}, cfg)
	srv.endpointProber = fakeEndpointProber{byURL: map[string]endpointProbe{
		"http://ollama:11434/v1":  {Probed: true, State: epStateReady},
		"http://gpu-host:8000/v1": {Probed: true, State: epStateOffline},
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var got apiStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Endpoints, 2)

	e0 := got.Endpoints[0]
	assert.Equal(t, "embed-1", e0.ID)
	assert.Equal(t, "embeddings", e0.Type)
	assert.Equal(t, "ollama", e0.Backend)
	assert.Equal(t, "http://ollama:11434/v1", e0.BaseURL)
	assert.Equal(t, "nomic-embed-text", e0.Model)
	assert.Equal(t, "embeddings", e0.Role)
	assert.Equal(t, "ready", e0.State)
	assert.True(t, e0.Probed)
	assert.Nil(t, e0.Options)

	e1 := got.Endpoints[1]
	assert.Equal(t, "eval-1", e1.ID)
	assert.Equal(t, "chat", e1.Type)
	assert.Equal(t, "eval", e1.Role)
	assert.Equal(t, "offline", e1.State)
	assert.Equal(t, map[string]string{"temperature": "0", "max_tokens": "256"}, e1.Options)
}
