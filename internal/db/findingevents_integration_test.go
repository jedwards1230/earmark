package db

// Postgres integration tests for finding version history (migration 8,
// CONTRACT §2.17 "Version history"). Skipped unless EARMARK_TEST_DATABASE_URL
// is set (see migrate_integration_test.go).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jedwards1230/earmark/internal/patch"
)

const (
	feJob        = "00000000-0000-0000-0000-0000000fe001"
	feTranscript = "00000000-0000-0000-0000-0000000fe0a1"
	feChunk0     = "00000000-0000-0000-0000-0000000fe0c0"
	feChunk1     = "00000000-0000-0000-0000-0000000fe0c1"
)

// seedFindingEvents makes one transcript with two chunks and returns the ids
// of n proposed judge findings on chunk 0 (finding i also on chunk i%2).
func seedFindingEvents(t *testing.T, conn *pgx.Conn, n int) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES ('`+feJob+`', '/b/FE/01.m4b', 'fe', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name)
		VALUES ('`+feTranscript+`', '`+feJob+`', '/b/FE/01.m4b', 'fe', 'en', 60, '[]', 'x', 'parakeet');
		INSERT INTO transcript_chunks (id, transcript_id, file_path, chunk_index, start_sec, end_sec, text, source_text, embedding) VALUES
		  ('`+feChunk0+`', '`+feTranscript+`', '/b/FE/01.m4b', 0, 0, 30, 'ganema said', 'ganema said', array_fill(0.1, ARRAY[768])::vector),
		  ('`+feChunk1+`', '`+feTranscript+`', '/b/FE/01.m4b', 1, 30, 60, 'and left', 'and left', array_fill(0.2, ARRAY[768])::vector);`); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	ids := make([]string, n)
	for i := range ids {
		if err := conn.QueryRow(ctx, `
			INSERT INTO transcript_findings (transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
			       issue_type, suggested_correction, confidence, model, origin, patch_state)
			VALUES ($1, '/b/FE/01.m4b', $2, 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9, 'm', 'judge', 'proposed')
			RETURNING id::text`, feTranscript, i%2).Scan(&ids[i]); err != nil {
			t.Fatalf("seed finding: %v", err)
		}
	}
	return ids
}

// registerTestRecipe registers a decide recipe and returns its id.
func registerTestRecipe(t *testing.T, conn *pgx.Conn, id string, version int) {
	t.Helper()
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO recipes (recipe_id, step, step_version, code_version) VALUES ($1, 'decide', $2, 'test')`,
		id, version); err != nil {
		t.Fatalf("register recipe: %v", err)
	}
}

type transitionRow struct{ from, to, actor, recipe string }

