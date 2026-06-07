package log

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name           string
		module         string
		debugEnabled   bool
		expectedModule string
	}{
		{
			name:           "basic_module",
			module:         "test-module",
			debugEnabled:   false,
			expectedModule: "test-module",
		},
		{
			name:           "empty_module",
			module:         "",
			debugEnabled:   false,
			expectedModule: "",
		},
		{
			name:           "module_with_debug",
			module:         "debug-module",
			debugEnabled:   true,
			expectedModule: "debug-module",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original debug state
			originalDebug := debugEnabled
			defer func() { debugEnabled = originalDebug }()

			debugEnabled = tt.debugEnabled

			logger := NewLogger(tt.module)
			assert.NotNil(t, logger.Logger)
		})
	}
}

func TestPrettyHandlerEnabled(t *testing.T) {
	tests := []struct {
		name           string
		level          slog.Level
		debugEnabled   bool
		verboseEnabled bool
		expected       bool
	}{
		{
			name:         "debug_level_debug_enabled",
			level:        slog.LevelDebug,
			debugEnabled: true,
			expected:     true,
		},
		{
			name:         "debug_level_debug_disabled",
			level:        slog.LevelDebug,
			debugEnabled: false,
			expected:     false,
		},
		{
			name:           "verbose_level_verbose_enabled",
			level:          LevelVerbose,
			verboseEnabled: true,
			expected:       true,
		},
		{
			name:           "verbose_level_verbose_disabled",
			level:          LevelVerbose,
			verboseEnabled: false,
			expected:       false,
		},
		{
			name:         "info_level_debug_disabled",
			level:        slog.LevelInfo,
			debugEnabled: false,
			expected:     true,
		},
		{
			name:         "warn_level_debug_disabled",
			level:        slog.LevelWarn,
			debugEnabled: false,
			expected:     true,
		},
		{
			name:         "error_level_debug_disabled",
			level:        slog.LevelError,
			debugEnabled: false,
			expected:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original state
			originalDebug := debugEnabled
			originalVerbose := verboseEnabled
			defer func() {
				debugEnabled = originalDebug
				verboseEnabled = originalVerbose
			}()

			debugEnabled = tt.debugEnabled
			verboseEnabled = tt.verboseEnabled

			handler := &PrettyHandler{
				l:        log.New(os.Stdout, "", 0),
				module:   "test",
				logLevel: slog.LevelInfo,
			}

			result := handler.Enabled(context.Background(), tt.level)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPrettyHandlerWithAttrs(t *testing.T) {
	handler := &PrettyHandler{
		l:        log.New(os.Stdout, "", 0),
		module:   "original",
		logLevel: slog.LevelInfo,
	}

	attrs := []slog.Attr{
		slog.String("module", "new-module"),
		slog.String("other", "value"),
	}

	newHandler := handler.WithAttrs(attrs)
	prettyHandler, ok := newHandler.(*PrettyHandler)
	require.True(t, ok)

	assert.Equal(t, "new-module", prettyHandler.module)
	assert.Equal(t, handler.logLevel, prettyHandler.logLevel)
}

func TestPrettyHandlerWithGroup(t *testing.T) {
	handler := &PrettyHandler{
		l:        log.New(os.Stdout, "", 0),
		module:   "test",
		logLevel: slog.LevelInfo,
	}

	newHandler := handler.WithGroup("group-name")
	// WithGroup should return the same handler (as implemented)
	assert.Equal(t, handler, newHandler)
}

func TestPrettyHandlerHandle(t *testing.T) {
	tests := []struct {
		name             string
		level            slog.Level
		module           string
		message          string
		attrs            []slog.Attr
		expectedContains []string
	}{
		{
			name:    "info_level_basic",
			level:   slog.LevelInfo,
			module:  "test-module",
			message: "test message",
			attrs:   []slog.Attr{},
			expectedContains: []string{
				"→",
				"[test-module]",
				"test message",
			},
		},
		{
			name:    "debug_level",
			level:   slog.LevelDebug,
			module:  "debug-module",
			message: "debug message",
			attrs:   []slog.Attr{},
			expectedContains: []string{
				"•",
				"[debug-module]",
				"debug message",
			},
		},
		{
			name:    "verbose_level",
			level:   LevelVerbose,
			module:  "verbose-module",
			message: "verbose message",
			attrs:   []slog.Attr{},
			expectedContains: []string{
				"…",
				"[verbose-module]",
				"verbose message",
			},
		},
		{
			name:    "warn_level",
			level:   slog.LevelWarn,
			module:  "warn-module",
			message: "warning message",
			attrs:   []slog.Attr{},
			expectedContains: []string{
				"!",
				"[warn-module]",
				"warning message",
			},
		},
		{
			name:    "error_level",
			level:   slog.LevelError,
			module:  "error-module",
			message: "error message",
			attrs:   []slog.Attr{},
			expectedContains: []string{
				"✗",
				"[error-module]",
				"error message",
			},
		},
		{
			name:    "with_attributes",
			level:   slog.LevelInfo,
			module:  "attr-module",
			message: "message with attrs",
			attrs: []slog.Attr{
				slog.String("key1", "value1"),
				slog.Int("key2", 42),
			},
			expectedContains: []string{
				"→",
				"[attr-module]",
				"message with attrs",
				"=value1",
				"=42",
			},
		},
		{
			name:    "empty_module",
			level:   slog.LevelInfo,
			module:  "",
			message: "test message",
			attrs:   []slog.Attr{},
			expectedContains: []string{
				"→",
				"[app]", // Should default to "app"
				"test message",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture output
			var buf bytes.Buffer
			handler := &PrettyHandler{
				l:        log.New(&buf, "", 0),
				module:   tt.module,
				logLevel: slog.LevelInfo,
			}

			// Create a log record
			record := slog.NewRecord(time.Now(), tt.level, tt.message, 0)
			for _, attr := range tt.attrs {
				record.AddAttrs(attr)
			}

			err := handler.Handle(context.Background(), record)
			assert.NoError(t, err)

			output := buf.String()
			for _, expected := range tt.expectedContains {
				assert.Contains(t, output, expected, "Output should contain: %s", expected)
			}
		})
	}
}

func TestColorCodes(t *testing.T) {
	tests := []struct {
		name          string
		level         slog.Level
		expectedColor string
	}{
		{
			name:          "debug_level_blue",
			level:         slog.LevelDebug,
			expectedColor: colorBlue,
		},
		{
			name:          "verbose_level_gray",
			level:         LevelVerbose,
			expectedColor: colorGray,
		},
		{
			name:          "info_level_white",
			level:         slog.LevelInfo,
			expectedColor: colorWhite,
		},
		{
			name:          "warn_level_yellow",
			level:         slog.LevelWarn,
			expectedColor: colorYellow,
		},
		{
			name:          "error_level_red",
			level:         slog.LevelError,
			expectedColor: colorRed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := &PrettyHandler{
				l:        log.New(&buf, "", 0),
				module:   "test",
				logLevel: slog.LevelInfo,
			}

			record := slog.NewRecord(time.Now(), tt.level, "test", 0)
			err := handler.Handle(context.Background(), record)
			assert.NoError(t, err)

			output := buf.String()
			assert.Contains(t, output, tt.expectedColor)
			assert.Contains(t, output, colorReset) // Should always end with reset
		})
	}
}

func TestDebugEnvironmentVariable(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected bool
	}{
		{
			name:     "debug_true",
			envValue: "true",
			expected: true,
		},
		{
			name:     "debug_1",
			envValue: "1",
			expected: true,
		},
		{
			name:     "debug_false",
			envValue: "false",
			expected: false,
		},
		{
			name:     "debug_0",
			envValue: "0",
			expected: false,
		},
		{
			name:     "debug_empty",
			envValue: "",
			expected: false,
		},
		{
			name:     "debug_invalid",
			envValue: "invalid",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original environment
			originalEnv := os.Getenv("LOG_DEBUG")
			defer func() { _ = os.Setenv("LOG_DEBUG", originalEnv) }()

			// Set test environment
			_ = os.Setenv("LOG_DEBUG", tt.envValue)

			// Re-initialize the debug flag (simulate package init)
			debugEnabled = os.Getenv("LOG_DEBUG") == "1" || os.Getenv("LOG_DEBUG") == "true"

			assert.Equal(t, tt.expected, debugEnabled)
		})
	}
}

