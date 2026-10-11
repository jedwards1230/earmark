package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// settingsBase is a registry shaped like production: decide and scan share
// one systemone endpoint, plus a second systemone endpoint (Clef) and a second
// chat endpoint, with MODELS_FILE pins on propose and decide.
func settingsBase() *Config {
	return &Config{
		AIEndpoints: []AIEndpoint{
			{ID: "embed-1", Type: AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "embed-2", Type: AIEndpointTypeEmbeddings, BaseURL: "http://vllm:8000/v1", Model: "bge-m3"},
			{ID: "judge", Type: AIEndpointTypeChat, BaseURL: "http://litellm:4000/v1", Model: "anthropic/claude-haiku-5-5",
				APIKey: "k", Options: map[string]string{"max_tokens": "4096"}},
			{ID: "ollama-chat", Type: AIEndpointTypeChat, BaseURL: "http://ollama:11434/v1", Model: "qwen3.8"},
			{ID: "jev", Type: AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
			{ID: "clef", Type: AIEndpointTypeSystemOne, BaseURL: "http://ollama:11434", Model: "clef-1.0.0"},
		},
		AIRoles: &AIRoles{Embeddings: "embed-1", Eval: "judge", Decide: "jev", Scan: "jev"},
		Models: &ModelRegistry{Steps: map[string]ModelPin{
			recipe.StepPropose: {Alias: "anthropic/claude-haiku-5-5", ExpectedModel: "claude-haiku-5-5", Revision: "r1", PromptVersion: "judge@v2"},
			recipe.StepDecide:  {Params: map[string]any{ParamUSDPerMTokIn: 0.042}},
		}},
	}
}

func stepModel(t *testing.T, c *Config, step string) string {
	t.Helper()
	m, ok := c.StepModel(step)
	require.True(t, ok, "step %s has no model", step)
	return m
}

// Precedence: per-run override > db row > env default > unset.
func TestStepSettingsPrecedence(t *testing.T) {
	db := StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"}
	ov := StepSetting{Step: recipe.StepPropose, EndpointID: "ollama-chat", Model: "qwen3.8", Source: SourceOverride}
	tests := []struct {
		name               string
		settings           []StepSetting
		wantSource, wantMd string
		wantEndpoint       string
	}{
		{"env", nil, SourceEnv, "anthropic/claude-haiku-5-5", "judge"},
		{"db", []StepSetting{db}, SourceDB, "ollama/qwen3.8:27b-q8_0", "judge"},
		{"override beats db", []StepSetting{db, ov}, SourceOverride, "qwen3.8", "ollama-chat"},
		{"override alone", []StepSetting{ov}, SourceOverride, "qwen3.8", "ollama-chat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := settingsBase().WithStepSettings(tt.settings...)
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, c.StepSource(recipe.StepPropose))
			assert.Equal(t, tt.wantMd, stepModel(t, c, recipe.StepPropose))
			ep, ok := c.EvalEndpoint()
			require.True(t, ok)
			assert.Equal(t, tt.wantMd, ep.Model)
			base := ep.ID
			if ep.BaseID != "" {
				base = ep.BaseID
			}
			assert.Equal(t, tt.wantEndpoint, base)
		})
	}

	t.Run("unset", func(t *testing.T) {
		c := settingsBase()
		c.AIRoles.Scan = ""
		assert.Equal(t, SourceUnset, c.StepSource(recipe.StepScan))
		_, ok := c.StepModel(recipe.StepScan)
		assert.False(t, ok)
		// A db row turns an unbound step on.
		on, err := c.WithStepSettings(StepSetting{Step: recipe.StepScan, EndpointID: "clef", Model: "clef-1.0.0"})
		require.NoError(t, err)
		assert.Equal(t, SourceDB, on.StepSource(recipe.StepScan))
		ep, ok := on.ScanEndpoint()
		require.True(t, ok)
		assert.Equal(t, "clef", ep.ID)
	})
}

