package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"transcriber/internal/log"
)

var logger = log.NewLogger("config")

type Config struct {
	// Directory config
	AudioDir  string `json:"audio_dir"`
	CacheDir  string `json:"cache_dir"`
	ModelsDir string `json:"models_dir"`
	OutputDir string `json:"output_dir"`
	StateFile string `json:"state_file"`

	// Whisper Local transcription config
	WhisperModel       string `json:"whisper_model"`
	WhisperThreads     int    `json:"whisper_threads"`
	WhisperComputeType string `json:"whisper_compute_type"`

	// Service config
	Debug      bool `json:"debug"`
	ResetState bool `json:"reset_state"`

	// Postgres with PGVector config
	DBHost     string `json:"db_host"`
	DBUser     string `json:"db_user"`
	DBPassword string `json:"db_password"`
	DBName     string `json:"db_name"`

	// Vectors
	ChunkSize int `json:"chunk_size"`

	// OpenAI API config
	OpenAIAPIKey  string `json:"openai_api_key"`
	OpenAIBaseURL string `json:"openai_base_url"`
}

func LoadConfig() (*Config, error) {
	logger.Info("Loading configuration...")

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

	if env := os.Getenv("WHISPER_MODEL"); env != "" {
		config.WhisperModel = env
	}

	if env := os.Getenv("WHISPER_THREADS"); env != "" {
		if threads, err := strconv.Atoi(env); err == nil {
			config.WhisperThreads = threads
		} else {
			return nil, err
		}
	} else {
		config.WhisperThreads = 1
	}

	if env := os.Getenv("WHISPER_COMPUTE_TYPE"); env != "" {
		config.WhisperComputeType = env
	} else {
		config.WhisperComputeType = "int8"
	}

	// Override with environment variables
	if env := os.Getenv("DB_HOST"); env != "" {
		config.DBHost = env
	}
	if env := os.Getenv("DB_USER"); env != "" {
		config.DBUser = env
	}
	if env := os.Getenv("DB_PASSWORD"); env != "" {
		config.DBPassword = env
	}
	if env := os.Getenv("DB_NAME"); env != "" {
		config.DBName = env
	}

	if env := os.Getenv("CHUNK_SIZE"); env != "" {
		if chunkSize, err := strconv.Atoi(env); err == nil {
			config.ChunkSize = chunkSize
		} else {
			return nil, err
		}
	} else {
		config.ChunkSize = 1024
	}

	if env := os.Getenv("OPENAI_API_KEY"); env != "" {
		config.OpenAIAPIKey = env
	}

	if env := os.Getenv("OPENAI_BASE_URL"); env != "" {
		config.OpenAIBaseURL = env
	} else {
		config.OpenAIBaseURL = "https://api.openai.com/v1"
	}

	if env := os.Getenv("DEBUG"); env != "" {
		config.Debug = env == "1" || env == "true"
	}

	if env := os.Getenv("RESET_STATE"); env != "" {
		config.ResetState = env == "1" || env == "true"
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

func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) > 8 {
		return strings.Repeat("*", 8)
	}
	return strings.Repeat("*", len(secret))
}

func (c *Config) PrintEnvVars() {
	logger.Info("=== Current Configuration ===")
	logger.Info("Whisper Model", "value", c.WhisperModel)
	logger.Info("Whisper Threads", "value", c.WhisperThreads)
	logger.Info("Whisper Compute Type", "value", c.WhisperComputeType)
	logger.Info("Debug", "value", c.Debug)
	logger.Info("Reset State", "value", c.ResetState)

	// Database configuration
	logger.Info("DB Host", "value", c.DBHost)
	logger.Info("DB User", "value", c.DBUser)
	logger.Info("DB Password", "value", MaskSecret(c.DBPassword))
	logger.Info("DB Name", "value", c.DBName)

	// OpenAI configuration
	logger.Info("OpenAI Base URL", "value", c.OpenAIBaseURL)
	logger.Info("OpenAI API Key", "value", MaskSecret(c.OpenAIAPIKey))

	// Directory configuration
	logger.Info("Audio Directory", "value", c.AudioDir)
	logger.Info("Cache Directory", "value", c.CacheDir)
	logger.Info("Models Directory", "value", c.ModelsDir)
	logger.Info("Output Directory", "value", c.OutputDir)
	logger.Info("State File", "value", c.StateFile)

	// Other configuration
	logger.Info("Chunk Size", "value", c.ChunkSize)
}
