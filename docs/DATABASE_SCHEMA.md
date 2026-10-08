# Database Schema Reference

> **See CONTRACT.md §§1.1, 1.2, 1.5, 1.6, 1.8, 1.9 and 3 for authoritative definitions.**
> This document summarises the current schema; CONTRACT.md wins on any discrepancy.

## Migrations

The schema is owned by [goose](https://github.com/pressly/goose) migrations
embedded from `internal/db/migrations/` and applied by every earmark process on
startup (`db.New`), serialized by an advisory lock (CONTRACT §1.8). The applied
version is in `goose_db_version`.

| Version | File | Adds |
|---|---|---|
| 1 | `00001_baseline.sql` | everything below as of v0.40.2 (the former inline `initialize()` DDL). On a database the old code built it is only *recorded*, never executed. |
| 2 | `00002_recipes.sql` | `recipes`; `recipe_id` on `transcripts`, `transcript_findings`, `transcript_chunks`; legacy backfill |
| 3 | `00003_stale_work.sql` | `current_recipes`; the `stale_work` view |
| 4 | `00004_unanchorable.sql` | `unanchorable` patch state; `unanchorable_reason`, `reanchored_at` on `transcript_findings` |
| 5 | `00005_requeue_archive.sql` | `superseded` patch state + `superseded_at`; `transcript_findings.transcript_id` nullable, FK to `transcripts(id) ON DELETE SET NULL`, CHECK `transcript_id IS NOT NULL OR patch_state = 'superseded'`; existing orphans archived as `superseded` first; `stale_work` skips superseded findings |
| 6 | `00006_asr_provenance_identity.sql` | runner-reported provenance on `transcripts` (`embedded_asin`, `asr_model_sha256`, `asr_runner_version`, `asr_params`) + partial index `transcripts_asr_unstamped_idx`; `book_metadata.asin_source` / `identity_status` |
| 7 | `00007_fn_calls.sql` | `fn_calls` (pure-function call log + cache); partial unique `fn_calls_cache_key_idx`, `fn_calls_recipe_id_idx` |
| 8 | `00008_finding_events.sql` | `finding_events` (append-only finding version history); transition triggers on `transcript_findings`; append-only guard; backfill of decided findings; `decide` arm of `stale_work` |
| 9 | `00009_chunk_scan.sql` | `chunk_scan` (per-chunk System One quality scan); unique `(transcript_id, chunk_index, chunk_text_sha256, recipe_id)`, `chunk_scan_recipe_id_idx`; `scan` arm of `stale_work` |

New schema = a new numbered file. Never edit a shipped migration. Run the
Postgres proofs locally with:

```bash
docker run -d --name earmark-it -e POSTGRES_PASSWORD=pw -p 55432:5432 pgvector/pgvector:pg16
EARMARK_TEST_DATABASE_URL='postgres://postgres:pw@localhost:55432/postgres?sslmode=disable' \
  go test -race -run Integration ./internal/db/
```

## Extensions Required

```sql
CREATE EXTENSION IF NOT EXISTS vector;   -- pgvector
CREATE EXTENSION IF NOT EXISTS pg_trgm;  -- trigram full-text search
```

## Tables

### 1. `transcription_jobs` — Job Queue (CONTRACT §1.1)

Producer: Go monitor. Consumer: Python ASR runner on the GPU/ASR host.

```sql
CREATE TABLE transcription_jobs (
    id           UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    file_path    TEXT        NOT NULL,   -- relative to BOOKS_DIR, e.g. "Author/Book/01.m4b"
    checksum     TEXT        NOT NULL,   -- SHA-256 hex (dedup key)
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'claimed', 'done', 'failed')),
    claimed_by   TEXT,                   -- runner identity string
    claimed_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    error        TEXT,                   -- last error when status='failed'
    attempts     INTEGER     NOT NULL DEFAULT 0,

    CONSTRAINT transcription_jobs_checksum_unique   UNIQUE (checksum),
    CONSTRAINT transcription_jobs_file_path_unique  UNIQUE (file_path)   -- one job per file
);

CREATE INDEX transcription_jobs_status_idx    ON transcription_jobs (status, created_at);
CREATE INDEX transcription_jobs_file_path_idx ON transcription_jobs (file_path);

-- Auto-update updated_at on any row change
CREATE OR REPLACE FUNCTION transcription_jobs_set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$;
CREATE TRIGGER transcription_jobs_updated_at
    BEFORE UPDATE ON transcription_jobs
    FOR EACH ROW EXECUTE FUNCTION transcription_jobs_set_updated_at();
```

Status lifecycle: `pending` → `claimed` → `done` | `failed`.
Stale `claimed` rows (silent > `STALE_JOB_TIMEOUT`) are reset to `pending` by the Go worker.

### 2. `transcripts` — Completed Transcripts (CONTRACT §1.2)

Written by the Python runner (atomically with the job status update).

```sql
CREATE TABLE transcripts (
    id               UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    job_id           UUID        NOT NULL REFERENCES transcription_jobs(id) ON DELETE CASCADE,
    file_path        TEXT        NOT NULL,
    checksum         TEXT        NOT NULL,
    language         TEXT        NOT NULL,   -- ISO 639-1, e.g. "en"
    duration_seconds FLOAT8      NOT NULL,   -- length of THIS track (see time bases below)
    speaker_count    INTEGER,               -- NULL when diarization disabled
    segments         JSONB       NOT NULL,  -- []Segment (see CONTRACT §1.2.1)
    raw_text         TEXT        NOT NULL,  -- full transcript, concatenated
    model_name       TEXT        NOT NULL,  -- ASR model id, e.g. "nvidia/parakeet-tdt-0.6b-v3"
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    recipe_id        TEXT REFERENCES recipes (recipe_id), -- asr recipe (§8); NULL = unstamped
    -- runner-reported (00006); NULL from a runner that predates it
    embedded_asin      TEXT,   -- ASIN tag read from the audio by ffprobe (validated)
    asr_model_sha256   TEXT,   -- sha256 of the .nemo file loaded
    asr_runner_version TEXT,   -- runner version tag
    asr_params         JSONB,  -- runner output-shaping settings

    CONSTRAINT transcripts_job_id_unique UNIQUE (job_id)
);

CREATE INDEX transcripts_file_path_idx     ON transcripts (file_path);
CREATE INDEX transcripts_raw_text_trgm_idx ON transcripts USING gin (raw_text gin_trgm_ops);
CREATE INDEX transcripts_asr_unstamped_idx ON transcripts (created_at)
    WHERE recipe_id IS NULL AND asr_runner_version IS NOT NULL;
```

The Go worker builds the asr recipe from the four runner-reported columns and
stamps `recipe_id` (CONTRACT §1.9); `embedded_asin` is the third ASIN source
for `book_metadata` (CONTRACT §1.6).

#### ⚠️ Time bases: one transcript per TRACK, not per book

There is exactly **one row per audio file** (`transcripts_job_id_unique`, and one
job per file). A multi-track book therefore has several `transcripts` rows, and
every time value in them — `segments[].start/end`, `duration_seconds`, and the
derived `transcript_chunks.start_sec/end_sec` — is **relative to the start of
that track**, restarting at ~0 for each new file.

Provider chapter lists (`book_metadata.chapters`, CONTRACT §1.6) use the opposite
base: they are **book-absolute** across all tracks concatenated in play order.
Anything mapping a chunk to a chapter must first add the track's book offset —
the summed `duration_seconds` of the preceding tracks of the same `book_dir`,
ordered by `file_path` (see `metaprovider.ChapterForTrackSec`). Mixing the two
bases silently maps every track to the book's opening chapters. A NULL/zero
preceding `duration_seconds` makes the offset unknowable, and callers must then
report no chapter rather than a guess.

#### Segment JSON Shape (`segments` column)

```jsonc
[
  {
    "id":      0,
    "start":   12.34,           // float64 seconds
    "end":     15.78,
    "text":    "Hello, world.", // may have leading space — preserve as-is
    "speaker": "SPEAKER_00",   // null when diarization disabled
    "words": [
      {
        "word":    "Hello,",
        "start":   12.34,
        "end":     12.71,
        "score":   0.983,       // null when alignment unavailable
        "speaker": "SPEAKER_00"
      }
    ]
  }
]
```

`recipe_id` (migration 2, nullable FK → `recipes`) is the `asr` recipe that
produced the transcript. Pre-existing rows carry a legacy recipe per
`model_name`; the runner does not stamp new rows yet (NULL) — CONTRACT §1.9.

### 3. `transcript_chunks` — pgvector Embeddings (CONTRACT §3)

Written by the Go worker after embedding each transcript.

```sql
CREATE TABLE transcript_chunks (
    id            UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    transcript_id UUID        NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
    file_path     TEXT        NOT NULL,
    chunk_index   INTEGER     NOT NULL,
    start_sec     FLOAT8      NOT NULL,   -- earliest segment start in this chunk; TRACK-relative
    end_sec       FLOAT8      NOT NULL,   -- latest segment end in this chunk; TRACK-relative
    text          TEXT        NOT NULL,   -- CORRECTED surface (source_text + replayed overlay)
    source_text   TEXT,                   -- PRISTINE regenerated text; NULL on legacy rows
    speaker       TEXT,                   -- dominant speaker, or NULL
    embedding     VECTOR(768) NOT NULL,   -- nomic-embed-text; MUST match EMBEDDINGS_MODEL
    embedding_stale BOOLEAN   NOT NULL DEFAULT false,  -- needs re-embed (correction accepted/reverted)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    recipe_id     TEXT        REFERENCES recipes (recipe_id),  -- embed recipe; restamped on re-embed (migration 2)

    CONSTRAINT transcript_chunks_transcript_chunk_unique UNIQUE (transcript_id, chunk_index)
);

CREATE INDEX transcript_chunks_embedding_idx
    ON transcript_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX transcript_chunks_file_path_idx ON transcript_chunks (file_path);
CREATE INDEX transcript_chunks_text_trgm_idx
    ON transcript_chunks USING gin (text gin_trgm_ops);
```

**This table is a derived projection, not storage.** The worker regenerates
every row from `transcripts.segments`/`raw_text` on each embed and upserts over
the existing rows, so anything written directly into `text` is destroyed by the
next re-embed. Human corrections therefore live in `transcript_findings` and are
**replayed** onto the regenerated text: `source_text` is the pristine
regeneration (the judge is always shown this), `text` is that text with the
accepted-correction overlay applied (what is embedded and searched), and
`embedding_stale` marks a chunk whose overlay changed — the embed worker's
rebuild pass selects on it and clears it (guarded by the overlay-read watermark)
once the rebuilt chunk is inserted. The upsert refreshes every derived column
(text, `source_text`, embedding, `file_path`, `start_sec`, `end_sec`, `speaker`)
but keeps the row `id`, and the same transaction deletes the rows at
`chunk_index >=` the new chunk count, retiring (never deleting) the findings
addressed to them — findings resolve their chunk by `(transcript_id,
chunk_index)`, by `chunk_id` only when they have no index. See CONTRACT §2.17.

**Timestamps are TRACK-relative**: `start_sec`/`end_sec` are copied from the
parent transcript's segment boundaries, so they are offsets into the chunk's own
audio file — chunk 0 of track 7 starts at ~0, not at track 7's position in the
book. They are **not** comparable with `book_metadata.chapters` times, which are
book-absolute; see the time-bases note under `transcripts` above.

**Vector dimension**: 768 (nomic-embed-text). Any model change requires a full
re-embed and a column type migration.

**Task-instruction prefixes**: `nomic-embed-text` requires task prefixes (see
CONTRACT §2.3). Stored passages in this column are embedded with the
`search_document: ` prefix; search queries use `search_query: `. The prefixes do
not change the column shape (still `VECTOR(768)`), but they DO change the vector
values — changing prefix behavior requires a full re-embed
(`earmark requeue --reembed "" --yes`) for stored and query vectors to stay
compatible.

**Chunk size**: 512 tokens, 64-token overlap (Go tokenizer). Controlled by
`CHUNK_SIZE` env var.

### 4. `run_metrics` — Per-run Observability (CONTRACT §1.5)

One nullable-columns row per job, written by three independent UPSERTers (Go
monitor → `audio_bytes`; Python runner → audio probe + transcription
timing/counts; Go embed worker → embedding timing/model/token counts). All
writes are best-effort and never block the pipeline. See CONTRACT §1.5 for the
full column list and per-writer ownership.

```sql
CREATE TABLE run_metrics (
    job_id UUID PRIMARY KEY REFERENCES transcription_jobs(id) ON DELETE CASCADE,
    -- audio probe (monitor: audio_bytes; runner: the rest)
    audio_bytes BIGINT, audio_channels INT, audio_sample_rate INT, audio_codec TEXT, audio_format TEXT,
    -- transcription (runner)
    transcribe_started_at TIMESTAMPTZ, transcribe_finished_at TIMESTAMPTZ, asr_model TEXT, compute_type TEXT,
    runner_host TEXT, chunked BOOLEAN, n_windows INT, char_count INT, word_count INT, segment_count INT,
    -- ASR backend descriptor (runner, CONTRACT §2.13 / §1.5)
    asr_family TEXT, asr_runtime TEXT,
    caps_applied JSONB, caps_requested JSONB, caps_skipped_reason JSONB,
    mean_word_confidence FLOAT8,
    -- embedding (Go embed worker)
    embed_started_at TIMESTAMPTZ, embed_finished_at TIMESTAMPTZ, embed_model TEXT, embed_chunk_count INT,
    embed_prompt_tokens INT, embed_total_tokens INT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`embed_total_tokens` is the authoritative local tokenizer count;
`embed_prompt_tokens` is the provider-reported value (NULL when Ollama omits it).

### 5. `runner_control` — Pipeline Gate (CONTRACT §1.3)

Singleton row that gates the Python ASR runner's claim loop. Written by the Go
service (dashboard + control API); read by the runner at the top of each poll
cycle. Missing row = not paused / unlimited (safe degraded default).

```sql
CREATE TABLE IF NOT EXISTS runner_control (
    id         INTEGER     NOT NULL PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    paused     BOOLEAN     NOT NULL DEFAULT false,
    run_limit  INTEGER         CHECK (run_limit IS NULL OR run_limit >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT
);
-- Seeded once by the Go service on init:
INSERT INTO runner_control (id, paused) VALUES (1, false) ON CONFLICT (id) DO NOTHING;
```

- `paused=true` — runner declines all new claims.
- `run_limit=N` — bounded run: claim at most N more jobs, then stop. `NULL` = unlimited.

### 6. `book_metadata` — Per-book Enrichment (CONTRACT §1.6)

One row per book directory. Written best-effort by the Go monitor at enqueue.
Read by the Python runner to drive NeMo word-boosting (`bias_terms`), and by the
Go **MCP layer** for `chapters` + `series` (see below). A missing row never
blocks the pipeline.

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
    description TEXT,   -- ABS publisher blurb (verbatim, may contain HTML)
    genres      TEXT[], -- ABS genre list
    isbn        TEXT,   -- ABS isbn
    asin_source     TEXT,  -- dir | filename | embedded_tag (00006)
    identity_status TEXT,  -- exact | conflict; NULL = local-only / not resolved (00006)
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`asin_source` / `identity_status` record the ASIN step (CONTRACT §1.6): `exact`
when the ABS record was matched by ASIN, `conflict` when the embedded tag named
a record that is not this book (title two-way coverage, author check; the
record is then not used). They are written as a pair whenever a lookup
resolves identity, and kept when it resolves nothing; a conflict also clears
the catalogue columns (`asin`, `description`, `chapters`, `genres`, `isbn`,
`narrator`, `series`).

`bias_terms` is re-derived from metadata on every write (never COALESCE-guarded).
`description`, `genres` and `isbn` (added to existing tables by the pre-goose inline schema, now part of the baseline) are
ABS-only enrichment: a non-empty value from a re-lookup overwrites the stored
one, an empty/NULL value keeps it.

`chapters` is a JSONB array of `{"Index","Title","StartSec","EndSec"}` objects (plus an optional `"RawTitle"`)
whose times are **book-absolute** — measured from the start of the whole book,
all tracks concatenated in play order (that is what Audiobookshelf's
`media.chapters` reports). Mapping a `transcript_chunks` row into this list
requires adding the chunk's track offset first; see the time-bases note under
`transcripts` (§2).

Chapter titles are cleaned of filename debris (`metaprovider.CleanChapterTitles`,
CONTRACT §1.6): ABS file-derived titles such as `"12 - Project Hail Mary: Chapter
11"` or `"<Book> [<ASIN>] - 01 - Chapter 1 (1)"` become `"Chapter 11"` /
`"Chapter 1"`. Cleaning happens at ingest and on read, so rows stored before
it existed still read clean; a row whose entries already carry `"RawTitle"` was
cleaned at ingest and is read back as stored (a list is cleaned at most once). When a title was changed at ingest the entry
carries an optional `"RawTitle"` key holding the provider original; it is absent
on entries whose title was already clean and on rows written before cleaning
(their stored `Title` *is* the original). A re-lookup
(`earmark backfill-metadata --yes`) rewrites a stored row with clean titles.

`series` is a comma-joined list of `Name #Sequence` entries — a book can belong
to several series, e.g. `"Dune #2, The Dune Sequence #13"`. The `#Sequence` part
is optional, and the sequence is **not** necessarily an integer (novellas are
commonly `#1.5`; non-numeric positions are legal). The **Go MCP layer reads this
column** — `metaprovider.ParseSeries` turns it into `[{name, sequence}]` with a
**string**-typed sequence, surfaced on `list_books` entries and on every search /
`get_chunk_context` result row (CONTRACT §2.2.1). It is read together with
`chapters` in one `SELECT chapters, series FROM book_metadata WHERE book_dir = $1`
per book directory (cached per result set), and via a `LEFT JOIN` on `book_dir`
for the `list_books` inventory and its `series` filter.

### 7. `transcript_findings` — Read-only Eval Layer (CONTRACT §2.15)

Advisory suspected-error findings recorded by the read-only LLM judge
(`internal/eval`, `earmark eval`). The eval layer is **strictly read-then-insert**:
it READS `transcripts`/`transcript_chunks` and INSERTs here; it NEVER updates,
deletes, or alters the transcript tables. The one foreign key points the other
way and never cascades into them: `transcript_id → transcripts(id) ON DELETE SET
NULL` (migration 5), so a requeue that deletes a transcript keeps its findings,
archived as `superseded` with `transcript_id` NULL (CONTRACT §1.4).
`suggested_correction` is informational only — never applied.

```sql
CREATE TABLE IF NOT EXISTS transcript_findings (
    id                   UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    transcript_id        UUID        REFERENCES transcripts (id) ON DELETE SET NULL,  -- NULL only when superseded (CHECK)
    file_path            TEXT        NOT NULL,
    chunk_id             UUID,
    chunk_index          INTEGER,
    start_sec            FLOAT8      NOT NULL,
    end_sec              FLOAT8      NOT NULL,
    original_text        TEXT        NOT NULL,
    issue_type           TEXT        NOT NULL,
    suggested_correction TEXT,
    confidence           FLOAT8      NOT NULL,
    model                TEXT        NOT NULL,
    transcription_run_id UUID,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- indexes: file_path, transcript_id, transcription_run_id, issue_type
```

`confidence` is the judge's self-score (0–1, the triage/scoring signal).
`transcription_run_id` is the `transcription_jobs.id` of the run that produced
the transcript, so findings are attributable per ASR backend/run.

Additive columns from the reviewable-patch migration are omitted above for
brevity: `patch_state` (the `proposed → accepted → applied → reverted` /
`rejected` / `stale` / `unanchorable` machine, plus the terminal `superseded`
that requeue archives every state into), `superseded_at` (when a requeue
archived it; migration 5), the anchor trio (`anchor_offset`,
`anchor_occurrence`, `chunk_text_sha256`), `decided_at`/`decided_by`,
`applied_at`/`applied_before_text`/`applied_after_text` (**span**-level, not
whole-chunk), and `stale_reason`. This table is the authoritative home of a
human-accepted correction — `transcript_chunks` only ever carries a replayed
copy. CONTRACT §2.17 is the reference. Also omitted: `origin` (`judge` |
`human`), `resolved_model` (the model that answered), and `recipe_id`
(migration 2; the judge's `propose` recipe, NULL for `origin='human'`).

Migration 4 adds the re-anchor pass's columns (`earmark reanchor`, CONTRACT
§2.17 "Re-anchoring"):

```sql
unanchorable_reason TEXT,         -- 'anchor_not_found' | 'anchor_ambiguous'; set iff patch_state = 'unanchorable'
reanchored_at       TIMESTAMPTZ,  -- last time earmark reanchor wrote this row's anchor or state; NULL = never
-- CHECK patch_state IN (proposed, accepted, rejected, applied, stale, reverted, unanchorable)
--   (migration 5 appends 'superseded')
-- CHECK (patch_state = 'unanchorable') = (unanchorable_reason IS NOT NULL)
```

A re-anchor rewrites the existing anchor columns in place — `chunk_id`,
`chunk_index` (from the chunk row), `chunk_text_sha256`, `anchor_offset`,
`anchor_occurrence` — and never touches a finding outside
`proposed`/`unanchorable`. `start_sec`/`end_sec` keep the judged audio window
(the next re-anchor searches by it). An `unanchorable` row keeps its original
anchor. A requeue supersedes `unanchorable` findings like any other and clears
their `unanchorable_reason` (the CHECK above); the re-anchor pass never reads a
`superseded` row or one with a NULL `transcript_id`.

### 8. `recipes` — Provenance (CONTRACT §1.9)

Immutable, content-addressed records of how an output row was made. Written
insert-if-absent by the Go writers — for `asr`, the worker's
`StampASRRecipes` from the runner-reported provenance columns (migration 6); never
updated or deleted.

```sql
CREATE TABLE recipes (
    recipe_id      TEXT        NOT NULL PRIMARY KEY,  -- hex sha256 of the canonical JSON (CONTRACT §1.9)
    step           TEXT        NOT NULL,              -- asr | propose | decide | propagate | scan | format | embed
    step_version   INTEGER     NOT NULL,              -- 0 = legacy
    code_version   TEXT        NOT NULL,              -- "<tag>+<commit>" or 'legacy-unknown'
    model_alias    TEXT,                              -- what was asked for
    model_resolved TEXT,                              -- what answered
    model_revision TEXT,
    prompt_version TEXT,
    prompt_sha256  TEXT,
    params         JSONB       NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- index: (step, created_at)
```

Referenced by the nullable `recipe_id` on `transcripts` (asr),
`transcript_findings` (propose) and `transcript_chunks` (embed), each indexed.

### 9. `current_recipes` and the `stale_work` view (CONTRACT §1.9)

```sql
CREATE TABLE current_recipes (
    step       TEXT        NOT NULL PRIMARY KEY,
    recipe_id  TEXT        NOT NULL REFERENCES recipes (recipe_id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- stale_work(step, source_table, row_id, recipe_id, current_recipe_id)
```

The ingest process upserts the current `embed` and `propose` recipes at
startup. Right after the first deploy every legacy row is stale (≈39,644 chunks and
≈32,337 findings on production): none was made by the current configuration. `stale_work` lists every output row whose recipe differs from its
step's current recipe in anything but `code_version` (unstamped rows count as
stale; steps without a current recipe, human corrections and superseded findings never appear).
Since migration 8 a `decide` arm lists findings whose latest unrevoked
`finding_events` decision came from a recipe other than the current `decide`
recipe.
Since migration 9 a `scan` arm lists every chunk with no scan of its *current* text by a
recipe equivalent to the current `scan` recipe (`earmark monitor` sets one when
`AI_ROLES.scan` is bound).

### 10. `fn_calls` — Pure-function call log and cache (CONTRACT §1.9)

One row per call an `internal/fn` function makes to a decision model.

```sql
CREATE TABLE fn_calls (
    id             BIGSERIAL     PRIMARY KEY,
    fn             TEXT          NOT NULL,
    prompt_version TEXT          NOT NULL,
    prompt_sha256  TEXT          NOT NULL,
    model_alias    TEXT          NOT NULL,              -- pinned model asked for
    model_resolved TEXT,                                -- what answered
    model_revision TEXT,
    recipe_id      TEXT          REFERENCES recipes (recipe_id),
    input_sha256   TEXT          NOT NULL,              -- hex sha256 of the canonical input JSON
    input          JSONB         NOT NULL,
    output         JSONB,                               -- NULL on error
    error_class    TEXT,                                -- NULL on success
    latency_ms     INTEGER,
    input_tokens   INTEGER,
    output_tokens  INTEGER,
    cost_usd       NUMERIC(14,8),
    cache_hit      BOOLEAN       NOT NULL DEFAULT false,
    cached_from    BIGINT        REFERENCES fn_calls (id),
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT now()
);
-- unique index fn_calls_cache_key_idx: (fn, prompt_sha256, model_alias, input_sha256)
--   WHERE error_class IS NULL AND NOT cache_hit   -- the cache + pay-once guard
-- index fn_calls_recipe_id_idx: (recipe_id, created_at)
```

Inserts are `ON CONFLICT DO NOTHING` on the cache index; rows are never
updated. A cached row is served only when `model_resolved` is the expected
model (case and route prefix ignored). Cache hits are logged as their own rows (`cache_hit`, `cached_from`).

### 11. `finding_events` — Finding version history (CONTRACT §2.17)

Append-only log of every finding's history: `transition` rows (written by
triggers on `transcript_findings` for every `patch_state` change and every
insert not in `proposed`), `decision` rows (a decide recipe's apply / hold /
reject verdict) and `revoke` rows (withdraw one decision).

