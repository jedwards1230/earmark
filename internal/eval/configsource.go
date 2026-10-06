package eval

import (
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
	if pin.PromptVersion != "" && pin.PromptVersion != judgePromptVersion {
		j.logger.Warn("MODELS_FILE pins a different judge prompt version than this build runs",
			"pinned", pin.PromptVersion, "running", judgePromptVersion)
	}
	return j
}

// CurrentRecipe resolves the judge cfg configures and returns its current
// recipe, for registration at startup. ok=false when no eval chat endpoint is
// configured (no judge, so no current propose recipe).
func CurrentRecipe(cfg *config.Config) (recipe.Recipe, bool) {
	chat, err := ResolveChatClient(ConfigSource(cfg))
	if err != nil {
		return recipe.Recipe{}, false
	}
	return NewJudgeForConfig(chat, cfg).Recipe(), true
}
