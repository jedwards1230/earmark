package transcribe

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/queue"
	"transcriber/internal/state"
)

// Create custom logger without timestamps
var customLog = log.New(os.Stdout, "", 0)

func init() {
	// Replace default logger with custom logger
	log.SetFlags(0)
}

type Transcriber struct {
	config       *config.Config
	stateManager *state.StateManager
	queue        *queue.Queue
	done         chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewTranscriber(cfg *config.Config, sm *state.StateManager, q *queue.Queue) *Transcriber {
	if err := checkDependencies(); err != nil {
		log.Fatalf("Error checking dependencies: %v", err)
	}

	// Clear cache directory on startup
	if err := clearCacheDir(cfg.CacheDir); err != nil {
		log.Fatalf("Error clearing cache directory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Transcriber{
		config:       cfg,
		stateManager: sm,
		queue:        q,
		done:         make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (t *Transcriber) StartWorker() {
	customLog.Println("Starting worker...")
	for {
		select {
		case <-t.ctx.Done():
			customLog.Println("Transcriber context canceled, exiting worker.")
			close(t.done)
			return
		default:
			audioFilePath, ok := t.queue.Dequeue()
			if !ok {
				customLog.Println("Queue shutdown signal received, exiting worker.")
				close(t.done)
				return
			}
			if audioFilePath == "" {
				// No work available, wait a bit before checking again
				time.Sleep(time.Second)
				continue
			}

			// Process files sequentially
			t.transcribeAudio(audioFilePath)
			customLog.Printf("Finished processing file: %s", audioFilePath)
		}
	}
}

func (t *Transcriber) getRelativePath(audioFilePath string) string {
	// Find the "audiobooks" directory in the path
	audioBooksIndex := bytes.LastIndex([]byte(audioFilePath), []byte("/audiobooks/"))
	if audioBooksIndex == -1 {
		return "" // Return empty if "audiobooks" not found
	}

	// Get everything after "audiobooks/"
	relativePath := audioFilePath[audioBooksIndex+len("/audiobooks/"):]
	// Get the directory part of the relative path
	lastSlash := bytes.LastIndex([]byte(relativePath), []byte("/"))
	if lastSlash == -1 {
		return ""
	}
	return relativePath[:lastSlash]
}

func (t *Transcriber) ensureOutputDir(relativePath string) (string, error) {
	fullOutputDir := t.config.OutputDir
	if relativePath != "" {
		fullOutputDir = fmt.Sprintf("%s/%s", t.config.OutputDir, relativePath)
	}

	err := os.MkdirAll(fullOutputDir, 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create output directory: %v", err)
	}
	return fullOutputDir, nil
}

func formatFileSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func shortenPath(path string) string {
	// Remove common prefixes like absolute paths and 'audiobooks/'
	if idx := strings.Index(path, "/audiobooks/"); idx != -1 {
		return path[idx+len("/audiobooks/"):]
	}
	return filepath.Base(path)
}

func (t *Transcriber) transcribeAudio(audioFilePath string) {
	shortPath := shortenPath(audioFilePath)

	if t.stateManager.IsProcessed(audioFilePath) {
		customLog.Printf("Skipping already processed: %s", shortPath)
		return
	}

	// Preprocess audio
	preprocessedPath, err := t.preprocessAudio(audioFilePath)
	if err != nil {
		customLog.Printf("Failed to preprocess: %s (%v)", shortPath, err)
		return
	}
	defer os.Remove(preprocessedPath)

	fileInfo, err := os.Stat(preprocessedPath)
	if err != nil {
		customLog.Printf("Failed to get file info: %s (%v)", shortPath, err)
		return
	}

	customLog.Printf("Processing: %s (%s)", shortPath, formatFileSize(fileInfo.Size()))

	// Get the relative path and create output directory
	relativePath := t.getRelativePath(audioFilePath)
	outputDir, err := t.ensureOutputDir(relativePath)
	if err != nil {
		customLog.Printf("Failed to create output directory: %v", err)
		return
	}

	threads := os.Getenv("WHISPER_THREADS")
	if threads == "" {
		threads = "1"
	}

	cmd := exec.Command(
		"whisper-ctranslate2",
		preprocessedPath,
		"--model", t.config.WhisperModel,
		"--compute_type", "float32",
		"--language", "en",
		"--beam_size", "5",
		"--output_dir", outputDir,
		"--model_dir", t.config.ModelsDir,
		"--output_format", "txt",
		"--vad_filter", "True", // Voice Activity Detection - On by default when batched is True
		"--batched", "True",
		"--batch_size", "16",
		"--threads", threads,
		"--initial_prompt", "", // https://cookbook.openai.com/examples/whisper_prompting_guide
		"--verbose", fmt.Sprintf("%v", t.config.Debug),
	)

	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		customLog.Printf("Transcription failed: %s (%v)", shortPath, err)
		return
	}

	if _, err := os.Stat(audioFilePath); os.IsNotExist(err) {
		customLog.Printf("Audio file not found: %s", audioFilePath)
		return
	}

	if err := t.stateManager.MarkProcessed(audioFilePath); err != nil {
		customLog.Printf("Failed to mark as processed: %s (%v)", shortPath, err)
		return
	}

	customLog.Printf("✓ Completed: %s", shortPath)
}

func checkDependencies() error {
	if err := checkAndInstallWhisperCtranslate2(); err != nil {
		return fmt.Errorf("whisper-ctranslate2 check failed: %w", err)
	}

	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg is not installed: %w", err)
	}

	return nil
}

func clearCacheDir(cacheDir string) error {
	if err := os.RemoveAll(cacheDir); err != nil {
		return err
	}
	return os.MkdirAll(cacheDir, 0755)
}

func (t *Transcriber) preprocessAudio(audioFilePath string) (string, error) {
	shortPath := shortenPath(audioFilePath)

	// Get original file size
	originalInfo, err := os.Stat(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to get original file info: %w", err)
	}
	originalSize := originalInfo.Size()

	// Compute relative path from AudioDir
	relPath, err := filepath.Rel(t.config.AudioDir, audioFilePath)
	if err != nil {
		return "", fmt.Errorf("unable to determine relative path: %w", err)
	}

	// Build cached file path using the same structure and filename
	cachedPath := filepath.Join(t.config.CacheDir, relPath)
	if err := os.MkdirAll(filepath.Dir(cachedPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directories: %w", err)
	}

	// Improved ffmpeg settings for better compression
	cmd := exec.Command(
		"ffmpeg",
		"-i", audioFilePath,
		"-c:a", "libmp3lame",
		"-b:a", "32k", // Reduced bitrate
		"-ac", "1", // Mono
		"-ar", "16000", // Sample rate
		"-compression_level", "9", // Max compression
		"-y", // Overwrite output files
		cachedPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to preprocess audio: %w\nffmpeg error: %s", err, stderr.String())
	}

	// Check processed file size
	fileInfo, err := os.Stat(cachedPath)
	if err != nil {
		return "", err
	}
	processedSize := fileInfo.Size()

	// If processed file is larger, try with even more aggressive compression
	if processedSize > originalSize {
		cmd = exec.Command(
			"ffmpeg",
			"-i", audioFilePath,
			"-c:a", "libmp3lame",
			"-b:a", "24k", // Even lower bitrate
			"-ac", "1", // Mono
			"-ar", "16000", // Sample rate
			"-compression_level", "9",
			"-y",
			cachedPath,
		)

		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("failed to reprocess audio: %w", err)
		}

		fileInfo, err = os.Stat(cachedPath)
		if err != nil {
			return "", err
		}
		processedSize = fileInfo.Size()
	}

	// Calculate size reduction percentage
	reductionPct := float64(originalSize-processedSize) / float64(originalSize) * 100

	customLog.Printf("Converting: %s", shortPath)
	customLog.Printf("  Size: %s → %s (%+.1f%%)",
		formatFileSize(originalSize),
		formatFileSize(processedSize),
		reductionPct)

	if processedSize > 500*1024*1024 {
		customLog.Printf("  Warning: Output still exceeds 500MB")
	}

	if t.config.Debug {
		customLog.Printf("ffmpeg output file: %s", cachedPath)
	}

	return cachedPath, nil
}

func checkAndInstallWhisperCtranslate2() error {
	// Check if whisper-ctranslate2 is installed
	_, err := exec.LookPath("whisper-ctranslate2")
	if err == nil {
		return nil
	}

	// Attempt to install whisper-ctranslate2 using pip
	customLog.Println("Installing whisper-ctranslate2...")
	cmd := exec.Command("pip", "install", "-U", "whisper-ctranslate2")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install whisper-ctranslate2: %v", err)
	}

	return nil
}
