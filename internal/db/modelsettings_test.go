package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// fakeSettings is an in-memory SettingsReader.
type fakeSettings struct {
	mu   sync.Mutex
	rows []ModelSetting
	err  error
}

func (f *fakeSettings) ListModelSettings(context.Context) ([]ModelSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.err
}

func (f *fakeSettings) set(rows []ModelSetting, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

func settingsBaseCfg() *config.Config {
	return &config.Config{
		ChunkSize: 512,
		AIEndpoints: []config.AIEndpoint{
			{ID: "e1", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "e2", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://vllm:8000/v1", Model: "mxbai-embed-large"},
			{ID: "jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
			{ID: "s1", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000", Model: "jev-1.13.0"},
		},
		AIRoles: &config.AIRoles{Embeddings: "e1", Decide: "jev", Scan: "jev"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{}},
	}
}

// ResolveStep: row then override, only for the step asked; a read error and
// an invalid row fail; nothing is ever written (the reader has no writes).
func TestResolveStep(t *testing.T) {
	ctx := context.Background()
	base := settingsBaseCfg()
	r := &fakeSettings{rows: []ModelSetting{
		{Step: recipe.StepDecide, EndpointID: "s1", Model: "clef:27b"},
		{Step: recipe.StepScan, EndpointID: "gone", Model: "x-1.0"},
	}}
	tests := []struct {
		name, endpoint, model string
		wantModel, wantSource string
		wantEndpoint          string
	}{
		{"row", "", "", "clef:27b", config.SourceDB, "s1"},
		{"model override keeps the row's endpoint", "", "clef:12b", "clef:12b", config.SourceOverride, "s1"},
		{"endpoint override takes its model", "jev", "", "jev-1.13.0", config.SourceOverride, "jev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ResolveStep(ctx, r, base, recipe.StepDecide, tt.endpoint, tt.model)
			if err != nil {
				t.Fatal(err)
			}
			ep, _ := cfg.DecideEndpoint()
			id := ep.ID
			if ep.BaseID != "" {
				id = ep.BaseID
			}
			if ep.Model != tt.wantModel || cfg.StepSource(recipe.StepDecide) != tt.wantSource || id != tt.wantEndpoint {
				t.Errorf("got %s@%s (%s), want %s@%s (%s)", ep.Model, id, cfg.StepSource(recipe.StepDecide),
					tt.wantModel, tt.wantEndpoint, tt.wantSource)
			}
		})
	}
	// The invalid scan row does not touch decide, but fails scan.
	if _, err := ResolveStep(ctx, r, base, recipe.StepScan, "", ""); err == nil || !strings.Contains(err.Error(), "earmark models unset scan") {
		t.Errorf("invalid scan row: %v", err)
	}
	// A clef model on the TypeSafe pass-through is refused (it would go to TypeSafe).
	if _, err := ResolveStep(ctx, r, base, recipe.StepDecide, "jev", "clef:27b"); err == nil || !strings.Contains(err.Error(), "pass-through") {
		t.Errorf("clef on the pass-through: %v", err)
	}
	r.set(nil, errors.New("down"))
	if _, err := ResolveStep(ctx, r, base, recipe.StepDecide, "", ""); err == nil {
		t.Error("a read error resolved a config")
	}
}

func testEmbedDB(cfg *config.Config) *DB {
	d := &DB{cfg: cfg, log: log.NewLogger("db-test"), e: openai.NewEmbeddings(cfg)}
	d.embedRecipe = d.EmbedRecipe()
	d.embedKey = embedConfigKey(cfg)
	return d
}

// A failed dimension probe keeps the current embedder and reports why; a
// passing one swaps; ApplyEmbedSettings follows the row.
func TestApplyEmbedConfigProbe(t *testing.T) {
	base := settingsBaseCfg()
	d := testEmbedDB(base)
	envID := mustRecipeID(t, d.currentEmbedRecipe())
	eff, err := base.WithStepSettings(config.StepSetting{Step: recipe.StepEmbed, EndpointID: "e2", Model: "mxbai-embed-large"})
	if err != nil {
		t.Fatal(err)
	}

	d.embedProbe = func(context.Context, *openai.Embeddings) error {
		return errors.New("returns 1024-dimension vectors; transcript_chunks.embedding is vector(768)")
	}
	if changed, err := d.ApplyEmbedConfig(eff); changed || err == nil {
		t.Fatalf("a failed probe swapped: changed %v err %v", changed, err)
	}
	if got := mustRecipeID(t, d.currentEmbedRecipe()); got != envID {
		t.Error("a refused swap changed the chunk stamp")
	}
	if !strings.Contains(d.EmbedSettingError(), "1024-dimension") {
		t.Errorf("EmbedSettingError = %q", d.EmbedSettingError())
	}

	d.embedProbe = func(context.Context, *openai.Embeddings) error { return nil }
	r := &fakeSettings{rows: []ModelSetting{{Step: recipe.StepEmbed, EndpointID: "e2", Model: "mxbai-embed-large"}}}
	if changed, err := d.ApplyEmbedSettings(context.Background(), r, base); !changed || err != nil {
		t.Fatalf("ApplyEmbedSettings: changed %v err %v", changed, err)
	}
	if d.EmbedSettingError() != "" || mustRecipeID(t, d.currentEmbedRecipe()) == envID {
		t.Error("a passing probe did not swap")
	}
	// An invalid row keeps the embedder and is reported.
	r.set([]ModelSetting{{Step: recipe.StepEmbed, EndpointID: "gone", Model: "x"}}, nil)
	if _, err := d.ApplyEmbedSettings(context.Background(), r, base); err == nil || d.EmbedSettingError() == "" {
		t.Errorf("invalid row: err %v, reported %q", err, d.EmbedSettingError())
	}
}

// ApplyEmbedConfig swapping while search reads the embedder and the chunk
// stamp: race-free (run with -race), and every read sees a consistent pair.
func TestApplyEmbedConfigConcurrent(t *testing.T) {
	base := settingsBaseCfg()
	d := testEmbedDB(base)
	d.embedProbe = nil
	r := &fakeSettings{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.WatchEmbedSettings(ctx, r, base, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if d.embedder() == nil {
					t.Error("nil embedder")
					return
				}
				_ = d.currentEmbedRecipe()
				_ = d.EmbedRecipe()
				_ = d.EmbedSettingError()
			}
		}()
	}
	for i := 0; i < 50; i++ {
		rows := []ModelSetting(nil)
		if i%2 == 0 {
			rows = []ModelSetting{{Step: recipe.StepEmbed, EndpointID: "e2", Model: "mxbai-embed-large"}}
		}
		r.set(rows, nil)
		if _, err := d.ApplyEmbedConfig(base); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Microsecond)
	}
	wg.Wait()
}

func mustRecipeID(t *testing.T, r recipe.Recipe) string {
	t.Helper()
	id, err := r.ID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
