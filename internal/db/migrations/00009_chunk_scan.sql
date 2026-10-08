-- 00009_chunk_scan.sql — per-chunk quality scan results and the scan arm of
-- stale_work (CONTRACT §1.9 "Chunk scan").
--
-- `earmark scan` asks System One (internal/scan, fn scan_chunk) six questions
-- about one chunk's pristine text — does it need a fix, a 1..5 quality score,
-- is it boilerplate / garbled / dialogue, which issue type — and stores one
-- row per (chunk position, chunk text hash, recipe). The text hash is the
-- sha256 of COALESCE(source_text, text) the model was shown, so a row never
-- describes text the chunk no longer has: a re-chunk changes the hash and the
-- chunk reads as unscanned again. fn_call_id is the fn_calls row that answered
-- (NULL only when a concurrent identical call won the cache row first).
--
-- Rows are insert-only (ON CONFLICT on the unique key DO NOTHING) and are
-- removed only with their transcript (ON DELETE CASCADE): a requeue drops the
-- transcript, its chunks and their scans together. Nothing else references
-- chunk_scan.
--
-- stale_work gains a 'scan' arm (source_table transcript_chunks, recipe_id =
-- the chunk's latest scan's recipe): a chunk is listed when no scan of its
-- CURRENT text was made by a recipe equivalent to the current scan recipe —
-- never scanned, scanned before a re-chunk changed its text, or scanned only
-- by another recipe. Like every arm it reports nothing until current_recipes
-- has a 'scan' row (earmark monitor registers one when AI_ROLES.scan is set).
-- The asr/propose/embed/decide arms are restated verbatim from 00008.
--
-- A new table and a view replacement: no ALTER, no lock on an existing table
-- beyond the view's own catalog row.

-- +goose Up
CREATE TABLE chunk_scan (
    id                 BIGSERIAL   PRIMARY KEY,
    transcript_id      UUID        NOT NULL REFERENCES transcripts (id) ON DELETE CASCADE,
    chunk_index        INTEGER     NOT NULL
                       CONSTRAINT chunk_scan_chunk_index_nonneg CHECK (chunk_index >= 0),
    chunk_text_sha256  TEXT        NOT NULL
                       CONSTRAINT chunk_scan_chunk_text_sha256_hex CHECK (chunk_text_sha256 ~ '^[0-9a-f]{64}$'),
    recipe_id          TEXT        NOT NULL REFERENCES recipes (recipe_id),
    fn_call_id         BIGINT      REFERENCES fn_calls (id),
    p_needs_fix        FLOAT8      NOT NULL,
    quality            FLOAT8      NOT NULL,
    quality_confidence FLOAT8,
    p_boilerplate      FLOAT8      NOT NULL,
    p_garbled          FLOAT8      NOT NULL,
    p_dialogue         FLOAT8      NOT NULL,
    issue_type         TEXT        NOT NULL,
    issue_probs        JSONB       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chunk_scan_quality_range CHECK (quality BETWEEN 1 AND 5),
    CONSTRAINT chunk_scan_probabilities_unit CHECK (
        p_needs_fix   BETWEEN 0 AND 1 AND
        p_boilerplate BETWEEN 0 AND 1 AND
        p_garbled     BETWEEN 0 AND 1 AND
        p_dialogue    BETWEEN 0 AND 1 AND
        (quality_confidence IS NULL OR quality_confidence BETWEEN 0 AND 1)),
    CONSTRAINT chunk_scan_issue_type_valid CHECK (issue_type IN
        ('misheard_proper_noun', 'misheard_word', 'repeated_text', 'number_artifact',
         'homophone', 'dropped_word', 'none')),
    CONSTRAINT chunk_scan_issue_probs_object CHECK (jsonb_typeof(issue_probs) = 'object'),
    CONSTRAINT chunk_scan_unique UNIQUE (transcript_id, chunk_index, chunk_text_sha256, recipe_id)
);

-- "What did scan recipe X produce" — the quality index groups by recipe.
CREATE INDEX chunk_scan_recipe_id_idx ON chunk_scan (recipe_id);

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
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'scan', 'transcript_chunks', c.id, ls.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'scan'
  LEFT JOIN LATERAL (
        SELECT s.recipe_id
          FROM chunk_scan s
         WHERE s.transcript_id = c.transcript_id
           AND s.chunk_index   = c.chunk_index
         ORDER BY s.created_at DESC, s.id DESC
         LIMIT 1) ls ON true
 WHERE NOT EXISTS (
        SELECT 1
          FROM chunk_scan s
          JOIN recipes r ON r.recipe_id = s.recipe_id
         WHERE s.transcript_id = c.transcript_id
           AND s.chunk_index   = c.chunk_index
           AND s.chunk_text_sha256 =
               encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex')
           AND (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
                r.prompt_version, r.prompt_sha256, r.params)
               IS NOT DISTINCT FROM
               (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
                cur.prompt_version, cur.prompt_sha256, cur.params));

-- +goose Down
-- The view goes back to 00008's definition (no scan arm) before the table it
-- reads is dropped. Lossy: every scan result is deleted.
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

DROP INDEX chunk_scan_recipe_id_idx;
DROP TABLE chunk_scan;
