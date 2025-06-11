package db

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"transcriber/internal/config"
)

func TestComputeFileChecksum(t *testing.T) {
	// Create a temporary file with known content
	tmpFile, err := os.CreateTemp("", "test_checksum_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	testContent := "Hello, World! This is test content."
	if _, err := tmpFile.WriteString(testContent); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Create a DB instance (we don't need a real connection for this test)
	db := &DB{}

	// Compute checksum
	checksum, err := db.ComputeFileChecksum(tmpFile.Name())
	if err != nil {
		t.Fatalf("ComputeFileChecksum failed: %v", err)
	}

	// Verify checksum is not empty
	if checksum == "" {
		t.Error("Expected non-empty checksum")
	}

	// Verify checksum is consistent
	checksum2, err := db.ComputeFileChecksum(tmpFile.Name())
	if err != nil {
		t.Fatalf("ComputeFileChecksum failed on second call: %v", err)
	}

	if checksum != checksum2 {
		t.Errorf("Expected consistent checksums, got %s and %s", checksum, checksum2)
	}

	// Verify checksum matches expected SHA256
	hasher := sha256.New()
	file, err := os.Open(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to open temp file: %v", err)
	}
	defer file.Close()

	if _, err := io.Copy(hasher, file); err != nil {
		t.Fatalf("Failed to compute expected checksum: %v", err)
	}

	expectedChecksum := fmt.Sprintf("%x", hasher.Sum(nil))
	if checksum != expectedChecksum {
		t.Errorf("Expected checksum %s, got %s", expectedChecksum, checksum)
	}
}

func TestComputeFileChecksum_NonExistentFile(t *testing.T) {
	db := &DB{}

	_, err := db.ComputeFileChecksum("/path/to/nonexistent/file.txt")
	if err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
}

func TestComputeFileChecksum_DifferentFiles(t *testing.T) {
	// Create two temporary files with different content
	tmpFile1, err := os.CreateTemp("", "test_checksum1_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file 1: %v", err)
	}
	defer os.Remove(tmpFile1.Name())

	tmpFile2, err := os.CreateTemp("", "test_checksum2_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file 2: %v", err)
	}
	defer os.Remove(tmpFile2.Name())

	// Write different content to each file
	tmpFile1.WriteString("Content A")
	tmpFile1.Close()
	tmpFile2.WriteString("Content B")
	tmpFile2.Close()

	db := &DB{}

	checksum1, err := db.ComputeFileChecksum(tmpFile1.Name())
	if err != nil {
		t.Fatalf("ComputeFileChecksum failed for file 1: %v", err)
	}

	checksum2, err := db.ComputeFileChecksum(tmpFile2.Name())
	if err != nil {
		t.Fatalf("ComputeFileChecksum failed for file 2: %v", err)
	}

	if checksum1 == checksum2 {
		t.Error("Expected different checksums for different files")
	}
}

func TestComputeSettingsHash(t *testing.T) {
	tests := []struct {
		name   string
		config *config.Config
	}{
		{
			name: "basic_config",
			config: &config.Config{
				WhisperModel:       "small",
				WhisperThreads:     4,
				WhisperComputeType: "int8",
				ChunkSize:          1024,
			},
		},
		{
			name: "large_model_config",
			config: &config.Config{
				WhisperModel:       "large-v3",
				WhisperThreads:     8,
				WhisperComputeType: "float32",
				ChunkSize:          2048,
			},
		},
	}

	db := &DB{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := db.ComputeSettingsHash(tt.config)

			// Verify hash is not empty
			if hash == "" {
				t.Error("Expected non-empty settings hash")
			}

			// Verify hash is consistent
			hash2 := db.ComputeSettingsHash(tt.config)
			if hash != hash2 {
				t.Errorf("Expected consistent settings hash, got %s and %s", hash, hash2)
			}
		})
	}
}

func TestComputeSettingsHash_DifferentConfigs(t *testing.T) {
	config1 := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     4,
		WhisperComputeType: "int8",
		ChunkSize:          1024,
	}

	config2 := &config.Config{
		WhisperModel:       "large-v3", // Different model
		WhisperThreads:     4,
		WhisperComputeType: "int8",
		ChunkSize:          1024,
	}

	config3 := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     8, // Different threads
		WhisperComputeType: "int8",
		ChunkSize:          1024,
	}

	config4 := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     4,
		WhisperComputeType: "float32", // Different compute type
		ChunkSize:          1024,
	}

	config5 := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     4,
		WhisperComputeType: "int8",
		ChunkSize:          2048, // Different chunk size
	}

	db := &DB{}

	hash1 := db.ComputeSettingsHash(config1)
	hash2 := db.ComputeSettingsHash(config2)
	hash3 := db.ComputeSettingsHash(config3)
	hash4 := db.ComputeSettingsHash(config4)
	hash5 := db.ComputeSettingsHash(config5)

	// All hashes should be different
	hashes := []string{hash1, hash2, hash3, hash4, hash5}
	for i := 0; i < len(hashes); i++ {
		for j := i + 1; j < len(hashes); j++ {
			if hashes[i] == hashes[j] {
				t.Errorf("Expected different hashes for different configs at indices %d and %d", i, j)
			}
		}
	}
}

