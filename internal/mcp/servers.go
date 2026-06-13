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

	"github.com/jedwards1230/earmark/internal/asr"
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

	// Backend descriptor (CONTRACT §2.13). Family/Runtime resolve observed >
	// configured, same precedence as Model. *Source records which won so the
	// table can mark a config-only value "(expected)".
	Family        string // resolved family id ("" when neither observed nor configured)
	FamilySource  string // "observed" / "configured" / ""
	FamilyKnown   bool   // family is a recommended canonical id (cosmetic labeling only)
	Runtime       string // resolved runtime id ("" when neither observed nor configured)
	RuntimeSource string // "observed" / "configured" / ""
	RuntimeKnown  bool   // runtime is a recommended canonical id (cosmetic labeling only)

	// Caps is the compact capability strip (observed caps_applied wins over the
	// configured capabilities map). Empty when neither is known → "unknown" in UI.
	Caps       []capBadge
	CapsSource string // "observed" / "configured" / ""

	// MeanConfidence is the most-recent observed mean per-word confidence (0–1),
	// or nil when the backend emits no scores. Blank in the table when nil.
	MeanConfidence *float64

	// Live GPU readiness from gpu-arbiter (only when a gpuArbiterUrl is
	// configured for this server). Probed is false when no probe ran; the rest
	// are then zero. Exposed in the JSON API as the fallback-automation hook.
	Probed      bool
	Reachable   bool
	GPUState    string // "available" / "gaming" / "evicting" / "" (unknown)
	VRAMUsedMB  *int
	VRAMTotalMB *int
}

// capBadge is one entry in a server's capability strip: a short label, whether
// the capability was applied (true) or declined/absent (false), and the optional
// skipped-reason tooltip carried on a declined cap. It is the honest-degradation
// surface — a `bias✗` with a reason says "asked, but this backend declined", not
// "never asked".
type capBadge struct {
	Key     string // the closed-enum capability key, e.g. "context_biasing"
	Label   string // compact strip label, e.g. "bias" / "words" / "diar"
	Applied bool   // true → applied this run / advertised; false → declined/absent
	Reason  string // skipped-reason tooltip (only meaningful when !Applied)
}

// capsMap renders the resolved capability strip back to a plain {key: bool} map
// for the JSON API (the applied-or-declared map, §8.2). Returns nil when the
// server has no capability data so the field stays omitempty.
func (v serverView) capsMap() map[string]bool {
	if len(v.Caps) == 0 {
		return nil
	}
	m := make(map[string]bool, len(v.Caps))
	for _, b := range v.Caps {
		m[b.Key] = b.Applied
	}
	return m
}

// capsSkippedReasons returns the key→reason map for declined capabilities (only
// observed rows carry reasons), for the JSON API. Nil when none.
func (v serverView) capsSkippedReasons() map[string]string {
	var m map[string]string
	for _, b := range v.Caps {
		if !b.Applied && b.Reason != "" {
			if m == nil {
				m = map[string]string{}
			}
			m[b.Key] = b.Reason
		}
	}
	return m
}

// capStripOrder fixes the badge order so two backends are visually comparable
// (a stable left-to-right reading), and capLabels gives each key a compact label.
var capStripOrder = []asr.Capability{
	asr.CapWordTimestamps,
	asr.CapContextBiasing,
	asr.CapDiarization,
	asr.CapConfidenceScores,
	asr.CapLanguageDetection,
}

var capLabels = map[asr.Capability]string{
	asr.CapWordTimestamps:    "words",
	asr.CapContextBiasing:    "bias",
	asr.CapDiarization:       "diar",
	asr.CapConfidenceScores:  "conf",
	asr.CapLanguageDetection: "lang",
}

// buildCapBadges renders the capability strip from a capability map (either the
// observed caps_applied or the configured declaration) plus the optional
// skipped-reason map (only present on observed rows). It emits a badge only for
// keys present in caps, in the stable strip order, so an unknown/absent
// capability simply doesn't appear (rendered as "unknown" upstream when the whole
// map is nil). reasons attaches a tooltip to a declined (false) cap.
func buildCapBadges(caps asr.Capabilities, reasons map[string]string) []capBadge {
	if len(caps) == 0 {
		return nil
	}
	out := make([]capBadge, 0, len(caps))
	for _, k := range capStripOrder {
		applied, ok := caps[k]
		if !ok {
			continue
		}
		b := capBadge{Key: string(k), Label: capLabels[k], Applied: applied}
		if !applied && reasons != nil {
			b.Reason = reasons[string(k)]
		}
		out = append(out, b)
	}
	return out
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
		applyObserved(&v, live, host, probeFor(probes, c.Name), jobs, c, now, staleAfter)
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
		applyObserved(&v, &lr, host, nil, jobs, config.ASRServer{}, now, staleAfter)
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
		applyObserved(&v, nil, &h, nil, h.JobsDone, config.ASRServer{}, now, staleAfter)
		views = append(views, v)
	}

	return views
}

