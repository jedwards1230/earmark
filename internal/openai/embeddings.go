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
		tokenLimit: 2048,
	}
}

// GetEmbedding returns a vector embedding suitable for similarity search
func (e *Embeddings) GetEmbedding(content string) ([]float32, error) {
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

	embedding := resp.Data[0].Embedding

	return embedding, nil

	// compile all embeddings into a single array
	/* embeddings := make([][]float32, len(resp.Data))
	for i, emb := range resp.Data {
		embeddings[i] = emb.Embedding
	}

	return embeddings, nil */
}

// func loadEmbeddingFromString(raw string) (*openai.Embedding, error) {
// 	// Assuming raw is a base64 encoded string of float32 values
// 	decodedData, err := base64.StdEncoding.DecodeString(raw)
// 	if err != nil {
// 		return nil, err
// 	}

// 	const sizeOfFloat32 = 4
// 	floats := make([]float32, len(decodedData)/sizeOfFloat32)
// 	for i := 0; i < len(floats); i++ {
// 		floats[i] = math.Float32frombits(binary.LittleEndian.Uint32(decodedData[i*4 : (i+1)*4]))
// 	}

// 	return &openai.Embedding{Embedding: floats}, nil
// }
