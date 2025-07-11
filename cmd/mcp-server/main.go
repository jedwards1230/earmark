package main

import (
	"fmt"
	"log"
	"os"

	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/mcp"
)

func main() {
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

	// Start the MCP service
	fmt.Println("Starting MCP server for lilbro-whisper...")
	fmt.Println("Available tools:")
	fmt.Println("  - semantic_search_audiobooks: Search using semantic similarity")
	fmt.Println("  - text_search_audiobooks: Search using full-text search")
	fmt.Println("  - browse_audiobook_library: Browse library structure")
	fmt.Println("")
	fmt.Println("Transport:", os.Getenv("MCP_TRANSPORT"))
	if os.Getenv("MCP_TRANSPORT") == "http" {
		fmt.Println("HTTP Address:", os.Getenv("MCP_HTTP_ADDR"))
	}
	fmt.Println("")

	if err := mcp.StartMCPService(database, cfg); err != nil {
		log.Fatalf("Failed to start MCP service: %v", err)
	}
}
