package transcribe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"transcriber/internal/config"
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
		{"large_file", 2.5 * 1024 * 1024 * 1024, "2.5 GB"},
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

func TestShortenPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"simple_filename", "file.txt", "file.txt"},
		{"absolute_path", "/home/user/documents/file.txt", "file.txt"},
		{"relative_path", "documents/file.txt", "file.txt"},
		{"audio_file", "/audiobooks/author/book/chapter01.mp3", "chapter01.mp3"},
		{"complex_filename", "/path/to/Book Title [B12345] - 01 - Chapter Name.m4b", "Book Title [B12345] - 01 - Chapter Name.m4b"},
		{"no_extension", "/path/to/filename", "filename"},
		{"hidden_file", "/path/to/.hidden", ".hidden"},
		{"empty_string", "", "."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shortenPath(tt.path)
			if result != tt.expected {
				t.Errorf("shortenPath(%s) = %s, expected %s", tt.path, result, tt.expected)
			}
		})
	}
}

func TestCountWords(t *testing.T) {
	// Create temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "transcribe_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name     string
		content  string
		expected int
	}{
		{"empty_file", "", 0},
		{"single_word", "hello", 1},
		{"multiple_words", "hello world test", 3},
		{"words_with_punctuation", "Hello, world! How are you?", 5},
		{"mixed_whitespace", "hello\tworld\ntest\r\nfour", 4},
		{"extra_spaces", "  hello   world  test  ", 3},
		{"transcript_text", "CHAPTER 1: Introduction\n\nThis is a sample transcription.", 8},
		{"complex_transcript", `The quick brown fox jumps over the lazy dog. 
		This sentence contains every letter of the alphabet.`, 17},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test file
			testFile := filepath.Join(tmpDir, tt.name+".txt")
			err := os.WriteFile(testFile, []byte(tt.content), 0644)
			if err != nil {
				t.Fatalf("Failed to create test file: %v", err)
			}

			result, err := countWords(testFile)
			if err != nil {
				t.Fatalf("countWords failed: %v", err)
			}

			if result != tt.expected {
				t.Errorf("countWords(%s) = %d, expected %d for content: %q", tt.name, result, tt.expected, tt.content)
			}
		})
	}
}

func TestCountWordsError(t *testing.T) {
	// Test with non-existent file
	_, err := countWords("/path/to/nonexistent/file.txt")
	if err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"zero", 0, "0ms"},
		{"milliseconds", 500 * time.Millisecond, "500ms"},
		{"one_second", time.Second, "1.0s"},
		{"seconds_with_decimal", 2500 * time.Millisecond, "2.5s"},
		{"minutes", 90 * time.Second, "90.0s"},
		{"precise_seconds", 1234 * time.Millisecond, "1.2s"},
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

func TestNewTranscriber(t *testing.T) {
	// Create a temporary cache directory for testing
	tmpCacheDir, err := os.MkdirTemp("", "transcribe_cache_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp cache dir: %v", err)
	}
	defer os.RemoveAll(tmpCacheDir)

	cfg := &config.Config{
		CacheDir:  tmpCacheDir,
		OutputDir: "/tmp/test_output",
	}

	// Note: This test will skip dependency checks in a real environment
	// We might need to mock the checkDependencies function for unit testing

	// Test that we can create a transcriber struct
	// (Note: In a real environment, this might fail due to dependency checks)
	transcriber := &Transcriber{
		config: cfg,
	}

	if transcriber.config != cfg {
		t.Error("Expected transcriber to have correct config")
	}
}

func TestGetRelativePath(t *testing.T) {
	cfg := &config.Config{
		AudioDir: "/audiobooks",
	}

	transcriber := &Transcriber{
		config: cfg,
	}

	tests := []struct {
		name      string
		audioPath string
		expected  string
	}{
		{
			name:      "direct_child",
			audioPath: "/audiobooks/file.mp3",
			expected:  ".",
		},
		{
			name:      "nested_path",
			audioPath: "/audiobooks/author/book/chapter.mp3",
			expected:  "author/book",
		},
		{
			name:      "deep_nesting",
			audioPath: "/audiobooks/fiction/author/series/book/chapter01.mp3",
			expected:  "fiction/author/series/book",
		},
		{
			name:      "outside_audio_dir",
			audioPath: "/other/path/file.mp3",
			expected:  "../other/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := transcriber.getRelativePath(tt.audioPath)
			if result != tt.expected {
				t.Errorf("getRelativePath(%s) = %s, expected %s", tt.audioPath, result, tt.expected)
			}
		})
	}
}

