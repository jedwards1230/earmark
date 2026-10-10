package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── Runtime step model settings (CONTRACT §2.20) ─────────────────────────────
//
// AI_ENDPOINTS + AI_ROLES + MODELS_FILE are the env default: which endpoint and
// model each AI step uses, and its pins. A runtime setting — a model_settings
// row, or a per-run --endpoint/--model override — picks another registered
// endpoint and/or model for one step without a deploy. Precedence:
//
//	per-run override > model_settings row > env default
//
// A setting names an AI_ENDPOINTS id, never a URL or a key: where to connect
// and with which credential stays in the env registry. WithStepSettings
// applies settings to a CLONE of the config, so every consumer
// (EvalEndpoint, DecideEndpoint, ScanEndpoint, EmbeddingsEndpoint, ModelPin)
// and the recipe each step stamps see the effective model with no change.

// Where a step's effective model came from (StepSource).
const (
	SourceOverride = "override" // a per-run --endpoint/--model flag
	SourceDB       = "db"       // a model_settings row
	SourceEnv      = "env"      // AI_ENDPOINTS + AI_ROLES (+ EVAL_CHAT_* for propose)
	SourceUnset    = "unset"    // the step has no endpoint: it is off
)

// SettingSteps are the steps a runtime setting may target, in display order.
// propose is AI_ROLES.eval, embed is AI_ROLES.embeddings.
var SettingSteps = []string{recipe.StepPropose, recipe.StepDecide, recipe.StepScan, recipe.StepEmbed}

// stepEndpointType is the endpoint type each setting step requires.
var stepEndpointType = map[string]AIEndpointType{
	recipe.StepPropose: AIEndpointTypeChat,
	recipe.StepDecide:  AIEndpointTypeSystemOne,
	recipe.StepScan:    AIEndpointTypeSystemOne,
	recipe.StepEmbed:   AIEndpointTypeEmbeddings,
}

// StepSetting is one step's runtime model choice: the endpoint (an
// AI_ENDPOINTS id) and the model id to request from it, plus optional pins.
// The pin fields replace the env MODELS_FILE pin for the step wholesale
// whenever the setting changes the step's endpoint or model, or sets any pin
// field itself; empty pin fields are then unpinned (the expected model is the
// requested one), exactly like an unpinned env step.
type StepSetting struct {
	Step       string
	EndpointID string
	Model      string
	// ExpectedModel / Revision / Params are the pin (MODELS_FILE semantics).
	ExpectedModel string
	Revision      string
	Params        map[string]any
	// Source is SourceDB or SourceOverride; "" is SourceDB.
	Source string
}

// HasPin reports whether the setting sets any pin field.
func (s StepSetting) HasPin() bool {
	return s.ExpectedModel != "" || s.Revision != "" || len(s.Params) > 0
}

func (s StepSetting) source() string {
	if s.Source == "" {
		return SourceDB
	}
	return s.Source
}

// StepSettingError is a setting that cannot be applied to this config. For a
// model_settings row it says how to fix it: a stored row is never silently
// replaced by another model.
type StepSettingError struct {
	Step   string
	Source string
	Err    error
}

func (e *StepSettingError) Error() string {
	if e.Source == SourceOverride {
		return fmt.Sprintf("--endpoint/--model for step %s: %v", e.Step, e.Err)
	}
	return fmt.Sprintf("model_settings row for step %s is invalid against this deployment's AI_ENDPOINTS: %v "+
		"(fix: earmark models unset %s, or earmark models set %s --endpoint <id> --model <id>)",
		e.Step, e.Err, e.Step, e.Step)
}

func (e *StepSettingError) Unwrap() error { return e.Err }

// StepEndpointType returns the endpoint type a setting step requires.
func StepEndpointType(step string) (AIEndpointType, bool) {
	t, ok := stepEndpointType[step]
	return t, ok
}

