package db

// Recipe identity across builds (CONTRACT §1.9 "Canonical form / ID" and
// "Builds"): ID format 2 does not hash code_version, so a release that changes
// nothing about a step keeps its recipe; recipe_builds records every build
// that registered it; and work selectors treat a format-1 recipe that differs
// only in its build as the same recipe. Skipped unless
// EARMARK_TEST_DATABASE_URL is set.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
)

const (
	buildA = "0.47.12+f2166fd"
	buildB = "0.48.0+53b7268"
	buildC = "0.48.1+0c0ffee"
)

func recipeBuilds(t *testing.T, d *DB, id string) []string {
	t.Helper()
	rows, err := d.pool.Query(context.Background(),
		`SELECT code_version FROM recipe_builds WHERE recipe_id = $1 ORDER BY code_version`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// registerFormat1 registers r under its ID format 1 id — what a build before
// format 2 registered — and returns that id.
func registerFormat1(t *testing.T, d *DB, r recipe.Recipe) string {
	t.Helper()
	id, err := r.IDV1()
	if err != nil {
		t.Fatal(err)
	}
	params, err := r.ParamsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(context.Background(), `
		INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias, model_resolved,
		                     model_revision, prompt_version, prompt_sha256, params)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10::jsonb)`,
		id, r.Step, r.StepVersion, r.CodeVersion, r.ModelAlias, r.ModelResolved, r.ModelRevision,
		r.PromptVersion, r.PromptSHA256, string(params)); err != nil {
		t.Fatalf("register format-1 recipe: %v", err)
	}
	return id
}

// TestIntegrationRecipeIDStableAcrossBuilds: two builds with different
// CodeVersion register the SAME recipe; the first build stays on the recipes
// row, every build lands in recipe_builds; a code-only deploy leaves
// current_recipes and stale_work untouched; a step_version bump still mints a
// new recipe and marks the outputs stale.
func TestIntegrationRecipeIDStableAcrossBuilds(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status)
		VALUES ('00000000-0000-0000-0000-0000000001aa', '/b/R/01.m4b', 'r1', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name)
		VALUES ('00000000-0000-0000-0000-0000000001a1', '00000000-0000-0000-0000-0000000001aa',
		        '/b/R/01.m4b', 'r1', 'en', 60, '[]', 'x', 'parakeet');`); err != nil {
		t.Fatal(err)
	}

	// Build A deploys: its embed recipe becomes current, then the worker
	// writes chunks (stamped with this test binary's build).
	embedA := d.EmbedRecipe()
	embedA.CodeVersion = buildA
	if err := d.SetCurrentRecipes(ctx, embedA); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertChunks(ctx, []Chunk{
		{TranscriptID: "00000000-0000-0000-0000-0000000001a1", FilePath: "/b/R/01.m4b", ChunkIndex: 0, EndSec: 30, Text: "a", Embedding: vec(0.1)},
		{TranscriptID: "00000000-0000-0000-0000-0000000001a1", FilePath: "/b/R/01.m4b", ChunkIndex: 1, StartSec: 30, EndSec: 60, Text: "b", Embedding: vec(0.2)},
	}); err != nil {
		t.Fatal(err)
	}
	id := mustID(t, embedA)
	var stamped int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM transcript_chunks WHERE recipe_id = $1`, id).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 2 {
		t.Fatalf("%d chunks carry build A's embed recipe, want 2 (the worker's build registers the same recipe)", stamped)
	}

	// Build B deploys: same configuration, new build.
	embedB := embedA
	embedB.CodeVersion = buildB
	idB, err := d.RegisterRecipe(ctx, embedB)
	if err != nil {
		t.Fatal(err)
	}
	if idB != id {
		t.Fatalf("build B registered recipe %s, build A %s — a code-only release minted a new recipe", idB, id)
	}
	if err := d.SetCurrentRecipes(ctx, embedB); err != nil {
		t.Fatal(err)
	}
	if n := countRowsPool(t, d, `SELECT count(*) FROM recipes WHERE step = 'embed'`); n != 1 {
		t.Errorf("%d embed recipes after two builds, want 1", n)
	}
	var first, current string
	if err := d.pool.QueryRow(ctx, `SELECT r.code_version, cr.recipe_id FROM recipes r
		JOIN current_recipes cr ON cr.recipe_id = r.recipe_id WHERE cr.step = 'embed'`).Scan(&first, &current); err != nil {
		t.Fatal(err)
	}
	if first != buildA || current != id {
		t.Errorf("recipes.code_version = %q (want first build %q), current = %s (want %s)", first, buildA, current, id)
	}
	builds := recipeBuilds(t, d, id)
	for _, want := range []string{buildA, buildB, recipe.CodeVersion()} {
		if !slices.Contains(builds, want) {
			t.Errorf("recipe_builds %v lacks build %q", builds, want)
		}
	}
	// Registering again records nothing new.
	if _, err := d.RegisterRecipe(ctx, embedB); err != nil {
		t.Fatal(err)
	}
	if again := recipeBuilds(t, d, id); !slices.Equal(again, builds) {
		t.Errorf("re-registering changed recipe_builds: %v → %v", builds, again)
	}
	if got := staleRows(t, d); len(got) != 0 {
		t.Errorf("a code-only deploy made outputs stale: %v", got)
	}

	// A logic change (step_version bump) is still a new recipe, and the old
	// outputs are stale under it.
	embedC := embedB
	embedC.CodeVersion, embedC.StepVersion = buildC, embedB.StepVersion+1
	if mustID(t, embedC) == id {
		t.Fatal("a step_version bump did not change the recipe id")
	}
	if err := d.SetCurrentRecipes(ctx, embedC); err != nil {
		t.Fatal(err)
	}
	if got := staleRows(t, d); len(got) != 2 {
		t.Errorf("after a step_version bump stale_work = %v, want both chunks", got)
	}
}

