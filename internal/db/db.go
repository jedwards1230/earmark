// Package db provides PostgreSQL access for the lilbro-whisper service.
//
// Schema (see CONTRACT.md):
//   - transcription_jobs  — job queue; producer: Go monitor, consumer: Python runner
//   - transcripts         — completed transcripts with JSONB segments
//   - transcript_chunks   — pgvector embeddings of chunked transcript text
package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/openai"
)

// ─── Domain types ────────────────────────────────────────────────────────────

// Job represents a row from transcription_jobs.
type Job struct {
	ID        string
	FilePath  string
	Checksum  string
	Status    string // pending | claimed | done | failed
	ClaimedBy *string
	ClaimedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	Error     *string
	Attempts  int
}

// Word is a single word token from a transcript segment (CONTRACT §1.2.1).
type Word struct {
	Word    string   `json:"word"`
	Start   float64  `json:"start"`
	End     float64  `json:"end"`
	Score   *float64 `json:"score"`
	Speaker *string  `json:"speaker"`
}

// Segment is one transcript segment (CONTRACT §1.2.1).
type Segment struct {
	ID      int     `json:"id"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker *string `json:"speaker"`
	Words   []Word  `json:"words"`
}

// Transcript represents a completed transcript row.
type Transcript struct {
	ID              string
	JobID           string
	FilePath        string
	Checksum        string
	Language        string
	DurationSeconds float64
	SpeakerCount    *int
	Segments        []Segment
	RawText         string
	ModelName       string
	CreatedAt       time.Time
}

// Chunk is a row from transcript_chunks (after embedding).
type Chunk struct {
	ID           string
	TranscriptID string
	FilePath     string
	ChunkIndex   int
	StartSec     float64
	EndSec       float64
	Text         string
	Speaker      *string
	Embedding    []float32
	CreatedAt    time.Time
}

// SearchResult is a chunk match from a vector or FTS query.
type SearchResult struct {
	ChunkID      string
	TranscriptID string
	FilePath     string
	ChunkIndex   int
	StartSec     float64
	EndSec       float64
	Text         string
	Speaker      *string
	Similarity   float64
}

// HierarchicalEntry groups chunks by file for the browse-library tool.
type HierarchicalEntry struct {
	FilePath   string
	ChunkCount int
}

// SearchResultWithMetadata extends SearchResult with extra fields for the MCP
// layer so the existing MCP tool formatters keep working.
type SearchResultWithMetadata struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	FilePath   string  `json:"filePath"`
	ChunkIndex int     `json:"chunkIndex"`
	StartSec   float64 `json:"startSec"`
	EndSec     float64 `json:"endSec"`
	Speaker    *string `json:"speaker,omitempty"`
	Similarity float64 `json:"similarity"`
	// Legacy fields kept so the MCP formatter compiles unchanged.
	Author        string `json:"author"`
	Title         string `json:"title"`
	Chapter       string `json:"chapter"`
	ChapterIndex  int    `json:"chapterIndex"`
	ChapterTitle  string `json:"chapterTitle"`
	TotalChunks   int    `json:"totalChunks"`
	TotalChapters int    `json:"totalChapters"`
	ChunkID       string `json:"chunkID"`
	WordCount     int    `json:"wordCount"`
	ChunkStart    int    `json:"chunkStart"`
	ChunkEnd      int    `json:"chunkEnd"`
	FileChecksum  string `json:"fileChecksum"`
	ISBN          string `json:"isbn,omitempty"`
	ASIN          string `json:"asin,omitempty"`
}

// ─── DB ──────────────────────────────────────────────────────────────────────

// DB is the service's database handle.
// pool is a *pgxpool.Pool (goroutine-safe) replacing the former single *pgx.Conn.
type DB struct {
	pool *pgxpool.Pool
	e    *openai.Embeddings
	cfg  *config.Config
	log  log.Logger
}

// New opens a PostgreSQL connection pool and runs schema migrations.
func New(cfg *config.Config) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	// Register pgvector types for every connection in the pool.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	logger := log.NewLogger("db")
	db := &DB{
		pool: pool,
		e:    openai.NewEmbeddings(cfg),
		cfg:  cfg,
		log:  logger,
	}

	if err := db.initialize(context.Background()); err != nil {
		pool.Close()
		return nil, err
	}

	logger.Info("database initialized")
	return db, nil
}

// Close closes the underlying connection pool.
func (db *DB) Close() {
	db.pool.Close()
}

// Ping checks that the database connection pool is healthy.
// It is used by the /readyz health endpoint.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// initialize creates the CONTRACT schema and indexes in a single transaction.
func (db *DB) initialize(ctx context.Context) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin init tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE EXTENSION IF NOT EXISTS pg_trgm;
	`); err != nil {
		return fmt.Errorf("create extensions: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		-- transcription_jobs: job queue (CONTRACT §1.1)
		CREATE TABLE IF NOT EXISTS transcription_jobs (
			id           UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
			file_path    TEXT        NOT NULL,
			checksum     TEXT        NOT NULL,
			status       TEXT        NOT NULL DEFAULT 'pending'
			             CHECK (status IN ('pending','claimed','done','failed')),
			claimed_by   TEXT,
			claimed_at   TIMESTAMPTZ,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			error        TEXT,
			attempts     INTEGER     NOT NULL DEFAULT 0,
			CONSTRAINT transcription_jobs_checksum_unique UNIQUE (checksum)
		);

		CREATE INDEX IF NOT EXISTS transcription_jobs_status_idx
			ON transcription_jobs (status, created_at);
		CREATE INDEX IF NOT EXISTS transcription_jobs_file_path_idx
			ON transcription_jobs (file_path);

		-- transcripts: completed transcript storage (CONTRACT §1.2)
		CREATE TABLE IF NOT EXISTS transcripts (
			id                  UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
			job_id              UUID        NOT NULL REFERENCES transcription_jobs(id) ON DELETE CASCADE,
			file_path           TEXT        NOT NULL,
			checksum            TEXT        NOT NULL,
			language            TEXT        NOT NULL,
			duration_seconds    FLOAT8      NOT NULL,
			speaker_count       INTEGER,
			segments            JSONB       NOT NULL,
			raw_text            TEXT        NOT NULL,
			model_name          TEXT        NOT NULL,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT transcripts_job_id_unique UNIQUE (job_id)
		);

		CREATE INDEX IF NOT EXISTS transcripts_file_path_idx
			ON transcripts (file_path);
		CREATE INDEX IF NOT EXISTS transcripts_raw_text_trgm_idx
			ON transcripts USING gin (raw_text gin_trgm_ops);

		-- transcript_chunks: pgvector embeddings (CONTRACT §3)
		CREATE TABLE IF NOT EXISTS transcript_chunks (
			id            UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
			transcript_id UUID        NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
			file_path     TEXT        NOT NULL,
			chunk_index   INTEGER     NOT NULL,
			start_sec     FLOAT8      NOT NULL,
			end_sec       FLOAT8      NOT NULL,
			text          TEXT        NOT NULL,
			speaker       TEXT,
			embedding     VECTOR(768) NOT NULL,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT transcript_chunks_transcript_chunk_unique UNIQUE (transcript_id, chunk_index)
		);

		CREATE INDEX IF NOT EXISTS transcript_chunks_embedding_idx
			ON transcript_chunks USING hnsw (embedding vector_cosine_ops);
		CREATE INDEX IF NOT EXISTS transcript_chunks_file_path_idx
			ON transcript_chunks (file_path);
		CREATE INDEX IF NOT EXISTS transcript_chunks_text_trgm_idx
			ON transcript_chunks USING gin (text gin_trgm_ops);

		-- updated_at trigger for transcription_jobs
		CREATE OR REPLACE FUNCTION transcription_jobs_set_updated_at()
		RETURNS TRIGGER LANGUAGE plpgsql AS $$
		BEGIN
			NEW.updated_at = now();
			RETURN NEW;
		END;
		$$;

		DROP TRIGGER IF EXISTS transcription_jobs_updated_at ON transcription_jobs;
		CREATE TRIGGER transcription_jobs_updated_at
			BEFORE UPDATE ON transcription_jobs
			FOR EACH ROW EXECUTE FUNCTION transcription_jobs_set_updated_at();
	`); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	return tx.Commit(ctx)
}

// ─── Job queue ───────────────────────────────────────────────────────────────

// InsertJobIfAbsent inserts a pending job row if no row with the same checksum
// exists. Satisfies the transcribe.JobInserter interface.
//
// Returns (jobID, true, nil) on insert, or (existingID, false, nil) if present.
func (db *DB) InsertJobIfAbsent(ctx context.Context, filePath, checksum string) (string, bool, error) {
	// Use INSERT … ON CONFLICT DO NOTHING and check rows affected.
	id := uuid.New().String()
	tag, err := db.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum)
		VALUES ($1, $2, $3)
		ON CONFLICT (checksum) DO NOTHING
	`, id, filePath, checksum)
	if err != nil {
		return "", false, fmt.Errorf("insert job: %w", err)
	}

	if tag.RowsAffected() == 1 {
		return id, true, nil
	}

	// Row already exists — return its id.
	var existingID string
	err = db.pool.QueryRow(ctx, `SELECT id FROM transcription_jobs WHERE checksum = $1`, checksum).Scan(&existingID)
	if err != nil {
		return "", false, fmt.Errorf("fetch existing job id: %w", err)
	}
	return existingID, false, nil
}