```sql
CREATE TABLE finding_events (
    id                BIGSERIAL   PRIMARY KEY,
    finding_id        UUID        NOT NULL REFERENCES transcript_findings (id) ON DELETE CASCADE,
    transcript_id     UUID,                   -- not a FK: survives a requeue
    kind              TEXT        NOT NULL,   -- transition | decision | revoke
    from_state        TEXT,
    to_state          TEXT,
    outcome           TEXT,                   -- apply | hold | reject
    reason            TEXT,
    actor             TEXT        NOT NULL,   -- decided_by, earmark.actor setting, or 'system'
    recipe_id         TEXT        REFERENCES recipes (recipe_id),
    p                 FLOAT8,                 -- [0,1]
    evidence          TEXT,                   -- asin_verbatim | exact_repeat | none
    fn_call_id        BIGINT      REFERENCES fn_calls (id),
    chunk_text_sha256 TEXT,
    issue_type        TEXT,
    revokes_event_id  BIGINT      REFERENCES finding_events (id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
-- indexes: (finding_id, id DESC), (recipe_id, created_at), (transcript_id, created_at)
-- unique finding_events_revokes_idx: (revokes_event_id) WHERE kind = 'revoke'
-- triggers: transcript_findings_record_transition (AFTER UPDATE OF patch_state),
--           transcript_findings_record_insert (AFTER INSERT, non-proposed),
--           finding_events_append_only (BEFORE UPDATE OR DELETE: raises unless cascade)
```

