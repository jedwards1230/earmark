package mcp

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
)

// MCPServer wraps the MCP server functionality for the lilbro-whisper service
type MCPServer struct {
	server   *server.MCPServer
	handlers *ToolHandlers
	logger   log.Logger
}

// NewMCPServer creates a new MCP server instance
func NewMCPServer(database DBInterface, cfg *config.Config) *MCPServer {
	logger := log.NewLogger("mcp-server")

	// Create MCP server with all capabilities enabled
	mcpServer := server.NewMCPServer("lilbro-whisper", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, false), // Not implementing resources yet
		server.WithPromptCapabilities(false),          // Not implementing prompts yet
		server.WithLogging(),
	)

	handlers := NewToolHandlers(database)

	// Add semantic search tool
	mcpServer.AddTool(mcp.NewTool("semantic_search_audiobooks",
		mcp.WithDescription("Search audiobook transcriptions using semantic similarity"),
		mcp.WithString("query",
			mcp.Description("The search query to find relevant content"),
			mcp.Required(),
		),
		mcp.WithNumber("threshold",
			mcp.Description("Similarity threshold (0.0-1.0, default: 0.3)"),
			mcp.DefaultNumber(0.3),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10)"),
			mcp.DefaultNumber(10),
		),
	), handlers.handleSemanticSearch)

	// Add text search tool
	mcpServer.AddTool(mcp.NewTool("text_search_audiobooks",
		mcp.WithDescription("Search audiobook transcriptions using full-text search"),
		mcp.WithString("query",
			mcp.Description("The search query to find exact text matches"),
			mcp.Required(),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10)"),
			mcp.DefaultNumber(10),
		),
	), handlers.handleTextSearch)

	// Add browse library tool
	mcpServer.AddTool(mcp.NewTool("browse_audiobook_library",
		mcp.WithDescription("Browse the audiobook library structure and metadata"),
		mcp.WithString("author",
			mcp.Description("Filter by author name (case-insensitive partial match)"),
		),
		mcp.WithString("book",
			mcp.Description("Filter by book title (case-insensitive partial match)"),
		),
	), handlers.handleBrowseLibrary)

	// Add chunk context tool
	mcpServer.AddTool(mcp.NewTool("get_chunk_context",
		mcp.WithDescription("Get surrounding chunks for better context around a specific chunk"),
		mcp.WithString("chunkID",
			mcp.Description("The unique chunk ID (format: author_book_chapter_chunk)"),
			mcp.Required(),
		),
		mcp.WithNumber("contextWindow",
			mcp.Description("Number of chunks before and after to include (default: 2)"),
			mcp.DefaultNumber(2),
		),
	), handlers.handleGetContext)

	return &MCPServer{
		server:   mcpServer,
		handlers: handlers,
		logger:   logger,
	}
}

// StartStdio starts the MCP server using stdio transport
func (s *MCPServer) StartStdio() error {
	s.logger.Info("Starting MCP server with stdio transport")

	return server.ServeStdio(s.server)
}

// StartHTTP starts the MCP server using HTTP transport on the specified address
func (s *MCPServer) StartHTTP(addr string) error {
	s.logger.Info("Starting MCP server with HTTP transport", "address", addr)

	httpServer := server.NewStreamableHTTPServer(s.server)
	return httpServer.Start(addr)
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