// GetCompletedTranscripts returns transcripts that have been completed by the
// runner but not yet embedded (i.e. no rows in transcript_chunks for that transcript_id).
func (db *DB) GetCompletedTranscripts(ctx context.Context) ([]*Transcript, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.job_id, t.file_path, t.checksum,
		       t.language, t.duration_seconds, t.speaker_count,
		       t.segments, t.raw_text, t.model_name, t.created_at
		FROM transcripts t
		WHERE NOT EXISTS (
			SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id
		)
		ORDER BY t.created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query completed transcripts: %w", err)
	}
	defer rows.Close()

	var results []*Transcript
	for rows.Next() {
		var t Transcript
		var segJSON []byte
		if err := rows.Scan(
			&t.ID, &t.JobID, &t.FilePath, &t.Checksum,
			&t.Language, &t.DurationSeconds, &t.SpeakerCount,
			&segJSON, &t.RawText, &t.ModelName, &t.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan transcript: %w", err)
		}
		if err := json.Unmarshal(segJSON, &t.Segments); err != nil {
			return nil, fmt.Errorf("unmarshal segments: %w", err)
		}
		results = append(results, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (completed transcripts): %w", err)
	}
	return results, nil
}

// RecoverStaleJobs resets jobs stuck in "claimed" state longer than the
// configured stale timeout. Jobs that have reached max attempts are marked
// failed. (CONTRACT §1.3 — stale-claim recovery.)
func (db *DB) RecoverStaleJobs(ctx context.Context, timeout time.Duration) error {
	// Use an integer-seconds interval to avoid PostgreSQL misreading Go duration
	// strings (e.g. "30m0s" where bare 'm' means months in Postgres).
	secs := int(timeout.Seconds())

	// Reset below-max-attempts jobs to pending.
	if _, err := db.pool.Exec(ctx, `
		UPDATE transcription_jobs
		SET    status     = 'pending',
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  status     = 'claimed'
		  AND  updated_at < now() - ($1 * interval '1 second')
		  AND  attempts   < 3
	`, secs); err != nil {
		return fmt.Errorf("reset stale jobs: %w", err)
	}

	// Mark max-attempts jobs failed.
	if _, err := db.pool.Exec(ctx, `
		UPDATE transcription_jobs
		SET    status = 'failed',
		       error  = 'max attempts reached'
		WHERE  status     = 'claimed'
		  AND  updated_at < now() - ($1 * interval '1 second')
		  AND  attempts   >= 3
	`, secs); err != nil {
		return fmt.Errorf("fail max-attempts jobs: %w", err)
	}
	return nil
}

