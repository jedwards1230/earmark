package db

import (
	"context"
	"fmt"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Provenance recipes (CONTRACT §1.9). Recipes are immutable and content-
// addressed, so registering one is an idempotent INSERT … ON CONFLICT DO
// NOTHING, done in the same transaction as the rows that reference it: the FK
// can never fail, and a recipe first seen mid-run (a LiteLLM fallback answering
// in place of the requested model) is registered on the fly.

// insertRecipeSQL registers a recipe. Package var so tests can assert it never
// overwrites an existing recipe (recipes are immutable).
var insertRecipeSQL = `
	INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias,
	                     model_resolved, model_revision, prompt_version, prompt_sha256, params)
	VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''),
	        NULLIF($8, ''), NULLIF($9, ''), $10::jsonb)
	ON CONFLICT (recipe_id) DO NOTHING
`

// setCurrentRecipeSQL points a step at its current recipe.
var setCurrentRecipeSQL = `
	INSERT INTO current_recipes (step, recipe_id) VALUES ($1, $2)
	ON CONFLICT (step) DO UPDATE
	SET recipe_id = EXCLUDED.recipe_id,
	    updated_at = CASE WHEN current_recipes.recipe_id = EXCLUDED.recipe_id
	                      THEN current_recipes.updated_at ELSE now() END
`

// registerRecipe inserts r if absent and returns its ID.
func registerRecipe(ctx context.Context, ex execer, r recipe.Recipe) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	id, err := r.ID()
	if err != nil {
		return "", err
	}
	params, err := r.ParamsJSON()
	if err != nil {
		return "", fmt.Errorf("recipe %s params: %w", r.Step, err)
	}
	if _, err := ex.Exec(ctx, insertRecipeSQL, id, r.Step, r.StepVersion, r.CodeVersion,
		r.ModelAlias, r.ModelResolved, r.ModelRevision, r.PromptVersion, r.PromptSHA256,
		string(params)); err != nil {
		return "", fmt.Errorf("register %s recipe %s: %w", r.Step, id, err)
	}
	return id, nil
}

// RegisterRecipe records a recipe (idempotent) and returns its ID.
func (db *DB) RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error) {
	return registerRecipe(ctx, db.pool, r)
}

// SetCurrentRecipes registers each recipe and makes it the current recipe for
// its step, in one transaction. Called by the ingest process at startup; the
// stale_work view compares every output row against these.
func (db *DB) SetCurrentRecipes(ctx context.Context, rs ...recipe.Recipe) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin current-recipes tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, r := range rs {
		id, err := registerRecipe(ctx, tx, r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, setCurrentRecipeSQL, r.Step, id); err != nil {
			return fmt.Errorf("set current %s recipe: %w", r.Step, err)
		}
	}
	return tx.Commit(ctx)
}

// embedStepVersion is bumped whenever earmark's chunking or embedding logic
// changes the stored chunks (CONTRACT §1.9).
const embedStepVersion = 1

// EmbedRecipe is the recipe every chunk this process writes is stamped with.
func (db *DB) EmbedRecipe() recipe.Recipe {
	return embedRecipe(db.cfg, db.e.DocumentPrefix())
}

// embedRecipe builds the embed recipe: the embeddings endpoint's model (what we
// ask for), the registry's expected model and revision pin, and the chunking/
// embedding parameters including the document prefix actually sent.
//
// The embeddings API response is not read for a resolved model yet, so
// model_resolved is the registry's expected model, else the alias.
func embedRecipe(cfg *config.Config, documentPrefix string) recipe.Recipe {
	var alias string
	if ep, ok := cfg.EmbeddingsEndpoint(); ok {
		alias = ep.Model
	}
	pin := cfg.ModelPin(recipe.StepEmbed)
	resolved := pin.ExpectedModel
	if resolved == "" {
		resolved = alias
	}
	return recipe.Recipe{
		Step:          recipe.StepEmbed,
		StepVersion:   embedStepVersion,
		CodeVersion:   recipe.CodeVersion(),
		ModelAlias:    alias,
		ModelResolved: resolved,
		ModelRevision: pin.Revision,
		Params: map[string]any{
			"chunk_size":      cfg.ChunkSize,
			"dimensions":      openai.EmbeddingDimension,
			"document_prefix": documentPrefix,
		},
	}
}
