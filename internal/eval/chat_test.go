package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

// captureServer is an OpenAI-compatible /chat/completions endpoint that records
// the decoded request body and Authorization header, and answers with
// respModel as the response "model" field.
type captureServer struct {
	srv   *httptest.Server
	body  map[string]any
	auth  string
	calls int
}

func newCaptureServer(t *testing.T, respModel string) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.calls++
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		cs.body = map[string]any{}
		if err := json.Unmarshal(raw, &cs.body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		cs.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"role":    "assistant",
				"content": `{"findings":[{"original_text":"quick","issue_type":"misheard_word","suggested_correction":"quack","confidence":0.9}]}`,
			}}},
		}
		if respModel != "" {
			resp["model"] = respModel
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

// TestChatClient_ThinkingControlsRequestBody pins the request body per route:
// the local Ollama/qwen default keeps today's reasoning_effort "none" +
// chat_template_kwargs {"enable_thinking": false}; an anthropic/* LiteLLM route
// sends neither by default; and both are overridable per deployment.
func TestChatClient_ThinkingControlsRequestBody(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		effortEnv  string
		kwargsEnv  string
		wantEffort any // nil = key absent
		wantKwargs any // nil = key absent
	}{
		{name: "ollama qwen default", model: "qwen3:8b",
			wantEffort: "none", wantKwargs: map[string]any{"enable_thinking": false}},
		{name: "anthropic route default omits both", model: "anthropic/claude-sonnet-4-5"},
		{name: "anthropic route is case-insensitive", model: "Anthropic/claude-haiku-4-5"},
		{name: "explicit auto equals default", model: "qwen3:8b", effortEnv: "auto", kwargsEnv: "AUTO",
			wantEffort: "none", wantKwargs: map[string]any{"enable_thinking": false}},
		{name: "omit both on a local model", model: "qwen3:8b", effortEnv: "omit", kwargsEnv: "omit"},
		{name: "force effort on a hosted route", model: "anthropic/claude-sonnet-4-5", effortEnv: "low",
			wantEffort: "low"},
		{name: "custom kwargs JSON", model: "qwen3:8b", kwargsEnv: `{"enable_thinking":false,"top_k":20}`,
			wantEffort: "none", wantKwargs: map[string]any{"enable_thinking": false, "top_k": float64(20)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newCaptureServer(t, "")
			t.Setenv(envReasoningEffort, tc.effortEnv)
			t.Setenv(envChatTemplateKwargs, tc.kwargsEnv)
			c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{
				BaseURL: cs.srv.URL + "/v1", Model: tc.model, APIKey: "sk-litellm",
			}})
			if err != nil {
				t.Fatalf("ResolveChatClient: %v", err)
			}
			if _, err := c.Complete(context.Background(), "sys", "usr"); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			if got := cs.body["model"]; got != tc.model {
				t.Errorf("model = %v, want %q", got, tc.model)
			}
			assertKey(t, cs.body, "reasoning_effort", tc.wantEffort)
			assertKey(t, cs.body, "chat_template_kwargs", tc.wantKwargs)
			// Unchanged on every route: schema-pinned output, the default
			// reply budget, and the apiKeyEnv-resolved bearer token. Temperature
			// is 0 locally and omitted on hosted routes (TestChatClient_Temperature).
			if _, ok := cs.body["response_format"]; !ok {
				t.Error("response_format must always be sent")
			}
			assertKey(t, cs.body, "max_tokens", defaultJudgeMaxTokens)
			var wantTemp any = 0
			if isHostedRoute(tc.model) {
				wantTemp = nil
			}
			assertKey(t, cs.body, "temperature", wantTemp)
			if cs.auth != "Bearer sk-litellm" {
				t.Errorf("Authorization = %q, want the apiKeyEnv-resolved bearer", cs.auth)
			}
		})
	}
}

