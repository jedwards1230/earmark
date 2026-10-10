package mcp

import (
	"context"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Demo fixtures for the "Running now" panel (CONTRACT §1.10), rendered
// through the real builders (buildRunView, linkRoleRuns):
//
//	active / batch-analyze — a live `decide --yes` at 12,400 / 29,000 asking
//	                         jev-1.13.0 (Decide card links to it), and a
//	                         `scan` whose heartbeat stopped 7 min ago → stale
//	stale                  — only the stale scan (a killed kubectl exec)
//	failed                 — a live eval backfill with no total (Judge card
//	                         links to it); the newest recent run failed
//	empty                  — nothing running, no history
//	every other scenario   — nothing running; the recent runs only

// StepRuns returns the scenario's runs, newest first, as the database would.
func (d demoDB) StepRuns(_ context.Context, activeLimit, recentLimit int) (db.StepRunList, error) {
	if d.scenario == "empty" {
		return db.StepRunList{}, nil
	}
	now := time.Now()
	l := db.StepRunList{Recent: demoRecentRuns(now, d.scenario)}
	switch d.scenario {
	case "", "active", "batch-analyze":
		l.Active = []db.StepRun{demoDecideRun(now), demoStaleScanRun(now)}
	case "stale":
		l.Active = []db.StepRun{demoStaleScanRun(now)}
	case "failed":
		l.Active = []db.StepRun{demoBackfillRun(now)}
	}
	if len(l.Active) > activeLimit {
		l.Active = l.Active[:activeLimit]
	}
	if len(l.Recent) > recentLimit {
		l.Recent = l.Recent[:recentLimit]
	}
	return l, nil
}

func i64(n int64) *int64        { return &n }
func f64(f float64) *float64    { return &f }
func tp(t time.Time) *time.Time { return &t }

// demoDecideRun: last night's 29k-finding decide --yes, 20 minutes in.
func demoDecideRun(now time.Time) db.StepRun {
	return db.StepRun{
		ID: 118, Step: "decide", Mode: db.StepRunWrite,
		RecipeID: demoRecipeID(recipe.StepDecide), Model: demoJevModel,
		Args: "--yes --limit 29000", Host: "earmark-mcp-7d9c5b6f4-x2kq8",
		StartedAt: now.Add(-20 * time.Minute), HeartbeatAt: now.Add(-1 * time.Second),
		Status: db.StepRunRunning, Total: i64(29_000), Done: i64(12_400),
		Counters: map[string]int64{"apply": 3_180, "hold": 5_920, "reject": 2_870, "reanchor": 430,
			"cached": 2_210, "errors": 12, "skipped": 4},
		CostUSD: f64(3.8412), Message: "page 25: asking " + demoJevModel, ObservedAt: now,
	}
}

// demoStaleScanRun: a scan whose kubectl exec was killed 7 minutes ago.
func demoStaleScanRun(now time.Time) db.StepRun {
	return db.StepRun{
		ID: 117, Step: "scan", Mode: db.StepRunWrite,
		RecipeID: demoRecipeID(recipe.StepScan), Model: demoJevModel,
		Args: "--sample 500 --seed q4 --yes", Host: "earmark-mcp-7d9c5b6f4-x2kq8",
		StartedAt: now.Add(-41 * time.Minute), HeartbeatAt: now.Add(-7 * time.Minute),
		Status: db.StepRunRunning, Total: i64(500), Done: i64(212),
		Counters: map[string]int64{"scanned": 209, "cached": 50, "errors": 3, "written": 209},
		CostUSD:  f64(0.6120), Message: "asking " + demoJevModel, ObservedAt: now,
	}
}

// demoBackfillRun: the hourly CronJob's eval backfill, total unknown.
func demoBackfillRun(now time.Time) db.StepRun {
	return db.StepRun{
		ID: 119, Step: "eval_backfill", Mode: db.StepRunWrite,
		RecipeID: demoRecipeID(recipe.StepPropose), Model: demoJudgeAlias,
		Args: "--backfill-unevaluated --limit 25 --write", Host: "earmark-eval-backfill-29340180-6vx2m",
		StartedAt: now.Add(-4 * time.Minute), HeartbeatAt: now.Add(-3 * time.Second),
		Status: db.StepRunRunning, Done: i64(9),
		Counters: map[string]int64{"latched": 7, "failed": 1, "skipped": 1, "chunks": 412, "findings": 38, "chunk_errors": 2},
		CostUSD:  f64(0.2214), Message: "judging 07 - The Sardaukar.m4b · asking " + demoJudgeAlias, ObservedAt: now,
	}
}

func demoRecentRuns(now time.Time, scenario string) []db.StepRun {
	finished := func(start, dur time.Duration) (time.Time, time.Time, *time.Time) {
		s := now.Add(-start)
		f := s.Add(dur)
		return s, f, tp(f)
	}
	var runs []db.StepRun
	if scenario == "failed" {
		s, h, f := finished(70*time.Minute, 3*time.Minute)
		runs = append(runs, db.StepRun{
			ID: 116, Step: "eval_backfill", Mode: db.StepRunWrite, Model: demoJudgeAlias,
			Args: "--backfill-unevaluated --limit 25 --write", Host: "earmark-eval-backfill-29340120-k8w1d",
			StartedAt: s, HeartbeatAt: h, FinishedAt: f, Status: db.StepRunFailed, Done: i64(5),
			Counters: map[string]int64{"failed": 5, "chunk_errors": 214},
			Error:    "judge outage: 5 consecutive transcripts failed on every chunk", ObservedAt: now,
		})
	}
	s1, h1, f1 := finished(3*time.Hour, 6*time.Minute)
	s2, h2, f2 := finished(5*time.Hour, 90*time.Second)
	s3, h3, f3 := finished(26*time.Hour, 12*time.Second)
	s4, h4 := now.Add(-3*24*time.Hour), now.Add(-3*24*time.Hour+18*time.Minute)
	return append(runs,
		db.StepRun{
			ID: 115, Step: "decide", Mode: db.StepRunDryRun, RecipeID: demoRecipeID(recipe.StepDecide), Model: demoJevModel,
			Args: "--sample 200 --seed q4", Host: "earmark-mcp-7d9c5b6f4-x2kq8",
			StartedAt: s1, HeartbeatAt: h1, FinishedAt: f1, Status: db.StepRunDone, Total: i64(200), Done: i64(200),
			Counters: map[string]int64{"apply": 61, "hold": 88, "reject": 45, "reanchor": 6, "cached": 12},
			CostUSD:  f64(0.0712), ObservedAt: now,
		},
		db.StepRun{
			ID: 114, Step: "decide_revert", Mode: db.StepRunWrite, RecipeID: demoRecipeID(recipe.StepDecide),
			Args: "recipe " + demoRecipeID(recipe.StepDecide), Host: "earmark-mcp-7d9c5b6f4-x2kq8",
			StartedAt: s2, HeartbeatAt: h2, FinishedAt: f2, Status: db.StepRunDone, Done: i64(140),
			Counters: map[string]int64{"moved": 140, "revoked": 152, "reflagged": 97}, ObservedAt: now,
		},
		db.StepRun{
			ID: 113, Step: "eval_sample", Mode: db.StepRunDryRun, Model: demoJudgeAlias,
			Args: "--sample 50 --seed q4", Host: "earmark-mcp-7d9c5b6f4-x2kq8",
			StartedAt: s3, HeartbeatAt: h3, FinishedAt: f3, Status: db.StepRunCancelled, Total: i64(50), Done: i64(17),
			Counters: map[string]int64{"evaluated": 17, "findings": 9}, CostUSD: f64(0.0301),
			Error: "judge run stopped: context canceled", ObservedAt: now,
		},
		// Abandoned: running, its heartbeat stopped days ago → listed here as stale.
		db.StepRun{
			ID: 109, Step: "decide", Mode: db.StepRunWrite, RecipeID: demoRecipeID(recipe.StepDecide), Model: demoJevModel,
			Args: "--yes --limit 2000", Host: "earmark-mcp-6f8b9d7c5-p4lzt",
			StartedAt: s4, HeartbeatAt: h4, Status: db.StepRunRunning, Total: i64(2_000), Done: i64(1_130),
			Counters: map[string]int64{"apply": 290, "hold": 610, "reject": 230}, CostUSD: f64(0.4022),
			Message: "page 3: writing batch 4/12", ObservedAt: now,
		},
	)
}
