# Architecture Overview

earmark is the search/knowledge layer over an audiobook library. It does **no transcription** — an external Python ASR runner on the GPU host handles that. earmark enqueues work, processes results, and serves search.

## Data Flow

```
NFS audiobooks (read-only)
        │
        ▼
  Go monitor
  (walks BOOKS_DIR, SHA-256 dedup)
        │ INSERT pending rows
        ▼
  transcription_jobs   ←──────────────────────────────┐
        │                                              │
        │ SELECT … FOR UPDATE SKIP LOCKED              │ heartbeat / mark done/failed
        ▼                                              │
  Python ASR runner (desktop-1, CUDA)                 │
  NeMo Parakeet-TDT-0.6b-v3                           │
  reads audio over NFS                                │
        │ INSERT INTO transcripts (segments JSONB)    │
        └────────────────────────────────────────────→┘
                                          ▼
                                   Go embed worker
                          (polls done jobs with no chunks)
                          chunks by token → Ollama nomic-embed-text
                                          │
                                          ▼
                                  transcript_chunks
                                  VECTOR(768), HNSW index
                                          │
                                          ▼
                                  Go MCP server (:8081/mcp)
                                  + status dashboard (/)
```

## Components

### monitor (`internal/monitor`)

Walks `BOOKS_DIR` on NFS. For each audio file not yet in the DB, computes a SHA-256 and inserts a `pending` row into `transcription_jobs`. Also upserts `book_metadata` (title/author from path or Audiobookshelf API). Deduplication is by checksum — the same file imported twice produces one job.

### Python ASR runner (external, GPU host)

Runs natively on desktop-1 (CUDA, RTX 5090). Uses **NeMo Parakeet-TDT-0.6b-v3** (`bfloat16`). Polls `transcription_jobs` for `pending` rows via an atomic `FOR UPDATE SKIP LOCKED` claim, transcribes audio from NFS, and writes a structured `transcripts` row (segments as JSONB with word-level timestamps). Sends a heartbeat every 60s while running; the Go service resets stale claims after 30m. The runner honors a `runner_control` DB row (pause / bounded run) and a local busy-flag file (`/tmp/earmark-busy`) for GPU-contention gating.

### embed worker (`internal/worker`)

Polls for `done` transcript jobs that have no corresponding `transcript_chunks`. Reads the segments JSONB, splits into 512-token chunks (64-token overlap), calls Ollama (`nomic-embed-text`, 768-dim, OpenAI-compatible API) for embeddings, and inserts rows into `transcript_chunks`. Writes telemetry to `run_metrics` (best-effort).

### database — CNPG Postgres (`earmark-pg`)

PostgreSQL 16 with **pgvector** and **pg_trgm**. Three core tables:

| Table | Purpose |
|-------|---------|
| `transcription_jobs` | Job queue: `pending → claimed → done/failed`. Dedup by SHA-256. |
| `transcripts` | Full structured output from the ASR runner. `segments` JSONB with timestamps; `raw_text` for FTS. |
| `transcript_chunks` | Embedding/search units. `VECTOR(768)`, HNSW index for ANN; btree on `file_path` for exact scoped search. |

Supporting tables: `book_metadata` (title/author/bias terms per book directory), `run_metrics` (per-job telemetry), `runner_control` (pause/bounded-run gate).

### MCP server + dashboard (`internal/mcp`)

Streamable-HTTP MCP server on `:8081/mcp`. Status dashboard (htmx, auto-refresh) at `/`. Five read-only tools:

| Tool | Purpose |
|------|---------|
| `list_books` | Library inventory with per-book progress, duration, word count. `flat` or `tree` format. |
| `semantic_search_audiobooks` | Vector cosine similarity search (HNSW or exact scoped). Optional `book` scope, `snippet` excerpt. |
| `text_search_audiobooks` | Trigram keyword search (`pg_trgm`). Hits labelled "ranked by trigram match". |
| `get_transcript` | Paginated raw segments for a track (timestamped). |
| `get_chunk_context` | Neighbouring chunks around a chunk UUID. |

Exposed to AI clients via mcp-proxy upstream key `"audiobooks"` at `http://earmark.earmark:8081/mcp`.

## Deployment

Two Kubernetes Deployments rendered by the in-repo OCI Helm chart (`deploy/helm/earmark/`):

- **ingest**: monitor + embed worker (reads NFS, writes DB)
- **mcp**: MCP server + dashboard (serves `:8081`)

Both run in the `earmark` namespace. Image: `ghcr.io/jedwards1230/earmark`. ArgoCD auto-syncs from `homelab-k8s`. The Python ASR runner is a native host service on desktop-1, not a K8s pod — it connects to CNPG directly over the LAN.
