package batch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

// fakeLocker is an in-memory Locker. held simulates another coordinator owning
// the lock; tryErr a failed lock query. onRelease runs inside release so a test
// can observe the store at the moment the lock is dropped.
type fakeLocker struct {
	mu        sync.Mutex
	held      bool
	tryErr    error
	relErr    error
	tries     int
	releases  int
	onRelease func()
}

func (l *fakeLocker) TryBatchLock(context.Context) (func() error, bool, error) {
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
	return func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releases++
		l.held = false
		if l.onRelease != nil {
			l.onRelease()
		}
		return l.relErr
	}, true, nil
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

			if lock.releases != 1 {
				t.Fatalf("release calls = %d, want 1", lock.releases)
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
