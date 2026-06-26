# earmark

Search and knowledge layer over an audiobook library. Runs as a Go service in Kubernetes, parallel to Audiobookshelf (which handles media playback). Does no transcription itself.

## Pipeline

```
GPU host — Python ASR runner (NeMo Parakeet-TDT-0.6b-v3, CUDA)
    │  reads audio over NFS, writes transcription_jobs + transcripts → CNPG Postgres
    ▼
PostgreSQL (CNPG earmark-pg, pgvector + pg_trgm)
    │  polled by Go service
    ▼
Go "ingest" Deployment (K8s)
    ├── monitor  walks BOOKS_DIR, inserts pending jobs (dedup by SHA-256)
    └── worker   polls completed transcripts → chunks (512 tok) → Ollama nomic-embed-text (768-dim) → transcript_chunks
Go "mcp" Deployment (K8s)
    └── MCP server  streamable-HTTP on :8081/mcp  +  status dashboard at /
```

## Subcommands

| Command | What it does |
|---------|-------------|
| `earmark monitor` | File watcher — enqueues new audio files, backfills book metadata |
| `earmark mcp` | MCP server (stdio default; `MCP_TRANSPORT=http` for HTTP on `:8081`) |
| `earmark serve` | HTTP search API |
| `earmark list` | List embedded books |
| `earmark search <query>` | Quick semantic search from the CLI |
| `earmark requeue [title]` | Re-transcribe or re-embed a book (preview unless `--yes`) |
| `earmark eval [book]` | Read-only LLM judge — flags suspected transcript errors (dry-run unless `--write`) |
| `earmark update` | Update earmark binary to the latest GitHub release |
| `earmark backfill-metadata` | Re-derive book metadata for all jobs without re-transcribing |

## MCP Tools (5)

| Tool | Purpose |
|------|---------|
| `list_books` | Library inventory with per-book progress, duration, word count |
| `semantic_search_audiobooks` | Vector-similarity search; results show cosine similarity % |
| `text_search_audiobooks` | Trigram literal/keyword search |
| `get_transcript` | Read a track's full timestamped transcript (paginated) |
| `get_chunk_context` | Surrounding chunks around a search-hit chunk UUID |

See [`internal/mcp/README.md`](internal/mcp/README.md) for full parameter docs.

## Deployment

Deployed via ArgoCD from `repos/homelab-k8s/apps/earmark/helmfile.yaml` using the in-repo Helm chart at `deploy/helm/earmark/` (published as `oci://ghcr.io/jedwards1230/charts/earmark`). Image: `ghcr.io/jedwards1230/earmark`. Auto-sync (`prune: true`, `selfHeal: true`).

The chart renders two Deployments: **ingest** (monitor + worker) and **mcp** (server). MCP is proxied through mcp-proxy at upstream key `"audiobooks"`.

## Configuration

Copy `.env.example` and set at minimum `DATABASE_URL`. All canonical env var names and defaults are in [`docs/CONTRACT.md §2.4`](docs/CONTRACT.md).

Key variables:

| Variable | Default | Notes |
|----------|---------|-------|
| `DATABASE_URL` | — | **Required.** `postgres://earmark:<pass>@earmark-pg-rw.earmark:5432/earmark` |
| `BOOKS_DIR` | `/books` | Read-only NFS mount |
| `AI_ENDPOINTS` | — | JSON array of AI endpoint descriptors (CONTRACT §2.14). When set, `AI_ROLES` is required and `EMBEDDINGS_*` vars are ignored. Malformed value is fatal. |
| `AI_ROLES` | — | JSON object mapping role names (e.g. `"embeddings"`, `"eval"`) to endpoint names in `AI_ENDPOINTS`. Required when `AI_ENDPOINTS` is set. |
| `EMBEDDINGS_BASE_URL` | `http://ollama:11434/v1` | **Deprecated** — use `AI_ENDPOINTS`/`AI_ROLES` instead. Synthesized into a `_legacy` endpoint when `AI_ENDPOINTS` is unset. |
| `EMBEDDINGS_MODEL` | `nomic-embed-text` | **Deprecated** — use `AI_ENDPOINTS`/`AI_ROLES` instead. 768-dim vectors. |
| `MCP_HTTP_ADDR` | `:8081` | HTTP transport bind address |
| `CHUNK_SIZE` | `512` | Target tokens per chunk |
| `CONTROL_API_TOKEN` | — | Bearer token for mutating control API; unset → fail closed |
| `METADATA_PROVIDER` | `path` | `path`, `abs`, or `chain:abs,path` |

## Demo dashboard (no database required)

```bash
go run . mcp --demo     # http://localhost:8081/
# or: make dashboard
```

Renders the htmx status dashboard with synthetic data. Set `DEMO_SCENARIO` to `active` (default), `empty`, `stale`, `failed`, or `multibackend` (three ASR families across three servers).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test/lint instructions, branching conventions, and PR guidelines.

## Reference

- [`docs/CONTRACT.md`](docs/CONTRACT.md) — authoritative schema, env vars, deployment interface
- [`docs/ARCHITECTURE_OVERVIEW.md`](docs/ARCHITECTURE_OVERVIEW.md) — component diagram and data flow
- [`docs/DATABASE_SCHEMA.md`](docs/DATABASE_SCHEMA.md) — table definitions and indexes
- [`internal/mcp/README.md`](internal/mcp/README.md) — MCP tool reference
