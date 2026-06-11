package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/metaprovider"
)

// MCPServer wraps the MCP server functionality for the lilbro-whisper service
type MCPServer struct {
	server   *server.MCPServer
	handlers *ToolHandlers
	logger   log.Logger
	db       DBInterface // kept for /readyz probe

	// runnerStaleAfter is how old the runner's last heartbeat may be before the
	// dashboard reports it as "stale" instead of "active". A claimed job alone
	// doesn't prove the runner is alive — a crashed runner keeps a job claimed.
	runnerStaleAfter time.Duration

	// embedURL is the configured embeddings endpoint, shown in the embed-stall
	// warning so an operator knows which Ollama to check.
	embedURL string

	// meta derives (author, title) from book paths using LIBRARY_COLLECTIONS
	// config, so labels aren't hardcoded to one directory layout.
	meta metaprovider.MetadataProvider

	// controlToken is the bearer token required on mutating control-API endpoints.
	// Empty → those endpoints fail closed (503); see requireToken in api.go.
	controlToken string
}

// NewMCPServer creates a new MCP server instance
func NewMCPServer(database DBInterface, cfg *config.Config) *MCPServer {
	logger := log.NewLogger("mcp-server")

	staleAfter := cfg.StaleJobTimeout
	if staleAfter <= 0 {
		staleAfter = 30 * time.Minute
	}

	// Create MCP server with all capabilities enabled
	mcpServer := server.NewMCPServer("lilbro-whisper", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, false), // Not implementing resources yet
		server.WithPromptCapabilities(false),          // Not implementing prompts yet
		server.WithLogging(),
	)

	// Build the metadata provider from config. NewPathProvider handles parse
	// errors internally (falls back to generic resolver) — labels are cosmetic,
	// never a startup blocker.
	meta := metaprovider.NewPathProvider(cfg.LibraryCollections, cfg.BooksDir)

	// The tool handlers share the same provider so the `book` argument on the
	// search tools and the list_books labels match the dashboard's labels.
	handlers := NewToolHandlers(database, meta)

	// All search/browse/read tools are read-only and non-destructive — they only
	// query the database; they never mutate state or produce side-effects.
	readOnlyAnnotations := mcp.ToolAnnotation{
		ReadOnlyHint:    mcp.ToBoolPtr(true),
		DestructiveHint: mcp.ToBoolPtr(false),
		IdempotentHint:  mcp.ToBoolPtr(true),
	}

	// Add semantic search tool
	mcpServer.AddTool(mcp.NewTool("semantic_search_audiobooks",
		mcp.WithDescription("Search audiobook transcriptions by meaning (vector similarity). "+
			"Searches the WHOLE library by default; pass `book` to scope the search to a single title. "+
			"Each hit is a chunk (the embedding/search unit — a chunk is tens of consecutive ASR "+
			"segments grouped together, so one book has far fewer chunks than segments). "+
			"Use this to find passages about a concept; use list_books to discover titles, "+
			"get_chunk_context to expand around a hit, and get_transcript to read full text."),
		mcp.WithToolAnnotation(readOnlyAnnotations),
		mcp.WithString("query",
			mcp.Description("The search query to find relevant content"),
			mcp.Required(),
		),
		mcp.WithString("book",
			mcp.Description("Optional: restrict the search to one book (a title or directory substring, e.g. \"Project Hail Mary\"). Omit to search the entire library. Run list_books to see available titles."),
		),
		mcp.WithNumber("threshold",
			mcp.Description("Similarity threshold (0.0-1.0, default: 0.3)"),
			mcp.DefaultNumber(0.3),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10)"),
			mcp.DefaultNumber(10),
		),
		mcp.WithNumber("snippet",
			mcp.Description("Optional: cap each hit's quoted text to roughly this many characters to keep iterative searches cheap. "+
				"Omit to return the full ~400-word chunk (default). A semantic hit has no sub-chunk match position, so the "+
				"snippet is a leading PREVIEW of the chunk, not a centered excerpt — use get_chunk_context for the full "+
				"surrounding text."),
		),
	), handlers.handleSemanticSearch)

	// Add text search tool
	mcpServer.AddTool(mcp.NewTool("text_search_audiobooks",
		mcp.WithDescription("Search audiobook transcriptions by literal/keyword text (trigram match). "+
			"Searches the WHOLE library by default; pass `book` to scope the search to a single title. "+
			"Each hit is a chunk (the search unit — a chunk is tens of consecutive ASR segments grouped "+
			"together). Ranked by trigram match, not vector distance — results carry a \"trigram match\" "+
			"label, NOT a semantic-similarity score. "+
			"Use this for exact phrases or names; use semantic_search_audiobooks for conceptual queries."),
		mcp.WithToolAnnotation(readOnlyAnnotations),
		mcp.WithString("query",
			mcp.Description("The search query to find exact text matches"),
			mcp.Required(),
		),
		mcp.WithString("book",
			mcp.Description("Optional: restrict the search to one book (a title or directory substring, e.g. \"Dune\"). Omit to search the entire library. Run list_books to see available titles."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10)"),
			mcp.DefaultNumber(10),
		),
		mcp.WithNumber("snippet",
			mcp.Description("Optional: cap each hit's quoted text to roughly this many characters to keep iterative searches cheap. "+
				"Omit to return the full ~400-word chunk (default). When set, the excerpt is CENTERED on the literal query "+
				"match within the chunk. Use get_chunk_context for the full surrounding text."),
		),
	), handlers.handleTextSearch)

	// Add list_books tool — the library inventory.
	mcpServer.AddTool(mcp.NewTool("list_books",
		mcp.WithDescription("List the audiobook library inventory: each book with author, title, "+
			"track progress (done/total), total duration, word count, and embedded-chunk count. "+
			"This is the inventory tool — use it to discover which titles exist before scoping a "+
			"search or fetching a transcript. Pass format=tree to group the same books under their "+
			"authors instead of a flat list."),
		mcp.WithToolAnnotation(readOnlyAnnotations),
		mcp.WithString("author",
			mcp.Description("Optional: filter to books whose path/author matches this substring (case-insensitive)."),
		),
		mcp.WithString("format",
			mcp.Description("Output shape: \"flat\" (default) — one entry per book; or \"tree\" — group books by author. "+
				"Both list the same books with the same metadata; tree only changes the grouping."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of books to return (default: 50)"),
			mcp.DefaultNumber(50),
		),
		mcp.WithNumber("offset",
			mcp.Description("Pagination offset into the book list (default: 0)"),
			mcp.DefaultNumber(0),
		),
	), handlers.handleListBooks)

	// Add get_transcript tool — read the full transcript text (paginated).
	mcpServer.AddTool(mcp.NewTool("get_transcript",
		mcp.WithDescription("Read the full transcript of a book/track as timestamped segments, so you can "+
			"READ the text rather than only search fragments. This paginates SEGMENTS (raw ASR "+
			"timestamp units — there are far more segments than search chunks; a chunk is tens of "+
			"consecutive segments grouped for embedding). Provide `book` (a title) or `trackID` (a job id "+
			"from list_books / a track chooser). Transcripts are large, so segments are paginated via "+
			"offset/limit; the response footer tells you the next offset. If a book has multiple tracks, this "+
			"returns the track list so you can pick one by trackID."),
		mcp.WithToolAnnotation(readOnlyAnnotations),
		mcp.WithString("book",
			mcp.Description("A book title or directory substring to read (e.g. \"Project Hail Mary\"). Either this or trackID is required."),
		),
		mcp.WithString("trackID",
			mcp.Description("A specific track/job id (UUID) to read. Takes precedence over `book`. Use when a book has multiple tracks."),
		),
		mcp.WithNumber("offset",
			mcp.Description("Segment offset to start the page at (default: 0)"),
			mcp.DefaultNumber(0),
		),
		mcp.WithNumber("limit",
			mcp.Description("Number of segments to return per page (default: 50)"),
			mcp.DefaultNumber(50),
		),
	), handlers.handleGetTranscript)

	// Add chunk context tool
	mcpServer.AddTool(mcp.NewTool("get_chunk_context",
		mcp.WithDescription("Get the chunks surrounding a search hit, so you can read the full text around a match. "+
			"Operates on CHUNKS (the search/embedding unit — a chunk is tens of consecutive ASR segments grouped "+
			"together; use get_transcript to page raw segments instead). Pass the chunk's UUID from a search result."),
		mcp.WithToolAnnotation(readOnlyAnnotations),
		mcp.WithString("chunkID",
			mcp.Description("The chunk UUID returned in the `ID` field of semantic_search_audiobooks / "+
				"text_search_audiobooks results."),
			mcp.Required(),
		),
		mcp.WithNumber("contextWindow",
			mcp.Description("Number of chunks before and after to include (default: 1, i.e. ~3 chunks; clamped to 0–50)"),
			mcp.DefaultNumber(1),
		),
	), handlers.handleGetContext)

	return &MCPServer{
		server:           mcpServer,
		handlers:         handlers,
		logger:           logger,
		db:               database,
		runnerStaleAfter: staleAfter,
		embedURL:         cfg.EmbeddingsBaseURL,
		meta:             meta,
		controlToken:     cfg.ControlAPIToken,
	}
}

