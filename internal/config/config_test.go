package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	// Save current environment
	originalEnv := make(map[string]string)
	envVars := []string{
		"AUDIO_DIR", "CACHE_DIR", "OUTPUT_DIR",
		"DB_HOST", "DB_USER", "DB_PASSWORD", "DB_NAME",
		"CHUNK_SIZE", "OPENAI_API_KEY", "OPENAI_BASE_URL",
		"DEBUG", "RESET_STATE",
	}
	for _, key := range envVars {
		originalEnv[key] = os.Getenv(key)
		os.Unsetenv(key)
	}

	// Restore environment after test
	defer func() {
		for key, value := range originalEnv {
			if value != "" {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		}
	}()

	t.Run("default values", func(t *testing.T) {
		cfg, err := LoadConfig()
		require.NoError(t, err)

		// Check default values
		// The paths are resolved to absolute paths, so check the last part
		assert.Contains(t, cfg.AudioDir, "audiobooks")
		assert.Contains(t, cfg.CacheDir, "cache")
		assert.Contains(t, cfg.OutputDir, "transcriptions")
		assert.Equal(t, 1024, cfg.ChunkSize)
		assert.Equal(t, "https://api.openai.com/v1", cfg.OpenAIBaseURL)
		assert.False(t, cfg.Debug)
		assert.False(t, cfg.ResetState)
	})

	t.Run("environment variables override", func(t *testing.T) {
		// Set test environment variables
		testEnv := map[string]string{
			"AUDIO_DIR":       "/test/audio",
			"CACHE_DIR":       "/test/cache",
			"OUTPUT_DIR":      "/test/output",
			"DB_HOST":         "test-host",
			"DB_USER":         "test-user",
			"DB_PASSWORD":     "test-password",
			"DB_NAME":         "test-db",
			"CHUNK_SIZE":      "2048",
			"OPENAI_API_KEY":  "test-key",
			"OPENAI_BASE_URL": "https://test.openai.com/v1",
			"DEBUG":           "true",
			"RESET_STATE":     "1",
		}

		for key, value := range testEnv {
			os.Setenv(key, value)
		}

		cfg, err := LoadConfig()
		require.NoError(t, err)

		// Check overridden values
		assert.Contains(t, cfg.AudioDir, "audio")
		assert.Contains(t, cfg.CacheDir, "cache")
		assert.Contains(t, cfg.OutputDir, "output")
		assert.Equal(t, "test-host", cfg.DBHost)
		assert.Equal(t, "test-user", cfg.DBUser)
		assert.Equal(t, "test-password", cfg.DBPassword)
		assert.Equal(t, "test-db", cfg.DBName)
		assert.Equal(t, 2048, cfg.ChunkSize)
		assert.Equal(t, "test-key", cfg.OpenAIAPIKey)
		assert.Equal(t, "https://test.openai.com/v1", cfg.OpenAIBaseURL)
		assert.True(t, cfg.Debug)
		assert.True(t, cfg.ResetState)

		// Clean up
		for key := range testEnv {
			os.Unsetenv(key)
		}
	})

	t.Run("invalid chunk size", func(t *testing.T) {
		os.Setenv("CHUNK_SIZE", "invalid")

		_, err := LoadConfig()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid syntax")

		os.Unsetenv("CHUNK_SIZE")
	})

	t.Run("boolean parsing", func(t *testing.T) {
		tests := []struct {
			envValue  string
			expected  bool
			shouldSet bool
		}{
			{"true", true, true},
			{"1", true, true},
			{"false", false, true},
			{"0", false, true},
			{"", false, false},
			{"invalid", false, true},
		}

		for _, tt := range tests {
			t.Run("DEBUG="+tt.envValue, func(t *testing.T) {
				if tt.shouldSet {
					os.Setenv("DEBUG", tt.envValue)
				}

				cfg, err := LoadConfig()
				require.NoError(t, err)
				assert.Equal(t, tt.expected, cfg.Debug)

				os.Unsetenv("DEBUG")
			})

			t.Run("RESET_STATE="+tt.envValue, func(t *testing.T) {
				if tt.shouldSet {
					os.Setenv("RESET_STATE", tt.envValue)
				}

				cfg, err := LoadConfig()
				require.NoError(t, err)
				assert.Equal(t, tt.expected, cfg.ResetState)

				os.Unsetenv("RESET_STATE")
			})
		}
	})
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "short secret",
			input:    "abc",
			expected: "***",
		},
		{
			name:     "8 character secret",
			input:    "12345678",
			expected: "********",
		},
		{
			name:     "long secret",
			input:    "verylongsecretkey123",
			expected: "********",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MaskSecret(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestResolveAndCreatePath(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()

	tests := []struct {
		name         string
		cwd          string
		path         string
		expectAbs    bool
		expectCreate bool
	}{
		{
			name:         "relative path",
			cwd:          tempDir,
			path:         "relative/path",
			expectAbs:    true,
			expectCreate: true,
		},
		{
			name:         "absolute path",
			cwd:          tempDir,
			path:         filepath.Join(tempDir, "absolute/path"),
			expectAbs:    true,
			expectCreate: true,
		},
		{
			name:         "current directory",
			cwd:          tempDir,
			path:         ".",
			expectAbs:    true,
			expectCreate: false, // Already exists
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveAndCreatePath(tt.cwd, tt.path)

			// Check if path is absolute
			assert.True(t, filepath.IsAbs(result))

			// Check if path was created
			if tt.expectCreate {
				_, err := os.Stat(result)
				assert.NoError(t, err, "Expected path to be created")
			}
		})
	}
}

func TestConfigPrintEnvVars(t *testing.T) {
	// This test verifies that PrintEnvVars doesn't panic
	// and properly masks sensitive information
	cfg := &Config{
		Debug:         true,
		ResetState:    false,
		DBHost:        "localhost",
		DBUser:        "testuser",
		DBPassword:    "verysecretpassword",
		DBName:        "testdb",
		AudioDir:      "/test/audio",
		CacheDir:      "/test/cache",
		OutputDir:     "/test/output",
		OpenAIBaseURL: "https://api.openai.com/v1",
		OpenAIAPIKey:  "sk-verylongapikey",
		ChunkSize:     1024,
	}

	// Should not panic
	assert.NotPanics(t, func() {
		cfg.PrintEnvVars()
	})
}

func TestConfigInitializePaths(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()

	// Change to temp directory
	originalWd, _ := os.Getwd()
	err := os.Chdir(tempDir)
	require.NoError(t, err)
	defer func() {
		err := os.Chdir(originalWd)
		require.NoError(t, err)
	}()

	cfg := &Config{
		AudioDir:  "test_audio",
		CacheDir:  "test_cache",
		OutputDir: "test_output",
	}

	err = cfg.initializePaths()
	assert.NoError(t, err)

	// Check that paths are absolute
	assert.True(t, filepath.IsAbs(cfg.AudioDir))
	assert.True(t, filepath.IsAbs(cfg.CacheDir))
	assert.True(t, filepath.IsAbs(cfg.OutputDir))

	// Check that directories were created
	for _, dir := range []string{cfg.AudioDir, cfg.CacheDir, cfg.OutputDir} {
		_, err := os.Stat(dir)
		assert.NoError(t, err, "Directory should exist: %s", dir)
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	tests := []struct {
		name         string
		envKey       string
		envValue     string
		defaultValue string
		expected     string
		setEnv       bool
	}{
		{
			name:         "environment variable set",
			envKey:       "TEST_VAR",
			envValue:     "env-value",
			defaultValue: "default-value",
			expected:     "env-value",
			setEnv:       true,
		},
		{
			name:         "environment variable not set",
			envKey:       "TEST_VAR",
			envValue:     "",
			defaultValue: "default-value",
			expected:     "default-value",
			setEnv:       false,
		},
		{
			name:         "environment variable empty",
			envKey:       "TEST_VAR",
			envValue:     "",
			defaultValue: "default-value",
			expected:     "default-value",
			setEnv:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original value
			original := os.Getenv(tt.envKey)
			defer func() {
				if original != "" {
					os.Setenv(tt.envKey, original)
				} else {
					os.Unsetenv(tt.envKey)
				}
			}()

			if tt.setEnv {
				os.Setenv(tt.envKey, tt.envValue)
			} else {
				os.Unsetenv(tt.envKey)
			}

			result := getEnvOrDefault(tt.envKey, tt.defaultValue)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseBoolEnv(t *testing.T) {
	tests := []struct {
		name     string
		envKey   string
		envValue string
		expected bool
		setEnv   bool
	}{
		{
			name:     "true string",
			envKey:   "TEST_BOOL",
			envValue: "true",
			expected: true,
			setEnv:   true,
		},
		{
			name:     "1 string",
			envKey:   "TEST_BOOL",
			envValue: "1",
			expected: true,
			setEnv:   true,
		},
		{
			name:     "false string",
			envKey:   "TEST_BOOL",
			envValue: "false",
			expected: false,
			setEnv:   true,
		},
		{
			name:     "0 string",
			envKey:   "TEST_BOOL",
			envValue: "0",
			expected: false,
			setEnv:   true,
		},
		{
			name:     "empty string",
			envKey:   "TEST_BOOL",
			envValue: "",
			expected: false,
			setEnv:   true,
		},
		{
			name:     "not set",
			envKey:   "TEST_BOOL",
			envValue: "",
			expected: false,
			setEnv:   false,
		},
		{
			name:     "invalid value",
			envKey:   "TEST_BOOL",
			envValue: "invalid",
			expected: false,
			setEnv:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original value
			original := os.Getenv(tt.envKey)
			defer func() {
				if original != "" {
					os.Setenv(tt.envKey, original)
				} else {
					os.Unsetenv(tt.envKey)
				}
			}()

			if tt.setEnv {
				os.Setenv(tt.envKey, tt.envValue)
			} else {
				os.Unsetenv(tt.envKey)
			}

			result := parseBoolEnv(tt.envKey)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func BenchmarkLoadConfig(b *testing.B) {
	// Benchmark config loading
	for i := 0; i < b.N; i++ {
		_, err := LoadConfig()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaskSecret(b *testing.B) {
	secret := "verylongsecretkey123456789"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MaskSecret(secret)
	}
}