func transitions(t *testing.T, conn *pgx.Conn, findingID string) []transitionRow {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT COALESCE(from_state, ''), to_state, actor, COALESCE(recipe_id, '')
		  FROM finding_events WHERE finding_id = $1 AND kind = 'transition' ORDER BY id`, findingID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (transitionRow, error) {
		var x transitionRow
		return x, r.Scan(&x.from, &x.to, &x.actor, &x.recipe)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func chunkStale(t *testing.T, conn *pgx.Conn, id string) bool {
	t.Helper()
	var b bool
	if err := conn.QueryRow(context.Background(), `SELECT embedding_stale FROM transcript_chunks WHERE id = $1`, id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIntegrationFindingEventsRecordTransitions: every path that changes a
// finding's state leaves a transition row, attributed per the trigger's rule.
func TestIntegrationFindingEventsRecordTransitions(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 4)
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events`); n != 0 {
		t.Fatalf("proposed inserts recorded %d events, want 0", n)
	}

	// Human accept, then the rebuild's apply and revert.
	if err := d.SetPatchState(ctx, ids[0], patch.StateProposed, patch.StateAccepted, "mcp:justin"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkFindingsApplied(ctx, []AppliedFinding{{ID: ids[0], Before: "ganema", After: "Ghanima"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPatchState(ctx, ids[0], patch.StateApplied, patch.StateReverted, "mcp:justin"); err != nil {
		t.Fatal(err)
	}
	// Reconsider stamps no decision: attributed through SetEventContext.
	if err := d.SetPatchState(ctx, ids[0], patch.StateReverted, patch.StateProposed, "mcp:sam"); err != nil {
		t.Fatal(err)
	}
	want := []transitionRow{
		{patch.StateProposed, patch.StateAccepted, "mcp:justin", ""},
		{patch.StateAccepted, patch.StateApplied, "system", ""},
		{patch.StateApplied, patch.StateReverted, "mcp:justin", ""},
		{patch.StateReverted, patch.StateProposed, "mcp:sam", ""},
	}
	if got := transitions(t, conn, ids[0]); !equalRows(got, want) {
		t.Errorf("single-row history = %+v\nwant %+v", got, want)
	}
	// The setting was transaction-local: a later machine move is 'system'.
	if err := d.MarkFindingsStale(ctx, []string{ids[3]}, patch.StaleReasonChunkChanged); err != nil {
		t.Fatal(err)
	}
	if got := transitions(t, conn, ids[3]); len(got) != 1 || got[0].actor != "system" || got[0].to != patch.StateStale {
		t.Errorf("stale retire history = %+v", got)
	}

	// Bulk accept by a registered decide recipe.
	recipe := strings.Repeat("1a", 32)
	registerTestRecipe(t, conn, recipe, 1)
	if _, err := conn.Exec(ctx, `UPDATE transcript_chunks SET embedding_stale = false`); err != nil {
		t.Fatal(err)
	}
	res, err := d.SetPatchStateBulk(ctx, patch.StateProposed, patch.StateAccepted, "jev:"+recipe, []string{ids[1], ids[2]})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("bulk = %+v", res)
	}
	for _, id := range ids[1:3] {
		got := transitions(t, conn, id)
		if len(got) != 1 || got[0] != (transitionRow{patch.StateProposed, patch.StateAccepted, "jev:" + recipe, recipe}) {
			t.Errorf("bulk history of %s = %+v", id, got)
		}
	}
	if !chunkStale(t, conn, feChunk0) || !chunkStale(t, conn, feChunk1) {
		t.Error("bulk accept must flag both findings' chunks embedding_stale")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE id = ANY($1) AND decided_by = $2 AND decided_at IS NOT NULL`, ids[1:3], "jev:"+recipe); n != 2 {
		t.Errorf("bulk stamped %d decisions, want 2", n)
	}

	// A bulk decision by an unregistered recipe fails: the trigger's recipe_id is a FK.
	if _, err := d.SetPatchStateBulk(ctx, patch.StateAccepted, patch.StateRejected,
		"jev:"+strings.Repeat("ee", 32), []string{ids[1]}); err == nil {
		t.Error("a decision by an unregistered recipe must fail")
	}
	if findingState(t, d, ids[1]) != patch.StateAccepted {
		t.Error("the failed bulk must have rolled back")
	}

	// A human's direct correction is inserted accepted: an insert transition.
	var manual string
	if err := conn.QueryRow(ctx, `
		INSERT INTO transcript_findings (transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
		       issue_type, suggested_correction, confidence, model, origin, patch_state, decided_at, decided_by)
		VALUES ($1, '/b/FE/01.m4b', 1, 30, 60, 'left', 'misheard_word', 'leapt', 1, 'manual', 'human', 'accepted', now(), 'mcp:justin')
		RETURNING id::text`, feTranscript).Scan(&manual); err != nil {
		t.Fatal(err)
	}
	if got := transitions(t, conn, manual); len(got) != 1 || got[0] != (transitionRow{"", patch.StateAccepted, "mcp:justin", ""}) {
		t.Errorf("manual correction history = %+v", got)
	}
}

func equalRows(a, b []transitionRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIntegrationFindingEventsAppendOnly: an event cannot be edited or
// deleted directly; deleting its finding still cascades.
func TestIntegrationFindingEventsAppendOnly(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 2)
	if err := d.SetPatchState(ctx, ids[0], patch.StateProposed, patch.StateRejected, "mcp:justin"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE finding_events SET actor = 'forged'`); err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE must be refused as append-only, got %v", err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM finding_events`); err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE must be refused as append-only, got %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE actor = 'mcp:justin'`); n != 1 {
		t.Fatalf("the event must survive the refused writes, got %d", n)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM transcript_findings WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("deleting a finding must cascade its history: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events`); n != 0 {
		t.Errorf("%d events survived their finding", n)
	}
}

// TestIntegrationBulkSkipsConcurrentChange: a finding decided by someone else
// between the caller's read and the bulk write is skipped, not clobbered.
func TestIntegrationBulkSkipsConcurrentChange(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 3)
	recipe := strings.Repeat("2b", 32)
	registerTestRecipe(t, conn, recipe, 1)

	// A reviewer rejects ids[1] first, holding the row lock while the bulk
	// statement starts; the bulk waits, re-checks and skips it.
	rev, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rev.Rollback(ctx) }()
	if _, err := rev.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'rejected', decided_at = now(),
		decided_by = 'mcp:justin' WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	type out struct {
		res BulkResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := d.SetPatchStateBulk(ctx, patch.StateProposed, patch.StateAccepted, "jev:"+recipe, ids)
		done <- out{r, err}
	}()
	time.Sleep(300 * time.Millisecond) // let the bulk block on ids[1]'s lock
	if err := rev.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	o := <-done
	if o.err != nil {
		t.Fatal(o.err)
	}
	if len(o.res.Changed) != 2 || len(o.res.Skipped) != 1 || o.res.Skipped[0] != ids[1] {
		t.Fatalf("bulk = %+v, want ids[1] skipped", o.res)
	}
	if got := findingState(t, d, ids[1]); got != patch.StateRejected {
		t.Errorf("the reviewer's decision was clobbered: %s", got)
	}
	if got := transitions(t, conn, ids[1]); len(got) != 1 || got[0].actor != "mcp:justin" {
		t.Errorf("skipped finding history = %+v, want only the reviewer's reject", got)
	}
}

