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
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/genai"
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

	// Sampling and budget fields resolved from the endpoint options (see
	// newOpenAIChatClient for the per-route defaults). temperature/topP nil →
	// omitted from the request; extra holds unknown option keys forwarded
	// into the request body.
	temperature *float64
	topP        *float64
	maxTokens   int
	extra       map[string]any
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
	// Params are the request knobs parsed from the endpoint's options
	// (parseChatOptions). The zero value means "all defaults".
	Params chatParams
}

// defaultJudgeMaxTokens is the max_tokens sent when the endpoint's options do
// not set one. The judge's reply itself is small (at most a handful of
// findings, a few hundred tokens), but on hosted reasoning models (Claude
// Haiku 5.5 thinks adaptively by default) the output budget is SHARED with
// thinking, so a tight cap truncates the answer after the model spent it
// thinking (finish_reason "length" → ErrTruncatedResponse). 8192 leaves ample
// room for adaptive thinking plus the JSON reply, and is still a hard ceiling
// on a runaway generation. Local Ollama maps it onto num_predict.
const defaultJudgeMaxTokens = 8192

// chatParams are the sampling / budget fields of one chat request, parsed from
// an AI_ENDPOINTS entry's options (CONTRACT §2.14).
type chatParams struct {
	// Temperature / TopP are nil when not configured. A nil Temperature is
	// filled per route by newOpenAIChatClient (0 for local models, omitted for
	// hosted routes).
	Temperature *float64
	TopP        *float64
	// MaxTokens is 0 when not configured (→ defaultJudgeMaxTokens).
	MaxTokens int
	// Extra are the unknown option keys, forwarded into the request body.
	Extra map[string]any
}

// Option keys with dedicated handling. optionAPIKey is the deprecated
// back-compat bearer token: consumed by ResolveChatClient, never forwarded.
const (
	optionTemperature = "temperature"
	optionMaxTokens   = "max_tokens"
	optionTopP        = "top_p"
	optionAPIKey      = "apiKey"
)

// reservedRequestKeys are request-body fields earmark sets itself. An option
// with one of these keys would silently fight the judge's contract (the reply
// schema, the thinking controls, the prompt), so it is a configuration error
// rather than an override. Thinking controls have their own env vars.
var reservedRequestKeys = map[string]string{
	"model":                "set the endpoint's model field",
	"messages":             "the judge builds the prompt",
	"stream":               "the judge needs a single non-streamed reply",
	"response_format":      "the judge pins its own JSON schema",
	"reasoning_effort":     "use " + envReasoningEffort,
	"chat_template_kwargs": "use " + envChatTemplateKwargs,
}

// parseChatOptions turns an endpoint's string-valued options into request
// params. Known keys are parsed as numbers (temperature, top_p: floats;
// max_tokens: a positive integer) and an unparseable value is a configuration
// error — failing loud beats silently sending the backend default. "apiKey"
// is skipped (it is a credential, handled by ResolveChatClient). Reserved keys
// (reservedRequestKeys) are rejected. Every other key is forwarded into the
// request body: as the JSON value when the string is a JSON number, boolean,
// array or object (options are strings on the wire, so this is the only way to
// send e.g. top_k: 20), else as the string itself.
func parseChatOptions(opts map[string]string) (chatParams, error) {
	var p chatParams
	for k, raw := range opts {
		v := strings.TrimSpace(raw)
		switch k {
		case optionAPIKey:
			continue
		case optionTemperature, optionTopP:
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return chatParams{}, fmt.Errorf("eval endpoint option %q must be a non-negative number, got %q", k, raw)
			}
			if k == optionTemperature {
				p.Temperature = &f
			} else {
				p.TopP = &f
			}
		case optionMaxTokens:
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return chatParams{}, fmt.Errorf("eval endpoint option %q must be a positive integer, got %q", k, raw)
			}
			p.MaxTokens = n
		default:
			if why, reserved := reservedRequestKeys[k]; reserved {
				return chatParams{}, fmt.Errorf("eval endpoint option %q is set by earmark and cannot be overridden (%s)", k, why)
			}
			if p.Extra == nil {
				p.Extra = map[string]any{}
			}
			p.Extra[k] = optionValue(raw)
		}
	}
	return p, nil
}

// optionValue decodes an unknown option's string as a JSON number, boolean,
// array or object when it is one, else returns the string unchanged.
func optionValue(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		switch v.(type) {
		case float64, bool, []any, map[string]any:
			return json.RawMessage(strings.TrimSpace(raw))
		}
	}
	return raw
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
	// temperature, max_tokens and top_p are parsed and sent; other keys are
	// forwarded into the request body (parseChatOptions).
	Options map[string]string
}

