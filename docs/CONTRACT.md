# earmark Interface Contract

> **Status: AUTHORITATIVE — treat every value here as law.**
> Downstream agents implementing the Go service, the Python runner, and the
> Kubernetes manifests MUST NOT deviate from these definitions without updating
> this file first and getting explicit sign-off.

---

## 1. DATA CONTRACT

### 1.1 Transcription Job Queue — `transcription_jobs` table

Producer: **Go** (enqueues new files, reads completed results).
Consumer/runner: **Python ASR runner on the GPU/ASR host** (claims jobs, writes results).

```sql
CREATE TABLE transcription_jobs (
    id           UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    file_path    TEXT        NOT NULL,           -- relative to the books NFS root, e.g. "audio-libation/Author/Book/01.m4b"
    checksum     TEXT        NOT NULL,           -- SHA-256 hex of the audio file (dedup key)
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'claimed', 'done', 'failed')),
    claimed_by   TEXT,                           -- runner identity string, e.g. "asr-runner-pid-1234"
    claimed_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,                     -- stamped by a trigger on the transition INTO status='done'; NULL otherwise (incl. old rows pre-dating this column)
    error        TEXT,                           -- last error message when status='failed'
    attempts     INTEGER     NOT NULL DEFAULT 0,

    CONSTRAINT transcription_jobs_checksum_unique  UNIQUE (checksum),
    CONSTRAINT transcription_jobs_file_path_unique UNIQUE (file_path)  -- one job per file
);

CREATE INDEX transcription_jobs_status_idx ON transcription_jobs (status, created_at);
CREATE INDEX transcription_jobs_file_path_idx ON transcription_jobs (file_path);

-- Auto-update updated_at on any row change
CREATE OR REPLACE FUNCTION transcription_jobs_set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER transcription_jobs_updated_at
    BEFORE UPDATE ON transcription_jobs
    FOR EACH ROW EXECUTE FUNCTION transcription_jobs_set_updated_at();

-- completed_at: stamp the exact completion time when a row transitions INTO
-- 'done'. The runner owns the mark-done UPDATE (the Go side never marks jobs
-- done), so a BEFORE UPDATE trigger is the Go-only way to record it. It clears
-- completed_at when a row leaves 'done' (operator requeue), so the column always
-- reflects the current run. Old 'done' rows keep NULL (no backfill — there is no
-- historical completion time to recover); DoneLastHour uses
-- COALESCE(completed_at, updated_at) so it stays correct on those old rows.
CREATE OR REPLACE FUNCTION transcription_jobs_set_completed_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'done' AND (OLD.status IS DISTINCT FROM 'done') THEN
        NEW.completed_at = now();
    ELSIF NEW.status <> 'done' AND OLD.status = 'done' THEN
        NEW.completed_at = NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER transcription_jobs_completed_at
    BEFORE UPDATE ON transcription_jobs
    FOR EACH ROW EXECUTE FUNCTION transcription_jobs_set_completed_at();
```

### 1.2 Transcript Storage — `transcripts` table

Results written by the Python runner are stored in a dedicated `transcripts`
table with a `JSONB` column for the full structured output. This allows the Go
side to query the structured data with Postgres JSON operators without a
separate document store.

```sql
CREATE TABLE transcripts (
    id                  UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    job_id              UUID        NOT NULL REFERENCES transcription_jobs(id) ON DELETE CASCADE,
    file_path           TEXT        NOT NULL,   -- denormalized from job for query convenience
    checksum            TEXT        NOT NULL,   -- denormalized from job
    language            TEXT        NOT NULL,   -- ISO 639-1, e.g. "en"
    duration_seconds    FLOAT8      NOT NULL,
    speaker_count       INTEGER,                -- NULL when diarization disabled
    segments            JSONB       NOT NULL,   -- array of Segment objects (schema below)
    raw_text            TEXT        NOT NULL,   -- full transcript concatenated, for FTS
    model_name          TEXT        NOT NULL,   -- ASR model used, e.g. "nvidia/parakeet-tdt-0.6b-v3"
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transcripts_job_id_unique UNIQUE (job_id)
);

CREATE INDEX transcripts_file_path_idx ON transcripts (file_path);
-- Full-text search on raw_text using pg_trgm (enable via: CREATE EXTENSION IF NOT EXISTS pg_trgm)
CREATE INDEX transcripts_raw_text_trgm_idx ON transcripts USING gin (raw_text gin_trgm_ops);
```

#### 1.2.1 Segment JSON Schema (the `segments` JSONB column)

The Python runner writes and the Go side reads this exact shape. Every field
is required unless marked optional.

```jsonc
// transcripts.segments — top-level is a JSON array
[
  {
    "id": 0,                          // integer segment index
    "start": 12.34,                   // float, seconds from start of audio
    "end":   15.78,                   // float, seconds from start of audio
    "text":  "Hello, welcome back.",  // segment text (may have leading space — preserve as-is)
    "speaker": "SPEAKER_00",          // string | null — null when diarization unavailable
    "words": [                        // array of word-level timestamps
      {
        "word":        "Hello,",      // string — the word token (may include punctuation)
        "start":       12.34,         // float, seconds
        "end":         12.71,         // float, seconds
        "score":       0.983,         // float 0–1, confidence; null if unavailable
        "speaker":     "SPEAKER_00"   // string | null — speaker at word level
      }
      // ... more words
    ]
  }
  // ... more segments
]
```

Rules:
- `speaker` at both segment and word level is `null` when diarization is
  disabled or the runner flag `ASR_DIARIZE=false` is set (the default).
- `words` array is always present (never `null`); it may be empty if the ASR
  model emits no word-level timestamps for a segment.
- `score` in word objects is `null` when the alignment model does not produce a
  confidence value.
- All timestamps are float64 seconds, not milliseconds.
- **Segment granularity is sentence-sized, derived from word timestamps.** NeMo
  Parakeet-TDT returns exactly one "segment" per `transcribe()` call (one per
  ~600 s transcription window — a 60 s clip still yields a single segment), so
  its segment-level boundaries are NOT used. The runner instead derives segments
  from the per-word timestamps: a new segment starts on an inter-word silence
  gap > `ASR_SEGMENT_GAP_SECONDS` (default 0.6 s ≈ a sentence boundary), a
  speaker change, or a hard cap of `ASR_SEGMENT_MAX_SECONDS` (30 s) /
  `ASR_SEGMENT_MAX_WORDS` (80) so no segment exceeds a few tens of seconds even
  in gap-less speech. Each segment always retains its `words[]`, so per-second
  precision is available regardless of segment size. Transcripts produced before
  this scheme (one ~600 s segment per window) can be upgraded in place by
  `runner/resegment.py` — it re-derives segments from the already-stored words
  with no re-transcription — followed by `earmark requeue --reembed "" --yes` to
  rebuild chunks. Before segmenting, the runner normalizes the word stream
  (sorts by start time, drops overlapping-window duplicate boundary words), so a
  segment is always monotonic (`end >= start`) even though long-audio ASR
  emits overlapping inference windows whose stored `words[]` are not globally
  ordered.

### 1.3 Claim Semantics

#### Atomic claim (Python runner, on startup of each claim cycle)

```sql
-- Claim up to one pending job atomically
UPDATE transcription_jobs
SET    status     = 'claimed',
       claimed_by = $1,          -- runner identity string
       claimed_at = now(),
       attempts   = attempts + 1
WHERE  id = (
    SELECT id
    FROM   transcription_jobs
    WHERE  status = 'pending'
       AND (attempts < 3)        -- hard retry cap
    ORDER  BY created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, file_path, checksum;
```

If no rows are returned, the runner sleeps and retries (poll interval:
`RUNNER_POLL_INTERVAL_SECONDS`, default `30`).

#### Heartbeat

The runner MUST UPDATE `updated_at` on the claimed row every
`RUNNER_HEARTBEAT_SECONDS` (default `60`) while transcription is in progress:

```sql
UPDATE transcription_jobs
SET    updated_at = now()
WHERE  id = $1 AND status = 'claimed';
```

#### Stale-claim recovery (Go service, background goroutine)

The Go service reclaims jobs stuck in `claimed` state for longer than
`STALE_JOB_TIMEOUT` (default `30m`) by resetting them to `pending`:

```sql
UPDATE transcription_jobs
SET    status     = 'pending',
       claimed_by = NULL,
       claimed_at = NULL
WHERE  status     = 'claimed'
  AND  updated_at < now() - INTERVAL '30 minutes'
  AND  attempts   < 3;
```

Jobs where `attempts >= 3` are set to `failed` instead:

```sql
UPDATE transcription_jobs
SET    status = 'failed',
       error  = 'max attempts reached'
WHERE  status     = 'claimed'
  AND  updated_at < now() - INTERVAL '30 minutes'
  AND  attempts   >= 3;
```

#### Mark done (Python runner, after successful transcript write)

```sql
-- Write transcript first (within the same transaction)
INSERT INTO transcripts (job_id, file_path, checksum, language, duration_seconds,
                         speaker_count, segments, raw_text, model_name)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9);

-- Then mark the job done
UPDATE transcription_jobs
SET    status = 'done',
       error  = NULL
WHERE  id     = $1;
```

Both writes MUST be in a single transaction. If the INSERT fails, the job
remains `claimed` and the heartbeat will expire it back to `pending`.

#### Mark failed (Python runner, on unrecoverable error)

```sql
UPDATE transcription_jobs
SET    status = 'failed',
       error  = $2          -- truncated error message, max 2000 chars
WHERE  id     = $1;
```

#### GPU/ASR host busy gate

The runner honors a `RUNNER_BUSY_FLAG_PATH` environment variable (default
`/tmp/earmark-busy`). When this file exists, the runner skips claiming
new jobs (finishes any in-flight job first, then pauses). An external process
writes this file when the host should not accept new jobs and removes it when
the host is available again. The runner checks the flag at the top of each poll
cycle before issuing the claim UPDATE.

The busy flag is **host-local and ephemeral** (tmpfs): it is the right channel
for a transient, host-side GPU-contention gate (e.g. gaming), but it does NOT
survive a host reboot and is invisible to the in-cluster Go service. For a
durable, operator-controlled pause use the pause-control table below.

#### Pause + bounded-run control — `runner_control` table

A singleton row gates the runner's claims. The Go service (dashboard + control
API) writes it; the runner reads and decrements it. Because it lives in the
shared database it is durable across reboots and visible to both the off-host
runner and the in-cluster service (unlike the busy flag).

```sql
CREATE TABLE IF NOT EXISTS runner_control (
    id         INTEGER     NOT NULL PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    paused     BOOLEAN     NOT NULL DEFAULT false,
    run_limit  INTEGER         CHECK (run_limit IS NULL OR run_limit >= 0),
    phase      TEXT            CHECK (phase IS NULL OR phase IN ('idle','transcribe','analyze')),
    runner_heartbeat_at TIMESTAMPTZ,  -- liveness stamp; runner writes it every poll cycle
    runner_version         TEXT,         -- tag the runner reports it is RUNNING (stamped on heartbeat)
    desired_runner_version TEXT,         -- tag the dashboard asks it to run (self-update target)
    runner_update_state    TEXT          CHECK (runner_update_state IS NULL OR runner_update_state IN
                                            ('idle','requested','updating','success','failed')),
    runner_update_error    TEXT,         -- failure detail for the last update attempt
    runner_update_at       TIMESTAMPTZ,  -- when the update state last changed
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT
);
-- seeded once by the Go service on init:
INSERT INTO runner_control (id, paused) VALUES (1, false) ON CONFLICT (id) DO NOTHING;
```

- **`paused`** — true means decline all new claims.
- **`run_limit`** — `NULL` means unlimited (normal operation); a non-negative
  integer is a **bounded run** with that many claims remaining (e.g. `1` for a
  single-job smoke test).
- **`runner_heartbeat_at`** — liveness stamp the runner writes to `now()` on
  **every** poll cycle, whether it is working, idle (empty queue), or
  paused/parked. Unlike `transcription_jobs.updated_at` (which only moves while a
  job is *claimed*), it stays fresh on a drained queue, so it distinguishes
  "alive but idle" from "down" (§1.7). The Go collector exposes it as
  `earmark_runner_alive_seconds` (§2.16). `NULL` until the first stamp; writes
  are best-effort and never block the claim path.
- **`runner_version` / `desired_runner_version`** — the runner self-update
  channel (§2.12). `runner_version` is the earmark git tag the runner reports it
  is **running** (stamped alongside the heartbeat; resolved from a
  `/opt/asr-runner/VERSION` file written by a self-update, else the
  `RUNNER_VERSION` env the ansible role renders from `asr_runner_source_ref`).
  `desired_runner_version` is the tag the dashboard button asks it to run. Skew =
  `desired_runner_version` set and ≠ `runner_version`. The runner reads the
  desired version every poll cycle and, **between jobs** (no claimed job) and not
  in `analyze` phase, fetches `runner/runner.py` at that tag from
  `raw.githubusercontent.com` (mirroring the ansible `get_url`), runs it with
  `--self-check` (parse/import, no model load), atomically swaps it in (keeping a
  `.backup`), writes the `VERSION` file, and re-execs. Updating while `paused` is
  allowed (idle is the safe time); the model reload on the re-exec'd startup goes
  through the normal park gate, so it yields to a game/eval.
