package cmd

import (
	"context"
	"log"

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

func GetRootCmd() *cobra.Command {
	return rootCmd
}