// ResolveChatClient builds a ChatClient for the LLM-as-judge.
//
// Resolution order (#48 resolved — the eval layer now binds to the AI endpoint
// registry, falling back to the standalone env vars):
//
//  1. If src has an AI_ROLES["eval"] binding to a chat AI_ENDPOINTS entry, use
//     that endpoint's baseURL/model/options. The bearer token is the key resolved from
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
				apiKey = strings.TrimSpace(ep.Options[optionAPIKey])
			}
			// Sampling / budget options (CONTRACT §2.14): a malformed value is
			// a configuration error, surfaced at startup like a bad baseURL.
			params, err := parseChatOptions(ep.Options)
			if err != nil {
				return nil, err
			}
			cfg := chatConfig{
				BaseURL: strings.TrimSpace(ep.BaseURL),
				Model:   strings.TrimSpace(ep.Model),
				APIKey:  apiKey,
				Params:  params,
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
		return nil, fmt.Errorf("%w: bind AI_ROLES.eval to a chat AI_ENDPOINTS entry, or set EVAL_CHAT_BASE_URL and EVAL_CHAT_MODEL", ErrChatNotConfigured)
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

// newOpenAIChatClient builds the client, applying the per-route defaults for
// knobs the options left unset:
//   - max_tokens: defaultJudgeMaxTokens.
//   - temperature: 0 for local models (a deterministic, reproducible judge —
//     the same span should flag the same way run to run); OMITTED for hosted
//     routes (isHostedRoute), whose newer models reject any non-default
//     sampling parameter (Claude Haiku 5.5 400s on temperature/top_p/top_k).
//     An explicit temperature option is always sent.
func newOpenAIChatClient(cfg chatConfig) *openAIChatClient {
	temperature := cfg.Params.Temperature
	if temperature == nil && !isHostedRoute(cfg.Model) {
		zero := 0.0
		temperature = &zero
	}
	maxTokens := cfg.Params.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultJudgeMaxTokens
	}
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

		temperature: temperature,
		topP:        cfg.Params.TopP,
		maxTokens:   maxTokens,
		extra:       cfg.Params.Extra,
	}
}

func (c *openAIChatClient) Model() string { return c.model }

// Temperature reports the temperature this client sends; nil when the field
// is omitted (the provider's default applies). Implements TemperatureReporter.
func (c *openAIChatClient) Temperature() *float64 { return c.temperature }

// chatRequest / chatResponse are the trimmed OpenAI chat-completions shapes.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Temperature / TopP are pointers so "not configured" is omitted rather
	// than sent as 0: hosted reasoning models reject non-default sampling.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	// MaxTokens bounds the reply (shared with thinking on hosted reasoning
	// models). Always set by the client; omitempty only guards the zero value.
	MaxTokens int `json:"max_tokens,omitempty"`
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
	// Refusal is OpenAI's structured-output refusal text. Response-only; any
	// non-empty value is a refusal (ErrRefusalResponse).
	Refusal string `json:"refusal,omitempty"`
}

