package fn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// secret is book content: it must reach the fn_calls row and nothing else.
const secret = "SECRET-BOOK-TEXT-the-ship-sailed-at-dawn"

func testFn() Fn {
	return Fn{
		Name: "should_apply", Step: recipe.StepDecide, StepVersion: 1,
		PromptVersion: "decide@v1", PromptSHA256: recipe.PromptSHA256("q"),
		ModelAlias: "jev-1.13.0", Params: map[string]any{"threshold": 0.9},
	}
}

// TestCanonicalInputVectors pins the input hash: keys sorted at every level
// (struct fields too), no whitespace, no HTML escaping, numbers verbatim. The
// hex values are sha256 of the shown bytes, computed outside Go.
func TestCanonicalInputVectors(t *testing.T) {
	type pair struct {
		B string `json:"b"`
		A int    `json:"a"`
	}
	type finding struct {
		Suggested string `json:"suggested"`
		Original  string `json:"original"`
		Chunk     string `json:"chunk"`
	}
	tests := []struct {
		name  string
		input any
		canon string
		sum   string
	}{
		{"struct fields are sorted, html kept", pair{B: "x<y", A: 1},
			`{"a":1,"b":"x<y"}`, "7e897661c014a2799b01d100cf3db3088dbc1c2c0ba3c83a63dd58fd41634fcc"},
		{"map", map[string]any{"b": "x<y", "a": 1},
			`{"a":1,"b":"x<y"}`, "7e897661c014a2799b01d100cf3db3088dbc1c2c0ba3c83a63dd58fd41634fcc"},
		{"raw json is re-canonicalized", json.RawMessage(`{ "b" : "x<y", "a" : 1 }`),
			`{"a":1,"b":"x<y"}`, "7e897661c014a2799b01d100cf3db3088dbc1c2c0ba3c83a63dd58fd41634fcc"},
		{"nested struct", finding{Suggested: "box", Original: "fox", Chunk: "the quick brown fox"},
			`{"chunk":"the quick brown fox","original":"fox","suggested":"box"}`,
			"a7a3b2a52232325803bee87246c44ee389db56dd55a0894ee8621a922f9f27eb"},
		{"number literals kept", json.RawMessage(`[1.50, {"z": null, "k": true}]`),
			`[1.50,{"k":true,"z":null}]`, "a5bd873961d687c560f1b3e91ce770f47b61d0f7efa81b943a285b5df40f8359"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canon, sum, err := CanonicalInput(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if string(canon) != tt.canon || sum != tt.sum {
				t.Errorf("CanonicalInput = %s %s, want %s %s", canon, sum, tt.canon, tt.sum)
			}
		})
	}
	if _, _, err := CanonicalInput(func() {}); err == nil {
		t.Error("an unmarshalable input was accepted")
	}
}