// ─── Embedding pipeline ───────────────────────────────────────────────────────

// InsertChunks stores pre-computed chunks with embeddings for a transcript.
func (db *DB) InsertChunks(ctx context.Context, chunks []Chunk) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin chunk tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, c := range chunks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO transcript_chunks
			       (transcript_id, file_path, chunk_index, start_sec, end_sec,
			        text, speaker, embedding)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (transcript_id, chunk_index) DO UPDATE
			SET text      = EXCLUDED.text,
			    embedding = EXCLUDED.embedding
		`, c.TranscriptID, c.FilePath, c.ChunkIndex, c.StartSec, c.EndSec,
			c.Text, c.Speaker, pgvector.NewVector(c.Embedding),
		); err != nil {
			return fmt.Errorf("insert chunk %d: %w", c.ChunkIndex, err)
		}
	}
	return tx.Commit(ctx)
}

// GetEmbeddings delegates to the openai.Embeddings client.
func (db *DB) GetEmbeddings(texts []string) ([][]float32, error) {
	return db.e.GetEmbeddings(texts)
}

// ─── Search ──────────────────────────────────────────────────────────────────

// Search performs a vector-similarity search over transcript_chunks.
func (db *DB) Search(ctx context.Context, query string, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	vecs, err := db.e.GetEmbeddings([]string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned for query")
	}
	return db.findSimilar(ctx, vecs[0], limit, threshold)
}

func (db *DB) findSimilar(ctx context.Context, vec []float32, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT
			c.id,
			c.text,
			c.file_path,
			c.chunk_index,
			c.start_sec,
			c.end_sec,
			c.speaker,
			1 - (c.embedding <=> $1) AS similarity,
			(SELECT COUNT(*) FROM transcript_chunks WHERE transcript_id = c.transcript_id) AS total_chunks
		FROM transcript_chunks c
		WHERE ($3 = 0 OR 1 - (c.embedding <=> $1) >= $3)
		ORDER BY c.embedding <=> $1
		LIMIT $2
	`, pgvector.NewVector(vec), limit, threshold)
	if err != nil {
		return nil, fmt.Errorf("similarity query: %w", err)
	}
	defer rows.Close()

	return scanResults(rows)
}

