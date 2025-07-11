package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"github.com/jedwards1230/lil-whisper/internal/log"
)

var logger = log.NewLogger("config")

type Config struct {
	// Directory config
	AudioDir  string `env:"AUDIO_DIR"`
	CacheDir  string `env:"CACHE_DIR"`
	OutputDir string `env:"OUTPUT_DIR"`

	// Service config
	Debug      bool `env:"DEBUG"`
	ResetState bool `env:"RESET_STATE"`

	// Postgres with PGVector config
	DBHost     string `env:"DB_HOST"`
	DBUser     string `env:"DB_USER"`
	DBPassword string `env:"DB_PASSWORD"`
	DBName     string `env:"DB_NAME"`

	// Vectors
	ChunkSize int `env:"CHUNK_SIZE"`

	// OpenAI API config
	OpenAIAPIKey  string `env:"OPENAI_API_KEY"`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL"`
}

func LoadConfig() (*Config, error) {
	logger.Info("Loading configuration...")

	// Load .env file if it exists (environment variables take precedence)
	if err := godotenv.Load(); err != nil {
		logger.Debug("No .env file found or error loading it", "error", err)
	} else {
		logger.Debug("Loaded .env file")
	}

	config := &Config{}

	// Load directory configuration with defaults
	config.AudioDir = getEnvOrDefault("AUDIO_DIR", "media/audiobooks")
	config.CacheDir = getEnvOrDefault("CACHE_DIR", "cache")
	config.OutputDir = getEnvOrDefault("OUTPUT_DIR", "media/transcriptions")

	// Load database configuration
	config.DBHost = os.Getenv("DB_HOST")
	config.DBUser = os.Getenv("DB_USER")
	config.DBPassword = os.Getenv("DB_PASSWORD")
	config.DBName = os.Getenv("DB_NAME")

	// Load chunk size with default
	if env := os.Getenv("CHUNK_SIZE"); env != "" {
		if chunkSize, err := strconv.Atoi(env); err == nil {
			config.ChunkSize = chunkSize
		} else {
			return nil, err
		}
	} else {
		config.ChunkSize = 1024
	}

	// Load OpenAI configuration
	config.OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	config.OpenAIBaseURL = getEnvOrDefault("OPENAI_BASE_URL", "https://api.openai.com/v1")

	// Load boolean flags
	config.Debug = parseBoolEnv("DEBUG")
	config.ResetState = parseBoolEnv("RESET_STATE")

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

// getEnvOrDefault returns the environment variable value or a default value if not set
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// parseBoolEnv parses a boolean environment variable
func parseBoolEnv(key string) bool {
	value := os.Getenv(key)
	return value == "1" || value == "true"
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

	// Directory configuration
	logger.Debug("Audio Directory", "value", c.AudioDir)
	logger.Debug("Cache Directory", "value", c.CacheDir)
	logger.Debug("Output Directory", "value", c.OutputDir)

	// OpenAI configuration
	logger.Debug("OpenAI Base URL", "value", c.OpenAIBaseURL)
	logger.Debug("OpenAI API Key", "value", MaskSecret(c.OpenAIAPIKey))

	// Other configuration
	logger.Debug("Chunk Size", "value", c.ChunkSize)
}
