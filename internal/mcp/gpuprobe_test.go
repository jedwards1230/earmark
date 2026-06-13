package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPGPUProber_FetchAndCache(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"state":"gaming","claims":["steam:440"],
			"units":[{"unit":"asr-runner.service","running":false}],
			"gpu_vram_used_mb":7338,"gpu_vram_total_mb":32607}`))
	}))
	defer srv.Close()

	p := newHTTPGPUProber(2*time.Second, time.Minute)
	st := p.Probe(context.Background(), srv.URL)
	if !st.Reachable || st.State != "gaming" || st.ASRRunning == nil || *st.ASRRunning {
		t.Fatalf("unexpected status: %+v", st)
	}
	// Second call within TTL must be served from cache (no second hit).
	_ = p.Probe(context.Background(), srv.URL)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected 1 upstream hit (cache), got %d", got)
	}
}

func TestHTTPGPUProber_Unreachable(t *testing.T) {
	p := newHTTPGPUProber(500*time.Millisecond, time.Minute)
	// Reserved-for-docs TEST-NET-1 address → connection fails fast within timeout.
	st := p.Probe(context.Background(), "http://192.0.2.1:48750/status")
	if st.Reachable {
		t.Errorf("expected Reachable=false for an unroutable host, got %+v", st)
	}
}

func TestHTTPGPUProber_RejectsNonHTTPScheme(t *testing.T) {
	p := newHTTPGPUProber(2*time.Second, time.Minute)
	for _, u := range []string{"file:///etc/passwd", "gopher://x", "ftp://x/y", "://nope"} {
		if st := p.Probe(context.Background(), u); st.Reachable {
			t.Errorf("scheme %q should be rejected (unreachable), got %+v", u, st)
		}
	}
}

func TestHTTPGPUProber_DoesNotFollowRedirect(t *testing.T) {
	// A redirect must not be followed (SSRF guard): the 3xx fails the 200 check,
	// so the probe reports unreachable rather than fetching the redirect target.
	var targetHit int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&targetHit, 1)
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusFound)
	}))
	defer redir.Close()

	p := newHTTPGPUProber(2*time.Second, time.Minute)
	if st := p.Probe(context.Background(), redir.URL); st.Reachable {
		t.Errorf("redirect should not be followed → unreachable, got %+v", st)
	}
	if atomic.LoadInt32(&targetHit) != 0 {
		t.Errorf("redirect target must not be hit, got %d hits", targetHit)
	}
}

func TestHTTPGPUProber_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := newHTTPGPUProber(2*time.Second, time.Minute)
	if st := p.Probe(context.Background(), srv.URL); st.Reachable {
		t.Errorf("non-200 should be treated as unreachable, got %+v", st)
	}
}
