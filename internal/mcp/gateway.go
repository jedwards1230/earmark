package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
)

// LiteLLM gateway probe (CONTRACT §2.14).
//
// For every distinct LiteLLM gateway the AI registry routes through (gateway
// == "litellm", declared or inferred), the Models page reads two things,
// using ONLY the endpoint's own virtual key:
//
//   - GET <base>/health/readiness (fallback /health/liveliness): is the proxy
//     up, and is its DB connected. Neither needs a key or calls a model.
//   - GET <base>/key/info with "Authorization: Bearer <virtual key>" and NO
//     query parameter: LiteLLM answers with the CALLER's own key info — alias,
//     allowed models, spend, budget, limits, status. The master key is never
//     needed or used.
//
// It never sends a completion or embedding request. The response of
// /key/info also carries the hashed token; it is never decoded — only the
// fields in keyInfoWire are read, so no key material can be rendered, logged,
// or returned by the API.
//
// <base> is the endpoint baseURL with a trailing "/v1" stripped (LiteLLM's
// health and key routes live at the root, its OpenAI routes under /v1).

// gatewayProbeTTL is how long one gateway's status is reused. Spend and the
// allowlist change rarely; the page serves the cached value and refreshes in
// the background (refreshCache), so a slow gateway never delays a poll.
const gatewayProbeTTL = 60 * time.Second

// gatewayFirstWait bounds how long one page render waits for gateways whose
// status has never loaded (shared across all gateways in that render).
const gatewayFirstWait = 1500 * time.Millisecond

// maxGatewayBody caps each gateway response read.
const maxGatewayBody = 64 << 10 // 64 KB

// gatewayKeyInfo is the subset of LiteLLM's /key/info "info" object earmark
// reads. Deliberately excludes "token"/"key" and every other field.
type gatewayKeyInfo struct {
	KeyAlias string `json:"key_alias"`
	// TeamID marks a team key: an empty Models list then means "the team's
	// models", which this key cannot read — not "all models".
	TeamID         string   `json:"team_id"`
	Models         []string `json:"models"`
	Spend          float64  `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration"`
	BudgetResetAt  string   `json:"budget_reset_at"`
	Expires        string   `json:"expires"`
	Blocked        *bool    `json:"blocked"`
	Status         string   `json:"status"`
	TPMLimit       *int64   `json:"tpm_limit"`
	RPMLimit       *int64   `json:"rpm_limit"`
}

// keyInfoWire is the /key/info envelope; the top-level "key" is ignored.
type keyInfoWire struct {
	Info gatewayKeyInfo `json:"info"`
}

// readinessWire is the /health/readiness body subset.
type readinessWire struct {
	Status  string `json:"status"`
	DB      string `json:"db"`
	Version string `json:"litellm_version"`
}

// gatewayStatus is one probed gateway (as seen through one virtual key).
type gatewayStatus struct {
	Probed bool
	Base   string // scheme://host[:port] (trailing /v1 stripped)
	Host   string // host[:port]
	// Reachable is true when readiness (or the liveliness fallback) returned 200.
	Reachable bool
	Health    string // readiness "status" ("healthy"), "alive" via liveliness, "" when down
	DB        string // readiness "db" ("connected"), "" when unknown
	Version   string // litellm_version when reported
	// KeyInfoOK is true when /key/info answered 200 for this key; otherwise
	// KeyInfoErr says why (never containing key material).
	KeyInfoOK  bool
	KeyInfoErr string
	Key        gatewayKeyInfo
}

// gatewayProber probes one gateway base with one virtual key.
type gatewayProber interface {
	Probe(ctx context.Context, baseURL, apiKey string) gatewayStatus
}

// gatewayBase strips a trailing "/v1" (and slashes) from an endpoint baseURL.
// It parses the URL so a query or fragment can't defeat the strip and a path
// prefix is kept ("http://h/litellm/v1" → "http://h/litellm"); an unparseable
// value is returned trimmed (fetch then rejects it as not http(s)).
func gatewayBase(baseURL string) string {
	u, err := neturl.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return strings.TrimRight(baseURL, "/")
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/v1")
	u.Path, u.RawPath = strings.TrimRight(p, "/"), ""
	return u.String()
}

