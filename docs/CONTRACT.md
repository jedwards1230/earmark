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
- **`phase`** — the **batched two-phase pipeline** selector. `NULL` or `'idle'`
  is normal operation (both the ASR runner and the Go embed worker run freely —
  the default, fully backward-compatible); `'transcribe'` is the ASR-only phase
  (the embed worker idles so eval/embed don't contend for the GPU the ASR runner
  owns); `'analyze'` is the embed-only phase (the ASR runner is paused/off-GPU
  and the embed worker drains the just-transcribed transcripts). The two valid
  non-default values let a single GPU host a large ASR model **or** a large eval
  judge, never both at once. The column is gated by a CHECK to the closed set
  `{idle, transcribe, analyze}`; `NULL`/`idle` are equivalent and `idle` is
  stored as `NULL`. `paused`/`run_limit` and `phase` are **independent axes**: a
  paused pipeline in `transcribe` phase claims nothing AND idles the worker.

**Phase semantics (worker gate):** the Go embed worker reads `phase` at the top
of each poll cycle. When `phase = 'transcribe'` it SKIPS that cycle (idles for
the poll interval, processes nothing) so the ASR runner has the GPU to itself.
For `'idle'`/`'analyze'`/`NULL` it processes completed transcripts as usual. A
phase-read error defaults to `'idle'` (process) and is logged — a DB hiccup must
never wedge the worker. A missing row is treated as `'idle'`. With `phase` left
`NULL` the pipeline behaves exactly as before (both stages run concurrently);
the **`earmark batch` coordinator** (below) is what flips `phase` to orchestrate
transcribe- and analyze-batches.

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
2. **Phase A — transcribe.** Set `phase='transcribe'` and `run_limit=N`
   (`--batch-size`, default 10). The runner claims up to N jobs then stops; the
   embed worker idles. Wait until nothing is `claimed` AND the run budget is
   exhausted (`run_limit==0`) or no `pending` jobs remain.
3. **Phase B — analyze.** Set `phase='analyze'`. The runner parks its model
   (freeing the GPU) and the embed worker drains the just-transcribed
   transcripts (chunk → embed, with inline eval when `EVAL_IN_PIPELINE=true`).
   Wait until the embed backlog (completed transcripts with no chunks) is 0.

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
missing row never blocks the pipeline. Three independent writers each UPSERT
**only their own slice** of columns keyed on `job_id`, so they never clobber
each other:

