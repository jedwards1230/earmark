package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Provenance recipes (CONTRACT §1.9). Recipes are immutable and content-
// addressed, so registering one is an idempotent INSERT … ON CONFLICT DO
// NOTHING, done in the same transaction as the rows that reference it: the FK
// can never fail, and a recipe first seen mid-run (a LiteLLM fallback answering
// in place of the requested model) is registered on the fly.

// insertRecipeSQL registers a recipe and records the build registering it, in
// one statement. Package var so tests can assert it never overwrites an
// existing recipe (recipes are immutable).
//
// The recipe ID does not hash the build (ID format 2), so a later build
// registering an existing recipe hits the conflict: recipes.code_version keeps
// the build that FIRST registered it, and the recipe_builds insert records
// this build (once per pair, never updated). The recipe_builds FK sees the
// recipes row the CTE inserted: RI checks run at the end of the statement.
var insertRecipeSQL = `
	WITH r AS (
		INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias,
		                     model_resolved, model_revision, prompt_version, prompt_sha256, params)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''),
		        NULLIF($8, ''), NULLIF($9, ''), $10::jsonb)
		ON CONFLICT (recipe_id) DO NOTHING
	)
	INSERT INTO recipe_builds (recipe_id, code_version) VALUES ($1, $4)
	ON CONFLICT (recipe_id, code_version) DO NOTHING
`

// equivalentRecipesCTE is a CTE named eq listing every recipe equivalent to
// the recipe whose id is $1 — $1 itself included, even unregistered. Two
// recipes are equivalent when they agree on everything the stale_work view
// compares (step, step_version, the three model fields, prompt version and
// hash, params), i.e. they differ at most in code_version.
//
// Since ID format 2 a code-only change never mints a new recipe, so eq is
// just $1 for every recipe registered under format 2. It matters for the
// format-1 ids registered before it, which hashed code_version: the format-2
// recipe a build registers after the upgrade is equivalent to the format-1
// recipe(s) of the same configuration, and work selectors (decide --yes, scan)
// treat their outputs as already done instead of redoing them once.
//
// recipes is small (one row per configuration ever used) and has no index on
// these columns; the join is a scan of it.
const equivalentRecipesCTE = `
	eq AS (
		SELECT $1::text AS recipe_id
		 UNION
		SELECT o.recipe_id
		  FROM recipes c
		  JOIN recipes o
		    ON (o.step, o.step_version, o.model_alias, o.model_resolved, o.model_revision,
		        o.prompt_version, o.prompt_sha256, o.params)
		       IS NOT DISTINCT FROM
		       (c.step, c.step_version, c.model_alias, c.model_resolved, c.model_revision,
		        c.prompt_version, c.prompt_sha256, c.params)
		 WHERE c.recipe_id = $1
	)`

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
// It follows the effective embeddings endpoint (ApplyEmbedConfig).
func (db *DB) EmbedRecipe() recipe.Recipe {
	db.embedMu.RLock()
	defer db.embedMu.RUnlock()
	cfg := db.embedCfg
	if cfg == nil {
		cfg = db.cfg
	}
	return embedRecipe(cfg, db.e.DocumentPrefix())
}

// currentEmbedRecipe is the recipe InsertChunks stamps now.
func (db *DB) currentEmbedRecipe() recipe.Recipe {
	db.embedMu.RLock()
	defer db.embedMu.RUnlock()
	return db.embedRecipe
}

// embedder is the embeddings client in effect now.
func (db *DB) embedder() *openai.Embeddings {
	db.embedMu.RLock()
	defer db.embedMu.RUnlock()
	return db.e
}

// embedConfigKey identifies everything about the embed step a runtime setting
// can change: the endpoint (URL, key, model) and the embed pin.
func embedConfigKey(cfg *config.Config) string {
	ep, _ := cfg.EmbeddingsEndpoint()
	pin := cfg.ModelPin(recipe.StepEmbed)
	return strings.Join([]string{ep.ID, ep.BaseURL, ep.Model, ep.APIKey, pin.ExpectedModel, pin.Revision}, "\x00")
}

// embedProbeTimeout bounds the dimension probe of a new embeddings model.
const embedProbeTimeout = 15 * time.Second

// ApplyEmbedConfig points the embeddings client and the embed recipe at
// cfg's embeddings endpoint and pin — the effective config after runtime
// model settings (CONTRACT §2.20) — and reports whether anything changed.
// A new embedder is first probed (embedProbe: one query, which must return
// vector(768) values); a probe failure keeps the current embedder and
// returns the error, so neither search nor the worker ever switches to a
// model the vector column cannot hold. The worker calls it once per cycle,
// before it embeds, so one batch never mixes two embed models; search
// processes call it on a short timer so the query side follows the setting.
func (db *DB) ApplyEmbedConfig(cfg *config.Config) (bool, error) {
	key := embedConfigKey(cfg)
	db.embedMu.RLock()
	same := key == db.embedKey && db.e != nil
	db.embedMu.RUnlock()
	if same {
		return false, nil
	}
	e := openai.NewEmbeddings(cfg)
	if db.embedProbe != nil {
		ctx, cancel := context.WithTimeout(context.Background(), embedProbeTimeout)
		err := db.embedProbe(ctx, e)
		cancel()
		if err != nil {
			db.setEmbedSettingErr(err.Error())
			return false, err
		}
	}
	db.embedMu.Lock()
	defer db.embedMu.Unlock()
	db.e, db.embedCfg, db.embedKey = e, cfg, key
	db.embedRecipe = embedRecipe(cfg, e.DocumentPrefix())
	db.embedErr = ""
	return true, nil
}

// probeEmbedDimension is the default embedProbe.
func probeEmbedDimension(ctx context.Context, e *openai.Embeddings) error {
	return e.CheckDimension(ctx)
}

func (db *DB) setEmbedSettingErr(msg string) {
	db.embedMu.Lock()
	defer db.embedMu.Unlock()
	db.embedErr = msg
}

// EmbedSettingError is why this process's embedder is not on the effective
// embed setting ("" when it is): the last refused swap (a failed dimension
// probe) or invalid embed row seen by WatchEmbedSettings. Shown on the
// Models page.
func (db *DB) EmbedSettingError() string {
	db.embedMu.RLock()
	defer db.embedMu.RUnlock()
	return db.embedErr
}

// EmbedRecipeFor is the embed recipe cfg's embeddings endpoint would stamp —
// for deciding the current embed recipe of an effective config (CONTRACT
// §2.20) without swapping this process's embedder.
func EmbedRecipeFor(cfg *config.Config) recipe.Recipe {
	return embedRecipe(cfg, openai.NewEmbeddings(cfg).DocumentPrefix())
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
