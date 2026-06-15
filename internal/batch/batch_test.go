package batch

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// fakeStore is an in-memory PhaseStore. It records the sequence of phases set
// (so tests assert transition order) and serves a scripted slice of QueueStats
// snapshots, advancing one per GetServiceStatus call (the last one repeats).
type fakeStore struct {
	mu sync.Mutex

	phase     string
	runLimit  *int
	phaseLog  []string // every phase passed to SetPipelinePhase, in order
	statuses  []*db.QueueStats
	statusIdx int

	// optional error injections
	setPhaseErr    error
	getStatusErr   error
	getStatusAfter int // return getStatusErr only on/after this many status calls
	statusCalls    int
}

func newFakeStore(initialPhase string, statuses ...*db.QueueStats) *fakeStore {
	return &fakeStore{phase: initialPhase, statuses: statuses}
}

func (f *fakeStore) GetPipelinePhase(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.phase == "" {
		return db.PhaseIdle, nil
	}
	return f.phase, nil
}

func (f *fakeStore) SetPipelinePhase(_ context.Context, phase, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setPhaseErr != nil {
		return f.setPhaseErr
	}
	f.phase = phase
	f.phaseLog = append(f.phaseLog, phase)
	return nil
}

func (f *fakeStore) SetRunLimit(_ context.Context, limit *int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runLimit = limit
	return nil
}

func (f *fakeStore) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.getStatusErr != nil && f.statusCalls >= f.getStatusAfter {
		return nil, f.getStatusErr
	}
	if len(f.statuses) == 0 {
		return &db.QueueStats{}, nil
	}
	st := f.statuses[f.statusIdx]
	if f.statusIdx < len(f.statuses)-1 {
		f.statusIdx++
	}
	return st, nil
}

// transitions returns the phase log without the lock-protected mutation race.
func (f *fakeStore) transitions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.phaseLog))
	copy(out, f.phaseLog)
	return out
}

// fakeArbiter returns scripted gaming/ok results, advancing one per call (last
// repeats). An empty script means "never gaming, not configured" (ok=false).
type fakeArbiter struct {
	mu      sync.Mutex
	results []arbiterResult
	idx     int
	calls   int
}

type arbiterResult struct {
	gaming bool
	ok     bool
}

func (a *fakeArbiter) Gaming(context.Context) (bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if len(a.results) == 0 {
		return false, false
	}
	r := a.results[a.idx]
	if a.idx < len(a.results)-1 {
		a.idx++
	}
	return r.gaming, r.ok
}

func ptr(n int) *int { return &n }

// fastOpts uses tiny intervals so poll/yield loops don't slow the test.
func fastOpts() Options {
	return Options{
		BatchSize:    2,
		MaxBatches:   1,
		PollInterval: time.Millisecond,
		ArbiterPoll:  time.Millisecond,
	}
}

// TestRun_FullCycleDrivesPhaseTransitions verifies one batch flips
// transcribe → analyze and then restores idle on exit.
func TestRun_FullCycleDrivesPhaseTransitions(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		// pre-batch check: pending work exists
		&db.QueueStats{Pending: 2},
		// transcribe poll: batch transcribed (run_limit hit 0, nothing claimed)
		&db.QueueStats{Pending: 0, Claimed: 0, RunLimit: ptr(0), EmbedBacklog: 2},
		// analyze poll: backlog drained
		&db.QueueStats{Pending: 0, Claimed: 0, EmbedBacklog: 0},
	)
	arb := &fakeArbiter{} // never gaming

	if err := Run(context.Background(), io.Discard, store, arb, fastOpts()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{db.PhaseTranscribe, db.PhaseAnalyze, db.PhaseIdle}
	got := store.transitions()
	if len(got) != len(want) {
		t.Fatalf("phase transitions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phase transitions = %v, want %v", got, want)
		}
	}
	if store.runLimit != nil {
		t.Errorf("run_limit should be cleared (nil) on exit, got %v", *store.runLimit)
	}
}

// TestRun_RestoresIdleOnNormalCompletionWithNoWork: with an empty queue, no
// phase work happens but idle is still asserted on exit.
func TestRun_RestoresIdleOnNormalCompletionWithNoWork(t *testing.T) {
	store := newFakeStore(db.PhaseIdle, &db.QueueStats{Pending: 0, Claimed: 0})
	if err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, fastOpts()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := store.transitions()
	if len(got) != 1 || got[0] != db.PhaseIdle {
		t.Errorf("with no work, only an idle restore should occur, got %v", got)
	}
}

// TestRun_GamingMakesItWait verifies the coordinator polls the arbiter while it
// reports gaming and proceeds once it stops.
func TestRun_GamingMakesItWait(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 2},
		&db.QueueStats{Pending: 0, Claimed: 0, RunLimit: ptr(0)},
		&db.QueueStats{Pending: 0, Claimed: 0, EmbedBacklog: 0},
	)
	// gaming for the first two checks, then available.
	arb := &fakeArbiter{results: []arbiterResult{
		{gaming: true, ok: true},
		{gaming: true, ok: true},
		{gaming: false, ok: true},
	}}

	if err := Run(context.Background(), io.Discard, store, arb, fastOpts()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if arb.calls < 3 {
		t.Errorf("expected the arbiter to be polled while gaming (>=3 calls), got %d", arb.calls)
	}
	// It still completed the batch → transcribe must have been set after waiting.
	got := store.transitions()
	if len(got) == 0 || got[0] != db.PhaseTranscribe {
		t.Errorf("after gaming cleared, the transcribe phase should run, got %v", got)
	}
}

