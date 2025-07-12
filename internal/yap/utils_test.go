package yap

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		name     string
		size     int64
		expected string
	}{
		{"zero_bytes", 0, "0 B"},
		{"small_bytes", 512, "512 B"},
		{"one_kb", 1024, "1.0 KB"},
		{"one_mb", 1024 * 1024, "1.0 MB"},
		{"one_gb", 1024 * 1024 * 1024, "1.0 GB"},
		{"mixed_size", 1536, "1.5 KB"},
		{"large_file", int64(2.5 * 1024 * 1024 * 1024), "2.5 GB"},
		{"very_large", 5 * 1024 * 1024 * 1024 * 1024, "5.0 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatFileSize(tt.size)
			if result != tt.expected {
				t.Errorf("formatFileSize(%d) = %s, expected %s", tt.size, result, tt.expected)
			}
		})
	}
}

func TestCountWords(t *testing.T) {
	// Create a temporary file with known content
	tmpDir, err := os.MkdirTemp("", "yap-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "This is a test file with exactly ten words total."
	
	err = os.WriteFile(testFile, []byte(testContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	wordCount, err := countWords(testFile)
	if err != nil {
		t.Errorf("countWords() returned error: %v", err)
	}

	expected := 10
	if wordCount != expected {
		t.Errorf("countWords() = %d, expected %d", wordCount, expected)
	}

	// Test with empty file
	emptyFile := filepath.Join(tmpDir, "empty.txt")
	err = os.WriteFile(emptyFile, []byte(""), 0644)
	if err != nil {
		t.Fatalf("Failed to write empty file: %v", err)
	}

	wordCount, err = countWords(emptyFile)
	if err != nil {
		t.Errorf("countWords() on empty file returned error: %v", err)
	}

	if wordCount != 0 {
		t.Errorf("countWords() on empty file = %d, expected 0", wordCount)
	}

	// Test with nonexistent file
	_, err = countWords("nonexistent.txt")
	if err == nil {
		t.Error("countWords() on nonexistent file should return error")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"milliseconds", 500 * time.Millisecond, "500ms"},
		{"one_second", 1 * time.Second, "1.0s"},
		{"multiple_seconds", 2500 * time.Millisecond, "2.5s"},
		{"minutes", 90 * time.Second, "90.0s"},
		{"zero", 0, "0ms"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("formatDuration(%v) = %s, expected %s", tt.duration, result, tt.expected)
			}
		})
	}
}

func TestGetInputSize(t *testing.T) {
	// Create a temporary file with known size
	tmpDir, err := os.MkdirTemp("", "yap-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "Hello, World!"
	
	err = os.WriteFile(testFile, []byte(testContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	size, err := getInputSize(testFile)
	if err != nil {
		t.Errorf("getInputSize() returned error: %v", err)
	}

	expected := int64(len(testContent))
	if size != expected {
		t.Errorf("getInputSize() = %d, expected %d", size, expected)
	}

	// Test with nonexistent file
	_, err = getInputSize("nonexistent.txt")
	if err == nil {
		t.Error("getInputSize() on nonexistent file should return error")
	}
}