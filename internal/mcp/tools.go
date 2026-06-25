package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/library"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/predict"
)

// DBInterface defines the database operations needed by MCP tools
type DBInterface interface {
	Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error)
	TextSearch(ctx context.Context, query string, limit int) ([]db.SearchResultWithMetadata, error)
	// TextSearchInBook runs TextSearch scoped to one book directory (the per-book
	// search box on the book-detail page; also the scoped text_search MCP tool).
	TextSearchInBook(ctx context.Context, dir, query string, limit int) ([]db.SearchResultWithMetadata, error)
	// SearchInBook runs semantic search scoped to one book directory via an exact
	// (non-HNSW) distance scan, so a selective single-book filter keeps full recall.
	SearchInBook(ctx context.Context, query, dir string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error)
	GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]db.SearchResultWithMetadata, error)
	// Ping checks that the database is reachable; used by /readyz.
	Ping(ctx context.Context) error
	// GetServiceStatus returns aggregate queue/transcript/chunk counts for the
	// status dashboard.
	GetServiceStatus(ctx context.Context) (*db.QueueStats, error)
	// GetPredictInputs returns the empirical inputs (per-stage median rates,
	// remaining chunks, runner availability fraction) for the ETA model
	// (internal/predict, CONTRACT §4).
	GetPredictInputs(ctx context.Context) (predict.Inputs, error)
	// GetRecentJobs returns the most-recently-updated jobs for the dashboard
	// activity table.
	GetRecentJobs(ctx context.Context, limit int) ([]db.RecentJob, error)
	// RequeueByID re-transcribes a single job by UUID (dashboard requeue button).
	RequeueByID(ctx context.Context, id string) (string, error)
	// RequeueFailed re-transcribes all failed jobs (dashboard "retry all" button).
	RequeueFailed(ctx context.Context) ([]string, error)
	// SetPaused writes the global runner pause flag (dashboard pause/resume).
	SetPaused(ctx context.Context, paused bool, by string) error
	// GetControl returns the full runner_control state — pause flag plus the
	// bounded-run counter (nil = unlimited) — for the control API.
	GetControl(ctx context.Context) (paused bool, runLimit *int, err error)
	// SetRunLimit writes the bounded-run counter without touching the pause flag
	// (nil = unlimited). Used by the control API's "run N jobs then auto-pause".
	SetRunLimit(ctx context.Context, limit *int, by string) error
	// GetPipelinePhase returns the coordinator's current pipeline phase
	// ("idle"|"transcribe"|"analyze") from runner_control, normalizing NULL/missing
	// to "idle". The dashboard READS this for a read-only phase badge — it never
	// writes it; the `earmark batch` coordinator owns phase transitions (CONTRACT
	// §1.4). A NULL/missing row degrades to "idle".
	GetPipelinePhase(ctx context.Context) (string, error)
	// GetBookSummaries returns one row per book directory (the library view),
	// plus the total matching-book count for pagination.
	GetBookSummaries(ctx context.Context, f db.BookFilter) ([]db.BookSummary, int, error)
	// GetLibraryTotals returns whole-library book counts (total, fully
	// transcribed, with pending) for the list_books summary line. query scopes it
	// to the same author filter list_books uses (empty = whole library).
	GetLibraryTotals(ctx context.Context, query string) (db.LibraryTotals, error)
	// GetBookTracks returns the individual track jobs for one book directory.
	GetBookTracks(ctx context.Context, dir string) ([]db.RecentJob, error)
	// GetTrackDetail returns the full per-track view (job + transcript + metrics +
	// segments + chunks) for one job UUID (the /track page).
	GetTrackDetail(ctx context.Context, jobID string) (*db.TrackDetail, error)
	// RequeueByDir re-transcribes every track in one book directory (book page).
	RequeueByDir(ctx context.Context, dir string) ([]string, error)
	// GetFailedJobs returns failed jobs with full triage detail (failures view).
	GetFailedJobs(ctx context.Context) ([]db.FailedJob, error)
	// GetServerObservation returns observed runner activity (live claims + per-host
	// run_metrics) the Servers page merges with the configured ASR_SERVERS list.
	GetServerObservation(ctx context.Context) (*db.ServerObservation, error)
	// GetFindingsSummary returns the read-only eval-layer findings rollup
	// (totals, confidence buckets, issue-type tally, per-book) for the /findings
	// dashboard page (CONTRACT §2.15).
	GetFindingsSummary(ctx context.Context) (*db.FindingsSummary, error)
	// ListFindings returns individual finding rows (the triage worklist) sorted by
	// confidence DESC, for the /findings worklist and the per-book Book section. An
	// empty dir returns the whole library; a non-empty dir scopes to one book.
	ListFindings(ctx context.Context, dir string, limit int) ([]db.FindingRow, error)
	// GetFindingsCountByBook returns the findings count keyed by book directory,
	// in one aggregate query, for the ⚑ findings-count column on the library list
	// (avoids an N+1 over the paged book rows).
	GetFindingsCountByBook(ctx context.Context) (map[string]int, error)
	// ClearFindings deletes recorded findings (advisory eval metadata) and
	// returns the rows removed, for the /findings "clear findings" button. It
	// touches ONLY transcript_findings — never transcripts/segments/chunks — so
	// the read-only-over-transcripts invariant holds (§2.15) and a clear can
	// always be undone by re-running eval. An empty dir clears all findings; a
	// non-empty dir scopes the delete to one book.
	ClearFindings(ctx context.Context, dir string) (int64, error)
	// GetEvalChunksForBook / SampleEvalChunks / InsertFindings back the on-demand
	// "run eval" dashboard actions (CONTRACT §2.15). The first two are the
	// read-only eval.ChunkReader; the third is the insert-only eval.FindingWriter.
	// The judge never mutates transcripts — its only write is InsertFindings.
	GetEvalChunksForBook(ctx context.Context, dir string, limit int) ([]db.EvalChunk, error)
	SampleEvalChunks(ctx context.Context, limit int) ([]db.EvalChunk, error)
	InsertFindings(ctx context.Context, findings []db.Finding) error
}

