package yap

import (
	_ "embed"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
)

//go:embed embedded/yap
var yapBinary []byte

//go:embed embedded/version.txt
var yapVersion string

var (
	// Cache for the extracted binary path to avoid repeated extractions
	embeddedYapPath string
	embeddedYapOnce sync.Once
	embeddedYapErr  error
)

// Engine implements the SpeechEngine interface using Apple's Yap
type Engine struct {
	config *config.Config
	log    log.Logger
}

// NewEngine creates a new Yap speech engine
func NewEngine(cfg *config.Config) *Engine {
	logger := log.NewLogger("yap")
	
	// Log yap version at startup
	version := GetVersion()
	logger.Info("Initialized Yap speech engine", "version", version)
	
	return &Engine{
		config: cfg,
		log:    logger,
	}
}

// Transcribe converts audio file to text using Yap
func (e *Engine) Transcribe(ctx context.Context, audioFilePath string, fileMeta *meta.FileMetadata) (string, error) {
	shortPath := filepath.Base(audioFilePath)
	startTime := time.Now()

	e.log.Info("Starting transcription", "file", shortPath, "engine", "yap")

	// Preprocess audio
	preprocessedPath, err := e.preprocessAudio(audioFilePath)
	if err != nil {
		e.log.Error("Audio preprocessing failed", "file", shortPath, "error", err)
		return "", err
	}
	defer os.Remove(preprocessedPath)

	inputSize, err := getInputSize(preprocessedPath)
	if err != nil {
		return "", err
	}

	// Create output directory in the raw subdirectory
	rawDir := filepath.Join(e.config.OutputDir, "raw")
	err = os.MkdirAll(rawDir, 0750)
	if err != nil {
		e.log.Error("Failed to create raw output directory", "file", shortPath, "error", err)
		return "", err
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()

	outputFile := filepath.Join(rawDir, strings.TrimSuffix(filepath.Base(preprocessedPath), filepath.Ext(preprocessedPath))+".txt")

	// Get embedded yap binary path
	yapPath, err := getYapPath()
	if err != nil {
		e.log.Error("Failed to get embedded yap binary", "file", shortPath, "error", err)
		return "", err
	}

	// #nosec G204 - yap binary path is validated and paths are cleaned
	cmd := exec.CommandContext(ctx,
		yapPath,
		"transcribe",
		filepath.Clean(preprocessedPath),
		"--output-file", filepath.Clean(outputFile),
		// "--initial_prompt", fmt.Sprintf("Transcribe %s by %s", fileMeta.Title, fileMeta.Author),
	)

	// Create pipes for stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		e.log.Error("Failed to create stdout pipe", "file", shortPath, "error", err)
		return "", err
	}

	// Create a pipe for stderr that we can both read from and write to
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		e.log.Error("Failed to create stderr pipe", "file", shortPath, "error", err)
		return "", err
	}

	// Create a buffer for capturing stderr
	var stderr bytes.Buffer

	// Set up command stderr
	cmd.Stderr = stderrWriter

	// Add memory monitoring
	memStatChan := make(chan struct{})
	if e.config.Debug {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					var mem runtime.MemStats
					runtime.ReadMemStats(&mem)
					// Safely convert uint64 to int64 to avoid overflow
					allocSize := int64(mem.Alloc)
					if mem.Alloc > 9223372036854775807 { // max int64
						allocSize = 9223372036854775807
					}
					sysSize := int64(mem.Sys)
					if mem.Sys > 9223372036854775807 { // max int64
						sysSize = 9223372036854775807
					}
					
					e.log.Debug("Memory stats",
						"alloc", formatFileSize(allocSize),
						"sys", formatFileSize(sysSize),
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
		if err := stderrReader.Close(); err != nil {
			e.log.Warn("Failed to close stderr reader", "error", err)
		}
		if err := stderrWriter.Close(); err != nil {
			e.log.Warn("Failed to close stderr writer", "error", err)
		}
		e.log.Error("Failed to start transcription", "file", shortPath, "error", err)
		return "", fmt.Errorf("failed to start transcription process: %v", err)
	}

	// Create channels to signal when output processing is done
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Process stdout in real-time
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			e.log.Verbose(scanner.Text())
		}
		close(stdoutDone)
	}()

	// Process stderr in real-time while also capturing it
	go func() {
		tee := io.TeeReader(stderrReader, &stderr)
		scanner := bufio.NewScanner(tee)
		for scanner.Scan() {
			e.log.Verbose(scanner.Text())
		}
		close(stderrDone)
	}()

	// Handle process cleanup on context cancellation
	go func() {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			e.log.Warn("Transcription timeout", "file", shortPath)
		}
		// Kill process group
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			e.log.Warn("Failed to kill process group", "pid", cmd.Process.Pid, "error", err)
		}
	}()

	// Wait for command to complete and output processing to finish
	err = cmd.Wait()
	close(memStatChan)
	if err := stderrWriter.Close(); err != nil {
		e.log.Warn("Failed to close stderr writer", "error", err)
	}

	// Wait for output processors to finish
	<-stdoutDone
	<-stderrDone
	if err := stderrReader.Close(); err != nil {
		e.log.Warn("Failed to close stderr reader", "error", err)
	}

	if err != nil {
		errMsg := fmt.Sprintf("Transcription failed for %s: %v", shortPath, err)
		if exitErr, ok := err.(*exec.ExitError); ok {
			e.log.Error("Transcription process failed",
				"file", shortPath,
				"exit_code", exitErr.ExitCode(),
				"signal", exitErr.Sys(),
				"stderr", stderr.String())
		}
		return "", errors.New(errMsg)
	}

	// Verify output file exists
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		e.log.Error("Transcription failed: output file not created", "file", shortPath)
		return "", err
	}

	if _, err := os.Stat(audioFilePath); os.IsNotExist(err) {
		e.log.Error("Audio file not found", "file", audioFilePath)
		return "", err
	}

	// Get word count from output file
	wordCount, err := countWords(outputFile)
	if err != nil {
		e.log.Error("Failed to count words", "file", shortPath, "error", err)
		return "", err
	}
	duration := time.Since(startTime)

	// Calculate speed in KB/s instead of MB/s
	kbPerSec := float64(inputSize) / 1024 / duration.Seconds()
	wordsPerSec := float64(wordCount) / duration.Seconds()

	e.log.Info("Transcription completed",
		"file", shortPath,
		"duration", formatDuration(duration),
		"speed_kb_sec", fmt.Sprintf("%.1f", kbPerSec),
		"words_sec", fmt.Sprintf("%.0f", wordsPerSec))

	return outputFile, nil
}

// GetVersion returns the version of the embedded yap binary
func (e *Engine) GetVersion() string {
	return GetVersion()
}

// GetInfo returns information about the yap speech engine
func (e *Engine) GetInfo() (map[string]interface{}, error) {
	yapPath, err := getYapPath()
	if err != nil {
		return nil, err
	}

	info := map[string]interface{}{
		"engine":     "yap",
		"path":       yapPath,
		"embedded":   true,
		"version":    GetVersion(),
		"platform":   "macOS",
		"framework":  "Speech.framework",
	}

	// Add file info if binary exists
	if stat, err := os.Stat(yapPath); err == nil {
		info["size"] = stat.Size()
		info["mod_time"] = stat.ModTime()
	}

	return info, nil
}