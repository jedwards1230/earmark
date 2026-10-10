// Package recipe defines provenance recipes (CONTRACT §1.9): an immutable,
// content-addressed record of exactly how an output row was made — which step,
// which earmark logic version, which model was asked for and which answered,
// the model revision, the prompt version and hash, and the parameters.
//
// A recipe's ID is the SHA-256 of its canonical JSON (see Canonical), so the
// same configuration always yields the same ID and any change to an identifying
// field yields a new one. Output rows (transcripts, transcript_findings,
// transcript_chunks) carry the ID of the recipe that produced them.
//
// The ID hashes only what shapes the output — step, step_version, the three
// model fields, the prompt version and hash, and params. It does NOT hash the
// build (CodeVersion): a release that changes nothing about a step keeps its
// recipe. That is ID format 2 (IDFormat); format 1 (IDV1) also hashed
// code_version and is kept only because existing recipe ids, including the
// 00002 legacy backfill, were computed with it and stay valid forever.
//
// Leaf package: no database or HTTP dependencies.
package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jedwards1230/earmark/internal/version"
)

// Steps, in pipeline order: asr → propose → decide → propagate → format →
// embed, plus scan. The recipes.step CHECK constraint enumerates the same set.
const (
	StepASR       = "asr"
	StepPropose   = "propose"
	StepDecide    = "decide"
	StepPropagate = "propagate"
	StepScan      = "scan"
	StepFormat    = "format"
	StepEmbed     = "embed"
)

// Steps is every known step.
var Steps = []string{StepASR, StepPropose, StepDecide, StepPropagate, StepScan, StepFormat, StepEmbed}

// KnownStep reports whether s is a known step.
func KnownStep(s string) bool {
	for _, k := range Steps {
		if k == s {
			return true
		}
	}
	return false
}

// LegacyCodeVersion marks the recipes the 00002 migration backfilled for rows
// written before recipes existed. Those rows are told apart only by model name:
// no prompt or model-file hash was recorded, so none is invented.
const LegacyCodeVersion = "legacy-unknown"

// Recipe is one provenance record. Empty strings mean "unknown / not
// applicable" and are stored as NULL.
type Recipe struct {
	// Step is one of the Step* constants.
	Step string
	// StepVersion is bumped whenever earmark's own logic for the step changes
	// its output. 0 is reserved for legacy recipes.
	StepVersion int
	// CodeVersion is the earmark build (tag+commit) or, for asr, the runner
	// tag. It is recorded, not identifying: it is NOT part of the ID, so the
	// recipes row keeps the build that first registered the recipe and
	// recipe_builds every build that registered it since (CONTRACT §1.9).
	CodeVersion string
	// ModelAlias is what was asked for (the model id sent to the endpoint,
	// e.g. a LiteLLM alias).
	ModelAlias string
	// ModelResolved is what answered: the model the endpoint reported serving
	// the request. A fallback answer is therefore a different recipe.
	ModelResolved string
	// ModelRevision pins the weights: HF commit, .nemo sha256, Ollama digest
	// or provider snapshot id.
	ModelRevision string
	// PromptVersion names the prompt template ("judge@v1"); PromptSHA256 is
	// the hash of the exact template bytes, so an unversioned edit still
	// changes the recipe.
	PromptVersion string
	PromptSHA256  string
	// Params holds everything else that shapes the output: temperature,
	// thresholds, chunk size, dimensions, prefixes. JSON-encodable values only.
	Params map[string]any
}

// IDFormat is the recipe ID format Canonical/ID produce (CONTRACT §1.9
// "Canonical form / ID"). Format 1 hashed code_version too (IDV1); format 2
// dropped it so a release that changes nothing does not mint new recipes.
const IDFormat = 2

