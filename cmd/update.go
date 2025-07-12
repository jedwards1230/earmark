package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/build"
	"github.com/jedwards1230/lil-whisper/internal/version"
	"github.com/spf13/cobra"
)

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
			fmt.Fprintf(os.Stderr, "Usage: lil-whisper update [options]\n")
			fmt.Fprintf(os.Stderr, "Update lil-whisper to the latest version.\n\n")
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

	result, err := version.CheckForUpdatesWithDebug(ctx, true, debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking for updates: %v\n", err)
		os.Exit(1)
	}

	if debug {
		fmt.Printf("DEBUG: Update check result: %+v\n", result)
	}

	if !result.HasUpdate && !force {
		fmt.Println("✅ You are already running the latest version.")
		return
	}

	if checkOnly {
		if result.HasUpdate {
			fmt.Printf("🎉 Update available!\n")
			fmt.Printf("%s\n", result.UpdateMessage)
		} else {
			fmt.Printf("✅ No updates available.\n")
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
		if err := updateFromRelease(ctx, result.LatestVersion, true); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating from release: %v\n", err)
			os.Exit(1)
		}
	} else if shouldProceed {
		if err := updateFromSource(ctx, true); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating from source: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("✅ Update completed successfully!")
	fmt.Println("Run 'lil-whisper version' to see the new version.")
}

func updateFromRelease(ctx context.Context, latestVersion string, noConfirm bool) error {
	if !noConfirm {
		fmt.Printf("\nThis will update lil-whisper to version %s.\n", latestVersion)
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

	downloadURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/lil-whisper-%s-%s",
		version.GitHubRepo, latestVersion, runtime.GOOS, runtime.GOARCH)

	client := &http.Client{Timeout: 2 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("creating download request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
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

func updateFromSource(ctx context.Context, noConfirm bool) error {
	if !noConfirm {
		fmt.Print("\nThis will rebuild lil-whisper from the latest source.\nContinue? (y/N): ")

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
