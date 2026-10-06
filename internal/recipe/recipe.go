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
	// CodeVersion is the earmark build (tag+commit) or, for asr, the runner tag.
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

// canonical is the wire shape hashed into the ID. Field order is fixed by the
// struct; nil pointers encode as null.
type canonical struct {
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
	return marshalCanonical(r.Params)
}

// Canonical returns the bytes the ID hashes:
//
//	{"step":…,"step_version":…,"code_version":…,"model_alias":…,
//	 "model_resolved":…,"model_revision":…,"prompt_version":…,
//	 "prompt_sha256":…,"params":{…}}
//
// in exactly that key order, with no whitespace, empty strings as null, and
// params canonicalized by ParamsJSON. Anything that computes a recipe ID
// outside Go (the 00002 migration's legacy backfill, the ASR runner) must
// produce these exact bytes. TestCanonicalVector pins an example.
func (r Recipe) Canonical() ([]byte, error) {
	params, err := r.ParamsJSON()
	if err != nil {
		return nil, fmt.Errorf("recipe params: %w", err)
	}
	return marshalCanonical(canonical{
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

// ID is the recipe's content address: lowercase hex SHA-256 of Canonical.
func (r Recipe) ID() (string, error) {
	b, err := r.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
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

// marshalCanonical is json.Marshal without HTML escaping and without the
// encoder's trailing newline. encoding/json already sorts map keys.
func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// CodeVersion is this earmark build as recorded in recipes: "<version>+<commit>"
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
