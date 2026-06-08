package openai

import (
	"context"
	"fmt"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/sashabaranov/go-openai"
)

// EmbeddingDimension is the fixed vector dimension used for transcript_chunks.
// It must match the EMBEDDINGS_MODEL (nomic-embed-text produces 768-dim vectors).
// Any model change requires a column migration and a full re-embed of all chunks.
const EmbeddingDimension = 768

// Embeddings wraps the OpenAI-compatible embeddings client.
// The base URL is configurable so we can point at Ollama
// (http://ollama.external-services:11434/v1) instead of api.openai.com.
type Embeddings struct {
	c     *openai.Client
	model string
}

// NewEmbeddings creates an Embeddings client pointed at the Ollama-compatible
// endpoint specified by cfg.EmbeddingsBaseURL, using cfg.EmbeddingsModel.
func NewEmbeddings(cfg *config.Config) *Embeddings {
	oaiCfg := openai.DefaultConfig("ollama") // key value unused by Ollama but required by the client
	oaiCfg.BaseURL = cfg.EmbeddingsBaseURL
	client := openai.NewClientWithConfig(oaiCfg)
	return &Embeddings{
		c:     client,
		model: cfg.EmbeddingsModel,
	}
}

// GetEmbeddings returns one 768-float32 vector per chunk.
// Callers must ensure the result length equals len(chunks).
func (e *Embeddings) GetEmbeddings(chunks []string) ([][]float32, error) {
	if len(chunks) == 0 {
		return nil, nil
	}

	resp, err := e.c.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: chunks,
			Model: openai.EmbeddingModel(e.model),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("creating embeddings via %s: %w", e.model, err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned for %d chunks", len(chunks))
	}
	if len(resp.Data) != len(chunks) {
		return nil, fmt.Errorf("embedding count mismatch: got %d for %d", len(resp.Data), len(chunks))
	}

	embeddings := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		embeddings[i] = d.Embedding
	}
	return embeddings, nil
}
