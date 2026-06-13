package mcp

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

// ─── Servers page ─────────────────────────────────────────────────────────────
//
// The Servers page answers two operator questions the rest of the dashboard
// can't: which transcription servers (ASR runners) are configured for this
// deployment and which are live right now, and what model / compute mode each
// is actually running. Both are read-only views.
//
// IMPORTANT honesty constraint: there is no per-runner heartbeat/registry table
// (CONTRACT §1.4) — the only live-presence signal is a fresh claim heartbeat on
// a job the runner holds. So an idle-but-online runner is indistinguishable
// from an offline one; the page says "idle — last active X", never a false
// "ready". The configured list (ASR_SERVERS) lets a declared-but-idle fallback
// still appear. Job routing is NOT done here — the runner claims work itself.

// serverState is the derived liveness of one server, with its CSS/dot classes.
type serverState struct {
	Label string // TRANSCRIBING / READY / BUSY / STALLED / OFFLINE / IDLE / NOT SEEN
	Class string // state-running / state-busy / state-stalled / state-offline / state-idle / state-unknown
	Dot   string // green / amber / red / grey / blue
	Sub   string // human one-liner
	// Token is the machine-readable state for the JSON API, derived from Label
	// (e.g. "transcribing", "ready", "busy", "stalled", "offline", "idle",
	// "not_seen").
	Token string
}

// serverView is one row/card on the Servers page: the merge of a configured
// ASR_SERVERS entry with the runner activity observed in the database. An
// observed runner with no matching config entry is rendered too (Configured
// = false) so nothing is silently hidden.
type serverView struct {
	Name       string
	Host       string
	Role       string // "primary" / "fallback" / "" (informational)
	Configured bool   // false → observed-only (no ASR_SERVERS entry matched)

	State serverState

	// Models & modes (observed wins over configured; size derived from the model
	// name, e.g. "0.6B" from parakeet-tdt-0.6b-v3).
	Model       string // resolved model name ("" when neither observed nor configured)
	ModelSource string // "observed" / "configured" / ""
	ModelSize   string // "0.6B" or ""
	ComputeMode string // observed compute_type, e.g. "bfloat16", or ""
	JobsDone    int    // run_metrics rows attributed to this server
	AvgProc     string // humanized mean wall-clock, or "—"
	LastActive  string // rel time of last completion, or "—"

	// Live GPU readiness from gpu-arbiter (only when a gpuArbiterUrl is
	// configured for this server). Probed is false when no probe ran; the rest
	// are then zero. Exposed in the JSON API as the fallback-automation hook.
	Probed      bool
	Reachable   bool
	GPUState    string // "available" / "gaming" / "evicting" / "" (unknown)
	VRAMUsedMB  *int
	VRAMTotalMB *int
}