// StartStdio starts the MCP server using stdio transport
func (s *MCPServer) StartStdio() error {
	s.logger.Info("Starting MCP server with stdio transport")

	return server.ServeStdio(s.server)
}

// getOnly wraps a handler so non-GET requests get 405 Method Not Allowed.
// Used for the dashboard routes (which are read-only) in place of a "GET /"
// ServeMux method pattern, which would conflict with the method-less /mcp and
// /health routes and panic at registration.
func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

// buildMux constructs the HTTP mux used by the HTTP transport.
//
// Routes:
//
//	GET  /                     — status dashboard (full HTML shell)
//	GET  /track                — per-track detail page (shell)
//	GET  /track/data           — per-track detail fragment (header + reader + chunks)
//	GET  /status/data          — htmx-refreshed fragment (counts + recent jobs)
//	GET  /static/htmx.min.js   — vendored htmx library
//	POST /actions/requeue      — re-transcribe one job (htmx-guarded)
//	POST /actions/retry-failed — re-transcribe all failed jobs (htmx-guarded)
//	GET  /api/v1/status              — pipeline status snapshot (JSON, no auth)
//	GET  /api/v1/pipeline/pause      — current pause/run-limit state (JSON, no auth)
//	PUT  /api/v1/pipeline/pause      — pause/resume (JSON, bearer token)
//	POST /api/v1/pipeline/run        — run N jobs then auto-pause (JSON, bearer token)
//	DELETE /api/v1/pipeline/run      — clear a bounded run (JSON, bearer token)
//	GET  /health               — liveness probe (always 200 "ok")
//	GET  /readyz               — readiness probe (200 if DB ping OK, 503 otherwise)
//	*    /mcp                  — MCP streamable-HTTP handler
//
// Extracted so that tests can wire the same mux without binding a port.
func (s *MCPServer) buildMux() *http.ServeMux {
	mcpHandler := server.NewStreamableHTTPServer(s.server)

	mux := http.NewServeMux()

	// Three pages share a header/nav: Overview (/), Library (/library), and Book
	// detail (/book). Each page shell loads its data fragment via htmx. GET is
	// enforced with a wrapper rather than a "GET /" method pattern, which would
	// conflict with the method-less "/mcp" route and make ServeMux panic.
	// Only reachable under HTTP transport.
	mux.HandleFunc("/", getOnly(s.handleOverviewPage))
	mux.HandleFunc("/status/data", getOnly(s.handleStatusData))
	mux.HandleFunc("/library", getOnly(s.handleLibraryPage))
	mux.HandleFunc("/library/data", getOnly(s.handleLibraryData))
	mux.HandleFunc("/book", getOnly(s.handleBookPage))
	mux.HandleFunc("/book/data", getOnly(s.handleBookData))
	mux.HandleFunc("/track", getOnly(s.handleTrackPage))
	mux.HandleFunc("/track/data", getOnly(s.handleTrackData))
	mux.HandleFunc("/track/segments", getOnly(s.handleTrackSegments))
	mux.HandleFunc("/failed", getOnly(s.handleFailedPage))

	// Per-book transcript search (read-only; accepts the htmx form POST or a GET).
	// Specific path → method patterns are safe (no "/" catch-all conflict).
	mux.HandleFunc("GET /search/book", s.handleBookSearch)
	mux.HandleFunc("POST /search/book", s.handleBookSearch)
	mux.HandleFunc("/failed/data", getOnly(s.handleFailedData))

	// Vendored htmx (pinned), served from the binary so the dashboard needs no
	// external CDN at runtime.
	mux.HandleFunc("/static/htmx.min.js", getOnly(s.handleHTMX))

	// Mutating actions — POST-only (method pattern is safe here: these are
	// specific paths, unlike the "/" catch-all). htmx-guarded inside the handler.
	mux.HandleFunc("POST /actions/requeue", s.handleRequeueJob)
	mux.HandleFunc("POST /actions/retry-failed", s.handleRetryFailed)
	mux.HandleFunc("POST /actions/book-requeue", s.handleBookRequeue)
	mux.HandleFunc("POST /actions/pause", s.handlePause)
	mux.HandleFunc("POST /actions/resume", s.handleResume)

	// JSON control API (script/agent-facing) — distinct from the htmx dashboard
	// actions above. Reads are unauthenticated; mutations require the bearer token
	// (requireToken). These specific method+path patterns don't conflict with the
	// "/" catch-all or the method-less "/mcp" handler.
	mux.HandleFunc("GET /api/v1/status", s.handleAPIStatus)
	mux.HandleFunc("GET /api/v1/pipeline/pause", s.handleAPIPauseGet)
	mux.HandleFunc("PUT /api/v1/pipeline/pause", s.requireToken(s.handleAPIPausePut))
	mux.HandleFunc("POST /api/v1/pipeline/run", s.requireToken(s.handleAPIRun))
	mux.HandleFunc("DELETE /api/v1/pipeline/run", s.requireToken(s.handleAPIRunClear))

	// Liveness — no external deps.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Readiness — confirms the DB pool is reachable before accepting traffic.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			s.logger.Warn("readyz: DB ping failed", "error", err)
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// MCP protocol handler at /mcp (ServeHTTP handles all methods; path-based
	// routing is done internally by mcp-go when used as an http.Handler).
	mux.Handle("/mcp", mcpHandler)

	return mux
}

