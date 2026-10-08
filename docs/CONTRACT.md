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
    recipe_id           TEXT        REFERENCES recipes (recipe_id),  -- asr recipe (§1.9); NULL = unstamped
    -- Runner-reported provenance + identity (migration 00006); all NULL from a
    -- runner that predates it.
    embedded_asin       TEXT,       -- ASIN tag embedded in the audio (ffprobe), validated; §1.6
    asr_model_sha256    TEXT,       -- sha256 of the .nemo file the runner loaded → recipe model_revision
    asr_runner_version  TEXT,       -- runner version tag → recipe code_version
    asr_params          JSONB,      -- runner output-shaping settings → recipe params

    CONSTRAINT transcripts_job_id_unique UNIQUE (job_id)
);

CREATE INDEX transcripts_file_path_idx ON transcripts (file_path);
-- Full-text search on raw_text using pg_trgm (enable via: CREATE EXTENSION IF NOT EXISTS pg_trgm)
CREATE INDEX transcripts_raw_text_trgm_idx ON transcripts USING gin (raw_text gin_trgm_ops);
CREATE INDEX transcripts_asr_unstamped_idx ON transcripts (created_at)
    WHERE recipe_id IS NULL AND asr_runner_version IS NOT NULL;
```

**Runner-reported provenance (SHOULD, migration 00006).** A runner writes four
more columns on its `INSERT`:

| Column | Value |
|---|---|
| `embedded_asin` | The ASIN tag in the audio's ffprobe format (then stream) tags — key `ASIN`, `AUDIBLE_ASIN`, or a freeform `----:<mean>:ASIN`, case-insensitive — upper-cased, kept only if it has a catalogue-id shape (`B0` + 8 alphanumerics, 9 digits + `X`, or 6+ digits). NULL otherwise. The third ASIN source (§1.6). |
| `asr_model_sha256` | Lowercase hex sha256 of the `.nemo` file the model was loaded from (located in the local Hugging Face cache with `try_to_load_from_cache(<repo>, <name>.nemo)`, as NeMo's `from_pretrained` does; hashed once per process, streamed). NULL when it is not cached there (e.g. an NGC-hosted model). |
| `asr_runner_version` | The runner's version tag (`RUNNER_VERSION`). Its presence is what marks a row as provenance-bearing. NULL when the runner cannot tell its version (`unknown`), so the row is not stamped: no recipe is invented. |
| `asr_params` | A JSON object of the settings that shape output: `backend`, `compute_type`, `chunk_threshold_seconds`, `chunk_window_seconds`, `chunk_overlap_seconds`, `segment_gap_seconds`, `segment_max_seconds`, `segment_max_words`, `diarize`, `biasing_enabled`, `language`, and `biasing_alpha` when biasing is on. Configuration only — per-job inputs (a book's bias terms) are not recipe. |

The runner probes `information_schema` for these columns and falls back to the
original nine-column `INSERT` when they are absent, so a runner that updates
before earmark migrates keeps working. The Go worker turns them into the asr
recipe (§1.9).

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
`/tmp/earmark-asr-busy`). When this file exists, the runner skips claiming
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
     (select done, not-attempted, not-embedded → judge → write `eval_finished_at`
     on success, or the `eval_failed_*` record on failure — §1.5), then an
     embed pass (select done, eval'd-or-failed, not-embedded → embed). Phase B
     completes when **both** the eval backlog (`EvalBacklog`: done,
     not-attempted, not-embedded count) AND the embed backlog are 0. Both passes'
     selections are **bounded by `EMBED_BATCH_SIZE` (§2.4, default 32) with
     `ORDER BY t.created_at ASC LIMIT $1`** so the worker never loads an
     unbounded transcript backlog (and its `segments` JSONB) into memory at once;
     it drains across cycles, oldest-first, looping immediately on a full batch.

**Robustness contract:** the coordinator **always restores `phase='idle'` and
the pre-batch `run_limit`** on exit — normal completion, error, AND
`SIGINT`/`SIGTERM` — so it never gets stuck mid-phase and never leaves the
runner budget-gated. At startup it swaps `run_limit` to `0` and keeps the old
value, in one transaction under the same `runner_control` row lock
(`FOR UPDATE`) the runner's claim takes, so the runner cannot spend the
operator's budget while the coordinator waits (e.g. yielding to a game). The
captured value is put back on exit when the run started from `phase` idle and `run_limit` was `NULL` or `> 0`
(an operator's bounded run in progress). Otherwise — `run_limit=0` (a spent
budget), a non-idle starting phase (residue of an interrupted run), or an
unreadable control row — it restores `NULL` (unlimited, normal continuous
operation). The final log line states the restored value. `paused` is never
touched. (Earlier versions set `run_limit=0` on exit; with nothing re-running the
coordinator, that left the runner logging `Run limit reached (run_limit=0)`
indefinitely and new books never transcribed.)
It is **DB-driven and resumable**: it holds no critical state in memory and
derives everything (current phase, job counts, backlog) from the DB. On restart
it reconciles — if it finds `phase='analyze'`, it finishes Phase B before
starting a new Phase A. If a game starts mid-batch, gpu-arbiter stops the runner
and judge; the coordinator's per-batch yield-check handles re-entry and the
existing stale-job recovery (§1.3) reclaims interrupted jobs.

**Keep-awake on an idle-sleeping GPU host.** When the runner lives on a host
that sleeps when idle (e.g. a Windows GPU workstation, where `RUNNER_KEEP_AWAKE`
defaults on — §2.4), it holds the host awake through **both** phases:
**transcribe** (while claimable jobs are pending or a job is in flight) and
**analyze** (the runner is parked, but the eval judge on the same host still
needs it up). It releases once the coordinator returns to `phase='idle'` and no
claimable work remains (or the gate makes it unclaimable), so the host can
sleep again.

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
-- additive columns (not created by the runner; a separate migration adds them):
ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS phase TEXT;
ALTER TABLE runner_control ADD COLUMN IF NOT EXISTS runner_gpu_parked BOOLEAN;
```

`phase` is the **request** (coordinator → runner). `runner_gpu_parked` is the
**acknowledgement** (runner → coordinator): whether the model is, right now,
actually off the GPU. The runner owns this column and no one else writes it —
the mirror image of `phase`.

It exists because a request is not a result. A coordinator that sets
`phase='analyze'` has asked a tenant to step off the card, but "asked politely"
is not "the card is free" — the runner parks only **between jobs**, so an
in-flight transcription keeps the GPU until it finishes. Without an
acknowledgement the coordinator can only trust the request blindly (and hand the
GPU to something else on top of a tenant that never left) or wait out a timeout
and stop the service anyway — throwing away the in-flight job the cooperative
hand-off exists to protect.

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

**Acknowledgement — `runner_gpu_parked`.** After applying (or failing to apply)
the park/unpark, the runner stamps this column with the state it actually
reached:

| value | meaning to a coordinator |
|---|---|
| `true` | the model is off the GPU and its VRAM has been returned to the driver |
| `false` | the model is on the GPU — including when a park was requested but **raised** |
| `NULL` / column absent | this runner has not stamped yet, or predates the column — treat as **not parked** |

Two properties a consumer may rely on:

- **`true` is only ever published after a park that succeeded.** A `park()` that
  raised is stamped `false`, because the model may still be resident; a
  coordinator that read `true` there would free the card on paper while a tenant
  still held it. The reverse error (`false` while actually parked) costs only a
  redundant escalation, so the asymmetry is deliberate.
- **Absent is not parked.** A missing column, missing row, or un-stamped runner
  must read as "still on the GPU", so a deployment that has not migrated yet
  degrades to the pre-existing behaviour rather than to a false all-clear.

Stamping is best-effort and never disturbs the claim path: if the column does not
exist the write is swallowed, exactly as the `phase` read is.

A coordinator's hand-off is therefore: set `phase='analyze'` → poll
`runner_gpu_parked` until `true` → use the GPU; and on the way back, set `phase`
to `'idle'`/`'transcribe'` (or `NULL`). If the poll does not go `true` within the
coordinator's budget, the tenant is still working and the coordinator must decide
whether to keep waiting or stop the service outright — the acknowledgement makes
that a *decision* rather than a guess.

#### Operator requeue (out-of-band, `earmark requeue`)

In addition to the runner/service transitions above, an operator may move a job
**back** to `pending` to redo it. This is the only sanctioned way a `done` or
`failed` job returns to `pending`. It is always operator-initiated (never the
runner) and is transactional:

- **Re-transcribe** (requeue = archive, then re-judge). In **one transaction**,
  in this order:
  1. `UPDATE transcription_jobs SET status='pending', attempts=0, error=NULL,
     claimed_by=NULL, claimed_at=NULL … RETURNING id, file_path` — the selector
     (path substring, `--failed`, one id, one book dir). Every later step is
     keyed on the returned job ids, so all entry points behave identically.
  2. **Lock the transcripts**: `SELECT 1 FROM transcripts WHERE job_id =
     ANY($1) FOR UPDATE`. This conflicts with the `FOR KEY SHARE` lock the
     foreign-key check takes on every finding `INSERT` (a judge run, a human
     direct edit), so concurrent writers are serialized: an insert already in
     flight commits first and is archived by step 3; one that starts later
     blocks until the requeue commits and then fails with a foreign-key
     violation. Without it, a finding committed between steps 3 and 4 would
     survive as a live-looking row with no transcript.
  3. **Archive the findings**: every `transcript_findings` row of those jobs'
     transcripts moves to `patch_state='superseded'` with `superseded_at=now()`
     (§2.17). This includes human decisions — `accepted`, `applied`, `rejected`,
     `reverted` and `origin='human'` corrections — which are archived, **never
     deleted**. The from-states are derived from `patch.CanTransition`.
  4. Delete the transcripts. `transcript_chunks` cascade; the findings keep
     their rows and get `transcript_id = NULL` through the foreign key's
     `ON DELETE SET NULL`. Step 3 must come first: afterwards the findings can no
     longer be found by transcript.
  5. Delete the jobs' `run_metrics` rows. Required because requeue *updates* the
     job row rather than deleting it, so `run_metrics → transcription_jobs ON
     DELETE CASCADE` never fires; left in place, the row would describe the
     now-deleted transcript and keep its `eval_finished_at` latch.

  The runner then re-processes the job like any pending job, and the **new
  transcript is judged fresh**: it has a new id, so it has no findings (the eval
  dedupe keys are per transcript), nothing to replay, and a clear eval latch.
  Superseded findings stay readable for audit and the bench
  (`list_transcript_corrections state=superseded`). The CLI
  (`earmark requeue <substr>` / `--failed`), the dashboard buttons
  (`/actions/requeue`, `/actions/retry-failed`, `/actions/book-requeue`) all go
  through this one code path (`db.requeue`).
- **Re-embed only** (`--reembed`): delete the matching rows in
  `transcript_chunks` and leave `transcripts`/`transcription_jobs` — and the
  findings — untouched. The Go worker re-embeds on its next poll (it selects
  transcripts with no chunks). Use after an embedding model or `CHUNK_SIZE`
  change — no re-transcription. Findings are **not** superseded: the transcript
  they describe still exists. Their `chunk_id` no longer names a live chunk, but
  the overlay is keyed by transcript and `chunk_index` and guarded by the chunk
  hash, so accepted/applied corrections replay onto a rebuild that reproduces the
  same chunk text, and are retired as `stale` (`chunk_changed`) when it does not
  (e.g. after a `CHUNK_SIZE` change). Re-anchoring findings to re-chunked text is
  the re-anchor pass's job, not requeue's.

A re-embed regenerates every chunk: the same `chunk_index` can now hold
different text — under the same deterministic id (`ChunkUUID(transcript_id,
chunk_index)`) or a new one — and a `CHUNK_SIZE` change also moves the
boundaries. Either way the kept findings' anchors are orphaned (the hash, not
the id, is what proves an anchor current). Run `earmark reanchor` once the
worker has rebuilt the chunks (§2.17 "Re-anchoring"); until it runs, replaying
an old finding retires it as `stale`. A re-transcribe needs no re-anchor: its
findings are superseded and the new transcript is judged fresh.

**The `transcript_findings.transcript_id` foreign key** (migration 5).
`REFERENCES transcripts(id) ON DELETE SET NULL`, so a finding can never point at a
transcript that does not exist. Before it existed, requeue left findings
orphaned (no FK); migration 5 archives any such orphans as `superseded` (with
`transcript_id = NULL`, `superseded_at` NULL — when their requeue happened was
never recorded) before adding the constraint, validated in the same
transaction (live 2026-10-06: 0 orphans of 32,337 findings, so validation is a
millisecond index anti-join). The FK deliberately does **not** cascade: a
transcript delete must never delete a human decision.

A NULL `transcript_id` therefore always means "archived by a requeue", and the
database enforces it: `CHECK (transcript_id IS NOT NULL OR patch_state =
'superseded')` (`transcript_findings_null_transcript_superseded`). A transcript
`DELETE` that would leave a live finding behind — any delete other than
requeue's, or a requeue that missed a row — fails instead of orphaning it.
Together with the row lock in step 2, an `INSERT` of a finding for a transcript
that a requeue is replacing either lands first and is archived, or fails with a
foreign-key violation once the requeue has committed. The migration sets
`lock_timeout = '5s'`: its `ALTER`s hold `ACCESS EXCLUSIVE` on
`transcript_findings` for the transaction, and behind a long reader they would
otherwise queue and stall every later reader; a timed-out migration fails the
process start, which retries it.

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
  -- Eval failure record (latch-only-on-success): set by a judge run that could
  -- not evaluate every chunk; cleared by the next successful run.
  eval_failed_at TIMESTAMPTZ, eval_failed_chunks INT, eval_error TEXT,
  -- Model(s) the judge endpoint reported serving (vs eval_model = requested).
  eval_resolved_model TEXT,
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
| **Go eval layer** | after the in-pipeline judge runs over a transcript's chunks (`EVAL_IN_PIPELINE`), or `earmark eval --backfill-*` judges one | success: `eval_started_at`, `eval_finished_at`, `eval_model`, `eval_resolved_model`, `eval_chunks`, `eval_skipped`, `eval_findings` (and clears the failure record); failure: `eval_failed_at`, `eval_failed_chunks`, `eval_error` only |

**Eval slice + completion marker.** `eval_finished_at IS NOT NULL` is the
**per-job eval-completion marker** — a job has been judged iff its `run_metrics`
row has a non-NULL `eval_finished_at`. Eval coverage is thus a real ratio
(`COUNT(eval_finished_at) / COUNT(done jobs)`) rather than a findings-row count
(a clean job has 0 findings but is still "evaluated"). `eval_model` is the judge
model id; `eval_chunks`/`eval_skipped`/`eval_findings` are the run's
`ChunksEvaluated`/`ChunksSkipped`/`FindingsFound`. The eval slice is written
**only by the per-job paths** — the in-pipeline judge (`EVAL_IN_PIPELINE`) and
the `earmark eval --backfill-*` sweeps — where the chunk set maps cleanly to one
job (`job_id`).