// canonical is the wire shape hashed into the ID. Field order is fixed by the
// struct; nil pointers encode as null. It deliberately has no code_version:
// nothing build-varying may enter the ID.
type canonical struct {
	Step          string          `json:"step"`
	StepVersion   int             `json:"step_version"`
	ModelAlias    *string         `json:"model_alias"`
	ModelResolved *string         `json:"model_resolved"`
	ModelRevision *string         `json:"model_revision"`
	PromptVersion *string         `json:"prompt_version"`
	PromptSHA256  *string         `json:"prompt_sha256"`
	Params        json.RawMessage `json:"params"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ParamsJSON returns the params as canonical JSON: object keys sorted, no
// insignificant whitespace, no HTML escaping, and "{}" for none.
func (r Recipe) ParamsJSON() ([]byte, error) {
	if len(r.Params) == 0 {
		return []byte("{}"), nil
	}
	return MarshalCanonical(r.Params)
}

// Canonical returns the bytes the ID hashes (ID format 2):
//
//	{"step":…,"step_version":…,"model_alias":…,"model_resolved":…,
//	 "model_revision":…,"prompt_version":…,"prompt_sha256":…,"params":{…}}
//
// in exactly that key order, with no whitespace, empty strings as null, and
// params canonicalized by ParamsJSON. CodeVersion is not part of it. Anything
// that computes a recipe ID outside Go must produce these exact bytes.
// TestCanonicalVector pins an example.
func (r Recipe) Canonical() ([]byte, error) {
	params, err := r.ParamsJSON()
	if err != nil {
		return nil, fmt.Errorf("recipe params: %w", err)
	}
	return MarshalCanonical(canonical{
		Step:          r.Step,
		StepVersion:   r.StepVersion,
		ModelAlias:    nullable(r.ModelAlias),
		ModelResolved: nullable(r.ModelResolved),
		ModelRevision: nullable(r.ModelRevision),
		PromptVersion: nullable(r.PromptVersion),
		PromptSHA256:  nullable(r.PromptSHA256),
		Params:        params,
	})
}

// ID is the recipe's content address: lowercase hex SHA-256 of Canonical.
func (r Recipe) ID() (string, error) {
	b, err := r.Canonical()
	if err != nil {
		return "", err
	}
	return hashHex(b), nil
}

// canonicalV1 is ID format 1's wire shape: format 2 plus code_version after
// step_version.
type canonicalV1 struct {
	Step          string          `json:"step"`
	StepVersion   int             `json:"step_version"`
	CodeVersion   *string         `json:"code_version"`
	ModelAlias    *string         `json:"model_alias"`
	ModelResolved *string         `json:"model_resolved"`
	ModelRevision *string         `json:"model_revision"`
	PromptVersion *string         `json:"prompt_version"`
	PromptSHA256  *string         `json:"prompt_sha256"`
	Params        json.RawMessage `json:"params"`
}

// CanonicalV1 returns ID format 1's bytes — Canonical with "code_version"
// after "step_version". Every recipe registered before format 2, and every
// legacy recipe the 00002 migration backfills (by string concatenation in
// SQL), has a format-1 ID. Those IDs are never rewritten; this exists so they
// can still be recomputed and checked. Nothing registers new format-1 IDs.
func (r Recipe) CanonicalV1() ([]byte, error) {
	params, err := r.ParamsJSON()
	if err != nil {
		return nil, fmt.Errorf("recipe params: %w", err)
	}
	return MarshalCanonical(canonicalV1{
		Step:          r.Step,
		StepVersion:   r.StepVersion,
		CodeVersion:   nullable(r.CodeVersion),
		ModelAlias:    nullable(r.ModelAlias),
		ModelResolved: nullable(r.ModelResolved),
		ModelRevision: nullable(r.ModelRevision),
		PromptVersion: nullable(r.PromptVersion),
		PromptSHA256:  nullable(r.PromptSHA256),
		Params:        params,
	})
}

// IDV1 is the format-1 ID: lowercase hex SHA-256 of CanonicalV1.
func (r Recipe) IDV1() (string, error) {
	b, err := r.CanonicalV1()
	if err != nil {
		return "", err
	}
	return hashHex(b), nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks the fields the database constrains.
func (r Recipe) Validate() error {
	if !KnownStep(r.Step) {
		return fmt.Errorf("recipe: unknown step %q", r.Step)
	}
	if r.CodeVersion == "" {
		return fmt.Errorf("recipe %s: code_version is required", r.Step)
	}
	return nil
}

// MarshalCanonical is json.Marshal without HTML escaping and without the
// encoder's trailing newline. encoding/json already sorts map keys; struct
// fields keep their declaration order. It is the marshaller behind recipe IDs
// and, via internal/fn, pure-function input hashes.
func MarshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// CodeVersion is this earmark build as recorded in recipes.code_version (first
// seen) and recipe_builds: "<version>+<commit>"
// (e.g. "v0.41.0+abc1234"; "dev+unknown" for an unstamped local build).
func CodeVersion() string {
	return version.Version + "+" + version.Commit
}

// PromptSHA256 hashes prompt template parts into a recipe's prompt_sha256.
// Parts are joined with a NUL separator so ("ab","c") and ("a","bc") differ.
func PromptSHA256(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
