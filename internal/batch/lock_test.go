package batch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// fakeLocker is an in-memory Locker. held simulates another coordinator owning
// the lock; tryErr a failed lock query. onRelease runs inside Release so a test
// can observe the store at the moment the lock is dropped. lostAfter > 0 makes
// the lock's Check fail from that call on (a dropped connection).
type fakeLocker struct {
	mu        sync.Mutex
	held      bool
	tryErr    error
	relErr    error
	lostAfter int
	onLost    func() // runs once, at the first failing Check
	lostFired bool
	// Successor scripting: successor is the definitive answer; successorErrs
	// failing lookups come first (-1 = every lookup fails).
	successor      bool
	successorErrs  int
	successorCalls int
	checks         int
	tries          int
	releases       int
	onRelease      func()
}

func (l *fakeLocker) TryBatchLock(context.Context) (Lock, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tries++
	if l.tryErr != nil {
		return nil, false, l.tryErr
	}
	if l.held {
		return nil, false, nil
	}
	l.held = true
	return fakeLock{l}, true, nil
}

type fakeLock struct{ l *fakeLocker }

func (f fakeLock) Check(context.Context) error {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()
	f.l.checks++
	if f.l.lostAfter > 0 && f.l.checks >= f.l.lostAfter {
		if !f.l.lostFired && f.l.onLost != nil {
			f.l.lostFired = true
			f.l.onLost()
		}
		return errors.New("conn closed")
	}
	return nil
}

func (f fakeLock) Successor(context.Context) (bool, error) {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()
	f.l.successorCalls++
	if f.l.successorErrs < 0 || f.l.successorCalls <= f.l.successorErrs {
		return false, errors.New("holder lookup timed out")
	}
	return f.l.successor, nil
}

func (f fakeLock) Release() error {
	l := f.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases++
	l.held = false
	if l.onRelease != nil {
		l.onRelease()
	}
	return l.relErr
}

func (l *fakeLocker) counts() (checks, releases int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checks, l.releases
}

// untouched fails the test if the coordinator read or wrote runner_control.
func untouched(t *testing.T, store *fakeStore, wantLimit *int) {
	t.Helper()
	if got := store.transitions(); len(got) != 0 {
		t.Errorf("no phase writes allowed, got %v", got)
	}
	if store.statusCalls != 0 {
		t.Errorf("no status reads allowed, got %d", store.statusCalls)
	}
	if got := store.currentRunLimit(); got != wantLimit {
		t.Errorf("run_limit must be untouched: got %v, want %v", got, wantLimit)
	}
}

// A second coordinator must exit before touching runner_control: swapping
// run_limit or restoring idle would clobber the live coordinator's batch.
func TestRun_SecondInstanceExitsWithoutTouchingState(t *testing.T) {
	store := newFakeStore(db.PhaseTranscribe, &db.QueueStats{Pending: 5})
	limit := ptr(4) // the live coordinator's Phase A budget
	store.runLimit = limit
	o := fastOpts()
	o.Lock = &fakeLocker{held: true}

	var out bytes.Buffer
	err := Run(context.Background(), &out, store, &fakeArbiter{}, o)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run error = %v, want ErrAlreadyRunning", err)
	}
	untouched(t, store, limit)
	if store.phase != db.PhaseTranscribe {
		t.Errorf("phase must stay the live run's transcribe, got %q", store.phase)
	}
	if !strings.Contains(out.String(), "holds the batch lock") {
		t.Errorf("expected a log line naming the held lock, got:\n%s", out.String())
	}
}

func TestRun_LockErrorFailsWithoutTouchingState(t *testing.T) {
	store := newFakeStore(db.PhaseIdle, &db.QueueStats{Pending: 5})
	o := fastOpts()
	boom := errors.New("connection refused")
	o.Lock = &fakeLocker{tryErr: boom}

	err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o)
	if !errors.Is(err, boom) || errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run error = %v, want the wrapped lock error (not ErrAlreadyRunning)", err)
	}
	untouched(t, store, nil)
}

// The lock is held across the restore-idle cleanup and released exactly once
// after it, on the success, error and cancel paths alike.
func TestRun_ReleasesLockAfterRestore(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeStore) context.Context
	}{
		{"completion", func(*fakeStore) context.Context { return context.Background() }},
		{"error", func(s *fakeStore) context.Context {
			s.getStatusErr = errors.New("db down")
			return context.Background()
		}},
		{"cancel", func(*fakeStore) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(db.PhaseIdle,
				&db.QueueStats{Pending: 2},
				&db.QueueStats{RunLimit: ptr(0)},
				&db.QueueStats{},
			)
			ctx := tc.setup(store)
			var phaseAtRelease []string
			lock := &fakeLocker{}
			lock.onRelease = func() { phaseAtRelease = store.transitions() }
			o := fastOpts()
			o.Lock = lock

			_ = Run(ctx, io.Discard, store, &fakeArbiter{}, o)

			if _, rel := lock.counts(); rel != 1 {
				t.Fatalf("release calls = %d, want 1", rel)
			}
			if n := len(phaseAtRelease); n == 0 || phaseAtRelease[n-1] != db.PhaseIdle {
				t.Errorf("lock released before idle was restored; phases at release = %v", phaseAtRelease)
			}
		})
	}
}

