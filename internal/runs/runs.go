// Package runs records the live progress of an ad-hoc batch step — `earmark
// decide` (dry run, --yes, revert), `earmark scan`, `earmark eval` — in the
// step_runs table (CONTRACT §1.10), so the dashboard, GET /api/v1/runs and the
// earmark_step_run_* metrics can show what is running, what it is waiting on
// and how far it has got.
//
// The lifecycle is Start → any number of progress calls → Finish:
//
//	r := runs.Start(ctx, store, runs.Spec{Step: runs.StepDecide, Mode: db.StepRunWrite})
//	defer r.Finish(err) // or r.FinishStatus
//	r.Phase("asking jev-1.13.0")
//	r.Tick("apply", 0.0012)
//
// Two guarantees shape the implementation:
//
//   - Recording never fails the step. Every store error is logged (the first
//     one, then every 20th) and swallowed; a Run whose insert failed keeps
//     retrying it on later heartbeats. All methods are safe on a nil *Run, so
//     library code takes an optional recorder.
//   - Progress is throttled and off the hot path. Progress calls only update
//     memory under a mutex; one background goroutine writes at most once per
//     Interval (2 s), and at least once per Heartbeat (30 s) even when nothing
//     changed, so a run that is waiting a long time on a model call is not
//     mistaken for a dead one. Each write is its own short statement on the
//     pool, never inside a caller's transaction, bounded by WriteTimeout.
//
// A run whose process dies is never closed; readers call it stale once its
// heartbeat is older than db.StepRunStaleAfter (2 min) — computed at read
// time, so nothing has to reap it.
package runs

import (
	"context"
	"errors"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
)

// Step tokens recorded in step_runs.step. Bounded on purpose: they are a
// metric label.
const (
	StepDecide       = "decide"
	StepDecideRevert = "decide_revert"
	StepScan         = "scan"
	StepEvalSample   = "eval_sample"
	StepEvalBook     = "eval_book"
	StepEvalBackfill = "eval_backfill"
)

// Store is the slice of the database a Run writes (*db.DB implements it).
type Store interface {
	StartStepRun(ctx context.Context, s db.StepRunStart) (int64, error)
	UpdateStepRun(ctx context.Context, id int64, p db.StepRunProgress) error
	FinishStepRun(ctx context.Context, id int64, status string, p db.StepRunProgress, errText string) error
}

// Spec describes a run at Start.
type Spec struct {
	Step     string // a Step* token
	Mode     string // db.StepRunDryRun | db.StepRunWrite
	RecipeID string // may be set later with SetRecipe
	Model    string
	Args     string // a short human summary of the flags
	// Total is the item count when known up front; nil = unknown.
	Total *int64
}

// Options tunes the recorder. Zero values take the defaults.
type Options struct {
	// Interval is the minimum time between two progress writes (2 s).
	Interval time.Duration
	// Heartbeat is the maximum time between two writes while the run is open
	// (30 s), whether or not anything changed.
	Heartbeat time.Duration
	// WriteTimeout bounds each store call (5 s).
	WriteTimeout time.Duration
	// Host is recorded as step_runs.host (default os.Hostname — the pod name
	// in Kubernetes).
	Host string
	// Logger receives recording failures (default the "runs" logger).
	Logger log.Logger
}

// Defaults.
const (
	DefaultInterval     = 2 * time.Second
	DefaultHeartbeat    = 30 * time.Second
	DefaultWriteTimeout = 5 * time.Second
)

// logEvery is how often a repeating recording failure is logged.
const logEvery = 20

// Run is one recorded step run. Its methods are safe for concurrent use and
// on a nil *Run (no-ops).
type Run struct {
	store Store
	opts  Options
	start db.StepRunStart

	mu       sync.Mutex
	id       int64 // 0 until the insert succeeds
	state    db.StepRunProgress
	done     int64
	cost     float64
	counters map[string]int64
	dirty    bool
	closed   bool // Finish was called
	// abandoned: the row was closed or removed by someone else; stop writing.
	abandoned bool

	kick    chan struct{}
	quit    chan struct{}
	stopped chan struct{}

	// failures counts store errors; writes counts successful progress writes
	// (heartbeats). Read under mu.
	failures int
	writes   int
}

