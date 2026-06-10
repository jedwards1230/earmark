# CLAUDE.md

This file provides guidance to Claude Code when working with the lilbro-whisper repository.

## Project Overview

**lilbro-whisper** is a Go service that indexes audiobook transcriptions produced by an
external ASR runner (Python, running on the GPU/ASR host) and exposes them for semantic
search via an MCP server. It runs as a Linux container in Kubernetes.

### Architecture

```
GPU/ASR host (Python ASR runner)
    │ writes transcription_jobs + transcripts via CNPG Postgres
    ▼
PostgreSQL (CNPG cluster lilbro-whisper-pg, pgvector + pg_trgm)
    │ polled by Go worker
    ▼
Go monitor + worker (K8s Deployment)
    ├── monitor: walks BOOKS_DIR, inserts pending jobs for new audio files
    ├── worker:  polls for completed transcripts → chunks → Ollama embeds → transcript_chunks
    └── mcp:     streamable-HTTP MCP server on :8081/mcp
```

Key facts:
- **No local transcription** — the Go service only enqueues jobs and processes results.
- **Embeddings**: Ollama (`nomic-embed-text`, 768-dim) via OpenAI-compatible API.
- **MCP transport**: `streamable-http` (default); `stdio` for local testing.
- **Database schema**: Three tables — `transcription_jobs`, `transcripts`,
  `transcript_chunks`. See `docs/DATABASE_SCHEMA.md` and `docs/CONTRACT.md`.

## Contract

`docs/CONTRACT.md` is **authoritative**. Do not change env var names, column
names, or the MCP upstream key without updating it first.

## Commands

```bash
# Build
go build ./...

# Test
go test ./...

# Lint
golangci-lint run ./...

# Run (requires DATABASE_URL)
./lil-whisper monitor   # file watcher + worker
./lil-whisper mcp       # MCP server (stdio by default)
MCP_TRANSPORT=http ./lil-whisper mcp  # HTTP transport on :8081
./lil-whisper serve     # HTTP search API
./lil-whisper list      # list embedded books
./lil-whisper search "query"

# Redo work without DEBUG_DB_RESET (dry-run unless --yes):
./lil-whisper requeue "Project Hail Mary"          # preview matches
./lil-whisper requeue "Project Hail Mary" --yes    # re-transcribe (drops transcript+chunks, job→pending)
./lil-whisper requeue --failed --yes               # retry all failed jobs
./lil-whisper requeue --reembed "" --yes           # re-embed only (drop chunks; e.g. after model/chunk change)
```

The status dashboard also exposes requeue as buttons: a per-row **requeue** on
each done/failed job and a **retry all failed** button. They POST to
`/actions/requeue?id=…` / `/actions/retry-failed` (htmx-guarded via the
`HX-Request` header) and re-render the status fragment.

For scripts/agents there is a JSON **control API** under `/api/v1` (see
`docs/CONTRACT.md §2.12`): `GET /status`, `GET|PUT /pipeline/pause`, and
`POST|DELETE /pipeline/run` (run N jobs then auto-pause — a one-call smoke test).
Mutations require `Authorization: Bearer $CONTROL_API_TOKEN` and fail closed
(`503`) when the token is unset.

## Visual Verification

The `mcp` HTTP transport serves a status dashboard at `/` (htmx, auto-refreshing
the `/status/data` fragment every 3s). For UI changes, verify it visually before
opening a PR — no database required:

```bash
go run . mcp --demo     # serves http://localhost:8081/ with synthetic data
# or: make dashboard
```

`--demo` backs the dashboard with an in-memory fixture (`internal/mcp/demo.go`)
so the page renders fully with no Postgres, no DATABASE_URL, and no ASR runner.
Set `DEMO_SCENARIO` to render a specific state: `active` (default), `empty`
(fresh install), `stale` (crashed runner — old heartbeat), or `failed` (failures
incl. a long multi-line error). To see the connection-lost banner, open the page
then stop the server — htmx flags the data stale instead of freezing silently.

Playwright is wired for AI agents via `.claude/mcp.json` (the `playwright` MCP
server). With the demo server running, use Playwright (MCP) to navigate to
`http://localhost:8081/`, then `browser_snapshot` / `browser_take_screenshot`.
Drive the htmx refresh by waiting, or fetch the fragment directly at
`http://localhost:8081/status/data`.

## Environment Variables

See `.env.example` and `docs/CONTRACT.md §2.4` for the canonical list.

Required: `DATABASE_URL`

Optional (with defaults): `BOOKS_DIR`, `EMBEDDINGS_BASE_URL`, `EMBEDDINGS_MODEL`,
`MCP_HTTP_ADDR`, `STALE_JOB_TIMEOUT`, `CHUNK_SIZE`, `DEBUG`.

Debug-only (both must be set):
- `DEBUG_DB_RESET=true`
- `DEBUG_DB_RESET_CONFIRM=yes-delete-everything`

## Key Packages

| Package | Responsibility |
|---------|----------------|
| `internal/db` | pgxpool-based DB handle; schema init; job queue; search; chunks |
| `internal/worker` | Polls completed transcripts, chunks via segments, embeds, stores |
| `internal/monitor` | Walks BOOKS_DIR, inserts pending jobs (dedup by SHA-256) |
| `internal/mcp` | MCP server + tool handlers — 5 tools: semantic/text search (optional per-book scope + `snippet` excerpt window; text hits labelled "trigram match", not similarity; ASIN-aware `book` resolution — bracketed `[B0…]`/`[digits]` matches ASIN exactly, else title+author substring with ASIN stripped), `list_books` (`format=flat\|tree`; transcribed-first ordering, whole-library summary line, flat omits `dir:`), `get_transcript` (paginates segments), `get_chunk_context` (chunk UUID → neighbours; `contextWindow` default 1). No `browse` tool. Result formatter suppresses the dead `Chapter 0:` label. |
| `internal/chunker` | Token-based text splitter |
| `internal/openai` | OpenAI-compatible embeddings client (pointed at Ollama) |
| `internal/config` | Env-var configuration loader |

## Development Notes

- `internal/db/db.go` uses `*pgxpool.Pool` (goroutine-safe). Never use `*pgx.Conn` directly.
- Chunk timestamps (`start_sec`, `end_sec`) come from WhisperX segment boundaries.
- The stale-job timeout uses integer-seconds SQL (`$1 * interval '1 second'`) to
  avoid PostgreSQL misinterpreting Go duration strings.
- MCP stdio transport: all diagnostics go to `os.Stderr`, never `os.Stdout`.
- `DEBUG_DB_RESET` requires a second env var confirmation to prevent accidental drops.

## TODO (deferred from review)

- M-8: Full testcontainers DB integration suite (needs Docker in CI).
- M-9: Replace vacuous compile-only tests with behaviour tests.
