package db

// Postgres integration tests for migration 00013's audit lineage
// (docs/design/audit-lineage.md): the judge_passes backfill, the transcript
// archive a requeue writes, clear-as-supersede, origin_transcript_id,
// correction_key, the duplicate state and the reanchor events. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const (
	alRecipeA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	alRecipeB     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	alRecipeB2    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	alJob1        = "00000000-0000-0000-0000-0000000a1001"
	alJob2        = "00000000-0000-0000-0000-0000000a1002"
	alJob3        = "00000000-0000-0000-0000-0000000a1003"
	alJob4        = "00000000-0000-0000-0000-0000000a1004"
	alTranscript1 = "00000000-0000-0000-0000-0000000a1101"
	alTranscript2 = "00000000-0000-0000-0000-0000000a1102"
	alTranscript3 = "00000000-0000-0000-0000-0000000a1103"
	alTranscript4 = "00000000-0000-0000-0000-0000000a1104"
	alJob5        = "00000000-0000-0000-0000-0000000a1005"
	alTranscript5 = "00000000-0000-0000-0000-0000000a1105"
	alRecipeL     = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func providerFor(t *testing.T, dbURL string) *goose.Provider {
	t.Helper()
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := newMigrationProvider(sqlDB, testLog())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// seedAuditLineageAt12 writes, on a schema at version 12:
//
//	transcript 1: judge findings from recipe A (2, one rejected), unstamped (1),
//	              a human correction, and a latch;
//	transcript 2: latched, no findings, eval_model naming exactly one propose recipe (A);
//	transcript 3: latched, no findings, eval_model naming two propose recipes (B, B2);
//	transcript 4: neither findings nor a latch;
//	transcript 5: findings from recipe A, then a latch by the newest recipe (L),
//	              which found nothing;
//	plus a finding a requeue already archived (transcript_id NULL).
func seedAuditLineageAt12(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	_, err := conn.Exec(context.Background(), `
		INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias, model_resolved) VALUES
		  ('`+alRecipeA+`',  'propose', 2, 'test', 'gemma3:12b', 'gemma3:12b-it-q4'),
		  ('`+alRecipeB+`',  'propose', 2, 'test', 'qwen3:27b',  NULL),
		  ('`+alRecipeB2+`', 'propose', 3, 'test', 'qwen3:27b',  NULL),
		  ('`+alRecipeL+`',  'propose', 3, 'test', 'llama4:17b', NULL);
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('`+alJob1+`', '/b/AL/1.m4b', 'al1', 'done'),
		  ('`+alJob2+`', '/b/AL/2.m4b', 'al2', 'done'),
		  ('`+alJob3+`', '/b/AL/3.m4b', 'al3', 'done'),
		  ('`+alJob4+`', '/b/AL/4.m4b', 'al4', 'done'),
		  ('`+alJob5+`', '/b/AL/5.m4b', 'al5', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name) VALUES
		  ('`+alTranscript1+`', '`+alJob1+`', '/b/AL/1.m4b', 'al1', 'en', 60, '[]', 'one', 'parakeet'),
		  ('`+alTranscript2+`', '`+alJob2+`', '/b/AL/2.m4b', 'al2', 'en', 60, '[]', 'two', 'parakeet'),
		  ('`+alTranscript3+`', '`+alJob3+`', '/b/AL/3.m4b', 'al3', 'en', 60, '[]', 'three', 'parakeet'),
		  ('`+alTranscript4+`', '`+alJob4+`', '/b/AL/4.m4b', 'al4', 'en', 60, '[]', 'four', 'parakeet'),
		  ('`+alTranscript5+`', '`+alJob5+`', '/b/AL/5.m4b', 'al5', 'en', 60, '[]', 'five', 'parakeet');
		INSERT INTO run_metrics (job_id, eval_started_at, eval_finished_at, eval_model, eval_chunks) VALUES
		  ('`+alJob1+`', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', 'gemma3:12b', 10),
		  ('`+alJob2+`', '2026-01-02T00:00:00Z', '2026-01-02T01:00:00Z', 'gemma3:12b-it-q4', 7),
		  ('`+alJob3+`', NULL,                   '2026-01-03T01:00:00Z', 'qwen3:27b', 5),
		  ('`+alJob5+`', '2026-01-04T00:00:00Z', '2026-01-04T01:00:00Z', 'llama4:17b', 9);
		INSERT INTO transcript_findings (transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
		       issue_type, suggested_correction, confidence, model, origin, patch_state, recipe_id, chunk_text_sha256, created_at) VALUES
		  ('`+alTranscript1+`', '/b/AL/1.m4b', 0, 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'judge', 'proposed', '`+alRecipeA+`', 'h0', '2026-01-01T00:10:00Z'),
		  ('`+alTranscript1+`', '/b/AL/1.m4b', 1, 30, 60, 'said',  'misheard_word',        'sad',     0.4, 'gemma3:12b', 'judge', 'rejected', '`+alRecipeA+`', 'h1', '2026-01-01T00:20:00Z'),
		  ('`+alTranscript1+`', '/b/AL/1.m4b', 1, 30, 60, 'left',  'misheard_word',        'leapt',   0.4, 'old',        'judge', 'proposed', NULL,              NULL, '2025-12-01T00:00:00Z'),
		  ('`+alTranscript1+`', '/b/AL/1.m4b', 1, 30, 60, 'left',  'misheard_word',        'lift',    1,   'manual',     'human', 'accepted', NULL,              NULL, '2026-01-05T00:00:00Z'),
		  ('`+alTranscript5+`', '/b/AL/5.m4b', 0, 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'judge', 'proposed', '`+alRecipeA+`', 'h5', '2025-11-01T00:00:00Z');
		INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text, issue_type,
		       confidence, model, origin, patch_state) VALUES
		  (NULL, '/b/AL/gone.m4b', 0, 1, 'x', 'other', 0.5, 'm', 'judge', 'superseded');
	`)
	if err != nil {
		t.Fatalf("seed at 12: %v", err)
	}
}

// TestIntegrationAuditLineageMigration: 00013 on a populated version-12
// database backfills one legacy judge pass per (transcript, recipe) of judge
// findings, one per latched transcript with none (recipe resolved only when
// unambiguous), and links every finding with a live transcript to its pass
// and origin; Down returns exactly the version-12 schema (duplicates back to
// proposed) and Up runs again cleanly.
func TestIntegrationAuditLineageMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := newTestDatabase(t)
	provider := providerFor(t, dbURL)
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("up to 12: %v", err)
	}
	conn := connect(t, dbURL)
	seedAuditLineageAt12(t, conn)

	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("up to 13: %v", err)
	}

	type pass struct {
		transcript, recipe, status string
		kept                       int
		chunks                     *int
	}
	rows, err := conn.Query(ctx, `
		SELECT transcript_id::text, COALESCE(recipe_id, ''), status, findings_kept, chunks_total
		  FROM judge_passes ORDER BY transcript_id, recipe_id NULLS LAST`)
	if err != nil {
		t.Fatal(err)
	}
	passes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pass, error) {
		var p pass
		return p, r.Scan(&p.transcript, &p.recipe, &p.status, &p.kept, &p.chunks)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []pass{
		{alTranscript1, alRecipeA, "legacy", 2, nil},
		{alTranscript1, "", "legacy", 1, nil},
		{alTranscript2, alRecipeA, "legacy", 0, ptr(7)},
		{alTranscript3, "", "legacy", 0, ptr(5)}, // two qwen3:27b recipes: unattributable
		{alTranscript5, alRecipeA, "legacy", 1, nil},
		{alTranscript5, alRecipeL, "legacy", 0, ptr(9)}, // newest recipe found nothing
	}
	if len(passes) != len(want) {
		t.Fatalf("judge_passes = %+v, want %d rows", passes, len(want))
	}
	for i, w := range want {
		g := passes[i]
		if g.transcript != w.transcript || g.recipe != w.recipe || g.status != w.status || g.kept != w.kept ||
			(g.chunks == nil) != (w.chunks == nil) || (g.chunks != nil && *g.chunks != *w.chunks) {
			t.Errorf("pass %d = %+v, want %+v", i, g, w)
		}
	}
	if n := countRows(t, conn, `SELECT count(*) FROM judge_passes
		WHERE transcript_id = $1 AND started_at = '2026-01-03T01:00:00Z' AND finished_at = started_at`, alTranscript3); n != 1 {
		t.Error("a latch with no eval_started_at must start and finish at eval_finished_at")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM judge_passes
		WHERE transcript_id = $1 AND recipe_id IS NOT NULL
		  AND started_at = '2026-01-01T00:10:00Z' AND finished_at = '2026-01-01T00:20:00Z'
		  AND job_id = $2 AND file_path = '/b/AL/1.m4b'`, alTranscript1, alJob1); n != 1 {
		t.Error("a findings pass must span its first..last finding and carry the job and path")
	}

	// Every judge finding with a live transcript points at its (transcript,
	// recipe) pass; the human one and the archived orphan at none.
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings f
		JOIN judge_passes p ON p.id = f.judge_pass_id
		WHERE p.transcript_id = f.transcript_id AND p.recipe_id IS NOT DISTINCT FROM f.recipe_id
		  AND p.findings_kept > 0`); n != 4 {
		t.Errorf("%d findings linked to their legacy findings pass, want 4", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE judge_pass_id IS NOT NULL AND origin <> 'judge'`); n != 0 {
		t.Error("a human correction was linked to a judge pass")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE transcript_id IS NOT NULL AND origin_transcript_id IS DISTINCT FROM transcript_id`); n != 0 {
		t.Error("origin_transcript_id not backfilled from transcript_id")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE transcript_id IS NULL AND (origin_transcript_id IS NOT NULL OR judge_pass_id IS NOT NULL)`); n != 0 {
		t.Error("an already-archived finding cannot be attributed and must stay NULL")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE chunk_text_sha256 IS NULL AND correction_key IS NOT NULL`); n != 0 {
		t.Error("correction_key must be NULL without a chunk hash")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE chunk_text_sha256 IS NOT NULL AND correction_key IS NULL`); n != 0 {
		t.Error("correction_key must be set whenever the chunk hash is")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_constraint WHERE conname = 'transcript_findings_patch_state_valid'
		AND pg_get_constraintdef(oid) LIKE '%''duplicate''%' AND pg_get_constraintdef(oid) LIKE '%''superseded''%'
		AND pg_get_constraintdef(oid) LIKE '%''unanchorable''%'`); n != 1 {
		t.Error("patch_state CHECK must gain duplicate and keep every earlier state")
	}

	// Down: a duplicate returns to proposed (recorded), and the schema is
	// exactly version 12's.
	var rep, dup string
	if err := conn.QueryRow(ctx, `SELECT id::text FROM transcript_findings WHERE recipe_id = $1 AND patch_state = 'proposed'`,
		alRecipeA).Scan(&rep); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT id::text FROM transcript_findings WHERE recipe_id IS NULL AND origin = 'judge'
		AND transcript_id IS NOT NULL`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'duplicate', duplicate_of = $2 WHERE id = $1`,
		dup, rep); err != nil {
		t.Fatalf("mark duplicate: %v", err)
	}
	if _, err := provider.DownTo(ctx, 12); err != nil {
		t.Fatalf("down to 12: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE id = $1 AND patch_state = 'proposed'`, dup); n != 1 {
		t.Error("Down must return a duplicate to proposed")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1
		AND from_state = 'duplicate' AND to_state = 'proposed' AND reason LIKE 'migration 13 down%'`, dup); n != 1 {
		t.Error("Down's duplicate -> proposed move must be recorded with its reason")
	}
	at12 := newTestDatabase(t)
	if err := migrateTo(itCtx(t), at12, testLog(), 12); err != nil {
		t.Fatal(err)
	}
	requireSameSchema(t, "down to 12", schemaSnapshot(t, dbURL), "fresh at 12", schemaSnapshot(t, at12))

	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM judge_passes`); n != 6 {
		t.Errorf("re-Up backfilled %d passes, want 6", n)
	}
}

// TestIntegrationRequeueArchivesTranscript: a requeue copies the transcript —
// raw_text, segments, metadata and the run_metrics row — into
// transcript_archive before deleting it; its findings keep
// origin_transcript_id (which then joins to the archive) after the foreign key
// nulls transcript_id, and the supersede transitions carry reason 'requeue'.
func TestIntegrationRequeueArchivesTranscript(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	seedRequeueRows(t, dbURL)
	conn := connect(t, dbURL)
	if _, err := conn.Exec(ctx, `UPDATE transcripts SET segments = '[{"start":0,"end":1,"text":"ganema said"}]',
		asr_runner_version = 'v9', asr_params = '{"dtype":"bf16"}', embedded_asin = 'B00TEST' WHERE id = $1`,
		rqTranscriptA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RequeueByID(WithActor(ctx, "mcp:dashboard"), rqJobA); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	var (
		tid, rawText, reason, segText, runner, asin, embedModel string
		hasEval                                                 bool
	)
	if err := conn.QueryRow(ctx, `
		SELECT transcript_id::text, raw_text, archived_reason, segments->0->>'text', asr_runner_version,
		       embedded_asin, run_metrics->>'embed_model', run_metrics ? 'eval_finished_at'
		  FROM transcript_archive WHERE job_id = $1`, rqJobA).
		Scan(&tid, &rawText, &reason, &segText, &runner, &asin, &embedModel, &hasEval); err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if tid != rqTranscriptA || rawText != "ganema said" || reason != "requeue" || segText != "ganema said" ||
		runner != "v9" || asin != "B00TEST" || embedModel != "nomic-embed-text" || !hasEval {
		t.Errorf("archive row = (%s %q %s %q %s %s %s eval=%v)", tid, rawText, reason, segText, runner, asin, embedModel, hasEval)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_archive`); n != 1 {
		t.Errorf("%d archive rows, want only book A's", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_archive WHERE archived_by = 'mcp:dashboard'`); n != 1 {
		t.Error("archived_by must be the caller's actor")
	}

	// origin_transcript_id survives the FK's SET NULL and joins to the archive.
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings f
		JOIN transcript_archive a ON a.transcript_id = f.origin_transcript_id
		WHERE f.transcript_id IS NULL AND f.patch_state = 'superseded'`); n != 7 {
		t.Errorf("%d archived findings join to the archived transcript by origin, want 7", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events
		WHERE kind = 'transition' AND to_state = 'superseded' AND reason = 'requeue'
		  AND actor = 'mcp:dashboard' AND transcript_id = $1`, rqTranscriptA); n != 7 {
		t.Errorf("%d supersede transitions carry reason 'requeue' by the caller, want 7", n)
	}

	// The origin is frozen once set.
	_, err := conn.Exec(ctx, `UPDATE transcript_findings SET origin_transcript_id = $1
		WHERE origin_transcript_id = $2`, rqTranscriptB, rqTranscriptA)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23001" {
		t.Errorf("changing origin_transcript_id: err = %v, want restrict_violation", err)
	}
}