// TextSearch performs a trigram full-text search over transcript_chunks.
// Filtering at the chunk level (c.text ILIKE) makes LIMIT meaningful.
func (db *DB) TextSearch(ctx context.Context, query string, limit int) ([]SearchResultWithMetadata, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT
			c.id,
			c.text,
			c.file_path,
			c.chunk_index,
			c.start_sec,
			c.end_sec,
			c.speaker,
			0.0 AS similarity,
			(SELECT COUNT(*) FROM transcript_chunks WHERE transcript_id = c.transcript_id) AS total_chunks
		FROM transcript_chunks c
		WHERE c.text ILIKE '%' || $1 || '%'
		ORDER BY c.chunk_index ASC
		LIMIT $2
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("text search query: %w", err)
	}
	defer rows.Close()

	return scanResults(rows)
}

func scanResults(rows pgx.Rows) ([]SearchResultWithMetadata, error) {
	var results []SearchResultWithMetadata
	for rows.Next() {
		var r SearchResultWithMetadata
		var speaker *string
		if err := rows.Scan(
			&r.ID, &r.Content, &r.FilePath,
			&r.ChunkIndex, &r.StartSec, &r.EndSec,
			&speaker, &r.Similarity, &r.TotalChunks,
		); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.Speaker = speaker
		r.ChunkID = r.ID
		// Populate legacy fields from file path for the MCP formatter.
		r.Title = filepath.Base(filepath.Dir(r.FilePath))
		r.Author = filepath.Base(filepath.Dir(filepath.Dir(r.FilePath)))
		r.Chapter = filepath.Base(r.FilePath)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (scan results): %w", err)
	}
	return results, nil
}

