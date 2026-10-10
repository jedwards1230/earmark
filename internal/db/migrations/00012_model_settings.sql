-- 00012_model_settings.sql — runtime per-step model settings (CONTRACT §2.20
-- "Runtime step model settings").
--
-- AI_ENDPOINTS + AI_ROLES + MODELS_FILE decide which endpoint and model each
-- AI step uses; changing one is a deploy. A row here overrides that env
-- default for one step until it is unset: which AI_ENDPOINTS entry to call
-- (endpoint_id — the URL and key stay in the env registry, never here) and
-- which model id to request, plus optional pins with MODELS_FILE semantics.
-- Precedence: a per-run --endpoint/--model flag > this row > the env default.
--
-- model_setting_events is the append-only history: one row per `earmark
-- models set|unset`, written in the same transaction as the change, with the
-- row's value before and after.
--
-- Additive: two new tables, no ALTER, no lock on any existing table, nothing
-- backfilled. An empty model_settings is exactly the env behaviour.

-- +goose Up
CREATE TABLE model_settings (
    step           TEXT        PRIMARY KEY
                   CONSTRAINT model_settings_step_valid CHECK (step IN ('propose', 'decide', 'scan', 'embed')),
    endpoint_id    TEXT        NOT NULL
                   CONSTRAINT model_settings_endpoint_nonempty CHECK (endpoint_id <> ''),
    model          TEXT        NOT NULL
                   CONSTRAINT model_settings_model_nonempty CHECK (model <> ''),
    expected_model TEXT,
    revision       TEXT,
    params         JSONB       NOT NULL DEFAULT '{}'::jsonb
                   CONSTRAINT model_settings_params_object CHECK (jsonb_typeof(params) = 'object'),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by     TEXT        NOT NULL
);

CREATE TABLE model_setting_events (
    id         BIGSERIAL   PRIMARY KEY,
    step       TEXT        NOT NULL,
    action     TEXT        NOT NULL
               CONSTRAINT model_setting_events_action_valid CHECK (action IN ('set', 'unset')),
    old_value  JSONB,      -- the row before (NULL: the env default was in effect)
    new_value  JSONB,      -- the row after (NULL: unset, back to the env default)
    actor      TEXT        NOT NULL,
    note       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT model_setting_events_shape CHECK (
        (action = 'set' AND new_value IS NOT NULL) OR (action = 'unset' AND new_value IS NULL)
    )
);

CREATE INDEX model_setting_events_step_idx ON model_setting_events (step, id DESC);

-- +goose StatementBegin
CREATE FUNCTION model_setting_events_append_only() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'model_setting_events is append-only: % refused', TG_OP
        USING ERRCODE = 'restrict_violation';
END $$;
-- +goose StatementEnd

CREATE TRIGGER model_setting_events_append_only
    BEFORE UPDATE OR DELETE ON model_setting_events
    FOR EACH ROW EXECUTE FUNCTION model_setting_events_append_only();

-- +goose Down
DROP TABLE model_setting_events;
DROP FUNCTION model_setting_events_append_only();
DROP TABLE model_settings;
