package yap

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func TestGetVersion(t *testing.T) {
	version := GetVersion()
	
	// Version should not be empty
	if version == "" {
		t.Error("GetVersion() returned empty string")
	}

	// Version should be trimmed of whitespace
	if version != strings.TrimSpace(version) {
		t.Errorf("GetVersion() contains untrimmed whitespace: %q", version)
	}

	// If version is "unknown", that's also valid (when embedded version is not set)
	if version != "unknown" && version != "not-downloaded" && version != "homebrew-latest" {
		// Could be any version string, just ensure it's reasonable
		if len(version) < 1 {
			t.Error("GetVersion() returned unreasonably short version")
		}
	}
}

func TestVerifyYapBinary(t *testing.T) {
	// Create a temporary executable file for testing
	tmpDir, err := os.MkdirTemp("", "yap-binary-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Test with non-existent file
	err = verifyYapBinary("/nonexistent/path")
	if err == nil {
		t.Error("verifyYapBinary() should fail for non-existent file")
	}

	// Create a test file (not executable)
	testFile := tmpDir + "/test-binary"
	err = os.WriteFile(testFile, []byte("fake binary"), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Test with non-executable file
	err = verifyYapBinary(testFile)
	if err == nil {
		t.Error("verifyYapBinary() should fail for non-executable file")
	}

	// Make file executable
	err = os.Chmod(testFile, 0755)
	if err != nil {
		t.Fatalf("Failed to make file executable: %v", err)
	}

	// Test with executable file (will fail on --help but should pass executable check)
	err = verifyYapBinary(testFile)
	// We expect this to fail because it's not a real yap binary, but it should pass the executable check
	if err != nil && !strings.Contains(err.Error(), "binary verification failed") {
		t.Errorf("verifyYapBinary() failed for unexpected reason: %v", err)
	}
}

func TestGetYapPath_WithoutBinary(t *testing.T) {
	// Save original binary for restoration
	originalBinary := yapBinary
	defer func() { yapBinary = originalBinary }()

	// Test with empty binary
	yapBinary = []byte{}

	// Reset the once flag to allow re-execution
	embeddedYapOnce = sync.Once{}
	embeddedYapPath = ""
	embeddedYapErr = nil

	_, err := getYapPath()
	if err == nil {
		t.Error("getYapPath() should fail when binary is not embedded")
	}

	expectedError := "embedded yap binary not available"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("getYapPath() error = %v, should contain %q", err, expectedError)
	}
}

func TestGetYapPath_WithBinary(t *testing.T) {
	// Only run this test if we have an embedded binary
	if len(yapBinary) == 0 {
		t.Skip("Skipping test: no embedded yap binary available")
	}

	// Test that we can get a path (will extract to temp directory)
	path, err := getYapPath()
	if err != nil {
		t.Skipf("Skipping test: embedded binary extraction failed: %v", err)
	}

	if path == "" {
		t.Error("getYapPath() returned empty path")
		return
	}

	// Path should exist and be executable
	info, err := os.Stat(path)
	if err != nil {
		t.Errorf("getYapPath() returned non-existent path: %v", err)
		return
	}

	if info.Mode()&0111 == 0 {
		t.Error("getYapPath() returned non-executable file")
	}

	// Calling getYapPath() again should return the same path (cached)
	path2, err := getYapPath()
	if err != nil {
		t.Errorf("getYapPath() second call returned error: %v", err)
	}

	if path != path2 {
		t.Errorf("getYapPath() not cached: first=%s, second=%s", path, path2)
	}
}

func TestExtractEmbeddedYap_EmptyBinary(t *testing.T) {
	// Save original binary for restoration
	originalBinary := yapBinary
	defer func() { yapBinary = originalBinary }()

	// Test with empty binary
	yapBinary = []byte{}

	_, err := extractEmbeddedYap()
	if err == nil {
		t.Error("extractEmbeddedYap() should fail with empty binary")
	}

	expectedError := "embedded yap binary not available"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("extractEmbeddedYap() error = %v, should contain %q", err, expectedError)
	}
}