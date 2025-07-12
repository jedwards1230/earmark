package cmd

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

const (
	treeVertical = "│"
	treeBranch   = "├──"
	treeLeaf     = "└──"
)

var rootCmd = &cobra.Command{
	Use:   "lil-whisper",
	Short: "A transcription service using Yap and MacOS native APIs",
}

var monitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Start the file monitoring and transcription service",
	Long: `Start the file monitoring service that watches for new audio files,
transcribes them using Yap, and stores the results in the database.

This service handles:
- File system monitoring for new audio files
- Audio transcription using Yap (Apple's speech recognition)
- Database updates with transcription results
- Background processing queue management

The monitor service does NOT start the HTTP server. Use the 'serve' command
to start the HTTP API server separately.`,
	Run: func(cmd *cobra.Command, args []string) {
		runSubCommand("monitor", args)
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP API server",
	Long: `Start the HTTP API server that provides search endpoints for the
transcribed audiobook content.

This service handles:
- HTTP API endpoints for search functionality
- Vector similarity search using OpenAI embeddings
- Full-text search across transcriptions
- RESTful API responses

The serve command does NOT start file monitoring or transcription. Use the
'monitor' command to start the file monitoring and transcription service.`,
	Run: func(cmd *cobra.Command, args []string) {
		runSubCommand("serve", args)
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List content from the database",
	Run: func(cmd *cobra.Command, args []string) {
		runSubCommand("list", args)
	},
}

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search for content in the database",
	Long:  `Search for content in the database. By default, uses semantic vector similarity search.`,
	Run: func(cmd *cobra.Command, args []string) {
		runSubCommand("search", args)
	},
}

var mcpCmd = &cobra.Command{
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
	Run: func(cmd *cobra.Command, args []string) {
		runSubCommand("mcp", args)
	},
}

func runSubCommand(subCmd string, args []string) {
	// Build the path to the sub-command binary
	binaryPath := filepath.Join("cmd", subCmd, "main")
	
	// Check if the binary exists, if not try to build it
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		// Try to build the binary
		buildCmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/"+subCmd)
		if err := buildCmd.Run(); err != nil {
			log.Fatalf("Failed to build %s command: %v", subCmd, err)
		}
	}
	
	// Execute the sub-command
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	
	if err := cmd.Run(); err != nil {
		log.Fatalf("Error running %s command: %v", subCmd, err)
	}
}

func Run() {
	rootCmd.AddCommand(monitorCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(mcpCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Printf("Error executing command: %v", err)
		os.Exit(1)
	}
}
