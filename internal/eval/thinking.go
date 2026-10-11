package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ─── Thinking controls (CONTRACT §2.15 "Thinking controls") ───────────────────
//
// The judge request's two reasoning knobs, reasoning_effort and
// chat_template_kwargs, are resolved per field from three layers:
//
//	propose model setting param > EVAL_* env var > route default ("auto")
//
// The model setting param is the propose step's effective pin params (a
// model_settings row, or MODELS_FILE steps.propose.params — CONTRACT §2.20),
// so the knobs travel with the model they were tuned for: switching the judge
// to another model drops them with the old pin. The env vars stay the
// deployment-wide default for every model. The first layer that sets a field
// wins; "auto" in it means the route default, "omit" means never sent.

// Thinking-control env vars. Both default to "auto".
const (
	// envReasoningEffort overrides the reasoning_effort request field:
	// unset/"auto" → per-route default, "omit" → never sent, any other value →
	// sent verbatim (e.g. "none", "low").
	envReasoningEffort = "EVAL_REASONING_EFFORT"
	// envChatTemplateKwargs overrides chat_template_kwargs: unset/"auto" →
	// per-route default, "omit" → never sent, a JSON object → sent verbatim.
	envChatTemplateKwargs = "EVAL_CHAT_TEMPLATE_KWARGS"

	thinkingAuto = "auto"
	thinkingOmit = "omit"
)

// Where a resolved thinking field came from (Thinking.EffortSource /
// KwargsSource).
const (
	ThinkingFromSetting = "setting" // the propose model setting's params
	ThinkingFromEnv     = "env"     // EVAL_REASONING_EFFORT / EVAL_CHAT_TEMPLATE_KWARGS
	ThinkingFromAuto    = "auto"    // nothing set: the route default
)

// ThinkingSetting is the propose step's per-model thinking params, as text:
// "" (not set — fall through to the env var), "auto", "omit", or the value
// (an effort token; a JSON object for the kwargs).
type ThinkingSetting struct {
	ReasoningEffort    string
	ChatTemplateKwargs string
}

// ThinkingSettingSource is an optional EvalEndpointSource extension that
// supplies the propose model setting's thinking params. A source without it
// (or a nil source) has no setting layer: the env vars and the route default
// apply. An error means a stored param is invalid, which fails the judge's
// construction rather than silently dropping the knob.
type ThinkingSettingSource interface {
	JudgeThinking() (ThinkingSetting, error)
}

// Thinking is the resolved pair of thinking knobs one judge sends.
type Thinking struct {
	// ReasoningEffort is the reasoning_effort sent; "" = omitted.
	ReasoningEffort string
	// ChatTemplateKwargs is the chat_template_kwargs sent; nil = omitted.
	ChatTemplateKwargs map[string]any
	// EffortSource / KwargsSource name the layer each field came from
	// (ThinkingFromSetting / ThinkingFromEnv / ThinkingFromAuto).
	EffortSource string
	KwargsSource string
	// Hosted is whether the model id names a hosted provider route
	// (isHostedRoute), which decides the "auto" values.
	Hosted bool
}

// autoThinking is the route default: local models (Qwen-family on Ollama)
// are told not to think in both spellings; hosted routes get neither.
func autoThinking(hosted bool) (string, map[string]any) {
	if hosted {
		return "", nil
	}
	return "none", map[string]any{"enable_thinking": false}
}

// ResolveThinking resolves the thinking knobs for model from the setting
// layer s, then the env vars, then the route default. Invalid values in any
// layer are a configuration error naming where they came from.
func ResolveThinking(model string, s ThinkingSetting) (Thinking, error) {
	t := Thinking{Hosted: isHostedRoute(model)}
	autoEffort, autoKwargs := autoThinking(t.Hosted)

	raw, src, where := pickLayer(s.ReasoningEffort, envReasoningEffort, "reasoning_effort")
	t.EffortSource = src
	switch strings.ToLower(raw) {
	case "", thinkingAuto:
		t.ReasoningEffort = autoEffort
	case thinkingOmit:
	default:
		if strings.ContainsAny(raw, " \t\r\n") {
			return Thinking{}, fmt.Errorf("%s must be \"auto\", \"omit\" or a token without whitespace: %q", where, raw)
		}
		t.ReasoningEffort = raw
	}

	raw, src, where = pickLayer(s.ChatTemplateKwargs, envChatTemplateKwargs, "chat_template_kwargs")
	t.KwargsSource = src
	switch strings.ToLower(raw) {
	case "", thinkingAuto:
		t.ChatTemplateKwargs = autoKwargs
	case thinkingOmit:
	default:
		var kwargs map[string]any
		if err := json.Unmarshal([]byte(raw), &kwargs); err != nil || kwargs == nil {
			return Thinking{}, fmt.Errorf("%s must be \"auto\", \"omit\" or a JSON object: %q", where, raw)
		}
		t.ChatTemplateKwargs = kwargs
	}
	return t, nil
}

