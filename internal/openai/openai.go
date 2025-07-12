package openai

import (
	"context"
	"fmt"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/sashabaranov/go-openai"
)

type Client struct {
	c *openai.Client
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float32   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
}

func InitOpenAI(authToken, baseUrl string) *openai.Client {
	config := openai.DefaultConfig(authToken)
	config.BaseURL = baseUrl
	client := openai.NewClientWithConfig(config)
	return client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		c: InitOpenAI(cfg.LLMCorrectionAPIKey, cfg.LLMCorrectionBaseURL),
	}
}

func (c *Client) CreateChatCompletion(ctx context.Context, req ChatCompletionRequest) (string, int, error) {
	messages := make([]openai.ChatCompletionMessage, len(req.Messages))
	for i, msg := range req.Messages {
		messages[i] = openai.ChatCompletionMessage{
			Role:    msg.Role,
			Content: msg.Content,
		}
	}

	resp, err := c.c.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model:       req.Model,
			Messages:    messages,
			Temperature: req.Temperature,
			MaxTokens:   req.MaxTokens,
		},
	)
	if err != nil {
		return "", 0, fmt.Errorf("chat completion failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", 0, fmt.Errorf("no completion choices returned")
	}

	return resp.Choices[0].Message.Content, resp.Usage.TotalTokens, nil
}
