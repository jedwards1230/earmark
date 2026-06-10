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
  MCP_TRANSPORT=http MCP_HTTP_ADDR=:9000 lil-whisper mcp

  # Serve only the status dashboard with synthetic data and NO database
  # (for local UI work / visual verification — see CLAUDE.md)
  lil-whisper mcp --demo`,
	Run: runMCP,
}

var demoMode bool

func init() {
	MCPCmd.Flags().BoolVar(&demoMode, "demo", false,
		"Serve the status dashboard with synthetic data and no database connection")
}

func runMCP(cmd *cobra.Command, args []string) {
	// Demo mode: serve the dashboard over HTTP with synthetic data, no DB.
	// Useful for iterating on the UI and for AI-agent visual verification.
	if demoMode {
		addr := os.Getenv("MCP_HTTP_ADDR")
		if addr == "" {
			addr = ":8081"
		}
		fmt.Printf("Starting lilbro-whisper DEMO dashboard on %s (synthetic data, no database)\n", addr)
		fmt.Printf("  Dashboard: http://localhost%s/\n", addr)
		if err := mcp.StartDemoDashboard(addr); err != nil {
			log.Fatalf("Failed to start demo dashboard: %v", err)
		}
		return
	}

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
	diag("  - text_search_audiobooks: Search using trigram keyword match")
	diag("  - list_books: Library inventory (flat or author tree)")
	diag("  - get_transcript: Read a track's full transcript (paginated segments)")
	diag("  - get_chunk_context: Surrounding chunks around a search hit")
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
