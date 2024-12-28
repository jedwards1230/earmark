package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	// AudioDir should be an absolute path to the directory containing audio files
	AudioDir string `json:"audio_dir"`
	// ModelsDir should be an absolute path to the directory containing models
	ModelsDir string `json:"models_dir"`
	// OutputDir should be an absolute path to the directory where transcriptions will be saved
	OutputDir string `json:"output_dir"`
	// StateFile can be a relative path from the working directory, or an absolute path
	StateFile string `json:"state_file"`
	// WhisperModel should be an absolute path to the model file
	WhisperModel string `json:"whisper_model"`
	// CacheDir should be an absolute path to the directory for caching
	CacheDir string `json:"cache_dir"`
	Debug    bool   `json:"debug"`
}

func LoadConfig() (*Config, error) {
	configFile := "config.json"
	file, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	config := &Config{}
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(config); err != nil {
		return nil, err
	}

	// Resolve and create directories
	if err := config.initializePaths(); err != nil {
		return nil, err
	}

	return config, nil
}

func (c *Config) initializePaths() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	c.AudioDir = resolveAndCreatePath(cwd, c.AudioDir)
	c.ModelsDir = resolveAndCreatePath(cwd, c.ModelsDir)
	c.OutputDir = resolveAndCreatePath(cwd, c.OutputDir)
	c.CacheDir = resolveAndCreatePath(cwd, c.CacheDir)

	c.Debug = os.Getenv("WHISPER_DEBUG") == "1" || c.Debug

	// Resolve StateFile path (don't create directory yet)
	if !filepath.IsAbs(c.StateFile) {
		c.StateFile = filepath.Join(cwd, c.StateFile)
	}

	return nil
}

func resolveAndCreatePath(cwd, path string) string {
	// Convert to absolute path if relative
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}

	err := os.MkdirAll(path, 0755)
	if err != nil {
		// Log error but don't fail - the error will surface when the directory is actually needed
		// This helps in cases where we don't have permission but the directory might already exist
		// or be created by another process
		os.Stderr.WriteString("Warning: Could not create directory: " + err.Error() + "\n")
	}

	return path
}
