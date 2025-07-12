package correction

import (
	"context"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/meta"
)

func TestCorrectorDisabled(t *testing.T) {
	cfg := &config.Config{
		LLMCorrectionEnabled: false,
	}

	corrector := New(cfg)

	if corrector.IsEnabled() {
		t.Error("Expected corrector to be disabled")
	}

	// Test that disabled corrector returns original text
	ctx := context.Background()
	rawText := "This is some test text with errors."
	fileMeta := &meta.FileMetadata{
		FilePath: "/test/file.m4b",
		Title:    "Test Book",
		Author:   "Test Author",
		Chapter:  "Chapter 1",
	}

	result, err := corrector.CorrectText(ctx, rawText, fileMeta)
	if err != nil {
		t.Errorf("Expected no error for disabled corrector, got: %v", err)
	}

	if result.CorrectedText != rawText {
		t.Errorf("Expected original text, got: %s", result.CorrectedText)
	}

	if result.Metadata["correction_enabled"] != false {
		t.Error("Expected correction_enabled to be false in metadata")
	}
}

func TestCorrectorEnabled(t *testing.T) {
	cfg := &config.Config{
		LLMCorrectionEnabled:     true,
		LLMCorrectionModel:       "gpt-4o-mini",
		LLMCorrectionBaseURL:     "https://api.openai.com/v1",
		LLMCorrectionAPIKey:      "test-key",
		LLMCorrectionTemperature: 0.1,
		LLMCorrectionMaxRetries:  3,
		LLMCorrectionMaxTokens:   4000,
	}

	corrector := New(cfg)

	if !corrector.IsEnabled() {
		t.Error("Expected corrector to be enabled")
	}

	// Note: We can't easily test the actual LLM correction without mocking
	// the OpenAI client, which would require more complex test setup.
	// For now, we just test that the corrector is properly configured.
}

func TestCorrectorEmptyText(t *testing.T) {
	cfg := &config.Config{
		LLMCorrectionEnabled: true,
		LLMCorrectionAPIKey:  "test-key",
	}

	corrector := New(cfg)
	ctx := context.Background()
	fileMeta := &meta.FileMetadata{
		FilePath: "/test/file.m4b",
	}

	_, err := corrector.CorrectText(ctx, "", fileMeta)
	if err == nil {
		t.Error("Expected error for empty text")
	}

	if err.Error() != "cannot correct empty text" {
		t.Errorf("Expected 'cannot correct empty text' error, got: %v", err)
	}
}
