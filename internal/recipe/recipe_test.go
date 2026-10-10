package recipe

import (
	"strings"
	"testing"
)

// TestCanonicalVector pins the canonical bytes and the ID (format 2) for a
// fixed recipe. Anything computing recipe IDs outside Go must reproduce these
// exact bytes; if this test has to change, so do they — and existing recipes
// would stop matching their configuration, so bump IDFormat and document it
// (CONTRACT §1.9 "Canonical form / ID").
func TestCanonicalVector(t *testing.T) {
	if IDFormat != 2 {
		t.Fatalf("IDFormat = %d: a new format needs a new vector here", IDFormat)
	}
	r := Recipe{
		Step:          StepPropose,
		StepVersion:   1,
		CodeVersion:   "v0.41.0+abc1234",
		ModelAlias:    "earmark-judge",
		ModelResolved: "anthropic/claude-haiku-4-5-20251001",
		PromptVersion: "judge@v1",
		PromptSHA256:  "00ff",
		Params:        map[string]any{"temperature": 0, "min_confidence": 0.6, "a<b": "x&y"},
	}
	got, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"step":"propose","step_version":1,` +
		`"model_alias":"earmark-judge","model_resolved":"anthropic/claude-haiku-4-5-20251001",` +
		`"model_revision":null,"prompt_version":"judge@v1","prompt_sha256":"00ff",` +
		`"params":{"a<b":"x&y","min_confidence":0.6,"temperature":0}}`
	if string(got) != want {
		t.Errorf("canonical\n got %s\nwant %s", got, want)
	}
	id, err := r.ID()
	if err != nil {
		t.Fatal(err)
	}
	// = printf '%s' "$want" | sha256sum
	const wantID = "3b3d62674f5630afa8aa7209bfa32e5052389cc762ff8e3ba1f5655d2493e4ac"
	if id != wantID {
		t.Errorf("id = %s, want %s", id, wantID)
	}
}

// TestCanonicalVectorV1 pins ID format 1 — the format every recipe registered
// before format 2 (and the 00002 legacy backfill) carries. Those IDs are never
// rewritten, so this vector must never change.
func TestCanonicalVectorV1(t *testing.T) {
	r := Recipe{
		Step:          StepPropose,
		StepVersion:   1,
		CodeVersion:   "v0.41.0+abc1234",
		ModelAlias:    "earmark-judge",
		ModelResolved: "anthropic/claude-haiku-4-5-20251001",
		PromptVersion: "judge@v1",
		PromptSHA256:  "00ff",
		Params:        map[string]any{"temperature": 0, "min_confidence": 0.6, "a<b": "x&y"},
	}
	got, err := r.CanonicalV1()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"step":"propose","step_version":1,"code_version":"v0.41.0+abc1234",` +
		`"model_alias":"earmark-judge","model_resolved":"anthropic/claude-haiku-4-5-20251001",` +
		`"model_revision":null,"prompt_version":"judge@v1","prompt_sha256":"00ff",` +
		`"params":{"a<b":"x&y","min_confidence":0.6,"temperature":0}}`
	if string(got) != want {
		t.Errorf("canonical v1\n got %s\nwant %s", got, want)
	}
	id, err := r.IDV1()
	if err != nil {
		t.Fatal(err)
	}
	const wantID = "51274b9640f28301dccaf6fc52f6f9c60c83984ee21f62c00840c1e4007dcff1"
	if id != wantID {
		t.Errorf("v1 id = %s, want %s", id, wantID)
	}
}