// TestIntegrationFormat1RecipesAreEquivalent: the first build on ID format 2
// registers a new id for each step (the old ids hashed code_version), but the
// outputs of the format-1 recipe of the same configuration are not redone —
// decide --yes neither re-checks its accepts nor re-asks its final holds, and
// scan does not rescan its chunks — and stale_work does not list them. A
// genuinely different recipe still sees them as work.
func TestIntegrationFormat1RecipesAreEquivalent(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 4)
	anchorAll(t, d, ids)

	decide := recipe.Recipe{
		Step: recipe.StepDecide, StepVersion: 2, CodeVersion: buildB, ModelAlias: "jev-1.13.0",
		ModelResolved: "jev-1.13.0", PromptVersion: "should_apply@v2", PromptSHA256: "ab",
		Params: map[string]any{"fn": "should_apply", "rung0_version": "rung0@v2", "apply_p": 0.8},
	}
	oldID := registerFormat1(t, d, decide)
	sha := []string{patch.ChunkHash("ganema said"), patch.ChunkHash("and left")}
	ev := func(i int, outcome, reason string) DecisionEvent {
		return DecisionEvent{FindingID: ids[i], RecipeID: oldID, Outcome: outcome, Reason: reason, ChunkTextSHA256: sha[i%2]}
	}
	if _, err := d.ApplyDecisions(ctx, oldID, []DecisionEvent{
		ev(0, OutcomeApply, "confident_with_evidence"),
		ev(1, OutcomeHold, "uncertain"),
		ev(2, OutcomeHold, "jev_unavailable"),
	}); err != nil {
		t.Fatal(err)
	}

	// The upgrade: build C registers the same configuration under format 2.
	next := decide
	next.CodeVersion = buildC
	newID, err := d.RegisterRecipe(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if newID == oldID {
		t.Fatal("format 2 produced the format-1 id; the test does not exercise the upgrade")
	}
	// Only the retryable hold and the undecided finding are work.
	if got, want := workIDs(t, d, DecideWorkScope{RecipeID: newID}), sorted(ids[2], ids[3]); !slices.Equal(got, want) {
		t.Errorf("work under the format-2 recipe %v, want %v (the equivalent format-1 verdicts are final)", got, want)
	}
	// stale_work: the decide arm compares content, not ids.
	if err := d.SetCurrentRecipes(ctx, next); err != nil {
		t.Fatal(err)
	}
	for _, row := range staleRows(t, d) {
		t.Errorf("stale after the upgrade: %s", row)
	}

	// A real change (a param bump) is another recipe: everything is work again.
	bumped := next
	bumped.Params = map[string]any{"fn": "should_apply", "rung0_version": "rung0@v3", "apply_p": 0.8}
	bumpedID, err := d.RegisterRecipe(ctx, bumped)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := workIDs(t, d, DecideWorkScope{RecipeID: bumpedID}), sorted(ids[0], ids[1], ids[2], ids[3]); !slices.Equal(got, want) {
		t.Errorf("work under a bumped recipe %v, want %v", got, want)
	}
}

