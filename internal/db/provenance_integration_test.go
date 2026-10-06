package db

// Postgres integration tests for runner-reported ASR provenance (migration
// 00006, CONTRACT §1.9): the worker stamps transcripts.recipe_id with a real
// asr recipe built from the runner's fields, never stamps a runner that
// reported nothing, and the embedded ASIN / current-recipe / stale-count reads
// behind the identity step and the telemetry gauges. Skipped unless
// EARMARK_TEST_DATABASE_URL is set.

import (
	"context"
	"testing"

	"github.com/jedwards1230/earmark/internal/recipe"
)

const provSeedSQL = `
	INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
	  ('00000000-0000-0000-0000-00000000000a', '/b/Dune/01.m4b', 'c1', 'done'),
	  ('00000000-0000-0000-0000-00000000000b', '/b/Old/01.m4b', 'c2', 'done');
	-- A provenance-aware runner: model, .nemo sha, runner tag, params, ASIN tag.
	INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
	                         segments, raw_text, model_name,
	                         embedded_asin, asr_model_sha256, asr_runner_version, asr_params)
	VALUES ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-00000000000a',
	        '/b/Dune/01.m4b', 'c1', 'en', 60, '[]', 'x', 'nvidia/parakeet-tdt-1.1b',
	        'B002V57VRC', 'abc123', 'v0.41.0',
	        '{"compute_type":"bfloat16","chunk_window_seconds":600.0,"diarize":false}');
	-- A runner that predates 00006: reports nothing.
	INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
	                         segments, raw_text, model_name)
	VALUES ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-00000000000b',
	        '/b/Old/01.m4b', 'c2', 'en', 60, '[]', 'y', 'nvidia/parakeet-tdt-1.1b');
`

// TestIntegrationStampASRRecipes: the runner-reported fields become a
// registered asr recipe whose ID the Go canonical form reproduces; the row is
// stamped once; a runner that reported nothing stays unstamped.
func TestIntegrationStampASRRecipes(t *testing.T) {
	ctx := context.Background()
	d := newIntegrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, provSeedSQL); err != nil {
		t.Fatal(err)
	}

	stamped, err := d.StampASRRecipes(ctx, 10)
	if err != nil {
		t.Fatalf("StampASRRecipes: %v", err)
	}
	want := mustID(t, ASRRecipe(ASRProvenance{
		ModelName:     "nvidia/parakeet-tdt-1.1b",
		ModelSHA256:   "abc123",
		RunnerVersion: "v0.41.0",
		Params:        map[string]any{"compute_type": "bfloat16", "chunk_window_seconds": 600, "diarize": false},
	}))
	if len(stamped) != 1 || stamped[0].RecipeID != want || stamped[0].EmbeddedASIN != "B002V57VRC" ||
		stamped[0].FilePath != "/b/Dune/01.m4b" {
		t.Fatalf("stamped = %+v, want one row with recipe %s and ASIN B002V57VRC", stamped, want)
	}

	var step, code, alias, resolved, revision string
	var version int
	if err := d.pool.QueryRow(ctx, `
		SELECT r.step, r.step_version, r.code_version, r.model_alias, r.model_resolved, r.model_revision
		  FROM transcripts t JOIN recipes r ON r.recipe_id = t.recipe_id
		 WHERE t.id = '00000000-0000-0000-0000-0000000000a1'`).
		Scan(&step, &version, &code, &alias, &resolved, &revision); err != nil {
		t.Fatalf("stamped recipe: %v", err)
	}
	if step != recipe.StepASR || version != asrStepVersion || code != "v0.41.0" ||
		alias != "nvidia/parakeet-tdt-1.1b" || resolved != alias || revision != "abc123" {
		t.Errorf("recipe = %s v%d code=%s alias=%s resolved=%s rev=%s", step, version, code, alias, resolved, revision)
	}

	var oldRecipe *string
	if err := d.pool.QueryRow(ctx,
		`SELECT recipe_id FROM transcripts WHERE id = '00000000-0000-0000-0000-0000000000b1'`).Scan(&oldRecipe); err != nil {
		t.Fatal(err)
	}
	if oldRecipe != nil {
		t.Errorf("a transcript with no runner provenance got recipe %s, want NULL", *oldRecipe)
	}

	// Idempotent: nothing left to stamp.
	again, err := d.StampASRRecipes(ctx, 10)
	if err != nil || len(again) != 0 {
		t.Errorf("second pass = %+v, %v; want nothing", again, err)
	}

	// The embedded tag reads back per file; a file without one reads "".
	if got, err := d.EmbeddedASIN(ctx, "/b/Dune/01.m4b"); err != nil || got != "B002V57VRC" {
		t.Errorf("EmbeddedASIN = %q, %v", got, err)
	}
	if got, err := d.EmbeddedASIN(ctx, "/b/Old/01.m4b"); err != nil || got != "" {
		t.Errorf("EmbeddedASIN(no tag) = %q, %v", got, err)
	}
}

