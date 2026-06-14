package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
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

// chatConfig is resolved from the AI endpoint registry (AI_ROLES["eval"]) or,
// when no eval role is bound, from the standalone EVAL_CHAT_* env vars.
type chatConfig struct {
	BaseURL string
	Model   string
	APIKey  string
}

// EvalEndpointSource is the minimal slice of the parsed config the eval layer
// needs to find its chat endpoint. It is an interface (rather than *config.Config)
// so internal/eval stays decoupled from config's endpoint structs and the
// resolver is testable with a tiny fake — no full Config build, no import cycle.
// *config.Config satisfies it via its EvalEndpoint accessor.
type EvalEndpointSource interface {
	// EvalEndpoint reports the chat endpoint bound to the "eval" role, if any.
	// ok=false means no eval role is configured (fall back to env vars).
	EvalEndpoint() (EvalEndpoint, bool)
}

// EvalEndpoint is the resolved chat endpoint for the judge: the fields the chat
// client needs out of one AI_ENDPOINTS entry. It mirrors the relevant subset of
// config.AIEndpoint so eval doesn't depend on that type directly.
type EvalEndpoint struct {
	BaseURL string
	Model   string
	// Options are the endpoint's backend-specific key/values. An "apiKey" key
	// (if present) supplies the bearer token; other keys are ignored by the
	// dependency-free chat client here.
	Options map[string]string
}

// ResolveChatClient builds a ChatClient for the LLM-as-judge.
//
// Resolution order (#48 resolved — the eval layer now binds to the AI endpoint
// registry, falling back to the standalone env vars):
//
//  1. If src has an AI_ROLES["eval"] binding to a chat AI_ENDPOINTS entry, use
//     that endpoint's baseURL/model/options (apiKey via options["apiKey"]).
//  2. Otherwise fall back to EVAL_CHAT_BASE_URL / EVAL_CHAT_MODEL /
//     EVAL_CHAT_API_KEY (CONTRACT §2.15 stub, now the fallback).
//  3. If neither resolves, return a clear "not configured" error.
//
// src may be nil (e.g. a caller with no parsed config) — that just skips the
// registry and goes straight to the env-var fallback. The SSRF base-URL guard
// (validateBaseURL) is applied to both paths.
func ResolveChatClient(src EvalEndpointSource) (ChatClient, error) {
	if src != nil {
		if ep, ok := src.EvalEndpoint(); ok {
			cfg := chatConfig{
				BaseURL: strings.TrimSpace(ep.BaseURL),
				Model:   strings.TrimSpace(ep.Model),
				APIKey:  strings.TrimSpace(ep.Options["apiKey"]),
			}
			if cfg.BaseURL == "" || cfg.Model == "" {
				return nil, fmt.Errorf("eval chat endpoint (AI_ROLES.eval) is missing baseURL or model")
			}
			if err := validateBaseURL(cfg.BaseURL); err != nil {
				return nil, fmt.Errorf("invalid eval endpoint baseURL: %w", err)
			}
			return newOpenAIChatClient(cfg), nil
		}
	}

	cfg := chatConfig{
		BaseURL: strings.TrimSpace(os.Getenv("EVAL_CHAT_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("EVAL_CHAT_MODEL")),
		APIKey:  strings.TrimSpace(os.Getenv("EVAL_CHAT_API_KEY")),
	}
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, fmt.Errorf("eval chat endpoint not configured: bind AI_ROLES.eval to a chat AI_ENDPOINTS entry, or set EVAL_CHAT_BASE_URL and EVAL_CHAT_MODEL")
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("invalid EVAL_CHAT_BASE_URL: %w", err)
	}
	return newOpenAIChatClient(cfg), nil
}

// validateBaseURL requires a parseable http/https URL with a host, rejecting
// schemes like file://, gopher://, or a bare host. This is the same guard
// internal/config applies to AI_ENDPOINTS baseURLs and that endpointprobe.go
// applies before probing — it stops a mis- or maliciously-set EVAL_CHAT_BASE_URL
// (env injection) from steering the judge's request at a non-http target. It is
// inlined here (rather than importing internal/config) so the eval package stays
// independent of config's endpoint structs, which #48 is reshaping in parallel.
// Note this is a baseline scheme/host check, not an allowlist: an operator can
// still point it at any reachable http(s) host by design.
func validateBaseURL(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil {
		return fmt.Errorf("baseURL %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("baseURL %q must be http:// or https://", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("baseURL %q has no host", raw)
	}
	return nil
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
