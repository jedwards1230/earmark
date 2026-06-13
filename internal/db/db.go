// Package db provides PostgreSQL access for the earmark service.
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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/jedwards1230/earmark/internal/asr"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/openai"
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
	meta metaprovider.MetadataProvider
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

	// Build the metadata provider from config (METADATA_PROVIDER env var).
	// The factory handles parse errors and missing ABS credentials internally,
	// falling back to PathProvider — labels are cosmetic, never a startup blocker.
	db := &DB{
		pool: pool,
		e:    openai.NewEmbeddings(cfg),
		cfg:  cfg,
		log:  logger,
		meta: metaprovider.New(cfg),
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

		-- runner_control: singleton row gating the ASR runner's claims (CONTRACT §1.4).
		-- The runner reads it before each claim; the Go service (dashboard + control
		-- API) writes it. A DB row is the only channel the (separate-host) runner and
		-- service share, and it is durable across reboots — unlike the gaming busy-
		-- flag file, which lives in tmpfs on the GPU host.
		--   paused    — true means decline all new claims.
		--   run_limit — NULL means unlimited; a non-negative integer is a bounded run
		--               (e.g. a single-job smoke test). The runner decrements it as
		--               part of each claim and declines once it reaches 0.
		-- Gate: claim iff (NOT paused) AND (run_limit IS NULL OR run_limit > 0).
		CREATE TABLE IF NOT EXISTS runner_control (
			id         INTEGER     NOT NULL PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			paused     BOOLEAN     NOT NULL DEFAULT false,
			run_limit  INTEGER         CHECK (run_limit IS NULL OR run_limit >= 0),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_by TEXT
		);
		INSERT INTO runner_control (id, paused) VALUES (1, false)
			ON CONFLICT (id) DO NOTHING;

		-- run_metrics: per-run observability (CONTRACT §1.5). One row per job,
		-- written by three independent writers that each UPSERT only their slice
		-- of columns (all nullable) keyed on job_id:
		--   Go monitor       — audio_bytes (file size at enqueue time)
		--   Python runner     — audio probe (channels/sample_rate/codec/format) +
		--                       transcription timing/model/counts
		--   Go embed worker   — embedding timing/model/chunk count + token counts
		-- ON DELETE CASCADE keeps the row's lifetime tied to the job.
		CREATE TABLE IF NOT EXISTS run_metrics (
			job_id              UUID        PRIMARY KEY REFERENCES transcription_jobs(id) ON DELETE CASCADE,
			audio_bytes         BIGINT,
			audio_channels      INT,
			audio_sample_rate   INT,
			audio_codec         TEXT,
			audio_format        TEXT,
			transcribe_started_at  TIMESTAMPTZ,
			transcribe_finished_at TIMESTAMPTZ,
			asr_model           TEXT,
			compute_type        TEXT,
			runner_host         TEXT,
			chunked             BOOLEAN,
			n_windows           INT,
			char_count          INT,
			word_count          INT,
			segment_count       INT,
			embed_started_at    TIMESTAMPTZ,
			embed_finished_at   TIMESTAMPTZ,
			embed_model         TEXT,
			embed_chunk_count   INT,
			embed_prompt_tokens INT,
			embed_total_tokens  INT,
			-- ASR backend descriptor (CONTRACT §1.5 / §2.13). Runner-owned,
			-- all nullable, best-effort (SHOULD). Also added to existing tables
			-- via the ADD COLUMN IF NOT EXISTS migration below.
			asr_family           TEXT,
			asr_runtime          TEXT,
			caps_applied         JSONB,
			caps_requested       JSONB,
			caps_skipped_reason  JSONB,
			mean_word_confidence FLOAT8,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		-- book_metadata: per-book enrichment (CONTRACT §1.6). One row per book
		-- directory (book_dir = filepath.Dir of any track under the book). This is
		-- the DB seam for the provider-architecture: the Go monitor writes the
		-- initial row at enqueue time using the MetadataProvider; later PRs populate
		-- the nullable columns (chapters in PR 4, bias_terms in PR 5). No pipeline
		-- code reads this table yet — it is additive and a missing row is a no-op.
		CREATE TABLE IF NOT EXISTS book_metadata (
			book_dir    TEXT        NOT NULL PRIMARY KEY,
			title       TEXT,
			author      TEXT,
			narrator    TEXT,
			series      TEXT,
			asin        TEXT,
			chapters    JSONB,
			bias_terms  TEXT[],
			source      TEXT,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	// run_limit migration: CREATE TABLE IF NOT EXISTS won't add the column to an
	// existing prod table, so add it (and its CHECK) idempotently. ADD COLUMN IF
	// NOT EXISTS is a no-op when present; the CHECK is guarded by pg_constraint and
	// swallows the duplicate-on-race SQLSTATEs (same pattern as the file_path
	// constraint below).
	if _, err := tx.Exec(ctx, `
		ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS run_limit INTEGER;
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint WHERE conname = 'runner_control_run_limit_nonneg'
			) THEN
				ALTER TABLE runner_control
					ADD CONSTRAINT runner_control_run_limit_nonneg
					CHECK (run_limit IS NULL OR run_limit >= 0);
			END IF;
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$;
	`); err != nil {
		return fmt.Errorf("run_limit migration: %w", err)
	}

	// Path-level dedup migration: the original dedup was checksum-only, so a file
	// hashed mid-copy (over NFS) and again when complete produced two jobs for one
	// file_path. Collapse any such duplicates (keep the most-advanced, else oldest
	// — never discard a 'done' transcript) then enforce one job per file_path. The
	// DELETE is a no-op on a clean DB; the ADD CONSTRAINT is idempotent.
	if _, err := tx.Exec(ctx, `
		DELETE FROM transcription_jobs t
		USING (
			SELECT file_path,
			       (array_agg(id ORDER BY
			           CASE status WHEN 'done' THEN 0 WHEN 'claimed' THEN 1
			                       WHEN 'pending' THEN 2 ELSE 3 END,
			           created_at ASC))[1] AS keep_id
			FROM transcription_jobs
			GROUP BY file_path
			HAVING COUNT(*) > 1
		) d
		WHERE t.file_path = d.file_path AND t.id <> d.keep_id;

		-- Idempotent + concurrency-safe: skip if the constraint already exists, and
		-- still swallow the error if two pods race to create it on a fresh DB.
		-- (ADD CONSTRAINT on an existing constraint raises duplicate_table 42P07 —
		-- the backing index relation already exists — not duplicate_object.)
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint WHERE conname = 'transcription_jobs_file_path_unique'
			) THEN
				ALTER TABLE transcription_jobs
					ADD CONSTRAINT transcription_jobs_file_path_unique UNIQUE (file_path);
			END IF;
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$;
	`); err != nil {
		return fmt.Errorf("file_path dedup migration: %w", err)
	}

	// ASR backend-descriptor migration (CONTRACT §1.5 / §2.13): add the six
	// runner-owned columns to an existing run_metrics table. All additive +
	// nullable, so the existing single NeMo runner that writes none of them keeps
	// working (columns stay NULL → "unknown"). ADD COLUMN IF NOT EXISTS is a no-op
	// when the column is already present, so this is safe to run on every boot.
	if _, err := tx.Exec(ctx, `
		ALTER TABLE run_metrics
			ADD COLUMN IF NOT EXISTS asr_family           TEXT,
			ADD COLUMN IF NOT EXISTS asr_runtime          TEXT,
			ADD COLUMN IF NOT EXISTS caps_applied         JSONB,
			ADD COLUMN IF NOT EXISTS caps_requested       JSONB,
			ADD COLUMN IF NOT EXISTS caps_skipped_reason  JSONB,
			ADD COLUMN IF NOT EXISTS mean_word_confidence FLOAT8;
	`); err != nil {
		return fmt.Errorf("run_metrics asr-descriptor migration: %w", err)
	}

	return tx.Commit(ctx)
}

// ─── Job queue ───────────────────────────────────────────────────────────────

// InsertJobIfAbsent inserts a pending job row unless one already exists for the
// same checksum OR the same file_path. Satisfies the transcribe.JobInserter
// interface.
//
// Dedup is enforced by two UNIQUE constraints (checksum, file_path); a plain
// INSERT that violates either raises 23505, which we treat as "already present"
// and resolve to the existing id. Catching the error (rather than ON CONFLICT
// on a single column) is what closes the race where a file copied over NFS is
// hashed mid-copy and again when complete: the two differing checksums share one
// file_path, so the file_path constraint blocks the duplicate regardless of
// caller timing.
//
// Returns (jobID, true, nil) on insert, or (existingID, false, nil) if present.
func (db *DB) InsertJobIfAbsent(ctx context.Context, filePath, checksum string) (string, bool, error) {
	id := uuid.New().String()
	_, err := db.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum)
		VALUES ($1, $2, $3)
	`, id, filePath, checksum)
	if err == nil {
		return id, true, nil
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation (checksum or file_path)
		var existingID string
		qerr := db.pool.QueryRow(ctx, `
			SELECT id FROM transcription_jobs
			WHERE file_path = $1 OR checksum = $2
			LIMIT 1
		`, filePath, checksum).Scan(&existingID)
		if qerr != nil {
			return "", false, fmt.Errorf("fetch existing job id: %w", qerr)
		}
		return existingID, false, nil
	}
	return "", false, fmt.Errorf("insert job: %w", err)
}

// IsPathQueued reports whether a transcription_jobs row already exists for the
// given file_path (in any status). The monitor uses it to skip re-hashing
// already-known files on startup, turning a full-library rescan into a
// metadata-only scan.
func (db *DB) IsPathQueued(ctx context.Context, filePath string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM transcription_jobs WHERE file_path = $1)
	`, filePath).Scan(&exists)
	return exists, err
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

// GetEmbeddingsWithUsage delegates to the openai.Embeddings client, also
// returning the provider-reported token usage (which Ollama may leave zeroed).
func (db *DB) GetEmbeddingsWithUsage(texts []string) ([][]float32, openai.EmbeddingUsage, error) {
	return db.e.GetEmbeddingsWithUsage(texts)
}

// ─── Run metrics (per-run observability, CONTRACT §1.5) ──────────────────────

// EmbedMetrics is the embed worker's slice of run_metrics columns.
type EmbedMetrics struct {
	JobID        string
	StartedAt    time.Time
	FinishedAt   time.Time
	Model        string
	ChunkCount   int
	PromptTokens *int // provider-reported; nil when Ollama leaves usage zeroed
	TotalTokens  *int // authoritative local tokenizer count; nil (NULL) when unknown — a chunk failed to tokenize
}

// UpsertAudioBytes records the audio file size for a job (the monitor's slice of
// run_metrics). Best-effort: callers should log-and-continue on error so a
// metrics write never fails enqueue. Only the audio_bytes column is touched, so
// it never clobbers the runner's or embed worker's columns on the same row.
func (db *DB) UpsertAudioBytes(ctx context.Context, jobID string, bytes int64) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO run_metrics (job_id, audio_bytes)
		VALUES ($1, $2)
		ON CONFLICT (job_id) DO UPDATE
		SET audio_bytes = EXCLUDED.audio_bytes,
		    updated_at  = now()
	`, jobID, bytes)
	if err != nil {
		return fmt.Errorf("upsert audio_bytes: %w", err)
	}
	return nil
}

// UpsertEmbedMetrics records embedding timing, model, chunk count, and token
// counts for a job (the embed worker's slice of run_metrics). Only the embed_*
// columns are written, so it never clobbers the monitor's audio_bytes or the
// runner's transcription columns on the same row.
//
// Token mapping (see CONTRACT §1.5): embed_total_tokens is the authoritative
// local tokenizer count summed across the embedded chunk texts — but NULL
// (m.TotalTokens == nil) when any chunk failed to tokenize, so a partial count is
// never stored as if it were complete; embed_prompt_tokens is the
// provider-reported usage, stored only when non-zero (Ollama frequently leaves it
// at 0), nullable otherwise.
func (db *DB) UpsertEmbedMetrics(ctx context.Context, m EmbedMetrics) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO run_metrics
		       (job_id, embed_started_at, embed_finished_at, embed_model,
		        embed_chunk_count, embed_prompt_tokens, embed_total_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (job_id) DO UPDATE
		SET embed_started_at    = EXCLUDED.embed_started_at,
		    embed_finished_at   = EXCLUDED.embed_finished_at,
		    embed_model         = EXCLUDED.embed_model,
		    embed_chunk_count   = EXCLUDED.embed_chunk_count,
		    embed_prompt_tokens = EXCLUDED.embed_prompt_tokens,
		    embed_total_tokens  = EXCLUDED.embed_total_tokens,
		    updated_at          = now()
	`, m.JobID, m.StartedAt, m.FinishedAt, m.Model,
		m.ChunkCount, m.PromptTokens, m.TotalTokens)
	if err != nil {
		return fmt.Errorf("upsert embed metrics: %w", err)
	}
	return nil
}

