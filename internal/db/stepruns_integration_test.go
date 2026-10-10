package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestIntegrationStepRuns: a run's lifecycle through the real table — insert,
// heartbeat, close — and the read-time stale rule: a running row whose
// heartbeat stopped is stale in the active list, past the abandon window it
// moves to the recent list, and a closed row can never be reopened by a late
// heartbeat. Skipped unless EARMARK_TEST_DATABASE_URL.
func TestIntegrationStepRuns(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()

	total := int64(29000)
	id, err := d.StartStepRun(ctx, StepRunStart{Step: "decide", Mode: StepRunWrite, Args: "--yes --limit 29000",
		Host: "earmark-mcp-0", Total: &total, Message: "starting"})
	if err != nil {
		t.Fatalf("StartStepRun: %v", err)
	}
	if _, err := d.StartStepRun(ctx, StepRunStart{Step: "Decide!", Mode: StepRunWrite}); err == nil {
		t.Error("a non-token step was accepted")
	}

	done, cost := int64(1200), 0.37
	if err := d.UpdateStepRun(ctx, id, StepRunProgress{RecipeID: "r1", Model: "jev-1.13.0", Total: &total, Done: &done,
		Counters: map[string]int64{"apply": 700, "hold": 300, "reject": 200}, CostUSD: &cost,
		Message: "asking jev-1.13.0"}); err != nil {
		t.Fatalf("UpdateStepRun: %v", err)
	}
	l, err := d.StepRuns(ctx, 10, 10)
	if err != nil {
		t.Fatalf("StepRuns: %v", err)
	}
	if len(l.Active) != 1 || len(l.Recent) != 0 {
		t.Fatalf("active %d recent %d, want 1/0", len(l.Active), len(l.Recent))
	}
	a := l.Active[0]
	if a.ID != id || a.Status != StepRunRunning || *a.Done != 1200 || *a.Total != 29000 || a.Counters["apply"] != 700 ||
		a.RecipeID != "r1" || a.Model != "jev-1.13.0" || a.Message != "asking jev-1.13.0" || a.Host != "earmark-mcp-0" ||
		*a.CostUSD != 0.37 || a.FinishedAt != nil {
		t.Errorf("active row = %+v", a)
	}
	if a.Stale(StepRunStaleAfter) {
		t.Error("a fresh heartbeat reads as stale")
	}

	// The heartbeat stops (the exec was killed): 3 minutes later it is stale,
	// still listed as active; a day later it is listed with the recent runs.
	if _, err := d.pool.Exec(ctx, `UPDATE step_runs SET heartbeat_at = now() - interval '3 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	l, _ = d.StepRuns(ctx, 10, 10)
	if len(l.Active) != 1 || !l.Active[0].Stale(StepRunStaleAfter) {
		t.Fatalf("a 3-minute-old heartbeat should be active and stale: %+v", l.Active)
	}
	if e := l.Active[0].Elapsed(StepRunStaleAfter); e > time.Minute {
		// started now, heartbeat moved 3 min into the past: elapsed ends at
		// the last heartbeat, never counts the dead time.
		t.Errorf("stale elapsed = %v, want it to stop at the last heartbeat", e)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE step_runs SET heartbeat_at = now() - interval '25 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	l, _ = d.StepRuns(ctx, 10, 10)
	if len(l.Active) != 0 || len(l.Recent) != 1 || !l.Recent[0].Stale(StepRunStaleAfter) {
		t.Fatalf("an abandoned run should be recent and stale: active %+v recent %+v", l.Active, l.Recent)
	}

	// Close it; a late heartbeat cannot reopen it.
	if err := d.FinishStepRun(ctx, id, StepRunFailed, StepRunProgress{Total: &total, Done: &done}, "jev unavailable"); err != nil {
		t.Fatalf("FinishStepRun: %v", err)
	}
	if err := d.UpdateStepRun(ctx, id, StepRunProgress{Done: &done}); !errors.Is(err, ErrStepRunClosed) {
		t.Errorf("heartbeat on a closed run: %v, want ErrStepRunClosed", err)
	}
	if err := d.FinishStepRun(ctx, id, StepRunDone, StepRunProgress{}, ""); !errors.Is(err, ErrStepRunClosed) {
		t.Errorf("second finish: %v, want ErrStepRunClosed", err)
	}
	if err := d.FinishStepRun(ctx, id, StepRunRunning, StepRunProgress{}, ""); err == nil {
		t.Error("running is not a closing status")
	}
	l, _ = d.StepRuns(ctx, 10, 10)
	r := l.Recent[0]
	if r.Status != StepRunFailed || r.FinishedAt == nil || r.Error != "jev unavailable" || r.Stale(StepRunStaleAfter) {
		t.Errorf("closed row = %+v", r)
	}

	// Metrics: one live write run with a total, one live dry run without.
	t2 := int64(200)
	d2, _ := d.StartStepRun(ctx, StepRunStart{Step: "scan", Mode: StepRunWrite, Total: &t2})
	half := int64(50)
	if err := d.UpdateStepRun(ctx, d2, StepRunProgress{Total: &t2, Done: &half, Counters: map[string]int64{"scanned": 50}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.StartStepRun(ctx, StepRunStart{Step: "scan", Mode: StepRunDryRun}); err != nil {
		t.Fatal(err)
	}
	st, err := d.StepRunStats(ctx)
	if err != nil {
		t.Fatalf("StepRunStats: %v", err)
	}
	if len(st.Active) != 2 || st.Active[0].Step != "scan" || st.Active[0].N != 1 {
		t.Errorf("active = %+v (the stale decide run must not count)", st.Active)
	}
	if len(st.Progress) != 1 || st.Progress[0].Step != "scan" || st.Progress[0].Ratio != 0.25 {
		t.Errorf("progress = %+v, want scan 0.25", st.Progress)
	}
	items := map[string]int64{}
	for _, i := range st.Items {
		items[i.Step+"/"+i.Outcome] = i.N
	}
	if items["scan/scanned"] != 50 || items["decide/apply"] != 0 {
		// the closed decide run was finished with nil counters → {}
		t.Errorf("items = %v", items)
	}
}

// TestIntegrationStepRunsIndexes: 00010 creates the two read indexes the
// dashboard's LIMIT queries rely on.
func TestIntegrationStepRunsIndexes(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	var n int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'step_runs'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 { // pkey + running + started_at
		t.Errorf("step_runs indexes = %d, want 3", n)
	}
}
