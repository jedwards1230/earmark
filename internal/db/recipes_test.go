package db

import (
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// TestEmbedRecipe: the chunk recipe records the requested model, the
// registry's expected model and revision, and every parameter that changes a
// stored vector.
func TestEmbedRecipe(t *testing.T) {
	cfg := &config.Config{
		ChunkSize:   512,
		AIEndpoints: []config.AIEndpoint{{ID: "e", Type: config.AIEndpointTypeEmbeddings, Model: "nomic-embed-text"}},
		AIRoles:     &config.AIRoles{Embeddings: "e"},
	}
	r := embedRecipe(cfg, "search_document: ")
	if r.Step != recipe.StepEmbed || r.ModelAlias != "nomic-embed-text" || r.ModelResolved != "nomic-embed-text" {
		t.Errorf("unpinned embed recipe = %+v", r)
	}
	if r.Params["chunk_size"] != 512 || r.Params["dimensions"] != 768 || r.Params["document_prefix"] != "search_document: " {
		t.Errorf("params = %v", r.Params)
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}

	cfg.Models = &config.ModelRegistry{Steps: map[string]config.ModelPin{
		recipe.StepEmbed: {ExpectedModel: "nomic-embed-text:v1.5", Revision: "sha256:0a10"},
	}}
	p := embedRecipe(cfg, "search_document: ")
	if p.ModelResolved != "nomic-embed-text:v1.5" || p.ModelRevision != "sha256:0a10" {
		t.Errorf("pinned embed recipe = %+v", p)
	}

	// A different chunk size or prefix is a different recipe.
	base, _ := r.ID()
	cfg.Models = nil
	cfg.ChunkSize = 1024
	bigger, _ := embedRecipe(cfg, "search_document: ").ID()
	cfg.ChunkSize = 512
	unprefixed, _ := embedRecipe(cfg, "").ID()
	if base == bigger || base == unprefixed {
		t.Error("chunk size / document prefix do not change the embed recipe")
	}
}

// TestInsertRecipeSQLNeverOverwrites: recipes are immutable — registering an
// existing ID must be a no-op, never an update.
func TestInsertRecipeSQLNeverOverwrites(t *testing.T) {
	sql := norm(insertRecipeSQL)
	if !strings.Contains(sql, "ON CONFLICT (recipe_id) DO NOTHING") {
		t.Errorf("insertRecipeSQL must not overwrite an existing recipe:\n%s", sql)
	}
	if strings.Contains(strings.ToUpper(sql), "DO UPDATE") {
		t.Errorf("insertRecipeSQL updates on conflict:\n%s", sql)
	}
}
