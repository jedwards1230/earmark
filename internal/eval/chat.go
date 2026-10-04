package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	// responseFormat, when non-nil, pins replies to a JSON schema server-side.
	// reasoningEffort / chatTemplateKwargs suppress extended thinking on models
	// that support either spelling. All three are set at construction because
	// this client serves exactly one caller (the judge) with one output shape.
	responseFormat     *responseFormat
	reasoningEffort    string
	chatTemplateKwargs map[string]any
}

// chatConfig is resolved from the AI endpoint registry (AI_ROLES["eval"]) or,
// when no eval role is bound, from the standalone EVAL_CHAT_* env vars.
type chatConfig struct {
	BaseURL string
	Model   string
	APIKey  string
	// ReasoningEffort / ChatTemplateKwargs are the thinking-suppression fields
	// to send; "" / nil omits them. Resolved by thinkingControls.
	ReasoningEffort    string
	ChatTemplateKwargs map[string]any
}

// Thinking-control env vars (CONTRACT §2.15). Both default to "auto".
const (
	// envReasoningEffort overrides the reasoning_effort request field:
	// unset/"auto" → per-route default, "omit" → never sent, any other value →
	// sent verbatim (e.g. "none", "low").
	envReasoningEffort = "EVAL_REASONING_EFFORT"
	// envChatTemplateKwargs overrides chat_template_kwargs: unset/"auto" →
	// per-route default, "omit" → never sent, a JSON object → sent verbatim.
	envChatTemplateKwargs = "EVAL_CHAT_TEMPLATE_KWARGS"

	thinkingAuto = "auto"
	thinkingOmit = "omit"
)

// hostedRouteMarkers are model-id segments that name a hosted provider route
// (LiteLLM's "<provider>/<model>" convention) rather than a local Ollama/vLLM
// model. They match ANYWHERE in the id, so nested routes such as
// "bedrock/anthropic/claude-…" or "openrouter/anthropic/…" count too. Hosted
// APIs reject or mis-map the local thinking knobs — Anthropic has no
// reasoning_effort "none" and no chat_template_kwargs — so by default neither
// is sent to them.
//
// A LiteLLM alias that hides the provider (e.g. model "judge" mapped to an
// Anthropic model in the proxy config) cannot be detected from the id: set
// EVAL_REASONING_EFFORT=omit and EVAL_CHAT_TEMPLATE_KWARGS=omit for it.
var hostedRouteMarkers = []string{"anthropic/"}

// isHostedRoute reports whether model names a hosted provider route.
func isHostedRoute(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, marker := range hostedRouteMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// thinkingControls resolves the reasoning_effort / chat_template_kwargs to send
// for model. Default ("auto"): keep today's local-model behavior —
// reasoning_effort "none" plus {"enable_thinking": false}, which is what stops
// Qwen-family models on Ollama from spending the reply on chain-of-thought —
// except for hosted routes (isHostedRoute), where both are omitted. Either can
// be forced per deployment through EVAL_REASONING_EFFORT /
// EVAL_CHAT_TEMPLATE_KWARGS. An unparseable kwargs value is a configuration
// error (fail loud rather than silently drop the knob).
func thinkingControls(model string) (string, map[string]any, error) {
	hosted := isHostedRoute(model)

	effort := ""
	switch v := strings.TrimSpace(os.Getenv(envReasoningEffort)); strings.ToLower(v) {
	case "", thinkingAuto:
		if !hosted {
			effort = "none"
		}
	case thinkingOmit:
	default:
		effort = v
	}

	var kwargs map[string]any
	switch v := strings.TrimSpace(os.Getenv(envChatTemplateKwargs)); strings.ToLower(v) {
	case "", thinkingAuto:
		if !hosted {
			kwargs = map[string]any{"enable_thinking": false}
		}
	case thinkingOmit:
	default:
		if err := json.Unmarshal([]byte(v), &kwargs); err != nil || kwargs == nil {
			return "", nil, fmt.Errorf("%s must be \"auto\", \"omit\" or a JSON object: %q", envChatTemplateKwargs, v)
		}
	}
	return effort, kwargs, nil
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
	// APIKey is the bearer token resolved from the endpoint's apiKeyEnv. It
	// takes precedence over Options["apiKey"].
	APIKey string
	// Options are the endpoint's backend-specific key/values. An "apiKey" key
	// is still honored as a back-compat bearer token when APIKey is empty, but
	// options are plaintext and shown on the dashboard, so prefer apiKeyEnv.
	// Other keys are ignored by the dependency-free chat client here.
	Options map[string]string
}

// ResolveChatClient builds a ChatClient for the LLM-as-judge.
//
// Resolution order (#48 resolved — the eval layer now binds to the AI endpoint
// registry, falling back to the standalone env vars):
//
//  1. If src has an AI_ROLES["eval"] binding to a chat AI_ENDPOINTS entry, use
//     that endpoint's baseURL/model. The bearer token is the key resolved from
//     its apiKeyEnv, else options["apiKey"] (back-compat).
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
			apiKey := strings.TrimSpace(ep.APIKey)
			if apiKey == "" {
				apiKey = strings.TrimSpace(ep.Options["apiKey"])
			}
			cfg := chatConfig{
				BaseURL: strings.TrimSpace(ep.BaseURL),
				Model:   strings.TrimSpace(ep.Model),
				APIKey:  apiKey,
			}
			if cfg.BaseURL == "" || cfg.Model == "" {
				return nil, fmt.Errorf("eval chat endpoint (AI_ROLES.eval) is missing baseURL or model")
			}
			if err := validateBaseURL(cfg.BaseURL); err != nil {
				return nil, fmt.Errorf("invalid eval endpoint baseURL: %w", err)
			}
			return withThinkingControls(cfg)
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
	return withThinkingControls(cfg)
}

