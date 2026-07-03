// Package batch holds the dependency-injected core of the `earmark batch`
// coordinator (CONTRACT §1.4). It drives runner_control.phase + run_limit and
// reads queue status to run the pipeline in transcribe→analyze batches so the
// ASR model and the eval judge time-share one GPU.
//
// Everything the core needs is an interface (PhaseStore for the DB, Arbiter for
// gpu-arbiter), so the phase-transition logic is unit-testable with fakes — no
// DB, no ASR runner, no HTTP. The coordinator is hardware-agnostic: it only
// flips phases and reads status; it knows nothing about CUDA.
package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// actor is recorded in runner_control.updated_by for the coordinator's writes.
const actor = "batch"

// PhaseStore is the slice of *db.DB the coordinator drives. *db.DB satisfies it.
//
// Writes: SetPipelinePhase (the transcribe/analyze/idle selector) and
// SetRunLimit (the bounded-run counter the ASR runner decrements). Reads:
// GetPipelinePhase (for resume reconciliation) and GetServiceStatus (the queue
// snapshot used to decide when a phase is complete).
type PhaseStore interface {
	GetPipelinePhase(ctx context.Context) (string, error)
	SetPipelinePhase(ctx context.Context, phase, by string) error
	SetRunLimit(ctx context.Context, limit *int, by string) error
	GetServiceStatus(ctx context.Context) (*db.QueueStats, error)
}

// Arbiter reports gpu-arbiter readiness. The coordinator only ever reads it
// (GET /status) — it never tells the arbiter to do anything.
type Arbiter interface {
	// Gaming reports whether the GPU is currently busy with a game — actively
	// gaming OR mid-eviction (a game just launched and the arbiter is tearing
	// down GPU tenants). ok=false means the arbiter was unreachable / not
	// configured (the caller then proceeds, degrading gracefully — arbiter
	// absence must never wedge the coordinator).
	Gaming(ctx context.Context) (gaming bool, ok bool)
}

// Waiter is an OPTIONAL capability an Arbiter may additionally implement
// (execArbiter does; httpArbiter does not). When present, yieldToGames uses it
// instead of its own sleep-and-repoll step: Wait blocks for roughly one
// "attempt" at the GPU becoming available (its own internal bound — e.g. the
// gpu-arbiter CLI's `wait --timeout`), then returns. The caller's very next
// Gaming() check independently re-derives the real state (busy vs available vs
// unreachable), so Wait itself does NOT need to distinguish "still gaming" from
// "arbiter unreachable" — both simply resolve on the next Gaming() call. This
// is what lets `earmark batch` delegate to `gpu-arbiter wait` (hardening plan
// #18) while keeping the exact same observable poll-and-yield behavior as the
// hand-rolled Go loop.
//
// Wait returns a non-nil error ONLY when ctx is done, so a cancelled batch run
// unwinds exactly like the sleep-based poll loop does (propagated up through
// yieldToGames → Run's deferred restore-idle).
type Waiter interface {
	Wait(ctx context.Context) error
}

// EventSink records pipeline_events for the coordinator (CONTRACT §1.7). It is a
// seam so the coordinator stays unit-testable: tests inject a recording/no-op
// sink. *db.DB satisfies AppendEvent; the coordinator wraps it. Best-effort —
// the sink must log-and-continue on error, never wedge the coordinator.
type EventSink interface {
	AppendEvent(ctx context.Context, e db.PipelineEvent) error
}

// nopSink discards events. Used as the default when no sink is injected so the
// coordinator core never needs a nil check.
type nopSink struct{}

func (nopSink) AppendEvent(context.Context, db.PipelineEvent) error { return nil }

// availabilityEmitter emits a runner_availability event ONLY on a state change
// (debounced), so a steady-state poll produces no rows. The three states map the
// arbiter's (gaming, ok) result: ok=false → "unreachable", gaming=true →
// "gaming", else → "idle" (GPU free for transcription).
type availabilityEmitter struct {
	sink EventSink
	last string // last-emitted reason; "" until the first emit
	p    func(string, ...any)
}

// arbiterReason maps the arbiter result to a runner_availability reason token.
func arbiterReason(gaming, ok bool) (reason string, available bool) {
	switch {
	case !ok:
		return "unreachable", false
	case gaming:
		return "gaming", false
	default:
		return "idle", true
	}
}

// observe records the current arbiter state and emits a runner_availability
// event if (and only if) it changed since the last emit. Best-effort.
func (e *availabilityEmitter) observe(ctx context.Context, gaming, ok bool) {
	reason, available := arbiterReason(gaming, ok)
	if reason == e.last {
		return
	}
	e.last = reason
	if err := e.sink.AppendEvent(ctx, db.PipelineEvent{
		Stage:      db.StageRunnerAvailability,
		Event:      db.EventState,
		RunnerHost: db.HostGoMonitor,
		Reason:     reason,
		Detail:     map[string]any{"available": available, "reason": reason},
	}); err != nil && e.p != nil {
		e.p("WARNING: runner_availability event write failed: %v\n", err)
	}
}

