package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"transcriber/internal/log"

	"github.com/joho/godotenv"
)

var logger = log.NewLogger("config")

type Config struct {
	// Directory config
	AudioDir  string `json:"audio_dir"`
	CacheDir  string `json:"cache_dir"`
	OutputDir string `json:"output_dir"`

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

	if err := godotenv.Overload(); err != nil {
		logger.Info("No .env file found")
	}

	config := &Config{}

	if env := os.Getenv("AUDIO_DIR"); env != "" {
		config.AudioDir = env
	} else {
		config.AudioDir = "media/audiobooks"
	}

	if env := os.Getenv("CACHE_DIR"); env != "" {
		config.CacheDir = env
	} else {
		config.CacheDir = "cache"
	}

	if env := os.Getenv("OUTPUT_DIR"); env != "" {
		config.OutputDir = env
	} else {
		config.OutputDir = "media/transcriptions"
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
	c.OutputDir = resolveAndCreatePath(cwd, c.OutputDir)
	c.CacheDir = resolveAndCreatePath(cwd, c.CacheDir)

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
	logger.Debug("=== Current Configuration ===")
	logger.Debug("Debug", "value", c.Debug)
	logger.Debug("Reset State", "value", c.ResetState)

	// Database configuration
	logger.Debug("DB Host", "value", c.DBHost)
	logger.Debug("DB User", "value", c.DBUser)
	logger.Debug("DB Password", "value", MaskSecret(c.DBPassword))
	logger.Debug("DB Name", "value", c.DBName)

	// OpenAI configuration
	logger.Debug("OpenAI Base URL", "value", c.OpenAIBaseURL)
	logger.Debug("OpenAI API Key", "value", MaskSecret(c.OpenAIAPIKey))

	// Directory configuration
	logger.Debug("Audio Directory", "value", c.AudioDir)
	logger.Debug("Cache Directory", "value", c.CacheDir)
	logger.Debug("Output Directory", "value", c.OutputDir)

	// Other configuration
	logger.Debug("Chunk Size", "value", c.ChunkSize)
}