// Start records a new run and starts its heartbeat. It never fails: when
// store is nil it returns nil (every method is then a no-op), and when the
// insert fails the error is logged and retried on the next heartbeat.
func Start(ctx context.Context, store Store, spec Spec) *Run {
	return StartWith(ctx, store, spec, Options{})
}

// StartIfStore is Start when store implements Store and nil otherwise, so a
// command's testable core can take a narrow store interface and still record
// its run against the real database (*db.DB implements Store).
func StartIfStore(ctx context.Context, store any, spec Spec) *Run {
	s, ok := store.(Store)
	if !ok {
		return nil
	}
	return Start(ctx, s, spec)
}

// StartWith is Start with explicit Options.
func StartWith(ctx context.Context, store Store, spec Spec, o Options) *Run {
	if store == nil {
		return nil
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = DefaultHeartbeat
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	if o.Host == "" {
		o.Host, _ = os.Hostname()
	}
	if o.Logger.Logger == nil {
		o.Logger = log.NewLogger("runs")
	}
	r := &Run{
		store: store, opts: o,
		start: db.StepRunStart{
			Step: spec.Step, Mode: spec.Mode, RecipeID: spec.RecipeID, Model: spec.Model,
			Args: spec.Args, Host: o.Host, Total: spec.Total, Message: "starting",
		},
		state:    db.StepRunProgress{RecipeID: spec.RecipeID, Model: spec.Model, Total: spec.Total, Message: "starting"},
		counters: map[string]int64{},
		kick:     make(chan struct{}, 1),
		quit:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	// The insert happens synchronously so a run is visible the moment it
	// starts; a cancelled caller context does not stop it. Only the writer
	// goroutine (and Finish, after it stopped) writes afterwards, so store
	// calls never overlap.
	r.insert(context.WithoutCancel(ctx))
	go r.loop()
	return r
}

// ID is the step_runs id, 0 while the insert has not succeeded (or r is nil).
func (r *Run) ID() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id
}

// SetRecipe records the recipe and model once they are known (e.g. after the
// step registered its recipe). Empty values keep the current ones.
func (r *Run) SetRecipe(recipeID, model string) {
	r.update(func() {
		if recipeID != "" {
			r.state.RecipeID = recipeID
			r.start.RecipeID = recipeID
		}
		if model != "" {
			r.state.Model = model
			r.start.Model = model
		}
	})
}

// SetTotal records the item count; n < 0 means unknown.
func (r *Run) SetTotal(n int64) {
	r.update(func() {
		if n < 0 {
			r.state.Total = nil
		} else {
			r.state.Total = &n
		}
		r.start.Total = r.state.Total
	})
}

// Phase records what the run is doing now ("rung-0", "asking jev-1.13.0",
// "writing batch 3/12").
func (r *Run) Phase(msg string) {
	r.update(func() { r.state.Message = msg })
}

// Progress sets the absolute progress: items done, the outcome counters
// (copied; nil keeps the current ones), the spend so far and, when msg is not
// empty, the phase.
func (r *Run) Progress(done int64, counters map[string]int64, costUSD float64, msg string) {
	r.update(func() {
		r.done = max(done, 0)
		if counters != nil {
			r.counters = maps.Clone(counters)
		}
		if costUSD >= 0 {
			r.cost = costUSD
		}
		if msg != "" {
			r.state.Message = msg
		}
	})
}

// Tick counts one finished item: done+1, counters[outcome]+1 (when outcome
// is not empty) and costUSD added to the spend.
func (r *Run) Tick(outcome string, costUSD float64) {
	r.update(func() {
		r.done++
		if outcome != "" {
			r.counters[outcome]++
		}
		if costUSD > 0 {
			r.cost += costUSD
		}
	})
}

// Count adds n to counters[name] without counting an item done.
func (r *Run) Count(name string, n int64) {
	if name == "" || n == 0 {
		return
	}
	r.update(func() { r.counters[name] += n })
}

func (r *Run) update(f func()) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	f()
	r.dirty = true
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Status maps a step's error to the closing status: nil → done, a
// cancellation (SIGINT/SIGTERM) → cancelled, anything else → failed.
func Status(err error) string {
	switch {
	case err == nil:
		return db.StepRunDone
	case errors.Is(err, context.Canceled):
		return db.StepRunCancelled
	default:
		return db.StepRunFailed
	}
}

// Finish closes the run with Status(err) and err's text. Idempotent; safe on
// nil. It stops the heartbeat and writes the final state synchronously
// (bounded by WriteTimeout, not by any caller context, so an interrupted run
// is still recorded as cancelled). A failure is logged, never returned.
func (r *Run) Finish(err error) {
	text := ""
	if err != nil {
		text = err.Error()
	}
	r.FinishStatus(Status(err), text)
}

// FinishStatus is Finish with an explicit status and error text.
func (r *Run) FinishStatus(status, errText string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	close(r.quit)
	<-r.stopped // the writer has exited: this is now the only store caller

	r.mu.Lock()
	id, abandoned := r.id, r.abandoned
	r.mu.Unlock()
	if abandoned {
		return
	}
	if id == 0 {
		if id = r.insert(context.Background()); id == 0 {
			return
		}
	}
	r.mu.Lock()
	p := r.snapshotLocked()
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.WriteTimeout)
	defer cancel()
	if err := r.store.FinishStepRun(ctx, id, status, p, errText); err != nil {
		r.fail("finish", err)
	}
}

