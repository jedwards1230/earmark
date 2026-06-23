package config

import (
	"os"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/asr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearContractEnvVars unsets all CONTRACT-defined env vars so tests start clean.
func clearContractEnvVars(t *testing.T) {
	t.Helper()
	vars := []string{
		"DATABASE_URL", "BOOKS_DIR",
		"EMBEDDINGS_BASE_URL", "EMBEDDINGS_MODEL",
		"MCP_HTTP_ADDR", "STALE_JOB_TIMEOUT",
		"CHUNK_SIZE", "DEBUG", "DEBUG_DB_RESET",
		"ASR_SERVERS", "AI_ENDPOINTS", "AI_ROLES",
	}
	for _, k := range vars {
		t.Setenv(k, "") // t.Setenv restores on cleanup
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@host:5432/db")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, "postgres://user:pass@host:5432/db", cfg.DatabaseURL)
	assert.Equal(t, "/books", cfg.BooksDir)
	assert.Equal(t, "http://ollama:11434/v1", cfg.EmbeddingsBaseURL)
	assert.Equal(t, "nomic-embed-text", cfg.EmbeddingsModel)
	assert.Equal(t, ":8081", cfg.MCPHTTPAddr)
	assert.Equal(t, 30*time.Minute, cfg.StaleJobTimeout)
	assert.Equal(t, 512, cfg.ChunkSize)
	assert.False(t, cfg.Debug)
	assert.False(t, cfg.DebugDBReset)
}

func TestLoadConfig_MissingDatabaseURL(t *testing.T) {
	clearContractEnvVars(t)
	// DATABASE_URL not set
	_, err := LoadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@host:5432/earmark")
	t.Setenv("BOOKS_DIR", "/mnt/books")
	t.Setenv("EMBEDDINGS_BASE_URL", "http://custom-ollama:11434/v1")
	t.Setenv("EMBEDDINGS_MODEL", "mxbai-embed-large")
	t.Setenv("MCP_HTTP_ADDR", ":9000")
	t.Setenv("STALE_JOB_TIMEOUT", "1h")
	t.Setenv("CHUNK_SIZE", "256")
	t.Setenv("DEBUG", "true")
	t.Setenv("DEBUG_DB_RESET", "1")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, "/mnt/books", cfg.BooksDir)
	assert.Equal(t, "http://custom-ollama:11434/v1", cfg.EmbeddingsBaseURL)
	assert.Equal(t, "mxbai-embed-large", cfg.EmbeddingsModel)
	assert.Equal(t, ":9000", cfg.MCPHTTPAddr)
	assert.Equal(t, time.Hour, cfg.StaleJobTimeout)
	assert.Equal(t, 256, cfg.ChunkSize)
	assert.True(t, cfg.Debug)
	assert.True(t, cfg.DebugDBReset)
}

func TestLoadConfig_InvalidStaleJobTimeout(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("STALE_JOB_TIMEOUT", "not-a-duration")

	_, err := LoadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "STALE_JOB_TIMEOUT")
}

func TestLoadConfig_InvalidChunkSize(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("CHUNK_SIZE", "abc")

	_, err := LoadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CHUNK_SIZE")
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"abc", "***"},
		{"12345678", "********"},
		{"verylongsecretkey123", "********"},
	}
	for _, tt := range tests {
		got := MaskSecret(tt.input)
		assert.Equal(t, tt.expected, got, "input=%q", tt.input)
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	const key = "TEST_GET_ENV_OR_DEFAULT"
	_ = os.Unsetenv(key)
	assert.Equal(t, "default", getEnvOrDefault(key, "default"))

	t.Setenv(key, "custom")
	assert.Equal(t, "custom", getEnvOrDefault(key, "default"))
}

func TestParseBoolEnv(t *testing.T) {
	const key = "TEST_PARSE_BOOL_ENV"

	for _, v := range []string{"true", "1"} {
		t.Setenv(key, v)
		assert.True(t, parseBoolEnv(key), "expected true for %q", v)
	}
	for _, v := range []string{"false", "0", "invalid", ""} {
		t.Setenv(key, v)
		assert.False(t, parseBoolEnv(key), "expected false for %q", v)
	}
}

func TestConfigPrintEnvVars(t *testing.T) {
	cfg := &Config{
		DatabaseURL:       "postgres://user:pass@host:5432/db",
		BooksDir:          "/books",
		EmbeddingsBaseURL: "http://ollama:11434/v1",
		EmbeddingsModel:   "nomic-embed-text",
		MCPHTTPAddr:       ":8081",
		StaleJobTimeout:   30 * time.Minute,
		ChunkSize:         512,
		Debug:             true,
		DebugDBReset:      false,
	}
	// Should not panic.
	assert.NotPanics(t, func() { cfg.PrintEnvVars() })
}