// Fail closed: every invalid setting is refused with an error naming the step.
func TestStepSettingsValidation(t *testing.T) {
	tests := []struct {
		name    string
		s       StepSetting
		wantErr string
	}{
		{"unknown step", StepSetting{Step: "asr", EndpointID: "judge", Model: "x"}, `unknown step "asr"`},
		{"unknown endpoint", StepSetting{Step: recipe.StepDecide, EndpointID: "gone", Model: "clef-1.0.0"}, `endpoint "gone" is not in AI_ENDPOINTS`},
		{"empty endpoint", StepSetting{Step: recipe.StepDecide, Model: "clef-1.0.0"}, "endpoint is required"},
		{"wrong type", StepSetting{Step: recipe.StepScan, EndpointID: "judge", Model: "x"}, `needs a "systemone" endpoint`},
		{"embed on chat", StepSetting{Step: recipe.StepEmbed, EndpointID: "judge", Model: "x"}, `needs a "embeddings" endpoint`},
		{"empty model", StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "  "}, "model is required"},
		{"model with spaces", StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "a b"}, "contains whitespace"},
		{"moving alias", StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-latest"}, "moving alias"},
		{"unversioned", StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef"}, "names no version"},
		{"unknown param", StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0",
			Params: map[string]any{"usd": 1.0}}, `unknown key "usd"`},
		{"negative param", StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0",
			Params: map[string]any{ParamUSDPerMTokIn: -1.0}}, "non-negative number"},
		{"string param", StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0",
			Params: map[string]any{ParamUSDPerMTokIn: "cheap"}}, "non-negative number"},
		{"synthesized endpoint", StepSetting{Step: recipe.StepScan, EndpointID: "jev@decide", Model: "jev-1.13.0"}, `not in AI_ENDPOINTS`},
		{"thinking param on decide", StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0",
			Params: map[string]any{ParamReasoningEffort: "low"}}, `unknown key "reasoning_effort" (known for decide: usd_per_mtok_in)`},
		{"numeric reasoning_effort", StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "x",
			Params: map[string]any{ParamReasoningEffort: 1.0}}, "reasoning_effort must be a string"},
		{"blank reasoning_effort", StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "x",
			Params: map[string]any{ParamReasoningEffort: " "}}, "reasoning_effort must be a non-empty token"},
		{"kwargs not an object", StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "x",
			Params: map[string]any{ParamChatTemplateKwargs: "[1]"}}, `chat_template_kwargs must be "auto", "omit" or a JSON object`},
		{"kwargs a number", StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "x",
			Params: map[string]any{ParamChatTemplateKwargs: 3.0}}, "chat_template_kwargs must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := settingsBase()
			if tt.name == "synthesized endpoint" {
				var err error
				base, err = base.WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.14.0"})
				require.NoError(t, err)
			}
			_, err := base.WithStepSettings(tt.s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			var se *StepSettingError
			require.True(t, errors.As(err, &se))
			assert.Equal(t, tt.s.Step, se.Step)
			assert.Contains(t, err.Error(), "earmark models unset "+tt.s.Step, "a db row's error names the fix")
		})
	}

	t.Run("override error names the flags", func(t *testing.T) {
		_, err := settingsBase().WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "gone", Model: "x-1.0", Source: SourceOverride})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--endpoint/--model for step decide")
	})

	t.Run("lenient apply reports invalid rows and applies the rest", func(t *testing.T) {
		c, errs := settingsBase().WithValidStepSettings(
			StepSetting{Step: recipe.StepDecide, EndpointID: "gone", Model: "clef-1.0.0"},
			StepSetting{Step: recipe.StepScan, EndpointID: "clef", Model: "clef-1.0.0"},
		)
		require.Len(t, errs, 1)
		require.Error(t, errs[recipe.StepDecide])
		assert.Equal(t, "jev-1.13.0", stepModel(t, c, recipe.StepDecide), "an invalid row leaves the binding alone")
		assert.Equal(t, SourceEnv, c.StepSource(recipe.StepDecide))
		assert.Equal(t, "clef-1.0.0", stepModel(t, c, recipe.StepScan))
	})
}