// Stats reports the successful heartbeat writes and the recording failures
// so far (for tests and diagnostics).
func (r *Run) Stats() (writes, failures int) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes, r.failures
}

// loop is the single writer: it waits for a change or the heartbeat tick,
// holds at least Interval between writes, and exits on quit.
func (r *Run) loop() {
	defer close(r.stopped)
	beat := time.NewTicker(r.opts.Heartbeat)
	defer beat.Stop()
	last := time.Now()
	for {
		select {
		case <-r.quit:
			return
		case <-r.kick:
		case <-beat.C:
			r.mu.Lock()
			r.dirty = true // a heartbeat is due even with nothing new
			r.mu.Unlock()
		}
		if wait := r.opts.Interval - time.Since(last); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-r.quit:
				t.Stop()
				return
			case <-t.C:
			}
		}
		r.flush()
		last = time.Now()
	}
}

// flush writes the current state if anything changed since the last write.
// The store call happens without the lock, so Tick and friends never wait on
// the database.
func (r *Run) flush() {
	r.mu.Lock()
	if !r.dirty || r.closed || r.abandoned {
		r.mu.Unlock()
		return
	}
	r.dirty = false
	id := r.id
	p := r.snapshotLocked()
	r.mu.Unlock()

	if id == 0 {
		if id = r.insert(context.Background()); id == 0 {
			r.markDirty()
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.WriteTimeout)
	defer cancel()
	err := r.store.UpdateStepRun(ctx, id, p)
	if err != nil {
		r.fail("heartbeat", err)
		r.mu.Lock()
		if errors.Is(err, db.ErrStepRunClosed) {
			r.abandoned = true // someone else closed or removed the row: stop writing to it
		} else {
			r.dirty = true // retry on the next tick
		}
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	r.writes++
	r.mu.Unlock()
}

func (r *Run) markDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}

// insert tries the start insert and returns the new id (0 on failure, which
// is logged). Callers guarantee no concurrent store call.
func (r *Run) insert(ctx context.Context) int64 {
	r.mu.Lock()
	s := r.start
	s.Message = r.state.Message
	r.mu.Unlock()
	ictx, cancel := context.WithTimeout(ctx, r.opts.WriteTimeout)
	defer cancel()
	id, err := r.store.StartStepRun(ictx, s)
	if err != nil {
		r.fail("start", err)
		return 0
	}
	r.mu.Lock()
	r.id = id
	r.mu.Unlock()
	return id
}

func (r *Run) snapshotLocked() db.StepRunProgress {
	p := r.state
	done, cost := r.done, r.cost
	p.Done, p.CostUSD = &done, &cost
	p.Counters = maps.Clone(r.counters)
	return p
}

func (r *Run) fail(what string, err error) {
	r.mu.Lock()
	r.failures++
	n, id := r.failures, r.id
	r.mu.Unlock()
	if n == 1 || n%logEvery == 0 {
		r.opts.Logger.Warn("step run recording failed; the step continues",
			"what", what, "step", r.start.Step, "id", id, "failures", n, "error", err)
	}
}
