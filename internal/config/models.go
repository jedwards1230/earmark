package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── Model registry (CONTRACT §2.18) ──────────────────────────────────────────
//
// MODELS_FILE names a YAML file pinning, per pipeline step, what the model
// behind that step is expected to be. It does NOT duplicate the AI endpoint
// registry (§2.14): where to send a request, and the model id / LiteLLM alias
// to ask for, still come from AI_ENDPOINTS + AI_ROLES. The registry adds what
// those cannot say — which model is expected to ANSWER for that alias, the
// revision it is pinned to, and the prompt version the step should run — and
// those values go into the step's provenance recipe (§1.9). Upgrading a model
// is a one-line PR to this file.
//
//	steps:
//	  propose:                       # the eval judge (AI_ROLES.eval)
//	    alias: earmark-judge         # optional; must equal the eval endpoint's model
//	    expected_model: anthropic/claude-haiku-4-5-20251001
//	    revision: "20251001"
//	    prompt_version: judge@v1     # optional; checked against the code's prompt
//	  embed:                         # AI_ROLES.embeddings
//	    expected_model: nomic-embed-text
//	    revision: sha256:0a109f422b47
//	  asr:                           # recorded only; the ASR runner owns its recipe
//	    expected_model: nvidia/parakeet-tdt-1.1b
//
// Unset → no pins: recipes are still stamped, with the requested model as the
// expected one and no revision. A file that is unreadable, malformed, names an
// unknown step or field, or whose alias contradicts the endpoint registry is a
// startup error — the same fail-closed posture as AI_ENDPOINTS.

// ModelPin is one step's entry in the model registry. Empty fields are unpinned.
type ModelPin struct {
	// Alias, when set, must equal the model id the step's endpoint is
	// configured with (AI_ENDPOINTS) — an assertion that the two files agree.
	Alias string `yaml:"alias"`
	// ExpectedModel is the model that should answer for the alias: what the
	// endpoint reports in its response "model" field. It is the current
	// recipe's model_resolved, so a fallback answer becomes a different recipe.
	ExpectedModel string `yaml:"expected_model"`
	// Revision pins the weights: HF commit, .nemo sha256, Ollama digest,
	// provider snapshot id.
	Revision string `yaml:"revision"`
	// PromptVersion, when set, is the prompt version this deployment expects
	// the step to run; a mismatch with the code is logged at startup.
	PromptVersion string `yaml:"prompt_version"`
}

// ModelRegistry is the parsed MODELS_FILE.
type ModelRegistry struct {
	Steps map[string]ModelPin `yaml:"steps"`
}

// stepRole maps the steps backed by an AI_ROLES endpoint to that role.
var stepRole = map[string]string{
	recipe.StepPropose: roleEval,
	recipe.StepEmbed:   roleEmbeddings,
}

// ParseModelRegistry decodes and validates a model registry document. Unknown
// fields and unknown steps are errors: a typo must not silently unpin a model.
func ParseModelRegistry(r io.Reader) (*ModelRegistry, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var reg ModelRegistry
	if err := dec.Decode(&reg); err != nil {
		if errors.Is(err, io.EOF) {
			return &ModelRegistry{}, nil
		}
		return nil, fmt.Errorf("decode model registry: %w", err)
	}
	var unknown []string
	for step := range reg.Steps {
		if !recipe.KnownStep(step) {
			unknown = append(unknown, step)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("model registry: unknown step(s) %s (known: %s)",
			strings.Join(unknown, ", "), strings.Join(recipe.Steps, ", "))
	}
	return &reg, nil
}

// loadModelRegistry reads MODELS_FILE into c.Models and checks every pinned
// alias against the endpoint the step's role is bound to. Unset → empty.
func (c *Config) loadModelRegistry() error {
	path := strings.TrimSpace(os.Getenv("MODELS_FILE"))
	if path == "" {
		c.Models = &ModelRegistry{}
		return nil
	}
	// The path is the operator's own MODELS_FILE setting, read once at startup —
	// the same trust level as every other env var; there is no untrusted input.
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- operator-supplied config path
	if err != nil {
		return fmt.Errorf("MODELS_FILE: %w", err)
	}
	reg, err := ParseModelRegistry(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("MODELS_FILE %s: %w", path, err)
	}
	c.Models = reg
	return c.checkModelAliases()
}

// checkModelAliases rejects a pinned alias that contradicts the model the
// step's endpoint is configured to request.
func (c *Config) checkModelAliases() error {
	for step, pin := range c.Models.Steps {
		if pin.Alias == "" {
			continue
		}
		configured, ok := c.stepModel(step)
		if !ok {
			continue // no endpoint for this step here; nothing to contradict
		}
		if configured != pin.Alias {
			return fmt.Errorf("MODELS_FILE: steps.%s.alias is %q but the configured endpoint requests %q",
				step, pin.Alias, configured)
		}
	}
	return nil
}

// stepModel is the model id this deployment requests for a step: the model of
// the endpoint bound to the step's role, or for propose the EVAL_CHAT_MODEL
// fallback (CONTRACT §2.15).
func (c *Config) stepModel(step string) (string, bool) {
	role, ok := stepRole[step]
	if !ok {
		return "", false
	}
	if ep, ok := c.endpointForRole(role); ok {
		return ep.Model, true
	}
	if step == recipe.StepPropose {
		if m := strings.TrimSpace(os.Getenv("EVAL_CHAT_MODEL")); m != "" {
			return m, true
		}
	}
	return "", false
}

// ModelPin returns the registry entry for a step (zero value when unpinned or
// when no registry is loaded).
func (c *Config) ModelPin(step string) ModelPin {
	if c == nil || c.Models == nil {
		return ModelPin{}
	}
	return c.Models.Steps[step]
}
