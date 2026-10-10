package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

func settingsCfg() *config.Config {
	return &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "judge", Type: config.AIEndpointTypeChat, BaseURL: "http://litellm:4000/v1", Model: "anthropic/claude-haiku-5-5"},
			{ID: "jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
		},
		AIRoles: &config.AIRoles{Embeddings: "embed-1", Eval: "judge", Decide: "jev", Scan: "jev"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{}},
	}
}

// The role cards show the effective model and its source; an invalid row is
// a FAILING card carrying the row (never the env model) and its error.
func TestApplySettingState(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	em := applyModelSettings(settingsCfg(), []db.ModelSetting{
		{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0", UpdatedBy: "cli:justin", UpdatedAt: at},
		{Step: recipe.StepDecide, EndpointID: "gone", Model: "clef-1.0.0", UpdatedBy: "cli:carol", UpdatedAt: at},
	})
	require.Len(t, em.Invalid, 1)
	cards := buildRoleCards(roleInputs{Cfg: em.Cfg, Settings: em, Now: time.Now(), ServersKnown: true,
		Snap: &modelsSnapshot{Activity: db.ModelActivity{EvalLastModel: "claude-haiku-5-5"},
			FnRoles: []db.FnRoleActivity{{Step: recipe.StepDecide, Fn: "should_apply", LastModel: "jev-1.13.0"}}}})
	byKey := map[string]roleCard{}
	for _, c := range cards {
		byKey[c.Key] = c
	}

	j := byKey["judge"]
	assert.Equal(t, config.SourceDB, j.ModelSource)
	assert.Equal(t, "ollama/qwen3.8:27b-q8_0", j.Requested)
	assert.Equal(t, "judge", j.EndpointLabel)
	assert.Equal(t, "judge@propose", j.EndpointID)
	assert.Equal(t, "cli:justin", j.SettingBy)
	assert.Empty(t, j.SettingError)

	d := byKey["decide"]
	assert.Equal(t, roleFailing, d.Health.Token)
	assert.Contains(t, d.Health.Sub, "earmark models unset decide")
	assert.Equal(t, "clef-1.0.0", d.Requested)
	assert.Equal(t, "gone", d.EndpointID)
	assert.Contains(t, d.SettingError, `endpoint "gone" is not in AI_ENDPOINTS`)
	assert.Equal(t, matchUnchecked, d.AnsweredMatch, "an answer to another request is not compared")
	assert.True(t, d.Configured)

	assert.Equal(t, config.SourceEnv, byKey["scan"].ModelSource)
	assert.Equal(t, "jev-1.13.0", byKey["scan"].Requested, "scan shares decide's endpoint but not its row")
	assert.Equal(t, config.SourceEnv, byKey["embeddings"].ModelSource)
	assert.Empty(t, byKey["asr"].ModelSource, "ASR has no runtime setting")
	assert.Empty(t, byKey["format"].ModelSource)

	api := apiRolesFrom(cards, modelsEvidence{})
	for _, r := range api {
		switch r.Role {
		case "judge":
			assert.Equal(t, "judge", r.Endpoint, "the API names the registered endpoint")
			assert.Equal(t, "db", r.ModelSource)
			require.NotNil(t, r.SettingUpdatedAt)
		case "decide":
			require.NotNil(t, r.SettingError)
			assert.Equal(t, "failing", r.State)
		}
	}
}

func TestApplySettingStateReadError(t *testing.T) {
	em := effectiveModels{Cfg: settingsCfg(), Err: errors.New("down")}
	cards := buildRoleCards(roleInputs{Cfg: em.Cfg, Settings: em, Now: time.Now(), ServersKnown: true})
	for _, c := range cards {
		if c.Key == "judge" {
			assert.Equal(t, "unknown", c.ModelSource, "a failed read never claims env")
		}
	}
}

// The demo's model-settings scenario serves rows; the others none.
func TestDemoModelSettings(t *testing.T) {
	rows, err := demoDB{scenario: demoScenarioModelSettings}.ListModelSettings(context.Background())
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	rows, err = demoDB{scenario: "active"}.ListModelSettings(context.Background())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// settingsDemoDB is the demo DB with chosen model_settings rows.
type settingsDemoDB struct {
	demoDB
	rows []db.ModelSetting
}

func (d settingsDemoDB) ListModelSettings(context.Context) ([]db.ModelSetting, error) {
	return d.rows, nil
}

// Review regression: with no env judge, a valid propose row makes the judge
// card configured and enables the "run eval" buttons; without the row they
// stay off, and an invalid row does not enable them.
func TestEvalConfiguredFollowsProposeSetting(t *testing.T) {
	srv := newDemoServer(":0", "empty")
	cfg := srv.cfg.Clone()
	cfg.AIEndpoints = append(cfg.AIEndpoints, config.AIEndpoint{ID: "judge", Type: config.AIEndpointTypeChat,
		BaseURL: "http://litellm.demo:4000/v1", Model: "anthropic/claude-haiku-5-5"})
	srv.cfg = cfg
	srv.eval.run = demoEvalRun(srv.db)
	ctx := context.Background()

	srv.db = settingsDemoDB{demoDB: demoDB{scenario: "empty"}}
	assert.False(t, srv.evalConfigured(ctx))

	srv.db = settingsDemoDB{demoDB: demoDB{scenario: "empty"}, rows: []db.ModelSetting{
		{Step: recipe.StepPropose, EndpointID: "gone", Model: "x"}}}
	assert.False(t, srv.evalConfigured(ctx), "an invalid row enables nothing")

	srv.db = settingsDemoDB{demoDB: demoDB{scenario: "empty"}, rows: []db.ModelSetting{
		{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0", UpdatedBy: "cli:justin"}}}
	assert.True(t, srv.evalConfigured(ctx))

	em := srv.effectiveModels(ctx)
	cards := buildRoleCards(roleInputs{Cfg: em.Cfg, Settings: em, Judge: srv.judgeConfig(), Now: time.Now(), ServersKnown: true})
	for _, c := range cards {
		if c.Key == "judge" {
			assert.True(t, c.Configured)
			assert.NotEqual(t, roleNotConfigured, c.Health.Token)
			assert.Equal(t, config.SourceDB, c.ModelSource)
		}
	}
}

// A refused search-embedder swap is shown on the Embeddings card.
func TestEmbedSwapErrorOnCard(t *testing.T) {
	em := applyModelSettings(settingsCfg(), nil)
	em.EmbedErr = `embeddings model "bge-m3" returns 1024-dimension vectors; transcript_chunks.embedding is vector(768)`
	for _, c := range buildRoleCards(roleInputs{Cfg: em.Cfg, Settings: em, Now: time.Now(), ServersKnown: true}) {
		if c.Key == "embeddings" {
			assert.Equal(t, roleDegraded, c.Health.Token)
			assert.Contains(t, c.SearchEmbedError, "1024-dimension")
		}
	}
}
