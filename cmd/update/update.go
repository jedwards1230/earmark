package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/auth"
	"github.com/jedwards1230/earmark/internal/build"
	"github.com/jedwards1230/earmark/internal/version"
	"github.com/spf13/cobra"
)

var UpdateCmd = &cobra.Command{
	Use:                "update",
	Short:              "Update earmark to the latest version",
	DisableFlagParsing: true,
	Long: `Update earmark to the latest version from GitHub.

This command checks for updates and downloads the latest version, either from
GitHub releases (when available) or by rebuilding from source.

Options:
  --force      Force update even if no newer version is available
  --check      Only check for updates, don't perform update
  --yes        Skip confirmation prompts

Examples:
  # Check and update if newer version is available
  earmark update

  # Only check for updates
  earmark update --check

  # Force update without prompts
  earmark update --force --yes`,
	Run: runUpdate,
}

func runUpdate(cmd *cobra.Command, args []string) {
	force := false
	checkOnly := false
	noConfirm := false
	debug := false

	// Parse custom flags from args
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		case "--check":
			checkOnly = true
		case "--yes":
			noConfirm = true
		case "--debug":
			debug = true
		case "--help", "-h":
			fmt.Fprintf(os.Stderr, "Usage: earmark update [options]\n")
			fmt.Fprintf(os.Stderr, "Update earmark to the latest version.\n\n")
			fmt.Fprintf(os.Stderr, "Options:\n")
			fmt.Fprintf(os.Stderr, "  --force      Force update even if no newer version is available\n")
			fmt.Fprintf(os.Stderr, "  --check      Only check for updates, don't perform update\n")
			fmt.Fprintf(os.Stderr, "  --yes        Skip confirmation prompts\n")
			fmt.Fprintf(os.Stderr, "  --debug      Enable debug output\n")
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("Checking for updates...")

	if debug {
		fmt.Printf("DEBUG: Current version info: %+v\n", version.GetInfo())
	}

	// Initialize authentication manager for update checks
	authManager := auth.NewAuthManager(debug)

	// Use authenticated client for version checking
	var result *version.CheckResult
	var err error
	if client := authManager.GetAuthenticatedClient(); client != nil {
		result, err = version.CheckForUpdatesWithAuthenticatedClient(ctx, true, debug, client)
	} else {
		result, err = version.CheckForUpdatesWithDebug(ctx, true, debug)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking for updates: %v\n", err)
		os.Exit(1)
	}

	if debug {
		fmt.Printf("DEBUG: Update check result: %+v\n", result)
	}

	if checkOnly {
		if result.HasUpdate {
			fmt.Printf("🎉 Update available!\n")
			fmt.Printf("%s\n", result.UpdateMessage)
		} else {
			if result.LatestVersion != "" {
				fmt.Printf("✅ No updates available. Latest: %s\n", result.LatestVersion)
			} else if result.LatestCommit != "" {
				fmt.Printf("✅ No updates available. Latest commit: %s\n", result.LatestCommit[:7])
			} else {
				fmt.Printf("✅ No updates available.\n")
				fmt.Printf("💡 For private repositories, check releases manually at:\n")
				fmt.Printf("   https://github.com/%s/releases\n", version.GitHubRepo)
			}
		}
		return
	}

	if !result.HasUpdate && !force {
		if result.LatestVersion != "" {
			fmt.Printf("✅ You are already running the latest version (%s).\n", result.LatestVersion)
		} else if result.LatestCommit != "" {
			fmt.Printf("✅ You are already running the latest commit (%s).\n", result.LatestCommit[:7])
		} else {
			fmt.Println("✅ You are already running the latest version.")
			fmt.Printf("💡 For private repositories, check releases manually at:\n")
			fmt.Printf("   https://github.com/%s/releases\n", version.GitHubRepo)
		}
		return
	}

	if result.HasUpdate {
		fmt.Printf("🎉 Update available!\n")
		fmt.Printf("%s\n", result.UpdateMessage)
	} else if force {
		fmt.Printf("⚠️ Forcing update (no newer version detected)\n")
	}

	shouldProceed := noConfirm
	if !noConfirm && version.Version == "dev" && result.HasUpdate {
		fmt.Printf("\nYou are running a development build (dev).\n")
		fmt.Printf("Do you want to update to the latest release version %s? (y/N): ", result.LatestVersion)

		var response string
		if _, err := fmt.Scanln(&response); err != nil {
			response = "n"
		}
		shouldProceed = strings.ToLower(response) == "y"
		if !shouldProceed {
			fmt.Println("Update cancelled.")
			return
		}
	} else if !noConfirm && version.Version != "dev" && result.HasUpdate {
		shouldProceed = true
	}

	if result.UseReleases && result.LatestVersion != "" && shouldProceed {
		if err := updateFromRelease(ctx, result.LatestVersion, true, debug); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating from release: %v\n", err)
			os.Exit(1)
		}
	} else if shouldProceed {
		if err := updateFromSource(ctx, true, debug); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating from source: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("✅ Update completed successfully!")
	fmt.Println("Run 'earmark version' to see the new version.")
}

