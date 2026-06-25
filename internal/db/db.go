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

// ─── Deterministic chunk UUID ────────────────────────────────────────────────

// chunkUUIDNamespace is the fixed UUID namespace for deriving chunk IDs.
// It is UUIDv5(DNS, "earmark-chunk-v1") — a stable, well-known value that
// makes chunk IDs reproducible given the same (transcript_id, chunk_index)
// inputs. CONTRACT §1.5: required when EVAL_GATES_EMBED=true so the eval pass
// and the embed pass agree on which UUID a given chunk will have.
var chunkUUIDNamespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("earmark-chunk-v1"))

// ChunkUUID derives a deterministic UUIDv5 for a chunk identified by its
// transcript ID and ordinal index within that transcript. Both passes (eval and
// embed) must call this function with the same inputs to produce the same ID.
// The format is "<transcript_id>/<chunk_index>".
func ChunkUUID(transcriptID string, chunkIndex int) string {
	key := fmt.Sprintf("%s/%d", transcriptID, chunkIndex)
	return uuid.NewSHA1(chunkUUIDNamespace, []byte(key)).String()
}

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
		--   phase     — batched two-phase pipeline selector (CONTRACT §1.4). NULL or
		--               'idle' = normal (both ASR runner and embed worker run freely,
		--               today's behavior); 'transcribe' = ASR-only phase (embed worker
		--               idles); 'analyze' = embed-only phase (ASR paused). A future
		--               coordinator flips this; default NULL keeps backward compat.
		-- Gate: claim iff (NOT paused) AND (run_limit IS NULL OR run_limit > 0).
		CREATE TABLE IF NOT EXISTS runner_control (
			id         INTEGER     NOT NULL PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			paused     BOOLEAN     NOT NULL DEFAULT false,
			run_limit  INTEGER         CHECK (run_limit IS NULL OR run_limit >= 0),
			phase      TEXT            CHECK (phase IS NULL OR phase IN ('idle','transcribe','analyze')),
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

		-- transcript_findings: read-only LLM-as-judge output (CONTRACT §2.15).
		-- Each row is an ADVISORY suspected transcription error recorded by the
		-- eval layer (internal/eval). The eval layer is strictly read-then-insert:
		-- it READS transcripts/segments/transcript_chunks and INSERTs here; it
		-- NEVER updates/deletes/alters the transcript tables, and this table has
		-- no FK that could cascade a mutation back into them (the immutability
		-- asymmetry — a wrong flag is harmless, a wrong correction corrupts the
		-- corpus, so corrections are never applied). suggested_correction is
		-- informational only. transcription_run_id ties a finding to the job/run
		-- (hence the ASR backend) that produced the transcript, so the same judge
		-- over different backends yields a comparative quality metric (§2.15).
		CREATE TABLE IF NOT EXISTS transcript_findings (
			id                    UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
			transcript_id         UUID        NOT NULL,
			file_path             TEXT        NOT NULL,
			chunk_id              UUID,
			chunk_index           INTEGER,
			start_sec             FLOAT8      NOT NULL,
			end_sec               FLOAT8      NOT NULL,
			original_text         TEXT        NOT NULL,
			issue_type            TEXT        NOT NULL,
			suggested_correction  TEXT,
			confidence            FLOAT8      NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
			model                 TEXT        NOT NULL,
			transcription_run_id  UUID,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE INDEX IF NOT EXISTS transcript_findings_file_path_idx
			ON transcript_findings (file_path);
		CREATE INDEX IF NOT EXISTS transcript_findings_transcript_id_idx
			ON transcript_findings (transcript_id);
		CREATE INDEX IF NOT EXISTS transcript_findings_run_id_idx
			ON transcript_findings (transcription_run_id);
		CREATE INDEX IF NOT EXISTS transcript_findings_issue_type_idx
			ON transcript_findings (issue_type);

		-- pipeline_events: append-only audit log of pipeline stage transitions
		-- (CONTRACT §1.7). Every Go-observable stage boundary (enqueue, embed
		-- start/finish, eval start/finish, fail/requeue, runner_availability,
		-- heartbeat-derived) appends one immutable row. job_id is nullable so
		-- runner_availability/heartbeat events (not tied to a job) can be recorded;
		-- file_path is denormalized so a timeline survives a requeue mutating the
		-- job row. Append-only by convention (no UPDATE/DELETE except the retention
		-- prune of high-frequency heartbeat/availability rows and the ON DELETE
		-- CASCADE that ties a job's history to the job). Writes are best-effort — a
		-- failed insert logs and continues; it NEVER fails the pipeline stage.
		CREATE TABLE IF NOT EXISTS pipeline_events (
			id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			job_id         UUID        REFERENCES transcription_jobs(id) ON DELETE CASCADE,
			file_path      TEXT,
			stage          TEXT        NOT NULL CHECK (stage IN
			                 ('discover','enqueue','claim','transcribe','chunk','embed','eval',
			                  'done','fail','requeue','heartbeat','runner_availability')),
			event          TEXT        NOT NULL CHECK (event IN
			                 ('start','finish','error','skip','retry','state')),
			runner_host    TEXT,
			model          TEXT,
			model_version  TEXT,
			duration_ms    BIGINT,
			item_count     INT,
			token_count    BIGINT,
			attempt        INT,
			reason         TEXT,
			detail         JSONB,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE INDEX IF NOT EXISTS pipeline_events_job_id_idx  ON pipeline_events (job_id, created_at);
		CREATE INDEX IF NOT EXISTS pipeline_events_stage_idx   ON pipeline_events (stage, event, created_at);
		CREATE INDEX IF NOT EXISTS pipeline_events_created_idx ON pipeline_events (created_at);
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

	// phase migration (CONTRACT §1.4): add the nullable phase column + its CHECK to
	// an existing runner_control table. ADD COLUMN IF NOT EXISTS is a no-op when
	// present; the CHECK is guarded by pg_constraint and swallows the duplicate-on-
	// race SQLSTATEs (same pattern as run_limit above). Default NULL keeps the
	// existing single-phase behavior (both runner and embed worker run freely).
	if _, err := tx.Exec(ctx, `
		ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS phase TEXT;
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint WHERE conname = 'runner_control_phase_valid'
			) THEN
				ALTER TABLE runner_control
					ADD CONSTRAINT runner_control_phase_valid
					CHECK (phase IS NULL OR phase IN ('idle','transcribe','analyze'));
			END IF;
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$;
	`); err != nil {
		return fmt.Errorf("phase migration: %w", err)
	}

	// runner_heartbeat_at migration (CONTRACT §1.7): a liveness timestamp the
	// runner stamps EVERY poll cycle — working, idle, or paused — so "alive but
	// idle" is distinguishable from "down" (the per-job updated_at heartbeat goes
	// quiet the moment the queue drains). Default NULL until the first stamp; the
	// earmark_runner_alive_seconds metric is simply absent until then.
	if _, err := tx.Exec(ctx, `
		ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS runner_heartbeat_at TIMESTAMPTZ;
	`); err != nil {
		return fmt.Errorf("runner_heartbeat_at migration: %w", err)
	}

	// Runner self-update migration (CONTRACT §1.4 + §2.12): version-skew detection
	// + the update state machine. runner_version is the tag the runner reports it
	// is RUNNING (stamped on the heartbeat); desired_runner_version is the tag the
	// dashboard button asks it to run; the runner owns the
	// requested→updating→success/failed transitions. All NULL until first use.
	if _, err := tx.Exec(ctx, `
		ALTER TABLE runner_control
			ADD COLUMN IF NOT EXISTS runner_version         TEXT,
			ADD COLUMN IF NOT EXISTS desired_runner_version TEXT,
			ADD COLUMN IF NOT EXISTS runner_update_state    TEXT,
			ADD COLUMN IF NOT EXISTS runner_update_error    TEXT,
			ADD COLUMN IF NOT EXISTS runner_update_at       TIMESTAMPTZ;
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint WHERE conname = 'runner_control_update_state_valid'
			) THEN
				ALTER TABLE runner_control
					ADD CONSTRAINT runner_control_update_state_valid
					CHECK (runner_update_state IS NULL OR runner_update_state IN
						('idle','requested','updating','success','failed'));
			END IF;
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$;
	`); err != nil {
		return fmt.Errorf("runner self-update migration: %w", err)
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

	// Eval-slice migration (CONTRACT §1.5): add the six eval_* columns to an
	// existing run_metrics table — the LLM-judge's slice (a fourth column-selective
	// writer, UpsertEvalMetrics). All additive + nullable, so a deployment that
	// never runs eval keeps every column NULL. eval_finished_at IS NOT NULL is the
	// per-job eval-completion marker (the first such marker — none existed before).
	// ADD COLUMN IF NOT EXISTS is a no-op when already present, safe on every boot.
	if _, err := tx.Exec(ctx, `
		ALTER TABLE run_metrics
			ADD COLUMN IF NOT EXISTS eval_started_at  TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS eval_finished_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS eval_model       TEXT,
			ADD COLUMN IF NOT EXISTS eval_chunks      INT,
			ADD COLUMN IF NOT EXISTS eval_skipped     INT,
			ADD COLUMN IF NOT EXISTS eval_findings    INT;
	`); err != nil {
		return fmt.Errorf("run_metrics eval-slice migration: %w", err)
	}

	// completed_at + trigger (CONTRACT §1.1): stamp completed_at = now() whenever a
	// transcription_jobs row transitions INTO status='done'. The runner owns the
	// mark-done UPDATE (the Go side never marks jobs done), so a trigger is the
	// only Go-only way to record completion time. Old 'done' rows keep NULL (no
	// backfill — there is no historical completion time to recover). DoneLastHour
	// uses COALESCE(completed_at, updated_at) so it stays correct on old rows.
	if _, err := tx.Exec(ctx, `
		ALTER TABLE transcription_jobs ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

		CREATE OR REPLACE FUNCTION transcription_jobs_set_completed_at()
		RETURNS TRIGGER LANGUAGE plpgsql AS $$
		BEGIN
			-- Stamp only on the transition INTO 'done' (so a heartbeat UPDATE on an
			-- already-done row, or a requeue out of 'done', never re-stamps it).
			IF NEW.status = 'done' AND (OLD.status IS DISTINCT FROM 'done') THEN
				NEW.completed_at = now();
			-- Leaving 'done' (e.g. operator requeue back to 'pending') clears it so
			-- the column always reflects the current run's completion, not a stale one.
			ELSIF NEW.status <> 'done' AND OLD.status = 'done' THEN
				NEW.completed_at = NULL;
			END IF;
			RETURN NEW;
		END;
		$$;

		DROP TRIGGER IF EXISTS transcription_jobs_completed_at ON transcription_jobs;
		CREATE TRIGGER transcription_jobs_completed_at
			BEFORE UPDATE ON transcription_jobs
			FOR EACH ROW EXECUTE FUNCTION transcription_jobs_set_completed_at();
	`); err != nil {
		return fmt.Errorf("completed_at migration: %w", err)
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
// This is the ungated embed-eligible selection (EVAL_GATES_EMBED=false, default).
// When the gate is enabled the worker uses GetUnevaluatedTranscripts (eval pass)
// and GetEvaluatedUnembeddedTranscripts (embed pass) instead.
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
	return scanTranscriptRows(rows)
}

// GetUnevaluatedTranscripts returns done transcripts that have not been eval'd
// (run_metrics.eval_finished_at IS NULL or no run_metrics row) AND have no
// chunks yet (not yet embedded). This is the eval-pass selection for the
// EVAL_GATES_EMBED two-pass gated flow (CONTRACT §2.4, §1.5).
//
// The eval pass judges these transcripts and writes eval_finished_at so the
// embed pass can pick them up. We restrict to not-embedded so we don't
// re-judge transcripts that are already embedded (they are handled by
// earmark eval --backfill-unevaluated).
func (db *DB) GetUnevaluatedTranscripts(ctx context.Context) ([]*Transcript, error) {
	return db.getUnevaluatedTranscripts(ctx, db.pool)
}

// unevaluatedTranscriptsSQL is the eval-pass selection: done jobs, NOT embedded
// (no transcript_chunks), NOT eval'd (no run_metrics row with eval_finished_at).
// Exported as a package var so the execution-level test can match it exactly.
const unevaluatedTranscriptsSQL = `
		SELECT t.id, t.job_id, t.file_path, t.checksum,
		       t.language, t.duration_seconds, t.speaker_count,
		       t.segments, t.raw_text, t.model_name, t.created_at
		FROM transcripts t
		JOIN transcription_jobs j ON j.id = t.job_id
		WHERE j.status = 'done'
		  AND NOT EXISTS (
		    SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM run_metrics rm
		    WHERE rm.job_id = j.id AND rm.eval_finished_at IS NOT NULL
		  )
		ORDER BY t.created_at ASC
	`

func (db *DB) getUnevaluatedTranscripts(ctx context.Context, q rowQuerier) ([]*Transcript, error) {
	rows, err := q.Query(ctx, unevaluatedTranscriptsSQL)
	if err != nil {
		return nil, fmt.Errorf("query unevaluated transcripts: %w", err)
	}
	return scanTranscriptRows(rows) // scanTranscriptRows closes rows
}

// GetEvaluatedUnembeddedTranscripts returns done transcripts that have been
// eval'd (run_metrics.eval_finished_at IS NOT NULL) AND have no chunks yet
// (not yet embedded). This is the embed-pass selection for the EVAL_GATES_EMBED
// two-pass gated flow (CONTRACT §2.4, §1.5).
//
// A transcript appearing here is guaranteed to have been judged; the caller
// embeds it and it becomes searchable. eval_finished_at IS NOT NULL is the
// hand-off latch that separates the eval pass from the embed pass.
func (db *DB) GetEvaluatedUnembeddedTranscripts(ctx context.Context) ([]*Transcript, error) {
	return db.getEvaluatedUnembeddedTranscripts(ctx, db.pool)
}

// evaluatedUnembeddedTranscriptsSQL is the embed-pass selection: done jobs that
// ARE eval'd (run_metrics.eval_finished_at IS NOT NULL) and NOT embedded (no
// transcript_chunks). Exported as a package const so the execution-level test
// can match it exactly.
const evaluatedUnembeddedTranscriptsSQL = `
		SELECT t.id, t.job_id, t.file_path, t.checksum,
		       t.language, t.duration_seconds, t.speaker_count,
		       t.segments, t.raw_text, t.model_name, t.created_at
		FROM transcripts t
		JOIN transcription_jobs j ON j.id = t.job_id
		JOIN run_metrics rm ON rm.job_id = j.id
		WHERE j.status = 'done'
		  AND rm.eval_finished_at IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id
		  )
		ORDER BY t.created_at ASC
	`

func (db *DB) getEvaluatedUnembeddedTranscripts(ctx context.Context, q rowQuerier) ([]*Transcript, error) {
	rows, err := q.Query(ctx, evaluatedUnembeddedTranscriptsSQL)
	if err != nil {
		return nil, fmt.Errorf("query evaluated unembedded transcripts: %w", err)
	}
	return scanTranscriptRows(rows) // scanTranscriptRows closes rows
}

// scanTranscriptRows scans a pgx.Rows result into []*Transcript. Shared by
// GetCompletedTranscripts, GetUnevaluatedTranscripts, and
// GetEvaluatedUnembeddedTranscripts to keep scanning logic DRY.
func scanTranscriptRows(rows pgx.Rows) ([]*Transcript, error) {
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
		return nil, fmt.Errorf("rows error (transcripts): %w", err)
	}
	return results, nil
}

// RecoverStaleJobs resets jobs stuck in "claimed" state longer than the
// configured stale timeout. Jobs that have reached max attempts are marked
// failed. (CONTRACT §1.3 — stale-claim recovery.) Returns the number of jobs
// newly marked 'failed' (for the best-effort failed-jobs metric).
func (db *DB) RecoverStaleJobs(ctx context.Context, timeout time.Duration) (int, error) {
	// Use an integer-seconds interval to avoid PostgreSQL misreading Go duration
	// strings (e.g. "30m0s" where bare 'm' means months in Postgres).
	secs := int(timeout.Seconds())

	// Reset below-max-attempts jobs to pending. RETURNING the affected rows lets us
	// emit best-effort requeue audit events (CONTRACT §1.7) without a second query.
	reset, err := db.pool.Query(ctx, `
		UPDATE transcription_jobs
		SET    status     = 'pending',
		       claimed_by = NULL,
		       claimed_at = NULL
		WHERE  status     = 'claimed'
		  AND  updated_at < now() - ($1 * interval '1 second')
		  AND  attempts   < 3
		RETURNING id, file_path, attempts
	`, secs)
	if err != nil {
		return 0, fmt.Errorf("reset stale jobs: %w", err)
	}
	requeued, rerr := scanStaleRows(reset)
	if rerr != nil {
		return 0, fmt.Errorf("scan reset stale jobs: %w", rerr)
	}

	// Mark max-attempts jobs failed.
	failed, err := db.pool.Query(ctx, `
		UPDATE transcription_jobs
		SET    status = 'failed',
		       error  = 'max attempts reached'
		WHERE  status     = 'claimed'
		  AND  updated_at < now() - ($1 * interval '1 second')
		  AND  attempts   >= 3
		RETURNING id, file_path, attempts
	`, secs)
	if err != nil {
		return 0, fmt.Errorf("fail max-attempts jobs: %w", err)
	}
	failedRows, ferr := scanStaleRows(failed)
	if ferr != nil {
		return 0, fmt.Errorf("scan failed stale jobs: %w", ferr)
	}

	// Emit audit events for each recovered job (CONTRACT §1.7). Best-effort: a
	// stale-recovery row that became 'pending' is a requeue; one that hit the
	// attempt cap is a discarded failure. The reason+attempt distinguish them.
	for _, r := range requeued {
		db.emitEvent(ctx, PipelineEvent{
			JobID: r.id, FilePath: r.filePath, Stage: StageRequeue, Event: EventRetry,
			RunnerHost: HostGoWorker, Attempt: IntPtr(r.attempts),
			Reason: "stale claim recovered (heartbeat expired); retryable",
		})
	}
	for _, r := range failedRows {
		db.emitEvent(ctx, PipelineEvent{
			JobID: r.id, FilePath: r.filePath, Stage: StageFail, Event: EventError,
			RunnerHost: HostGoWorker, Attempt: IntPtr(r.attempts),
			Reason: "max attempts reached after stale-claim recovery; discarded",
		})
	}
	return len(failedRows), nil
}

// staleRow is a recovered job row from RecoverStaleJobs' RETURNING clauses.
type staleRow struct {
	id       string
	filePath string
	attempts int
}

// scanStaleRows drains a RETURNING(id, file_path, attempts) result set.
func scanStaleRows(rows pgx.Rows) ([]staleRow, error) {
	defer rows.Close()
	var out []staleRow
	for rows.Next() {
		var r staleRow
		if err := rows.Scan(&r.id, &r.filePath, &r.attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// emitEvent appends a pipeline event best-effort: a write failure is logged and
// swallowed (CONTRACT §1.7 — events never fail the pipeline operation).
func (db *DB) emitEvent(ctx context.Context, e PipelineEvent) {
	if err := db.AppendEvent(ctx, e); err != nil {
		db.log.Warn("pipeline event write failed (continuing)",
			"stage", e.Stage, "event", e.Event, "job_id", e.JobID, "error", err)
	}
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
		// id is supplied by the caller when chunks were pre-identified (e.g. the
		// worker generates UUIDs so in-pipeline eval findings can reference the
		// chunk before it is inserted). An empty id falls back to the column
		// default — COALESCE(NULLIF(...)) keeps both callers working.
		if _, err := tx.Exec(ctx, `
			INSERT INTO transcript_chunks
			       (id, transcript_id, file_path, chunk_index, start_sec, end_sec,
			        text, speaker, embedding)
			VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
			        $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (transcript_id, chunk_index) DO UPDATE
			SET text      = EXCLUDED.text,
			    embedding = EXCLUDED.embedding
		`, c.ID, c.TranscriptID, c.FilePath, c.ChunkIndex, c.StartSec, c.EndSec,
			c.Text, c.Speaker, pgvector.NewVector(c.Embedding),
		); err != nil {
			return fmt.Errorf("insert chunk %d: %w", c.ChunkIndex, err)
		}
	}
	return tx.Commit(ctx)
}

// EmbedDocuments delegates to the openai.Embeddings client's DOCUMENT path —
// passages stored/indexed in transcript_chunks.embedding. For nomic-embed-text
// each input is prefixed with "search_document: ". Never use this for a search
// query (use EmbedQuery) — the two embedding sides must diverge.
func (db *DB) EmbedDocuments(texts []string) ([][]float32, error) {
	return db.e.EmbedDocuments(texts)
}

// EmbedDocumentsWithUsage is EmbedDocuments plus the provider-reported token
// usage (which Ollama may leave zeroed).
func (db *DB) EmbedDocumentsWithUsage(texts []string) ([][]float32, openai.EmbeddingUsage, error) {
	return db.e.EmbedDocumentsWithUsage(texts)
}

// EmbedQuery delegates to the openai.Embeddings client's QUERY path — a single
// search query embedded for semantic search. For nomic-embed-text the query is
// prefixed with "search_query: ". Never use this to embed a stored passage (use
// EmbedDocuments).
func (db *DB) EmbedQuery(query string) ([]float32, error) {
	return db.e.EmbedQuery(query)
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

// EvalMetrics is the LLM-judge's slice of run_metrics columns (CONTRACT §1.5).
// It is the fourth column-selective writer (after the monitor, runner, and embed
// worker). FinishedAt is the per-job eval-completion marker: a job has been
// judged iff run_metrics.eval_finished_at IS NOT NULL.
type EvalMetrics struct {
	JobID      string
	StartedAt  time.Time
	FinishedAt time.Time
	Model      string // judge model id (chat client's Model())
	Chunks     int    // ChunksEvaluated
	Skipped    int    // ChunksSkipped (transient per-chunk judge errors)
	Findings   int    // FindingsFound
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

// upsertEvalMetricsSQL is the column-selective UPSERT for the eval slice of
// run_metrics. A package-level var (not a constant) so tests can assert its
// shape — that it touches ONLY the eval_* columns + updated_at, never clobbering
// the monitor's audio_bytes, the runner's transcription slice, or the embed
// worker's columns on the same job_id row.
//
// Parameter order: $1=job_id $2=eval_started_at $3=eval_finished_at $4=eval_model
//
//	$5=eval_chunks $6=eval_skipped $7=eval_findings
var upsertEvalMetricsSQL = `
	INSERT INTO run_metrics
	       (job_id, eval_started_at, eval_finished_at, eval_model,
	        eval_chunks, eval_skipped, eval_findings)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (job_id) DO UPDATE
	SET eval_started_at  = EXCLUDED.eval_started_at,
	    eval_finished_at = EXCLUDED.eval_finished_at,
	    eval_model       = EXCLUDED.eval_model,
	    eval_chunks      = EXCLUDED.eval_chunks,
	    eval_skipped     = EXCLUDED.eval_skipped,
	    eval_findings    = EXCLUDED.eval_findings,
	    updated_at       = now()
`

// UpsertEvalMetrics records eval timing, judge model, and chunk/skip/finding
// counts for a job (the eval layer's slice of run_metrics, CONTRACT §1.5). Only
// the eval_* columns are written, so it never clobbers the monitor's,  runner's,
// or embed worker's columns on the same row. eval_finished_at is the per-job
// eval-completion marker.
//
// Best-effort: callers should log-and-continue on error so a metrics write never
// fails the underlying eval/embed step.
func (db *DB) UpsertEvalMetrics(ctx context.Context, m EvalMetrics) error {
	_, err := db.pool.Exec(ctx, upsertEvalMetricsSQL,
		m.JobID, m.StartedAt, m.FinishedAt, m.Model,
		m.Chunks, m.Skipped, m.Findings)
	if err != nil {
		return fmt.Errorf("upsert eval metrics: %w", err)
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
	vec, err := db.e.EmbedQuery(query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return db.findSimilar(ctx, vec, limit, threshold)
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
	vec, err := db.e.EmbedQuery(query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return db.searchInBook(ctx, db.pool, vec, dir, limit, threshold)
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

	// EvalBacklog counts done transcripts that have not been eval'd
	// (run_metrics.eval_finished_at IS NULL) AND have no chunks yet. This is the
	// eval-pass backlog when EVAL_GATES_EMBED=true — Phase B is complete only
	// when BOTH EvalBacklog==0 AND EmbedBacklog==0. Always 0 when the gate is
	// disabled (ungated deployments ignore it). CONTRACT §1.4, §1.5.
	EvalBacklog int
	// LastEmbedAt is the newest transcript_chunks.created_at — i.e. when the
	// worker last finished embedding a transcript. Paired with EmbedBacklog it
	// distinguishes a genuine stall (backlog high AND no embed in a while) from
	// normal catch-up (backlog high but embeds still landing). nil when nothing
	// has been embedded yet.
	LastEmbedAt *time.Time
	// TotalJobs is Pending+Claimed+Done+Failed — the full backlog denominator.
	TotalJobs int
	// DoneLastHour is the number of jobs whose row entered 'done' in the last
	// hour, a throughput proxy. It counts on COALESCE(completed_at, updated_at):
	// completed_at is the exact completion time (a BEFORE UPDATE trigger stamps it
	// on the transition into 'done', CONTRACT §1.1); updated_at is the fallback for
	// rows that became 'done' before completed_at existed.
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
	LastHeartbeat *time.Time // updated_at of the most-recently-updated claimed job
	// LatestActivity is the newest of (most-recent claimed-job heartbeat,
	// most-recent completion). It is the broadest "the runner did something
	// recently" signal. nil only on a fresh install with no claims and no
	// completions.
	LatestActivity *time.Time
	// RunnerHeartbeatAt mirrors runner_control.runner_heartbeat_at — the liveness
	// stamp the runner writes on EVERY poll cycle (working, idle, or paused),
	// unlike LastHeartbeat/LatestActivity which only move while a job is claimed.
	// This is what distinguishes "alive but idle" from "down": a fresh value with
	// an empty queue is idle-done; a stale value is the runner actually gone. nil
	// until the runner has stamped at least once (column default NULL).
	RunnerHeartbeatAt *time.Time

	// Runner self-update (CONTRACT §1.4 + §2.12). RunnerVersion is the tag the
	// runner reports it is RUNNING (stamped on the heartbeat); DesiredRunnerVersion
	// is the tag the dashboard asked it to run; RunnerUpdateState is the state
	// machine token (idle|requested|updating|success|failed) and RunnerUpdateError
	// carries the failure detail. All nil/"" until the feature is exercised.
	RunnerVersion        *string
	DesiredRunnerVersion *string
	RunnerUpdateState    *string
	RunnerUpdateError    *string
	RunnerUpdateAt       *time.Time

	// EvalCoverageDone is the number of done jobs whose run_metrics row has a
	// non-NULL eval_finished_at (i.e. has been judged). Paired with Done it gives
	// the eval coverage ratio (CONTRACT §1.5). 0 when nothing is judged yet.
	EvalCoverageDone int

	// RunnerAvailable / RunnerAvailableKnown reflect the latest
	// runner_availability event (CONTRACT §1.7): Available is true when the GPU
	// host is free for transcription (not gaming). Known is false until any
	// runner_availability event has been recorded (so a metric is only emitted
	// when there is a real signal).
	RunnerAvailable      bool
	RunnerAvailableKnown bool

	// Per-run aggregates (run_metrics). AvgProcessingSeconds is the mean
	// transcription wall-clock over jobs the runner has timed; TotalEmbedTokens
	// is the summed authoritative local token count over all embedded runs. Both
	// are nil when no run_metrics rows carry the underlying data yet.
	AvgProcessingSeconds *float64
	TotalEmbedTokens     *int64

	// Library-wide totals over indexed content (the overview "library" card).
	// TotalDurationSeconds sums transcripts.duration_seconds across every
	// transcript; TotalWords sums run_metrics.word_count; BooksFullyDone counts
	// book directories whose every track job is 'done'; BooksTotal is the total
	// distinct book directories (the denominator for BooksFullyDone). The first
	// two are nil when nothing is transcribed/metered yet.
	//
	// A "book" is a directory of track jobs — job/track counts (Pending/Done/…)
	// are per audio file, so BooksTotal/BooksFullyDone are the book-level rollup.
	TotalDurationSeconds *float64
	TotalWords           *int64
	BooksFullyDone       int
	BooksTotal           int

	// PipelineBuckets is the per-track furthest pipeline stage breakdown used by
	// the segmented progress bar. It is populated by a single FILTER-aggregate
	// query over transcription_jobs at the end of GetServiceStatus; individual
	// Pending/Claimed/Done/Failed counts above are already read by then so no
	// duplicate round-trip occurs.
	//
	// Denominator for the bar = NotStarted + Transcribing + TranscribedOnly +
	// EvaldOnly + EmbeddedReady (i.e. all non-failed tracks). Failed is excluded
	// from the bar fill (it is a side-exit, not a pipeline stage).
	PipelineBuckets PipelineBuckets
}

// PipelineBuckets holds per-track furthest-stage counts for the segmented
// pipeline bar. Each track falls into exactly one bucket:
//
//   - NotStarted     — status IN ('pending','claimed') with no chunks/eval yet.
//     Pending and Transcribing are the pending+claimed split surfaced separately
//     so the bar can show "transcribing" (Claimed) distinctly from "waiting".
//   - TranscribedOnly — status='done', no eval completion, no chunks.
//   - EvaldOnly       — status='done', eval finished, no chunks (rare today).
//   - EmbeddedReady   — status='done', has chunks (the goal state).
//   - Failed          — status='failed' (kept for the API; off the bar fill).
//
// Pending and Claimed are NOT separate bucket fields — they duplicate the
// top-level QueueStats.Pending/Claimed (which the DB already read) and are
// derived from them at render time: NotStarted = Pending + Claimed.
type PipelineBuckets struct {
	// NotStarted = Pending + Claimed (split below for the "transcribing" segment).
	// These duplicate QueueStats.Pending / .Claimed so bar logic can work from
	// one struct; they are filled by the same query that fills the other buckets.
	Pending int // status='pending'
	Claimed int // status='claimed' (in-flight transcription)
	// done tracks by embedding+eval state
	TranscribedOnly int // done, no eval, no chunks
	EvaldOnly       int // done, eval finished, no chunks
	EmbeddedReady   int // done, has chunks (terminal)
	Failed          int // status='failed' (off the bar; kept for API parity)
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

	// Chunk count + last embed time (newest chunk written). MAX(created_at) is
	// NULL on an empty table, scanned into the nilable LastEmbedAt.
	if err := db.pool.QueryRow(ctx, `SELECT COUNT(*), MAX(created_at) FROM transcript_chunks`).Scan(&q.Chunks, &q.LastEmbedAt); err != nil {
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

	// Eval backlog: done transcripts not yet eval'd AND not yet embedded.
	// This is the eval-pass backlog when EVAL_GATES_EMBED=true (CONTRACT §1.4).
	// Under the gate, Phase B is complete when BOTH EvalBacklog==0 AND
	// EmbedBacklog==0. Always 0 when no gate is in use (ungated deployments
	// ignore it; the value is still computed for the dashboard and control API).
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transcripts t
		JOIN transcription_jobs j ON j.id = t.job_id
		WHERE j.status = 'done'
		  AND NOT EXISTS (
		    SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM run_metrics rm
		    WHERE rm.job_id = j.id AND rm.eval_finished_at IS NOT NULL
		  )
	`).Scan(&q.EvalBacklog); err != nil {
		return nil, fmt.Errorf("eval backlog count: %w", err)
	}

	// Backfill progress + throughput.
	q.TotalJobs = q.Pending + q.Claimed + q.Done + q.Failed
	// Throughput proxy: jobs completed in the last hour. Prefer the trigger-stamped
	// completed_at (CONTRACT §1.1); fall back to updated_at for rows that became
	// 'done' before the completed_at column existed (no backfill — COALESCE keeps
	// the count correct and back-compatible on those old rows).
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transcription_jobs
		WHERE status = 'done'
		  AND COALESCE(completed_at, updated_at) > now() - interval '1 hour'
	`).Scan(&q.DoneLastHour); err != nil {
		return nil, fmt.Errorf("done-last-hour count: %w", err)
	}

	// Global control row (runner_control singleton): pause flag + bounded-run
	// counter. Tolerate a missing row by defaulting to not-paused/unlimited; the
	// init seed normally guarantees it exists.
	if err := db.pool.QueryRow(ctx,
		`SELECT paused, run_limit, runner_heartbeat_at,
		        runner_version, desired_runner_version,
		        runner_update_state, runner_update_error, runner_update_at
		 FROM runner_control WHERE id = 1`,
	).Scan(&q.Paused, &q.RunLimit, &q.RunnerHeartbeatAt,
		&q.RunnerVersion, &q.DesiredRunnerVersion,
		&q.RunnerUpdateState, &q.RunnerUpdateError, &q.RunnerUpdateAt,
	); err != nil && !errors.Is(err, pgx.ErrNoRows) {
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

	// LatestActivity: the newest of (last claimed-job heartbeat, last completion).
	// This is the broadest "the runner did something recently" signal and is what
	// the runner-liveness metric derives from. NULL on a fresh install (no claims,
	// no completions). NOTE: the runner only stamps updated_at while a job is
	// CLAIMED — there is no idle heartbeat — so a large age here means
	// claim-activity is old, NOT necessarily that the runner is down (CONTRACT §1.7).
	if err := db.pool.QueryRow(ctx, `
		SELECT GREATEST(
		  (SELECT MAX(updated_at)  FROM transcription_jobs WHERE status = 'claimed'),
		  (SELECT MAX(COALESCE(completed_at, updated_at)) FROM transcription_jobs WHERE status = 'done')
		)
	`).Scan(&q.LatestActivity); err != nil {
		return nil, fmt.Errorf("latest activity query: %w", err)
	}

	// Eval coverage: done jobs whose run_metrics row has a non-NULL
	// eval_finished_at (the per-job eval-completion marker, CONTRACT §1.5).
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM transcription_jobs j
		JOIN run_metrics m ON m.job_id = j.id
		WHERE j.status = 'done' AND m.eval_finished_at IS NOT NULL
	`).Scan(&q.EvalCoverageDone); err != nil {
		return nil, fmt.Errorf("eval coverage query: %w", err)
	}

	// Latest runner availability (CONTRACT §1.7): the most-recent
	// runner_availability event's reason; available iff reason='idle'. Known is
	// false until any such event exists (so the metric is only emitted on a real
	// signal). Tolerate no-rows.
	var availReason *string
	if err := db.pool.QueryRow(ctx, `
		SELECT COALESCE(detail->>'reason', reason)
		FROM pipeline_events
		WHERE stage = 'runner_availability'
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(&availReason); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("runner availability query: %w", err)
	}
	if availReason != nil {
		q.RunnerAvailableKnown = true
		q.RunnerAvailable = *availReason == "idle"
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
		   ) fully_done),
		  (SELECT COUNT(DISTINCT regexp_replace(file_path, '/[^/]+$', ''))
		     FROM transcription_jobs)
	`).Scan(&q.TotalDurationSeconds, &q.TotalWords, &q.BooksFullyDone, &q.BooksTotal); err != nil {
		return nil, fmt.Errorf("library totals: %w", err)
	}

	// Pipeline stage buckets for the segmented bar. One FILTER-aggregate pass
	// over transcription_jobs; correlated EXISTS subqueries check eval and embed
	// completion without a JOIN (eval: run_metrics.eval_finished_at IS NOT NULL;
	// embed: any transcript_chunks row for the job via transcripts.job_id).
	// has_chunks is the terminal "embedded" signal; has_eval is advisory.
	// Pending and Claimed duplicate q.Pending/q.Claimed but are re-read here so
	// PipelineBuckets is self-contained for callers that use it independently.
	if err := db.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE status = 'pending')  AS pending,
		  COUNT(*) FILTER (WHERE status = 'claimed')  AS claimed,
		  COUNT(*) FILTER (WHERE status = 'failed')   AS failed,
		  COUNT(*) FILTER (WHERE status = 'done'
		    AND NOT EXISTS (
		      SELECT 1 FROM run_metrics rm
		      WHERE rm.job_id = j.id AND rm.eval_finished_at IS NOT NULL
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM transcripts tr
		      JOIN transcript_chunks tc ON tc.transcript_id = tr.id
		      WHERE tr.job_id = j.id LIMIT 1
		    )
		  ) AS transcribed_only,
		  COUNT(*) FILTER (WHERE status = 'done'
		    AND EXISTS (
		      SELECT 1 FROM run_metrics rm
		      WHERE rm.job_id = j.id AND rm.eval_finished_at IS NOT NULL
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM transcripts tr
		      JOIN transcript_chunks tc ON tc.transcript_id = tr.id
		      WHERE tr.job_id = j.id LIMIT 1
		    )
		  ) AS evald_only,
		  COUNT(*) FILTER (WHERE status = 'done'
		    AND EXISTS (
		      SELECT 1 FROM transcripts tr
		      JOIN transcript_chunks tc ON tc.transcript_id = tr.id
		      WHERE tr.job_id = j.id LIMIT 1
		    )
		  ) AS embedded_ready
		FROM transcription_jobs j
	`).Scan(
		&q.PipelineBuckets.Pending,
		&q.PipelineBuckets.Claimed,
		&q.PipelineBuckets.Failed,
		&q.PipelineBuckets.TranscribedOnly,
		&q.PipelineBuckets.EvaldOnly,
		&q.PipelineBuckets.EmbeddedReady,
	); err != nil {
		return nil, fmt.Errorf("pipeline bucket counts: %w", err)
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

// SetDesiredRunnerVersion records the operator's update intent (CONTRACT §2.12):
// the dashboard/control-API writes the target tag and flips the state machine to
// 'requested'. The runner reads desired_runner_version each cycle and, when it
// differs from the version it is running, self-updates (CONTRACT §1.4). by
// records who requested it. Upserts the singleton so it works even if the seed
// insert was skipped.
func (db *DB) SetDesiredRunnerVersion(ctx context.Context, version, by string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO runner_control (id, desired_runner_version, runner_update_state, runner_update_error, runner_update_at, updated_at, updated_by)
		VALUES (1, $1, 'requested', NULL, now(), now(), $2)
		ON CONFLICT (id) DO UPDATE
			SET desired_runner_version = EXCLUDED.desired_runner_version,
			    runner_update_state = 'requested',
			    runner_update_error = NULL,
			    runner_update_at = now(),
			    updated_at = now(),
			    updated_by = EXCLUDED.updated_by
	`, version, by)
	if err != nil {
		return fmt.Errorf("set desired runner version: %w", err)
	}
	return nil
}

// ClearRunnerUpdate cancels/acknowledges an update request: clears the desired
// version and resets the state machine to 'idle'. Used by the dashboard's
// clear/abort affordance after a success or to dismiss a failure.
func (db *DB) ClearRunnerUpdate(ctx context.Context, by string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE runner_control
		SET desired_runner_version = NULL,
		    runner_update_state = 'idle',
		    runner_update_error = NULL,
		    runner_update_at = now(),
		    updated_at = now(),
		    updated_by = $1
		WHERE id = 1
	`, by)
	if err != nil {
		return fmt.Errorf("clear runner update: %w", err)
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

// PipelinePhase values gate the batched two-phase pipeline (CONTRACT §1.4).
// PhaseIdle is the default (both ASR runner and embed worker run freely);
// PhaseTranscribe is the ASR-only phase (embed worker idles); PhaseAnalyze is
// the embed-only phase (ASR runner paused/off-GPU).
const (
	PhaseIdle       = "idle"
	PhaseTranscribe = "transcribe"
	PhaseAnalyze    = "analyze"
)

// validPhases is the closed set of explicit phase names SetPipelinePhase
// validates against. NOTE: the empty string "" is intentionally NOT a member,
// yet SetPipelinePhase still accepts it — it special-cases "" *before* the map
// lookup as an alias for "idle" (both store NULL). So the set of values
// SetPipelinePhase accepts is {"", "idle", "transcribe", "analyze"}; this map
// only covers the three non-empty ones. Don't read the map's omission of "" as
// "" being rejected.
var validPhases = map[string]bool{
	PhaseIdle:       true,
	PhaseTranscribe: true,
	PhaseAnalyze:    true,
}

// GetPipelinePhase returns the pipeline phase from runner_control, normalizing a
// NULL column (the default) to PhaseIdle. A missing row (the Go service not yet
// initialized) is likewise treated as PhaseIdle so callers degrade to today's
// run-both behavior. Returns one of "idle", "transcribe", or "analyze".
func (db *DB) GetPipelinePhase(ctx context.Context) (string, error) {
	var phase *string
	err := db.pool.QueryRow(ctx, `SELECT phase FROM runner_control WHERE id = 1`).Scan(&phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return PhaseIdle, nil
	}
	if err != nil {
		return PhaseIdle, fmt.Errorf("get pipeline phase: %w", err)
	}
	if phase == nil || *phase == "" {
		return PhaseIdle, nil
	}
	return *phase, nil
}

// SetPipelinePhase writes the pipeline phase, validating it against the closed
// set {"idle","transcribe","analyze"}. The empty string and "idle" both store
// NULL (normal operation, the default). by records who set it for the audit
// column. This is an UPDATE of the phase column only — it touches neither
// paused nor run_limit (the three are independent axes; CONTRACT §1.4) — so it
// can never clobber pause/run-budget intent. The singleton row is seeded at
// init (`initialize`), so the UPDATE always matches; a missing row (which would
// only happen pre-init) is a no-op, consistent with GetPipelinePhase treating
// it as idle.
func (db *DB) SetPipelinePhase(ctx context.Context, phase, by string) error {
	if phase != "" && !validPhases[phase] {
		return fmt.Errorf("invalid pipeline phase %q (want one of idle, transcribe, analyze)", phase)
	}
	// "idle"/"" → NULL so the default and the explicit idle phase are stored
	// identically (both mean "run both freely").
	var arg *string
	if phase != "" && phase != PhaseIdle {
		arg = &phase
	}
	_, err := db.pool.Exec(ctx, `
		UPDATE runner_control
		SET phase = $1,
		    updated_at = now(),
		    updated_by = $2
		WHERE id = 1
	`, arg, by)
	if err != nil {
		return fmt.Errorf("set pipeline phase: %w", err)
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
	// Status filters to books that have at least one track in the given status.
	// Valid values: "", "pending", "claimed", "done", "failed", "queued".
	// "queued" matches books with at least one pending OR claimed track (remaining work).
	Status string
	Query  string // case-insensitive substring match on file_path (author/title/track)
	Limit  int    // page size (defaulted if ≤ 0)
	Offset int    // page offset
	// Sort controls the ORDER BY: "" or "default" uses the standard transcribed-first
	// order (done-ratio desc, done count desc, last_updated desc, book_dir).
	// "activity" orders by most-recently-updated first (last_updated DESC, book_dir)
	// so the activity feed surfaces the books that changed most recently.
	// "queue" orders by active-first: (claimed>0) DESC, claimed DESC, pending DESC,
	// last_updated ASC, book_dir — putting actively-transcribing books at the top.
	Sort string
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
	case "queued":
		// Books with remaining work: at least one track that is pending or claimed.
		statusHaving = "HAVING COUNT(*) FILTER (WHERE j.status IN ('pending','claimed')) > 0"
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
	//
	// Sort is validated against an allow-list and mapped to a fixed ORDER BY
	// literal — same validate-then-map pattern as statusHaving above, so no
	// caller-supplied value is ever interpolated into the SQL. "activity" orders
	// most-recently-updated first (the pipeline activity feed); the default
	// keeps the library's transcribed-first order. An unknown value fails loudly
	// rather than silently defaulting.
	var orderBy string
	switch f.Sort {
	case "", "default":
		orderBy = `ORDER BY (done::float8 / NULLIF(total, 0)) DESC NULLS LAST,
		         done DESC,
		         last_updated DESC,
		         book_dir`
	case "activity":
		orderBy = `ORDER BY last_updated DESC, book_dir`
	case "queue":
		// Active books (claimed > 0) first, then most-claimed, then most-pending,
		// then longest-waiting (oldest last_updated first), then stable book_dir.
		// This puts actively-transcribing books at the top of the queue view.
		orderBy = `ORDER BY (claimed > 0) DESC, claimed DESC, pending DESC, last_updated ASC, book_dir`
	default:
		return nil, 0, fmt.Errorf("invalid sort filter: %q", f.Sort)
	}
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
		%s
		LIMIT $2 OFFSET $3
	`, statusHaving, orderBy)

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

// ─── Eval layer (read-only LLM judge, CONTRACT §2.15) ────────────────────────
//
// The methods in this section are the eval layer's ONLY contact with the
// database. They fall into exactly two kinds:
//   - READ a chunk (with its transcript + run ids) to feed the judge.
//   - INSERT a finding row.
// There is intentionally NO UpdateFinding/DeleteFinding and NO method here that
// writes to transcripts/segments/transcript_chunks — the read-only contract is
// enforced structurally by the absence of such methods, not just by convention.

// EvalChunk is one unit of work for the judge: a chunk's text plus the
// addressing it needs to attribute a finding (transcript, run, ordinal, span).
type EvalChunk struct {
	ChunkID            string
	TranscriptID       string
	TranscriptionRunID string // transcription_jobs.id — the run/backend that produced the transcript
	FilePath           string
	ChunkIndex         int
	StartSec           float64
	EndSec             float64
	Text               string
}

// evalChunkSelectSQL is the shared SELECT/JOIN shape used to pull eval chunks.
// It joins each chunk back to its transcript's originating job so a finding can
// carry transcription_run_id (per-backend attribution). It is a package var so
// tests can assert it never contains a write verb against the transcript tables.
var evalChunkSelectSQL = `
	SELECT c.id, c.transcript_id, t.job_id, c.file_path,
	       c.chunk_index, c.start_sec, c.end_sec, c.text
	FROM transcript_chunks c
	JOIN transcripts t ON t.id = c.transcript_id
`

// GetUnevaluatedJobTranscripts returns done transcripts whose run_metrics row
// has eval_finished_at IS NULL (or has no run_metrics row at all), regardless of
// whether the transcript has already been embedded. This is the backfill
// selection for `earmark eval --backfill-unevaluated` (CONTRACT §2.15, §1.5):
// it judges any done transcript that slipped through before EVAL_GATES_EMBED was
// enabled, writing eval_finished_at so they are retroactively covered.
//
// Unlike GetUnevaluatedTranscripts (which restricts to not-yet-embedded), this
// method returns ALL done+not-eval'd transcripts, including already-embedded
// ones, because the backfill command runs against live data and must not miss
// anything. The caller (cmd/eval --backfill-unevaluated) persists findings and
// eval_finished_at via the existing eval + UpsertEvalMetrics path.
func (db *DB) GetUnevaluatedJobTranscripts(ctx context.Context) ([]*Transcript, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.job_id, t.file_path, t.checksum,
		       t.language, t.duration_seconds, t.speaker_count,
		       t.segments, t.raw_text, t.model_name, t.created_at
		FROM transcripts t
		JOIN transcription_jobs j ON j.id = t.job_id
		WHERE j.status = 'done'
		  AND NOT EXISTS (
		    SELECT 1 FROM run_metrics rm
		    WHERE rm.job_id = j.id AND rm.eval_finished_at IS NOT NULL
		  )
		ORDER BY t.created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query unevaluated job transcripts: %w", err)
	}
	return scanTranscriptRows(rows) // scanTranscriptRows closes rows
}

// GetEvalChunksForBook returns the chunks whose file_path contains substr
// (case-insensitive), in path/chunk order, capped at limit (≤0 → a sane
// default). Read-only. The substring match mirrors `requeue` ergonomics so the
// operator can pass a title fragment (e.g. "Project Hail Mary").
func (db *DB) GetEvalChunksForBook(ctx context.Context, substr string, limit int) ([]EvalChunk, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.pool.Query(ctx, evalChunkSelectSQL+`
		WHERE c.file_path ILIKE $1
		ORDER BY c.file_path, c.chunk_index
		LIMIT $2
	`, likePattern(substr), limit)
	if err != nil {
		return nil, fmt.Errorf("eval chunks for book query: %w", err)
	}
	defer rows.Close()
	return scanEvalChunks(rows)
}

// SampleEvalChunks returns up to limit randomly-sampled chunks across the whole
// library (read-only). Used by `earmark eval --sample N` to bound cost to N
// judge calls regardless of library size while staying representative.
func (db *DB) SampleEvalChunks(ctx context.Context, limit int) ([]EvalChunk, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.pool.Query(ctx, evalChunkSelectSQL+`
		ORDER BY random()
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("sample eval chunks query: %w", err)
	}
	defer rows.Close()
	return scanEvalChunks(rows)
}

func scanEvalChunks(rows pgx.Rows) ([]EvalChunk, error) {
	var out []EvalChunk
	for rows.Next() {
		var c EvalChunk
		if err := rows.Scan(&c.ChunkID, &c.TranscriptID, &c.TranscriptionRunID,
			&c.FilePath, &c.ChunkIndex, &c.StartSec, &c.EndSec, &c.Text); err != nil {
			return nil, fmt.Errorf("scan eval chunk: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (eval chunks): %w", err)
	}
	return out, nil
}

// Finding is one advisory suspected-error row to INSERT into transcript_findings.
// suggested_correction is informational only and is never applied (§2.15).
type Finding struct {
	TranscriptID        string
	FilePath            string
	ChunkID             *string
	ChunkIndex          *int
	StartSec            float64
	EndSec              float64
	OriginalText        string
	IssueType           string
	SuggestedCorrection *string
	Confidence          float64
	Model               string
	TranscriptionRunID  *string
}

// insertFindingSQL is the INSERT for one finding. Package var so a test can
// assert it is an INSERT into transcript_findings and touches no other table —
// the read-only-over-transcripts guard at the SQL level.
var insertFindingSQL = `
	INSERT INTO transcript_findings
	       (transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
	        original_text, issue_type, suggested_correction, confidence, model,
	        transcription_run_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
`

// InsertFindings stores judge findings in one transaction. Insert-only: it never
// updates or deletes any row, and writes to no table other than
// transcript_findings. A nil/empty slice is a no-op.
func (db *DB) InsertFindings(ctx context.Context, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin findings tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for i, f := range findings {
		if _, err := tx.Exec(ctx, insertFindingSQL,
			f.TranscriptID, f.FilePath, f.ChunkID, f.ChunkIndex, f.StartSec, f.EndSec,
			f.OriginalText, f.IssueType, f.SuggestedCorrection, f.Confidence, f.Model,
			f.TranscriptionRunID,
		); err != nil {
			return fmt.Errorf("insert finding %d: %w", i, err)
		}
	}
	return tx.Commit(ctx)
}

// clearFindingsSQL deletes findings. Package var (like insertFindingSQL) so a
// test can assert it touches ONLY transcript_findings — the read-only-over-
// transcripts invariant (§2.15) holds: findings are advisory metadata, never
// the transcripts/segments/transcript_chunks themselves. An optional book-dir
// prefix scopes the delete; the unscoped form clears everything.
var (
	clearFindingsAllSQL = `DELETE FROM transcript_findings`
	clearFindingsDirSQL = `DELETE FROM transcript_findings WHERE file_path LIKE $1 || '/%' OR file_path = $1`
)

// ClearFindings deletes recorded findings and returns the number of rows
// removed. It writes to NO table other than transcript_findings — it never
// touches transcripts, segments, or transcript_chunks, so re-running eval
// simply regenerates the advisory rows. When dir is non-empty the delete is
// scoped to that book directory (prefix match); empty dir clears all findings.
func (db *DB) ClearFindings(ctx context.Context, dir string) (int64, error) {
	dir = strings.TrimRight(strings.TrimSpace(dir), "/")
	var (
		tag pgconn.CommandTag
		err error
	)
	if dir == "" {
		tag, err = db.pool.Exec(ctx, clearFindingsAllSQL)
	} else {
		tag, err = db.pool.Exec(ctx, clearFindingsDirSQL, dir)
	}
	if err != nil {
		return 0, fmt.Errorf("clear findings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// FindingsSummary is the dashboard's per-book + library findings rollup.
type FindingsSummary struct {
	TotalFindings    int
	MeanConfidence   *float64 // nil when no findings yet
	HighConfidence   int      // confidence >= 0.8
	MediumConfidence int      // 0.4 <= confidence < 0.8
	LowConfidence    int      // confidence < 0.4
	ByIssueType      []IssueTypeCount
	ByBook           []BookFindings
}

// IssueTypeCount is one issue-type tally for the library-wide breakdown.
type IssueTypeCount struct {
	IssueType string
	Count     int
}

// BookFindings is one book's findings rollup for the dashboard's per-book table.
type BookFindings struct {
	FilePath       string // a representative track path within the book
	BookDir        string
	Count          int
	MeanConfidence float64
	TopIssueType   string
}

// GetFindingsSummary returns the library-wide findings rollup (read-only) used by
// the dashboard /findings page: totals, confidence buckets, an issue-type tally,
// and a per-book breakdown. Empty (not nil-erroring) on a fresh install.
func (db *DB) GetFindingsSummary(ctx context.Context) (*FindingsSummary, error) {
	s := &FindingsSummary{}

	// Totals + confidence buckets in one pass. MeanConfidence is nil when there
	// are zero findings (AVG over no rows is NULL → nilable pointer).
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       AVG(confidence),
		       COUNT(*) FILTER (WHERE confidence >= 0.8),
		       COUNT(*) FILTER (WHERE confidence >= 0.4 AND confidence < 0.8),
		       COUNT(*) FILTER (WHERE confidence < 0.4)
		FROM transcript_findings
	`).Scan(&s.TotalFindings, &s.MeanConfidence,
		&s.HighConfidence, &s.MediumConfidence, &s.LowConfidence); err != nil {
		return nil, fmt.Errorf("findings totals query: %w", err)
	}

	// Issue-type tally, most common first.
	typeRows, err := db.pool.Query(ctx, `
		SELECT issue_type, COUNT(*) AS n
		FROM transcript_findings
		GROUP BY issue_type
		ORDER BY n DESC, issue_type
	`)
	if err != nil {
		return nil, fmt.Errorf("findings issue-type query: %w", err)
	}
	defer typeRows.Close()
	for typeRows.Next() {
		var it IssueTypeCount
		if err := typeRows.Scan(&it.IssueType, &it.Count); err != nil {
			return nil, fmt.Errorf("scan issue-type count: %w", err)
		}
		s.ByIssueType = append(s.ByIssueType, it)
	}
	if err := typeRows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (issue-type): %w", err)
	}

	// Per-book rollup: group by book directory (dirname of file_path).
	bookRows, err := db.pool.Query(ctx, `
		WITH per_book AS (
			SELECT regexp_replace(file_path, '/[^/]+$', '') AS book_dir,
			       MIN(file_path)        AS sample_path,
			       COUNT(*)              AS n,
			       AVG(confidence)       AS mean_conf,
			       mode() WITHIN GROUP (ORDER BY issue_type) AS top_issue
			FROM transcript_findings
			GROUP BY book_dir
		)
		SELECT book_dir, sample_path, n, mean_conf, top_issue
		FROM per_book
		ORDER BY n DESC, book_dir
	`)
	if err != nil {
		return nil, fmt.Errorf("findings per-book query: %w", err)
	}
	defer bookRows.Close()
	for bookRows.Next() {
		var b BookFindings
		if err := bookRows.Scan(&b.BookDir, &b.FilePath, &b.Count, &b.MeanConfidence, &b.TopIssueType); err != nil {
			return nil, fmt.Errorf("scan book findings: %w", err)
		}
		s.ByBook = append(s.ByBook, b)
	}
	if err := bookRows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (per-book findings): %w", err)
	}

	return s, nil
}

// FindingRow is one individual finding for the triage worklist (global + per-book).
// It is the row-level companion to FindingsSummary's roll-up: the /findings page
// and per-book Book section render these so the operator can read each suspected
// error (not just per-book counts) and jump to the book it belongs to. JobID is
// the transcription_jobs.id of the track this finding's file_path belongs to,
// carried now so a later PR can deep-link to the track reader without a schema or
// query change; it scans to nil when no job row matches (LEFT JOIN).
type FindingRow struct {
	ID                  string
	FilePath            string // track path
	BookDir             string // regexp_replace(file_path,'/[^/]+$','')
	JobID               *string
	ChunkIndex          *int
	StartSec            float64
	EndSec              float64
	OriginalText        string
	IssueType           string
	SuggestedCorrection *string
	Confidence          float64
}

// listFindingsSQL / listFindingsInBookSQL are the worklist queries, package-level
// vars so a test can assert their shape (SELECT-only, scoped vs global). The
// scoped form adds a single `file_path LIKE $2 ESCAPE '\'` clause; the prefix is
// built with likePrefix EXACTLY as textSearchInBookSQL and the scoped clear path
// do, so "show this book's findings" selects the identical set "search this book"
// and "clear this book's findings" do. The LEFT JOIN to transcription_jobs lets a
// finding still surface (with JobID nil) if its job row is gone.
var (
	listFindingsSQL = `
		SELECT tf.id, tf.file_path,
		       regexp_replace(tf.file_path, '/[^/]+$', '') AS book_dir,
		       j.id AS job_id,
		       tf.chunk_index, tf.start_sec, tf.end_sec, tf.original_text,
		       tf.issue_type, tf.suggested_correction, tf.confidence
		FROM transcript_findings tf
		LEFT JOIN transcription_jobs j ON j.file_path = tf.file_path
		ORDER BY tf.confidence DESC, tf.file_path, tf.start_sec
		LIMIT $1
	`
	listFindingsInBookSQL = `
		SELECT tf.id, tf.file_path,
		       regexp_replace(tf.file_path, '/[^/]+$', '') AS book_dir,
		       j.id AS job_id,
		       tf.chunk_index, tf.start_sec, tf.end_sec, tf.original_text,
		       tf.issue_type, tf.suggested_correction, tf.confidence
		FROM transcript_findings tf
		LEFT JOIN transcription_jobs j ON j.file_path = tf.file_path
		WHERE tf.file_path LIKE $2 ESCAPE '\'
		ORDER BY tf.confidence DESC, tf.file_path, tf.start_sec
		LIMIT $1
	`
)

// defaultFindingsListLimit caps a worklist query when the caller passes limit <= 0.
const defaultFindingsListLimit = 200

// ListFindings returns individual finding rows sorted by confidence DESC (highest
// first — the triage order). dir == "" returns the whole-library worklist; a
// non-empty dir scopes to one book (file_path under "<dir>/"). limit caps the rows
// (<= 0 → defaultFindingsListLimit). Read-only: it never touches any table other
// than transcript_findings (joined read-only to transcription_jobs for the JobID).
func (db *DB) ListFindings(ctx context.Context, dir string, limit int) ([]FindingRow, error) {
	return db.listFindings(ctx, db.pool, dir, limit)
}

// listFindings is the querier-parameterized core of ListFindings, split out so the
// query execution + scan path is testable against a mock pool (mirrors
// getBookTracks / textSearchInBook).
func (db *DB) listFindings(ctx context.Context, q rowQuerier, dir string, limit int) ([]FindingRow, error) {
	if limit <= 0 {
		limit = defaultFindingsListLimit
	}
	dir = strings.TrimRight(strings.TrimSpace(dir), "/")

	var rows pgx.Rows
	var err error
	if dir == "" {
		rows, err = q.Query(ctx, listFindingsSQL, limit)
	} else {
		prefix := likePrefix(dir) + "/%"
		rows, err = q.Query(ctx, listFindingsInBookSQL, limit, prefix)
	}
	if err != nil {
		return nil, fmt.Errorf("list findings query: %w", err)
	}
	defer rows.Close()

	var out []FindingRow
	for rows.Next() {
		var f FindingRow
		if err := rows.Scan(&f.ID, &f.FilePath, &f.BookDir, &f.JobID,
			&f.ChunkIndex, &f.StartSec, &f.EndSec, &f.OriginalText,
			&f.IssueType, &f.SuggestedCorrection, &f.Confidence,
		); err != nil {
			return nil, fmt.Errorf("scan finding row: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (list findings): %w", err)
	}
	return out, nil
}

// findingsCountByBookSQL is the whole-library findings-count aggregate, keyed by
// book directory exactly as GetFindingsSummary's per_book CTE — one GROUP BY over
// transcript_findings rather than one query per book row (the library list is
// paged, so a per-row count would be N+1). Package-level so a test can assert the
// SQL shape, mirroring listFindingsSQL.
var findingsCountByBookSQL = `
	SELECT regexp_replace(file_path, '/[^/]+$', '') AS book_dir, COUNT(*)
	FROM transcript_findings
	GROUP BY book_dir
`

// GetFindingsCountByBook returns the number of recorded findings keyed by book
// directory (dirname of file_path), for the ⚑ findings-count column on the
// library list. It runs ONE aggregate query for the whole library so the paged
// list can look up each row's count by its Dir without an N+1. Read-only: it
// touches only transcript_findings. The map is empty (not nil-erroring) on a
// fresh install.
func (db *DB) GetFindingsCountByBook(ctx context.Context) (map[string]int, error) {
	return db.getFindingsCountByBook(ctx, db.pool)
}

// getFindingsCountByBook is the querier-parameterized core of
// GetFindingsCountByBook, split out so the query + scan path is testable against
// a mock pool (mirrors listFindings / getBookTracks).
func (db *DB) getFindingsCountByBook(ctx context.Context, q rowQuerier) (map[string]int, error) {
	rows, err := q.Query(ctx, findingsCountByBookSQL)
	if err != nil {
		return nil, fmt.Errorf("findings count by book query: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var dir string
		var n int
		if err := rows.Scan(&dir, &n); err != nil {
			return nil, fmt.Errorf("scan findings count: %w", err)
		}
		out[dir] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (findings count by book): %w", err)
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

// likePattern wraps a user substring for a case-insensitive ILIKE match. It
// escapes the LIKE metacharacters (%, _, \) in the user input via likePrefix
// before adding the surrounding % wildcards, so a substring containing % or _
// matches those characters literally instead of widening the match. This keeps
// the match a true substring (preserving the title-fragment ergonomics) while
// closing the unbounded-wildcard hole that would otherwise let `eval`/`requeue`
// scope explode. The escaping assumes the default Postgres LIKE escape char (\),
// which the bare `ILIKE $1` call sites all use.
func likePattern(substr string) string { return "%" + likePrefix(substr) + "%" }

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

	ids, paths, err := requeueTx(ctx, tx, plan, args...)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit requeue tx: %w", err)
	}

	// Audit events for the operator requeue (CONTRACT §1.7). Emitted AFTER commit
	// (the requeue is the source of truth; an event-write failure must not roll it
	// back) and best-effort. The job moves back to 'pending', so this is a requeue.
	for i, id := range ids {
		var fp string
		if i < len(paths) {
			fp = paths[i]
		}
		db.emitEvent(ctx, PipelineEvent{
			JobID: id, FilePath: fp, Stage: StageRequeue, Event: EventRetry,
			RunnerHost: HostGoMonitor, Reason: "operator requeue (re-transcribe)",
		})
	}
	return paths, nil
}

// requeueTx is the transaction-body core of requeue, split out so the
// delete-transcripts → reset-jobs → clear-metrics sequence is testable against a
// pgxmock transaction. It does NOT begin/commit — the caller owns the tx
// lifecycle. Returns the reset jobs' ids and file paths (parallel slices).
func requeueTx(ctx context.Context, tx txQuerier, plan requeuePlan, args ...any) (ids, paths []string, err error) {
	if _, err := tx.Exec(ctx, plan.deleteTranscripts, args...); err != nil {
		return nil, nil, fmt.Errorf("delete transcripts: %w", err)
	}

	rows, err := tx.Query(ctx, plan.resetJobs, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("reset jobs: %w", err)
	}
	ids, paths, err = scanIDPaths(rows)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}

	// Clear the orphaned run_metrics for the requeued jobs so the next run's
	// telemetry starts clean. A no-op when nothing was reset.
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, requeueDeleteMetricsSQL, ids); err != nil {
			return nil, nil, fmt.Errorf("delete run_metrics: %w", err)
		}
	}
	return ids, paths, nil
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