// A failing release is logged, not fatal: closing the session drops the lock.
func TestRun_ReleaseErrorIsOnlyLogged(t *testing.T) {
	store := newFakeStore(db.PhaseIdle, &db.QueueStats{})
	o := fastOpts()
	o.Lock = &fakeLocker{relErr: errors.New("unlock failed")}
	var out bytes.Buffer
	if err := Run(context.Background(), &out, store, &fakeArbiter{}, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "releasing the batch lock") {
		t.Errorf("expected a release warning, got:\n%s", out.String())
	}
}

// phase=transcribe at start is residue of an interrupted run: a fresh Phase A
// runs (never a skip straight to analyze) and the exit restores unlimited, not
// the residual budget.
func TestRun_ResumeFromTranscribeStartsFreshPhaseA(t *testing.T) {
	store := newFakeStore(db.PhaseTranscribe,
		&db.QueueStats{Pending: 3, Claimed: 1},                   // pre-batch: work left, a job still claimed
		&db.QueueStats{Pending: 1, Claimed: 1, RunLimit: ptr(0)}, // claimed job not done yet
		&db.QueueStats{Pending: 1, Claimed: 0, RunLimit: ptr(0)}, // Phase A done
		&db.QueueStats{EmbedBacklog: 0},                          // Phase B drained
	)
	store.runLimit = ptr(3) // residual budget from the crashed run
	o := fastOpts()
	o.Lock = &fakeLocker{}

	var out bytes.Buffer
	if err := Run(context.Background(), &out, store, &fakeArbiter{}, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{db.PhaseTranscribe, db.PhaseAnalyze, db.PhaseIdle}
	got := store.transitions()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	if store.runLimitAtFirstStatus == nil || *store.runLimitAtFirstStatus != 0 {
		t.Errorf("the residual budget must be parked at 0 before polling; got %v", store.runLimitAtFirstStatus)
	}
	if store.currentRunLimit() != nil {
		t.Errorf("exit must restore run_limit=unlimited, got %d", *store.currentRunLimit())
	}
	if !strings.Contains(out.String(), "found phase=transcribe") {
		t.Errorf("expected the transcribe-resume log line, got:\n%s", out.String())
	}
}

// A lost lock with a successor holding it: the run stops and does NOT
// restore — the successor owns runner_control, and an idle/run_limit reset
// would clobber its batch. Run fails with ErrLockLost.
func TestRun_LockLostSkipsRestoreAndLeavesSuccessorState(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 5},
		&db.QueueStats{Pending: 5, Claimed: 1, RunLimit: ptr(1)}, // Phase A never finishes
	)
	var phasesBeforeSuccessor int
	lock := &fakeLocker{lostAfter: 3}
	lock.onLost = func() { // runs under lock.mu
		lock.successor = true
		phasesBeforeSuccessor = len(store.transitions())
		store.mu.Lock()
		store.phase = db.PhaseTranscribe
		store.runLimit = ptr(7)
		store.mu.Unlock()
	}
	o := fastOpts()
	o.Lock = lock

	var out bytes.Buffer
	err := Run(context.Background(), &out, store, &fakeArbiter{}, o)
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Run error = %v, want ErrLockLost", err)
	}
	if got := store.transitions(); len(got) != phasesBeforeSuccessor {
		t.Errorf("no phase write allowed after the successor took over; transitions=%v (took over after %d)", got, phasesBeforeSuccessor)
	}
	if store.phase != db.PhaseTranscribe {
		t.Errorf("successor's phase clobbered: got %q", store.phase)
	}
	if rl := store.currentRunLimit(); rl == nil || *rl != 7 {
		t.Errorf("successor's run_limit clobbered: got %v, want 7", rl)
	}
	if _, rel := lock.counts(); rel != 1 {
		t.Errorf("release calls = %d, want 1", rel)
	}
	for _, want := range []string{"batch lock lost", "NOT restoring idle"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("expected %q in the log, got:\n%s", want, out.String())
		}
	}
}

// A lost lock nobody has taken (the session died, no successor yet): the
// restore still runs — nothing can be clobbered, and skipping it would leave
// the runner budget-gated — and the run still fails with ErrLockLost.
func TestRun_LockLostWithoutSuccessorRestores(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 5},
		&db.QueueStats{Pending: 5, Claimed: 1, RunLimit: ptr(1)},
	)
	o := fastOpts()
	o.Lock = &fakeLocker{lostAfter: 3}
	err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o)
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Run error = %v, want ErrLockLost", err)
	}
	got := store.transitions()
	if len(got) == 0 || got[len(got)-1] != db.PhaseIdle {
		t.Errorf("idle must be restored when no successor holds the lock; transitions=%v", got)
	}
	if store.currentRunLimit() != nil {
		t.Errorf("pre-batch run_limit (unlimited) must be restored, got %d", *store.currentRunLimit())
	}
}

