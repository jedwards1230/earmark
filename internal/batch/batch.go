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
}

const (
	defaultPollInterval = 10 * time.Second
	defaultArbiterPoll  = 15 * time.Second
)

func (o *Options) normalize() error {
	if o.BatchSize < 1 {
		return fmt.Errorf("batch size must be >= 1, got %d", o.BatchSize)
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.ArbiterPoll <= 0 {
		o.ArbiterPoll = defaultArbiterPoll
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
		p("Restored pipeline to idle (run budget cleared).\n")
	}()

	// Resume reconciliation: if a previous run died mid-batch in analyze, finish
	// Phase B before starting a new Phase A (CONTRACT §1.4 resumable contract).
	phase, err := store.GetPipelinePhase(ctx)
	if err != nil {
		return fmt.Errorf("read initial phase: %w", err)
	}
	if phase == db.PhaseAnalyze {
		p("Resuming: found phase=analyze; finishing the in-flight analyze batch first.\n")
		// Re-assert analyze so the resume path and a normal Phase B share one
		// setter call each (keeps the transition log clean) before the wait loop.
		if err := store.SetPipelinePhase(ctx, db.PhaseAnalyze, actor); err != nil {
			return fmt.Errorf("set analyze phase (resume): %w", err)
		}
		if err := waitForAnalyzeDrained(ctx, p, store, o.PollInterval); err != nil {
			return err
		}
	}

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
		if err := yieldToGames(ctx, p, arb, o.ArbiterPoll); err != nil {
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
// proceeds, degrading gracefully.
func yieldToGames(ctx context.Context, p func(string, ...any), arb Arbiter, poll time.Duration) error {
	announced := false
	for {
		busy, ok := arb.Gaming(ctx)
		if !ok || !busy {
			if announced {
				p("gpu-arbiter GPU free again — resuming.\n")
			}
			return nil
		}
		if !announced {
			p("gpu-arbiter reports the GPU busy with a game — waiting (polling every %s)...\n", poll)
			announced = true
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

// transcribeDone reports whether a transcribe batch has finished: nothing is
// actively being claimed, AND either the run budget is exhausted (the runner
// claimed its N and stopped) or there is no pending work left. Requiring
// Claimed==0 prevents declaring "done" while a job is still mid-transcription.
func transcribeDone(st *db.QueueStats) bool {
	if st.Claimed > 0 {
		return false
	}
	budgetExhausted := st.RunLimit != nil && *st.RunLimit == 0
	return budgetExhausted || st.Pending == 0
}

// runAnalyzePhase sets phase=analyze (the runner parks its model, freeing the
// GPU; the embed worker drains the just-transcribed transcripts) and waits for
// the embed backlog to reach 0.
func runAnalyzePhase(ctx context.Context, p func(string, ...any), store PhaseStore, o Options) error {
	if err := store.SetPipelinePhase(ctx, db.PhaseAnalyze, actor); err != nil {
		return fmt.Errorf("set analyze phase: %w", err)
	}
	p("Phase B (analyze): waiting for the embed backlog to drain...\n")
	return waitForAnalyzeDrained(ctx, p, store, o.PollInterval)
}

// waitForAnalyzeDrained polls until EmbedBacklog (completed transcripts with no
// chunks) reaches 0. Used both by Phase B and by resume reconciliation. The
// caller is responsible for having set phase=analyze before calling this.
func waitForAnalyzeDrained(ctx context.Context, p func(string, ...any), store PhaseStore, poll time.Duration) error {
	for {
		st, err := store.GetServiceStatus(ctx)
		if err != nil {
			return fmt.Errorf("poll analyze status: %w", err)
		}
		if st.EmbedBacklog == 0 {
			p("Phase B complete (embed backlog drained).\n")
			return nil
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// restoreIdle returns the pipeline to normal continuous mode: phase=idle and
// run_limit cleared. Called from the deferred cleanup on every exit path.
func restoreIdle(ctx context.Context, store PhaseStore) error {
	var errs []error
	if err := store.SetPipelinePhase(ctx, db.PhaseIdle, actor); err != nil {
		errs = append(errs, fmt.Errorf("clear phase: %w", err))
	}
	if err := store.SetRunLimit(ctx, nil, actor); err != nil {
		errs = append(errs, fmt.Errorf("clear run limit: %w", err))
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
