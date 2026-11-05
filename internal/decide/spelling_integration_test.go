package decide

// Postgres proof of the one-spelling-per-book pass end to end: a book whose
// findings spell one name two ways is held spelling_conflict by a dry run
// (writing no decision) and by --yes (decision events with the group in
// detail), and a revert by the recipe undoes it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/spelling"
)

const (
	spT    = "00000000-0000-0000-0000-0000000005e1"
	spText = "ilmus came in and later ilmus left the hall"
	spS1   = "00000000-0000-0000-0000-0000000005f1"
	spS2   = "00000000-0000-0000-0000-0000000005f2"
	spS3   = "00000000-0000-0000-0000-0000000005f3"
)

func seedSpelling(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000050a', '/b/Ilm/01.m4b', 'cs1', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name) VALUES
		  ('`+spT+`', '00000000-0000-0000-0000-00000000050a', '/b/Ilm/01.m4b', 'cs1', 'en', 6,
		   '[{"id":0,"start":0,"end":6,"text":"`+spText+`","words":[]}]', 'x', 'parakeet');
		INSERT INTO transcript_chunks (id, transcript_id, file_path, chunk_index, start_sec, end_sec, text, source_text, embedding) VALUES
		  ('00000000-0000-0000-0000-0000000005c0', '`+spT+`', '/b/Ilm/01.m4b', 0, 0, 6, '`+spText+`', '`+spText+`', array_fill(0.1, ARRAY[768])::vector);
		INSERT INTO book_metadata (book_dir, title, asin, description) VALUES
		  ('/b/Ilm', 'The Ilmas Saga', 'B000ILMAS0', 'Ilmas walks the hall.');`); err != nil {
		t.Fatal(err)
	}
	hash := patch.ChunkHash(spText)
	occ := patch.Occurrences(spText, "ilmus")
	for _, r := range []struct {
		id, repl string
		offset   int
	}{
		{spS1, "Ilmas", occ[0]}, // book reference spells it: asin_verbatim → would apply
		{spS2, "Ilmis", occ[1]}, // no evidence → would hold no_evidence
	} {
		if _, err := conn.Exec(ctx, `
			INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
			       issue_type, suggested_correction, confidence, model, patch_state, origin, chunk_text_sha256,
			       anchor_offset, anchor_occurrence)
			VALUES ($1, $2, '/b/Ilm/01.m4b', 0, 0, 6, 'ilmus', $3, $4, 0.9, 'judge-model', 'proposed', 'judge', $5, $6, -1)`,
			r.id, spT, IssueMisheardProperNoun, r.repl, hash, r.offset); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

func TestIntegrationSpellingConflictApplyRevert(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	seedSpelling(t, conn)
	asker := &fakeAsker{p: func(string) (float64, error) { return 0.97, nil }}

	before := snapshot(t, conn)
	dry, err := DryRun(ctx, d, asker, RunOptions{Scope: db.DecideScope{Sample: 10, Seed: "q4"}, Concurrency: 2,
		Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Reasons[ReasonSpellingConflict] != 2 || dry.Outcomes[DecisionHold] != 2 {
		t.Fatalf("dry run outcomes %v reasons %v, want both held spelling_conflict", dry.Outcomes, dry.Reasons)
	}
	after := snapshot(t, conn)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("dry run changed %s", k)
		}
	}

	rep, err := Apply(ctx, d, asker, ApplyOptions{Concurrency: 2, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	if w := rep.Write; w.Held != 2 || w.Accepted != 0 || w.Rejected != 0 {
		t.Fatalf("write %+v, want 2 held", w)
	}
	rows, err := conn.Query(ctx, `SELECT right(finding_id::text, 2), outcome, reason, p, detail FROM finding_events
		WHERE kind = 'decision' AND recipe_id = $1 ORDER BY finding_id`, rep.RecipeID)
	if err != nil {
		t.Fatal(err)
	}
	type ev struct {
		id, outcome, reason string
		p                   *float64
		detail              []byte
	}
	var evs []ev
	var e ev
	if _, err := pgx.ForEachRow(rows, []any{&e.id, &e.outcome, &e.reason, &e.p, &e.detail}, func() error {
		evs = append(evs, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("%d decision events, want 2", len(evs))
	}
	priors := map[string]string{"f1": ReasonConfident, "f2": ReasonNoEvidence}
	for _, e := range evs {
		if e.outcome != DecisionHold || e.reason != ReasonSpellingConflict || e.p == nil || *e.p != 0.97 {
			t.Errorf("%s: %s %s p %v", e.id, e.outcome, e.reason, e.p)
		}
		var dt spellingDetail
		if err := json.Unmarshal(e.detail, &dt); err != nil {
			t.Fatalf("%s detail %s: %v", e.id, e.detail, err)
		}
		if dt.Rung != spelling.DetailRung || dt.Rule != spelling.RuleID || dt.Action != spellingHeldConflict ||
			dt.PriorReason != priors[e.id] || !dt.Contradictory || len(dt.Targets) != 2 || dt.Strong != "" {
			t.Errorf("%s detail %+v", e.id, dt)
		}
	}
	if n := countOf(t, conn, `SELECT step_version FROM recipes WHERE recipe_id = $1`, rep.RecipeID); n != ShouldApplyStepVersion {
		t.Errorf("recipe step_version %d", n)
	}
	var rule string
	if err := conn.QueryRow(ctx, `SELECT params->>'spelling_rule' FROM recipes WHERE recipe_id = $1`, rep.RecipeID).Scan(&rule); err != nil || rule != spelling.RuleID {
		t.Errorf("recipe spelling_rule %q, %v", rule, err)
	}
	// A recipe's holds are final: a re-run asks nothing and writes nothing.
	again, err := Apply(ctx, d, asker, ApplyOptions{Concurrency: 2, Params: DefaultShouldApplyParams()})
	if err != nil || again.Findings != 0 {
		t.Errorf("re-run decided %d findings (%v)", again.Findings, err)
	}

	rr, err := d.RevertDecisions(ctx, db.RevertScope{RecipeID: rep.RecipeID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Revoked != 2 {
		t.Errorf("revert %+v, want both holds revoked", rr)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.recipe_id = $1
		AND NOT EXISTS (SELECT 1 FROM finding_events r WHERE r.kind = 'revoke' AND r.revokes_event_id = e.id)`, rep.RecipeID); n != 0 {
		t.Errorf("%d live decisions after revert", n)
	}
	if s := findingStates(t, conn); s["f1"] != "proposed|" || s["f2"] != "proposed|" {
		t.Errorf("states after revert %v", s)
	}
}