// TestRun_UnreachableArbiterDoesNotWait: ok=false (unconfigured/unreachable)
// must not block — the coordinator proceeds immediately.
func TestRun_UnreachableArbiterDoesNotWait(t *testing.T) {
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 2},
		&db.QueueStats{Pending: 0, Claimed: 0, RunLimit: ptr(0)},
		&db.QueueStats{Pending: 0, Claimed: 0, EmbedBacklog: 0},
	)
	arb := &fakeArbiter{results: []arbiterResult{{gaming: true, ok: false}}} // gaming but ok=false

	if err := Run(context.Background(), io.Discard, store, arb, fastOpts()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if arb.calls != 1 {
		t.Errorf("unreachable arbiter should be checked once and not waited on, got %d calls", arb.calls)
	}
}

// TestRun_RestoresIdleOnError: a status read error mid-batch must still leave
// the pipeline restored to idle (the deferred cleanup runs on the error path).
func TestRun_RestoresIdleOnError(t *testing.T) {
	store := newFakeStore(db.PhaseIdle, &db.QueueStats{Pending: 2})
	store.getStatusErr = errors.New("db boom")
	store.getStatusAfter = 2 // succeed the pre-batch check, fail the transcribe poll

	err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, fastOpts())
	if err == nil {
		t.Fatal("expected an error from the injected status failure")
	}
	got := store.transitions()
	if len(got) == 0 || got[len(got)-1] != db.PhaseIdle {
		t.Errorf("idle must be restored even on error; transitions=%v", got)
	}
	if store.runLimit != nil {
		t.Errorf("run_limit must be cleared on error, got %v", *store.runLimit)
	}
}

// TestRun_RestoresIdleOnCancel: a cancelled context unwinds the loop and still
// restores idle (the cleanup uses a fresh context so the writes succeed).
func TestRun_RestoresIdleOnCancel(t *testing.T) {
	// Block in the transcribe poll forever (never done) so cancel is what ends it.
	store := newFakeStore(db.PhaseIdle,
		&db.QueueStats{Pending: 2},                               // pre-batch
		&db.QueueStats{Pending: 1, Claimed: 1, RunLimit: ptr(1)}, // never done
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	o := fastOpts()
	o.PollInterval = 5 * time.Millisecond
	err := Run(ctx, io.Discard, store, &fakeArbiter{}, o)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	got := store.transitions()
	if len(got) == 0 || got[len(got)-1] != db.PhaseIdle {
		t.Errorf("idle must be restored on cancel; transitions=%v", got)
	}
}

// TestRun_ResumeFromAnalyzeFinishesPhaseBFirst: starting with phase=analyze, the
// coordinator drains the in-flight analyze batch before any transcribe phase.
func TestRun_ResumeFromAnalyzeFinishesPhaseBFirst(t *testing.T) {
	store := newFakeStore(db.PhaseAnalyze,
		// resume analyze poll: backlog still draining, then drained
		&db.QueueStats{Pending: 0, Claimed: 0, EmbedBacklog: 3},
		&db.QueueStats{Pending: 0, Claimed: 0, EmbedBacklog: 0},
		// pre-batch check after resume: no pending work → stop
		&db.QueueStats{Pending: 0, Claimed: 0},
	)

	if err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, fastOpts()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := store.transitions()
	// First phase set on resume must be analyze (finish Phase B), never transcribe.
	if len(got) == 0 || got[0] != db.PhaseAnalyze {
		t.Fatalf("resume must finish analyze first; transitions=%v", got)
	}
	for _, ph := range got[:len(got)-1] { // exclude the trailing idle restore
		if ph == db.PhaseTranscribe {
			t.Fatalf("resume should not start a transcribe phase (no pending work); transitions=%v", got)
		}
	}
	if got[len(got)-1] != db.PhaseIdle {
		t.Errorf("idle must be restored on exit; transitions=%v", got)
	}
}

// TestRun_RejectsBadBatchSize guards the normalize() precondition.
func TestRun_RejectsBadBatchSize(t *testing.T) {
	store := newFakeStore(db.PhaseIdle)
	o := fastOpts()
	o.BatchSize = 0
	if err := Run(context.Background(), io.Discard, store, &fakeArbiter{}, o); err == nil {
		t.Fatal("expected an error for batch size 0")
	}
	if len(store.transitions()) != 0 {
		t.Error("no phase writes should happen when options are invalid")
	}
}

// TestTranscribeDone covers the batch-complete predicate.
func TestTranscribeDone(t *testing.T) {
	cases := []struct {
		name string
		st   *db.QueueStats
		want bool
	}{
		{"claimed in flight → not done", &db.QueueStats{Claimed: 1, RunLimit: ptr(0)}, false},
		{"budget exhausted, idle → done", &db.QueueStats{Claimed: 0, RunLimit: ptr(0)}, true},
		{"pending drained, idle → done", &db.QueueStats{Claimed: 0, Pending: 0, RunLimit: ptr(3)}, true},
		{"budget left, pending left, idle → not done", &db.QueueStats{Claimed: 0, Pending: 5, RunLimit: ptr(2)}, false},
		{"nil run_limit, pending left → not done", &db.QueueStats{Claimed: 0, Pending: 5, RunLimit: nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := transcribeDone(tc.st); got != tc.want {
				t.Errorf("transcribeDone(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}
