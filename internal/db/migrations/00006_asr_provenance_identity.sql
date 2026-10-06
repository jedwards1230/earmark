-- 00006_asr_provenance_identity.sql — runner-reported ASR provenance and the
-- embedded ASIN tag (CONTRACT §1.2, §1.6, §1.9).
--
-- transcripts gains four columns the ASR runner fills on INSERT:
--
--   embedded_asin       the ASIN tag embedded in the audio file (ffprobe format
--                       tags ASIN / AUDIBLE_ASIN / ----:…:ASIN), validated by
--                       the runner; NULL when the file carries none.
--   asr_model_sha256    sha256 of the .nemo model file the runner loaded
--                       (the asr recipe's model_revision); NULL when unknown.
--   asr_runner_version  the runner's version tag (the asr recipe's code_version).
--   asr_params          the runner's output-shaping parameters (dtype, chunk
--                       window/overlap, segmentation, diarization, biasing).
--
-- The Go worker builds the asr recipe from these and stamps
-- transcripts.recipe_id (the seam 00002 left NULL for new transcripts). A runner
-- that predates this migration writes none of them, and its rows stay
-- unstamped — nothing is invented.
--
-- book_metadata gains the identity outcome of the ASIN step:
--
--   asin_source      dir | filename | embedded_tag — where the ASIN came from.
--   identity_status  exact | conflict — exact: the catalogue record was matched
--                    by ASIN; conflict: the embedded tag named a record whose
--                    title does not match the book's, so it was NOT used.
--                    NULL: not resolved against a catalogue (local-only).
--
-- All columns are nullable and added without a default: no table rewrite, and
-- every existing reader keeps working.

-- +goose Up
ALTER TABLE transcripts
    ADD COLUMN embedded_asin      TEXT,
    ADD COLUMN asr_model_sha256   TEXT,
    ADD COLUMN asr_runner_version TEXT,
    ADD COLUMN asr_params         JSONB;

-- The worker's stamping pass: transcripts a provenance-aware runner wrote that
-- have no recipe yet. Partial, so it stays tiny however large the table grows.
CREATE INDEX transcripts_asr_unstamped_idx ON transcripts (created_at)
    WHERE recipe_id IS NULL AND asr_runner_version IS NOT NULL;

ALTER TABLE book_metadata
    ADD COLUMN asin_source     TEXT
        CONSTRAINT book_metadata_asin_source_valid
        CHECK (asin_source IN ('dir', 'filename', 'embedded_tag')),
    ADD COLUMN identity_status TEXT
        CONSTRAINT book_metadata_identity_status_valid
        CHECK (identity_status IN ('exact', 'conflict'));

-- +goose Down
ALTER TABLE book_metadata
    DROP COLUMN identity_status,
    DROP COLUMN asin_source;
DROP INDEX transcripts_asr_unstamped_idx;
ALTER TABLE transcripts
    DROP COLUMN asr_params,
    DROP COLUMN asr_runner_version,
    DROP COLUMN asr_model_sha256,
    DROP COLUMN embedded_asin;
