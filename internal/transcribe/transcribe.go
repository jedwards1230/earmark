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
	"strconv"
	"strings"
	"syscall"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/meta"
)

// Create custom logger without timestamps
var customLog = log.New(os.Stdout, "", 0)

func init() {
	// Replace default logger with custom logger
	log.SetFlags(0)
}

type Transcriber struct {
	config *config.Config
}

func NewTranscriber(cfg *config.Config) *Transcriber {
	if err := checkDependencies(); err != nil {
		log.Fatalf("Error checking dependencies: %v", err)
	}

	// Clear cache directory on startup
	if err := clearCacheDir(cfg.CacheDir); err != nil {
		log.Fatalf("Error clearing cache directory: %v", err)
	}

	return &Transcriber{
		config: cfg,
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

	customLog.Println("Preprocessing audio...")

	// Preprocess audio
	preprocessedPath, err := t.preprocessAudio(audioFilePath)
	if err != nil {
		customLog.Printf("Preprocessing failed: %v", err)
		return "", err
	}
	defer os.Remove(preprocessedPath)

	inputSize, err := getInputSize(audioFilePath)
	if err != nil {
		return "", err
	}

	// Get the relative path and create output directory
	relativePath := t.getRelativePath(audioFilePath)
	outputDir, err := t.ensureOutputDir(relativePath)
	if err != nil {
		customLog.Printf("Failed to create output directory: %v", err)
		return "", err
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()

	fmt.Println("Transcribing...")
	fmt.Println("Running whisper-ctranslate2 with:")
	fmt.Printf("  - Model: %s\n", t.config.WhisperModel)
	fmt.Printf("  - Compute Type: %s\n", computeType)
	fmt.Printf("  - Threads: %d\n", threads)

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
		customLog.Printf("Failed to create stdout pipe: %v", err)
		return "", err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		customLog.Printf("Failed to create stderr pipe: %v", err)
		return "", err
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		customLog.Printf("Failed to start transcription: %s (%v)", shortPath, err)
		return "", err
	}

	// Create channels to signal when output processing is done
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Process stdout in real-time
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			customLog.Printf("%s", scanner.Text())
		}
		close(stdoutDone)
	}()

	// Process stderr in real-time
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			customLog.Printf("%s", scanner.Text())
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
		return "", err
	}

	// Verify output file exists
	outputFile := filepath.Join(outputDir, strings.TrimSuffix(filepath.Base(preprocessedPath), filepath.Ext(preprocessedPath))+".txt")
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		customLog.Printf("Transcription failed: output file not created for %s", shortPath)
		return "", err
	}

	if _, err := os.Stat(audioFilePath); os.IsNotExist(err) {
		customLog.Printf("Audio file not found: %s", audioFilePath)
		return "", err
	}

	// Get word count from output file
	wordCount, err := countWords(outputFile)
	if err != nil {
		customLog.Printf("Failed to count words: %s (%v)", shortPath, err)
		return "", err
	}
	duration := time.Since(startTime)

	// Calculate speed in KB/s instead of MB/s
	kbPerSec := float64(inputSize) / 1024 / duration.Seconds()
	wordsPerSec := float64(wordCount) / duration.Seconds()

	customLog.Printf("Done: %s [%s | %.1fKB/s | %.0fw/s]",
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
		customLog.Printf("Failed to get preprocessed file info: %v", err)
		return 0, err
	}

	inputSize := fileInfo.Size()
	customLog.Printf("Processing audio file (%s)", formatFileSize(inputSize))
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
	customLog.Printf("Converted audio: %s → %s (-%d%%)",
		formatFileSize(originalSize),
		formatFileSize(processedSize),
		int(reductionPct),
	)

	if processedSize > 500*1024*1024 {
		customLog.Printf("Output exceeds 500MB")
	}

	if t.config.Debug {
		customLog.Printf("ffmpeg output file: %s", cachedPath)
	}

	return cachedPath, nil
}

func checkAndInstallWhisperCtranslate2() error {
	// Try to get version first
	if err := RunWhisperCTranslate2Cmd("--version"); err == nil {
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