func assertKey(t *testing.T, body map[string]any, key string, want any) {
	t.Helper()
	got, present := body[key]
	if want == nil {
		if present {
			t.Errorf("%s must be omitted, got %v", key, got)
		}
		return
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if !present || string(gb) != string(wb) {
		t.Errorf("%s = %s (present=%v), want %s", key, gb, present, wb)
	}
}

// The env-var fallback path (EVAL_CHAT_*) applies the same per-route rule.
func TestChatClient_EnvFallbackAppliesThinkingControls(t *testing.T) {
	cs := newCaptureServer(t, "")
	t.Setenv("EVAL_CHAT_BASE_URL", cs.srv.URL+"/v1")
	t.Setenv("EVAL_CHAT_MODEL", "anthropic/claude-sonnet-4-5")
	t.Setenv(envReasoningEffort, "")
	t.Setenv(envChatTemplateKwargs, "")
	c, err := ResolveChatClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	assertKey(t, cs.body, "reasoning_effort", nil)
	assertKey(t, cs.body, "chat_template_kwargs", nil)
}

// A malformed EVAL_CHAT_TEMPLATE_KWARGS is a configuration error, not a
// silently dropped knob.
func TestResolveChatClient_RejectsBadTemplateKwargs(t *testing.T) {
	for _, v := range []string{"{not json", "[1,2]", `"enable_thinking"`, "null"} {
		t.Setenv(envChatTemplateKwargs, v)
		_, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "http://gw:4000/v1", Model: "m"}})
		if err == nil || !strings.Contains(err.Error(), envChatTemplateKwargs) {
			t.Errorf("%s=%q: want a config error naming the var, got %v", envChatTemplateKwargs, v, err)
		}
	}
}

func TestIsHostedRoute(t *testing.T) {
	for model, want := range map[string]bool{
		"anthropic/claude-sonnet-4-5": true,
		" anthropic/x":                true,
		"bedrock/anthropic/claude-x":  true, // nested route
		"openrouter/Anthropic/claude": true,
		"judge":                       false, // alias hiding the provider: needs explicit omit
		"qwen3:8b":                    false,
		"qwen2.5:7b-instruct":         false,
		"claude-local-finetune":       false, // not a provider route
		"":                            false,
	} {
		if got := isHostedRoute(model); got != want {
			t.Errorf("isHostedRoute(%q) = %v, want %v", model, got, want)
		}
	}
}

// The response "model" field is recorded as the resolved model on each finding
// while Model keeps the requested id; RunStats aggregates the distinct models.
func TestJudge_RecordsResolvedModel(t *testing.T) {
	const requested = "anthropic/claude-sonnet-4-5"
	const resolved = "claude-sonnet-4-5-20250929"
	cs := newCaptureServer(t, resolved)
	c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{BaseURL: cs.srv.URL + "/v1", Model: requested}})
	if err != nil {
		t.Fatal(err)
	}
	findings, stats, err := RunOnChunks(context.Background(), NewJudge(c), nil, []db.EvalChunk{sampleChunk()}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Model != requested {
		t.Errorf("Model = %q, want the requested %q", f.Model, requested)
	}
	if f.ResolvedModel == nil || *f.ResolvedModel != resolved {
		t.Errorf("ResolvedModel = %v, want %q", f.ResolvedModel, resolved)
	}
	if stats.ResolvedModel != resolved {
		t.Errorf("stats.ResolvedModel = %q, want %q", stats.ResolvedModel, resolved)
	}
}

