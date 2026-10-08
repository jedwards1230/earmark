-- 00008_finding_events.sql — append-only version history for findings
-- (CONTRACT §2.17 "Version history", "Automated decisions").
--
-- A transcript's text is its pristine source plus its accepted findings
-- replayed (§2.17 "Applying"); a finding is one anchored patch. This table is
-- the patch log: every state change of every finding, every decide-recipe
-- verdict, and every revocation of one, appended and never rewritten. Blame,
-- point-in-time reconstruction and undo (by finding, recipe or time) are
-- queries over it.
--
-- Three kinds of row:
--   * transition — patch_state changed. Written ONLY by the triggers below, so
--     every path that moves a finding (a reviewer, `earmark decide`, the
--     rebuild's applied/stale, reanchor, requeue's supersede) is recorded
--     without each writer having to remember to. An INSERT of a finding in a
--     state other than proposed (a human's direct correction) is a transition
--     from NULL.
--   * decision — a decide recipe's verdict on a finding (apply / hold /
--     reject), with its probability, evidence class and fn_calls row. A hold
--     is a decision whose finding stays proposed.
--   * revoke — withdraws one decision (revokes_event_id). Undo appends; it
--     never edits or deletes the row it undoes.
--
-- Transition attribution. actor is NEW.decided_by when the UPDATE stamped a
-- decision (decided_at or decided_by changed: accept, reject, revert, and a
-- bulk decide), else the transaction-local setting earmark.actor
-- (db.SetEventContext, set_config(..., true)), else 'system'. recipe_id is
-- the setting earmark.recipe_id when set, else the recipe named by a jev
-- decider (decided_by 'jev:<id>' or 'revert:jev:<id>'). It is a foreign key,
-- so a decision attributed to an unregistered recipe fails the transition.
--
-- Append-only: UPDATE is refused outright; DELETE only when fired by a cascade
-- from transcript_findings (pg_trigger_depth() > 1 — the FK action runs as a
-- trigger), so deleting a finding still takes its history with it.
--
-- transcript_id is a plain column, not a foreign key: history outlives a
-- requeue that deletes the transcript, so "what did transcript T read as at
-- time X" still answers for an archived T. It is NULL only for a backfilled
-- finding a requeue already archived (5's superseded rows with no
-- transcript).
--
-- Backfill: one transition per finding with decided_at, created_at =
-- decided_at, to_state = its current state. Earlier history was never
-- recorded; the reason column says so.
--
-- Locks: CREATE TRIGGER takes SHARE ROW EXCLUSIVE on transcript_findings,
-- which blocks writers until this migration commits. That is what makes the
-- backfill exact (no decision can land between it and the trigger), and why
-- the triggers are created first and the migration gives up after 5s behind a
-- long transaction (the process retries on its next start).

-- +goose Up
SET LOCAL lock_timeout = '5s';