// ToolHandlers contains the database interface and logger for MCP tools
type ToolHandlers struct {
	db     DBInterface
	logger log.Logger
	// meta derives (author, title) labels from book paths, used to match a
	// user-supplied `book` argument to a canonical book directory and to label the
	// list_books inventory. Never nil once constructed via NewToolHandlers.
	meta metaprovider.MetadataProvider
}

// NewToolHandlers creates a new ToolHandlers instance. meta may be nil (e.g.
// in unit tests that don't exercise book resolution); a no-op PathProvider is
// substituted so handlers can always call it safely.
func NewToolHandlers(database DBInterface, meta metaprovider.MetadataProvider) *ToolHandlers {
	if meta == nil {
		meta = metaprovider.NewPathProvider("", "")
	}
	return &ToolHandlers{
		db:     database,
		logger: log.NewLogger("mcp-tools"),
		meta:   meta,
	}
}

// clampLimit returns def if limit is outside [1, 1000], otherwise returns limit.
func clampLimit(limit, def int) int {
	if limit < 1 || limit > 1000 {
		return def
	}
	return limit
}

// clampThreshold bounds the cosine-similarity threshold to [0.0, 1.0]. Out-of-range
// values are nonsensical: < 0 bypasses the filter entirely, > 1 guarantees zero
// results. An out-of-range value falls back to the default 0.3.
func clampThreshold(threshold float64) float64 {
	if threshold < 0.0 || threshold > 1.0 {
		return 0.3
	}
	return threshold
}

// maxOffset bounds pagination offset to keep a malicious/garbage value from
// forcing Postgres to skip through a huge result set (SQL OFFSET cost is linear
// in the offset). No real library or transcript needs to page this deep.
const maxOffset = 1_000_000

// minSnippetChars is the floor a non-zero `snippet` is raised to, so a tiny
// value (e.g. snippet=5) still yields a useful excerpt rather than a few letters.
const minSnippetChars = 80

// maxSnippetChars is the ceiling for the `snippet` parameter. Chunks are
// ~400 words / ~2500 chars; any value at or above the full chunk length yields
// the whole chunk anyway, so capping at 4000 is harmless and prevents large
// rune-slice allocations from a malicious or misconfigured client.
const maxSnippetChars = 4000

