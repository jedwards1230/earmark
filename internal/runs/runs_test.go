package runs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// fakeStore records every call. failStart/failUpdate/failFinish make the
// matching call fail (the n-th and later when set to n > 0).
type fakeStore struct {
	mu                    sync.Mutex
	starts                []db.StepRunStart
	updates               []db.StepRunProgress
	finishes              []string
	finishP               db.StepRunProgress
	finishErr             string
	failStart, failUpdate int // fail the first N starts / every update when -1
	failFinish            bool
	updateErr             error
}

func (f *fakeStore) StartStepRun(_ context.Context, s db.StepRunStart) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, s)
	if len(f.starts) <= f.failStart {
		return 0, errors.New("start: connection refused")
	}
	return 42, nil
}

func (f *fakeStore) UpdateStepRun(_ context.Context, id int64, p db.StepRunProgress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != 42 {
		return fmt.Errorf("unexpected id %d", id)
	}
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.failUpdate < 0 {
		return errors.New("update: relation \"step_runs\" does not exist")
	}
	f.updates = append(f.updates, p)
	return nil
}

func (f *fakeStore) FinishStepRun(_ context.Context, _ int64, status string, p db.StepRunProgress, errText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFinish {
		return errors.New("finish: connection reset")
	}
	f.finishes = append(f.finishes, status)
	f.finishP, f.finishErr = p, errText
	return nil
}

func (f *fakeStore) snapshot() (starts, updates, finishes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts), len(f.updates), len(f.finishes)
}

var fast = Options{Interval: 5 * time.Millisecond, Heartbeat: time.Hour, WriteTimeout: time.Second, Host: "pod-1"}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestLifecycle: Start inserts the spec synchronously, progress reaches the
// store, Finish closes with the final counters, and Finish is idempotent.
func TestLifecycle(t *testing.T) {
	st := &fakeStore{}
	total := int64(3)
	r := StartWith(context.Background(), st, Spec{Step: StepDecide, Mode: db.StepRunWrite, Args: "--yes", Total: &total}, fast)
	if r.ID() != 42 {
		t.Fatalf("id = %d, want 42 (insert is synchronous)", r.ID())
	}
	if s := st.starts[0]; s.Step != StepDecide || s.Mode != db.StepRunWrite || s.Host != "pod-1" || *s.Total != 3 || s.Message != "starting" {
		t.Errorf("start row = %+v", s)
	}
	r.SetRecipe("abc123", "jev-1.13.0")
	r.Phase("asking jev-1.13.0")
	r.Tick("apply", 0.01)
	r.Tick("hold", 0.02)
	r.Tick("", 0)
	r.Count("cache_hits", 2)
	waitFor(t, "a heartbeat", func() bool { _, u, _ := st.snapshot(); return u > 0 })
	r.Finish(nil)
	r.Finish(errors.New("ignored: already finished"))

	if _, _, f := st.snapshot(); f != 1 || st.finishes[0] != db.StepRunDone {
		t.Fatalf("finishes = %v, want [done]", st.finishes)
	}
	p := st.finishP
	if *p.Done != 3 || p.Counters["apply"] != 1 || p.Counters["hold"] != 1 || p.Counters["cache_hits"] != 2 ||
		p.RecipeID != "abc123" || p.Model != "jev-1.13.0" || p.Message != "asking jev-1.13.0" || *p.Total != 3 {
		t.Errorf("final progress = %+v counters %v", p, p.Counters)
	}
	if c := *p.CostUSD; c < 0.0299 || c > 0.0301 {
		t.Errorf("cost = %v, want 0.03", c)
	}
	// After Finish, progress is ignored and nothing more is written.
	_, u, _ := st.snapshot()
	r.Tick("apply", 1)
	time.Sleep(20 * time.Millisecond)
	if _, u2, _ := st.snapshot(); u2 != u {
		t.Errorf("wrote after Finish: %d → %d updates", u, u2)
	}
}

