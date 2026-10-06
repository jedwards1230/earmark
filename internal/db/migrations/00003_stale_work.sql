-- 00003_stale_work.sql — current recipes and the stale_work view (CONTRACT §1.9).
--
-- current_recipes holds, per step, the recipe earmark would use RIGHT NOW. The
-- ingest process (earmark monitor) upserts it at startup from its config and
-- the model registry (MODELS_FILE). A step with no row has no notion of
-- "current" yet (asr until the runner stamps recipes), so none of its rows
-- are reported stale.
--
-- stale_work lists output rows whose recipe is not equivalent to the current
-- recipe for their step. "Equivalent" compares everything that shapes the
-- output — step_version, models (asked/answered/revision), prompt version and
-- hash, params — and deliberately NOT code_version: a release that does not
-- change a step's output must not mark the whole library stale. A change in
-- earmark's own logic for a step is signalled by bumping its step_version.
-- Rows with no recipe at all (written before stamping, or by an unstamped
-- writer) are stale whenever their step has a current recipe.

-- +goose Up
CREATE TABLE current_recipes (
    step       TEXT        NOT NULL PRIMARY KEY
               CONSTRAINT current_recipes_step_valid CHECK (step IN
                 ('asr','propose','decide','propagate','scan','format','embed')),
    recipe_id  TEXT        NOT NULL REFERENCES recipes (recipe_id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE VIEW stale_work AS
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

-- +goose Down
DROP VIEW stale_work;
DROP TABLE current_recipes;
