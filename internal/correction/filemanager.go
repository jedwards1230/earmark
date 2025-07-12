package correction

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
)

type FileManager struct {
	outputDir    string
	rawDir       string
	correctedDir string
	log          log.Logger
}

func NewFileManager(cfg *config.Config) *FileManager {
	logger := log.NewLogger("correction-files")

	fm := &FileManager{
		outputDir: cfg.OutputDir,
		log:       logger,
	}

	// Create subdirectories for raw and corrected text
	fm.rawDir = filepath.Join(cfg.OutputDir, "raw")
	fm.correctedDir = filepath.Join(cfg.OutputDir, "corrected")

	// Ensure directories exist
	if err := fm.ensureDirectories(); err != nil {
		logger.Error("Failed to create correction directories", "error", err)
	}

	return fm
}

func (fm *FileManager) ensureDirectories() error {
	dirs := []string{fm.rawDir, fm.correctedDir}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	fm.log.Debug("Created correction directories",
		"raw_dir", fm.rawDir,
		"corrected_dir", fm.correctedDir)

	return nil
}

func (fm *FileManager) SaveRawText(audioFilePath, content string) error {
	outputPath := fm.getRawTextPath(audioFilePath)
	return fm.saveTextFile(outputPath, content)
}

func (fm *FileManager) SaveCorrectedText(audioFilePath, content string) error {
	outputPath := fm.getCorrectedTextPath(audioFilePath)
	return fm.saveTextFile(outputPath, content)
}

func (fm *FileManager) GetRawText(audioFilePath string) (string, error) {
	outputPath := fm.getRawTextPath(audioFilePath)
	return fm.readTextFile(outputPath)
}

func (fm *FileManager) GetCorrectedText(audioFilePath string) (string, error) {
	outputPath := fm.getCorrectedTextPath(audioFilePath)
	return fm.readTextFile(outputPath)
}

func (fm *FileManager) RawTextExists(audioFilePath string) bool {
	outputPath := fm.getRawTextPath(audioFilePath)
	_, err := os.Stat(outputPath)
	return err == nil
}

func (fm *FileManager) CorrectedTextExists(audioFilePath string) bool {
	outputPath := fm.getCorrectedTextPath(audioFilePath)
	_, err := os.Stat(outputPath)
	return err == nil
}

func (fm *FileManager) getRawTextPath(audioFilePath string) string {
	return fm.getTextPath(fm.rawDir, audioFilePath, ".raw.txt")
}

func (fm *FileManager) getCorrectedTextPath(audioFilePath string) string {
	return fm.getTextPath(fm.correctedDir, audioFilePath, ".corrected.txt")
}

func (fm *FileManager) getTextPath(baseDir, audioFilePath, suffix string) string {
	// Extract the filename without extension
	filename := filepath.Base(audioFilePath)

	// Remove audio file extension
	ext := filepath.Ext(filename)
	filenameWithoutExt := strings.TrimSuffix(filename, ext)

	// Create text filename with suffix
	textFilename := filenameWithoutExt + suffix

	return filepath.Join(baseDir, textFilename)
}

func (fm *FileManager) saveTextFile(filePath, content string) error {
	// Ensure parent directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Write content to file
	if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	fm.log.Debug("Saved text file", "path", filePath, "size", len(content))
	return nil
}

func (fm *FileManager) readTextFile(filePath string) (string, error) {
	// #nosec G304 - filePath is controlled by caller and validated elsewhere
	content, err := os.ReadFile(filepath.Clean(filePath))
	if err != nil {
		return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	fm.log.Debug("Read text file", "path", filePath, "size", len(content))
	return string(content), nil
}

// CleanupOldFiles removes text files that don't have corresponding audio files
func (fm *FileManager) CleanupOldFiles(validAudioFiles []string) error {
	validPaths := make(map[string]bool)

	// Create a map of valid text file paths
	for _, audioFile := range validAudioFiles {
		validPaths[fm.getRawTextPath(audioFile)] = true
		validPaths[fm.getCorrectedTextPath(audioFile)] = true
	}

	// Clean up both directories
	dirs := []string{fm.rawDir, fm.correctedDir}

	for _, dir := range dirs {
		if err := fm.cleanupDirectory(dir, validPaths); err != nil {
			return fmt.Errorf("failed to cleanup directory %s: %w", dir, err)
		}
	}

	return nil
}

func (fm *FileManager) cleanupDirectory(dir string, validPaths map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Directory doesn't exist, nothing to clean
		}
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	var deletedCount int

	for _, entry := range entries {
		if entry.IsDir() {
			continue // Skip subdirectories
		}

		filePath := filepath.Join(dir, entry.Name())

		if !validPaths[filePath] {
			if err := os.Remove(filePath); err != nil {
				fm.log.Warn("Failed to remove orphaned file", "path", filePath, "error", err)
			} else {
				fm.log.Debug("Removed orphaned file", "path", filePath)
				deletedCount++
			}
		}
	}

	if deletedCount > 0 {
		fm.log.Info("Cleaned up orphaned files", "directory", dir, "count", deletedCount)
	}

	return nil
}
