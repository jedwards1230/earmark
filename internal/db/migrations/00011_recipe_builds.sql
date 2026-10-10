-- 00011_recipe_builds.sql — which earmark builds ran each recipe (CONTRACT
-- §1.9 "Recipe ID format" and "Builds").
--
-- Recipe IDs (format 2, from this release on) no longer hash code_version:
-- a release that changes nothing about a step keeps its recipe instead of
-- minting a new one for every step. The build therefore moves out of the
-- identity and into provenance:
--
--   * recipes.code_version stays, unchanged, and now means the build that
--     FIRST registered the recipe. No rename: every reader keeps working.
--   * recipe_builds records every (recipe, build) pair: one row the first time
--     a build registers a recipe, never updated. db.RegisterRecipe writes it
--     in the same statement as the recipes insert, so it is as cheap as the
--     recipe registration that already happens and covers every step (asr,
--     propose, decide, scan, embed), including the long-running worker's.
--
-- Existing recipe ids are NOT rewritten: findings, finding_events, chunks,
-- chunk_scan, fn_calls and current_recipes reference them. Recipes registered
-- before this migration keep their format-1 ids; the backfill below records
-- the build each was registered by, so recipe_builds is complete from day one.
--
-- A new table plus a column comment. The comment takes SHARE UPDATE EXCLUSIVE
-- on recipes, which does not conflict with the INSERTs writers run.

-- +goose Up
CREATE TABLE recipe_builds (
    recipe_id    TEXT        NOT NULL REFERENCES recipes (recipe_id),
    code_version TEXT        NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT recipe_builds_pkey PRIMARY KEY (recipe_id, code_version)
);

-- "What did build X run": the PK serves the per-recipe direction.
CREATE INDEX recipe_builds_code_version_idx ON recipe_builds (code_version);

INSERT INTO recipe_builds (recipe_id, code_version, first_seen)
SELECT recipe_id, code_version, created_at FROM recipes
ON CONFLICT DO NOTHING;

COMMENT ON COLUMN recipes.code_version IS
    'The build that first registered this recipe (not part of recipe_id since ID format 2; every build that ran it is in recipe_builds).';

-- +goose Down
COMMENT ON COLUMN recipes.code_version IS NULL;
DROP INDEX recipe_builds_code_version_idx;
DROP TABLE recipe_builds;