**Latch only on success.** `eval_finished_at` is written **only when the judge
evaluated every chunk of the transcript AND its findings were stored**. A run
that skipped any chunk (endpoint error, a single request hitting the chat
client's 120 s timeout, …) or failed to store its findings does **not** latch:
it records `eval_failed_at` (when), `eval_failed_chunks` (how many chunks it
could not judge — all of them when the findings write failed) and `eval_error`
(the first judge error / the persist error, ≤500 chars) and leaves the eval
success columns alone, so a failed re-judge of an already-latched job never
erases that job's earlier successful run. Its partial findings are still
inserted (advisory signal is not discarded); the re-judge paths skip findings
already recorded for the transcript, so a retry never doubles them. A shutdown
mid-judge records nothing. The next successful run latches and clears the three
failure columns. Rationale: a latched job is never re-judged, so a failure
latched as done (the pre-fix behavior) was lost for good.

**No hot retry loop.** The pipeline never retries a failed judge run by itself:
the gated eval pass selects only jobs with **neither** `eval_finished_at` nor
`eval_failed_at`, so a failed job leaves its selection after one attempt. Retries
are operator-driven and bounded: `earmark eval --backfill-unevaluated` (selects
`eval_finished_at IS NULL`, which includes recorded failures) and `earmark eval
--backfill-eval-errors` (§2.15), both with `--limit`. If the outcome itself
cannot be written (latch or failure record — e.g. `run_metrics` unavailable),
the job is still unattempted and will be re-selected, so the gated loop backs
off for the poll interval instead of draining a "full" batch immediately. The **standalone** `earmark eval` / `/actions/eval*`
paths evaluate a whole book (many jobs) or a library-wide sample, so they do
**not** write the per-job `run_metrics` eval slice (the mapping to a single job
is ambiguous); they emit a `pipeline_events` `stage='eval'` row instead (§1.7).

**`eval_finished_at` as the embed gate (`EVAL_GATES_EMBED=true`).** When
`EVAL_GATES_EMBED` is enabled, `eval_finished_at IS NOT NULL` is **the latch
that allows a transcript to be embedded**: the embed pass selects only transcripts
whose `run_metrics.eval_finished_at IS NOT NULL` (eval'd) — **or whose judge
attempt failed (`eval_failed_at IS NOT NULL`)** — AND have no chunks (not yet
embedded). Under this gate the invariant is `embedded ⟹ judge attempted`: a
transcript is never searchable until the judge has run on it. The gate **fails
open** on a judge failure — exactly as it did before latch-only-on-success, when
the failure was latched as done — so a judge outage never blocks search; the
difference is that the failure now stays visible and re-judgeable. Column ownership is
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
covers the ~790 already-embedded tracks, any job whose judge run failed, and
everything embedded while `EVAL_IN_PIPELINE=false`), judges them, and writes
`eval_finished_at` (on success — see "Latch only on success"). It walks the
selection in keyset pages of 32 (`ORDER BY created_at, id`), never loading the
whole backlog's `segments` at once. It is read-only over transcripts: it only INSERTs findings and
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
  -- ABS catalogue enrichment (additive, nullable; ADD COLUMN IF NOT EXISTS).
  description TEXT,
  genres      TEXT[],
  isbn        TEXT,
  -- ASIN identity (migration 00006, additive, nullable).
  asin_source     TEXT CHECK (asin_source IN ('dir', 'filename', 'embedded_tag')),
  identity_status TEXT CHECK (identity_status IN ('exact', 'conflict')),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### Identity: where the ASIN comes from

The ASIN is taken from, in order: the **directory** (`[B0…]` in the book
directory path), the **filename**, then the **ASIN tag embedded in the audio**,
which the ASR runner reads with ffprobe (the Go image has none) and reports in
`transcripts.embedded_asin` (§1.2). The embedded tag is the least trusted
source, so a record found through it is used only when it names the same book
as the path (`library.SameBook`):

- **Titles, two-way.** Titles are normalized (lower-cased, punctuation and
  bracketed ids stripped, stopwords and the authors' name tokens dropped) and
  compared in a few forms: whole, with series groups such as `(The Expanse,
  Book 1)` removed, the part after the last colon (a series prefix: `Red
  Rising: Golden Son`), and the part before the first colon only when what
  follows is a series marker (`Children of Dune: Dune Chronicles, Book 3`).
  Some pair of forms must cover **at least 80% of each title's tokens in both
  directions** — so a series sibling (`Dune` vs `Dune Messiah`), a subset
  (`Hail Mary` vs `Project Hail Mary`) or another product (`Fourth Wing (1 of
  2) [Dramatized Adaptation]`) does not match. A record with a plain subtitle
  the path lacks reads as a conflict: the safe direction.
- **Authors.** When both the record's and the path's author are known they must
  share a name token (`Frank Herbert` matches `Frank Herbert, Brian Herbert`).

On a mismatch the record is **not** used: the
book keeps its path metadata, no ASIN, and `identity_status = 'conflict'`
(logged). So an ASIN typo or a reused tag can never attach another book's
description and chapters.

| `identity_status` | Meaning |
|---|---|
| `exact` | The catalogue (ABS) record was matched by ASIN (`asin_source` says from where). |
| `conflict` | The embedded tag named a record that is not this book; it was not used. |
| NULL | Not resolved against a catalogue: local-only (no ASIN anywhere — e.g. Libro.fm books), path-only provider, or not looked up since 00006. |

`asin_source` and `identity_status` are written as a pair whenever a lookup
resolves identity. A **conflict also clears** the catalogue columns (`asin`,
`description`, `chapters`, `genres`, `isbn`, `narrator`, `series`), so data an
earlier match attached never survives under a conflict. A lookup that resolves
nothing (path-only, ABS unreachable) keeps the last outcome.

The embedded tag only exists once a **provenance-aware runner** (migration
00006) has transcribed the file: `transcripts.embedded_asin` is written only at
transcription time, so transcripts made before the runner update have no tag,
and nothing re-probes old files (`earmark backfill-metadata` finds no tag for
them; re-transcription would). For files that do have one it is applied (a) by
the Go worker right after it stamps the transcript (§1.9) — it re-runs the
book's metadata lookup, unless the path already carries an ASIN — and (b) by
`earmark backfill-metadata`. Only these write paths consult it; the hot read
paths (search, dashboard) build providers without it and read the stored
result.

#### Column ownership

| Writer | When | Columns it writes |
|--------|------|-------------------|
| **Go monitor** | at every enqueue (via `db.UpsertBookMetadata`) | `title`, `author`, `bias_terms`, `source` |
| **Go monitor — ABS path** | when METADATA_PROVIDER includes ABS | `narrator`, `series`, `asin`, `chapters`, `description`, `genres`, `isbn`, `asin_source`, `identity_status` |
| **Go worker** | after stamping a transcript with an `embedded_asin` | re-runs the monitor's lookup for that book (same columns) |

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
  asin, chapters, description, genres, isbn, asin_source, identity_status) so a PathProvider call can never
  clobber ABS-sourced data: a NULL/empty value keeps the stored one, while a
  non-empty value from a re-lookup (the monitor at enqueue, or `earmark
  backfill-metadata --yes`) **overwrites** it — that is how the enrichment is
  refreshed. `description` is the ABS publisher blurb stored verbatim (it may
  contain HTML); `genres` is the ABS genre list (blank entries dropped; an empty
  list is NULL); `isbn` is the ABS `isbn` field. No reader consumes these three
  yet — they are stored for later enrichment work.
- **Enrichment never clears a field — except on an identity conflict** (see
  above). Because every ABS enrichment column (narrator, series, asin,
  chapters, description, genres, isbn) is otherwise `COALESCE`-guarded, a
  lookup can only add or overwrite values, never remove one: if ABS later drops a genre list or blanks a description, the stored value
  stays. Clearing one takes a manual `UPDATE book_metadata SET <col> = NULL`
  (then a re-lookup repopulates whatever ABS still has).
  `bias_terms` is always overwritten (not COALESCE-guarded) so an improved
  metadata source is reflected on the next write.
- `chapters` is nullable and left `NULL` when no ABS provider is configured.
- **`chapters` times are BOOK-ABSOLUTE.** Each entry is
  `{Index, Title, StartSec, EndSec}` (plus an optional `RawTitle`, see below) with `StartSec`/`EndSec` measured from the
  start of the **whole book** (all tracks concatenated in play order) — that is
  what Audiobookshelf's `media.chapters` reports. This is a *different time base*
  from `transcript_chunks.start_sec`, which is track-relative (§3); readers must
  add the track's book offset before mapping a chunk into this list (§2.2.1).
- **`chapters` titles are cleaned of filename debris** by
  `metaprovider.CleanChapterTitles`. ABS derives chapter titles from track file
  names when a book has no embedded chapter metadata, so they arrive as
  `"<N> - <Book>: <chapter>"` (e.g. `"12 - Project Hail Mary: Chapter 11"`) or as
  a raw filename `"<Book> [<ASIN>] - <NN> - <chapter>[ (<k>)]"`. The cleaner strips
  the `<N> - ` track number only when **every** chapter carries one equal to its
  1-based position, strips the `<Book>: ` segment (and an immediate repeat of it)
  only when every chapter shares it, and strips the filename form only on a
  bracketed catalogue id followed by ` - <digits> - `; real titles such as
  `"1984: Part One"`, `"0000"` or a list of genuine `"Part I: …"` titles are left
  alone, and a strip that would leave an empty title is skipped. Cleaning runs
  **at ingest** (`mapABSChapters`, so a lookup stores clean titles) **and on
  read** (`db.decodeChapters`, so rows stored before cleaning existed surface
  clean titles without a rewrite). A list is cleaned **at most once**: cleaning
  is not idempotent in general (`["1 - Book: 1 - Intro", "2 - Book: 2 - Body"]`
  cleans to `["1 - Intro", "2 - Body"]`, which would strip again), so the read
  path returns a stored list unchanged when any entry already carries `RawTitle`
  (it was cleaned at ingest). The book segment is the text before the first
  `": "`, so a book whose own title contains `": "` keeps a residue (never a
  damaged title). Whenever a title changes the
  provider original is kept in an optional **`RawTitle`** key on that entry
  (absent when the title was already clean) — nothing is discarded. A stored row
  is rewritten with clean titles + `RawTitle` on its next re-lookup
  (`earmark backfill-metadata --yes`).
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
> down. The liveness stamp is written before the claim gate, so a runner that is
> alive but gated (`paused`, `run_limit=0`, `phase='analyze'`) keeps it fresh
> while its claim-activity age grows for as long as the gate holds — even with
> `pending > 0`. Alerts on runner liveness MUST use
> `earmark_runner_alive_seconds`; the claim-activity gauge only answers "has
> anything been transcribed lately", and pairing it with queue-non-empty +
> not-paused still misfires under `run_limit=0` or the analyze phase.

### 1.8 Schema migrations (goose)

The schema is owned by [goose](https://github.com/pressly/goose) running the
numbered migrations embedded from `internal/db/migrations/`. Every earmark
process (`db.New` → `initialize()`) migrates to the latest version on startup;
there is no other schema code. The version is recorded in `goose_db_version`.

| Version | File | What |
|---|---|---|
| 1 | `00001_baseline.sql` | The schema as of v0.40.2 — the DDL the pre-goose `initialize()` ran inline on every boot, copied verbatim. |
| 2 | `00002_recipes.sql` | `recipes` + nullable `recipe_id` on `transcripts`, `transcript_findings`, `transcript_chunks`; legacy backfill (§1.9). |
| 3 | `00003_stale_work.sql` | `current_recipes` + the `stale_work` view (§1.9). |
| 4 | `00004_unanchorable.sql` | The `unanchorable` patch state, `unanchorable_reason` and `reanchored_at` on `transcript_findings` (§2.17 "Re-anchoring"). |
| 5 | `00005_requeue_archive.sql` | Requeue archives findings (§1.4 "Operator requeue"): `patch_state` gains `superseded`, `superseded_at` column, `transcript_findings.transcript_id` becomes nullable with a foreign key to `transcripts(id) ON DELETE SET NULL` and a CHECK that only superseded findings may have no transcript; existing orphans are archived as `superseded` first; `stale_work` skips superseded findings. Not purely additive — see §1.4. |
| 6 | `00006_asr_provenance_identity.sql` | Runner-reported provenance + `embedded_asin` on `transcripts`; `asin_source` / `identity_status` on `book_metadata` (§1.2, §1.6, §1.9). |
| 7 | `00007_fn_calls.sql` | `fn_calls` — the pure-function call log and cache, with the partial unique cache index `fn_calls_cache_key_idx` and `fn_calls_recipe_id_idx` (§1.9 "Pure-function calls"). New table only. |

**Rules.** Schema changes are new numbered files; a migration that has shipped
is never edited. **Migrations are merged and deployed strictly in version
order.** goose runs without `AllowOutofOrder`, and that stays off: a process
that finds a migration file numbered *below* the database's version refuses to
start ("detected missing (out-of-order) migration"). So a branch carrying a
higher number (e.g. 00006) must not merge or deploy before the branches
carrying the lower ones (00004, 00005); otherwise renumber it at merge time to
the next free version. Migrations stay additive unless a change says otherwise, and
every table a migration adds is also dropped by `DEBUG_DB_RESET` (`resetSQL`,
pinned by `TestIntegrationResetRebuildsFreshSchema`).

**`DEBUG_DB_RESET` scope (wider than before goose).** The double-confirmation
guard is unchanged (`DEBUG_DB_RESET=true` **and**
`DEBUG_DB_RESET_CONFIRM=yes-delete-everything`). What it drops is not: the
pre-goose reset dropped only `transcript_chunks`, `transcripts` and
`transcription_jobs`, leaving the rest. A goose database must be dropped
whole — otherwise the recorded version would make the re-migration create
nothing — so the reset now drops **every** earmark object and
`goose_db_version`, then migrates from version 1. That includes
`transcript_findings` (**human corrections too**, `origin='human'`),
`book_metadata`, `run_metrics`, `pipeline_events`, `recipes` /
`current_recipes`, `fn_calls`, and `runner_control` — the runner comes back **unpaused**
with no `run_limit`.

**Deadlines.** A migration must not outlive the pod's patience:

- `lock_timeout` = **30 s** on goose's connection (`migrateLockTimeout`). A DDL
  statement queued behind a long reader (e.g. the old pod mid-search) fails
  with SQLSTATE `55P03` instead of waiting — and, while waiting, holding up
  every later reader of that table. The migration's transaction rolls back
  cleanly and the next start retries. The advisory-lock wait is exempt
  (`lock_timeout = 0` on the lock connection): queueing behind the other pod's
  migration is the design.
- The whole run, advisory-lock wait included, is bounded at **90 s**
  (`migrateDeadline`, a context deadline in `initialize`) — just under the
  ingest pod's liveness budget, so a migration that cannot finish fails startup
  with an error in the log instead of being SIGKILLed mid-transaction.
- The ingest pod's `/healthz` listener starts only after `db.New`, so a
  migration counts against its liveness budget (`initialDelaySeconds` 10 +
  3 × 30 s ≈ 100 s); the mcp pod's startup probe allows 300 s. Every migration
  so far finishes in a few seconds at production size (00002: 0.9–2.8 s
  measured). A migration that cannot — a large backfill — must be written to
  stay inside that budget (batched, or outside startup), or the probes raised
  for that release; serving `/healthz` before migrating is not done because the
  listener is built around the metrics registry, which needs the database.

**The baseline (version 1) adopts the existing database without touching it.**
It is a Go migration with three outcomes, decided inside goose's transaction
and under the schema lock:

| Database | Detected by | Action |
|---|---|---|
| empty | no `transcription_jobs` | execute the baseline |
| built by the old inline code | `transcription_jobs` exists and every baseline object is present | record version 1, execute **nothing** |
| built by an *older* inline earmark | `transcription_jobs` exists, some baseline object missing | execute the baseline. It is the v0.40.2 inline code verbatim — idempotent (`IF NOT EXISTS` / guarded `DO` blocks) and including its one DML step, the duplicate-`file_path` DELETE that runs before `transcription_jobs_file_path_unique` is added (keeping the most-advanced job, never a `done` one) — so it does what booting v0.40.2 would have done (`TestIntegrationBaselineCatchUpDedupsFilePaths`) |

"Every baseline object" is an inventory parsed from the baseline file itself
(tables, `ADD COLUMN` columns, indexes, named constraints, functions,
triggers). The integration suite proves a database built by the frozen pre-goose
DDL (`internal/db/testdata/legacy_initialize.sql`) and one built by the baseline
are catalog-identical, and that migrating the former runs no DDL except goose's
version table (recorded by an event trigger).

**Serialization (binding for new processes).** `earmark-ingest` and
`earmark-mcp` migrate concurrently on every rollout, and concurrent DDL
deadlocks (observed 2026-08-14 during `CREATE FUNCTION`, "while updating tuple
in relation `pg_proc`", and 2026-08-19 during `DROP TRIGGER`). Migration is
therefore serialized by an advisory lock:

- **Same key everywhere:** `0x4541524D5343484D` ("EARM","SCHM") — the key the
  pre-goose `initialize()` used with `pg_advisory_xact_lock`. Session and
  transaction advisory locks share one lock space, so an old pod and a new pod
  rolling together still serialize.
- **It waits:** `pg_advisory_lock`, never a try-lock. The loser blocks, then
  finds nothing pending.
- **It covers everything goose does,** including creating `goose_db_version`.
  goose's own `WithSessionLocker` does not (its `Up` runs `HasPending`, which
  creates the version table, before the locker) — so earmark takes the lock
  itself and gives goose no locker.
- **It cannot leak.** Goose commits each migration separately, so the lock
  must be session-scoped to span the run — and a session lock left on a pooled
  connection would block every later migration forever. It is therefore taken
  on a dedicated `database/sql` handle (`application_name=earmark-migrate`,
  `MaxIdleConns(0)`, closed before `migrate()` returns), never the service's
  `pgxpool`, and released explicitly on a detached context; even a failed
  unlock is released when the session ends.

`internal/db/schema_lock_test.go` pins these at the source level; the Postgres
integration tests (CI job "Integration (Postgres)", `EARMARK_TEST_DATABASE_URL`)
prove that concurrent migrators serialize, that `migrate` waits on a held lock
and creates nothing before it, and that no lock or session survives it.

**If you add a process that touches this database, it must migrate through
`db.New` (and so take the same lock) or reintroduce the deadlock.**

### 1.9 Provenance recipes — `recipes`, `current_recipes`, `stale_work`

A **recipe** is an immutable, content-addressed record of exactly how an output
row was made. Output rows carry the `recipe_id` of the step that produced them,
so three questions are queries: what made this text, what is out of date, and
(later) is the new model better.

```sql
CREATE TABLE recipes (
    recipe_id      TEXT        NOT NULL PRIMARY KEY,  -- lowercase hex sha256 of the canonical JSON below
    step           TEXT        NOT NULL,              -- asr | propose | decide | propagate | scan | format | embed
    step_version   INTEGER     NOT NULL,              -- bumped when earmark's logic for the step changes output; 0 = legacy
    code_version   TEXT        NOT NULL,              -- earmark "<tag>+<commit>" (runner tag for asr); 'legacy-unknown'
    model_alias    TEXT,                              -- what was asked for (the model id / LiteLLM alias sent)
    model_resolved TEXT,                              -- what answered (the response "model" field)
    model_revision TEXT,                              -- HF commit / .nemo sha256 / Ollama digest / provider snapshot
    prompt_version TEXT,                              -- e.g. "judge@v1"
    prompt_sha256  TEXT,                              -- hash of the exact prompt template bytes
    params         JSONB       NOT NULL DEFAULT '{}', -- temperature, thresholds, chunk size, dimensions, prefixes…
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

| Row | Column | Recipe of | Written by |
|---|---|---|---|
| `transcripts` | `recipe_id` | `asr` | the Go worker (`db.StampASRRecipes`), from the runner-reported provenance columns (§1.2); rows from a runner that reports none stay NULL |
| `transcript_findings` | `recipe_id` | `propose` (the judge) | `InsertFindings`; NULL for `origin='human'` rows |
| `transcript_chunks` | `recipe_id` | `embed` (chunking + embedding) | `InsertChunks`, on insert and re-embed |

All three are nullable FKs to `recipes`. NULL means "unstamped".

**Canonical form / ID.** `recipe_id` = lowercase hex SHA-256 of these exact
bytes — keys in this order, no whitespace, empty strings as `null`, `params`
canonicalized (object keys sorted, no whitespace, no HTML escaping, `{}` when
empty):

```json
{"step":"propose","step_version":1,"code_version":"v0.41.0+abc1234","model_alias":"earmark-judge","model_resolved":"anthropic/claude-haiku-4-5-20251001","model_revision":null,"prompt_version":"judge@v1","prompt_sha256":"00ff","params":{"a<b":"x&y","min_confidence":0.6,"temperature":0}}
```

→ `51274b9640f28301dccaf6fc52f6f9c60c83984ee21f62c00840c1e4007dcff1`
(`internal/recipe` `TestCanonicalVector`). Any writer outside Go (the runner)
MUST reproduce these bytes, and registers its recipe with
`INSERT … ON CONFLICT (recipe_id) DO NOTHING` before referencing it. Recipes are
never updated or deleted.

**What answered.** The judge records the model the chat response reports
serving the request, not just the one it asked for. A LiteLLM fallback answer
therefore has a different recipe, which `InsertFindings` registers on the fly
in the same transaction. Embeddings responses are not read for a model yet: the
embed recipe's `model_resolved` is the registry's expected model, else the
alias.

**ASR stamping.** The runner reports what it ran (§1.2: `model_name`,
`asr_model_sha256`, `asr_runner_version`, `asr_params`); each worker cycle
(`StampASRRecipes`, up to `EMBED_BATCH_SIZE` rows, via the partial index
`transcripts_asr_unstamped_idx`) builds the asr recipe — `step_version` 1,
`code_version` = runner version, `model_alias` = `model_resolved` = the loaded
model, `model_revision` = the `.nemo` sha256, `params` = `asr_params` — registers
it and sets `recipe_id` where it is still NULL, in one transaction. Rows without
`asr_runner_version` are never stamped (nothing is invented). A runner model
that differs from the registry's `asr` pin (§2.18) is logged; the recipe still
records what actually ran. `asr` still has no **current** recipe: the Go side
cannot know the runner's configuration before it reports, so asr rows are not
yet reported stale.

**Current steps.** `propose`: `step_version` 1, prompt `judge@v1` + sha256 of
(system prompt, user template, response JSON schema), params `temperature`
(**only when it is sent** — omitted for hosted routes unless configured, §2.15
"Request parameters"), `min_confidence`, `max_findings_per_chunk`. `max_tokens`
is deliberately not a param: a reply that hits it is an error, never findings. `embed`: `step_version` 1, params
`chunk_size`, `dimensions`, `document_prefix`. Both take `expected_model` /
`revision` from the model registry (§2.18).

**Legacy rows.** Migration 2 gives every pre-existing row one recipe per
`(step, model)` with `step_version` 0 and `code_version` `legacy-unknown`,
told apart only by model name — no prompt, revision or runner hash was ever
recorded, so none is invented: `asr` per `transcripts.model_name`; `propose`
per `(model, resolved_model)` of judge findings; `embed` per
`run_metrics.embed_model` of the chunk's job — a chunk whose job recorded no
embed model (that slice is best-effort and written after the chunks commit)
is attributed to the library's embed model when exactly one is recorded, and
to an "unknown" legacy recipe otherwise. Writers to the three stamped tables
are blocked (`LOCK TABLE … IN SHARE ROW EXCLUSIVE MODE`) for the migration so
the recipe set cannot change between the statements that compute it. Where
every row of a table maps
to one legacy recipe, the column is stamped through a constant default that
Postgres keeps as the column's missing value and the default is then dropped —
no rewrite, no `UPDATE` (at production size: 0.9 s, versus 91 s re-inserting
every chunk into the HNSW index).

**Current recipes and `stale_work`.** `current_recipes(step PK, recipe_id,
updated_at)` holds the recipe each step would use now; the ingest process
(`earmark monitor`) upserts the `embed` and (when an eval endpoint is
configured) `propose` rows at startup — one writer, so an ad-hoc `earmark eval`
with other settings stamps its own findings without redefining "current".
The `stale_work` view lists `(step, source_table, row_id, recipe_id,
current_recipe_id)` for every output row whose recipe is not equivalent to its
step's current recipe. Equivalence compares `step_version`, the three model
fields, prompt version and hash, and `params` — **not** `code_version`, so a
release that changes nothing does not mark the library stale; a logic change
bumps `step_version`. Unstamped rows are stale whenever their step has a
current recipe; a step with no current recipe (asr, today) reports nothing;
human corrections and `superseded` findings (archived by a requeue, §1.4 —
not work to redo; filtered since migration 5) are never listed.
`earmark_stale_items{step}` (§2.16) counts it per step.

**Expect a large `stale_work` right after the first deploy.** Every legacy row
has `step_version` 0 and no prompt hash, so none is equivalent to a current
recipe: on production that is all ≈39,644 chunks and ≈32,337 judge findings
(asr reports nothing — no current asr recipe yet). That is true, not noise:
none of those rows was made by the current configuration. Chunks converge as
the worker re-embeds; findings only by re-judging.

The Models page (§2.14) shows `current_recipes` and per-step `stale_work`
counts (the stale counts cached 5 min — the view scans every chunk and
finding, ≈1.6 s on production), and `GET /api/v1/status` `roles[].stale` (§2.12) carries
the same count per role.

**Pin `expected_model` behind an alias.** The current propose recipe expects
the endpoint to report `MODELS_FILE` `steps.propose.expected_model`, or the
requested model id when unpinned. LiteLLM usually reports the provider's id,
not the alias — unpinned, every new finding is then stamped with a non-current
recipe and listed as stale. The judge logs a warning on the first response
from a model other than the expected one (that is only knowable once the
endpoint answers), and an info line at startup when no pin is set.


#### Pure-function calls — `fn_calls`

Every call a pure function (`internal/fn`) makes to a decision model — today
TypeSafe System One (`jev-*`) — is one row. The log doubles as the cache.

```sql
CREATE TABLE fn_calls (
    id             BIGSERIAL     PRIMARY KEY,
    fn             TEXT          NOT NULL,              -- the function, e.g. "should_apply"
    prompt_version TEXT          NOT NULL,
    prompt_sha256  TEXT          NOT NULL,
    model_alias    TEXT          NOT NULL,              -- the pinned model id asked for, e.g. "jev-1.13.0"
    model_resolved TEXT,                                -- the reply's "model"; NULL when no reply
    model_revision TEXT,
    recipe_id      TEXT          REFERENCES recipes (recipe_id),
    input_sha256   TEXT          NOT NULL CHECK (input_sha256 ~ '^[0-9a-f]{64}$'), -- sha256 of the canonical input JSON
    input          JSONB         NOT NULL,
    output         JSONB,                               -- NULL on error
    error_class    TEXT,                                -- NULL on success
    latency_ms     INTEGER,
    input_tokens   INTEGER,
    output_tokens  INTEGER,
    cost_usd       NUMERIC(14,8),
    cache_hit      BOOLEAN       NOT NULL DEFAULT false,
    cached_from    BIGINT        REFERENCES fn_calls (id), -- the row a cache hit was served from
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX fn_calls_cache_key_idx ON fn_calls (fn, prompt_sha256, model_alias, input_sha256)
    WHERE error_class IS NULL AND NOT cache_hit;
CREATE INDEX fn_calls_recipe_id_idx ON fn_calls (recipe_id, created_at);
```

- **Cache.** The partial unique index admits one successful, non-cached row
  per `(fn, prompt_sha256, model_alias, input_sha256)`; that row is the cache
  entry. Writers insert with `ON CONFLICT … DO NOTHING` (`db.InsertFnCall`), so
  an input is never paid for twice and log rows are never updated.
- **Serving** (`db.LookupFnCache`) returns that row only when its
  `model_resolved` is the expected model (the registry's `expected_model`,
  else the alias), compared without case or a route prefix
  (`typesafe/jev-1.13.0` = `jev-1.13.0`): a reply from any other model is
  stored but never served. A fallback reply is logged with `error_class =
  'model_fallback'` and its output kept, which also keeps it out of the cache
  slot; the caller still receives it, flagged as a fallback.
- **A served call is still logged**, as its own row with `cache_hit = true`
  and `cached_from` set, so the table counts every call.
- **Errors** are logged with `error_class` and no output; they are never
  served. `error_class` is a bounded label: the HTTP status (`422`, `429`,
  `503`…), `timeout`, `canceled`, `invalid_reply` (a 200 that does not answer
  the request, §2.14 "System One"), `model_fallback`, or `_OTHER`.
- **Input hash.** `input_sha256` is the lowercase hex sha256 of the input's
  canonical JSON (`fn.CanonicalInput`): every object's keys sorted (struct
  fields included), no whitespace, no HTML escaping, numbers as written —
  `{"b":"x<y","a":1}` → `{"a":1,"b":"x<y"}` →
  `7e897661c014a2799b01d100cf3db3088dbc1c2c0ba3c83a63dd58fd41634fcc`.
- **Recipe.** A call's recipe is the function's step and `step_version`, the
  pinned alias as `model_alias`, the reply's model as `model_resolved`, the
  prompt version and hash, and the function's params plus `"fn": <name>` (so
  two functions of a step never share a recipe). A cache hit carries the
  served row's recipe.
- **Pinned models only.** A function refuses an alias that can move under the
  same name — `*-latest`, `*-preview`, `:latest`, or an id with no dotted
  version (`jev-latest` is refused, `jev-1.13.0` accepted); a `systemone`
  endpoint is refused at startup for the same reason (§2.14).
- `recipe_id` is the call's recipe, registered (insert-if-absent) before the
  row is written. No prompt or completion text is stored outside `input` /
  `output`.
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
**segments** (and, with `includeWordTimestamps=true`, their per-word times) — or,
for a track with reviewed corrections, **chunks** of the corrected text.

| Tool | Purpose | Key params |
|------|---------|-----------|
| `list_books` | Library **inventory**: per book → author, title, track progress (done/total), total duration, word count, embedded-chunk count. Em dash / 0 for books with no `run_metrics` yet. Ordered **transcribed-first** (fully-done books, then partial, then fully-pending). Leads with a one-line whole-library summary (`Library: T books — P fully transcribed, Q with pending tracks.` — TRUE totals across the library, not just the page). `format=flat` (default) **omits each book's `dir:` line** to keep the payload small; `format=tree` groups rows under their authors **and** keeps the `dir:` line; `format=series` groups rows under their **series name**, ordered by sequence within each group, with a trailing **"No series"** group so no book disappears (a book in several series is listed under each — by design). | `author?` (substring filter), `series?` (case-insensitive substring on `book_metadata.series`, §1.6 — so `Dune` matches both `Dune #2` and `The Dune Sequence #13`; books with no series row are excluded), `format?` (`flat` default \| `tree` \| `series`), `limit?` (default 50), `offset?` |
| `semantic_search_audiobooks` | Vector-similarity (meaning) search; hits show a real cosine `similarity: NN%`. Whole library by default; `book` scopes it. `snippet?` caps each hit's quoted text (leading **preview** — no sub-chunk match position). | `query` (required), `book?`, `threshold?` (0.3), `limit?` (10), `snippet?` (max chars; floored to 80) |
| `text_search_audiobooks` | Trigram literal/keyword search; hits are labelled **"ranked by trigram match"** (NOT a similarity %, which would mislead on a literal hit). Whole library by default; `book` scopes it. `snippet?` returns an excerpt **centred on the literal match**. | `query` (required), `book?`, `limit?` (10), `snippet?` (max chars; floored to 80) |
| `get_transcript` | Read a track's full transcript (paginated — `raw_text` can be 600k+ chars). When the track has reviewed corrections it serves the **corrected** text — the chunk projection search returns — as **chunks** with their time ranges and no word timestamps (`corrected: true`); otherwise the ASR record as timestamped **segments** (`corrected: false`). Multi-track book → returns a track chooser to pick a `trackID`. Per-word timestamps are **hidden by default**; `includeWordTimestamps=true` returns the ASR segments with each segment's `words[]` (word/start/end, plus score/speaker when present) for "exactly when was X said" queries — always the uncorrected text, flagged in `note` when corrections exist. | `book?` or `trackID?` (one required), `offset?` (0), `limit?` (50 segments; 10 chunks, max 25, in corrected mode), `includeWordTimestamps?` (false) |
| `get_chunk_context` | Surrounding **chunks** around a chunk. `chunkID` is the **UUID** in a search hit's `ID` field. | `chunkID` (required, the search-hit UUID), `contextWindow?` (**default 1** → ~3 chunks; clamped to 0–50 to bound the response size) |
| `list_transcript_corrections` | Read-only review **worklist** (§2.17): each row is a finding with pristine chunk context, an anchor-resolution status, and its legal next actions (`allowedActions`). Defaults to the undecided (`proposed`) queue. | `state?` (comma-separated patch states, or `all`/`any`; default `proposed`), `book?`, `path?`, `id?` (a single finding), `min_confidence?` (0), `limit?` (20, capped 200), `offset?` (0) |
| `decide_transcript_correction` | **Writes.** Accept / reject / revert / reconsider one finding — drives `db.SetPatchState`, validated against `patch.CanTransition` and compare-and-swapped on the expected current state. Accept/revert flag the chunk `embedding_stale`; reject changes no text. An `unanchorable` finding offers no action (only `earmark reanchor` moves it, §2.17). | `id` (required), `action` (required: `accept`\|`reject`\|`revert`\|`reconsider`), `decided_by?` (default `"agent"`, stored `mcp:`-prefixed), `expected_state?` (optional CAS guard) |
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
durationSeconds, corrected, unit, segments[] | chunks[], offset, limit,
totalSegments, totalChunks?, correctedChunks?, note?, nextOffset? }` for a
page, or `{ kind: "trackChooser", book, tracks[] }` when a book has multiple
tracks. `corrected` is always present and says which text the page is:
`false` → `unit: "segment"`, `segments[]` from the immutable ASR record;
`true` → `unit: "chunk"`, `chunks[]` of the corrected projection (§2.17), each
`{ chunkID, chunkIndex, start, end, text, corrected }` (`corrected` marks a
chunk that differs from the ASR text), with **no word timestamps**.
`offset`/`limit`/`nextOffset` count `unit`s. A track is served corrected when at
least one of its chunks' projected `text` differs from its pristine
`source_text` — i.e. exactly when search would return different text from the
ASR record; an accepted correction not yet rebuilt into the projection does not
count yet. `totalChunks`/`correctedChunks` are set whenever corrections exist,
including when `includeWordTimestamps=true` forces the ASR segments (then
`note` says the text is UNCORRECTED and how to get the corrected text). If the
corrected text cannot be read the call fails (`isError`) rather than serving
the ASR text as though there were no corrections. Each segment is `{ start, end,
text }`; with `includeWordTimestamps=true` it also carries `words[]` — each `{
word, start, end, score?, speaker? }` (`score`/`speaker` present only when the
ASR backend supplied them). The `words` field is **omitted entirely** by
default. (**Additive response-shape change**: `corrected`/`unit` are new on every
transcript page; a track with no corrections returns the same `segments[]` page
as before, and a track *with* corrections now returns `chunks[]` instead.)
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
(non-zero index or a non-empty title) renders as `Chapter N: <title>`, where
`<title>` is the debris-cleaned chapter title (§1.6).

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
| `AI_ROLES` | no | JSON object binding role names (`embeddings`, `eval`, `decide`, `scan`) to endpoint IDs (§2.14). Required when `AI_ENDPOINTS` is set. |
| `MODELS_FILE` | no | Path to the model registry YAML (§2.18): per step, the expected answering model, revision pin and prompt version stamped into provenance recipes (§1.9). Unset → no pins. Unreadable / malformed / unknown step or field / an `alias` contradicting `AI_ENDPOINTS` is **fatal** (fail-closed). |
| `BOOKS_DIR` | no | `/books` (read-only NFS mount inside container) |
| `MCP_HTTP_ADDR` | no | `:8081` |
| `INGEST_HTTP_ADDR` | no | `:8082`. The `earmark monitor` (ingest) process serves a minimal HTTP listener here for `/healthz` (liveness) and `/metrics` (Prometheus, §2.16). The mcp pod uses `MCP_HTTP_ADDR` for its surface; this is the ingest pod's only HTTP port. Chosen to avoid colliding with `:8081`. |
| `LOG_FORMAT` | no | `pretty` (default — human-readable, ANSI-colored `PrettyHandler`). Set `json` for a `slog` JSON handler writing one JSON object per line to stdout (parseable in Loki); a record logged inside an OpenTelemetry span also carries `trace_id`/`span_id` (§2.16). Both carry the `module` attribute and honor `LOG_DEBUG`/`LOG_VERBOSE`. Used by both Go pods. `earmark mcp` in stdio mode writes all log lines to stderr. |
| `OTEL_SDK_DISABLED` | no | `true` turns the OpenTelemetry SDK off entirely (§2.16). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | OTLP collector endpoint (also the per-signal `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `_METRICS_ENDPOINT`). **Unset → no OTLP export at all.** No default in code; the cluster's collector belongs in deploy values. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | no | `http/protobuf` only (the default; per-signal variants honored). Any other value (e.g. `grpc`) turns that signal's OTLP export off with a warning. Point the endpoint at the collector's OTLP/HTTP port (Alloy: `:4318`). |
| `OTEL_METRICS_EXPORTER` / `OTEL_TRACES_EXPORTER` | no | Exporter selection: metrics `prometheus,otlp` (default), traces `otlp` (default); `none` disables. OTLP still needs an endpoint. |
| `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` | no | Resource attributes; `service.name` defaults to `earmark`. Other standard `OTEL_*` exporter variables (headers, insecure, timeout, export interval) are read by the exporters. |
| `STALE_JOB_TIMEOUT` | no | `30m` (Go duration string) |
| `SCAN_INTERVAL` | no | `1h` (Go duration string). How often the monitor **re-walks `BOOKS_DIR`** looking for new audio files, in addition to the walk it does at startup. Required for correctness on NFS: fsnotify/inotify only reports writes that pass through the monitor pod's own kernel, so a book written directly on the file server — or by any other NFS client — raises **no** watch event and would otherwise stay undiscovered until the pod restarted. The recurring walk is the backstop; fsnotify remains the low-latency path for local writes. The walk is metadata-only for known paths (already-queued `file_path`s are skipped without re-hashing, §1.1), so it is cheap over a multi-TB library. Per-entry errors (e.g. a transient NFS `EIO` on one subdirectory) are logged, counted, and skipped — one bad directory must never abort, and thereby silently disable, every subsequent scan. **`0` (or a negative value) disables periodic scanning**, leaving only the startup walk and fsnotify. |
| `CHUNK_SIZE` | no | `512` (target tokens per chunk; overlap is 64 tokens) |
| `EMBED_BATCH_SIZE` | no | `32`. Max transcripts the embed worker selects **per poll cycle** for BOTH gated-flow (`EVAL_GATES_EMBED`) passes — the eval pass and the embed pass each `… ORDER BY t.created_at ASC LIMIT $1`. Bounds the worker's per-cycle memory: an unbounded selection loads every matching transcript's `segments` JSONB into one slice, which OOM-kills the pod on a large backlog (e.g. a full re-embed after re-segmentation). The worker drains a backlog across cycles — a transcript that gets eval'd/embedded drops out of the next cycle's selection — and **loops immediately (no `pollInterval` sleep) whenever a pass returns a full batch**, so a multi-thousand-item backlog drains in back-to-back cycles rather than one batch per poll interval. Must be a **positive integer** — a non-positive or non-numeric value is fatal at startup (the OOM guard must never silently round-trip to unbounded). The ungated single-pass selection (`GetCompletedTranscripts`) is bounded by the same value but **keyset-paged** (`ORDER BY created_at, id`, `(created_at, id) > cursor`, `LIMIT`): each cycle walks every page, embedding one page before loading the next, so memory is bounded by one page while transcripts that keep failing to embed can never starve the ones behind them; the worker then sleeps `pollInterval` unless something was embedded (no hot retry of a failing transcript). |
| `LIBRARY_COLLECTIONS` | no | JSON array describing each library root's shape, for the dashboard's author/title labels (see below). Empty → generic fallback. |
| `CONTROL_API_TOKEN` | no | Bearer token required on the mutating control-API endpoints (§2.7). Empty → those endpoints fail closed (`503`); read endpoints are always open. |
| `EVAL_MAX_FINDINGS_PER_CHUNK` | no | `5`. Cap on findings kept per chunk by the eval judge (highest-confidence retained; the judge over-flags). `<= 0` disables the cap. See §2.15. |
| `EVAL_REASONING_EFFORT` | no | `auto`. The judge request's `reasoning_effort`. `auto`: `"none"` for local models, omitted when the model id contains `anthropic/` (aliases that hide the provider need `omit`); `omit`: never sent; any other value is sent verbatim. See §2.15. |
| `EVAL_CHAT_TEMPLATE_KWARGS` | no | `auto`. The judge request's `chat_template_kwargs`. `auto`: `{"enable_thinking": false}` for local models, omitted when the model id contains `anthropic/` (aliases that hide the provider need `omit`); `omit`: never sent; a JSON object is sent verbatim. Anything else is a configuration error (the eval client fails to resolve). See §2.15. |
| `EVAL_MIN_CONFIDENCE` | no | `0.6`. Confidence floor — findings below it are dropped before the cap. `<= 0` disables the floor. See §2.15. |
| `EVAL_IN_PIPELINE` | no | `false`. When true, the embed worker runs the eval judge on each transcript's chunks **before embedding** (the repositioned, in-pipeline eval) — each chunk can then hold the embed for up to the chat client's 120 s request timeout. When false (**decoupled mode**) the worker builds no judge and makes **no judge call** on the embed path, so embedding never waits on the judge; judging runs as its own pass, `earmark eval --backfill-unevaluated --write` (§2.15), which picks up every done transcript without an `eval_finished_at` latch, embedded or not. (Decoupled mode implies `EVAL_GATES_EMBED=false` — the gate is fatal without `EVAL_IN_PIPELINE=true`.) Requires an eval chat endpoint (`AI_ROLES.eval` / `EVAL_CHAT_*`); if none resolves, inline eval is logged-skipped, not fatal. See also `EVAL_GATES_EMBED`. |
| `EVAL_GATES_EMBED` | no | `false`. When true, the pipeline becomes strictly linear — a transcript is NOT embedded (not searchable) until it has been judged. Implements the **two-pass gated flow**: an **eval pass** selects done, not-yet-attempted, not-embedded transcripts and judges them (writing `eval_finished_at` on a complete run, the `eval_failed_*` record otherwise); an **embed pass** then selects done, eval'd-or-failed, not-embedded transcripts and embeds them. The `eval_finished_at` latch / `eval_failed_at` record (CONTRACT §1.5) is the hand-off between the two passes. **Invariant**: under this gate, `embedded ⟹ judge attempted` (a judge failure fails open; it is retried by `earmark eval --backfill-*`, never in a hot loop). **Fail-closed** (two conditions, both fatal at startup): (1) if no eval judge endpoint resolves (`AI_ROLES["eval"]` / `EVAL_CHAT_*`); and (2) if `EVAL_IN_PIPELINE` is not also `true`. The gate makes eval a strict prerequisite for embedding, and the eval judge is only built when `EVAL_IN_PIPELINE=true`; so `EVAL_GATES_EMBED=true` **requires** `EVAL_IN_PIPELINE=true` (and a resolvable judge) — otherwise the worker would run gated with a nil judge, stalling the corpus (or risking a nil-judge deref). Both failures fail at startup, never silently stalling the corpus (mirror of the §2.14 malformed-registry fail-closed). Default `false` → behavior is identical to the pre-gate deployment (no behavior change for unconfigured deployments). **Chunk UUIDs**: under this gate, chunk UUIDs are derived deterministically as UUIDv5 over `(transcript_id, chunk_index)`, so the eval pass (which chunks to judge) and the embed pass (which chunks to insert) produce identical IDs without coordination — findings written in the eval pass reference the same chunk rows the embed pass inserts. |
| `GPU_ARBITER_URL` | no | gpu-arbiter `/status` URL (e.g. `http://gpu-host:48750/status`) read by the `earmark batch` coordinator (§1.4) to yield the GPU to games. **Read-only** — the coordinator only `GET`s it, never `POST`s. Unset or unreachable → the coordinator logs it and proceeds (degrades gracefully). The `batch --gpu-arbiter-url` flag overrides it. |
| `ARBITER_WAIT_CMD` | no | Explicit path to a gpu-arbiter binary the `earmark batch` coordinator should delegate waits to (§1.4). Lower precedence than the `batch --arbiter-wait-cmd` flag, higher than `gpu-arbiter` auto-detected on `PATH`. Unset → the coordinator auto-detects on `PATH`, falling back to its built-in HTTP poll loop if nothing resolves. Purely an optimization — never required. |
| `ASR_SERVERS` | no | JSON array declaring the transcription servers (ASR runners) for this deployment, so the Models dashboard page can show a configured-but-idle server (e.g. a fallback). Empty → the page lists only observed runners. Cosmetic/read-only: a malformed value logs a warning and is ignored, and the list does **not** influence job routing (the runner claims work itself). See below. |
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
falls back to history inference (`idle`/`not_seen`). The Models dashboard page
(ASR runners section) and the `servers` array in `GET /api/v1/status` merge the configured list with
observed activity; an observed runner with no matching entry is still shown,
marked *unconfigured* — **if** it holds a live claim or finished a transcription
within the last 30 days (`runnerHistoryWindow`). A retired host that only
appears in old `run_metrics` history is hidden; configured servers always show.
Host recency is `run_metrics.transcribe_finished_at` (also the "last active"
value and the source of a host's latest model/mode), never `updated_at`, which
the eval and embed stages bump on old rows. Example:

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
| `RUNNER_BUSY_FLAG_PATH` | no | `/tmp/earmark-asr-busy` (a native Windows runner sets an explicit path) |
| `RUNNER_TMP_DIR` | no | Unset → the platform temp dir (`tempfile.gettempdir()`: `/tmp` on Linux unless `TMPDIR`/`TEMP`/`TMP` is set; the service account's `%TEMP%` on Windows). Directory for the runner's short-lived temp files: 16 kHz mono WAVs, chunk windows, and the context-biasing phrases file, each created with `mkstemp` and deleted after the job. Owner-only (`0600`) on POSIX; on Windows a file inherits the directory ACL, so point this at a directory only the service account can read. When set it also becomes Python's `tempfile.tempdir`, so NeMo's own temp files land there too. If set, it must already exist and be writable; on Windows it must also be short enough that temp paths stay under `MAX_PATH` (260). Otherwise startup and `--self-check` fail. |
| `ASR_MODEL` | no | `nvidia/parakeet-tdt-0.6b-v3` (model id; written to `transcripts.model_name` + `run_metrics.asr_model`) |
| `ASR_FAMILY` | no | Model-family id, e.g. `nemo-parakeet`, `whisper` (§2.13). Free-form-but-conventional. Written to `run_metrics.asr_family`. Unset → NULL ("unknown"). |
| `ASR_RUNTIME` | no | Runtime id, e.g. `nemo-cuda`, `whisper.cpp-sycl` (§2.13). Free-form-but-conventional. Written to `run_metrics.asr_runtime`. Unset → NULL ("unknown"). |
| `ASR_CAPABILITIES` | no | JSON map of the runner's **advertised** capabilities (its static truth), keys from the §2.13 closed enum → bool, e.g. `{"word_timestamps":true,"context_biasing":false}`. Defaults per known family. Used for the deferred routing match and as `caps_applied` defaults. Unknown keys are ignored with a warning. |
| `ASR_DIARIZE` | no | `false` (default). Set `true` to run speaker diarization (e.g. NeMo Sortformer) for multi-voice/full-cast titles. **Global** (per-job diarization is a deferred Phase-3 concern). |
| `ASR_COMPUTE_TYPE` | no | `bfloat16` (native on RTX 5090 / Blackwell) |
| `ASR_CHUNK_THRESHOLD_SECONDS` | no | `3600` — single-pass below this duration; chunked/buffered inference above |
| `BOOKS_MOUNT` | no | `/mnt/media/books` — this host's mount of the books share (an NFS path on Linux; a UNC path such as `\\nas\books` on a native Windows runner). DB `file_path`s are re-rooted onto it. |
| `BOOKS_DB_ROOT` | no | `/books` — the producer-side root that absolute DB `file_path`s are rooted at (the Go service's container `BOOKS_DIR`). Always parsed as a POSIX path, whatever OS the runner is on. |
| `BOOKS_PATH_ENCODING` | no | `none` (default) or `sfm`; any other value fails startup. `sfm` encodes NTFS-illegal characters in each re-rooted path component (never the `BOOKS_MOUNT` prefix) to the Services-for-Mac private-use code points: U+0001–U+001F → U+F001–U+F01F, `"` → U+F020, `*` → U+F021, `:` → U+F022, `<` → U+F023, `>` → U+F024, `?` → U+F025, `\` → U+F026, `\|` → U+F027. This is Samba's `macos_string_replace_map` (`source3/lib/string_replace.c`), which `vfs_fruit` installs as `catia:mappings` under `fruit:encoding = native`. As in Samba, there is no trailing-space or trailing-period rule. Use it when a Windows runner reads a share configured that way, so names containing `:` etc. resolve. |
| `RUNNER_KEEP_AWAKE` | no | `auto` (default: on for Windows, off elsewhere), `true` or `false` (also `1`/`0`, `yes`/`no`, `on`/`off`; case-insensitive); any other value fails startup and `--self-check`. When on, the runner keeps an idle-sleeping host awake via `SetThreadExecutionState(ES_CONTINUOUS \| ES_SYSTEM_REQUIRED)` from its main-loop thread (never display-required or away mode), changing it only on a state transition. It holds while a job is in flight, while a claimable job (`pending`, `attempts < 3`) exists and the gate would let it be claimed (not `paused`, `run_limit` NULL or > 0, `phase` ≠ `analyze`), or while the batch `phase` is `transcribe` or `analyze` (§1.4); otherwise, or when an evaluation fails (e.g. the DB is unreachable), it releases and the host's normal idle timer resumes. Forcing `true` on a non-Windows host logs once and does nothing. Off → no extra queries. |

**Breaking changes: none** for the existing runner — every new var is optional
and the defaults preserve current behavior, with one intended exception:
`RUNNER_KEEP_AWAKE=auto` turns keep-awake on for Windows runners (set `false` to
opt out); Linux runners are unchanged.

#### Runner result obligation — report provenance and the embedded ASIN (SHOULD)

On the transcript `INSERT` the runner **SHOULD** write `embedded_asin`,
`asr_model_sha256`, `asr_runner_version` and `asr_params` (§1.2) when the
schema has them. No new env var: the sha256 is computed once per process at
model load from the `.nemo` in the local Hugging Face cache, and `asr_params`
is derived from the variables above. The runner does not compute recipe IDs;
the Go worker does (§1.9).

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

Rendered workloads: Deployments `<release>-ingest` (`earmark monitor`) and
`<release>-mcp` (`earmark mcp`), and — only when `evalBackfill.enabled` (default
`false`) — CronJob `<release>-eval-backfill`, which runs
`earmark eval --backfill-unevaluated --write [--limit N]` (§2.15 decoupled mode).
The CronJob shares the Deployments' image, `earmark.commonEnv` env block (so it
judges with the same `AI_ENDPOINTS`/`AI_ROLES` and gateway key), pod/container
security contexts, `nodeSelector` and `tolerations`; it mounts no books volume and
exposes no ports. When `config.models` is set it mounts the model registry like
the Deployments do (`commonEnv` then sets `MODELS_FILE`, and the judge stamps
propose recipes from it — §2.18). It gets the **whole** `commonEnv`, including env it never
reads (`BOOKS_DIR`, `SCAN_INTERVAL`, `LIBRARY_COLLECTIONS`, `ASR_SERVERS`,
`METADATA_PROVIDER`, `ABS_URL`/`ABS_LIBRARY_ID`) and the `ABS_TOKEN` secret ref
(eval never calls Audiobookshelf). That is an accepted cost: one env block for
every earmark pod is what keeps the judge config from drifting, and splitting
`commonEnv` would reintroduce a second list to keep in step.
`evalBackfill.extraEnv` appends judge-only env (e.g. `EVAL_REASONING_EFFORT=omit`)
and is **additions only** — a name `commonEnv` already sets fails the render.
`evalBackfill.extraArgs` appends extra `earmark eval` flags. Enabling the CronJob
while `config.evalInPipeline: true` renders (a one-off backlog catch-up is
legitimate) but `NOTES` warns that both paths will judge. Defaults: schedule
`17 * * * *`, `--limit 25` (`0` omits the flag = no cap), `concurrencyPolicy: Forbid`, `backoffLimit: 0`,
`activeDeadlineSeconds: 3000`, `startingDeadlineSeconds: 600`,
`ttlSecondsAfterFinished: 86400`, history limits 3/3.

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
| `GET` | `/api/v1/status` | none | — | `200` queue/runner snapshot (JSON), incl. a `servers[]` array (name, host, role, configured, state, model, modelSize, computeMode, jobsDone; plus gpuProbed/gpuReachable/gpuState/vramUsedMb/vramTotalMb when a `gpuArbiterUrl` is configured), an `endpoints[]` array (id, type, backend, baseURL, model, options, role, state, probed, gateway, gatewayInferred — the AI endpoint registry with **liveness-only** probes, §2.14), a **`roles[]`** array (the Models page role board — see below), an `eta` object (the empirical ETA, §4: `{remainingChunks, workSeconds, calendarSeconds, calendarKnown, evalIncluded, hasWork, label}`; `null` when no estimate could be computed), and a **`pipeline`** object (see below) |
| `GET` | `/api/v1/pipeline/pause` | none | — | `200 {"paused":bool,"runLimit":int\|null}` |
| `PUT` | `/api/v1/pipeline/pause` | bearer | `{"paused":bool}` | `200` current state (`paused:false` resumes + clears bound) |
| `POST` | `/api/v1/pipeline/run` | bearer | `{"limit":N}` (N≥1) | `202 {"paused":false,"runLimit":N}` — run N then auto-pause |
| `DELETE` | `/api/v1/pipeline/run` | bearer | — | `200` clears the bounded run (`run_limit→NULL`) |
| `POST` | `/api/v1/runner/update` | bearer | `{"version":"<tag>"}` (empty/omitted clears) | `202 {runningVersion,desiredVersion,state,updateAvailable,…}` — request the runner self-update to `<tag>` (sets `desired_runner_version` + `'requested'`); the runner performs the swap (§1.4) |
| `GET` | `/api/v1/openapi.yaml` | none | — | `200` this API's OpenAPI 3.1 contract (`application/yaml`), served verbatim from bytes embedded in the binary so a deployed instance is self-describing |

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

**`roles[]` array** — one entry per pipeline role, always five, in order `asr`,
`judge`, `embeddings`, `decide`, `format`. Built by the same code as the Models
page role cards (§2.14), so the two cannot disagree. `endpoints[].state` is
liveness only (`GET /models`); `roles[].state` folds in call outcomes, so a
gateway that lists the model but fails every chat call is `endpoints[].state =
ready` and `roles[judge].state = failing`.

```jsonc
{
  "role": "judge", "step": "propose",
  "state": "degraded",          // healthy | idle | degraded | failing | down | not_configured | unknown
  "reason": "answered by qwen3.8, expected anthropic/claude-haiku-4-5-20251001 — …",
  "configured": true,
  "endpoint": "litellm-judge",  // AI_ENDPOINTS id; "EVAL_CHAT_*" for an env judge; omitted for asr
  "gateway": "litellm", "gatewayInferred": false,
  "requested": "earmark-judge", // the model id earmark sends
  "expected": "anthropic/claude-haiku-4-5-20251001", // MODELS_FILE pin; omitted when unpinned
  "answered": "qwen3.8",        // what actually answered most recently
  "answeredMatch": "mismatch",  // match | mismatch | unreported | none | unchecked
  "modelAllowed": true,         // on the LiteLLM key allowlist, reconciled with /v1/models (see §2.14); null = unknown
  "lastOkAt": "2026-10-06T00:12:00Z",   // null = never / unknown
  "lastFailedAt": null,
  "lastError": null,            // judge only; truncated to 300 chars
  "failingNow": 0,              // judge only (null otherwise / when counts unavailable); transcripts whose LATEST attempt failed
  "stale": 32337,               // stale_work rows for the step; null = not tracked / unavailable / still counting
  "countsAsOf": "2026-10-06T00:13:30Z", // the 30 s aggregate snapshot; null = never loaded
  "countsError": false,         // the latest aggregate refresh failed (last good still served)
  "staleAsOf": "2026-10-06T00:10:02Z"   // the separate 5 min stale-count snapshot; null = not loaded yet
}
```

The DB-derived fields come from two caches served stale-while-revalidate (a
request never waits on a refresh once a value exists): `answered`, the judge's
`lastOkAt`/`lastFailedAt`/`lastError`/`failingNow` and the ASR `lastOkAt` from
the 30 s aggregate snapshot (`countsAsOf`); `stale` from the 5 min stale-count
snapshot (`staleAsOf`). Embeddings' `lastOkAt` comes from the live queue stats.
A failed refresh keeps the last good snapshot (`countsError: true`); with none,
those fields are `null` and the judge's `state` is `unknown` — the response
itself never fails on it, and a slow or failed stale count never affects the
other fields.

**`gateways[]` array** — one entry per distinct LiteLLM gateway (and virtual
key) the AI registry routes through (§2.14 "LiteLLM gateway"); empty when none.
Never carries key material:

```jsonc
{
  "baseHost": "llm-gateway:4000", "endpoints": ["litellm-embed", "litellm-judge"],
  "ready": true, "health": "healthy", "db": "connected", "version": "1.102.1",
  "keyAlias": "earmark", "keyStatus": "active", "keyBlocked": false, "keyExpires": "",
  "spend": 12.25, "maxBudget": null, "budgetResetAt": "",
  "rpmLimit": null, "tpmLimit": null,
  "allowedModels": ["anthropic/claude-haiku-4-5-20251001", "nomic-embed-text"], // [] = all models
  "keyInfoError": ""            // e.g. "key info not readable by earmark's key (HTTP 403)"; key fields then null/absent
}
```

**Auth**: mutating endpoints require `Authorization: Bearer <CONTROL_API_TOKEN>`
(constant-time compared). When `CONTROL_API_TOKEN` is unset they **fail closed**
with `503` — the pipeline can never be paused/driven by an unauthenticated
caller. Read endpoints are always open. This is layered on the LAN-only ingress.

**Machine-readable contract**: `docs/openapi.yaml` (OpenAPI 3.1) specifies the
seven `/api/v1` operations above and is embedded in the binary, served at
`GET /api/v1/openapi.yaml`. It covers **only** this JSON API — the htmx
dashboard routes below return HTML fragments and are deliberately out of scope.
The spec cannot drift: `apiRoutes` (internal/mcp/routes.go) is the single source
of truth for route registration, and `TestOpenAPISync` fails the build on any
route documented-but-unregistered or registered-but-undocumented.

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
| `POST` | `/actions/runner-update` (form body `version`, else query `?version=`) | htmx + token | request the runner self-update to `version` (or clear when empty); writes `desired_runner_version` + `'requested'` and re-renders the `/servers` (Models) fragment (the runner version line) |
| `POST` | `/actions/run-clear` | htmx + token | clear the bounded run (`run_limit→NULL`) without touching the pause flag |
| `POST` | `/actions/eval?dir=…` | htmx + token | run the LLM judge over one book (async, §2.15) |
| `POST` | `/actions/eval-sample?n=N` | htmx + token | run the LLM judge over an N-chunk sample (async, §2.15) |
| `POST` | `/actions/findings-clear[?dir=…]` | htmx + token | **delete** recorded findings (advisory metadata only; §2.15), then re-render the `/findings` fragment. Optional `dir` scopes the delete to one book; absent clears all. Touches only `transcript_findings` — transcripts are never modified, so a clear is always recoverable by re-running eval. |

#### 2.12.1 Dashboard page routes (HTML)

The htmx dashboard's page shells and their data fragments. All are GET and
LAN-only; only reachable under the HTTP transport.

| Path | Purpose |
|------|---------|
| `GET /` | **Home — the Pipeline ops page** (same as `/pipeline`). The `/` route is a catch-all, so an unmatched path 404s. |
| `GET /pipeline` | **Pipeline ops page** — the auto-refreshing status fragment (counts, pipeline state, read-only phase badge, pause + run-budget controls) with the **Failed jobs view folded in** as a second region. |
| `GET /library` | **Library page** (book list with search, status chips, sort + ⚑ has-findings filter). |
| `GET /library/data` | Library fragment. Query: `status`, `q`, `sort` (`recent`\|`title`\|`progress`\|`findings`), `findings` (`1` → only books with recorded findings), `offset`. Sort + has-findings filter are applied **all-in-Go** over the full filtered set. |
| `GET /status/data` | Status fragment (htmx-refreshed every 3 s): counts, pipeline state, read-only phase badge, and the token-gated pause + run-budget controls. |
| `GET /failed/data` | Failed-jobs fragment. **No standalone `/failed` page** — it renders inside `/pipeline`. |
| `GET /servers` · `/servers/data` | **Models page** + fragment (§2.14): the role board, recipes & stale work, LiteLLM gateway, AI endpoints, judge output, ASR runners. The fragment polls every 5 s; the runner-update form lives in the static shell (outside the polled region) so a poll never wipes typed input. DB aggregates are cached 30 s and stale counts 5 min, both served stale-while-revalidate; LiteLLM gateway status 60 s. Only a runner-observation read failure returns 5xx; a failed snapshot still renders `200` with "counts unavailable" for exactly the sections it backs. |
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
    "type": "embeddings",            // "embeddings" | "chat" | "systemone" (required)
    "backend": "ollama",             // "ollama" | "vllm" | "openai-compat" (required)
    "baseURL": "http://ollama:11434/v1", // OpenAI-compatible base (http/https, required)
    "model": "nomic-embed-text",     // model id passed to the API (required)
    "options": { "temperature": "0", "max_tokens": "256" }, // optional; string values
    "apiKeyEnv": "LITELLM_API_KEY",  // optional; NAME of the env var holding the bearer token
    "gateway": "litellm"             // optional; display/API only (see below)
  }
]
```

`gateway` optionally names the routing gateway the endpoint sits behind
(free-form; trimmed and lower-cased; `litellm` is the one the page knows). It is
**display and API only** — no code path routes or behaves differently on it.
When absent, the Models page infers `litellm` from a host name containing
`litellm` or `llm-gateway` and marks it *(inferred)*; `backend` is not used for
this (LiteLLM, bare vLLM and any OpenAI-compatible server all declare
`openai-compat`). Setting `gateway` on an `ollama` endpoint logs a warning (not
fatal).

All three backends speak the OpenAI-compatible REST API; `backend` selects the
dashboard label only (no behavioral difference today). For the `eval` role's
chat endpoint, `options` are sent in the `/chat/completions` request body:

- **Known keys** are parsed: `temperature` and `top_p` as non-negative numbers,
  `max_tokens` as a positive integer. An unparseable value on a `chat` entry
  is a fatal `AI_ENDPOINTS` error at startup, like a malformed baseURL (fail
  loud — never silently the backend default, and never a judge that silently
  fails to build).
- **`apiKey`** is the deprecated bearer-token fallback (below) and is never
  forwarded.
- **Reserved keys** earmark sets itself — `model`, `messages`, `stream`,
  `response_format`, `reasoning_effort`, `chat_template_kwargs` — are rejected
  the same way (the thinking controls have their own env vars, §2.15).
- **Every other key is forwarded** so a future backend needs no code change.
  Option values are strings on the wire, so a value that is a JSON number,
  boolean, array or object is sent as that JSON value (`"top_k": "20"` → `20`,
  `"stop": "[\"END\"]"` → `["END"]`); anything else is sent as the string.

Defaults when unset and the temperature rule for hosted routes are in §2.15
"Request parameters". The embeddings client does not read `options`.

`apiKeyEnv` authenticates an endpoint behind a gateway that requires
`Authorization: Bearer <key>` (e.g. a LiteLLM virtual key). It holds the **name**
of an environment variable, never the token: `AI_ENDPOINTS` is plaintext and
`options` are rendered on the dashboard and in `/api/v1/status`. At startup the
named var is read and the token is sent as the bearer on embeddings requests,
eval-judge chat requests, and the `/models` health probe. The resolved token is
never logged, rendered, or serialized; the var name may be. Omitted → no key:
the embeddings client keeps sending its historical `Bearer ollama` placeholder
and the probe sends no `Authorization` header, so existing Ollama/vLLM
deployments are unchanged. For the eval judge, the resolved key takes precedence
over the deprecated `options.apiKey` (still honored for back-compat; do not put
real secrets there).

#### `systemone` endpoints (TypeSafe System One)

A `systemone` endpoint is a decision model ("Jev") reached through the
LiteLLM pass-through, e.g.

```jsonc
{ "id": "jev", "type": "systemone", "backend": "openai-compat",
  "baseURL": "http://litellm.llm-gateway.svc:4000/typesafe",
  "model": "jev-1.13.0", "apiKeyEnv": "LITELLM_API_KEY" }
```

- `baseURL` is the route prefix: the client (`internal/systemone`) calls
  `POST {baseURL}/v1/systemone` and `GET {baseURL}/v1/models`, with the
  `apiKeyEnv` token as `Authorization: Bearer`. It never follows a redirect.
- `model` MUST be a pinned version (`jev-1.13.0`); a moving alias
  (`jev-latest`, `jev-preview`, any `*-latest`/`*-preview`/`:latest`) or an id
  with no dotted version is a startup error. `options` are refused (the client
  sends only `model`, `state`, `questions`).
- **Request** `{"model", "state": <string>, "questions": {<name>: {"type",
  "instructions", "criteria"}}}` — `model` always sent (omitting it is a 422);
  every question needs `instructions`; `noul` criteria optional
  (`{"true","false"}`), `choice` criteria 1–255 labels → descriptions, `score`
  criteria a list of ≥ 2 levels. The client validates this before sending.
- **Reply** `{"model", "answers": {<name>: {...}}, "usage": {"input_tokens",
  "output_tokens"}}`; per type `noul` → `noul` ∈ [0,1]; `choice` → `choice`
  (an asked label), `probabilities`, `confidence`; `score` → `score` ∈
  [0, levels−1], `confidence`, `legend`, `probabilities`. Decoding is
  **strict** — a missing answer, a type other than the one asked, a missing
  required field, any probability/confidence outside [0,1] or a score outside
  its levels is an error (`invalid_reply`), never a partial answer; answers
  to unasked questions are dropped.
- **Errors are typed**: 422 (not retryable), 429 (with `Retry-After`), 5xx,
  other statuses, and timeouts (15 s default; the caller's deadline counts) —
  429, 5xx and timeouts are retryable. Error values carry no request or
  response body and never the key.
- **Cost**: the `x-litellm-response-cost` header when present and a
  non-negative number, else `input_tokens × usd_per_mtok_in / 1e6` (output is
  not billed) with `usd_per_mtok_in` from `MODELS_FILE`
  `steps.<step>.params` (§2.18), default 0.042.
- Calls go through `internal/fn`, which logs every call in `fn_calls` and
  serves repeats from it (§1.9).

#### `AI_ROLES` (JSON object)

```jsonc
{ "embeddings": "embed-1", "eval": "eval-1", "decide": "jev", "scan": "jev" }
```

`embeddings` is **required** when `AI_ENDPOINTS` is set and MUST resolve to an
endpoint of type `embeddings` — it is the endpoint the worker embeds chunks
with. `eval` is **optional** and MUST resolve to a `chat` endpoint when present
(it is reserved for a future read-only eval layer; absent → no eval).
`decide` and `scan` are **optional** and MUST resolve to `systemone`
endpoints when present (absent → the step is disabled). Configs without them
are unaffected.

#### Validation (fail-closed)

Unlike `ASR_SERVERS` (cosmetic; warn-and-degrade), a malformed AI registry is a
**startup error** — embeddings is the critical path and a silent degrade would
cause invisible embed failures. earmark refuses to start when:

- `AI_ENDPOINTS` is not valid JSON, or any entry has a missing `id`, duplicate
  `id`, missing `model`, unknown `type`/`backend`, or a `baseURL` that is not a
  valid http/https URL with a host.
- An entry's `apiKeyEnv` is not a valid env var name (`[A-Za-z_][A-Za-z0-9_]*`),
  or names a var that is unset or empty — a gateway that needs a key would
  otherwise 401 every embed.
- `AI_ENDPOINTS` is set but `AI_ROLES` is absent.
- `AI_ROLES.embeddings` is empty, points at an unknown id, or points at a
  non-`embeddings` endpoint.
- `AI_ROLES.eval` is set but points at an unknown id or a non-`chat` endpoint.
- `AI_ROLES.decide` or `AI_ROLES.scan` is set but points at an unknown id or a
  non-`systemone` endpoint.
- A `systemone` endpoint names an unpinned model or sets `options`.

(When `AI_ENDPOINTS` is **absent**, a malformed `EMBEDDINGS_BASE_URL` is not
re-validated — the legacy path preserves the prior behavior.)

#### Health probe + dashboard

Each endpoint is probed for liveness on every Models page refresh and in
`GET /api/v1/status` (§2.12): a `GET <baseURL>/models` request
(`GET <baseURL>/v1/models` for a `systemone` endpoint) with a 2s
timeout (carrying the endpoint's bearer token when `apiKeyEnv` is set),
TTL-cached so both render paths share one upstream call. State tokens:

| Condition | Page label | API `state` |
|---|---|---|
| 200 OK + model present (or empty model list) | `✓ lists model` (green) | `ready` |
| 200 OK but configured model not in `/models` | `▲ model not listed` (amber); behind LiteLLM `▲ not allowed` — LiteLLM's `/v1/models` lists only the virtual key's allowed models, so the model is off earmark's key allowlist (403 on call) | `model_not_loaded` |
| non-200 / timeout / unreachable | `✗ unreachable` (grey) | `offline` |
| not probed yet | `? unknown` (grey); an `EVAL_CHAT_*` row `? not probed` | `unknown` |

The page labels say what the probe saw rather than "READY", so endpoint
liveness never reads as role health or runner readiness; the API tokens are
unchanged.

**Liveness is not call success.** `lists model` (`ready`) means the gateway lists the model,
not that calls to it succeed: a judge that 401s on every chat call, or that
LiteLLM silently routes to a fallback, still `lists model`. Role health (below)
folds in call outcomes.

#### Models page (`/servers`)

The Models dashboard page (nav label **Models**; the former Servers /
Models/Services page — the URL stays `/servers`) is a **role board**:
observability only, earmark does **not** route work between endpoints. Top to
bottom:

1. **Roles** — one card per configured pipeline role, in fixed order; roles
   in state `not_configured` are not cards but one muted "○ Not configured:"
   line under them (Decide with its human-decided count; ASR/Judge/Embeddings
   with the reason). Each card shows *requested* (the model id earmark sends,
   `@ <endpoint id>`), *pinned* (`MODELS_FILE` `expected_model` + revision,
   §2.18; the row is omitted when unpinned), *answered* (what the endpoint
   reported serving the call) with `✓ matches pin` or `≠ expected <x>`, last
   ok / last fail ("no failures" when none), and the step's stale count
   linking to its recipe row. A role behind a gateway carries a "via LiteLLM"
   badge in its header. The LiteLLM key allowlist verdict is not a card row:
   a denied model shows as the health line or, when another state outranks
   it, a red "✗ <model> is not on earmark's LiteLLM key allowlist" line.

   | Role | Step | Configured from | Answered from |
   |---|---|---|---|
   | ASR | `asr` | `ASR_SERVERS` primary entry's `model` (else the first) | newest `transcripts` row: `model_name`, `asr_runner_version`, `asr_model_sha256` |
   | Judge | `propose` | `AI_ROLES.eval`, else `EVAL_CHAT_*` (shown as `@ EVAL_CHAT_*`, not probed) | newest `run_metrics.eval_resolved_model`; other models seen in 7 days listed as *also* |
   | Embeddings | `embed` | `AI_ROLES.embeddings` | newest `run_metrics.embed_model` |
   | Decide | `decide` | none yet (`not_configured`; the strip shows judge findings decided by humans) | — |
   | Format | `format` | none yet (`not_configured`) | — |

   Role health (`roles[].state` in the API) — a glyph always pairs with the word:

   | Token | Label | Meaning |
   |---|---|---|
   | `healthy` | ✓ HEALTHY | working; the answering model is the expected one |
   | `idle` | ✓ HEALTHY · idle (green, like `healthy`) | configured and reachable, nothing to do, no recent failure |
   | `degraded` | ▲ DEGRADED | works but something is off: the model is not on the LiteLLM key allowlist (calls 403), a different model answered, the model is not loaded, the asr-runner is stopped on a free GPU, GPUs held by games while jobs wait, or no success for a while with work waiting |
   | `failing` | ✗ FAILING | attempts are failing (judge: newest failure after newest success), or an ASR claim is stalled |
   | `down` | ✗ DOWN | configured but unreachable (red — a configured role that cannot be reached is an alarm, unlike the neutral grey `OFFLINE` of an endpoint row) |
   | `not_configured` | ○ Not configured (one line under the cards, not a card) | no binding |
   | `unknown` | ? UNKNOWN | evidence unavailable (the counts snapshot or runner read failed) |

   Precedence, first match wins.
   - **Judge:** not configured → probe offline (`down`) → requested model not
     on the LiteLLM key allowlist (`degraded`, "every call 403s") → last fail
     after last ok (`failing`) → model not loaded (behind LiteLLM: "not on
     earmark's LiteLLM key allowlist (403 on call)") → answered ≠ expected →
     counts or queue stats unavailable (`unknown`) → transcripts unjudged and
     no success yet, or none in 3 h (3× the hourly backfill; `degraded`) →
     nothing unjudged (`idle`; "nothing to judge yet" on an empty library) →
     `healthy`.
   - **Embeddings:** not configured → offline (`down`) → not on the key
     allowlist → model not loaded → `embed_model` ≠ expected (`degraded`) →
     queue stats unavailable (`unknown`) → backlog with nothing ever embedded,
     or nothing embedded in 1 h (`degraded`) → no backlog (`idle`) → `healthy`.
   - **ASR** (from the runner states; `unknown` when the runner observation
     read failed): any transcribing (`healthy`) → any stalled (`failing`) → any
     ready (`healthy`) → no probed server usable, one of them a free GPU with
     the asr-runner stopped (`degraded`, regardless of the queue — `idle` in the
     `earmark batch` analyze phase, which parks the runner on purpose) → no probed
     server usable, one held by a game or evicting (`degraded` while jobs are
     pending, `idle` when nothing is queued) → every probed server offline
     (`down`) → any idle → no servers (`not_configured`) → `unknown`.

   "answered ≠ expected" compares case-insensitively, ignores a router's
   provider prefix (the same rule as `earmark_model_calls_total`'s `fallback`,
   §2.16: LiteLLM answering `claude-haiku-5-5` for a requested
   `anthropic/claude-haiku-5-5` is a match) and treats Ollama's implicit
   `:latest` as the bare name, but a bare pin does not match an arbitrary tag
   (`qwen3.8` ≠ `qwen3.8:14b`) or a dated snapshot; when unpinned, the answer
   is compared with the requested id. The LiteLLM key-allowlist check stays
   prefix-sensitive — LiteLLM matches the requested name exactly. A failing
   judge shows its last error in red under the state (monospace, clamped to
   two lines; full text on hover); otherwise the last error is a muted "last
   error" row with its time.

   *last ok* on the Judge card is `max(run_metrics.eval_finished_at)`: the
   newest judge success **from any source** (the hourly backfill CronJob,
   in-pipeline judging, the dashboard). earmark records no per-backfill-run
   marker. *failing* counts transcripts whose latest attempt failed (a success
   clears it), not attempts.
2. **Recipes & stale work** — one row per *tracked* step (a step with a
   current recipe): its current recipe (id, model, prompt, since — prompt and
   since hidden on narrow screens) and its `stale_work` count (§1.9), with how
   stale rows converge on hover. Every other step is named on one muted
   "Not tracked:" line (`asr` noted as *provenance per transcript*). Only
   counts are read — never rows.
3. **LiteLLM gateway** — one card per distinct LiteLLM gateway and key (see
   below): readiness, *used by* (role titles; an unbound endpoint by id), key
   alias/status/expiry, spend and budget, rpm/tpm limits, the allowed models,
   and — only when some role's model is not allowed or cannot be checked —
   each role's model marked ✓ allowed / ✗ NOT ALLOWED / ? not checked.
4. **AI endpoints** — the registry as a table: id, role (the role card's
   title, e.g. *Judge* for `eval`), type, gateway (declared, *inferred*, or
   *direct*), host, model, liveness, and options (column shown only when some
   row has options). An env-configured judge appears as an unprobed
   `EVAL_CHAT_*` row.
5. **Judge output** — judge findings by `coalesce(resolved_model, model)`:
   proposed, unanchorable, decided (accepted + rejected + applied + reverted),
   superseded / other (superseded + the patch state `stale`), total.
6. **ASR runners** — last, so the static update form in the shell follows it
   directly. The runner cards (state table above, §2.4, each state paired
   with a glyph; unconfigured history-only hosts only within 30 days), one
   read-only runner version line (running / requested / state, §2.12
   self-update — the update form itself is in the static shell below the
   region; it posts `version` in the form body, and its separate "Clear
   request" control posts an explicit empty `version`, never the typed text),
   a model/runtime/caps table (Runtime and Caps columns hidden while no runner
   reports them; Caps lists only supported capabilities, the full list with
   declined ones and their reasons on hover), and transcript provenance
   grouped by (model, runner version, `.nemo` sha256), newest first, at most 8
   groups; transcripts from a runner that reported no provenance form one
   *not reported* group.

Each table's description is a paragraph above its scroll container (not a
`<caption>`, which would scroll out of view on a phone) and labels the table
via `aria-labelledby`. Times are relative, with the absolute UTC time on hover —
except the header's "updated HH:MM:SS UTC" render clock, which stays absolute
because it is the one value that visibly freezes when polling stops.

**Caching.** The page and `GET /api/v1/status` read the same caches, all served
stale-while-revalidate: a value past its TTL is returned immediately and one
background refresh starts (single-flight); only the very first load waits, and
that wait is bounded by the request. A failed refresh keeps the last good value,
logs once, and is not retried for one TTL (a failed *first* stale-count load
retries after 30 s). A panic inside a background load is recovered and recorded
as that refresh's error.

| Data | TTL | Refresh timeout | Header stamp |
|---|---|---|---|
| Aggregates: model activity, current recipes, judge findings, ASR provenance | 30 s | 3 s | "counts as of" |
| Per-step `stale_work` counts (a full scan of chunks + findings, ≈1.6 s at production size) | 5 min | 30 s | "stale counts as of"; "loading…" for at most 1.5 s on the first load, then shown when ready |
| LiteLLM gateway readiness + key info | 60 s | 2 s per request (≤ 3 requests) | — |

A slow or failed stale count marks only the stale numbers unavailable; with no
good aggregate snapshot the page shows "counts unavailable" for the sections it
backs and still returns `200`.

#### LiteLLM gateway

For every distinct LiteLLM gateway the registry routes through (`gateway:
litellm`, declared or inferred from the host), earmark reads — with **each
endpoint's own virtual key** (`apiKeyEnv`), never the master key, and never a
model call:

- `GET <base>/health/readiness` (fallback `GET <base>/health/liveliness`):
  proxy health and DB connection. No key needed.
- `GET <base>/key/info` with `Authorization: Bearer <virtual key>` and **no**
  query parameter: LiteLLM returns the caller's own key — `key_alias`,
  `models` (the allowlist; empty = all models; `provider/*` wildcards and
  `all-proxy-models` honored), `spend`, `max_budget`, `budget_duration`,
  `budget_reset_at`, `expires`, `blocked`, `status`, `tpm_limit`, `rpm_limit`.
  Only those fields are decoded; the hashed token and every other field are
  ignored, so no key material is rendered, logged, or returned.

`<base>` is the endpoint `baseURL` without a trailing `/v1`. Requests use a 2 s
timeout, no redirects, http/https only, and a 64 KB body cap. A 401/403 from
`/key/info` degrades the gateway card ("key info not readable by earmark's
key"); roles are then not checked against the allowlist (`modelAllowed:
null`).

**Allowlist verdict.** The endpoint's own `GET /v1/models` probe is
authoritative — behind LiteLLM it lists exactly what the virtual key may call —
and the `/key/info` allowlist never overrides it:

| Allowlist (wildcard-aware) | `/v1/models` probe | `modelAllowed` |
|---|---|---|
| allows | any | `true` |
| does not match | lists the model (`ready`) | `true` — the entry was an access group earmark can't expand |
| does not match | does not list it (`model_not_loaded`) | `false` → role `degraded`, "every call 403s" |
| does not match | offline / not run | `null` — no deny without the probe |
| team key with an empty list (inherits the team's models), `all-team-models`, empty requested model, unreadable key info | — | `null` |

The first load of a gateway's status is waited for at most 1.5 s per render
(shared across gateways); after that it is served from cache.

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
   Its bearer token is the key resolved from the entry's `apiKeyEnv`, else the
   deprecated `options.apiKey`.
2. Standalone `EVAL_CHAT_*` env vars (fallback when no `eval` role is bound).

The call uses the OpenAI-compatible `POST {base}/chat/completions` shape.

| Env var | Required | Meaning |
|---------|----------|---------|
| `EVAL_CHAT_BASE_URL` | if no `eval` role in `AI_ROLES` | OpenAI-compatible base URL, e.g. `http://vllm:8000/v1` |
| `EVAL_CHAT_MODEL` | if no `eval` role in `AI_ROLES` | judge model id |
| `EVAL_CHAT_API_KEY` | no | bearer token if the endpoint requires one |
| `EVAL_REASONING_EFFORT` | no | `auto` \| `omit` \| a literal value — see "Thinking controls" below |
| `EVAL_CHAT_TEMPLATE_KWARGS` | no | `auto` \| `omit` \| a JSON object — see "Thinking controls" below |

**Thinking controls.** Local reasoning models (Qwen-family on Ollama) must be
told not to think, or they spend the reply on chain-of-thought and return empty
content (`ErrThinkingOnlyResponse`). The two families spell it differently, so by
default (`auto`) the judge sends both `reasoning_effort: "none"` and
`chat_template_kwargs: {"enable_thinking": false}`. Hosted provider routes reject
or mis-map those fields (Anthropic has no `"none"` effort and no chat-template
passthrough), so for a model id containing `anthropic/` anywhere (LiteLLM's
provider route convention, case-insensitive — so nested routes like
`bedrock/anthropic/…` match too) `auto` omits **both**. A LiteLLM **alias** that
hides the provider (e.g. model `judge` mapped to Claude in the proxy config)
cannot be detected from the id: set `EVAL_REASONING_EFFORT=omit` and
`EVAL_CHAT_TEMPLATE_KWARGS=omit` for it. Either field can be
forced per deployment: `omit` never sends it; any other value is sent verbatim
(`EVAL_CHAT_TEMPLATE_KWARGS` must then be a JSON object). Route-independent:
`response_format` (JSON schema) and the bearer token from the endpoint's
`apiKeyEnv` (§2.14).

**Request parameters.** From the eval endpoint's `options` (§2.14), with these
defaults when a key is unset (the `EVAL_CHAT_*` fallback has no options, so it
always gets the defaults):

| Field | Default | Why |
|---|---|---|
| `max_tokens` | `8192` (`defaultJudgeMaxTokens`) | The reply itself is small, but on hosted reasoning models (Claude Haiku 5.5 thinks adaptively by default) the output budget is **shared with thinking** — a tight cap gets spent thinking and the answer is truncated. 8192 leaves room for that and is still a ceiling on a runaway generation. Ollama maps it onto `num_predict`. |
| `temperature` | `0` for local models; **omitted** for hosted routes | Local judges need determinism (the same span should flag the same way run to run). Hosted routes (`isHostedRoute`: `anthropic/` anywhere in the model id) are sent no temperature, because newer Claude models reject any non-default sampling parameter (`temperature`/`top_p`/`top_k` → 400). An explicit `temperature` option is always sent, on any route. |
| `top_p` | omitted | Sent only when configured. |

The judge's `propose` recipe params (§1.9) and the span's
`gen_ai.request.temperature` (§2.16) record the temperature **actually sent**,
and omit it when none is. **Recipe consequence:** on a hosted route the recipe
params lose `temperature` (previously always `0`), so the current `propose`
recipe id changes and every prior propose finding from that route is listed in
`stale_work`. That is intended to coincide with the judge model switch (a new
model changes the recipe anyway); nothing re-judges automatically — findings
converge only by re-judging (`earmark eval --backfill-*`). Local routes keep
`temperature: 0` and so keep their recipe id.

**Fail-closed replies.** A reply the judge cannot use is an **error**, never
"no findings" — an empty answer read as zero findings is indistinguishable
from a clean chunk, would latch the transcript as judged (§1.5), and would
never be retried. The client checks the first choice's `finish_reason` and
message, in this order:

| Condition | Error | `error.type` |
|---|---|---|
| `finish_reason` `content_filter` or `refusal`, or a non-empty `message.refusal` | `ErrRefusalResponse` | `refusal` |
| `finish_reason` `length` (hit `max_tokens`; any content is cut off) | `ErrTruncatedResponse` | `truncated` |
| empty/whitespace content with non-empty `reasoning`/`reasoning_content` | `ErrThinkingOnlyResponse` | `thinking_only` |
| empty/whitespace content otherwise (e.g. Haiku 5.5's empty thinking blocks → empty `reasoning_content`) | `ErrEmptyResponse` | `empty` |

Each goes through the existing per-chunk failure path: the chunk is counted
skipped (not evaluated), the run is not complete, and the transcript gets a
failure record (`eval_failed_at`, `eval_error`) instead of `eval_finished_at`
— so `earmark eval --backfill-eval-errors` re-judges it. The content is
dropped; the reported model and usage are still recorded. (A reply that has
content but is not valid findings JSON is still the advisory soft-fail of
`JudgeChunk`; a reply cut off by `max_tokens` never reaches it.)

**Resolved model.** The judge records the model the endpoint **reports** serving
each request (the response's `model` field) next to the requested id, because a
router can resolve an alias to a dated id or fall back to another model:
`transcript_findings.resolved_model` per finding (`model` stays the requested
id) and `run_metrics.eval_resolved_model` per judged job (distinct values,
comma-joined in first-seen order, if a router switched models mid-run). Both are
NULL when the endpoint omits the field. In-pipeline and standalone eval events
carry it as `detail.resolved_model`.

Each finding also carries `recipe_id` (§1.9): the judge's `propose` recipe —
requested model, the model that answered, prompt version `judge@v1` and the
hash of every prompt part sent, and the confidence floor / per-chunk cap — so
a fallback answer is a different, re-runnable recipe. Bump `judgePromptVersion`
(`internal/eval/prompt.go`) whenever the prompt or response schema changes;
`TestJudgePromptVersionPinned` fails until you do.

#### `transcript_findings` table

```sql
CREATE TABLE transcript_findings (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    transcript_id        UUID        REFERENCES transcripts (id) ON DELETE SET NULL,
                                                  -- NULL once a requeue replaced the transcript (§1.4); never cascades into transcripts
    file_path            TEXT        NOT NULL,
    chunk_id             UUID,                    -- the evaluated chunk (nullable)
    chunk_index          INTEGER,
    start_sec            FLOAT8      NOT NULL,
    end_sec              FLOAT8      NOT NULL,
    original_text        TEXT        NOT NULL,    -- the suspected-wrong span, verbatim
    issue_type           TEXT        NOT NULL,    -- see vocabulary below
    suggested_correction TEXT,                    -- ADVISORY ONLY — never applied
    confidence           FLOAT8      NOT NULL,    -- judge self-score 0..1 (the triage/scoring signal)
    model                TEXT        NOT NULL,    -- judge model id REQUESTED (attribution)
    resolved_model       TEXT,                    -- model the endpoint reported serving it (NULL = not reported)
    transcription_run_id UUID,                    -- transcription_jobs.id — per-backend/run attribution
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    recipe_id            TEXT REFERENCES recipes (recipe_id), -- propose recipe that made it (§1.9); NULL for origin='human'
    superseded_at        TIMESTAMPTZ          -- when a requeue archived it (§2.17 superseded); NULL otherwise
);
-- indexes: file_path, transcript_id, transcription_run_id, issue_type, recipe_id
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

**Decoupled mode (`EVAL_IN_PIPELINE=false`, `EVAL_GATES_EMBED=false`).** The
recommended shape when the judge is slow or remote (e.g. a hosted model behind
LiteLLM): the worker only embeds — no judge call can delay search — and judging
runs as its own pass, `earmark eval --backfill-unevaluated --write [--limit N]`,
on whatever schedule the deployment chooses (the chart's optional
`evalBackfill` CronJob, §2.7). That pass selects
every done transcript without the `eval_finished_at` latch regardless of embed
state, judges its **stored** chunk rows (so findings reference the real chunk
IDs), and latches it on success (§1.5). Without the gate the embed worker assigns
**random** chunk IDs at insert time, so a transcript that has **not been embedded
yet** is skipped and left unlatched — judging regenerated chunks would record
findings against IDs that never exist (orphans; `chunk_id` has no FK) and latch the job
for good. The next run judges it once it is embedded. (Under
`EVAL_GATES_EMBED=true` chunk IDs are deterministic UUIDv5, so a not-yet-embedded
transcript is judged on regenerated chunks, exactly like the gated eval pass.)
`eval_findings` counts the findings that run actually recorded (re-judge
duplicates of an earlier partial run are not counted twice).

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
existing deployments enabling the gate, the retry path for judge runs that
failed, and the judging pass for a deployment that keeps the judge out of the
embed path (below). It selects done jobs with `eval_finished_at IS NULL`
regardless of embed state, judges them, and writes `eval_finished_at` — only
when every chunk was judged (§1.5 "Latch only on success"); a failed transcript
gets the `eval_failed_*` record and is picked up again by the next run. It is
safe over live embedded data: it only INSERTs findings and UPSERTs the eval
slice — it does NOT touch `transcript_chunks` or `transcripts`. Run with
`--write` to persist; omit for a dry-run preview. `--limit N` caps the
transcripts judged **successfully** per run — latched, or in a dry run, that
would latch (0 = all). Transcripts skipped without a judge call (empty raw text,
not embedded yet under an ungated deployment, a read error) and transcripts
whose judging failed do **not** count: neither ever leaves the selection, and
keyset order puts them first on every run, so counting them let a head of
never-latching rows use up the limit on every run and stall the backfill for
good. Counting successes guarantees progress — each run latches N more or
exhausts the selection. The selection is walked in keyset pages of 32 so memory
stays bounded, and the cursor moves past every row it visits, so no row is
selected twice within a run and the run ends at the end of the selection.
`--limit` is the progress target; **`--max-attempts N` is the spend cap**: it
counts every transcript the judge was actually called for, latched or not, and
stops the run when reached. It defaults to **3 × `--limit`** when `--limit` is
set, and is unbounded only when `--limit` is 0 too (an explicit `--max-attempts`
caps an unlimited run). Without it a judge that fails *some* chunk of every
transcript (rate limiting, a flaky endpoint) or a systematic write failure after
judging would leave every transcript unlatched and turn `--limit 25` into a paid
sweep of the whole selection. A dry run counts attempts the same way (its judge
calls cost the same). A `--limit` run also stops early, with an error, after
**5 consecutive transcripts fail on every chunk** — the fast exit for a dead
endpoint. **Exit status:** a backfill that judged at least one transcript but
latched none (`failed > 0`, `latched == 0`) exits **non-zero**, so a dead API
key or endpoint fails the scheduled Job instead of hiding behind exit 0; a run
that latched anything, or only skipped rows, exits 0. **Run
`earmark prune-chunks --yes` before any `--backfill-*`** on a corpus that may
carry orphan chunk tails (§2.17): the backfill judges stored rows, so a finding
judged against an orphan tail that a prune later deletes is anchored to a chunk
that no longer exists. After the command completes, `embedded ⟹ eval'd`
holds for the existing corpus (minus any transcripts the judge failed on, which
remain selectable).

**`earmark eval --backfill-eval-errors`** re-judges done jobs whose judging
**failed**, including legacy ones the pre-fix pipeline latched as done anyway.
A job is selected when any of these holds:

1. `run_metrics.eval_failed_at IS NOT NULL` (a failure recorded under the
   latch-only-on-success rule);
2. it is latched but `eval_skipped > 0` (the old ungated path latched partial
   runs);
3. it is latched but a per-job `pipeline_events` row with `stage='eval'` and
   `event='error'` — or `event='finish'` with a numeric `detail.skipped > 0` —
   was written at/after that job's `eval_started_at` (the old gated pass latched
   every judge failure and zeroed `eval_skipped`, so the event log is the only
   record; this is how the ~85 historical eval errors are found);
4. it is latched with `eval_resolved_model IS NULL`, its `eval_chunks` is lower
   than the number of `transcript_chunks` rows the transcript has, **and** no
   chunk was added since that run's `eval_started_at` (`eval_finished_at` when
   unset). The old CLI backfill stopped on the first client timeout and still
   latched, with `eval_skipped = 0`, the chunks judged so far, and no event. A
   NULL resolved model covers every pre-0a run plus post-0a runs whose endpoint
   reported no model (or that latched with no judge configured); post-0a runs
   latch only on full success, so they cannot have a short `eval_chunks`. The
   chunk-age guard exists because chunks added after the run — a re-embed
   (`requeue --reembed` deletes, the worker re-inserts) or a re-chunk that adds
   rows — change the count without the judge having seen them. An in-place
   stale rebuild keeps `created_at`; it keeps the count too unless the re-chunk
   yields fewer chunks, in which case the tail is pruned (§2.17) and the count
   drops — which can only hide a match, never add a re-judge. `eval_started_at`
   is host time and `created_at` is the database's `now()`, so clock skew
   matters only for chunks written within the skew of the run (a lagging host
   clock hides a match; a leading one can add a re-judge). **Known gap:** a
   legacy aborted run whose chunks were added to or replaced after it cannot be
   detected by this rule — the set it judged is gone from the count. Re-judge
   such a book explicitly (`earmark eval <book> --write`) if needed.

Findings already recorded for a transcript (same chunk, span, issue type and
correction) are not inserted again. A successful re-judge writes a new
`eval_started_at`, `eval_skipped = 0`, `eval_chunks` = every chunk, and clears
the failure record, so the job leaves all four conditions. Same `--write` / dry-run / `--limit` semantics as
`--backfill-unevaluated`; the two flags are mutually exclusive.

```bash
earmark eval --backfill-eval-errors --limit 10           # preview the first 10
earmark eval --backfill-eval-errors --limit 10 --write   # re-judge them
earmark eval --backfill-eval-errors --write              # all of them
```

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
new route, env var, or column; informational only. The rollups, worklists and
the library's per-book findings count all **exclude `superseded`** findings
(archived by a requeue, §1.4), so a re-judged book is not counted twice; they
stay readable through `list_transcript_corrections state=superseded`.

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
| `earmark_runner_last_heartbeat_seconds` | gauge | — | Seconds since the runner's last **claim-activity**. The runner only stamps a heartbeat while a job is claimed (no idle heartbeat, §1.7), so this is NOT idle liveness and CANNOT distinguish "idle, queue empty" from "down". **Omitted entirely when there is no claim/completion history** (so an alert can't misread a multi-day age). It also grows while work is pending if the runner is deliberately gated (`paused`, `run_limit=0`, batch analyze phase), so it is not a liveness signal even with a non-empty queue — alert on `earmark_runner_alive_seconds` for that (§1.7). |
| `earmark_runner_alive_seconds` | gauge | — | Seconds since the runner's last **liveness** heartbeat (`runner_control.runner_heartbeat_at`), stamped **every** poll cycle — working, idle, or paused. Unlike `earmark_runner_last_heartbeat_seconds` this stays fresh on a drained queue, so it distinguishes "idle, queue empty" (small value) from "runner down" (large/growing value). **Omitted until the runner has stamped at least once.** |
| `earmark_runner_available` | gauge | — | `1` when the GPU host is free for transcription (gpu-arbiter not gaming), `0` when gaming/evicting. Omitted until a `runner_availability` event has been observed. |
| `earmark_stage_duration_seconds` | histogram | `stage` | Per-stage processing duration, observed at Go-emitted finish events (`embed`, `eval`). |
| `earmark_jobs_completed_total` | counter | — | Go-observable embed-stage completions (best-effort — the **runner** owns the job `done` transition, so this counts the worker's embed finishes, not the runner's mark-done). |
| `earmark_jobs_failed_total` | counter | — | Go-observable job failures (the stale-claim attempt-cap path). Best-effort — runner-side failures are not counted here; `earmark_jobs{status="failed"}` is the authoritative current failed count. |
| `earmark_eta_work_seconds` | gauge | — | Empirical busy-time ETA for the remaining chunks (§4). Omitted when there is no remaining work / no rate history. |
| `earmark_eta_calendar_seconds` | gauge | — | Empirical calendar ETA (work ÷ runner-availability fraction, §4). Omitted when availability history is absent. |

Standard `go_*` and `process_*` collectors are also registered for baseline
observability. Both pods also serve `/healthz` (liveness, always-200).

#### OpenTelemetry (metrics, traces, logs)

Both Go pods (and `earmark eval`) run the OpenTelemetry SDK (`internal/telemetry`):
one MeterProvider whose instruments are read by **both** a Prometheus pull
exporter — served on the same `/metrics`, next to the metrics above, whose names
are unchanged — and, when an OTLP endpoint is configured, an OTLP push exporter.
The push also carries the client_golang metrics above (bridged, same names), so a
push-only backend sees the whole surface. Traces go out over OTLP only. A
setup error is logged and never stops a process. `earmark monitor` (after its
graceful stop) and `earmark mcp` (on SIGTERM/SIGINT) flush and shut the
providers down, bounded at 10 s / 5 s; `earmark eval` flushes when it
finishes, but installs no signal handler, so an interrupted run drops its
buffered spans. An in-flight `earmark_stale_items` count is cancelled first
and never holds shutdown past that bound.

Configuration is **only** the standard OpenTelemetry environment (§2.4) —
nothing about any collector is hard-coded:

| Variable | Effect |
|---|---|
| `OTEL_SDK_DISABLED=true` | Everything off: no OTel series on `/metrics`, no spans. The metrics above are unaffected. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_{TRACES,METRICS}_ENDPOINT`) | Turns OTLP on. **Unset → no OTLP at all** (no SDK default endpoint), nothing dials. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` (or per signal) | `http/protobuf` only — the spec default and the one exporter earmark ships. Use the collector's OTLP/HTTP port (Alloy `:4318`, not the gRPC `:4317`). Any other value turns that signal's OTLP export off with a warning. |
| `OTEL_METRICS_EXPORTER` | Comma list of `prometheus`, `otlp`, `none`. Default `prometheus,otlp` (otlp only with an endpoint). |
| `OTEL_TRACES_EXPORTER` | `otlp` (default, only with an endpoint) or `none`. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Resource; `service.name` defaults to `earmark`, `service.version` to the build. |

The exporters read the rest of the standard set themselves
(`OTEL_EXPORTER_OTLP_HEADERS`, `_INSECURE`, `_TIMEOUT`,
`OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_BSP_*`).

> **Do not ingest metrics twice.** If the same Prometheus both scrapes
> `/metrics` (PodMonitor) and receives the OTLP metric push, every series
> arrives twice under different `job` labels. Pick one per deployment — e.g.
> keep the scrape and set `OTEL_METRICS_EXPORTER=prometheus` while traces go
> over OTLP.

The Prometheus exporter emits no `target_info` and no `otel_scope_*` labels:
the series below are the whole addition to `/metrics`.

OpenTelemetry instruments (Prometheus names; the cardinality rule holds —
labels are only step, recipe, model, fn, outcome; book, ASIN and chunk ids go
on spans and logs, never labels):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `earmark_build_info` | gauge = 1 | `version`, `commit` | The running build. |
| `earmark_recipe_info` | gauge = 1 | `step`, `recipe`, `model`, `revision`, `prompt_version` | One per step in `current_recipes` (§1.9), loaded by the ingest pod at startup after it registers them. |
| `earmark_stale_items` | gauge | `step` | Rows of the `stale_work` view per step that has a current recipe (0 included). Ingest pod; refreshed every 5 min (one aggregate per step, ~15 ms at 39k chunks + 34k findings). |
| `earmark_model_calls_total` | counter | `fn` (`judge`, or a pure function's name), `model` (requested), `outcome` (`ok` · `error` · `fallback` · `cached`) | Every judge call and every pure-function call (§1.9 `fn_calls`). `fallback` = answered by a model other than the registry's `expected_model` (§2.18), compared without a router's route prefix (`anthropic/claude-…` = `claude-…`). `cached` = a pure-function call served from `fn_calls`, no model request made. |

**Traces.** Each judge call is one `chat <model>` client span following the
OpenTelemetry GenAI semantic conventions, **pinned to semconv v1.40.0** (they
are still "development"; earmark's own `earmark.*` attributes are
authoritative): `gen_ai.operation.name=chat`, `gen_ai.provider.name` (the
LiteLLM route prefix of the model id, e.g. `anthropic`, else
`openai_compatible`), `gen_ai.request.model`, `gen_ai.request.temperature`
(only when the request sends one, §2.15),
`gen_ai.response.model` (the resolved model), `gen_ai.usage.input_tokens` /
`output_tokens` (from the response `usage`), `server.address` / `server.port`,
on failure an error status whose description and `error.type` are a bounded
class only — the HTTP status code (`422`), `timeout`, `canceled`, an
unusable-reply class (`thinking_only`, `empty`, `truncated`, `refusal`; §2.15
"Fail-closed replies") or `_OTHER` — never the error text, because an upstream error
body can echo the request (no exception event is recorded; the full error
still reaches the caller's logs), and `earmark.step`, `earmark.fn`,
`earmark.recipe_id` (the recipe that stamped its findings), `earmark.transcript_id`,
`earmark.chunk_id`. Prompt and completion content are **never** recorded.

**Pure-function spans.** Each `internal/fn` call — served from cache or
not — is one `systemone <model>` client span: `gen_ai.operation.name=systemone`
(custom; semconv has no decision operation), `gen_ai.provider.name=typesafe`,
`gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens` /
`output_tokens` (when a request was made), `earmark.fn`, `earmark.step`,
`earmark.recipe_id`, `earmark.cache_hit`, and on failure the same bounded
`error.type` / status rule as the judge. The state text, the questions and the
answers are **never** recorded on spans, metrics or logs — only in the
`fn_calls` row.

**Logs.** With `LOG_FORMAT=json`, a record logged with a context inside a span
carries `trace_id` and `span_id`. In MCP stdio mode every log line goes to
stderr (stdout carries JSON-RPC).

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
    │               │                        │
    │               └──> proposed            └──> stale
    │
    └──reanchor──> unanchorable ──reanchor──> proposed

(any state) ──requeue──> superseded            (terminal)
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
- **unanchorable** — `earmark reanchor` could not place the finding's span in
  the transcript's current chunks: it is in none of them (`anchor_not_found`)
  or in more than one candidate place (`anchor_ambiguous`), recorded in
  `unanchorable_reason`. **Not terminal**, unlike `stale`: a later re-anchor
  (after another re-chunk) that places the span returns it to `proposed`. It is
  never in the overlay, and its only legal moves are back to `proposed` (a
  re-anchor placed it) or to `superseded` (a requeue replaced its transcript) —
  it can never be accepted or applied. Both re-anchor moves belong to the
  re-anchor pass, not to a reviewer: `patch.IsMachineTransition` marks them, and
  the review tool (`decide_transcript_correction`) offers no action on an
  unanchorable finding.
- **superseded** — the finding's transcript was replaced by an operator requeue
  (§1.4). Set by requeue only, from **any** other state — human decisions and
  `unanchorable` included — in the same transaction as the transcript delete,
  stamped `superseded_at` (an `unanchorable` finding's `unanchorable_reason` is
  cleared: the schema allows it only on `unanchorable` rows); `transcript_id`
  then becomes NULL. `patch.IsMachineTransition` marks the move, so
  `SetPatchState` refuses it. **Terminal**, and
  **excluded from the overlay** (`GetCorrectionOverlay` selects only
  `accepted`/`applied`): the text it describes no longer exists, and the new
  transcript is judged fresh. Kept for audit and the bench; never deleted, and
  no review action (`accept`/`reject`/`revert`/`reconsider`) applies to it.
  The re-anchor pass never reads a superseded finding (nor, equivalently, one
  with a NULL `transcript_id`).

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

#### Re-anchoring (`earmark reanchor`)

> Added 2026-10 (v2 phase 0b). Migration 4.

An anchor names one revision of one chunk, and re-chunking (`requeue
--reembed`, a `CHUNK_SIZE` change, a re-transcription) regenerates every chunk.
The same index can then hold different text, under the same deterministic id
or a new one — so the **hash, not the id**, is what proves an anchor current.
Every finding recorded before it describes text that no longer exists, and the
first rebuild that replays it retires it as `stale` — terminally. Measured
2026-10-06: 25,442 of the 25,452 gemma3:12b-era findings
named a `chunk_id` with no `transcript_chunks` row, and only 5,551 of them had
their span (as a substring) in the chunk at their `chunk_index`.

`earmark reanchor [--book S] [--limit N] [--batch N] [--yes]` re-anchors the
backlog. It reads `proposed` and `unanchorable` findings only — a finding a
human has decided (`accepted`, `applied`, `rejected`, `reverted`), a `stale`
one, or a `superseded` one (archived by a requeue; its `transcript_id` may be
NULL) is never touched — and classifies each with `patch.Reanchor` against the
transcript's current **pristine** chunks (`COALESCE(source_text, text)`):

| Outcome | Meaning | Write (`--yes`) |
|---|---|---|
| `already` | the existing anchor resolves — exactly what replay checks: the chunk the finding addresses (by `chunk_index`, by `chunk_id` only when it recorded no index — "How a finding names its chunk" below) still has the recorded hash and `Locate` places the span. The `chunk_id` itself is not compared — a legacy id that names no row does not stop the anchor from replaying | none |
| `unique` | the one candidate is in the chunk the finding names (same `chunk_index`) | fresh anchor |
| `moved` | the one candidate is in another chunk covering the judged audio window | fresh anchor, on that chunk |
| `ambiguous` | two or more candidates | `unanchorable`, `anchor_ambiguous` |
| `none` | no candidate a single chunk can hold: the span is not in the judged text, or its only occurrence straddles a chunk boundary | `unanchorable`, `anchor_not_found` |
| `pending` | the transcript has no chunks right now (mid re-embed) | none — "no text yet" is not "span gone" |

**The match.** Case-sensitive (replay splices the exact span, so a case-folded
anchor would only go stale later) and **word-bounded**: an edge of the span
that is a word character (Unicode letter, digit, `_`) must not continue a word
in the chunk — `ganema` does not match inside `proganema` — while a
punctuation edge needs no boundary. Overlapping matches count (`ha ha` occurs
twice in `ha ha ha`): two placements are an ambiguity either way.

**Where it looks.** `start_sec`/`end_sec` is the audio window of the chunk the
judge was shown. Audio time does not move when text is re-chunked, so the judged
occurrence always lies in the current chunks overlapping that window — and
nowhere else. Without an offset (no gemma-era row has one), a second copy of the
span inside the window means nobody knows which one the judge meant. So:

1. **Named chunk unchanged** (its pristine text still has the recorded hash):
   it *is* the judged text and alone decides — one match re-anchors, more is
   ambiguous, none means the judge misquoted it (the same words elsewhere are
   a different place).
2. **Otherwise**, the chunks overlapping `[start_sec, end_sec)` are joined in
   `chunk_index` order with `" "` (how the worker joins segments) and searched
   as **one** text, so an occurrence straddling a chunk boundary is still a
   candidate. Exactly one candidate that lies wholly inside one chunk
   re-anchors there; two or more is `ambiguous`; a lone straddler cannot be
   anchored to any single chunk and is `none`. The named `chunk_index` gets no
   preference — after a re-chunk it is just one of the window's chunks — and a
   match outside the window is never a candidate.
3. **No usable window** (`end_sec <= start_sec`; no live row has one): the named
   chunk, then the whole transcript.

**A fresh anchor** is `chunk_id` and `chunk_index` (read FROM the chunk row),
`chunk_text_sha256` = `patch.ChunkHash` of the pristine
text, `anchor_offset` = the rune offset of the match, and `anchor_occurrence` =
its index among the **plain-substring** occurrences — `Locate`'s numbering, so
the existing offset → occurrence ladder lands on exactly this span even when
the word-bounded match is not the first raw substring hit. `reanchored_at` is
stamped; `unanchorable_reason` is cleared. `start_sec`/`end_sec` are **not**
rewritten: the judged window is the evidence the next re-anchor (after the next
re-chunk) searches by, and the chunk's own timing is one join away via
`chunk_id`. An `unanchorable` row keeps its
original anchor columns (the only record of where the judge saw it) and is only
rewritten when its reason changes or it can now be placed.

**Concurrency.** Dry-run (the default) reads without locks and writes no
finding — but like every command it opens the database through `db.New`, which
applies any pending migrations first, so a new binary's dry-run against an
un-migrated database still applies migration 4.
`--yes` walks transcripts in keyset batches (`--batch`, default 25 — the batch
bounds both lock time and the chunk text held in memory), one transaction per
batch: findings are read `FOR UPDATE SKIP LOCKED` (a finding a reviewer is
deciding right now is skipped and picked up next run), then the batch's chunks
`FOR SHARE` (the worker's rebuild upsert waits for the batch instead of changing
the text mid-write; taken after the finding locks, in chunk order, so the two
cannot deadlock). That is the repo-wide order recipes → findings → chunks
(§2.17 "Lock order"): re-anchoring takes no `recipes` lock, never waits on a
finding (SKIP LOCKED), and only waits on chunk rows, which a rebuild locks
after every finding lock it needs. Both writes are compare-and-swaps on the state the row was
read in, and the anchor write re-checks the chunk's pristine hash in SQL, so a
row decided or rebuilt between read and write matches nothing and is reported
as skipped. Every state change is checked against `patch.CanTransition` first.
The pass never stamps `decided_at`/`decided_by` (it is not a review), never
flags `embedding_stale` (a `proposed` finding is not in the overlay) and never
writes chunk text or `transcripts`.

**Idempotent.** A re-anchored finding reads as `already` on the next run; an
unanchorable one with an unchanged reason is left alone. Re-run after every
re-chunk.

**Measuring it without the data.** `internal/db/testdata/reanchor_survival.sql`
is a server-side mirror of the classifier that returns only counts per
`(model, outcome)` — run it bounded (a transcript-id bucket per statement, with
`SET statement_timeout`) rather than pulling chunk text out of Postgres.
`TestIntegrationReanchorSurvivalSQLMatchesGo` pins it to the Go matcher.

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

**How a finding names its chunk.** Every statement that resolves a finding's
chunk — this invalidation, the prune below, the reviewer worklist
(`listCorrectionsSQL`) and the re-anchor pass's named chunk — addresses it by `(transcript_id, chunk_index)` whenever
the finding recorded a `chunk_index`, and by `chunk_id` only when it did not.
`chunk_index` is what the replay is keyed by, so every other statement must
resolve the same row. `chunk_id` is not reliable alone: on 2026-10-06, 25,442
of 32,337 live findings carried a deterministic UUIDv5 `chunk_id` while the row
at their index had a random id from the ungated embed path, so a
`chunk_id`-first lookup flagged nothing on accept and showed reviewers an empty
chunk. Where both resolve to a row they resolve to the same one (verified live:
0 findings whose `chunk_id` names a row at a different index), so index-first
addressing is a strict superset; the arms are mutually exclusive, so a finding
never matches two chunks.

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

**Re-chunking into fewer chunks: the tail is pruned.** `InsertChunks` upserts on
`(transcript_id, chunk_index)` and, in the **same transaction**, deletes the
transcript's rows at `chunk_index >=` the new chunk count. Without the prune, a
rebuild whose re-chunk yields fewer chunks (a `CHUNK_SIZE` or chunker change
since the first embed) would leave the old tail behind — stale text that still
matches searches and still carries findings. The upsert also refreshes the
position columns (`file_path`, `start_sec`, `end_sec`, `speaker`) on conflict,
not just `text`/`source_text`/`embedding`, because a re-chunk can move a chunk's
boundaries without changing its index; the existing row `id` is kept, since
findings reference it. Nothing has a foreign key to `transcript_chunks`
(`transcript_findings.chunk_id` is a bare UUID, §2.15), so the delete can never
fail or cascade. Findings are **never deleted**: in the same statement, the
findings addressed to the tail — `chunk_index >=` the new count, or (only for a
finding with no `chunk_index`) a `chunk_id` naming a pruned row — that may
legally become `stale` — `proposed`, `accepted`, `applied` — move to `stale`
with `stale_reason = chunk_changed`, which is exactly what
the replay already does to an accepted/applied correction whose chunk index no
longer exists. `decided_at`/`decided_by` are untouched, and `rejected`/`reverted`
decisions keep their state. Without that retire, accepting a `proposed` finding
on a pruned chunk would flag no chunk for rebuild and leave it `accepted` and
invisible forever. An empty chunk set never prunes.

**Lock order.** `SetPatchState` locks the finding and then its chunk. The
prune transaction (both `InsertChunks` and the standalone `prune-chunks`) takes
the same order: before touching any chunk row it row-locks the findings it may
retire (`SELECT … FOR UPDATE OF f` over the transcript's findings at
`chunk_index >= keep`), then upserts and prunes. `InsertChunks` registers its
embed recipe (§1.9) first, in the same transaction — that touches only
`recipes`, which no finding/chunk lock holder waits on — and the upsert
refreshes `recipe_id` on conflict along with the other derived columns. An accept racing a rebuild
therefore waits rather than deadlocks: if the accept committed first, the prune
retires it (its state guard re-checks); if the prune locked first, the accept
then finds the finding `stale` and gets `ErrPatchStateConflict`.

Rows pruned **before** this fix are cleaned up by `earmark prune-chunks`
(dry-run unless `--yes`). It re-chunks every chunked transcript with the
deployment's `CHUNK_SIZE`, compares pristine-text hashes, and prunes only the
clean-rebuild signature: rows `0..n-1` all match the re-chunk and rows `>= n`
exist. A transcript whose head differs was embedded under another chunking —
its extra rows are real coverage — and is reported as `drift` (fix with
`requeue --reembed`), never pruned.

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
| MCP `get_transcript` | CORRECTED | **closed**: serves the chunk projection (`transcript_chunks.text`, `GetCorrectedTranscriptPage`) when any chunk differs from its pristine text, `corrected: true`, no word timestamps; else the segments, `corrected: false` |
| MCP `get_transcript` with `includeWordTimestamps=true` | ORIGINAL (by necessity) | word times exist only on segments; the response flags `corrected: false` and, when corrections exist, says so in `note` |
| web transcript reader (`GetTrackDetail` → segments) | CORRECTED | **KNOWN GAP** |

> **Known gap (narrowed).** `get_transcript` now serves the corrected text —
> the same projection search returns — at **chunk** granularity, because
> corrections are anchored to chunks, not segments. What remains open: corrected
> **word timestamps** (re-aligning corrected chunk text back onto segment/word
> times — a later task), and the web transcript reader, which still renders
> `transcripts.segments` and therefore uncorrected ASR. Mutating segments to
> "fix" either is forbidden: it would destroy the provenance record.

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
`ADD COLUMN IF NOT EXISTS` in the pre-goose inline schema, now part of the
`00001_baseline.sql` migration (§1.8). A
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

### 2.18 Model Registry (`MODELS_FILE`)

One registry maps each pipeline step to the model expected behind it. It
annotates the AI endpoint registry (§2.14) rather than duplicating it: *where*
to send a request and *which model id or LiteLLM alias to ask for* still come
from `AI_ENDPOINTS` + `AI_ROLES`. The registry adds what those cannot say, and
those values go into the step's recipe (§1.9). Upgrading a model is a one-line
change here.

```yaml
steps:
  propose:                       # the eval judge — AI_ROLES.eval (or EVAL_CHAT_MODEL)
    alias: earmark-judge         # optional assertion: must equal the model that endpoint requests
    expected_model: anthropic/claude-haiku-4-5-20251001   # what should ANSWER → recipe model_resolved
    revision: "20251001"         # weights pin → recipe model_revision
    prompt_version: judge@v1     # optional: logged as a warning if the build runs another
  embed:                         # AI_ROLES.embeddings
    expected_model: nomic-embed-text
    revision: sha256:0a109f422b47
  asr:                           # checked against what the runner reports (§1.9 ASR stamping); a mismatch is logged
    expected_model: nvidia/parakeet-tdt-1.1b
  decide:                        # AI_ROLES.decide (a systemone endpoint, §2.14)
    alias: jev-1.13.0
    params:
      usd_per_mtok_in: 0.042     # cost estimate when the gateway reports none
```

| Key | Meaning |
|---|---|
| `steps.<step>` | one of `asr`, `propose`, `decide`, `propagate`, `scan`, `format`, `embed` |
| `alias` | optional; must equal the configured endpoint's `model` for the step's role (`propose`→`eval`, `embed`→`embeddings`, `decide`→`decide`, `scan`→`scan`) |
| `expected_model` | the model the endpoint should report serving; the current recipe's `model_resolved`. Unset → the requested model |
| `revision` | HF commit / `.nemo` sha256 / Ollama digest / provider snapshot |
| `prompt_version` | the prompt version this deployment expects; the code's own version is authoritative, a mismatch is logged |
| `params` | optional step settings. Known key: `usd_per_mtok_in` (decide, scan) — a non-negative number, the System One input price used when the gateway reports no cost (§2.14). An unknown key or a bad value is a startup error |

Because `expected_model` becomes the current recipe's `model_resolved`, a
response served by any other model (a LiteLLM fallback, or an alias silently
re-pointed) stamps a different recipe and shows up in `stale_work`. Behind an
alias, set `expected_model` to the id the endpoint actually reports, or every
finding will read as a fallback.

**Helm.** Set `config.models` to the YAML above (as values). The chart renders
it into the `<fullname>-models` ConfigMap, mounts it read-only at
`/etc/earmark/models.yaml` on every pod that uses the common env/volumes, and
sets `MODELS_FILE` — on both Deployments and the `evalBackfill` CronJob;
`config.models` is in the config checksum, so editing it
rolls the pods. Unset (the default) renders nothing, byte-identical to the chart
without it. `values.schema.json`
rejects unknown steps and fields.

Validation is fail-closed like §2.14: an unreadable file, malformed YAML, an
unknown step or field, or an `alias` that contradicts the endpoint registry
stops startup. Unset → an empty registry: recipes are still stamped, with the
requested model as the expected one and no revision.

---

## 3. SCHEMA — pgvector chunks table

The Go service reads completed transcripts, chunks them, and embeds each chunk.
Chunks are stored alongside the transcripts in the same database.

> **Schema changes go through goose migrations (§1.8)**, which every earmark
> process runs on startup under a waiting, non-leaking advisory lock. The DDL
> below is the resulting shape, not something any process executes directly.

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
    recipe_id    TEXT        REFERENCES recipes (recipe_id),  -- embed recipe (§1.9); restamped on re-embed

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
- The recipe canonical form, `recipe_id` derivation, or step names in section 1.9
- The `MODELS_FILE` shape in section 2.18
- A shipped migration in `internal/db/migrations/` (never edited — add a new one, §1.8)

...requires updating this file **before** writing implementation code. All
three components (this Go service, the ASR runner, the deployment manifests)
must be updated atomically when a contract value changes.
