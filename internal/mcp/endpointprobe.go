package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"
)

// AI endpoint health probe.
//
// Every endpoint in the AI registry (CONTRACT §2.14) is probed for liveness on
// each /servers/data refresh so the Models/Services page can show whether the
// embeddings (and any chat) endpoint is reachable, without blocking the page on
// a slow upstream. The probe is a `GET <baseURL>/models` request — the standard
// OpenAI-compatible health path that both Ollama and vLLM serve — with a short
// timeout. Mirrors the gpu-arbiter prober (gpuprobe.go): a TTL cache coalesces
// the /servers + /api/v1/status probes within one refresh window.

// endpointProbeState is the derived liveness of one AI endpoint.
type endpointProbeState string

const (
	// epStateReady — 200 OK and the configured model appears in the /models list.
	epStateReady endpointProbeState = "ready"
	// epStateModelMissing — 200 OK but the configured model is not in the list.
	epStateModelMissing endpointProbeState = "model_not_loaded"
	// epStateOffline — non-200, timeout, or unreachable.
	epStateOffline endpointProbeState = "offline"
	// epStateUnknown — not probed yet (no prober wired).
	epStateUnknown endpointProbeState = "unknown"
)

// endpointProbe is the parsed result of probing one endpoint. Probed=false
// means no probe ran (no prober configured) → state stays unknown.
type endpointProbe struct {
	Probed bool
	State  endpointProbeState
}

// endpointProber resolves a (baseURL, model) pair to a health result.
// Implemented by httpEndpointProber in production and a static fake in the demo.
type endpointProber interface {
	Probe(ctx context.Context, baseURL, model string) endpointProbe
}

// httpEndpointProber polls an OpenAI-compatible /models endpoint with a short
// timeout and a TTL cache keyed by baseURL, so the /servers fragment and
// /api/v1/status share a single upstream call per refresh window.
type httpEndpointProber struct {
	client *http.Client
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]endpointProbeEntry
}

type endpointProbeEntry struct {
	models map[string]bool // model ids reported by /models (lower-cased)
	ok     bool            // the GET /models call itself succeeded (200 + parseable)
	at     time.Time
}

// maxModelsBody caps the /models response we read. A real list is small; the
// cap stops a malicious/buggy endpoint from forcing an unbounded allocation.
const maxModelsBody = 256 << 10 // 256 KB

func newHTTPEndpointProber(timeout, ttl time.Duration) *httpEndpointProber {
	return &httpEndpointProber{
		client: &http.Client{
			Timeout: timeout,
			// Don't follow redirects: a /models probe never redirects, and following
			// one would let a compromised endpoint bounce the request to an internal
			// target (SSRF). A 3xx then fails the StatusOK check → treated offline.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ttl:   ttl,
		now:   time.Now,
		cache: map[string]endpointProbeEntry{},
	}
}

func (p *httpEndpointProber) Probe(ctx context.Context, baseURL, model string) endpointProbe {
	p.mu.Lock()
	entry, ok := p.cache[baseURL]
	if ok && p.now().Sub(entry.at) >= p.ttl {
		ok = false // expired
	}
	p.mu.Unlock()

	if !ok {
		entry = p.fetch(ctx, baseURL)
		p.mu.Lock()
		entry.at = p.now()
		p.cache[baseURL] = entry
		p.mu.Unlock()
	}

	if !entry.ok {
		return endpointProbe{Probed: true, State: epStateOffline}
	}
	// 200 OK. If the model is named and present → ready; named but absent →
	// model_not_loaded. An empty model list (some servers return nothing useful)
	// is treated as ready: the endpoint is up, which is the load-bearing signal.
	if model != "" && len(entry.models) > 0 && !modelLoaded(entry.models, model) {
		return endpointProbe{Probed: true, State: epStateModelMissing}
	}
	return endpointProbe{Probed: true, State: epStateReady}
}

// modelLoaded reports whether the configured model appears in the endpoint's
// reported model set, tolerating ollama's `:tag` suffix. Ollama lists a model as
// `nomic-embed-text:latest` while the config often names it bare
// (`nomic-embed-text`), so an exact-match-only check spuriously reports
// model_not_loaded for a working endpoint. We match when, after stripping a
// trailing `:latest` from both sides, the configured model equals a returned id
// OR a returned id is the configured model carrying any tag (`model:<tag>`), and
// vice versa (configured `model:latest` vs returned bare `model`). The map keys
// are already lower-cased by fetch(); we lower-case the input to match.
func modelLoaded(models map[string]bool, model string) bool {
	want := strings.ToLower(model)
	wantBase := strings.TrimSuffix(want, ":latest")
	for id := range models {
		idBase := strings.TrimSuffix(id, ":latest")
		// Exact, or either side is the other carrying a `:tag` suffix. Comparing
		// the `:latest`-stripped bases also covers configured-bare vs returned
		// `:latest` (and the reverse).
		if want == id ||
			wantBase == idBase ||
			strings.HasPrefix(id, want+":") ||
			strings.HasPrefix(want, id+":") {
			return true
		}
	}
	return false
}

// modelsURL joins an OpenAI-compatible base URL with the "/models" path,
// tolerating a trailing slash on the base.
func modelsURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/models"
}

// openAIModelsList mirrors the OpenAI-compatible GET /v1/models response.
type openAIModelsList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (p *httpEndpointProber) fetch(ctx context.Context, baseURL string) endpointProbeEntry {
	rawURL := modelsURL(baseURL)
	// Only http/https — reject file://, gopher://, etc. so a mis- or maliciously
	// configured baseURL can't become an SSRF/file-read primitive.
	if u, err := neturl.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return endpointProbeEntry{ok: false}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return endpointProbeEntry{ok: false}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return endpointProbeEntry{ok: false}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return endpointProbeEntry{ok: false}
	}
	var list openAIModelsList
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&list); err != nil {
		// Reachable + 200 but unparseable body: the endpoint is up. Treat as ok
		// with an empty model set (→ ready, the up-ness is what matters).
		return endpointProbeEntry{ok: true}
	}
	models := make(map[string]bool, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			models[strings.ToLower(m.ID)] = true
		}
	}
	return endpointProbeEntry{ok: true, models: models}
}