CREATE TABLE finding_events (
    id                BIGSERIAL   PRIMARY KEY,
    finding_id        UUID        NOT NULL REFERENCES transcript_findings (id) ON DELETE CASCADE,
    transcript_id     UUID,
    kind              TEXT        NOT NULL
                      CONSTRAINT finding_events_kind_valid CHECK (kind IN ('transition', 'decision', 'revoke')),
    from_state        TEXT,
    to_state          TEXT,
    outcome           TEXT
                      CONSTRAINT finding_events_outcome_valid CHECK (outcome IN ('apply', 'hold', 'reject')),
    reason            TEXT,
    actor             TEXT        NOT NULL,
    recipe_id         TEXT        REFERENCES recipes (recipe_id),
    p                 FLOAT8
                      CONSTRAINT finding_events_p_range CHECK (p IS NULL OR p BETWEEN 0 AND 1),
    evidence          TEXT
                      CONSTRAINT finding_events_evidence_valid CHECK (evidence IN ('asin_verbatim', 'exact_repeat', 'none')),
    fn_call_id        BIGINT      REFERENCES fn_calls (id),
    chunk_text_sha256 TEXT,
    issue_type        TEXT,
    revokes_event_id  BIGINT      REFERENCES finding_events (id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- Each kind carries what it means.
    CONSTRAINT finding_events_kind_shape CHECK (
        (kind = 'transition' AND to_state IS NOT NULL AND outcome IS NULL AND revokes_event_id IS NULL)
     OR (kind = 'decision' AND outcome IS NOT NULL AND recipe_id IS NOT NULL AND revokes_event_id IS NULL)
     OR (kind = 'revoke' AND revokes_event_id IS NOT NULL AND outcome IS NULL)
    )
);

-- A finding's history, newest first (FindingHistory, the stale_work decide arm).
CREATE INDEX finding_events_finding_idx ON finding_events (finding_id, id DESC);
-- What did recipe X decide, and when (undo by recipe, cost/quality queries).
CREATE INDEX finding_events_recipe_idx ON finding_events (recipe_id, created_at);
-- A transcript's patch set at time T (TranscriptPatchSetAt).
CREATE INDEX finding_events_transcript_idx ON finding_events (transcript_id, created_at);
-- A decision is revoked at most once; also the "is it revoked" lookup.
CREATE UNIQUE INDEX finding_events_revokes_idx ON finding_events (revokes_event_id)
    WHERE kind = 'revoke';

-- +goose StatementBegin
CREATE FUNCTION finding_events_record_transition() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    stamped BOOLEAN := TG_OP = 'INSERT'
        OR NEW.decided_at IS DISTINCT FROM OLD.decided_at
        OR NEW.decided_by IS DISTINCT FROM OLD.decided_by;
    setting_actor TEXT := NULLIF(current_setting('earmark.actor', true), '');
    who TEXT;
    recipe TEXT := NULLIF(current_setting('earmark.recipe_id', true), '');
BEGIN
    IF stamped THEN
        who := COALESCE(NEW.decided_by, setting_actor, 'system');
    ELSE
        who := COALESCE(setting_actor, 'system');
    END IF;
    IF recipe IS NULL AND who ~ '^(revert:)?jev:[0-9a-f]{64}$' THEN
        recipe := substring(who FROM '[0-9a-f]{64}$');
    END IF;
    INSERT INTO finding_events (finding_id, transcript_id, kind, from_state, to_state,
                                actor, recipe_id, chunk_text_sha256, issue_type)
    VALUES (NEW.id,
            CASE WHEN TG_OP = 'INSERT' THEN NEW.transcript_id
                 ELSE COALESCE(NEW.transcript_id, OLD.transcript_id) END,
            'transition',
            CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE OLD.patch_state END,
            NEW.patch_state, who, recipe, NEW.chunk_text_sha256, NEW.issue_type);
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION finding_events_append_only() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1 THEN
        RETURN OLD; -- the ON DELETE CASCADE from transcript_findings
    END IF;
    RAISE EXCEPTION 'finding_events is append-only: % refused (append a revoke event instead)', TG_OP
        USING ERRCODE = 'restrict_violation';
END $$;
-- +goose StatementEnd

CREATE TRIGGER finding_events_append_only
    BEFORE UPDATE OR DELETE ON finding_events
    FOR EACH ROW EXECUTE FUNCTION finding_events_append_only();

CREATE TRIGGER transcript_findings_record_transition
    AFTER UPDATE OF patch_state ON transcript_findings
    FOR EACH ROW
    WHEN (OLD.patch_state IS DISTINCT FROM NEW.patch_state)
    EXECUTE FUNCTION finding_events_record_transition();

CREATE TRIGGER transcript_findings_record_insert
    AFTER INSERT ON transcript_findings
    FOR EACH ROW
    WHEN (NEW.patch_state <> 'proposed')
    EXECUTE FUNCTION finding_events_record_transition();

INSERT INTO finding_events (finding_id, transcript_id, kind, from_state, to_state, reason,
                            actor, chunk_text_sha256, issue_type, created_at)
SELECT f.id, f.transcript_id, 'transition', NULL, f.patch_state,
       'backfill: state at migration 8; earlier history was not recorded',
       COALESCE(f.decided_by, 'unknown'), f.chunk_text_sha256, f.issue_type, f.decided_at
  FROM transcript_findings f
 WHERE f.decided_at IS NOT NULL
 ORDER BY f.decided_at, f.id;

CREATE OR REPLACE VIEW stale_work AS
WITH cur AS (
    SELECT cr.step, r.recipe_id, r.step_version, r.model_alias, r.model_resolved,
           r.model_revision, r.prompt_version, r.prompt_sha256, r.params
      FROM current_recipes cr
      JOIN recipes r ON r.recipe_id = cr.recipe_id
)
SELECT 'asr'::text AS step, 'transcripts'::text AS source_table, t.id AS row_id,
       t.recipe_id, cur.recipe_id AS current_recipe_id
  FROM transcripts t
  JOIN cur ON cur.step = 'asr'
  LEFT JOIN recipes r ON r.recipe_id = t.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'propose', 'transcript_findings', f.id, f.recipe_id, cur.recipe_id
  FROM transcript_findings f
  JOIN cur ON cur.step = 'propose'
  LEFT JOIN recipes r ON r.recipe_id = f.recipe_id
 WHERE f.origin = 'judge'
   AND f.patch_state <> 'superseded'
   AND (r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params))
