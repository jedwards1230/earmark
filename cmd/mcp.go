package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/mcp"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the Model Context Protocol (MCP) server",
	Long: `Start the MCP server for lilbro-whisper audiobook transcription service.

The MCP server provides four tools for AI assistant integration with audiobook search and browsing capabilities.

*See internal/mcp/README.md for detailed tool documentation.*

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
	Run: func(cmd *cobra.Command, args []string) {
		runMCPService()
	},
}

func runMCPService() {
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

	// Set environment variable for MCP transport (defaults to stdio)
	if os.Getenv("MCP_TRANSPORT") == "" {
		os.Setenv("MCP_TRANSPORT", "stdio")
	}

	// Print startup information
	fmt.Println("Starting MCP server for lilbro-whisper...")
	fmt.Println("Available tools:")
	fmt.Println("  - semantic_search_audiobooks: Search using semantic similarity")
	fmt.Println("  - text_search_audiobooks: Search using full-text search")
	fmt.Println("  - browse_audiobook_library: Browse library structure")
	fmt.Println("")
	fmt.Println("Transport:", os.Getenv("MCP_TRANSPORT"))
	if os.Getenv("MCP_TRANSPORT") == "http" {
		addr := os.Getenv("MCP_HTTP_ADDR")
		if addr == "" {
			addr = ":8081"
		}
		fmt.Println("HTTP Address:", addr)
	}
	fmt.Println("")

	// Start the MCP service
	if err := mcp.StartMCPService(database, cfg); err != nil {
		log.Fatalf("Failed to start MCP service: %v", err)
	}
}
