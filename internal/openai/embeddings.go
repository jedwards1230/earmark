package openai

import (
	"context"
	"fmt"
	"log"
	"github.com/jedwards1230/lil-whisper/internal/config"

	"github.com/sashabaranov/go-openai"
)

type Embeddings struct {
	c *openai.Client
}

func NewEmbeddings(cfg *config.Config) *Embeddings {
	return &Embeddings{
		c: InitOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL),
	}
}

// GetEmbedding returns a vector embedding suitable for similarity search
func (e *Embeddings) GetEmbeddings(chunks []string) (embeddings [][]float32, err error) {
	resp, err := e.c.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: chunks,
			Model: openai.SmallEmbedding3,
		},
	)
	if err != nil {
		log.Printf("OpenAI API error: %v", err)
		return nil, fmt.Errorf("creating embedding: %v", err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}

	// compile all embeddings into a single array
	embeddings = make([][]float32, len(resp.Data))
	for i, emb := range resp.Data {
		embeddings[i] = emb.Embedding
	}

	return embeddings, nil
}