// ValidateStepSetting checks a setting against this config's registry: a
// setting step, an AI_ENDPOINTS id of the type the step needs, a model (a
// pinned version for a systemone step), and only known params.
func (c *Config) ValidateStepSetting(s StepSetting) error {
	want, ok := stepEndpointType[s.Step]
	if !ok {
		return fmt.Errorf("unknown step %q (settable: %s)", s.Step, strings.Join(SettingSteps, ", "))
	}
	id := strings.TrimSpace(s.EndpointID)
	if id == "" {
		return errors.New("endpoint is required")
	}
	ep, ok := c.registeredEndpoint(id)
	if !ok {
		return fmt.Errorf("endpoint %q is not in AI_ENDPOINTS (registered: %s)", id, strings.Join(c.registeredIDs(), ", "))
	}
	if ep.Type != want {
		return fmt.Errorf("endpoint %q is a %q endpoint; the %s step needs a %q endpoint", id, ep.Type, s.Step, want)
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		return errors.New("model is required")
	}
	if strings.ContainsAny(model, " \t\r\n") {
		return fmt.Errorf("model %q contains whitespace", s.Model)
	}
	if want == AIEndpointTypeSystemOne {
		if err := PinnedModel(model); err != nil {
			return err
		}
		if err := CheckSystemOneRoute(ep, model); err != nil {
			return err
		}
	}
	for name, v := range map[string]string{"expected model": s.ExpectedModel, "revision": s.Revision} {
		if strings.ContainsAny(strings.TrimSpace(v), " \t\r\n") {
			return fmt.Errorf("%s %q contains whitespace", name, v)
		}
	}
	if err := validatePinParams(s.Params); err != nil {
		return fmt.Errorf("params%w", err)
	}
	return nil
}

// registeredEndpoint finds an AI_ENDPOINTS entry by id (never a synthesized
// one: a setting must name a real registry entry).
func (c *Config) registeredEndpoint(id string) (AIEndpoint, bool) {
	for _, ep := range c.AIEndpoints {
		if ep.ID == id && ep.BaseID == "" {
			return ep, true
		}
	}
	return AIEndpoint{}, false
}

func (c *Config) registeredIDs() []string {
	var ids []string
	for _, ep := range c.AIEndpoints {
		if ep.BaseID == "" {
			ids = append(ids, ep.ID)
		}
	}
	if len(ids) == 0 {
		return []string{"none"}
	}
	return ids
}

// Clone returns a copy of c whose AI registry, role bindings, model registry
// and step sources can be changed without touching c. Other fields are
// shared values (slices of them are never mutated after LoadConfig).
func (c *Config) Clone() *Config {
	n := *c
	n.AIEndpoints = slices.Clone(c.AIEndpoints)
	if c.AIRoles != nil {
		r := *c.AIRoles
		n.AIRoles = &r
	} else {
		n.AIRoles = &AIRoles{}
	}
	n.Models = &ModelRegistry{Steps: map[string]ModelPin{}}
	if c.Models != nil {
		maps.Copy(n.Models.Steps, c.Models.Steps)
	}
	n.stepSources = maps.Clone(c.stepSources)
	if n.env == nil {
		// c is the env config itself (no setting applied yet): remember its
		// binding and pins, so a later setting equal to env restores them.
		n.env = snapshotEnv(c)
	}
	return &n
}

// envStep is one setting step as the env config binds it.
type envStep struct {
	endpoint string // registered endpoint id ("" when unbound or EVAL_CHAT_*)
	model    string
	pin      ModelPin
}

// envSnapshot is the env binding of every setting step, shared read-only by
// all clones derived from one env config.
type envSnapshot map[string]envStep

func snapshotEnv(c *Config) *envSnapshot {
	snap := envSnapshot{}
	for _, step := range SettingSteps {
		e := envStep{pin: c.ModelPin(step)}
		if ep, ok := c.StepEndpoint(step); ok {
			e.endpoint, e.model = ep.ID, ep.Model
		}
		snap[step] = e
	}
	return &snap
}

// isEnvBinding reports whether a setting names exactly the env endpoint and
// model of its step.
func (c *Config) isEnvBinding(s StepSetting) (envStep, bool) {
	if c.env == nil {
		return envStep{}, false
	}
	e := (*c.env)[s.Step]
	return e, e.endpoint != "" && e.endpoint == s.EndpointID && e.model == s.Model
}

