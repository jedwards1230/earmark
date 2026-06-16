# MCP Server

Streamable-HTTP MCP server for earmark. Serves 5 read-only tools on `:8081/mcp`
plus a status dashboard at `/`. The dashboard's `/findings` page and per-book
`/book` section render individual finding rows (read-only eval worklist, CONTRACT
§2.15) — confidence, issue type, `original → correction`, sorted highest-first —
with links into the book each finding belongs to; the Book page also offers a
token-gated scoped clear (`/actions/findings-clear?dir=…`).

## Files

- `server.go` — server setup and tool registration
- `tools.go` — tool handlers (`ToolHandlers`, `DBInterface`)
- `types.go` — shared result formatters
- `demo.go` — in-memory fixture for `--demo` mode
- `*_test.go` — test coverage

## Tools

There are exactly **5 tools**. The legacy `browse_audiobook_library` tool has been
removed — `list_books format=tree` covers that use case.

### `list_books`

Library inventory: per-book author, title, track progress (done/total), duration,
word count, and embedded-chunk count. Ordered transcribed-first. Leads with a
one-line library summary (`Library: T books — P fully transcribed, Q with pending
tracks.`).

| Param | Default | Notes |
|-------|---------|-------|
| `author` | — | substring filter |
| `format` | `flat` | `flat` (omits `dir:`) or `tree` (groups by author, keeps `dir:`) |
| `limit` | 50 | |
| `offset` | 0 | |

### `semantic_search_audiobooks`

Vector-similarity search. Hits show a cosine `similarity: NN%`. Whole library by
default; `book` scopes to one title via ASIN-aware resolution.

| Param | Default | Notes |
|-------|---------|-------|
| `query` | required | |
| `book` | — | title/author substring or bracketed ASIN |
| `threshold` | 0.3 | cosine similarity floor (0–1) |
| `limit` | 10 | |
| `snippet` | 0 (full chunk) | max chars; values below 80 raised to 80, above 4000 capped |

### `text_search_audiobooks`

Trigram/keyword search. Hits are labelled **"ranked by trigram match"** (no
similarity %). `snippet` centres the excerpt on the literal match.

| Param | Default | Notes |
|-------|---------|-------|
| `query` | required | |
| `book` | — | same resolution as semantic search |
| `limit` | 10 | |
| `snippet` | 0 (full chunk) | centres window on the match |

### `get_transcript`

Full transcript of a track as paginated timestamped **segments** (raw_text can be
600k+ chars). Multi-track books return a track-chooser listing; pick a `trackID`
to continue.

| Param | Default | Notes |
|-------|---------|-------|
| `book` | — | one of `book` or `trackID` required |
| `trackID` | — | job UUID from the track-chooser |
| `offset` | 0 | segment offset |
| `limit` | 50 | segments per page |

### `get_chunk_context`

Returns surrounding **chunks** around a search hit. `chunkID` is the UUID in a
search result's `ID` field.

| Param | Default | Notes |
|-------|---------|-------|
| `chunkID` | required | UUID from a search hit |
| `contextWindow` | 1 | chunks before/after (clamped 0–50); default → ~3 chunks |

## Transport

| Env var | Default | Values |
|---------|---------|--------|
| `MCP_TRANSPORT` | `stdio` | `stdio`, `http` |
| `MCP_HTTP_ADDR` | `:8081` | |

```bash
# stdio (default — Claude Desktop / local testing)
./earmark mcp

# HTTP (set MCP_TRANSPORT=http, e.g. in K8s)
MCP_TRANSPORT=http ./earmark mcp

# Demo mode — no database required
go run . mcp --demo
```

## Testing

```bash
go test ./internal/mcp/...
```
