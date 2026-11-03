-- 00018_runner_arbiter_yield.sql — gpu-arbiter's own "step off the GPU"
-- request on runner_control (CONTRACT §1.4 "GPU phase control", Contract C1).
--
-- Until now two writers shared runner_control.phase: `earmark batch` (and
-- operators) for the transcribe/analyze time-share, and gpu-arbiter's gpu_gate
-- (yield = phase 'analyze', updated_by 'gpu-arbiter:yield:<prior>'; resume =
-- restore <prior>). Each could overwrite the other's intent. This splits them:
--
--   arbiter_yield     written ONLY by gpu_gate: true on yield, false on resume.
--                     The runner parks when paused OR phase = 'analyze' OR
--                     arbiter_yield — between jobs and, since this release,
--                     between the windows of a long job.
--   arbiter_yield_at  when gpu_gate last wrote arbiter_yield (yield or resume).
--
-- `phase` is then written only by `earmark batch` and operators. A runner that
-- predates this column never reads it; a runner that reads it treats a missing
-- column as false. The gate change that writes it ships only with a runner pin
-- that reads it (homelab-ansible), so the column is never set with no reader.
--
-- Locks. ADD COLUMN with a constant default rewrites nothing (PostgreSQL 11+),
-- but takes ACCESS EXCLUSIVE on the one-row runner_control briefly; the
-- runner's claim holds that row FOR UPDATE for milliseconds, so fail fast
-- rather than queue behind it (the next boot retries). Idempotent: IF NOT
-- EXISTS / IF EXISTS on every statement.

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS arbiter_yield BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS arbiter_yield_at TIMESTAMPTZ;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_control DROP COLUMN IF EXISTS arbiter_yield_at;
ALTER TABLE runner_control DROP COLUMN IF EXISTS arbiter_yield;
