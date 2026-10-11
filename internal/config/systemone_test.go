package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// systemOneEnv sets a valid registry with a systemone endpoint bound to both
// decide and scan.
func systemOneEnv(t *testing.T) {
	t.Helper()
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("AI_ENDPOINTS", `[
		{"id":"embed-1","type":"embeddings","backend":"ollama","baseURL":"http://ollama:11434/v1","model":"nomic-embed-text"},
		{"id":"jev","type":"systemone","backend":"openai-compat","baseURL":"http://litellm:4000/typesafe","model":"jev-1.13.0"}
	]`)
	t.Setenv("AI_ROLES", `{"embeddings":"embed-1","decide":"jev","scan":"jev"}`)
}

func TestLoadConfig_SystemOneRoles(t *testing.T) {
	systemOneEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	d, ok := cfg.DecideEndpoint()
	require.True(t, ok)
	assert.Equal(t, AIEndpointTypeSystemOne, d.Type)
	assert.Equal(t, "jev-1.13.0", d.Model)
	s, ok := cfg.ScanEndpoint()
	require.True(t, ok)
	assert.Equal(t, "jev", s.ID)
	assert.Equal(t, "decide", cfg.RoleForEndpoint("jev"))
	_, ok = cfg.EvalEndpoint()
	assert.False(t, ok)
}

// Existing configs (no decide/scan) are unaffected.
func TestLoadConfig_SystemOneRolesOptional(t *testing.T) {
	registryEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	_, ok := cfg.DecideEndpoint()
	assert.False(t, ok)
	_, ok = cfg.ScanEndpoint()
	assert.False(t, ok)
}

