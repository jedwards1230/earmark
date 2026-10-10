-- 00010_step_runs.sql — live progress of ad-hoc batch steps (CONTRACT §1.10
-- "Step runs").
--
-- `earmark decide` (dry run, --yes, revert), `earmark scan` and `earmark eval`
-- (--sample, a book, --backfill-*) run from `kubectl exec` or a CronJob, not
-- from the long-running pods, so nothing in the dashboard knew they were
-- running. Each such run now writes one row here (internal/runs): inserted
-- when it starts, updated with a throttled heartbeat (at most one write every
-- ~2 s, and at least one every ~30 s) carrying done/total, outcome counters,
-- spend and the current phase, and closed with its final status.
--
-- A run whose process died (a killed `kubectl exec`, an OOM) is never closed:
-- its status stays 'running' and its heartbeat stops. Readers compute
-- "stale" at read time from heartbeat_at; nothing reaps rows.
--
-- step is a short token, not an enum, so a new step needs no migration; mode
-- and status are closed sets. recipe_id is a plain column, not a foreign key:
-- a rung-0 replay computes its recipe id without registering it. The writes
-- happen on their own short statements, never inside a decide write
-- transaction, and a failed write never fails the step.
--
-- A new table only: no ALTER, no lock on any existing table.

-- +goose Up
CREATE TABLE step_runs (
    id           BIGSERIAL   PRIMARY KEY,
    step         TEXT        NOT NULL
                 CONSTRAINT step_runs_step_token CHECK (step ~ '^[a-z][a-z0-9_]{0,31}$'),
    mode         TEXT        NOT NULL
                 CONSTRAINT step_runs_mode_valid CHECK (mode IN ('dry_run', 'write')),
    recipe_id    TEXT,
    model        TEXT,
    args         TEXT,
    host         TEXT,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,
    status       TEXT        NOT NULL DEFAULT 'running'
                 CONSTRAINT step_runs_status_valid CHECK (status IN ('running', 'done', 'failed', 'cancelled')),
    total        BIGINT      CONSTRAINT step_runs_total_nonneg CHECK (total >= 0),
    done         BIGINT      CONSTRAINT step_runs_done_nonneg CHECK (done >= 0),
    counters     JSONB       NOT NULL DEFAULT '{}'::jsonb
                 CONSTRAINT step_runs_counters_object CHECK (jsonb_typeof(counters) = 'object'),
    cost_usd     FLOAT8      CONSTRAINT step_runs_cost_nonneg CHECK (cost_usd >= 0),
    error        TEXT,
    last_message TEXT,
    CONSTRAINT step_runs_finished_iff_closed CHECK ((status = 'running') = (finished_at IS NULL))
);

-- "Running now": the open runs, newest first. Partial, so it stays tiny however
-- long the history grows.
CREATE INDEX step_runs_running_idx ON step_runs (started_at DESC) WHERE status = 'running';
-- "Recent runs": the newest N rows of any status.
CREATE INDEX step_runs_started_at_idx ON step_runs (started_at DESC, id DESC);

-- +goose Down
DROP TABLE step_runs;
