# Startup Process

earmark runs as two separate commands in Kubernetes: `earmark monitor` (ingest Deployment) and `earmark mcp` (MCP Deployment). Both share the same binary and the same Postgres database.

---

## `earmark monitor`

Runs the file watcher and embed worker together.

### 1. Config loading

`config.LoadConfig()` reads all env vars. `DATABASE_URL` is required; startup fails immediately if it is absent. Optional vars (`BOOKS_DIR`, `EMBEDDINGS_BASE_URL`, `EMBEDDINGS_MODEL`, `CHUNK_SIZE`, `STALE_JOB_TIMEOUT`, etc.) fall back to documented defaults.

### 2. DB connection and schema init

Opens a `pgxpool.Pool` to the CNPG read-write endpoint (`earmark-pg-rw.earmark:5432`). On first connect, a schema-init transaction runs:

- `CREATE EXTENSION IF NOT EXISTS vector` and `pg_trgm`
- `CREATE TABLE IF NOT EXISTS transcription_jobs` (with status check constraint, unique indexes, `updated_at` trigger)
- `CREATE TABLE IF NOT EXISTS transcripts` (with trgm GIN index on `raw_text`)
- `CREATE TABLE IF NOT EXISTS transcript_chunks` (`VECTOR(768)`, HNSW index)
- `CREATE TABLE IF NOT EXISTS run_metrics` (additive telemetry; never blocks the pipeline)
- `CREATE TABLE IF NOT EXISTS book_metadata` (additive enrichment)
- `CREATE TABLE IF NOT EXISTS runner_control` + `INSERT … ON CONFLICT DO NOTHING` to seed the singleton pause/run-limit row

Schema init is idempotent — safe on every restart.

### 3. Monitor goroutine

Walks `BOOKS_DIR` to discover audio files. For each file not already in `transcription_jobs` (dedup by SHA-256 checksum):

- Computes checksum
- Calls `MetadataProvider.Lookup` to derive title/author/bias_terms and upserts `book_metadata` (best-effort; failure does not block enqueue)
- UPSERTs a `pending` row into `transcription_jobs`
- UPSERTs a `run_metrics` row with `audio_bytes` (from `os.Stat`)

After the initial walk, the monitor enters a poll/watch loop for new files.

### 4. Stale-job recovery goroutine

A background goroutine runs on a ticker (period: `STALE_JOB_TIMEOUT`, default `30m`). It resets `claimed` jobs whose `updated_at` is older than the timeout back to `pending` (if `attempts < 3`) or marks them `failed` (if `attempts >= 3`). This recovers jobs from a crashed ASR runner without any manual intervention.

### 5. Worker goroutine

Polls `transcripts` for rows whose `job_id` has no corresponding `transcript_chunks`. For each:

1. Reads the `segments` JSONB column
2. Chunks segments into ~512-token windows (64-token overlap) via `internal/chunker`
3. Calls Ollama (`nomic-embed-text`, 768-dim) via the OpenAI-compatible embeddings API at `EMBEDDINGS_BASE_URL`
4. Bulk-inserts `transcript_chunks` rows
5. UPSERTs `run_metrics` embed columns (best-effort)

The worker does **no transcription** — it only processes transcripts already written by the external Python ASR runner.

### 6. Signal handling

`SIGINT`/`SIGTERM` triggers a graceful shutdown: the monitor and worker goroutines drain, the DB pool closes, the process exits cleanly.

---

## `earmark mcp`

Runs the MCP HTTP server and status dashboard.

### 1. Config loading

Same `config.LoadConfig()`. `DATABASE_URL` is required.

### 2. DB connection

Same `pgxpool.Pool` open; same idempotent schema init (safe to run on both pods).

### 3. HTTP server

Listens on `MCP_HTTP_ADDR` (default `:8081`). Serves:

- `/mcp` — streamable-HTTP MCP transport (5 tools: `list_books`, `semantic_search_audiobooks`, `text_search_audiobooks`, `get_transcript`, `get_chunk_context`)
- `/` — htmx status dashboard (auto-refreshes `/status/data` fragment every 3 s)
- `/api/v1/*` — JSON control API (pause/resume/run-N; mutating endpoints require `Authorization: Bearer $CONTROL_API_TOKEN`)
- `/actions/*` — htmx-guarded dashboard actions (requeue, retry-failed)

### 4. Signal handling

`SIGINT`/`SIGTERM` triggers an HTTP graceful shutdown (30 s timeout for in-flight requests), then closes the DB pool.

---

## Startup failure modes

| Condition | Behavior |
|-----------|----------|
| `DATABASE_URL` missing | Immediate fatal exit |
| DB unreachable | Fatal exit with connection error |
| `BOOKS_DIR` unreadable | Monitor logs a warning; enqueue loop skips (service stays up) |
| Ollama unreachable | Worker logs per-job errors and retries on the next poll cycle |
| `book_metadata` / `run_metrics` write failure | Logged and skipped; never blocks enqueue or embed |