// With an applied finding establishing the spelling, a proposal toward it held
// no_evidence is promoted, and one away from it is held off-established.
func TestIntegrationSpellingEstablishedByApplied(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	seedSpelling(t, conn)
	// The applied finding lives on another track of the same book.
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000050b', '/b/Ilm/02.m4b', 'cs2', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name) VALUES
		  ('`+spS3[:len(spS3)-2]+`e2', '00000000-0000-0000-0000-00000000050b', '/b/Ilm/02.m4b', 'cs2', 'en', 6, '[]', 'x', 'parakeet');
		INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
		       issue_type, suggested_correction, confidence, model, patch_state, origin)
		VALUES ('`+spS3+`', '`+spS3[:len(spS3)-2]+`e2', '/b/Ilm/02.m4b', 0, 0, 6, 'ilmus', 'misheard_proper_noun', 'Ilmis', 0.9, 'm', 'applied', 'judge');`); err != nil {
		t.Fatal(err)
	}
	asker := &fakeAsker{p: func(string) (float64, error) { return 0.97, nil }}
	rep, err := Apply(ctx, d, asker, ApplyOptions{Concurrency: 1, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	if w := rep.Write; w.Accepted != 1 || w.Held != 1 {
		t.Fatalf("write %+v, want f2 (toward ilmis) promoted and f1 (ilmas) held", w)
	}
	reasons := map[string]string{}
	rows, err := conn.Query(ctx, `SELECT right(finding_id::text, 2), outcome || '/' || reason FROM finding_events
		WHERE kind = 'decision' AND recipe_id = $1`, rep.RecipeID)
	if err != nil {
		t.Fatal(err)
	}
	var k, v string
	if _, err := pgx.ForEachRow(rows, []any{&k, &v}, func() error { reasons[k] = v; return nil }); err != nil {
		t.Fatal(err)
	}
	if reasons["f1"] != "hold/"+ReasonSpellingOffEstablished || reasons["f2"] != "apply/"+ReasonSpellingEstablished {
		t.Errorf("reasons %v", reasons)
	}
	if s := findingStates(t, conn); s["f2"] != "accepted|jev:"+rep.RecipeID {
		t.Errorf("f2 state %q", s["f2"])
	}
}

// The original shadow-winner pair: "Arecibo" and "Arrecibo" are two spellings
// of one name on the same span. Neither is established, so the pass holds the
// winner spelling_conflict and conflict ranking then holds the loser
// overlap_pending naming it: nothing is applied or rejected.
func TestIntegrationSpellingConflictingShadowPair(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	gemma, _ := seedGroups(t, d, conn)
	if _, err := conn.Exec(ctx, `DELETE FROM transcript_findings WHERE transcript_id = $1`, gT); err != nil {
		t.Fatal(err)
	}
	seedGroupRows(t, conn,
		groupRow{id: gLuna, recipe: gemma, issue: IssueMisheardProperNoun, orig: "auto sebo", repl: "Arecibo", state: "proposed",
			conf: 0.5, created: time.Now()},
		groupRow{id: gConf, recipe: gemma, issue: IssueMisheardWord, orig: "auto sebo", repl: "Arrecibo", state: "proposed",
			conf: 0.9, created: time.Now()},
	)
	asker := &fakeAsker{p: func(state string) (float64, error) {
		if strings.Contains(state, "[[Arrecibo]]") {
			return 0.4, nil
		}
		return 0.97, nil
	}}
	rep, err := Apply(ctx, d, asker, ApplyOptions{Concurrency: 1, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	if w := rep.Write; w.Accepted != 0 || w.Rejected != 0 || w.Held != 2 {
		t.Fatalf("write %+v, want both held", w)
	}
	if s := groupStates(t, conn); s[gLuna].state != "proposed" || s[gConf].state != "proposed" {
		t.Errorf("states %+v", s)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1 AND kind = 'decision'
		AND outcome = 'hold' AND reason = $2 AND detail->>'rung' = 'spelling'`, gLuna, ReasonSpellingConflict); n != 1 {
		t.Errorf("%d spelling_conflict holds on the winner", n)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1 AND kind = 'decision'
		AND outcome = 'hold' AND reason = 'overlap_pending' AND related_finding_id = $2`, gConf, gLuna); n != 1 {
		t.Errorf("%d overlap_pending holds on the loser naming the held winner", n)
	}
}