// ─── Book metadata (CONTRACT §1.6) ───────────────────────────────────────────

// upsertBookMetadataSQL is the column-selective UPSERT for book_metadata.
// It is a package-level variable (not a constant) so tests can inspect the
// SQL shape to catch regressions — e.g. verifying that bias_terms is present
// and not accidentally COALESCE-guarded. A full round-trip test requires
// testcontainers (M-8).
//
// Parameter order: $1=book_dir $2=title $3=author $4=narrator $5=series
//
//	$6=asin $7=chapters(jsonb) $8=bias_terms $9=source
var upsertBookMetadataSQL = `
	INSERT INTO book_metadata
	       (book_dir, title, author, narrator, series, asin, chapters, bias_terms, source, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, now())
	ON CONFLICT (book_dir) DO UPDATE
	SET title      = EXCLUDED.title,
	    author     = EXCLUDED.author,
	    narrator   = COALESCE(EXCLUDED.narrator,   book_metadata.narrator),
	    series     = COALESCE(EXCLUDED.series,     book_metadata.series),
	    asin       = COALESCE(EXCLUDED.asin,       book_metadata.asin),
	    chapters   = COALESCE(EXCLUDED.chapters,   book_metadata.chapters),
	    bias_terms = EXCLUDED.bias_terms,
	    source     = EXCLUDED.source,
	    updated_at = now()
`

// UpsertBookMetadata writes or refreshes the book_metadata row for a book
// directory. The UPSERT is column-selective: it always updates title, author,
// source, and bias_terms (every call re-derives bias_terms from the current
// meta so the list stays current when metadata improves), and additionally
// updates narrator, series, asin, and chapters when the provider returned them
// (non-zero values only — a PathProvider result never clobbers ABS-sourced
// chapter data because PathProvider sets none of those fields).
//
// bias_terms is derived by calling metaprovider.DeriveBiasTerms(meta) and is
// always written — even when the derived list is empty (an empty array is
// stored as NULL to stay consistent with "not yet populated" for other
// nullable columns). The runner reads bias_terms to drive NeMo word-boosting
// (CONTRACT §1.6, PR 5).
//
// chapters is serialised as JSONB when non-empty; a nil or empty slice leaves
// the column NULL (not an empty array), consistent with the "not yet populated"
// sentinel used by chapter readers.
//
// Best-effort: callers must log and continue on error so a metadata write
// never fails enqueue. A missing row in book_metadata is always a no-op for
// the rest of the pipeline.
func (db *DB) UpsertBookMetadata(ctx context.Context, bookDir string, meta metaprovider.BookMeta) error {
	// Serialise chapters to JSONB; nil when no chapters (leave column NULL).
	var chaptersJSON []byte
	if len(meta.Chapters) > 0 {
		var err error
		chaptersJSON, err = json.Marshal(meta.Chapters)
		if err != nil {
			return fmt.Errorf("marshal chapters: %w", err)
		}
	}

	// Narrator is only non-empty for "abs" or richer sources. We coerce a nil
	// narrator to NULL by using a pointer — pgx handles *string → NULL cleanly.
	var narrator, series, asin *string
	if meta.Narrator != "" {
		narrator = &meta.Narrator
	}
	if meta.Series != "" {
		series = &meta.Series
	}
	if meta.ASIN != "" {
		asin = &meta.ASIN
	}

	// Derive ASR bias terms from the current metadata. An empty list is stored
	// as NULL (nil slice) rather than an empty array — consistent with the
	// "not yet populated" sentinel used by other nullable columns.
	biasTerms := metaprovider.DeriveBiasTerms(meta)
	var biasTermsArg interface{}
	if len(biasTerms) > 0 {
		biasTermsArg = biasTerms
	}

	_, err := db.pool.Exec(ctx, upsertBookMetadataSQL,
		bookDir, meta.Title, meta.Author, narrator, series, asin, chaptersJSON, biasTermsArg, meta.Source)
	if err != nil {
		return fmt.Errorf("upsert book_metadata: %w", err)
	}
	return nil
}

// GetBookChapters reads the chapters JSONB column from book_metadata for the
// given book directory. Returns nil (not an error) when no row exists or
// chapters is NULL — callers treat nil as "no chapter data yet".
func (db *DB) GetBookChapters(ctx context.Context, bookDir string) ([]metaprovider.Chapter, error) {
	var chaptersJSON []byte
	err := db.pool.QueryRow(ctx, `
		SELECT chapters FROM book_metadata WHERE book_dir = $1
	`, bookDir).Scan(&chaptersJSON)
	if errors.Is(err, pgx.ErrNoRows) || chaptersJSON == nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get book chapters: %w", err)
	}
	var chapters []metaprovider.Chapter
	if err := json.Unmarshal(chaptersJSON, &chapters); err != nil {
		return nil, fmt.Errorf("unmarshal chapters: %w", err)
	}
	return chapters, nil
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

	return db.scanResults(ctx, db.pool, rows)
}

