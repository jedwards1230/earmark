package openai

import (
	"context"
	"fmt"
	"log"
	"transcriber/internal/config"
	"transcriber/internal/tokenizer"

	"github.com/sashabaranov/go-openai"
)

type Embeddings struct {
	c          *openai.Client
	tokenLimit int
}

func NewEmbeddings(cfg *config.Config) *Embeddings {
	return &Embeddings{
		c:          InitOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL),
		tokenLimit: cfg.ChunkSize,
	}
}

// GetEmbedding returns a vector embedding suitable for similarity search
func (e *Embeddings) GetEmbeddings(content string) (embeddings [][]float32, err error) {
	tokens, err := tokenizer.GetTokens(content)
	if err != nil {
		return nil, fmt.Errorf("getting tokens: %v", err)
	}
	fmt.Printf("Token count: %d\n", len(tokens))

	chunks := chunker(content, e.tokenLimit, SplitTypeToken)

	fmt.Printf("Splitting content into %d chunks\n", len(chunks))

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
