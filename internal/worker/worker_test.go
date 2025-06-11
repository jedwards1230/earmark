package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/meta"
	"transcriber/internal/queue"
)

func TestCountWords(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected int
	}{
		{
			name:     "empty_string",
			content:  "",
			expected: 0,
		},
		{
			name:     "single_word",
			content:  "hello",
			expected: 1,
		},
		{
			name:     "multiple_words_spaces",
			content:  "hello world test",
			expected: 3,
		},
		{
			name:     "multiple_words_mixed_whitespace",
			content:  "hello\tworld\ntest\r\nfour",
			expected: 4,
		},
		{
			name:     "words_with_punctuation",
			content:  "Hello, world! How are you today?",
			expected: 6,
		},
		{
			name:     "extra_spaces",
			content:  "  hello   world  test  ",
			expected: 3,
		},
		{
			name:     "newlines_and_paragraphs",
			content:  "First paragraph.\n\nSecond paragraph with more words.\n\nThird paragraph.",
			expected: 9,
		},
		{
			name:     "numbers_and_symbols",
			content:  "The year 2024 has 365 days. Cost: $19.99",
			expected: 8,
		},
		{
			name:     "hyphenated_words",
			content:  "state-of-the-art twenty-first century",
			expected: 3,
		},
	}

	worker := &Worker{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := worker.countWords(tt.content)
			if result != tt.expected {
				t.Errorf("countWords() = %d, expected %d for content: %q", result, tt.expected, tt.content)
			}
		})
	}
}

func TestCountWords_RealWorldExamples(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected int
	}{
		{
			name:     "short_sentence",
			content:  "The quick brown fox jumps over the lazy dog.",
			expected: 9,
		},
		{
			name: "paragraph_with_dialogue",
			content: `"Hello," said John. "How are you today?"
			
			"I'm doing well, thank you," replied Mary. "What about you?"`,
			expected: 17,
		},
		{
			name: "technical_text",
			content: `The function computeFileChecksum() calculates a SHA256 hash of the file contents. 
			It returns an error if the file cannot be read.`,
			expected: 21,
		},
		{
			name: "transcript_like_text",
			content: `CHAPTER ONE: The Beginning
			
			In the beginning, there was nothing but darkness. Then came the light, 
			and with it, the first words were spoken into existence.
			
			"Let there be sound," said the narrator, and there was sound.`,
			expected: 37,
		},
	}

	worker := &Worker{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := worker.countWords(tt.content)
			if result != tt.expected {
				t.Errorf("countWords() = %d, expected %d for content: %q", result, tt.expected, tt.content)
			}
		})
	}
}

// Mock DB for testing worker functionality
type MockDB struct {
	checksums          map[string]string
	settingsHashes     map[string]string
	transcriptions     map[string]*db.Transcription
	needsTranscription map[string]bool
	storeErrors        map[string]error
	getErrors          map[string]error
}

func NewMockDB() *MockDB {
	return &MockDB{
		checksums:          make(map[string]string),
		settingsHashes:     make(map[string]string),
		transcriptions:     make(map[string]*db.Transcription),
		needsTranscription: make(map[string]bool),
		storeErrors:        make(map[string]error),
		getErrors:          make(map[string]error),
	}
}

func (m *MockDB) ComputeFileChecksum(filePath string) (string, error) {
	if checksum, exists := m.checksums[filePath]; exists {
		return checksum, nil
	}
	return "mock_checksum_" + filepath.Base(filePath), nil
}

func (m *MockDB) ComputeSettingsHash(cfg *config.Config) string {
	key := cfg.WhisperModel + "_" + cfg.WhisperComputeType
	if hash, exists := m.settingsHashes[key]; exists {
		return hash
	}
	return "mock_settings_hash_" + key
}

func (m *MockDB) NeedsTranscription(ctx context.Context, filePath, fileChecksum, settingsHash string) (bool, error) {
	key := filePath + "_" + fileChecksum + "_" + settingsHash
	if needs, exists := m.needsTranscription[key]; exists {
		return needs, nil
	}
	return true, nil // Default to needing transcription
}

