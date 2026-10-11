package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

func baseCfg() *config.Config {
	return &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "judge", Type: config.AIEndpointTypeChat, BaseURL: "http://litellm:4000/v1", Model: "anthropic/claude-haiku-5-5"},
			{ID: "jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
			{ID: "clef", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://ollama:11434", Model: "clef-1.0.0"},
		},
		AIRoles: &config.AIRoles{Embeddings: "embed-1", Eval: "judge", Decide: "jev", Scan: "jev"},
		Models: &config.ModelRegistry{Steps: map[string]config.ModelPin{
			recipe.StepDecide: {Params: map[string]any{config.ParamUSDPerMTokIn: 0.042}},
		}},
	}
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	rows   map[string]db.ModelSetting
	events []db.ModelSettingEvent
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]db.ModelSetting{}} }

func (f *fakeStore) ListModelSettings(context.Context) ([]db.ModelSetting, error) {
	var out []db.ModelSetting
	for _, s := range config.SettingSteps {
		if r, ok := f.rows[s]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) SetModelSetting(_ context.Context, s db.ModelSetting, actor, note string) (*db.ModelSetting, db.ModelSetting, error) {
	var prev *db.ModelSetting
	if r, ok := f.rows[s.Step]; ok {
		prev = &r
	}
	s.UpdatedBy, s.UpdatedAt = actor, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	f.rows[s.Step] = s
	f.events = append(f.events, db.ModelSettingEvent{Step: s.Step, Action: db.ModelSettingSet, Actor: actor, Note: note})
	return prev, s, nil
}

func (f *fakeStore) UnsetModelSetting(_ context.Context, step, actor, note string) (*db.ModelSetting, error) {
	r, ok := f.rows[step]
	if !ok {
		return nil, nil
	}
	delete(f.rows, step)
	f.events = append(f.events, db.ModelSettingEvent{Step: step, Action: db.ModelSettingUnset, Actor: actor, Note: note})
	return &r, nil
}

func (f *fakeStore) ModelSettingHistory(context.Context, string, int) ([]db.ModelSettingEvent, error) {
	return f.events, nil
}

func TestNormalizeStep(t *testing.T) {
	for in, want := range map[string]string{
		"propose": "propose", "eval": "propose", "Judge": "propose", "decide": "decide",
		"scan": "scan", "embed": "embed", "embeddings": "embed",
	} {
		got, err := normalizeStep(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"asr", "format", ""} {
		_, err := normalizeStep(bad)
		assert.Error(t, err, bad)
	}
}

func TestActor(t *testing.T) {
	t.Setenv("USER", "justin")
	assert.Equal(t, "cli:justin", Actor())
	t.Setenv("USER", "")
	assert.True(t, strings.HasPrefix(Actor(), "cli:"))
}

func TestBuildSetting(t *testing.T) {
	tests := []struct {
		name    string
		step    string
		o       setOptions
		want    config.StepSetting
		wantErr string
	}{
		{name: "model only defaults the env endpoint", step: "propose", o: setOptions{model: "ollama/qwen3.8:27b-q8_0"},
			want: config.StepSetting{Step: "propose", EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0", Source: config.SourceDB}},
		{name: "endpoint only takes its model", step: "decide", o: setOptions{endpoint: "clef"},
			want: config.StepSetting{Step: "decide", EndpointID: "clef", Model: "clef-1.0.0", Source: config.SourceDB}},
		{name: "pins and params", step: "decide",
			o: setOptions{endpoint: "jev", model: "jev-1.14.0", expectedModel: "typesafe/jev-1.14.0", revision: "r1",
				params: []string{"usd_per_mtok_in=0.05"}},
			want: config.StepSetting{Step: "decide", EndpointID: "jev", Model: "jev-1.14.0", ExpectedModel: "typesafe/jev-1.14.0",
				Revision: "r1", Params: map[string]any{"usd_per_mtok_in": 0.05}, Source: config.SourceDB}},
		{name: "unknown endpoint", step: "decide", o: setOptions{endpoint: "gone", model: "x-1.0"}, wantErr: `"gone" is not in AI_ENDPOINTS`},
		{name: "wrong type", step: "scan", o: setOptions{endpoint: "judge"}, wantErr: `needs a "systemone" endpoint`},
		{name: "moving alias", step: "scan", o: setOptions{model: "jev-latest"}, wantErr: "moving alias"},
		{name: "bad param key", step: "scan", o: setOptions{params: []string{"usd=1"}}, wantErr: `unknown key "usd"`},
		{name: "bad param value", step: "scan", o: setOptions{params: []string{"usd_per_mtok_in=cheap"}}, wantErr: "non-negative number"},
		{name: "malformed param", step: "scan", o: setOptions{params: []string{"usd_per_mtok_in"}}, wantErr: "key=value"},
		{name: "judge thinking params", step: "propose",
			o: setOptions{model: "openrouter/openai/gpt-6-luna", params: []string{"reasoning_effort=low", "chat_template_kwargs=omit", "temperature=0"}},
			want: config.StepSetting{Step: "propose", EndpointID: "judge", Model: "openrouter/openai/gpt-6-luna",
				Params: map[string]any{"reasoning_effort": "low", "chat_template_kwargs": "omit", "temperature": 0.0}, Source: config.SourceDB}},
		{name: "bad temperature", step: "propose", o: setOptions{params: []string{"temperature=cold"}}, wantErr: "temperature must be a non-negative number"},
		{name: "judge kwargs JSON", step: "propose",
			o: setOptions{params: []string{`chat_template_kwargs={"enable_thinking":false}`}},
			want: config.StepSetting{Step: "propose", EndpointID: "judge", Model: "anthropic/claude-haiku-5-5",
				Params: map[string]any{"chat_template_kwargs": `{"enable_thinking":false}`}, Source: config.SourceDB}},
		{name: "numeric reasoning_effort", step: "propose", o: setOptions{params: []string{"reasoning_effort=1"}}, wantErr: "reasoning_effort must be a string"},
		{name: "bad kwargs", step: "propose", o: setOptions{params: []string{"chat_template_kwargs=yes"}}, wantErr: "chat_template_kwargs must be"},
		{name: "thinking param on decide", step: "decide", o: setOptions{params: []string{"reasoning_effort=low"}}, wantErr: `unknown key "reasoning_effort"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildSetting(baseCfg(), tt.step, tt.o)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("unbound step needs --endpoint", func(t *testing.T) {
		c := baseCfg()
		c.AIRoles.Scan = ""
		_, err := BuildSetting(c, "scan", setOptions{model: "clef-1.0.0"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pass --endpoint")
		s, err := BuildSetting(c, "scan", setOptions{endpoint: "clef"})
		require.NoError(t, err)
		assert.Equal(t, "clef-1.0.0", s.Model)
	})
}

// View: per step, the effective model and its source; an invalid row is
// reported with the row's own values, never as the env model.
func TestView(t *testing.T) {
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	rows := []db.ModelSetting{
		{Step: "propose", EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0", UpdatedBy: "cli:justin", UpdatedAt: at},
		{Step: "scan", EndpointID: "gone", Model: "clef-1.0.0", UpdatedBy: "cli:carol", UpdatedAt: at},
	}
	views := View(baseCfg(), rows)
	require.Len(t, views, 4)
	byStep := map[string]StepView{}
	for _, v := range views {
		byStep[v.Step] = v
	}
	p := byStep["propose"]
	assert.Equal(t, "db", p.Source)
	assert.Equal(t, "judge", p.Endpoint, "the registered endpoint, not the synthesized id")
	assert.Equal(t, "ollama/qwen3.8:27b-q8_0", p.Model)
	assert.Equal(t, "anthropic/claude-haiku-5-5", p.EnvModel)
	assert.Equal(t, "cli:justin", p.UpdatedBy)

	s := byStep["scan"]
	assert.Equal(t, "db", s.Source)
	assert.Equal(t, "gone", s.Endpoint)
	assert.Equal(t, "clef-1.0.0", s.Model)
	assert.Contains(t, s.Error, "earmark models unset scan")

	d := byStep["decide"]
	assert.Equal(t, "env", d.Source)
	assert.Equal(t, 0.042, d.Params[config.ParamUSDPerMTokIn], "an env step keeps its MODELS_FILE pin")
	assert.Empty(t, d.UpdatedBy)

	var buf bytes.Buffer
	require.NoError(t, Show(&buf, baseCfg(), rows, false))
	out := buf.String()
	assert.Contains(t, out, "db INVALID")
	assert.Contains(t, out, "INVALID model_settings rows")
	assert.Contains(t, out, "usd_per_mtok_in=0.042")

	buf.Reset()
	require.NoError(t, Show(&buf, baseCfg(), rows, true))
	var js []StepView
	require.NoError(t, json.Unmarshal(buf.Bytes(), &js))
	require.Len(t, js, 4)
	assert.Equal(t, "propose", js[0].Step)
}

// The judge's effective thinking knobs are shown with their sources: the
// propose setting's params win over the env vars, and the route default
// fills the rest.
func TestViewJudgeThinking(t *testing.T) {
	t.Setenv("EVAL_REASONING_EFFORT", "none")
	t.Setenv("EVAL_CHAT_TEMPLATE_KWARGS", "")
	rows := []db.ModelSetting{{Step: "propose", EndpointID: "judge", Model: "openrouter/openai/gpt-6-luna",
		Params: map[string]any{"reasoning_effort": "low", "chat_template_kwargs": "omit"}}}
	views := View(baseCfg(), rows)
	p := views[0]
	require.Equal(t, "propose", p.Step)
	require.NotNil(t, p.Thinking)
	assert.Equal(t, "low", p.Thinking.ReasoningEffort)
	assert.Equal(t, "setting", p.Thinking.ReasoningEffortSource)
	assert.Equal(t, "omit", p.Thinking.ChatTemplateKwargs)
	assert.Equal(t, "setting", p.Thinking.ChatTemplateKwargsSource)
	for _, v := range views[1:] {
		assert.Nil(t, v.Thinking, "only the judge has thinking knobs (%s)", v.Step)
	}

	var buf bytes.Buffer
	require.NoError(t, Show(&buf, baseCfg(), rows, false))
	assert.Contains(t, buf.String(), `propose thinking: reasoning_effort "low" (model setting) · chat_template_kwargs omitted (model setting)`)
	assert.Contains(t, buf.String(), "reasoning_effort=low")

	// No row: the env model, the env var's effort, the route's kwargs.
	buf.Reset()
	require.NoError(t, Show(&buf, baseCfg(), nil, false))
	assert.Contains(t, buf.String(), `propose thinking: reasoning_effort "none" (EVAL_REASONING_EFFORT) · chat_template_kwargs omitted (auto: hosted route)`)

	// An invalid env value is shown, not hidden.
	t.Setenv("EVAL_CHAT_TEMPLATE_KWARGS", "[1]")
	buf.Reset()
	require.NoError(t, Show(&buf, baseCfg(), nil, false))
	assert.Contains(t, buf.String(), "propose thinking: INVALID — EVAL_CHAT_TEMPLATE_KWARGS")
}

func TestSetUnset(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	var buf bytes.Buffer
	require.NoError(t, Set(ctx, &buf, st, baseCfg(), "decide", setOptions{endpoint: "clef", note: "arm C"}, "cli:justin"))
	assert.Contains(t, buf.String(), "decide: env default → clef-1.0.0 @ clef (by cli:justin)")
	assert.Equal(t, "clef-1.0.0", st.rows["decide"].Model)

	// An invalid set writes nothing.
	err := Set(ctx, &buf, st, baseCfg(), "decide", setOptions{endpoint: "judge"}, "cli:justin")
	require.Error(t, err)
	assert.Len(t, st.events, 1)

	// embed: the dimension probe gates the write.
	probed := 0
	orig := probeEmbed
	t.Cleanup(func() { probeEmbed = orig })
	probeEmbed = func(context.Context, *config.Config) error {
		probed++
		return errors.New("embeddings model \"mxbai-embed-large\" returns 1024-dimension vectors; transcript_chunks.embedding is vector(768)")
	}
	err = Set(ctx, &buf, st, baseCfg(), "embed", setOptions{model: "mxbai-embed-large"}, "cli:justin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing stored")
	_, stored := st.rows["embed"]
	assert.False(t, stored)
	buf.Reset()
	require.NoError(t, Set(ctx, &buf, st, baseCfg(), "embed", setOptions{model: "mxbai-embed-large", noProbe: true}, "cli:justin"))
	assert.Equal(t, 1, probed, "--no-probe skips the probe")
	assert.Contains(t, buf.String(), "requeue --reembed", "an embed change warns about re-embedding")
	probeEmbed = func(context.Context, *config.Config) error { return nil }
	require.NoError(t, Set(ctx, &buf, st, baseCfg(), "embed", setOptions{endpoint: "embed-1"}, "cli:justin"))

	buf.Reset()
	require.NoError(t, Unset(ctx, &buf, st, baseCfg(), "decide", "back", "cli:justin"))
	assert.Contains(t, buf.String(), "decide: clef-1.0.0 @ clef → env default")
	buf.Reset()
	require.NoError(t, Unset(ctx, &buf, st, baseCfg(), "decide", "", "cli:justin"))
	assert.Contains(t, buf.String(), "already on the env default")
	assert.Len(t, st.events, 4, "a no-op unset records nothing")
}

func TestHistory(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, History(&buf, nil, false))
	assert.Contains(t, buf.String(), "no model setting changes")
	buf.Reset()
	require.NoError(t, History(&buf, nil, true))
	assert.Equal(t, "[]\n", buf.String())

	buf.Reset()
	evs := []db.ModelSettingEvent{{ID: 2, Step: "decide", Action: "unset", Actor: "cli:a",
		Old: &db.ModelSettingValue{EndpointID: "clef", Model: "clef-1.0.0", Revision: "r1"}, At: time.Now()}}
	require.NoError(t, History(&buf, evs, false))
	assert.Contains(t, buf.String(), "clef-1.0.0 @ clef (rev r1)")
	assert.Contains(t, buf.String(), "env default")
}

// The decide step shows its effective decide policy and source (CONTRACT
// §2.19 "Decide policy"); set validates the merged policy before storing.
func TestDecidePolicy(t *testing.T) {
	ctx := context.Background()
	views := View(baseCfg(), nil)
	var d StepView
	for _, v := range views {
		if v.Step == recipe.StepDecide {
			d = v
		} else {
			assert.Nil(t, v.Policy, "only decide has a policy (%s)", v.Step)
		}
	}
	require.NotNil(t, d.Policy)
	assert.Equal(t, "preset", d.Policy.Source)
	assert.Equal(t, "preset jev", d.Policy.SourceLabel)
	assert.Equal(t, 0.85, d.Policy.Params[config.ParamApplyP])

	st := newFakeStore()
	var buf bytes.Buffer
	require.NoError(t, Set(ctx, &buf, st, baseCfg(), "decide", setOptions{endpoint: "clef",
		params: []string{"reject_floor=0.05", "usd_per_mtok_in=0.01"}}, "cli:justin"))
	assert.Equal(t, 0.05, st.rows["decide"].Params[config.ParamRejectFloor])
	out := buf.String()
	assert.Contains(t, out, "decide policy: apply p ≥ 0.85 with asin_verbatim · reject p ≤ 0.20 if the replacement is lowercase or p < 0.05")
	assert.Contains(t, out, "source db (reject_floor) over preset clef")
	assert.Contains(t, out, "usd_per_mtok_in=0.01")
	assert.NotContains(t, out, "reject_floor=0.05", "policy keys are not shown as pins")

	buf.Reset()
	require.NoError(t, Show(&buf, baseCfg(), mustRows(t, st), true))
	var js []StepView
	require.NoError(t, json.Unmarshal(buf.Bytes(), &js))
	for _, v := range js {
		if v.Step == recipe.StepDecide {
			require.NotNil(t, v.Policy)
			assert.Equal(t, "db", v.Policy.Source)
			assert.Equal(t, "clef", v.Policy.Preset)
		}
	}

	// Invalid policies are refused and nothing is stored.
	for _, params := range [][]string{
		{"apply_p=0.1"},                     // below the preset's reject_p
		{"reject_floor=0.5"},                // above reject_p
		{"apply_evidence=asin"},             // not an evidence kind
		{"reject_requires_lowercase=maybe"}, // not a boolean
		{"apply_p=2"},                       // out of range
		{"reject_requires_lowercase=false", "reject_floor=0.1"}, // floor without the rule
	} {
		events := len(st.events)
		err := Set(ctx, &buf, st, baseCfg(), "decide", setOptions{endpoint: "clef", params: params}, "cli:justin")
		require.Error(t, err, "%v", params)
		assert.Len(t, st.events, events, "%v stored", params)
	}
	// Policy keys belong to decide only.
	err := Set(ctx, &buf, st, baseCfg(), "scan", setOptions{params: []string{"apply_p=0.9"}}, "cli:justin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown key "apply_p"`)

	// A row the policy check would refuse (stored before, or by hand) shows
	// as invalid rather than as a working policy.
	bad := []db.ModelSetting{{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0",
		Params: map[string]any{config.ParamApplyP: 0.1}}}
	buf.Reset()
	require.NoError(t, Show(&buf, baseCfg(), bad, false))
	assert.Contains(t, buf.String(), "decide policy INVALID")
}

func mustRows(t *testing.T, st *fakeStore) []db.ModelSetting {
	t.Helper()
	rows, err := st.ListModelSettings(context.Background())
	require.NoError(t, err)
	return rows
}
