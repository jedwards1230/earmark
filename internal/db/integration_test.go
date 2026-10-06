package db

// Integration tests for the chunk prune against a REAL Postgres + pgvector.
// Each runs in its own throwaway database (newTestDatabase); see
// migrate_integration_test.go for how to run them and the shared helpers.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jedwards1230/earmark/internal/patch"
)

// seedTranscript inserts a done job + transcript with unique checksum/path and
// removes them (cascading to chunks) plus their findings on cleanup.
func seedTranscript(t *testing.T, database *DB) (tID, path string) {
	t.Helper()
	ctx := context.Background()
	sum := "it-" + uuid.NewString() // checksum and file_path are UNIQUE
	path = "/books/a/b/" + sum + ".m4b"
	var jobID string
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
	return tID, path
}

// seedFinding inserts a finding addressed by chunkID/idx in the given state.
func seedFinding(t *testing.T, database *DB, tID, chunkID string, idx int, state string) string {
	t.Helper()
	var id string
	if err := database.pool.QueryRow(context.Background(), `
		INSERT INTO transcript_findings (transcript_id, file_path, chunk_id, chunk_index,
		       start_sec, end_sec, original_text, issue_type, confidence, model, patch_state)
		VALUES ($1, 'p', $2, $3, 0, 0, 'x', 'mishearing', 0.9, 'm', $4) RETURNING id`,
		tID, chunkID, idx, state).Scan(&id); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return id
}

func findingState(t *testing.T, database *DB, id string) string {
	t.Helper()
	var s string
	if err := database.pool.QueryRow(context.Background(), `
		SELECT patch_state || COALESCE(':' || stale_reason, '') FROM transcript_findings WHERE id = $1`,
		id).Scan(&s); err != nil {
		t.Fatalf("read finding %s: %v", id, err)
	}
	return s
}

type chunkRow struct {
	id         string
	idx        int
	start, end float64
	text       string
}

func readChunks(t *testing.T, database *DB, tID string) []chunkRow {
	t.Helper()
	rows, err := database.pool.Query(context.Background(), `
		SELECT id::text, chunk_index, start_sec, end_sec, text FROM transcript_chunks
		WHERE transcript_id = $1 ORDER BY chunk_index`, tID)
	if err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	defer rows.Close()
	var got []chunkRow
	for rows.Next() {
		var r chunkRow
		if err := rows.Scan(&r.id, &r.idx, &r.start, &r.end, &r.text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	return got
}

// TestIntegrationInsertChunksPrune proves the rebuild semantics end-to-end: a
// re-chunk into fewer chunks prunes the tail, refreshes the shifted
// boundaries, keeps chunk ids, and retires (never deletes) the findings on the
// pruned rows while leaving a rejected decision alone.
func TestIntegrationInsertChunksPrune(t *testing.T) {
	database := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	tID, path := seedTranscript(t, database)

	emb := make([]float32, 768)
	mk := func(idx int, start, end float64, text string) Chunk {
		return Chunk{ID: ChunkUUID(tID, idx), TranscriptID: tID, FilePath: path,
			ChunkIndex: idx, StartSec: start, EndSec: end, Text: text, SourceText: text, Embedding: emb}
	}
	first := []Chunk{mk(0, 0, 10, "zero"), mk(1, 10, 20, "one"), mk(2, 20, 30, "two"), mk(3, 30, 40, "three")}
	if err := database.InsertChunks(ctx, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	survivor := seedFinding(t, database, tID, ChunkUUID(tID, 1), 1, patch.StateProposed)
	onTail := seedFinding(t, database, tID, ChunkUUID(tID, 2), 2, patch.StateProposed)
	rejected := seedFinding(t, database, tID, ChunkUUID(tID, 3), 3, patch.StateRejected)

	// Re-chunk into two chunks with shifted boundaries.
	second := []Chunk{mk(0, 0, 15, "zero one"), mk(1, 15, 40, "two three")}
	if err := database.InsertChunks(ctx, second); err != nil {
		t.Fatalf("rebuild insert: %v", err)
	}

	got := readChunks(t, database, tID)
	want := []chunkRow{
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
	for id, w := range map[string]string{
		survivor: patch.StateProposed,                                    // chunk survives
		onTail:   patch.StateStale + ":" + patch.StaleReasonChunkChanged, // pruned → retired
		rejected: patch.StateRejected,                                    // human decision kept
	} {
		if s := findingState(t, database, id); s != w {
			t.Errorf("finding %s: want %q, got %q", id, w, s)
		}
	}

	// The standalone prune is idempotent once the tail is gone.
	r, err := database.PruneChunks(ctx, tID, 2)
	if err != nil || r != (PruneResult{}) {
		t.Errorf("second prune: want no-op, got %+v, %v", r, err)
	}
}

// TestIntegrationPruneRetiresFindingsWithDanglingChunkID is the M1 regression:
// the ungated embed path stores RANDOM chunk ids, while most live judge
// findings carry the deterministic UUIDv5 id — a chunk_id that names no row.
// A prune must still retire such a finding on the tail (by chunk_index), and
// accepting one must still flag its chunk for rebuild.
func TestIntegrationPruneRetiresFindingsWithDanglingChunkID(t *testing.T) {
	database := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	tID, path := seedTranscript(t, database)

	emb := make([]float32, 768)
	var first []Chunk
	for i, text := range []string{"zero", "one", "two"} {
		// ID "" → gen_random_uuid(): exactly what the ungated worker stores.
		first = append(first, Chunk{TranscriptID: tID, FilePath: path, ChunkIndex: i,
			Text: text, SourceText: text, Embedding: emb})
	}
	if err := database.InsertChunks(ctx, first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Findings with UUIDv5 ids that match no stored row.
	onSurvivor := seedFinding(t, database, tID, ChunkUUID(tID, 1), 1, patch.StateProposed)
	onTail := seedFinding(t, database, tID, ChunkUUID(tID, 2), 2, patch.StateProposed)

	// Accepting the survivor must flag ITS chunk (index 1) for rebuild.
	if err := database.SetPatchState(ctx, onSurvivor, patch.StateProposed, patch.StateAccepted, "it"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	var flagged int
	if err := database.pool.QueryRow(ctx, `
		SELECT chunk_index FROM transcript_chunks WHERE transcript_id = $1 AND embedding_stale`, tID).
		Scan(&flagged); err != nil || flagged != 1 {
		t.Fatalf("accept must flag chunk 1 for rebuild: got %d, %v", flagged, err)
	}

	// The reviewer's worklist resolves the chunk text by index too.
	rows, err := database.ListCorrections(ctx, CorrectionFilter{ID: onSurvivor})
	if err != nil || len(rows) != 1 || rows[0].ChunkText != "one" {
		t.Fatalf("ListCorrections must resolve the chunk by index: %+v, %v", rows, err)
	}

	// Re-chunk into two: the tail finding is retired even though its chunk_id
	// never named the pruned row.
	second := first[:2]
	if err := database.InsertChunks(ctx, second); err != nil {
		t.Fatalf("rebuild insert: %v", err)
	}
	if s := findingState(t, database, onTail); s != patch.StateStale+":"+patch.StaleReasonChunkChanged {
		t.Errorf("tail finding with dangling chunk_id: want stale:chunk_changed, got %q", s)
	}
	if s := findingState(t, database, onSurvivor); s != patch.StateAccepted {
		t.Errorf("survivor finding: want accepted, got %q", s)
	}
}
