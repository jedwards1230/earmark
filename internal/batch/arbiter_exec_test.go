package batch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestArbiterBaseURL covers deriving gpu-arbiter's --url (a base URL) from
// GPU_ARBITER_URL (documented as the full /status URL, CONTRACT §2.4).
func TestArbiterBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trailing status", "http://gpu-host:48750/status", "http://gpu-host:48750"},
		{"trailing status and slash", "http://gpu-host:48750/status/", "http://gpu-host:48750"},
		{"no status suffix", "http://gpu-host:48750", "http://gpu-host:48750"},
		{"empty", "", ""},
		{"trailing slash only", "http://gpu-host:48750/", "http://gpu-host:48750"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := arbiterBaseURL(tc.in); got != tc.want {
				t.Errorf("arbiterBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// fakeExecRunner scripts a sequence of (exitCode, stderr, err) results,
// recording every invocation's (bin, args) so tests can assert exact command
// construction without executing a real binary.
type fakeExecRunner struct {
	results []fakeExecResult
	idx     int
	calls   []fakeExecCall
	// blockUntilCancel, if set, makes run() block on ctx.Done() instead of
	// returning a scripted result — used to simulate a long-running child that
	// only exits when its context is cancelled (exec.CommandContext killing it).
	blockUntilCancel bool
	// delay, if set, makes run() take this long (or until ctx is done,
	// whichever comes first) before returning its scripted result — used to
	// simulate a slow-failing child (e.g. a network call that errors partway
	// through the wait window).
	delay time.Duration
}

type fakeExecResult struct {
	exitCode int
	stderr   string
	err      error
}

type fakeExecCall struct {
	bin  string
	args []string
}

func (f *fakeExecRunner) run(ctx context.Context, bin string, args []string) (int, string, error) {
	f.calls = append(f.calls, fakeExecCall{bin: bin, args: append([]string(nil), args...)})
	if f.blockUntilCancel {
		<-ctx.Done()
		return -1, "", ctx.Err()
	}
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return -1, "", ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if len(f.results) == 0 {
		return 0, "", nil
	}
	r := f.results[f.idx]
	if f.idx < len(f.results)-1 {
		f.idx++
	}
	return r.exitCode, r.stderr, r.err
}

// TestExecArbiter_Gaming_CommandConstruction verifies the exact argv built for
// `gpu-arbiter status -q --url <base>` and the exit-code → (gaming, ok) mapping
// (cli::quiet_exit_code / run_status in gpu-arbiter's main.rs).
func TestExecArbiter_Gaming_CommandConstruction(t *testing.T) {
	cases := []struct {
		name       string
		result     fakeExecResult
		wantGaming bool
		wantOK     bool
	}{
		{"available", fakeExecResult{exitCode: 0}, false, true},
		{"claimed (gaming/evicting)", fakeExecResult{exitCode: 1}, true, true},
		{"unreachable", fakeExecResult{exitCode: 2}, false, false},
		{"unexpected exit code", fakeExecResult{exitCode: 7}, false, false},
		{"exec failed to start", fakeExecResult{exitCode: -1, err: errors.New("exec: \"gpu-arbiter\": not found")}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeExecRunner{results: []fakeExecResult{tc.result}}
			a := &execArbiter{
				bin:          "gpu-arbiter",
				baseURL:      "http://gpu-host:48750",
				waitTimeout:  time.Second,
				probeTimeout: time.Second,
				runner:       runner,
			}
			gaming, ok := a.Gaming(context.Background())
			if gaming != tc.wantGaming || ok != tc.wantOK {
				t.Errorf("Gaming() = (%v, %v), want (%v, %v)", gaming, ok, tc.wantGaming, tc.wantOK)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("expected exactly 1 exec call, got %d", len(runner.calls))
			}
			call := runner.calls[0]
			if call.bin != "gpu-arbiter" {
				t.Errorf("bin = %q, want %q", call.bin, "gpu-arbiter")
			}
			wantArgs := []string{"status", "-q", "--url", "http://gpu-host:48750"}
			if !equalStrings(call.args, wantArgs) {
				t.Errorf("args = %v, want %v", call.args, wantArgs)
			}
		})
	}
}

// TestExecArbiter_Gaming_Unconfigured: an empty baseURL must be a no-op (no
// exec at all) returning ok=false, mirroring httpArbiter's empty-URL behavior.
func TestExecArbiter_Gaming_Unconfigured(t *testing.T) {
	runner := &fakeExecRunner{}
	a := &execArbiter{bin: "gpu-arbiter", baseURL: "", waitTimeout: time.Second, probeTimeout: time.Second, runner: runner}
	if gaming, ok := a.Gaming(context.Background()); gaming || ok {
		t.Errorf("Gaming() = (%v, %v), want (false, false)", gaming, ok)
	}
	if len(runner.calls) != 0 {
		t.Errorf("expected no exec calls for an unconfigured arbiter, got %d", len(runner.calls))
	}
}

// TestExecArbiter_Wait_CommandConstruction verifies the exact argv built for
// `gpu-arbiter wait --for available --timeout <N> --url <base>`.
func TestExecArbiter_Wait_CommandConstruction(t *testing.T) {
	runner := &fakeExecRunner{results: []fakeExecResult{{exitCode: 0}}}
	a := &execArbiter{
		bin:          "/usr/local/bin/gpu-arbiter",
		baseURL:      "http://gpu-host:48750",
		waitTimeout:  15 * time.Second,
		probeTimeout: 3 * time.Second,
		runner:       runner,
	}
	if err := a.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly 1 exec call, got %d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.bin != "/usr/local/bin/gpu-arbiter" {
		t.Errorf("bin = %q, want %q", call.bin, "/usr/local/bin/gpu-arbiter")
	}
	wantArgs := []string{"wait", "--for", "available", "--timeout", "15", "--url", "http://gpu-host:48750"}
	if !equalStrings(call.args, wantArgs) {
		t.Errorf("args = %v, want %v", call.args, wantArgs)
	}
}

// TestExecArbiter_Wait_TimeoutOrUnreachableNeverErrors: per the type doc
// comment, Wait must NOT surface a non-zero exit (timeout OR unreachable) as
// an error — the caller's next Gaming() call is what classifies the outcome.
// Only a genuine ctx cancellation should produce an error.
func TestExecArbiter_Wait_TimeoutOrUnreachableNeverErrors(t *testing.T) {
	cases := []fakeExecResult{
		{exitCode: 1, stderr: "ERROR: timed out after 15s waiting for state to reach Available"},
		{exitCode: 1, stderr: "ERROR: querying http://gpu-host:48750/status: connect error\nIs the gpu-arbiter daemon running?"},
	}
	for _, tc := range cases {
		runner := &fakeExecRunner{results: []fakeExecResult{tc}}
		a := &execArbiter{bin: "gpu-arbiter", baseURL: "http://gpu-host:48750", waitTimeout: time.Millisecond, probeTimeout: time.Millisecond, runner: runner}
		if err := a.Wait(context.Background()); err != nil {
			t.Errorf("Wait() with exit %d returned error %v, want nil", tc.exitCode, err)
		}
	}
}

// TestExecArbiter_Wait_PropagatesCancellation ensures a cancelled ctx unwinds
// Wait with a non-nil error (mirroring sleep()'s contract), even though the
// exec'd child is simulated as long-running (killed only by ctx cancellation —
// standing in for exec.CommandContext's real kill-on-cancel behavior).
func TestExecArbiter_Wait_PropagatesCancellation(t *testing.T) {
	runner := &fakeExecRunner{blockUntilCancel: true}
	a := &execArbiter{bin: "gpu-arbiter", baseURL: "http://gpu-host:48750", waitTimeout: time.Hour, probeTimeout: time.Hour, runner: runner}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Wait(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Wait() after ctx cancellation = nil error, want non-nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after ctx cancellation")
	}
}

// TestExecArbiter_Wait_FastFailureFallsBackToFloorSleep guards against a tight
// subprocess-spawn loop: a non-network failure that returns near-instantly
// (well under waitTimeout) must still consume roughly waitTimeout before
// returning, so a persistent bug can't hammer exec() in a hot loop.
func TestExecArbiter_Wait_FastFailureFallsBackToFloorSleep(t *testing.T) {
	runner := &fakeExecRunner{results: []fakeExecResult{{exitCode: -1, err: errors.New("exec: \"gpu-arbiter\": not found")}}}
	a := &execArbiter{bin: "gpu-arbiter", baseURL: "http://gpu-host:48750", waitTimeout: 50 * time.Millisecond, probeTimeout: time.Second, runner: runner}

	start := time.Now()
	if err := a.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("Wait() returned after %s, expected it to floor-sleep out roughly waitTimeout (50ms)", elapsed)
	}
}

// TestExecArbiter_Wait_SlowFailureAlsoFloorSleeps closes the gap the PR review
// flagged: a SLOW failure — one that consumes more than half of waitTimeout
// but returns before waitTimeout has fully elapsed (e.g. a child killed by a
// mid-window network error) — must ALSO sleep out the remainder. The invariant
// is that every non-success Wait() consumes >= waitTimeout, regardless of how
// long the child itself took, so no failure latency can produce a tight
// subprocess-spawn loop.
func TestExecArbiter_Wait_SlowFailureAlsoFloorSleeps(t *testing.T) {
	const waitTimeout = 60 * time.Millisecond
	runner := &fakeExecRunner{
		// 40ms > waitTimeout/2 (30ms): under the old waitTimeout/2 guard this
		// returned immediately after the child; now it must sleep the rest.
		delay:   40 * time.Millisecond,
		results: []fakeExecResult{{exitCode: 1, stderr: "ERROR: querying /status: connection reset"}},
	}
	a := &execArbiter{bin: "gpu-arbiter", baseURL: "http://gpu-host:48750", waitTimeout: waitTimeout, probeTimeout: time.Second, runner: runner}

	start := time.Now()
	if err := a.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < waitTimeout-10*time.Millisecond {
		t.Errorf("Wait() returned after %s, expected a slow failure to still consume >= waitTimeout (%s)", elapsed, waitTimeout)
	}
}

// TestExecArbiter_Wait_ChildBoundedByWaitTimeout verifies the child's context
// deadline is waitTimeout itself (not waitTimeout+probeTimeout): a wedged
// child that only exits on context kill must be reaped at ~waitTimeout, and a
// Wait() against it must not run anywhere near waitTimeout+probeTimeout.
func TestExecArbiter_Wait_ChildBoundedByWaitTimeout(t *testing.T) {
	runner := &fakeExecRunner{blockUntilCancel: true} // wedged child: exits only on ctx kill
	a := &execArbiter{
		bin: "gpu-arbiter", baseURL: "http://gpu-host:48750",
		waitTimeout:  50 * time.Millisecond,
		probeTimeout: time.Hour, // must NOT extend the child's deadline
		runner:       runner,
	}

	start := time.Now()
	if err := a.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Errorf("Wait() returned after %s, want ~waitTimeout (50ms)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Wait() took %s — the wedged child was not bounded by waitTimeout", elapsed)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
