package openai

import (
	"context"
	"fmt"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/sashabaranov/go-openai"
)

// EmbeddingDimension is the fixed vector dimension used for transcript_chunks.
// It must match the EMBEDDINGS_MODEL (nomic-embed-text produces 768-dim vectors).
// Any model change requires a column migration and a full re-embed of all chunks.
const EmbeddingDimension = 768

// Embeddings wraps the OpenAI-compatible embeddings client.
// The base URL is configurable so we can point at Ollama
// (http://ollama:11434/v1) instead of api.openai.com.
type Embeddings struct {
	c       *openai.Client
	model   string
	baseURL string
}

// NewEmbeddings creates an Embeddings client for the endpoint bound to the
// "embeddings" role in the AI endpoint registry (CONTRACT §2.14). After
// config.LoadConfig the registry always has this binding — either from
// AI_ENDPOINTS/AI_ROLES or synthesized from the legacy EMBEDDINGS_* vars — so a
// missing binding only happens when a Config is hand-built in a test; in that
// case we fall back to the flat EmbeddingsBaseURL/EmbeddingsModel fields to
// preserve the previous behavior.
func NewEmbeddings(cfg *config.Config) *Embeddings {
	baseURL, model := cfg.EmbeddingsBaseURL, cfg.EmbeddingsModel
	if emb, ok := cfg.EmbeddingsEndpoint(); ok {
		baseURL, model = emb.BaseURL, emb.Model
	}
	oaiCfg := openai.DefaultConfig("ollama") // key value unused by Ollama but required by the client
	oaiCfg.BaseURL = baseURL
	client := openai.NewClientWithConfig(oaiCfg)
	return &Embeddings{
		c:       client,
		model:   model,
		baseURL: baseURL,
	}
}

// BaseURL returns the configured embeddings endpoint, for diagnostics/logging.
func (e *Embeddings) BaseURL() string { return e.baseURL }

// EmbeddingUsage is the provider-reported token usage for an embeddings call.
// Ollama does not reliably populate these for embeddings (they are frequently
// zero), so callers that need an authoritative count should also tokenize the
// inputs locally; this carries the provider numbers when present.
type EmbeddingUsage struct {
	PromptTokens int
	TotalTokens  int
}

// GetEmbeddings returns one 768-float32 vector per chunk.
// Callers must ensure the result length equals len(chunks).
func (e *Embeddings) GetEmbeddings(chunks []string) ([][]float32, error) {
	vecs, _, err := e.GetEmbeddingsWithUsage(chunks)
	return vecs, err
}

// GetEmbeddingsWithUsage is GetEmbeddings plus the provider-reported token usage
// (which Ollama may leave zeroed — see EmbeddingUsage).
func (e *Embeddings) GetEmbeddingsWithUsage(chunks []string) ([][]float32, EmbeddingUsage, error) {
	if len(chunks) == 0 {
		return nil, EmbeddingUsage{}, nil
	}

	resp, err := e.c.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: chunks,
			Model: openai.EmbeddingModel(e.model),
		},
	)
	if err != nil {
		// Include the endpoint URL: an unreachable Ollama or an un-pulled model
		// both surface here, and the URL is what an operator needs to act on.
		return nil, EmbeddingUsage{}, fmt.Errorf("creating embeddings via model %q at %s: %w", e.model, e.baseURL, err)
	}

	if len(resp.Data) == 0 {
		return nil, EmbeddingUsage{}, fmt.Errorf("no embeddings returned for %d chunks", len(chunks))
	}
	if len(resp.Data) != len(chunks) {
		return nil, EmbeddingUsage{}, fmt.Errorf("embedding count mismatch: got %d for %d", len(resp.Data), len(chunks))
	}

	embeddings := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		embeddings[i] = d.Embedding
	}
	usage := EmbeddingUsage{
		PromptTokens: resp.Usage.PromptTokens,
		TotalTokens:  resp.Usage.TotalTokens,
	}
	return embeddings, usage, nil
}