func TestValidateRefusesUnpinnedModels(t *testing.T) {
	for _, alias := range []string{"jev-latest", "jev-preview", "JEV-LATEST", "jev-2-preview", "jev", "", "qwen:latest"} {
		f := testFn()
		f.ModelAlias = alias
		if _, err := New(f); err == nil {
			t.Errorf("alias %q accepted", alias)
		}
	}
	f := testFn()
	f.ExpectedModel = "typesafe/jev-latest"
	if err := f.Validate(); err == nil {
		t.Error("unpinned expected model accepted")
	}
	for name, mutate := range map[string]func(*Fn){
		"no name":           func(f *Fn) { f.Name = "" },
		"unknown step":      func(f *Fn) { f.Step = "ponder" },
		"no prompt version": func(f *Fn) { f.PromptVersion = "" },
		"no prompt hash":    func(f *Fn) { f.PromptSHA256 = "" },
	} {
		f := testFn()
		mutate(&f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(testFn()); err != nil {
		t.Errorf("pinned function refused: %v", err)
	}
}

func TestRecipe(t *testing.T) {
	f := testFn()
	r := f.Recipe("")
	if r.Step != recipe.StepDecide || r.StepVersion != 1 || r.ModelAlias != "jev-1.13.0" ||
		r.ModelResolved != "jev-1.13.0" || r.PromptVersion != "decide@v1" || r.PromptSHA256 != f.PromptSHA256 {
		t.Errorf("recipe = %+v", r)
	}
	if r.Params["fn"] != "should_apply" || r.Params["threshold"] != 0.9 {
		t.Errorf("params = %v", r.Params)
	}
	if _, ok := f.Params["fn"]; ok {
		t.Error("Recipe mutated the function's params")
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
	other := f
	other.Name = "is_name"
	a, _ := f.Recipe("jev-1.13.0").ID()
	b, _ := other.Recipe("jev-1.13.0").ID()
	fb, _ := f.Recipe("jev-1.12.0").ID()
	if a == b || a == fb {
		t.Error("function name / resolved model do not change the recipe")
	}

	// A route prefix or case on the expected model is the same model: one
	// recipe, the current one.
	for _, spelled := range []string{"typesafe/jev-1.13.0", " JEV-1.13.0 ", "litellm/typesafe/jev-1.13.0"} {
		r := f.Recipe(spelled)
		if id, _ := r.ID(); id != a || r.ModelResolved != "jev-1.13.0" {
			t.Errorf("Recipe(%q) = %s / %q, want the expected model's recipe %s", spelled, id, r.ModelResolved, a)
		}
	}
}

// ─── Invoke ──────────────────────────────────────────────────────────────────

// fakeStore is an in-memory Store with the fn_calls cache semantics (the SQL
// itself is pinned by internal/db's pgxmock and Postgres tests).
type fakeStore struct {
	mu        sync.Mutex
	rows      []db.FnCall
	recipes   map[string]recipe.Recipe
	lookupErr error
	insertErr error
}

func (s *fakeStore) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	id, err := r.ID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recipes == nil {
		s.recipes = map[string]recipe.Recipe{}
	}
	s.recipes[id] = r
	return id, nil
}

func cacheable(c db.FnCall) bool { return c.ErrorClass == "" && !c.CacheHit }

func (s *fakeStore) LookupFnCache(_ context.Context, k db.FnCacheKey, expected string) (*db.FnCall, bool, error) {
	if s.lookupErr != nil {
		return nil, false, s.lookupErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.Key() == k && cacheable(r) && strings.EqualFold(r.ModelResolved, expected) {
			c := r
			return &c, true, nil
		}
	}
	return nil, false, nil
}

func (s *fakeStore) InsertFnCall(_ context.Context, c db.FnCall) (int64, bool, error) {
	if s.insertErr != nil {
		return 0, false, s.insertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cacheable(c) {
		for _, r := range s.rows {
			if r.Key() == c.Key() && cacheable(r) {
				return 0, false, nil
			}
		}
	}
	c.ID = int64(len(s.rows) + 1)
	s.rows = append(s.rows, c)
	return c.ID, true, nil
}

func installTestProviders(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prevT, prevM := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevT)
		otel.SetMeterProvider(prevM)
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return spans, reader
}

// modelCalls returns earmark_model_calls points as "fn|model|outcome" → n.
func modelCalls(t *testing.T, r *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "earmark_model_calls" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				fn, _ := dp.Attributes.Value("fn")
				model, _ := dp.Attributes.Value("model")
				outcome, _ := dp.Attributes.Value("outcome")
				out[fn.AsString()+"|"+model.AsString()+"|"+outcome.AsString()] += dp.Value
			}
		}
	}
	return out
}

func spanAttrs(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes {
		out[kv.Key] = kv.Value
	}
	return out
}

// assertNoContent: no span name, attribute, event or status of any span
// carries the secret.
func assertNoContent(t *testing.T, spans []tracetest.SpanStub) {
	t.Helper()
	for _, s := range spans {
		texts := []string{s.Name, s.Status.Description}
		for _, kv := range s.Attributes {
			texts = append(texts, string(kv.Key), kv.Value.String())
		}
		for _, ev := range s.Events {
			texts = append(texts, ev.Name)
			for _, kv := range ev.Attributes {
				texts = append(texts, kv.Value.String())
			}
		}
		for _, x := range texts {
			if strings.Contains(x, "SECRET") {
				t.Errorf("span %q carries content: %q", s.Name, x)
			}
		}
	}
}

func input() map[string]string { return map[string]string{"state": secret} }

func okCall(model string) (Call, *int) {
	n := 0
	return func(context.Context) (Result, error) {
		n++
		in, out := 391, 67
		cost := 0.0000164
		return Result{Output: json.RawMessage(`{"noul":0.99,"note":"` + secret + `"}`), Model: model,
			InputTokens: &in, OutputTokens: &out, CostUSD: &cost}, nil
	}, &n
}

