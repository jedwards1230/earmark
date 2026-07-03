package batch

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// execArbiter delegates gpu-arbiter readiness checks to the gpu-arbiter CLI
// (https://github.com/jedwards1230/gpu-arbiter, `wait`/`status` subcommands,
// hardening plan #18) instead of hand-rolling an HTTP GET + JSON decode in Go.
// It is used ONLY when a gpu-arbiter binary is actually available (resolved by
// the caller — see cmd/batch's PATH lookup / --arbiter-wait-cmd override); the
// coordinator otherwise falls back to httpArbiter unchanged (arbiter.go). This
// keeps `earmark batch` free of a hard dependency on the gpu-arbiter binary —
// it is a strict optimization when the binary happens to be present (e.g. on
// the same desktop-1 host gpu-arbiter itself runs on), never a requirement.
//
// Both subcommands this type shells out to are, like the coordinator's own
// contract (CONTRACT §1.4), strictly read-only GETs against gpu-arbiter's
// `/status` endpoint — the CLI never POSTs.
//
//   - Gaming (the Arbiter interface) runs `gpu-arbiter status -q --url <base>`.
//     Its quiet exit codes are unambiguous: 0 = available, 1 = claimed
//     (gaming/evicting), 2 = daemon unreachable — so no output parsing is
//     needed, unlike the `wait` subcommand below.
//   - Wait (the optional Waiter interface, see batch.go) runs
//     `gpu-arbiter wait --for available --timeout <N> --url <base>` and
//     blocks for up to one polling interval. Its exit code does NOT
//     distinguish "timed out, still gaming" from "daemon unreachable" (both
//     are 1) — but Wait doesn't need to: the caller's very next Gaming() call
//     re-derives the real state via the unambiguous `status -q` exit codes
//     above, so Wait simply reports whether it had to fall back to a floor
//     delay (see run) and otherwise ignores its own exit code entirely.
type execArbiter struct {
	// bin is the resolved gpu-arbiter executable (an absolute path or a bare
	// name resolved against PATH by the caller).
	bin string
	// baseURL is gpu-arbiter's daemon base URL — i.e. GPU_ARBITER_URL with any
	// trailing "/status" stripped, since the CLI's --url expects the base and
	// appends "/status" itself (unlike GPU_ARBITER_URL, which is the full
	// status URL — see arbiterBaseURL).
	baseURL string
	// waitTimeout is passed as `wait --timeout <seconds>`; matches the
	// coordinator's --arbiter-poll cadence so a delegated wait attempt takes
	// about as long as a single Go-side poll would have.
	waitTimeout time.Duration
	// probeTimeout bounds each exec'd subprocess via a context deadline — a
	// safety net in case the child hangs despite its own internal timeout
	// (e.g. a wedged network call). Mirrors httpArbiter's request timeout.
	probeTimeout time.Duration
	runner       execRunner
}

// execRunner abstracts subprocess execution so tests can fake `gpu-arbiter`
// without executing a real binary. The real implementation runs via
// exec.CommandContext, so ctx cancellation kills the child.
type execRunner interface {
	// run executes bin with args, honoring ctx (killing the child on
	// cancellation). It returns the process exit code (or -1 if the process
	// could not even be started — missing binary, permission error, etc., in
	// which case err is non-nil) and anything written to stderr.
	run(ctx context.Context, bin string, args []string) (exitCode int, stderr string, err error)
}

// realExecRunner shells out via os/exec.
type realExecRunner struct{}

func (realExecRunner) run(ctx context.Context, bin string, args []string) (int, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stderr.String(), nil
	}
	// Couldn't start it at all (binary missing/removed mid-run, permission
	// denied, ctx already cancelled before start, ...).
	return -1, stderr.String(), err
}

