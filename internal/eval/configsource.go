package eval

import "github.com/jedwards1230/earmark/internal/config"

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