// WithStepSettings returns a clone of c with each setting applied, in order
// (apply DB rows first, then per-run overrides, for the documented
// precedence). c is never modified, so a long-running process can re-apply
// fresh settings onto its base config every batch. Any invalid setting fails
// the whole call with a *StepSettingError naming the step: a step never runs
// on a model other than the one it is set to.
func (c *Config) WithStepSettings(settings ...StepSetting) (*Config, error) {
	n := c.Clone()
	for _, s := range settings {
		if err := n.applyStepSetting(s); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// WithValidStepSettings applies every valid setting and returns the invalid
// ones' errors by step instead of failing — for display (the Models page,
// `earmark models`) and for deciding the current recipes, never for running a
// step: a step whose setting is invalid keeps its previous binding here.
func (c *Config) WithValidStepSettings(settings ...StepSetting) (*Config, map[string]error) {
	n := c.Clone()
	var errs map[string]error
	for _, s := range settings {
		if err := n.applyStepSetting(s); err != nil {
			if errs == nil {
				errs = map[string]error{}
			}
			errs[s.Step] = err
		}
	}
	return n, errs
}

// applyStepSetting applies one validated setting to n in place (n is a clone).
func (c *Config) applyStepSetting(s StepSetting) error {
	s.EndpointID, s.Model = strings.TrimSpace(s.EndpointID), strings.TrimSpace(s.Model)
	if err := c.ValidateStepSetting(s); err != nil {
		return &StepSettingError{Step: s.Step, Source: s.source(), Err: err}
	}
	role := stepRole[s.Step]
	base, _ := c.registeredEndpoint(s.EndpointID)

	cur, bound := c.endpointForRole(role)
	if bound && !s.HasPin() && cur.Model == s.Model && (cur.ID == base.ID || cur.BaseID == base.ID) {
		// Same endpoint and model as now: the step is unchanged, so its pin
		// (the env MODELS_FILE pin, or a layer below's) still describes it.
		c.setStepSource(s.Step, s.source())
		return nil
	}

	// Synthesized endpoint lifecycle. A synthesized "<id>@<step>" entry
	// belongs to exactly one step, so it is replaced, never stacked:
	//   1. If the step is currently bound to a synthesized entry (cur.BaseID
	//      != ""), remove that entry by ITS OWN id, cur.ID — never by
	//      base.ID, which is the registered AI_ENDPOINTS entry other roles
	//      may still be bound to.
	//   2. Below, a new synthesized entry is created only when the setting
	//      asks the registered endpoint for a model other than its own
	//      (base.Model != s.Model); otherwise the role binds to the
	//      registered entry directly and no synthesized entry remains.
	if bound && cur.BaseID != "" {
		c.AIEndpoints = slices.DeleteFunc(c.AIEndpoints, func(ep AIEndpoint) bool { return ep.ID == cur.ID })
	}
	id := base.ID
	if base.Model != s.Model {
		// Another model on a registered endpoint: synthesize a copy for this
		// step only, so a role sharing the endpoint (decide and scan on one
		// systemone route) keeps its own model.
		derived := base
		derived.ID = c.derivedEndpointID(base.ID, s.Step)
		derived.Model = s.Model
		derived.BaseID = base.ID
		c.AIEndpoints = append(c.AIEndpoints, derived)
		id = derived.ID
	}
	c.bindRole(role, id)

	if env, ok := c.isEnvBinding(s); ok && !s.HasPin() {
		// Back to the env endpoint and model (an override undoing a db row,
		// or a row equal to env): the env MODELS_FILE pin describes it again,
		// so the recipe is the env recipe.
		c.Models.Steps[s.Step] = env.pin
		c.setStepSource(s.Step, s.source())
		return nil
	}

	// The step's model changed (or the setting pins it): the env pin no longer
	// describes it. The setting's pin replaces it wholesale; prompt_version
	// stays, since it pins the prompt, not the model.
	prev := c.Models.Steps[s.Step]
	c.Models.Steps[s.Step] = ModelPin{
		ExpectedModel: strings.TrimSpace(s.ExpectedModel),
		Revision:      strings.TrimSpace(s.Revision),
		PromptVersion: prev.PromptVersion,
		Params:        maps.Clone(s.Params),
	}
	c.setStepSource(s.Step, s.source())
	return nil
}

// derivedEndpointID names a synthesized endpoint "<base>@<step>", suffixed
// when that id is taken by a registry entry.
func (c *Config) derivedEndpointID(base, step string) string {
	id := base + "@" + step
	taken := func(id string) bool {
		return slices.ContainsFunc(c.AIEndpoints, func(ep AIEndpoint) bool { return ep.ID == id })
	}
	for i := 2; taken(id); i++ {
		id = fmt.Sprintf("%s@%s-%d", base, step, i)
	}
	return id
}

func (c *Config) bindRole(role, id string) {
	switch role {
	case roleEmbeddings:
		c.AIRoles.Embeddings = id
	case roleEval:
		c.AIRoles.Eval = id
	case roleDecide:
		c.AIRoles.Decide = id
	case roleScan:
		c.AIRoles.Scan = id
	}
}

func (c *Config) setStepSource(step, source string) {
	if c.stepSources == nil {
		c.stepSources = map[string]string{}
	}
	c.stepSources[step] = source
}

// StepSource reports where a step's effective model comes from: SourceOverride,
// SourceDB, SourceEnv, or SourceUnset when the step has no endpoint.
func (c *Config) StepSource(step string) string {
	if c == nil {
		return SourceUnset
	}
	if s, ok := c.stepSources[step]; ok {
		return s
	}
	if _, ok := c.stepModel(step); ok {
		return SourceEnv
	}
	return SourceUnset
}

// StepEndpoint returns the endpoint bound to a setting step's role (propose →
// AI_ROLES.eval, embed → embeddings, decide, scan). ok=false when unbound; a
// propose step running on the EVAL_CHAT_* fallback has no registry endpoint.
func (c *Config) StepEndpoint(step string) (AIEndpoint, bool) {
	role, ok := stepRole[step]
	if c == nil || !ok {
		return AIEndpoint{}, false
	}
	return c.endpointForRole(role)
}

// StepModel is the model id a step requests ("" and false when the step is
// off): its endpoint's model, or for propose the EVAL_CHAT_MODEL fallback.
func (c *Config) StepModel(step string) (string, bool) {
	if c == nil {
		return "", false
	}
	return c.stepModel(step)
}

// OverrideSetting builds a per-run override from --endpoint / --model flags
// against c (the config with the DB rows already applied). An omitted
// endpoint keeps the step's current endpoint; an omitted model is the named
// endpoint's own model when the endpoint changes, else the current model.
// ok=false when neither flag is set. The returned setting is validated by
// WithStepSettings.
func (c *Config) OverrideSetting(step, endpointID, model string) (s StepSetting, ok bool, err error) {
	endpointID, model = strings.TrimSpace(endpointID), strings.TrimSpace(model)
	if endpointID == "" && model == "" {
		return StepSetting{}, false, nil
	}
	if _, known := stepEndpointType[step]; !known {
		return StepSetting{}, true, fmt.Errorf("unknown step %q", step)
	}
	cur, bound := c.StepEndpoint(step)
	curBase := cur.ID
	if cur.BaseID != "" {
		curBase = cur.BaseID
	}
	if endpointID == "" {
		if !bound {
			return StepSetting{}, true, &StepSettingError{Step: step, Source: SourceOverride,
				Err: fmt.Errorf("the step has no endpoint to apply --model %q to; pass --endpoint <id> too", model)}
		}
		endpointID = curBase
	}
	if model == "" {
		switch ep, found := c.registeredEndpoint(endpointID); {
		case bound && endpointID == curBase:
			model = cur.Model
		case found:
			model = ep.Model
		}
	}
	return StepSetting{Step: step, EndpointID: endpointID, Model: model, Source: SourceOverride}, true, nil
}

// DescribeStep is a one-line description of a step's effective model for
// logs: "propose: <model> on <endpoint> (source db)".
func (c *Config) DescribeStep(step string) string {
	model, ok := c.StepModel(step)
	if !ok {
		return step + ": off (" + SourceUnset + ")"
	}
	where := "EVAL_CHAT_*"
	if ep, ok := c.StepEndpoint(step); ok {
		where = ep.ID
		if ep.BaseID != "" {
			where = ep.BaseID
		}
	}
	return fmt.Sprintf("%s: %s on %s (source %s)", step, model, where, c.StepSource(step))
}
