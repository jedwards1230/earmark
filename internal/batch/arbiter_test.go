package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGPUBusyForGame covers the state→wait mapping: the coordinator must yield
// on both "gaming" and "evicting", and proceed on everything else.
func TestGPUBusyForGame(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"gaming", true},
		{"evicting", true}, // mid-eviction: a game just launched — don't race it
		{"available", false},
		{"", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			if got := gpuBusyForGame(tc.state); got != tc.want {
				t.Errorf("gpuBusyForGame(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestHTTPArbiter_Gaming exercises the real HTTP arbiter against an httptest
// server for each /status state, asserting the (gaming, ok) result. Notably it
// must report gaming=true for "evicting" as well as "gaming".
func TestHTTPArbiter_Gaming(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		status     int
		wantGaming bool
		wantOK     bool
	}{
		{"gaming", `{"state":"gaming"}`, 200, true, true},
		{"evicting", `{"state":"evicting"}`, 200, true, true},
		{"available", `{"state":"available"}`, 200, false, true},
		{"empty state", `{"state":""}`, 200, false, true},
		{"non-200", `{"state":"gaming"}`, 503, false, false},
		{"bad json", `not json`, 200, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			arb := NewHTTPArbiter(srv.URL, time.Second)
			gaming, ok := arb.Gaming(context.Background())
			if gaming != tc.wantGaming || ok != tc.wantOK {
				t.Errorf("Gaming() = (%v, %v), want (%v, %v)", gaming, ok, tc.wantGaming, tc.wantOK)
			}
		})
	}
}

// TestHTTPArbiter_Unconfigured: an empty URL must be a no-op that returns
// ok=false so an unconfigured arbiter never blocks the coordinator.
func TestHTTPArbiter_Unconfigured(t *testing.T) {
	arb := NewHTTPArbiter("", time.Second)
	if gaming, ok := arb.Gaming(context.Background()); gaming || ok {
		t.Errorf("empty-URL arbiter Gaming() = (%v, %v), want (false, false)", gaming, ok)
	}
}

// TestHTTPArbiter_RejectsNonHTTPScheme: a non-http(s) URL must be rejected
// (ok=false) to avoid SSRF/file-read primitives.
func TestHTTPArbiter_RejectsNonHTTPScheme(t *testing.T) {
	arb := NewHTTPArbiter("file:///etc/passwd", time.Second)
	if gaming, ok := arb.Gaming(context.Background()); gaming || ok {
		t.Errorf("file:// arbiter Gaming() = (%v, %v), want (false, false)", gaming, ok)
	}
}
