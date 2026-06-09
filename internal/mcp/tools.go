package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/log"
)

// DBInterface defines the database operations needed by MCP tools
type DBInterface interface {
	Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error)
	TextSearch(ctx context.Context, query string, limit int) ([]db.SearchResultWithMetadata, error)
	GetHierarchicalData(ctx context.Context) ([]db.HierarchicalEntry, error)
	GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]db.SearchResultWithMetadata, error)
	// Ping checks that the database is reachable; used by /readyz.
	Ping(ctx context.Context) error
	// GetServiceStatus returns aggregate queue/transcript/chunk counts for the
	// status dashboard.
	GetServiceStatus(ctx context.Context) (*db.QueueStats, error)
	// GetRecentJobs returns the most-recently-updated jobs for the dashboard
	// activity table.
	GetRecentJobs(ctx context.Context, limit int) ([]db.RecentJob, error)
	// RequeueByID re-transcribes a single job by UUID (dashboard requeue button).
	RequeueByID(ctx context.Context, id string) (string, error)
	// RequeueFailed re-transcribes all failed jobs (dashboard "retry all" button).
	RequeueFailed(ctx context.Context) ([]string, error)
	// SetPaused writes the global runner pause flag (dashboard pause/resume).
	SetPaused(ctx context.Context, paused bool, by string) error
	// GetBookSummaries returns one row per book directory (the library view),
	// plus the total matching-book count for pagination.
	GetBookSummaries(ctx context.Context, f db.BookFilter) ([]db.BookSummary, int, error)
	// GetBookTracks returns the individual track jobs for one book directory.
	GetBookTracks(ctx context.Context, dir string) ([]db.RecentJob, error)
	// RequeueByDir re-transcribes every track in one book directory (book page).
	RequeueByDir(ctx context.Context, dir string) ([]string, error)
}

// ToolHandlers contains the database interface and logger for MCP tools
type ToolHandlers struct {
	db     DBInterface
	logger log.Logger
}

// NewToolHandlers creates a new ToolHandlers instance
func NewToolHandlers(database DBInterface) *ToolHandlers {
	return &ToolHandlers{
		db:     database,
		logger: log.NewLogger("mcp-tools"),
	}
}

// handleSemanticSearch performs semantic search on audiobook transcriptions
func (h *ToolHandlers) handleSemanticSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract required query parameter
	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Missing or invalid query parameter: %v", err)), nil
	}

	// Extract optional parameters with defaults
	threshold := request.GetFloat("threshold", 0.3)
	limit := request.GetInt("limit", 10)

	h.logger.Info("Performing semantic search", "query", query, "threshold", threshold, "limit", limit)

	// Perform semantic search
	results, err := h.db.Search(ctx, query, limit, threshold)
	if err != nil {
		h.logger.Error("Semantic search failed", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Search failed: %v", err)), nil
	}

	// Format results using the shared formatting function
	return formatSearchResults(results), nil
}

// handleTextSearch performs full-text search on audiobook transcriptions
func (h *ToolHandlers) handleTextSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract required query parameter
	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Missing or invalid query parameter: %v", err)), nil
	}

	// Extract optional limit parameter with default
	limit := request.GetInt("limit", 10)

	h.logger.Info("Performing text search", "query", query, "limit", limit)

	// Perform text search
	results, err := h.db.TextSearch(ctx, query, limit)
	if err != nil {
		h.logger.Error("Text search failed", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Search failed: %v", err)), nil
	}

	// Format results using the shared formatting function
	return formatSearchResults(results), nil
}

// handleBrowseLibrary browses the audiobook library structure
func (h *ToolHandlers) handleBrowseLibrary(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract optional filter parameters
	authorFilter := request.GetString("author", "")
	bookFilter := request.GetString("book", "")

	h.logger.Info("Browsing audiobook library", "author", authorFilter, "book", bookFilter)

	// Get hierarchical library data
	data, err := h.db.GetHierarchicalData(ctx)
	if err != nil {
		h.logger.Error("Failed to get library data", "error", err)
		return mcp.NewToolResultError(fmt.Sprintf("Failed to browse library: %v", err)), nil
	}

	// Apply filter if provided
	var filteredData []db.HierarchicalEntry
	if authorFilter != "" || bookFilter != "" {
		filteredData = filterHierarchicalData(data, authorFilter, bookFilter)
	} else {
		filteredData = data
	}

	// Format results using the shared formatting function
	return formatHierarchicalData(filteredData), nil
}

// handleGetContext retrieves surrounding chunks for better context
func (h *ToolHandlers) handleGetContext(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract required chunk ID parameter
	chunkID, err := request.RequireString("chunkID")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Missing or invalid chunkID parameter: %v", err)), nil
	}

	// Extract optional context window size (default 2 chunks before and after)
	contextWindow := request.GetInt("contextWindow", 2)

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
