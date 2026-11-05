-- 00019_finding_events_detail.sql — a structured audit detail on decision
-- events (CONTRACT §2.17 "Version history", §2.19 "Audio-vote rung").
--
-- The audio-vote rung (`earmark decide --audio-vote`) decides held findings
-- from the second-ASR votes of migration 17: this ends 17's "nothing reads it
-- in decide" — the rung reads finding_asr_votes, and nothing else in decide
-- does. A vote row is upserted in place (a re-vote overwrites it), so the
-- event must carry a snapshot of the votes it rested on: finding_events.detail,
-- a JSON object ({"rung": "asr_vote", "rule": …, "votes": [{vote_id, model_id,
-- model_revision, pilot_transcript_id, vote, updated_at}, …]}). Every other
-- writer leaves it NULL, and every existing row stays NULL.
--
-- Locks. ADD COLUMN with no default rewrites nothing, but takes ACCESS
-- EXCLUSIVE on finding_events for a moment; transitions are appended to it by
-- triggers on transcript_findings, so fail fast rather than queue every
-- writer behind it (the next boot retries). The append-only triggers are row
-- triggers (BEFORE UPDATE OR DELETE) and do not fire for DDL. The CHECK is
-- added NOT VALID: it holds for every row written from now on, and the rows
-- already there are all NULL, so a validating scan of the table under the
-- exclusive lock would prove nothing. Neither statement is guarded with IF NOT
-- EXISTS (PostgreSQL has none for ADD CONSTRAINT): goose applies the version
-- once, in one transaction, so both land or neither does. Down drops both with
-- IF EXISTS.

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE finding_events ADD COLUMN detail JSONB;
ALTER TABLE finding_events ADD CONSTRAINT finding_events_detail_object
    CHECK (detail IS NULL OR jsonb_typeof(detail) = 'object') NOT VALID;

-- +goose Down
-- The detail is the only record of the votes an audio-vote decision rested
-- on (the vote rows change in place). Down therefore REFUSES while any row
-- carries one, unless the session sets earmark.allow_data_loss = 'on' (14's
-- guard).
SET LOCAL lock_timeout = '5s';
LOCK TABLE finding_events IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM finding_events WHERE detail IS NOT NULL)
       AND COALESCE(current_setting('earmark.allow_data_loss', true), '') <> 'on' THEN
        RAISE EXCEPTION 'refusing to roll back migration 19: % finding_events row(s) carry a detail',
            (SELECT count(*) FROM finding_events WHERE detail IS NOT NULL)
            USING HINT = 'back the column up, then set earmark.allow_data_loss = ''on'' for the session running the rollback';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE finding_events DROP CONSTRAINT IF EXISTS finding_events_detail_object;
ALTER TABLE finding_events DROP COLUMN IF EXISTS detail;
