package decide

// Postgres proof of decide's grouping (CONTRACT §2.19 "Agreeing and
// conflicting findings"): agreeing findings become duplicates of one decided
// representative — never rejections — conflicts are ranked by p, a re-run
// does not flip-flop, rejected findings stay out, and undoing a
// representative returns its duplicates to proposed. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see integration_test.go).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
)

const (
	gT     = "00000000-0000-0000-0000-0000000000e2"
	gGemma = "00000000-0000-0000-0000-0000000000b1" // older recipe, oldest
	gLuna  = "00000000-0000-0000-0000-0000000000b2" // current propose recipe
	gNewer = "00000000-0000-0000-0000-0000000000b3" // older recipe, newest
	gRej   = "00000000-0000-0000-0000-0000000000b4" // the same fix, already rejected
	gConf  = "00000000-0000-0000-0000-0000000000c1" // confident, low p
	gLikly = "00000000-0000-0000-0000-0000000000c2" // less confident, higher p
	gLate  = "00000000-0000-0000-0000-0000000000b5" // proposed after the first run
	gHuman = "00000000-0000-0000-0000-0000000000a1" // a person accepted this fix
)

// groupRecipes registers the gemma propose recipe and makes luna's current.
func groupRecipes(t *testing.T, d *db.DB) (gemma, luna string) {
	t.Helper()
	ctx := context.Background()
	g := recipe.Recipe{Step: recipe.StepPropose, StepVersion: 2, CodeVersion: "test", ModelAlias: "gemma-4",
		ModelResolved: "gemma-4", PromptVersion: "judge@v2", PromptSHA256: "aa"}
	l := g
	l.ModelAlias, l.ModelResolved, l.PromptVersion = "openrouter/openai/gpt-6-luna", "gpt-6-luna", "judge@v3"
	var err error
	if gemma, err = d.RegisterRecipe(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := d.SetCurrentRecipes(ctx, l); err != nil {
		t.Fatal(err)
	}
	if luna, err = l.ID(); err != nil {
		t.Fatal(err)
	}
	return gemma, luna
}

type groupRow struct {
	id, recipe, issue, orig, repl, state, decidedBy string
	chunk                                           int
	conf                                            float64
	created                                         time.Time
}

func seedGroupRows(t *testing.T, conn *pgx.Conn, rows ...groupRow) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rows {
		text := runText
		if r.chunk == 1 {
			text = libText
		}
		var by, at any
		if r.decidedBy != "" {
			by, at = r.decidedBy, r.created
		}
		if _, err := conn.Exec(ctx, `
			INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
			       issue_type, suggested_correction, confidence, model, patch_state, origin, chunk_text_sha256,
			       anchor_offset, anchor_occurrence, decided_by, decided_at, recipe_id, created_at)
			VALUES ($1, $2, '/b/PHM/02.m4b', $3, 0, 6, $4, $5, $6, $7, 'judge-model', $8, 'judge', $9, $10, 0, $11, $12, $13, $14)`,
			r.id, gT, r.chunk, r.orig, r.issue, r.repl, r.conf, r.state, patch.ChunkHash(text),
			patch.Occurrences(text, r.orig)[0], by, at, r.recipe, r.created); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

// seedGroups: one transcript, two chunks. Chunk 0 holds three proposals of
// the same fix by two recipes (one differing only in case and surrounding
// whitespace, an NBSP included — the same correction_key) and a rejected one; chunk 1 holds two
// different fixes to the same word.
func seedGroups(t *testing.T, d *db.DB, conn *pgx.Conn) (gemma, luna string) {
	t.Helper()
	ctx := context.Background()
	gemma, luna = groupRecipes(t, d)
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000000b', '/b/PHM/02.m4b', 'c2', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name) VALUES
		  ('`+gT+`', '00000000-0000-0000-0000-00000000000b', '/b/PHM/02.m4b', 'c2', 'en', 10, '[]', 'x', 'parakeet');
		INSERT INTO transcript_chunks (id, transcript_id, file_path, chunk_index, start_sec, end_sec, text, source_text, embedding) VALUES
		  ('00000000-0000-0000-0000-0000000000d0', '`+gT+`', '/b/PHM/02.m4b', 0, 0, 6, '`+runText+`', '`+runText+`', array_fill(0.1, ARRAY[768])::vector),
		  ('00000000-0000-0000-0000-0000000000d1', '`+gT+`', '/b/PHM/02.m4b', 1, 6, 10, '`+libText+`', '`+libText+`', array_fill(0.1, ARRAY[768])::vector);
		INSERT INTO book_metadata (book_dir, title, asin, chapters) VALUES
		  ('/b/PHM', 'Project Hail Mary', 'B08GB58KD5', '[{"Index":0,"Title":"The Arecibo Message","StartSec":0,"EndSec":6}]')
		  ON CONFLICT DO NOTHING;`); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedGroupRows(t, conn,
		groupRow{id: gGemma, recipe: gemma, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: "Arecibo", state: "proposed", conf: 0.9, created: base},
		groupRow{id: gLuna, recipe: luna, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: "Arecibo", state: "proposed", conf: 0.6, created: base.Add(time.Hour)},
		groupRow{id: gNewer, recipe: gemma, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: " arecibo\u00a0", state: "proposed", conf: 0.8, created: base.Add(2 * time.Hour)},
		groupRow{id: gRej, recipe: gemma, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: "Arecibo", state: "rejected",
			decidedBy: "mcp:bob", conf: 0.9, created: base},
		groupRow{id: gConf, recipe: gemma, issue: IssueMisheardWord, orig: "walked", repl: "worked", state: "proposed", chunk: 1, conf: 0.95, created: base},
		groupRow{id: gLikly, recipe: luna, issue: IssueMisheardWord, orig: "walked", repl: "woked", state: "proposed", chunk: 1, conf: 0.5, created: base},
	)
	return gemma, luna
}

// groupAsker: the Arecibo fix is confident; of the two fixes to "walked",
// the less confident one is the likelier.
func groupAsker() *fakeAsker {
	return &fakeAsker{p: func(state string) (float64, error) {
		switch {
		case strings.Contains(state, "[[woked]]"):
			return 0.6, nil
		case strings.Contains(state, "[[worked]]"):
			return 0.3, nil
		default:
			return 0.97, nil
		}
	}}
}

type findingRow struct{ state, by, dupOf string }

func groupStates(t *testing.T, conn *pgx.Conn) map[string]findingRow {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT id::text, patch_state, COALESCE(decided_by, ''), COALESCE(duplicate_of::text, '')
		FROM transcript_findings WHERE transcript_id = $1`, gT)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]findingRow{}
	var id string
	var r findingRow
	if _, err := pgx.ForEachRow(rows, []any{&id, &r.state, &r.by, &r.dupOf}, func() error { out[id] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIntegrationDecideDuplicatesAndConflicts(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	seedGroups(t, d, conn)
	asker := groupAsker()
	opts := ApplyOptions{Concurrency: 2, Params: DefaultShouldApplyParams()}

	rep, err := Apply(ctx, d, asker, opts)
	if err != nil {
		t.Fatal(err)
	}
	actor := "jev:" + rep.RecipeID
	if n := countOf(t, conn, `SELECT step_version FROM recipes WHERE recipe_id = $1`, rep.RecipeID); n != ShouldApplyStepVersion {
		t.Errorf("decide recipe step_version %d", n)
	}
	s := groupStates(t, conn)
	// Luna's finding — the current propose recipe — represents the group,
	// though gNewer is newer and gGemma more confident.
	if s[gLuna] != (findingRow{"accepted", actor, ""}) {
		t.Errorf("representative %+v", s[gLuna])
	}
	for _, id := range []string{gGemma, gNewer} {
		if s[id] != (findingRow{"duplicate", actor, gLuna}) {
			t.Errorf("%s: %+v, want a duplicate of the representative (never a rejection)", id, s[id])
		}
	}
	if s[gRej].state != "rejected" || s[gRej].dupOf != "" {
		t.Errorf("an already-rejected finding left its state: %+v", s[gRej])
	}
	// The conflict: the more confident fix has the lower p and loses.
	if s[gConf].state != "rejected" || s[gLikly].state != "proposed" {
		t.Errorf("conflict: confident %+v, likely %+v", s[gConf], s[gLikly])
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1 AND kind = 'decision'
		AND outcome = 'reject' AND reason = 'overlap_dup' AND related_finding_id = $2 AND p = 0.3`, gConf, gLikly); n != 1 {
		t.Errorf("the overlap loser's decision must name its winner and keep its p (%d)", n)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE kind = 'decision' AND outcome = 'duplicate'
		AND reason = 'duplicate' AND related_finding_id = $1 AND actor = $2`, gLuna, actor); n != 2 {
		t.Errorf("%d duplicate decisions naming the representative, want 2", n)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE kind = 'transition' AND from_state = 'proposed'
		AND to_state = 'duplicate' AND reason = 'duplicate' AND actor = $1`, actor); n != 2 {
		t.Errorf("%d duplicate transitions with reason duplicate, want 2", n)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE kind = 'transition' AND to_state = 'accepted' AND reason IS NOT NULL`); n != 0 {
		t.Error("the duplicate reason leaked onto another transition of the transaction")
	}
	if asker.calls != 3 || rep.Write.Duplicates != 2 || rep.Grouping.Groups != 1 || rep.Grouping.Losers != 1 {
		t.Errorf("calls %d write %+v grouping %+v (duplicates are never asked)", asker.calls, rep.Write, rep.Grouping)
	}

	// A re-run asks nothing and moves nothing: no flip-flop.
	before := groupStates(t, conn)
	events := countOf(t, conn, `SELECT count(*) FROM finding_events`)
	again, err := Apply(ctx, d, asker, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Findings != 0 || asker.calls != 3 || countOf(t, conn, `SELECT count(*) FROM finding_events`) != events {
		t.Errorf("re-run decided %d with %d calls", again.Findings, asker.calls)
	}
	for id, r := range groupStates(t, conn) {
		if before[id] != r {
			t.Errorf("re-run moved %s: %+v → %+v", id, before[id], r)
		}
	}

	// A later proposal of the same fix joins the group under the accepted
	// representative.
	seedGroupRows(t, conn, groupRow{id: gLate, recipe: mustGemma(t, conn), issue: IssueMisheardProperNoun,
		orig: "auto sebo", repl: "Arecibo", state: "proposed", conf: 0.99, created: time.Now()})
	late, err := Apply(ctx, d, asker, opts)
	if err != nil {
		t.Fatal(err)
	}
	s = groupStates(t, conn)
	if s[gLate] != (findingRow{"duplicate", actor, gLuna}) || s[gLuna].state != "accepted" || late.Write.Accepted != 0 || asker.calls != 3 {
		t.Errorf("late %+v rep %+v write %+v calls %d", s[gLate], s[gLuna], late.Write, asker.calls)
	}

	// Undoing the representative returns its duplicates to proposed.
	rr, err := d.RevertDecisions(ctx, db.RevertScope{FindingID: gLuna}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Moved != 1 || rr.Cascaded != 3 || rr.Transitions["duplicate->proposed"] != 3 || rr.Transitions["accepted->rejected"] != 1 {
		t.Errorf("revert %+v", rr)
	}
	s = groupStates(t, conn)
	for _, id := range []string{gGemma, gNewer, gLate} {
		if s[id] != (findingRow{"proposed", "revert:" + actor, ""}) {
			t.Errorf("%s after the representative's undo: %+v", id, s[id])
		}
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE kind = 'transition' AND from_state = 'duplicate'
		AND to_state = 'proposed' AND reason = 'duplicate_released' AND actor = $1`, "revert:"+actor); n != 3 {
		t.Errorf("%d release transitions, want 3", n)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.outcome = 'duplicate'
		AND NOT EXISTS (SELECT 1 FROM finding_events v WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`); n != 0 {
		t.Errorf("%d duplicate decisions still live after the cascade", n)
	}
	// Released, they are this recipe's work again; a second undo is a no-op.
	work, err := d.DecideWork(ctx, db.DecideWorkScope{RecipeID: rep.RecipeID, RetryReason: ReasonJevUnavailable, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range work {
		got[f.ID] = true
	}
	if !got[gGemma] || !got[gNewer] || !got[gLate] || got[gLuna] || got[gRej] {
		t.Errorf("work after the undo %v", got)
	}
	if again, err := d.RevertDecisions(ctx, db.RevertScope{FindingID: gLuna}, true); err != nil || again.Moved != 0 || again.Cascaded != 0 {
		t.Errorf("second undo %+v, %v", again, err)
	}
}

func mustGemma(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var id string
	if err := conn.QueryRow(context.Background(), `SELECT recipe_id FROM recipes WHERE model_alias = 'gemma-4'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// `decide revert --recipe` undoes the recipe's grouping with its decisions:
// duplicates it made return to proposed, in scope rather than by cascade.
func TestIntegrationRevertRecipeReleasesDuplicates(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	seedGroups(t, d, conn)
	rep, err := Apply(ctx, d, groupAsker(), ApplyOptions{Concurrency: 2, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	dry, err := d.RevertDecisions(ctx, db.RevertScope{RecipeID: rep.RecipeID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Transitions["duplicate->proposed"] != 2 || dry.Cascaded != 0 {
		t.Errorf("dry run %+v", dry)
	}
	rr, err := d.RevertDecisions(ctx, db.RevertScope{RecipeID: rep.RecipeID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Transitions["duplicate->proposed"] != 2 || rr.Transitions["accepted->rejected"] != 1 || rr.Transitions["rejected->proposed"] != 1 {
		t.Errorf("revert %+v", rr)
	}
	s := groupStates(t, conn)
	if s[gGemma].state != "proposed" || s[gGemma].dupOf != "" || s[gNewer].state != "proposed" || s[gLuna].state != "rejected" {
		t.Errorf("states %+v", s)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.recipe_id = $1
		AND NOT EXISTS (SELECT 1 FROM finding_events v WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`, rep.RecipeID); n != 0 {
		t.Errorf("%d live decisions of the reverted recipe", n)
	}
}

// A person's accepted fix represents the group: a new proposal of the same
// fix is its duplicate, not an overlaps_overlay reject.
func TestIntegrationDuplicateOfAcceptedFix(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	gemma, _ := seedGroups(t, d, conn)
	if _, err := conn.Exec(ctx, `DELETE FROM transcript_findings WHERE transcript_id = $1`, gT); err != nil {
		t.Fatal(err)
	}
	seedGroupRows(t, conn,
		groupRow{id: gHuman, recipe: gemma, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: "Arecibo", state: "accepted",
			decidedBy: "mcp:alice", conf: 0.5, created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		groupRow{id: gLuna, recipe: gemma, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: "Arecibo", state: "proposed",
			conf: 0.9, created: time.Now()},
	)
	asker := groupAsker()
	rep, err := Apply(ctx, d, asker, ApplyOptions{Concurrency: 1, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	s := groupStates(t, conn)
	if s[gLuna] != (findingRow{"duplicate", "jev:" + rep.RecipeID, gHuman}) || s[gHuman] != (findingRow{"accepted", "mcp:alice", ""}) {
		t.Errorf("states %+v", s)
	}
	if asker.calls != 0 || rep.Write.Rejected != 0 || rep.Write.Duplicates != 1 {
		t.Errorf("calls %d write %+v", asker.calls, rep.Write)
	}
}
