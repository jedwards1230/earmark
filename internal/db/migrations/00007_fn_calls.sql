-- 00007_fn_calls.sql — the pure-function call log and cache (CONTRACT §1.9).
--
-- Every call a pure function (internal/fn) makes to a decision model is one
-- row: what was asked (fn, prompt, model alias, the canonical input and its
-- sha256), what answered (model_resolved / model_revision, the output), and
-- what it cost (latency, tokens, cost_usd). recipe_id points at the recipe of
-- the call; error_class is set when the call failed (output NULL).
--
-- The log is also the cache. The partial UNIQUE index below admits at most
-- one successful, non-cached row per (fn, prompt_sha256, model_alias,
-- input_sha256): the writer inserts with ON CONFLICT DO NOTHING, so the same
-- input is never paid for twice, and a later identical call is served from
-- that row. A served call is still logged — as its own row with cache_hit =
-- true and cached_from = the row it was served from — so the log counts every
-- call. Failed rows and cache-hit rows are outside the index and never serve.
--
-- A new table only: no lock is taken on any existing table.

-- +goose Up
CREATE TABLE fn_calls (
    id             BIGSERIAL     PRIMARY KEY,
    fn             TEXT          NOT NULL,
    prompt_version TEXT          NOT NULL,
    prompt_sha256  TEXT          NOT NULL,
    model_alias    TEXT          NOT NULL,
    model_resolved TEXT,
    model_revision TEXT,
    recipe_id      TEXT          REFERENCES recipes (recipe_id),
    input_sha256   TEXT          NOT NULL
                   CONSTRAINT fn_calls_input_sha256_hex CHECK (input_sha256 ~ '^[0-9a-f]{64}$'),
    input          JSONB         NOT NULL,
    output         JSONB,
    error_class    TEXT,
    latency_ms     INTEGER,
    input_tokens   INTEGER,
    output_tokens  INTEGER,
    cost_usd       NUMERIC(14,8),
    cache_hit      BOOLEAN       NOT NULL DEFAULT false,
    cached_from    BIGINT        REFERENCES fn_calls (id),
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now()
);

-- The cache key and the pay-once guard (see the header).
CREATE UNIQUE INDEX fn_calls_cache_key_idx
    ON fn_calls (fn, prompt_sha256, model_alias, input_sha256)
    WHERE error_class IS NULL AND NOT cache_hit;

-- "What did recipe X call, and when" — cost and re-run queries.
CREATE INDEX fn_calls_recipe_id_idx ON fn_calls (recipe_id, created_at);

-- +goose Down
DROP INDEX fn_calls_recipe_id_idx;
DROP INDEX fn_calls_cache_key_idx;
DROP TABLE fn_calls;
