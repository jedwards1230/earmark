package mcp

import (
	"fmt"
	"log"
	"os"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/mcp"
	"github.com/spf13/cobra"
)

var MCPCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the Model Context Protocol (MCP) server",
	Long: `Start the MCP server for lilbro-whisper audiobook transcription service.

The MCP server provides tools for AI assistant integration with audiobook search and browsing capabilities.

Environment Variables:
  MCP_TRANSPORT: Transport type - "stdio" (default) or "http"
  MCP_HTTP_ADDR: HTTP server address (default ":8081")

Examples:
  # Start with stdio transport (default)
  lil-whisper mcp

  # Start with HTTP transport
  MCP_TRANSPORT=http lil-whisper mcp

  # Start with custom HTTP address
  MCP_TRANSPORT=http MCP_HTTP_ADDR=:9000 lil-whisper mcp`,
	Run: runMCP,
}

func runMCP(cmd *cobra.Command, args []string) {
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize database connection
	database, err := db.New(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.Close()

	// Determine transport from env; default to stdio.
	// Do NOT set the env var globally — read it once and pass it through.
	transport := os.Getenv("MCP_TRANSPORT")
	if transport == "" {
		transport = "stdio"
	}

	// Startup diagnostics: write to stderr for stdio transport so we don't
	// corrupt the JSON-RPC framing on stdout; write to stdout for http.
	diag := func(format string, a ...interface{}) {
		if transport == "stdio" {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		} else {
			fmt.Printf(format+"\n", a...)
		}
	}

	diag("Starting MCP server for lilbro-whisper...")
	diag("Available tools:")
	diag("  - semantic_search_audiobooks: Search using semantic similarity")
	diag("  - text_search_audiobooks: Search using full-text search")
	diag("  - browse_audiobook_library: Browse library structure")
	diag("")
	diag("Transport: %s", transport)
	if transport == "http" {
		addr := cfg.MCPHTTPAddr
		if addr == "" {
			addr = ":8081"
		}
		diag("HTTP Address: %s", addr)
	}
	diag("")

	// Start the MCP service
	if err := mcp.StartMCPService(database, cfg); err != nil {
		log.Fatalf("Failed to start MCP service: %v", err)
	}
}