// GetHierarchicalData returns a list of files with their chunk counts for the
// browse_audiobook_library MCP tool.
func (db *DB) GetHierarchicalData(ctx context.Context) ([]HierarchicalEntry, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT file_path, COUNT(*) AS chunk_count
		FROM transcript_chunks
		GROUP BY file_path
		ORDER BY file_path
	`)
	if err != nil {
		return nil, fmt.Errorf("hierarchical query: %w", err)
	}
	defer rows.Close()

	var entries []HierarchicalEntry
	for rows.Next() {
		var e HierarchicalEntry
		if err := rows.Scan(&e.FilePath, &e.ChunkCount); err != nil {
			return nil, fmt.Errorf("scan hierarchical: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (hierarchical): %w", err)
	}
	return entries, nil
}

// GetChunkContext returns surrounding chunks for a given chunk ID string.
func (db *DB) GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]SearchResultWithMetadata, error) {
	// Resolve the target chunk's transcript_id and chunk_index.
	var transcriptID string
	var chunkIndex int
	err := db.pool.QueryRow(ctx, `
		SELECT transcript_id, chunk_index FROM transcript_chunks WHERE id = $1
	`, chunkID).Scan(&transcriptID, &chunkIndex)
	if err != nil {
		return nil, fmt.Errorf("find chunk %q: %w", chunkID, err)
	}

	lo := chunkIndex - contextWindow
	if lo < 0 {
		lo = 0
	}
	hi := chunkIndex + contextWindow

	rows, err := db.pool.Query(ctx, `
		SELECT
			c.id,
			c.text,
			c.file_path,
			c.chunk_index,
			c.start_sec,
			c.end_sec,
			c.speaker,
			0.0 AS similarity,
			(SELECT COUNT(*) FROM transcript_chunks WHERE transcript_id = c.transcript_id) AS total_chunks
		FROM transcript_chunks c
		WHERE c.transcript_id = $1
		  AND c.chunk_index BETWEEN $2 AND $3
		ORDER BY c.chunk_index
	`, transcriptID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("context query: %w", err)
	}
	defer rows.Close()

	return scanResults(rows)
}

// ─── Processing stats ────────────────────────────────────────────────────────

// Statistics holds aggregate counts for the monitor's startup log.
type Statistics struct {
	PendingJobs    int
	CompletedJobs  int
	EmbeddedChunks int
}

// GetProcessingStats returns aggregate counts.
func (db *DB) GetProcessingStats(ctx context.Context) (*Statistics, error) {
	s := &Statistics{}
	err := db.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status IN ('pending','claimed')) AS pending,
			COUNT(*) FILTER (WHERE status = 'done')                AS done
		FROM transcription_jobs
	`).Scan(&s.PendingJobs, &s.CompletedJobs)
	if err != nil {
		return nil, err
	}
	err = db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transcript_chunks`).Scan(&s.EmbeddedChunks)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ─── Dashboard stats ─────────────────────────────────────────────────────────

// QueueStats holds per-status job counts and related counters for the status
// dashboard.
type QueueStats struct {
	Pending     int
	Claimed     int
	Done        int
	Failed      int
	Transcripts int
	Chunks      int
	// Runner fields — populated when at least one job has status='claimed'.
	RunnerActive  bool
	RunnerID      string     // claimed_by of the most-recently-updated claimed job
	LastHeartbeat *time.Time // updated_at of that job
}

// GetServiceStatus returns a single aggregate snapshot used by the status
// dashboard. It issues two queries: one GROUP BY for job counts plus transcript
// and chunk totals, one for the active runner heartbeat.
func (db *DB) GetServiceStatus(ctx context.Context) (*QueueStats, error) {
	q := &QueueStats{}

	// Job counts by status in one pass.
	rows, err := db.pool.Query(ctx, `
		SELECT status, COUNT(*) AS n
		FROM transcription_jobs
		GROUP BY status
	`)
	if err != nil {
		return nil, fmt.Errorf("queue stats query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan queue stats: %w", err)
		}
		switch status {
		case "pending":
			q.Pending = n
		case "claimed":
			q.Claimed = n
		case "done":
			q.Done = n
		case "failed":
			q.Failed = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (queue stats): %w", err)
	}

	// Transcript count.
	if err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transcripts`).Scan(&q.Transcripts); err != nil {
		return nil, fmt.Errorf("transcript count: %w", err)
	}

	// Chunk count.
	if err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transcript_chunks`).Scan(&q.Chunks); err != nil {
		return nil, fmt.Errorf("chunk count: %w", err)
	}

	// Active runner: most-recently-updated claimed job.
	var claimedBy *string
	var updatedAt *time.Time
	err = db.pool.QueryRow(ctx, `
		SELECT claimed_by, updated_at
		FROM transcription_jobs
		WHERE status = 'claimed'
		ORDER BY updated_at DESC
		LIMIT 1
	`).Scan(&claimedBy, &updatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("runner heartbeat query: %w", err)
	}
	if claimedBy != nil {
		q.RunnerActive = true
		q.RunnerID = *claimedBy
		q.LastHeartbeat = updatedAt
	}

	return q, nil
}

// RecentJob is a lightweight view of a transcription_jobs row for the recent-
// activity table on the status dashboard.
type RecentJob struct {
	ID        string
	FilePath  string
	Status    string
	UpdatedAt time.Time
	Error     *string
}

// GetRecentJobs returns the most-recently-updated jobs, newest first.
func (db *DB) GetRecentJobs(ctx context.Context, limit int) ([]RecentJob, error) {
	if limit <= 0 {
		limit = 15
	}
	rows, err := db.pool.Query(ctx, `
		SELECT id, file_path, status, updated_at, error
		FROM transcription_jobs
		ORDER BY updated_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent jobs query: %w", err)
	}
	defer rows.Close()

	var jobs []RecentJob
	for rows.Next() {
		var j RecentJob
		if err := rows.Scan(&j.ID, &j.FilePath, &j.Status, &j.UpdatedAt, &j.Error); err != nil {
			return nil, fmt.Errorf("scan recent job: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (recent jobs): %w", err)
	}
	return jobs, nil
}

