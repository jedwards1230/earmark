package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture model ids and revisions: opaque strings, not the deployment's pins.
const (
	avModelA = "org-a/asr-a"
	avModelB = "org-b/asr-b"
	avRevA   = "1111111111111111111111111111111111111111"
	avRevB   = "2222222222222222222222222222222222222222"
)

func TestModelRegistry_AudioVoteLoads(t *testing.T) {
	registryEnv(t)
	t.Setenv("MODELS_FILE", writeModelsFile(t, `
audio_vote:
  rule: asr_vote@v2
  models:
    - model_id: `+avModelA+`
      revision: "`+avRevA+`"
    - {model_id: `+avModelB+`, revision: "`+avRevB+`"}
`))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	av := cfg.AudioVote()
	require.NotNil(t, av)
	assert.Equal(t, "asr_vote@v2", av.Rule)
	assert.Equal(t, []AudioVoteModel{{avModelA, avRevA}, {avModelB, avRevB}}, av.Models)
	assert.Same(t, av, cfg.Clone().AudioVote(), "Clone keeps the audio_vote block")
}

func TestModelRegistry_AudioVoteAbsent(t *testing.T) {
	registryEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg.AudioVote())
	var nilCfg *Config
	assert.Nil(t, nilCfg.AudioVote())
}

// The block is decoded strictly and validated: a typo or a half-configured
// rung is a startup error, never a rung that counts the wrong votes.
func TestModelRegistry_AudioVoteFailsClosed(t *testing.T) {
	two := "  models:\n    - {model_id: " + avModelA + ", revision: r1}\n    - {model_id: " + avModelB + ", revision: r2}\n"
	tests := []struct{ name, body, wantErr string }{
		{"unknown field", "audio_vote:\n  rule: r\n  modles: []\n", "field modles not found"},
		{"unknown model field", "audio_vote:\n  rule: r\n  models:\n    - {model: x, revision: y}\n", "field model not found"},
		{"no rule", "audio_vote:\n" + two, "audio_vote.rule"},
		{"blank rule", "audio_vote:\n  rule: 'a b'\n" + two, "audio_vote.rule"},
		{"one model", "audio_vote:\n  rule: r\n  models:\n    - {model_id: a, revision: r1}\n", "need at least 2"},
		{"no revision", "audio_vote:\n  rule: r\n  models:\n    - {model_id: a, revision: r1}\n    - {model_id: b}\n", "models[1].revision"},
		{"no model id", "audio_vote:\n  rule: r\n  models:\n    - {model_id: a, revision: r1}\n    - {revision: r2}\n", "models[1].model_id"},
		{"at in id", "audio_vote:\n  rule: r\n  models:\n    - {model_id: a@x, revision: r1}\n    - {model_id: b, revision: r2}\n", "models[0].model_id"},
		{"same model twice", "audio_vote:\n  rule: r\n  models:\n    - {model_id: a, revision: r1}\n    - {model_id: a, revision: r2}\n", `lists "a" twice`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseModelRegistry(strings.NewReader(tt.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestParseAudioModelFlag(t *testing.T) {
	m, err := ParseAudioModelFlag(avModelA + "@" + avRevA)
	require.NoError(t, err)
	assert.Equal(t, AudioVoteModel{ModelID: avModelA, Revision: avRevA}, m)
	assert.Equal(t, avModelA+"@"+avRevA, m.String())
	for _, bad := range []string{"", "x", "x@", "@r", avModelA} {
		_, err := ParseAudioModelFlag(bad)
		assert.Error(t, err, bad)
	}
}
