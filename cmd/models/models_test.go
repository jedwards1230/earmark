package models

import (
	"bytes"
	"context"
	"encoding/json"
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

	buf.Reset()
	require.NoError(t, Set(ctx, &buf, st, baseCfg(), "embed", setOptions{model: "mxbai-embed-large"}, "cli:justin"))
	assert.Contains(t, buf.String(), "768-dimension", "an embed change warns about the vector column and re-embedding")

	buf.Reset()
	require.NoError(t, Unset(ctx, &buf, st, baseCfg(), "decide", "back", "cli:justin"))
	assert.Contains(t, buf.String(), "decide: clef-1.0.0 @ clef → env default")
	buf.Reset()
	require.NoError(t, Unset(ctx, &buf, st, baseCfg(), "decide", "", "cli:justin"))
	assert.Contains(t, buf.String(), "already on the env default")
	assert.Len(t, st.events, 3, "a no-op unset records nothing")
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
