package main

import (
	"fmt"
	"log"
	"os"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/mcp"
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
	fmt.Println("Four tools available for AI assistant integration.")
	fmt.Println("See internal/mcp/README.md for detailed tool documentation.")
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