### 12. `chunk_scan` — Chunk quality scan (CONTRACT §1.9 "Chunk scan")

One row per (chunk position, chunk text hash, scan recipe), written by
`earmark scan --yes`.

```sql
CREATE TABLE chunk_scan (
    id                 BIGSERIAL   PRIMARY KEY,
    transcript_id      UUID        NOT NULL REFERENCES transcripts (id) ON DELETE CASCADE,
    chunk_index        INTEGER     NOT NULL,              -- >= 0
    chunk_text_sha256  TEXT        NOT NULL,              -- sha256 of COALESCE(source_text, text) as scanned
    recipe_id          TEXT        NOT NULL REFERENCES recipes (recipe_id),
    fn_call_id         BIGINT      REFERENCES fn_calls (id),
    p_needs_fix        FLOAT8      NOT NULL,              -- [0,1]
    quality            FLOAT8      NOT NULL,              -- [1,5] = System One score + 1
    quality_confidence FLOAT8,                            -- [0,1], NULL when not reported
    p_boilerplate      FLOAT8      NOT NULL,
    p_garbled          FLOAT8      NOT NULL,
    p_dialogue         FLOAT8      NOT NULL,
    issue_type         TEXT        NOT NULL,              -- six issue types or 'none'
    issue_probs        JSONB       NOT NULL,              -- {label: p}
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transcript_id, chunk_index, chunk_text_sha256, recipe_id)
);
-- index chunk_scan_recipe_id_idx: (recipe_id)
```

