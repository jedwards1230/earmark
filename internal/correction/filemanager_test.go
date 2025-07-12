package correction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/config"
)

func TestFileManager(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "correction_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := &config.Config{
		OutputDir: tempDir,
	}

	fm := NewFileManager(cfg)

	// Test directory creation
	rawDir := filepath.Join(tempDir, "raw")
	correctedDir := filepath.Join(tempDir, "corrected")

	if _, err := os.Stat(rawDir); os.IsNotExist(err) {
		t.Error("Raw directory was not created")
	}

	if _, err := os.Stat(correctedDir); os.IsNotExist(err) {
		t.Error("Corrected directory was not created")
	}

	// Test file operations
	audioFilePath := "/path/to/audiobook/chapter1.m4b"
	rawText := "This is the raw transcription text."
	correctedText := "This is the corrected transcription text."

	// Test saving and retrieving raw text
	if err := fm.SaveRawText(audioFilePath, rawText); err != nil {
		t.Errorf("Failed to save raw text: %v", err)
	}

	if !fm.RawTextExists(audioFilePath) {
		t.Error("Raw text file should exist")
	}

	retrievedRaw, err := fm.GetRawText(audioFilePath)
	if err != nil {
		t.Errorf("Failed to get raw text: %v", err)
	}

	if retrievedRaw != rawText {
		t.Errorf("Retrieved raw text doesn't match. Expected: %s, Got: %s", rawText, retrievedRaw)
	}

	// Test saving and retrieving corrected text
	if err := fm.SaveCorrectedText(audioFilePath, correctedText); err != nil {
		t.Errorf("Failed to save corrected text: %v", err)
	}

	if !fm.CorrectedTextExists(audioFilePath) {
		t.Error("Corrected text file should exist")
	}

	retrievedCorrected, err := fm.GetCorrectedText(audioFilePath)
	if err != nil {
		t.Errorf("Failed to get corrected text: %v", err)
	}

	if retrievedCorrected != correctedText {
		t.Errorf("Retrieved corrected text doesn't match. Expected: %s, Got: %s", correctedText, retrievedCorrected)
	}
}

func TestFileManagerPaths(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "correction_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := &config.Config{
		OutputDir: tempDir,
	}

	fm := NewFileManager(cfg)

	tests := []struct {
		name          string
		audioPath     string
		expectedRaw   string
		expectedCorrected string
	}{
		{
			name:          "simple_filename",
			audioPath:     "/path/to/file.m4b",
			expectedRaw:   filepath.Join(tempDir, "raw", "file.raw.txt"),
			expectedCorrected: filepath.Join(tempDir, "corrected", "file.corrected.txt"),
		},
		{
			name:          "complex_filename",
			audioPath:     "/path/to/book_chapter_01.mp3",
			expectedRaw:   filepath.Join(tempDir, "raw", "book_chapter_01.raw.txt"),
			expectedCorrected: filepath.Join(tempDir, "corrected", "book_chapter_01.corrected.txt"),
		},
		{
			name:          "no_extension",
			audioPath:     "/path/to/audiofile",
			expectedRaw:   filepath.Join(tempDir, "raw", "audiofile.raw.txt"),
			expectedCorrected: filepath.Join(tempDir, "corrected", "audiofile.corrected.txt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawPath := fm.getRawTextPath(tt.audioPath)
			correctedPath := fm.getCorrectedTextPath(tt.audioPath)

			if rawPath != tt.expectedRaw {
				t.Errorf("Raw path mismatch. Expected: %s, Got: %s", tt.expectedRaw, rawPath)
			}

			if correctedPath != tt.expectedCorrected {
				t.Errorf("Corrected path mismatch. Expected: %s, Got: %s", tt.expectedCorrected, correctedPath)
			}
		})
	}
}

func TestFileManagerCleanup(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "correction_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := &config.Config{
		OutputDir: tempDir,
	}

	fm := NewFileManager(cfg)

	// Create some test files
	validAudioFiles := []string{
		"/path/to/book1_chapter1.m4b",
		"/path/to/book1_chapter2.m4b",
	}

	orphanedAudioFiles := []string{
		"/path/to/old_book_chapter1.m4b",
		"/path/to/deleted_book.m4b",
	}

	// Save text files for both valid and orphaned audio files
	allFiles := append(validAudioFiles, orphanedAudioFiles...)
	for _, audioFile := range allFiles {
		if err := fm.SaveRawText(audioFile, "test content"); err != nil {
			t.Errorf("Failed to save raw text: %v", err)
		}
		if err := fm.SaveCorrectedText(audioFile, "corrected content"); err != nil {
			t.Errorf("Failed to save corrected text: %v", err)
		}
	}

	// Verify all files exist before cleanup
	for _, audioFile := range allFiles {
		if !fm.RawTextExists(audioFile) {
			t.Errorf("Raw text should exist for %s", audioFile)
		}
		if !fm.CorrectedTextExists(audioFile) {
			t.Errorf("Corrected text should exist for %s", audioFile)
		}
	}

	// Run cleanup with only valid audio files
	if err := fm.CleanupOldFiles(validAudioFiles); err != nil {
		t.Errorf("Cleanup failed: %v", err)
	}

	// Verify valid files still exist
	for _, audioFile := range validAudioFiles {
		if !fm.RawTextExists(audioFile) {
			t.Errorf("Valid raw text should still exist for %s", audioFile)
		}
		if !fm.CorrectedTextExists(audioFile) {
			t.Errorf("Valid corrected text should still exist for %s", audioFile)
		}
	}

	// Verify orphaned files were removed
	for _, audioFile := range orphanedAudioFiles {
		if fm.RawTextExists(audioFile) {
			t.Errorf("Orphaned raw text should be removed for %s", audioFile)
		}
		if fm.CorrectedTextExists(audioFile) {
			t.Errorf("Orphaned corrected text should be removed for %s", audioFile)
		}
	}
}