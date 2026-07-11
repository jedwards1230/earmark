package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/metrics"
)

// MCPServer wraps the MCP server functionality for the earmark service
type MCPServer struct {
	server   *mcp.Server
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

	// asrServers is the static ASR_SERVERS registry (may be empty) the Servers
	// page merges with observed runner activity. Read-only; not used for routing.
	asrServers []config.ASRServer

	// prober polls gpu-arbiter for the live readiness of servers that declare a
	// gpuArbiterUrl (TTL-cached). Swapped for a static fake in the demo.
	prober gpuProber

	// cfg is retained so the Models/Services page can read the AI endpoint
	// registry (CONTRACT §2.14). Read-only after construction.
	cfg *config.Config

	// endpointProber probes each AI endpoint's /models for liveness (TTL-cached).
	// Swapped for a static fake in the demo.
	endpointProber endpointProber

	// eval holds the on-demand LLM-as-judge wiring for the "run eval" dashboard
	// actions (CONTRACT §2.15). Nil-judge / unconfigured → the buttons are hidden
	// with an explanation rather than POSTing into a guaranteed failure.
	eval evalState

	// evalInPipeline reflects the EVAL_IN_PIPELINE config value: when true the
	// embed worker runs the judge inline before embedding, so eval coverage is a
	// live signal the lifecycle view can surface honestly.
	evalInPipeline bool

	// metrics is the Prometheus registry mounted at /metrics (CONTRACT §2.16).
	// nil in the demo (no DB-backed scrape source).
	metrics *metrics.Registry
}

// evalState carries the dashboard's on-demand eval-run wiring. configured
// reflects whether ResolveChatClient succeeded at construction (an eval chat
// endpoint resolves); run is the bound runner (nil when unconfigured or in a
// test that doesn't exercise it). inFlight guards against overlapping runs — the
// judge issues real LLM calls, so a second click while one is running is
// rejected rather than doubling cost.
type evalState struct {
	configured bool
	run        evalRunFunc
	inFlight   atomic.Bool
}

// evalRunFunc runs the judge over the selected chunks and persists findings.
// Mirrors eval.Run's signature minus the reader/judge/writer (bound at
// construction). Injectable so handler tests don't need a live LLM.
type evalRunFunc func(ctx context.Context, opts eval.RunOptions) (eval.RunStats, error)

// readOnlyAnnotations is shared by every search/browse/read tool: they only
// query the database; they never mutate state or produce side-effects.
// OpenWorldHint is deliberately left unset (nil), matching the original
// mcp-go tool.ToolAnnotation, which never set it either.
func readOnlyAnnotations() *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &f,
		IdempotentHint:  true,
	}
}

