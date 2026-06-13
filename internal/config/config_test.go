package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearContractEnvVars unsets all CONTRACT-defined env vars so tests start clean.
func clearContractEnvVars(t *testing.T) {
	t.Helper()
	vars := []string{
		"DATABASE_URL", "BOOKS_DIR",
		"EMBEDDINGS_BASE_URL", "EMBEDDINGS_MODEL",
		"MCP_HTTP_ADDR", "STALE_JOB_TIMEOUT",
		"CHUNK_SIZE", "DEBUG", "DEBUG_DB_RESET",
		"ASR_SERVERS",
	}
	for _, k := range vars {
		t.Setenv(k, "") // t.Setenv restores on cleanup
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@host:5432/db")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, "postgres://user:pass@host:5432/db", cfg.DatabaseURL)
	assert.Equal(t, "/books", cfg.BooksDir)
	assert.Equal(t, "http://ollama:11434/v1", cfg.EmbeddingsBaseURL)
	assert.Equal(t, "nomic-embed-text", cfg.EmbeddingsModel)
	assert.Equal(t, ":8081", cfg.MCPHTTPAddr)
	assert.Equal(t, 30*time.Minute, cfg.StaleJobTimeout)
	assert.Equal(t, 512, cfg.ChunkSize)
	assert.False(t, cfg.Debug)
	assert.False(t, cfg.DebugDBReset)
}

func TestLoadConfig_MissingDatabaseURL(t *testing.T) {
	clearContractEnvVars(t)
	// DATABASE_URL not set
	_, err := LoadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@host:5432/earmark")
	t.Setenv("BOOKS_DIR", "/mnt/books")
	t.Setenv("EMBEDDINGS_BASE_URL", "http://custom-ollama:11434/v1")
	t.Setenv("EMBEDDINGS_MODEL", "mxbai-embed-large")
	t.Setenv("MCP_HTTP_ADDR", ":9000")
	t.Setenv("STALE_JOB_TIMEOUT", "1h")
	t.Setenv("CHUNK_SIZE", "256")
	t.Setenv("DEBUG", "true")
	t.Setenv("DEBUG_DB_RESET", "1")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, "/mnt/books", cfg.BooksDir)
	assert.Equal(t, "http://custom-ollama:11434/v1", cfg.EmbeddingsBaseURL)
	assert.Equal(t, "mxbai-embed-large", cfg.EmbeddingsModel)
	assert.Equal(t, ":9000", cfg.MCPHTTPAddr)
	assert.Equal(t, time.Hour, cfg.StaleJobTimeout)
	assert.Equal(t, 256, cfg.ChunkSize)
	assert.True(t, cfg.Debug)
	assert.True(t, cfg.DebugDBReset)
}

func TestLoadConfig_InvalidStaleJobTimeout(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("STALE_JOB_TIMEOUT", "not-a-duration")

	_, err := LoadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "STALE_JOB_TIMEOUT")
}

func TestLoadConfig_InvalidChunkSize(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("CHUNK_SIZE", "abc")

	_, err := LoadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CHUNK_SIZE")
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"abc", "***"},
		{"12345678", "********"},
		{"verylongsecretkey123", "********"},
	}
	for _, tt := range tests {
		got := MaskSecret(tt.input)
		assert.Equal(t, tt.expected, got, "input=%q", tt.input)
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	const key = "TEST_GET_ENV_OR_DEFAULT"
	_ = os.Unsetenv(key)
	assert.Equal(t, "default", getEnvOrDefault(key, "default"))

	t.Setenv(key, "custom")
	assert.Equal(t, "custom", getEnvOrDefault(key, "default"))
}

func TestParseBoolEnv(t *testing.T) {
	const key = "TEST_PARSE_BOOL_ENV"

	for _, v := range []string{"true", "1"} {
		t.Setenv(key, v)
		assert.True(t, parseBoolEnv(key), "expected true for %q", v)
	}
	for _, v := range []string{"false", "0", "invalid", ""} {
		t.Setenv(key, v)
		assert.False(t, parseBoolEnv(key), "expected false for %q", v)
	}
}

func TestConfigPrintEnvVars(t *testing.T) {
	cfg := &Config{
		DatabaseURL:       "postgres://user:pass@host:5432/db",
		BooksDir:          "/books",
		EmbeddingsBaseURL: "http://ollama:11434/v1",
		EmbeddingsModel:   "nomic-embed-text",
		MCPHTTPAddr:       ":8081",
		StaleJobTimeout:   30 * time.Minute,
		ChunkSize:         512,
		Debug:             true,
		DebugDBReset:      false,
	}
	// Should not panic.
	assert.NotPanics(t, func() { cfg.PrintEnvVars() })
}

func TestLoadConfig_ASRServers(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("ASR_SERVERS", `[
		{"name":"gpu-1","host":"gpu-1","model":"nvidia/parakeet-tdt-0.6b-v3","role":"primary","gpuArbiterUrl":"http://gpu-1:48750/status"},
		{"name":"gpu-2","role":"fallback","match":"gpu-2"}
	]`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.ASRServers, 2)
	assert.Equal(t, "gpu-1", cfg.ASRServers[0].Name)
	assert.Equal(t, "primary", cfg.ASRServers[0].Role)
	assert.Equal(t, "http://gpu-1:48750/status", cfg.ASRServers[0].GPUArbiterURL)
	// Match defaults to the (lower-cased) name when omitted.
	assert.Equal(t, "gpu-1", cfg.ASRServers[0].MatchToken())
	assert.Equal(t, "gpu-2", cfg.ASRServers[1].MatchToken())
}

func TestLoadConfig_ASRServersInvalidJSONIsIgnored(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("ASR_SERVERS", "{not json")

	// A bad list is cosmetic — it must not block startup.
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.ASRServers)
}
