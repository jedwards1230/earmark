package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CorrectedChunk is one chunk of a track's CORRECTED text: the projection
// search returns (transcript_chunks.text = the pristine regenerated chunk with
// the correction overlay replayed, CONTRACT §2.17). Corrected reports whether
// this chunk's projection differs from its pristine text.
type CorrectedChunk struct {
	ID         string
	ChunkIndex int
	StartSec   float64
	EndSec     float64
	Text       string
	Corrected  bool
}

// CorrectedTranscriptPage is a page of a track's corrected text, in chunk order.
//
// CorrectedChunks is how many of the track's chunks carry at least one replayed
// correction. When it is 0 the projection says exactly what the ASR segments
// say, and the page is left empty: the reader is better served by the segments,
// which also carry word timestamps.
type CorrectedTranscriptPage struct {
	TotalChunks     int
	CorrectedChunks int
	Chunks          []CorrectedChunk
}

// correctedChunkCountsSQL counts a job's chunks and how many of them differ
// from their pristine text. A legacy row (source_text NULL) was never
// corrected — CONTRACT §2.17 reads it as COALESCE(source_text, text).
// Read-only.
var correctedChunkCountsSQL = `
	SELECT count(*)::int,
	       count(*) FILTER (WHERE c.source_text IS NOT NULL AND c.text <> c.source_text)::int
	FROM transcript_chunks c
	JOIN transcripts t ON t.id = c.transcript_id
	WHERE t.job_id = $1::uuid
`

// correctedChunkPageSQL reads one page of a job's corrected chunk text, in
// chunk order. Read-only. $2 offset, $3 limit (chunks).
var correctedChunkPageSQL = `
	SELECT c.id, c.chunk_index, c.start_sec, c.end_sec, c.text,
	       (c.source_text IS NOT NULL AND c.text <> c.source_text) AS corrected
	FROM transcript_chunks c
	JOIN transcripts t ON t.id = c.transcript_id
	WHERE t.job_id = $1::uuid
	ORDER BY c.chunk_index
	OFFSET $2 LIMIT $3
`

// GetCorrectedTranscriptPage returns a page of one track's corrected text —
// the same surface search returns — for the MCP get_transcript tool. jobID is
// the track (transcription_jobs.id). offset/limit count chunks; limit <= 0
// returns the counts only.
//
// When no chunk of the track differs from its pristine text, the page carries
// the counts and no chunks: there is nothing corrected to show, and the caller
// serves the ASR segments instead. A track that is not embedded yet has no
// chunks and therefore no corrected text either.
//
// Read-only. The text here has no word timestamps: corrections are anchored to
// chunks, and re-aligning corrected words to audio time is a later step.
func (db *DB) GetCorrectedTranscriptPage(ctx context.Context, jobID string, offset, limit int) (*CorrectedTranscriptPage, error) {
	return getCorrectedTranscriptPage(ctx, db.pool, jobID, offset, limit)
}

// txOptionsBeginner is the slice of the pool API that opens a transaction with
// options. *pgxpool.Pool and pgxmock.PgxPoolIface both satisfy it.
type txOptionsBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// correctedPageTxOptions: both queries read ONE snapshot (REPEATABLE READ), so
// the counts cannot disagree with the page's per-chunk flags when a rebuild
// commits between them. Read-only, and rolled back rather than committed.
var correctedPageTxOptions = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

func getCorrectedTranscriptPage(ctx context.Context, b txOptionsBeginner, jobID string, offset, limit int) (*CorrectedTranscriptPage, error) {
	tx, err := b.BeginTx(ctx, correctedPageTxOptions)
	if err != nil {
		return nil, fmt.Errorf("begin corrected transcript tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p CorrectedTranscriptPage
	if err := tx.QueryRow(ctx, correctedChunkCountsSQL, jobID).Scan(&p.TotalChunks, &p.CorrectedChunks); err != nil {
		return nil, fmt.Errorf("corrected chunk counts for %s: %w", jobID, err)
	}
	if p.CorrectedChunks == 0 || limit <= 0 {
		return &p, nil
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := tx.Query(ctx, correctedChunkPageSQL, jobID, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("corrected chunk page for %s: %w", jobID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c CorrectedChunk
		if err := rows.Scan(&c.ID, &c.ChunkIndex, &c.StartSec, &c.EndSec, &c.Text, &c.Corrected); err != nil {
			return nil, fmt.Errorf("scan corrected chunk: %w", err)
		}
		p.Chunks = append(p.Chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (corrected chunks): %w", err)
	}
	return &p, nil
}