```sql
CREATE TABLE IF NOT EXISTS run_metrics (
  job_id UUID PRIMARY KEY REFERENCES transcription_jobs(id) ON DELETE CASCADE,
  audio_bytes BIGINT, audio_channels INT, audio_sample_rate INT, audio_codec TEXT, audio_format TEXT,
  transcribe_started_at TIMESTAMPTZ, transcribe_finished_at TIMESTAMPTZ, asr_model TEXT, compute_type TEXT,
  runner_host TEXT, chunked BOOLEAN, n_windows INT, char_count INT, word_count INT, segment_count INT,
  embed_started_at TIMESTAMPTZ, embed_finished_at TIMESTAMPTZ, embed_model TEXT, embed_chunk_count INT,
  embed_prompt_tokens INT, embed_total_tokens INT,
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
enqueue time via `MetadataProvider.Lookup`. Reader: the Python ASR runner reads
`bias_terms` to drive NeMo word-boosting (PR 5 homelab-ansible runner PR).

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
- The Go service creates the table in its schema-init transaction.

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

All tools are read-only (no side-effects). The two search tools default to the
**whole library** and take an optional `book` to scope to a single title. There
are **5 tools** (the legacy `browse_audiobook_library` was removed — `list_books`
strictly dominates it; its tree view is folded in via `list_books format=tree`).

**Chunks vs segments** — two granularities of the same text, surfaced by
different tools: a **chunk** is the embedding/search unit (~hundreds per book; a
chunk is *tens of consecutive ASR segments* grouped together), while a
**segment** is a single ASR timestamp unit (thousands per book). The search
tools + `get_chunk_context` operate on **chunks**; `get_transcript` paginates
raw **segments**.

| Tool | Purpose | Key params |
|------|---------|-----------|
| `list_books` | Library **inventory**: per book → author, title, track progress (done/total), total duration, word count, embedded-chunk count. Em dash / 0 for books with no `run_metrics` yet. Ordered **transcribed-first** (fully-done books, then partial, then fully-pending). Leads with a one-line whole-library summary (`Library: T books — P fully transcribed, Q with pending tracks.` — TRUE totals across the library, not just the page). `format=flat` (default) **omits each book's `dir:` line** to keep the payload small; `format=tree` groups rows under their authors **and** keeps the `dir:` line. | `author?` (substring filter), `format?` (`flat` default \| `tree`), `limit?` (default 50), `offset?` |
| `semantic_search_audiobooks` | Vector-similarity (meaning) search; hits show a real cosine `similarity: NN%`. Whole library by default; `book` scopes it. `snippet?` caps each hit's quoted text (leading **preview** — no sub-chunk match position). | `query` (required), `book?`, `threshold?` (0.3), `limit?` (10), `snippet?` (max chars; floored to 80) |
| `text_search_audiobooks` | Trigram literal/keyword search; hits are labelled **"ranked by trigram match"** (NOT a similarity %, which would mislead on a literal hit). Whole library by default; `book` scopes it. `snippet?` returns an excerpt **centred on the literal match**. | `query` (required), `book?`, `limit?` (10), `snippet?` (max chars; floored to 80) |
| `get_transcript` | Read a track's full transcript as timestamped **segments** (paginated — `raw_text` can be 600k+ chars). Multi-track book → returns a track chooser to pick a `trackID`. | `book?` or `trackID?` (one required), `offset?` (0), `limit?` (50 segments) |
| `get_chunk_context` | Surrounding **chunks** around a chunk. `chunkID` is the **UUID** in a search hit's `ID` field. | `chunkID` (required, the search-hit UUID), `contextWindow?` (**default 1** → ~3 chunks; clamped to 0–50 to bound the response size) |

**Structured output**: every tool advertises an `outputSchema` and returns
`structuredContent` (machine-readable) **in addition to** the existing
human-readable text, which is kept as the spec-required back-compat fallback
(`content[0]`). The structured payloads are: the two search tools +
`get_chunk_context` → `{ kind, query?, count, results[] }` (`kind` is
`semantic` \| `trigram` \| `context`; `results` are the raw chunk rows);
`list_books` → `{ format, books[], totals, total, offset, nextOffset? }`;
`get_transcript` → `{ kind: "transcript", filePath, language, modelName,
durationSeconds, segments[], offset, limit, totalSegments, nextOffset? }` for a
page, or `{ kind: "trackChooser", book, tracks[] }` when a book has multiple
tracks. Bad user input (missing/unmatched `book`, bad `chunkID`, etc.) returns a
tool-execution error (`isError`), never a protocol error.

**Snippet windows** (`snippet` on both search tools): omitted → the full ~400-word
chunk (backward-compatible). When set, the hit's quoted text is truncated to
~`snippet` chars with a `…(truncated, use get_chunk_context for full text)`
marker. Text search centres the window on the literal query match; semantic
search returns a **leading preview** (there is no sub-chunk match position) and
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

**Result formatting (search + context):** chapter mapping is not yet populated
(a future ABS-integration PR fills it in), so the formatter **suppresses the
chapter label entirely** when there is no real chapter data (chapter index 0 AND
empty title) — no misleading `Chapter 0:` prefix is emitted. A populated chapter
(non-zero index or a non-empty title) still renders as `Chapter N: <title>`.

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
| `STALE_JOB_TIMEOUT` | no | `30m` (Go duration string) |
| `CHUNK_SIZE` | no | `512` (target tokens per chunk; overlap is 64 tokens) |
| `LIBRARY_COLLECTIONS` | no | JSON array describing each library root's shape, for the dashboard's author/title labels (see below). Empty → generic fallback. |
| `CONTROL_API_TOKEN` | no | Bearer token required on the mutating control-API endpoints (§2.7). Empty → those endpoints fail closed (`503`); read endpoints are always open. |
| `EVAL_MAX_FINDINGS_PER_CHUNK` | no | `5`. Cap on findings kept per chunk by the eval judge (highest-confidence retained; the judge over-flags). `<= 0` disables the cap. See §2.15. |
| `EVAL_MIN_CONFIDENCE` | no | `0.6`. Confidence floor — findings below it are dropped before the cap. `<= 0` disables the floor. See §2.15. |
| `EVAL_IN_PIPELINE` | no | `false`. When true, the embed worker runs the eval judge on each transcript's chunks **before embedding** (the repositioned, in-pipeline eval). Default off → eval stays on-demand and the worker is unchanged. Requires an eval chat endpoint (`AI_ROLES.eval` / `EVAL_CHAT_*`); if none resolves, inline eval is logged-skipped, not fatal. |
| `GPU_ARBITER_URL` | no | gpu-arbiter `/status` URL (e.g. `http://gpu-host:48750/status`) read by the `earmark batch` coordinator (§1.4) to yield the GPU to games. **Read-only** — the coordinator only `GET`s it, never `POST`s. Unset or unreachable → the coordinator logs it and proceeds (degrades gracefully). The `batch --gpu-arbiter-url` flag overrides it. |
| `ASR_SERVERS` | no | JSON array declaring the transcription servers (ASR runners) for this deployment, so the Servers dashboard page can show a configured-but-idle server (e.g. a fallback). Empty → the page lists only observed runners. Cosmetic/read-only: a malformed value logs a warning and is ignored, and the list does **not** influence job routing (the runner claims work itself). See below. |
| `METADATA_PROVIDER` | no | `path` (default). Accepts `path`, `abs`, or `chain:<p1>,<p2>` (e.g. `chain:abs,path`). `path` derives title/author from the filesystem path only; `abs` queries Audiobookshelf; `chain` tries providers left-to-right and returns the first non-empty result. |
| `ABS_URL` | no | Base URL of the Audiobookshelf server (e.g. `https://audiobooks.example.com`). Required when `METADATA_PROVIDER=abs` or `abs` appears in a chain spec; ignored otherwise. |
| `ABS_TOKEN` | no | Audiobookshelf API token. Required when `ABS_URL` is set. |
| `ABS_LIBRARY_ID` | no | Audiobookshelf library ID to search for book metadata. Required when `ABS_URL` is set. Defaults to the first configured library if omitted (implementation may change). |

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