// TestIntegrationClearFindingsSupersedes: a clear archives, it does not
// delete — rows and their history stay, each move is a 'cleared' transition
// by the dashboard — and a re-judge of the cleared transcript is not deduped
// against the archived rows.
func TestIntegrationClearFindingsSupersedes(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	seedRequeueRows(t, dbURL)
	conn := connect(t, dbURL)
	if _, err := conn.Exec(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec,
		original_text, issue_type, suggested_correction, confidence, model, origin, patch_state, unanchorable_reason)
		VALUES ($1, '/b/Author/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9,
		        'gemma3:12b', 'judge', 'unanchorable', 'anchor_ambiguous')`, rqTranscriptA); err != nil {
		t.Fatal(err)
	}
	// The applied correction sits on chunk 0; the projection is up to date.
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET chunk_index = 0
		WHERE transcript_id = $1 AND patch_state = 'applied'`, rqTranscriptA); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE transcript_chunks SET embedding_stale = false`); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, conn, `SELECT count(*) FROM transcript_findings`)

	n, err := d.ClearFindings(ctx, "/b/Author/A/", testClearActor)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n != 8 {
		t.Errorf("cleared %d, want book A's 8", n)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_findings`); got != before {
		t.Errorf("a clear deleted rows: %d -> %d", before, got)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE transcript_id = $1
		AND patch_state = 'superseded' AND superseded_at IS NOT NULL AND unanchorable_reason IS NULL`, rqTranscriptA); got != 8 {
		t.Errorf("%d of 8 book-A findings superseded in place (transcript_id kept)", got)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE transcript_id = $1
		AND kind = 'transition' AND to_state = 'superseded' AND reason = 'cleared' AND actor = $2`,
		rqTranscriptA, testClearActor); got != 8 {
		t.Errorf("%d 'cleared' transitions, want 8", got)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE transcript_id = $1
		AND patch_state = 'proposed'`, rqTranscriptB); got != 1 {
		t.Error("a clear of book A touched book B")
	}
	// The withdrawn applied correction's chunk is flagged for rebuild; the
	// others are not.
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_chunks WHERE embedding_stale`); got != 1 {
		t.Errorf("%d chunks flagged, want only A's chunk 0", got)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_chunks
		WHERE transcript_id = $1 AND chunk_index = 0 AND embedding_stale`, rqTranscriptA); got != 1 {
		t.Error("clearing an applied correction must flag its chunk embedding_stale")
	}
	// Cleared rows stay out of the live readers.
	sum, err := d.GetFindingsSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.TotalFindings != 1 {
		t.Errorf("summary counts %d findings, want only book B's live one", sum.TotalFindings)
	}

	// Re-running eval after a clear proposes again: no dedupe keys left.
	keys, err := d.GetFindingKeys(ctx, rqTranscriptA)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("cleared transcript still has %d dedupe keys", len(keys))
	}
	corr := "Ghanima"
	if err := d.InsertFindings(ctx, []Finding{{TranscriptID: rqTranscriptA, FilePath: "/b/Author/A/01.m4b",
		StartSec: 0, EndSec: 30, OriginalText: "ganema", IssueType: "misheard_proper_noun",
		SuggestedCorrection: &corr, Confidence: 0.9, Model: "gemma3:12b"}}); err != nil {
		t.Fatalf("re-propose: %v", err)
	}
	if keys, _ := d.GetFindingKeys(ctx, rqTranscriptA); len(keys) != 1 {
		t.Errorf("re-proposed finding gives %d keys, want 1", len(keys))
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE transcript_id = $1
		AND origin_transcript_id = $1 AND patch_state = 'proposed'`, rqTranscriptA); n != 1 {
		t.Error("the trigger must fill origin_transcript_id on insert")
	}
	// The new finding keeps the overlay and stale_work honest: neither counts
	// the archived ones.
	overlay, _, err := d.GetCorrectionOverlay(ctx, rqTranscriptA)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 0 {
		t.Errorf("cleared accepted/applied findings still replay: %d", len(overlay))
	}

	// Idempotent: nothing left to clear in A except the fresh proposal.
	if n, err := d.ClearFindings(ctx, "/b/Author/A", testClearActor); err != nil || n != 1 {
		t.Errorf("second clear = %d, %v; want only the re-proposed finding", n, err)
	}
	// A requeue after a clear still archives the transcript and nulls the
	// cleared rows' transcript_id without re-superseding them.
	if _, err := d.RequeueByID(ctx, rqJobA); err != nil {
		t.Fatalf("requeue after clear: %v", err)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE reason = 'requeue'`); got != 0 {
		t.Errorf("requeue re-archived %d already-cleared findings", got)
	}
}

// TestIntegrationReanchorEvents: moving a finding's anchor appends a
// 'reanchor_moved' transition carrying the OLD hash and offset; one that also
// changes the state appends it before the state transition; an UPDATE that
// leaves the anchor alone appends none. A transaction's earmark.reason lands
// on its transitions.
func TestIntegrationReanchorEvents(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	id := seedFindingEvents(t, conn, 1)[0]
	// A legacy finding's first anchoring is a placement, not a move.
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET chunk_text_sha256 = 'h-old', anchor_offset = 3 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1
		AND reason = 'reanchor_placed' AND anchor_offset IS NULL AND chunk_text_sha256 IS NULL`, id); n != 1 {
		t.Error("first anchoring of an unanchored finding must be 'reanchor_placed'")
	}
	baseline := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1`, id)

	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET chunk_text_sha256 = 'h-new', anchor_offset = 11 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var from, to, hash, reason, actor string
	var off int
	if err := conn.QueryRow(ctx, `SELECT from_state, to_state, chunk_text_sha256, anchor_offset, reason, actor
		FROM finding_events WHERE finding_id = $1 ORDER BY id DESC LIMIT 1`, id).
		Scan(&from, &to, &hash, &off, &reason, &actor); err != nil {
		t.Fatal(err)
	}
	if from != "proposed" || to != "proposed" || reason != "reanchor_moved" || actor != "system" {
		t.Errorf("reanchor event = %s->%s %s by %s, want proposed->proposed reanchor_moved by system", from, to, reason, actor)
	}
	if hash != "h-old" || off != 3 {
		t.Errorf("reanchor event anchor = %s@%d, want the OLD h-old@3", hash, off)
	}

	// Unrelated update: no event.
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET confidence = 0.8, anchor_offset = 11 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1`, id); got != baseline+1 {
		t.Errorf("events %d, want %d (one reanchor, nothing for an unchanged anchor)", got, baseline+1)
	}

	// Park it, then a re-anchor that places it: two events, reanchor first.
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'unanchorable',
		unanchorable_reason = 'anchor_not_found' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := SetEventContext(ctx, tx, EventContext{Actor: "cli:reanchor", Reason: "placed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'proposed', unanchorable_reason = NULL,
		chunk_text_sha256 = 'h-3', anchor_offset = 20 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT from_state, to_state, COALESCE(reason, ''), actor, COALESCE(anchor_offset, -1)
		FROM finding_events WHERE finding_id = $1 ORDER BY id DESC LIMIT 2`, id)
	if err != nil {
		t.Fatal(err)
	}
	type ev struct {
		from, to, reason, actor string
		off                     int
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ev, error) {
		var e ev
		return e, r.Scan(&e.from, &e.to, &e.reason, &e.actor, &e.off)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantEvs := []ev{
		{"unanchorable", "proposed", "placed", "cli:reanchor", -1},
		{"unanchorable", "unanchorable", "reanchor_moved", "cli:reanchor", 11},
	}
	if len(evs) != 2 || evs[0] != wantEvs[0] || evs[1] != wantEvs[1] {
		t.Errorf("last two events (newest first) = %+v, want %+v", evs, wantEvs)
	}
	// Point in time still ends on the new state.
	if set, err := d.TranscriptPatchSetAt(ctx, feTranscript, dbNow(t, conn)); err != nil || len(set) != 0 {
		t.Errorf("patch set = %v, %v; want none (the finding is proposed)", set, err)
	}
}

// testClearActor is the attribution the tests pass to ClearFindings.
const testClearActor = "mcp:dashboard"

// newTestDatabaseCLocale is newTestDatabase with LC_COLLATE/LC_CTYPE = C, the
// way the live cluster was initdb'd.
func newTestDatabaseCLocale(t *testing.T) string {
	t.Helper()
	admin := integrationServer(t)
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "earmark_itc_" + hex.EncodeToString(b[:])
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name+
		" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestIntegrationCorrectionKey: the key is recipe-independent and
// normalises case and surrounding space, separates its parts so shifting a
// character between original and correction changes it, treats a NULL
// correction as ”, and is NULL without a chunk hash.
func TestIntegrationCorrectionKey(t *testing.T) {
	ctx := context.Background()
	// The live cluster is initdb'd LC_CTYPE=C / LC_COLLATE=C: build the test
	// database the same way, so plain lower() would fold ASCII only.
	dbURL := newTestDatabaseCLocale(t)
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn := connect(t, dbURL)
	var ctype, plain string
	if err := conn.QueryRow(ctx, `SELECT datctype, lower('ÉMILE') FROM pg_database WHERE datname = current_database()`).
		Scan(&ctype, &plain); err != nil {
		t.Fatal(err)
	}
	if ctype != "C" || plain != "Émile" {
		t.Fatalf("test database is not a C-locale database (ctype %q, lower('ÉMILE') = %q)", ctype, plain)
	}
	seedFindingEvents(t, conn, 0)
	if _, err := conn.Exec(ctx, `INSERT INTO recipes (recipe_id, step, step_version, code_version) VALUES
		('`+alRecipeA+`', 'propose', 1, 't'), ('`+alRecipeB+`', 'propose', 1, 't')`); err != nil {
		t.Fatal(err)
	}
	key := func(recipe *string, hash *string, original string, correction *string) *string {
		t.Helper()
		var k *string
		if err := conn.QueryRow(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, chunk_index, start_sec,
			end_sec, original_text, issue_type, suggested_correction, confidence, model, recipe_id, chunk_text_sha256)
			VALUES ($1, '/b/FE/01.m4b', 0, 0, 30, $2, 'misheard_word', $3, 0.5, 'm', $4, $5)
			RETURNING correction_key`, feTranscript, original, correction, recipe, hash).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	a, b := alRecipeA, alRecipeB
	h := "h1"
	fix, fixLoud := "Ghanima", "  GHANIMA "
	k1 := key(&a, &h, "ganema", &fix)
	k2 := key(&b, &h, " Ganema", &fixLoud)
	if k1 == nil || k2 == nil || *k1 != *k2 {
		t.Errorf("two recipes, same fix on the same chunk: keys %s vs %s", deref(k1), deref(k2))
	}
	same := func(name, o1, c1, o2, c2 string, want bool) {
		t.Helper()
		x, y := key(&a, &h, o1, &c1), key(&b, &h, o2, &c2)
		if x == nil || y == nil || (*x == *y) != want {
			t.Errorf("%s: keys %s vs %s, want equal=%v", name, deref(x), deref(y), want)
		}
	}
	same("non-ASCII case folds (ICU, not the C locale)", "ÉMILE", "ÉMILE ZOLA", "émile", "émile zola", true)
	same("tabs, newlines and runs of spaces collapse", "the\tcat", "the  cat\n", "the cat", " the cat", true)
	same("NBSP and other Unicode spaces collapse", "the\u00a0cat", "a\u2009b", "the cat", "a b", true)
	same("accents are not folded", "emile", "Emile", "émile", "Émile", false)
	c, bc := "c", "bc"
	if x, y := key(&a, &h, "ab", &c), key(&a, &h, "a", &bc); *x == *y {
		t.Error("('ab','c') and ('a','bc') must not share a key")
	}
	other := "h2"
	if x := key(&a, &other, "ganema", &fix); *x == *k1 {
		t.Error("a different chunk revision must change the key")
	}
	empty := ""
	if x, y := key(&a, &h, "the the", nil), key(&a, &h, "the the", &empty); x == nil || y == nil || *x != *y {
		t.Errorf("a NULL correction must key like '': %s vs %s", deref(x), deref(y))
	}
	if x := key(&a, nil, "ganema", &fix); x != nil {
		t.Errorf("no chunk hash, key = %v, want NULL", *x)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_collation WHERE collname = 'und-x-icu' AND collprovider = 'i'`); n != 1 {
		t.Error("the ICU root collation und-x-icu is missing")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_indexes WHERE indexname = 'transcript_findings_correction_key_idx'`); n != 1 {
		t.Error("correction_key is not indexed")
	}
}

