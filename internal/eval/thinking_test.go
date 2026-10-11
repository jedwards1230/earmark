package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

const lunaModel = "openrouter/openai/gpt-6-luna"

var localKwargs = map[string]any{"enable_thinking": false}

// TestResolveThinking_Precedence: per field, the propose model setting param
// beats the EVAL_* env var, which beats the route default; "auto" in a layer
// means the route default, "omit" never sends the field.
func TestResolveThinking_Precedence(t *testing.T) {
	cases := []struct {
		name                     string
		model                    string
		setting                  JudgeSetting
		effortEnv, kwargsEnv     string
		wantEffort               string
		wantKwargs               map[string]any
		wantEffortSrc, wantKwSrc string
	}{
		{name: "local default", model: "qwen3:8b",
			wantEffort: "none", wantKwargs: localKwargs, wantEffortSrc: ThinkingFromAuto, wantKwSrc: ThinkingFromAuto},
		{name: "hosted default", model: lunaModel,
			wantEffortSrc: ThinkingFromAuto, wantKwSrc: ThinkingFromAuto},
		{name: "env beats default", model: lunaModel, effortEnv: "medium", kwargsEnv: `{"x":1}`,
			wantEffort: "medium", wantKwargs: map[string]any{"x": float64(1)}, wantEffortSrc: ThinkingFromEnv, wantKwSrc: ThinkingFromEnv},
		{name: "setting beats env", model: lunaModel, effortEnv: "none", kwargsEnv: `{"enable_thinking":false}`,
			setting:    JudgeSetting{ReasoningEffort: "low", ChatTemplateKwargs: "omit"},
			wantEffort: "low", wantEffortSrc: ThinkingFromSetting, wantKwSrc: ThinkingFromSetting},
		{name: "setting auto beats env, means route default", model: "qwen3:8b", effortEnv: "omit", kwargsEnv: "omit",
			setting:    JudgeSetting{ReasoningEffort: "auto", ChatTemplateKwargs: "AUTO"},
			wantEffort: "none", wantKwargs: localKwargs, wantEffortSrc: ThinkingFromSetting, wantKwSrc: ThinkingFromSetting},
		{name: "per field: setting effort, env kwargs", model: "qwen3:8b", kwargsEnv: "omit",
			setting:    JudgeSetting{ReasoningEffort: "high"},
			wantEffort: "high", wantEffortSrc: ThinkingFromSetting, wantKwSrc: ThinkingFromEnv},
		{name: "setting kwargs object", model: "qwen3:8b",
			setting:    JudgeSetting{ChatTemplateKwargs: `{"enable_thinking":true}`},
			wantEffort: "none", wantKwargs: map[string]any{"enable_thinking": true}, wantEffortSrc: ThinkingFromAuto, wantKwSrc: ThinkingFromSetting},
		{name: "env omit on local", model: "qwen3:8b", effortEnv: "omit", kwargsEnv: "omit",
			wantEffortSrc: ThinkingFromEnv, wantKwSrc: ThinkingFromEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envReasoningEffort, tc.effortEnv)
			t.Setenv(envChatTemplateKwargs, tc.kwargsEnv)
			got, err := ResolveThinking(tc.model, tc.setting)
			if err != nil {
				t.Fatal(err)
			}
			if got.ReasoningEffort != tc.wantEffort || !sameJSON(got.ChatTemplateKwargs, tc.wantKwargs) {
				t.Errorf("got effort %q kwargs %v, want %q %v", got.ReasoningEffort, got.ChatTemplateKwargs, tc.wantEffort, tc.wantKwargs)
			}
			if got.EffortSource != tc.wantEffortSrc || got.KwargsSource != tc.wantKwSrc {
				t.Errorf("sources = %s/%s, want %s/%s", got.EffortSource, got.KwargsSource, tc.wantEffortSrc, tc.wantKwSrc)
			}
		})
	}
}