// snippetChars normalizes the optional `snippet` param (max chars per hit).
// 0 or negative → 0 (omitted: return the full chunk). A positive value below the
// floor is raised to minSnippetChars; a value above the ceiling is clamped to
// maxSnippetChars (a snippet ≥ the full chunk just returns the full chunk, so
// the cap is harmless).
func snippetChars(n int) int {
	if n <= 0 {
		return 0
	}
	if n < minSnippetChars {
		return minSnippetChars
	}
	if n > maxSnippetChars {
		return maxSnippetChars
	}
	return n
}

// clampOffset bounds the pagination offset to [0, maxOffset].
func clampOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	if offset > maxOffset {
		return maxOffset
	}
	return offset
}

// maxContextWindow caps get_chunk_context's neighbour radius. GetChunkContext
// pulls chunks in [index-window, index+window], so an unbounded window (e.g.
// contextWindow=2147483647) would scan/return an entire transcript. 50 each side
// is far more surrounding context than any caller needs.
const maxContextWindow = 50

// clampContextWindow bounds the chunk-context radius to [0, maxContextWindow].
func clampContextWindow(window int) int {
	if window < 0 {
		return 0
	}
	if window > maxContextWindow {
		return maxContextWindow
	}
	return window
}

// handleSemanticSearch performs semantic search on audiobook transcriptions,
// over the whole library by default or scoped to one book when `book` is given.
func (h *ToolHandlers) handleSemanticSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract required query parameter
	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Missing or invalid query parameter: %v", err)), nil
	}

	// Extract optional parameters with defaults
	threshold := clampThreshold(request.GetFloat("threshold", 0.3))
	limit := clampLimit(request.GetInt("limit", 10), 10)
	book := strings.TrimSpace(request.GetString("book", ""))
	// snippet (max chars per hit). 0/omitted → full chunk; <0 ignored.
	snippet := snippetChars(request.GetInt("snippet", 0))

	// Scoped: resolve `book` → a canonical dir, then run an exact (non-HNSW)
	// distance scan within that book so a selective filter keeps full recall.
	if book != "" {
		dir, errResult := h.resolveBookDir(ctx, book)
		if errResult != nil {
			return errResult, nil
		}
		h.logger.Info("Performing scoped semantic search", "query", query, "book", book, "dir", dir, "threshold", threshold, "limit", limit, "snippet", snippet)
		results, err := h.db.SearchInBook(ctx, query, dir, limit, threshold)
		if err != nil {
			h.logger.Error("Scoped semantic search failed", "error", err)
			return mcp.NewToolResultError(fmt.Sprintf("Search failed: %v", err)), nil
		}
		return formatSearchResultsOpts(results, searchSemantic, query, snippet), nil
	}

	h.logger.Info("Performing semantic search", "query", query, "threshold", threshold, "limit", limit, "snippet", snippet)

	// Perform semantic search (whole library, HNSW-accelerated)
	results, err := h.db.Search(ctx, query, limit, threshold)
	if err != nil {
		h.logger.Error("Semantic search failed", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Search failed: %v", err)), nil
	}

	// Format results using the shared formatting function
	return formatSearchResultsOpts(results, searchSemantic, query, snippet), nil
}

// handleTextSearch performs full-text search on audiobook transcriptions, over
// the whole library by default or scoped to one book when `book` is given.
func (h *ToolHandlers) handleTextSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract required query parameter
	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Missing or invalid query parameter: %v", err)), nil
	}

	// Extract optional parameters
	limit := clampLimit(request.GetInt("limit", 10), 10)
	book := strings.TrimSpace(request.GetString("book", ""))
	// snippet (max chars per hit). 0/omitted → full chunk; <0 ignored.
	snippet := snippetChars(request.GetInt("snippet", 0))

	// Scoped: resolve `book` → a canonical dir and reuse the per-book text search.
	if book != "" {
		dir, errResult := h.resolveBookDir(ctx, book)
		if errResult != nil {
			return errResult, nil
		}
		h.logger.Info("Performing scoped text search", "query", query, "book", book, "dir", dir, "limit", limit, "snippet", snippet)
		results, err := h.db.TextSearchInBook(ctx, dir, query, limit)
		if err != nil {
			h.logger.Error("Scoped text search failed", "error", err)
			return mcp.NewToolResultError(fmt.Sprintf("Search failed: %v", err)), nil
		}
		return formatSearchResultsOpts(results, searchText, query, snippet), nil
	}

	h.logger.Info("Performing text search", "query", query, "limit", limit, "snippet", snippet)

	// Perform text search (whole library)
	results, err := h.db.TextSearch(ctx, query, limit)
	if err != nil {
		h.logger.Error("Text search failed", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Search failed: %v", err)), nil
	}

	// Format results using the shared formatting function
	return formatSearchResultsOpts(results, searchText, query, snippet), nil
}

