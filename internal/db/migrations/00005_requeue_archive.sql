-- 00005_requeue_archive.sql — requeue archives findings instead of orphaning
-- them (CONTRACT §1.4 "Operator requeue", §2.17 "superseded").
--
-- Before this migration a requeue deleted the transcript and left its findings
-- pointing at an id that no longer existed: transcript_findings.transcript_id
-- had no foreign key. Now:
--
--   * requeue moves the transcript's findings to patch_state 'superseded' (kept
--     for audit and the bench, never replayed) in the same transaction as the
--     delete, stamping superseded_at;
--   * transcript_findings.transcript_id REFERENCES transcripts(id) ON DELETE SET
--     NULL, so no finding can point at a missing transcript again. The column
--     becomes nullable for that; a NULL transcript_id means "its transcript was
--     replaced". file_path and transcription_run_id still say which track and
--     which run it came from.
--
-- Existing orphans (findings whose transcript is already gone) are archived the
-- same way BEFORE the foreign key is added: the only thing that deletes a
-- transcript is a requeue (or the debug reset, which drops the table), so an
-- orphan is exactly a finding a requeue would now supersede. Their
-- superseded_at stays NULL — when the requeue happened was never recorded.
-- Live count when this was written (2026-10-06): 0 of 32,337.
--
-- The foreign key is added VALIDATED, in one step, rather than NOT VALID plus
-- a later VALIDATE. Splitting only pays off when validation runs in a separate
-- transaction so writers are not blocked while it scans; goose runs this file
-- in one transaction, and at live scale (~32k findings, ~4.3k transcripts) the
-- validating anti-join over the transcripts primary key takes milliseconds.
-- The orphan UPDATE above runs first, so validation cannot fail.
--
-- Two guards keep it that way:
--   * CHECK (transcript_id IS NOT NULL OR patch_state = 'superseded'): a NULL
--     transcript_id means "archived by a requeue", enforced by the database. A
--     transcript delete that would leave a live finding behind (anything but a
--     requeue, or a requeue that missed a row) fails instead of orphaning it.
--   * requeue locks the transcript rows FOR UPDATE before superseding, so a
--     finding INSERT racing it either lands first (and is archived) or fails
--     on the foreign key after it commits (internal/db requeueTx).
--
-- stale_work stops listing superseded findings: they are an archive, not work
-- to redo.
--
-- lock_timeout: the ALTERs take ACCESS EXCLUSIVE on transcript_findings and
-- the foreign key SHARE ROW EXCLUSIVE on transcripts for the whole
-- transaction. Behind a long reader they would queue and stall every later
-- reader of the table, so the migration gives up after 5s instead (the
-- process retries the migration on its next start).
--
-- The patch_state CHECK is rebuilt from its CURRENT value list plus
-- 'superseded', rather than restated. Another migration in flight adds its own
-- state to the same constraint; restating the list here would silently drop
-- that state again, depending only on which file sorts last.

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE transcript_findings ALTER COLUMN transcript_id DROP NOT NULL;
ALTER TABLE transcript_findings ADD COLUMN superseded_at TIMESTAMPTZ;

-- +goose StatementBegin
CREATE FUNCTION pg_temp.set_patch_states(add_state TEXT, drop_state TEXT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    states TEXT[];
BEGIN
    SELECT array_agg(m.v[1] ORDER BY m.n)
      INTO states
      FROM pg_constraint c,
           regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''', 'g')
             WITH ORDINALITY AS m(v, n)
     WHERE c.conrelid = 'transcript_findings'::regclass
       AND c.conname = 'transcript_findings_patch_state_valid';
    IF states IS NULL THEN
        RAISE EXCEPTION 'transcript_findings_patch_state_valid not found or has no values';
    END IF;
    states := array_remove(states, drop_state);
    IF add_state IS NOT NULL AND NOT add_state = ANY (states) THEN
        states := states || add_state;
    END IF;
    ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_patch_state_valid;
    EXECUTE format(
        'ALTER TABLE transcript_findings ADD CONSTRAINT transcript_findings_patch_state_valid '
        'CHECK (patch_state IN (%s))',
        (SELECT string_agg(quote_literal(s), ', ' ORDER BY i)
           FROM unnest(states) WITH ORDINALITY AS u(s, i)));
END $$;
-- +goose StatementEnd

SELECT pg_temp.set_patch_states('superseded', NULL);

UPDATE transcript_findings f
   SET transcript_id = NULL,
       patch_state   = 'superseded'
 WHERE f.transcript_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM transcripts t WHERE t.id = f.transcript_id);

ALTER TABLE transcript_findings
    ADD CONSTRAINT transcript_findings_transcript_id_fkey
    FOREIGN KEY (transcript_id) REFERENCES transcripts (id) ON DELETE SET NULL;

ALTER TABLE transcript_findings
    ADD CONSTRAINT transcript_findings_null_transcript_superseded
    CHECK (transcript_id IS NOT NULL OR patch_state = 'superseded');

DROP FUNCTION pg_temp.set_patch_states(TEXT, TEXT);

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

-- +goose Down
-- Lossy, like every Down here: 'superseded' did not exist before, so archived
-- findings become 'stale' (the nearest terminal state) with
-- stale_reason='superseded'. transcript_id goes back to NOT NULL only when no
-- row has lost its transcript; otherwise it stays nullable rather than delete
-- or invent data.
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

ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_null_transcript_superseded;
ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_transcript_id_fkey;

UPDATE transcript_findings
   SET patch_state  = 'stale',
       stale_reason = COALESCE(stale_reason, 'superseded')
 WHERE patch_state = 'superseded';

-- +goose StatementBegin
CREATE FUNCTION pg_temp.set_patch_states(add_state TEXT, drop_state TEXT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    states TEXT[];
BEGIN
    SELECT array_agg(m.v[1] ORDER BY m.n)
      INTO states
      FROM pg_constraint c,
           regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''', 'g')
             WITH ORDINALITY AS m(v, n)
     WHERE c.conrelid = 'transcript_findings'::regclass
       AND c.conname = 'transcript_findings_patch_state_valid';
    IF states IS NULL THEN
        RAISE EXCEPTION 'transcript_findings_patch_state_valid not found or has no values';
    END IF;
    states := array_remove(states, drop_state);
    IF add_state IS NOT NULL AND NOT add_state = ANY (states) THEN
        states := states || add_state;
    END IF;
    ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_patch_state_valid;
    EXECUTE format(
        'ALTER TABLE transcript_findings ADD CONSTRAINT transcript_findings_patch_state_valid '
        'CHECK (patch_state IN (%s))',
        (SELECT string_agg(quote_literal(s), ', ' ORDER BY i)
           FROM unnest(states) WITH ORDINALITY AS u(s, i)));
END $$;
-- +goose StatementEnd

SELECT pg_temp.set_patch_states(NULL, 'superseded');
DROP FUNCTION pg_temp.set_patch_states(TEXT, TEXT);

ALTER TABLE transcript_findings DROP COLUMN superseded_at;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM transcript_findings WHERE transcript_id IS NULL) THEN
        ALTER TABLE transcript_findings ALTER COLUMN transcript_id SET NOT NULL;
    ELSE
        RAISE NOTICE 'transcript_findings.transcript_id left nullable: some findings have no transcript';
    END IF;
END $$;
-- +goose StatementEnd