// modelSizeRe extracts a parameter-count token like "0.6b" / "1b" / "7b" from an
// ASR model id (e.g. "nvidia/parakeet-tdt-0.6b-v3" → "0.6B").
var modelSizeRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)b(?:[-_.]|$)`)

// modelSize renders a human parameter-size label from a model name, or "" when
// the name carries no recognizable size token.
func modelSize(model string) string {
	m := modelSizeRe.FindStringSubmatch(model)
	if m == nil {
		return ""
	}
	return m[1] + "B"
}

// buildServerViews merges the configured ASR_SERVERS list with observed runner
// activity into the Servers-page model. It is pure (deterministic given now) so
// the state logic is unit-testable without a DB or HTTP server.
// probes maps a configured server's Name to its gpu-arbiter readiness (only for
// servers with a gpuArbiterUrl); nil/absent → readiness is inferred from job
// activity. Kept as a parameter so buildServerViews stays pure (no HTTP).
func buildServerViews(configured []config.ASRServer, obs *db.ServerObservation, probes map[string]arbiterStatus, now time.Time, staleAfter time.Duration) []serverView {
	if obs == nil {
		obs = &db.ServerObservation{}
	}
	liveUsed := make([]bool, len(obs.LiveRunners))
	hostUsed := make([]bool, len(obs.Hosts))

	views := make([]serverView, 0, len(configured)+len(obs.LiveRunners)+len(obs.Hosts))

	for _, c := range configured {
		token := c.MatchToken()

		// Freshest matching live runner (claimed_by contains the token).
		liveIdx := -1
		for i, lr := range obs.LiveRunners {
			if liveUsed[i] || token == "" || !strings.Contains(strings.ToLower(lr.ClaimedBy), token) {
				continue
			}
			if liveIdx < 0 || lr.LastHeartbeat.After(obs.LiveRunners[liveIdx].LastHeartbeat) {
				liveIdx = i
			}
		}

		// Most-recent matching host (runner_host contains the token). Sum jobs
		// across every matching host so the count isn't split.
		hostIdx, jobs := -1, 0
		for i, h := range obs.Hosts {
			if hostUsed[i] || token == "" || !strings.Contains(strings.ToLower(h.Host), token) {
				continue
			}
			jobs += h.JobsDone
			if hostIdx < 0 || laterFinish(obs.Hosts[i], obs.Hosts[hostIdx]) {
				hostIdx = i
			}
		}

		var live *db.LiveRunner
		if liveIdx >= 0 {
			liveUsed[liveIdx] = true
			live = &obs.LiveRunners[liveIdx]
		}
		var host *db.HostMetrics
		if hostIdx >= 0 {
			for i := range obs.Hosts { // consume all matches so they don't double-render
				if !hostUsed[i] && token != "" && strings.Contains(strings.ToLower(obs.Hosts[i].Host), token) {
					hostUsed[i] = true
				}
			}
			host = &obs.Hosts[hostIdx]
		}

		v := serverView{Name: c.Name, Host: c.Host, Role: c.Role, Configured: true}
		applyObserved(&v, live, host, probeFor(probes, c.Name), jobs, c.Model, now, staleAfter)
		views = append(views, v)
	}

	// Unconfigured live runners: pair with an as-yet-unused host when the host
	// token sits inside the claimed_by string (e.g. host "gpu-1" ⊂
	// "asr-runner-gpu-1"), so its model/mode shows too.
	for i, lr := range obs.LiveRunners {
		if liveUsed[i] {
			continue
		}
		liveUsed[i] = true
		hostIdx, jobs := -1, 0
		lc := strings.ToLower(lr.ClaimedBy)
		for j, h := range obs.Hosts {
			if hostUsed[j] || h.Host == "" || !strings.Contains(lc, strings.ToLower(h.Host)) {
				continue
			}
			jobs += h.JobsDone
			if hostIdx < 0 || laterFinish(obs.Hosts[j], obs.Hosts[hostIdx]) {
				hostIdx = j
			}
		}
		var host *db.HostMetrics
		if hostIdx >= 0 {
			hostUsed[hostIdx] = true
			host = &obs.Hosts[hostIdx]
		}
		lr := lr
		v := serverView{Name: lr.ClaimedBy, Configured: false}
		applyObserved(&v, &lr, host, nil, jobs, "", now, staleAfter)
		views = append(views, v)
	}

	// Unconfigured hosts with only historical metrics (no live claim).
	for i, h := range obs.Hosts {
		if hostUsed[i] {
			continue
		}
		hostUsed[i] = true
		h := h
		v := serverView{Name: h.Host, Configured: false}
		applyObserved(&v, nil, &h, nil, h.JobsDone, "", now, staleAfter)
		views = append(views, v)
	}

	return views
}

// probeFor returns the probe result for a server name, or nil when none ran.
func probeFor(probes map[string]arbiterStatus, name string) *arbiterStatus {
	if probes == nil {
		return nil
	}
	if st, ok := probes[name]; ok {
		return &st
	}
	return nil
}

// busySubtext describes why a reachable runner is not usable right now (its
// GPU is gaming/evicting, or free-but-runner-stopped), with the active game
// claim and VRAM appended when known.
func busySubtext(probe *arbiterStatus) string {
	var sub string
	switch probe.State {
	case "gaming":
		sub = "connected — GPU in use (game mode)"
	case "evicting":
		sub = "connected — switching workloads (evicting)"
	case "available": // ready() was false → the runner unit is down
		sub = "connected — GPU free but asr-runner stopped"
	default:
		sub = "connected — GPU busy"
	}
	if len(probe.Claims) > 0 {
		sub += " · " + probe.Claims[0]
	}
	return sub + vramSuffix(probe)
}

// vramSuffix renders " · VRAM 7.3/32 GB" when the probe reported both figures.
func vramSuffix(probe *arbiterStatus) string {
	if probe == nil || probe.VRAMUsedMB == nil || probe.VRAMTotalMB == nil || *probe.VRAMTotalMB <= 0 {
		return ""
	}
	used := float64(*probe.VRAMUsedMB) / 1024
	total := float64(*probe.VRAMTotalMB) / 1024
	return fmt.Sprintf(" · VRAM %.1f/%.0f GB", used, total)
}

// laterFinish reports whether host a completed work more recently than b
// (NULLs sort last), used to pick the representative among matching hosts.
func laterFinish(a, b db.HostMetrics) bool {
	if a.LastFinished == nil {
		return false
	}
	if b.LastFinished == nil {
		return true
	}
	return a.LastFinished.After(*b.LastFinished)
}

// applyObserved fills the state + model/mode fields of v from the optional live
// runner, host metrics, and gpu-arbiter probe, falling back to the configured
// model when no run has reported one yet.
//
// State precedence: an active claim (TRANSCRIBING/STALLED) trumps everything —
// if the runner holds a job, the GPU is plainly serving it. Otherwise, when a
// probe is configured, live reachability decides (OFFLINE / READY / BUSY).
// Only without a probe do we fall back to historical inference (IDLE/NOT SEEN).
func applyObserved(v *serverView, live *db.LiveRunner, host *db.HostMetrics, probe *arbiterStatus, jobs int, configuredModel string, now time.Time, staleAfter time.Duration) {
	if probe != nil {
		v.Probed = true
		v.Reachable = probe.Reachable
		v.GPUState = probe.State
		v.VRAMUsedMB = probe.VRAMUsedMB
		v.VRAMTotalMB = probe.VRAMTotalMB
	}

	switch {
	case live != nil && now.Sub(live.LastHeartbeat) <= staleAfter:
		sub := "transcribing"
		if f := path.Base(live.CurrentFile); f != "" && f != "." && f != "/" {
			sub = "transcribing " + f
		}
		v.State = serverState{Label: "TRANSCRIBING", Class: "state-running", Dot: "green", Sub: sub}
	case live != nil:
		v.State = serverState{Label: "STALLED", Class: "state-stalled", Dot: "red",
			Sub: "claim heartbeat stale (" + humanizeSince(now.Sub(live.LastHeartbeat)) + ") — runner may have crashed"}
	case probe != nil && !probe.Reachable:
		v.State = serverState{Label: "OFFLINE", Class: "state-offline", Dot: "grey",
			Sub: "host unreachable (gpu-arbiter not responding)"}
	case probe != nil && probe.ready():
		v.State = serverState{Label: "READY", Class: "state-running", Dot: "green",
			Sub: "connected — GPU available" + vramSuffix(probe)}
	case probe != nil:
		v.State = serverState{Label: "BUSY", Class: "state-busy", Dot: "amber",
			Sub: busySubtext(probe)}
	case host != nil && host.JobsDone > 0:
		sub := "idle — no live claim"
		if host.LastFinished != nil {
			sub = "idle — last active " + humanizeSince(now.Sub(*host.LastFinished))
		}
		v.State = serverState{Label: "IDLE", Class: "state-idle", Dot: "blue", Sub: sub}
	default:
		v.State = serverState{Label: "NOT SEEN", Class: "state-unknown", Dot: "grey",
			Sub: "configured — no activity observed yet"}
	}
	v.State.Token = strings.ReplaceAll(strings.ToLower(v.State.Label), " ", "_")

	// Model & mode: observed run wins; else the configured expectation.
	if host != nil && host.ASRModel != nil && *host.ASRModel != "" {
		v.Model, v.ModelSource = *host.ASRModel, "observed"
	} else if configuredModel != "" {
		v.Model, v.ModelSource = configuredModel, "configured"
	}
	v.ModelSize = modelSize(v.Model)
	if host != nil {
		if host.ComputeType != nil {
			v.ComputeMode = *host.ComputeType
		}
		v.AvgProc = "—"
		if host.AvgProcessingSeconds != nil && *host.AvgProcessingSeconds > 0 {
			v.AvgProc = humanizeSeconds(*host.AvgProcessingSeconds)
		}
		if host.LastFinished != nil {
			v.LastActive = humanizeSince(now.Sub(*host.LastFinished))
		}
	}
	if v.AvgProc == "" {
		v.AvgProc = "—"
	}
	if v.LastActive == "" {
		v.LastActive = "—"
	}
	v.JobsDone = jobs
}

// ─── Templates ────────────────────────────────────────────────────────────────

var serversPage = mustPage(`{{define "content"}}
<p class="subtitle">transcription servers &nbsp;·&nbsp; auto-refreshes every 5 s</p>
<div id="conn" class="conn-lost" role="status" aria-live="polite" hidden>&#9888;&#xFE0F;&nbsp;connection lost — data below may be stale</div>
<div id="servers-region"
     hx-get="/servers/data" hx-trigger="load, every 5s" hx-swap="innerHTML"
     hx-sync="this:replace" hx-request='{"timeout": 5000}'
     hx-on::response-error="document.getElementById('conn').hidden = false"
     hx-on::send-error="document.getElementById('conn').hidden = false"
     hx-on::timeout="document.getElementById('conn').hidden = false"
     hx-on::after-request="if (event.detail.successful) document.getElementById('conn').hidden = true">
  <p class="htmx-indicator">loading…</p>