func updateFromRelease(ctx context.Context, latestVersion string, noConfirm bool, debug bool) error {
	// Initialize authentication manager
	authManager := auth.NewAuthManager(debug)
	if !noConfirm {
		fmt.Printf("\nThis will update earmark to version %s.\n", latestVersion)
		fmt.Print("Continue? (y/N): ")

		var response string
		if _, err := fmt.Scanln(&response); err != nil {
			// Treat scan error as 'no'
			response = "n"
		}
		if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
			fmt.Println("Update cancelled.")
			return nil
		}
	}

	fmt.Printf("Downloading version %s...\n", latestVersion)

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting current executable path: %w", err)
	}

	// Get authenticated download URL from GitHub API for private repositories
	downloadURL, err := getAssetDownloadURL(ctx, latestVersion, authManager, debug)
	if err != nil {
		return fmt.Errorf("getting asset download URL: %w", err)
	}

	fmt.Printf("Downloading from: %s\n", downloadURL)

	client := authManager.GetAuthenticatedClient()
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("creating download request: %w", err)
	}

	// Add GitHub token authentication for private repositories
	if err := authManager.AddAuthHeader(req); err != nil {
		fmt.Printf("Warning: Failed to add authentication header: %v\n", err)
		fmt.Println("Attempting download without authentication...")
	} else {
		fmt.Println("Using authenticated access for private repository")
	}

	// For GitHub API asset URLs, we need to set the Accept header to get the binary
	if strings.Contains(downloadURL, "api.github.com/repos/") && strings.Contains(downloadURL, "/releases/assets/") {
		req.Header.Set("Accept", "application/octet-stream")
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading binary from %s: %w", downloadURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case 404:
			return fmt.Errorf("download failed with status 404 - URL: %s (release binary not found - likely private repository or binary not published)", downloadURL)
		case 403:
			return fmt.Errorf("download failed with status 403 - URL: %s (access denied - authentication failed or insufficient permissions)\n\nTry one of these authentication methods:\n  1. Set GITHUB_TOKEN environment variable\n  2. Use GitHub CLI: 'gh auth login'\n  3. Configure SSH keys for GitHub", downloadURL)
		case 401:
			return fmt.Errorf("download failed with status 401 - URL: %s (authentication required)\n\nTry one of these authentication methods:\n  1. Set GITHUB_TOKEN environment variable\n  2. Use GitHub CLI: 'gh auth login'\n  3. Configure SSH keys for GitHub", downloadURL)
		default:
			return fmt.Errorf("download failed with status %d - URL: %s", resp.StatusCode, downloadURL)
		}
	}

	tempFile := executable + ".tmp"
	// #nosec G304 - tempFile path is constructed safely
	// #nosec G302 - 0750 permissions are appropriate for executable
	file, err := os.OpenFile(filepath.Clean(tempFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0750)
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}

	_, err = io.Copy(file, resp.Body)
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err != nil {
		if err := os.Remove(tempFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove temp file: %v\n", err)
		}
		return fmt.Errorf("writing downloaded binary: %w", err)
	}

	backupFile := executable + ".backup"
	if err := os.Rename(executable, backupFile); err != nil {
		if err := os.Remove(tempFile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove temp file: %v\n", err)
		}
		return fmt.Errorf("creating backup: %w", err)
	}

	if err := os.Rename(tempFile, executable); err != nil {
		if restoreErr := os.Rename(backupFile, executable); restoreErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore backup: %v\n", restoreErr)
		}
		return fmt.Errorf("installing new binary: %w", err)
	}

	if err := os.Remove(backupFile); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove backup file: %v\n", err)
	}
	return nil
}