UNION ALL
SELECT 'embed', 'transcript_chunks', c.id, c.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'embed'
  LEFT JOIN recipes r ON r.recipe_id = c.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
-- decide: a finding whose latest live (unrevoked) decision was made by a
-- recipe that is not equivalent to the current decide recipe.
SELECT 'decide', 'transcript_findings', d.finding_id, d.recipe_id, cur.recipe_id
  FROM (SELECT DISTINCT ON (e.finding_id) e.finding_id, e.recipe_id
          FROM finding_events e
         WHERE e.kind = 'decision'
           AND NOT EXISTS (SELECT 1 FROM finding_events v
                            WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
         ORDER BY e.finding_id, e.id DESC) d
  JOIN transcript_findings f ON f.id = d.finding_id
  JOIN cur ON cur.step = 'decide'
  JOIN recipes r ON r.recipe_id = d.recipe_id
 WHERE f.patch_state <> 'superseded'
   AND (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params);

-- +goose Down
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE VIEW stale_work AS
WITH cur AS (
    SELECT cr.step, r.recipe_id, r.step_version, r.model_alias, r.model_resolved,
           r.model_revision, r.prompt_version, r.prompt_sha256, r.params
      FROM current_recipes cr
      JOIN recipes r ON r.recipe_id = cr.recipe_id
)
SELECT 'asr'::text AS step, 'transcripts'::text AS source_table, t.id AS row_id,
       t.recipe_id, cur.recipe_id AS current_recipe_id
  FROM transcripts t
  JOIN cur ON cur.step = 'asr'
  LEFT JOIN recipes r ON r.recipe_id = t.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'propose', 'transcript_findings', f.id, f.recipe_id, cur.recipe_id
  FROM transcript_findings f
  JOIN cur ON cur.step = 'propose'
  LEFT JOIN recipes r ON r.recipe_id = f.recipe_id
 WHERE f.origin = 'judge'
   AND f.patch_state <> 'superseded'
   AND (r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params))
UNION ALL
SELECT 'embed', 'transcript_chunks', c.id, c.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'embed'
  LEFT JOIN recipes r ON r.recipe_id = c.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params);

DROP TRIGGER transcript_findings_record_insert ON transcript_findings;
DROP TRIGGER transcript_findings_record_transition ON transcript_findings;
DROP TABLE finding_events;
DROP FUNCTION finding_events_append_only();
DROP FUNCTION finding_events_record_transition();
