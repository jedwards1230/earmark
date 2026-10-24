package mcp

import (
	"context"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// Demo fixtures for the Models page's Track record (CONTRACT §2.14), rendered
// through the real builder (buildTrackRows):
//
//	empty          — no passes, no decisions
//	snapshot-error — the read fails: "track record unavailable", the rest of
//	                 the page is unaffected
//	every other    — the judge (complete, partial, failed, one running), a
//	                 fallback judge, a model with legacy passes only, a pass
//	                 without a recipe; Jev (2,000 decisions since undone) and
//	                 Clef (with duplicates) on the decide side
func (d demoDB) ModelTrackRecord(context.Context) ([]db.ModelTrack, error) {
	switch d.scenario {
	case "empty":
		return nil, nil
	case demoScenarioSnapshotError:
		return nil, errDemoSnapshot
	}
	now := time.Now()
	ago := func(dur time.Duration) time.Time { return now.Add(-dur) }
	// In the order the database returns them: by alias, the alias-less row last.
	return []db.ModelTrack{
		{ModelAlias: demoJudgeModel, PassesComplete: 4_287, PassesPartial: 41, PassesFailed: 7, PassesRunning: 1, Last: ago(14 * time.Minute)},
		{ModelAlias: "clef:27b", Apply: 1_903, Hold: 38_385, Reject: 7_675, Duplicate: 415, Last: ago(3 * time.Hour)},
		{ModelAlias: "gemma3:12b", PassesLegacy: 4_117, Last: ago(120 * 24 * time.Hour)},
		{ModelAlias: demoJevModel, Apply: 131, Hold: 10_255, Reject: 19_041, Undone: 2_000, Last: ago(30 * time.Hour)},
		{ModelAlias: demoFallback, PassesLegacy: 68, Last: ago(110 * 24 * time.Hour)},
		{ModelAlias: "", PassesLegacy: 12, Last: ago(200 * 24 * time.Hour)},
	}, nil
}
