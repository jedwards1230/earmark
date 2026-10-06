package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeModelsFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// registryEnv sets a valid AI registry whose eval endpoint requests
// "earmark-judge" and whose embeddings endpoint requests "nomic-embed-text".
func registryEnv(t *testing.T) {
	t.Helper()
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("AI_ENDPOINTS", `[
		{"id":"embed-1","type":"embeddings","backend":"ollama","baseURL":"http://ollama:11434/v1","model":"nomic-embed-text"},
		{"id":"eval-1","type":"chat","backend":"openai-compat","baseURL":"http://litellm:4000/v1","model":"earmark-judge"}
	]`)
	t.Setenv("AI_ROLES", `{"embeddings":"embed-1","eval":"eval-1"}`)
}

func TestModelRegistry_UnsetIsEmpty(t *testing.T) {
	registryEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg.Models)
	assert.Equal(t, ModelPin{}, cfg.ModelPin("propose"))
	var nilCfg *Config
	assert.Equal(t, ModelPin{}, nilCfg.ModelPin("propose"), "nil config is unpinned, not a panic")
}

func TestModelRegistry_Loads(t *testing.T) {
	registryEnv(t)
	t.Setenv("MODELS_FILE", writeModelsFile(t, `
steps:
  propose:
    alias: earmark-judge
    expected_model: anthropic/claude-haiku-4-5-20251001
    revision: "20251001"
    prompt_version: judge@v1
  embed:
    expected_model: nomic-embed-text
    revision: sha256:0a109f422b47
  asr:
    expected_model: nvidia/parakeet-tdt-1.1b
`))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, ModelPin{
		Alias: "earmark-judge", ExpectedModel: "anthropic/claude-haiku-4-5-20251001",
		Revision: "20251001", PromptVersion: "judge@v1",
	}, cfg.ModelPin("propose"))
	assert.Equal(t, "sha256:0a109f422b47", cfg.ModelPin("embed").Revision)
	assert.Equal(t, "nvidia/parakeet-tdt-1.1b", cfg.ModelPin("asr").ExpectedModel)
	assert.Equal(t, ModelPin{}, cfg.ModelPin("scan"))
}

// Fail-closed, like AI_ENDPOINTS: a typo must not silently unpin a model, and
// the registry must not contradict the endpoint registry it annotates.
func TestModelRegistry_FailsClosed(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{"unknown step", "steps:\n  judge:\n    expected_model: x\n", "unknown step(s) judge"},
		{"unknown field", "steps:\n  propose:\n    expected: x\n", "field expected not found"},
		{"malformed yaml", "steps: [\n", "decode model registry"},
		{"alias contradicts eval endpoint", "steps:\n  propose:\n    alias: other-judge\n", `requests "earmark-judge"`},
		{"alias contradicts embeddings endpoint", "steps:\n  embed:\n    alias: bge-m3\n", `requests "nomic-embed-text"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registryEnv(t)
			t.Setenv("MODELS_FILE", writeModelsFile(t, tt.body))
			_, err := LoadConfig()
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tt.wantErr), "error %q should contain %q", err, tt.wantErr)
		})
	}

	t.Run("missing file", func(t *testing.T) {
		registryEnv(t)
		t.Setenv("MODELS_FILE", filepath.Join(t.TempDir(), "absent.yaml"))
		_, err := LoadConfig()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MODELS_FILE")
	})
}

// The propose alias is checked against the EVAL_CHAT_MODEL fallback when no
// eval role is bound (CONTRACT §2.15).
func TestModelRegistry_AliasAgainstEvalChatFallback(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("EVAL_CHAT_MODEL", "gemma3:12b")
	t.Setenv("MODELS_FILE", writeModelsFile(t, "steps:\n  propose:\n    alias: qwen3.8\n"))
	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requests "gemma3:12b"`)

	t.Setenv("MODELS_FILE", writeModelsFile(t, "steps:\n  propose:\n    alias: gemma3:12b\n"))
	_, err = LoadConfig()
	require.NoError(t, err)
}

func TestParseModelRegistry_Empty(t *testing.T) {
	reg, err := ParseModelRegistry(strings.NewReader(""))
	require.NoError(t, err)
	assert.Empty(t, reg.Steps)
}
