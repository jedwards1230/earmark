package monitor

import (
	"errors"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// recipeEnv is a production-shaped registry: decide and scan share one
// systemone endpoint, a second systemone endpoint serves Clef.
func recipeEnv(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("EVAL_CHAT_BASE_URL", "")
	t.Setenv("EVAL_CHAT_MODEL", "")
	return &config.Config{
		ChunkSize: 512,
		AIEndpoints: []config.AIEndpoint{
			{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "nomic-embed-text"},
			{ID: "embed-2", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://ollama:11434/v1", Model: "mxbai-embed-large"},
			{ID: "judge", Type: config.AIEndpointTypeChat, BaseURL: "http://litellm:4000/v1", Model: "anthropic/claude-haiku-5-5"},
			{ID: "jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
			{ID: "clef", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://ollama:11434", Model: "clef-1.0.0"},
		},
		AIRoles: &config.AIRoles{Embeddings: "embed-1", Eval: "judge", Decide: "jev", Scan: "jev"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{}},
	}
}

func recipeIDs(t *testing.T, cfg *config.Config, invalid map[string]error) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range currentRecipeSet(cfg, invalid) {
		id, err := r.ID()
		if err != nil {
			t.Fatal(err)
		}
		out[r.Step] = id
	}
	return out
}

// Recipes stay honest (CONTRACT §1.9, §2.20): a db-set model is a different
// recipe for its step only, and taking the setting away (the base config)
// gives back exactly the env recipe ids.
func TestCurrentRecipesFollowModelSettings(t *testing.T) {
	base := recipeEnv(t)
	env := recipeIDs(t, base, nil)
	for _, step := range []string{recipe.StepEmbed, recipe.StepPropose, recipe.StepDecide, recipe.StepScan} {
		if env[step] == "" {
			t.Fatalf("env config registers no %s recipe", step)
		}
	}
	tests := []struct {
		name    string
		setting config.StepSetting
	}{
		{"propose on another model", config.StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"}},
		{"propose pinned", config.StepSetting{Step: recipe.StepPropose, EndpointID: "judge", Model: "anthropic/claude-haiku-5-5", ExpectedModel: "claude-haiku-5-5"}},
		{"decide on Clef", config.StepSetting{Step: recipe.StepDecide, EndpointID: "clef", Model: "clef-1.0.0"}},
		{"scan on a newer Jev", config.StepSetting{Step: recipe.StepScan, EndpointID: "jev", Model: "jev-1.14.0"}},
		{"embed on another model", config.StepSetting{Step: recipe.StepEmbed, EndpointID: "embed-2", Model: "mxbai-embed-large"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eff, err := base.WithStepSettings(tt.setting)
			if err != nil {
				t.Fatal(err)
			}
			got := recipeIDs(t, eff, nil)
			for step, id := range env {
				if step == tt.setting.Step {
					if got[step] == id {
						t.Errorf("%s: the db-set model reused the env recipe %s", step, id)
					}
				} else if got[step] != id {
					t.Errorf("%s changed (%s → %s) though only %s was set", step, id, got[step], tt.setting.Step)
				}
			}
			// Unset: the env config again, the env recipes again.
			if again := recipeIDs(t, base, nil); again[tt.setting.Step] != env[tt.setting.Step] {
				t.Errorf("unsetting did not return the env recipe")
			}
		})
	}

	t.Run("a setting equal to env is the env recipe", func(t *testing.T) {
		eff, err := base.WithStepSettings(config.StepSetting{Step: recipe.StepDecide, EndpointID: "jev", Model: "jev-1.13.0"})
		if err != nil {
			t.Fatal(err)
		}
		if got := recipeIDs(t, eff, nil); got[recipe.StepDecide] != env[recipe.StepDecide] {
			t.Errorf("same endpoint+model minted a new decide recipe")
		}
	})

	t.Run("an invalid step registers nothing", func(t *testing.T) {
		got := recipeIDs(t, base, map[string]error{recipe.StepScan: errors.New("bad row")})
		if _, ok := got[recipe.StepScan]; ok {
			t.Error("an invalid scan row still registered a current scan recipe")
		}
		if got[recipe.StepDecide] != env[recipe.StepDecide] {
			t.Error("another step's recipe moved")
		}
	})
}
