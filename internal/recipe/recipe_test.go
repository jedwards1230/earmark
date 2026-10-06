package recipe

import (
	"strings"
	"testing"
)

// TestCanonicalVector pins the canonical bytes and the ID for a fixed recipe.
// Anything computing recipe IDs outside Go (the 00002 legacy backfill in SQL,
// the ASR runner in Python) must reproduce these exact bytes; if this test has
// to change, so do they.
func TestCanonicalVector(t *testing.T) {
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
	const want = `{"step":"propose","step_version":1,"code_version":"v0.41.0+abc1234",` +
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
	const wantID = "51274b9640f28301dccaf6fc52f6f9c60c83984ee21f62c00840c1e4007dcff1"
	if id != wantID {
		t.Errorf("id = %s, want %s", id, wantID)
	}
}

// TestLegacyCanonical pins the shape the SQL backfill builds by string
// concatenation (internal/db/migrations/00002_recipes.sql): step_version 0,
// code_version legacy-unknown, empty params.
func TestLegacyCanonical(t *testing.T) {
	r := Recipe{Step: StepPropose, CodeVersion: LegacyCodeVersion, ModelAlias: "gemma3:12b"}
	got, err := r.Canonical()
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

// TestIDSensitivity: every identifying field changes the ID; nothing else does.
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
		"code_version":   func(r *Recipe) { r.CodeVersion = "v1+b" },
		"model_alias":    func(r *Recipe) { r.ModelAlias = "m2" },
		"model_resolved": func(r *Recipe) { r.ModelResolved = "fallback" },
		"model_revision": func(r *Recipe) { r.ModelRevision = "r2" },
		"prompt_version": func(r *Recipe) { r.PromptVersion = "p@v2" },
		"prompt_sha256":  func(r *Recipe) { r.PromptSHA256 = "t" },
		"params value":   func(r *Recipe) { r.Params = map[string]any{"chunk_size": 1024} },
		"params key":     func(r *Recipe) { r.Params = map[string]any{"chunk_size": 512, "dims": 768} },
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