// Options configures a coordinator run.
type Options struct {
	// BatchSize is the run_limit set for each Phase A (jobs transcribed per
	// batch). Must be >= 1.
	BatchSize int
	// MaxBatches stops the loop after N batches. 0 = run until the queue drains.
	MaxBatches int
	// PollInterval is how often DB state is polled to detect phase completion.
	PollInterval time.Duration
	// ArbiterPoll is how often gpu-arbiter is re-checked while it reports gaming.
	ArbiterPoll time.Duration
	// Sink records runner_availability events on arbiter-state transitions
	// (CONTRACT §1.7). nil → a no-op sink (events disabled). Best-effort.
	Sink EventSink
	// EvalGatesEmbed mirrors cfg.EvalGatesEmbed: when true Phase B completion
	// requires BOTH the eval backlog AND the embed backlog to reach 0. When
	// false (default) Phase B completes when only the embed backlog reaches 0,
	// preserving today's behavior. CONTRACT §1.4, §2.4.
	EvalGatesEmbed bool
}

const (
	defaultPollInterval = 10 * time.Second
	defaultArbiterPoll  = 15 * time.Second
)

func (o *Options) normalize() error {
	if o.BatchSize < 1 {
		return fmt.Errorf("batch size must be >= 1, got %d", o.BatchSize)
	}
	// 0 means "until the queue drains"; a negative value would make the batch
	// loop's `batchNum <= MaxBatches` guard never enter, silently skipping all
	// work — reject it as a user mistake rather than no-op.
	if o.MaxBatches < 0 {
		return fmt.Errorf("max batches must be >= 0, got %d", o.MaxBatches)
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.ArbiterPoll <= 0 {
		o.ArbiterPoll = defaultArbiterPoll
	}
	if o.Sink == nil {
		o.Sink = nopSink{}
	}
	return nil
}

// Run executes the batch coordinator until the queue drains, MaxBatches is hit,
// or the run is cancelled (SIGINT/SIGTERM, or the parent ctx). It ALWAYS
// restores the pipeline to idle and clears the run budget on exit — normal
// completion, error, or cancel — via a deferred restore, so the system never
// gets stuck mid-phase.
//
// The SIGINT/SIGTERM handler is installed *inside* Run (not by the caller) so
// that the signal-aware scope strictly encloses the restore-idle defer: there
// is no window where a signal could arrive after the handler is torn down but
// before the restore runs. The parent ctx is still honored — cancelling it (or
// using a test context) unwinds the loop the same way a signal does.
func Run(ctx context.Context, out io.Writer, store PhaseStore, arb Arbiter, o Options) (err error) {
	if nerr := o.normalize(); nerr != nil {
		return nerr
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	// Install the signal handler first, then defer the restore. Defers run
	// LIFO, so on return the restore-idle cleanup runs BEFORE stop() releases
	// the signal handler — the handler stays armed across the entire cleanup,
	// closing the "signal at return time" race.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = sigCtx

	// Robustness: restore idle + clear budget on EVERY exit path. Uses
	// context.Background() (not ctx) so the cleanup still runs after a
	// cancellation — the cancelled ctx would reject the writes otherwise.
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rerr := restoreIdle(restoreCtx, store); rerr != nil {
			p("WARNING: failed to restore idle phase on exit: %v\n", rerr)
			if err == nil {
				err = fmt.Errorf("restore idle on exit: %w", rerr)
			}
			return
		}
		p("Restored pipeline to idle (run budget set to 0).\n")
	}()

	// Resume reconciliation: if a previous run died mid-batch in analyze, finish
	// Phase B before starting a new Phase A (CONTRACT §1.4 resumable contract).
	phase, err := store.GetPipelinePhase(ctx)
	if err != nil {
		return fmt.Errorf("read initial phase: %w", err)
	}
	if phase == db.PhaseAnalyze {
		p("Resuming: found phase=analyze; finishing the in-flight analyze batch first.\n")
		// Clear any residual run budget left by a crash mid-Phase-A to 0. In the
		// analyze phase the runner is off-GPU so a lingering run_limit is inert,
		// but resetting it now keeps the resumed state clean and upholds the
		// invariant that the coordinator never leaves run_limit NULL (=unlimited).
		zero := 0
		if err := store.SetRunLimit(ctx, &zero, actor); err != nil {
			return fmt.Errorf("clear run limit (resume): %w", err)
		}
		// Re-assert analyze so the resume path and a normal Phase B share one
		// setter call each (keeps the transition log clean) before the wait loop.
		if err := store.SetPipelinePhase(ctx, db.PhaseAnalyze, actor); err != nil {
			return fmt.Errorf("set analyze phase (resume): %w", err)
		}
		if err := waitForAnalyzeDrained(ctx, p, store, o.PollInterval, o.EvalGatesEmbed); err != nil {
			return err
		}
	}

	// arbiterWarned guards the one-time "arbiter unavailable" notice across all
	// batches in this run (passed by pointer into yieldToGames).
	arbiterWarned := false

	// availability emitter: records a runner_availability event only when the
	// arbiter state changes (debounced across the whole run), so the coordinator's
	// gpu-arbiter poll becomes the source of the availability windows used for the
	// calendar ETA (CONTRACT §1.7, §4.3).
	avail := &availabilityEmitter{sink: o.Sink, p: p}

	for batchNum := 1; o.MaxBatches == 0 || batchNum <= o.MaxBatches; batchNum++ {
		// Is there anything left to transcribe?
		st, err := store.GetServiceStatus(ctx)
		if err != nil {
			return fmt.Errorf("read status before batch %d: %w", batchNum, err)
		}
		if st.Pending == 0 && st.Claimed == 0 {
			p("No pending transcription jobs remain — done after %d batch(es).\n", batchNum-1)
			return nil
		}

		p("=== Batch %d (pending=%d) ===\n", batchNum, st.Pending)

		// 1. Yield to games before any GPU work.
		if err := yieldToGames(ctx, p, arb, o.ArbiterPoll, &arbiterWarned, avail); err != nil {
			return err
		}

		// 2. Phase A: transcribe up to BatchSize jobs.
		if err := runTranscribePhase(ctx, p, store, o); err != nil {
			return err
		}

		// 3. Phase B: analyze (embed + inline eval) the just-transcribed batch.
		if err := runAnalyzePhase(ctx, p, store, o); err != nil {
			return err
		}
	}

	p("Reached --max-batches=%d; stopping.\n", o.MaxBatches)
	return nil
}

// yieldToGames blocks while gpu-arbiter reports the GPU is busy with a game
// (gaming or evicting; see arbiter.go), polling every poll interval. An
// unreachable/unset arbiter (ok=false) returns immediately — the coordinator
// proceeds, degrading gracefully. arbiterWarned (owned by Run) ensures the
// "arbiter unavailable" notice is logged at most once per run, not per batch.
//
// When arb also implements Waiter (the exec-delegated gpu-arbiter CLI path —
// see arbiter_exec.go), the "wait for the next poll to be worth checking" step
// is delegated to it instead of a Go-side sleep: Wait blocks roughly one poll
// interval's worth of time, then the loop goes right back to the top and lets
// the ordinary Gaming() check re-derive the state (busy / available /
// unreachable) exactly as it would have after a plain sleep. Every other
// behavior — messaging, the availability-event emission, the degrade-once
// notice — is unchanged.
func yieldToGames(ctx context.Context, p func(string, ...any), arb Arbiter, poll time.Duration, arbiterWarned *bool, avail *availabilityEmitter) error {
	waiter, delegated := arb.(Waiter)
	announced := false
	for {
		busy, ok := arb.Gaming(ctx)
		// Record the availability transition (debounced) before acting on it.
		avail.observe(ctx, busy, ok)
		if !ok {
			// Unconfigured or unreachable arbiter: degrade gracefully (proceed
			// without game-yield). Log once for the whole run (the flag is owned
			// by Run) so the operator knows GPU contention isn't being guarded
			// against, without spamming every batch's yield check.
			if !*arbiterWarned {
				p("gpu-arbiter unreachable or unconfigured — proceeding without game-yield (GPU contention not guarded).\n")
				*arbiterWarned = true
			}
			return nil
		}
		if !busy {
			if announced {
				p("gpu-arbiter GPU free again — resuming.\n")
			}
			return nil
		}
		if !announced {
			p("gpu-arbiter reports the GPU busy with a game — waiting (polling every %s)...\n", poll)
			announced = true
		}
		if delegated {
			if err := waiter.Wait(ctx); err != nil {
				return err
			}
			continue
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// runTranscribePhase sets phase=transcribe + run_limit=BatchSize, then waits
// until the batch is transcribed: no jobs actively claimed AND the run budget is
// exhausted (run_limit==0) or no pending jobs remain. The runner claims up to N
// then stops; the embed worker idles for the duration.
func runTranscribePhase(ctx context.Context, p func(string, ...any), store PhaseStore, o Options) error {
	if err := store.SetPipelinePhase(ctx, db.PhaseTranscribe, actor); err != nil {
		return fmt.Errorf("set transcribe phase: %w", err)
	}
	n := o.BatchSize
	if err := store.SetRunLimit(ctx, &n, actor); err != nil {
		return fmt.Errorf("set run limit %d: %w", n, err)
	}
	p("Phase A (transcribe): run_limit=%d — waiting for the batch to transcribe...\n", n)

	for {
		st, err := store.GetServiceStatus(ctx)
		if err != nil {
			return fmt.Errorf("poll transcribe status: %w", err)
		}
		if transcribeDone(st) {
			p("Phase A complete (pending=%d, claimed=%d, run_limit reached).\n", st.Pending, st.Claimed)
			return nil
		}
		if err := sleep(ctx, o.PollInterval); err != nil {
			return err
		}
	}
}

// transcribeDone reports whether a transcribe batch has finished. The predicate
// is: nothing is actively claimed AND (the run budget is spent OR there is no
// pending work left).
//
//   - nothingClaimed: requiring Claimed==0 prevents declaring "done" while a job
//     is still mid-transcription.
//   - runBudgetSpent: run_limit reached 0 — the runner claimed its N and stopped.
//   - queueEmpty: no pending jobs remain — the batch drained the queue early.
func transcribeDone(st *db.QueueStats) bool {
	nothingClaimed := st.Claimed == 0
	runBudgetSpent := st.RunLimit != nil && *st.RunLimit == 0
	queueEmpty := st.Pending == 0
	return nothingClaimed && (runBudgetSpent || queueEmpty)
}

// runAnalyzePhase sets phase=analyze (the runner parks its model, freeing the
// GPU; the embed worker drains the just-transcribed transcripts) and waits for
// the backlogs to reach 0. Under EVAL_GATES_EMBED both the eval backlog and the
// embed backlog must reach 0 (CONTRACT §1.4); otherwise only the embed backlog.
func runAnalyzePhase(ctx context.Context, p func(string, ...any), store PhaseStore, o Options) error {
	if err := store.SetPipelinePhase(ctx, db.PhaseAnalyze, actor); err != nil {
		return fmt.Errorf("set analyze phase: %w", err)
	}
	if o.EvalGatesEmbed {
		p("Phase B (analyze): waiting for the eval backlog and embed backlog to drain (EVAL_GATES_EMBED)...\n")
	} else {
		p("Phase B (analyze): waiting for the embed backlog to drain...\n")
	}
	return waitForAnalyzeDrained(ctx, p, store, o.PollInterval, o.EvalGatesEmbed)
}

// waitForAnalyzeDrained polls until the relevant backlogs reach 0. When
// evalGatesEmbed is true, BOTH EvalBacklog and EmbedBacklog must be 0 (the eval
// pass must finish before the embed pass can pick anything up). When false, only
// EmbedBacklog must be 0 (today's behavior). Used both by Phase B and by resume
// reconciliation. The caller is responsible for having set phase=analyze.
func waitForAnalyzeDrained(ctx context.Context, p func(string, ...any), store PhaseStore, poll time.Duration, evalGatesEmbed bool) error {
	for {
		st, err := store.GetServiceStatus(ctx)
		if err != nil {
			return fmt.Errorf("poll analyze status: %w", err)
		}
		if analyzeDrained(st, evalGatesEmbed) {
			if evalGatesEmbed {
				p("Phase B complete (eval backlog and embed backlog drained).\n")
			} else {
				p("Phase B complete (embed backlog drained).\n")
			}
			return nil
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// analyzeDrained reports whether the Phase B completion condition is satisfied.
// Under the gate: both eval and embed backlogs must be 0.
// Without the gate: only the embed backlog must be 0 (today's behavior).
func analyzeDrained(st *db.QueueStats, evalGatesEmbed bool) bool {
	if evalGatesEmbed {
		return st.EvalBacklog == 0 && st.EmbedBacklog == 0
	}
	return st.EmbedBacklog == 0
}

// restoreIdle returns the pipeline to a SAFE idle resting state: phase=idle and
// run_limit=0. Called from the deferred cleanup on every exit path.
//
// run_limit is set to 0 (NOT cleared to NULL): the runner's claim gate treats
// NULL as *unlimited*, so leaving it NULL would let the runner drain the entire
// backlog the moment it isn't paused — the opposite of the batched model.
// run_limit=0 leaves the runner idle-but-armed: it claims nothing until the next
// batch sets a budget, and never runs unbounded between batches.
func restoreIdle(ctx context.Context, store PhaseStore) error {
	var errs []error
	if err := store.SetPipelinePhase(ctx, db.PhaseIdle, actor); err != nil {
		errs = append(errs, fmt.Errorf("clear phase: %w", err))
	}
	zero := 0
	if err := store.SetRunLimit(ctx, &zero, actor); err != nil {
		errs = append(errs, fmt.Errorf("set run limit to 0: %w", err))
	}
	return errors.Join(errs...)
}

// sleep waits d or returns ctx.Err() if the context is cancelled first. Lets a
// SIGINT unwind a poll/yield loop promptly instead of blocking a full interval.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
