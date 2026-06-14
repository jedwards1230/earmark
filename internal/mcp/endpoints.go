package mcp

import (
	"context"
	neturl "net/url"
	"sort"
	"strings"

	"github.com/jedwards1230/earmark/internal/config"
)

// ─── AI Endpoints section (Models/Services page) ────────────────────────────────
//
// The Models/Services page lists the AI endpoint registry (CONTRACT §2.14)
// alongside the ASR runners: one card per configured endpoint with its type,
// backend, model, baseURL host, options, the role it serves (if any), and a
// liveness probe. Observability only — no job routing. The baseURL is shown
// HOST-ONLY (no scheme/path) so the card stays compact and the page never
// surfaces a full internal URL the way the JSON API does.

// endpointStateMeta maps a probe state to its display label + dot color,
// mirroring the serverState convention used by the ASR cards.
type endpointStateMeta struct {
	Label string // READY / MODEL NOT LOADED / OFFLINE / UNKNOWN
	Class string
	Dot   string // green / amber / grey
	Sub   string
}

func endpointStateMetaFor(p endpointProbe) endpointStateMeta {
	switch {
	case !p.Probed:
		return endpointStateMeta{Label: "UNKNOWN", Class: "state-unknown", Dot: "grey", Sub: "not probed yet"}
	case p.State == epStateReady:
		return endpointStateMeta{Label: "READY", Class: "state-running", Dot: "green", Sub: "reachable — model available"}
	case p.State == epStateModelMissing:
		return endpointStateMeta{Label: "MODEL NOT LOADED", Class: "state-busy", Dot: "amber",
			Sub: "reachable, but the configured model is not in /models"}
	default: // offline
		return endpointStateMeta{Label: "OFFLINE", Class: "state-offline", Dot: "grey",
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
	Type     string // "embeddings" | "chat"
	Backend  string // "ollama" | "vllm" | "openai-compat"
	Model    string
	BaseURL  string // full URL — JSON API only
	HostOnly string // host[:port] for the card (no scheme/path)
	Role     string // "embeddings" | "eval" | "" (unbound)
	Options  []optionKV

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
		v := endpointView{
			ID:         ep.ID,
			Type:       string(ep.Type),
			Backend:    string(ep.Backend),
			Model:      ep.Model,
			BaseURL:    ep.BaseURL,
			HostOnly:   hostOnly(ep.BaseURL),
			Role:       cfg.RoleForEndpoint(ep.ID),
			Options:    sortedOptions(ep.Options),
			State:      meta,
			Probed:     probe.Probed,
			StateToken: string(probeStateToken(probe)),
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
		out[ep.ID] = s.endpointProber.Probe(ctx, ep.BaseURL, ep.Model)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// roleNote renders the assigned-role suffix on a card ("role: embeddings
// (assigned)"), or "" when the endpoint is unbound.
func (v endpointView) roleNote() string {
	if v.Role == "" {
		return ""
	}
	return "role: " + v.Role + " (assigned)"
}

// optionsLine renders the options as "k=v k=v …" for the card, or "" when none.
func (v endpointView) optionsLine() string {
	if len(v.Options) == 0 {
		return ""
	}
	parts := make([]string, 0, len(v.Options))
	for _, o := range v.Options {
		parts = append(parts, o.Key+"="+o.Value)
	}
	return strings.Join(parts, " ")
}