// withThinkingControls resolves the per-route thinking knobs into cfg and
// builds the client.
func withThinkingControls(cfg chatConfig) (ChatClient, error) {
	effort, kwargs, err := thinkingControls(cfg.Model)
	if err != nil {
		return nil, err
	}
	cfg.ReasoningEffort = effort
	cfg.ChatTemplateKwargs = kwargs
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

		// Pin the reply shape server-side, and (per thinkingControls) ask a
		// local thinking-capable model to stop thinking. Both reasoning
		// spellings are sent by default: local endpoints that do not recognise
		// a field ignore it, and the two model families disagree on which one
		// to honour. Hosted routes get neither by default — they reject them.
		// If a reasoning model ignores both, Complete fails with
		// ErrThinkingOnlyResponse rather than reporting a clean transcript.
		responseFormat:     findingsResponseFormat,
		reasoningEffort:    cfg.ReasoningEffort,
		chatTemplateKwargs: cfg.ChatTemplateKwargs,
	}
}

func (c *openAIChatClient) Model() string { return c.model }

// chatRequest / chatResponse are the trimmed OpenAI chat-completions shapes.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	// ResponseFormat pins the reply to a JSON schema server-side. Verified
	// against Ollama's OpenAI-compatible endpoint (v0.32.x): it honours
	// {"type":"json_schema"} and returns schema-conforming output, which is
	// what makes the patch contract reliable enough to drive a review UI
	// instead of hoping the prompt is obeyed. Omitted when nil so endpoints
	// that do not support it are unaffected.
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	// ReasoningEffort is the OpenAI-style knob for suppressing extended
	// thinking. Sent only when configured — see the reasoning-model note on
	// chatResponse for why this matters.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ChatTemplateKwargs is Ollama's passthrough into the model's chat
	// template; {"enable_thinking": false} is how several Qwen-family models
	// disable thinking. Belt-and-braces alongside ReasoningEffort because the
	// two families spell the same intent differently.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

// responseFormat is the OpenAI structured-output envelope.
type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string `json:"name"`
	Strict bool   `json:"strict"`
	Schema any    `json:"schema"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Reasoning captures the field a thinking model puts its chain-of-thought
	// in. We never use its contents — it exists so the judge can tell
	// "the model thought but said nothing" apart from "the model found no
	// errors". Without it a reasoning model returns empty Content and the
	// judge records zero findings, which is indistinguishable from a clean
	// transcript. That failure is silent and corpus-wide, and it is exactly
	// how gemma4:12b was rejected for this role.
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type chatResponse struct {
	// Model is the model that actually served the request. A router (LiteLLM)
	// may resolve the requested alias to a dated id or fall back to another
	// model, so it is recorded alongside the requested one.
	Model   string `json:"model"`
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Completion is one chat reply plus the model the endpoint reports serving it.
type Completion struct {
	Content string
	// ResolvedModel is the response's "model" field; "" when the endpoint
	// omits it.
	ResolvedModel string
}

// ErrThinkingOnlyResponse means the model returned reasoning but no answer.
// Surfaced rather than swallowed: an empty judge reply must never be quietly
// read as "no findings".
var ErrThinkingOnlyResponse = errors.New("model returned reasoning but empty content (thinking not suppressed)")

// Complete posts a system+user prompt to /chat/completions and returns the first
// choice's message content. See CompleteWithModel.
func (c *openAIChatClient) Complete(ctx context.Context, system, user string) (string, error) {
	r, err := c.CompleteWithModel(ctx, system, user)
	return r.Content, err
}

// CompleteWithModel is Complete plus the resolved model the endpoint reports.
// Temperature is 0 for a deterministic, reproducible judge (the same span
// should flag the same way run to run).
func (c *openAIChatClient) CompleteWithModel(ctx context.Context, system, user string) (Completion, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat:     c.responseFormat,
		ReasoningEffort:    c.reasoningEffort,
		ChatTemplateKwargs: c.chatTemplateKwargs,
	})
	if err != nil {
		return Completion{}, fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Completion{}, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Completion{}, fmt.Errorf("chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Completion{}, fmt.Errorf("read chat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Completion{}, fmt.Errorf("chat endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Completion{}, fmt.Errorf("unmarshal chat response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Completion{}, fmt.Errorf("chat endpoint returned no choices")
	}

	msg := parsed.Choices[0].Message
	// Fail loudly on the thinking-only reply. Returning "" here would parse to
	// zero findings and be recorded as a clean chunk — a silent false negative
	// across every chunk the judge touches.
	if strings.TrimSpace(msg.Content) == "" &&
		(strings.TrimSpace(msg.Reasoning) != "" || strings.TrimSpace(msg.ReasoningContent) != "") {
		return Completion{}, ErrThinkingOnlyResponse
	}
	return Completion{Content: msg.Content, ResolvedModel: strings.TrimSpace(parsed.Model)}, nil
}