// TestIntegrationStampASRSkipsMalformedParams: a row whose asr_params is not a
// JSON object is reported, not stamped, and does not block the others.
func TestIntegrationStampASRSkipsMalformedParams(t *testing.T) {
	ctx := context.Background()
	d := newIntegrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, provSeedSQL+`
		INSERT INTO transcription_jobs (id, file_path, checksum, status)
		VALUES ('00000000-0000-0000-0000-00000000000c', '/b/Bad/01.m4b', 'c3', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name, asr_runner_version, asr_params)
		VALUES ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-00000000000c',
		        '/b/Bad/01.m4b', 'c3', 'en', 60, '[]', 'z', 'm', 'v1', '[1,2]');
	`); err != nil {
		t.Fatal(err)
	}
	stamped, err := d.StampASRRecipes(ctx, 10)
	if err == nil {
		t.Error("malformed asr_params: want an error")
	}
	if len(stamped) != 1 || stamped[0].ID != "00000000-0000-0000-0000-0000000000a1" {
		t.Errorf("stamped = %+v, want only the well-formed row", stamped)
	}
}

// TestIntegrationRecipeInfoAndStaleCounts: the reads behind earmark_recipe_info
// and earmark_stale_items.
func TestIntegrationRecipeInfoAndStaleCounts(t *testing.T) {
	ctx := context.Background()
	d := newIntegrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, provSeedSQL); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertChunks(ctx, []Chunk{
		{TranscriptID: "00000000-0000-0000-0000-0000000000a1", FilePath: "/b/Dune/01.m4b", EndSec: 30, Text: "a", Embedding: vec(0.1)},
	}); err != nil {
		t.Fatal(err)
	}
	// No current recipes yet: nothing is stale, nothing to report.
	if n, err := d.StaleItemCounts(ctx); err != nil || len(n) != 0 {
		t.Fatalf("StaleItemCounts before current = %v, %v", n, err)
	}

	cur := d.EmbedRecipe()
	if err := d.SetCurrentRecipes(ctx, cur); err != nil {
		t.Fatal(err)
	}
	infos, err := d.ListCurrentRecipes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Step != recipe.StepEmbed || infos[0].RecipeID != mustID(t, cur) ||
		infos[0].Model != "nomic-embed-text" {
		t.Errorf("ListCurrentRecipes = %+v", infos)
	}
	n, err := d.StaleItemCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != 1 || n[recipe.StepEmbed] != 0 {
		t.Errorf("StaleItemCounts = %v, want embed:0", n)
	}

	// A new embed recipe makes the one chunk stale.
	next := cur
	next.Params = map[string]any{"chunk_size": 999}
	if err := d.SetCurrentRecipes(ctx, next); err != nil {
		t.Fatal(err)
	}
	if n, err := d.StaleItemCounts(ctx); err != nil || n[recipe.StepEmbed] != 1 {
		t.Errorf("StaleItemCounts after recipe change = %v, %v; want embed:1", n, err)
	}
}