// TestIntegrationDuplicateState: 'duplicate' is a legal state carrying
// duplicate_of; duplicate_of is refused on any other live state, survives
// an archive, must be cleared when the duplicate returns to proposed, and a
// duplicate never replays.
func TestIntegrationDuplicateState(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 3)
	rep, dup, other := ids[0], ids[1], ids[2]
	exec := func(sql string, args ...any) error {
		_, err := conn.Exec(ctx, sql, args...)
		return err
	}
	isCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514"
	}
	if err := exec(`UPDATE transcript_findings SET patch_state = 'duplicate', duplicate_of = $2 WHERE id = $1`, dup, rep); err != nil {
		t.Fatalf("mark duplicate: %v", err)
	}
	if err := exec(`UPDATE transcript_findings SET duplicate_of = $2 WHERE id = $1`, other, rep); !isCheck(err) {
		t.Errorf("duplicate_of on a proposed finding: err = %v, want a CHECK violation", err)
	}
	if err := exec(`UPDATE transcript_findings SET patch_state = 'accepted' WHERE id = $1`, rep); err != nil {
		t.Fatal(err)
	}
	overlay, _, err := d.GetCorrectionOverlay(ctx, feTranscript)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 1 || overlay[0].ID != rep {
		t.Errorf("overlay = %+v, want only the representative", overlay)
	}
	if err := exec(`UPDATE transcript_findings SET patch_state = 'proposed' WHERE id = $1`, dup); !isCheck(err) {
		t.Errorf("undo without clearing duplicate_of: err = %v, want a CHECK violation", err)
	}
	if err := exec(`UPDATE transcript_findings SET patch_state = 'proposed', duplicate_of = NULL WHERE id = $1`, dup); err != nil {
		t.Errorf("undo clearing duplicate_of: %v", err)
	}
	if err := exec(`UPDATE transcript_findings SET patch_state = 'duplicate', duplicate_of = $2 WHERE id = $1`, dup, rep); err != nil {
		t.Fatal(err)
	}
	if n, err := d.ClearFindings(ctx, "/b/FE", testClearActor); err != nil || n != 3 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE id = $1
		AND patch_state = 'superseded' AND duplicate_of = $2`, dup, rep); got != 1 {
		t.Error("archiving a duplicate must keep which finding it agreed with")
	}
	if got := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1
		AND from_state = 'proposed' AND to_state = 'duplicate'`, dup); got != 2 {
		t.Errorf("%d proposed->duplicate transitions recorded, want 2", got)
	}
}