func TestVerboseEnvironmentVariable(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected bool
	}{
		{
			name:     "verbose_true",
			envValue: "true",
			expected: true,
		},
		{
			name:     "verbose_1",
			envValue: "1",
			expected: true,
		},
		{
			name:     "verbose_false",
			envValue: "false",
			expected: false,
		},
		{
			name:     "verbose_0",
			envValue: "0",
			expected: false,
		},
		{
			name:     "verbose_empty",
			envValue: "",
			expected: false,
		},
		{
			name:     "verbose_invalid",
			envValue: "invalid",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original environment
			originalEnv := os.Getenv("LOG_VERBOSE")
			defer func() { _ = os.Setenv("LOG_VERBOSE", originalEnv) }()

			// Set test environment
			_ = os.Setenv("LOG_VERBOSE", tt.envValue)

			// Re-initialize the verbose flag (simulate package init)
			verboseEnabled = os.Getenv("LOG_VERBOSE") == "1" || os.Getenv("LOG_VERBOSE") == "true"

			assert.Equal(t, tt.expected, verboseEnabled)
		})
	}
}

func TestLoggerVerboseMethod(t *testing.T) {
	// Save original verbose state
	originalVerbose := verboseEnabled
	defer func() { verboseEnabled = originalVerbose }()

	verboseEnabled = true

	// Capture output by redirecting stdout
	originalStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	logger := NewLogger("verbose-test")

	// Test verbose method
	logger.Verbose("verbose test message", "key", "value")

	// Restore stdout
	_ = w.Close()
	os.Stdout = originalStdout

	// Read captured output
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	assert.NoError(t, err)
	output := buf.String()

	// Verify verbose message was logged
	assert.Contains(t, output, "verbose test message")
	assert.Contains(t, output, "[verbose-test]")
	assert.Contains(t, output, "…") // verbose symbol
}