- **`runner_update_state`** — the update state machine, written by the runner
  (the dashboard only sets `desired_runner_version` + `'requested'`):
  `requested` (operator asked) → `updating` (runner fetched/swapping) →
  `success` (the re-exec'd new version stamped its version) | `failed`
  (`runner_update_error` carries why; the running version is unchanged — the swap
  is atomic and self-check-gated, so a broken candidate never replaces a good
  one). No retry until the button is pressed again (which resets to `requested`).
  `runner_update_at` timestamps the last transition. **Precondition:** the
  runner's install dir must be writable by the runner's user (the swap renames
  `runner.py.new` into it); a non-writable dir fails fast with an actionable
  `failed` error before any fetch, rather than a cryptic mid-swap `EACCES`.
- **`phase`** — the **batched two-phase pipeline** selector. `NULL` or `'idle'`
  is normal operation (both the ASR runner and the Go embed worker run freely —
  the default, fully backward-compatible); `'transcribe'` is the ASR-only phase
  (the embed worker idles so eval/embed don't contend for the GPU the ASR runner
  owns); `'analyze'` is the embed-only phase (the ASR runner is paused/off-GPU
  and the embed worker drains the just-transcribed transcripts). The two valid
  non-default values let a single GPU host a large ASR model **or** a large eval
  judge, never both at once. The column is gated by a CHECK to the closed set
  `{idle, transcribe, analyze}`; `NULL`/`idle` are equivalent and `idle` is
  stored as `NULL`. `paused`/`run_limit` and `phase` are **independent axes**:
  `paused` is a **global stop** (it gates the ASR runner AND the Go embed worker,
  regardless of phase), while `phase` is the batched selector used during normal
  (non-paused) operation. So a paused pipeline in `transcribe` phase claims
  nothing AND idles the worker — and a paused pipeline in any phase idles the
  worker. The `earmark batch` coordinator orchestrates via `run_limit` + `phase`
  and **never** sets `paused`, so batched transcribe/analyze are unaffected by
  the pause gate.

**Phase semantics (worker gate):** the Go embed worker checks the global
`paused` flag, then `phase`, at the top of each poll cycle. When `paused` is set
(the dashboard Pause button — a global stop) it SKIPS that cycle no matter the
phase, so the dashboard PAUSED banner is truthful (embeddings actually stop).
Otherwise, when `phase = 'transcribe'` it SKIPS that cycle (idles for the poll
interval, processes nothing) so the ASR runner has the GPU to itself; for
`'idle'`/`'analyze'`/`NULL` it processes completed transcripts as usual. A
pause-read error defaults to NOT paused (process) and a phase-read error
defaults to `'idle'` (process); both are logged — a DB hiccup must never wedge
the worker. A missing row is treated as not-paused / `'idle'`. With `paused`
false and `phase` left `NULL` the pipeline behaves exactly as before (both
stages run concurrently); the **`earmark batch` coordinator** (below) is what
flips `phase` to orchestrate transcribe- and analyze-batches, and it never sets
`paused`.

**The `earmark batch` coordinator.** A standalone, hardware-agnostic command
that runs the pipeline in batches so the ASR model and the eval-judge LLM
time-share one GPU. It only flips `phase` + `run_limit` and reads queue status —
it never touches CUDA. Per batch, repeated until no pending jobs remain,
`--max-batches` is reached, or it is interrupted:

1. **Yield to games.** If `GPU_ARBITER_URL` (§2.4) is set and gpu-arbiter
   reports the GPU is busy with a game — `state == "gaming"` (a game holds the
   GPU) OR `state == "evicting"` (a game just launched and the arbiter is
   tearing down GPU tenants) — wait (poll every `--arbiter-poll`, default 15s)
   until it is neither, before doing GPU work. The arbiter read is a **read-only
   `GET /status`** — the coordinator never `POST`s to it. An unset or unreachable
   arbiter is logged and the coordinator proceeds (degrades gracefully — arbiter
   absence never wedges it).

   **gpu-arbiter CLI delegation (optional, gpu-arbiter hardening plan #18).**
   When `GPU_ARBITER_URL` is set AND a [gpu-arbiter](https://github.com/jedwards1230/gpu-arbiter)
   binary (≥ v0.10.0, which ships `wait`/`status -q`) resolves — checked in
   order: `--arbiter-wait-cmd`, `$ARBITER_WAIT_CMD`, then `gpu-arbiter` on
   `PATH` — the coordinator shells out to the CLI (`gpu-arbiter status -q
   --url <base>` for the busy check, `gpu-arbiter wait --for available
   --timeout <arbiter-poll-seconds> --url <base>` for the actual wait) instead
   of its own hand-rolled HTTP GET + JSON decode. This is a **strict
   optimization, never a requirement**: `earmark batch` does not depend on the
   gpu-arbiter binary being installed, and the container image that ships the
   `earmark` binary does not bundle it. If no binary resolves (e.g. running
   inside the earmark container, or on any host without gpu-arbiter
   installed), the coordinator falls back to the built-in HTTP poll loop
   described above, unchanged. Both paths are read-only GETs against
   gpu-arbiter's `/status` endpoint and preserve the same observable contract
   — degrade-gracefully on an unreachable/unconfigured arbiter, never wedge,
   honor `ctx` cancellation (SIGINT/SIGTERM kills the delegated subprocess via
   `exec.CommandContext`). The chosen path (delegated CLI vs HTTP poll loop) is
   logged once at coordinator startup.
2. **Phase A — transcribe.** Set `phase='transcribe'` and `run_limit=N`
   (`--batch-size`, default 10). The runner claims up to N jobs then stops; the
   embed worker idles. Wait until nothing is `claimed` AND the run budget is
   exhausted (`run_limit==0`) or no `pending` jobs remain.
3. **Phase B — analyze.** Set `phase='analyze'`. The runner parks its model
   (freeing the GPU) and the embed worker drains the just-transcribed
   transcripts via the two-pass gated flow (see `EVAL_GATES_EMBED` below):
   - **Ungated** (`EVAL_GATES_EMBED=false`, default): embed worker runs the
     combined chunk→eval→embed path as before; Phase B completes when the embed
     backlog (`EmbedBacklog`) is 0.
   - **Gated** (`EVAL_GATES_EMBED=true`): the worker runs an eval pass first
     (select done, not-eval'd, not-embedded → judge → write `eval_finished_at`),
     then an embed pass (select done, eval'd, not-embedded → embed). Phase B
     completes when **both** the eval backlog (`EvalBacklog`: done,
     not-eval'd, not-embedded count) AND the embed backlog are 0. Both passes'
     selections are **bounded by `EMBED_BATCH_SIZE` (§2.4, default 32) with
     `ORDER BY t.created_at ASC LIMIT $1`** so the worker never loads an
     unbounded transcript backlog (and its `segments` JSONB) into memory at once;
     it drains across cycles, oldest-first, looping immediately on a full batch.

**Robustness contract:** the coordinator **always restores `phase='idle'` and
sets `run_limit=0`** on exit — normal completion, error, AND `SIGINT`/`SIGTERM` —
leaving the runner idle-but-armed (claims nothing until the next batch sets a
budget) and never gets stuck mid-phase. It sets `run_limit=0`, **not** `NULL`:
`NULL` means *unlimited*, so clearing it would let the runner drain the whole
backlog the moment it isn't paused — the opposite of the batched model.
It is **DB-driven and resumable**: it holds no critical state in memory and
derives everything (current phase, job counts, backlog) from the DB. On restart
it reconciles — if it finds `phase='analyze'`, it finishes Phase B before
starting a new Phase A. If a game starts mid-batch, gpu-arbiter stops the runner
and judge; the coordinator's per-batch yield-check handles re-entry and the
existing stale-job recovery (§1.3) reclaims interrupted jobs.

**Gate (the load-bearing rule):** the runner claims a job only when

```
NOT paused  AND  (run_limit IS NULL OR run_limit > 0)
```

and, in the **same transaction as the claim**, decrements `run_limit` by 1 when
it is non-NULL — so exactly `N` jobs are claimed even if a poll races the write.
The decrement is conditional on a row actually being claimed (an empty-queue poll
must NOT decrement). When `run_limit` reaches 0 the runner declines further
claims (it does **not** also set `paused`; the two axes are independent). The
decrement happens at **claim** time, so a job that is claimed and then fails still
consumes one of the `N`.

A missing row (the Go service not yet initialized) MUST be treated as
**not paused / unlimited** so the runner degrades safely. The gate governs new
claims only; an in-flight transcription always runs to completion.

The Go service exposes these write shapes (see §2.7 Control API):

| Operation | `paused` | `run_limit` |
|-----------|----------|-------------|
| pause     | `true`   | unchanged |
| resume    | `false`  | `NULL` (clears any bound) |
| run N     | `false`  | `N` |
| clear run | unchanged | `NULL` |

resume/run set `run_limit` **before** flipping `paused=false`, so the runner is
never momentarily unbounded.

#### GPU phase control + self-parking — `runner_control.phase`

To let a single GPU host **time-share** between the ASR model and a (future)
eval-judge LLM, `runner_control` carries an optional `phase` column. It is
**additive** — the runner reads it defensively, so a deployment whose DB does not
yet have the column (or row) behaves exactly as before. The runner never creates
or writes `phase`; a future coordinator/Go service owns those writes.

```sql
-- additive column (not yet created by the runner; a separate migration adds it):
ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS phase TEXT;
```

Phase values **as the runner interprets them**:

| `phase` | Meaning to the runner | GPU model |
|---------|-----------------------|-----------|
| `NULL` (absent column/row too) | normal, continuous operation (today's behavior) | **on GPU** |
| `'idle'` | no special directive | **on GPU** |
| `'transcribe'` | ASR phase — the runner may use the GPU | **on GPU** |
| `'analyze'` | judge phase — the runner must step off the GPU | **parked to CPU** |
| any other value | unrecognised → fail safe (treat as "do not use the GPU") | **parked to CPU** |

**Self-parking rule.** Between jobs (never mid-transcription) the runner decides:

```
park the model OFF the GPU  iff  paused  OR  phase NOT IN (NULL, 'idle', 'transcribe')
```

When parking, the runner moves its model to host RAM (`asr_model.cpu()` +
`torch.cuda.empty_cache()`) — parking weights in RAM in seconds, **not** a
from-disk reload — so the freed VRAM is returned to the driver for the judge.
When it becomes active again (not paused **and** phase in NULL/`idle`/`transcribe`)
it restores the model (`asr_model.cuda()`). The transition fires **only on a state
change** (the runner tracks its parked/loaded state), so a steady-state poll does
no redundant `.cpu()`/`.cuda()`. While parked, the runner skips claiming entirely.
A restart while parked simply loads fresh on startup (no special recovery). A
missing `phase` column/row, or any read error, degrades to `'idle'` (model stays
on the GPU).

This is independent of `paused`/`run_limit`: `paused=true` always parks (and
declines claims); `phase` adds the `'analyze'` axis for GPU hand-off without
pausing the broader pipeline semantics.

#### Operator requeue (out-of-band, `earmark requeue`)

In addition to the runner/service transitions above, an operator may move a job
**back** to `pending` to redo it. This is the only sanctioned way a `done` or
`failed` job returns to `pending`. It is always operator-initiated (never the
runner) and is transactional:

- **Re-transcribe**: delete the job's row in `transcripts` (which cascades to
  `transcript_chunks`), delete the job's `run_metrics` row, and `UPDATE … SET
  status='pending', attempts=0, error=NULL, claimed_by=NULL, claimed_at=NULL`.
  All in **one transaction**. The runner then re-processes it like any pending
  job. The `run_metrics` delete is required because requeue *updates* the job row
  rather than deleting it, so the `run_metrics → transcription_jobs ON DELETE
  CASCADE` never fires; left in place, the row would describe the now-deleted
  transcript (orphaned telemetry for the prior run).
- **Re-embed only**: delete the matching rows in `transcript_chunks` and leave
  `transcripts`/`transcription_jobs` untouched. The Go worker re-embeds on its
  next poll (it selects transcripts with no chunks). Use after an embedding
  model or `CHUNK_SIZE` change — no re-transcription.

### 1.5 Per-run observability — `run_metrics` table

One row per job capturing telemetry across the whole run (probe → transcribe →
embed). It is **additive**: nothing in §1.1–§1.4 or §3 depends on it, and a
missing row never blocks the pipeline. Four independent writers each UPSERT
**only their own slice** of columns keyed on `job_id`, so they never clobber
each other (the Go monitor, the Python ASR runner, the Go embed worker, and the
Go eval layer):

```sql
CREATE TABLE IF NOT EXISTS run_metrics (
  job_id UUID PRIMARY KEY REFERENCES transcription_jobs(id) ON DELETE CASCADE,
  audio_bytes BIGINT, audio_channels INT, audio_sample_rate INT, audio_codec TEXT, audio_format TEXT,
  transcribe_started_at TIMESTAMPTZ, transcribe_finished_at TIMESTAMPTZ, asr_model TEXT, compute_type TEXT,
  runner_host TEXT, chunked BOOLEAN, n_windows INT, char_count INT, word_count INT, segment_count INT,
  embed_started_at TIMESTAMPTZ, embed_finished_at TIMESTAMPTZ, embed_model TEXT, embed_chunk_count INT,
  embed_prompt_tokens INT, embed_total_tokens INT,
  -- Eval slice (the LLM judge — a fourth column-selective writer). All nullable,
  -- best-effort. eval_finished_at IS NOT NULL is the per-job eval-completion marker.
  eval_started_at TIMESTAMPTZ, eval_finished_at TIMESTAMPTZ, eval_model TEXT,
  eval_chunks INT, eval_skipped INT, eval_findings INT,
  -- ASR backend descriptor (§2.13). All nullable, runner-owned, best-effort.
  asr_family TEXT, asr_runtime TEXT,
  caps_applied JSONB, caps_requested JSONB, caps_skipped_reason JSONB,
  mean_word_confidence FLOAT8,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**All columns are nullable** (except the PK and `created_at`/`updated_at`). Each
writer UPSERTs via `INSERT … ON CONFLICT (job_id) DO UPDATE SET <its cols>=EXCLUDED…, updated_at=now()`.

The six ASR backend-descriptor columns (`asr_family`, `asr_runtime`,
`caps_applied`, `caps_requested`, `caps_skipped_reason`, `mean_word_confidence`)
are **additive and nullable** — added via `ADD COLUMN IF NOT EXISTS` in the Go
service's schema-init, so an existing prod table gains them with no migration
ceremony. They are written **only** by the Python ASR runner, and only as a
**SHOULD** (see below): the existing single NeMo runner that writes none of them
stays fully contract-compliant — the columns simply stay NULL and the dashboard
renders them as "unknown", never an error. **This is the back-compat guarantee:
no breaking change.** Their shapes and the capability vocabulary are defined in
§2.13.

#### Column ownership (which writer writes which columns)

| Writer | When | Columns it writes |
|--------|------|-------------------|
| **Go monitor** | at enqueue (file size from `os.Stat`) | `audio_bytes` |
| **Python ASR runner** | after transcribing | `audio_channels`, `audio_sample_rate`, `audio_codec`, `audio_format`, `transcribe_started_at`, `transcribe_finished_at`, `asr_model`, `compute_type`, `runner_host`, `chunked`, `n_windows`, `char_count`, `word_count`, `segment_count`; **SHOULD also** `asr_family`, `asr_runtime`, `caps_applied`, `caps_requested`, `caps_skipped_reason`, `mean_word_confidence` (the §2.13 backend descriptor) |
| **Go embed worker** | after `transcript_chunks` insert | `embed_started_at`, `embed_finished_at`, `embed_model`, `embed_chunk_count`, `embed_prompt_tokens`, `embed_total_tokens` |
| **Go eval layer** | after the in-pipeline judge runs over a transcript's chunks (`EVAL_IN_PIPELINE`) | `eval_started_at`, `eval_finished_at`, `eval_model`, `eval_chunks`, `eval_skipped`, `eval_findings` |

**Eval slice + completion marker.** `eval_finished_at IS NOT NULL` is the
**per-job eval-completion marker** — a job has been judged iff its `run_metrics`
row has a non-NULL `eval_finished_at`. Eval coverage is thus a real ratio
(`COUNT(eval_finished_at) / COUNT(done jobs)`) rather than a findings-row count
(a clean job has 0 findings but is still "evaluated"). `eval_model` is the judge
model id; `eval_chunks`/`eval_skipped`/`eval_findings` are the run's
`ChunksEvaluated`/`ChunksSkipped`/`FindingsFound`. The eval slice is written
**only by the in-pipeline path** (`EVAL_IN_PIPELINE`), where the chunk set maps
cleanly to one job (`job_id`). The **standalone** `earmark eval` / `/actions/eval*`
paths evaluate a whole book (many jobs) or a library-wide sample, so they do
**not** write the per-job `run_metrics` eval slice (the mapping to a single job
is ambiguous); they emit a `pipeline_events` `stage='eval'` row instead (§1.7).

**`eval_finished_at` as the embed gate (`EVAL_GATES_EMBED=true`).** When
`EVAL_GATES_EMBED` is enabled, `eval_finished_at IS NOT NULL` is **the latch
that allows a transcript to be embedded**: the embed pass selects only transcripts
whose `run_metrics.eval_finished_at IS NOT NULL` (eval'd) AND have no chunks (not
yet embedded). Under this gate the invariant `embedded ⟹ eval'd` holds: a
transcript is never searchable until it has been judged. Column ownership is
preserved — the eval pass writes `eval_finished_at` (its column), the embed pass
writes the embed slice, the runner writes the transcribe slice; no clobber.

**Deterministic chunk UUIDs (gated mode).** When `EVAL_GATES_EMBED=true`, chunk
UUIDs are derived as **UUIDv5 over the namespace `earmark-chunk-v1` (a fixed
UUID) and the string `<transcript_id>/<chunk_index>`**. This is the correctness
invariant: the eval pass chunks the transcript to judge it, and the embed pass
re-chunks the same transcript to embed it. Because both passes use the same
deterministic function over the same inputs (segments + `CHUNK_SIZE`), they
produce identical chunk sets and identical UUIDs — findings written in the eval
pass (referencing those UUIDs) correctly point to the chunk rows the embed pass
inserts. Without determinism, the two passes would generate different random UUIDs
and findings would be orphaned (referencing chunk IDs that were never inserted).
UUIDv5 is idempotent under retry: re-running either pass regenerates the same
IDs. The `transcript_chunks` table's `UNIQUE(transcript_id, chunk_index)`
constraint catches any divergence at insert time (it would be an impossible
double-insert of the same chunk, not a silent mismatch).

**Backfill (`earmark eval --backfill-unevaluated`).** When transitioning an
existing deployment from ungated to gated (`EVAL_GATES_EMBED=true`), the
~embedded-but-never-judged corpus needs a one-time backfill. The command selects
done jobs whose `eval_finished_at IS NULL` **regardless of embed state** (so it
covers the ~790 already-embedded tracks), judges them, and writes
`eval_finished_at`. It is read-only over transcripts: it only INSERTs findings and
UPSERTs the eval `run_metrics` slice — it does NOT re-embed or touch
`transcript_chunks`. After the backfill, `embedded ⟹ eval'd` holds for the
existing corpus, matching the invariant the gate enforces going forward. The
command respects `--sample` / batch bounds to control cost. See `earmark eval
--backfill-unevaluated --write` (§2.15 and `earmark eval --help`).

The new runner-owned columns join the runner's existing single UPSERT slice — no
clobber risk, since they are columns no other writer touches. Populating them is
**SHOULD, not MUST**: a runner that omits them is still compliant (the columns
stay NULL). When the runner declines a *requested* capability it
SHOULD record `applied=false` for that key in `caps_applied` and a short
human-readable reason under the same key in `caps_skipped_reason` — that
honest-degradation record is the entire point of the backend descriptor (e.g.
NeMo Parakeet-TDT seeing a bias list but declining boosting because TDT word
timestamps break under it). `mean_word_confidence` is written only when the model
emits per-word scores; NULL otherwise.

Rules:
- Every write is **best-effort** — a `run_metrics` failure MUST NOT fail the
  underlying enqueue/transcribe/embed. Writers log and continue.
- The Go service creates the table in its schema-init transaction, so it exists
  before the runner ever writes; the runner's UPSERT is still defensive (treats a
  missing table/row as a no-op-equivalent best-effort write).
- **Token mapping (embed worker):** `embed_total_tokens` is the **authoritative**
  count — the Go service tokenizes the embedded chunk texts locally with the same
  tokenizer the chunker uses, because Ollama does not reliably populate `usage`
  for embeddings. It is written **only when every chunk tokenizes successfully**;
  if any chunk fails to tokenize the column is left **NULL = unknown** (a partial
  sum is never stored, since it would be indistinguishable from a complete count),
  and the worker logs a warning naming the failed-chunk count. Consumers must
  treat NULL as "unknown", not zero. `embed_prompt_tokens` stores the
  provider-reported `usage.prompt_tokens` only when non-zero, and is left NULL
  otherwise.
- `chunked` / `n_windows` describe the runner's chunked-vs-single-pass inference
  (driven by `ASR_CHUNK_THRESHOLD_SECONDS`, §2.4), not the Go embed chunking.

### 1.6 Per-book enrichment — `book_metadata` table

One row per **book directory** (`book_dir = filepath.Dir(file_path)` of any
track under the book). It is **additive** — nothing in §1.1–§1.5 or §3 depends
on it, and a missing row never blocks the pipeline. Writer: **Go monitor**, at
enqueue time via `MetadataProvider.Lookup`. Readers: the Python ASR runner reads
`bias_terms` to drive NeMo word-boosting; the **Go MCP read path** reads
`chapters` and `series` (see below).

```sql
CREATE TABLE IF NOT EXISTS book_metadata (
  book_dir   TEXT        NOT NULL PRIMARY KEY,
  title      TEXT,
  author     TEXT,
  narrator   TEXT,
  series     TEXT,
  asin       TEXT,
  chapters   JSONB,
  bias_terms TEXT[],
  source     TEXT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### Column ownership

| Writer | When | Columns it writes |
|--------|------|-------------------|
| **Go monitor** | at every enqueue (via `db.UpsertBookMetadata`) | `title`, `author`, `bias_terms`, `source` |
| **Go monitor — ABS path** | when METADATA_PROVIDER includes ABS | `narrator`, `series`, `asin`, `chapters` |

#### Column readers

| Reader | When | Columns it reads |
|--------|------|------------------|
| **Python ASR runner** | before each transcription | `bias_terms` (NeMo word-boosting) |
| **Go MCP layer** | per search-result book (`db.loadBookContext`, cached per `book_dir`) | `chapters`, `series` |
| **Go MCP layer** | `list_books` (LEFT JOIN on `book_dir`) | `series` |

`chapters` and `series` are read in a **single** `SELECT chapters, series FROM
book_metadata WHERE book_dir = $1` per distinct book directory in a result set,
cached for the rest of that set — never per row.

**`series` format:** a comma-joined list of `Name #Sequence` entries, because a
book can belong to several series — e.g. `"Dune #2, The Dune Sequence #13"`. The
`#Sequence` part is optional (a membership with no declared position), and the
sequence is **not** necessarily an integer: novellas are commonly `#1.5`, and
non-numeric positions are legal. `metaprovider.ParseSeries` splits this into
`[]SeriesRef{{Name, Sequence}}` with **Sequence typed as a string** so no real
value is truncated or rejected. A series name containing a comma is
indistinguishable from the separator — an accepted limitation, since ABS joins
memberships with `", "`.

`bias_terms` is derived by `metaprovider.DeriveBiasTerms(meta)` inside
`db.UpsertBookMetadata` at every call — both at enqueue time (monitor) and
during a metadata backfill (`earmark backfill-metadata --yes`). It is
always written (never COALESCE-guarded), so a richer metadata source
(e.g. ABS providing series/narrator) triggers a re-derive of bias terms on
the next enqueue or backfill call.

**Key choice — `book_dir` as primary key:** the monitor groups files by
`book_dir = filepath.Dir(file_path)` (one row per book directory, not per
track). This is the same granularity the rest of the pipeline uses for
per-book queries, and it lets the runner derive the key from a job's
`file_path` with a single `filepath.Dir` call.

Rules:
- Every write is **best-effort** — a `book_metadata` failure MUST NOT fail
  enqueue. The monitor logs and continues.
- The UPSERT is column-selective for ABS enrichment columns (narrator, series,
  asin, chapters) so a PathProvider call can never clobber ABS-sourced data.
  `bias_terms` is always overwritten (not COALESCE-guarded) so an improved
  metadata source is reflected on the next write.
- `chapters` is nullable and left `NULL` when no ABS provider is configured.
- **`chapters` times are BOOK-ABSOLUTE.** Each entry is
  `{Index, Title, StartSec, EndSec}` with `StartSec`/`EndSec` measured from the
  start of the **whole book** (all tracks concatenated in play order) — that is
  what Audiobookshelf's `media.chapters` reports. This is a *different time base*
  from `transcript_chunks.start_sec`, which is track-relative (§3); readers must
  add the track's book offset before mapping a chunk into this list (§2.2.1).
- The Go service creates the table in its schema-init transaction.

### 1.7 Append-only pipeline audit log — `pipeline_events` table

An **append-only** record of every Go-observable pipeline stage boundary —
the immutable timeline (what happened, when, by whom, how long) that complements
`run_metrics` (the current-state projection). It is **additive** and
**best-effort**: a failed event insert logs and continues; it NEVER fails the
pipeline stage that produced it. Writer: the **Go monitor**, **Go embed worker**,
and **Go DB layer** (requeue/recovery). The Python runner's claim/transcribe/done
events are **deferred** (see below).

```sql
CREATE TABLE IF NOT EXISTS pipeline_events (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id         UUID        REFERENCES transcription_jobs(id) ON DELETE CASCADE,
    file_path      TEXT,                          -- denormalized so a timeline survives a job's requeue churn
    stage          TEXT        NOT NULL CHECK (stage IN
                     ('discover','enqueue','claim','transcribe','chunk','embed','eval',
                      'done','fail','requeue','heartbeat','runner_availability')),
    event          TEXT        NOT NULL CHECK (event IN
                     ('start','finish','error','skip','retry','state')),
    runner_host    TEXT,                          -- who: a runner id / 'go-worker' / 'go-monitor'
    model          TEXT,                          -- asr/embed/eval model id for this stage
    model_version  TEXT,                          -- family+runtime or chart/image version
    duration_ms    BIGINT,                        -- set on 'finish'/'error'
    item_count     INT,                           -- chunks/windows/findings, stage-dependent
    token_count    BIGINT,                        -- prompt+total where applicable
    attempt        INT,                           -- transcription_jobs.attempts at the time
    reason         TEXT,                          -- failure/skip reason (free text)
    detail         JSONB,                         -- stage-specific extras (eval stats, availability, …)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS pipeline_events_job_id_idx  ON pipeline_events (job_id, created_at);
CREATE INDEX IF NOT EXISTS pipeline_events_stage_idx   ON pipeline_events (stage, event, created_at);
CREATE INDEX IF NOT EXISTS pipeline_events_created_idx ON pipeline_events (created_at);
```

Rules:
- **`job_id` nullable** so `runner_availability` and `heartbeat` events (not tied
  to a job) can be recorded; **`file_path` denormalized** so the timeline is
  reconstructable even after a requeue mutates/clears the job.
- **Append-only by convention** — no UPDATE/DELETE in Go except the retention
  prune below and the `ON DELETE CASCADE` (a job's history dies with the job,
  matching `run_metrics`; the denormalized `file_path` keeps the timeline legible
  within the retention window even as a job is requeued).
- **Best-effort** — same contract as `run_metrics`: a failed insert logs and
  continues; it never fails the pipeline stage.
- **Retention prune** — a periodic best-effort
  `DELETE FROM pipeline_events WHERE created_at < now() - interval '180 days' AND
  stage IN ('heartbeat','runner_availability')` (run from the monitor: once at
  startup, then every 24h). Only the high-frequency heartbeat/availability rows
  are pruned; per-job stage events are low-volume and kept indefinitely.

The retention prune is one of two recurring monitor tickers. The other is the
**periodic library scan** (`SCAN_INTERVAL`, default `1h`, §2.4), which re-walks
`BOOKS_DIR` to discover files fsnotify cannot see — writes made by another NFS
client never raise an inotify event in the monitor pod. Unlike the retention
prune, the scan ticker does **not** run once immediately: `Start` has already
performed the startup walk, so it waits for the first tick.

#### Stages: Go-emitted vs deferred (runner-side)

| Stage / event | Source | Status |
|---|---|---|
| `enqueue` / `finish` | Go monitor, at job creation | **Go-emitted** |
| `embed` / `start`,`finish` | Go embed worker | **Go-emitted** (duration_ms, model, item_count=chunks) |
| `eval` / `start`,`finish`,`error` | Go embed worker (in-pipeline judge) + standalone eval | **Go-emitted** (duration_ms, model, item_count=findings, detail={evaluated,skipped}) |
| `requeue` / `retry` | Go: operator requeue + stale-claim recovery (reset-to-pending) | **Go-emitted** |
| `fail` / `error` | Go: stale-claim recovery hitting the attempt cap | **Go-emitted** |
| `runner_availability` / `state` | Go batch coordinator (gpu-arbiter poll, on transition) | **Go-emitted** (Phase 3, §1.4) |
| `heartbeat` | derived from the runner's existing claimed-job heartbeat | **derived** — see the heartbeat caveat below |
| `claim`, `transcribe`, `done` | Python runner | **DEFERRED** — requires runner.py changes + on-hardware validation (NeMo/CUDA). The runner owns the claim/transcribe/mark-done UPDATEs; emitting these from runner.py is a follow-up PR. `done` cannot be observed Go-side (the completion time is captured by the `completed_at` trigger, §1.1, but no Go code sees the transition). |

> **Two heartbeats — claim-activity vs idle liveness.** The per-job heartbeat
> stamps `transcription_jobs.updated_at` only while a job is **claimed** (every
> `RUNNER_HEARTBEAT_SECONDS`, default 60); it goes quiet on a drained or paused
> queue, so `earmark_runner_last_heartbeat_seconds` (§2.16) reflects "time since
> last claim-activity" and **cannot** distinguish "idle, queue empty" from
> "down." The runner therefore ALSO stamps `runner_control.runner_heartbeat_at`
> on **every** poll cycle (working, idle, or paused), surfaced as
> `earmark_runner_alive_seconds` (§2.16) — this IS true idle liveness: a small
> value with an empty queue is idle-done, a large/growing value is the runner
> down. Alerts keyed on the old claim-activity gauge MUST still pair it with a
> queue-non-empty + not-paused condition; alerts on liveness should use
> `earmark_runner_alive_seconds`.

---

## 2. DEPLOYMENT INTERFACE CONTRACT

### 2.1 Identity

| Property | Value |
|----------|-------|
| Namespace | `earmark` |
| Go binary image | `ghcr.io/jedwards1230/earmark` |
| Helm chart OCI ref | `oci://ghcr.io/jedwards1230/charts/earmark` |
| Ingress hostname | `audiobooks-kb.example.com` |
| CNPG cluster name | `earmark-pg` |

`audiobooks.example.com` may be taken by an existing Audiobookshelf instance. Do NOT use it here.

### 2.2 MCP Transport

| Property | Value |
|----------|-------|
| Transport | `streamable-http` (wiki parity) |
| Container port | `8081` |
| URL path | `/mcp` |
| In-cluster URL | `http://earmark.earmark:8081/mcp` |
| mcp-proxy upstream key | `"audiobooks"` |

The mcp-proxy configmap entry (add to `mcpServers` object):

```json
"audiobooks": {
  "url": "http://earmark.earmark:8081/mcp",
  "transportType": "streamable-http"
}
```

#### 2.2.1 MCP Tools

The MCP server exposes **8 tools**: **6 are read-only** (no side-effects) and
**2 write** — `decide_transcript_correction` and `create_transcript_correction`,
the human gate on the correction overlay (§2.17). `list_transcript_corrections`,
the third correction tool, is itself read-only — it is a worklist query, not a
decision. The two search tools default to the **whole library** and take an
optional `book` to scope to a single title. (The legacy `browse_audiobook_library`
tool was removed — `list_books` strictly dominates it; its tree view is folded in
via `list_books format=tree`.)

**What the write tools may write.** Both are narrow by construction: they write
only `transcript_findings` (the decisions layer — a `patch_state` transition, or
a new human-authored row) and `transcript_chunks.embedding_stale` (the flag that
triggers the rebuild pass's re-embed). Neither ever writes
`transcripts.segments`/`transcripts.raw_text` — the immutable ASR record — and
neither ever writes chunk *text*: the projection is regenerated and the overlay
replayed onto it on every embed, so text written anywhere else would just be
overwritten by the next rebuild (§2.17). Both carry non-read-only tool
annotations (`ReadOnlyHint: false`, `DestructiveHint: false`,
`IdempotentHint: false`), so an MCP host can prompt a user for confirmation
before calling either — deciding the same finding twice is refused by the state
guard rather than being silently idempotent, and `create_transcript_correction`
genuinely creates a new row on each call, so neither tool could honestly claim
otherwise.

**Open question: MCP writes carry no bearer token.** Unlike the §2.12 control
API, which fails closed (`503`) on a mutating call with no
`Authorization: Bearer <CONTROL_API_TOKEN>`, the MCP tool-call transport has no
equivalent per-call credential — any client that can reach `/mcp` can call
`decide_transcript_correction` or `create_transcript_correction`. As it stands
this is judged acceptable, not settled, on three grounds: the MCP HTTP transport
is LAN-only (the same posture as the dashboard and the control API); no write
here deletes anything or touches transcript provenance; and every decision is
reversible by construction (a state flip has an inverse, and a finding — human-
or judge-authored — is never deleted). Per-call authorization for MCP writes (a
bearer token, an MCP-host-level allowlist, or similar) remains an **open
question**, not a decision this contract has made.

**Chunks vs segments** — two granularities of the same text, surfaced by
different tools: a **chunk** is the embedding/search unit (~hundreds per book; a
chunk is *tens of consecutive segments* grouped to a token budget), while a
**segment** is a sentence-sized unit derived from the per-word timestamps
(silence-gap split with a duration cap, §1.2.1 — typically a few seconds, many
per chunk) that always carries its own `words[]`. The search tools +
`get_chunk_context` operate on **chunks**; `get_transcript` paginates
**segments** (and, with `includeWordTimestamps=true`, their per-word times).

| Tool | Purpose | Key params |
|------|---------|-----------|
| `list_books` | Library **inventory**: per book → author, title, track progress (done/total), total duration, word count, embedded-chunk count. Em dash / 0 for books with no `run_metrics` yet. Ordered **transcribed-first** (fully-done books, then partial, then fully-pending). Leads with a one-line whole-library summary (`Library: T books — P fully transcribed, Q with pending tracks.` — TRUE totals across the library, not just the page). `format=flat` (default) **omits each book's `dir:` line** to keep the payload small; `format=tree` groups rows under their authors **and** keeps the `dir:` line; `format=series` groups rows under their **series name**, ordered by sequence within each group, with a trailing **"No series"** group so no book disappears (a book in several series is listed under each — by design). | `author?` (substring filter), `series?` (case-insensitive substring on `book_metadata.series`, §1.6 — so `Dune` matches both `Dune #2` and `The Dune Sequence #13`; books with no series row are excluded), `format?` (`flat` default \| `tree` \| `series`), `limit?` (default 50), `offset?` |
| `semantic_search_audiobooks` | Vector-similarity (meaning) search; hits show a real cosine `similarity: NN%`. Whole library by default; `book` scopes it. `snippet?` caps each hit's quoted text (leading **preview** — no sub-chunk match position). | `query` (required), `book?`, `threshold?` (0.3), `limit?` (10), `snippet?` (max chars; floored to 80) |
| `text_search_audiobooks` | Trigram literal/keyword search; hits are labelled **"ranked by trigram match"** (NOT a similarity %, which would mislead on a literal hit). Whole library by default; `book` scopes it. `snippet?` returns an excerpt **centred on the literal match**. | `query` (required), `book?`, `limit?` (10), `snippet?` (max chars; floored to 80) |
| `get_transcript` | Read a track's full transcript as timestamped **segments** (paginated — `raw_text` can be 600k+ chars). Multi-track book → returns a track chooser to pick a `trackID`. Per-word timestamps are **hidden by default**; `includeWordTimestamps=true` adds each segment's `words[]` (word/start/end, plus score/speaker when present) for "exactly when was X said" queries. | `book?` or `trackID?` (one required), `offset?` (0), `limit?` (50 segments), `includeWordTimestamps?` (false) |
| `get_chunk_context` | Surrounding **chunks** around a chunk. `chunkID` is the **UUID** in a search hit's `ID` field. | `chunkID` (required, the search-hit UUID), `contextWindow?` (**default 1** → ~3 chunks; clamped to 0–50 to bound the response size) |
| `list_transcript_corrections` | Read-only review **worklist** (§2.17): each row is a finding with pristine chunk context, an anchor-resolution status, and its legal next actions (`allowedActions`). Defaults to the undecided (`proposed`) queue. | `state?` (comma-separated patch states, or `all`/`any`; default `proposed`), `book?`, `path?`, `id?` (a single finding), `min_confidence?` (0), `limit?` (20, capped 200), `offset?` (0) |
| `decide_transcript_correction` | **Writes.** Accept / reject / revert / reconsider one finding — drives `db.SetPatchState`, validated against `patch.CanTransition` and compare-and-swapped on the expected current state. Accept/revert flag the chunk `embedding_stale`; reject changes no text. | `id` (required), `action` (required: `accept`\|`reject`\|`revert`\|`reconsider`), `decided_by?` (default `"agent"`, stored `mcp:`-prefixed), `expected_state?` (optional CAS guard) |
| `create_transcript_correction` | **Writes.** The direct-edit escape hatch: records a correction no model proposed, after passing the same gates an accepted judge patch passes (anchor resolution, chunk-hash verification, overlap refusal). Writes no transcript text; flags the chunk `embedding_stale`. | `chunk_id` (required), `original_text` (required, verbatim span), `correction` (required, non-empty), `occurrence?`, `offset?` (hint only — the stored anchor is always the resolved position), `issue_type?` (default `other`), `decided_by?` (default `"agent"`), `expected_chunk_sha256?`, `dry_run?` (default `false`) |

**Structured output**: every tool advertises an `outputSchema` and returns
`structuredContent` (machine-readable) **in addition to** the existing
human-readable text, which is kept as the spec-required back-compat fallback
(`content[0]`). The structured payloads are: the two search tools +
`get_chunk_context` → `{ kind, query?, count, results[] }` (`kind` is
`semantic` \| `trigram` \| `context`; `results` are the chunk rows, with each
row's `content` honouring the `snippet` window when one is set);
`list_books` → `{ format, books[], totals, total, offset, nextOffset? }`;
`get_transcript` → `{ kind: "transcript", filePath, language, modelName,
durationSeconds, segments[], offset, limit, totalSegments, nextOffset? }` for a
page, or `{ kind: "trackChooser", book, tracks[] }` when a book has multiple
tracks. Each segment is `{ start, end, text }`; with `includeWordTimestamps=true`
it also carries `words[]` — each `{ word, start, end, score?, speaker? }`
(`score`/`speaker` present only when the ASR backend supplied them). The `words`
field is **omitted entirely** by default, so the default response is unchanged.
Bad user input (missing/unmatched `book`, bad `chunkID`, etc.) returns a
tool-execution error (`isError`), never a protocol error.

**Series in structured output** (**additive response-shape change** — no existing
field changed type or meaning, and nothing was removed): both the `list_books`
`books[]` entries and the search / `get_chunk_context` `results[]` rows carry a
new optional `series` array parsed from `book_metadata.series` (§1.6). Each entry
is `{ name, sequence? }`; `sequence` is a **string**, not a number, because
novellas are commonly `#1.5` and non-numeric positions exist — a numeric type
would truncate or reject real values. A book can be in several series, so the
array can hold more than one entry (`"Dune #2, The Dune Sequence #13"` →
`[{"name":"Dune","sequence":"2"},{"name":"The Dune Sequence","sequence":"13"}]`).
The field is **omitted entirely** (`omitempty`) for a book with no series
metadata — the common case for path-sourced books — so a consumer that ignores it
sees a byte-identical payload. `list_books format=series` regroups only the text
rendering; the structured `books[]` stays the same flat page in all three formats.
The whole-library summary line is scoped by `author` only, **not** by `series` —
it answers "how big is the library", while `total` reports the filtered count.

**Snippet windows** (`snippet` on both search tools): omitted → the full ~400-word
chunk (backward-compatible). When set, the hit's quoted text is truncated to
~`snippet` chars with a `…(truncated, use get_chunk_context for full text)`
marker. The cap applies to **`structuredContent.results[].content` as well as
the human-readable text rendering** — a structured consumer never receives the
full chunk when a window is set (the structured row carries the bare excerpt,
whose leading/trailing `…` already signal truncation; the prose marker is
text-rendering only, since `chunkID` already names the follow-up call). Text
search centres the window on the literal query match; semantic search returns a
**leading preview** (there is no sub-chunk match position) and
`get_chunk_context` returns the full surrounding text. A positive value below 80
is raised to 80 so the excerpt stays readable, and a value above 4000 is capped
to 4000 (well past a full chunk, so the cap only guards against absurd inputs).

**`book` resolution** (both search tools + `get_transcript`): the `book` string
is resolved to a single canonical `file_path` directory prefix via
`GetBookSummaries`. Matching is **ASIN-aware** to avoid catalogue-id collisions:

- A **bracketed catalogue id** in the query (`[B0…]` or `[<digits>]`, e.g.
  `[1984832069]`) is matched against each book's embedded ASIN **exactly**.
- Otherwise the query is substring-matched against the human **title + author**
  label **with the bracketed ASIN stripped** — NOT the raw path/ASIN. So
  `book="1984"` resolves to a *1984* title (and Orwell) but never to a book whose
  ASIN merely contains `1984` (e.g. Kahneman's *Noise* at ASIN `1984832069`).

Zero or multiple matches return a helpful error listing the candidates.

**Result formatting (search + context):** chapter mapping **is** populated when
the book has a provider chapter list (`book_metadata.chapters`, §1.6 — in
practice an ABS-enriched book). The formatter still **suppresses the chapter
label entirely** when there is no real chapter data (chapter index 0 AND empty
title) — no misleading `Chapter 0:` prefix is emitted. A populated chapter
(non-zero index or a non-empty title) renders as `Chapter N: <title>`.

**Chapter time bases (search + context):** a chunk's `startSec`/`endSec` are
**track-relative** — offsets into the chunk's own audio file, because there is
one ASR transcript per track — while a provider chapter list is **book-absolute**
across all of a book's tracks concatenated in play order. Mapping a chunk to a
chapter therefore adds the chunk's **track offset within the book** (the summed
durations of the preceding tracks, from `transcripts.duration_seconds` ordered by
`file_path`) before the lookup. Single-track books have a zero offset and are
unaffected. When the offset cannot be established — a preceding track has no
transcript row yet, or a NULL/zero `duration_seconds` — the chapter fields are
left **unset** and the label is suppressed: a missing chapter label is honest,
a plausible-but-wrong chapter title is not.

**Per-result metadata fields** in the structured payload of both search tools and
`get_chunk_context`: `wordCount` is the whitespace-delimited word count of the
**full** chunk text (not of a `snippet`-truncated excerpt); `totalChapters` is the
size of the book's provider chapter list (`0` means "no chapter data for this
book"); `fileChecksum` is the SHA-256 of the chunk's track from `transcripts`.
The former `chunkStart`/`chunkEnd` fields (character offsets into `raw_text`)
**have been removed** — no such column exists and no writer ever populated them,
so they always serialized as `0`; use the populated `startSec`/`endSec` time
offsets instead. This is a response-shape change to the structured payload; the
human-readable text output is unchanged apart from the now-rendered
`| Words: N` citation segment and the now-populated chapter label.

**Scoped semantic search query strategy (`book` set):** scoped semantic search
does **NOT** add a `WHERE file_path LIKE` predicate to the HNSW query — pgvector
HNSW returns the global top-K then filters, so a selective single-book filter
under-returns (filtered-ANN recall loss). Instead it runs an **exact (non-HNSW)
distance scan within the book**: the `file_path` btree
(`transcript_chunks_file_path_idx`, usable under C-collation for the `LIKE
prefix || '%'` prefix) narrows to that book's few-hundred chunks first, then an
exact `ORDER BY embedding <=> $vec LIMIT $k` orders them — fast AND recall-perfect.
Unscoped semantic search keeps using the HNSW index.

### 2.3 Embeddings

| Property | Value |
|----------|-------|
| Default base URL | `http://ollama:11434/v1` |
| Embedding model | `nomic-embed-text` |
| Vector dimension | **768** |

The pgvector column storing embeddings MUST be declared as `VECTOR(768)`.
The `nomic-embed-text` model produces 768-dimensional vectors and is already
available in the cluster's Ollama instance. Any change to the model requires
a full re-embedding of all chunks and a column type migration.

#### Task-instruction prefixes (nomic-embed-text)

`nomic-embed-text` is trained with **task-instruction prefixes** and runs in an
undefined regime without them. The two sides of the pipeline MUST use different
prefixes so stored vectors and query vectors land in the same learned space:

| Side | Prefix | Where applied |
|------|--------|---------------|
| Document (stored/indexed passages → `transcript_chunks.embedding`) | `search_document: ` | `Embeddings.EmbedDocuments` (worker embed path) |
| Query (search query string) | `search_query: ` | `Embeddings.EmbedQuery` (`Search` / `SearchInBook`) |

Prefixing is **model-gated**: it applies only when the resolved embeddings model
name contains `nomic` (case-insensitive). A model not trained with these prefixes
(e.g. `bge-m3`) receives the text verbatim, since the wrong prefix would degrade
retrieval. The document/query divergence is enforced in `internal/openai`
(`EmbedDocuments` vs `EmbedQuery`) — these two methods MUST NOT collapse to one
prefix.

**Operational note**: changing whether/which prefixes are applied makes existing
stored vectors incompatible with new query vectors. After deploying a prefix
change, run a full re-embed (`earmark requeue --reembed "" --yes`); until then
semantic search is *more* wrong (query prefixed, stored not), so the re-embed
must accompany the rollout. Dimension is unchanged (768) — no schema migration.

### 2.4 Environment Variables (canonical names)

All env var names are fixed. No synonyms, no alternatives.

#### Go service (in-cluster Deployment)

| Variable | Required | Default / Notes |
|----------|----------|-----------------|
| `DATABASE_URL` | yes | PostgreSQL DSN: `postgres://earmark:<pass>@earmark-pg-rw.earmark:5432/earmark` |
| `PGHOST` | no | Convenience alias; `DATABASE_URL` takes precedence |
| `PGPORT` | no | Convenience alias |
| `PGUSER` | no | Convenience alias |
| `PGPASSWORD` | no | Convenience alias |
| `PGDATABASE` | no | Convenience alias |
| `EMBEDDINGS_BASE_URL` | no | **Deprecated** — `http://ollama:11434/v1`. Superseded by `AI_ENDPOINTS` (§2.14); still honored (synthesized into a `_legacy` embeddings endpoint) when `AI_ENDPOINTS` is unset. |
| `EMBEDDINGS_MODEL` | no | **Deprecated** — `nomic-embed-text`. See `EMBEDDINGS_BASE_URL` above and §2.14. |
| `AI_ENDPOINTS` | no | JSON array of AI endpoint descriptors (the AI endpoint registry, §2.14). When set, `AI_ROLES` is required and the `EMBEDDINGS_*` vars are ignored. **Malformed value is fatal** (fail-closed). Empty → the `EMBEDDINGS_*` legacy path applies. |
| `AI_ROLES` | no | JSON object binding role names (`embeddings`, `eval`) to endpoint IDs (§2.14). Required when `AI_ENDPOINTS` is set. |
| `BOOKS_DIR` | no | `/books` (read-only NFS mount inside container) |
| `MCP_HTTP_ADDR` | no | `:8081` |
| `INGEST_HTTP_ADDR` | no | `:8082`. The `earmark monitor` (ingest) process serves a minimal HTTP listener here for `/healthz` (liveness) and `/metrics` (Prometheus, §2.16). The mcp pod uses `MCP_HTTP_ADDR` for its surface; this is the ingest pod's only HTTP port. Chosen to avoid colliding with `:8081`. |
| `LOG_FORMAT` | no | `pretty` (default — human-readable, ANSI-colored `PrettyHandler`). Set `json` for a `slog` JSON handler writing one JSON object per line to stdout (parseable in Loki). Both carry the `module` attribute and honor `LOG_DEBUG`/`LOG_VERBOSE`. Used by both Go pods. |
| `STALE_JOB_TIMEOUT` | no | `30m` (Go duration string) |
| `SCAN_INTERVAL` | no | `1h` (Go duration string). How often the monitor **re-walks `BOOKS_DIR`** looking for new audio files, in addition to the walk it does at startup. Required for correctness on NFS: fsnotify/inotify only reports writes that pass through the monitor pod's own kernel, so a book written directly on the file server — or by any other NFS client — raises **no** watch event and would otherwise stay undiscovered until the pod restarted. The recurring walk is the backstop; fsnotify remains the low-latency path for local writes. The walk is metadata-only for known paths (already-queued `file_path`s are skipped without re-hashing, §1.1), so it is cheap over a multi-TB library. Per-entry errors (e.g. a transient NFS `EIO` on one subdirectory) are logged, counted, and skipped — one bad directory must never abort, and thereby silently disable, every subsequent scan. **`0` (or a negative value) disables periodic scanning**, leaving only the startup walk and fsnotify. |
| `CHUNK_SIZE` | no | `512` (target tokens per chunk; overlap is 64 tokens) |
| `EMBED_BATCH_SIZE` | no | `32`. Max transcripts the embed worker selects **per poll cycle** for BOTH gated-flow (`EVAL_GATES_EMBED`) passes — the eval pass and the embed pass each `… ORDER BY t.created_at ASC LIMIT $1`. Bounds the worker's per-cycle memory: an unbounded selection loads every matching transcript's `segments` JSONB into one slice, which OOM-kills the pod on a large backlog (e.g. a full re-embed after re-segmentation). The worker drains a backlog across cycles — a transcript that gets eval'd/embedded drops out of the next cycle's selection — and **loops immediately (no `pollInterval` sleep) whenever a pass returns a full batch**, so a multi-thousand-item backlog drains in back-to-back cycles rather than one batch per poll interval. Must be a **positive integer** — a non-positive or non-numeric value is fatal at startup (the OOM guard must never silently round-trip to unbounded). The ungated single-pass selection (`GetCompletedTranscripts`) is unaffected. |
| `LIBRARY_COLLECTIONS` | no | JSON array describing each library root's shape, for the dashboard's author/title labels (see below). Empty → generic fallback. |
| `CONTROL_API_TOKEN` | no | Bearer token required on the mutating control-API endpoints (§2.7). Empty → those endpoints fail closed (`503`); read endpoints are always open. |
| `EVAL_MAX_FINDINGS_PER_CHUNK` | no | `5`. Cap on findings kept per chunk by the eval judge (highest-confidence retained; the judge over-flags). `<= 0` disables the cap. See §2.15. |
| `EVAL_MIN_CONFIDENCE` | no | `0.6`. Confidence floor — findings below it are dropped before the cap. `<= 0` disables the floor. See §2.15. |
| `EVAL_IN_PIPELINE` | no | `false`. When true, the embed worker runs the eval judge on each transcript's chunks **before embedding** (the repositioned, in-pipeline eval). Default off → eval stays on-demand and the worker is unchanged. Requires an eval chat endpoint (`AI_ROLES.eval` / `EVAL_CHAT_*`); if none resolves, inline eval is logged-skipped, not fatal. See also `EVAL_GATES_EMBED`. |
| `EVAL_GATES_EMBED` | no | `false`. When true, the pipeline becomes strictly linear — a transcript is NOT embedded (not searchable) until it has been judged. Implements the **two-pass gated flow**: an **eval pass** selects done, not-eval'd, not-embedded transcripts and judges them (writing `eval_finished_at`); an **embed pass** then selects done, eval'd, not-embedded transcripts and embeds them. The `eval_finished_at` latch (CONTRACT §1.5) is the hand-off between the two passes. **Invariant**: under this gate, `embedded ⟹ eval'd`. **Fail-closed** (two conditions, both fatal at startup): (1) if no eval judge endpoint resolves (`AI_ROLES["eval"]` / `EVAL_CHAT_*`); and (2) if `EVAL_IN_PIPELINE` is not also `true`. The gate makes eval a strict prerequisite for embedding, and the eval judge is only built when `EVAL_IN_PIPELINE=true`; so `EVAL_GATES_EMBED=true` **requires** `EVAL_IN_PIPELINE=true` (and a resolvable judge) — otherwise the worker would run gated with a nil judge, stalling the corpus (or risking a nil-judge deref). Both failures fail at startup, never silently stalling the corpus (mirror of the §2.14 malformed-registry fail-closed). Default `false` → behavior is identical to the pre-gate deployment (no behavior change for unconfigured deployments). **Chunk UUIDs**: under this gate, chunk UUIDs are derived deterministically as UUIDv5 over `(transcript_id, chunk_index)`, so the eval pass (which chunks to judge) and the embed pass (which chunks to insert) produce identical IDs without coordination — findings written in the eval pass reference the same chunk rows the embed pass inserts. |
| `GPU_ARBITER_URL` | no | gpu-arbiter `/status` URL (e.g. `http://gpu-host:48750/status`) read by the `earmark batch` coordinator (§1.4) to yield the GPU to games. **Read-only** — the coordinator only `GET`s it, never `POST`s. Unset or unreachable → the coordinator logs it and proceeds (degrades gracefully). The `batch --gpu-arbiter-url` flag overrides it. |
| `ARBITER_WAIT_CMD` | no | Explicit path to a gpu-arbiter binary the `earmark batch` coordinator should delegate waits to (§1.4). Lower precedence than the `batch --arbiter-wait-cmd` flag, higher than `gpu-arbiter` auto-detected on `PATH`. Unset → the coordinator auto-detects on `PATH`, falling back to its built-in HTTP poll loop if nothing resolves. Purely an optimization — never required. |
| `ASR_SERVERS` | no | JSON array declaring the transcription servers (ASR runners) for this deployment, so the Servers dashboard page can show a configured-but-idle server (e.g. a fallback). Empty → the page lists only observed runners. Cosmetic/read-only: a malformed value logs a warning and is ignored, and the list does **not** influence job routing (the runner claims work itself). See below. |
| `METADATA_PROVIDER` | no | `path` (default). Accepts `path`, `abs`, or `chain:<p1>,<p2>` (e.g. `chain:abs,path`). `path` derives title/author from the filesystem path only; `abs` queries Audiobookshelf; `chain` tries providers left-to-right and returns the first non-empty result. |
| `ABS_URL` | no | Base URL of the Audiobookshelf server (e.g. `https://audiobooks.example.com`). Required when `METADATA_PROVIDER=abs` or `abs` appears in a chain spec; ignored otherwise. |
| `ABS_TOKEN` | no | Audiobookshelf API token. Required when `ABS_URL` is set. |
| `ABS_LIBRARY_ID` | no | Audiobookshelf library ID to search for book metadata. Required when `ABS_URL` is set — there is no default, since the value is deployment-specific. When unset, the ABS provider is skipped with a warning and metadata falls back to the path provider. |

`LIBRARY_COLLECTIONS` is a JSON array of `{"root","layout"}` objects. `root` is a
path prefix (absolute, or relative to `BOOKS_DIR`); `layout` is a slash-delimited
list of segment roles (`author`/`title`/`series`/`_`) for the directories below
the root. If `title` is not one of the directory roles, the title is parsed from
the filename. The longest-matching root wins; unmatched paths fall back to a
generic author/title split. Labels are cosmetic — a malformed value logs a
warning and falls back, never failing startup. Example:

```json
[{"root":"audio-libation","layout":"author/title"},
 {"root":"audio-libro","layout":"author"}]
```

`ASR_SERVERS` is a JSON array of
`{"name","host","model","role","match","gpuArbiterUrl"}` objects; only `name` is
required. `match` is a case-insensitive substring tested against both
`transcription_jobs.claimed_by` and `run_metrics.runner_host` to attribute
observed activity to the server (defaults to `name`). `role` is free-form
(conventionally `primary`/`fallback`) and informational. `gpuArbiterUrl` is an
optional [gpu-arbiter](https://github.com/jedwards1230/gpu-arbiter) `/status`
endpoint the dashboard polls (2s timeout, 5s TTL cache) for **live readiness**:

| gpu-arbiter `/status` | Servers-page state | API `state` |
|---|---|---|
| reachable, `state=available`, runner unit up | `READY` (green) | `ready` |
| reachable, `state=gaming`/`evicting` (or runner unit down) | `BUSY` — "connected but not usable" (amber) | `busy` |
| unreachable | `OFFLINE` (grey) | `offline` |

A fresh DB claim still wins (`TRANSCRIBING`); without a `gpuArbiterUrl` the state
falls back to history inference (`idle`/`not_seen`). The Servers dashboard page
and the `servers` array in `GET /api/v1/status` merge the configured list with
observed activity; an observed runner with no matching entry is still shown,
marked *unconfigured*. Example:

```json
[{"name":"gpu-1","host":"gpu-1","model":"nvidia/parakeet-tdt-0.6b-v3","role":"primary","gpuArbiterUrl":"http://gpu-1:48750/status"},
 {"name":"gpu-2","host":"gpu-2","model":"nvidia/parakeet-tdt-0.6b-v3","role":"fallback"}]
```

> **Not routing.** This is observability only. earmark does not move work between
> servers — the runner claims its own jobs. The readiness probe is the intended
> *signal* for a future fallback automation (read `gpuState`/`gpuReachable` from
> `/api/v1/status`), but actually routing job types to specific servers and
> primary/fallback selection still require runner-side changes and a contract
> amendment.

#### Python ASR runner (any backend — GPU/ASR host native service)

The runner is no longer assumed to be NeMo Parakeet-TDT specifically; earmark
supports multiple, swappable backends (different model families, runtimes, and
hosts) reporting into the same `transcription_jobs` / `transcripts` /
`run_metrics` contract. The variables below are backend-agnostic; the defaults
preserve the original NeMo-Parakeet behavior, so the existing runner keeps
working unchanged. The three `ASR_FAMILY` / `ASR_RUNTIME` / `ASR_CAPABILITIES`
vars are new and **optional** — see §2.13 for the vocabulary.

| Variable | Required | Default / Notes |
|----------|----------|-----------------|
| `DATABASE_URL` | yes | Same DSN as Go service — runner connects directly to CNPG rw endpoint |
| `RUNNER_IDENTITY` | no | `asr-runner` (included in `claimed_by`) |
| `RUNNER_POLL_INTERVAL_SECONDS` | no | `30` |
| `RUNNER_HEARTBEAT_SECONDS` | no | `60` |
| `RUNNER_BUSY_FLAG_PATH` | no | `/tmp/earmark-busy` |
| `ASR_MODEL` | no | `nvidia/parakeet-tdt-0.6b-v3` (model id; written to `transcripts.model_name` + `run_metrics.asr_model`) |
| `ASR_FAMILY` | no | Model-family id, e.g. `nemo-parakeet`, `whisper` (§2.13). Free-form-but-conventional. Written to `run_metrics.asr_family`. Unset → NULL ("unknown"). |
| `ASR_RUNTIME` | no | Runtime id, e.g. `nemo-cuda`, `whisper.cpp-sycl` (§2.13). Free-form-but-conventional. Written to `run_metrics.asr_runtime`. Unset → NULL ("unknown"). |
| `ASR_CAPABILITIES` | no | JSON map of the runner's **advertised** capabilities (its static truth), keys from the §2.13 closed enum → bool, e.g. `{"word_timestamps":true,"context_biasing":false}`. Defaults per known family. Used for the deferred routing match and as `caps_applied` defaults. Unknown keys are ignored with a warning. |
| `ASR_DIARIZE` | no | `false` (default). Set `true` to run speaker diarization (e.g. NeMo Sortformer) for multi-voice/full-cast titles. **Global** (per-job diarization is a deferred Phase-3 concern). |
| `ASR_COMPUTE_TYPE` | no | `bfloat16` (native on RTX 5090 / Blackwell) |
| `ASR_CHUNK_THRESHOLD_SECONDS` | no | `3600` — single-pass below this duration; chunked/buffered inference above |
| `BOOKS_MOUNT` | no | `/srv/audiobooks` (NFS export path on the storage host) |

**Breaking changes: none** for the existing runner — every new var is optional
and the defaults preserve current behavior.

#### Runner result obligation — report applied capabilities (SHOULD)

In the same best-effort `run_metrics` UPSERT it already performs on "mark done",
the runner **SHOULD** populate the §2.13 backend descriptor for the run:
`asr_family`, `asr_runtime`, `caps_applied` (what it actually did), and — when it
*declined* a requested capability — `caps_skipped_reason` (key → short reason),
plus `mean_word_confidence` when the model emits per-word scores. This is
**SHOULD, not MUST**: a runner that omits them is still contract-compliant (the
columns stay NULL → "unknown"). This is the explicit backward-compat carve-out;
the obligation is additive and introduces no breaking change.

### 2.5 CNPG Cluster

| Property | Value |
|----------|-------|
| Cluster name | `earmark-pg` |
| Namespace | `earmark` |
| PostgreSQL image | `ghcr.io/cloudnative-pg/postgresql:16-pgvector` |
| Storage size | `20Gi` |
| StorageClass | `nfs-databases` |
| Read-write endpoint | `earmark-pg-rw.earmark` (port 5432) |
| Database name | `earmark` |
| Database owner | `earmark` |
| PostInitSQL extensions | `CREATE EXTENSION IF NOT EXISTS vector;` `CREATE EXTENSION IF NOT EXISTS pg_trgm;` |
| Backup destination | `s3://postgres-backups/` via an S3-compatible object store (e.g. Garage at `http://s3.example.com:3900`) |
| Backup plugin | `barman-cloud.cloudnative-pg.io` |
| Backup retention | `30d` |
| Backup schedule | `0 0 3 * * *` (daily 3 AM, six-field cron) |
| ObjectStore name | `garage-backup-store` |

#### 1Password item paths (follow `k8s-<ns>-<service>-<type>` convention)

| Secret | 1Password item path |
|--------|---------------------|
| DB credentials (CNPG) | `vaults/example/items/k8s-earmark-pg-credentials` |
| S3 credentials for CNPG | `vaults/example/items/k8s-earmark-cnpg-garage-secret` |
| HuggingFace token (runner) | Stored in a secrets manager (not a K8s secret — runner is a GPU/ASR host native service) |

The `cnpg-garage-secret` secret provides `ACCESS_KEY_ID`,
`ACCESS_SECRET_KEY`, and `REGION` keys for the S3-compatible object store.

### 2.6 Audiobook Library NFS Mount

| Property | Value |
|----------|-------|
| NFS server | `<nfs-server-ip>` (e.g. `192.0.2.10`) |
| NFS export path | `/srv/audiobooks` |
| PVC name | `books` (existing PVC in `media` namespace — re-used read-only) |
| StorageClass | `nfs-static-media` |
| Access mode in Deployment | `ReadOnlyMany` — declare `readOnly: true` in the volumeMount |
| Container mount path | `/books` |

The `books` PVC already exists in the `media` namespace with `ReadWriteMany`.
The earmark Deployment in the `earmark` namespace MUST define
its own static PV + PVC pointing to the same NFS export with `ReadOnlyMany`
to enforce the read-only constraint without depending on the `media` namespace.

```yaml
# PersistentVolume — declare in earmark namespace manifests
apiVersion: v1
kind: PersistentVolume
metadata:
  name: earmark-books-ro
spec:
  capacity:
    storage: 100Gi
  accessModes:
    - ReadOnlyMany
  storageClassName: nfs-static-media
  persistentVolumeReclaimPolicy: Retain
  nfs:
    server: 192.0.2.10
    path: /srv/audiobooks
  claimRef:
    name: books-ro
    namespace: earmark
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: books-ro
  namespace: earmark
spec:
  storageClassName: nfs-static-media
  volumeName: earmark-books-ro
  accessModes:
    - ReadOnlyMany
  resources:
    requests:
      storage: 100Gi
```

### 2.7 Helm Chart Structure (cardigan model)

Chart lives at `deploy/helm/earmark/` in the `earmark` repo.
Published as `oci://ghcr.io/jedwards1230/charts/earmark`.

The helmfile release and the ArgoCD Application CRD live in the private
deployment repo, alongside the other cluster manifests.

Sync policy: **auto-sync** (`prune: true`, `selfHeal: true`) — this is a
non-critical new service, not in the manual-sync exceptions list.

### 2.8 Standard Ingress Annotations

```yaml
annotations:
  cert-manager.io/cluster-issuer: "letsencrypt-prod"
  traefik.ingress.kubernetes.io/router.entrypoints: web,websecure
  traefik.ingress.kubernetes.io/router.middlewares: kube-system-redirect-lan@kubernetescrd
```

TLS secret name: `audiobooks-kb-tls`

### 2.9 Node Placement

The Go Deployment must run on AMD64 nodes only (no ARM64 cross-compile
requirement imposed, but the image pipeline targets amd64):

```yaml
nodeSelector:
  kubernetes.io/arch: amd64
```

No `role` selector is needed — the service is stateless and any available AMD64 node
is acceptable.

### 2.10 Required Labels

```yaml
labels:
  app.kubernetes.io/name: earmark
  app.kubernetes.io/instance: earmark-prod
  app.kubernetes.io/component: mcp-server        # for the Go Deployment
  app.kubernetes.io/part-of: earmark-stack
  app.kubernetes.io/managed-by: Helm
```

### 2.11 Security Context

```yaml
securityContext:
  fsGroup: 100    # NFS compatibility (users group)
```

### 2.12 Control API

The MCP HTTP transport (`:8081`) serves a JSON control API under `/api/v1` for
driving the pipeline from scripts/agents — distinct from the htmx dashboard
actions (`/actions/*`, guarded by the `HX-Request` header). It writes the
`runner_control` row described in §1.4.

| Method | Path | Auth | Body | Result |
|--------|------|------|------|--------|
| `GET` | `/api/v1/status` | none | — | `200` queue/runner snapshot (JSON), incl. a `servers[]` array (name, host, role, configured, state, model, modelSize, computeMode, jobsDone; plus gpuProbed/gpuReachable/gpuState/vramUsedMb/vramTotalMb when a `gpuArbiterUrl` is configured), an `endpoints[]` array (id, type, backend, baseURL, model, options, role, state, probed — the AI endpoint registry with health probes, §2.14), an `eta` object (the empirical ETA, §4: `{remainingChunks, workSeconds, calendarSeconds, calendarKnown, evalIncluded, hasWork, label}`; `null` when no estimate could be computed), and a **`pipeline`** object (see below) |
| `GET` | `/api/v1/pipeline/pause` | none | — | `200 {"paused":bool,"runLimit":int\|null}` |
| `PUT` | `/api/v1/pipeline/pause` | bearer | `{"paused":bool}` | `200` current state (`paused:false` resumes + clears bound) |
| `POST` | `/api/v1/pipeline/run` | bearer | `{"limit":N}` (N≥1) | `202 {"paused":false,"runLimit":N}` — run N then auto-pause |
| `DELETE` | `/api/v1/pipeline/run` | bearer | — | `200` clears the bounded run (`run_limit→NULL`) |
| `POST` | `/api/v1/runner/update` | bearer | `{"version":"<tag>"}` (empty/omitted clears) | `202 {runningVersion,desiredVersion,state,updateAvailable,…}` — request the runner self-update to `<tag>` (sets `desired_runner_version` + `'requested'`); the runner performs the swap (§1.4) |

**`pipeline` object** — a derived 3-stage lifecycle view (Transcribe → Eval → Embed) computed at read-time from already-fetched signals:

```jsonc
{
  "activity":        "transcribing" | "evaluating" | "embedding" | "winding-down" | "idle" | "paused",
  "phase":           "idle" | "transcribe" | "analyze",   // raw coordinator phase (§1.4)
  "transcribeDone":  317,   // done tracks
  "transcribeTotal": 362,   // all tracks
  "evalCoverage":    0.88,  // fraction of done jobs judged (0..1); -1 when evalInPipeline=false
  "evalInPipeline":  true,  // whether EVAL_IN_PIPELINE is enabled
  "embedBacklog":    3,     // completed transcripts not yet embedded
  "gpuCommitted":    true,  // pipeline currently owns the GPU (transcribing or evaluating)
  "gpuProbed":       true,  // gpu-arbiter probe is configured and reachable
  "fullyDone":       false, // all stages complete (pending==0 && claimed==0 && embedBacklog==0 && eval covered)

  // Per-track stage bucket counts for the segmented pipeline bar.
  // Denominator (bar total): notStarted + transcribing + transcribedOnly + evaldOnly + embeddedReady.
  // failed is off the bar fill — kept here for agent convenience.
  "notStarted":      42,    // pending (not yet claimed by any runner)
  "transcribing":    1,     // claimed (in-flight, the "transcribing" pulse segment)
  "transcribedOnly": 20,    // done, no eval completion, no embedded chunks
  "evaldOnly":       4,     // done, eval finished, no embedded chunks
  "embeddedReady":   250,   // done, has embedded chunks — the terminal / goal state
  "failed":          2      // status='failed' (off the bar; separate failed-callout)
}
```

`activity` precedence: `paused` → `transcribing` → `evaluating` → `embedding` → `winding-down` → `idle`.
`evalCoverage == -1` means eval is not in-pipeline (not applicable). `winding-down` is the key state the original dashboard missed: transcribe queue drained but GPU still busy (eval / embed catch-up).
No new DB queries — the bucket counts are populated from a single FILTER-aggregate query over `transcription_jobs`.

**Auth**: mutating endpoints require `Authorization: Bearer <CONTROL_API_TOKEN>`
(constant-time compared). When `CONTROL_API_TOKEN` is unset they **fail closed**
with `503` — the pipeline can never be paused/driven by an unauthenticated
caller. Read endpoints are always open. This is layered on the LAN-only ingress.

Single-job smoke test (one call):

```bash
curl -fsS -X POST https://<host>/api/v1/pipeline/run \
  -H "Authorization: Bearer $CONTROL_API_TOKEN" \
  -H 'Content-Type: application/json' -d '{"limit":1}'
```

**Dashboard mutating actions** (`/actions/*`) are the htmx-driven counterpart to
the JSON API above: each is guarded by the `HX-Request` header (so a cross-origin
form can't drive it) and re-renders an htmx fragment rather than returning JSON.
The pipeline-control, findings, and eval actions additionally **fail closed**
(banner, no-op) when `CONTROL_API_TOKEN` is unset, matching the JSON API's
posture — and their buttons render as a disabled affordance rather than a button
that would 503 on click. (`requeue`/`retry-failed`/`book-requeue` stay htmx-only,
unchanged.):

| Method | Path | Auth | Effect |
|--------|------|------|--------|
| `POST` | `/actions/requeue?id=…` | htmx | re-transcribe one job; re-render status fragment |
| `POST` | `/actions/retry-failed` | htmx | re-transcribe all failed jobs |
| `POST` | `/actions/book-requeue?dir=…` | htmx | re-transcribe one book |
| `POST` | `/actions/pause` / `/actions/resume` | htmx + token | toggle the runner pause flag (Pipeline page) |
| `POST` | `/actions/run` (form/query `n≥1`) | htmx + token | arm a bounded run of N claims then auto-pause — sets `run_limit=N` then unpauses (limit before unpause, mirroring `POST /api/v1/pipeline/run`) |
| `POST` | `/actions/runner-update` (form/query `version`) | htmx + token | request the runner self-update to `version` (or clear when empty); writes `desired_runner_version` + `'requested'` and re-renders the `/servers` fragment (the version-skew card) |
| `POST` | `/actions/run-clear` | htmx + token | clear the bounded run (`run_limit→NULL`) without touching the pause flag |
| `POST` | `/actions/eval?dir=…` | htmx + token | run the LLM judge over one book (async, §2.15) |
| `POST` | `/actions/eval-sample?n=N` | htmx + token | run the LLM judge over an N-chunk sample (async, §2.15) |
| `POST` | `/actions/findings-clear[?dir=…]` | htmx + token | **delete** recorded findings (advisory metadata only; §2.15), then re-render the `/findings` fragment. Optional `dir` scopes the delete to one book; absent clears all. Touches only `transcript_findings` — transcripts are never modified, so a clear is always recoverable by re-running eval. |

#### 2.12.1 Dashboard page routes (HTML)

The htmx dashboard's page shells and their data fragments. All are GET and
LAN-only; only reachable under the HTTP transport.

| Path | Purpose |
|------|---------|
| `GET /` | **Home — the Library page** (book list with search, status chips, sort + ⚑ has-findings filter). The `/` route is a catch-all, so an unmatched path 404s. |
| `GET /pipeline` | **Pipeline ops page** — the auto-refreshing status fragment (counts, pipeline state, read-only phase badge, pause + run-budget controls) with the **Failed jobs view folded in** as a second region. |
| `GET /library` | Same Library page as `/` (kept so existing `?status=…` deep links and the book back-link resolve). |
| `GET /library/data` | Library fragment. Query: `status`, `q`, `sort` (`recent`\|`title`\|`progress`\|`findings`), `findings` (`1` → only books with recorded findings), `offset`. Sort + has-findings filter are applied **all-in-Go** over the full filtered set. |
| `GET /status/data` | Status fragment (htmx-refreshed every 3 s): counts, pipeline state, read-only phase badge, and the token-gated pause + run-budget controls. |
| `GET /failed/data` | Failed-jobs fragment. **No standalone `/failed` page** — it renders inside `/pipeline`. |
| `GET /servers` · `/servers/data` | Models/Services page + fragment (§2.14). |
| `GET /findings` · `/findings/data` | Findings page + fragment (§2.15). |
| `GET /book` · `/book/data` | Per-book detail page + fragment. |
| `GET /track?id=…[&t=<startSec>]` | Per-track detail page. The optional **`t`** (seconds) is the finding "Where" deep-jump: the reader preloads pages `[0 .. the page containing the segment spanning t]`, marks that segment active, and scrolls to it. |
| `GET /track/data` · `/track/segments` | Track fragment + reader "load more" page. |

> **The dashboard READS `runner_control.phase` but never WRITES it.** The phase
> badge on every page (topbar) and on the status fragment is read-only; the
> `earmark batch` coordinator owns all phase transitions (§1.4). Only `paused`
> and `run_limit` are dashboard-writable (pause/resume + run-budget actions). A
> phase read error degrades to `idle` and is logged; it never blocks a page.

### 2.13 ASR Backend Capability Vocabulary

earmark supports multiple, swappable ASR backends that vary by **model family**,
**runtime**, **compute type**, and **host**, and that may or may not support a
given **capability**. To compare backends honestly (A/B), the data model records
*which* backend ran and *what capabilities it actually applied* vs what was
requested. This section defines the shared vocabulary.

#### Capability enum (closed)

These are the **only** valid capability keys. Both the Go service
(`internal/asr`) and any runner MUST use exactly these strings. Unknown keys are
**ignored with a warning** (forward-compat — a future earmark release may add
keys; an older consumer drops what it doesn't recognize rather than erroring).

| Key | Meaning |
|-----|---------|
| `word_timestamps` | per-word start/end timestamps in `segments[].words` |
| `context_biasing` | word-boosting / context biasing from `book_metadata.bias_terms` |
| `diarization` | speaker labels (`segments[].speaker`, `words[].speaker`) |
| `confidence_scores` | per-word confidence (`words[].score`) |
| `language_detection` | auto language id vs a fixed language |

Languages are modeled **separately** as a string set (ISO-639-1 codes), not as a
boolean capability — "which languages" is the useful fact. (Config carries an
optional per-server `languages` list; there is no `run_metrics` language-set
column in Phase 1.)

#### Capability JSON shapes (`run_metrics` columns)

Each is a JSONB object whose keys are drawn from the enum above. All three are
nullable and runner-written (SHOULD, §2.4):

```jsonc
// caps_applied — what the runner actually did this run (key → bool)
{ "word_timestamps": true, "context_biasing": false, "diarization": false, "confidence_scores": false }

// caps_requested — what the job asked for (snapshot, key → bool). In Phase 1 the
// runner authors this too (it knows it saw bias_terms / ASR_DIARIZE); Phase 3
// moves authorship to earmark at enqueue time. Omitted keys mean "not requested".
{ "context_biasing": true, "diarization": false }

// caps_skipped_reason — why a *requested* capability was NOT applied (key →
// short human-readable reason). Only keys present in caps_requested-but-declined
// appear here; it drives the dashboard's honest-degradation tooltip.
{ "context_biasing": "parakeet-tdt timestamps break under boosting" }
```

`requested && supported && !applied` is a real, recordable outcome (a backend
that *could* do a thing but declined this run) — that is the difference between
"this backend ignored my bias list" and "this backend can't do bias lists", and
both must remain legible after the fact.

#### Recommended `family` / `runtime` ids (open, not enforced)

`family` and `runtime` are **free-form strings**, not a closed enum — a new
runtime must not require an earmark release. earmark does **not** gatekeep which
families/runtimes exist; unknown values render verbatim on the dashboard.
`internal/asr` carries a *recommended* canonical-id set + a `KnownFamily` helper
purely for nice labels. The table below is the convention runners SHOULD converge
on so the same backend reports the same id everywhere:

| Axis | Recommended id | Notes |
|------|----------------|-------|
| family | `nemo-parakeet` | NVIDIA NeMo Parakeet (TDT/CTC) |
| family | `nemo-canary` | NVIDIA NeMo Canary (AED — context biasing works) |
| family | `granite-speech` | IBM Granite Speech |
| family | `whisper` | OpenAI Whisper / faster-whisper / WhisperX |
| runtime | `nemo-cuda` | NeMo on CUDA (NVIDIA GPU) |
| runtime | `parakeet-mlx` | Parakeet on Apple Silicon (MLX) |
| runtime | `parakeet.cpp` | Parakeet C++ / sherpa-onnx (CPU) |
| runtime | `whisper.cpp-sycl` | whisper.cpp SYCL (Intel iGPU) |
| runtime | `whisper.cpp` | whisper.cpp (CPU) |
| runtime | `openvino` | OpenVINO runtime |

These ids are **recommendations**; a runner may report any string and earmark
stores/displays it as-is.

### 2.14 AI Endpoint Registry

earmark talks to a pluggable registry of AI endpoints for embeddings (and, in
future, chat/generation) tasks. The registry decouples *which endpoints exist*
from *which function uses each one*, so an operator can swap a backend by
changing config, not code. It is configured via the `AI_ENDPOINTS` and
`AI_ROLES` environment variables. The legacy `EMBEDDINGS_BASE_URL` /
`EMBEDDINGS_MODEL` vars (§2.4) remain valid but **deprecated**: when
`AI_ENDPOINTS` is unset they are synthesized into a single `_legacy` embeddings
endpoint bound to the `embeddings` role, so existing deployments keep working
with no change.

#### `AI_ENDPOINTS` (JSON array)

```jsonc
[
  {
    "id": "embed-1",                 // unique within this deployment (required)
    "type": "embeddings",            // "embeddings" | "chat" (required)
    "backend": "ollama",             // "ollama" | "vllm" | "openai-compat" (required)
    "baseURL": "http://ollama:11434/v1", // OpenAI-compatible base (http/https, required)
    "model": "nomic-embed-text",     // model id passed to the API (required)
    "options": { "temperature": "0", "max_tokens": "256" } // optional; string values
  }
]
```

All three backends speak the OpenAI-compatible REST API; `backend` selects the
dashboard label only (no behavioral difference today). `options` keys are
forwarded as-is — known keys are `temperature`, `max_tokens`, `top_p`; unknown
keys are preserved so a future backend needs no code change.

#### `AI_ROLES` (JSON object)

```jsonc
{ "embeddings": "embed-1", "eval": "eval-1" }
```

`embeddings` is **required** when `AI_ENDPOINTS` is set and MUST resolve to an
endpoint of type `embeddings` — it is the endpoint the worker embeds chunks
with. `eval` is **optional** and MUST resolve to a `chat` endpoint when present
(it is reserved for a future read-only eval layer; absent → no eval).

#### Validation (fail-closed)

Unlike `ASR_SERVERS` (cosmetic; warn-and-degrade), a malformed AI registry is a
**startup error** — embeddings is the critical path and a silent degrade would
cause invisible embed failures. earmark refuses to start when:

- `AI_ENDPOINTS` is not valid JSON, or any entry has a missing `id`, duplicate
  `id`, missing `model`, unknown `type`/`backend`, or a `baseURL` that is not a
  valid http/https URL with a host.
- `AI_ENDPOINTS` is set but `AI_ROLES` is absent.
- `AI_ROLES.embeddings` is empty, points at an unknown id, or points at a
  non-`embeddings` endpoint.
- `AI_ROLES.eval` is set but points at an unknown id or a non-`chat` endpoint.

(When `AI_ENDPOINTS` is **absent**, a malformed `EMBEDDINGS_BASE_URL` is not
re-validated — the legacy path preserves the prior behavior.)

#### Health probe + dashboard

Each endpoint is probed for liveness on every Models/Services page refresh and
in `GET /api/v1/status` (§2.12): a `GET <baseURL>/models` request with a 2s
timeout, TTL-cached so both render paths share one upstream call. State tokens:

| Condition | Page label | API `state` |
|---|---|---|
| 200 OK + model present (or empty model list) | `READY` (green) | `ready` |
| 200 OK but configured model not in `/models` | `MODEL NOT LOADED` (amber) | `model_not_loaded` |
| non-200 / timeout / unreachable | `OFFLINE` (grey) | `offline` |
| not probed yet | `UNKNOWN` (grey) | `unknown` |

The Models/Services dashboard page (the former Servers page; URL stays
`/servers`) lists ASR runners and every configured AI endpoint. Observability
only — earmark does **not** route work between endpoints.

---

### 2.15 Eval Layer (read-only LLM judge)

> §2.14 defines the AI endpoint registry (#48/#50). This section (#49)
> documents the read-only eval layer, which binds to it.
>
> **Amended (2026-08-18, reviewable patches).** `suggested_correction` is no
> longer a dead end: a finding is now a **proposed patch** that a human can
> accept and apply (§2.17). The eval layer's read-only guarantee below is
> UNCHANGED and still binding — the judge writes nothing but findings. What
> changed is that a *separate, human-gated* component may act on one. Read
> §2.17 before touching either.

The eval layer is a **read-only LLM-as-judge** (`internal/eval`, `earmark eval`)
that READS transcript chunks and records **suspected** transcription errors as
**proposed patches** — it NEVER edits transcripts itself. The asymmetry is the
whole point: a wrong flag is harmless (triage by confidence, or reject it in
review), a wrong *autonomous correction* would corrupt the corpus, so
`suggested_correction` is recorded and **never applied by the judge**.

**Read-only contract (binding, unchanged):** the eval layer issues no
`UPDATE`/`DELETE`/`ALTER`/`DROP`/`TRUNCATE` against `transcripts`, `segments`, or
`transcript_chunks`. Its only write is `INSERT INTO transcript_findings`. The
findings table carries no foreign key that cascade-mutates the transcript tables.

This is enforced structurally, not by convention: the apply path lives in
`internal/patch`, a different package, so `internal/eval` contains no transcript
write at all and its SQL guard test continues to assert exactly that. If you
find yourself adding an `UPDATE` to `internal/eval`, you are in the wrong
package.

#### Env vars

The chat endpoint is resolved in priority order:

1. `AI_ROLES["eval"]` bound to a `chat` entry in `AI_ENDPOINTS` (preferred — see §2.14).
2. Standalone `EVAL_CHAT_*` env vars (fallback when no `eval` role is bound).

The call uses the OpenAI-compatible `POST {base}/chat/completions` shape.

| Env var | Required | Meaning |
|---------|----------|---------|
| `EVAL_CHAT_BASE_URL` | if no `eval` role in `AI_ROLES` | OpenAI-compatible base URL, e.g. `http://vllm:8000/v1` |
| `EVAL_CHAT_MODEL` | if no `eval` role in `AI_ROLES` | judge model id |
| `EVAL_CHAT_API_KEY` | no | bearer token if the endpoint requires one |

#### `transcript_findings` table

```sql
CREATE TABLE transcript_findings (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    transcript_id        UUID        NOT NULL,   -- no cascade-mutate into transcripts
    file_path            TEXT        NOT NULL,
    chunk_id             UUID,                    -- the evaluated chunk (nullable)
    chunk_index          INTEGER,
    start_sec            FLOAT8      NOT NULL,
    end_sec              FLOAT8      NOT NULL,
    original_text        TEXT        NOT NULL,    -- the suspected-wrong span, verbatim
    issue_type           TEXT        NOT NULL,    -- see vocabulary below
    suggested_correction TEXT,                    -- ADVISORY ONLY — never applied
    confidence           FLOAT8      NOT NULL,    -- judge self-score 0..1 (the triage/scoring signal)
    model                TEXT        NOT NULL,    -- judge model id (attribution)
    transcription_run_id UUID,                    -- transcription_jobs.id — per-backend/run attribution
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- indexes: file_path, transcript_id, transcription_run_id, issue_type
```

`issue_type` is a closed vocabulary the judge prompt advertises; an unknown value
returned by the model is coerced to `other`:

| `issue_type` | Meaning |
|--------------|---------|
| `misheard_proper_noun` | a name/place/brand/title mis-recognized (e.g. "auto sebo" → "Arecibo") |
| `misheard_word` | an ordinary (non-name) word/phrase mis-recognized, or words wrongly fused/split (e.g. "Placenes" → "place names") |
| `repeated_text` | a word/phrase accidentally duplicated, visible in the span (e.g. "the the" → "the") |
| `number_artifact` | a number/date/unit that came out wrong (NOT numeral-vs-spelled-out style) |
| `homophone` | wrong word, right sound (e.g. "pin name" → "pen name") |
| `dropped_word` | likely omission leaving the sentence broken |
| `other` | coercion sink for an unknown model value — the prompt instructs the judge **never** to choose it |

> **Taxonomy rev 2 (2026-06).** `run_on` was removed: an audit found it over-fired
> on normal long sentences (the genuine, detectable subset — literal duplication —
> is now `repeated_text`). `misheard_word` was added so non-proper-noun
> mis-recognitions get a real category instead of `other`. A model emitting
> `run_on` (or any retired value) coerces to `other`. **Every finding now requires
> a non-empty `suggested_correction`** — the prompt mandates it and the parser
> drops findings without one (the dominant noise class was "flagged, no fix").

#### Sampling / cost

The judge is **sampled, on-demand, or in-pipeline** — never an unbounded
always-on pass: `earmark eval "<book>"` evaluates one book; `earmark eval
--sample N` judges N random chunks library-wide; and with `EVAL_IN_PIPELINE` the
embed worker evaluates each transcript's chunks before embedding (bounded by the
batch the coordinator drives). The unit of evaluation is the **chunk**. The CLI
is dry-run by default (prints what it
would record) and persists only with `--write` (alias `--yes`); the in-pipeline
path always persists.

**Gated mode (`EVAL_GATES_EMBED=true`).** When the gate is enabled, eval is
mandatory per-track (not optional/sampled) for any transcript to become
searchable. Cost is still bounded: the batch coordinator drives Phase B in batches
of `--batch-size` tracks per round, so the eval judge processes exactly as many
transcripts as the current batch contains. The gate does NOT change how many
tracks are evaluated per judge call — it changes WHEN (before embed, not after)
and WHAT happens if the judge is unavailable (startup fatal, not skip).

**Fail-closed matrix.**

| `EVAL_GATES_EMBED` | `EVAL_IN_PIPELINE` | Eval judge configured | Behavior |
|---|---|---|---|
| `false` (default) | `false` (default) | — | Today's behavior: eval on-demand only |
| `false` | `true` | yes | Best-effort inline eval before embed (judge nil → logged-skip) |
| `false` | `true` | no | Judge nil (warn at startup), embed proceeds normally |
| `true` | `true` | yes | Gated two-pass: eval pass → embed pass |
| `true` | `true` | no | **Fatal startup error** — gate on without a resolvable judge endpoint is a misconfiguration |
| `true` | `false` | — | **Fatal startup error** — the gate makes eval a strict prerequisite for embed, but the eval judge is only built when `EVAL_IN_PIPELINE=true`; reaching the worker with gate=true + judge=nil would stall the corpus (or risk a nil-judge deref), so startup fails closed |

The gate therefore **requires both** an eval judge endpoint **and**
`EVAL_IN_PIPELINE=true` — the two left-hand `true`-gate rows that lack either
are fatal, so a worker is never constructed with `EvalGatesEmbed=true` and a
nil judge.

**`earmark eval --backfill-unevaluated`** is the one-time migration command for
existing deployments enabling the gate. It selects done jobs with
`eval_finished_at IS NULL` regardless of embed state, judges them, and writes
`eval_finished_at`. It is safe over live embedded data: it only INSERTs findings
and UPSERTs the eval slice — it does NOT touch `transcript_chunks` or
`transcripts`. Run with `--write` to persist; omit for a dry-run preview. Cost is
bounded per `--sample N` / `--limit N` flags. After the command completes,
`embedded ⟹ eval'd` holds for the existing corpus.

**Noise filters (applied per chunk, before persistence).** Two precision filters
trim the judge's over-flagging, in this order:

1. **Confidence floor.** Findings below `EVAL_MIN_CONFIDENCE` (default **0.6**,
   `<= 0` disables) are dropped. A ground-truth audit found high-confidence
   findings were ~100% real while the low tail was mostly noise, so the floor
   trades a little recall for precision.
2. **Per-chunk cap.** The survivors are capped at `EVAL_MAX_FINDINGS_PER_CHUNK`
   (default **5**, `<= 0` disables) — when a chunk exceeds the cap the
   **highest-confidence** findings are kept and the remainder dropped (logged at
   DEBUG).

Both are per-chunk and applied before persistence, so they bound noise without
affecting how many chunks are evaluated. (Findings with an empty
`suggested_correction` are dropped earlier, at parse time — see the vocabulary
note above.)

**Clearing.** Findings only accumulate (re-running eval appends); the `/findings`
dashboard page exposes a token-gated **clear findings** button (and the
`POST /actions/findings-clear` action, §2.12) that deletes recorded findings.
This is the one place the findings subsystem `DELETE`s — and it deletes ONLY
`transcript_findings`, never the transcript tables, so the read-only-transcripts
contract holds and a clear is recoverable by re-running eval.

#### Dashboard surfaces (read-only)

The `/findings` page and the per-book Book section both render the **individual
finding rows** (the triage worklist: confidence, issue type, `original →
suggested correction`, and where), not just the per-book roll-up counts — sorted
by confidence DESC. Rows link to the **book** they belong to (`/book?dir=…`); the
deeper track-segment jump is deferred. The Book page's per-book section also
exposes the scoped clear (`POST /actions/findings-clear?dir=…`, token-gated,
re-renders the Book fragment). All of this is read-only/advisory surfacing — no
new route, env var, or column; informational only.

#### Two payoffs

1. **Quality observability** — error counts/types and a confidence spread per
   book, on the read-only `/findings` dashboard page.
2. **Backend eval harness** — `transcription_run_id` attributes each finding to
   the ASR run (hence backend) that produced the transcript, so running the same
   judge over Parakeet vs Whisper vs Granite output yields a comparative quality
   metric; `confidence` is the scoring signal. This is the measurement the
   deferred multi-backend A/B needs.

The judge has false positives and misses; because findings are advisory-only
that is harmless. Track judge precision over time by spot-checking high-confidence
findings.

---

### 2.16 Prometheus metrics

Both Go pods expose a Prometheus `/metrics` endpoint (the mcp pod mounts it on
its existing `:8081` mux; the ingest pod on its `INGEST_HTTP_ADDR` listener,
§2.4). The surface is **gauges/counters only — NO per-job series** (high-
cardinality per-job history belongs in Postgres/Grafana, not Prometheus). These
metric **names are load-bearing** — the deployment's alert rules, dashboards,
and scrape config depend on them verbatim; do not rename without updating
those consumers.

The current-state gauges are produced by a scrape-time collector that reads the
DB on each scrape (always fresh, no refresh goroutine). The counters and the
histogram are incremented at the Go-emitted pipeline event sites.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `earmark_jobs` | gauge | `status` (`pending`/`claimed`/`done`/`failed`) | Current `transcription_jobs` count by status. |
| `earmark_embed_backlog` | gauge | — | Completed transcripts with no chunks yet (the embed worker's needs-embedding set). |
| `earmark_eval_coverage_ratio` | gauge | — | Done jobs judged (`run_metrics.eval_finished_at` non-NULL) ÷ done jobs; `0` when no done jobs. |
| `earmark_runner_last_heartbeat_seconds` | gauge | — | Seconds since the runner's last **claim-activity**. The runner only stamps a heartbeat while a job is claimed (no idle heartbeat, §1.7), so this is NOT idle liveness and CANNOT distinguish "idle, queue empty" from "down". **Omitted entirely when there is no claim/completion history** (so an alert can't misread a multi-day age). Pair with `earmark_jobs{status="pending"}+earmark_jobs{status="claimed"}>0` before alerting. |
| `earmark_runner_alive_seconds` | gauge | — | Seconds since the runner's last **liveness** heartbeat (`runner_control.runner_heartbeat_at`), stamped **every** poll cycle — working, idle, or paused. Unlike `earmark_runner_last_heartbeat_seconds` this stays fresh on a drained queue, so it distinguishes "idle, queue empty" (small value) from "runner down" (large/growing value). **Omitted until the runner has stamped at least once.** |
| `earmark_runner_available` | gauge | — | `1` when the GPU host is free for transcription (gpu-arbiter not gaming), `0` when gaming/evicting. Omitted until a `runner_availability` event has been observed. |
| `earmark_stage_duration_seconds` | histogram | `stage` | Per-stage processing duration, observed at Go-emitted finish events (`embed`, `eval`). |
| `earmark_jobs_completed_total` | counter | — | Go-observable embed-stage completions (best-effort — the **runner** owns the job `done` transition, so this counts the worker's embed finishes, not the runner's mark-done). |
| `earmark_jobs_failed_total` | counter | — | Go-observable job failures (the stale-claim attempt-cap path). Best-effort — runner-side failures are not counted here; `earmark_jobs{status="failed"}` is the authoritative current failed count. |
| `earmark_eta_work_seconds` | gauge | — | Empirical busy-time ETA for the remaining chunks (§4). Omitted when there is no remaining work / no rate history. |
| `earmark_eta_calendar_seconds` | gauge | — | Empirical calendar ETA (work ÷ runner-availability fraction, §4). Omitted when availability history is absent. |

Standard `go_*` and `process_*` collectors are also registered for baseline
observability. Both pods also serve `/healthz` (liveness, always-200).

---

### 2.17 Reviewable Patches (human-gated apply)

> Added 2026-08-18. This section is the counterpart to §2.15: it defines how a
> finding stops being a note and becomes an edit. §2.15's read-only guarantee is
> unchanged — everything here happens in `internal/patch`, never in
> `internal/eval`.

A finding is a **proposed patch**. The judge proposes; a human disposes. No
transcript text changes without an explicit, recorded human decision on a
specific finding.

#### Lifecycle

`transcript_findings.patch_state` holds the state. Legal transitions are
centralised in `patch.CanTransition` so the DB layer and the UI cannot disagree:

```
proposed ──accept──> accepted ──apply──> applied ──revert──> reverted
    │                    │                   │                   │
    └──reject──> rejected┘                   │                   └──> proposed
                    │                        │
                    └──> proposed            └──> stale
```

- **proposed** — the judge's output. Existing rows migrate here by default.
- **accepted** — a human approved it; in the overlay, not yet reflected in the
  projection (the chunk is flagged `embedding_stale`).
- **applied** — the correction is reflected in the current projection. Both
  `accepted` and `applied` replay on every rebuild — see "Applying" below.
- **rejected** — a human declined it. May return to `proposed` for reconsideration.
- **reverted** — an applied correction was withdrawn; the next rebuild simply
  stops replaying it.
- **stale** — the correction can no longer be replayed (the chunk changed, or
  its anchor no longer resolves), so it describes text that no longer exists.
  **Terminal**: re-run the judge rather than resurrecting it. Stale findings stay
  visible; they are never deleted.

`proposed → applied` is deliberately **illegal**. Reaching `applied` requires
passing through `accepted`, which is the human gate; skipping it would be an
autonomous correction, which is exactly what §2.15 forbids.

#### Anchoring

`original_text` alone cannot locate an edit — a span like `the fox` may occur
several times in one chunk, and patching the wrong occurrence is corpus
corruption that reads as a successful edit. Three additive columns fix that:

| Column | Meaning |
|---|---|
| `anchor_offset` | 0-based **rune** index of the span in the chunk text. NULL = model did not say. |
| `anchor_occurrence` | 0-based index among identical spans. NULL = model did not say. |
| `chunk_text_sha256` | Fingerprint of the chunk **as the judge saw it**. |

Offsets are rune-indexed, not byte-indexed, so an offset can never split a
multi-byte character and the numbers mean the same thing to the review UI as to
the apply path.

`patch.Locate` resolves them most-precise-first: offset hit → occurrence hit →
unique-match recovery (for legacy rows with no anchor) → **refuse**. The refusal
is load-bearing: an ambiguous anchor yields `ErrAnchorAmbiguous` and the patch is
never applied on a guess.

Before replaying, the apply path re-hashes the chunk's **pristine** text and
compares to `chunk_text_sha256`. A mismatch means the chunk changed after the
judge reviewed it, so the patch is marked `stale` rather than applied to text no
model ever saw.

Double-application is impossible by construction rather than by hash: every
rebuild starts from the pristine `source_text` and replays the whole overlay, so
replaying an already-applied correction reproduces the same bytes instead of
compounding. (Replaying onto already-corrected text — which the projection never
does — is separately refused by the hash check.)

#### Applying: the replayable correction overlay

> **Revised 2026-08-19.** The earlier model — "an applied patch rewrites
> `transcript_chunks.text`" — was **wrong and is replaced by this section**. It
> could not work: the embed worker regenerates every chunk from
> `transcripts.segments` (falling back to `raw_text`) on each embed and upserts
> over the existing rows by deterministic UUID, so a correction written into
> `transcript_chunks.text` is destroyed by the next re-embed. Chunks can never
> be authoritative storage for corrections.

Chunking is therefore defined as:

```
regenerate from source  →  replay applied corrections  →  embed
```

##### Three layers, one writer each

| Layer | Tables | Writer | Mutability |
|---|---|---|---|
| Source | `transcripts.segments`, `transcripts.raw_text` | ingest (the ASR runner), once | **immutable** |
| Decisions | `transcript_findings` | human accept/reject | append-only; only `patch_state` transitions |
| Projection | `transcript_chunks` + embeddings | the chunker | **disposable, rebuildable** |

A correction lives in the *decisions* layer and is **replayed** onto the
projection. It is never stored in the projection, because the projection is
thrown away and rebuilt.

##### `source_text` — why a chunk carries two texts

`transcript_chunks.source_text TEXT` (nullable, additive) holds the **pristine
regenerated** chunk — the projection's input. `text` holds
`ApplyCorrections(source_text, overlay)` — the embedded, searched, and displayed
surface. Legacy rows have `source_text IS NULL` and are read as
`COALESCE(source_text, text)`, which is correct because a legacy row has no
corrections applied.

The split exists because of a hard constraint: **the judge must always be shown
pristine text.** It records `anchor_offset`/`anchor_occurrence` and
`chunk_text_sha256` against exactly the text it was given, and replay always
starts from the pristine regenerated text. Judge the corrected surface and every
new finding is born `stale` — its hash could never match the projection's input.
So `evalChunkSelectSQL` (which feeds `earmark eval`) selects
`COALESCE(c.source_text, c.text)`, and the worker's eval passes never replay the
overlay.

##### Replay order is load-bearing

All patches on one chunk were judged against the same revision, so they share
one coordinate system. Replay therefore resolves **every** span against the
pristine input text (never against a partially-patched string), then splices in
**descending start-offset order**, tiebroken by end offset descending then
finding id ascending.

Descending is not a preference. Splicing left-to-right invalidates every later
offset the moment an earlier replacement changes length — a silent corruption
that yields plausible text and no error. Applying from the end backwards leaves
every not-yet-applied span's indices untouched.

Two corrections claiming **overlapping** spans have no well-defined composition
and are refused rather than silently ordered.

##### Byte-identical rebuild

`patch.ApplyCorrections` is pure — no database, no I/O, no clock. Same pristine
text plus same patch set produces a **byte-identical** result on every rebuild
and in any input row order. That property is what makes `transcript_chunks` safe
to throw away: a rebuild cannot make the searchable corpus drift.

##### Stale rule (never delete a finding)

`patch.Replay` is the lenient wrapper the projection uses. A correction that
fails the hash check, the empty-correction check, anchor resolution, or the
overlap check is **quarantined**, not dropped: it is returned with a reason and
written back as `patch_state = 'stale'` plus `stale_reason` ∈

`chunk_changed` · `anchor_not_found` · `anchor_ambiguous` · `overlapping_patch` ·
`empty_correction`

A stale finding **stays visible** — it is an `UPDATE`, never a `DELETE`. Every
correction ends up either applied or explicitly retired; none is silently lost.
`stale` remains terminal (re-run the judge rather than resurrecting it).

##### Invalidation, the rebuild trigger, and the watermark

Accepting or reverting a finding sets `transcript_chunks.embedding_stale = true`
for that one chunk, in the same transaction as the decision.

**The rebuild trigger.** The embed worker runs a **rebuild pass** each cycle over
transcripts with at least one flagged chunk, and puts them back through the
normal regenerate → replay → embed path (`GetTranscriptsWithStaleChunks` →
`embedTranscript`). This pass is what closes the loop, and it is load-bearing:
the other three worker selections all require a transcript to have **no** chunks
yet, and findings only ever exist for transcripts that are already embedded — so
without it an accepted correction would sit in the overlay forever and never
reach the searchable text. The pass is bounded by `EMBED_BATCH_SIZE` like every
other selection, and it does **not** run the judge: per this section a
stale-driven re-embed does not require re-judging, and the eval gate's
`run_metrics.eval_finished_at` latch is untouched.

**Ordering.** The replay outcome (`applied` / `stale` / clearing the flag) is
computed during the rebuild but persisted **only after `InsertChunks` succeeds**.
A failed embed or insert means the projection was never written, so recording
corrections as applied — or retiring one to the terminal `stale` state — would
describe something that never happened. Same rule the in-pipeline findings
already follow.

**The watermark.** `InsertChunks` deliberately does **not** clear
`embedding_stale`. An unconditional clear there is a lost update: a human accept
committing between the rebuild's overlay read and its insert sets the flag, and
the clear would wipe it — that correction would never be replayed and never be
re-flagged. Instead `GetCorrectionOverlay` returns the server-side timestamp its
read was taken at, and the clear is guarded: a chunk is only unflagged when it
carries no finding `decided_at` later than that watermark **and** no finding
still sitting in `accepted`. A blocked chunk simply gets rebuilt again next
cycle, so the failure mode is one redundant re-embed rather than a lost
correction. (The second condition also makes a failed best-effort
`MarkFindingsApplied` self-healing.) Because the watermark is what protects a
concurrent decision, **revert stamps `decided_at` too** — not only accept and
reject.

Invalidation is **exactly one chunk**, and this is a property of the schema
rather than a lucky accident: chunks store their text denormalized and carry no
absolute character offsets into the transcript, so a correction in one chunk
never shifts another's anchors. A correction therefore costs one re-embed, not a
corpus re-embed.

`embedding_stale` exists because `transcript_chunks.embedding` is
`VECTOR(768) NOT NULL` — staleness cannot be signalled by nulling the vector.

**Interaction with `EVAL_GATES_EMBED` (§2.15):** the gate latches on
`run_metrics.eval_finished_at`, which is per-job and unaffected by applying a
correction. A re-embed triggered by `embedding_stale` is therefore independent
of the eval gate and does not require re-judging the job.

**Fail closed.** If the overlay cannot be read, the worker does **not** embed
pristine text as though it were corrected — in the corpus that is
indistinguishable from "there were no corrections". It logs and returns an
error, leaving the transcript for the next cycle.

#### Reversibility

Revert is **"flip `patch_state` and rebuild the projection"**. There is no text
swap: the next rebuild regenerates from source and replays an overlay that no
longer contains the reverted correction, so the correction simply stops being
applied. Reverting sets `embedding_stale` on the affected chunk (which the
rebuild pass then picks up) and stamps `decided_at`/`decided_by` like any other
human decision.

`applied_at` plus `applied_before_text` / `applied_after_text` remain an **audit
trail** of what landed, with one **semantic narrowing**: they now hold the
**span-level** before/after (the located span and its replacement), not the
whole chunk. Whole-chunk before/after became ill-defined once a chunk can carry
several corrections — there is no single "before" that attributes a difference
to one finding.

`decided_at` / `decided_by` record who made a human decision — accept, reject,
**or revert** — and `stale_reason` records why a correction was retired. The
machine transitions (`applied`, `stale`, written by the embed worker) never
stamp them, so the record of who approved a correction is not overwritten by a
rebuild.

State transitions go through `patch.CanTransition` at the DB boundary: an
illegal move (notably `proposed → applied`, which would skip the human gate) is
refused before any SQL runs, and the `UPDATE` is guarded on the expected current
state so a concurrent decision cannot be clobbered.

#### What is deliberately NOT edited

`transcripts.segments` and `transcripts.raw_text` are the **immutable ASR
record** — what the recognizer actually produced. They are provenance, no Go
code writes them, and corrections never touch them. The findings table is the
audit trail of divergence between the ASR output and the reviewed text.

> **Resolved (was: "`raw_text` can drift from the chunks").** Under the overlay
> model `raw_text` cannot drift, because nothing ever writes it. Corrections are
> not stored in any text column: they are replayed onto a projection that is
> regenerated from `segments`/`raw_text` every time. The two can no longer
> disagree about a correction, because only one of them ever carries one.

##### Reader audit — who wants ORIGINAL text, who wants CORRECTED

| Site | Wants | Status |
|---|---|---|
| `worker.processTranscript` / `evalTranscript` / `embedTranscript` empty-guards | ORIGINAL | `raw_text` read only as an emptiness guard |
| `worker.chunkTranscript` (segments + raw-text fallback) | ORIGINAL | it **is** the regeneration step |
| `cmd/eval --backfill-unevaluated` | ORIGINAL | the judge must see pristine text |
| `db` transcript SELECTs (feed the chunker) | ORIGINAL | correct as-is |
| `evalChunkSelectSQL` (feeds `earmark eval`) | ORIGINAL | selects `COALESCE(c.source_text, c.text)` |
| search / `transcript_chunks.text` | CORRECTED | the overlay is replayed before embedding |
| MCP `get_transcript`, web transcript reader (`GetTrackDetail` → segments) | CORRECTED | **KNOWN GAP** |

> **Known gap (accepted, documented, not fixed here).** The MCP `get_transcript`
> tool and the web transcript reader render `transcripts.segments`. A reader
> genuinely wants corrected text there — but segments are immutable provenance
> and carry no corrections, so **search shows corrected text while the reader
> shows uncorrected ASR.** Closing the gap means projecting corrections onto
> segments at render time (segments have no chunk anchors today). Mutating
> segments to "fix" it is forbidden: it would destroy the provenance record.

#### The review surface (MCP tools)

> Added 2026-08-19. The lifecycle above is enforced in `internal/patch` and
> `internal/db`; this subsection documents the MCP tools (`internal/mcp`) that
> reach it — the surface an agent or host actually calls. Full parameter tables
> are in §2.2.1 and `internal/mcp/README.md`.

Three tools, three layers written:

| Tool | Writes |
|---|---|
| `list_transcript_corrections` | nothing — a worklist query joining `transcript_findings` to the chunk's pristine text |
| `decide_transcript_correction` | `transcript_findings.patch_state` (+ `decided_at`/`decided_by`); `transcript_chunks.embedding_stale` on accept/revert |
| `create_transcript_correction` | a new `transcript_findings` row at `patch_state='accepted'`; `transcript_chunks.embedding_stale` |

**Provenance: the `origin` column.** `transcript_findings.origin TEXT NOT NULL
DEFAULT 'judge'`, `CHECK (origin IN ('judge','human'))`, added by an additive
`ADD COLUMN IF NOT EXISTS` migration in `initialize()` (`internal/db/db.go`). A
hand-authored edit and a judge finding are structurally identical rows — same
table, same anchor columns, same replay — so without provenance recorded *in the
row* the two are indistinguishable after the fact, and "judge precision"
silently starts counting decisions a person already made instead of the judge's
own guesses.

Consequence for readers: **any metric or dashboard that measures the judge must
filter `origin = 'judge'`.** The existing `/findings` dashboard and
`GetFindingsSummary` do **not** filter yet — this is a known, deferred gap, not
an oversight this section is pretending away. A human-authored correction
recorded through `create_transcript_correction` currently inflates those
counts/rollups as if the judge had proposed it.

**The direct-edit path**, as `create_transcript_correction` runs it:

1. Read the chunk's pristine text — `COALESCE(source_text, text)` — never the
   corrected surface.
2. Verify with `patch.PlanDirectEdit`: non-empty correction → chunk-hash match
   → unambiguous anchor via `patch.Locate` → no overlap with the chunk's
   existing overlay (`GetCorrectionOverlay`/`BuildOverlay`, the same overlay the
   rebuild reads — there is only one definition of "the overlay").
3. INSERT the finding at `patch_state='accepted'`, with `decided_at`/`decided_by`
   stamped in the same statement. The INSERT is an `INSERT … SELECT` over the
   chunk row — `transcript_id`, `file_path`, `chunk_index`, and the timings come
   FROM THE CHUNK, so a caller cannot file a correction against one chunk while
   labelling it another — and the statement **re-checks the chunk fingerprint in
   SQL** (`encode(sha256(convert_to(COALESCE(c.source_text, c.text),
   'UTF8')),'hex') = $12`), so a rebuild racing the edit between verification and
   insert matches zero rows and the edit is refused instead of anchoring to text
   nobody actually verified.
4. Flag `transcript_chunks.embedding_stale` in the **same transaction** as the
   INSERT — a correction recorded without its invalidation would leave the
   corpus permanently showing text that disagrees with its own findings.

**Why `accepted`, not `proposed`.** A direct edit skips the `proposed` state
because there is nothing to propose — the edit itself *is* the human decision,
made by the person calling the tool. It still does not skip `applied`: that
state is reached only by a successful rebuild replaying the overlay (see
"Applying" above), so the audit trail keeps "a human approved this" and "this is
in the searchable corpus" as two distinct, separately-observable facts.

**Reserved sentinels: `model='manual'`, `confidence=1`.** Both columns are
`NOT NULL` on `transcript_findings` and mean "which judge produced this, and its
self-reported score" — a direct edit had no judge, so it needs values that are
honest about that rather than a borrowed model id that would corrupt per-model
attribution, or a confidence that implies uncertainty a human decision doesn't
carry.

**Pristine, always.** Both `list_transcript_corrections` and
`create_transcript_correction` resolve anchors, and return context, against the
chunk's pristine text (`COALESCE(source_text, text)`) — never the corrected
surface. An anchor taken from corrected text would record a fingerprint the
projection's input (the pristine regenerated chunk) can never match, so the
correction would be born `stale` before a reviewer ever saw it.

## 3. SCHEMA — pgvector chunks table

The Go service reads completed transcripts, chunks them, and embeds each chunk.
Chunks are stored alongside the transcripts in the same database.

> **Schema init is serialized by an advisory lock (binding for new processes).**
> Every earmark process runs the same idempotent schema-init transaction on
> startup — today that is `earmark-ingest` and `earmark-mcp`, which Kubernetes
> rolls together, so they execute the identical DDL block concurrently.
> Concurrent DDL **deadlocks**: observed in production 2026-08-14 (during
> `CREATE FUNCTION`, "while updating tuple in relation `pg_proc`") and again
> 2026-08-19 (during `DROP TRIGGER`). The loser crashed and recovered on
> restart, so the symptom was a crash-loop on deploy rather than data loss —
> survivable only because every statement is `IF NOT EXISTS`-guarded.
>
> `initialize()` therefore takes `pg_advisory_xact_lock` as the **first**
> statement in its transaction, before any DDL. The second process waits
> instead of racing. The lock is transaction-scoped, so COMMIT, ROLLBACK, or a
> process crash all release it — there is no unlock path to forget.
>
> **If you add a third process that touches this database, it must run the same
> schema-init (and therefore take the same lock) or reintroduce the deadlock.**
> Two constraints are load-bearing and are covered by tests in
> `internal/db/schema_lock_test.go`: the key must be the same constant in every
> process, and the lock must stay `pg_advisory_xact_lock` — the session-scoped
> `pg_advisory_lock` would survive COMMIT on a *pooled* connection and block
> every later initialization permanently.

```sql
CREATE TABLE transcript_chunks (
    id           UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    transcript_id UUID       NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
    file_path    TEXT        NOT NULL,   -- denormalized for query convenience
    chunk_index  INTEGER     NOT NULL,   -- ordinal position within transcript
    start_sec    FLOAT8      NOT NULL,   -- earliest segment start in this chunk
    end_sec      FLOAT8      NOT NULL,   -- latest segment end in this chunk
    text         TEXT        NOT NULL,   -- CORRECTED surface: source_text + replayed overlay (§2.17)
    source_text  TEXT,                   -- PRISTINE regenerated text; NULL on legacy rows (§2.17)
    speaker      TEXT,                   -- dominant speaker in chunk, or NULL
    embedding    VECTOR(768) NOT NULL,   -- nomic-embed-text dimension, MUST match EMBEDDINGS_MODEL
    embedding_stale BOOLEAN  NOT NULL DEFAULT false,  -- set on accept/revert, cleared on rebuild (§2.17)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transcript_chunks_transcript_chunk_unique UNIQUE (transcript_id, chunk_index)
);

CREATE INDEX transcript_chunks_embedding_idx
    ON transcript_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX transcript_chunks_file_path_idx ON transcript_chunks (file_path);
```

Chunk size target: **512 tokens** (Go tokenizer), overlap: **64 tokens**.
These are implementation constants in the Go chunker, not a DB concern.

`transcript_chunks` is a **derived projection**, not storage: the embed worker
regenerates every row from `transcripts.segments`/`raw_text` and upserts over
the existing rows. Nothing durable may live only here — see §2.17 for why
corrections are replayed onto it rather than written into it, and what
`source_text` / `embedding_stale` are for.

---

## 4. CHANGE CONTROL

Any change to:
- A column name, type, or constraint in sections 1.1, 1.2, 1.5, 1.6, or 3
- An env var name in section 2.4
- The mcp-proxy upstream key or URL in section 2.2
- The embedding model or vector dimension in section 2.3
- The capability enum or the `caps_*` JSON shapes in section 2.13
- The `AI_ENDPOINTS` / `AI_ROLES` JSON shapes or role names in section 2.14

...requires updating this file **before** writing implementation code. All
three components (this Go service, the ASR runner, the deployment manifests)
must be updated atomically when a contract value changes.
