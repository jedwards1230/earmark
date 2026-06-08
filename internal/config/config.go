// Package config loads and validates service configuration from environment
// variables (or an optional .env file). All canonical env var names are defined
// in the CONTRACT.md document — do not rename them without updating that file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/joho/godotenv"
)

var logger = log.NewLogger("config")

// Config holds all runtime configuration for the lilbro-whisper Go service.
// Field names mirror the canonical env var names from CONTRACT.md §2.4.
type Config struct {
	// DATABASE_URL — PostgreSQL DSN (required).
	// Example: postgres://lilbro_whisper:<pass>@lilbro-whisper-pg-rw.lilbro-whisper:5432/lilbro_whisper
	DatabaseURL string

	// BOOKS_DIR — read-only NFS mount of the audiobook library.
	// Default: /books (matches the Kubernetes volumeMount path).
	BooksDir string

	// EMBEDDINGS_BASE_URL — OpenAI-compatible base URL for the embeddings
	// endpoint (Ollama). Default: http://ollama.external-services:11434/v1
	EmbeddingsBaseURL string

	// EMBEDDINGS_MODEL — embedding model to use. Must produce 768-dim vectors.
	// Default: nomic-embed-text (CONTRACT §2.3).
	EmbeddingsModel string

	// MCP_HTTP_ADDR — address for the streamable-HTTP MCP server.
	// Default: :8081 (CONTRACT §2.2).
	MCPHTTPAddr string

	// STALE_JOB_TIMEOUT — how long a claimed job may be silent before the Go
	// service resets it to pending. Default: 30m (CONTRACT §1.3).
	StaleJobTimeout time.Duration

	// ChunkSize — target token count per transcript chunk. Default: 512.
	// Overlap is 64 tokens (implementation constant in the chunker).
	ChunkSize int

	// Debug enables verbose structured logging.
	Debug bool

	// DebugDBReset drops and recreates all tables on startup (development only).
	DebugDBReset bool
}

// LoadConfig reads the environment (preceded by an optional .env file) and
// returns a validated Config. An error is returned for required vars that are
// absent or for values that fail to parse.
func LoadConfig() (*Config, error) {
	logger.Info("Loading configuration...")

	if err := godotenv.Load(); err != nil {
		logger.Debug("No .env file found", "error", err)
	} else {
		logger.Debug("Loaded .env file")
	}

	cfg := &Config{}

	// Required
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	// Optional with defaults
	cfg.BooksDir = getEnvOrDefault("BOOKS_DIR", "/books")
	cfg.EmbeddingsBaseURL = getEnvOrDefault("EMBEDDINGS_BASE_URL", "http://ollama.external-services:11434/v1")
	cfg.EmbeddingsModel = getEnvOrDefault("EMBEDDINGS_MODEL", "nomic-embed-text")
	cfg.MCPHTTPAddr = getEnvOrDefault("MCP_HTTP_ADDR", ":8081")

	var err error

	staleStr := getEnvOrDefault("STALE_JOB_TIMEOUT", "30m")
	cfg.StaleJobTimeout, err = time.ParseDuration(staleStr)
	if err != nil {
		return nil, fmt.Errorf("STALE_JOB_TIMEOUT %q: %w", staleStr, err)
	}

	if cs := os.Getenv("CHUNK_SIZE"); cs != "" {
		cfg.ChunkSize, err = strconv.Atoi(cs)
		if err != nil {
			return nil, fmt.Errorf("CHUNK_SIZE %q: %w", cs, err)
		}
	} else {
		cfg.ChunkSize = 512
	}

	cfg.Debug = parseBoolEnv("DEBUG")
	cfg.DebugDBReset = parseBoolEnv("DEBUG_DB_RESET")

	return cfg, nil
}

// MaskSecret redacts all but the length of a secret string for logging.
func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) > 8 {
		return "********"
	}
	n := len(secret)
	out := make([]byte, n)
	for i := range out {
		out[i] = '*'
	}
	return string(out)
}

// PrintEnvVars logs the current configuration at DEBUG level.
func (c *Config) PrintEnvVars() {
	logger.Debug("=== Current Configuration ===")
	logger.Debug("Debug", "value", c.Debug)
	logger.Debug("Debug DB Reset", "value", c.DebugDBReset)
	logger.Debug("Database URL", "value", MaskSecret(c.DatabaseURL))
	logger.Debug("Books Dir", "value", c.BooksDir)
	logger.Debug("Embeddings Base URL", "value", c.EmbeddingsBaseURL)
	logger.Debug("Embeddings Model", "value", c.EmbeddingsModel)
	logger.Debug("MCP HTTP Addr", "value", c.MCPHTTPAddr)
	logger.Debug("Stale Job Timeout", "value", c.StaleJobTimeout)
	logger.Debug("Chunk Size", "value", c.ChunkSize)
}

// getEnvOrDefault returns the env var value or defaultValue when unset/empty.
func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// parseBoolEnv returns true for "1" or "true", false for everything else.
func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true"
}
