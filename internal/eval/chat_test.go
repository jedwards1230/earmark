package eval

import (
	"context"
	"encoding/json"
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
			// Unchanged on every route: schema-pinned output, deterministic judge,
			// and the apiKeyEnv-resolved bearer token.
			if _, ok := cs.body["response_format"]; !ok {
				t.Error("response_format must always be sent")
			}
			if cs.body["temperature"] != float64(0) {
				t.Errorf("temperature = %v, want 0", cs.body["temperature"])
			}
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
