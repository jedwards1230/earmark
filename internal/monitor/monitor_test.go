package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/queue"
)

// MonitorDB interface defines the database methods used by FileMonitor
type MonitorDB interface {
	IsProcessed(ctx context.Context, filePath string) (bool, error)
	GetProcessingStats(ctx context.Context) (*db.Statistics, error)
	CheckForMismatchedChunks(ctx context.Context, chunkSize int) ([]meta.BookMetadata, error)
	DeleteBookChunks(ctx context.Context, bookID int) error
}

// Mock DB for monitor testing
type MockMonitorDB struct {
	processedFiles map[string]bool
	statistics     *db.Statistics
	errors         map[string]error
}

func NewMockMonitorDB() *MockMonitorDB {
	return &MockMonitorDB{
		processedFiles: make(map[string]bool),
		statistics: &db.Statistics{
			ProcessedBooks:    0,
			ProcessedChapters: 0,
			ReprocessingBooks: 0,
		},
		errors: make(map[string]error),
	}
}

func (m *MockMonitorDB) IsProcessed(ctx context.Context, filePath string) (bool, error) {
	if err, exists := m.errors[filePath]; exists {
		return false, err
	}
	return m.processedFiles[filePath], nil
}

func (m *MockMonitorDB) GetProcessingStats(ctx context.Context) (*db.Statistics, error) {
	return m.statistics, nil
}

func (m *MockMonitorDB) CheckForMismatchedChunks(ctx context.Context, chunkSize int) ([]meta.BookMetadata, error) {
	return []meta.BookMetadata{}, nil // Return empty slice instead of nil
}

func (m *MockMonitorDB) DeleteBookChunks(ctx context.Context, bookID int) error {
	return nil // Mock delete operation
}

func (m *MockMonitorDB) SetProcessed(filePath string, processed bool) {
	m.processedFiles[filePath] = processed
}

func (m *MockMonitorDB) SetError(filePath string, err error) {
	m.errors[filePath] = err
}

func TestFileMonitorInitialization(t *testing.T) {
	cfg := &config.Config{
		AudioDir: "/test/audio",
	}
	q := queue.NewQueue()

	// Test basic initialization without full constructor
	monitor := &FileMonitor{
		config:      cfg,
		queue:       q,
		queuedFiles: make(map[string]bool),
	}

	if monitor.config != cfg {
		t.Error("Expected monitor to have correct config")
	}

	if monitor.queue != q {
		t.Error("Expected monitor to have correct queue")
	}

	if monitor.queuedFiles == nil {
		t.Error("Expected queuedFiles map to be initialized")
	}
}

func TestFindAudioFilesInDir(t *testing.T) {
	// Create temporary test directory
	tmpDir, err := os.MkdirTemp("", "monitor_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	testFiles := []struct {
		name    string
		isAudio bool
	}{
		{"chapter01.mp3", true},
		{"chapter02.m4a", true},
		{"chapter03.m4b", true},
		{"chapter04.wav", true},
		{"chapter05.flac", true},
		{"metadata.json", false},
		{"cover.jpg", false},
		{"readme.txt", false},
		{"chapter06.MP3", true}, // Test case sensitivity
	}

	expectedAudioFiles := 0
	for _, tf := range testFiles {
		filePath := filepath.Join(tmpDir, tf.name)
		err := os.WriteFile(filePath, []byte("test content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create test file %s: %v", tf.name, err)
		}
		if tf.isAudio {
			expectedAudioFiles++
		}
	}

	// Test findAudioFilesInDir
	audioFiles, err := findAudioFilesInDir(tmpDir)
	if err != nil {
		t.Fatalf("findAudioFilesInDir failed: %v", err)
	}

	if len(audioFiles) != expectedAudioFiles {
		t.Errorf("Expected %d audio files, got %d", expectedAudioFiles, len(audioFiles))
	}

	// Verify all returned files are audio files
	for _, file := range audioFiles {
		found := false
		for _, tf := range testFiles {
			if filepath.Base(file) == tf.name && tf.isAudio {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Unexpected audio file: %s", file)
		}
	}
}

func TestFindAudioFilesInDir_EmptyDir(t *testing.T) {
	// Create empty temporary directory
	tmpDir, err := os.MkdirTemp("", "monitor_empty_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	audioFiles, err := findAudioFilesInDir(tmpDir)
	if err != nil {
		t.Fatalf("findAudioFilesInDir failed: %v", err)
	}

	if len(audioFiles) != 0 {
		t.Errorf("Expected 0 audio files in empty directory, got %d", len(audioFiles))
	}
}

func TestFindAudioFilesInDir_NonExistentDir(t *testing.T) {
	_, err := findAudioFilesInDir("/path/to/nonexistent/directory")
	if err == nil {
		t.Error("Expected error for non-existent directory, got nil")
	}
}

func TestIsAudioFile(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"chapter01.mp3", true},
		{"chapter01.MP3", true}, // Test case insensitivity
		{"book.m4a", true},
		{"audiobook.m4b", true},
		{"music.wav", true},
		{"audio.flac", true},
		{"voice.aac", true},
		{"sound.ogg", true},
		{"audio.wma", true},
		{"metadata.json", false},
		{"cover.jpg", false},
		{"readme.txt", false},
		{"document.pdf", false},
		{"", false},
		{"noextension", false},
		{".mp3", true}, // Just extension
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			result := isAudioFile(tt.filename)
			if result != tt.expected {
				t.Errorf("isAudioFile(%q) = %v, expected %v", tt.filename, result, tt.expected)
			}
		})
	}
}

