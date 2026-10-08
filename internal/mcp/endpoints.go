package mcp

import (
	"context"
	neturl "net/url"
	"sort"
	"strings"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── AI endpoints table (Models page) ──────────────────────────────────────────
//
// The Models page lists the AI endpoint registry (CONTRACT §2.14) as a table:
// one row per configured endpoint with its role, type, gateway, baseURL host,
// model, liveness, and options (the backend stays in the JSON API only). Liveness is a GET /models probe ONLY —
// role health on the cards above folds in call outcomes. Observability only, no
// job routing. The baseURL is shown HOST-ONLY (no scheme/path) so the page never
// surfaces a full internal URL the way the JSON API does.

// endpointStateMeta maps a probe state to its display label + dot color. The
// labels say what the GET /models probe saw ("lists model", not "READY"), so a
// liveness result never reads as role health or runner readiness.
type endpointStateMeta struct {
	Label string // lists model / model not listed / unreachable / unknown
	Glyph string // ✓ / ▲ / ✗ / ? — paired with the word, never color alone
	Class string
	Dot   string // green / amber / grey
	Sub   string
}

func endpointStateMetaFor(p endpointProbe) endpointStateMeta {
	switch {
	case !p.Probed:
		return endpointStateMeta{Label: "unknown", Glyph: "?", Class: "state-unknown", Dot: "grey", Sub: "not probed yet"}
	case p.State == epStateReady:
		return endpointStateMeta{Label: "lists model", Glyph: "✓", Class: "state-running", Dot: "green", Sub: "reachable — model available"}
	case p.State == epStateModelMissing:
		return endpointStateMeta{Label: "model not listed", Glyph: "▲", Class: "state-busy", Dot: "amber",
			Sub: "reachable, but the configured model is not in /models"}
	default: // offline
		return endpointStateMeta{Label: "unreachable", Glyph: "✗", Class: "state-offline", Dot: "grey",
			Sub: "endpoint unreachable (GET /models failed)"}
	}
}

// optionKV is one rendered key=value option pair for the endpoint card.
type optionKV struct {
	Key   string
	Value string
}

// endpointView is one card on the Models/Services page: a registry entry merged
// with its health probe and resolved role.
type endpointView struct {
	ID       string
	Type     string // "embeddings" | "chat" | "systemone"
	Backend  string // "ollama" | "vllm" | "openai-compat"
	Model    string
	BaseURL  string // full URL — JSON API only
	HostOnly string // host[:port] for the card (no scheme/path)
	Role     string // "embeddings" | "eval" | "decide" | "scan" | "" (unbound)
	Options  []optionKV
	// Gateway is the routing gateway ("litellm", or a declared value), "" for a
	// direct endpoint; GatewayInferred marks a host-name guess rather than an
	// AI_ENDPOINTS declaration (display only).
	Gateway         string
	GatewayInferred bool
	// FromEnv marks the synthetic EVAL_CHAT_* row: a judge configured outside
	// the registry, never probed (its key is not held by the registry).
	FromEnv bool

	State  endpointStateMeta
	Probed bool
	// StateToken is the machine token for the JSON API
	// ("ready"|"model_not_loaded"|"offline"|"unknown").
	StateToken string
}

// hostOnly extracts host[:port] from a base URL for compact display, falling
// back to the raw string when it can't be parsed.
func hostOnly(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// sortedOptions renders an endpoint's options map as a deterministically-ordered
// slice (sorted by key) so the card and tests are stable.
func sortedOptions(opts map[string]string) []optionKV {
	if len(opts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]optionKV, 0, len(keys))
	for _, k := range keys {
		out = append(out, optionKV{Key: k, Value: opts[k]})
	}
	return out
}

// buildEndpointViews merges the configured AI endpoints with their health
// probes and resolved roles into the page model. Pure (no HTTP) so the state
// logic is unit-testable: probes maps an endpoint id to its probe result.
func buildEndpointViews(cfg *config.Config, probes map[string]endpointProbe) []endpointView {
	if cfg == nil {
		return nil
	}
	views := make([]endpointView, 0, len(cfg.AIEndpoints))
	for _, ep := range cfg.AIEndpoints {
		probe := probes[ep.ID] // zero value → Probed:false → UNKNOWN
		meta := endpointStateMetaFor(probe)
		gw, inferred := gatewayFor(ep.Gateway, ep.BaseURL)
		if gw == "litellm" && probe.Probed && probe.State == epStateModelMissing {
			// LiteLLM's /v1/models lists only the virtual key's allowed models,
			// so "missing" means "not on earmark's key allowlist": calls 403.
			meta.Label = "not allowed"
			meta.Sub = "not on earmark's LiteLLM key allowlist (403 on call)"
		}
		v := endpointView{
			Gateway:         gw,
			GatewayInferred: inferred,
			ID:              ep.ID,
			Type:            string(ep.Type),
			Backend:         string(ep.Backend),
			Model:           ep.Model,
			BaseURL:         ep.BaseURL,
			HostOnly:        hostOnly(ep.BaseURL),
			Role:            cfg.RoleForEndpoint(ep.ID),
			Options:         sortedOptions(ep.Options),
			State:           meta,
			Probed:          probe.Probed,
			StateToken:      string(probeStateToken(probe)),
		}
		views = append(views, v)
	}
	return views
}

// probeStateToken returns the machine token for the JSON API, collapsing an
// un-probed endpoint to "unknown".
func probeStateToken(p endpointProbe) endpointProbeState {
	if !p.Probed {
		return epStateUnknown
	}
	return p.State
}

// probeEndpoints probes every configured AI endpoint, keyed by endpoint id.
// Returns nil when no prober is wired (the views then render UNKNOWN). The
// prober caches per baseURL, so calling this on both render paths is cheap.
func (s *MCPServer) probeEndpoints(ctx context.Context) map[string]endpointProbe {
	if s.endpointProber == nil || s.cfg == nil {
		return nil
	}
	out := make(map[string]endpointProbe, len(s.cfg.AIEndpoints))
	for _, ep := range s.cfg.AIEndpoints {
		out[ep.ID] = s.endpointProber.Probe(ctx, probeBase(ep), ep.Model, ep.APIKey)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// probeBase is the base the /models probe is appended to: the OpenAI-style
// base for embeddings and chat endpoints, and {base}/v1 for a systemone
// endpoint, whose base is the route prefix the client appends /v1/… to
// (GET {base}/v1/models, CONTRACT §2.14).
func probeBase(ep config.AIEndpoint) string {
	if ep.Type == config.AIEndpointTypeSystemOne {
		return strings.TrimRight(ep.BaseURL, "/") + "/v1"
	}
	return ep.BaseURL
}

// OptionsLine renders the options as "k=v k=v …" for the card, or "" when none.
// Exported because html/template only invokes exported methods — an
// unexported method name is treated as a (missing) field lookup and errors at
// execute time.
func (v endpointView) OptionsLine() string {
	if len(v.Options) == 0 {
		return ""
	}
	parts := make([]string, 0, len(v.Options))
	for _, o := range v.Options {
		parts = append(parts, o.Key+"="+o.Value)
	}
	return strings.Join(parts, " ")
}

// GatewayLabel renders the Gateway cell: "LiteLLM", a declared value verbatim,
// or "direct". Exported for template method dispatch.
func (v endpointView) GatewayLabel() string {
	if v.Gateway == "" {
		return "direct"
	}
	return gatewayLabel(v.Gateway)
}

// RoleTitle names the bound role the way the role cards do ("Judge" for the
// eval role), "" when unbound. Exported for template method dispatch.
func (v endpointView) RoleTitle() string {
	switch v.Role {
	case "":
		return ""
	case "eval":
		return roleTitleForStep(recipe.StepPropose)
	case "embeddings":
		return roleTitleForStep(recipe.StepEmbed)
	case "decide":
		return roleTitleForStep(recipe.StepDecide)
	case "scan":
		return "Scan"
	default:
		return v.Role
	}
}

// envJudgeView is the synthetic endpoint-table row for a judge configured from
// EVAL_CHAT_* rather than AI_ENDPOINTS. It is never probed: probing would mean
// holding the API key outside the registry.
func envJudgeView(j judgeConfig) *endpointView {
	if !j.Configured || j.Source != judgeSourceEnv {
		return nil
	}
	gw, inferred := gatewayFor("", j.Host)
	return &endpointView{
		ID: "EVAL_CHAT_*", Type: "chat", Backend: "env", Model: j.Model, HostOnly: j.Host,
		Role: "eval", Gateway: gw, GatewayInferred: inferred, FromEnv: true,
		State:      endpointStateMeta{Label: "not probed", Glyph: "?", Class: "state-unknown", Dot: "grey", Sub: "from env, not in AI_ENDPOINTS"},
		StateToken: string(epStateUnknown),
	}
}