// An endpoint that omits "model" leaves the resolved model unset (NULL).
func TestJudge_NoResolvedModelWhenOmitted(t *testing.T) {
	cs := newCaptureServer(t, "")
	c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{BaseURL: cs.srv.URL + "/v1", Model: "qwen3:8b"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewJudge(c).JudgeChunk(context.Background(), sampleChunk())
	if err != nil {
		t.Fatal(err)
	}
	if res.ResolvedModel != "" || res.Findings[0].ResolvedModel != nil {
		t.Errorf("resolved model must be unset when the endpoint omits it, got %q / %v",
			res.ResolvedModel, res.Findings[0].ResolvedModel)
	}
}

func TestRunStats_AddResolvedDistinct(t *testing.T) {
	var s RunStats
	for _, m := range []string{"a", "", "a", "b", "a"} {
		s.addResolved(m)
	}
	if s.ResolvedModel != "a,b" {
		t.Errorf("ResolvedModel = %q, want \"a,b\" (distinct, first-seen order)", s.ResolvedModel)
	}
}

// newReplyServer answers every /chat/completions call with resp verbatim.
func newReplyServer(t *testing.T, resp string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// replyJSON builds a one-choice chat response.
func replyJSON(message map[string]any, finishReason string) string {
	choice := map[string]any{"message": message}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-haiku-5-5", "choices": []any{choice}})
	return string(b)
}

// TestChatClient_UnusableReplyFailsClosed: every reply the judge cannot use is
// a typed error (errors.Is-able, with a bounded errorClass label), never an
// empty Content that would parse to "no findings".
func TestChatClient_UnusableReplyFailsClosed(t *testing.T) {
	const findings = `{"findings":[]}`
	cases := []struct {
		name      string
		message   map[string]any
		finish    string
		wantErr   error // nil = success
		wantClass string
	}{
		{name: "normal success", message: map[string]any{"content": findings}, finish: "stop"},
		{name: "success without finish_reason", message: map[string]any{"content": findings}},
		{name: "empty content, no reasoning", message: map[string]any{"content": ""}, finish: "stop",
			wantErr: ErrEmptyResponse, wantClass: "empty"},
		{name: "whitespace content", message: map[string]any{"content": " \n\t "}, finish: "stop",
			wantErr: ErrEmptyResponse, wantClass: "empty"},
		{name: "null content, empty reasoning_content (Haiku 5.5 adaptive thinking)",
			message: map[string]any{"content": nil, "reasoning_content": ""}, finish: "stop",
			wantErr: ErrEmptyResponse, wantClass: "empty"},
		{name: "thinking-only", message: map[string]any{"content": "", "reasoning_content": "let me think"}, finish: "stop",
			wantErr: ErrThinkingOnlyResponse, wantClass: "thinking_only"},
		{name: "thinking-only via reasoning", message: map[string]any{"content": "", "reasoning": "hmm"},
			wantErr: ErrThinkingOnlyResponse, wantClass: "thinking_only"},
		{name: "length with partial content", message: map[string]any{"content": `{"findings":[{"orig`}, finish: "length",
			wantErr: ErrTruncatedResponse, wantClass: "truncated"},
		{name: "length with empty content (budget spent thinking)", message: map[string]any{"content": ""}, finish: "length",
			wantErr: ErrTruncatedResponse, wantClass: "truncated"},
		{name: "content_filter refusal", message: map[string]any{"content": ""}, finish: "content_filter",
			wantErr: ErrRefusalResponse, wantClass: "refusal"},
		{name: "refusal finish_reason", message: map[string]any{"content": ""}, finish: "refusal",
			wantErr: ErrRefusalResponse, wantClass: "refusal"},
		{name: "message.refusal set", message: map[string]any{"content": "", "refusal": "I can't help with that"}, finish: "stop",
			wantErr: ErrRefusalResponse, wantClass: "refusal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newReplyServer(t, replyJSON(tc.message, tc.finish))
			c := newOpenAIChatClient(chatConfig{BaseURL: srv.URL + "/v1", Model: "anthropic/claude-haiku-5-5"})
			comp, err := c.CompleteWithModel(context.Background(), "s", "u")
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				if comp.Content != findings {
					t.Errorf("Content = %q, want %q", comp.Content, findings)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if comp.Content != "" {
				t.Errorf("an unusable reply must not surface content, got %q", comp.Content)
			}
			if comp.ResolvedModel != "claude-haiku-5-5" {
				t.Errorf("ResolvedModel = %q, want it reported alongside the error", comp.ResolvedModel)
			}
			if got := errorClass(fmt.Errorf("judge chunk x: %w", err)); got != tc.wantClass {
				t.Errorf("errorClass = %q, want %q", got, tc.wantClass)
			}
			// Complete (the plain ChatClient path) fails the same way.
			if _, err := c.Complete(context.Background(), "s", "u"); !errors.Is(err, tc.wantErr) {
				t.Errorf("Complete err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRunOnChunks_UnusableReplyIsSkippedNotEvaluated: a fail-closed reply goes
// through the existing per-chunk failure path — skipped, FirstError set, run
// not Complete (so no caller latches eval_finished_at) — never "evaluated with
// zero findings".
func TestRunOnChunks_UnusableReplyIsSkippedNotEvaluated(t *testing.T) {
	for name, resp := range map[string]string{
		"empty":     replyJSON(map[string]any{"content": ""}, "stop"),
		"truncated": replyJSON(map[string]any{"content": `{"findings":[`}, "length"),
		"refusal":   replyJSON(map[string]any{"content": ""}, "content_filter"),
		"thinking":  replyJSON(map[string]any{"content": "", "reasoning_content": "x"}, "stop"),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newReplyServer(t, resp)
			j := NewJudge(newOpenAIChatClient(chatConfig{BaseURL: srv.URL + "/v1", Model: "anthropic/claude-haiku-5-5"}))
			chunks := []db.EvalChunk{sampleChunk(), sampleChunk()}
			chunks[1].ChunkID = "chunk-2"
			findings, stats, err := RunOnChunks(context.Background(), j, nil, chunks, false)
			if err != nil {
				t.Fatalf("a per-chunk reply failure must not abort the run: %v", err)
			}
			if len(findings) != 0 || stats.ChunksEvaluated != 0 || stats.ChunksSkipped != 2 {
				t.Errorf("findings=%d evaluated=%d skipped=%d, want 0/0/2",
					len(findings), stats.ChunksEvaluated, stats.ChunksSkipped)
			}
			if stats.Complete() {
				t.Error("a run with unusable replies must not be Complete (it would be latched)")
			}
			if stats.FirstError == "" {
				t.Error("FirstError must record the failure for run_metrics.eval_error")
			}
		})
	}
}

func TestParseChatOptions(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		opts    map[string]string
		want    chatParams
		wantErr string
	}{
		{name: "nil", opts: nil, want: chatParams{}},
		{name: "known keys", opts: map[string]string{"temperature": "0.2", "max_tokens": " 512 ", "top_p": "0.9"},
			want: chatParams{Temperature: f(0.2), MaxTokens: 512, TopP: f(0.9)}},
		{name: "explicit zero temperature", opts: map[string]string{"temperature": "0"},
			want: chatParams{Temperature: f(0)}},
		{name: "apiKey is a credential, never forwarded", opts: map[string]string{"apiKey": "sk-x"}, want: chatParams{}},
		{name: "unknown keys forwarded", opts: map[string]string{"top_k": "20", "seed": "7", "stop": `["\n"]`, "flag": "true", "user": "earmark"},
			want: chatParams{Extra: map[string]any{
				"top_k": json.RawMessage("20"), "seed": json.RawMessage("7"),
				"stop": json.RawMessage(`["\n"]`), "flag": json.RawMessage("true"), "user": "earmark",
			}}},
		{name: "JSON string or null stays the literal string", opts: map[string]string{"a": `"q"`, "b": "null"},
			want: chatParams{Extra: map[string]any{"a": `"q"`, "b": "null"}}},
		{name: "bad temperature", opts: map[string]string{"temperature": "warm"}, wantErr: `"temperature"`},
		{name: "negative top_p", opts: map[string]string{"top_p": "-1"}, wantErr: `"top_p"`},
		{name: "zero max_tokens", opts: map[string]string{"max_tokens": "0"}, wantErr: `"max_tokens"`},
		{name: "non-integer max_tokens", opts: map[string]string{"max_tokens": "1.5"}, wantErr: `"max_tokens"`},
		{name: "reserved response_format", opts: map[string]string{"response_format": "{}"}, wantErr: "cannot be overridden"},
		{name: "reserved reasoning_effort names its env var", opts: map[string]string{"reasoning_effort": "low"}, wantErr: envReasoningEffort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChatOptions(tc.opts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(tc.want)
			if string(gb) != string(wb) {
				t.Errorf("params = %s, want %s", gb, wb)
			}
		})
	}
}

// AI_ENDPOINTS options reach the request body through the registry resolver:
// known keys parsed and sent, unknown keys forwarded, apiKey only as the bearer.
func TestChatClient_OptionsForwarded(t *testing.T) {
	cs := newCaptureServer(t, "")
	t.Setenv(envReasoningEffort, "")
	t.Setenv(envChatTemplateKwargs, "")
	c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{
		BaseURL: cs.srv.URL + "/v1", Model: "anthropic/claude-haiku-5-5",
		Options: map[string]string{
			"temperature": "0.3", "max_tokens": "16000", "top_p": "0.95",
			"top_k": "20", "metadata": `{"tag":"earmark"}`, "user": "earmark-judge",
			"apiKey": "sk-from-options",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	assertKey(t, cs.body, "temperature", 0.3)
	assertKey(t, cs.body, "max_tokens", 16000)
	assertKey(t, cs.body, "top_p", 0.95)
	assertKey(t, cs.body, "top_k", 20)
	assertKey(t, cs.body, "metadata", map[string]any{"tag": "earmark"})
	assertKey(t, cs.body, "user", "earmark-judge")
	assertKey(t, cs.body, "apiKey", nil)
	if cs.auth != "Bearer sk-from-options" {
		t.Errorf("Authorization = %q, want the options.apiKey back-compat bearer", cs.auth)
	}
	// earmark's own fields are intact.
	if cs.body["model"] != "anthropic/claude-haiku-5-5" {
		t.Errorf("model = %v", cs.body["model"])
	}
	if _, ok := cs.body["response_format"]; !ok {
		t.Error("response_format must still be sent")
	}
}

// A bad option fails ResolveChatClient (startup), it is not silently dropped.
func TestResolveChatClient_RejectsBadOptions(t *testing.T) {
	_, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{
		BaseURL: "http://gw:4000/v1", Model: "m", Options: map[string]string{"max_tokens": "lots"},
	}})
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("want a config error naming max_tokens, got %v", err)
	}
}

// Temperature per route: omitted on a hosted (anthropic/) route unless
// configured, 0 on a local (ollama/) route, explicit option always sent — and
// the request, the recipe params and the span attribute all agree.
func TestChatClient_TemperaturePerRoute(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		opts     map[string]string
		wantTemp any // nil = omitted everywhere
	}{
		{name: "hosted route omits", model: "anthropic/claude-haiku-5-5"},
		{name: "nested hosted route omits", model: "bedrock/anthropic/claude-haiku-5-5"},
		{name: "ollama route sends 0", model: "ollama/qwen3.8", wantTemp: 0},
		{name: "bare local model sends 0", model: "qwen3:8b", wantTemp: 0},
		{name: "explicit option on hosted route is sent", model: "anthropic/claude-haiku-4-5",
			opts: map[string]string{"temperature": "0"}, wantTemp: 0},
		{name: "explicit option on local route overrides 0", model: "qwen3:8b",
			opts: map[string]string{"temperature": "0.7"}, wantTemp: 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans, _ := installTestProviders(t)
			cs := newCaptureServer(t, "")
			c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{
				BaseURL: cs.srv.URL + "/v1", Model: tc.model, Options: tc.opts,
			}})
			if err != nil {
				t.Fatal(err)
			}
			j := NewJudge(c)
			if _, err := j.JudgeChunk(context.Background(), sampleChunk()); err != nil {
				t.Fatal(err)
			}

			assertKey(t, cs.body, "temperature", tc.wantTemp)
			assertKey(t, cs.body, "max_tokens", defaultJudgeMaxTokens)

			param, inRecipe := j.Recipe().Params["temperature"]
			spanTemp, inSpan := spanAttrs(spans.GetSpans()[0])["gen_ai.request.temperature"]
			if tc.wantTemp == nil {
				if inRecipe || inSpan {
					t.Errorf("temperature not sent, but recipe has %v (%v) / span has %v (%v)",
						param, inRecipe, spanTemp.AsFloat64(), inSpan)
				}
				return
			}
			want := float64(0)
			if f, ok := tc.wantTemp.(float64); ok {
				want = f
			}
			if param != want {
				t.Errorf("recipe temperature = %v, want %v", param, want)
			}
			if !inSpan || spanTemp.AsFloat64() != want {
				t.Errorf("span temperature = %v (present=%v), want %v", spanTemp.AsFloat64(), inSpan, want)
			}
		})
	}
}

