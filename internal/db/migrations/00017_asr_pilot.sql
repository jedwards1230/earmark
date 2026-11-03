-- 00017_asr_pilot.sql — second-ASR pilot transcripts and the asr_vote
-- evidence recorded from them (CONTRACT §1.11 "ASR pilot").
--
-- A pilot runs a second ASR model (Granite Speech, Canary-Qwen) over tracks
-- that already have a production transcript, to measure it against the
-- production model and to ask, for each held finding, which of the two
-- texts the second model heard. Nothing here touches production:
--
--   * pilot_transcripts — one row per (track, model, revision, scope), written
--     by runner/pilot.py (INSERT only). It is NOT a second `transcripts` row:
--     transcripts are UNIQUE per job and drive chunking/embedding, which a
--     pilot must never trigger. production_transcript_id names the
--     production transcript the pilot ran against, ON DELETE CASCADE: a
--     requeue deletes that transcript, and a pilot measured against text that
--     no longer exists measures nothing. The pilot rows and their votes are
--     NOT archived (they are GPU hours, but reproducible from the audio);
--     `earmark requeue` prints how many it will delete before it runs, so
--     back them up first if they matter. scope 'full' is the whole track;
--     'findings' is only the windows around held findings (segments carry
--     absolute track times, audio_sec is the sum of the window lengths).
--     has_word_times is false for a model that emits segment times only;
--     its words then carry no start/end.
--   * finding_asr_votes — one row per (finding, pilot transcript), written by
--     `earmark asr-pilot vote --yes` (upsert): heard_replacement /
--     heard_original / neither, unmappable when the span could not be
--     placed in time, or style_only for a correction that changes only case,
--     punctuation or a spelling variant (what was heard is in
--     detail.style_vote; it is never counted as agreement on the word). chunk_text_sha256 is the chunk revision the vote was
--     made against. Nothing reads it in decide; it is evidence for a later,
--     versioned decide recipe. finding_events is untouched.
--
-- Locks. Two new tables; each foreign key takes SHARE ROW EXCLUSIVE on the
-- table it references (blocking its writers, not its readers, for the
-- length of the migration). Both are taken up front under a 5s lock
-- timeout, transcript_findings first. A requeue row-locks its transcripts
-- (FOR UPDATE: ROW SHARE, which SHARE ROW EXCLUSIVE does not conflict
-- with), then writes transcript_findings, then deletes the transcripts. Taken
-- in this order, a requeue that is under way only makes the migration wait
-- at its first lock (or the requeue wait at its findings write) — never
-- each holding what the other needs, as transcripts-first would against the
-- requeue's DELETE. No ALTER, no rewrite, no backfill.

-- +goose Up
SET LOCAL lock_timeout = '5s';
LOCK TABLE transcript_findings, transcripts IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE pilot_transcripts (
    id                       BIGSERIAL   PRIMARY KEY,
    file_path                TEXT        NOT NULL,
    production_transcript_id UUID        NOT NULL REFERENCES transcripts (id) ON DELETE CASCADE,
    backend                  TEXT        NOT NULL,
    model_id                 TEXT        NOT NULL,
    model_revision           TEXT        NOT NULL,
    scope                    TEXT        NOT NULL DEFAULT 'full'
                             CONSTRAINT pilot_transcripts_scope_valid CHECK (scope IN ('full', 'findings')),
    window_sec               DOUBLE PRECISION,
    has_word_times           BOOLEAN     NOT NULL,
    segments                 JSONB       NOT NULL
                             CONSTRAINT pilot_transcripts_segments_array CHECK (jsonb_typeof(segments) = 'array'),
    raw_text                 TEXT        NOT NULL,
    audio_sec                DOUBLE PRECISION,
    wall_sec                 DOUBLE PRECISION,
    rtfx                     DOUBLE PRECISION,
    peak_vram_mib            INTEGER,
    peak_reserved_mib        INTEGER,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pilot_transcripts_track_model_unique UNIQUE (file_path, model_id, model_revision, scope)
);
CREATE INDEX pilot_transcripts_production_idx ON pilot_transcripts (production_transcript_id);
CREATE INDEX pilot_transcripts_model_idx ON pilot_transcripts (model_id, model_revision);

CREATE TABLE finding_asr_votes (
    id                  BIGSERIAL   PRIMARY KEY,
    finding_id          UUID        NOT NULL REFERENCES transcript_findings (id) ON DELETE CASCADE,
    pilot_transcript_id BIGINT      NOT NULL REFERENCES pilot_transcripts (id) ON DELETE CASCADE,
    model_id            TEXT        NOT NULL,
    chunk_text_sha256   TEXT,
    span_start_sec      DOUBLE PRECISION,
    span_end_sec        DOUBLE PRECISION,
    vote                TEXT        NOT NULL
                        CONSTRAINT finding_asr_votes_vote_valid
                        CHECK (vote IN ('heard_replacement', 'heard_original', 'neither', 'unmappable', 'style_only')),
    heard_text          TEXT,
    detail              JSONB       NOT NULL DEFAULT '{}'
                        CONSTRAINT finding_asr_votes_detail_object CHECK (jsonb_typeof(detail) = 'object'),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT finding_asr_votes_finding_pilot_unique UNIQUE (finding_id, pilot_transcript_id)
);
CREATE INDEX finding_asr_votes_pilot_idx ON finding_asr_votes (pilot_transcript_id);
CREATE INDEX finding_asr_votes_model_idx ON finding_asr_votes (model_id, vote);

-- +goose Down
-- pilot_transcripts holds GPU hours of output that only a fresh pilot run
-- can recreate. Down therefore REFUSES while it has rows, unless the session
-- sets earmark.allow_data_loss = 'on' (14's guard). finding_asr_votes is
-- recomputable from it and goes with it.
SET LOCAL lock_timeout = '5s';
-- Locked before the count, so no row pilot.py commits can land between the
-- check and the DROP.
LOCK TABLE pilot_transcripts, finding_asr_votes IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pilot_transcripts)
       AND COALESCE(current_setting('earmark.allow_data_loss', true), '') <> 'on' THEN
        RAISE EXCEPTION 'refusing to roll back migration 17: pilot_transcripts has % row(s)',
            (SELECT count(*) FROM pilot_transcripts)
            USING HINT = 'back the table up, then set earmark.allow_data_loss = ''on'' for the session running the rollback';
    END IF;
END $$;
-- +goose StatementEnd

DROP TABLE finding_asr_votes;
DROP TABLE pilot_transcripts;