func TestLoadConfig_ASRServers(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("ASR_SERVERS", `[
		{"name":"gpu-1","host":"gpu-1","model":"nvidia/parakeet-tdt-0.6b-v3","role":"primary","gpuArbiterUrl":"http://gpu-1:48750/status"},
		{"name":"gpu-2","role":"fallback","match":"gpu-2"}
	]`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.ASRServers, 2)
	assert.Equal(t, "gpu-1", cfg.ASRServers[0].Name)
	assert.Equal(t, "primary", cfg.ASRServers[0].Role)
	assert.Equal(t, "http://gpu-1:48750/status", cfg.ASRServers[0].GPUArbiterURL)
	// Match defaults to the (lower-cased) name when omitted.
	assert.Equal(t, "gpu-1", cfg.ASRServers[0].MatchToken())
	assert.Equal(t, "gpu-2", cfg.ASRServers[1].MatchToken())
}

func TestLoadConfig_ASRServersInvalidJSONIsIgnored(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("ASR_SERVERS", "{not json")

	// A bad list is cosmetic — it must not block startup.
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.ASRServers)
}

func TestLoadConfig_ASRServersBackendDescriptor(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("ASR_SERVERS", `[
		{"name":"gpu-1","host":"gpu-1","model":"nvidia/parakeet-tdt-0.6b-v3",
		 "role":"primary","family":"nemo-parakeet","runtime":"nemo-cuda",
		 "capabilities":{"word_timestamps":true,"context_biasing":false,"diarization":false},
		 "languages":["en","es","fr"]},
		{"name":"mac-1","family":"whisper","runtime":"parakeet-mlx"}
	]`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.ASRServers, 2)

	s0 := cfg.ASRServers[0]
	assert.Equal(t, "nemo-parakeet", s0.Family)
	assert.Equal(t, "nemo-cuda", s0.Runtime)
	assert.Equal(t, asr.Capabilities{
		asr.CapWordTimestamps: true,
		asr.CapContextBiasing: false,
		asr.CapDiarization:    false,
	}, s0.Capabilities)
	assert.Equal(t, []string{"en", "es", "fr"}, s0.Languages)

	// A server with no capabilities/languages declared → nil (unknown), not an error.
	s1 := cfg.ASRServers[1]
	assert.Equal(t, "whisper", s1.Family)
	assert.Nil(t, s1.Capabilities)
	assert.Nil(t, s1.Languages)
}

func TestLoadConfig_ASRServersDropsUnknownCapabilityKeys(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	// "speaker_count" and "magic" are not in the closed enum — they must be
	// dropped (warn+drop), leaving only the recognized keys; the server itself
	// still loads (best-effort, never blocks startup).
	t.Setenv("ASR_SERVERS", `[
		{"name":"gpu-1","capabilities":{"word_timestamps":true,"speaker_count":true,"magic":false}}
	]`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.ASRServers, 1)
	assert.Equal(t, asr.Capabilities{asr.CapWordTimestamps: true}, cfg.ASRServers[0].Capabilities)
}

func TestLoadConfig_ASRServersBackCompatNoNewFields(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	// An old ASR_SERVERS value with none of the new fields must keep working
	// byte-for-byte: new fields default to empty/nil.
	t.Setenv("ASR_SERVERS", `[{"name":"gpu-1","host":"gpu-1","model":"m","role":"primary"}]`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.ASRServers, 1)
	s := cfg.ASRServers[0]
	assert.Equal(t, "gpu-1", s.Name)
	assert.Empty(t, s.Family)
	assert.Empty(t, s.Runtime)
	assert.Nil(t, s.Capabilities)
	assert.Nil(t, s.Languages)
}

// ─── AI endpoint registry (CONTRACT §2.14) ──────────────────────────────────────

func TestLoadConfig_LegacyEmbeddingsSynth(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	// No AI_ENDPOINTS: the deprecated EMBEDDINGS_* vars synthesize a _legacy endpoint.
	t.Setenv("EMBEDDINGS_BASE_URL", "http://custom-ollama:11434/v1")
	t.Setenv("EMBEDDINGS_MODEL", "mxbai-embed-large")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.AIEndpoints, 1)
	ep := cfg.AIEndpoints[0]
	assert.Equal(t, legacyEmbeddingsID, ep.ID)
	assert.Equal(t, AIEndpointTypeEmbeddings, ep.Type)
	assert.Equal(t, AIBackendOllama, ep.Backend)
	assert.Equal(t, "http://custom-ollama:11434/v1", ep.BaseURL)
	assert.Equal(t, "mxbai-embed-large", ep.Model)

	require.NotNil(t, cfg.AIRoles)
	assert.Equal(t, legacyEmbeddingsID, cfg.AIRoles.Embeddings)

	got, ok := cfg.EmbeddingsEndpoint()
	require.True(t, ok)
	assert.Equal(t, legacyEmbeddingsID, got.ID)
	assert.Equal(t, "mxbai-embed-large", got.Model)
}

