package batch

import (
	"errors"
	"testing"
	"time"

	ibatch "github.com/jedwards1230/earmark/internal/batch"
)

// fakeLookup returns a scripted lookup function standing in for exec.LookPath,
// so tests never depend on what happens to actually be installed on PATH.
func fakeLookup(found map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if path, ok := found[name]; ok {
			return path, nil
		}
		return "", errors.New("exec: \"" + name + "\": executable file not found in $PATH")
	}
}

// TestResolveArbiterBinary_Precedence covers the fallback-selection order:
// --arbiter-wait-cmd flag > $ARBITER_WAIT_CMD env var > PATH auto-detection >
// "" (none resolved).
func TestResolveArbiterBinary_Precedence(t *testing.T) {
	cases := []struct {
		name    string
		flagVal string
		envVal  string
		onPath  map[string]string
		want    string
	}{
		{
			name:    "flag wins over everything",
			flagVal: "/opt/explicit/gpu-arbiter",
			envVal:  "/opt/env/gpu-arbiter",
			onPath:  map[string]string{"gpu-arbiter": "/usr/local/bin/gpu-arbiter"},
			want:    "/opt/explicit/gpu-arbiter",
		},
		{
			name:   "env wins over PATH when flag unset",
			envVal: "/opt/env/gpu-arbiter",
			onPath: map[string]string{"gpu-arbiter": "/usr/local/bin/gpu-arbiter"},
			want:   "/opt/env/gpu-arbiter",
		},
		{
			name:   "PATH auto-detection when flag and env unset",
			onPath: map[string]string{"gpu-arbiter": "/usr/local/bin/gpu-arbiter"},
			want:   "/usr/local/bin/gpu-arbiter",
		},
		{
			name: "none resolve",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal != "" {
				t.Setenv(arbiterWaitCmdEnvVar, tc.envVal)
			}
			got := resolveArbiterBinary(tc.flagVal, fakeLookup(tc.onPath))
			if got != tc.want {
				t.Errorf("resolveArbiterBinary(%q, ...) = %q, want %q", tc.flagVal, got, tc.want)
			}
		})
	}
}

// TestBuildArbiter_FallbackSelection is table-driven over the decision matrix:
// (arbiterURL set?, binary resolvable?) → exec-delegated vs plain-HTTP arbiter.
// It never executes a real binary — the exec-based arbiter is only
// constructed, not invoked.
func TestBuildArbiter_FallbackSelection(t *testing.T) {
	cases := []struct {
		name         string
		arbiterURL   string
		flagVal      string
		onPath       map[string]string
		wantDelegate bool
	}{
		{
			name:       "no arbiter URL configured — always HTTP (no-op) arbiter, even if a binary exists",
			arbiterURL: "",
			onPath:     map[string]string{"gpu-arbiter": "/usr/local/bin/gpu-arbiter"},
		},
		{
			name:       "arbiter URL set, no binary found — falls back to HTTP poll loop",
			arbiterURL: "http://gpu-host:48750/status",
		},
		{
			name:         "arbiter URL set, binary found on PATH — delegates",
			arbiterURL:   "http://gpu-host:48750/status",
			onPath:       map[string]string{"gpu-arbiter": "/usr/local/bin/gpu-arbiter"},
			wantDelegate: true,
		},
		{
			name:         "arbiter URL set, explicit --arbiter-wait-cmd override — delegates without a PATH lookup",
			arbiterURL:   "http://gpu-host:48750/status",
			flagVal:      "/opt/custom/gpu-arbiter",
			wantDelegate: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := options{
				arbiterWaitCmd: tc.flagVal,
				arbiterPoll:    15 * time.Second,
				arbiterTimeout: 3 * time.Second,
			}
			arb := buildArbiter(tc.arbiterURL, o, fakeLookup(tc.onPath))
			_, isWaiter := arb.(ibatch.Waiter)
			if isWaiter != tc.wantDelegate {
				t.Errorf("buildArbiter(%q, flag=%q) delegated = %v, want %v", tc.arbiterURL, tc.flagVal, isWaiter, tc.wantDelegate)
			}
		})
	}
}