// familyGroup is one family's rows in the Models & modes table: a header label
// plus the servers resolved to that family. Grouping puts the two backends of an
// A/B side by side under one heading so they read as comparable.
type familyGroup struct {
	Family  string // resolved family id, or "" for the "unknown family" bucket
	Label   string // display label ("unknown" when Family == "")
	Known   bool   // family is a recommended canonical id (cosmetic)
	Servers []serverView
}

// groupByFamily buckets server views by their resolved family, preserving the
// first-seen order of both families and servers (deterministic for tests and a
// stable render). Servers with no family land in a trailing "unknown" group so
// nothing is hidden. With zero or one distinct family the result is a single
// group — the template then skips the per-family headers and renders a flat
// table, matching today's look when family data is absent.
func groupByFamily(views []serverView) []familyGroup {
	groups := make([]familyGroup, 0, 4)
	idx := map[string]int{}
	for _, v := range views {
		key := v.Family // "" → the unknown bucket
		i, ok := idx[key]
		if !ok {
			label := v.Family
			if label == "" {
				label = "unknown"
			}
			groups = append(groups, familyGroup{Family: v.Family, Label: label, Known: v.FamilyKnown})
			i = len(groups) - 1
			idx[key] = i
		}
		groups[i].Servers = append(groups[i].Servers, v)
	}
	return groups
}

// multiFamily reports whether the grouped table should show per-family headers
// (more than one distinct family resolved). With ≤1 family the table renders
// flat, preserving the pre-Phase-2 appearance when no family data exists.
func multiFamily(groups []familyGroup) bool { return len(groups) > 1 }

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
func applyObserved(v *serverView, live *db.LiveRunner, host *db.HostMetrics, probe *arbiterStatus, jobs int, cfg config.ASRServer, now time.Time, staleAfter time.Duration) {
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
	} else if cfg.Model != "" {
		v.Model, v.ModelSource = cfg.Model, "configured"
	}
	v.ModelSize = modelSize(v.Model)

	// Family / Runtime: same observed > configured precedence as Model. *Known is
	// purely cosmetic (recommended canonical ids get a curated label); an unknown
	// value still renders verbatim.
	if host != nil && host.ASRFamily != nil && *host.ASRFamily != "" {
		v.Family, v.FamilySource = *host.ASRFamily, "observed"
	} else if cfg.Family != "" {
		v.Family, v.FamilySource = cfg.Family, "configured"
	}
	v.FamilyKnown = v.Family != "" && asr.KnownFamily(v.Family)
	if host != nil && host.ASRRuntime != nil && *host.ASRRuntime != "" {
		v.Runtime, v.RuntimeSource = *host.ASRRuntime, "observed"
	} else if cfg.Runtime != "" {
		v.Runtime, v.RuntimeSource = cfg.Runtime, "configured"
	}
	v.RuntimeKnown = v.Runtime != "" && asr.KnownRuntime(v.Runtime)

	// Capability strip: observed caps_applied wins (and carries skipped reasons);
	// else the configured declaration (no reasons — config has no run to decline).
	if host != nil && len(host.CapsApplied) > 0 {
		v.Caps, v.CapsSource = buildCapBadges(host.CapsApplied, host.CapsSkippedReason), "observed"
	} else if len(cfg.Capabilities) > 0 {
		v.Caps, v.CapsSource = buildCapBadges(cfg.Capabilities, nil), "configured"
	}

	// Mean word confidence is observed-only (a per-run quality signal); nil when
	// the backend emits no scores → blank in the table.
	if host != nil {
		v.MeanConfidence = host.MeanWordConfidence
	}

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
    <thead><tr>
      <th>Server</th>
      <th>Model</th>
      <th title="parameter size derived from the model name">Size</th>
      <th title="model family — observed from run_metrics, else the configured expectation">Family</th>
      <th title="runtime — observed from run_metrics, else the configured expectation">Runtime</th>
      <th title="capabilities this backend applied (observed) or declares (configured); ✗ = requested-but-declined or absent, hover for why">Caps</th>
      <th title="compute precision the runner reported">Mode</th>
      <th title="mean per-word confidence the model reported (blank when it emits no scores)">Conf</th>
      <th title="transcripts this server has produced">Jobs</th>
      <th title="mean transcription wall-clock">Avg proc</th>
      <th>Last active</th>
    </tr></thead>
    <tbody>
    {{if .MultiFamily}}
      {{range .FamilyGroups}}
        <tr class="family-row"><td colspan="11"><span class="family-label{{if .Known}} family-known{{end}}">{{.Label}}</span> <span class="time-muted">({{len .Servers}})</span></td></tr>
        {{range .Servers}}{{template "serverModesRow" .}}{{end}}
      {{end}}
    {{else}}
      {{range .Servers}}{{template "serverModesRow" .}}{{end}}
    {{end}}
    </tbody>
  </table>
  </div>