// TestLegacyCanonical pins the shape the SQL backfill builds by string
// concatenation (internal/db/migrations/00002_recipes.sql): ID format 1,
// step_version 0, code_version legacy-unknown, empty params.
func TestLegacyCanonical(t *testing.T) {
	r := Recipe{Step: StepPropose, CodeVersion: LegacyCodeVersion, ModelAlias: "gemma3:12b"}
	got, err := r.CanonicalV1()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"step":"propose","step_version":0,"code_version":"legacy-unknown",` +
		`"model_alias":"gemma3:12b","model_resolved":null,"model_revision":null,` +
		`"prompt_version":null,"prompt_sha256":null,"params":{}}`
	if string(got) != want {
		t.Errorf("legacy canonical\n got %s\nwant %s", got, want)
	}
}

// TestIDExcludesBuild: two builds of the same configuration register the same
// recipe. Putting code_version (or any build stamp) back into the hash fails
// this — every release would mint a new recipe for every step.
func TestIDExcludesBuild(t *testing.T) {
	a := Recipe{
		Step: StepDecide, StepVersion: 2, CodeVersion: "0.47.12+f2166fd", ModelAlias: "jev-1.13.0",
		ModelResolved: "jev-1.13.0", PromptVersion: "should_apply@v2", PromptSHA256: "ab",
		Params: map[string]any{"fn": "should_apply", "rung0_version": "rung0@v2"},
	}
	b := a
	b.CodeVersion = "0.48.0+53b7268"
	if mustID(t, a) != mustID(t, b) {
		t.Error("a code-only change (new build) changed the recipe ID")
	}
	ca, _ := a.Canonical()
	if strings.Contains(string(ca), "code_version") || strings.Contains(string(ca), a.CodeVersion) {
		t.Errorf("canonical bytes carry the build: %s", ca)
	}
	// Format 1 did hash it — which is the bug format 2 fixes.
	va, _ := a.IDV1()
	vb, _ := b.IDV1()
	if va == vb {
		t.Error("format 1 ID ignored code_version; the V1 vector is wrong")
	}
}

// TestIDSensitivity: every identifying field changes the ID; nothing else does.
// A step_version, prompt version/hash or param bump (e.g. rung0_version) must
// still mint a new recipe.
func TestIDSensitivity(t *testing.T) {
	base := Recipe{
		Step: StepEmbed, StepVersion: 1, CodeVersion: "v1+a", ModelAlias: "m",
		ModelResolved: "m", ModelRevision: "r", PromptVersion: "p@v1", PromptSHA256: "s",
		Params: map[string]any{"chunk_size": 512},
	}
	baseID := mustID(t, base)

	mutations := map[string]func(*Recipe){
		"step":           func(r *Recipe) { r.Step = StepPropose },
		"step_version":   func(r *Recipe) { r.StepVersion = 2 },
		"model_alias":    func(r *Recipe) { r.ModelAlias = "m2" },
		"model_resolved": func(r *Recipe) { r.ModelResolved = "fallback" },
		"model_revision": func(r *Recipe) { r.ModelRevision = "r2" },
		"prompt_version": func(r *Recipe) { r.PromptVersion = "p@v2" },
		"prompt_sha256":  func(r *Recipe) { r.PromptSHA256 = "t" },
		"params value":   func(r *Recipe) { r.Params = map[string]any{"chunk_size": 1024} },
		"params key":     func(r *Recipe) { r.Params = map[string]any{"chunk_size": 512, "dims": 768} },
		"rung0_version":  func(r *Recipe) { r.Params = map[string]any{"chunk_size": 512, "rung0_version": "rung0@v3"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := base
			r.Params = map[string]any{"chunk_size": 512}
			mutate(&r)
			if mustID(t, r) == baseID {
				t.Errorf("changing %s did not change the recipe ID", name)
			}
		})
	}

	// Not identifying: the build.
	same := base
	same.CodeVersion = "v2+c"
	if mustID(t, same) != baseID {
		t.Error("changing code_version changed the recipe ID")
	}

	// Map construction order must not matter.
	a := Recipe{Step: StepEmbed, CodeVersion: "x", Params: map[string]any{"a": 1, "b": 2, "c": 3}}
	b := Recipe{Step: StepEmbed, CodeVersion: "x", Params: map[string]any{"c": 3, "a": 1, "b": 2}}
	if mustID(t, a) != mustID(t, b) {
		t.Error("params key order changed the ID")
	}
	// nil and empty params are the same recipe.
	c := Recipe{Step: StepEmbed, CodeVersion: "x"}
	d := Recipe{Step: StepEmbed, CodeVersion: "x", Params: map[string]any{}}
	if mustID(t, c) != mustID(t, d) {
		t.Error("nil vs empty params changed the ID")
	}
}

func TestValidate(t *testing.T) {
	if err := (Recipe{Step: "bogus", CodeVersion: "x"}).Validate(); err == nil {
		t.Error("unknown step accepted")
	}
	if err := (Recipe{Step: StepEmbed}).Validate(); err == nil {
		t.Error("missing code_version accepted")
	}
	if err := (Recipe{Step: StepEmbed, CodeVersion: "x"}).Validate(); err != nil {
		t.Errorf("valid recipe rejected: %v", err)
	}
}

func TestPromptSHA256Separates(t *testing.T) {
	if PromptSHA256("ab", "c") == PromptSHA256("a", "bc") {
		t.Error("prompt parts are not separated in the hash")
	}
	if got := PromptSHA256("x"); len(got) != 64 || strings.ToLower(got) != got {
		t.Errorf("PromptSHA256 = %q, want 64 lowercase hex", got)
	}
}

func TestCodeVersion(t *testing.T) {
	if !strings.Contains(CodeVersion(), "+") {
		t.Errorf("CodeVersion() = %q, want <version>+<commit>", CodeVersion())
	}
}

func mustID(t *testing.T, r Recipe) string {
	t.Helper()
	id, err := r.ID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
