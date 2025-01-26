package transcribe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"transcriber/internal/config"
	"transcriber/internal/meta"
)

func init() {
	// Replace default logger with custom logger
	log.SetFlags(0)
}

type Transcriber struct {
	config *config.Config
	log    *log.Logger
}

func NewTranscriber(cfg *config.Config) *Transcriber {
	logger := log.New(os.Stdout, "(transcribe) ", 0)

	if err := checkDependencies(); err != nil {
		logger.Fatalf("Error checking dependencies: %v", err)
	}

	// Clear cache directory on startup
	if err := clearCacheDir(cfg.CacheDir); err != nil {
		logger.Fatalf("Error clearing cache directory: %v", err)
	}

	return &Transcriber{
		config: cfg,
		log:    logger,
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

func (t *Transcriber) TranscribeAudio(
	ctx context.Context,
	audioFilePath string,
	fileMeta *meta.FileMetadata,
	threads int,
	computeType string,
) (string, error) {
	shortPath := shortenPath(audioFilePath)
	startTime := time.Now()

	t.log.Println("Preprocessing audio...")

	// Preprocess audio
	preprocessedPath, err := t.preprocessAudio(audioFilePath)
	if err != nil {
		t.log.Printf("Preprocessing failed: %v", err)
		return "", err
	}
	defer os.Remove(preprocessedPath)

	inputSize, err := getInputSize(preprocessedPath)
	if err != nil {
		return "", err
	}

	// Get the relative path and create output directory
	relativePath := t.getRelativePath(audioFilePath)
	outputDir, err := t.ensureOutputDir(relativePath)
	if err != nil {
		t.log.Printf("Failed to create output directory: %v", err)
		return "", err
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()

	t.log.Println("Transcribing...")
	t.log.Println("Running whisper-ctranslate2 with:")
	t.log.Printf("  - Model: %s\n", t.config.WhisperModel)
	t.log.Printf("  - Compute Type: %s\n", computeType)
	t.log.Printf("  - Threads: %d\n", threads)

	cmd := exec.CommandContext(ctx,
		"whisper-ctranslate2",
		preprocessedPath,
		"--model", t.config.WhisperModel,
		"--compute_type", computeType,
		"--language", "en",
		"--beam_size", "5",
		"--output_dir", outputDir,
		"--model_dir", t.config.ModelsDir,
		"--output_format", "txt",
		"--vad_filter", "True", // Voice Activity Detection - On by default when batched is True
		"--batched", "True",
		"--batch_size", "16",
		"--threads", strconv.Itoa(threads),
		"--initial_prompt", fmt.Sprintf("Transcribe %s by %s", fileMeta.Title, fileMeta.Author), // https://cookbook.openai.com/examples/whisper_prompting_guide
		"--verbose", fmt.Sprintf("%v", t.config.Debug),
	)

	// Create pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.log.Printf("Failed to create stdout pipe: %v", err)
		return "", err
	}

	// Create a pipe for stderr that we can both read from and write to
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.log.Printf("Failed to create stderr pipe: %v", err)
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
					t.log.Printf("Memory stats - Alloc: %s, Sys: %s, NumGC: %d",
						formatFileSize(int64(mem.Alloc)),
						formatFileSize(int64(mem.Sys)),
						mem.NumGC)
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
		t.log.Printf("Failed to start transcription: %s (%v)", shortPath, err)
		return "", fmt.Errorf("failed to start transcription process: %v", err)
	}

	// Create channels to signal when output processing is done
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Process stdout in real-time
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			t.log.Printf("%s", scanner.Text())
		}
		close(stdoutDone)
	}()

	// Process stderr in real-time while also capturing it
	go func() {
		tee := io.TeeReader(stderrReader, &stderr)
		scanner := bufio.NewScanner(tee)
		for scanner.Scan() {
			t.log.Printf("%s", scanner.Text())
		}
		close(stderrDone)
	}()

	// Handle process cleanup on context cancellation
	go func() {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			t.log.Printf("Transcription timeout for: %s", shortPath)
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
			errMsg += fmt.Sprintf("\nExit Code: %d", exitErr.ExitCode())
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				errMsg += fmt.Sprintf("\nSignal: %v", status.Signal())
				if status.Signaled() {
					errMsg += fmt.Sprintf("\nTerminated by signal: %v", status.Signal())
				}
			}
		}
		errMsg += fmt.Sprintf("\nStderr Output:\n%s", stderr.String())
		t.log.Print(errMsg)
		return "", errors.New(errMsg)
	}

	// Verify output file exists
	outputFile := filepath.Join(outputDir, strings.TrimSuffix(filepath.Base(preprocessedPath), filepath.Ext(preprocessedPath))+".txt")
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		t.log.Printf("Transcription failed: output file not created for %s", shortPath)
		return "", err
	}

	if _, err := os.Stat(audioFilePath); os.IsNotExist(err) {
		t.log.Printf("Audio file not found: %s", audioFilePath)
		return "", err
	}

	// Get word count from output file
	wordCount, err := countWords(outputFile)
	if err != nil {
		t.log.Printf("Failed to count words: %s (%v)", shortPath, err)
		return "", err
	}
	duration := time.Since(startTime)

	// Calculate speed in KB/s instead of MB/s
	kbPerSec := float64(inputSize) / 1024 / duration.Seconds()
	wordsPerSec := float64(wordCount) / duration.Seconds()

	t.log.Printf("Done: %s [%s | %.1fKB/s | %.0fw/s]",
		shortPath,
		formatDuration(duration),
		kbPerSec,
		wordsPerSec,
	)

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
	t.log.Printf("Converted audio: %s → %s (-%d%%)",
		formatFileSize(originalSize),
		formatFileSize(processedSize),
		int(reductionPct),
	)

	if processedSize > 500*1024*1024 {
		t.log.Printf("Output exceeds 500MB")
	}

	if t.config.Debug {
		t.log.Printf("ffmpeg output file: %s", cachedPath)
	}

	return cachedPath, nil
}

func checkAndInstallWhisperCtranslate2() error {
	// Try to get version first
	if err := RunWhisperCTranslate2Cmd("--version"); err == nil {
		return nil
	}

	// Attempt to install whisper-ctranslate2 using pip
	fmt.Println("Installing whisper-ctranslate2...")
	installCmd := exec.Command("pip", "install", "-U", "whisper-ctranslate2")

	var stderr bytes.Buffer
	installCmd.Stderr = &stderr

	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("failed to install whisper-ctranslate2: %v\nError output: %s", err, stderr.String())
	}

	// Verify installation
	if err := RunWhisperCTranslate2Cmd("--version"); err != nil {
		return fmt.Errorf("whisper-ctranslate2 installation verification failed: %w", err)
	}

	return nil
}

func RunWhisperCTranslate2Cmd(args ...string) error {
	cmd := exec.Command("whisper-ctranslate2", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("whisper-ctranslate2 error: %w\n%s", err, stderr.String())
	}
	return nil
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