// A successor present at exit, even if the heartbeat never noticed a loss,
// blocks the restore.
func TestRun_SuccessorAtExitSkipsRestore(t *testing.T) {
	store := newFakeStore(db.PhaseIdle, &db.QueueStats{}) // no work: straight to exit
	o := fastOpts()
	o.PollInterval = time.Hour // the heartbeat never ticks
	o.Lock = &fakeLocker{successor: true}
	err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o)
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Run error = %v, want ErrLockLost", err)
	}
	if got := store.transitions(); len(got) != 0 {
		t.Errorf("no restore allowed with a successor holding the lock; transitions=%v", got)
	}
}

// An inconclusive holder lookup is retried; transient errors followed by a
// clean "no successor" restore normally, and errors throughout still restore
// (with a warning) rather than leaving the runner budget-gated.
func TestRun_InconclusiveHolderLookupStillRestores(t *testing.T) {
	old := successorBackoff
	successorBackoff = time.Millisecond
	t.Cleanup(func() { successorBackoff = old })

	for _, tc := range []struct {
		name      string
		errs      int
		wantCalls int
		wantWarn  bool
	}{
		{"transient then clean", 1, 2, false},
		{"always failing", -1, successorAttempts, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(db.PhaseIdle, &db.QueueStats{})
			lock := &fakeLocker{successorErrs: tc.errs}
			o := fastOpts()
			o.PollInterval = time.Hour
			o.Lock = lock
			var out bytes.Buffer
			if err := Run(context.Background(), &out, store, &fakeArbiter{}, o); err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := store.transitions()
			if len(got) != 1 || got[0] != db.PhaseIdle {
				t.Errorf("idle must be restored; transitions=%v", got)
			}
			if lock.successorCalls != tc.wantCalls {
				t.Errorf("holder lookups = %d, want %d", lock.successorCalls, tc.wantCalls)
			}
			if warned := strings.Contains(out.String(), "restoring idle anyway"); warned != tc.wantWarn {
				t.Errorf("warning logged = %v, want %v:\n%s", warned, tc.wantWarn, out.String())
			}
		})
	}
}

// A healthy heartbeat never interferes with a normal run.
func TestRun_HeartbeatChecksWhileRunning(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 2},
		&db.QueueStats{Pending: 2, Claimed: 1, RunLimit: ptr(1)},
		&db.QueueStats{Pending: 2, Claimed: 1, RunLimit: ptr(1)},
		&db.QueueStats{Pending: 2, Claimed: 1, RunLimit: ptr(1)},
		&db.QueueStats{Pending: 1, Claimed: 0, RunLimit: ptr(0)},
		&db.QueueStats{EmbedBacklog: 0},
	)
	lock := &fakeLocker{}
	o := fastOpts()
	o.Lock = lock
	if err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, rel := lock.counts(); rel != 1 {
		t.Errorf("release calls = %d, want 1", rel)
	}
}

// Phase A that never finishes (runner asleep: nothing claimed, budget unspent)
// hits PhaseTimeout, restores idle and fails with ErrPhaseTimeout.
func TestRun_PhaseTimeout(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 5},
		&db.QueueStats{Pending: 5, RunLimit: ptr(2)},
	)
	o := fastOpts()
	o.PhaseTimeout = 20 * time.Millisecond
	err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o)
	if !errors.Is(err, ErrPhaseTimeout) {
		t.Fatalf("Run error = %v, want ErrPhaseTimeout", err)
	}
	want := []string{db.PhaseTranscribe, db.PhaseIdle}
	if got := store.transitions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("transitions = %v, want %v (no analyze after a timed-out Phase A)", got, want)
	}
}

// A cancel of the run itself is not reported as a phase timeout.
func TestRun_CancelIsNotPhaseTimeout(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 5},
		&db.QueueStats{Pending: 5, RunLimit: ptr(2)},
	)
	o := fastOpts()
	o.PhaseTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := Run(ctx, io.Discard, store, &fakeArbiter{}, o)
	if err == nil || errors.Is(err, ErrPhaseTimeout) {
		t.Fatalf("Run error = %v, want a cancellation, not ErrPhaseTimeout", err)
	}
}

func TestRun_RejectsNegativePhaseTimeout(t *testing.T) {
	store := newFakeStore(db.PhaseIdle)
	o := fastOpts()
	o.PhaseTimeout = -time.Second
	if err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o); err == nil {
		t.Fatal("expected an error for a negative phase timeout")
	}
}
