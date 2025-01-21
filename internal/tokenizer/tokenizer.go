package tokenizer

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"github.com/sashabaranov/go-openai"
)

// GetTokens returns raw tokens (probably not what you want for similarity search)
func GetTokens(filepath string) ([]int, error) {
	// Open the file
	file, err := os.Open(filepath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	// Read the file content
	data, err := io.ReadAll(file)
	if err != nil {
		log.Fatal(err)
	}

	// Convert byte slice to string
	content := string(data)
	encoding := "cl100k_base"

	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tkm, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		err = fmt.Errorf("getEncoding: %v", err)
		return nil, err
	}

	// encode
	tokens := tkm.Encode(content, nil, nil)

	return tokens, nil
}

// GetEmbedding returns a vector embedding suitable for similarity search
func GetEmbedding(content string, apiKey string) ([]float32, error) {
	client := openai.NewClient(apiKey)
	resp, err := client.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: []string{content},
			Model: openai.SmallEmbedding3,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("creating embedding: %v", err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}

	return resp.Data[0].Embedding, nil
}
