package transcribe

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/yap"
)

type Transcriber struct {
	config  *config.Config
	log     log.Logger
	engine  *EngineManager
}

func NewTranscriber(cfg *config.Config) *Transcriber {
	logger := log.NewLogger("transcribe")

	if err := checkDependencies(); err != nil {
		logger.Error("Failed checking dependencies", "error", err)
		os.Exit(1)
	}

	// Initialize Yap speech engine
	yapEngine := yap.NewEngine(cfg)
	engineManager := NewEngineManager(yapEngine)

	// Log speech engine info at startup
	if info, err := engineManager.GetInfo(); err == nil {
		logger.Info("Initialized speech transcription", 
			"engine", info["engine"],
			"version", info["version"],
			"platform", info["platform"])
	}

	// Clear cache directory on startup
	if err := clearCacheDir(cfg.CacheDir); err != nil {
		logger.Error("Failed clearing cache directory", "error", err)
		os.Exit(1)
	}

	return &Transcriber{
		config: cfg,
		log:    logger,
		engine: engineManager,
	}
}


func (t *Transcriber) TranscribeAudio(
	ctx context.Context,
	audioFilePath string,
	fileMeta *meta.FileMetadata,
) (string, error) {
	// Delegate transcription to the configured speech engine
	return t.engine.Transcribe(ctx, audioFilePath, fileMeta)
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

func clearCacheDir(cacheDir string) error {
	if err := os.RemoveAll(cacheDir); err != nil {
		return err
	}
	return os.MkdirAll(cacheDir, 0750)
}
