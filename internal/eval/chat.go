package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// openAIChatClient is a minimal OpenAI-compatible /v1/chat/completions client.
// It targets any OpenAI-shaped endpoint (e.g. vLLM, Ollama) and is deliberately
// dependency-free (net/http) — the embeddings client lives in internal/openai,
// but the chat path is small and self-contained here.
type openAIChatClient struct {
	baseURL string // e.g. "http://vllm:8000/v1"
	model   string
	apiKey  string
	http    *http.Client
}

// chatConfig is resolved from env vars (the #48 stub) or, eventually, the AI
// endpoint registry.
type chatConfig struct {
	BaseURL string
	Model   string
	APIKey  string
}

// ResolveChatClient builds a ChatClient from configuration.
//
// TODO(#48): bind to AI_ROLES["eval"] once the endpoint registry lands. Until
// then we resolve the chat endpoint from standalone env vars so this package
// stays independent of internal/config's endpoint structs (which #48 is
// reshaping in parallel — editing them here would create a merge conflict).
//
//	EVAL_CHAT_BASE_URL  (required to run) — OpenAI-compatible base URL
//	EVAL_CHAT_MODEL     (required to run) — judge model id
//	EVAL_CHAT_API_KEY   (optional)        — bearer token if the endpoint needs one
func ResolveChatClient() (ChatClient, error) {
	cfg := chatConfig{
		BaseURL: strings.TrimSpace(os.Getenv("EVAL_CHAT_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("EVAL_CHAT_MODEL")),
		APIKey:  strings.TrimSpace(os.Getenv("EVAL_CHAT_API_KEY")),
	}
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, fmt.Errorf("eval chat endpoint not configured: set EVAL_CHAT_BASE_URL and EVAL_CHAT_MODEL")
	}
	return newOpenAIChatClient(cfg), nil
}

func newOpenAIChatClient(cfg chatConfig) *openAIChatClient {
	return &openAIChatClient{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		apiKey:  cfg.APIKey,
		// Timeout is only a backstop for a hung endpoint (120s ≈ typical LLM
		// latency ceiling). The caller's context takes precedence: Complete builds
		// the request with http.NewRequestWithContext, so Do() returns the context
		// error as soon as ctx is cancelled or its deadline passes, regardless of
		// this timeout.
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *openAIChatClient) Model() string { return c.model }

// chatRequest / chatResponse are the trimmed OpenAI chat-completions shapes.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Complete posts a system+user prompt to /chat/completions and returns the first
// choice's message content. Temperature is 0 for a deterministic, reproducible
// judge (the same span should flag the same way run to run).
func (c *openAIChatClient) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read chat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal chat response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("chat endpoint returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