func TestAbs(t *testing.T) {
	tests := []struct {
		input    int
		expected int
	}{
		{5, 5},
		{-5, 5},
		{0, 0},
		{1, 1},
		{-1, 1},
		{100, 100},
		{-100, 100},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("abs(%d)", tt.input), func(t *testing.T) {
			result := abs(tt.input)
			if result != tt.expected {
				t.Errorf("abs(%d) = %d, expected %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFindChapterInfo(t *testing.T) {
	// Create a test monitor with minimal setup
	monitor := &FileMonitor{}

	// Test data
	chaptersInfo := []meta.ChapterInfo{
		{Title: "Opening Credits"},
		{Title: "Chapter 1: The Beginning"},
		{Title: "Chapter 2: The Journey"},
		{Title: "Chapter 3: The End"},
		{Title: "End Credits"},
	}

	tests := []struct {
		name         string
		audioFile    string
		fileIndex    int
		expectedIdx  int
		expectedName string
	}{
		{
			name:         "exact match",
			audioFile:    "Chapter 1: The Beginning.mp3",
			fileIndex:    0,
			expectedIdx:  2,
			expectedName: "Chapter 1: The Beginning",
		},
		{
			name:         "partial match",
			audioFile:    "Journey.mp3",
			fileIndex:    1,
			expectedIdx:  3,
			expectedName: "Chapter 2: The Journey",
		},
		{
			name:         "no match",
			audioFile:    "Random.mp3",
			fileIndex:    2,
			expectedIdx:  3,
			expectedName: "3",
		},
		{
			name:         "nil chapters",
			audioFile:    "Random.mp3",
			fileIndex:    0,
			expectedIdx:  1,
			expectedName: "1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var chapters []meta.ChapterInfo
			if tt.name != "nil chapters" {
				chapters = chaptersInfo
			}

			idx, name := monitor.findChapterInfo(chapters, tt.audioFile, tt.fileIndex)
			if idx != tt.expectedIdx {
				t.Errorf("Expected index %d, got %d", tt.expectedIdx, idx)
			}
			if name != tt.expectedName {
				t.Errorf("Expected name %q, got %q", tt.expectedName, name)
			}
		})
	}
}

func TestMockMonitorDB(t *testing.T) {
	ctx := context.Background()
	mockDB := NewMockMonitorDB()

	// Test IsProcessed with unprocessed file
	processed, err := mockDB.IsProcessed(ctx, "/test/file.mp3")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if processed {
		t.Error("Expected file to not be processed initially")
	}

	// Test setting processed
	mockDB.SetProcessed("/test/file.mp3", true)
	processed, err = mockDB.IsProcessed(ctx, "/test/file.mp3")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if !processed {
		t.Error("Expected file to be processed after setting")
	}

	// Test error handling
	testErr := fmt.Errorf("test error")
	mockDB.SetError("/test/error.mp3", testErr)
	processed, err = mockDB.IsProcessed(ctx, "/test/error.mp3")
	if err != testErr {
		t.Errorf("Expected error %v, got %v", testErr, err)
	}

	// Test GetProcessingStats
	stats, err := mockDB.GetProcessingStats(ctx)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if stats == nil {
		t.Error("Expected non-nil statistics")
	}

	// Test CheckForMismatchedChunks
	chunks, err := mockDB.CheckForMismatchedChunks(ctx, 1000)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if chunks == nil {
		t.Error("Expected non-nil chunks slice")
	}

	// Test DeleteBookChunks
	err = mockDB.DeleteBookChunks(ctx, 1)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

// Benchmark tests
func BenchmarkIsAudioFile(b *testing.B) {
	testFiles := []string{
		"chapter01.mp3",
		"book.m4a",
		"audiobook.m4b",
		"metadata.json",
		"cover.jpg",
		"readme.txt",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, file := range testFiles {
			_ = isAudioFile(file)
		}
	}
}

func BenchmarkFindAudioFilesInDir(b *testing.B) {
	// Create temporary test directory
	tmpDir, err := os.MkdirTemp("", "monitor_bench_*")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	for i := 0; i < 100; i++ {
		filename := fmt.Sprintf("chapter%03d.mp3", i)
		filePath := filepath.Join(tmpDir, filename)
		err := os.WriteFile(filePath, []byte("test"), 0644)
		if err != nil {
			b.Fatalf("Failed to create test file: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := findAudioFilesInDir(tmpDir)
		if err != nil {
			b.Fatalf("findAudioFilesInDir failed: %v", err)
		}
	}
}

func BenchmarkAbs(b *testing.B) {
	testValues := []int{-100, -50, -10, -1, 0, 1, 10, 50, 100}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, val := range testValues {
			_ = abs(val)
		}
	}
}