// Two roles sharing one endpoint: a model change for one must not leak into
// the other, and the base config is never mutated.
func TestStepSettingsSharedEndpointIsolation(t *testing.T) {
	base := settingsBase()
	c, err := base.WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.14.0"})
	require.NoError(t, err)

	dec, ok := c.DecideEndpoint()
	require.True(t, ok)
	assert.Equal(t, "jev-1.14.0", dec.Model)
	assert.Equal(t, "jev@decide", dec.ID)
	assert.Equal(t, "jev", dec.BaseID)
	assert.Equal(t, "http://litellm:4000/typesafe", dec.BaseURL, "the synthesized endpoint keeps URL and key")

	scan, ok := c.ScanEndpoint()
	require.True(t, ok)
	assert.Equal(t, "jev", scan.ID)
	assert.Equal(t, "jev-1.13.0", scan.Model, "scan shares the endpoint but keeps its model")
	assert.Equal(t, "decide", c.RoleForEndpoint("jev@decide"))

	// The base is untouched: same roles, endpoints and pins.
	assert.Equal(t, settingsBase(), base)

	// Re-applying replaces the step's synthesized endpoint, never stacks them.
	c2, err := c.WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.15.0", Source: SourceOverride})
	require.NoError(t, err)
	n := 0
	for _, ep := range c2.AIEndpoints {
		if ep.BaseID != "" {
			n++
		}
	}
	assert.Equal(t, 1, n)
	assert.Equal(t, "jev-1.15.0", stepModel(t, c2, recipe.StepDecide))

	// Back to the endpoint's own model: bound to the registered entry again,
	// the synthesized one dropped.
	c3, err := c2.WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.13.0", Params: map[string]any{ParamUSDPerMTokIn: 0.01}})
	require.NoError(t, err)
	dec3, _ := c3.DecideEndpoint()
	assert.Equal(t, "jev", dec3.ID)
	assert.Len(t, c3.AIEndpoints, len(base.AIEndpoints))

	t.Run("derived id collision", func(t *testing.T) {
		b := settingsBase()
		b.AIEndpoints = append(b.AIEndpoints, AIEndpoint{ID: "jev@decide", Type: AIEndpointTypeSystemOne, BaseURL: "http://x:1", Model: "jev-9.9.9"})
		c, err := b.WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.14.0"})
		require.NoError(t, err)
		ep, _ := c.DecideEndpoint()
		assert.Equal(t, "jev@decide-2", ep.ID)
	})
}

// The env pin describes the env model only: a setting that changes the model
// (or pins anything) replaces it wholesale; one that matches env keeps it.
func TestStepSettingsPinReplacement(t *testing.T) {
	envPin := settingsBase().ModelPin(recipe.StepPropose)
	tests := []struct {
		name string
		s    StepSetting
		want ModelPin
	}{
		{"same endpoint and model, no pins: env pin kept",
			StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "anthropic/claude-haiku-5-5"}, envPin},
		{"other model: unpinned, prompt_version kept",
			StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"},
			ModelPin{PromptVersion: "judge@v2"}},
		{"other endpoint, same model id: unpinned",
			StepSetting{Step: recipe.StepPropose, EndpointID: "ollama-chat", Model: "anthropic/claude-haiku-5-5"},
			ModelPin{PromptVersion: "judge@v2"}},
		{"setting pins replace wholesale",
			StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0",
				ExpectedModel: "qwen3.8:27b-q8_0", Revision: "sha256:abc"},
			ModelPin{ExpectedModel: "qwen3.8:27b-q8_0", Revision: "sha256:abc", PromptVersion: "judge@v2"}},
		{"pins on the env model replace the env pin",
			StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "anthropic/claude-haiku-5-5", Revision: "r2"},
			ModelPin{Revision: "r2", PromptVersion: "judge@v2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := settingsBase().WithStepSettings(tt.s)
			require.NoError(t, err)
			assert.Equal(t, tt.want, c.ModelPin(recipe.StepPropose))
			assert.Equal(t, SourceDB, c.StepSource(recipe.StepPropose))
		})
	}

	t.Run("decide params pin replaced by a model change", func(t *testing.T) {
		c, err := settingsBase().WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0"})
		require.NoError(t, err)
		_, set := c.ModelPin(recipe.StepDecide).FloatParam(ParamUSDPerMTokIn)
		assert.False(t, set, "Jev's price must not be charged to Clef")
	})

	t.Run("an override equal to the db row keeps the db row's pin", func(t *testing.T) {
		c, err := settingsBase().WithStepSettings(
			StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0", ExpectedModel: "qwen"},
			StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0", Source: SourceOverride},
		)
		require.NoError(t, err)
		assert.Equal(t, "qwen", c.ModelPin(recipe.StepPropose).ExpectedModel)
		assert.Equal(t, SourceOverride, c.StepSource(recipe.StepPropose))
	})
}

