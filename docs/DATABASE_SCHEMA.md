# Database Schema Reference

> **CURRENT SCHEMA — see CONTRACT.md §§1.1, 1.2, 3 for authoritative definitions.**
>
> The old five-table schema (authors / books / chapters / vectors / transcriptions,
> VECTOR(1536), OpenAI ada-002) has been replaced. If you have an existing
> database from that era, drop it and re-run the service to auto-migrate.

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

    CONSTRAINT transcription_jobs_checksum_unique UNIQUE (checksum)
);

CREATE INDEX transcription_jobs_status_idx  ON transcription_jobs (status, created_at);
CREATE INDEX transcription_jobs_file_path_idx ON transcription_jobs (file_path);
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
    duration_seconds FLOAT8      NOT NULL,
    speaker_count    INTEGER,               -- NULL when diarization disabled
    segments         JSONB       NOT NULL,  -- []Segment (see CONTRACT §1.2.1)
    raw_text         TEXT        NOT NULL,  -- full transcript, concatenated
    model_name       TEXT        NOT NULL,  -- e.g. "large-v3"
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transcripts_job_id_unique UNIQUE (job_id)
);

CREATE INDEX transcripts_file_path_idx     ON transcripts (file_path);
CREATE INDEX transcripts_raw_text_trgm_idx ON transcripts USING gin (raw_text gin_trgm_ops);
```

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
    start_sec     FLOAT8      NOT NULL,   -- earliest segment start in this chunk
    end_sec       FLOAT8      NOT NULL,   -- latest segment end in this chunk
    text          TEXT        NOT NULL,
    speaker       TEXT,                   -- dominant speaker, or NULL
    embedding     VECTOR(768) NOT NULL,   -- nomic-embed-text; MUST match EMBEDDINGS_MODEL
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transcript_chunks_transcript_chunk_unique UNIQUE (transcript_id, chunk_index)
);

CREATE INDEX transcript_chunks_embedding_idx
    ON transcript_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX transcript_chunks_file_path_idx ON transcript_chunks (file_path);
CREATE INDEX transcript_chunks_text_trgm_idx
    ON transcript_chunks USING gin (text gin_trgm_ops);
```

**Vector dimension**: 768 (nomic-embed-text). Any model change requires a full
re-embed and a column type migration.

**Chunk size**: 512 tokens, 64-token overlap (Go tokenizer). Controlled by
`CHUNK_SIZE` env var.

## Relationships

```
transcription_jobs (1) ←── transcripts (1) ←── transcript_chunks (N)
```

Cascade deletes propagate: deleting a job removes its transcript and all chunks.

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
> VECTOR(1536), OpenAI text-embedding-ada-002) was removed when the service was
> rewritten for the Linux/Postgres/Ollama/WhisperX architecture. Do not reference
> those table names in new code.
