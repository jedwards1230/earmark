# earmark

Search and knowledge layer over an audiobook library.

earmark makes a shelf of audiobooks *searchable by meaning*. It indexes the transcripts an
external ASR runner produces, embeds them, and exposes the library to AI assistants over
[MCP](https://modelcontextprotocol.io) — so you can ask "which book talks about tidally
locked planets?" and get the passage back with its timestamp.

It runs alongside a media server such as Audiobookshelf rather than replacing one: earmark
never plays audio, and it does no transcription itself.

## How it works

```
GPU host — Python ASR runner (NVIDIA NeMo Parakeet-TDT, CUDA)
    │  reads audio over NFS, writes transcription_jobs + transcripts
    ▼
PostgreSQL (pgvector + pg_trgm)
    │  polled by the Go service
    ▼
earmark ingest  (`earmark monitor`)
    ├── monitor  walks BOOKS_DIR, enqueues new audio files (dedup by SHA-256)
    └── worker   completed transcripts → chunks → embeddings → transcript_chunks
earmark mcp     (`earmark mcp`)
    └── MCP server on :8081/mcp  +  htmx status dashboard at :8081/
```

The Go service and the ASR runner share nothing but the database — the schema and the
env-var names are specified in [`docs/CONTRACT.md`](docs/CONTRACT.md), which is
authoritative for both sides. The runner itself lives in [`runner/`](runner/README.md) and
is deployed separately, on a GPU host.

## Quickstart

Requires Go (the version is pinned in `go.mod`) and a PostgreSQL with the `vector` and
`pg_trgm` extensions available. earmark creates those extensions and its own tables on
first connect — there is no migration step to run.

```bash
git clone https://github.com/jedwards1230/earmark.git
cd earmark
make build          # → ./earmark
```

Start a local database and point earmark at it:

```bash
cp .env.example .env
# Set DATABASE_URL. compose.yaml passes .env straight through to the Postgres
# container, so also add POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB —
# .env.example does not include them.
docker compose up -d db

./earmark list                       # should connect and report an empty library
./earmark search "a spice-mining desert"
```

To index a library you also need the ASR runner producing transcripts into the same
database; see [`runner/README.md`](runner/README.md).

### Try the dashboard without a database

```bash
make dashboard      # or: go run . mcp --demo   →  http://localhost:8081/
```

This renders the full status dashboard against synthetic data — no Postgres, no
`DATABASE_URL`. Set `DEMO_SCENARIO` to render a different state — `active` is the default;
`empty`, `stale`, `failed`, `winddown`, and `multibackend` are also available.

## Commands

| Command | What it does |
|---------|-------------|
| `earmark monitor` | Ingest service — watches `BOOKS_DIR`, enqueues jobs, embeds finished transcripts |
| `earmark mcp` | MCP server (stdio by default; `MCP_TRANSPORT=http` for HTTP on `:8081`) |
| `earmark serve` | Standalone HTTP search API — `GET /search` on `:8080` |
| `earmark list` | List content from the database |
| `earmark search <query>` | Semantic search from the CLI (`--text` for keyword, `--limit`, `--precision`) |
| `earmark requeue [book]` | Re-transcribe, retry failed, or re-embed (`--reembed`, `--failed`; dry-run unless `--yes`) |
| `earmark eval [book]` | Read-only LLM judge — flags suspected transcript errors (dry-run unless `--write`) |
| `earmark batch` | Two-phase pipeline coordinator that time-shares a GPU with other tenants |
| `earmark backfill-metadata` | Re-derive book metadata for existing jobs without re-transcribing |
| `earmark version` | Version, commit, build time |
| `earmark update` | Self-update from the latest GitHub release. **Not usable today** — releases publish a container image and a Helm chart, not binary assets, so this cannot find one to download |

Run `earmark <command> --help` for the full flag list.

## Using it from an MCP client

Point your client at the server over stdio:

```json
{
  "mcpServers": {
    "earmark": {
      "command": "/usr/local/bin/earmark",
      "args": ["mcp"],
      "env": { "DATABASE_URL": "postgres://earmark:…@localhost:5432/earmark" }
    }
  }
}
```

Or run it over HTTP (`MCP_TRANSPORT=http earmark mcp`) and connect to
`http://<host>:8081/mcp`.

It exposes eight tools — six read-only, and two that write, which together form the human
gate on proposed transcript corrections:

| Tool | Purpose |
|------|---------|
| `list_books` | Library inventory: author, title, series, track progress, duration, word count |
| `semantic_search_audiobooks` | Vector-similarity search; hits carry a cosine similarity score |
| `text_search_audiobooks` | Trigram literal/keyword search |
| `get_transcript` | Read a track's full transcript as timestamped segments (paginated) |
| `get_chunk_context` | Expand the chunks surrounding a search hit |
| `list_transcript_corrections` | Worklist of proposed/decided transcript corrections |
| `decide_transcript_correction` | **Writes.** Accept, reject, revert, or reconsider a correction |
| `create_transcript_correction` | **Writes.** Record a correction no model proposed |

Corrections never edit the ASR transcript, which is immutable. They are stored as a
replayable overlay and reapplied onto regenerated chunk text, so every decision is
reversible. Full parameter documentation is in
[`docs/API_REFERENCE.md`](docs/API_REFERENCE.md) and
[`internal/mcp/README.md`](internal/mcp/README.md).

## HTTP endpoints

`earmark mcp` with `MCP_TRANSPORT=http` serves everything on `MCP_HTTP_ADDR` (`:8081`):

| Path | What it is |
|------|-----------|
| `/mcp` | MCP streamable-HTTP endpoint |
| `/` | htmx status dashboard — pipeline, library, per-book/track, servers, findings |
| `/api/v1/status` | JSON pipeline status (read-only, unauthenticated) |
| `/api/v1/pipeline/pause`, `/api/v1/pipeline/run` | Pause/resume and run-N-then-pause. `PUT`/`POST`/`DELETE` require `Authorization: Bearer $CONTROL_API_TOKEN` and fail closed with `503` when it is unset |
| `/api/v1/openapi.yaml` | OpenAPI 3.1 contract for the JSON control API, embedded in the binary (read-only, unauthenticated) |
| `/healthz`, `/health` | Liveness |
| `/metrics` | Prometheus metrics |

`earmark monitor` serves its own `/healthz` and `/metrics` on `INGEST_HTTP_ADDR` (`:8082`).

## Configuration

Configuration is entirely environment-driven; an optional `.env` file is loaded at startup.
[`.env.example`](.env.example) documents every variable, and
[`docs/CONTRACT.md`](docs/CONTRACT.md) §2.4 is the authoritative list of names and defaults.

The ones you are most likely to set:

| Variable | Default | Notes |
|----------|---------|-------|
| `DATABASE_URL` | — | **Required.** PostgreSQL DSN |
| `BOOKS_DIR` | `/books` | Library root the monitor walks; mounted read-only in production |
| `SCAN_INTERVAL` | `1h` | How often to re-walk `BOOKS_DIR`. fsnotify misses writes made by another NFS client, so this walk is what actually finds new books. `0` disables it |
| `CHUNK_SIZE` | `512` | Target tokens per chunk |
| `AI_ENDPOINTS` | — | JSON array of OpenAI-compatible endpoints: `{id, type: embeddings\|chat, backend, baseURL, model}`. When set, `AI_ROLES` is required and the `EMBEDDINGS_*` vars are ignored. A malformed value is fatal |
| `AI_ROLES` | — | JSON mapping roles to endpoint ids: `{"embeddings": "…", "eval": "…"}`. `embeddings` is required whenever `AI_ENDPOINTS` is set |
| `EMBEDDINGS_BASE_URL` | `http://ollama:11434/v1` | **Deprecated** — synthesized into a `_legacy` endpoint when `AI_ENDPOINTS` is unset |
| `EMBEDDINGS_MODEL` | `nomic-embed-text` | **Deprecated.** 768-dimension vectors |
| `MCP_HTTP_ADDR` | `:8081` | Bind address for the HTTP transport and dashboard |
| `INGEST_HTTP_ADDR` | `:8082` | Bind address for the ingest pod's health/metrics listener |
| `METADATA_PROVIDER` | `path` | `path`, `abs` (Audiobookshelf — needs `ABS_URL` + `ABS_TOKEN`), or `chain:abs,path` |
| `LIBRARY_COLLECTIONS` | — | JSON describing each library root's directory layout, so author/title labels come from config rather than a hardcoded path shape |
| `ASR_SERVERS` | — | JSON describing the transcription hosts, for the Servers dashboard page. Read-only — it does not route work |
| `CONTROL_API_TOKEN` | — | Bearer token for the mutating control API; unset means those endpoints fail closed |
| `LOG_FORMAT` | `pretty` | `pretty` or `json` |

## Deployment

Releases publish a container image to `ghcr.io/jedwards1230/earmark` and a Helm chart to
`oci://ghcr.io/jedwards1230/charts/earmark`. The chart source is in
[`deploy/helm/earmark/`](deploy/helm/earmark).

```bash
helm install earmark oci://ghcr.io/jedwards1230/charts/earmark
```

It renders two Deployments — **ingest** (monitor + worker) and **mcp** (server) — plus an
Ingress, a Prometheus `PodMonitor`, and optionally a CloudNativePG PostgreSQL cluster
(`cnpg.enabled`) and 1Password `OnePasswordItem` secrets (`secrets.enabled`). The books
directory is expected as an existing `ReadOnlyMany` PVC named by `booksPvcName`.
[`values.yaml`](deploy/helm/earmark/values.yaml) documents every key inline.

The ASR runner is **not** deployed by this chart — it runs natively on a GPU host.

## Documentation

- [`docs/CONTRACT.md`](docs/CONTRACT.md) — authoritative schema, env vars, and deployment interface
- [`docs/ARCHITECTURE_OVERVIEW.md`](docs/ARCHITECTURE_OVERVIEW.md) — components and data flow
- [`docs/API_REFERENCE.md`](docs/API_REFERENCE.md) — MCP tools and the JSON control API
- [`docs/DATABASE_SCHEMA.md`](docs/DATABASE_SCHEMA.md) — tables and indexes
- [`docs/STARTUP_PROCESS.md`](docs/STARTUP_PROCESS.md) — what each process does at boot
- [`docs/MAKEFILE_GUIDE.md`](docs/MAKEFILE_GUIDE.md) — the make targets
- [`docs/design/`](docs/design) — design notes for the ASR backend interface and the eval layer
- [`runner/README.md`](runner/README.md) — the Python ASR runner

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites, build/test/lint commands,
branching conventions, and the PR and release flow.

## License

[MIT](LICENSE)
