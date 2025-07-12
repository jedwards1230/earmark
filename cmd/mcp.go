package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/mcp"
	"github.com/spf13/cobra"
)

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