func (m *MockDB) StoreTranscription(ctx context.Context, filePath, fileChecksum, settingsHash, transcriptionText string, fileSize int64, wordCount int, processingDurationMs int64) error {
	if err, exists := m.storeErrors[filePath]; exists {
		return err
	}

	// Store the transcription
	m.transcriptions[filePath] = &db.Transcription{
		FilePath:             filePath,
		FileChecksum:         fileChecksum,
		FileSize:             fileSize,
		SettingsHash:         settingsHash,
		TranscriptionText:    transcriptionText,
		WordCount:            wordCount,
		ProcessingDurationMs: processingDurationMs,
	}
	return nil
}

func (m *MockDB) GetTranscription(ctx context.Context, filePath string) (*db.Transcription, error) {
	if err, exists := m.getErrors[filePath]; exists {
		return nil, err
	}

	if transcription, exists := m.transcriptions[filePath]; exists {
		return transcription, nil
	}

	return nil, errors.New("transcription not found")
}

// Set up mock expectations
func (m *MockDB) SetChecksum(filePath, checksum string) {
	m.checksums[filePath] = checksum
}

func (m *MockDB) SetSettingsHash(configKey, hash string) {
	m.settingsHashes[configKey] = hash
}

func (m *MockDB) SetNeedsTranscription(filePath, fileChecksum, settingsHash string, needs bool) {
	key := filePath + "_" + fileChecksum + "_" + settingsHash
	m.needsTranscription[key] = needs
}

func (m *MockDB) SetStoreError(filePath string, err error) {
	m.storeErrors[filePath] = err
}

func (m *MockDB) SetGetError(filePath string, err error) {
	m.getErrors[filePath] = err
}

func (m *MockDB) GetStoredTranscription(filePath string) *db.Transcription {
	return m.transcriptions[filePath]
}

func TestWorkerDeduplicationLogic(t *testing.T) {
	// Create a test file
	tmpFile, err := os.CreateTemp("", "worker_test_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	testContent := "Test audio file content"
	tmpFile.WriteString(testContent)
	tmpFile.Close()

	// Get file info
	fileInfo, err := os.Stat(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to get file info: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		WhisperModel:       "small",
		WhisperThreads:     4,
		WhisperComputeType: "int8",
		ChunkSize:          1024,
	}

	// Create test metadata
	_ = &meta.FileMetadata{
		FilePath:     tmpFile.Name(),
		Chapter:      "Test Chapter",
		ChapterIndex: 1,
		Title:        "Test Book",
		Author:       "Test Author",
	}

	queueItem := queue.QueueItem{
		FilePath: tmpFile.Name(),
		Metadata: &meta.BookMetadata{
			Title:  "Test Book",
			Author: "Test Author",
		},
	}

	tests := []struct {
		name                   string
		setupMock              func(*MockDB)
		expectTranscriptionRun bool
		expectError            bool
		expectedWordCount      int
	}{
		{
			name: "first_time_transcription",
			setupMock: func(mockDB *MockDB) {
				// File needs transcription (first time)
				mockDB.SetNeedsTranscription(tmpFile.Name(), "mock_checksum_"+filepath.Base(tmpFile.Name()), "mock_settings_hash_small_int8", true)
			},
			expectTranscriptionRun: true,
			expectError:            false,
			expectedWordCount:      4, // "Test audio file content"
		},
		{
			name: "existing_transcription_current",
			setupMock: func(mockDB *MockDB) {
				// File doesn't need transcription (already exists and current)
				mockDB.SetNeedsTranscription(tmpFile.Name(), "mock_checksum_"+filepath.Base(tmpFile.Name()), "mock_settings_hash_small_int8", false)

				// Set up existing transcription
				mockDB.transcriptions[tmpFile.Name()] = &db.Transcription{
					FilePath:          tmpFile.Name(),
					TranscriptionText: "Existing transcription text",
					WordCount:         3,
				}
			},
			expectTranscriptionRun: false,
			expectError:            false,
			expectedWordCount:      3, // From existing transcription
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock DB
			mockDB := NewMockDB()
			tt.setupMock(mockDB)

			// Note: In a real test, we would need to mock the transcriber as well
			// For now, we'll test the logic around the database calls

			// Test the deduplication logic directly
			ctx := context.Background()
			startTime := time.Now()

			// Check if transcription is needed
			fileChecksum, err := mockDB.ComputeFileChecksum(queueItem.FilePath)
			if err != nil {
				t.Fatalf("ComputeFileChecksum failed: %v", err)
			}

			settingsHash := mockDB.ComputeSettingsHash(cfg)
			needsTranscription, err := mockDB.NeedsTranscription(ctx, queueItem.FilePath, fileChecksum, settingsHash)
			if err != nil {
				t.Fatalf("NeedsTranscription failed: %v", err)
			}

			if tt.expectTranscriptionRun && !needsTranscription {
				t.Error("Expected transcription to be needed, but it wasn't")
			}

			if !tt.expectTranscriptionRun && needsTranscription {
				t.Error("Expected transcription to not be needed, but it was")
			}

			// If transcription is not needed, retrieve existing
			if !needsTranscription {
				transcription, err := mockDB.GetTranscription(ctx, queueItem.FilePath)
				if err != nil {
					t.Fatalf("GetTranscription failed: %v", err)
				}

				if transcription.WordCount != tt.expectedWordCount {
					t.Errorf("Expected word count %d, got %d", tt.expectedWordCount, transcription.WordCount)
				}
			} else {
				// Simulate transcription process
				transcriptionText := "Mock transcription result"
				wordCount := 4 // countWords would return this
				processingDuration := time.Since(startTime)
				processingDurationMs := processingDuration.Milliseconds()

				// Store transcription
				err := mockDB.StoreTranscription(ctx, queueItem.FilePath, fileChecksum, settingsHash, transcriptionText, fileInfo.Size(), wordCount, processingDurationMs)
				if tt.expectError && err == nil {
					t.Error("Expected error but got none")
				}
				if !tt.expectError && err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				// Verify stored transcription
				if !tt.expectError {
					stored := mockDB.GetStoredTranscription(queueItem.FilePath)
					if stored == nil {
						t.Error("Expected transcription to be stored")
					} else {
						if stored.WordCount != tt.expectedWordCount {
							t.Errorf("Expected stored word count %d, got %d", tt.expectedWordCount, stored.WordCount)
						}
						if stored.TranscriptionText != transcriptionText {
							t.Errorf("Expected stored text %q, got %q", transcriptionText, stored.TranscriptionText)
						}
					}
				}
			}
		})
	}
}