// TestThrottle: thousands of progress calls in a burst produce at most one
// write per Interval.
func TestThrottle(t *testing.T) {
	st := &fakeStore{}
	o := fast
	o.Interval = 50 * time.Millisecond
	r := StartWith(context.Background(), st, Spec{Step: StepScan, Mode: db.StepRunDryRun}, o)
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Since(start) < 300*time.Millisecond {
				r.Tick("scanned", 0)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	_, u, _ := st.snapshot()
	r.Finish(nil)
	maxWrites := int(elapsed/o.Interval) + 2
	if u == 0 || u > maxWrites {
		t.Errorf("%d progress writes in %v at a %v interval, want 1..%d", u, elapsed, o.Interval, maxWrites)
	}
	if *st.finishP.Done < 1000 {
		t.Errorf("done = %d; the throttle must not drop progress, only writes", *st.finishP.Done)
	}
}

// TestHeartbeatWithoutProgress: a run waiting on a slow call still writes
// every Heartbeat, so it is not mistaken for a dead one.
func TestHeartbeatWithoutProgress(t *testing.T) {
	st := &fakeStore{}
	o := fast
	o.Heartbeat = 20 * time.Millisecond
	r := StartWith(context.Background(), st, Spec{Step: StepEvalBackfill, Mode: db.StepRunWrite}, o)
	defer r.Finish(nil)
	waitFor(t, "three heartbeats with no progress", func() bool { _, u, _ := st.snapshot(); return u >= 3 })
}

// TestRecordingFailuresNeverFail: every store call failing leaves the step
// untouched — no panic, no error, no hang — and the failures are counted.
func TestRecordingFailuresNeverFail(t *testing.T) {
	st := &fakeStore{failStart: 1 << 30, failUpdate: -1, failFinish: true}
	r := StartWith(context.Background(), st, Spec{Step: StepDecide, Mode: db.StepRunWrite}, fast)
	if r == nil || r.ID() != 0 {
		t.Fatalf("a failed insert must still return a usable run with id 0, got %v", r)
	}
	for i := 0; i < 100; i++ {
		r.Tick("apply", 0.1)
	}
	waitFor(t, "a retried insert", func() bool { s, _, _ := st.snapshot(); return s >= 2 })
	done := make(chan struct{})
	go func() { r.Finish(errors.New("boom")); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Finish hung on a failing store")
	}
	if _, f := r.Stats(); f < 2 {
		t.Errorf("failures = %d, want the failed inserts counted", f)
	}
}

// TestInsertRetried: a database that is down at Start but back later still
// gets the row, with the progress made meanwhile.
func TestInsertRetried(t *testing.T) {
	st := &fakeStore{failStart: 1}
	r := StartWith(context.Background(), st, Spec{Step: StepScan, Mode: db.StepRunWrite}, fast)
	if r.ID() != 0 {
		t.Fatal("first insert should have failed")
	}
	r.Tick("scanned", 0)
	waitFor(t, "the retried insert", func() bool { return r.ID() == 42 })
	r.Finish(nil)
	if *st.finishP.Done != 1 || st.finishes[0] != db.StepRunDone {
		t.Errorf("finish = %v %+v", st.finishes, st.finishP)
	}
}

// TestAbandonedRow: when the row was closed elsewhere the recorder stops
// writing, and Finish neither hangs nor writes.
func TestAbandonedRow(t *testing.T) {
	st := &fakeStore{updateErr: fmt.Errorf("write: %w", db.ErrStepRunClosed)}
	r := StartWith(context.Background(), st, Spec{Step: StepScan, Mode: db.StepRunWrite}, fast)
	r.Tick("x", 0)
	waitFor(t, "the failed heartbeat", func() bool { _, f := r.Stats(); return f >= 1 })
	r.Finish(nil)
	if _, _, f := st.snapshot(); f != 0 {
		t.Errorf("finished an abandoned row %d times", f)
	}
}

// TestFinishAfterCancel: an interrupted run is still recorded, as cancelled.
func TestFinishAfterCancel(t *testing.T) {
	st := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	r := StartWith(ctx, st, Spec{Step: StepDecide, Mode: db.StepRunWrite}, fast)
	cancel()
	r.Finish(fmt.Errorf("decide: %w", ctx.Err()))
	if len(st.finishes) != 1 || st.finishes[0] != db.StepRunCancelled || st.finishErr == "" {
		t.Errorf("finishes = %v (%q), want [cancelled] with the error", st.finishes, st.finishErr)
	}
}

func TestNilRunIsANoOp(t *testing.T) {
	var r *Run
	r.SetRecipe("a", "b")
	r.SetTotal(3)
	r.Phase("x")
	r.Progress(1, map[string]int64{"a": 1}, 0.1, "y")
	r.Tick("a", 0.1)
	r.Count("a", 1)
	r.Finish(errors.New("x"))
	if r.ID() != 0 {
		t.Error("nil run has an id")
	}
	if Start(context.Background(), nil, Spec{}) != nil {
		t.Error("Start with a nil store should return nil")
	}
}

func TestProgressAbsoluteAndTotal(t *testing.T) {
	st := &fakeStore{}
	r := StartWith(context.Background(), st, Spec{Step: StepEvalSample, Mode: db.StepRunDryRun}, fast)
	r.SetTotal(10)
	r.Progress(4, map[string]int64{"evaluated": 4}, 0.5, "judging")
	r.Progress(-1, nil, -1, "") // clamps done, keeps counters, cost and phase
	r.SetTotal(-1)
	r.Finish(nil)
	p := st.finishP
	if *p.Done != 0 || p.Counters["evaluated"] != 4 || *p.CostUSD != 0.5 || p.Message != "judging" || p.Total != nil {
		t.Errorf("progress = done %d counters %v cost %v msg %q total %v", *p.Done, p.Counters, *p.CostUSD, p.Message, p.Total)
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, db.StepRunDone},
		{context.Canceled, db.StepRunCancelled},
		{fmt.Errorf("run: %w", context.Canceled), db.StepRunCancelled},
		{context.DeadlineExceeded, db.StepRunFailed},
		{errors.New("boom"), db.StepRunFailed},
	} {
		if got := Status(tc.err); got != tc.want {
			t.Errorf("Status(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}