func TestComputeSettingsHash_IgnoresNonTranscriptionSettings(t *testing.T) {
	// These configs differ only in non-transcription settings
	config1 := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     4,
		WhisperComputeType: "int8",
		ChunkSize:          1024,
		// Non-transcription settings
		AudioDir:     "/path1",
		OutputDir:    "/output1",
		OpenAIAPIKey: "key1",
		DBHost:       "host1",
	}

	config2 := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     4,
		WhisperComputeType: "int8",
		ChunkSize:          1024,
		// Different non-transcription settings
		AudioDir:     "/path2",
		OutputDir:    "/output2",
		OpenAIAPIKey: "key2",
		DBHost:       "host2",
	}

	db := &DB{}

	hash1 := db.ComputeSettingsHash(config1)
	hash2 := db.ComputeSettingsHash(config2)

	if hash1 != hash2 {
		t.Error("Expected same hash for configs with same transcription settings")
	}
}

// Test helper to create a temporary directory with test files
func createTestDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "db_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	return dir
}

// Test helper to create a test file with specific content
func createTestFile(t *testing.T, dir, filename, content string) string {
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create test file %s: %v", filePath, err)
	}
	return filePath
}

func TestFileChecksumIntegration(t *testing.T) {
	testDir := createTestDir(t)
	defer os.RemoveAll(testDir)

	db := &DB{}

	// Test 1: Create a file, compute checksum
	file1 := createTestFile(t, testDir, "test1.txt", "Original content")
	checksum1, err := db.ComputeFileChecksum(file1)
	if err != nil {
		t.Fatalf("Failed to compute checksum: %v", err)
	}

	// Test 2: Same file should have same checksum
	checksum1b, err := db.ComputeFileChecksum(file1)
	if err != nil {
		t.Fatalf("Failed to compute checksum on second call: %v", err)
	}
	if checksum1 != checksum1b {
		t.Error("Expected consistent checksums for same file")
	}

	// Test 3: Modify file content, checksum should change
	if err := os.WriteFile(file1, []byte("Modified content"), 0644); err != nil {
		t.Fatalf("Failed to modify file: %v", err)
	}
	checksum2, err := db.ComputeFileChecksum(file1)
	if err != nil {
		t.Fatalf("Failed to compute checksum after modification: %v", err)
	}
	if checksum1 == checksum2 {
		t.Error("Expected different checksums after file modification")
	}

	// Test 4: Create identical file with different name, should have same checksum as modified file
	file2 := createTestFile(t, testDir, "test2.txt", "Modified content")
	checksum3, err := db.ComputeFileChecksum(file2)
	if err != nil {
		t.Fatalf("Failed to compute checksum for second file: %v", err)
	}
	if checksum2 != checksum3 {
		t.Error("Expected same checksums for files with identical content")
	}
}

func TestSettingsHashScenarios(t *testing.T) {
	db := &DB{}

	scenarios := []struct {
		name        string
		config1     *config.Config
		config2     *config.Config
		shouldMatch bool
	}{
		{
			name: "identical_configs",
			config1: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			config2: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			shouldMatch: true,
		},
		{
			name: "different_model",
			config1: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			config2: &config.Config{
				WhisperModel: "large-v3", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			shouldMatch: false,
		},
		{
			name: "different_threads",
			config1: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			config2: &config.Config{
				WhisperModel: "small", WhisperThreads: 8, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			shouldMatch: false,
		},
		{
			name: "different_compute_type",
			config1: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			config2: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "float32", ChunkSize: 1024,
			},
			shouldMatch: false,
		},
		{
			name: "different_chunk_size",
			config1: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 1024,
			},
			config2: &config.Config{
				WhisperModel: "small", WhisperThreads: 4, WhisperComputeType: "int8", ChunkSize: 2048,
			},
			shouldMatch: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			hash1 := db.ComputeSettingsHash(scenario.config1)
			hash2 := db.ComputeSettingsHash(scenario.config2)

			if scenario.shouldMatch && hash1 != hash2 {
				t.Errorf("Expected matching hashes, got %s and %s", hash1, hash2)
			}

			if !scenario.shouldMatch && hash1 == hash2 {
				t.Errorf("Expected different hashes, but both were %s", hash1)
			}
		})
	}
}

// Benchmark tests for performance
func BenchmarkComputeFileChecksum(b *testing.B) {
	// Create a test file
	tmpFile, err := os.CreateTemp("", "benchmark_checksum_*.txt")
	if err != nil {
		b.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write some content
	content := strings.Repeat("Hello, World! ", 1000) // ~13KB
	tmpFile.WriteString(content)
	tmpFile.Close()

	db := &DB{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := db.ComputeFileChecksum(tmpFile.Name())
		if err != nil {
			b.Fatalf("ComputeFileChecksum failed: %v", err)
		}
	}
}

func BenchmarkComputeSettingsHash(b *testing.B) {
	config := &config.Config{
		WhisperModel:       "large-v3",
		WhisperThreads:     8,
		WhisperComputeType: "float32",
		ChunkSize:          2048,
	}

	db := &DB{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = db.ComputeSettingsHash(config)
	}
}
