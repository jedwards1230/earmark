package asr

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllCapabilities(t *testing.T) {
	all := AllCapabilities()
	require.Len(t, all, 5)
	// Stable order is part of the contract for the dashboard's badge strip.
	assert.Equal(t, []Capability{
		CapWordTimestamps,
		CapContextBiasing,
		CapDiarization,
		CapConfidenceScores,
		CapLanguageDetection,
	}, all)

	// Returned slice is a copy — mutating it must not affect the package state.
	all[0] = "mutated"
	assert.Equal(t, CapWordTimestamps, AllCapabilities()[0])
}

func TestIsKnownCapability(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"word_timestamps", true},
		{"context_biasing", true},
		{"diarization", true},
		{"confidence_scores", true},
		{"language_detection", true},
		{"", false},
		{"speaker_count", false},
		{"Word_Timestamps", false}, // case-sensitive — enum keys are exact
		{"word_timestamps ", false},
	}
	for _, tt := range tests {
		assert.Equalf(t, tt.want, IsKnownCapability(tt.key), "key=%q", tt.key)
	}
}

func TestParseCapabilities(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]bool
		want Capabilities
	}{
		{"nil", nil, nil},
		{"empty", map[string]bool{}, nil},
		{
			"all known",
			map[string]bool{"word_timestamps": true, "context_biasing": false},
			Capabilities{CapWordTimestamps: true, CapContextBiasing: false},
		},
		{
			"drops unknown keys",
			map[string]bool{"word_timestamps": true, "bogus": true, "another": false},
			Capabilities{CapWordTimestamps: true},
		},
		{
			"only unknown keys collapses to nil",
			map[string]bool{"bogus": true, "nope": false},
			nil,
		},
		{
			"all five",
			map[string]bool{
				"word_timestamps":    true,
				"context_biasing":    true,
				"diarization":        false,
				"confidence_scores":  true,
				"language_detection": false,
			},
			Capabilities{
				CapWordTimestamps:    true,
				CapContextBiasing:    true,
				CapDiarization:       false,
				CapConfidenceScores:  true,
				CapLanguageDetection: false,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCapabilities(tt.in, "test")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseCapabilitiesJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Capabilities
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace", "   ", nil, false},
		{"valid", `{"word_timestamps":true,"diarization":false}`,
			Capabilities{CapWordTimestamps: true, CapDiarization: false}, false},
		{"drops unknown", `{"word_timestamps":true,"bogus":true}`,
			Capabilities{CapWordTimestamps: true}, false},
		{"malformed", `{not json`, nil, true},
		{"wrong value type", `{"word_timestamps":"yes"}`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCapabilitiesJSON(tt.in, "test")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestCapabilitiesJSONRoundTrip confirms Capabilities serializes as a plain
// {string: bool} object — the shape the run_metrics caps_* JSONB columns and the
// ASR_CAPABILITIES env value expect.
func TestCapabilitiesJSONRoundTrip(t *testing.T) {
	caps := Capabilities{CapWordTimestamps: true, CapContextBiasing: false}
	b, err := json.Marshal(caps)
	require.NoError(t, err)

	var raw map[string]bool
	require.NoError(t, json.Unmarshal(b, &raw))
	assert.Equal(t, map[string]bool{"word_timestamps": true, "context_biasing": false}, raw)

	// Re-parse through the validator and confirm it's stable.
	got := ParseCapabilities(raw, "roundtrip")
	assert.Equal(t, caps, got)
}

func TestKnownFamily(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{FamilyNeMoParakeet, true},
		{FamilyNeMoCanary, true},
		{FamilyGraniteSpeech, true},
		{FamilyWhisper, true},
		{"NEMO-PARAKEET", true}, // case-insensitive
		{"  whisper  ", true},   // trimmed
		{"some-future-family", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equalf(t, tt.want, KnownFamily(tt.id), "id=%q", tt.id)
	}
}

func TestKnownRuntime(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{RuntimeNeMoCUDA, true},
		{RuntimeParakeetMLX, true},
		{RuntimeParakeetCPP, true},
		{RuntimeWhisperCPPSYCL, true},
		{RuntimeWhisperCPP, true},
		{RuntimeOpenVINO, true},
		{"NEMO-CUDA", true}, // case-insensitive
		{"  openvino ", true},
		{"some-future-runtime", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equalf(t, tt.want, KnownRuntime(tt.id), "id=%q", tt.id)
	}
}
