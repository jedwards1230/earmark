package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/patch"
)

// expectChunkInserts expects one upsert per chunk, in order.
func expectChunkInserts(mock pgxmock.PgxPoolIface, chunks []Chunk) {
	for _, c := range chunks {
		mock.ExpectExec("INSERT INTO transcript_chunks").
			WithArgs(c.ID, c.TranscriptID, c.FilePath, c.ChunkIndex, c.StartSec, c.EndSec,
				c.Text, c.SourceText, c.Speaker, pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
}

// TestInsertChunks_PrunesTailInSameTransaction: a re-chunk into fewer chunks
// must delete the old tail (chunk_index >= the new count) in the SAME
// transaction as the upsert — otherwise the orphans survive the rebuild.
func TestInsertChunks_PrunesTailInSameTransaction(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// Two transcripts in one call: each is pruned against its OWN highest index.
	chunks := []Chunk{
		{TranscriptID: "t-1", ChunkIndex: 0, Text: "a"},
		{TranscriptID: "t-1", ChunkIndex: 1, Text: "b"},
		{TranscriptID: "t-2", ChunkIndex: 0, Text: "c"},
	}
	mock.ExpectBegin()
	expectChunkInserts(mock, chunks)
	mock.ExpectQuery("WITH pruned AS").
		WithArgs("t-1", 2, patch.StaleReasonChunkChanged, staleFromStates).
		WillReturnRows(pgxmock.NewRows([]string{"chunks", "findings"}).AddRow(3, 1))
	mock.ExpectQuery("WITH pruned AS").
		WithArgs("t-2", 1, patch.StaleReasonChunkChanged, staleFromStates).
		WillReturnRows(pgxmock.NewRows([]string{"chunks", "findings"}).AddRow(0, 0))
	mock.ExpectCommit()

	if err := database.insertChunks(context.Background(), mock, chunks); err != nil {
		t.Fatalf("insertChunks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestInsertChunks_PruneFailureRollsBack: if the prune fails the upsert must
// not commit on its own — a half-replaced projection is worse than a retry.
func TestInsertChunks_PruneFailureRollsBack(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	chunks := []Chunk{{TranscriptID: "t-1", ChunkIndex: 0, Text: "a"}}
	boom := errors.New("boom")
	mock.ExpectBegin()
	expectChunkInserts(mock, chunks)
	mock.ExpectQuery("WITH pruned AS").
		WithArgs("t-1", 1, patch.StaleReasonChunkChanged, staleFromStates).
		WillReturnError(boom)
	mock.ExpectRollback()

	err = database.insertChunks(context.Background(), mock, chunks)
	if !errors.Is(err, boom) {
		t.Fatalf("want prune error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestInsertChunks_EmptyNeverPrunes: an empty chunk set has no index to prune
// against; it must not open a transaction, let alone delete every chunk.
func TestInsertChunks_EmptyNeverPrunes(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	if err := database.insertChunks(context.Background(), mock, nil); err != nil {
		t.Fatalf("insertChunks(nil): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected calls: %v", err)
	}
}

// TestInsertChunkSQL_RefreshesPositionColumns: a re-chunk can move a chunk's
// boundaries without changing its index, so the upsert must refresh the
// position columns too — not just the text and embedding.
func TestInsertChunkSQL_RefreshesPositionColumns(t *testing.T) {
	for _, col := range []string{"file_path", "start_sec", "end_sec", "speaker",
		"text", "source_text", "embedding"} {
		if !strings.Contains(insertChunkSQL, col+" ") || !strings.Contains(insertChunkSQL, "= EXCLUDED."+col) {
			t.Errorf("insertChunkSQL must refresh %s on conflict:\n%s", col, insertChunkSQL)
		}
	}
	// The id is what findings reference — it must survive a rebuild.
	if strings.Contains(insertChunkSQL, "id          = EXCLUDED.id") || strings.Contains(insertChunkSQL, "SET id") {
		t.Errorf("insertChunkSQL must keep the existing chunk id on conflict:\n%s", insertChunkSQL)
	}
}

// TestPruneChunksSQL_Shape pins the prune's blast radius: it deletes only
// transcript_chunks rows of one transcript at/after the bound index, never a
// finding and never transcript provenance; findings are retired through the
// bound stale-from states with a bound reason.
func TestPruneChunksSQL_Shape(t *testing.T) {
	upper := strings.ToUpper(pruneChunksSQL)
	if strings.Count(upper, "DELETE ") != 1 || !strings.Contains(upper, "DELETE FROM TRANSCRIPT_CHUNKS") {
		t.Errorf("pruneChunksSQL must DELETE only from transcript_chunks:\n%s", pruneChunksSQL)
	}
	if !strings.Contains(pruneChunksSQL, "WHERE transcript_id = $1 AND chunk_index >= $2") {
		t.Errorf("pruneChunksSQL must be scoped to one transcript's tail:\n%s", pruneChunksSQL)
	}
	for _, banned := range []string{"DELETE FROM TRANSCRIPT_FINDINGS", "UPDATE TRANSCRIPTS ",
		"DELETE FROM TRANSCRIPTS ", "SEGMENTS", "RAW_TEXT", "DROP ", "TRUNCATE "} {
		if strings.Contains(upper, banned) {
			t.Errorf("pruneChunksSQL must not contain %q:\n%s", banned, pruneChunksSQL)
		}
	}
	if !strings.Contains(pruneChunksSQL, "f.patch_state = ANY($4)") || !strings.Contains(pruneChunksSQL, "stale_reason = $3") {
		t.Errorf("pruneChunksSQL must retire via bound states and reason:\n%s", pruneChunksSQL)
	}
	if strings.Contains(pruneChunksSQL, "decided_at") || strings.Contains(pruneChunksSQL, "decided_by") {
		t.Errorf("pruneChunksSQL must not overwrite the human decision record:\n%s", pruneChunksSQL)
	}
}

// TestPruneChunks_RejectsNonPositiveKeep: pruning to zero rows would delete a
// whole projection; that is requeue --reembed's job, never a prune's.
func TestPruneChunks_RejectsNonPositiveKeep(t *testing.T) {
	database := newTestDB()
	if _, err := database.PruneChunks(context.Background(), "t-1", 0); err == nil {
		t.Fatal("want error for keep=0")
	}
}

// TestGetStoredChunkHashes_HashesPristineText: the fingerprint must be taken
// over the PRISTINE text and match patch.ChunkHash's encoding.
func TestGetStoredChunkHashes_HashesPristineText(t *testing.T) {
	if !strings.Contains(storedChunkHashesSQL, "COALESCE(source_text, text)") ||
		!strings.Contains(storedChunkHashesSQL, "encode(sha256(convert_to(") ||
		!strings.Contains(storedChunkHashesSQL, "ORDER BY chunk_index") {
		t.Errorf("storedChunkHashesSQL must hash pristine text in index order:\n%s", storedChunkHashesSQL)
	}
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery("FROM transcript_chunks").WithArgs("t-1").
		WillReturnRows(pgxmock.NewRows([]string{"chunk_index", "sha"}).AddRow(0, "aa").AddRow(1, "bb"))
	got, err := getStoredChunkHashes(context.Background(), mock, "t-1")
	if err != nil {
		t.Fatalf("getStoredChunkHashes: %v", err)
	}
	if len(got) != 2 || got[1] != (StoredChunkHash{ChunkIndex: 1, SHA256: "bb"}) {
		t.Errorf("mis-scanned: %+v", got)
	}
}

// TestInsertChunks_RealPostgres proves the rebuild semantics end-to-end
// against a real Postgres+pgvector (skipped unless EARMARK_TEST_DATABASE_URL
// points at a THROWAWAY database — it runs migrations and writes rows):
// a re-chunk into fewer chunks prunes the tail, refreshes the shifted
// boundaries, keeps chunk ids, and retires (never deletes) the findings on the
// pruned rows while leaving a rejected decision alone.
func TestInsertChunks_RealPostgres(t *testing.T) {
	url := os.Getenv("EARMARK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("EARMARK_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	database, err := New(&config.Config{DatabaseURL: url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer database.Close()

	sum := "prune-test-" + uuid.NewString() // unique per run: checksum and file_path are UNIQUE
	path := "/books/a/b/" + sum + ".m4b"
	var jobID, tID string
	if err := database.pool.QueryRow(ctx, `
		INSERT INTO transcription_jobs (file_path, checksum, status)
		VALUES ($2, $1, 'done') RETURNING id`, sum, path).Scan(&jobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if err := database.pool.QueryRow(ctx, `
		INSERT INTO transcripts (job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name)
		VALUES ($1, $3, $2, 'en', 40, '[]', 'x', 'm') RETURNING id`, jobID, sum, path).Scan(&tID); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.pool.Exec(ctx, `DELETE FROM transcript_findings WHERE transcript_id = $1`, tID)
		_, _ = database.pool.Exec(ctx, `DELETE FROM transcription_jobs WHERE id = $1`, jobID)
	})
	emb := make([]float32, 768)
	mk := func(idx int, start, end float64, text string) Chunk {
		return Chunk{ID: ChunkUUID(tID, idx), TranscriptID: tID, FilePath: path,
			ChunkIndex: idx, StartSec: start, EndSec: end, Text: text, SourceText: text, Embedding: emb}
	}
	first := []Chunk{mk(0, 0, 10, "zero"), mk(1, 10, 20, "one"), mk(2, 20, 30, "two"), mk(3, 30, 40, "three")}
	if err := database.InsertChunks(ctx, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Findings on a surviving chunk and on two pruned chunks (one proposed,
	// one rejected by a human).
	for _, f := range []struct {
		idx   int
		state string
	}{{1, patch.StateProposed}, {2, patch.StateProposed}, {3, patch.StateRejected}} {
		if _, err := database.pool.Exec(ctx, `
			INSERT INTO transcript_findings (transcript_id, file_path, chunk_id, chunk_index,
			       start_sec, end_sec, original_text, issue_type, confidence, model, patch_state)
			VALUES ($1, 'p', $2, $3, 0, 0, 'x', 'mishearing', 0.9, 'm', $4)`,
			tID, ChunkUUID(tID, f.idx), f.idx, f.state); err != nil {
			t.Fatalf("seed finding: %v", err)
		}
	}

	// Re-chunk into two chunks with shifted boundaries.
	second := []Chunk{mk(0, 0, 15, "zero one"), mk(1, 15, 40, "two three")}
	if err := database.InsertChunks(ctx, second); err != nil {
		t.Fatalf("rebuild insert: %v", err)
	}

	rows, err := database.pool.Query(ctx, `
		SELECT id::text, chunk_index, start_sec, end_sec, text FROM transcript_chunks
		WHERE transcript_id = $1 ORDER BY chunk_index`, tID)
	if err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	type row struct {
		id         string
		idx        int
		start, end float64
		text       string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.idx, &r.start, &r.end, &r.text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []row{
		{ChunkUUID(tID, 0), 0, 0, 15, "zero one"},
		{ChunkUUID(tID, 1), 1, 15, 40, "two three"},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d chunks after rebuild, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chunk %d: want %+v, got %+v", i, want[i], got[i])
		}
	}

	states := map[int]string{}
	frows, err := database.pool.Query(ctx, `
		SELECT chunk_index, patch_state || COALESCE(':' || stale_reason, '')
		FROM transcript_findings WHERE transcript_id = $1`, tID)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	for frows.Next() {
		var idx int
		var s string
		if err := frows.Scan(&idx, &s); err != nil {
			t.Fatalf("scan finding: %v", err)
		}
		states[idx] = s
	}
	frows.Close()
	wantStates := map[int]string{
		1: patch.StateProposed,                                    // chunk survives
		2: patch.StateStale + ":" + patch.StaleReasonChunkChanged, // pruned → retired
		3: patch.StateRejected,                                    // human decision kept
	}
	for idx, w := range wantStates {
		if states[idx] != w {
			t.Errorf("finding on chunk %d: want %q, got %q", idx, w, states[idx])
		}
	}

	// The standalone prune is idempotent once the tail is gone.
	r, err := database.PruneChunks(ctx, tID, 2)
	if err != nil || r != (PruneResult{}) {
		t.Errorf("second prune: want no-op, got %+v, %v", r, err)
	}
}