// ─── Checksum helper ─────────────────────────────────────────────────────────

// ComputeFileChecksum returns the SHA-256 hex digest of a file.
func (db *DB) ComputeFileChecksum(filePath string) (string, error) {
	// #nosec G304 — path validated by caller
	f, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IsJobQueued returns true if a transcription_jobs row already exists for the
// given checksum (in any status).
func (db *DB) IsJobQueued(ctx context.Context, checksum string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM transcription_jobs WHERE checksum = $1)
	`, checksum).Scan(&exists)
	return exists, err
}

// ─── Requeue / redo operations ─────────────────────────────────────────────────

// JobMatch is a lightweight view of a job used for requeue previews.
type JobMatch struct {
	ID       string
	FilePath string
	Status   string
}

// likePattern wraps a user substring for a case-insensitive ILIKE match.
func likePattern(substr string) string { return "%" + substr + "%" }

// FindJobs returns jobs whose file_path contains substr (case-insensitive),
// for previewing a requeue before it runs.
func (db *DB) FindJobs(ctx context.Context, substr string) ([]JobMatch, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, file_path, status
		FROM   transcription_jobs
		WHERE  file_path ILIKE $1
		ORDER BY file_path
	`, likePattern(substr))
	if err != nil {
		return nil, fmt.Errorf("find jobs: %w", err)
	}
	defer rows.Close()
	return scanJobMatches(rows)
}

