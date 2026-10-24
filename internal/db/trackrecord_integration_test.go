package db

// Postgres integration test for the Models page's Track record
// (ModelTrackRecord) over the scorecard fixture. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"
)

func TestIntegrationModelTrackRecord(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)

	t.Run("empty database", func(t *testing.T) {
		got, err := d.ModelTrackRecord(ctx)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("got %#v, %v; want an empty non-nil slice", got, err)
		}
	})

	f := seedScorecard(t, conn, d)
	by := func(t *testing.T) map[string]ModelTrack {
		t.Helper()
		rows, err := d.ModelTrackRecord(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]ModelTrack{}
		for _, r := range rows {
			if _, dup := out[r.ModelAlias]; dup {
				t.Fatalf("alias %q twice: %+v", r.ModelAlias, rows)
			}
			r.Last = r.Last.UTC() // the driver returns the machine's zone
			out[r.ModelAlias] = r
		}
		return out
	}
	day := func(s string) time.Time {
		tm, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}

	t.Run("the scorecard fixture", func(t *testing.T) {
		rows, err := d.ModelTrackRecord(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var order []string
		for _, r := range rows {
			order = append(order, r.ModelAlias)
		}
		if want := []string{"clef:27b", "gemma3:12b", "jev-1.13.0", "openrouter/openai/gpt-6-luna"}; !slices.Equal(order, want) {
			t.Fatalf("aliases %v, want %v", order, want)
		}
		got := by(t)
		want := map[string]ModelTrack{
			// Clef applied G1, the unattributed copy of G1's fix and G2.
			"clef:27b": {ModelAlias: "clef:27b", Apply: 3, Last: day("2026-10-05")},
			// Two legacy passes (one per transcript), no decisions.
			"gemma3:12b": {ModelAlias: "gemma3:12b", PassesLegacy: 2, Last: day("2026-06-03")},
			// Jev: G1 apply, G2 reject, L2 hold, L1 hold; G3's apply was revoked.
			"jev-1.13.0": {ModelAlias: "jev-1.13.0", Apply: 1, Hold: 2, Reject: 1, Undone: 1, Last: day("2026-10-04")},
			// Luna: its complete pass and a legacy one; it decided nothing.
			"openrouter/openai/gpt-6-luna": {ModelAlias: "openrouter/openai/gpt-6-luna", PassesComplete: 1, PassesLegacy: 1, Last: day("2026-10-03")},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("track record\n got %+v\nwant %+v", got, want)
		}
	})

	t.Run("latest decision per finding, passes of every status", func(t *testing.T) {
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := conn.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		decide := func(finding, recipe, outcome, at string) {
			t.Helper()
			exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, p, evidence, created_at)
				VALUES ($1, $2, 'decision', $3, 'r', 'jev:' || $4, $4, 0.5, 'none', $5)`,
				finding, lbTranscript, outcome, recipe, at)
		}
		// Jev retries a hold: still one finding on hold. Then it decides L1
		// again, rejecting what it first held: that finding moves holds -> rejects.
		decide(f.lunaHold, scJev, "hold", "2026-10-06")
		decide(f.lunaName, scJev, "reject", "2026-10-07")
		// Clef records L1 as a duplicate of G1.
		exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, related_finding_id, created_at)
			VALUES ($1, $2, 'decision', 'duplicate', 'duplicate', 'jev:' || $3, $3, $4, '2026-10-08')`,
			f.lunaName, lbTranscript, scClef, f.gemmaName)
		// A decision of Jev's that is revoked after a newer one: the older
		// (unrevoked) decision of the same finding is the live one again.
		decide(f.gemmaWord, scJev, "hold", "2026-10-09")
		var newest int64
		if err := conn.QueryRow(ctx, `SELECT max(id) FROM finding_events WHERE kind = 'decision' AND recipe_id = $1`, scJev).Scan(&newest); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
			VALUES ($1, $2, 'revoke', 'decide revert', 'revert:jev:' || $3, $3, $4)`, f.gemmaWord, lbTranscript, scJev, newest)
		// Judge passes in the other statuses, and one with no recipe.
		exec(`INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status, started_at, finished_at) VALUES
			($1, $2, '/b/LB/01.m4b', $3, 'failed',  '2026-10-10', '2026-10-10'),
			($1, $2, '/b/LB/01.m4b', $3, 'partial', '2026-10-11', '2026-10-11'),
			($1, $2, '/b/LB/01.m4b', NULL, 'legacy', '2026-05-01', '2026-05-01')`, lbTranscript, lbJob, scLuna)
		exec(`INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status, started_at)
			VALUES ($1, $2, '/b/LB/01.m4b', $3, 'running', '2026-10-12')`, lbTranscript, lbJob, scLuna)

		got := by(t)
		jev := got["jev-1.13.0"]
		// apply G1; reject G2 (the hold is revoked) and L1 (the reject replaced its
		// hold); hold L2 once however often it was decided; undone: G3's and G2's hold.
		if jev.Apply != 1 || jev.Hold != 1 || jev.Reject != 2 || jev.Undone != 2 {
			t.Errorf("jev = %+v, want apply 1 hold 1 reject 2 undone 2", jev)
		}
		if !jev.Last.Equal(day("2026-10-07")) {
			t.Errorf("jev last = %v, want 2026-10-07 (the revoked 10-09 hold does not count)", jev.Last)
		}
		if c := got["clef:27b"]; c.Duplicate != 1 || c.Apply != 3 || !c.Last.Equal(day("2026-10-08")) {
			t.Errorf("clef = %+v, want 3 applies and 1 duplicate, last 10-08", c)
		}
		luna := got["openrouter/openai/gpt-6-luna"]
		if luna.PassesComplete != 1 || luna.PassesLegacy != 1 || luna.PassesFailed != 1 || luna.PassesPartial != 1 ||
			luna.PassesRunning != 1 || luna.PassTotal() != 5 || !luna.Last.Equal(day("2026-10-12")) {
			t.Errorf("luna = %+v", luna)
		}
		none, ok := got[""]
		if !ok || none.PassesLegacy != 1 || none.HasDecisions() {
			t.Errorf("alias-less row = %+v (present %v), want the one recipe-less pass", none, ok)
		}
		rows, err := d.ModelTrackRecord(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if last := rows[len(rows)-1]; last.ModelAlias != "" {
			t.Errorf("last row = %q, want the alias-less one last", last.ModelAlias)
		}
	})
}

// TestIntegrationTrackRecordCoversTheVocabularies fails when a migration adds a
// decision outcome or a judge pass status the Track record does not count, so
// that a new value is decided on (counted, or deliberately left out) instead of
// silently missing from the table.
func TestIntegrationTrackRecordCoversTheVocabularies(t *testing.T) {
	ctx := context.Background()
	dbURL, _ := migratedTestDB(t)
	conn := connect(t, dbURL)
	quoted := regexp.MustCompile(`'([a-z_]+)'`)
	vocab := func(constraint string) []string {
		t.Helper()
		var def string
		if err := conn.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def); err != nil {
			t.Fatalf("%s: %v", constraint, err)
		}
		var out []string
		for _, m := range quoted.FindAllStringSubmatch(def, -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	for constraint, want := range map[string][]string{
		"finding_events_outcome_valid": {OutcomeApply, OutcomeDuplicate, OutcomeHold, OutcomeReject},
		"judge_passes_status_valid":    {JudgePassComplete, JudgePassFailed, judgePassLegacy, JudgePassPartial, JudgePassRunning},
	} {
		slices.Sort(want)
		if got := vocab(constraint); !slices.Equal(got, want) {
			t.Errorf("%s allows %v, the Track record counts %v: count the new value in modelTrackRecord or leave it out on purpose", constraint, got, want)
		}
	}
}
