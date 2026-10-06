-- 00002_recipes.sql — provenance recipes (CONTRACT §1.9).
--
-- A recipe is an immutable, content-addressed record of how an output row was
-- made. recipe_id = lowercase hex sha256 of the recipe's canonical JSON (see
-- internal/recipe.Recipe.Canonical); the same configuration always yields the
-- same id. Output rows point at the recipe of the step that produced them:
--
--   transcripts.recipe_id          the asr recipe   (written by the ASR runner)
--   transcript_findings.recipe_id  the propose recipe (the judge; NULL for
--                                  origin='human' rows, which no model made)
--   transcript_chunks.recipe_id    the embed recipe (chunking + embedding)
--
-- All three columns are nullable: a writer that predates recipes keeps working
-- and its rows read as "unstamped".
--
-- Existing rows get ONE legacy recipe per (step, model), with step_version 0
-- and code_version 'legacy-unknown'. They are told apart only by model name:
-- no prompt hash, model revision or runner version was ever recorded, so none
-- is invented.

-- +goose Up
-- Freeze the three stamped tables for the whole migration. The legacy recipe
-- set is computed more than once below (INSERT … SELECT DISTINCT, the id list
-- add_recipe_column reads, its UPDATE); under READ COMMITTED a writer
-- committing in between could add a key the recipes INSERT missed (an FK
-- failure) or, worse, be silently labelled by the constant default. SHARE ROW
-- EXCLUSIVE blocks writers while letting readers (MCP search) continue until
-- each ALTER takes its own ACCESS EXCLUSIVE lock.
LOCK TABLE transcripts, transcript_findings, transcript_chunks IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE recipes (
    recipe_id      TEXT        NOT NULL PRIMARY KEY,
    step           TEXT        NOT NULL
                   CONSTRAINT recipes_step_valid CHECK (step IN
                     ('asr','propose','decide','propagate','scan','format','embed')),
    step_version   INTEGER     NOT NULL,
    code_version   TEXT        NOT NULL,
    model_alias    TEXT,
    model_resolved TEXT,
    model_revision TEXT,
    prompt_version TEXT,
    prompt_sha256  TEXT,
    params         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT recipes_recipe_id_sha256 CHECK (recipe_id ~ '^[0-9a-f]{64}$')
);

CREATE INDEX recipes_step_idx ON recipes (step, created_at);

-- legacy_recipe_id builds the canonical JSON of a legacy recipe by string
-- concatenation — byte-identical to internal/recipe for these fields
-- (TestLegacyCanonical pins the Go side; the integration test checks they
-- agree) — and hashes it. pg_temp: it exists only for this migration's session.
-- +goose StatementBegin
CREATE FUNCTION pg_temp.legacy_recipe_id(p_step TEXT, p_alias TEXT, p_resolved TEXT)
RETURNS TEXT LANGUAGE sql IMMUTABLE AS $$
    SELECT encode(sha256(convert_to(
        '{"step":' || to_json(p_step)::text
        || ',"step_version":0,"code_version":"legacy-unknown"'
        || ',"model_alias":'    || coalesce(to_json(nullif(p_alias, ''))::text, 'null')
        || ',"model_resolved":' || coalesce(to_json(nullif(p_resolved, ''))::text, 'null')
        || ',"model_revision":null,"prompt_version":null,"prompt_sha256":null,"params":{}}',
        'UTF8')), 'hex')
$$;
-- +goose StatementEnd

-- add_recipe_column adds the nullable recipe_id FK to a table and stamps its
-- existing rows. p_ids_sql selects the DISTINCT recipe id per existing row
-- (NULL for a row that must stay unstamped); it is run here rather than passed
-- in as an array, because a caller's subquery on the table would still be
-- open when this ALTERs it (SQLSTATE 55006).
--
-- When every existing row gets the SAME recipe (production: one ASR model, one
-- embedding model), the column is added with that id as a constant DEFAULT and
-- the default is then dropped. Postgres (11+) records a constant default as
-- the column's "missing value" for rows that already exist, so they read the
-- legacy id without the table being rewritten — and without an UPDATE, which
-- on transcript_chunks would re-insert ~40k vectors into the HNSW index (≈80 s
-- measured at production size, holding this migration's ACCESS EXCLUSIVE lock
-- the whole time). Rows written later get NULL, as for a plain ADD COLUMN.
-- Otherwise (several recipes, or rows that must stay NULL): plain ADD COLUMN
-- plus p_update.
-- +goose StatementBegin
CREATE FUNCTION pg_temp.add_recipe_column(p_table REGCLASS, p_ids_sql TEXT, p_update TEXT)
RETURNS VOID LANGUAGE plpgsql AS $$
DECLARE
    p_ids TEXT[];
BEGIN
    EXECUTE 'SELECT ARRAY(' || p_ids_sql || ')' INTO p_ids;
    IF cardinality(p_ids) = 1 AND p_ids[1] IS NOT NULL THEN
        EXECUTE format('ALTER TABLE %s ADD COLUMN recipe_id TEXT DEFAULT %L REFERENCES recipes (recipe_id)',
                       p_table, p_ids[1]);
        EXECUTE format('ALTER TABLE %s ALTER COLUMN recipe_id DROP DEFAULT', p_table);
    ELSE
        EXECUTE format('ALTER TABLE %s ADD COLUMN recipe_id TEXT REFERENCES recipes (recipe_id)', p_table);
        IF cardinality(p_ids) > 0 THEN
            EXECUTE p_update;
        END IF;
    END IF;
