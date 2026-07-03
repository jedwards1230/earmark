package batch

import (
	"context"
	"errors"
	"io"
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
//     above, so beyond detecting success (exit 0) Wait ignores its own exit
//     code entirely and just guarantees a >= waitTimeout floor on failure.
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
	// probeTimeout bounds the quick Gaming() status probe via a context
	// deadline — a safety net in case the child hangs (e.g. a wedged network
	// call). Mirrors httpArbiter's request timeout. It does NOT apply to the
	// deliberately-long Wait() child, which is bounded by waitTimeout itself.
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
	// Discard stdout rather than inherit the parent's: the two subcommands we
	// run are quiet by design (`status -q` prints nothing; `wait` reports only
	// on stderr) and the exit code carries the whole answer — but a future CLI
	// version growing chatty must not pollute the coordinator's stdout logs.
	cmd.Stdout = io.Discard
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
// gpu-arbiter's own --url flag expects. If statusURL is empty, the result is a
// no-op arbiter: Gaming returns (false, false) with no subprocess execution,
// mirroring NewHTTPArbiter's empty-URL behavior, so an unconfigured arbiter
// never blocks the coordinator. waitTimeout should match the coordinator's
// --arbiter-poll cadence and bounds the Wait() child (both its CLI --timeout
// and its context deadline); probeTimeout bounds only the Gaming() status
// probe (both fall back to sane defaults if <= 0, mirroring NewHTTPArbiter).
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
// The child is bounded by a context deadline of exactly waitTimeout, matching
// the CLI's own --timeout: the CLI is expected to exit on its own at
// ~waitTimeout, and the context kill is only the backstop for a wedged child.
// probeTimeout plays no role here — it bounds the quick Gaming() status probe,
// never the deliberately-long wait.
//
// A legitimate success (the GPU became available) always returns immediately.
// EVERY other outcome (unreachable, a genuine internal timeout, an exec
// failure, a CLI/version mismatch, ...) that returns before waitTimeout has
// elapsed sleeps out the remainder before returning — the invariant is that a
// non-success Wait() always consumes >= waitTimeout, so a persistent failure
// of ANY latency (an instant exec error and a multi-second network failure
// alike) can never turn into a tight subprocess-spawn loop against
// yieldToGames' Gaming()+Wait() cycle.
func (a *execArbiter) Wait(ctx context.Context) error {
	timeoutSecs := int64(a.waitTimeout.Seconds())
	if timeoutSecs < 1 {
		timeoutSecs = 1
	}
	waitCtx, cancel := context.WithTimeout(ctx, a.waitTimeout)
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
	if elapsed := time.Since(start); elapsed < a.waitTimeout {
		return sleep(ctx, a.waitTimeout-elapsed)
	}
	return nil
}