// A local route's recipe is byte-identical to the historical one (params
// temperature 0 / min_confidence / max_findings_per_chunk), so local judges do
// not see their findings go stale; a hosted route's differs (temperature
// dropped) — the intended one-time re-stamp at the judge model switch.
func TestJudgeRecipe_TemperatureOnlyWhenSent(t *testing.T) {
	legacyID := func(j *Judge) string {
		r := j.Recipe()
		r.Params = map[string]any{
			"temperature":            0,
			"min_confidence":         j.minConf,
			"max_findings_per_chunk": j.maxPerChunk,
		}
		return mustRecipeID(t, r)
	}
	local := NewJudge(newOpenAIChatClient(chatConfig{BaseURL: "http://ollama:11434/v1", Model: "qwen3.8"}))
	if got, want := mustRecipeID(t, local.Recipe()), legacyID(local); got != want {
		t.Errorf("local recipe id changed: %s, want the historical %s", got, want)
	}
	hosted := NewJudge(newOpenAIChatClient(chatConfig{BaseURL: "http://litellm:4000/v1", Model: "anthropic/claude-haiku-5-5"}))
	if _, ok := hosted.Recipe().Params["temperature"]; ok {
		t.Error("hosted recipe must not record a temperature that is not sent")
	}
	if mustRecipeID(t, hosted.Recipe()) == legacyID(hosted) {
		t.Error("dropping temperature must change the hosted recipe id")
	}
	// A ChatClient that does not report its temperature keeps the historical 0.
	if p := NewJudge(&fakeChat{}).Recipe().Params["temperature"]; p != float64(0) {
		t.Errorf("non-reporting client temperature param = %v, want 0", p)
	}
}