// FindFailedJobs returns all jobs currently in the 'failed' state.
func (db *DB) FindFailedJobs(ctx context.Context) ([]JobMatch, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, file_path, status
		FROM   transcription_jobs
		WHERE  status = 'failed'
		ORDER BY file_path
	`)
	if err != nil {
		return nil, fmt.Errorf("find failed jobs: %w", err)
	}
	defer rows.Close()
	return scanJobMatches(rows)
}

func scanJobMatches(rows pgx.Rows) ([]JobMatch, error) {
	var matches []JobMatch
	for rows.Next() {
		var m JobMatch
		if err := rows.Scan(&m.ID, &m.FilePath, &m.Status); err != nil {
			return nil, fmt.Errorf("scan job match: %w", err)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (job matches): %w", err)
	}
	return matches, nil
}

// RequeueJobs re-runs the full pipeline for jobs whose file_path contains substr:
// it deletes their transcripts (cascading to chunks) and resets the jobs to
// 'pending' with attempts cleared. Returns the file paths that were reset.
func (db *DB) RequeueJobs(ctx context.Context, substr string) ([]string, error) {
	return db.requeue(ctx, requeueByPath, likePattern(substr))
}

// RequeueFailed re-runs the pipeline for every job in the 'failed' state.
// Returns the file paths that were reset.
func (db *DB) RequeueFailed(ctx context.Context) ([]string, error) {
	return db.requeue(ctx, requeueFailed)
}

// requeuePlan is a pair of fully-formed, static SQL statements for one requeue
// selector. The statements are package constants — nothing is concatenated at
// runtime, so the only dynamic input is the bound $1 parameter (when present).
type requeuePlan struct {
	deleteTranscripts string // delete transcripts for the selected jobs (chunks cascade)
	resetJobs         string // reset those jobs to pending; RETURNING file_path
}

var (
	// requeueByPath selects jobs by a case-insensitive file_path match ($1).
	requeueByPath = requeuePlan{
		deleteTranscripts: `DELETE FROM transcripts
			WHERE job_id IN (SELECT id FROM transcription_jobs WHERE file_path ILIKE $1)`,
		resetJobs: `UPDATE transcription_jobs
			SET    status = 'pending', attempts = 0, error = NULL,
			       claimed_by = NULL, claimed_at = NULL, updated_at = now()
			WHERE  file_path ILIKE $1
			RETURNING file_path`,
	}
	// requeueFailed selects every job in the 'failed' state (no parameters).
	requeueFailed = requeuePlan{
		deleteTranscripts: `DELETE FROM transcripts
			WHERE job_id IN (SELECT id FROM transcription_jobs WHERE status = 'failed')`,
		resetJobs: `UPDATE transcription_jobs
			SET    status = 'pending', attempts = 0, error = NULL,
			       claimed_by = NULL, claimed_at = NULL, updated_at = now()
			WHERE  status = 'failed'
			RETURNING file_path`,
	}
)

// requeue runs a requeuePlan's two static statements in one transaction: delete
// the selected transcripts (chunks cascade) and reset the jobs to pending. args
// are the bound parameters for the plan's $N placeholders (one for by-path,
// none for failed).
func (db *DB) requeue(ctx context.Context, plan requeuePlan, args ...any) ([]string, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin requeue tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, plan.deleteTranscripts, args...); err != nil {
		return nil, fmt.Errorf("delete transcripts: %w", err)
	}

	rows, err := tx.Query(ctx, plan.resetJobs, args...)
	if err != nil {
		return nil, fmt.Errorf("reset jobs: %w", err)
	}
	paths, err := scanPaths(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit requeue tx: %w", err)
	}
	return paths, nil
}

// ReembedJobs deletes the embedded chunks for transcripts whose file_path
// contains substr, so the worker re-embeds them on its next poll (the transcript
// and job are left untouched — no re-transcription). Use this after changing the
// embedding model or chunk size. Returns the transcript file paths affected.
func (db *DB) ReembedJobs(ctx context.Context, substr string) ([]string, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reembed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT id, file_path FROM transcripts WHERE file_path ILIKE $1`, likePattern(substr))
	if err != nil {
		return nil, fmt.Errorf("find transcripts: %w", err)
	}
	var ids, paths []string
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan transcript: %w", err)
		}
		ids = append(ids, id)
		paths = append(paths, path)
	}
	rerr := rows.Err()
	rows.Close()
	if rerr != nil {
		return nil, fmt.Errorf("rows error (transcripts): %w", rerr)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	if _, err := tx.Exec(ctx, `DELETE FROM transcript_chunks WHERE transcript_id = ANY($1)`, ids); err != nil {
		return nil, fmt.Errorf("delete chunks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit reembed tx: %w", err)
	}
	return paths, nil
}

func scanPaths(rows pgx.Rows) ([]string, error) {
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan path: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (paths): %w", err)
	}
	return paths, nil
}

// Reset drops all tables and re-initialises the schema (DEBUG_DB_RESET only).
// A second confirmation env var DEBUG_DB_RESET_CONFIRM=yes-delete-everything
// is required to prevent accidental data destruction.
func (db *DB) Reset(ctx context.Context) error {
	confirm := os.Getenv("DEBUG_DB_RESET_CONFIRM")
	if confirm != "yes-delete-everything" {
		db.log.Error("Reset() refused: set DEBUG_DB_RESET_CONFIRM=yes-delete-everything to confirm")
		return fmt.Errorf("reset refused: DEBUG_DB_RESET_CONFIRM not set to 'yes-delete-everything'")
	}
	db.log.Warn("performing complete database reset")
	if _, err := db.pool.Exec(ctx, `
		DROP TABLE IF EXISTS transcript_chunks   CASCADE;
		DROP TABLE IF EXISTS transcripts         CASCADE;
		DROP TABLE IF EXISTS transcription_jobs  CASCADE;
	`); err != nil {
		return fmt.Errorf("drop tables: %w", err)
	}
	return db.initialize(ctx)
}
