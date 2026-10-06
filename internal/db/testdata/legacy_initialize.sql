-- legacy_initialize.sql — FROZEN copy of the inline schema DDL that
-- db.initialize() executed on every boot before goose (origin/main d511da5,
-- jedwards1230/earmark v0.40.2), extracted verbatim, one block per tx.Exec call,
-- in order. Test fixture only: migrate_integration_test.go builds a database
-- with it and proves 00001_baseline.sql produces the identical catalog, and
-- that migrating such a database stamps version 1 without running DDL.
-- Never edit: it is history.

		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE EXTENSION IF NOT EXISTS pg_trgm;

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
			-- The acknowledgement paired with the phase column above: phase is the
			-- request (coordinator -> runner), this is the result (runner ->
			-- coordinator). NULL means NOT parked — an un-stamped runner must never
			-- read as a free GPU. Declared inline AND migrated, mirroring phase.
			runner_gpu_parked BOOLEAN,
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
		-- the nullable columns (chapters in PR 4, bias_terms in PR 5). It is
		-- additive and a missing row is a no-op (search results simply carry no
		-- chapter label). chapters times are BOOK-absolute across all of the
		-- book's tracks — see scanResults for the track-offset translation.
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

		ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS runner_heartbeat_at TIMESTAMPTZ;

		ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS runner_gpu_parked BOOLEAN;

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

		ALTER TABLE transcript_findings
			ADD COLUMN IF NOT EXISTS patch_state         TEXT NOT NULL DEFAULT 'proposed',
			ADD COLUMN IF NOT EXISTS anchor_offset       INTEGER,
			ADD COLUMN IF NOT EXISTS anchor_occurrence   INTEGER,
			ADD COLUMN IF NOT EXISTS chunk_text_sha256   TEXT,
			ADD COLUMN IF NOT EXISTS decided_at          TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS decided_by          TEXT,
			ADD COLUMN IF NOT EXISTS applied_at          TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS applied_before_text TEXT,
			ADD COLUMN IF NOT EXISTS applied_after_text  TEXT,
			ADD COLUMN IF NOT EXISTS stale_reason        TEXT;
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint WHERE conname = 'transcript_findings_patch_state_valid'
			) THEN
				ALTER TABLE transcript_findings
					ADD CONSTRAINT transcript_findings_patch_state_valid
					CHECK (patch_state IN
						('proposed','accepted','rejected','applied','stale','reverted'));
			END IF;
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$;
		CREATE INDEX IF NOT EXISTS transcript_findings_patch_state_idx
			ON transcript_findings (patch_state);

		ALTER TABLE transcript_findings
			ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'judge';
		DO $$ BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint WHERE conname = 'transcript_findings_origin_valid'
			) THEN
				ALTER TABLE transcript_findings
					ADD CONSTRAINT transcript_findings_origin_valid
					CHECK (origin IN ('judge','human'));
			END IF;
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$;

		ALTER TABLE transcript_chunks
			ADD COLUMN IF NOT EXISTS embedding_stale BOOLEAN NOT NULL DEFAULT false;
		CREATE INDEX IF NOT EXISTS transcript_chunks_embedding_stale_idx
			ON transcript_chunks (embedding_stale) WHERE embedding_stale;

		ALTER TABLE transcript_chunks
			ADD COLUMN IF NOT EXISTS source_text TEXT;

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

		ALTER TABLE run_metrics
			ADD COLUMN IF NOT EXISTS asr_family           TEXT,
			ADD COLUMN IF NOT EXISTS asr_runtime          TEXT,
			ADD COLUMN IF NOT EXISTS caps_applied         JSONB,
			ADD COLUMN IF NOT EXISTS caps_requested       JSONB,
			ADD COLUMN IF NOT EXISTS caps_skipped_reason  JSONB,
			ADD COLUMN IF NOT EXISTS mean_word_confidence FLOAT8;

		ALTER TABLE book_metadata
			ADD COLUMN IF NOT EXISTS description TEXT,
			ADD COLUMN IF NOT EXISTS genres      TEXT[],
			ADD COLUMN IF NOT EXISTS isbn        TEXT;

		ALTER TABLE run_metrics
			ADD COLUMN IF NOT EXISTS eval_started_at  TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS eval_finished_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS eval_model       TEXT,
			ADD COLUMN IF NOT EXISTS eval_chunks      INT,
			ADD COLUMN IF NOT EXISTS eval_skipped     INT,
			ADD COLUMN IF NOT EXISTS eval_findings    INT;

		ALTER TABLE run_metrics
			ADD COLUMN IF NOT EXISTS eval_failed_at     TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS eval_failed_chunks INT,
			ADD COLUMN IF NOT EXISTS eval_error         TEXT;

		ALTER TABLE transcript_findings ADD COLUMN IF NOT EXISTS resolved_model TEXT;
		ALTER TABLE run_metrics ADD COLUMN IF NOT EXISTS eval_resolved_model TEXT;

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

