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

    CONSTRAINT transcription_jobs_checksum_unique UNIQUE (checksum)
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

#### Pause control — `runner_control` table

A singleton row holds the global pause flag. The Go service (dashboard) writes
it; the runner reads it. Because it lives in the shared database it is durable
across reboots and visible to both the off-host runner and the in-cluster
service (unlike the busy flag).

```sql
CREATE TABLE IF NOT EXISTS runner_control (
    id         INTEGER     NOT NULL PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    paused     BOOLEAN     NOT NULL DEFAULT false,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT
);
-- seeded once by the Go service on init:
INSERT INTO runner_control (id, paused) VALUES (1, false) ON CONFLICT (id) DO NOTHING;
```

The runner MUST check this flag at the top of each poll cycle, before the claim
UPDATE (alongside the busy-flag check). When `paused` is true it finishes any
in-flight job, then skips claiming new work:

```sql
SELECT paused FROM runner_control WHERE id = 1;
```

A missing row (the Go service not yet initialized) MUST be treated as
**not paused** so the runner degrades safely. The flag gates new claims only; an
in-flight transcription always runs to completion.

#### Operator requeue (out-of-band, `lil-whisper requeue`)

In addition to the runner/service transitions above, an operator may move a job
**back** to `pending` to redo it. This is the only sanctioned way a `done` or
`failed` job returns to `pending`. It is always operator-initiated (never the
runner) and is transactional:

- **Re-transcribe**: delete the job's row in `transcripts` (which cascades to
  `transcript_chunks`) and `UPDATE … SET status='pending', attempts=0,
  error=NULL, claimed_by=NULL, claimed_at=NULL`. The runner then re-processes it
  like any pending job.
- **Re-embed only**: delete the matching rows in `transcript_chunks` and leave
  `transcripts`/`transcription_jobs` untouched. The Go worker re-embeds on its
  next poll (it selects transcripts with no chunks). Use after an embedding
  model or `CHUNK_SIZE` change — no re-transcription.

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
- A column name, type, or constraint in sections 1.1, 1.2, or 3
- An env var name in section 2.4
- The mcp-proxy upstream key or URL in section 2.2
- The embedding model or vector dimension in section 2.3

...requires updating this file **before** writing implementation code. All
three repos (lilbro-whisper Go, homelab-ansible runner, homelab-k8s manifests)
must be updated atomically when a contract value changes.