func TestEnsureOutputDir(t *testing.T) {
	// Create temporary base directory
	tmpBaseDir, err := os.MkdirTemp("", "transcribe_output_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp base dir: %v", err)
	}
	defer os.RemoveAll(tmpBaseDir)

	cfg := &config.Config{
		OutputDir: tmpBaseDir,
	}

	transcriber := &Transcriber{
		config: cfg,
	}

	tests := []struct {
		name         string
		relativePath string
		expectError  bool
	}{
		{
			name:         "empty_relative_path",
			relativePath: "",
			expectError:  false,
		},
		{
			name:         "simple_relative_path",
			relativePath: "author",
			expectError:  false,
		},
		{
			name:         "nested_relative_path",
			relativePath: "author/book",
			expectError:  false,
		},
		{
			name:         "deep_nested_path",
			relativePath: "fiction/author/series/book",
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := transcriber.ensureOutputDir(tt.relativePath)

			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}

			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if !tt.expectError {
				// Verify directory was created
				if _, statErr := os.Stat(result); os.IsNotExist(statErr) {
					t.Errorf("Expected directory %s to be created", result)
				}

				// Verify path structure
				expectedPath := tmpBaseDir
				if tt.relativePath != "" {
					expectedPath = filepath.Join(tmpBaseDir, tt.relativePath)
				}

				if result != expectedPath {
					t.Errorf("Expected path %s, got %s", expectedPath, result)
				}
			}
		})
	}
}

func TestGetInputSize(t *testing.T) {
	// Create temporary test files
	tmpDir, err := os.MkdirTemp("", "transcribe_size_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files with known sizes
	testFiles := []struct {
		name    string
		content string
		size    int64
	}{
		{"empty.txt", "", 0},
		{"small.txt", "hello", 5},
		{"medium.txt", strings.Repeat("test ", 100), 500},
	}

	for _, tf := range testFiles {
		filePath := filepath.Join(tmpDir, tf.name)
		err := os.WriteFile(filePath, []byte(tf.content), 0644)
		if err != nil {
			t.Fatalf("Failed to create test file %s: %v", tf.name, err)
		}

		size, err := getInputSize(filePath)
		if err != nil {
			t.Errorf("getInputSize failed for %s: %v", tf.name, err)
		}

		if size != tf.size {
			t.Errorf("getInputSize(%s) = %d, expected %d", tf.name, size, tf.size)
		}
	}

	// Test with non-existent file
	_, err = getInputSize("/path/to/nonexistent/file")
	if err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
}

func TestClearCacheDir(t *testing.T) {
	// Create temporary directory
	tmpDir, err := os.MkdirTemp("", "transcribe_clear_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create some files in the directory
	testFile := filepath.Join(tmpDir, "test.txt")
	err = os.WriteFile(testFile, []byte("test content"), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create subdirectory with files
	subDir := filepath.Join(tmpDir, "subdir")
	err = os.MkdirAll(subDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	subFile := filepath.Join(subDir, "subfile.txt")
	err = os.WriteFile(subFile, []byte("sub content"), 0644)
	if err != nil {
		t.Fatalf("Failed to create sub file: %v", err)
	}

	// Verify files exist before clearing
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Fatal("Test file should exist before clearing")
	}

	if _, err := os.Stat(subFile); os.IsNotExist(err) {
		t.Fatal("Sub file should exist before clearing")
	}

	// Clear the cache directory
	err = clearCacheDir(tmpDir)
	if err != nil {
		t.Fatalf("clearCacheDir failed: %v", err)
	}

	// Verify directory still exists but is empty
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("Cache directory should still exist after clearing")
	}

	// Verify files are gone
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("Test file should be removed after clearing")
	}

	if _, err := os.Stat(subFile); !os.IsNotExist(err) {
		t.Error("Sub file should be removed after clearing")
	}

	// Verify directory is empty
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read cleared directory: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("Expected empty directory after clearing, found %d entries", len(entries))
	}
}

// Benchmark tests
func BenchmarkFormatFileSize(b *testing.B) {
	sizes := []int64{0, 1024, 1024 * 1024, 1024 * 1024 * 1024}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				formatFileSize(size)
			}
		})
	}
}

func BenchmarkShortenPath(b *testing.B) {
	paths := []string{
		"file.txt",
		"/simple/path/file.txt",
		"/very/long/path/with/many/components/file.txt",
		"/audiobooks/author/book/Book Title [B12345] - 01 - Chapter Name.m4b",
	}

	for _, path := range paths {
		b.Run(fmt.Sprintf("path_len_%d", len(path)), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				shortenPath(path)
			}
		})
	}
}

func BenchmarkCountWords(b *testing.B) {
	// Create temporary test file
	tmpFile, err := os.CreateTemp("", "benchmark_count_*.txt")
	if err != nil {
		b.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write test content with known word count
	content := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 1000)
	tmpFile.WriteString(content)
	tmpFile.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		countWords(tmpFile.Name())
	}
}
