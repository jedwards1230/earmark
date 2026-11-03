package db

// Postgres proofs for the second-ASR pilot tables (migration 00017, CONTRACT
// §1.11). Skipped unless EARMARK_TEST_DATABASE_URL; see
// migrate_integration_test.go.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// TestIntegrationASRPilot: a pilot transcript round-trips; only held findings
// (proposed, latest unrevoked decision a hold) are listed, with their pristine
// chunk text; votes upsert (re-running replaces); the tallies count the
// current vote per model; the scope is part of the unique key; and deleting
// the production transcript cascades to its pilot transcripts and votes.
func TestIntegrationASRPilot(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	tID, path := seedTranscript(t, d)
	if _, err := d.pool.Exec(ctx, `INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index,
		start_sec, end_sec, text, source_text, embedding)
		VALUES ($1, $2, 0, 0, 5, 'the portrait of Dorian Gray', 'the portrait of dorian grey', array_fill(0.1, ARRAY[768])::vector)`,
		tID, path); err != nil {
		t.Fatal(err)
	}
	var chunkID string
	if err := d.pool.QueryRow(ctx, `SELECT id::text FROM transcript_chunks WHERE transcript_id = $1`, tID).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	held := seedFinding(t, d, tID, chunkID, 0, "proposed")
	undecided := seedFinding(t, d, tID, chunkID, 0, "proposed")
	revoked := seedFinding(t, d, tID, chunkID, 0, "proposed")
	// hold, then a later reject: no longer held (the state is left proposed
	// on purpose, so the decision rule alone must exclude it).
	rejectedLater := seedFinding(t, d, tID, chunkID, 0, "proposed")
	// hold, then a reject that was revoked: the hold is the latest live
	// decision again, so it is held.
	heldAgain := seedFinding(t, d, tID, chunkID, 0, "proposed")
	sha := patch.ChunkHash("the portrait of dorian grey")
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_findings SET original_text = 'dorian grey',
		suggested_correction = 'dorian gray', anchor_offset = 16, chunk_text_sha256 = $2 WHERE transcript_id = $1`, tID, sha); err != nil {
		t.Fatal(err)
	}
	r := strings.Repeat("a7", 32)
	registerTestRecipe(t, conn, r, 3)
	for _, f := range []string{held, revoked, rejectedLater, heldAgain} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id)
			VALUES ($1, $2, 'decision', 'hold', 'no_evidence', $3, $4)`, f, tID, "jev:"+r, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, actor, revokes_event_id)
		SELECT finding_id, transcript_id, 'revoke', 'revert:test', id FROM finding_events
		 WHERE finding_id = $1 AND kind = 'decision'`, revoked); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{rejectedLater, heldAgain} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id)
			VALUES ($1, $2, 'decision', 'reject', 'jev_reject', $3, $4)`, f, tID, "jev:"+r, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, actor, revokes_event_id)
		SELECT finding_id, transcript_id, 'revoke', 'revert:test', id FROM finding_events
		 WHERE finding_id = $1 AND kind = 'decision' AND outcome = 'reject'`, heldAgain); err != nil {
		t.Fatal(err)
	}

	page, err := d.HeldFindings(ctx, tID, FirstFindingID, MaxPilotPage)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range page {
		got[h.ID] = true
	}
	if len(page) != 2 || !got[held] || !got[heldAgain] {
		t.Fatalf("held = %v, want %s and %s (held again after its reject was revoked); not %s undecided, %s revoked, %s rejected later",
			got, held, heldAgain, undecided, revoked, rejectedLater)
	}
	var h HeldFinding
	for _, x := range page {
		if x.ID == held {
			h = x
		}
	}
	if !h.ChunkFound || h.ChunkText != "the portrait of dorian grey" || h.ChunkTextSHA256 != sha || h.AnchorOffset != 16 ||
		h.AnchorOccurrence != -1 || h.ChunkEndSec != 5 || h.SuggestedCorrection != "dorian gray" {
		t.Errorf("held finding = %+v (want the pristine source_text)", h)
	}
	if _, err := d.HeldFindings(ctx, tID, FirstFindingID, MaxPilotPage+1); err == nil {
		t.Error("a page over MaxPilotPage must be refused")
	}

	segs := `[{"id":0,"start":0,"end":5,"text":"The portrait of Dorian Gray.","words":[{"word":"The"},{"word":"portrait"}]}]`
	insertPilot := func(model, scope string) int64 {
		var id int64
		if err := d.pool.QueryRow(ctx, `INSERT INTO pilot_transcripts (file_path, production_transcript_id, backend,
			model_id, model_revision, scope, has_word_times, segments, raw_text, audio_sec, wall_sec, rtfx, peak_vram_mib, peak_reserved_mib)
			VALUES ($1, $2, 'canary', $3, 'rev1', $4, false, $5::jsonb, 'x', 5, 1, 5.1, 9000, 12000) RETURNING id`,
			path, tID, model, scope, segs).Scan(&id); err != nil {
			t.Fatalf("insert pilot %s/%s: %v", model, scope, err)
		}
		return id
	}
	full := insertPilot("canary", "full")
	findings := insertPilot("canary", "findings") // same track+model+revision, other scope: allowed
	other := insertPilot("granite", "full")
	if _, err := d.pool.Exec(ctx, `INSERT INTO pilot_transcripts (file_path, production_transcript_id, backend,
		model_id, model_revision, has_word_times, segments, raw_text) VALUES ($1, $2, 'canary', 'canary', 'rev1', false, '[]', 'x')`,
		path, tID); err == nil {
		t.Error("a second (track, model, revision, scope) row must violate the unique key")
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO pilot_transcripts (file_path, production_transcript_id, backend,
		model_id, model_revision, scope, has_word_times, segments, raw_text) VALUES ($1, $2, 'x', 'x', 'r', 'partial', false, '[]', 'x')`,
		path, tID); err == nil {
		t.Error("an unknown scope must be refused")
	}

	list, err := d.PilotTranscripts(ctx, PilotFilter{ModelID: "canary", BookDirs: []string{"/books/a/b"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != full || list[0].ProductionTranscriptID != tID || list[0].Scope != "full" ||
		*list[0].RTFx != 5.1 || // float8: exact, not 5.0999999
		*list[0].PeakVRAMMiB != 9000 || list[1].Scope != "findings" {
		t.Errorf("pilots = %+v", list)
	}
	if l, _ := d.PilotTranscripts(ctx, PilotFilter{BookDirs: []string{"/elsewhere"}, Limit: 10}); len(l) != 0 {
		t.Errorf("book dir filter: %d rows", len(l))
	}
	ps, err := d.PilotSegments(ctx, full)
	if err != nil || len(ps) != 1 || ps[0].Words[0].Start != nil || ps[0].Text != "The portrait of Dorian Gray." {
		t.Errorf("pilot segments = %+v, %v (words without times must decode with nil Start)", ps, err)
	}

	t0, t1 := 36000.123, 36001.457 // 10 h in: float4 would round to ~4 ms
	detail, _ := json.Marshal(map[string]any{"rule": "asr_vote@v1"})
	vote := func(pilot int64, model, v string) FindingASRVote {
		return FindingASRVote{FindingID: held, PilotTranscriptID: pilot, ModelID: model, ChunkTextSHA256: sha,
			SpanStartSec: &t0, SpanEndSec: &t1, Vote: v, HeardText: "Dorian Gray", Detail: detail}
	}
	if n, err := d.UpsertFindingASRVotes(ctx, []FindingASRVote{vote(full, "canary", "neither"), vote(other, "granite", "heard_replacement")}); err != nil || n != 2 {
		t.Fatalf("upsert: %d, %v", n, err)
	}
	if n, err := d.UpsertFindingASRVotes(ctx, []FindingASRVote{vote(full, "canary", "heard_replacement")}); err != nil || n != 1 {
		t.Fatalf("re-upsert: %d, %v", n, err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_asr_votes`); n != 2 {
		t.Errorf("%d vote rows after re-upsert, want 2", n)
	}
	var gotT0 float64
	if err := conn.QueryRow(ctx, `SELECT span_start_sec FROM finding_asr_votes WHERE pilot_transcript_id = $1`, full).Scan(&gotT0); err != nil || gotT0 != t0 {
		t.Errorf("span_start_sec = %v, want %v exactly (%v)", gotT0, t0, err)
	}
	if pt, pv, err := d.PilotRowsForJobs(ctx, []string{jobOf(t, d, tID)}); err != nil || pt != 3 || pv != 2 {
		t.Errorf("PilotRowsForJobs = %d, %d, %v; want the 3 pilot transcripts and 2 votes a requeue would delete", pt, pv, err)
	}
	var gotDetail string
	if err := conn.QueryRow(ctx, `SELECT detail->>'rule' FROM finding_asr_votes WHERE pilot_transcript_id = $1`, full).Scan(&gotDetail); err != nil || gotDetail != "asr_vote@v1" {
		t.Errorf("detail rule %q, %v", gotDetail, err)
	}
	if _, err := d.UpsertFindingASRVotes(ctx, []FindingASRVote{vote(full, "canary", "maybe")}); err == nil {
		t.Error("an unknown vote must violate the CHECK")
	}
	// The same finding voted against the findings-scope transcript too: the
	// tally still counts the finding once per model (its latest vote).
	if _, err := d.UpsertFindingASRVotes(ctx, []FindingASRVote{vote(findings, "canary", "heard_original")}); err != nil {
		t.Fatal(err)
	}
	styleDetail, _ := json.Marshal(map[string]any{"rule": "asr_vote@v1", "style_vote": "heard_replacement"})
	sv := vote(other, "granite", "style_only")
	sv.FindingID, sv.Detail = heldAgain, styleDetail
	notHeld := vote(other, "granite", "heard_original")
	notHeld.FindingID = rejectedLater // voted, then decided: left out of the tallies
	if _, err := d.UpsertFindingASRVotes(ctx, []FindingASRVote{sv, notHeld}); err != nil {
		t.Fatal(err)
	}
	counts, err := d.PilotVoteCounts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []PilotVoteCount{{"canary", "rev1", "heard_original", "", 1}, {"granite", "rev1", "heard_replacement", "", 1},
		{"granite", "rev1", "style_only", "heard_replacement", 1}}
	if len(counts) != 3 || counts[0] != want[0] || counts[1] != want[1] || counts[2] != want[2] {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	pairs, err := d.PilotVotePairs(ctx, []string{"/books/a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].ModelA != "canary" || pairs[0].VoteA != "heard_original" ||
		pairs[0].ModelB != "granite" || pairs[0].VoteB != "heard_replacement" || pairs[0].N != 1 {
		t.Errorf("pairs = %+v", pairs)
	}

	// Requeue supersedes the findings and deletes the production transcript:
	// its pilots and their votes go with it.
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'superseded', superseded_at = now()
		WHERE transcript_id = $1`, tID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM transcripts WHERE id = $1`, tID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pilot_transcripts`) + countRows(t, conn, `SELECT count(*) FROM finding_asr_votes`); n != 0 {
		t.Errorf("%d pilot rows survived their production transcript", n)
	}
}

// TestIntegrationASRPilotDownRefuses: rolling 00017 back refuses while
// pilot_transcripts has rows, and succeeds once the session opts in.
func TestIntegrationASRPilotDownRefuses(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	tID, path := seedTranscript(t, d)
	if _, err := conn.Exec(ctx, `INSERT INTO pilot_transcripts (file_path, production_transcript_id, backend,
		model_id, model_revision, has_word_times, segments, raw_text) VALUES ($1, $2, 'granite', 'g', 'r', true, '[]', '')`,
		path, tID); err != nil {
		t.Fatal(err)
	}
	if _, err := providerFor(t, dbURL).DownTo(ctx, 15); err == nil || !strings.Contains(err.Error(), "refusing to roll back migration 17") {
		t.Fatalf("Down with pilot rows: err = %v, want a refusal", err)
	}
	var name string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+pgx.Identifier{name}.Sanitize()+` SET earmark.allow_data_loss = 'on'`); err != nil {
		t.Fatal(err)
	}
	if _, err := providerFor(t, dbURL).DownTo(ctx, 15); err != nil {
		t.Fatalf("opted-in Down: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_class WHERE relname IN ('pilot_transcripts', 'finding_asr_votes')`); n != 0 {
		t.Errorf("%d pilot tables left after Down", n)
	}
	if _, err := providerFor(t, dbURL).UpTo(ctx, 17); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}