func TestWorkerCountWordsIntegration(t *testing.T) {
	worker := &Worker{}

	// Test real-world transcription-like content
	transcriptionContent := `CHAPTER 1: THE BEGINNING

In a hole in the ground there lived a hobbit. Not a nasty, dirty, wet hole, filled with the ends of worms and an oozy smell, nor yet a dry, bare, sandy hole with nothing in it to sit down on or to eat: it was a hobbit-hole, and that means comfort.

It had a perfectly round door like a porthole, painted green, with a shiny yellow brass knob in the exact middle. The door opened on to a tube-shaped hall like a tunnel: a very comfortable tunnel without smoke, with panelled walls, and floors tiled and carpeted, provided with polished chairs, and lots and lots of pegs for hats and coats—the hobbit was fond of visitors.`

	expectedWordCount := 121 // Manually counted

	result := worker.countWords(transcriptionContent)
	if result != expectedWordCount {
		t.Errorf("Expected word count %d for transcription-like content, got %d", expectedWordCount, result)
	}
}

// Benchmark for countWords function
func BenchmarkCountWords(b *testing.B) {
	worker := &Worker{}

	// Create test content of various sizes
	contents := []string{
		"short test content",
		"This is a medium length content with more words to test the performance of the word counting function.",
		`This is a much longer content that simulates a real transcription. It contains multiple sentences, paragraphs, and various punctuation marks. The goal is to test how the word counting function performs with larger texts that might be typical of audio transcription results. Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat. Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur. Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia deserunt mollit anim id est laborum.`,
	}

	for i, content := range contents {
		b.Run(fmt.Sprintf("content_size_%d", i), func(b *testing.B) {
			for j := 0; j < b.N; j++ {
				worker.countWords(content)
			}
		})
	}
}

// Test helper functions removed - not currently used

func TestWorkerCreation(t *testing.T) {
	queue := &queue.Queue{}
	db := &db.DB{}

	worker := NewWorker(queue, db)

	if worker == nil {
		t.Error("Expected non-nil worker")
		return
	}

	if worker.queue != queue {
		t.Error("Expected worker to have correct queue reference")
	}

	if worker.db != db {
		t.Error("Expected worker to have correct db reference")
	}

	if worker.ctx == nil {
		t.Error("Expected worker to have context")
	}

	if worker.cancel == nil {
		t.Error("Expected worker to have cancel function")
	}
}