Insert-only (`ON CONFLICT DO NOTHING`, after re-hashing the chunk in the same
statement); rows go only with their transcript. A row whose hash no longer
matches the chunk describes replaced text and is ignored by the quality index
and by `stale_work`.

## Relationships

```
transcription_jobs (1) ←── transcripts (1) ←── transcript_chunks (N)
                   (1) ←── run_metrics (0..1)
book_metadata      (key: book_dir — filepath.Dir of any file_path in the book)
runner_control     (singleton, id=1)
recipes (1) ←── transcripts / transcript_findings / transcript_chunks (N, via nullable recipe_id)
transcript_findings (1) ←── finding_events (N, ON DELETE CASCADE)
        (1) ←── current_recipes (one per step)
        (1) ←── fn_calls (N, via nullable recipe_id)
fn_calls (1) ←── fn_calls (N cache-hit rows, via cached_from)
transcripts (1) ←── chunk_scan (N, ON DELETE CASCADE; addressed by chunk_index + text hash)
recipes (1) ←── chunk_scan (N);  fn_calls (1) ←── chunk_scan (N, nullable fn_call_id)
```

Cascade deletes propagate: deleting a job removes its transcript, all chunks,
and its run_metrics row.

## Common Queries

### Claim a pending job (Python runner)

```sql
UPDATE transcription_jobs
SET    status     = 'claimed',
       claimed_by = $1,
       claimed_at = now(),
       attempts   = attempts + 1
WHERE  id = (
    SELECT id FROM transcription_jobs
    WHERE  status = 'pending' AND attempts < 3
    ORDER  BY created_at ASC
    FOR UPDATE SKIP LOCKED LIMIT 1
)
RETURNING id, file_path, checksum;
```