func TestOverrideSetting(t *testing.T) {
	db, err := settingsBase().WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.14.0"})
	require.NoError(t, err)
	tests := []struct {
		name, step, endpoint, model string
		wantOK                      bool
		want                        StepSetting
		wantErr                     string
	}{
		{name: "no flags", step: recipe.StepDecide},
		{name: "model only keeps the effective endpoint", step: recipe.StepDecide, model: "jev-1.15.0", wantOK: true,
			want: StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.15.0", Source: SourceOverride}},
		{name: "endpoint only takes its own model", step: recipe.StepDecide, endpoint: "clef", wantOK: true,
			want: StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0", Source: SourceOverride}},
		{name: "same endpoint keeps the effective model", step: recipe.StepDecide, endpoint: "jev", wantOK: true,
			want: StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.14.0", Source: SourceOverride}},
		{name: "unknown step", step: "asr", model: "x", wantOK: true, wantErr: "unknown step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := db.OverrideSetting(tt.step, tt.endpoint, tt.model)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("model on an unbound step needs an endpoint", func(t *testing.T) {
		c := settingsBase()
		c.AIRoles.Scan = ""
		_, ok, err := c.OverrideSetting(recipe.StepScan, "", "clef-1.0.0")
		assert.True(t, ok)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--endpoint")
	})
}

func TestDescribeStep(t *testing.T) {
	c, err := settingsBase().WithStepSettings(StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.14.0"})
	require.NoError(t, err)
	assert.Equal(t, "decide: jev-1.14.0 on jev (source db)", c.DescribeStep(recipe.StepDecide))
	assert.Equal(t, "scan: jev-1.13.0 on jev (source env)", c.DescribeStep(recipe.StepScan))
	c.AIRoles.Scan = ""
	assert.Equal(t, "scan: off (unset)", c.DescribeStep(recipe.StepScan))
}

// Review regression: an override back to the env model after a db row must
// restore the env MODELS_FILE pin (so the run stamps the env recipe and keeps
// decide's price), not leave the step unpinned.
func TestStepSettingsBackToEnvRestoresEnvPin(t *testing.T) {
	base := settingsBase()
	envPropose, envDecide := base.ModelPin(recipe.StepPropose), base.ModelPin(recipe.StepDecide)

	c, err := base.WithStepSettings(
		StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"},
		StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "anthropic/claude-haiku-5-5", Source: SourceOverride},
		StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0"},
		StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.13.0", Source: SourceOverride},
	)
	require.NoError(t, err)
	assert.Equal(t, envPropose, c.ModelPin(recipe.StepPropose))
	assert.Equal(t, envDecide, c.ModelPin(recipe.StepDecide))
	p, ok := c.ModelPin(recipe.StepDecide).FloatParam(ParamUSDPerMTokIn)
	assert.True(t, ok)
	assert.Equal(t, 0.042, p)
	ep, _ := c.EvalEndpoint()
	assert.Equal(t, "judge", ep.ID, "bound to the registered endpoint again, no synthesized copy")
	assert.Len(t, c.AIEndpoints, len(base.AIEndpoints))
	assert.Equal(t, SourceOverride, c.StepSource(recipe.StepPropose))

	// The override's own pins still win over env.
	c, err = base.WithStepSettings(
		StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"},
		StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "anthropic/claude-haiku-5-5", Revision: "r9", Source: SourceOverride},
	)
	require.NoError(t, err)
	assert.Equal(t, ModelPin{Revision: "r9", PromptVersion: "judge@v2"}, c.ModelPin(recipe.StepPropose))
}

// Documented (CONTRACT §2.20): an override that changes only the endpoint,
// keeping a pinned db row's model id, is a new binding — the row's pin does
// not carry over (it pinned what the other endpoint serves).
func TestStepSettingsEndpointOnlyOverrideUnpins(t *testing.T) {
	c, err := settingsBase().WithStepSettings(
		StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "qwen3.8", ExpectedModel: "qwen3.8:27b"},
	)
	require.NoError(t, err)
	ov, ok, err := c.OverrideSetting(recipe.StepPropose, "ollama-chat", "qwen3.8")
	require.NoError(t, err)
	require.True(t, ok)
	c, err = c.WithStepSettings(ov)
	require.NoError(t, err)
	assert.Equal(t, ModelPin{PromptVersion: "judge@v2"}, c.ModelPin(recipe.StepPropose))
}

func TestStepSettingsPinWhitespaceAndRoute(t *testing.T) {
	base := settingsBase()
	base.AIEndpoints = append(base.AIEndpoints,
		AIEndpoint{ID: "litellm-jev", Type: AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
		AIEndpoint{ID: "litellm-s1", Type: AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000", Model: "jev-1.13.0"})
	for _, s := range []StepSetting{
		{Step: recipe.StepPropose, EndpointID: "judge", Model: "m", ExpectedModel: "a b"},
		{Step: recipe.StepPropose, EndpointID: "judge", Model: "m", Revision: "r\t1"},
		{Step: recipe.StepDecide, EndpointID: "litellm-jev", Model: "clef:27b"},
	} {
		_, err := base.WithStepSettings(s)
		assert.Error(t, err, "%+v", s)
	}
	c, err := base.WithStepSettings(
		StepSetting{Step: recipe.StepDecide, EndpointID: "litellm-s1", Model: "clef:27b"},
		StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "m", ExpectedModel: " e "},
	)
	require.NoError(t, err)
	ep, _ := c.DecideEndpoint()
	assert.Equal(t, "clef:27b", ep.Model)
	assert.Equal(t, "http://litellm:4000", ep.BaseURL, "the unified route: the client posts {base}/v1/systemone")
	assert.Equal(t, "e", c.ModelPin(recipe.StepPropose).ExpectedModel)
}

// The propose step accepts the judge thinking params, as strings or (for the
// kwargs) an object, and they become the step's pin params.
func TestStepSettingsJudgeThinkingParams(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"flag strings":  {ParamReasoningEffort: "low", ParamChatTemplateKwargs: "omit"},
		"kwargs json":   {ParamChatTemplateKwargs: `{"enable_thinking":false}`},
		"kwargs object": {ParamChatTemplateKwargs: map[string]any{"enable_thinking": false}},
		"auto":          {ParamReasoningEffort: "auto", ParamChatTemplateKwargs: "AUTO"},
		"with price":    {ParamReasoningEffort: "high", ParamUSDPerMTokIn: 0.1},
	} {
		t.Run(name, func(t *testing.T) {
			eff, err := settingsBase().WithStepSettings(StepSetting{Step: recipe.StepPropose, EndpointID: "judge",
				Model: "openrouter/openai/gpt-6-luna", Params: params})
			require.NoError(t, err)
			assert.Equal(t, params, eff.ModelPin(recipe.StepPropose).Params)
		})
	}
}

func TestChatTemplateKwargsParam(t *testing.T) {
	for in, want := range map[any]string{
		"omit": "omit", " OMIT ": "omit", "auto": "auto", `{"a":1}`: `{"a":1}`,
	} {
		got, err := ChatTemplateKwargsParam(in)
		require.NoError(t, err, "%v", in)
		assert.Equal(t, want, got)
	}
	got, err := ChatTemplateKwargsParam(map[string]any{"b": true, "a": 1})
	require.NoError(t, err)
	assert.Equal(t, `{"a":1,"b":true}`, got)
	for _, bad := range []any{"", "null", "[]", "{", 1.0, true, []any{"x"}} {
		_, err := ChatTemplateKwargsParam(bad)
		assert.Error(t, err, "%v", bad)
	}
}
