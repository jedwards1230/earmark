package eval

import (
	"errors"
	"fmt"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// configSource adapts a *config.Config to the EvalEndpointSource interface,
// mapping config.AIEndpoint → the eval-local EvalEndpoint shape. It is the only
// file in this package that imports internal/config; the judge core (eval.go,
// run.go, chat.go) stays decoupled from config's endpoint structs and is tested
// with a tiny fake source. There is no import cycle: config does not import eval.
type configSource struct{ cfg *config.Config }

// EvalEndpoint resolves the chat endpoint bound to the "eval" role, if any.
func (s configSource) EvalEndpoint() (EvalEndpoint, bool) {
	ep, ok := s.cfg.EvalEndpoint()
	if !ok {
		return EvalEndpoint{}, false
	}
	return EvalEndpoint{
		BaseURL: ep.BaseURL,
		Model:   ep.Model,
		APIKey:  ep.APIKey,
		Options: ep.Options,
	}, true
}

// JudgeSetting is the setting layer of the judge's request params: the
// propose step's effective pin params (a model_settings row, or MODELS_FILE
// steps.propose.params — CONTRACT §2.20). Values were validated when the
// setting was applied; they are re-read with the same parsers so a value that
// slipped past (a hand-built config) still fails closed.
func (s configSource) JudgeSetting() (JudgeSetting, error) {
	var out JudgeSetting
	params := s.cfg.ModelPin(recipe.StepPropose).Params
	if v, ok := params[config.ParamReasoningEffort]; ok {
		effort, err := config.ReasoningEffortParam(v)
		if err != nil {
			return JudgeSetting{}, fmt.Errorf("propose params.%s %w", config.ParamReasoningEffort, err)
		}
		out.ReasoningEffort = effort
	}
	if v, ok := params[config.ParamChatTemplateKwargs]; ok {
		kwargs, err := config.ChatTemplateKwargsParam(v)
		if err != nil {
			return JudgeSetting{}, fmt.Errorf("propose params.%s %w", config.ParamChatTemplateKwargs, err)
		}
		out.ChatTemplateKwargs = kwargs
	}
	if v, ok := params[config.ParamTemperature]; ok {
		temp, err := config.TemperatureParam(v)
		if err != nil {
			return JudgeSetting{}, fmt.Errorf("propose params.%s %w", config.ParamTemperature, err)
		}
		out.Temperature = &temp
	}
	return out, nil
}

// ConfigSource wraps a parsed *config.Config as an EvalEndpointSource for
// ResolveChatClient. A nil cfg yields a nil source (env-var fallback only).
func ConfigSource(cfg *config.Config) EvalEndpointSource {
	if cfg == nil {
		return nil
	}
	return configSource{cfg: cfg}
}

// NewJudgeForConfig is NewJudge plus the model registry's propose pin
// (MODELS_FILE, CONTRACT §2.18), so the judge's recipe records the expected
// model and revision. A nil cfg is a plain NewJudge.
func NewJudgeForConfig(chat ChatClient, cfg *config.Config) *Judge {
	j := NewJudge(chat)
	pin := cfg.ModelPin(recipe.StepPropose)
	j.SetModelPin(ModelPin{ExpectedModel: pin.ExpectedModel, Revision: pin.Revision})
	if pin.ExpectedModel == "" {
		// Not knowable at startup whether the endpoint reports the alias
		// verbatim (Ollama does) or a provider id (LiteLLM usually does); the
		// judge warns once on the first mismatching response.
		j.logger.Info("no MODELS_FILE expected_model pin for the judge; the current propose recipe expects the endpoint to report the requested model id",
			"requested", chat.Model())
	}
	if pin.PromptVersion != "" && pin.PromptVersion != judgePromptVersion {
		j.logger.Warn("MODELS_FILE pins a different judge prompt version than this build runs",
			"pinned", pin.PromptVersion, "running", judgePromptVersion)
	}
	return j
}

// CurrentRecipe resolves the judge cfg configures and returns its current
// recipe, for registration at startup. ok=false with a nil error when no eval
// chat endpoint is configured (no judge, so no current propose recipe); a
// configured-but-invalid endpoint returns the error so the caller can say so.
func CurrentRecipe(cfg *config.Config) (r recipe.Recipe, ok bool, err error) {
	chat, err := ResolveChatClient(ConfigSource(cfg))
	if errors.Is(err, ErrChatNotConfigured) {
		return recipe.Recipe{}, false, nil
	}
	if err != nil {
		return recipe.Recipe{}, false, err
	}
	return NewJudgeForConfig(chat, cfg).Recipe(), true, nil
}