// TestIntegrationClearFindingsEscapesBookDir: a book directory's '_' and '%'
// are literals — clearing Book_1 must not reach BookX1 or Book%1.
func TestIntegrationClearFindingsEscapesBookDir(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	for i, dir := range []string{"/b/A/Book_1", "/b/A/BookX1", "/b/A/Book_1x"} {
		path := dir + "/01.m4b"
		var tid string
		if err := conn.QueryRow(ctx, `
			WITH j AS (INSERT INTO transcription_jobs (file_path, checksum, status) VALUES ($1, $2, 'done') RETURNING id)
			INSERT INTO transcripts (job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name)
			SELECT j.id, $1, $2, 'en', 1, '[]', 'x', 'm' FROM j RETURNING id::text`, path, "esc"+string(rune('0'+i))).Scan(&tid); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec,
			original_text, issue_type, confidence, model) VALUES ($1, $2, 0, 1, 'x', 'other', 0.5, 'm')`, tid, path); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.ClearFindings(ctx, "/b/A/Book_1", testClearActor)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleared %d findings, want only Book_1's", n)
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE patch_state = 'superseded' AND file_path = '/b/A/Book_1/01.m4b'`); got != 1 {
		t.Error("Book_1's finding was not cleared")
	}
	if got := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE patch_state = 'proposed'`); got != 2 {
		t.Errorf("%d other books' findings untouched, want 2 (BookX1, Book_1x)", got)
	}
}

// TestIntegrationLiveReadersSkipClearedAndDuplicates: findings a clear
// archived (transcript_id kept) and duplicates stay out of the findings
// summary, the decide candidates and stale_work's propose arm.
func TestIntegrationLiveReadersSkipClearedAndDuplicates(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 4)
	if _, err := conn.Exec(ctx, `
		INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias) VALUES
		  ('`+alRecipeA+`', 'propose', 1, 't', 'old'), ('`+alRecipeB+`', 'propose', 2, 't', 'new');
		INSERT INTO current_recipes (step, recipe_id) VALUES ('propose', '`+alRecipeB+`');
		UPDATE transcript_findings SET recipe_id = '`+alRecipeA+`', chunk_text_sha256 = 'h', anchor_offset = 0;`); err != nil {
		t.Fatal(err)
	}
	// ids[0] stays live; ids[1] becomes a duplicate of it; ids[2..3] are
	// archived the way a clear leaves them (superseded, transcript_id kept).
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'duplicate', duplicate_of = $2 WHERE id = $1`,
		ids[1], ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'superseded', superseded_at = now()
		WHERE id = ANY($1)`, []string{ids[2], ids[3]}); err != nil {
		t.Fatal(err)
	}

	sum, err := d.GetFindingsSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.TotalFindings != 1 {
		t.Errorf("summary counts %d, want 1 (cleared and duplicate excluded)", sum.TotalFindings)
	}
	counts, err := d.GetFindingsCountByBook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["/b/FE"] != 1 {
		t.Errorf("per-book count = %v, want /b/FE: 1", counts)
	}
	sample, err := d.DecideSample(ctx, DecideScope{Sample: 50, Seed: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sample) != 1 || sample[0].ID != ids[0] {
		t.Errorf("decide candidates = %d, want only the live proposed finding", len(sample))
	}
	if got := countRows(t, conn, `SELECT count(*) FROM stale_work WHERE step = 'propose'`); got != 1 {
		t.Errorf("stale_work propose arm lists %d findings, want only the live one", got)
	}
}