// bookCandidate pairs a resolved book directory with its display labels, used
// both for book-scope resolution and the candidate list in disambiguation errors.
type bookCandidate struct {
	dir    string
	author string
	title  string
}

// resolveBookDir maps a user-supplied `book` string (a book name or directory
// substring) to a single canonical book directory. It queries GetBookSummaries
// with the term as an ILIKE filter, resolves each candidate's author/title via
// the resolver, then keeps candidates by one of two rules:
//
//   - If the query contains a bracketed catalogue id (`[B0...]` or `[<digits>]`),
//     it is matched against each book's ASIN EXACTLY. This is the precise lookup
//     and avoids a bare title colliding on an unrelated book's ASIN.
//   - Otherwise the (ASIN-stripped) "author + title" label is substring-matched.
//     Crucially the raw dir/path/ASIN is NOT part of the haystack, so a query like
//     "1984" matches the *1984* titles but never a book whose ASIN merely contains
//     "1984" (e.g. Kahneman's *Noise* at ASIN 1984832069).
//
// Exactly one match → its dir; zero or many → a helpful error result listing
// candidate labels. The returned *mcp.CallToolResult is non-nil only on the
// error/ambiguous path (already formatted for return to the model); dir is set
// only on success.
func (h *ToolHandlers) resolveBookDir(ctx context.Context, book string) (string, *mcp.CallToolResult) {
	// A generous page so substring matches across a large library aren't truncated
	// before the in-Go label filter runs.
	summaries, _, err := h.db.GetBookSummaries(ctx, db.BookFilter{Query: book, Limit: 200})
	if err != nil {
		h.logger.Error("book resolution failed", "book", book, "error", err)
		return "", mcp.NewToolResultError(fmt.Sprintf("Failed to resolve book %q: %v", book, err))
	}

	// A bracketed catalogue id in the query switches to exact-ASIN matching.
	queryASIN := library.ExtractASIN(book)
	term := strings.ToLower(book)

	var matches []bookCandidate
	for _, s := range summaries {
		bookMeta, _ := h.meta.Lookup(ctx, s.SamplePath, s.SamplePath)
		author, title := bookMeta.Author, bookMeta.Title

		if queryASIN != "" {
			// Exact ASIN lookup: compare against the id embedded in the book's
			// title/dir, never a substring of the surrounding text.
			bookASIN := library.ExtractASIN(title)
			if bookASIN == "" {
				bookASIN = library.ExtractASIN(s.Dir)
			}
			if strings.EqualFold(bookASIN, queryASIN) {
				matches = append(matches, bookCandidate{dir: s.Dir, author: author, title: title})
			}
			continue
		}

		// Substring match against the human "author + title" label only — with the
		// ASIN stripped — so the raw path/ASIN can't cause a false hit.
		label := strings.ToLower(strings.TrimSpace(library.StripASIN(author) + " " + library.StripASIN(title)))
		if strings.Contains(label, term) {
			matches = append(matches, bookCandidate{dir: s.Dir, author: author, title: title})
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].dir, nil
	case 0:
		return "", mcp.NewToolResultError(fmt.Sprintf(
			"No book matched %q. Use list_books to see the available titles, or omit `book` to search the whole library.", book))
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matched %d books — please be more specific:\n", book, len(matches))
		for _, m := range matches {
			label := strings.TrimSpace(m.title)
			if m.author != "" {
				label = strings.TrimSpace(m.author + " — " + m.title)
			}
			fmt.Fprintf(&b, "  • %s  (%s)\n", label, m.dir)
		}
		return "", mcp.NewToolResultError(b.String())
	}
}