// TestIntegrationScanSkipsFormat1Equivalent: chunks scanned under a format-1
// scan recipe are not candidates for the format-2 recipe of the same
// configuration, and are for a different one.
func TestIntegrationScanSkipsFormat1Equivalent(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	seedChunkScan(t, d)

	old := scanRecipe("p1")
	old.CodeVersion = buildB
	oldID := registerFormat1(t, d, old)
	all, err := d.ScanCandidates(ctx, ScanScope{RecipeID: oldID, ContextSegments: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if _, err := d.InsertChunkScan(ctx, scanRow(c, oldID, 4, 0)); err != nil {
			t.Fatal(err)
		}
	}

	next := old
	next.CodeVersion = buildC
	newID, err := d.RegisterRecipe(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if newID == oldID {
		t.Fatal("format 2 produced the format-1 id")
	}
	if c, err := d.ScanCandidates(ctx, ScanScope{RecipeID: newID}); err != nil || len(c) != 0 {
		t.Errorf("format-2 scan candidates = %d, %v; want 0 (scanned under the equivalent format-1 recipe)", len(c), err)
	}
	other, err := d.RegisterRecipe(ctx, scanRecipe("p2"))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := d.ScanCandidates(ctx, ScanScope{RecipeID: other}); err != nil || len(c) != len(all) {
		t.Errorf("candidates under another prompt = %d, %v; want %d", len(c), err, len(all))
	}
}

func countRowsPool(t *testing.T, d *DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestIntegrationRecipeBuildsBackfill: migration 11 records, for every recipe
// registered before it (format-1 ids, legacy backfill included), the build
// that registered it — so recipe_builds is complete from the start — and
// rewrites no recipe id.
func TestIntegrationRecipeBuildsBackfill(t *testing.T) {
	ctx := context.Background()
	dbURL := newTestDatabase(t)
	if err := migrateTo(itCtx(t), dbURL, testLog(), 10); err != nil {
		t.Fatalf("migrate to 10: %v", err)
	}
	conn := connect(t, dbURL)
	if _, err := conn.Exec(ctx, `
		INSERT INTO recipes (recipe_id, step, step_version, code_version, created_at) VALUES
		  (repeat('a', 64), 'decide', 2, '`+buildA+`', '2026-10-01'),
		  (repeat('b', 64), 'decide', 2, '`+buildB+`', '2026-10-09');
		INSERT INTO current_recipes (step, recipe_id) VALUES ('decide', repeat('b', 64));`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got := queryStrings(t, conn, `
		SELECT b.recipe_id || ' ' || b.code_version || ' ' || to_char(b.first_seen AT TIME ZONE 'UTC', 'YYYY-MM-DD')
		  FROM recipe_builds b ORDER BY 1`)
	want := []string{
		strings.Repeat("a", 64) + " " + buildA + " 2026-10-01",
		strings.Repeat("b", 64) + " " + buildB + " 2026-10-09",
	}
	if !slices.Equal(got, want) {
		t.Errorf("recipe_builds after backfill = %v, want %v", got, want)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM current_recipes WHERE recipe_id = repeat('b', 64)`); n != 1 {
		t.Error("the migration touched current_recipes")
	}
}