// searchInBookSQL is the book-scoped semantic search.
//
// CRITICAL — why this deliberately does NOT use the HNSW index: an HNSW ANN scan
// finds the global top-K nearest chunks first and *then* applies the WHERE
// filter, so a selective single-book filter (a few hundred chunks out of the
// whole library) under-returns — most of the ANN top-K belong to other books and
// get filtered away, leaving far fewer than `limit` hits (the well-known
// filtered-ANN recall problem). Instead we narrow to the book FIRST via the
// btree on file_path (`transcript_chunks_file_path_idx`, usable under C-collation
// for the `LIKE prefix || '%'` prefix), then do an EXACT distance scan + order
// over just that book's chunks. Exact ordering over a few-hundred-row set is both
// fast and recall-perfect — no ANN approximation. $1=vec, $2=limit, $3=dir
// prefix ("<dir>/%"), $4=threshold (0 disables).
var searchInBookSQL = `
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
	WHERE c.file_path LIKE $3 ESCAPE '\'
	  AND ($4 = 0 OR 1 - (c.embedding <=> $1) >= $4)
	ORDER BY c.embedding <=> $1
	LIMIT $2
`

// SearchInBook performs a vector-similarity search scoped to one book directory.
// It embeds the query, then runs an exact (non-HNSW) distance scan over only that
// book's chunks — see searchInBookSQL for why HNSW is bypassed here. dir must be
// an exact book_dir as returned by GetBookSummaries.
func (db *DB) SearchInBook(ctx context.Context, query, dir string, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	vecs, err := db.e.GetEmbeddings([]string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned for query")
	}
	return db.searchInBook(ctx, db.pool, vecs[0], dir, limit, threshold)
}

// searchInBook is the querier-parameterized core of SearchInBook, split out so
// the exact-scan query + scan path is testable against a mock pool. The dir
// prefix is LIKE-escaped so a book name containing %, _, or \ can't widen scope.
func (db *DB) searchInBook(ctx context.Context, q rowScanner, vec []float32, dir string, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	prefix := likePrefix(dir) + "/%"
	rows, err := q.Query(ctx, searchInBookSQL, pgvector.NewVector(vec), limit, prefix, threshold)
	if err != nil {
		return nil, fmt.Errorf("book similarity query: %w", err)
	}
	defer rows.Close()
	return db.scanResults(ctx, q, rows)
}

// textSearchSQL is the trigram-ranked full-text search query used by TextSearch.
// It is a package-level variable (not a constant) solely so that tests can
// inspect the SQL shape and catch regressions to the old ILIKE-only form.
//
// The query uses:
//   - `c.text % $1`          — trigram similarity operator (GIN-accelerated)
//   - `similarity(c.text, $1) DESC` — similarity-ranked ordering
//   - `ILIKE '%' || $1 || '%'` — fallback for very short queries below the
//     pg_trgm threshold (leading wildcard, so GIN not used for this clause)
var textSearchSQL = `
	SELECT
		c.id,
		c.text,
		c.file_path,
		c.chunk_index,
		c.start_sec,
		c.end_sec,
		c.speaker,
		similarity(c.text, $1) AS similarity,
		(SELECT COUNT(*) FROM transcript_chunks WHERE transcript_id = c.transcript_id) AS total_chunks
	FROM transcript_chunks c
	WHERE c.text % $1
	   OR c.text ILIKE '%' || $1 || '%'
	ORDER BY similarity(c.text, $1) DESC, c.chunk_index ASC
	LIMIT $2
`

// rowQuerier is the narrow slice of the pgx pool API that TextSearch needs to
// run a query. Both *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it, which
// lets TextSearch be exercised at execution level by a pure-Go mock pool in
// tests (no live Postgres required).
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// rowScanner extends rowQuerier with QueryRow, the slice needed by queries that
// fetch a single row plus a follow-up list (e.g. GetTrackDetail). Both
// *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it, so the single-row + chunk
// path is execution-testable with a mock pool.
type rowScanner interface {
	rowQuerier
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TextSearch performs a trigram full-text search over transcript_chunks using
// pg_trgm similarity so the GIN index on text (text_trgm_idx) is usable.
//
// Strategy: rank by trigram similarity descending (best match first), with an
// ILIKE fallback so short substrings that don't meet the similarity threshold
// are still surfaced. The GIN index accelerates the similarity() ranking path;
// the ILIKE clause is an additional safety net for very short queries where
// pg_trgm similarity may be 0. Combined, the results are still sorted by
// similarity DESC so the most relevant chunks appear first.
//
// Tradeoff: ILIKE with a leading wildcard prevents the GIN index from being
// used for that clause alone, but the similarity() predicate keeps overall
// performance good for typical query lengths (3+ chars). For very short
// single-character queries the ILIKE clause dominates; those are rare in
// practice.
func (db *DB) TextSearch(ctx context.Context, query string, limit int) ([]SearchResultWithMetadata, error) {
	return db.textSearch(ctx, db.pool, query, limit)
}

// textSearch is the querier-parameterized core of TextSearch, split out so the
// query execution + scan path can be tested against a mock pool.
func (db *DB) textSearch(ctx context.Context, q rowScanner, query string, limit int) ([]SearchResultWithMetadata, error) {
	rows, err := q.Query(ctx, textSearchSQL, query, limit)
	if err != nil {
		return nil, fmt.Errorf("text search query: %w", err)
	}
	defer rows.Close()

	return db.scanResults(ctx, q, rows)
}

// textSearchInBookSQL is textSearchSQL scoped to one book directory: the same
// trigram-ranked search, but with the chunk's file_path constrained to that
// book's tracks ($3 is the dir prefix, e.g. "<dir>/%"). Same SELECT/scan shape
// as textSearchSQL so scanResults is reused unchanged.
var textSearchInBookSQL = `
	SELECT
		c.id,
		c.text,
		c.file_path,
		c.chunk_index,
		c.start_sec,
		c.end_sec,
		c.speaker,
		similarity(c.text, $1) AS similarity,
		(SELECT COUNT(*) FROM transcript_chunks WHERE transcript_id = c.transcript_id) AS total_chunks
	FROM transcript_chunks c
	WHERE c.file_path LIKE $3 ESCAPE '\'
	  AND (c.text % $1 OR c.text ILIKE '%' || $1 || '%')
	ORDER BY similarity(c.text, $1) DESC, c.chunk_index ASC
	LIMIT $2
`

// TextSearchInBook runs the trigram text search scoped to a single book
// directory (the per-book search box on the book-detail page). dir is an exact
// book_dir as returned by GetBookSummaries; matching is restricted to chunks
// whose file_path is under "<dir>/". Reuses the same SELECT/scan shape as
// TextSearch.
func (db *DB) TextSearchInBook(ctx context.Context, dir, query string, limit int) ([]SearchResultWithMetadata, error) {
	return db.textSearchInBook(ctx, db.pool, dir, query, limit)
}

// textSearchInBook is the querier-parameterized core of TextSearchInBook. The
// dir prefix is built with a LIKE-escape so a book whose name contains %, _, or
// \ can't widen the match.
func (db *DB) textSearchInBook(ctx context.Context, q rowScanner, dir, query string, limit int) ([]SearchResultWithMetadata, error) {
	prefix := likePrefix(dir) + "/%"
	rows, err := q.Query(ctx, textSearchInBookSQL, query, limit, prefix)
	if err != nil {
		return nil, fmt.Errorf("book text search query: %w", err)
	}
	defer rows.Close()
	return db.scanResults(ctx, q, rows)
}

// likePrefix escapes LIKE metacharacters (%, _, \) in a literal so it can be
// used as an exact prefix in a `... LIKE prefix || '%'` pattern. Postgres LIKE
// uses backslash as the default escape character.
func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// scanResults scans a result set from findSimilar, TextSearch, or
// GetChunkContext, populating Author/Title from the MetadataProvider and
// ChapterIndex/ChapterTitle by mapping the chunk's start_sec into the book's
// chapter list stored in book_metadata.chapters.
//
// Chapter mapping: for each result we read book_metadata.chapters (one DB call
// per distinct book_dir; a simple inline call is acceptable here because each
// search result set is ≤50 rows and book_metadata is tiny). The chapter whose
// [StartSec, EndSec) contains the chunk's start_sec is selected. When no chapter
// data exists the ChapterIndex/ChapterTitle stay zero, and the MCP formatter
// already suppresses the label in that case (CONTRACT §2.2.1).
func (db *DB) scanResults(ctx context.Context, q rowScanner, rows pgx.Rows) ([]SearchResultWithMetadata, error) {
	// Cache book chapters per book_dir to avoid redundant DB reads within one
	// result set (a typical 10-result search touches 1–3 books).
	chaptersCache := make(map[string][]metaprovider.Chapter)

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

		// Derive Author/Title using the layout-aware provider so that both
		// 3-level (author/title/track) and 2-level (author/book.m4b) collections
		// return correct labels. Chapter is the audio filename (sans path), which
		// is meaningful for multi-track books and empty-ish for single-file ones.
		bookMeta, _ := db.meta.Lookup(ctx, r.FilePath, r.FilePath)
		r.Author, r.Title = bookMeta.Author, bookMeta.Title
		r.Chapter = filepath.Base(r.FilePath)

		// Chapter mapping: map the chunk's start_sec into the book's chapter list.
		bookDir := filepath.Dir(r.FilePath)
		chapters, seen := chaptersCache[bookDir]
		if !seen {
			var err error
			chapters, err = db.getBookChaptersQ(ctx, q, bookDir)
			if err != nil {
				db.log.Debug("chapter lookup failed (continuing without chapter label)",
					"book_dir", bookDir, "error", err)
			}
			chaptersCache[bookDir] = chapters // cache even on error (nil chapters)
		}
		if idx, title, ok := metaprovider.ChapterForSec(chapters, r.StartSec); ok {
			r.ChapterIndex = idx
			r.ChapterTitle = title
		}

		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (scan results): %w", err)
	}
	return results, nil
}