// StartHTTP starts the MCP server using HTTP transport on the specified address.
func (s *MCPServer) StartHTTP(addr string) error {
	s.logger.Info("Starting MCP server with HTTP transport", "address", addr)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.buildMux(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return httpSrv.ListenAndServe()
}

// GetServerInfo returns server information for introspection
func (s *MCPServer) GetServerInfo() (string, string) {
	return "lilbro-whisper", "1.0.0"
}

// Close gracefully shuts down the MCP server
func (s *MCPServer) Close() error {
	s.logger.Info("Shutting down MCP server")
	return nil
}

// StartMCPService is a convenience function to start MCP server with configuration
func StartMCPService(database DBInterface, cfg *config.Config) error {
	mcpServer := NewMCPServer(database, cfg)

	// Check for MCP_TRANSPORT environment variable to determine transport type
	transport := os.Getenv("MCP_TRANSPORT")
	if transport == "" {
		transport = "stdio" // Default to stdio
	}

	switch transport {
	case "stdio":
		return mcpServer.StartStdio()
	case "http":
		addr := cfg.MCPHTTPAddr // use config value (populated from MCP_HTTP_ADDR) rather than re-reading env
		if addr == "" {
			addr = ":8081"
		}
		return mcpServer.StartHTTP(addr)
	default:
		return fmt.Errorf("unsupported MCP transport: %s (use 'stdio' or 'http')", transport)
	}
}