// Invalid values fail closed and name the layer they came from. A setting
// that sets a field shadows an invalid env var for that field.
func TestResolveThinking_Invalid(t *testing.T) {
	cases := []struct {
		name                 string
		setting              JudgeSetting
		effortEnv, kwargsEnv string
		wantErr              string // "" = no error
	}{
		{name: "setting effort with whitespace", setting: JudgeSetting{ReasoningEffort: "lo w"}, wantErr: "propose model setting param reasoning_effort"},
		{name: "setting kwargs array", setting: JudgeSetting{ChatTemplateKwargs: "[1]"}, wantErr: "propose model setting param chat_template_kwargs"},
		{name: "setting kwargs null", setting: JudgeSetting{ChatTemplateKwargs: "null"}, wantErr: "chat_template_kwargs"},
		{name: "setting kwargs empty object", setting: JudgeSetting{ChatTemplateKwargs: "{}"}, wantErr: "non-empty JSON object"},
		{name: "env kwargs empty object", kwargsEnv: " {} ", wantErr: envChatTemplateKwargs},
		{name: "env kwargs not JSON", kwargsEnv: "{nope", wantErr: envChatTemplateKwargs},
		{name: "env effort with whitespace", effortEnv: "very low", wantErr: envReasoningEffort},
		{name: "setting shadows a bad env", setting: JudgeSetting{ChatTemplateKwargs: "omit"}, kwargsEnv: "{nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envReasoningEffort, tc.effortEnv)
			t.Setenv(envChatTemplateKwargs, tc.kwargsEnv)
			_, err := ResolveThinking("qwen3:8b", tc.setting)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// lunaSource is a registry source whose propose setting carries the bake-off
// params, as ConfigSource reports them.
type lunaSource struct {
	fakeSource
	setting JudgeSetting
	err     error
}

func (s lunaSource) JudgeSetting() (JudgeSetting, error) { return s.setting, s.err }

// TestChatClient_LunaSettingRequestBody: the bake-off switch — GPT-6 Luna via
// OpenRouter with reasoning_effort=low, chat_template_kwargs=omit,
// temperature=0 — reproduces the request the bake-off ran (pre-#189 code,
// where openrouter/ was not a hosted route): reasoning_effort "low",
// temperature 0, no chat_template_kwargs, even with a deployment env that
// says otherwise.
func TestChatClient_LunaSettingRequestBody(t *testing.T) {
	cs := newCaptureServer(t, "openai/gpt-6-luna")
	t.Setenv(envReasoningEffort, "none")
	t.Setenv(envChatTemplateKwargs, `{"enable_thinking":false}`)
	c, err := ResolveChatClient(lunaSource{
		fakeSource: fakeSource{ok: true, ep: EvalEndpoint{BaseURL: cs.srv.URL + "/v1", Model: lunaModel}},
		setting:    JudgeSetting{ReasoningEffort: "low", ChatTemplateKwargs: "omit", Temperature: ptr(0.0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	assertKey(t, cs.body, "reasoning_effort", "low")
	assertKey(t, cs.body, "chat_template_kwargs", nil)
	assertKey(t, cs.body, "temperature", 0)
	assertKey(t, cs.body, "max_tokens", defaultJudgeMaxTokens)
}

func ptr[T any](v T) *T { return &v }

// The setting temperature outranks the endpoint's temperature option and the
// route default; without it the hosted route sends none, and the endpoint
// option still applies. A negative setting temperature fails closed.
func TestChatClient_SettingTemperature(t *testing.T) {
	cases := []struct {
		name    string
		opts    map[string]string
		setting *float64
		want    any // nil = omitted
	}{
		{name: "hosted default omits", want: nil},
		{name: "setting 0 on a hosted route", setting: ptr(0.0), want: 0},
		{name: "setting beats endpoint option", opts: map[string]string{"temperature": "0.7"}, setting: ptr(0.2), want: 0.2},
		{name: "endpoint option without setting", opts: map[string]string{"temperature": "0.7"}, want: 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := newCaptureServer(t, "")
			c, err := ResolveChatClient(lunaSource{
				fakeSource: fakeSource{ok: true, ep: EvalEndpoint{BaseURL: cs.srv.URL + "/v1", Model: lunaModel, Options: tc.opts}},
				setting:    JudgeSetting{Temperature: tc.setting},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
				t.Fatal(err)
			}
			assertKey(t, cs.body, "temperature", tc.want)
		})
	}
	_, err := ResolveChatClient(lunaSource{
		fakeSource: fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "http://gw:4000/v1", Model: lunaModel}},
		setting:    JudgeSetting{Temperature: ptr(-1.0)},
	})
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Fatalf("negative setting temperature: want a config error, got %v", err)
	}
}

// With no setting and no env, a hosted OpenRouter/OpenAI route gets neither
// knob (it used to get the local-model defaults).
func TestChatClient_OpenRouterDefaultOmitsBoth(t *testing.T) {
	for _, model := range []string{lunaModel, "openai/gpt-6-luna", "openrouter/qwen/qwen3-235b"} {
		t.Run(model, func(t *testing.T) {
			cs := newCaptureServer(t, "")
			t.Setenv(envReasoningEffort, "")
			t.Setenv(envChatTemplateKwargs, "")
			c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{BaseURL: cs.srv.URL + "/v1", Model: model}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
				t.Fatal(err)
			}
			assertKey(t, cs.body, "reasoning_effort", nil)
			assertKey(t, cs.body, "chat_template_kwargs", nil)
		})
	}
}

// A setting layer that reports an invalid param fails the judge's
// construction (never a silent fallback to env or auto).
func TestResolveChatClient_SettingErrorFailsClosed(t *testing.T) {
	_, err := ResolveChatClient(lunaSource{
		fakeSource: fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "http://gw:4000/v1", Model: lunaModel}},
		setting:    JudgeSetting{ChatTemplateKwargs: "[1]"},
	})
	if err == nil || !strings.Contains(err.Error(), "chat_template_kwargs") {
		t.Fatalf("want a config error naming chat_template_kwargs, got %v", err)
	}
}

// judgeConfig is a registry with one chat endpoint bound to the eval role.
func judgeConfig(baseURL, model string) *config.Config {
	return &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "litellm-judge", Type: config.AIEndpointTypeChat, BaseURL: baseURL, Model: model},
		},
		AIRoles: &config.AIRoles{Eval: "litellm-judge"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{}},
	}
}

