# lilbro-whisper Interface Contract

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
    model_name          TEXT        NOT NULL,   -- WhisperX model used, e.g. "large-v3"
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
`/tmp/lilbro-whisper-busy`). When this file exists, the runner skips claiming
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

#### Operator requeue (out-of-band, `lil-whisper requeue`)

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
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**All columns are nullable** (except the PK and `created_at`/`updated_at`). Each
writer UPSERTs via `INSERT … ON CONFLICT (job_id) DO UPDATE SET <its cols>=EXCLUDED…, updated_at=now()`.

#### Column ownership (which writer writes which columns)

| Writer | When | Columns it writes |
|--------|------|-------------------|
| **Go monitor** | at enqueue (file size from `os.Stat`) | `audio_bytes` |
| **Python ASR runner** | after transcribing | `audio_channels`, `audio_sample_rate`, `audio_codec`, `audio_format`, `transcribe_started_at`, `transcribe_finished_at`, `asr_model`, `compute_type`, `runner_host`, `chunked`, `n_windows`, `char_count`, `word_count`, `segment_count` |
| **Go embed worker** | after `transcript_chunks` insert | `embed_started_at`, `embed_finished_at`, `embed_model`, `embed_chunk_count`, `embed_prompt_tokens`, `embed_total_tokens` |

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
enqueue time via `MetadataProvider.Lookup`. Readers: added by PR 4 (ABS
chapters) and PR 5 (bias terms); no reader exists yet.

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
| **Go monitor** | at enqueue (PathProvider) | `title`, `author`, `source` |
| **PR 4 — ABS integration** | after ABS chapter fetch | `narrator`, `series`, `asin`, `chapters` |
| **PR 5 — bias terms** | derived from chapters + title | `bias_terms` |

**Key choice — `book_dir` as primary key:** the monitor groups files by
`book_dir = filepath.Dir(file_path)` (one row per book directory, not per
track). This is the same granularity the rest of the pipeline uses for
per-book queries, and it lets the runner in PR 5 derive the key from a job's
`file_path` with a single `filepath.Dir` call.

Rules:
- Every write is **best-effort** — a `book_metadata` failure MUST NOT fail
  enqueue. The monitor logs and continues.
- The UPSERT touches only the columns its writer owns, so concurrent writes
  from later PRs can't clobber each other.
- `chapters` and `bias_terms` are nullable and left `NULL` by the monitor; PR 4
  and PR 5 fill them in independently.
- The Go service creates the table in its schema-init transaction.

---

## 2. DEPLOYMENT INTERFACE CONTRACT

### 2.1 Identity

| Property | Value |
|----------|-------|
| Namespace | `lilbro-whisper` |
| Go binary image | `ghcr.io/jedwards1230/lilbro-whisper` |
| Helm chart OCI ref | `oci://ghcr.io/jedwards1230/charts/lilbro-whisper` |
| Ingress hostname | `audiobooks-kb.example.com` |
| CNPG cluster name | `lilbro-whisper-pg` |

`audiobooks.example.com` may be taken by an existing Audiobookshelf instance. Do NOT use it here.

### 2.2 MCP Transport

| Property | Value |
|----------|-------|
| Transport | `streamable-http` (wiki parity) |
| Container port | `8081` |
| URL path | `/mcp` |
| In-cluster URL | `http://lilbro-whisper.lilbro-whisper:8081/mcp` |
| mcp-proxy upstream key | `"audiobooks"` |

The mcp-proxy configmap entry (add to `mcpServers` object):

```json
"audiobooks": {
  "url": "http://lilbro-whisper.lilbro-whisper:8081/mcp",
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
| `DATABASE_URL` | yes | PostgreSQL DSN: `postgres://lilbro_whisper:<pass>@lilbro-whisper-pg-rw.lilbro-whisper:5432/lilbro_whisper` |
| `PGHOST` | no | Convenience alias; `DATABASE_URL` takes precedence |
| `PGPORT` | no | Convenience alias |
| `PGUSER` | no | Convenience alias |
| `PGPASSWORD` | no | Convenience alias |
| `PGDATABASE` | no | Convenience alias |
| `EMBEDDINGS_BASE_URL` | no | `http://ollama:11434/v1` |
| `EMBEDDINGS_MODEL` | no | `nomic-embed-text` |
| `BOOKS_DIR` | no | `/books` (read-only NFS mount inside container) |
| `MCP_HTTP_ADDR` | no | `:8081` |
| `STALE_JOB_TIMEOUT` | no | `30m` (Go duration string) |
| `CHUNK_SIZE` | no | `512` (target tokens per chunk; overlap is 64 tokens) |
| `LIBRARY_COLLECTIONS` | no | JSON array describing each library root's shape, for the dashboard's author/title labels (see below). Empty → generic fallback. |
| `CONTROL_API_TOKEN` | no | Bearer token required on the mutating control-API endpoints (§2.7). Empty → those endpoints fail closed (`503`); read endpoints are always open. |
| `METADATA_PROVIDER` | no | `path` (default). Accepts `path`, `abs`, or `chain:<p1>,<p2>` (e.g. `chain:abs,path`). `path` derives title/author from the filesystem path only; `abs` queries Audiobookshelf; `chain` tries providers left-to-right and returns the first non-empty result. |
| `ABS_URL` | no | Base URL of the Audiobookshelf server (e.g. `https://audiobooks.lilbro.cloud`). Required when `METADATA_PROVIDER=abs` or `abs` appears in a chain spec; ignored otherwise. |
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