func TestInvokeMissThenHit(t *testing.T) {
	spans, reader := installTestProviders(t)
	store := &fakeStore{}
	f := testFn()
	call, calls := okCall("jev-1.13.0")
	ctx := context.Background()

	res, meta, err := f.Invoke(ctx, store, input(), call)
	if err != nil {
		t.Fatalf("miss: %v", err)
	}
	if *calls != 1 || meta.CacheHit || meta.Fallback || meta.CallID != 1 || meta.RecipeID == "" {
		t.Fatalf("miss meta = %+v, calls %d", meta, *calls)
	}
	if !strings.Contains(string(res.Output), secret) {
		t.Errorf("output = %s", res.Output)
	}
	miss := store.rows[0]
	wantRecipe, _ := f.Recipe("jev-1.13.0").ID()
	if miss.RecipeID != wantRecipe || miss.ModelResolved != "jev-1.13.0" || *miss.InputTokens != 391 ||
		*miss.CostUSD != 0.0000164 || miss.LatencyMS == nil || !strings.Contains(string(miss.Input), secret) {
		t.Errorf("miss row = %+v", miss)
	}
	if _, ok := store.recipes[wantRecipe]; !ok {
		t.Error("recipe not registered before the row")
	}

	res2, meta2, err := f.Invoke(ctx, store, input(), call)
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if *calls != 1 {
		t.Error("a cache hit called the model")
	}
	if !meta2.CacheHit || meta2.CachedFrom != 1 || meta2.CallID != 2 || meta2.RecipeID != wantRecipe {
		t.Errorf("hit meta = %+v", meta2)
	}
	if string(res2.Output) != string(res.Output) || res2.Model != "jev-1.13.0" {
		t.Errorf("hit result = %+v", res2)
	}
	hit := store.rows[1]
	if !hit.CacheHit || hit.CachedFrom == nil || *hit.CachedFrom != 1 || hit.CostUSD != nil || hit.InputTokens != nil {
		t.Errorf("hit row = %+v", hit)
	}

	got := spans.GetSpans()
	if len(got) != 2 {
		t.Fatalf("%d spans, want 2", len(got))
	}
	for i, s := range got {
		if s.Name != "systemone jev-1.13.0" {
			t.Errorf("span name %q", s.Name)
		}
		a := spanAttrs(s)
		for k, v := range map[string]any{
			"gen_ai.operation.name": "systemone",
			"gen_ai.provider.name":  "typesafe",
			"gen_ai.request.model":  "jev-1.13.0",
			"gen_ai.response.model": "jev-1.13.0",
			"earmark.fn":            "should_apply",
			"earmark.step":          "decide",
			"earmark.recipe_id":     wantRecipe,
			"earmark.cache_hit":     i == 1,
		} {
			if fmt.Sprint(a[attribute.Key(k)].AsInterface()) != fmt.Sprint(v) {
				t.Errorf("span %d %s = %v, want %v", i, k, a[attribute.Key(k)].AsInterface(), v)
			}
		}
	}
	if a := spanAttrs(got[0]); a["gen_ai.usage.input_tokens"].AsInt64() != 391 || a["gen_ai.usage.output_tokens"].AsInt64() != 67 {
		t.Errorf("usage attributes = %v", a)
	}
	assertNoContent(t, got)

	want := map[string]int64{"should_apply|jev-1.13.0|ok": 1, "should_apply|jev-1.13.0|cached": 1}
	if m := modelCalls(t, reader); fmt.Sprint(m) != fmt.Sprint(want) {
		t.Errorf("model calls = %v, want %v", m, want)
	}
}