// NewMCPServer creates a new MCP server instance
func NewMCPServer(database DBInterface, cfg *config.Config) *MCPServer {
	logger := log.NewLogger("mcp-server")

	staleAfter := cfg.StaleJobTimeout
	if staleAfter <= 0 {
		staleAfter = 30 * time.Minute
	}

	// Create the MCP server. Capabilities is left NIL deliberately: the SDK
	// auto-derives {"tools":{"listChanged":true}} from the 5 tools registered
	// below and, since no resources/prompts are registered, omits those keys
	// entirely (never a falsy `resources: {}`) — this is exactly what
	// ContextForge's federation gate (`if capabilities.get("resources")`,
	// which treats `{}` as falsy) needs. Hand-setting Capabilities here would
	// only be able to reintroduce that trap, so don't. A nil Capabilities also
	// keeps the SDK's default `{"logging":{}}` capability, the equivalent of
	// mcp-go's WithLogging(). Identity metadata (Title) helps a host present
	// the server in a picker; Instructions carries the long-form description
	// mcp-go exposed via the (non-spec) Implementation.Description field,
	// which the official Implementation struct has no equivalent of.
	// websiteUrl/icons are omitted — the only HTTP surface is the LAN-only
	// status dashboard, not a public site, so there's no canonical URL to
	// advertise.
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "earmark",
		Version: "1.0.0",
		Title:   "Audiobook Processor",
	}, &mcp.ServerOptions{
		Instructions: "Semantic and keyword search over audiobook transcripts, plus full-transcript reading and library browsing.",
	})

	// Build the metadata provider from config. NewPathProvider handles parse
	// errors internally (falls back to generic resolver) — labels are cosmetic,
	// never a startup blocker.
	meta := metaprovider.NewPathProvider(cfg.LibraryCollections, cfg.BooksDir)

	// The tool handlers share the same provider so the `book` argument on the
	// search tools and the list_books labels match the dashboard's labels.
	handlers := NewToolHandlers(database, meta)

	annotations := readOnlyAnnotations()

	// Add semantic search tool
	mcpServer.AddTool(&mcp.Tool{
		Name: "semantic_search_audiobooks",
		Description: "Search audiobook transcriptions by meaning (vector similarity). " +
			"Searches the WHOLE library by default; pass `book` to scope the search to a single title. " +
			"Each hit is a chunk (the embedding/search unit — a chunk is tens of consecutive ASR " +
			"segments grouped together, so one book has far fewer chunks than segments). " +
			"Use this to find passages about a concept; use list_books to discover titles, " +
			"get_chunk_context to expand around a hit, and get_transcript to read full text.",
		Annotations:  annotations,
		InputSchema:  semanticSearchSchema(),
		OutputSchema: outputSchemaFor[SearchResultsOutput](),
	}, handlers.handleSemanticSearch)

	// Add text search tool
	mcpServer.AddTool(&mcp.Tool{
		Name: "text_search_audiobooks",
		Description: "Search audiobook transcriptions by literal/keyword text (trigram match). " +
			"Searches the WHOLE library by default; pass `book` to scope the search to a single title. " +
			"Each hit is a chunk (the search unit — a chunk is tens of consecutive ASR segments grouped " +
			"together). Ranked by trigram match, not vector distance — results carry a \"trigram match\" " +
			"label, NOT a semantic-similarity score. " +
			"Use this for exact phrases or names; use semantic_search_audiobooks for conceptual queries.",
		Annotations:  annotations,
		InputSchema:  textSearchSchema(),
		OutputSchema: outputSchemaFor[SearchResultsOutput](),
	}, handlers.handleTextSearch)

	// Add list_books tool — the library inventory.
	mcpServer.AddTool(&mcp.Tool{
		Name: "list_books",
		Description: "List the audiobook library inventory: each book with author, title, " +
			"track progress (done/total), total duration, word count, and embedded-chunk count. " +
			"This is the inventory tool — use it to discover which titles exist before scoping a " +
			"search or fetching a transcript. Pass format=tree to group the same books under their " +
			"authors instead of a flat list.",
		Annotations:  annotations,
		InputSchema:  listBooksSchema(),
		OutputSchema: outputSchemaFor[ListBooksOutput](),
	}, handlers.handleListBooks)

	// Add get_transcript tool — read the full transcript text (paginated).
	mcpServer.AddTool(&mcp.Tool{
		Name: "get_transcript",
		Description: "Read the full transcript of a book/track as timestamped segments, so you can " +
			"READ the text rather than only search fragments. This paginates SEGMENTS (raw ASR " +
			"timestamp units — there are far more segments than search chunks; a chunk is tens of " +
			"consecutive segments grouped for embedding). Provide `book` (a title) or `trackID` (a job id " +
			"from list_books / a track chooser). Transcripts are large, so segments are paginated via " +
			"offset/limit; the response footer tells you the next offset. If a book has multiple tracks, this " +
			"returns the track list so you can pick one by trackID. Per-word timestamps are HIDDEN by default; " +
			"set includeWordTimestamps=true to get each segment's word-level start/end times (for queries like " +
			"\"exactly when was X said\").",
		Annotations:  annotations,
		InputSchema:  getTranscriptSchema(),
		OutputSchema: outputSchemaFor[TranscriptOutput](),
	}, handlers.handleGetTranscript)

	// Add chunk context tool
	mcpServer.AddTool(&mcp.Tool{
		Name: "get_chunk_context",
		Description: "Get the chunks surrounding a search hit, so you can read the full text around a match. " +
			"Operates on CHUNKS (the search/embedding unit — a chunk is tens of consecutive ASR segments grouped " +
			"together; use get_transcript to page raw segments instead). Pass the chunk's UUID from a search result.",
		Annotations:  annotations,
		InputSchema:  getChunkContextSchema(),
		OutputSchema: outputSchemaFor[SearchResultsOutput](),
	}, handlers.handleGetContext)

	s := &MCPServer{
		server:           mcpServer,
		handlers:         handlers,
		logger:           logger,
		db:               database,
		runnerStaleAfter: staleAfter,
		embedURL:         cfg.EmbeddingsBaseURL,
		meta:             meta,
		controlToken:     cfg.ControlAPIToken,
		asrServers:       cfg.ASRServers,
		cfg:              cfg,
		evalInPipeline:   cfg.EvalInPipeline,
		// 2s timeout keeps a slow/unreachable gpu-arbiter from stalling the page;
		// 5s TTL coalesces the /servers + /api/v1/status probes within one refresh.
		prober: newHTTPGPUProber(2*time.Second, 5*time.Second),
		// AI endpoint /models probe: same timeout/TTL budget as the gpu-arbiter
		// prober so a slow upstream can't stall the Models/Services page.
		endpointProber: newHTTPEndpointProber(2*time.Second, 5*time.Second),
	}
	s.initEval(cfg)
	// Prometheus metrics (CONTRACT §2.16): the scrape-time collector reads the DB
	// for current-state gauges. s.db satisfies metrics.StatsSource.
	s.metrics = metrics.New(s.db, 2*time.Second)
	return s
}