// httpGatewayProber implements gatewayProber over HTTP with a per-(base, key)
// stale-while-revalidate cache.
type httpGatewayProber struct {
	client *http.Client
	ttl    time.Duration
	now    func() time.Time

	mu     sync.Mutex
	caches map[endpointProbeKey]*refreshCache[gatewayStatus]
}

func newHTTPGatewayProber(timeout, ttl time.Duration) *httpGatewayProber {
	return &httpGatewayProber{
		// The default transport honours HTTPS_PROXY/HTTP_PROXY, so in a pod with a
		// proxy configured the bearer travels via that proxy, exactly like the
		// embed and judge clients' calls to the same gateway. NO_PROXY the gateway
		// host if that is not wanted.
		client: &http.Client{
			Timeout: timeout,
			// No redirects: a compromised gateway must not bounce the bearer
			// token (or the request) to another host. A 3xx is a failure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ttl:    ttl,
		now:    time.Now,
		caches: map[endpointProbeKey]*refreshCache[gatewayStatus]{},
	}
}

// Probe returns the cached status for (baseURL, apiKey), waiting (bounded by
// ctx) only for the very first load.
func (p *httpGatewayProber) Probe(ctx context.Context, baseURL, apiKey string) gatewayStatus {
	base := gatewayBase(baseURL)
	key := endpointProbeKey{baseURL: base}
	if apiKey != "" {
		key.keySum = sha256.Sum256([]byte(apiKey))
	}
	p.mu.Lock()
	c, ok := p.caches[key]
	if !ok {
		// Up to three sequential requests per refresh (readiness, the liveliness
		// fallback, key info), each bounded by the client timeout, plus slack.
		c = newRefreshCache(p.ttl, 3*p.client.Timeout+time.Second, func(ctx context.Context) (gatewayStatus, error) {
			return p.fetch(ctx, base, apiKey), nil
		})
		c.now = p.now
		p.caches[key] = c
	}
	p.mu.Unlock()

	e, _ := c.get(ctx) // fetch never errors; failures are encoded in the status
	if e == nil {
		return gatewayStatus{Base: base, Host: hostOnly(base)} // first load cancelled
	}
	return e.Val
}

func (p *httpGatewayProber) fetch(ctx context.Context, base, apiKey string) gatewayStatus {
	st := gatewayStatus{Probed: true, Base: base, Host: hostOnly(base)}
	if u, err := neturl.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		st.KeyInfoErr = "gateway base URL is not http(s)"
		return st
	}

	var rd readinessWire
	if code, err := p.getJSON(ctx, base+"/health/readiness", "", &rd); err == nil && code == http.StatusOK {
		st.Reachable, st.Health, st.DB, st.Version = true, cmp.Or(rd.Status, "ready"), rd.DB, rd.Version
	} else if code, err := p.getJSON(ctx, base+"/health/liveliness", "", nil); err == nil && code == http.StatusOK {
		st.Reachable, st.Health = true, "alive"
	}

	if apiKey == "" {
		st.KeyInfoErr = "no API key configured for this endpoint (apiKeyEnv)"
	} else {
		var ki keyInfoWire
		code, err := p.getJSON(ctx, base+"/key/info", apiKey, &ki)
		switch {
		case err != nil && code == 0:
			st.KeyInfoErr = "key info unreachable"
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			st.KeyInfoErr = fmt.Sprintf("key info not readable by earmark's key (HTTP %d)", code)
		case code != http.StatusOK:
			st.KeyInfoErr = fmt.Sprintf("key info returned HTTP %d", code)
		case err != nil:
			st.KeyInfoErr = "key info response was not understood"
		default:
			st.KeyInfoOK, st.Key = true, ki.Info
			sort.Strings(st.Key.Models)
		}
	}
	return st
}

