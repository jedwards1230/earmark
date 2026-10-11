package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
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
//	    prompt_version: judge@v2     # optional; checked against the code's prompt
//	  embed:                         # AI_ROLES.embeddings
//	    expected_model: nomic-embed-text
//	    revision: sha256:0a109f422b47
//	  asr:                           # recorded only; the ASR runner owns its recipe
//	    expected_model: nvidia/parakeet-tdt-1.1b
//	  decide:                        # AI_ROLES.decide (a systemone endpoint)
//	    alias: jev-1.13.0
//	    params:
//	      usd_per_mtok_in: 0.042     # cost estimate when the gateway reports none
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
	// Params are step-specific settings. ParseModelRegistry accepts only
	// known keys (knownPinParams): usd_per_mtok_in (decide, scan), a
	// non-negative number, and on the propose step the judge thinking params
	// reasoning_effort / chat_template_kwargs.
	Params map[string]any `yaml:"params"`
}

// ParamUSDPerMTokIn is the per-million-input-token price used to estimate a
// System One call's cost when the gateway reports none (CONTRACT §2.18).
const ParamUSDPerMTokIn = "usd_per_mtok_in"

// FloatParam returns a numeric param; ok=false when absent.
func (p ModelPin) FloatParam(key string) (float64, bool) {
	v, ok := p.Params[key]
	if !ok {
		return 0, false
	}
	f, ok := toFloat(v)
	return f, ok
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// ModelRegistry is the parsed MODELS_FILE.
type ModelRegistry struct {
	Steps map[string]ModelPin `yaml:"steps"`
}

// stepRole maps the steps backed by an AI_ROLES endpoint to that role.
var stepRole = map[string]string{
	recipe.StepPropose: roleEval,
	recipe.StepEmbed:   roleEmbeddings,
	recipe.StepDecide:  roleDecide,
	recipe.StepScan:    roleScan,
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
	for step, pin := range reg.Steps {
		if err := validatePinParams(step, pin.Params); err != nil {
			return nil, fmt.Errorf("model registry: steps.%s.params%w", step, err)
		}
	}
	return &reg, nil
}

// pinParamError is a params validation failure. Its message starts with the
// key path suffix (": unknown key …" or ".<key> must be …") so callers can
// prefix the location ("steps.decide.params", "--param").
type pinParamError struct{ msg string }

func (e *pinParamError) Error() string { return e.msg }

// Judge thinking params (CONTRACT §2.15 "Thinking controls"): the propose
// step's per-model reasoning knobs, settable on the propose step only (a
// model_settings row's params, or MODELS_FILE steps.propose.params). They
// outrank the deployment-wide EVAL_REASONING_EFFORT / EVAL_CHAT_TEMPLATE_KWARGS,
// so the knobs travel with the model they were tuned for.
const (
	// ParamReasoningEffort is the judge request's reasoning_effort: "auto"
	// (the route default), "omit" (never sent), or any other token sent
	// verbatim ("none", "low", "medium", "high", …). A string.
	ParamReasoningEffort = "reasoning_effort"
	// ParamChatTemplateKwargs is the judge request's chat_template_kwargs:
	// "auto", "omit", or a JSON object (an object value, or its JSON text).
	ParamChatTemplateKwargs = "chat_template_kwargs"
)

// knownPinParams are the param keys a step accepts: usd_per_mtok_in on any
// step, plus the step's own keys.
func knownPinParams(step string) []string {
	if step == recipe.StepPropose {
		return []string{ParamUSDPerMTokIn, ParamReasoningEffort, ParamChatTemplateKwargs}
	}
	return []string{ParamUSDPerMTokIn}
}

// validatePinParams accepts only the step's known pin params (knownPinParams)
// with valid values. Shared by MODELS_FILE and the runtime model settings
// (CONTRACT §2.20) so both refuse the same input.
func validatePinParams(step string, params map[string]any) error {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	known := knownPinParams(step)
	for _, k := range keys {
		v := params[k]
		if !slices.Contains(known, k) {
			// A misspelt key must not silently fall back to a default.
			return &pinParamError{fmt.Sprintf(": unknown key %q (known for %s: %s)", k, step, strings.Join(known, ", "))}
		}
		switch k {
		case ParamUSDPerMTokIn:
			if f, ok := toFloat(v); !ok || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
				return &pinParamError{fmt.Sprintf(".%s must be a non-negative number, got %v", k, v)}
			}
		case ParamReasoningEffort:
			if _, err := ReasoningEffortParam(v); err != nil {
				return &pinParamError{fmt.Sprintf(".%s %v", k, err)}
			}
		case ParamChatTemplateKwargs:
			if _, err := ChatTemplateKwargsParam(v); err != nil {
				return &pinParamError{fmt.Sprintf(".%s %v", k, err)}
			}
		}
	}
	return nil
}

// ReasoningEffortParam reads a reasoning_effort param: a non-empty string
// token with no whitespace ("auto", "omit", "none", "low", …), trimmed.
// Numbers and other types are refused: a typo must not turn into a request
// field the provider rejects at run time.
func ReasoningEffortParam(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("must be a string (auto, omit, none, low, medium, high, …), got %v", v)
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("must be a non-empty token without whitespace, got %q", v)
	}
	return s, nil
}

// ChatTemplateKwargsParam reads a chat_template_kwargs param and returns it
// as text: "auto" or "omit" (lower-cased), or the JSON of an object. It
// accepts an object (YAML / a JSON params document) or a string holding
// "auto", "omit" or a JSON object (a --param flag).
func ChatTemplateKwargsParam(v any) (string, error) {
	// An empty object is refused: the judge request drops an empty
	// chat_template_kwargs (omitempty), so it could never take effect.
	const want = "must be \"auto\", \"omit\" or a non-empty JSON object"
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return "", fmt.Errorf("%s (use omit), got {}", want)
		}
		b, err := json.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("must be a JSON object: %w", err)
		}
		return string(b), nil
	case string:
		s := strings.TrimSpace(t)
		if l := strings.ToLower(s); l == "auto" || l == "omit" {
			return l, nil
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err != nil || len(obj) == 0 {
			return "", fmt.Errorf("%s, got %q", want, t)
		}
		return s, nil
	}
	return "", fmt.Errorf("%s, got %v", want, v)
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
