package decide

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── Per-model decide policy (CONTRACT §2.19 "Decide policy") ────────────────
//
// The decide policy is the apply/reject rule should_apply's probability feeds:
// apply_p, reject_p, apply_evidence, reject_requires_lowercase and
// reject_floor. Models calibrate differently, so each decide model has its own
// policy. It is resolved per key, each layer overriding the one below:
//
//	per-run --policy key=value            (source override)
//	the decide step's pin params          (source db: a model_settings row;
//	                                       source env: MODELS_FILE steps.decide.params)
//	the built-in preset for the model id  (source preset: jev-*, clef*)
//	DefaultShouldApplyParams              (source default)
//
// The policy is part of the decide recipe's params, so changing it is a new
// recipe; the fn_calls cache is keyed by model and input, not params, so a new
// policy re-decides from cached answers without paying for them again.

// Where an effective policy came from (Policy.Source): the highest layer that
// set any key.
const (
	PolicySourceOverride = config.SourceOverride
	PolicySourceDB       = config.SourceDB
	PolicySourceEnv      = config.SourceEnv
	PolicySourcePreset   = "preset"
	PolicySourceDefault  = "default"
)

// Built-in preset names.
const (
	PresetJev  = "jev"
	PresetClef = "clef"
)

// PresetFor returns the built-in policy for a decide model id, matched on the
// id with any provider prefix dropped ("typesafe/jev-1.13.0" is jev-1.13.0):
//
//	jev-*  → jev:  the default policy. Measured on 2,000 held findings
//	         (446 human-labelled): apply at p ≥ 0.85 with either evidence
//	         kind was 27 applies at 100% precision; its rejects are kept at
//	         p ≤ 0.20 (it never scores lower usefully).
//	clef*  → clef: apply at p ≥ 0.85 with asin_verbatim only (26/26 good;
//	         it over-scores intentional repetition, so exact_repeat is
//	         excluded); reject at p ≤ 0.20 only a lowercase replacement or
//	         p < 0.10 (55 such rejects, 96% junk) — a capitalised low-p
//	         replacement holds.
//
// ok=false for any other model.
func PresetFor(model string) (name string, p ShouldApplyParams, ok bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	switch {
	case strings.HasPrefix(id, "jev-"):
		return PresetJev, DefaultShouldApplyParams(), true
	case strings.HasPrefix(id, "clef"):
		p := DefaultShouldApplyParams()
		p.ApplyEvidence = []string{EvidenceASINVerbatim}
		p.RejectRequiresLowercase = true
		p.RejectFloor = 0.10
		return PresetClef, p, true
	}
	return "", ShouldApplyParams{}, false
}

// Policy is the decide step's effective policy and where it came from.
type Policy struct {
	Params ShouldApplyParams
	// Source is the highest layer that set any key: override, db, env,
	// preset or default.
	Source string
	// Preset names the built-in preset under the set keys ("" = the default).
	Preset string
	// Layers are the layers that set keys over the preset or default,
	// highest first.
	Layers []PolicyLayer
}

// PolicyLayer is one layer's policy keys: its source (override, db, env) and
// the keys it set, sorted.
type PolicyLayer struct {
	Source string   `json:"source"`
	Keys   []string `json:"keys"`
}

// SourceLabel is the policy's source for display: "override (apply_p) + db
// (reject_floor) over preset clef", "preset clef", "default".
func (p Policy) SourceLabel() string {
	base := PolicySourceDefault
	if p.Preset != "" {
		base = PolicySourcePreset + " " + p.Preset
	}
	if len(p.Layers) == 0 {
		return base
	}
	parts := make([]string, len(p.Layers))
	for i, l := range p.Layers {
		parts[i] = fmt.Sprintf("%s (%s)", l.Source, strings.Join(l.Keys, ", "))
	}
	return strings.Join(parts, " + ") + " over " + base
}

// ResolvePolicy is the decide policy cfg's effective decide model runs under
// (see the precedence above). override is a per-run --policy map (nil =
// none). The merged policy is validated: a bad value fails closed, naming
// where it came from — a step never runs under a policy other than the one it
// is set to.
func ResolvePolicy(cfg *config.Config, override map[string]any) (Policy, error) {
	m, _ := ModelFromConfig(cfg)
	m = m.orDefault()
	pol := Policy{Params: DefaultShouldApplyParams(), Source: PolicySourceDefault}
	if name, p, ok := PresetFor(m.Alias); ok {
		pol.Params, pol.Source, pol.Preset = p, PolicySourcePreset, name
	}
	layer := func(params map[string]any, source string) error {
		set, err := pol.Params.with(params)
		if err != nil {
			return fmt.Errorf("decide policy (%s): %w", source, err)
		}
		if len(set) > 0 {
			pol.Source = source
			pol.Layers = append([]PolicyLayer{{Source: source, Keys: set}}, pol.Layers...)
		}
		return nil
	}
	if err := layer(cfg.ModelPin(recipe.StepDecide).Params, cfg.PolicySource(recipe.StepDecide)); err != nil {
		return Policy{}, err
	}
	if err := layer(override, PolicySourceOverride); err != nil {
		return Policy{}, err
	}
	if err := pol.Params.Validate(); err != nil {
		return Policy{}, fmt.Errorf("decide policy for %s (%s): %w", m.Alias, pol.SourceLabel(), err)
	}
	return pol, nil
}

// with applies the decide policy keys of params to p in place and returns the
// keys it set, sorted. Other keys (usd_per_mtok_in) are ignored; a policy key
// of the wrong type is an error.
func (p *ShouldApplyParams) with(params map[string]any) ([]string, error) {
	var set []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		if !config.IsDecidePolicyParam(k) {
			continue
		}
		v := params[k]
		switch k {
		case config.ParamApplyP, config.ParamRejectP, config.ParamRejectFloor:
			f, ok := config.ParamFloat(v)
			if !ok {
				return nil, fmt.Errorf("%s must be a number, got %v", k, v)
			}
			switch k {
			case config.ParamApplyP:
				p.ApplyP = f
			case config.ParamRejectP:
				p.RejectP = f
			default:
				p.RejectFloor = f
			}
		case config.ParamRejectRequiresLowercase:
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be true or false, got %v", k, v)
			}
			p.RejectRequiresLowercase = b
		case config.ParamApplyEvidence:
			kinds, err := config.StringList(v)
			if err != nil {
				return nil, fmt.Errorf("%s %w", k, err)
			}
			p.ApplyEvidence = kinds
		}
		set = append(set, k)
	}
	return set, nil
}

// ParsePolicyFlags parses per-run --policy key=value flags: only decide
// policy keys; a number, true/false, or (apply_evidence) a comma-separated
// list. nil when none.
func ParsePolicyFlags(kvs []string) (map[string]any, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("--policy %q must be key=value", kv)
		}
		if !config.IsDecidePolicyParam(k) {
			return nil, fmt.Errorf("--policy %q: unknown key (known: %s)", k, strings.Join(config.DecidePolicyParams, ", "))
		}
		out[k] = config.ParseParamValue(v)
	}
	return out, nil
}
