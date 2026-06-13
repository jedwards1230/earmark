package openai

import (
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
)

func TestEmbeddingDimensionConstant(t *testing.T) {
	// The CONTRACT mandates VECTOR(768) for nomic-embed-text.
	// This test guards against accidental changes to the constant.
	const expected = 768
	if EmbeddingDimension != expected {
		t.Errorf("EmbeddingDimension = %d, want %d — update CONTRACT.md before changing this", EmbeddingDimension, expected)
	}
}

func TestNewEmbeddings(t *testing.T) {
	cfg := &config.Config{
		EmbeddingsBaseURL: "http://ollama:11434/v1",
		EmbeddingsModel:   "nomic-embed-text",
	}
	e := NewEmbeddings(cfg)
	if e == nil {
		t.Fatal("NewEmbeddings returned nil")
	}
	if e.model != "nomic-embed-text" {
		t.Errorf("expected model nomic-embed-text, got %q", e.model)
	}
}

func TestGetEmbeddings_EmptyInput(t *testing.T) {
	cfg := &config.Config{
		EmbeddingsBaseURL: "http://ollama:11434/v1",
		EmbeddingsModel:   "nomic-embed-text",
	}
	e := NewEmbeddings(cfg)
	result, err := e.GetEmbeddings(nil)
	if err != nil {
		t.Errorf("unexpected error for nil input: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for nil input, got %v", result)
	}

	result2, err2 := e.GetEmbeddings([]string{})
	if err2 != nil {
		t.Errorf("unexpected error for empty slice: %v", err2)
	}
	if result2 != nil {
		t.Errorf("expected nil result for empty slice, got %v", result2)
	}
}