</div>
{{end}}`)

var serversFragmentTmpl = template.Must(template.New("servers").Funcs(tmplFuncs).Parse(`
<div class="updated">updated {{.RenderedAt}}</div>

{{if not .Servers}}
<p class="lib-empty">No servers configured or observed yet. Set <code>ASR_SERVERS</code> to declare your transcription servers, or wait for a runner to claim its first job.</p>
{{else}}

<div class="section">
  <div class="section-title">Status ({{len .Servers}})</div>
  <div class="panels">
  {{range .Servers}}
    <div class="panel server-card {{.State.Class}}">
      <div class="server-head">
        <span class="server-name"><span class="dot {{.State.Dot}}"></span>{{.Name}}</span>
        {{if .Role}}<span class="badge role-{{.Role}}">{{.Role}}</span>{{end}}
        {{if not .Configured}}<span class="badge unconfigured" title="observed in the data but not in ASR_SERVERS">unconfigured</span>{{end}}
      </div>
      <div class="server-state {{.State.Class}}">{{.State.Label}}</div>
      <div class="server-sub">{{.State.Sub}}</div>
      {{if .Host}}<div class="server-host">{{.Host}}</div>{{end}}
    </div>
  {{end}}
  </div>
</div>

<div class="section">
  <div class="section-title">Models &amp; modes</div>
  <div class="table-wrap">
  <table>
    <thead><tr><th>Server</th><th>Model</th><th title="parameter size derived from the model name">Size</th><th title="compute precision the runner reported">Mode</th><th title="transcripts this server has produced">Jobs</th><th title="mean transcription wall-clock">Avg proc</th><th>Last active</th></tr></thead>
    <tbody>
    {{range .Servers}}
      <tr>
        <td>{{.Name}}{{if .Role}} <span class="time-muted">({{.Role}})</span>{{end}}</td>
        <td>{{if .Model}}{{.Model}}{{if eq .ModelSource "configured"}} <span class="time-muted" title="expected from ASR_SERVERS; no run has reported a model yet">(expected)</span>{{end}}{{else}}<span class="time-muted">—</span>{{end}}</td>
        <td class="time-muted">{{if .ModelSize}}{{.ModelSize}}{{else}}—{{end}}</td>
        <td class="time-muted">{{if .ComputeMode}}{{.ComputeMode}}{{else}}—{{end}}</td>
        <td class="time-muted">{{commafy .JobsDone}}</td>
        <td class="time-muted">{{.AvgProc}}</td>
        <td class="time-muted">{{.LastActive}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>

<p class="server-note">Servers with a <code>gpuArbiterUrl</code> show live readiness from gpu-arbiter: <em>ready</em> (GPU free), <em>busy</em> (GPU held by a game — connected but not usable, the fallback signal), or <em>offline</em> (host unreachable). Servers without a probe fall back to <em>idle</em>/<em>not&nbsp;seen</em> inferred from job history. Routing work to a specific server (primary/fallback by job type) is not yet implemented; the runner claims jobs itself.</p>
{{end}}
`))

// serversData backs the Servers fragment.
type serversData struct {
	RenderedAt string
	Servers    []serverView
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *MCPServer) handleServersPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, serversPage, pageShell{Title: "servers", Nav: "servers"})
}

func (s *MCPServer) handleServersData(w http.ResponseWriter, r *http.Request) {
	obs, err := s.db.GetServerObservation(r.Context())
	if err != nil {
		s.logger.Error("GetServerObservation error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := serversData{
		RenderedAt: time.Now().UTC().Format("15:04:05 UTC"),
		Servers:    buildServerViews(s.asrServers, obs, s.probeServers(r.Context()), time.Now(), s.runnerStaleAfter),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := serversFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("servers fragment render error", "error", err)
	}
}

// probeServers polls gpu-arbiter for every configured server that declares a
// gpuArbiterUrl, keyed by server Name. Returns nil when none are configured (or
// no prober is wired), so buildServerViews falls back to history-only inference.
// The prober caches per URL, so calling this on both render paths is cheap.
func (s *MCPServer) probeServers(ctx context.Context) map[string]arbiterStatus {
	if s.prober == nil {
		return nil
	}
	out := map[string]arbiterStatus{}
	for _, c := range s.asrServers {
		if c.GPUArbiterURL == "" {
			continue
		}
		out[c.Name] = s.prober.Probe(ctx, c.GPUArbiterURL)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