func TestLoadConfig_LegacyDefaults(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	// Neither AI_ENDPOINTS nor EMBEDDINGS_* set: defaults flow into the _legacy endpoint.
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.AIEndpoints, 1)
	assert.Equal(t, "http://ollama:11434/v1", cfg.AIEndpoints[0].BaseURL)
	assert.Equal(t, "nomic-embed-text", cfg.AIEndpoints[0].Model)
}

func TestLoadConfig_AIEndpointsValid(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("AI_ENDPOINTS", `[
		{"id":"embed-1","type":"embeddings","backend":"ollama","baseURL":"http://ollama:11434/v1","model":"nomic-embed-text"},
		{"id":"eval-1","type":"chat","backend":"vllm","baseURL":"http://gpu-host:8000/v1","model":"Qwen2.5-7B-Instruct","options":{"temperature":"0","max_tokens":"256"}}
	]`)
	t.Setenv("AI_ROLES", `{"embeddings":"embed-1","eval":"eval-1"}`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Len(t, cfg.AIEndpoints, 2)
	assert.Equal(t, map[string]string{"temperature": "0", "max_tokens": "256"}, cfg.AIEndpoints[1].Options)

	emb, ok := cfg.EmbeddingsEndpoint()
	require.True(t, ok)
	assert.Equal(t, "embed-1", emb.ID)

	assert.Equal(t, "embeddings", cfg.RoleForEndpoint("embed-1"))
	assert.Equal(t, "eval", cfg.RoleForEndpoint("eval-1"))
	assert.Equal(t, "", cfg.RoleForEndpoint("unbound"))
}

func TestLoadConfig_AIEndpoints_AIRolesRequired(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("AI_ENDPOINTS", `[{"id":"embed-1","type":"embeddings","backend":"ollama","baseURL":"http://ollama:11434/v1","model":"m"}]`)
	// AI_ROLES intentionally unset.
	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AI_ROLES is required")
}

func TestLoadConfig_AIEndpoints_MalformedJSONFailsClosed(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("AI_ENDPOINTS", `[{"id":"embed-1",`) // truncated JSON
	t.Setenv("AI_ROLES", `{"embeddings":"embed-1"}`)
	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AI_ENDPOINTS is not valid JSON")
}

func TestLoadConfig_AIEndpoints_ValidationErrors(t *testing.T) {
	cases := []struct {
		name      string
		endpoints string
		roles     string
		wantErr   string
	}{
		{
			name:      "duplicate id",
			endpoints: `[{"id":"x","type":"embeddings","backend":"ollama","baseURL":"http://h/v1","model":"m"},{"id":"x","type":"chat","backend":"vllm","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"x"}`,
			wantErr:   "duplicate id",
		},
		{
			name:      "invalid type",
			endpoints: `[{"id":"x","type":"speech","backend":"ollama","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"x"}`,
			wantErr:   "invalid type",
		},
		{
			name:      "invalid backend",
			endpoints: `[{"id":"x","type":"embeddings","backend":"tensorrt","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"x"}`,
			wantErr:   "invalid backend",
		},
		{
			name:      "bad url scheme",
			endpoints: `[{"id":"x","type":"embeddings","backend":"ollama","baseURL":"ftp://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"x"}`,
			wantErr:   "must be http",
		},
		{
			name:      "missing model",
			endpoints: `[{"id":"x","type":"embeddings","backend":"ollama","baseURL":"http://h/v1"}]`,
			roles:     `{"embeddings":"x"}`,
			wantErr:   "model is required",
		},
		{
			name:      "embeddings role points at nonexistent id",
			endpoints: `[{"id":"x","type":"embeddings","backend":"ollama","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"nope"}`,
			wantErr:   "does not match any AI_ENDPOINTS id",
		},
		{
			name:      "embeddings role points at a chat endpoint",
			endpoints: `[{"id":"c","type":"chat","backend":"vllm","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"c"}`,
			wantErr:   `want "embeddings"`,
		},
		{
			name:      "eval role points at an embeddings endpoint",
			endpoints: `[{"id":"e","type":"embeddings","backend":"ollama","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{"embeddings":"e","eval":"e"}`,
			wantErr:   `want "chat"`,
		},
		{
			name:      "embeddings role empty",
			endpoints: `[{"id":"e","type":"embeddings","backend":"ollama","baseURL":"http://h/v1","model":"m"}]`,
			roles:     `{}`,
			wantErr:   "AI_ROLES.embeddings is required",
		},
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

func TestLoadConfig_AIEndpointsWinsOverLegacy(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	// Both set: AI_ENDPOINTS wins, EMBEDDINGS_* is ignored.
	t.Setenv("EMBEDDINGS_BASE_URL", "http://legacy:11434/v1")
	t.Setenv("EMBEDDINGS_MODEL", "legacy-model")
	t.Setenv("AI_ENDPOINTS", `[{"id":"e","type":"embeddings","backend":"ollama","baseURL":"http://new:11434/v1","model":"new-model"}]`)
	t.Setenv("AI_ROLES", `{"embeddings":"e"}`)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	emb, ok := cfg.EmbeddingsEndpoint()
	require.True(t, ok)
	assert.Equal(t, "new-model", emb.Model)
	assert.Equal(t, "http://new:11434/v1", emb.BaseURL)
}

// ─── EVAL_GATES_EMBED fail-closed startup (CONTRACT §2.4) ───────────────────

// TestLoadConfig_EvalGatesEmbed_FailClosedWithNoEndpoint verifies that
// EVAL_GATES_EMBED=true without any eval endpoint is a fatal startup error.
// This is the fail-closed contract: a gate without a judge would silently stall
// the corpus (no transcript can ever be embedded), so it must be detected at
// startup rather than at runtime. CONTRACT §2.4.
func TestLoadConfig_EvalGatesEmbed_FailClosedWithNoEndpoint(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("EVAL_GATES_EMBED", "true")
	// No AI_ROLES["eval"], no EVAL_CHAT_BASE_URL, no EVAL_CHAT_MODEL.

	_, err := LoadConfig()
	require.Error(t, err, "EVAL_GATES_EMBED=true without eval endpoint must fail at startup")
	assert.Contains(t, err.Error(), "EVAL_GATES_EMBED=true requires an eval chat endpoint")
}

// TestLoadConfig_EvalGatesEmbed_PassesWithEvalChatEnvVars verifies that
// EVAL_GATES_EMBED=true + EVAL_CHAT_BASE_URL + EVAL_CHAT_MODEL does NOT fail at
// startup. The standalone EVAL_CHAT_* env vars are the escape hatch for
// deployments that don't use the AI_ENDPOINTS registry.
func TestLoadConfig_EvalGatesEmbed_PassesWithEvalChatEnvVars(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("EVAL_GATES_EMBED", "true")
	t.Setenv("EVAL_CHAT_BASE_URL", "http://vllm:8000/v1")
	t.Setenv("EVAL_CHAT_MODEL", "mistral-7b-instruct")

	cfg, err := LoadConfig()
	require.NoError(t, err, "EVAL_GATES_EMBED=true + EVAL_CHAT_* must not fail startup")
	assert.True(t, cfg.EvalGatesEmbed, "EvalGatesEmbed must be set")
}

// TestLoadConfig_EvalGatesEmbed_PassesWithAIRolesEval verifies that
// EVAL_GATES_EMBED=true + AI_ENDPOINTS/AI_ROLES["eval"] does NOT fail at startup.
// The embeddings role is left unset in AI_ROLES so it falls back to the legacy
// EMBEDDINGS_BASE_URL/EMBEDDINGS_MODEL synthesis path (no "embeddings" endpoint
// required for this test — we're only validating the eval role resolves).
func TestLoadConfig_EvalGatesEmbed_PassesWithAIRolesEval(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	t.Setenv("EVAL_GATES_EMBED", "true")
	// Two endpoints: one for eval (chat), one for embeddings.
	t.Setenv("AI_ENDPOINTS", `[`+
		`{"id":"ev","type":"chat","backend":"openai-compat","baseURL":"http://vllm:8000/v1","model":"mistral"},`+
		`{"id":"emb","type":"embeddings","backend":"openai-compat","baseURL":"http://ollama:11434/v1","model":"nomic-embed-text"}`+
		`]`)
	t.Setenv("AI_ROLES", `{"embeddings":"emb","eval":"ev"}`)

	cfg, err := LoadConfig()
	require.NoError(t, err, "EVAL_GATES_EMBED=true with AI_ROLES[eval] must not fail startup")
	assert.True(t, cfg.EvalGatesEmbed, "EvalGatesEmbed must be set")
	_, ok := cfg.EvalEndpoint()
	assert.True(t, ok, "EvalEndpoint() must resolve from AI_ROLES")
}

// TestLoadConfig_EvalGatesEmbed_FalseByDefault verifies EVAL_GATES_EMBED
// defaults to false (opt-in, no behavior change for unconfigured deployments).
func TestLoadConfig_EvalGatesEmbed_FalseByDefault(t *testing.T) {
	clearContractEnvVars(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	// EVAL_GATES_EMBED not set.

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.False(t, cfg.EvalGatesEmbed, "EVAL_GATES_EMBED must default to false")
}
