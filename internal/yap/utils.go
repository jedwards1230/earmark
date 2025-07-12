package yap

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

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

func countWords(path string) (int, error) {
	// #nosec G304 - path is controlled by caller and validated elsewhere
	content, err := os.ReadFile(filepath.Clean(path))
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

func getInputSize(filepath string) (int64, error) {
	fileInfo, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}

	inputSize := fileInfo.Size()
	return inputSize, nil
}

func (e *Engine) preprocessAudio(audioFilePath string) (string, error) {
	originalInfo, err := os.Stat(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to get original file info: %w", err)
	}
	originalSize := originalInfo.Size()

	// Compute relative path from AudioDir
	relPath, err := filepath.Rel(e.config.AudioDir, audioFilePath)
	if err != nil {
		return "", fmt.Errorf("unable to determine relative path: %w", err)
	}

	// Build cached file path using the same structure but with .mp3 extension
	cachedPath := filepath.Join(e.config.CacheDir, strings.TrimSuffix(relPath, filepath.Ext(relPath))+".mp3")
	if err := os.MkdirAll(filepath.Dir(cachedPath), 0750); err != nil {
		return "", fmt.Errorf("failed to create cache directories: %w", err)
	}

	if err := runFfmpegCmd(
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
		if err := runFfmpegCmd(
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
	e.log.Info("Audio conversion completed",
		"original_size", formatFileSize(originalSize),
		"processed_size", formatFileSize(processedSize),
		"reduction_percent", int(reductionPct))

	if processedSize > 500*1024*1024 {
		e.log.Warn("Output file exceeds size limit", "size", formatFileSize(processedSize))
	}

	if e.config.Debug {
		e.log.Debug("FFmpeg output file created", "path", cachedPath)
	}

	return cachedPath, nil
}

func runFfmpegCmd(args ...string) error {
	cmd := exec.Command("ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg error: %w\n%s", err, stderr.String())
	}
	return nil
}