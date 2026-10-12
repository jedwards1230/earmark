-- 00015_decide_duplicates.sql — the decide step records agreement and
-- conflict (CONTRACT §2.17 "Version history", §2.19 "Agreeing and conflicting
-- findings"; docs/design/audit-lineage.md "Agreement is not rejection").
--
-- 00013 added patch_state 'duplicate' and transcript_findings.duplicate_of;
-- the decide step (decide step_version 3) is the first writer. Two things the
-- log could not yet say:
--
--   * finding_events.outcome gains 'duplicate': a decide recipe's verdict that
--     a finding proposes the same fix as another (its representative), which
--     carries the decision. It is a decision — attributed to the recipe,
--     revocable by `earmark decide revert`, counted per recipe — but neither
--     an apply, a hold nor a reject. A duplicate decision must name its
--     representative (related_finding_id).
--   * finding_events.related_finding_id: the other finding an event is about —
--     a duplicate's representative, and the winner an overlap_dup reject lost
--     to. A plain column, like transcript_id: finding_events is append-only,
--     so a foreign key could neither SET NULL nor CASCADE without rewriting
--     history, and the event must outlive the finding it points at.
--
-- Locks. Up takes ACCESS EXCLUSIVE on finding_events only, under a 5s lock
-- timeout. Writers reach finding_events after locking transcript_findings
-- (the transition triggers, the decide write), and this migration takes no
-- other lock, so it cannot deadlock with them; it waits for, and then blocks,
-- in-flight writers for the length of the CHECK re-validation (a scan of the
-- event log, well under a second at live scale). The new column has no
-- default, so nothing is rewritten.

-- +goose Up
SET LOCAL lock_timeout = '5s';
LOCK TABLE finding_events IN ACCESS EXCLUSIVE MODE;

ALTER TABLE finding_events ADD COLUMN related_finding_id UUID;

ALTER TABLE finding_events DROP CONSTRAINT finding_events_outcome_valid;
ALTER TABLE finding_events
    ADD CONSTRAINT finding_events_outcome_valid
    CHECK (outcome IN ('apply', 'hold', 'reject', 'duplicate'));
-- NOT VALID: every new row is checked, but rows a rollback left behind (a
-- Down keeps the append-only log's 'duplicate' decisions and drops their
-- related_finding_id) are not re-validated, so Down → Up round-trips.
ALTER TABLE finding_events
    ADD CONSTRAINT finding_events_duplicate_names_representative
    CHECK (outcome IS DISTINCT FROM 'duplicate' OR related_finding_id IS NOT NULL) NOT VALID;

-- "Which events point at finding X" (a representative's duplicates, an
-- overlap winner's losers).
CREATE INDEX finding_events_related_idx ON finding_events (related_finding_id)
    WHERE related_finding_id IS NOT NULL;

-- +goose Down
-- related_finding_id is dropped with its values (the duplicates themselves
-- keep transcript_findings.duplicate_of). Down therefore REFUSES while any
-- event carries one, unless the session sets earmark.allow_data_loss = 'on'
-- (see 00013's Down). 'duplicate' decisions are never deleted —
-- finding_events is append-only — so the restored outcome CHECK is added NOT
-- VALID: it binds every new row and leaves the recorded ones as they are.
SET LOCAL lock_timeout = '5s';
-- Locked before the check, so no event naming a related finding can land
-- between the check and the DROP COLUMN.
LOCK TABLE finding_events IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM finding_events WHERE related_finding_id IS NOT NULL)
       AND COALESCE(current_setting('earmark.allow_data_loss', true), '') <> 'on' THEN
        RAISE EXCEPTION 'refusing to roll back migration 15: % finding event(s) name a related finding',
            (SELECT count(*) FROM finding_events WHERE related_finding_id IS NOT NULL)
            USING HINT = 'back the table up, then set earmark.allow_data_loss = ''on'' for the session running the rollback';
    END IF;
END $$;
-- +goose StatementEnd

DROP INDEX finding_events_related_idx;
ALTER TABLE finding_events DROP CONSTRAINT finding_events_duplicate_names_representative;
ALTER TABLE finding_events DROP CONSTRAINT finding_events_outcome_valid;
ALTER TABLE finding_events
    ADD CONSTRAINT finding_events_outcome_valid
    CHECK (outcome IN ('apply', 'hold', 'reject')) NOT VALID;
ALTER TABLE finding_events DROP COLUMN related_finding_id;