func TestInvokeFallbackIsStoredButNeverServed(t *testing.T) {
	_, reader := installTestProviders(t)
	store := &fakeStore{}
	f := testFn()
	fallback, n := okCall("jev-1.12.0")
	_, meta, err := f.Invoke(context.Background(), store, input(), fallback)
	if err != nil || !meta.Fallback {
		t.Fatalf("fallback = %+v, %v", meta, err)
	}
	if r := store.rows[0]; r.ErrorClass != db.ErrorClassModelFallback || r.Output == nil || r.ModelResolved != "jev-1.12.0" {
		t.Errorf("fallback row = %+v", r)
	}
	fbRecipe, _ := f.Recipe("jev-1.12.0").ID()
	if meta.RecipeID != fbRecipe {
		t.Error("fallback not stamped with its own recipe")
	}

	// The next call is not served from the fallback and its answer is cached.
	right, m := okCall("typesafe/jev-1.13.0")
	_, meta2, err := f.Invoke(context.Background(), store, input(), right)
	if err != nil || meta2.CacheHit || meta2.Fallback || *m != 1 || *n != 1 || meta2.CallID == 0 {
		t.Fatalf("after fallback = %+v, %v (fallback calls %d, right calls %d)", meta2, err, *n, *m)
	}
	if wantRID, _ := f.Recipe("").ID(); meta2.RecipeID != wantRID {
		t.Errorf("a route-prefixed reply got recipe %s, want the expected model's %s", meta2.RecipeID, wantRID)
	}
	if got := store.rows[len(store.rows)-1].ModelResolved; got != "typesafe/jev-1.13.0" {
		t.Errorf("fn_calls.model_resolved = %q; the reply's spelling must be kept", got)
	}
	want := map[string]int64{"should_apply|jev-1.13.0|fallback": 1, "should_apply|jev-1.13.0|ok": 1}
	if got := modelCalls(t, reader); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("model calls = %v, want %v", got, want)
	}
}

func TestInvokeErrorIsLoggedNeverCached(t *testing.T) {
	spans, reader := installTestProviders(t)
	store := &fakeStore{}
	f := testFn()
	calls := 0
	fail := func(context.Context) (Result, error) {
		calls++
		// An error whose text echoes the input must not reach the span.
		return Result{}, fmt.Errorf("upstream said %q: %w", secret, &systemone.RateLimitError{})
	}
	for range 2 {
		_, meta, err := f.Invoke(context.Background(), store, input(), fail)
		var rl *systemone.RateLimitError
		if !errors.As(err, &rl) || meta.CallID == 0 {
			t.Fatalf("err = %v, meta %+v", err, meta)
		}
	}
	if calls != 2 {
		t.Errorf("an error was served from cache (%d calls)", calls)
	}
	for _, r := range store.rows {
		if r.ErrorClass != "429" || r.Output != nil || r.RecipeID != "" {
			t.Errorf("error row = %+v", r)
		}
	}
	got := spans.GetSpans()
	if len(got) != 2 || got[0].Status.Code != codes.Error || spanAttrs(got[0])["error.type"].AsString() != "429" {
		t.Errorf("error span = %+v", got)
	}
	assertNoContent(t, got)
	if m := modelCalls(t, reader); m["should_apply|jev-1.13.0|error"] != 2 {
		t.Errorf("model calls = %v", m)
	}
}

func TestInvokeRejectsBadResultsAndStoreErrors(t *testing.T) {
	installTestProviders(t)
	f := testFn()
	for name, res := range map[string]Result{
		"no model":     {Output: json.RawMessage(`{}`)},
		"no output":    {Model: "jev-1.13.0"},
		"invalid json": {Model: "jev-1.13.0", Output: json.RawMessage(`{`)},
	} {
		store := &fakeStore{}
		_, _, err := f.Invoke(context.Background(), store, input(), func(context.Context) (Result, error) { return res, nil })
		if err == nil || len(store.rows) != 1 || store.rows[0].ErrorClass != "invalid_reply" {
			t.Errorf("%s: err %v rows %+v", name, err, store.rows)
		}
	}

	boom := errors.New("db down")
	call, n := okCall("jev-1.13.0")
	if _, _, err := f.Invoke(context.Background(), &fakeStore{lookupErr: boom}, input(), call); !errors.Is(err, boom) || *n != 0 {
		t.Errorf("lookup error = %v (calls %d)", err, *n)
	}
	if _, _, err := f.Invoke(context.Background(), &fakeStore{insertErr: boom}, input(), call); !errors.Is(err, boom) {
		t.Errorf("insert error = %v", err)
	}
	bad := f
	bad.ModelAlias = "jev-latest"
	if _, _, err := bad.Invoke(context.Background(), &fakeStore{}, input(), call); err == nil {
		t.Error("Invoke ran an unpinned function")
	}
}
