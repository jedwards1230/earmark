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

// gpu-arbiter readiness probe.
//
// gpu-arbiter (https://github.com/jedwards1230/gpu-arbiter) is a host daemon on
// the GPU box (gpu-1) that treats the machine as a gaming PC first: when a
// game launches it evicts GPU tenants (Ollama, the ASR runner) and restores
// them when gaming ends. It serves an unauthenticated LAN /status endpoint.
//
// Polling it gives the Servers page a live readiness signal the database can't:
// a configured runner that is reachable but whose GPU is held by a game is
// "connected but not usable" (the future fallback trigger), distinct from an
// unreachable host (offline) and from one actively transcribing.

// arbiterStatus is the parsed, dashboard-facing view of a gpu-arbiter /status
// response. Reachable=false means the probe failed (timeout, refused, bad body)
// — every other field is then zero.
type arbiterStatus struct {
	Reachable   bool
	State       string   // "available" | "gaming" | "evicting" | "" (unknown)
	Claims      []string // why the GPU is claimed, e.g. ["steam:413150"]
	ASRRunning  *bool    // asr-runner.service running (nil when no such unit reported)
	VRAMUsedMB  *int
	VRAMTotalMB *int
	// ResidentUnits is ALL reported units with their running state, so the
	// dashboard can list which models are currently resident (parakeet, gemma3 …)
	// without having to re-interpret the raw JSON.
	ResidentUnits []residentUnit
}

// residentUnit is one gpu-arbiter unit (service) with its running state.
type residentUnit struct {
	Unit    string // systemd unit name, e.g. "asr-runner.service", "ollama.service"
	Running bool
}

// ready reports whether the GPU is available for transcription right now:
// reachable, state "available", and (if known) the ASR runner unit is up.
func (a arbiterStatus) ready() bool {
	return a.Reachable && a.State == "available" && (a.ASRRunning == nil || *a.ASRRunning)
}

// arbiterRaw mirrors the gpu-arbiter /status JSON (only the fields we use).
type arbiterRaw struct {
	State  string   `json:"state"`
	Claims []string `json:"claims"`
	Units  []struct {
		Unit    string `json:"unit"`
		Running bool   `json:"running"`
	} `json:"units"`
	VRAMUsedMB  *int `json:"gpu_vram_used_mb"`
	VRAMTotalMB *int `json:"gpu_vram_total_mb"`
}

// gpuProber resolves a gpu-arbiter /status URL to an arbiterStatus. Implemented
// by httpGPUProber in production and a static fake in the demo/tests.
type gpuProber interface {
	Probe(ctx context.Context, url string) arbiterStatus
}

// httpGPUProber polls gpu-arbiter over HTTP with a short timeout and a TTL cache
// so multiple render paths (the /servers fragment + /api/v1/status) within one
// refresh window share a single upstream call. Any error → Reachable:false.
type httpGPUProber struct {
	client *http.Client
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]probeEntry
}

type probeEntry struct {
	status arbiterStatus
	at     time.Time
}

// maxProbeBody caps the gpu-arbiter response we read. The real /status payload
// is well under 1 KB; the cap stops a malicious/buggy endpoint from forcing an
// unbounded allocation (a huge claims/units array).
const maxProbeBody = 64 << 10 // 64 KB

func newHTTPGPUProber(timeout, ttl time.Duration) *httpGPUProber {
	return &httpGPUProber{
		client: &http.Client{
			Timeout: timeout,
			// Don't follow redirects: a status probe never redirects, and following
			// one would let a compromised endpoint bounce the request to an internal
			// target (SSRF). A 3xx then fails the StatusOK check → treated offline.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ttl:   ttl,
		now:   time.Now,
		cache: map[string]probeEntry{},
	}
}

func (p *httpGPUProber) Probe(ctx context.Context, url string) arbiterStatus {
	p.mu.Lock()
	if e, ok := p.cache[url]; ok && p.now().Sub(e.at) < p.ttl {
		p.mu.Unlock()
		return e.status
	}
	p.mu.Unlock()

	st := p.fetch(ctx, url)

	p.mu.Lock()
	p.cache[url] = probeEntry{status: st, at: p.now()}
	p.mu.Unlock()
	return st
}

func (p *httpGPUProber) fetch(ctx context.Context, rawURL string) arbiterStatus {
	// Only http/https — reject file://, gopher://, etc. so a mis- or maliciously
	// configured gpuArbiterUrl can't be turned into an SSRF/file-read primitive.
	if u, err := neturl.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return arbiterStatus{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return arbiterStatus{}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return arbiterStatus{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return arbiterStatus{}
	}
	var raw arbiterRaw
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProbeBody)).Decode(&raw); err != nil {
		return arbiterStatus{}
	}
	return raw.toStatus()
}

// toStatus converts the raw JSON into the dashboard view, locating the ASR
// runner unit by an "asr"/"parakeet" substring on the unit name, and carrying
// ALL reported units so the dashboard can list resident models.
func (r arbiterRaw) toStatus() arbiterStatus {
	st := arbiterStatus{
		Reachable:   true,
		State:       r.State,
		Claims:      r.Claims,
		VRAMUsedMB:  r.VRAMUsedMB,
		VRAMTotalMB: r.VRAMTotalMB,
	}
	for _, u := range r.Units {
		name := strings.ToLower(u.Unit)
		if strings.Contains(name, "asr") || strings.Contains(name, "parakeet") {
			running := u.Running
			st.ASRRunning = &running
		}
		st.ResidentUnits = append(st.ResidentUnits, residentUnit{Unit: u.Unit, Running: u.Running})
	}
	return st
}
