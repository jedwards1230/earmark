package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

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

// expectTailLock expects the finding lock a prune to keep takes first.
func expectTailLock(mock pgxmock.PgxPoolIface, tid string, keep int) {
	mock.ExpectExec("FOR UPDATE OF f").WithArgs(tid, keep).
		WillReturnResult(pgxmock.NewResult("SELECT", 0))
}

// TestInsertChunks_PrunesTailInSameTransaction: a re-chunk into fewer chunks
// must delete the old tail (chunk_index >= the new count) in the SAME
// transaction as the upsert — otherwise the orphans survive the rebuild. The
// finding locks come FIRST (before any chunk row is touched), the same
// findings → chunks order SetPatchState uses, so the two cannot deadlock.
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
	expectTailLock(mock, "t-1", 2)
	expectTailLock(mock, "t-2", 1)
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
	expectTailLock(mock, "t-1", 1)
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
	// Retire by index, not only by chunk_id: most live findings carry a
	// chunk_id that names no row (M1).
	if !strings.Contains(pruneChunksSQL, "f.chunk_index >= $2") {
		t.Errorf("pruneChunksSQL must retire findings by chunk_index:\n%s", pruneChunksSQL)
	}
	if strings.Contains(pruneChunksSQL, "decided_at") || strings.Contains(pruneChunksSQL, "decided_by") {
		t.Errorf("pruneChunksSQL must not overwrite the human decision record:\n%s", pruneChunksSQL)
	}
}

// TestPruneChunks_LocksFindingsFirst: the standalone prune (prune-chunks
// --yes) runs in its own transaction and takes the finding locks before the
// DELETE, the same order as InsertChunks and SetPatchState.
func TestPruneChunks_LocksFindingsFirst(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectBegin()
	expectTailLock(mock, "t-1", 3)
	mock.ExpectQuery("WITH pruned AS").
		WithArgs("t-1", 3, patch.StaleReasonChunkChanged, staleFromStates).
		WillReturnRows(pgxmock.NewRows([]string{"chunks", "findings"}).AddRow(2, 4))
	mock.ExpectCommit()
	r, err := database.pruneChunksTx(context.Background(), mock, "t-1", 3)
	if err != nil || r != (PruneResult{Chunks: 2, Findings: 4}) {
		t.Fatalf("pruneChunksTx = %+v, %v", r, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestFindingChunkAddressing_IndexFirst pins the shared addressing: every
// statement resolving a finding's chunk goes by (transcript_id, chunk_index)
// and falls back to chunk_id only when the finding has no index (M1).
func TestFindingChunkAddressing_IndexFirst(t *testing.T) {
	for name, sql := range map[string]string{
		"markChunkStaleForFindingSQL": markChunkStaleForFindingSQL,
		"listCorrectionsSQL":          listCorrectionsSQL,
	} {
		if !strings.Contains(sql, "f.chunk_index IS NOT NULL") ||
			!strings.Contains(sql, "c.chunk_index = f.chunk_index") ||
			!strings.Contains(sql, "f.chunk_index IS NULL AND c.id = f.chunk_id") {
			t.Errorf("%s must address the chunk by index first, chunk_id only without one:\n%s", name, sql)
		}
		if strings.Contains(sql, "f.chunk_id IS NULL") {
			t.Errorf("%s still prefers chunk_id over chunk_index:\n%s", name, sql)
		}
	}
	if !strings.Contains(lockTailFindingsSQL, "FOR UPDATE OF f") || !strings.Contains(lockTailFindingsSQL, "f.chunk_index >= $2") {
		t.Errorf("lockTailFindingsSQL must lock the tail findings:\n%s", lockTailFindingsSQL)
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