// NewExecArbiter builds an Arbiter (and Waiter) that delegates to the
// gpu-arbiter CLI at bin. statusURL is GPU_ARBITER_URL in its documented form
// (a full .../status URL, CONTRACT §2.4) — arbiterBaseURL strips the suffix
// gpu-arbiter's own --url flag expects. waitTimeout should match the
// coordinator's --arbiter-poll cadence; probeTimeout bounds each subprocess
// call (falls back to a sane default if <= 0, mirroring NewHTTPArbiter).
func NewExecArbiter(bin, statusURL string, waitTimeout, probeTimeout time.Duration) Arbiter {
	if probeTimeout <= 0 {
		probeTimeout = 3 * time.Second
	}
	if waitTimeout <= 0 {
		waitTimeout = defaultArbiterPoll
	}
	return &execArbiter{
		bin:          bin,
		baseURL:      arbiterBaseURL(statusURL),
		waitTimeout:  waitTimeout,
		probeTimeout: probeTimeout,
		runner:       realExecRunner{},
	}
}

// arbiterBaseURL derives gpu-arbiter's --url (a base URL) from GPU_ARBITER_URL
// (documented, CONTRACT §2.4, as the full /status URL — httpArbiter GETs it
// directly with no suffix appended). Trims exactly one trailing "/status" (and
// any trailing slash) if present; otherwise returns the value unchanged as a
// best-effort — a malformed value here just means the exec'd CLI call fails
// and Gaming/Wait report ok=false, same degrade-gracefully path as any other
// unreachable arbiter.
func arbiterBaseURL(statusURL string) string {
	u := strings.TrimSuffix(statusURL, "/")
	u = strings.TrimSuffix(u, "/status")
	return u
}

// quiet exit codes from `gpu-arbiter status -q` (cli::quiet_exit_code +
// run_status in gpu-arbiter's main.rs): 0 available, 1 claimed
// (gaming/evicting), 2 daemon unreachable.
const (
	statusExitAvailable   = 0
	statusExitClaimed     = 1
	statusExitUnreachable = 2
)

// Gaming implements Arbiter by running `gpu-arbiter status -q --url <base>`.
func (a *execArbiter) Gaming(ctx context.Context) (gaming bool, ok bool) {
	if a.baseURL == "" {
		return false, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, a.probeTimeout)
	defer cancel()
	exitCode, _, err := a.runner.run(probeCtx, a.bin, []string{"status", "-q", "--url", a.baseURL})
	if err != nil {
		return false, false
	}
	switch exitCode {
	case statusExitAvailable:
		return false, true
	case statusExitClaimed:
		return true, true
	default: // statusExitUnreachable, or any unexpected code
		return false, false
	}
}

// Wait implements the optional Waiter capability: it runs
// `gpu-arbiter wait --for available --timeout <N> --url <base>` for up to
// waitTimeout. It deliberately does NOT interpret the exit code beyond
// detecting a genuine ctx cancellation — see the type doc comment for why the
// caller's next Gaming() call is what actually classifies the outcome.
//
// A legitimate success (the GPU became available) always returns immediately.
// Any OTHER outcome (unreachable, a genuine internal timeout, an exec failure,
// a CLI/version mismatch, ...) that returns suspiciously fast — well under
// waitTimeout — instead sleeps out the remainder of waitTimeout before
// returning, so a persistent non-network failure (e.g. an argument/version
// mismatch that always exits immediately) can never turn into a tight
// subprocess-spawn loop against yieldToGames' Gaming()+Wait() cycle.
func (a *execArbiter) Wait(ctx context.Context) error {
	timeoutSecs := int64(a.waitTimeout.Seconds())
	if timeoutSecs < 1 {
		timeoutSecs = 1
	}
	waitCtx, cancel := context.WithTimeout(ctx, a.waitTimeout+a.probeTimeout)
	defer cancel()

	start := time.Now()
	exitCode, _, runErr := a.runner.run(waitCtx, a.bin, []string{
		"wait", "--for", "available",
		"--timeout", strconv.FormatInt(timeoutSecs, 10),
		"--url", a.baseURL,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runErr == nil && exitCode == 0 {
		// Became available — a legitimately fast success needs no guarding.
		return nil
	}
	if elapsed := time.Since(start); elapsed < a.waitTimeout/2 {
		return sleep(ctx, a.waitTimeout-elapsed)
	}
	return nil
}
