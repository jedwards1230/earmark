package yap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// getYapPath returns the path to the embedded yap binary
func getYapPath() (string, error) {
	embeddedYapOnce.Do(func() {
		embeddedYapPath, embeddedYapErr = extractEmbeddedYap()
	})
	
	return embeddedYapPath, embeddedYapErr
}

// extractEmbeddedYap extracts the embedded yap binary to a temporary location
func extractEmbeddedYap() (string, error) {
	if len(yapBinary) == 0 {
		return "", fmt.Errorf("embedded yap binary not available - run 'make download-yap' to embed the binary")
	}

	// Create application-specific temp directory
	tempDir, err := os.MkdirTemp("", "lil-whisper-yap-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Extract binary to temp directory
	yapPath := filepath.Join(tempDir, "yap")
	if err := os.WriteFile(yapPath, yapBinary, 0755); err != nil {
		os.RemoveAll(tempDir) // Clean up on error
		return "", fmt.Errorf("failed to extract yap binary: %w", err)
	}

	// Verify the extracted binary is executable
	if runtime.GOOS == "darwin" {
		if err := verifyYapBinary(yapPath); err != nil {
			os.RemoveAll(tempDir) // Clean up on error
			return "", fmt.Errorf("embedded yap binary verification failed: %w", err)
		}
	}

	// Register cleanup function to remove temp directory on exit
	// Note: This is best-effort cleanup; temp files will be cleaned by OS eventually
	runtime.SetFinalizer(&yapPath, func(*string) {
		os.RemoveAll(tempDir)
	})

	return yapPath, nil
}

// verifyYapBinary performs basic verification of the yap binary
func verifyYapBinary(path string) error {
	// Check if file exists and is executable
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("binary not accessible: %w", err)
	}

	if info.Mode()&0111 == 0 {
		return fmt.Errorf("binary is not executable")
	}

	// Try to run yap help to verify it's working (yap doesn't have --version)
	cmd := exec.Command(path, "--help")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binary verification failed (yap --help): %w", err)
	}

	return nil
}

// GetVersion returns the version of the embedded yap binary
func GetVersion() string {
	if yapVersion == "" {
		return "unknown"
	}
	return strings.TrimSpace(yapVersion)
}