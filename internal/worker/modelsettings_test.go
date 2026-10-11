package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/queue"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// settingsDB is a fakeDB that also serves model_settings rows and records
// ApplyEmbedConfig calls.
type settingsDB struct {
	fakeDB
	smu     sync.Mutex
	rows    []db.ModelSetting
	readErr error
	applied []string // the embed model of each ApplyEmbedConfig that changed it
	cur     string
	refuse  string // an embed model whose dimension probe fails
}

func (s *settingsDB) ListModelSettings(context.Context) ([]db.ModelSetting, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.rows, s.readErr
}

func (s *settingsDB) ApplyEmbedConfig(cfg *config.Config) (bool, error) {
	ep, _ := cfg.EmbeddingsEndpoint()
	if ep.Model == s.cur {
		return false, nil
	}
	if ep.Model == s.refuse {
		return false, errors.New("returns 1024-dimension vectors; transcript_chunks.embedding is vector(768)")
	}
	s.cur = ep.Model
	s.applied = append(s.applied, ep.Model)
	return true, nil
}

func settingsWorkerCfg() *config.Config {
	return &config.Config{
		ChunkSize: 64, EvalInPipeline: true,
		AIEndpoints: []config.AIEndpoint{
			{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "embed-2", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://vllm:8000/v1", Model: "mxbai-embed-large"},
			{ID: "judge", Type: config.AIEndpointTypeChat, BaseURL: "http://litellm:4000/v1", Model: "anthropic/claude-haiku-5-5"},
			{ID: "jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
		},
		AIRoles: &config.AIRoles{Embeddings: "embed-1", Eval: "judge", Decide: "jev"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{}},
	}
}

// Each cycle runs on base + the current model_settings rows (CONTRACT §2.20):
// a change re-points the judge and the embedder at the next cycle, an invalid
// row for one of the worker's steps idles the cycle (never the env model),
// and a bad row for a step the worker does not run does not stop it.
func TestCycleConfigFollowsModelSettings(t *testing.T) {
	base := settingsWorkerCfg()
	sdb := &settingsDB{cur: "nomic-embed-text"}
	w := NewWorker(&queue.Queue{}, sdb, base)
	var seen []*config.Config
	var seenInvalid []map[string]error
	w.SetModelSettings(sdb, func(full *config.Config, invalid map[string]error) {
		seen, seenInvalid = append(seen, full), append(seenInvalid, invalid)
	})
	require.NotNil(t, w.judge)
	require.Equal(t, "anthropic/claude-haiku-5-5", w.judge.Model())

	// No rows: the env config, nothing swapped.
	cfg, ok := w.cycleConfig(base)
	require.True(t, ok)
	require.Equal(t, config.SourceEnv, cfg.StepSource(recipe.StepPropose))
	require.Empty(t, sdb.applied)
	require.Len(t, seen, 1)

	// Rows for both worker steps: the judge and the embedder follow.
	sdb.rows = []db.ModelSetting{
		{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"},
		{Step: recipe.StepEmbed, EndpointID: "embed-2", Model: "mxbai-embed-large"},
	}
	cfg, ok = w.cycleConfig(base)
	require.True(t, ok)
	require.Equal(t, "ollama/qwen3.8:27b-q8_0", w.judge.Model())
	require.Equal(t, []string{"mxbai-embed-large"}, sdb.applied)
	require.Equal(t, "mxbai-embed-large", embedModel(cfg), "run_metrics names the effective embed model")
	judge := w.judge

	// Same rows next cycle: nothing rebuilt.
	_, ok = w.cycleConfig(base)
	require.True(t, ok)
	require.Same(t, judge, w.judge)
	require.Len(t, sdb.applied, 1)

	// A bad decide row: not the worker's step — it runs on, and the full
	// config handed to onChange reports it.
	sdb.rows = append(sdb.rows, db.ModelSetting{Step: recipe.StepDecide, EndpointID: "gone", Model: "clef-1.0.0"})
	lastGood, ok := w.cycleConfig(base)
	require.True(t, ok)
	require.Error(t, seenInvalid[len(seenInvalid)-1][recipe.StepDecide])

	// A bad embed row: the cycle idles; the judge and embedder stay as they were.
	sdb.rows = []db.ModelSetting{{Step: recipe.StepEmbed, EndpointID: "gone", Model: "x"}}
	_, ok = w.cycleConfig(base)
	require.False(t, ok, "an invalid embed row must idle the worker, not embed with the env model")
	require.Equal(t, []string{"mxbai-embed-large"}, sdb.applied)

	// Unreadable settings: keep the last config that applied.
	sdb.rows, sdb.readErr = nil, errors.New("connection reset")
	cfg, ok = w.cycleConfig(base)
	require.True(t, ok)
	require.Same(t, lastGood, cfg)
}

// Before any cycle has applied the settings, an unreadable model_settings
// idles rather than running on the env config.
func TestCycleConfigUnreadableBeforeFirstCycle(t *testing.T) {
	base := settingsWorkerCfg()
	sdb := &settingsDB{readErr: errors.New("down")}
	w := NewWorker(&queue.Queue{}, sdb, base)
	w.SetModelSettings(sdb, nil)
	_, ok := w.cycleConfig(base)
	require.False(t, ok)

	sdb.readErr = nil
	cfg, ok := w.cycleConfig(base)
	require.True(t, ok)

	sdb.readErr = errors.New("blip")
	again, ok := w.cycleConfig(base)
	require.True(t, ok, "after a good cycle a read error keeps the last applied settings")
	require.Same(t, cfg, again)
}

// Without SetModelSettings the worker runs on its env config, as before.
func TestCycleConfigWithoutSettings(t *testing.T) {
	base := settingsWorkerCfg()
	w := NewWorker(&queue.Queue{}, &fakeDB{}, base)
	cfg, ok := w.cycleConfig(base)
	require.True(t, ok)
	require.Same(t, base, cfg)
}

// A refused embedder swap (dimension probe) idles the cycle and keeps the
// previous embedder.
func TestCycleConfigRefusedEmbedSwapIdles(t *testing.T) {
	base := settingsWorkerCfg()
	sdb := &settingsDB{cur: "nomic-embed-text", refuse: "mxbai-embed-large"}
	w := NewWorker(&queue.Queue{}, sdb, base)
	w.SetModelSettings(sdb, nil)
	sdb.rows = []db.ModelSetting{{Step: recipe.StepEmbed, EndpointID: "embed-2", Model: "mxbai-embed-large"}}
	_, ok := w.cycleConfig(base)
	require.False(t, ok)
	require.Empty(t, sdb.applied)
}

// Review regression: with no usable env judge, setting then unsetting a
// propose row must leave the worker embedding (inline eval off), exactly as
// before the row existed — not idle until a restart.
func TestCycleConfigUnsetProposeWithoutEnvJudge(t *testing.T) {
	t.Setenv("EVAL_CHAT_BASE_URL", "")
	t.Setenv("EVAL_CHAT_MODEL", "")
	base := settingsWorkerCfg()
	base.AIRoles.Eval = "" // no env judge
	sdb := &settingsDB{cur: "nomic-embed-text"}
	w := NewWorker(&queue.Queue{}, sdb, base)
	w.SetModelSettings(sdb, nil)
	require.Nil(t, w.judge)

	_, ok := w.cycleConfig(base)
	require.True(t, ok, "env with no judge embeds")

	sdb.rows = []db.ModelSetting{{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"}}
	_, ok = w.cycleConfig(base)
	require.True(t, ok)
	require.NotNil(t, w.judge)

	sdb.rows = nil
	_, ok = w.cycleConfig(base)
	require.True(t, ok, "after unset the worker must keep embedding")
	require.Nil(t, w.judge, "inline eval back off")

	// The same failure under EVAL_GATES_EMBED, or for a row-named judge, idles.
	w.evalGatesEmbed = true
	sdb.rows = []db.ModelSetting{{Step: recipe.StepPropose, EndpointID: "judge", Model: "x"}}
	_, ok = w.cycleConfig(base)
	require.True(t, ok, "a resolvable row judge runs")
}

// A propose row that changes only the judge's thinking params is a new
// judge: the key covers the pin params.
func TestJudgeKeyCoversPinParams(t *testing.T) {
	base := settingsWorkerCfg()
	set := func(params map[string]any) string {
		t.Helper()
		eff, err := base.WithStepSettings(config.StepSetting{Step: recipe.StepPropose, EndpointID: "judge",
			Model: "openrouter/openai/gpt-6-luna", Params: params})
		require.NoError(t, err)
		return judgeKey(eff)
	}
	low := set(map[string]any{config.ParamReasoningEffort: "low"})
	require.NotEqual(t, low, set(map[string]any{config.ParamReasoningEffort: "medium"}))
	require.NotEqual(t, low, set(nil))
	require.Equal(t, low, set(map[string]any{config.ParamReasoningEffort: "low"}))
}