// getJSON GETs url (with an optional bearer) and decodes a 200 body into out
// (nil = discard). It returns the status code (0 when no response) and a
// transport/decode error. The error text is never surfaced verbatim: callers
// map it to fixed strings, so neither a URL nor a key can leak through it.
func (p *httpGatewayProber) getJSON(ctx context.Context, url, bearer string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxGatewayBody)
	if resp.StatusCode != http.StatusOK || out == nil {
		_, _ = io.Copy(io.Discard, body)
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// gatewayTarget is one distinct (base, key) to probe and the endpoints using it.
type gatewayTarget struct {
	Base      string
	APIKey    string
	Endpoints []config.AIEndpoint
}

// gatewayTargets groups the LiteLLM endpoints (declared or inferred) by
// (base, key), in registry order.
func gatewayTargets(cfg *config.Config) []gatewayTarget {
	if cfg == nil {
		return nil
	}
	var out []gatewayTarget
	idx := map[[2]string]int{}
	for _, ep := range cfg.AIEndpoints {
		if gw, _ := gatewayFor(ep.Gateway, ep.BaseURL); gw != "litellm" {
			continue
		}
		k := [2]string{gatewayBase(ep.BaseURL), ep.APIKey}
		i, ok := idx[k]
		if !ok {
			out = append(out, gatewayTarget{Base: k[0], APIKey: ep.APIKey})
			i = len(out) - 1
			idx[k] = i
		}
		out[i].Endpoints = append(out[i].Endpoints, ep)
	}
	return out
}

// probeGateways probes every distinct LiteLLM gateway and returns the statuses
// in registry order plus a map from endpoint id to its gateway's status.
func (s *MCPServer) probeGateways(ctx context.Context) ([]gatewayStatus, []gatewayTarget, map[string]gatewayStatus) {
	if s.gatewayProber == nil {
		return nil, nil, nil
	}
	targets := gatewayTargets(s.cfg)
	if len(targets) == 0 {
		return nil, nil, nil
	}
	// Bound the FIRST load of every gateway together: after it, probes are
	// served from cache instantly; a slow first load shows "not probed yet"
	// and lands on a later poll instead of holding up the page.
	gctx, cancel := context.WithTimeout(ctx, gatewayFirstWait)
	defer cancel()
	statuses := make([]gatewayStatus, 0, len(targets))
	byEndpoint := map[string]gatewayStatus{}
	for _, t := range targets {
		st := s.gatewayProber.Probe(gctx, t.Base, t.APIKey)
		statuses = append(statuses, st)
		for _, ep := range t.Endpoints {
			byEndpoint[ep.ID] = st
		}
	}
	return statuses, targets, byEndpoint
}

// gatewayRoleModel is one role's requested model checked against the key
// allowlist on the gateway card.
type gatewayRoleModel struct {
	Role, Model string
	Mark        string // "✓ allowed" / "✗ NOT ALLOWED" / "? not checked"
	Class       string // match-ok / badge mismatch / time-muted
}

// gatewayView is one LiteLLM gateway card on the Models page.
type gatewayView struct {
	gatewayStatus
	EndpointIDs []string
	StateLabel  string // "✓ READY" / "✗ DOWN" / "? NOT PROBED"
	Class, Dot  string
	Sub         string
	Warnings    []string
	KeyState    string // "active" / "BLOCKED" / status verbatim
	ExpiresText string // "expires in 20d" / "EXPIRED 2d ago" / "no expiry"
	SpendText   string // "$12.25 · no budget set" / "$12.25 of $50.00 budget, resets in 12d"
	LimitsText  string // "rpm 60 · tpm 100,000" or ""
	AllowAll    bool
	TeamModels  bool // team key with no own list: inherits the team's models (unreadable)
	RoleModels  []gatewayRoleModel
}

// buildGatewayViews renders the probed gateways, checking each role whose
// endpoint sits behind the gateway against its key allowlist.
func buildGatewayViews(sts []gatewayStatus, targets []gatewayTarget, roles []roleCard, now time.Time) []gatewayView {
	out := make([]gatewayView, 0, len(sts))
	for i, st := range sts {
		v := gatewayView{gatewayStatus: st}
		ids := map[string]bool{}
		if i < len(targets) {
			for _, ep := range targets[i].Endpoints {
				v.EndpointIDs = append(v.EndpointIDs, ep.ID)
				ids[ep.ID] = true
			}
		}
		switch {
		case !st.Probed:
			v.StateLabel, v.Class, v.Dot, v.Sub = "? NOT PROBED", "state-unknown", "grey", "gateway status not loaded yet"
		case !st.Reachable:
			v.StateLabel, v.Class, v.Dot, v.Sub = "✗ DOWN", "state-stalled", "red", "readiness and liveliness probes failed — "+st.Host
		default:
			v.StateLabel, v.Class, v.Dot = "✓ READY", "state-running", "green"
			v.Sub = "proxy " + st.Health
			if st.DB != "" {
				v.Sub += " · db " + st.DB
			}
		}
		if st.KeyInfoOK {
			v.TeamModels = len(st.Key.Models) == 0 && st.Key.TeamID != ""
			v.AllowAll = len(st.Key.Models) == 0 && !v.TeamModels
			for _, m := range st.Key.Models {
				if m == "*" || m == "all-proxy-models" {
					v.AllowAll = true
				}
			}
			v.KeyState = cmp.Or(st.Key.Status, "active")
			if st.Key.Blocked != nil && *st.Key.Blocked {
				v.KeyState = "BLOCKED"
				v.Warnings = append(v.Warnings, "earmark's virtual key is blocked — every call fails")
			}
			v.ExpiresText = expiresText(st.Key.Expires, now)
			if strings.HasPrefix(v.ExpiresText, "EXPIRED") {
				v.Warnings = append(v.Warnings, "earmark's virtual key has expired — every call fails")
			}
			v.SpendText = spendText(st.Key, now)
			v.LimitsText = limitsText(st.Key)
		} else if st.Probed && st.KeyInfoErr != "" {
			v.Warnings = append(v.Warnings, st.KeyInfoErr)
		}
		for _, r := range roles {
			if !ids[r.EndpointID] {
				continue
			}
			rm := gatewayRoleModel{Role: strings.ToLower(r.Title), Model: r.Requested, Mark: "? not checked", Class: "time-muted"}
			switch r.AllowState {
			case "allowed":
				rm.Mark, rm.Class = "✓ allowed", "match-ok"
			case "denied":
				rm.Mark, rm.Class = "✗ NOT ALLOWED", "badge mismatch"
				v.Warnings = append(v.Warnings, r.Requested+" ("+rm.Role+") is not on the key allowlist — calls 403")
			}
			v.RoleModels = append(v.RoleModels, rm)
		}
		if len(v.Warnings) > 0 && v.Class == "state-running" {
			v.Class, v.Dot = "state-busy", "amber"
		}
		out = append(out, v)
	}
	return out
}

// parseLiteLLMTime parses LiteLLM's timestamps, which come with or without a
// zone and with or without fractional seconds (zone-less = UTC).
func parseLiteLLMTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// untilText renders a future instant as "in 12d" (past → "Nd ago").
func untilText(t, now time.Time) string {
	if !t.After(now) {
		return humanizeSince(now.Sub(t))
	}
	return "in " + sinceShort(t.Sub(now))
}

func expiresText(raw string, now time.Time) string {
	if raw == "" {
		return "no expiry"
	}
	t, ok := parseLiteLLMTime(raw)
	if !ok {
		return "expires " + raw
	}
	if !t.After(now) {
		return "EXPIRED " + humanizeSince(now.Sub(t))
	}
	return "expires " + untilText(t, now)
}

func spendText(k gatewayKeyInfo, now time.Time) string {
	spend := fmt.Sprintf("$%.2f", k.Spend)
	if k.MaxBudget == nil {
		return spend + " · no budget set"
	}
	s := fmt.Sprintf("%s of $%.2f budget", spend, *k.MaxBudget)
	if t, ok := parseLiteLLMTime(k.BudgetResetAt); ok {
		s += ", resets " + untilText(t, now)
	} else if k.BudgetDuration != "" {
		s += ", resets every " + k.BudgetDuration
	}
	return s
}

func limitsText(k gatewayKeyInfo) string {
	var parts []string
	if k.RPMLimit != nil {
		parts = append(parts, "rpm "+commafy64(*k.RPMLimit))
	}
	if k.TPMLimit != nil {
		parts = append(parts, "tpm "+commafy64(*k.TPMLimit))
	}
	return strings.Join(parts, " · ")
}
