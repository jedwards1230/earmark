package cmd

import (
	"context"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/version"
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
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Skip version check for version and update commands to avoid recursion
		if cmd.Name() == "version" || cmd.Name() == "update" {
			return
		}

		// Check for updates in background (non-blocking)
		go checkForUpdatesBackground()
	},
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
	Run: runMonitor,
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
	Run: runServe,
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List content from the database",
	Run:   runList,
}

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search for content in the database",
	Long:  `Search for content in the database. By default, uses semantic vector similarity search.`,
	Run:   runSearch,
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
	Run: runMCP,
}

var versionCmd = &cobra.Command{
	Use:                "version",
	Short:              "Show version information",
	DisableFlagParsing: true,
	Long: `Show version information for lil-whisper including version, commit hash, 
build time, and Go version.

Options:
  --check     Check for available updates from GitHub
  --no-cache  Skip cache and force fresh update check

Examples:
  # Show version information
  lil-whisper version

  # Check for updates
  lil-whisper version --check

  # Force fresh update check
  lil-whisper version --check --no-cache`,
	Run: runVersion,
}

var updateCmd = &cobra.Command{
	Use:                "update",
	Short:              "Update lil-whisper to the latest version",
	DisableFlagParsing: true,
	Long: `Update lil-whisper to the latest version from GitHub.

This command checks for updates and downloads the latest version, either from
GitHub releases (when available) or by rebuilding from source.

Options:
  --force      Force update even if no newer version is available
  --check      Only check for updates, don't perform update
  --yes        Skip confirmation prompts

Examples:
  # Check and update if newer version is available
  lil-whisper update

  # Only check for updates
  lil-whisper update --check

  # Force update without prompts
  lil-whisper update --force --yes`,
	Run: runUpdate,
}

func init() {
	// Add flags to search command
	searchCmd.Flags().IntVarP(&searchLimit, "limit", "l", 10, "maximum number of results to return")
	searchCmd.Flags().BoolVarP(&textMatch, "text", "t", false, "use text-based search instead of semantic search")
	searchCmd.Flags().Float64VarP(&searchThreshold, "precision", "p", 0.3, "minimum similarity threshold (0..1)")
}

func checkForUpdatesBackground() {
	// Load configuration to check version check settings
	cfg, err := config.LoadConfig()
	if err != nil {
		// Silently fail if we can't load config
		return
	}

	// Check if version checking is disabled
	if cfg.DisableVersionCheck {
		return
	}

	// Create a context with configured timeout for non-intrusive checking
	ctx, cancel := context.WithTimeout(context.Background(), cfg.VersionCheckTimeout)
	defer cancel()

	// Check for updates using cache with configured interval
	result, err := version.CheckForUpdatesWithExpiry(ctx, true, cfg.VersionCheckInterval)
	if err != nil {
		// Silently fail - don't interrupt user's workflow
		return
	}

	// Only show notification if there's an update available
	if result.HasUpdate {
		showUpdateNotification(result)
	}
}

func showUpdateNotification(result *version.CheckResult) {
	// Show a subtle notification about available updates
	if result.UseReleases && result.LatestVersion != "" {
		log.Printf("💡 New version %s is available! Run 'lil-whisper update' to upgrade.", result.LatestVersion)
	} else if result.LatestCommit != "" {
		log.Printf("💡 Newer version available (commit %s)! Run 'lil-whisper update' to upgrade.", result.LatestCommit[:7])
	}
}

func getGitCommit() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func getBuildTime() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func getGoVersion() string {
	return runtime.Version()
}

func Run() {
	rootCmd.AddCommand(monitorCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(updateCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Printf("Error executing command: %v", err)
		os.Exit(1)
	}
}