// getBookChaptersQ reads the chapters JSONB column from book_metadata using the
// supplied rowScanner — this lets scanResults be exercised against a mock pool
// in tests without hitting db.pool (which may be nil in test doubles).
func (db *DB) getBookChaptersQ(ctx context.Context, q rowScanner, bookDir string) ([]metaprovider.Chapter, error) {
	var chaptersJSON []byte
	err := q.QueryRow(ctx, `
		SELECT chapters FROM book_metadata WHERE book_dir = $1
	`, bookDir).Scan(&chaptersJSON)
	if errors.Is(err, pgx.ErrNoRows) || chaptersJSON == nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get book chapters: %w", err)
	}
	var chapters []metaprovider.Chapter
	if err := json.Unmarshal(chaptersJSON, &chapters); err != nil {
		return nil, fmt.Errorf("unmarshal chapters: %w", err)
	}
	return chapters, nil
}

// GetHierarchicalData returns a list of files with their chunk counts for the
// `earmark list` CLI command (a flat per-file chunk listing).
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

	return db.scanResults(ctx, db.pool, rows)
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
	// EmbedBacklog counts completed transcripts that have no chunks yet — i.e.
	// the worker's "needs embedding" set. A healthy pipeline drains this to ~0
	// quickly; a large, non-draining value means embedding is stalling (Ollama
	// down or model missing), which is otherwise invisible because the job rows
	// stay 'done' and never flip to 'failed'.
	EmbedBacklog int
	// TotalJobs is Pending+Claimed+Done+Failed — the full backlog denominator.
	TotalJobs int
	// DoneLastHour is the number of jobs whose row entered 'done' in the last
	// hour, a throughput proxy. (A 'done' row's updated_at is its completion
	// time: the runner's mark-done UPDATE fires the updated_at trigger, and a
	// requeue moves the row out of 'done' rather than re-stamping it. A future
	// completed_at column would make this exact.)
	DoneLastHour int
	// Paused mirrors runner_control.paused — true means the runner is declining
	// to claim new work (set via the dashboard pause toggle).
	Paused bool
	// RunLimit mirrors runner_control.run_limit — nil means unlimited (normal
	// operation); a non-negative value is a bounded run with that many claims
	// remaining (e.g. 1 for a single-job smoke test). The runner decrements it.
	RunLimit *int
	// Runner fields — populated when at least one job has status='claimed'.
	RunnerActive  bool
	RunnerID      string     // claimed_by of the most-recently-updated claimed job
	LastHeartbeat *time.Time // updated_at of that job

	// Per-run aggregates (run_metrics). AvgProcessingSeconds is the mean
	// transcription wall-clock over jobs the runner has timed; TotalEmbedTokens
	// is the summed authoritative local token count over all embedded runs. Both
	// are nil when no run_metrics rows carry the underlying data yet.
	AvgProcessingSeconds *float64
	TotalEmbedTokens     *int64

	// Library-wide totals over indexed content (the overview "library" card).
	// TotalDurationSeconds sums transcripts.duration_seconds across every
	// transcript; TotalWords sums run_metrics.word_count; BooksFullyDone counts
	// book directories whose every track job is 'done'. The first two are nil
	// when nothing is transcribed/metered yet.
	TotalDurationSeconds *float64
	TotalWords           *int64
	BooksFullyDone       int
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

	// Embed backlog: completed transcripts with no chunks yet (mirrors the
	// worker's GetCompletedTranscripts selection). Surfaces a silent embedding
	// stall that never shows up in the job-status counts.
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transcripts t
		WHERE NOT EXISTS (
			SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id
		)
	`).Scan(&q.EmbedBacklog); err != nil {
		return nil, fmt.Errorf("embed backlog count: %w", err)
	}

	// Backfill progress + throughput.
	q.TotalJobs = q.Pending + q.Claimed + q.Done + q.Failed
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transcription_jobs
		WHERE status = 'done' AND updated_at > now() - interval '1 hour'
	`).Scan(&q.DoneLastHour); err != nil {
		return nil, fmt.Errorf("done-last-hour count: %w", err)
	}

	// Global control row (runner_control singleton): pause flag + bounded-run
	// counter. Tolerate a missing row by defaulting to not-paused/unlimited; the
	// init seed normally guarantees it exists.
	if err := db.pool.QueryRow(ctx,
		`SELECT paused, run_limit FROM runner_control WHERE id = 1`,
	).Scan(&q.Paused, &q.RunLimit); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("control row query: %w", err)
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

	// Per-run aggregates from run_metrics. NULL-safe: AVG/SUM over zero matching
	// rows return NULL, scanned into the nilable pointers (so the dashboard shows
	// an em dash until the runner/worker have populated metrics).
	if err := db.pool.QueryRow(ctx, `
		SELECT AVG(EXTRACT(EPOCH FROM (transcribe_finished_at - transcribe_started_at)))
		         FILTER (WHERE transcribe_started_at IS NOT NULL
		                   AND transcribe_finished_at IS NOT NULL),
		       SUM(embed_total_tokens)
		         FILTER (WHERE embed_finished_at IS NOT NULL)
		FROM run_metrics
	`).Scan(&q.AvgProcessingSeconds, &q.TotalEmbedTokens); err != nil {
		return nil, fmt.Errorf("run_metrics aggregates: %w", err)
	}

	// Library-wide indexed totals: duration + words over all transcripts, and the
	// count of book directories whose every track is 'done'. NULL-safe (nilable
	// pointers) so a fresh install shows an em dash, not 0-as-known.
	if err := db.pool.QueryRow(ctx, `
		SELECT
		  (SELECT SUM(duration_seconds) FROM transcripts),
		  (SELECT SUM(word_count) FROM run_metrics),
		  (SELECT COUNT(*) FROM (
		     SELECT regexp_replace(file_path, '/[^/]+$', '') AS book_dir
		     FROM transcription_jobs
		     GROUP BY book_dir
		     HAVING COUNT(*) FILTER (WHERE status <> 'done') = 0
		   ) fully_done)
	`).Scan(&q.TotalDurationSeconds, &q.TotalWords, &q.BooksFullyDone); err != nil {
		return nil, fmt.Errorf("library totals: %w", err)
	}

	return q, nil
}

// ─── Server observation (Servers page) ───────────────────────────────────────

// ServerObservation is the raw, runner-reported activity the Servers page
// merges with the configured ASR_SERVERS list. It has two independent sources
// the DB cannot join — claimed_by is written on the job at claim time, while
// runner_host only appears in run_metrics after the transcript completes — so
// both are returned and attributed to a configured server by token match in the
// mcp layer. (There is no per-runner registry/heartbeat table; see CONTRACT
// §1.4. "Server" state is therefore inferred, never authoritative.)
type ServerObservation struct {
	// LiveRunners is one entry per distinct claimed_by that currently holds a
	// claimed job — the only live-presence signal available (a fresh heartbeat
	// means transcribing; a stale one means a crashed/wedged runner).
	LiveRunners []LiveRunner
	// Hosts is one entry per distinct run_metrics.runner_host — the historical
	// record of what model/mode each host has actually run, and how much.
	Hosts []HostMetrics
}

// LiveRunner is a runner currently holding claimed work, keyed by its free-form
// claimed_by identity string.
type LiveRunner struct {
	ClaimedBy     string
	ClaimedCount  int
	LastHeartbeat time.Time
	CurrentFile   string // most-recently-claimed file_path (the in-flight track)
}

