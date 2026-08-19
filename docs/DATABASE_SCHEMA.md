# Database Schema Reference

> **See CONTRACT.md §§1.1, 1.2, 1.5, 1.6, and 3 for authoritative definitions.**
> This document summarises the current schema; CONTRACT.md wins on any discrepancy.

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

    CONSTRAINT transcripts_job_id_unique UNIQUE (job_id)
);

CREATE INDEX transcripts_file_path_idx     ON transcripts (file_path);
CREATE INDEX transcripts_raw_text_trgm_idx ON transcripts USING gin (raw_text gin_trgm_ops);
```

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
once the rebuilt chunk is inserted. See CONTRACT §2.17.

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
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`bias_terms` is re-derived from metadata on every write (never COALESCE-guarded).

`chapters` is a JSONB array of `{"Index","Title","StartSec","EndSec"}` objects
whose times are **book-absolute** — measured from the start of the whole book,
all tracks concatenated in play order (that is what Audiobookshelf's
`media.chapters` reports). Mapping a `transcript_chunks` row into this list
requires adding the chunk's track offset first; see the time-bases note under
`transcripts` (§2).

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
deletes, or alters the transcript tables, and this table has no FK that could
cascade a mutation back into them. `suggested_correction` is informational only —
never applied.

```sql
CREATE TABLE IF NOT EXISTS transcript_findings (
    id                   UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    transcript_id        UUID        NOT NULL,
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
`rejected` / `stale` machine), the anchor trio (`anchor_offset`,
`anchor_occurrence`, `chunk_text_sha256`), `decided_at`/`decided_by`,
`applied_at`/`applied_before_text`/`applied_after_text` (**span**-level, not
whole-chunk), and `stale_reason`. This table is the authoritative home of a
human-accepted correction — `transcript_chunks` only ever carries a replayed
copy. CONTRACT §2.17 is the reference.

## Relationships

```
transcription_jobs (1) ←── transcripts (1) ←── transcript_chunks (N)
                   (1) ←── run_metrics (0..1)
book_metadata      (key: book_dir — filepath.Dir of any file_path in the book)
runner_control     (singleton, id=1)
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