// initEval resolves the LLM-as-judge chat endpoint once at construction and
// binds the on-demand eval runner. When no eval endpoint resolves (no
// AI_ROLES.eval and no EVAL_CHAT_* fallback), eval.configured stays false and
// the "run eval" buttons are hidden — the dashboard never POSTs into a
// guaranteed failure. The runner binds s.db as the read-only ChunkReader and
// insert-only FindingWriter; the judge issues no transcript mutations.
func (s *MCPServer) initEval(cfg *config.Config) {
	chat, err := eval.ResolveChatClient(eval.ConfigSource(cfg))
	if err != nil {
		s.logger.Info("eval layer disabled (no chat endpoint configured)", "reason", err)
		return
	}
	judge := eval.NewJudge(chat)
	db := s.db
	s.eval.configured = true
	s.eval.run = func(ctx context.Context, opts eval.RunOptions) (eval.RunStats, error) {
		_, stats, err := eval.Run(ctx, db, judge, db, opts)
		return stats, err
	}
}

// StartStdio starts the MCP server using stdio transport
func (s *MCPServer) StartStdio() error {
	s.logger.Info("Starting MCP server with stdio transport")

	return s.server.Run(context.Background(), &mcp.StdioTransport{})
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
//	GET  /                     — home: the Library page (full HTML shell)
//	GET  /pipeline             — Pipeline ops page (status fragment + folded-in Failed view)
//	GET  /track                — per-track detail page (shell)
//	GET  /track/data           — per-track detail fragment (header + reader + chunks)
//	GET  /status/data          — htmx-refreshed fragment (counts + recent jobs + controls)
//	GET  /failed/data          — failed-jobs fragment (no standalone page; rendered inside /pipeline)
//	GET  /servers              — transcription-servers page (shell)
//	GET  /servers/data         — servers fragment (status + models/modes)
//	GET  /static/htmx.min.js   — vendored htmx library
//	POST /actions/requeue      — re-transcribe one job (htmx-guarded)
//	POST /actions/retry-failed — re-transcribe all failed jobs (htmx-guarded)
//	POST /actions/run          — arm a bounded run of N claims then auto-pause (htmx + token)
//	POST /actions/run-clear    — clear a bounded run / back to unlimited (htmx + token)
//	POST /actions/eval         — run the LLM judge over one book (htmx + token, async)
//	POST /actions/eval-sample  — run the LLM judge over a library sample (htmx + token, async)
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
	// getServer returns the single prebuilt server for every session — the tool
	// set is process-wide, not per-request.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.server }, nil)

	mux := http.NewServeMux()

	// The home page (/) is now the Library; the Pipeline ops page lives at
	// /pipeline (status fragment + folded-in Failed view). Each page shell loads
	// its data fragment via htmx. GET is enforced with a wrapper rather than a
	// "GET /" method pattern, which would conflict with the method-less "/mcp"
	// route and make ServeMux panic. Only reachable under HTTP transport.
	// "/" is the home/default page → the Pipeline ops page. handlePipelinePage is
	// the catch-all (it 404s unmatched paths). Library moved to "/library".
	mux.HandleFunc("/", getOnly(s.handlePipelinePage))
	mux.HandleFunc("/pipeline", getOnly(s.handlePipelinePage))
	mux.HandleFunc("/status/data", getOnly(s.handleStatusData))
	mux.HandleFunc("/library", getOnly(s.handleLibraryPage))
	mux.HandleFunc("/library/data", getOnly(s.handleLibraryData))
	mux.HandleFunc("/book", getOnly(s.handleBookPage))
	mux.HandleFunc("/book/data", getOnly(s.handleBookData))
	mux.HandleFunc("/track", getOnly(s.handleTrackPage))
	mux.HandleFunc("/track/data", getOnly(s.handleTrackData))
	mux.HandleFunc("/track/segments", getOnly(s.handleTrackSegments))
	mux.HandleFunc("/servers", getOnly(s.handleServersPage))
	mux.HandleFunc("/servers/data", getOnly(s.handleServersData))
	mux.HandleFunc("/findings", getOnly(s.handleFindingsPage))
	mux.HandleFunc("/findings/data", getOnly(s.handleFindingsData))

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
	// Bounded-run controls (Pipeline page): arm a run of N claims then auto-pause,
	// or clear the bound. htmx-guarded + fail-closed on an unset CONTROL_API_TOKEN.
	mux.HandleFunc("POST /actions/run", s.handleRunBudget)
	mux.HandleFunc("POST /actions/run-clear", s.handleClearBudget)
	// On-demand LLM-as-judge runs (CONTRACT §2.15): htmx-guarded, fail-closed on
	// an unset CONTROL_API_TOKEN, async (background goroutine).
	mux.HandleFunc("POST /actions/eval", s.handleEvalBook)
	mux.HandleFunc("POST /actions/eval-sample", s.handleEvalSample)
	// Clear recorded findings (htmx-guarded, fail-closed on an unset
	// CONTROL_API_TOKEN like the eval actions). Deletes only advisory
	// transcript_findings rows, then re-renders the /findings fragment.
	mux.HandleFunc("POST /actions/findings-clear", s.handleFindingsClear)
	// Runner self-update (CONTRACT §2.12): request the runner switch to a target
	// earmark tag (or clear the request). htmx-guarded + fail-closed on an unset
	// CONTROL_API_TOKEN; the runner performs the swap.
	mux.HandleFunc("POST /actions/runner-update", s.handleRunnerUpdate)

	// JSON control API (script/agent-facing) — distinct from the htmx dashboard
	// actions above. Reads are unauthenticated; mutations require the bearer token
	// (requireToken). These specific method+path patterns don't conflict with the
	// "/" catch-all or the method-less "/mcp" handler.
	mux.HandleFunc("GET /api/v1/status", s.handleAPIStatus)
	mux.HandleFunc("GET /api/v1/pipeline/pause", s.handleAPIPauseGet)
	mux.HandleFunc("PUT /api/v1/pipeline/pause", s.requireToken(s.handleAPIPausePut))
	mux.HandleFunc("POST /api/v1/pipeline/run", s.requireToken(s.handleAPIRun))
	mux.HandleFunc("DELETE /api/v1/pipeline/run", s.requireToken(s.handleAPIRunClear))
	mux.HandleFunc("POST /api/v1/runner/update", s.requireToken(s.handleAPIRunnerUpdate))

	// Liveness — no external deps. Both /health (back-compat) and /healthz (the
	// uniform name the ingest pod also exposes) are served so Helm probes can use
	// /healthz on both pods.
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)

	// Prometheus metrics (CONTRACT §2.16). nil in the demo (no DB scrape source).
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics.Handler())
	}

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
	// routing is done internally by the SDK when used as an http.Handler).
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
	return "earmark", "1.0.0"
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
