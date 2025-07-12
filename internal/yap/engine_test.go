package yap

import (
	"context"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/meta"
)

func TestNewEngine(t *testing.T) {
	cfg := &config.Config{
		CacheDir:  "/tmp/test-cache",
		OutputDir: "/tmp/test-output",
		AudioDir:  "/tmp/test-audio",
	}

	engine := NewEngine(cfg)

	if engine == nil {
		t.Fatal("NewEngine returned nil")
	}

	if engine.config != cfg {
		t.Error("Engine config not set correctly")
	}
}

func TestEngineInterfaceMethods(t *testing.T) {
	cfg := &config.Config{
		CacheDir:  "/tmp/test-cache",
		OutputDir: "/tmp/test-output",
		AudioDir:  "/tmp/test-audio",
	}

	engine := NewEngine(cfg)

	// Test interface methods exist and are callable
	version := engine.GetVersion()
	if version == "" {
		t.Error("GetVersion() returned empty string")
	}

	info, err := engine.GetInfo()
	if err != nil {
		// If binary is not available, skip the detailed info tests
		t.Skipf("GetInfo() returned error (likely no embedded binary): %v", err)
	}
	if info == nil {
		t.Error("GetInfo() returned nil")
		return
	}

	// Test that expected fields are present in info
	expectedFields := []string{"engine", "embedded", "version", "platform", "framework"}
	for _, field := range expectedFields {
		if _, exists := info[field]; !exists {
			t.Errorf("GetInfo() missing expected field: %s", field)
		}
	}

	// Test Transcribe method exists (will fail due to missing binary, but should not panic)
	ctx := context.Background()
	fileMeta := &meta.FileMetadata{
		Title:  "Test Book",
		Author: "Test Author",
	}

	_, err = engine.Transcribe(ctx, "nonexistent.mp3", fileMeta)
	// We expect an error since the file doesn't exist and binary may not be available
	if err == nil {
		t.Error("Expected error for nonexistent file, but got nil")
	}
}

func TestEngineGetVersion(t *testing.T) {
	cfg := &config.Config{}
	engine := NewEngine(cfg)

	version := engine.GetVersion()
	
	// Version should not be empty and should match the global GetVersion
	if version == "" {
		t.Error("GetVersion() returned empty string")
	}

	if version != GetVersion() {
		t.Errorf("Engine.GetVersion() = %s, but global GetVersion() = %s", version, GetVersion())
	}
}

func TestEngineGetInfo(t *testing.T) {
	cfg := &config.Config{}
	engine := NewEngine(cfg)

	info, err := engine.GetInfo()
	if err != nil {
		t.Skipf("GetInfo() returned error (likely no embedded binary): %v", err)
	}

	if info == nil {
		t.Error("GetInfo() returned nil")
		return
	}

	// Check required fields
	requiredFields := map[string]string{
		"engine":    "yap",
		"embedded":  "true",
		"platform":  "macOS",
		"framework": "Speech.framework",
	}

	for field, expectedValue := range requiredFields {
		value, exists := info[field]
		if !exists {
			t.Errorf("GetInfo() missing required field: %s", field)
			continue
		}

		// Convert to string for comparison
		if str, ok := value.(string); ok {
			if str != expectedValue {
				t.Errorf("GetInfo()[%s] = %s, expected %s", field, str, expectedValue)
			}
		} else if field == "embedded" {
			// embedded should be boolean true
			if boolVal, ok := value.(bool); !ok || !boolVal {
				t.Errorf("GetInfo()[%s] = %v, expected true", field, value)
			}
		}
	}

	// Check that version field exists
	if _, exists := info["version"]; !exists {
		t.Error("GetInfo() missing version field")
	}
}