// Fail closed: decide/scan must resolve to a systemone endpoint, and a
// systemone endpoint must name a pinned model and take no options.
func TestLoadConfig_SystemOneFailsClosed(t *testing.T) {
	embed := `{"id":"embed-1","type":"embeddings","backend":"ollama","baseURL":"http://ollama:11434/v1","model":"nomic-embed-text"}`
	chat := `{"id":"chat-1","type":"chat","backend":"vllm","baseURL":"http://h/v1","model":"m"}`
	jev := func(model, extra string) string {
		return `{"id":"jev","type":"systemone","backend":"openai-compat","baseURL":"http://litellm:4000/typesafe","model":"` + model + `"` + extra + `}`
	}
	cases := []struct{ name, endpoints, roles, wantErr string }{
		{"decide on a chat endpoint", "[" + embed + "," + chat + "]", `{"embeddings":"embed-1","decide":"chat-1"}`, `AI_ROLES.decide "chat-1" resolves to a "chat" endpoint, want "systemone"`},
		{"scan on an embeddings endpoint", "[" + embed + "]", `{"embeddings":"embed-1","scan":"embed-1"}`, `want "systemone"`},
		{"decide unknown id", "[" + embed + "]", `{"embeddings":"embed-1","decide":"nope"}`, `AI_ROLES.decide "nope" does not match`},
		{"eval on a systemone endpoint", "[" + embed + "," + jev("jev-1.13.0", "") + "]", `{"embeddings":"embed-1","eval":"jev"}`, `want "chat"`},
		{"latest alias", "[" + embed + "," + jev("jev-latest", "") + "]", `{"embeddings":"embed-1","decide":"jev"}`, "moving alias"},
		{"preview alias", "[" + embed + "," + jev("jev-preview", "") + "]", `{"embeddings":"embed-1"}`, "moving alias"},
		{"no version", "[" + embed + "," + jev("jev", "") + "]", `{"embeddings":"embed-1"}`, "names no version"},
		{"options", "[" + embed + "," + jev("jev-1.13.0", `,"options":{"temperature":"0"}`) + "]", `{"embeddings":"embed-1"}`, "takes no options"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearContractEnvVars(t)
			t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
			t.Setenv("AI_ENDPOINTS", tc.endpoints)
			t.Setenv("AI_ROLES", tc.roles)
			_, err := LoadConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestPinnedModel(t *testing.T) {
	tests := []struct {
		model string
		ok    bool
	}{
		{"jev-1.13.0", true},
		{"typesafe/jev-1.13.0", true},
		{"claude-haiku-4.5", true},
		// Ollama tags name one build: pinned.
		{"clef:27b", true},
		{"qwen3.8:27b-q8_0", true},
		{"ollama/clef:27b", true},
		{"clef:27b-q4_K_M", true},
		{"", false},
		{" ", false},
		{"jev", false},
		{"clef", false},
		{"clef:", false},
		{"jev-latest", false},
		{"JEV-Preview", false},
		{"gemma3:latest", false},
		{"jev-1.13.0-latest", false},
		{"clef:latest-27b", false},
		{"clef:27b-preview", false},
		{"x-preview-2.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			err := PinnedModel(tt.model)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// The TypeSafe pass-through always calls api.typesafe.ai: only jev-* may be
// sent there; any model on another systemone endpoint (the LiteLLM root's
// unified /v1/systemone) is fine.
func TestCheckSystemOneRoute(t *testing.T) {
	pass := AIEndpoint{ID: "litellm-jev", Type: AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe"}
	unified := AIEndpoint{ID: "litellm-s1", Type: AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000"}
	assert.NoError(t, CheckSystemOneRoute(pass, "jev-1.13.0"))
	assert.NoError(t, CheckSystemOneRoute(pass, "typesafe/JEV-1.14.0"))
	assert.Error(t, CheckSystemOneRoute(pass, "clef:27b"))
	pass.BaseURL += "/"
	assert.Error(t, CheckSystemOneRoute(pass, "clef:27b"), "a trailing slash is the same route")
	assert.NoError(t, CheckSystemOneRoute(unified, "clef:27b"))
	assert.NoError(t, CheckSystemOneRoute(unified, "jev-1.13.0"))
	assert.NoError(t, CheckSystemOneRoute(AIEndpoint{Type: AIEndpointTypeChat, BaseURL: "http://x/typesafe"}, "clef:27b"))
}

// MODELS_FILE: decide/scan aliases are checked against AI_ROLES.decide/scan,
// and usd_per_mtok_in must be a non-negative number.
func TestModelRegistry_DecideScan(t *testing.T) {
	systemOneEnv(t)
	t.Setenv("MODELS_FILE", writeModelsFile(t, `
steps:
  decide:
    alias: jev-1.13.0
    params:
      usd_per_mtok_in: 0.05
  scan:
    alias: jev-1.13.0
`))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	p, ok := cfg.ModelPin("decide").FloatParam(ParamUSDPerMTokIn)
	assert.True(t, ok)
	assert.Equal(t, 0.05, p)
	_, ok = cfg.ModelPin("scan").FloatParam(ParamUSDPerMTokIn)
	assert.False(t, ok)

	for name, tc := range map[string]struct{ body, wantErr string }{
		"decide alias contradicts": {"steps:\n  decide:\n    alias: jev-1.12.0\n", `steps.decide.alias is "jev-1.12.0" but the configured endpoint requests "jev-1.13.0"`},
		"scan alias contradicts":   {"steps:\n  scan:\n    alias: jev-2.0.0\n", `steps.scan.alias`},
		"negative price":           {"steps:\n  decide:\n    params:\n      usd_per_mtok_in: -1\n", "non-negative number"},
		"string price":             {"steps:\n  decide:\n    params:\n      usd_per_mtok_in: cheap\n", "non-negative number"},
		"unknown param":            {"steps:\n  decide:\n    params:\n      usd_per_mtok: 1\n", `unknown key "usd_per_mtok"`},
		"thinking param on scan":   {"steps:\n  scan:\n    params:\n      reasoning_effort: low\n", `unknown key "reasoning_effort"`},
		"propose kwargs a list":    {"steps:\n  propose:\n    params:\n      chat_template_kwargs: [1]\n", "chat_template_kwargs must be"},
	} {
		t.Run(name, func(t *testing.T) {
			systemOneEnv(t)
			t.Setenv("MODELS_FILE", writeModelsFile(t, tc.body))
			_, err := LoadConfig()
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tc.wantErr), "error %q should contain %q", err, tc.wantErr)
		})
	}

	// An integer price is a number too.
	systemOneEnv(t)
	t.Setenv("MODELS_FILE", writeModelsFile(t, "steps:\n  decide:\n    params:\n      usd_per_mtok_in: 1\n"))
	cfg, err = LoadConfig()
	require.NoError(t, err)
	p, _ = cfg.ModelPin("decide").FloatParam(ParamUSDPerMTokIn)
	assert.Equal(t, 1.0, p)
}