### Stale-claim recovery (Go service)

```sql
-- Reset below-max-attempts back to pending
UPDATE transcription_jobs
SET    status = 'pending', claimed_by = NULL, claimed_at = NULL
WHERE  status = 'claimed'
  AND  updated_at < now() - ($1 * interval '1 second')
  AND  attempts < 3;

-- Mark max-attempts as failed
UPDATE transcription_jobs
SET    status = 'failed', error = 'max attempts reached'
WHERE  status = 'claimed'
  AND  updated_at < now() - ($1 * interval '1 second')
  AND  attempts >= 3;
```

### Vector similarity search

```sql
SELECT c.id, c.text, c.file_path, c.chunk_index,
       c.start_sec, c.end_sec, c.speaker,
       1 - (c.embedding <=> $1) AS similarity
FROM transcript_chunks c
WHERE 1 - (c.embedding <=> $1) >= $2
ORDER BY c.embedding <=> $1
LIMIT $3;
```

### Full-text search (trigram)

```sql
SELECT c.id, c.text, c.file_path, c.chunk_index, c.start_sec, c.end_sec, c.speaker
FROM transcript_chunks c
WHERE c.text ILIKE '%' || $1 || '%'
ORDER BY c.chunk_index ASC
LIMIT $2;
```

---

> **Tombstone**: The old schema (authors / books / chapters / vectors / transcriptions,
> VECTOR(1536), OpenAI text-embedding-ada-002) is gone. Do not reference those
> table names in new code.
