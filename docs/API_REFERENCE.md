# API Reference

earmark exposes two interfaces: an **MCP server** for AI assistants and a **JSON control API** for scripts/agents. There is no legacy `GET /search` HTTP endpoint — search is MCP-only.

---

## MCP Server

**Transport**: `streamable-http` (production) or `stdio` (local testing).
**Base URL**: `:8081/mcp` (in-cluster: `http://earmark.earmark:8081/mcp`).
**mcp-proxy key**: `"audiobooks"`.

All tools are **read-only**. The two search tools default to the whole library; pass `book` to scope to a single title.

### Chunks vs segments

Two granularities of the same text:
- **chunk** — embedding/search unit (~hundreds per book; tens of consecutive ASR segments grouped together). Used by search tools and `get_chunk_context`.
- **segment** — single ASR timestamp unit (thousands per book). Returned by `get_transcript`.

### Tool: `list_books`

Library inventory. Per book: author, title, track progress (done/total), total duration, word count, embedded-chunk count. Ordered transcribed-first (fully done → partial → fully pending). Leads with a one-line whole-library summary.

| Parameter | Type | Default | Notes |
|-----------|------|---------|-------|
| `author` | string | — | Substring filter |
| `format` | `flat`\|`tree` | `flat` | `flat` omits each book's `dir:` line; `tree` groups by author and keeps `dir:` |
| `limit` | integer | 50 | — |
| `offset` | integer | 0 | — |

### Tool: `semantic_search_audiobooks`

Vector-similarity (meaning) search. Hits show a real cosine `similarity: NN%`.

| Parameter | Type | Default | Notes |
|-----------|------|---------|-------|
| `query` | string | **required** | Natural-language query |
| `book` | string | — | Scope to one title (ASIN-aware resolution; see below) |
| `threshold` | float | 0.3 | Cosine similarity floor (0.0–1.0) |
| `limit` | integer | 10 | Max hits |
| `snippet` | integer | — | Max chars of quoted text; floored to 80, capped at 4000. Omit for full chunk text |

Unscoped: HNSW index. Scoped (`book` set): exact distance scan within the book's chunks (recall-perfect, no filtered-ANN loss).

### Tool: `text_search_audiobooks`

Trigram literal/keyword search. Hits are labelled **"ranked by trigram match"** — no similarity percentage (a similarity % would mislead on a literal match).

| Parameter | Type | Default | Notes |
|-----------|------|---------|-------|
| `query` | string | **required** | Keyword or phrase |
| `book` | string | — | Scope to one title |
| `limit` | integer | 10 | Max hits |
| `snippet` | integer | — | Returns an excerpt centred on the literal match |

### Tool: `get_transcript`

Full transcript for a track as paginated **segments** (raw ASR output; `raw_text` can be 600 k+ chars). Multi-track book → returns a track chooser first.

| Parameter | Type | Default | Notes |
|-----------|------|---------|-------|
| `book` | string | one required | Book title or ASIN |
| `trackID` | string | one required | Track UUID (from chooser or search hit) |
| `offset` | integer | 0 | Segment offset |
| `limit` | integer | 50 | Segments per page |

### Tool: `get_chunk_context`

Surrounding chunks around a chunk. `chunkID` is the **UUID** in a search hit's `ID` field.

| Parameter | Type | Default | Notes |
|-----------|------|---------|-------|
| `chunkID` | string | **required** | UUID from a search hit |
| `contextWindow` | integer | 1 | Chunks on each side; clamped to 0–50 |

### `book` parameter resolution (search tools + `get_transcript`)

- A **bracketed catalogue id** (`[B0…]` or `[<digits>]`) is matched against each book's ASIN exactly.
- Otherwise: substring match against title + author with any bracketed ASIN stripped — so `book="1984"` finds the Orwell title without matching books whose ASIN contains `1984`.

Zero or multiple matches return a helpful error listing the candidates.

### Chapter label suppression

Chapter mapping is filled in by a future ABS-integration PR. Until then the formatter suppresses the `Chapter N:` label entirely when there is no real chapter data (index 0 and empty title) — no misleading `Chapter 0:` prefix is emitted.

---

## Control API (`/api/v1`)

JSON endpoints on the same `:8081` port as the MCP server. Read endpoints are always open; mutating endpoints require `Authorization: Bearer <CONTROL_API_TOKEN>` (fails closed with `503` when the token is unset).

| Method | Path | Auth | Body | Response |
|--------|------|------|------|----------|
| `GET` | `/api/v1/status` | none | — | `200` queue/runner snapshot |
| `GET` | `/api/v1/pipeline/pause` | none | — | `200 {"paused":bool,"runLimit":int\|null}` |
| `PUT` | `/api/v1/pipeline/pause` | bearer | `{"paused":bool}` | `200` current state (`paused:false` also clears any run bound) |
| `POST` | `/api/v1/pipeline/run` | bearer | `{"limit":N}` (N≥1) | `202 {"paused":false,"runLimit":N}` |
| `DELETE` | `/api/v1/pipeline/run` | bearer | — | `200` clears the bounded run (`run_limit→NULL`) |

**Single-job smoke test**:

```bash
curl -fsS -X POST https://<host>/api/v1/pipeline/run \
  -H "Authorization: Bearer $CONTROL_API_TOKEN" \
  -H 'Content-Type: application/json' -d '{"limit":1}'
```

### Pipeline pause/run semantics

| Operation | `paused` | `run_limit` |
|-----------|----------|-------------|
| pause | `true` | unchanged |
| resume (`PUT paused:false`) | `false` | `NULL` (clears any bound) |
| run N | `false` | `N` |
| clear run (`DELETE`) | unchanged | `NULL` |

The runner claims a new job only when `NOT paused AND (run_limit IS NULL OR run_limit > 0)`. Each successful claim decrements `run_limit` by 1 (in the same transaction). When `run_limit` reaches 0 the runner stops claiming without setting `paused`.

---

## Status Dashboard

The MCP HTTP transport also serves a status dashboard at `/` (htmx, auto-refreshes the `/status/data` fragment every 3 s). The dashboard exposes requeue buttons (`/actions/requeue?id=…`, `/actions/retry-failed`) guarded by the `HX-Request` header.

**Demo mode** (no database required):

```bash
go run . mcp --demo     # http://localhost:8081/
DEMO_SCENARIO=stale go run . mcp --demo
```

`DEMO_SCENARIO` values: `active` (default), `empty`, `stale`, `failed`.

---

## Error Responses

All API endpoints return JSON on error:

```json
{"error": "message", "details": "optional additional context"}
```

| Code | Meaning |
|------|---------|
| 400 | Bad request / missing parameter |
| 503 | `CONTROL_API_TOKEN` not set (mutating endpoints only) |
| 500 | Internal / database error |
