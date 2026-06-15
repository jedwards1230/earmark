package batch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"time"
)

// gpu-arbiter readiness, coordinator side.
//
// gpu-arbiter (https://github.com/jedwards1230/gpu-arbiter) is a host daemon on
// the GPU box that treats the machine as a gaming PC first: when a game launches
// it evicts GPU tenants (the ASR runner, the eval judge) and restores them when
// gaming ends. It serves an unauthenticated LAN GET /status. The coordinator
// reads it before each batch and waits while it reports gaming — this is the
// ONLY gpu-arbiter interaction and it is strictly read-only (never POSTs).
//
// Mirrors internal/mcp/gpuprobe.go's JSON shape and SSRF guards, kept local so
// the coordinator stays a standalone, hardware-agnostic phase driver.

// gamingState is the gpu-arbiter /status state that means "the GPU is held by a
// game right now"; the coordinator waits while this is reported.
const gamingState = "gaming"

// arbiterRaw mirrors the gpu-arbiter /status JSON (only the field we use).
type arbiterRaw struct {
	State string `json:"state"` // "available" | "gaming" | "evicting"
}

// maxStatusBody caps the /status response we read; the real payload is well
// under 1 KB. The cap stops a buggy/hostile endpoint forcing a huge allocation.
const maxStatusBody = 64 << 10 // 64 KB

// httpArbiter polls gpu-arbiter over HTTP with a short timeout. A missing URL or
// any error yields ok=false so the coordinator proceeds (degrades gracefully).
type httpArbiter struct {
	url    string
	client *http.Client
}

// NewHTTPArbiter builds an Arbiter that GETs url (a gpu-arbiter /status
// endpoint). An empty url makes Gaming a no-op that always returns ok=false, so
// an unconfigured arbiter never blocks the coordinator.
func NewHTTPArbiter(url string, timeout time.Duration) Arbiter {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &httpArbiter{
		url: url,
		client: &http.Client{
			Timeout: timeout,
			// Don't follow redirects: a status probe never redirects, and following
			// one would let a compromised endpoint bounce the request to an internal
			// target (SSRF). A 3xx then fails the StatusOK check → ok=false.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Gaming reports whether gpu-arbiter currently has the GPU held by a game.
// ok=false means the arbiter is unconfigured or unreachable (caller proceeds).
func (a *httpArbiter) Gaming(ctx context.Context) (gaming bool, ok bool) {
	if a.url == "" {
		return false, false
	}
	// Only http/https — reject file://, gopher://, etc. so a mis- or maliciously
	// configured URL can't become an SSRF/file-read primitive.
	if u, err := neturl.Parse(a.url); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return false, false
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var raw arbiterRaw
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBody)).Decode(&raw); err != nil {
		return false, false
	}
	return raw.State == gamingState, true
}