// pickLayer returns the first set layer's raw value, its source token and a
// name for error messages: the setting param, else the env var, else auto.
func pickLayer(setting, envVar, param string) (raw, source, where string) {
	if v := strings.TrimSpace(setting); v != "" {
		return v, ThinkingFromSetting, "propose model setting param " + param
	}
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v, ThinkingFromEnv, envVar
	}
	return "", ThinkingFromAuto, ""
}

// recipeParams adds the thinking knobs to the propose recipe params — only a
// field that differs from the route default, so every judge running on the
// defaults keeps the recipe id it had before thinking became a recipe input,
// while any change to what is sent (a model setting's reasoning_effort "low",
// an "omit") is a new recipe. An omitted-but-not-by-default field is recorded
// as "omit".
func (t Thinking) recipeParams(params map[string]any) {
	autoEffort, autoKwargs := autoThinking(t.Hosted)
	if t.ReasoningEffort != autoEffort {
		params["reasoning_effort"] = orOmit(t.ReasoningEffort)
	}
	if !sameJSON(t.ChatTemplateKwargs, autoKwargs) {
		if t.ChatTemplateKwargs == nil {
			params["chat_template_kwargs"] = thinkingOmit
		} else {
			params["chat_template_kwargs"] = t.ChatTemplateKwargs
		}
	}
}

func orOmit(s string) string {
	if s == "" {
		return thinkingOmit
	}
	return s
}

// sameJSON compares two kwargs objects by their JSON encoding (map keys are
// sorted), treating nil and nil as equal and nil vs {} as different.
func sameJSON(a, b map[string]any) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// Describe renders the knobs and their sources for `earmark models` and the
// Models page: `reasoning_effort "low" (model setting) · chat_template_kwargs
// omitted (auto: hosted route)`.
func (t Thinking) Describe() string {
	effort := "omitted"
	if t.ReasoningEffort != "" {
		effort = fmt.Sprintf("%q", t.ReasoningEffort)
	}
	kwargs := "omitted"
	if t.ChatTemplateKwargs != nil {
		b, err := json.Marshal(t.ChatTemplateKwargs)
		if err == nil {
			kwargs = string(b)
		}
	}
	return fmt.Sprintf("reasoning_effort %s (%s) · chat_template_kwargs %s (%s)",
		effort, t.sourceLabel(t.EffortSource, envReasoningEffort),
		kwargs, t.sourceLabel(t.KwargsSource, envChatTemplateKwargs))
}

func (t Thinking) sourceLabel(src, envVar string) string {
	switch src {
	case ThinkingFromSetting:
		return "model setting"
	case ThinkingFromEnv:
		return envVar
	}
	if t.Hosted {
		return "auto: hosted route"
	}
	return "auto: local model"
}

// JudgeThinkingFor resolves the thinking knobs the judge would send for
// model under src's setting layer (nil src: env and route default only).
func JudgeThinkingFor(src EvalEndpointSource, model string) (Thinking, error) {
	setting, err := thinkingSetting(src)
	if err != nil {
		return Thinking{}, err
	}
	return ResolveThinking(model, setting)
}

// thinkingSetting reads the setting layer from src when it provides one.
func thinkingSetting(src EvalEndpointSource) (ThinkingSetting, error) {
	if ts, ok := src.(ThinkingSettingSource); ok {
		return ts.JudgeThinking()
	}
	return ThinkingSetting{}, nil
}