// HostMetrics aggregates run_metrics for one runner_host: its most-recent model
// and compute mode, jobs transcribed, last completion, and mean wall-clock.
//
// The ASR backend-descriptor fields (Family/Runtime/CapsApplied/
// CapsSkippedReason/MeanWordConfidence, CONTRACT §1.5 / §2.13) surface what the
// host's most-recent run actually reported. They are runner-written and
// best-effort, so all are nil/empty until a runner that populates them has run
// on this host — the Phase-2 dashboard renders nil as "unknown", never an error.
type HostMetrics struct {
	Host                 string
	ASRModel             *string
	ComputeType          *string
	JobsDone             int
	LastFinished         *time.Time
	AvgProcessingSeconds *float64

	// ASRFamily / ASRRuntime are the most-recent non-null family/runtime ids
	// reported by a run on this host (free-form strings; see asr.KnownFamily).
	ASRFamily  *string
	ASRRuntime *string
	// CapsApplied / CapsSkippedReason are the capability maps from the host's
	// most-recent run that reported them. CapsApplied is key→bool (what ran);
	// CapsSkippedReason is key→reason (why a requested cap was declined). Nil
	// when no run on this host has reported them.
	CapsApplied       asr.Capabilities
	CapsSkippedReason map[string]string
	// MeanWordConfidence is the most-recent non-null mean per-word confidence
	// (0–1) reported for this host; nil when the model emits no scores.
	MeanWordConfidence *float64
}

// GetServerObservation returns the live claimed-runner set and the per-host
// run_metrics aggregates. Both lists are empty (not nil-erroring) on a fresh
// install. Callers attribute these to configured servers by substring match.
func (db *DB) GetServerObservation(ctx context.Context) (*ServerObservation, error) {
	obs := &ServerObservation{}

	// Live runners: one row per claimed_by currently holding claimed work, with
	// its freshest heartbeat (max updated_at), how many jobs it holds, and the
	// most-recently-claimed file (the in-flight track).
	liveRows, err := db.pool.Query(ctx, `
		SELECT claimed_by,
		       COUNT(*)        AS claimed_count,
		       MAX(updated_at) AS last_heartbeat,
		       (ARRAY_AGG(file_path ORDER BY updated_at DESC))[1] AS current_file
		FROM transcription_jobs
		WHERE status = 'claimed' AND claimed_by IS NOT NULL AND claimed_by <> ''
		GROUP BY claimed_by
		ORDER BY last_heartbeat DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("live runners query: %w", err)
	}
	defer liveRows.Close()
	for liveRows.Next() {
		var lr LiveRunner
		if err := liveRows.Scan(&lr.ClaimedBy, &lr.ClaimedCount, &lr.LastHeartbeat, &lr.CurrentFile); err != nil {
			return nil, fmt.Errorf("scan live runner: %w", err)
		}
		obs.LiveRunners = append(obs.LiveRunners, lr)
	}
	if err := liveRows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (live runners): %w", err)
	}

	// Per-host metrics: latest non-null model/compute mode (by run recency),
	// jobs transcribed, last completion, mean transcription wall-clock, plus the
	// most-recent non-null ASR backend descriptor (family/runtime/applied caps/
	// skipped reasons/mean confidence — CONTRACT §1.5 / §2.13). The caps_* JSONB
	// columns use the same "latest non-null" recency pick as model/compute so a
	// run that didn't report them never blanks an earlier run that did.
	hostRows, err := db.pool.Query(ctx, `
		SELECT runner_host,
		       (ARRAY_AGG(asr_model    ORDER BY updated_at DESC) FILTER (WHERE asr_model    IS NOT NULL))[1] AS asr_model,
		       (ARRAY_AGG(compute_type ORDER BY updated_at DESC) FILTER (WHERE compute_type IS NOT NULL))[1] AS compute_type,
		       COUNT(*) AS jobs_done,
		       MAX(transcribe_finished_at) AS last_finished,
		       AVG(EXTRACT(EPOCH FROM (transcribe_finished_at - transcribe_started_at)))
		         FILTER (WHERE transcribe_started_at IS NOT NULL
		                   AND transcribe_finished_at IS NOT NULL) AS avg_proc,
		       (ARRAY_AGG(asr_family  ORDER BY updated_at DESC) FILTER (WHERE asr_family  IS NOT NULL))[1] AS asr_family,
		       (ARRAY_AGG(asr_runtime ORDER BY updated_at DESC) FILTER (WHERE asr_runtime IS NOT NULL))[1] AS asr_runtime,
		       (ARRAY_AGG(caps_applied        ORDER BY updated_at DESC) FILTER (WHERE caps_applied        IS NOT NULL))[1] AS caps_applied,
		       (ARRAY_AGG(caps_skipped_reason ORDER BY updated_at DESC) FILTER (WHERE caps_skipped_reason IS NOT NULL))[1] AS caps_skipped_reason,
		       (ARRAY_AGG(mean_word_confidence ORDER BY updated_at DESC) FILTER (WHERE mean_word_confidence IS NOT NULL))[1] AS mean_word_confidence
		FROM run_metrics
		WHERE runner_host IS NOT NULL AND runner_host <> ''
		GROUP BY runner_host
		ORDER BY last_finished DESC NULLS LAST
	`)
	if err != nil {
		return nil, fmt.Errorf("host metrics query: %w", err)
	}
	defer hostRows.Close()
	for hostRows.Next() {
		var (
			hm                    HostMetrics
			capsAppliedJSON       []byte
			capsSkippedReasonJSON []byte
		)
		if err := hostRows.Scan(&hm.Host, &hm.ASRModel, &hm.ComputeType, &hm.JobsDone,
			&hm.LastFinished, &hm.AvgProcessingSeconds,
			&hm.ASRFamily, &hm.ASRRuntime,
			&capsAppliedJSON, &capsSkippedReasonJSON, &hm.MeanWordConfidence); err != nil {
			return nil, fmt.Errorf("scan host metrics: %w", err)
		}
		// caps_* are best-effort runner telemetry: a malformed/legacy value is
		// dropped (logged) rather than failing the whole observation query.
		if len(capsAppliedJSON) > 0 {
			var raw map[string]bool
			if err := json.Unmarshal(capsAppliedJSON, &raw); err != nil {
				db.log.Warn("dropping malformed run_metrics.caps_applied", "host", hm.Host, "error", err)
			} else {
				hm.CapsApplied = asr.ParseCapabilities(raw, "run_metrics.caps_applied")
			}
		}
		if len(capsSkippedReasonJSON) > 0 {
			var reasons map[string]string
			if err := json.Unmarshal(capsSkippedReasonJSON, &reasons); err != nil {
				db.log.Warn("dropping malformed run_metrics.caps_skipped_reason", "host", hm.Host, "error", err)
			} else {
				hm.CapsSkippedReason = filterKnownCapReasons(reasons)
			}
		}
		obs.Hosts = append(obs.Hosts, hm)
	}
	if err := hostRows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (host metrics): %w", err)
	}

	return obs, nil
}

// filterKnownCapReasons keeps only skipped-reason entries whose key is a known
// capability (CONTRACT §2.13 closed enum), dropping the rest for forward-compat
// — the same drop-unknown-keys policy asr.ParseCapabilities applies to bool
// maps. Returns nil when nothing remains.
func filterKnownCapReasons(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if asr.IsKnownCapability(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RecentJob is a lightweight view of a transcription_jobs row for the recent-
// activity table on the status dashboard. The Metrics-* fields are populated by
// a LEFT JOIN on run_metrics and are nil when no metrics row exists yet (or for
// callers that don't join, e.g. GetBookTracks).
type RecentJob struct {
	ID        string
	FilePath  string
	Status    string
	UpdatedAt time.Time
	Error     *string

	// Per-run metrics (run_metrics LEFT JOIN; all nullable).
	ProcessingSeconds *float64 // transcribe_finished_at - transcribe_started_at
	Chunked           *bool
	NWindows          *int
	CharCount         *int
	EmbedTotalTokens  *int

	// Per-track detail (transcripts + run_metrics LEFT JOINs; all nullable).
	// Populated by GetBookTracks for the surfaced book-detail columns; nil on the
	// recent-activity path (GetRecentJobs doesn't select them).
	DurationSeconds *float64 // transcripts.duration_seconds
	WordCount       *int     // run_metrics.word_count
	AudioCodec      *string  // run_metrics.audio_codec (e.g. "aac")
	AudioChannels   *int     // run_metrics.audio_channels (e.g. 2 → "stereo")
	EmbedChunkCount *int     // run_metrics.embed_chunk_count
}

// GetRecentJobs returns the most-recently-updated jobs, newest first. It
// LEFT JOINs run_metrics to surface per-run observability (processing time,
// chunked flag, window/char counts, embed tokens) — all nil when no metrics
// row exists yet.
func (db *DB) GetRecentJobs(ctx context.Context, limit int) ([]RecentJob, error) {
	if limit <= 0 {
		limit = 15
	}
	rows, err := db.pool.Query(ctx, `
		SELECT j.id, j.file_path, j.status, j.updated_at, j.error,
		       EXTRACT(EPOCH FROM (m.transcribe_finished_at - m.transcribe_started_at)) AS processing_seconds,
		       m.chunked, m.n_windows, m.char_count, m.embed_total_tokens
		FROM transcription_jobs j
		LEFT JOIN run_metrics m ON m.job_id = j.id
		ORDER BY j.updated_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent jobs query: %w", err)
	}
	defer rows.Close()

	var jobs []RecentJob
	for rows.Next() {
		var j RecentJob
		if err := rows.Scan(&j.ID, &j.FilePath, &j.Status, &j.UpdatedAt, &j.Error,
			&j.ProcessingSeconds, &j.Chunked, &j.NWindows, &j.CharCount, &j.EmbedTotalTokens,
		); err != nil {
			return nil, fmt.Errorf("scan recent job: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (recent jobs): %w", err)
	}
	return jobs, nil
}

// ─── Pause control ───────────────────────────────────────────────────────────

// GetPaused returns the global pause flag from runner_control. A missing row
// (which init normally seeds) is treated as not-paused.
func (db *DB) GetPaused(ctx context.Context) (bool, error) {
	var paused bool
	err := db.pool.QueryRow(ctx, `SELECT paused FROM runner_control WHERE id = 1`).Scan(&paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get paused: %w", err)
	}
	return paused, nil
}

// SetPaused writes the global pause flag. by records who toggled it (e.g.
// "dashboard") for the audit column. Upserts the singleton row so it works even
// if the seed insert was somehow skipped.
func (db *DB) SetPaused(ctx context.Context, paused bool, by string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO runner_control (id, paused, updated_at, updated_by)
		VALUES (1, $1, now(), $2)
		ON CONFLICT (id) DO UPDATE
			SET paused = EXCLUDED.paused,
			    updated_at = EXCLUDED.updated_at,
			    updated_by = EXCLUDED.updated_by
	`, paused, by)
	if err != nil {
		return fmt.Errorf("set paused: %w", err)
	}
	return nil
}

// GetControl returns the full runner_control state: the pause flag and the
// bounded-run counter (nil = unlimited). A missing row is treated as
// not-paused/unlimited so callers degrade safely.
func (db *DB) GetControl(ctx context.Context) (paused bool, runLimit *int, err error) {
	err = db.pool.QueryRow(ctx,
		`SELECT paused, run_limit FROM runner_control WHERE id = 1`,
	).Scan(&paused, &runLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("get control: %w", err)
	}
	return paused, runLimit, nil
}

// SetRunLimit writes the bounded-run counter without touching the pause flag.
// A nil limit clears the bound (back to unlimited); a non-negative value starts
// a bounded run of that many claims. The runner — not this method — performs the
// per-claim decrement, so the counter and the claim stay atomic on the runner
// side; here we only set the target. by records who set it for the audit column.
func (db *DB) SetRunLimit(ctx context.Context, limit *int, by string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO runner_control (id, paused, run_limit, updated_at, updated_by)
		VALUES (1, false, $1, now(), $2)
		ON CONFLICT (id) DO UPDATE
			SET run_limit = EXCLUDED.run_limit,
			    updated_at = EXCLUDED.updated_at,
			    updated_by = EXCLUDED.updated_by
	`, limit, by)
	if err != nil {
		return fmt.Errorf("set run limit: %w", err)
	}
	return nil
}