END $$;
-- +goose StatementEnd

-- asr: transcripts record only the model name.
INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias)
SELECT DISTINCT pg_temp.legacy_recipe_id('asr', model_name, NULL), 'asr', 0, 'legacy-unknown',
       nullif(model_name, '')
  FROM transcripts;

SELECT pg_temp.add_recipe_column('transcripts',
    $q$SELECT DISTINCT pg_temp.legacy_recipe_id('asr', model_name, NULL) FROM transcripts$q$,
    $u$UPDATE transcripts SET recipe_id = pg_temp.legacy_recipe_id('asr', model_name, NULL)$u$);

-- propose: judge findings record the requested model and, since the LiteLLM
-- era, the model that answered. Human-authored corrections have no recipe.
INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias, model_resolved)
SELECT DISTINCT pg_temp.legacy_recipe_id('propose', model, resolved_model), 'propose', 0,
       'legacy-unknown', nullif(model, ''), nullif(resolved_model, '')
  FROM transcript_findings
 WHERE origin = 'judge';

SELECT pg_temp.add_recipe_column('transcript_findings',
    $q$SELECT DISTINCT CASE WHEN origin = 'judge'
                            THEN pg_temp.legacy_recipe_id('propose', model, resolved_model) END
         FROM transcript_findings$q$,
    $u$UPDATE transcript_findings
          SET recipe_id = pg_temp.legacy_recipe_id('propose', model, resolved_model)
        WHERE origin = 'judge'$u$);

-- embed: chunks record nothing; the embed worker's run_metrics slice records
-- the embedding model per job. That slice is best-effort and written AFTER the
-- chunks commit, so a job can have chunks but no embed_model (a pod stopped in
-- between, or a failed metrics write). When the library was embedded with
-- exactly ONE model, such chunks were embedded with it too — map them to it
-- (keeping the cheap constant-default path) instead of minting a separate
-- "unknown" recipe. With several models recorded the NULLs stay unknown: there
-- is no way to tell which one made them.
CREATE TEMP TABLE legacy_sole_embed_model ON COMMIT DROP AS
SELECT CASE WHEN count(DISTINCT rm.embed_model) = 1 THEN min(rm.embed_model) END AS model
  FROM transcript_chunks c
  JOIN transcripts t ON t.id = c.transcript_id
  JOIN run_metrics rm ON rm.job_id = t.job_id
 WHERE nullif(rm.embed_model, '') IS NOT NULL;

INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias)
SELECT DISTINCT pg_temp.legacy_recipe_id('embed', m.model, NULL), 'embed', 0, 'legacy-unknown', m.model
  FROM (SELECT nullif(coalesce(nullif(rm.embed_model, ''),
                               (SELECT model FROM legacy_sole_embed_model)), '') AS model
          FROM transcript_chunks c
          JOIN transcripts t ON t.id = c.transcript_id
          LEFT JOIN run_metrics rm ON rm.job_id = t.job_id) m;

SELECT pg_temp.add_recipe_column('transcript_chunks',
    $q$SELECT DISTINCT pg_temp.legacy_recipe_id('embed',
             coalesce(nullif(rm.embed_model, ''), (SELECT model FROM legacy_sole_embed_model)), NULL)
         FROM transcript_chunks c
         JOIN transcripts t ON t.id = c.transcript_id
         LEFT JOIN run_metrics rm ON rm.job_id = t.job_id$q$,
    $u$UPDATE transcript_chunks c
          SET recipe_id = pg_temp.legacy_recipe_id('embed',
                coalesce(nullif(rm.embed_model, ''), (SELECT model FROM legacy_sole_embed_model)), NULL)
         FROM transcripts t
         LEFT JOIN run_metrics rm ON rm.job_id = t.job_id
        WHERE t.id = c.transcript_id$u$);

DROP FUNCTION pg_temp.add_recipe_column(REGCLASS, TEXT, TEXT);
DROP FUNCTION pg_temp.legacy_recipe_id(TEXT, TEXT, TEXT);

-- Indexed after the backfill so the UPDATEs do not maintain them row by row.
-- "Which rows did recipe X make" is the re-run / stale-work query.
CREATE INDEX transcripts_recipe_id_idx         ON transcripts (recipe_id);
CREATE INDEX transcript_findings_recipe_id_idx ON transcript_findings (recipe_id);
CREATE INDEX transcript_chunks_recipe_id_idx   ON transcript_chunks (recipe_id);

-- +goose Down
DROP INDEX transcript_chunks_recipe_id_idx;
DROP INDEX transcript_findings_recipe_id_idx;
DROP INDEX transcripts_recipe_id_idx;
ALTER TABLE transcript_chunks   DROP COLUMN recipe_id;
ALTER TABLE transcript_findings DROP COLUMN recipe_id;
ALTER TABLE transcripts         DROP COLUMN recipe_id;
DROP TABLE recipes;