type chatResponse struct {
	// Model is the model that actually served the request. A router (LiteLLM)
	// may resolve the requested alias to a dated id or fall back to another
	// model, so it is recorded alongside the requested one.
	Model   string `json:"model"`
	Choices []struct {
		Message chatMessage `json:"message"`
		// FinishReason is why generation stopped: "stop" (normal), "length"
		// (hit max_tokens — the reply is truncated), "content_filter" (a
		// refusal; LiteLLM maps Anthropic's stop_reason "refusal" here), …
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage is the OpenAI token accounting; absent on some endpoints.
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// Completion is one chat reply plus the model the endpoint reports serving it.
type Completion struct {
	Content string
	// ResolvedModel is the response's "model" field; "" when the endpoint
	// omits it.
	ResolvedModel string
	// InputTokens / OutputTokens are the response's usage counts; HasUsage is
	// false when the endpoint reported none.
	InputTokens  int
	OutputTokens int
	HasUsage     bool
}

// Endpoint describes where a chat client sends requests, for telemetry
// (gen_ai.provider.name, server.address, server.port).
type Endpoint = genai.Endpoint

// EndpointReporter is an optional ChatClient extension describing its
// endpoint. openAIChatClient implements it.
type EndpointReporter interface {
	Endpoint() Endpoint
}

// Endpoint reports the client's provider and server. The provider is the
// LiteLLM-style route prefix of the model id ("anthropic/…" → "anthropic"),
// else "openai_compatible": the client speaks the OpenAI chat API, but the
// server behind it (Ollama, vLLM, a LiteLLM alias) is not knowable from here.
func (c *openAIChatClient) Endpoint() Endpoint {
	e := Endpoint{Provider: genai.RouteProvider(c.model, "openai_compatible")}
	e.Host, e.Port = genai.Server(c.baseURL)
	return e
}

// StatusError is a non-200 reply from the chat endpoint. Error() keeps the
// body for operator logs; telemetry records only the code (the body can echo
// the request, i.e. transcript text — see errorClass).
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("chat endpoint returned %d: %s", e.Code, e.Body)
}

// ErrChatNotConfigured: no eval chat endpoint is configured at all (as opposed
// to one that is configured but invalid).
var ErrChatNotConfigured = errors.New("eval chat endpoint not configured")

// Unusable-reply errors. The judge FAILS CLOSED on each: an unusable reply is
// returned as an error — so the chunk is counted skipped, the run is not
// Complete and the transcript is never latched as judged — instead of being
// parsed as "no findings", which would be indistinguishable from a clean
// chunk and never retried. errorClass maps each to a bounded label.
var (
	// ErrThinkingOnlyResponse means the model returned reasoning but no answer.
	ErrThinkingOnlyResponse = errors.New("model returned reasoning but empty content (thinking not suppressed)")
	// ErrEmptyResponse means the reply's content was empty or whitespace with
	// no reasoning either (e.g. a hosted reasoning model whose thinking blocks
	// come back empty).
	ErrEmptyResponse = errors.New("model returned empty content")
	// ErrTruncatedResponse means generation stopped at max_tokens
	// (finish_reason "length"): whatever content came back is cut off.
	ErrTruncatedResponse = errors.New("model reply truncated at max_tokens")
	// ErrRefusalResponse means the model refused (finish_reason
	// "content_filter"/"refusal", or a non-empty message.refusal).
	ErrRefusalResponse = errors.New("model refused the request")
)

// replyError classifies an unusable reply, or returns nil when the reply can
// be parsed. Checked in this order: refusal, truncation, then emptiness — a
// refusal or truncation explains an empty body better than "empty" does. The
// returned error never carries the content (it may echo transcript text).
func replyError(finishReason string, msg chatMessage) error {
	fr := strings.ToLower(strings.TrimSpace(finishReason))
	switch {
	case fr == "content_filter" || fr == "refusal" || strings.TrimSpace(msg.Refusal) != "":
		return fmt.Errorf("%w (finish_reason=%q)", ErrRefusalResponse, finishReason)
	case fr == "length":
		return fmt.Errorf("%w (finish_reason=%q)", ErrTruncatedResponse, finishReason)
	case strings.TrimSpace(msg.Content) != "":
		return nil
	case strings.TrimSpace(msg.Reasoning) != "" || strings.TrimSpace(msg.ReasoningContent) != "":
		return ErrThinkingOnlyResponse
	default:
		return fmt.Errorf("%w (finish_reason=%q)", ErrEmptyResponse, finishReason)
	}
}

// Complete posts a system+user prompt to /chat/completions and returns the first
// choice's message content. See CompleteWithModel.
func (c *openAIChatClient) Complete(ctx context.Context, system, user string) (string, error) {
	r, err := c.CompleteWithModel(ctx, system, user)
	return r.Content, err
}

// CompleteWithModel is Complete plus the resolved model the endpoint reports.
// The sampling/budget fields are the ones resolved at construction
// (newOpenAIChatClient). An unusable reply — empty, thinking-only, truncated
// or refused — is an error (replyError), never an empty Content.
func (c *openAIChatClient) CompleteWithModel(ctx context.Context, system, user string) (Completion, error) {
	body, err := c.requestBody(system, user)
	if err != nil {
		return Completion{}, err
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
		return Completion{}, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Completion{}, fmt.Errorf("unmarshal chat response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Completion{}, fmt.Errorf("chat endpoint returned no choices")
	}

	choice := parsed.Choices[0]
	out := Completion{Content: choice.Message.Content, ResolvedModel: strings.TrimSpace(parsed.Model)}
	if parsed.Usage != nil {
		out.InputTokens = parsed.Usage.PromptTokens
		out.OutputTokens = parsed.Usage.CompletionTokens
		out.HasUsage = true
	}
	// Fail closed on an unusable reply. Returning it as Content would parse to
	// zero findings and be recorded as a clean chunk — a silent false negative
	// across every chunk the judge touches. The resolved model and usage are
	// still reported alongside the error; the content is dropped.
	if rerr := replyError(choice.FinishReason, choice.Message); rerr != nil {
		out.Content = ""
		return out, rerr
	}
	return out, nil
}

// requestBody marshals the chat request: the typed fields, plus any unknown
// endpoint options merged in (never overriding a field earmark set).
func (c *openAIChatClient) requestBody(system, user string) ([]byte, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature:        c.temperature,
		TopP:               c.topP,
		MaxTokens:          c.maxTokens,
		ResponseFormat:     c.responseFormat,
		ReasoningEffort:    c.reasoningEffort,
		ChatTemplateKwargs: c.chatTemplateKwargs,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	if len(c.extra) == 0 {
		return body, nil
	}
	merged := map[string]any{}
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("merge chat request options: %w", err)
	}
	for k, v := range c.extra {
		if _, set := merged[k]; !set {
			merged[k] = v
		}
	}
	body, err = json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return body, nil
}
