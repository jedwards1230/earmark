package version

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jedwards1230/earmark/internal/version"
	"github.com/spf13/cobra"
)

var VersionCmd = &cobra.Command{
	Use:                "version",
	Short:              "Show version information",
	DisableFlagParsing: true,
	Long: `Show version information for earmark including version, commit hash, 
build time, and Go version.

Options:
  --check     Check for available updates from GitHub
  --no-cache  Skip cache and force fresh update check

Examples:
  # Show version information
  earmark version

  # Check for updates
  earmark version --check

  # Force fresh update check
  earmark version --check --no-cache`,
	Run: runVersion,
}

func runVersion(cmd *cobra.Command, args []string) {
	checkUpdate := false
	noCache := false

	// Parse custom flags from args
	for _, arg := range args {
		switch arg {
		case "--check":
			checkUpdate = true
		case "--no-cache":
			noCache = true
		case "--help", "-h":
			fmt.Fprintf(os.Stderr, "Usage: earmark version [options]\n")
			fmt.Fprintf(os.Stderr, "Show version information for earmark.\n\n")
			fmt.Fprintf(os.Stderr, "Options:\n")
			fmt.Fprintf(os.Stderr, "  --check     Check for available updates\n")
			fmt.Fprintf(os.Stderr, "  --no-cache  Skip cache and force fresh update check\n")
			return
		}
	}

	info := version.GetInfo()
	fmt.Print(info.String())

	if checkUpdate {
		fmt.Println("Checking for updates...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := version.CheckForUpdates(ctx, !noCache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error checking for updates: %v\n", err)
			os.Exit(1)
		}

		if result.HasUpdate {
			fmt.Printf("\n🎉 Update available!\n")
			fmt.Printf("%s\n", result.UpdateMessage)

			if result.UseReleases && result.LatestVersion != "" {
				fmt.Printf("Latest version: %s\n", result.LatestVersion)
			} else if result.LatestCommit != "" {
				fmt.Printf("Latest commit: %s\n", result.LatestCommit[:7])
			}

			fmt.Printf("\nRun 'earmark update' to update.\n")
		} else {
			fmt.Printf("\n✅ You are running the latest version.\n")
		}

		if !noCache {
			fmt.Printf("(Last checked: %s)\n", result.CheckedAt.Format("2006-01-02 15:04:05"))
		}
	}
}
