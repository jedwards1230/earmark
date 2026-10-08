package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// Postgres proof of the fn_calls cache (CONTRACT §1.9): the partial unique
// index is what ON CONFLICT infers, a second success for a key is dropped,
// errored / fallback / cache-hit rows always insert, and lookup's SQL model
// comparison matches genai.BareModel. Skipped unless EARMARK_TEST_DATABASE_URL
// is set.
func TestIntegrationFnCallsCache(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()

	rid, err := d.RegisterRecipe(ctx, recipe.Recipe{Step: recipe.StepDecide, StepVersion: 1, CodeVersion: "test",
		ModelAlias: "jev-1.13.0", ModelResolved: "typesafe/jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	c := FnCall{
		Fn: "should_apply", PromptVersion: "decide@v1", PromptSHA256: "00ff", ModelAlias: "jev-1.13.0",
		ModelResolved: "typesafe/JEV-1.13.0", RecipeID: rid, InputSHA256: sha64("a"),
		Input: json.RawMessage(`{"x":1}`), Output: json.RawMessage(`{"noul":0.99}`),
		CostUSD: ptr(0.0000164),
	}

	// A fallback row first: stored, but it must not take the cache slot.
	fb := c
	fb.ModelResolved, fb.ErrorClass = "jev-1.12.0", ErrorClassModelFallback
	if _, ok, err := d.InsertFnCall(ctx, fb); err != nil || !ok {
		t.Fatalf("fallback insert = %v, %v", ok, err)
	}
	if _, hit, err := d.LookupFnCache(ctx, c.Key(), ""); err != nil || hit {
		t.Fatalf("a fallback row was served: hit=%v err=%v", hit, err)
	}

	first, ok, err := d.InsertFnCall(ctx, c)
	if err != nil || !ok {
		t.Fatalf("first insert = %v, %v", ok, err)
	}
	if id, ok, err := d.InsertFnCall(ctx, c); err != nil || ok || id != 0 {
		t.Fatalf("duplicate success = %d, %v, %v; want dropped", id, ok, err)
	}

	got, hit, err := d.LookupFnCache(ctx, c.Key(), "")
	if err != nil || !hit || got.ID != first {
		t.Fatalf("lookup = %+v, %v, %v; want row %d", got, hit, err, first)
	}
	if string(got.Output) != `{"noul": 0.99}` && string(got.Output) != `{"noul":0.99}` {
		t.Errorf("output = %s", got.Output)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.0000164 {
		t.Errorf("cost = %v", got.CostUSD)
	}
	// Another expected model never serves this row.
	if _, hit, err := d.LookupFnCache(ctx, c.Key(), "jev-1.14.0"); err != nil || hit {
		t.Fatalf("served for another expected model: hit=%v err=%v", hit, err)
	}

	// Cache hits and errors always insert, even for the same key.
	h := c
	h.CacheHit, h.CachedFrom, h.CostUSD = true, &first, nil
	e := c
	e.ErrorClass, e.Output, e.ModelResolved = "429", nil, ""
	for name, row := range map[string]FnCall{"cache hit": h, "cache hit again": h, "error": e} {
		if _, ok, err := d.InsertFnCall(ctx, row); err != nil || !ok {
			t.Errorf("%s insert = %v, %v", name, ok, err)
		}
	}
	var n int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM fn_calls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("%d fn_calls rows, want 5 (fallback, success, 2 hits, error)", n)
	}
}

func sha64(c string) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c[0]
	}
	return string(b)
}
