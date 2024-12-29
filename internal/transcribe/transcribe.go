package transcribe

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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
	// Get relative path from configured AudioDir
	relPath, err := filepath.Rel(t.config.AudioDir, audioFilePath)
	if err != nil {
		return "" // Return empty if we can't determine relative path
	}
	return filepath.Dir(relPath)
}

func (t *Transcriber) ensureOutputDir(relativePath string) (string, error) {
	fullOutputDir := t.config.OutputDir
	if relativePath != "" {
		fullOutputDir = filepath.Join(t.config.OutputDir, relativePath)
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
	// Use filepath.Base to get just the filename
	return filepath.Base(path)
}

func countWords(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`\S+`)
	return len(re.FindAll(content, -1)), nil
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (t *Transcriber) transcribeAudio(audioFilePath string) {
	shortPath := shortenPath(audioFilePath)
	startTime := time.Now()

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

	inputSize := fileInfo.Size()
	customLog.Printf("▶ Processing: %s (%s)", shortPath, formatFileSize(inputSize))

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

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(t.ctx, 2*time.Hour)
	defer cancel()

	cmd := exec.CommandContext(ctx,
		"whisper-ctranslate2",
		preprocessedPath,
		"--model", t.config.WhisperModel,
		"--compute_type", "int8",
		"--language", "en",
		"--beam_size", "5",
		"--output_dir", outputDir,
		"--model_dir", t.config.ModelsDir,
		"--output_format", "txt",
		"--vad_filter", "True", // Voice Activity Detection - On by default when batched is True
		"--batched", "True",
		"--batch_size", "16",
		"--threads", threads,
		"--initial_prompt", fmt.Sprintf("Transcribe from %s", shortPath), // https://cookbook.openai.com/examples/whisper_prompting_guide
		"--verbose", fmt.Sprintf("%v", t.config.Debug),
	)

	// Create pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		customLog.Printf("Failed to create stdout pipe: %v", err)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		customLog.Printf("Failed to create stderr pipe: %v", err)
		return
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		customLog.Printf("Failed to start transcription: %s (%v)", shortPath, err)
		return
	}

	// Create channels to signal when output processing is done
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Process stdout in real-time
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			customLog.Printf("Whisper stdout: %s", scanner.Text())
		}
		close(stdoutDone)
	}()

	// Process stderr in real-time
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			customLog.Printf("Whisper stderr: %s", scanner.Text())
		}
		close(stderrDone)
	}()

	// Handle process cleanup on context cancellation
	go func() {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			customLog.Printf("Transcription timeout for: %s", shortPath)
		}
		// Kill process group
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()

	// Wait for command to complete and output processing to finish
	err = cmd.Wait()
	<-stdoutDone
	<-stderrDone

	if err != nil {
		customLog.Printf("Transcription failed: %s (%v)", shortPath, err)
		return
	}

	// Verify output file exists
	outputFile := filepath.Join(outputDir, strings.TrimSuffix(filepath.Base(preprocessedPath), filepath.Ext(preprocessedPath))+".txt")
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		customLog.Printf("Transcription failed: output file not created for %s", shortPath)
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

	// Get word count from output file
	wordCount, err := countWords(outputFile)
	if err != nil {
		customLog.Printf("Failed to count words: %s (%v)", shortPath, err)
		return
	}
	duration := time.Since(startTime)

	// Calculate speed in KB/s instead of MB/s
	kbPerSec := float64(inputSize) / 1024 / duration.Seconds()
	wordsPerSec := float64(wordCount) / duration.Seconds()

	customLog.Printf("✓ Done: %s [%s | %.1fKB/s | %.0fw/s]",
		shortPath,
		formatDuration(duration),
		kbPerSec,
		wordsPerSec,
	)
}

func checkDependencies() error {
	// Check for Python
	pythonCmd := exec.Command("python3", "--version")
	if err := pythonCmd.Run(); err != nil {
		pythonCmd = exec.Command("python", "--version")
		if err := pythonCmd.Run(); err != nil {
			return fmt.Errorf("python not found: %w", err)
		}
	}

	// Check for pip
	pipCmd := exec.Command("pip3", "--version")
	if err := pipCmd.Run(); err != nil {
		pipCmd = exec.Command("pip", "--version")
		if err := pipCmd.Run(); err != nil {
			return fmt.Errorf("pip not found: %w", err)
		}
	}

	// Check whisper-ctranslate2
	if err := checkAndInstallWhisperCtranslate2(); err != nil {
		return fmt.Errorf("whisper-ctranslate2 check failed: %w", err)
	}

	// Check ffmpeg with version
	ffmpegCmd := exec.Command("ffmpeg", "-version")
	_, err := ffmpegCmd.CombinedOutput()
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

	// Build cached file path using the same structure but with .mp3 extension
	cachedPath := filepath.Join(t.config.CacheDir, strings.TrimSuffix(relPath, filepath.Ext(relPath))+".mp3")
	if err := os.MkdirAll(filepath.Dir(cachedPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directories: %w", err)
	}

	// Only process audio stream, ignore video/cover art
	cmd := exec.Command(
		"ffmpeg",
		"-i", audioFilePath,
		"-vn", // Skip video streams
		"-c:a", "libmp3lame",
		"-b:a", "32k", // Reduced bitrate
		"-ac", "1", // Mono
		"-ar", "16000", // Sample rate
		"-compression_level", "9", // Max compression
		"-map", "0:a:0", // Only map the first audio stream
		"-f", "mp3", // Force MP3 format
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
			"-vn", // Skip video streams
			"-c:a", "libmp3lame",
			"-b:a", "24k", // Even lower bitrate
			"-ac", "1", // Mono
			"-ar", "16000", // Sample rate
			"-compression_level", "9",
			"-map", "0:a:0", // Only map the first audio stream
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

	reductionPct := float64(originalSize-processedSize) / float64(originalSize) * 100
	customLog.Printf("⚡ Converting: %s (%s → %s | -%d%%)",
		shortPath,
		formatFileSize(originalSize),
		formatFileSize(processedSize),
		int(reductionPct),
	)

	if processedSize > 500*1024*1024 {
		customLog.Printf("  ⚠ Output exceeds 500MB")
	}

	if t.config.Debug {
		customLog.Printf("ffmpeg output file: %s", cachedPath)
	}

	return cachedPath, nil
}

func checkAndInstallWhisperCtranslate2() error {
	// Try to get version first
	versionCmd := exec.Command("whisper-ctranslate2", "--version")
	if err := versionCmd.Run(); err == nil {
		return nil
	}

	// Attempt to install whisper-ctranslate2 using pip
	customLog.Println("Installing whisper-ctranslate2...")
	installCmd := exec.Command("pip", "install", "-U", "whisper-ctranslate2")

	var stderr bytes.Buffer
	installCmd.Stderr = &stderr

	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("failed to install whisper-ctranslate2: %v\nError output: %s", err, stderr.String())
	}

	// Verify installation
	versionCmd = exec.Command("whisper-ctranslate2", "--version")
	if err := versionCmd.Run(); err != nil {
		return fmt.Errorf("whisper-ctranslate2 installation verification failed: %w", err)
	}

	return nil
}
