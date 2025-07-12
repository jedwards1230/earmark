package transcribe

import (
	"context"
	"os"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/meta"
)

func init() {
	// Set GO_TEST environment variable for all tests in this package
	os.Setenv("GO_TEST", "1")
}

func TestNewTranscriber(t *testing.T) {
	// Create a minimal config
	cfg := &config.Config{
		CacheDir:  "/tmp/test-cache",
		OutputDir: "/tmp/test-output",
		AudioDir:  "/tmp/test-audio",
	}

	// Test creating a new transcriber
	transcriber := NewTranscriber(cfg)
	
	if transcriber == nil {
		t.Fatal("NewTranscriber returned nil")
	}
	
	if transcriber.config != cfg {
		t.Error("Transcriber config not set correctly")
	}
	
	if transcriber.engine == nil {
		t.Error("Transcriber engine not initialized")
	}
	
	// Logger is a value type, so just check that transcriber was created successfully
}

func TestTranscriberInterface(t *testing.T) {
	// Create a minimal config
	cfg := &config.Config{
		CacheDir:  "/tmp/test-cache",
		OutputDir: "/tmp/test-output",
		AudioDir:  "/tmp/test-audio",
	}

	transcriber := NewTranscriber(cfg)
	
	// Test that the transcriber implements the expected interface
	ctx := context.Background()
	fileMeta := &meta.FileMetadata{
		Title:  "Test Book",
		Author: "Test Author",
	}
	
	// This should not panic - we're just testing the interface delegation
	// The actual transcription will fail because we don't have a real audio file,
	// but we're testing that the method exists and delegates correctly
	_, err := transcriber.TranscribeAudio(ctx, "nonexistent.mp3", fileMeta)
	
	// We expect an error because the file doesn't exist, but the method should be callable
	if err == nil {
		t.Error("Expected error for nonexistent file, but got nil")
	}
}

func TestEngineManager(t *testing.T) {
	// Test that we can create an engine manager with nil engine
	manager := NewEngineManager(nil)
	
	if manager == nil {
		t.Fatal("NewEngineManager returned nil")
	}
	
	// Test behavior with nil engine
	version := manager.GetVersion()
	if version != "unknown" {
		t.Errorf("Expected 'unknown' version for nil engine, got %s", version)
	}
	
	// Test GetInfo with nil engine
	_, err := manager.GetInfo()
	if err == nil {
		t.Error("Expected error for GetInfo with nil engine")
	}
	
	// Test Transcribe with nil engine
	ctx := context.Background()
	fileMeta := &meta.FileMetadata{}
	_, err = manager.Transcribe(ctx, "test.mp3", fileMeta)
	if err == nil {
		t.Error("Expected error for Transcribe with nil engine")
	}
}