#### Python ASR runner — NeMo Parakeet-TDT (GPU/ASR host native service)

| Variable | Required | Default / Notes |
|----------|----------|-----------------|
| `DATABASE_URL` | yes | Same DSN as Go service — runner connects directly to CNPG rw endpoint |
| `RUNNER_IDENTITY` | no | `asr-runner` (included in `claimed_by`) |
| `RUNNER_POLL_INTERVAL_SECONDS` | no | `30` |
| `RUNNER_HEARTBEAT_SECONDS` | no | `60` |
| `RUNNER_BUSY_FLAG_PATH` | no | `/tmp/lilbro-whisper-busy` |
| `ASR_MODEL` | no | `nvidia/parakeet-tdt-0.6b-v3` (NeMo model id) |
| `ASR_DIARIZE` | no | `false` (default). Set `true` to run NeMo Sortformer speaker diarization for multi-voice/full-cast titles |
| `ASR_COMPUTE_TYPE` | no | `bfloat16` (native on RTX 5090 / Blackwell) |
| `ASR_CHUNK_THRESHOLD_SECONDS` | no | `3600` — single-pass below this duration; chunked/buffered inference above |
| `BOOKS_MOUNT` | no | `/srv/audiobooks` (NFS export path on the storage host) |

### 2.5 CNPG Cluster

| Property | Value |
|----------|-------|
| Cluster name | `lilbro-whisper-pg` |
| Namespace | `lilbro-whisper` |
| PostgreSQL image | `ghcr.io/cloudnative-pg/postgresql:16-pgvector` |
| Storage size | `20Gi` |
| StorageClass | `nfs-databases` |
| Read-write endpoint | `lilbro-whisper-pg-rw.lilbro-whisper` (port 5432) |
| Database name | `lilbro_whisper` |
| Database owner | `lilbro_whisper` |
| PostInitSQL extensions | `CREATE EXTENSION IF NOT EXISTS vector;` `CREATE EXTENSION IF NOT EXISTS pg_trgm;` |
| Backup destination | `s3://postgres-backups/` via an S3-compatible object store (e.g. Garage at `http://s3.example.com:3900`) |
| Backup plugin | `barman-cloud.cloudnative-pg.io` |
| Backup retention | `30d` |
| Backup schedule | `0 0 3 * * *` (daily 3 AM, six-field cron) |
| ObjectStore name | `garage-backup-store` |

#### 1Password item paths (follow `k8s-<ns>-<service>-<type>` convention)

| Secret | 1Password item path |
|--------|---------------------|
| DB credentials (CNPG) | `vaults/example/items/k8s-lilbro-whisper-pg-credentials` |
| S3 credentials for CNPG | `vaults/example/items/k8s-lilbro-whisper-cnpg-garage-secret` |
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
The lilbro-whisper Deployment in the `lilbro-whisper` namespace MUST define
its own static PV + PVC pointing to the same NFS export with `ReadOnlyMany`
to enforce the read-only constraint without depending on the `media` namespace.

```yaml
# PersistentVolume — declare in lilbro-whisper namespace manifests
apiVersion: v1
kind: PersistentVolume
metadata:
  name: lilbro-whisper-books-ro
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
    namespace: lilbro-whisper
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: books-ro
  namespace: lilbro-whisper
spec:
  storageClassName: nfs-static-media
  volumeName: lilbro-whisper-books-ro
  accessModes:
    - ReadOnlyMany
  resources:
    requests:
      storage: 100Gi
```

### 2.7 Helm Chart Structure (cardigan model)

Chart lives at `deploy/helm/lilbro-whisper/` in the `lilbro-whisper` repo.
Published as `oci://ghcr.io/jedwards1230/charts/lilbro-whisper`.

The helmfile release for homelab-k8s lives at:
`repos/homelab-k8s/apps/lilbro-whisper/helmfile.yaml`

ArgoCD Application CRD lives at:
`repos/homelab-k8s/base/core/argocd/applications/lilbro-whisper/lilbro-whisper-helmfile.yaml`

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
  app.kubernetes.io/name: lilbro-whisper
  app.kubernetes.io/instance: lilbro-whisper-prod
  app.kubernetes.io/component: mcp-server        # for the Go Deployment
  app.kubernetes.io/part-of: lilbro-whisper-stack
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
| `GET` | `/api/v1/status` | none | — | `200` queue/runner snapshot (JSON) |
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

...requires updating this file **before** writing implementation code. All
three repos (lilbro-whisper Go, homelab-ansible runner, homelab-k8s manifests)
must be updated atomically when a contract value changes.