// TestModelSettingParamsReachTheJudge: end to end through config — a propose
// model setting (`earmark models set propose --model … --param …`) applied
// with WithStepSettings drives the request body and the recipe, and
// switching to another model without params drops them.
func TestModelSettingParamsReachTheJudge(t *testing.T) {
	t.Setenv(envReasoningEffort, "")
	t.Setenv(envChatTemplateKwargs, "")
	cs := newCaptureServer(t, "")
	base := judgeConfig(cs.srv.URL+"/v1", "anthropic/claude-haiku-5-5")

	luna, err := base.WithStepSettings(config.StepSetting{
		Step: recipe.StepPropose, EndpointID: "litellm-judge", Model: lunaModel,
		Params: map[string]any{config.ParamReasoningEffort: "low", config.ParamChatTemplateKwargs: "omit",
			config.ParamTemperature: 0.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ResolveChatClient(ConfigSource(luna))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	assertKey(t, cs.body, "model", lunaModel)
	assertKey(t, cs.body, "reasoning_effort", "low")
	assertKey(t, cs.body, "chat_template_kwargs", nil)
	assertKey(t, cs.body, "temperature", 0)

	r, ok, err := CurrentRecipe(luna)
	if err != nil || !ok {
		t.Fatalf("CurrentRecipe: ok=%v err=%v", ok, err)
	}
	if r.Params["reasoning_effort"] != "low" || r.Params["temperature"] != 0.0 {
		t.Errorf("recipe params = %v, want reasoning_effort low and temperature 0", r.Params)
	}
	if _, ok := r.Params["chat_template_kwargs"]; ok {
		t.Errorf("omit on a hosted route is the route default, so not a recipe param: %v", r.Params)
	}

	// Same model, other effort: another recipe.
	medium, err := base.WithStepSettings(config.StepSetting{
		Step: recipe.StepPropose, EndpointID: "litellm-judge", Model: lunaModel,
		Params: map[string]any{config.ParamReasoningEffort: "medium"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rm, _, err := CurrentRecipe(medium)
	if err != nil {
		t.Fatal(err)
	}
	if mustRecipeID(t, rm) == mustRecipeID(t, r) {
		t.Error("changing reasoning_effort must change the propose recipe id")
	}

	// Same model, no params: the route default, a third recipe.
	bare, err := base.WithStepSettings(config.StepSetting{Step: recipe.StepPropose, EndpointID: "litellm-judge", Model: lunaModel})
	if err != nil {
		t.Fatal(err)
	}
	th, err := JudgeThinkingFor(ConfigSource(bare), lunaModel)
	if err != nil {
		t.Fatal(err)
	}
	if th.ReasoningEffort != "" || th.EffortSource != ThinkingFromAuto {
		t.Errorf("a model setting without params keeps no reasoning_effort: %+v", th)
	}
	rb, _, err := CurrentRecipe(bare)
	if err != nil {
		t.Fatal(err)
	}
	if mustRecipeID(t, rb) == mustRecipeID(t, r) {
		t.Error("dropping reasoning_effort must change the propose recipe id")
	}
}

// TestJudgeRecipe_ThinkingParams: only a knob that differs from the route
// default is a recipe param, so every default judge keeps its recipe id.
func TestJudgeRecipe_ThinkingParams(t *testing.T) {
	recipeFor := func(model string, s JudgeSetting) map[string]any {
		t.Helper()
		th, err := ResolveThinking(model, s)
		if err != nil {
			t.Fatal(err)
		}
		return NewJudge(newOpenAIChatClient(chatConfig{BaseURL: "http://gw/v1", Model: model, Thinking: th})).Recipe().Params
	}
	t.Setenv(envReasoningEffort, "")
	t.Setenv(envChatTemplateKwargs, "")
	cases := []struct {
		name       string
		model      string
		setting    JudgeSetting
		wantEffort any // nil = not a param
		wantKwargs any
	}{
		{name: "local default", model: "qwen3:8b"},
		{name: "hosted default", model: lunaModel},
		{name: "hosted explicit omit equals default", model: lunaModel, setting: JudgeSetting{ReasoningEffort: "omit", ChatTemplateKwargs: "omit"}},
		{name: "local explicit none equals default", model: "qwen3:8b", setting: JudgeSetting{ReasoningEffort: "none"}},
		{name: "hosted low", model: lunaModel, setting: JudgeSetting{ReasoningEffort: "low"}, wantEffort: "low"},
		{name: "local omit both", model: "qwen3:8b", setting: JudgeSetting{ReasoningEffort: "omit", ChatTemplateKwargs: "omit"},
			wantEffort: "omit", wantKwargs: "omit"},
		{name: "hosted kwargs object", model: lunaModel, setting: JudgeSetting{ChatTemplateKwargs: `{"a":1}`},
			wantKwargs: map[string]any{"a": float64(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := recipeFor(tc.model, tc.setting)
			for key, want := range map[string]any{"reasoning_effort": tc.wantEffort, "chat_template_kwargs": tc.wantKwargs} {
				got, present := p[key]
				if want == nil {
					if present {
						t.Errorf("%s = %v, want no param", key, got)
					}
					continue
				}
				if !present {
					t.Errorf("%s missing, want %v", key, want)
					continue
				}
				if m, ok := want.(map[string]any); ok {
					gm, _ := got.(map[string]any)
					if !sameJSON(gm, m) {
						t.Errorf("%s = %v, want %v", key, got, want)
					}
				} else if got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
		})
	}
}

func TestThinkingDescribe(t *testing.T) {
	t.Setenv(envReasoningEffort, "")
	t.Setenv(envChatTemplateKwargs, "")
	th, err := ResolveThinking(lunaModel, JudgeSetting{ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := th.Describe(), `reasoning_effort "low" (model setting) · chat_template_kwargs omitted (auto: hosted route)`; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
	th, _ = ResolveThinking("qwen3:8b", JudgeSetting{})
	if got, want := th.Describe(), `reasoning_effort "none" (auto: local model) · chat_template_kwargs {"enable_thinking":false} (auto: local model)`; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}