func TestLoggerIntegration(t *testing.T) {
	// Save original debug state
	originalDebug := debugEnabled
	defer func() { debugEnabled = originalDebug }()

	debugEnabled = true

	// Capture output by redirecting stdout
	originalStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	logger := NewLogger("integration-test")

	// Test different log levels
	logger.Info("info message", "key", "value")
	logger.Debug("debug message", "number", 42)
	logger.Warn("warning message")
	logger.Error("error message", "error", "test error")

	// Restore stdout
	_ = w.Close()
	os.Stdout = originalStdout

	// Read captured output
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	assert.NoError(t, err)
	output := buf.String()

	// Verify all messages were logged
	assert.Contains(t, output, "info message")
	assert.Contains(t, output, "debug message")
	assert.Contains(t, output, "warning message")
	assert.Contains(t, output, "error message")
	assert.Contains(t, output, "[integration-test]")
}

// Benchmark tests
func BenchmarkNewLogger(b *testing.B) {
	for i := 0; i < b.N; i++ {
		NewLogger("benchmark-module")
	}
}

func BenchmarkLoggerInfo(b *testing.B) {
	logger := NewLogger("benchmark")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		logger.Info("benchmark message", "iteration", i)
	}
}

func BenchmarkPrettyHandlerHandle(b *testing.B) {
	var buf bytes.Buffer
	handler := &PrettyHandler{
		l:        log.New(&buf, "", 0),
		module:   "benchmark",
		logLevel: slog.LevelInfo,
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark message", 0)
	record.AddAttrs(slog.String("key", "value"))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := handler.Handle(context.Background(), record)
		if err != nil {
			b.Fatal(err)
		}
	}
}