func updateFromSource(ctx context.Context, noConfirm bool, debug bool) error {
	if !noConfirm {
		fmt.Print("\nThis will rebuild earmark from the latest source.\nContinue? (y/N): ")

		var response string
		if _, err := fmt.Scanln(&response); err != nil {
			// Treat scan error as 'no'
			response = "n"
		}
		if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
			fmt.Println("Update cancelled.")
			return nil
		}
	}

	fmt.Println("Checking if we're in a git repository...")

	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is not available: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("not in a git repository - cannot update from source")
	}

	fmt.Println("Fetching latest changes...")
	cmd = exec.CommandContext(ctx, "git", "fetch", "origin", "main")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetching latest changes: %w", err)
	}

	fmt.Println("Updating to latest main...")
	cmd = exec.CommandContext(ctx, "git", "reset", "--hard", "origin/main")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("updating to latest main: %w", err)
	}

	fmt.Println("Building new binary...")

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting current executable path: %w", err)
	}

	buildArgs := []string{"build", "-o", executable + ".new"}

	now := time.Now().UTC().Format(time.RFC3339)
	gitCommit := getUpdateGitCommit(ctx)
	goVer := runtime.Version()

	// Get module path dynamically
	modulePath, err := build.GetModulePath()
	if err != nil {
		return fmt.Errorf("getting module path: %w", err)
	}

	ldflags := fmt.Sprintf("-X '%s/internal/version.Version=dev' -X '%s/internal/version.Commit=%s' -X '%s/internal/version.BuildTime=%s' -X '%s/internal/version.GoVersion=%s'",
		modulePath, modulePath, gitCommit, modulePath, now, modulePath, goVer)

	buildArgs = append(buildArgs, "-ldflags", ldflags, ".")

	// #nosec G204 - go command is trusted and buildArgs are constructed safely
	cmd = exec.CommandContext(ctx, "go", buildArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if err := os.Remove(executable + ".new"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove new binary: %v\n", err)
		}
		return fmt.Errorf("building new binary: %w", err)
	}

	backupFile := executable + ".backup"
	if err := os.Rename(executable, backupFile); err != nil {
		if err := os.Remove(executable + ".new"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove new binary: %v\n", err)
		}
		return fmt.Errorf("creating backup: %w", err)
	}

	if err := os.Rename(executable+".new", executable); err != nil {
		if restoreErr := os.Rename(backupFile, executable); restoreErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore backup: %v\n", restoreErr)
		}
		return fmt.Errorf("installing new binary: %w", err)
	}

	if err := os.Remove(backupFile); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove backup file: %v\n", err)
	}
	return nil
}

func getUpdateGitCommit(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

// GitHubAsset represents a GitHub release asset
type GitHubAsset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
}

// GitHubReleaseWithAssets represents a GitHub release with assets
type GitHubReleaseWithAssets struct {
	TagName string        `json:"tag_name"`
	Assets  []GitHubAsset `json:"assets"`
}

// getAssetDownloadURL gets the authenticated download URL for a release asset
func getAssetDownloadURL(ctx context.Context, releaseVersion string, authManager *auth.AuthManager, debug bool) (string, error) {
	// Always try GitHub API first (works for both public and private repos)
	if url, err := getAssetDownloadURLFromAPI(ctx, releaseVersion, authManager); err == nil {
		return url, nil
	} else {
		if debug {
			fmt.Printf("DEBUG: GitHub API failed: %v\n", err)
		}
	}

	// Fallback to direct URL construction for public repositories
	assetName := fmt.Sprintf("earmark-%s-%s", runtime.GOOS, runtime.GOARCH)
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", version.GitHubRepo, releaseVersion, assetName), nil
}

// getAssetDownloadURLFromAPI gets the asset download URL using GitHub API
func getAssetDownloadURLFromAPI(ctx context.Context, releaseVersion string, authManager *auth.AuthManager) (string, error) {
	// Get release info from GitHub API
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", version.GitHubRepo, releaseVersion)

	client := authManager.GetAuthenticatedClient()
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating API request: %w", err)
	}

	// Add authentication header
	if err := authManager.AddAuthHeader(req); err != nil {
		return "", fmt.Errorf("adding authentication header: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling GitHub API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release GitHubReleaseWithAssets
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("decoding GitHub API response: %w", err)
	}

	// Find the asset for the current platform
	assetName := fmt.Sprintf("earmark-%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			return asset.URL, nil
		}
	}

	return "", fmt.Errorf("asset %s not found in release %s", assetName, releaseVersion)
}
