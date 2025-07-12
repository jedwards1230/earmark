package transcribe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
)

type Transcriber struct {
	config *config.Config
	log    log.Logger
}

func NewTranscriber(cfg *config.Config) *Transcriber {
	logger := log.NewLogger("transcribe")

	if err := checkDependencies(); err != nil {
		logger.Error("Failed checking dependencies", "error", err)
		os.Exit(1)
	}

	// Clear cache directory on startup
	if err := clearCacheDir(cfg.CacheDir); err != nil {
		logger.Error("Failed clearing cache directory", "error", err)
		os.Exit(1)
	}

	return &Transcriber{
		config: cfg,
		log:    logger,
	}
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

func (t *Transcriber) TranscribeAudio(
	ctx context.Context,
	audioFilePath string,
	fileMeta *meta.FileMetadata,
) (string, error) {
	shortPath := shortenPath(audioFilePath)
	startTime := time.Now()

	t.log.Info("Starting audio preprocessing", "file", shortPath)

	// Preprocess audio
	preprocessedPath, err := t.preprocessAudio(audioFilePath)
	if err != nil {
		t.log.Error("Preprocessing failed", "file", shortPath, "error", err)
		return "", err
	}
	defer os.Remove(preprocessedPath)

	inputSize, err := getInputSize(preprocessedPath)
	if err != nil {
		return "", err
	}

	// Create output directory in the raw subdirectory
	rawDir := filepath.Join(t.config.OutputDir, "raw")
	err = os.MkdirAll(rawDir, 0755)
	if err != nil {
		t.log.Error("Failed to create raw output directory", "file", shortPath, "error", err)
		return "", err
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()

	t.log.Info("Starting transcription")

	outputFile := filepath.Join(rawDir, strings.TrimSuffix(filepath.Base(preprocessedPath), filepath.Ext(preprocessedPath))+".txt")

	cmd := exec.CommandContext(ctx,
		"yap",
		"transcribe",
		preprocessedPath,
		"--output-file", outputFile,
		// "--initial_prompt", fmt.Sprintf("Transcribe %s by %s", fileMeta.Title, fileMeta.Author),
	)

	// Create pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.log.Error("Failed to create stdout pipe", "file", shortPath, "error", err)
		return "", err
	}

	// Create a pipe for stderr that we can both read from and write to
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.log.Error("Failed to create stderr pipe", "file", shortPath, "error", err)
		return "", err
	}

	// Create a buffer for capturing stderr
	var stderr bytes.Buffer

	// Set up command stderr
	cmd.Stderr = stderrWriter

	// Add memory monitoring
	memStatChan := make(chan struct{})
	if t.config.Debug {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					var mem runtime.MemStats
					runtime.ReadMemStats(&mem)
					t.log.Debug("Memory stats",
						"alloc", formatFileSize(int64(mem.Alloc)),
						"sys", formatFileSize(int64(mem.Sys)),
						"num_gc", mem.NumGC)
				case <-memStatChan:
					return
				}
			}
		}()
	}

	// Start the command with detailed error capture
	if err := cmd.Start(); err != nil {
		close(memStatChan)
		stderrReader.Close()
		stderrWriter.Close()
		t.log.Error("Failed to start transcription", "file", shortPath, "error", err)
		return "", fmt.Errorf("failed to start transcription process: %v", err)
	}

	// Create channels to signal when output processing is done
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Process stdout in real-time
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			t.log.Verbose(scanner.Text())
		}
		close(stdoutDone)
	}()

	// Process stderr in real-time while also capturing it
	go func() {
		tee := io.TeeReader(stderrReader, &stderr)
		scanner := bufio.NewScanner(tee)
		for scanner.Scan() {
			t.log.Verbose(scanner.Text())
		}
		close(stderrDone)
	}()

	// Handle process cleanup on context cancellation
	go func() {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			t.log.Warn("Transcription timeout", "file", shortPath)
		}
		// Kill process group
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()

	// Wait for command to complete and output processing to finish
	err = cmd.Wait()
	close(memStatChan)
	stderrWriter.Close() // Close writer after command exits

	// Wait for output processors to finish
	<-stdoutDone
	<-stderrDone
	stderrReader.Close() // Close reader last

	if err != nil {
		errMsg := fmt.Sprintf("Transcription failed for %s: %v", shortPath, err)
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.log.Error("Transcription process failed",
				"file", shortPath,
				"exit_code", exitErr.ExitCode(),
				"signal", exitErr.Sys(),
				"stderr", stderr.String())
		}
		return "", errors.New(errMsg)
	}

	// Verify output file exists
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		t.log.Error("Transcription failed: output file not created", "file", shortPath)
		return "", err
	}

	if _, err := os.Stat(audioFilePath); os.IsNotExist(err) {
		t.log.Error("Audio file not found", "file", audioFilePath)
		return "", err
	}

	// Get word count from output file
	wordCount, err := countWords(outputFile)
	if err != nil {
		t.log.Error("Failed to count words", "file", shortPath, "error", err)
		return "", err
	}
	duration := time.Since(startTime)

	// Calculate speed in KB/s instead of MB/s
	kbPerSec := float64(inputSize) / 1024 / duration.Seconds()
	wordsPerSec := float64(wordCount) / duration.Seconds()

	t.log.Info("Transcription completed",
		"file", shortPath,
		"duration", formatDuration(duration),
		"speed_kb_sec", fmt.Sprintf("%.1f", kbPerSec),
		"words_sec", fmt.Sprintf("%.0f", wordsPerSec))

	return outputFile, nil
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

	// Check ffmpeg with version
	ffmpegCmd := exec.Command("ffmpeg", "-version")
	_, err := ffmpegCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg is not installed: %w", err)
	}

	return nil
}

func getInputSize(filepath string) (int64, error) {
	fileInfo, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}

	inputSize := fileInfo.Size()
	return inputSize, nil
}

func clearCacheDir(cacheDir string) error {
	if err := os.RemoveAll(cacheDir); err != nil {
		return err
	}
	return os.MkdirAll(cacheDir, 0755)
}

func (t *Transcriber) preprocessAudio(audioFilePath string) (string, error) {
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

	if err := RunFfmpegCmd(
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
	); err != nil {
		return "", err
	}

	fileInfo, err := os.Stat(cachedPath)
	if err != nil {
		return "", err
	}
	processedSize := fileInfo.Size()

	// If processed file is larger, try with even more aggressive compression
	if processedSize > originalSize {
		if err := RunFfmpegCmd(
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
		); err != nil {
			return "", err
		}

		fileInfo, err = os.Stat(cachedPath)
		if err != nil {
			return "", err
		}
		processedSize = fileInfo.Size()
	}

	reductionPct := float64(originalSize-processedSize) / float64(originalSize) * 100
	t.log.Info("Audio conversion completed",
		"original_size", formatFileSize(originalSize),
		"processed_size", formatFileSize(processedSize),
		"reduction_percent", int(reductionPct))

	if processedSize > 500*1024*1024 {
		t.log.Warn("Output file exceeds size limit", "size", formatFileSize(processedSize))
	}

	if t.config.Debug {
		t.log.Debug("FFmpeg output file created", "path", cachedPath)
	}

	return cachedPath, nil
}

func RunFfmpegCmd(args ...string) error {
	cmd := exec.Command("ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg error: %w\n%s", err, stderr.String())
	}
	return nil
}