// ─── Library (book-grouped jobs) ─────────────────────────────────────────────

// BookSummary aggregates all track jobs that share a book directory (the parent
// directory of the audio file). Author/Title are NOT derived here — the caller
// applies a MetadataProvider (config-driven) to Dir + SamplePath, since the
// right author/title split depends on each collection's directory shape.
type BookSummary struct {
	Dir         string // book directory (group key): dirname(file_path)
	SamplePath  string // one file_path within the book (for filename-based title parsing)
	Author      string // populated by the caller via MetadataProvider
	Title       string // populated by the caller via MetadataProvider
	Total       int
	Pending     int
	Claimed     int
	Done        int
	Failed      int
	LastUpdated time.Time

	// Per-book aggregates over the book's done tracks (transcripts + run_metrics
	// LEFT JOINs). All nullable — a book with no transcribed track, or whose
	// transcripts predate run_metrics, sums to NULL → nil here (rendered as an em
	// dash / 0 by the caller). DurationSeconds sums transcripts.duration_seconds;
	// WordCount and EmbedChunkCount sum the matching run_metrics columns.
	DurationSeconds *float64
	WordCount       *int
	EmbedChunkCount *int
}

// BookFilter narrows and paginates GetBookSummaries.
type BookFilter struct {
	Status string // "", "pending", "claimed", "done", "failed" — books having ≥1 track in this status
	Query  string // case-insensitive substring match on file_path (author/title/track)
	Limit  int    // page size (defaulted if ≤ 0)
	Offset int    // page offset
}

// GetBookSummaries returns one row per book directory, aggregating track-job
// counts, plus the total number of matching books (for pagination). Books are
// ordered transcribed-first (by done-ratio, then done count) so completed books
// lead the list; ties break on most-recently-updated then book_dir.
func (db *DB) GetBookSummaries(ctx context.Context, f BookFilter) ([]BookSummary, int, error) {
	return db.getBookSummaries(ctx, db.pool, f)
}

