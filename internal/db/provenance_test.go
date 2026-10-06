package db

import (
	"testing"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// TestASRRecipe pins how runner-reported provenance maps onto the asr recipe
// (CONTRACT §1.9): the loaded model is both asked-for and answered, the .nemo
// sha256 is the revision, the runner tag is the code version — and each of
// them, and every param, changes the recipe ID.
func TestASRRecipe(t *testing.T) {
	base := ASRProvenance{
		ModelName: "nvidia/parakeet-tdt-1.1b", ModelSHA256: "abc", RunnerVersion: "v0.41.0",
		Params: map[string]any{"compute_type": "bfloat16", "chunk_window_seconds": 600.0},
	}
	r := ASRRecipe(base)
	if r.Step != recipe.StepASR || r.StepVersion != asrStepVersion || r.CodeVersion != "v0.41.0" ||
		r.ModelAlias != "nvidia/parakeet-tdt-1.1b" || r.ModelResolved != r.ModelAlias || r.ModelRevision != "abc" {
		t.Errorf("ASRRecipe = %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	id := func(p ASRProvenance) string {
		s, err := ASRRecipe(p).ID()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	baseID := id(base)
	for name, mut := range map[string]func(*ASRProvenance){
		"model":  func(p *ASRProvenance) { p.ModelName = "nvidia/parakeet-tdt-0.6b-v3" },
		"sha":    func(p *ASRProvenance) { p.ModelSHA256 = "def" },
		"runner": func(p *ASRProvenance) { p.RunnerVersion = "v0.42.0" },
		"dtype": func(p *ASRProvenance) {
			p.Params = map[string]any{"compute_type": "float16", "chunk_window_seconds": 600.0}
		},
		"overlap": func(p *ASRProvenance) {
			p.Params = map[string]any{"compute_type": "bfloat16", "chunk_window_seconds": 300.0}
		},
	} {
		p := base
		mut(&p)
		if id(p) == baseID {
			t.Errorf("changing %s did not change the recipe ID", name)
		}
	}
}