// handleListBooks returns the library inventory: each book with author, title,
// track progress, duration, word count, and chunk count.
func (h *ToolHandlers) handleListBooks(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := clampLimit(request.GetInt("limit", 50), 50)
	offset := clampOffset(request.GetInt("offset", 0))
	author := strings.TrimSpace(request.GetString("author", ""))
	// format: "flat" (default, one entry per book) or "tree" (group books by
	// author). Both query the same rows; tree only regroups them in the formatter.
	format := strings.ToLower(strings.TrimSpace(request.GetString("format", "flat")))

	h.logger.Info("Listing books", "limit", limit, "offset", offset, "author", author, "format", format)

	summaries, total, err := h.db.GetBookSummaries(ctx, db.BookFilter{
		Query: author, Limit: limit, Offset: offset,
	})
	if err != nil {
		h.logger.Error("list books failed", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list books: %v", err)), nil
	}

	// Whole-library (filter-scoped) counts for the one-line summary header. This is
	// a TRUE total across the library, not just the current page. A failure here is
	// non-fatal — the inventory still renders, just without the summary line.
	totals, err := h.db.GetLibraryTotals(ctx, author)
	if err != nil {
		h.logger.Warn("library totals failed; omitting summary line", "error", err)
		totals = db.LibraryTotals{}
	}

	if format == "tree" {
		return formatBookTree(ctx, summaries, total, offset, totals, h.meta), nil
	}
	return formatBookList(ctx, summaries, total, offset, totals, h.meta), nil
}

// handleGetTranscript returns a page of a track's transcript so the model can
// read the full text. It resolves `book` → a track (disambiguating when a book
// has multiple tracks), then returns timestamped segments with offset/limit
// pagination.
func (h *ToolHandlers) handleGetTranscript(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	offset := clampOffset(request.GetInt("offset", 0))
	limit := clampLimit(request.GetInt("limit", 50), 50)
	// Opt-in per-word timestamps. Default false → the structured payload omits the
	// `words` field entirely (byte-identical to the pre-word-timestamp response).
	includeWords := request.GetBool("includeWordTimestamps", false)

	// Two ways in: an explicit track/job id, or a `book` to resolve.
	trackID := strings.TrimSpace(request.GetString("trackID", ""))
	if trackID == "" {
		book := strings.TrimSpace(request.GetString("book", ""))
		if book == "" {
			return mcp.NewToolResultError("Provide either `book` (a title/dir) or `trackID` (a job id). Use list_books to find titles."), nil
		}
		dir, errResult := h.resolveBookDir(ctx, book)
		if errResult != nil {
			return errResult, nil
		}
		tracks, err := h.db.GetBookTracks(ctx, dir)
		if err != nil {
			h.logger.Error("get book tracks failed", "dir", dir, "error", err)
			return mcp.NewToolResultError(fmt.Sprintf("Failed to load tracks for %q: %v", book, err)), nil
		}
		if len(tracks) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("No tracks found for %q.", book)), nil
		}
		if len(tracks) > 1 {
			// Multiple tracks → list them so the caller can pick one via trackID.
			return formatTrackChooser(book, tracks), nil
		}
		trackID = tracks[0].ID
	}

	detail, err := h.db.GetTrackDetail(ctx, trackID)
	if err != nil {
		h.logger.Error("get track detail failed", "trackID", trackID, "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Failed to load transcript for track %q: %v", trackID, err)), nil
	}
	if detail == nil {
		return mcp.NewToolResultError(fmt.Sprintf("No track found with id %q.", trackID)), nil
	}
	if !detail.HasTranscript {
		return mcp.NewToolResultError(fmt.Sprintf("Track %q is not transcribed yet (status: %s).", detail.FilePath, detail.Status)), nil
	}

	return formatTranscriptPage(detail, offset, limit, includeWords), nil
}

// handleGetContext retrieves surrounding chunks for better context
func (h *ToolHandlers) handleGetContext(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract required chunk ID parameter
	chunkID, err := request.RequireString("chunkID")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Missing or invalid chunkID parameter: %v", err)), nil
	}

	// Extract optional context window size (default 1 chunk before and after, so
	// the default response is ~3 chunks), bounded so a huge value can't pull an
	// entire transcript's chunks.
	contextWindow := clampContextWindow(request.GetInt("contextWindow", 1))

	h.logger.Info("Getting chunk context", "chunkID", chunkID, "contextWindow", contextWindow)

	// Get context chunks
	results, err := h.db.GetChunkContext(ctx, chunkID, contextWindow)
	if err != nil {
		h.logger.Error("Failed to get chunk context", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get context: %v", err)), nil
	}

	// Format results using the shared formatting function
	return formatSearchResults(results), nil
}