// TestIntegrationFindingHistoryAndPointInTime: decisions, revokes and
// transitions read back as one history, the patch set is reconstructable at
// any time, and stale_work's decide arm follows the live decisions.
func TestIntegrationFindingHistoryAndPointInTime(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 3)
	oldRecipe, newRecipe := strings.Repeat("3c", 32), strings.Repeat("4d", 32)
	registerTestRecipe(t, conn, oldRecipe, 1)
	registerTestRecipe(t, conn, newRecipe, 2)

	before := dbNow(t, conn)
	p := 0.97
	write := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		tx, err := d.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := fn(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	write(func(tx pgx.Tx) error {
		if _, err := InsertDecisionEvents(ctx, tx, []DecisionEvent{
			{FindingID: ids[0], RecipeID: oldRecipe, Outcome: OutcomeApply, Reason: "asin verbatim", P: &p,
				Evidence: EvidenceASINVerbatim, ChunkTextSHA256: patch.ChunkHash("ganema said")},
			{FindingID: ids[1], RecipeID: oldRecipe, Outcome: OutcomeHold, Reason: "no evidence", Evidence: EvidenceNone},
		}); err != nil {
			return err
		}
		_, err := setPatchStateBulkTx(ctx, tx, patch.StateProposed, patch.StateAccepted, "jev:"+oldRecipe, ids[:1])
		return err
	})
	afterAccept := dbNow(t, conn)

	// Before any decision the set is empty; after, ids[0] alone (ids[1] is held).
	if set, err := d.TranscriptPatchSetAt(ctx, feTranscript, before); err != nil || len(set) != 0 {
		t.Fatalf("patch set before = %+v, %v", set, err)
	}
	set, err := d.TranscriptPatchSetAt(ctx, feTranscript, afterAccept)
	if err != nil || len(set) != 1 || set[0].FindingID != ids[0] || set[0].State != patch.StateAccepted ||
		set[0].Actor != "jev:"+oldRecipe {
		t.Fatalf("patch set after accept = %+v, %v", set, err)
	}

	// stale_work: with newRecipe current, both live decisions of oldRecipe are stale.
	if _, err := conn.Exec(ctx, `INSERT INTO current_recipes (step, recipe_id) VALUES ('decide', $1)`, newRecipe); err != nil {
		t.Fatal(err)
	}
	decideStale := func() int {
		return countRows(t, conn, `SELECT count(*) FROM stale_work WHERE step = 'decide'`)
	}
	if n := decideStale(); n != 2 {
		t.Errorf("decide stale_work = %d, want 2", n)
	}

	// Undo the recipe: revoke its decisions, move its accept back.
	write(func(tx pgx.Tx) error {
		n, err := RevokeEvents(ctx, tx, oldRecipe, "cli:justin")
		if err != nil || n != 2 {
			return errors.Join(err, errors.New("want 2 revoked"))
		}
		_, err = setPatchStateBulkTx(ctx, tx, patch.StateAccepted, patch.StateRejected, "revert:jev:"+oldRecipe, ids[:1])
		return err
	})
	afterUndo := dbNow(t, conn)
	// Revoking again appends nothing.
	write(func(tx pgx.Tx) error {
		n, err := RevokeEvents(ctx, tx, oldRecipe, "cli:justin")
		if err == nil && n != 0 {
			err = errors.New("second revoke appended rows")
		}
		return err
	})

	if n := decideStale(); n != 0 {
		t.Errorf("decide stale_work after revoke = %d, want 0", n)
	}
	if set, err := d.TranscriptPatchSetAt(ctx, feTranscript, afterUndo); err != nil || len(set) != 0 {
		t.Errorf("patch set after undo = %+v, %v", set, err)
	}
	// The past is still the past.
	if set, err := d.TranscriptPatchSetAt(ctx, feTranscript, afterAccept); err != nil || len(set) != 1 {
		t.Errorf("patch set at afterAccept after undo = %+v, %v", set, err)
	}

	h, err := d.FindingHistory(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range h {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "decision,transition,revoke,transition" {
		t.Fatalf("history kinds = %v", kinds)
	}
	dec, acc, rev, undo := h[0], h[1], h[2], h[3]
	if !dec.Revoked || dec.Outcome != OutcomeApply || dec.P == nil || *dec.P != p ||
		dec.Evidence != EvidenceASINVerbatim || dec.Actor != "jev:"+oldRecipe || dec.IssueType != "misheard_proper_noun" ||
		dec.TranscriptID != feTranscript {
		t.Errorf("decision event = %+v", dec)
	}
	if acc.FromState != patch.StateProposed || acc.ToState != patch.StateAccepted || acc.RecipeID != oldRecipe {
		t.Errorf("accept transition = %+v", acc)
	}
	if rev.RevokesEventID == nil || *rev.RevokesEventID != dec.ID || rev.Actor != "cli:justin" {
		t.Errorf("revoke event = %+v", rev)
	}
	if undo.ToState != patch.StateRejected || undo.Actor != "revert:jev:"+oldRecipe || undo.RecipeID != oldRecipe {
		t.Errorf("undo transition = %+v", undo)
	}
}

func dbNow(t *testing.T, conn *pgx.Conn) time.Time {
	t.Helper()
	var ts time.Time
	if err := conn.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestIntegrationFindingEventsBackfill: migrating a database with decided
// findings gives each one transition, dated by its decision.
func TestIntegrationFindingEventsBackfill(t *testing.T) {
	ctx := context.Background()
	dbURL := newTestDatabase(t)
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := newMigrationProvider(sqlDB, testLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("up to 7: %v", err)
	}
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 3)
	for _, u := range []struct{ id, state, at, by string }{
		{ids[0], "applied", "2026-01-02T03:04:05Z", "mcp:justin"},
		{ids[1], "rejected", "2026-01-03T00:00:00Z", ""},
	} {
		if _, err := conn.Exec(ctx, `UPDATE transcript_findings
			SET patch_state = $2, decided_at = $3::timestamptz, decided_by = NULLIF($4, '') WHERE id = $1`,
			u.id, u.state, u.at, u.by); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events`); n != 2 {
		t.Fatalf("backfilled %d events, want 2 (one per decided finding)", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events
		WHERE finding_id = $1 AND to_state = 'applied' AND actor = 'mcp:justin'
		  AND created_at = '2026-01-02T03:04:05Z' AND transcript_id = $2`, ids[0], feTranscript); n != 1 {
		t.Error("applied finding not backfilled with its decider and decision time")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1 AND actor = 'unknown'`, ids[1]); n != 1 {
		t.Error("a decision with no decided_by must backfill as 'unknown'")
	}
	// Down 8 removes everything; Up again is clean.
	if _, err := provider.DownTo(ctx, 7); err != nil {
		t.Fatalf("down to 7: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_proc WHERE proname LIKE 'finding_events_%'`); n != 0 {
		t.Errorf("%d trigger functions survived Down", n)
	}
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}