// getBookSummaries is the querier-parameterized core of GetBookSummaries, split
// out so the dynamic query + scan path can be tested against a mock pool.
func (db *DB) getBookSummaries(ctx context.Context, qr rowQuerier, f BookFilter) ([]BookSummary, int, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	// status is validated against an allow-list so it can be interpolated into
	// the HAVING filter safely; everything else is a bound parameter.
	statusHaving := ""
	switch f.Status {
	case "":
		// no status filter
	case "pending", "claimed", "done", "failed":
		statusHaving = fmt.Sprintf(
			"HAVING COUNT(*) FILTER (WHERE j.status = '%s') > 0", f.Status)
	default:
		return nil, 0, fmt.Errorf("invalid status filter: %q", f.Status)
	}

	// COUNT(*) OVER() yields the total matching-book count alongside the page so
	// pagination needs only one round-trip. $1=query, $2=limit, $3=offset.
	// The per-book LEFT JOINs to transcripts/run_metrics let us SUM the stored
	// duration / word / embed-chunk totals across each book's tracks. A track with
	// no transcript (pending) or no run_metrics row contributes NULL to its SUM;
	// SUM ignores NULLs, and a book with zero contributing rows sums to NULL →
	// nilable pointer here (em dash in the UI). The join multiplies job rows only
	// by 0-or-1 (transcripts.job_id and run_metrics.job_id are both unique per
	// job), so the status COUNTs are unaffected.
	query := fmt.Sprintf(`
		WITH books AS (
			SELECT
				regexp_replace(j.file_path, '/[^/]+$', '')        AS book_dir,
				MIN(j.file_path)                                    AS sample_path,
				COUNT(*)                                            AS total,
				COUNT(*) FILTER (WHERE j.status = 'pending')        AS pending,
				COUNT(*) FILTER (WHERE j.status = 'claimed')        AS claimed,
				COUNT(*) FILTER (WHERE j.status = 'done')           AS done,
				COUNT(*) FILTER (WHERE j.status = 'failed')         AS failed,
				MAX(j.updated_at)                                   AS last_updated,
				SUM(t.duration_seconds)                             AS duration_seconds,
				SUM(m.word_count)                                   AS word_count,
				SUM(m.embed_chunk_count)                            AS embed_chunk_count
			FROM transcription_jobs j
			LEFT JOIN transcripts t ON t.job_id = j.id
			LEFT JOIN run_metrics m ON m.job_id = j.id
			WHERE ($1 = '' OR j.file_path ILIKE '%%' || $1 || '%%')
			GROUP BY book_dir
			%s
		)
		SELECT book_dir, sample_path, total, pending, claimed, done, failed, last_updated,
		       duration_seconds, word_count, embed_chunk_count,
		       COUNT(*) OVER() AS total_books
		FROM books
		-- Lead with transcribed books: fully-done first (done-ratio = 1), then
		-- partially-done, then fully-pending (ratio 0). Within a tier, the most
		-- recently-updated book surfaces first, with book_dir as a stable tiebreak.
		ORDER BY (done::float8 / NULLIF(total, 0)) DESC NULLS LAST,
		         done DESC,
		         last_updated DESC,
		         book_dir
		LIMIT $2 OFFSET $3
	`, statusHaving)

	rows, err := qr.Query(ctx, query, f.Query, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("book summaries query: %w", err)
	}
	defer rows.Close()

	var (
		out   []BookSummary
		total int
	)
	for rows.Next() {
		var b BookSummary
		if err := rows.Scan(&b.Dir, &b.SamplePath, &b.Total, &b.Pending, &b.Claimed, &b.Done,
			&b.Failed, &b.LastUpdated, &b.DurationSeconds, &b.WordCount, &b.EmbedChunkCount,
			&total); err != nil {
			return nil, 0, fmt.Errorf("scan book summary: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("rows error (book summaries): %w", err)
	}
	return out, total, nil
}

// LibraryTotals are library-wide book counts for the list_books summary line.
// These are TRUE totals across the whole library, independent of the current
// page or any author filter — so a single page can report the real library shape.
type LibraryTotals struct {
	TotalBooks       int // distinct book directories
	FullyTranscribed int // books whose every track is 'done'
	WithPending      int // books with ≥1 track not yet 'done' (pending/claimed/failed)
}

// GetLibraryTotals returns whole-library book counts (total, fully-transcribed,
// with-pending) for the list_books summary line. It groups transcription_jobs by
// book directory once. Pass query to scope the totals to the same author filter
// list_books uses (empty = whole library) so the summary matches the filtered view.
func (db *DB) GetLibraryTotals(ctx context.Context, query string) (LibraryTotals, error) {
	return db.getLibraryTotals(ctx, db.pool, query)
}

// getLibraryTotals is the querier-parameterized core of GetLibraryTotals, split
// out so the count query + scan can be tested against a mock pool.
func (db *DB) getLibraryTotals(ctx context.Context, qr rowScanner, query string) (LibraryTotals, error) {
	var t LibraryTotals
	// One pass: group jobs by book_dir, then count books overall vs. those with
	// no non-done track (fully transcribed). $1 mirrors the list_books ILIKE filter.
	err := qr.QueryRow(ctx, `
		WITH books AS (
			SELECT regexp_replace(file_path, '/[^/]+$', '') AS book_dir,
			       COUNT(*) FILTER (WHERE status <> 'done') AS not_done
			FROM transcription_jobs
			WHERE ($1 = '' OR file_path ILIKE '%' || $1 || '%')
			GROUP BY book_dir
		)
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE not_done = 0),
		       COUNT(*) FILTER (WHERE not_done > 0)
		FROM books
	`, query).Scan(&t.TotalBooks, &t.FullyTranscribed, &t.WithPending)
	if err != nil {
		return LibraryTotals{}, fmt.Errorf("library totals query: %w", err)
	}
	return t, nil
}

// GetBookTracks returns the individual track jobs for one book directory,
// ordered by file path, for the expand-to-tracks view. dir must be an exact
// book_dir as returned by GetBookSummaries.
//
// It LEFT JOINs transcripts and run_metrics to surface per-track detail
// (duration, word count, processing time, codec/channels, embed chunk count).
// Most transcripts have no run_metrics row yet — and a pending track has no
// transcript row at all — so every joined column is nullable and the caller
// renders an em dash when nil. The em-dash rendering lives in the templates.
func (db *DB) GetBookTracks(ctx context.Context, dir string) ([]RecentJob, error) {
	return db.getBookTracks(ctx, db.pool, dir)
}

// bookTracksSQL is the per-track detail query, a package-level variable so tests
// can assert its shape (the LEFT JOINs that make every metric nullable).
var bookTracksSQL = `
		SELECT j.id, j.file_path, j.status, j.updated_at, j.error,
		       t.duration_seconds,
		       EXTRACT(EPOCH FROM (m.transcribe_finished_at - m.transcribe_started_at)) AS processing_seconds,
		       m.word_count, m.audio_codec, m.audio_channels, m.embed_chunk_count
		FROM transcription_jobs j
		LEFT JOIN transcripts  t ON t.job_id = j.id
		LEFT JOIN run_metrics  m ON m.job_id = j.id
		WHERE regexp_replace(j.file_path, '/[^/]+$', '') = $1
		ORDER BY j.file_path
	`

// getBookTracks is the querier-parameterized core of GetBookTracks, split out so
// the query execution + scan path can be tested against a mock pool.
func (db *DB) getBookTracks(ctx context.Context, q rowQuerier, dir string) ([]RecentJob, error) {
	rows, err := q.Query(ctx, bookTracksSQL, dir)
	if err != nil {
		return nil, fmt.Errorf("book tracks query: %w", err)
	}
	defer rows.Close()

	var jobs []RecentJob
	for rows.Next() {
		var j RecentJob
		if err := rows.Scan(&j.ID, &j.FilePath, &j.Status, &j.UpdatedAt, &j.Error,
			&j.DurationSeconds, &j.ProcessingSeconds,
			&j.WordCount, &j.AudioCodec, &j.AudioChannels, &j.EmbedChunkCount,
		); err != nil {
			return nil, fmt.Errorf("scan book track: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (book tracks): %w", err)
	}
	return jobs, nil
}

// ─── Track detail (single-track page) ────────────────────────────────────────

// ChunkRow is one transcript_chunks row, trimmed to what the track-detail chunk
// list renders: ordinal, time range, and the text length (not the full text).
type ChunkRow struct {
	ChunkIndex int
	StartSec   float64
	EndSec     float64
	CharCount  int     // len([]rune(text)) — characters, not bytes
	Speaker    *string // dominant speaker, or nil
}

// TrackDetail is the full per-track view for the /track page: the job row plus
// its (optional) transcript and (optional) run_metrics, the transcript's
// segments (the timestamped reader), and its chunk list. HasTranscript is false
// for a pending/claimed/failed track that has no transcripts row yet — the page
// renders a "not transcribed yet" state in that case. Every transcript/metric
// field is nullable because most rows have no run_metrics and pending tracks
// have no transcript at all.
type TrackDetail struct {
	// Job (always present).
	ID        string
	FilePath  string
	Status    string
	UpdatedAt time.Time
	Error     *string
	Attempts  int
	ClaimedBy *string

	// Transcript (present only when HasTranscript).
	HasTranscript   bool
	Language        string
	DurationSeconds float64
	SpeakerCount    *int
	ModelName       string
	TranscriptAt    time.Time
	Segments        []Segment

	// run_metrics (all nullable — most rows have none).
	AudioBytes        *int64
	AudioChannels     *int
	AudioSampleRate   *int
	AudioCodec        *string
	AudioFormat       *string
	ProcessingSeconds *float64
	ASRModel          *string
	ComputeType       *string
	RunnerHost        *string
	Chunked           *bool
	NWindows          *int
	CharCount         *int
	WordCount         *int
	SegmentCount      *int
	EmbedModel        *string
	EmbedChunkCount   *int
	EmbedPromptTokens *int
	EmbedTotalTokens  *int

	// Embedded chunks (empty until the worker embeds the transcript).
	Chunks []ChunkRow
}

// trackDetailSQL fetches the job row plus its optional transcript and optional
// run_metrics in one LEFT-JOINed row. `t.id IS NOT NULL` (aliased
// has_transcript) tells the caller whether a transcript exists at all.
var trackDetailSQL = `
		SELECT j.id, j.file_path, j.status, j.updated_at, j.error, j.attempts, j.claimed_by,
		       (t.id IS NOT NULL)         AS has_transcript,
		       t.language, t.duration_seconds, t.speaker_count, t.model_name, t.created_at, t.segments,
		       m.audio_bytes, m.audio_channels, m.audio_sample_rate, m.audio_codec, m.audio_format,
		       EXTRACT(EPOCH FROM (m.transcribe_finished_at - m.transcribe_started_at)) AS processing_seconds,
		       m.asr_model, m.compute_type, m.runner_host, m.chunked, m.n_windows,
		       m.char_count, m.word_count, m.segment_count,
		       m.embed_model, m.embed_chunk_count, m.embed_prompt_tokens, m.embed_total_tokens
		FROM transcription_jobs j
		LEFT JOIN transcripts  t ON t.job_id = j.id
		LEFT JOIN run_metrics  m ON m.job_id = j.id
		WHERE j.id = $1::uuid
	`

// GetTrackDetail returns the full per-track view for one job UUID: the job, its
// optional transcript (with segments for the timestamped reader), its optional
// run_metrics, and its embedded chunk list. Returns pgx.ErrNoRows if no job has
// that id, which the handler maps to a 404.
func (db *DB) GetTrackDetail(ctx context.Context, jobID string) (*TrackDetail, error) {
	return db.getTrackDetail(ctx, db.pool, jobID)
}

// getTrackDetail is the querier-parameterized core of GetTrackDetail, split out
// so the single-row + chunk-list path can be tested against a mock pool.
func (db *DB) getTrackDetail(ctx context.Context, q rowScanner, jobID string) (*TrackDetail, error) {
	var d TrackDetail
	var (
		hasTranscript   bool
		language        *string
		durationSeconds *float64
		modelName       *string
		transcriptAt    *time.Time
		segJSON         []byte
	)
	err := q.QueryRow(ctx, trackDetailSQL, jobID).Scan(
		&d.ID, &d.FilePath, &d.Status, &d.UpdatedAt, &d.Error, &d.Attempts, &d.ClaimedBy,
		&hasTranscript,
		&language, &durationSeconds, &d.SpeakerCount, &modelName, &transcriptAt, &segJSON,
		&d.AudioBytes, &d.AudioChannels, &d.AudioSampleRate, &d.AudioCodec, &d.AudioFormat,
		&d.ProcessingSeconds,
		&d.ASRModel, &d.ComputeType, &d.RunnerHost, &d.Chunked, &d.NWindows,
		&d.CharCount, &d.WordCount, &d.SegmentCount,
		&d.EmbedModel, &d.EmbedChunkCount, &d.EmbedPromptTokens, &d.EmbedTotalTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("track detail query: %w", err)
	}

	d.HasTranscript = hasTranscript
	if hasTranscript {
		if language != nil {
			d.Language = *language
		}
		if durationSeconds != nil {
			d.DurationSeconds = *durationSeconds
		}
		if modelName != nil {
			d.ModelName = *modelName
		}
		if transcriptAt != nil {
			d.TranscriptAt = *transcriptAt
		}
		if len(segJSON) > 0 {
			if err := json.Unmarshal(segJSON, &d.Segments); err != nil {
				return nil, fmt.Errorf("unmarshal segments: %w", err)
			}
		}
	}

	// Chunk list (empty until embedded). Keyed on the job's transcript; a missing
	// transcript yields no rows.
	chunks, err := db.getTrackChunks(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("get track chunks for %s: %w", jobID, err)
	}
	d.Chunks = chunks
	return &d, nil
}

// trackChunksSQL fetches a job's embedded chunks, trimmed to the ordinal /
// time-range / char-length / speaker the chunk list shows.
var trackChunksSQL = `
		SELECT c.chunk_index, c.start_sec, c.end_sec,
		       char_length(c.text) AS char_count, c.speaker
		FROM transcript_chunks c
		JOIN transcripts t ON t.id = c.transcript_id
		WHERE t.job_id = $1::uuid
		ORDER BY c.chunk_index
	`

// getTrackChunks returns the embedded chunks for a job's transcript.
func (db *DB) getTrackChunks(ctx context.Context, q rowQuerier, jobID string) ([]ChunkRow, error) {
	rows, err := q.Query(ctx, trackChunksSQL, jobID)
	if err != nil {
		return nil, fmt.Errorf("track chunks query: %w", err)
	}
	defer rows.Close()

	var out []ChunkRow
	for rows.Next() {
		var c ChunkRow
		if err := rows.Scan(&c.ChunkIndex, &c.StartSec, &c.EndSec, &c.CharCount, &c.Speaker); err != nil {
			return nil, fmt.Errorf("scan track chunk: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (track chunks): %w", err)
	}
	return out, nil
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

// PruneAppleDoubleJobs deletes any jobs whose filename is a macOS AppleDouble
// sidecar (basename begins with "._"). These are metadata files that were
// enqueued before the monitor learned to skip them; their transcripts and
// chunks cascade-delete. Returns the number of rows removed. Idempotent.
func (db *DB) PruneAppleDoubleJobs(ctx context.Context) (int, error) {
	tag, err := db.pool.Exec(ctx, `
		DELETE FROM transcription_jobs
		WHERE file_path ~ '(^|/)\._[^/]*$'
	`)
	if err != nil {
		return 0, fmt.Errorf("prune appledouble jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
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

// FailedJob is a rich view of a failed job for the dashboard failures view —
// includes the full error, retry count, and which runner last claimed it.
type FailedJob struct {
	ID        string
	FilePath  string
	Error     *string
	Attempts  int
	ClaimedBy *string
	UpdatedAt time.Time
}

// GetFailedJobs returns every job in the 'failed' state with full triage detail,
// newest failure first.
func (db *DB) GetFailedJobs(ctx context.Context) ([]FailedJob, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, file_path, error, attempts, claimed_by, updated_at
		FROM   transcription_jobs
		WHERE  status = 'failed'
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("get failed jobs: %w", err)
	}
	defer rows.Close()

	var out []FailedJob
	for rows.Next() {
		var f FailedJob
		if err := rows.Scan(&f.ID, &f.FilePath, &f.Error, &f.Attempts, &f.ClaimedBy, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan failed job: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (failed jobs): %w", err)
	}
	return out, nil
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

// RequeueByID re-runs the pipeline for a single job, identified by its UUID
// (used by the dashboard's per-row requeue button). Returns the job's file path,
// or an error if no job has that id.
func (db *DB) RequeueByID(ctx context.Context, id string) (string, error) {
	paths, err := db.requeue(ctx, requeueByID, id)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no job with id %s", id)
	}
	return paths[0], nil
}

// RequeueByDir re-runs the full pipeline for every track in one book directory
// (exact dirname match). Returns the file paths that were reset.
func (db *DB) RequeueByDir(ctx context.Context, dir string) ([]string, error) {
	return db.requeue(ctx, requeueByDir, dir)
}

// requeuePlan is a pair of fully-formed, static SQL statements for one requeue
// selector. The statements are package constants — nothing is concatenated at
// runtime, so the only dynamic input is the bound $1 parameter (when present).
type requeuePlan struct {
	deleteTranscripts string // delete transcripts for the selected jobs (chunks cascade)
	resetJobs         string // reset those jobs to pending; RETURNING id, file_path
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
			RETURNING id, file_path`,
	}
	// requeueFailed selects every job in the 'failed' state (no parameters).
	requeueFailed = requeuePlan{
		deleteTranscripts: `DELETE FROM transcripts
			WHERE job_id IN (SELECT id FROM transcription_jobs WHERE status = 'failed')`,
		resetJobs: `UPDATE transcription_jobs
			SET    status = 'pending', attempts = 0, error = NULL,
			       claimed_by = NULL, claimed_at = NULL, updated_at = now()
			WHERE  status = 'failed'
			RETURNING id, file_path`,
	}
	// requeueByID selects a single job by its UUID ($1, cast for safety).
	requeueByID = requeuePlan{
		deleteTranscripts: `DELETE FROM transcripts
			WHERE job_id IN (SELECT id FROM transcription_jobs WHERE id = $1::uuid)`,
		resetJobs: `UPDATE transcription_jobs
			SET    status = 'pending', attempts = 0, error = NULL,
			       claimed_by = NULL, claimed_at = NULL, updated_at = now()
			WHERE  id = $1::uuid
			RETURNING id, file_path`,
	}
	// requeueByDir selects every track job in one book directory: an exact match
	// on dirname(file_path) ($1). Used by the per-book "requeue book" action.
	requeueByDir = requeuePlan{
		deleteTranscripts: `DELETE FROM transcripts
			WHERE job_id IN (
				SELECT id FROM transcription_jobs
				WHERE regexp_replace(file_path, '/[^/]+$', '') = $1)`,
		resetJobs: `UPDATE transcription_jobs
			SET    status = 'pending', attempts = 0, error = NULL,
			       claimed_by = NULL, claimed_at = NULL, updated_at = now()
			WHERE  regexp_replace(file_path, '/[^/]+$', '') = $1
			RETURNING id, file_path`,
	}
)

// requeueDeleteMetricsSQL drops the run_metrics rows for the requeued jobs in the
// same transaction. run_metrics references transcription_jobs (not transcripts),
// so deleting the transcript does NOT cascade to it — and requeue UPDATEs the job
// row rather than deleting it, so the ON DELETE CASCADE never fires either. Left
// untouched, the run_metrics row describes a now-deleted transcript (orphaned
// telemetry that mis-reports the new run). $1 is the requeued job-id array.
const requeueDeleteMetricsSQL = `DELETE FROM run_metrics WHERE job_id = ANY($1)`

// txQuerier is the slice of pgx.Tx used by the requeue core. Both pgx.Tx and
// pgxmock's transaction handle satisfy it, so requeueTx is execution-testable
// against a mock pool (ExpectBegin/ExpectExec/ExpectQuery/ExpectCommit).
type txQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// requeue runs a requeuePlan's statements in one transaction: delete the selected
// transcripts (chunks cascade), reset the jobs to pending, and clear the now-stale
// run_metrics rows for those jobs. args are the bound parameters for the plan's
// $N placeholders (one for by-path/by-id/by-dir, none for failed).
func (db *DB) requeue(ctx context.Context, plan requeuePlan, args ...any) ([]string, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin requeue tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	paths, err := requeueTx(ctx, tx, plan, args...)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit requeue tx: %w", err)
	}
	return paths, nil
}

// requeueTx is the transaction-body core of requeue, split out so the
// delete-transcripts → reset-jobs → clear-metrics sequence is testable against a
// pgxmock transaction. It does NOT begin/commit — the caller owns the tx
// lifecycle. Returns the reset jobs' file paths.
func requeueTx(ctx context.Context, tx txQuerier, plan requeuePlan, args ...any) ([]string, error) {
	if _, err := tx.Exec(ctx, plan.deleteTranscripts, args...); err != nil {
		return nil, fmt.Errorf("delete transcripts: %w", err)
	}

	rows, err := tx.Query(ctx, plan.resetJobs, args...)
	if err != nil {
		return nil, fmt.Errorf("reset jobs: %w", err)
	}
	ids, paths, err := scanIDPaths(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}

	// Clear the orphaned run_metrics for the requeued jobs so the next run's
	// telemetry starts clean. A no-op when nothing was reset.
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, requeueDeleteMetricsSQL, ids); err != nil {
			return nil, fmt.Errorf("delete run_metrics: %w", err)
		}
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

// scanIDPaths scans the (id, file_path) pairs RETURNING'd by a requeue reset, so
// the caller has both the job ids (for the run_metrics cleanup) and the paths
// (for the operator-facing report).
func scanIDPaths(rows pgx.Rows) (ids, paths []string, err error) {
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, nil, fmt.Errorf("scan id/path: %w", err)
		}
		ids = append(ids, id)
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rows error (id/paths): %w", err)
	}
	return ids, paths, nil
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