</div>

<p class="server-note">Models &amp; modes prefers <em>observed</em> values (what a run actually reported in <code>run_metrics</code>) over the <em>configured</em> <code>ASR_SERVERS</code> expectation, marked <em>(expected)</em> until a run reports. Capability badges show what each backend applied: <code class="cap-badge cap-on">words</code> = applied, <code class="cap-badge cap-off">bias✗</code> = requested but declined or absent (hover for the reason). This is the honest-degradation surface for A/B comparison — two backends on the same library are grouped by family side&nbsp;by&nbsp;side.</p>

<p class="server-note">Servers with a <code>gpuArbiterUrl</code> show live readiness from gpu-arbiter: <em>ready</em> (GPU free), <em>busy</em> (GPU held by a game — connected but not usable, the fallback signal), or <em>offline</em> (host unreachable). Servers without a probe fall back to <em>idle</em>/<em>not&nbsp;seen</em> inferred from job history. Routing work to a specific server (primary/fallback by job type) is not yet implemented; the runner claims jobs itself.</p>
{{end}}

{{define "serverModesRow"}}
<tr>
  <td>{{.Name}}{{if .Role}} <span class="time-muted">({{.Role}})</span>{{end}}</td>
  <td>{{if .Model}}{{.Model}}{{if eq .ModelSource "configured"}} <span class="time-muted" title="expected from ASR_SERVERS; no run has reported a model yet">(expected)</span>{{end}}{{else}}<span class="time-muted">—</span>{{end}}</td>
  <td class="time-muted">{{if .ModelSize}}{{.ModelSize}}{{else}}—{{end}}</td>
  <td class="time-muted">{{if .Family}}<span{{if .FamilyKnown}} class="family-known"{{end}}>{{.Family}}</span>{{if eq .FamilySource "configured"}} <span class="time-muted" title="expected from ASR_SERVERS; no run has reported a family yet">(expected)</span>{{end}}{{else}}<span title="no family observed or configured">unknown</span>{{end}}</td>
  <td class="time-muted">{{if .Runtime}}<span{{if .RuntimeKnown}} class="family-known"{{end}}>{{.Runtime}}</span>{{if eq .RuntimeSource "configured"}} <span class="time-muted" title="expected from ASR_SERVERS; no run has reported a runtime yet">(expected)</span>{{end}}{{else}}<span title="no runtime observed or configured">unknown</span>{{end}}</td>
  <td>
    {{if .Caps}}<span class="cap-strip" title="{{if eq .CapsSource "configured"}}declared in ASR_SERVERS (no run has reported applied caps yet){{else}}applied by the most recent run{{end}}">
      {{range .Caps}}<span class="cap-badge {{if .Applied}}cap-on{{else}}cap-off{{end}}"{{if and (not .Applied) .Reason}} title="{{.Key}}: {{.Reason}}"{{end}}>{{.Label}}{{if not .Applied}}✗{{end}}</span>{{end}}
    </span>{{else}}<span class="time-muted" title="no capabilities observed or configured">unknown</span>{{end}}
  </td>
  <td class="time-muted">{{if .ComputeMode}}{{.ComputeMode}}{{else}}—{{end}}</td>
  <td class="time-muted">{{confPct .MeanConfidence}}</td>
  <td class="time-muted">{{commafy .JobsDone}}</td>
  <td class="time-muted">{{.AvgProc}}</td>
  <td class="time-muted">{{.LastActive}}</td>
</tr>
{{end}}
`))

// serversData backs the Servers fragment. Servers is the flat list (Status
// panels); FamilyGroups is the same set bucketed by family for the Models &
// modes table; MultiFamily toggles the per-family headers.
type serversData struct {
	RenderedAt   string
	Servers      []serverView
	FamilyGroups []familyGroup
	MultiFamily  bool
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
	views := buildServerViews(s.asrServers, obs, s.probeServers(r.Context()), time.Now(), s.runnerStaleAfter)
	groups := groupByFamily(views)
	data := serversData{
		RenderedAt:   time.Now().UTC().Format("15:04:05 UTC"),
		Servers:      views,
		FamilyGroups: groups,
		MultiFamily:  multiFamily(groups),
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