The helmfile release for homelab-k8s lives at:
`repos/homelab-k8s/apps/earmark/helmfile.yaml`

ArgoCD Application CRD lives at:
`repos/homelab-k8s/base/core/argocd/applications/earmark/earmark-helmfile.yaml`

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
| `GET` | `/api/v1/status` | none | — | `200` queue/runner snapshot (JSON), incl. a `servers[]` array (name, host, role, configured, state, model, modelSize, computeMode, jobsDone; plus gpuProbed/gpuReachable/gpuState/vramUsedMb/vramTotalMb when a `gpuArbiterUrl` is configured) and an `endpoints[]` array (id, type, backend, baseURL, model, options, role, state, probed — the AI endpoint registry with health probes, §2.14) |
| `GET` | `/api/v1/pipeline/pause` | none | — | `200 {"paused":bool,"runLimit":int\|null}` |
| `PUT` | `/api/v1/pipeline/pause` | bearer | `{"paused":bool}` | `200` current state (`paused:false` resumes + clears bound) |
| `POST` | `/api/v1/pipeline/run` | bearer | `{"limit":N}` (N≥1) | `202 {"paused":false,"runLimit":N}` — run N then auto-pause |
| `DELETE` | `/api/v1/pipeline/run` | bearer | — | `200` clears the bounded run (`run_limit→NULL`) |

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
The findings/eval actions additionally **fail closed** (banner, no-op) when
`CONTROL_API_TOKEN` is unset, matching the JSON API's posture:

| Method | Path | Auth | Effect |
|--------|------|------|--------|
| `POST` | `/actions/requeue?id=…` | htmx | re-transcribe one job; re-render status fragment |
| `POST` | `/actions/retry-failed` | htmx | re-transcribe all failed jobs |
| `POST` | `/actions/book-requeue?dir=…` | htmx | re-transcribe one book |
| `POST` | `/actions/pause` / `/actions/resume` | htmx | toggle the runner pause flag |
| `POST` | `/actions/eval?dir=…` | htmx + token | run the LLM judge over one book (async, §2.15) |
| `POST` | `/actions/eval-sample?n=N` | htmx + token | run the LLM judge over an N-chunk sample (async, §2.15) |
| `POST` | `/actions/findings-clear[?dir=…]` | htmx + token | **delete** recorded findings (advisory metadata only; §2.15), then re-render the `/findings` fragment. Optional `dir` scopes the delete to one book; absent clears all. Touches only `transcript_findings` — transcripts are never modified, so a clear is always recoverable by re-running eval. |

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

The eval layer is a **read-only LLM-as-judge** (`internal/eval`, `earmark eval`)
that READS transcript chunks and records **suspected** transcription errors as
**advisory metadata** — it NEVER edits transcripts. The asymmetry is the whole
point: a wrong flag is harmless (triage by confidence), a wrong *correction*
would corrupt the corpus, so `suggested_correction` is recorded but **never
applied**.

**Read-only / advisory contract (binding):** the eval layer issues no
`UPDATE`/`DELETE`/`ALTER`/`DROP`/`TRUNCATE` against `transcripts`, `segments`, or
`transcript_chunks`. Its only write is `INSERT INTO transcript_findings`. The
findings table carries no foreign key that cascade-mutates the transcript tables.

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

## 3. SCHEMA — pgvector chunks table

The Go service reads completed transcripts, chunks them, and embeds each chunk.
Chunks are stored alongside the transcripts in the same database.

```sql
CREATE TABLE transcript_chunks (
    id           UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    transcript_id UUID       NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
    file_path    TEXT        NOT NULL,   -- denormalized for query convenience
    chunk_index  INTEGER     NOT NULL,   -- ordinal position within transcript
    start_sec    FLOAT8      NOT NULL,   -- earliest segment start in this chunk
    end_sec      FLOAT8      NOT NULL,   -- latest segment end in this chunk
    text         TEXT        NOT NULL,
    speaker      TEXT,                   -- dominant speaker in chunk, or NULL
    embedding    VECTOR(768) NOT NULL,   -- nomic-embed-text dimension, MUST match EMBEDDINGS_MODEL
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transcript_chunks_transcript_chunk_unique UNIQUE (transcript_id, chunk_index)
);

CREATE INDEX transcript_chunks_embedding_idx
    ON transcript_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX transcript_chunks_file_path_idx ON transcript_chunks (file_path);
```

Chunk size target: **512 tokens** (Go tokenizer), overlap: **64 tokens**.
These are implementation constants in the Go chunker, not a DB concern.

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
three repos (earmark Go, homelab-ansible runner, homelab-k8s manifests)
must be updated atomically when a contract value changes.
