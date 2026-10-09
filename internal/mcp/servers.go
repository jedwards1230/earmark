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

// ─── ASR runners (Models page) ────────────────────────────────────────────────
//
// The Models page's ASR runners section answers: which transcription servers
// (ASR runners) are configured for this deployment and which are live right
// now, and what model / compute mode each is actually running. The role board
// (models.go) aggregates these states into the ASR role card. Read-only.
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
	Glyph string // ✓ ▲ ✗ ○ ? — paired with the word so state is never color alone
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
	// LastActiveAt is the absolute last completion (zero when unknown), for the
	// "last active" cell's title.
	LastActiveAt time.Time

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

// AppliedCaps is the capability strip trimmed to what the backend supports
// (applied/advertised); the full list, declined ones included, is CapsTitle.
func (v serverView) AppliedCaps() []capBadge {
	var out []capBadge
	for _, b := range v.Caps {
		if b.Applied {
			out = append(out, b)
		}
	}
	return out
}

// CapsTitle renders every known capability for the Caps cell's tooltip:
// "words ✓ · bias ✗ (reason) · …", prefixed with where the data came from.
func (v serverView) CapsTitle() string {
	src := "applied by the most recent run"
	if v.CapsSource == "configured" {
		src = "declared in ASR_SERVERS (no run has reported applied caps yet)"
	}
	parts := make([]string, 0, len(v.Caps))
	for _, b := range v.Caps {
		p := b.Label + " ✓"
		if !b.Applied {
			p = b.Label + " ✗"
			if b.Reason != "" {
				p += " (" + b.Reason + ")"
			}
		}
		parts = append(parts, p)
	}
	return src + ": " + strings.Join(parts, " · ")
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

// runnerHistoryWindow bounds which UNconfigured runner hosts appear from
// run_metrics history alone: only those that finished a transcription within
// the window. A host retired months ago (whose rows the eval backfill still
// touches) otherwise lingers as an "unconfigured" card forever. Configured
// ASR_SERVERS entries always show, and an unconfigured runner holding a live
// claim always shows — the window only filters history-only hosts.
const runnerHistoryWindow = 30 * 24 * time.Hour

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

	// Unconfigured hosts with only historical metrics (no live claim), when they
	// transcribed within runnerHistoryWindow.
	for i, h := range obs.Hosts {
		if hostUsed[i] {
			continue
		}
		hostUsed[i] = true
		if !recentlyTranscribed(h, now) {
			continue
		}
		h := h
		v := serverView{Name: h.Host, Configured: false}
		applyObserved(&v, nil, &h, nil, h.JobsDone, config.ASRServer{}, now, staleAfter)
		views = append(views, v)
	}

	return views
}

// recentlyTranscribed reports whether a host finished a transcription within
// runnerHistoryWindow of now. A host with no finished transcription is not.
func recentlyTranscribed(h db.HostMetrics, now time.Time) bool {
	return h.LastFinished != nil && now.Sub(*h.LastFinished) <= runnerHistoryWindow
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
		v.State = serverState{Label: "TRANSCRIBING", Glyph: "✓", Class: "state-running", Dot: "green", Sub: sub}
	case live != nil:
		v.State = serverState{Label: "STALLED", Glyph: "✗", Class: "state-stalled", Dot: "red",
			Sub: "claim heartbeat stale (" + humanizeSince(now.Sub(live.LastHeartbeat)) + ") — runner may have crashed"}
	case probe != nil && !probe.Reachable:
		v.State = serverState{Label: "OFFLINE", Glyph: "✗", Class: "state-offline", Dot: "grey",
			Sub: "host unreachable (gpu-arbiter not responding)"}
	case probe != nil && probe.ready():
		v.State = serverState{Label: "READY", Glyph: "✓", Class: "state-running", Dot: "green",
			Sub: "connected — GPU available" + vramSuffix(probe)}
	case probe != nil:
		v.State = serverState{Label: "BUSY", Glyph: "▲", Class: "state-busy", Dot: "amber",
			Sub: busySubtext(probe)}
	case host != nil && host.JobsDone > 0:
		sub := "idle — no live claim"
		if host.LastFinished != nil {
			sub = "idle — last active " + humanizeSince(now.Sub(*host.LastFinished))
		}
		v.State = serverState{Label: "IDLE", Glyph: "●", Class: "state-idle", Dot: "blue", Sub: sub}
	default:
		v.State = serverState{Label: "NOT SEEN", Glyph: "?", Class: "state-unknown", Dot: "grey",
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
			v.LastActiveAt = *host.LastFinished
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

// serversPage is the static Models shell. Only #models-region is polled; the
// About text and the runner-update form live here, OUTSIDE the region, so a
// poll can neither collapse About nor wipe a version the operator is typing.
// The form follows the region directly, under its last section (ASR runners).
var serversPage = mustPage(`{{define "content"}}
<p class="subtitle">Each pipeline role: what is configured, what actually answered, whether it is healthy, and how much output predates the current recipe.</p>
<div id="conn" class="conn-lost" role="status" aria-live="polite" hidden>&#9888;&#xFE0F;&nbsp;connection lost — data below may be stale</div>
<div id="models-region"
     hx-get="/servers/data" hx-trigger="load, every 5s" hx-swap="innerHTML"
     hx-sync="this:replace" hx-config='{"timeout": 5000}' hx-status:5xx="swap:none"
     hx-on::response:error="document.getElementById('conn').hidden = false"
     hx-on::error="document.getElementById('conn').hidden = false"
     hx-on::after:request="if (event.detail.ctx.response?.status < 400) document.getElementById('conn').hidden = true">
  <p class="htmx-indicator">loading…</p>
</div>
<section class="section" aria-labelledby="runner-update-title">
  <h2 class="section-title" id="runner-update-title">Update ASR runner</h2>
  {{if .ControlEnabled}}
  <div class="rb-form runner-update-form">
    <form class="rb-form" hx-post="/actions/runner-update" hx-target="#models-region" hx-swap="innerHTML">
      <label for="runner-version" class="time-muted">version</label>
      <input id="runner-version" type="text" name="version" placeholder="vX.Y.Z" autocomplete="off" spellcheck="false" required />
      <button class="btn btn-primary" type="submit">Update runner</button>
    </form>
    <!-- Outside the form on purpose: clearing must never carry the typed version. -->
    <button class="btn" type="button" hx-post="/actions/runner-update" hx-vals='{"version": ""}' hx-target="#models-region" hx-swap="innerHTML" title="cancel or acknowledge the requested update">Clear request</button>
  </div>
  <p class="server-note">Requests the runner self-update to a release tag; the runner performs the swap. Current and requested versions are shown under ASR runners above.</p>
  {{else}}<p class="server-note">Set <code>CONTROL_API_TOKEN</code> to enable runner updates.</p>{{end}}
</section>
<details class="about-page">
  <summary>About this page</summary>
  <p class="server-note"><strong>Roles.</strong> One card per configured pipeline role; roles with no binding are listed on one line under the cards. <em>requested</em> is the model id earmark sends; <em>pinned</em> (shown only when set) is the <code>MODELS_FILE</code> expectation; <em>answered</em> is what the endpoint reported serving the call. A different answer (≠ expected) usually means a gateway fallback, and its output is stamped with a different recipe — i.e. stale. Only an answer newer than the step's current recipe is compared: after a model or prompt change, a role with no calls since reads <em>no calls since the model changed</em> (with the previous answer), not ≠ expected. Role health folds in call outcomes, so a gateway that lists the model but fails every call shows <em>FAILING</em> here while its endpoint row still reads <em>lists model</em>. <em>HEALTHY · idle</em> is a working role with nothing to do.</p>
  <p class="server-note"><em>last ok</em> on the Judge card is the newest judge success from any source (the hourly backfill CronJob, in-pipeline judging, the dashboard). earmark records no per-backfill-run marker.</p>
  <p class="server-note"><strong>Decide and Scan</strong> run through System One (<code>AI_ROLES.decide</code> / <code>AI_ROLES.scan</code>) on demand — <code>earmark decide</code>, <code>earmark scan</code> — so a quiet card is idle, not degraded. Their evidence is the <code>fn_calls</code> log under the current recipe (its function, model and prompt): last ok, last fail and its error class, failures in 24&thinsp;h, fallbacks (a reply from another model is stored, never served), and cache hits. <em>decided</em> splits judge findings by who decided them: Jev (<code>decided_by</code> <code>jev:&lt;recipe&gt;</code>), humans (<code>mcp:</code>, <code>cli:</code>, anything else, or unattributed), and undone (<code>revert:jev:&lt;recipe&gt;</code>); the Judge output table splits <em>Decided</em> the same way.</p>
  <p class="server-note"><strong>Recipes &amp; stale work.</strong> A recipe is everything that determined a step's output (code, model, prompt). Stale rows were made under any recipe other than the current one; legacy rows from before provenance count as stale by design and converge as the work is redone. Only steps with a current recipe are tracked; the rest are listed on one <em>Not tracked</em> line. Recipes and activity are cached for 30&thinsp;s; stale counts scan the whole library, so they are cached for 5&thinsp;min. The page itself refreshes every 5&thinsp;s.</p>
  <p class="server-note"><strong>LiteLLM gateway.</strong> For endpoints behind LiteLLM, earmark reads the proxy's readiness and its own virtual key's <code>/key/info</code> (alias, allowed models, spend, budget, limits) with that key — never the master key, and never a model call. A role whose model is not on the key's allowlist is DEGRADED: every call to it 403s; each role's model is listed on the gateway card only when one is not allowed (or cannot be checked).</p>
  <p class="server-note"><strong>AI endpoints.</strong> The <code>AI_ENDPOINTS</code> registry (<code>AI_ROLES</code> binds each to a role). Liveness is a <code>GET /models</code> probe only: <em>lists model</em>, <em>model not listed</em> (behind LiteLLM: <em>not allowed</em> — not on the key allowlist), or <em>unreachable</em>. <em>Gateway</em> is the declared <code>gateway</code> field, or LiteLLM inferred from the host name. The legacy <code>EMBEDDINGS_BASE_URL</code>/<code>EMBEDDINGS_MODEL</code> vars appear as a synthesized <code>_legacy</code> endpoint; a judge configured from <code>EVAL_CHAT_*</code> appears as an unprobed <code>EVAL_CHAT_*</code> row.</p>
  <p class="server-note"><strong>ASR runners.</strong> The model table prefers <em>observed</em> values (what a run reported in <code>run_metrics</code>) over the <em>configured</em> <code>ASR_SERVERS</code> expectation, marked <em>(expected)</em> until a run reports. <em>Caps</em> lists the capabilities a backend supports; hover for the full list, declined ones and why. A runner host that is not in <code>ASR_SERVERS</code> appears only while it holds a claim or transcribed in the last 30 days. Servers with a <code>gpuArbiterUrl</code> show live readiness from gpu-arbiter: <em>ready</em>, <em>busy</em> (GPU held by a game, or free with the asr-runner stopped), or <em>offline</em>; others fall back to <em>idle</em>/<em>not&nbsp;seen</em> inferred from job history. The runner claims jobs itself — nothing here routes work.</p>
</details>
{{end}}`)

var serversFragmentTmpl = template.Must(template.New("models").Funcs(tmplFuncs).Funcs(modelsFuncs).Parse(`
<div class="updated">updated {{.RenderedAt}}{{if .CountsErr}} · <span class="err">counts unavailable{{if .CountsAge}} (last good <span title="{{.CountsAt}}">{{.CountsAge}}</span>){{end}}</span>{{else if .CountsAge}} · counts as of <span title="{{.CountsAt}}">{{.CountsAge}}</span>{{end}}{{if .StaleErr}} · <span class="err">stale counts unavailable{{if .StaleAge}} (last good <span title="{{.StaleAt}}">{{.StaleAge}}</span>){{end}}</span>{{else if .StaleAge}} · stale counts as of <span title="{{.StaleAt}}">{{.StaleAge}}</span>{{else if .StalePending}} · stale counts loading…{{end}}</div>

<section class="section" aria-labelledby="roles-title">
  <h2 class="section-title" id="roles-title">Roles</h2>
  {{with .RoleCards}}<div class="panels role-panels">
  {{range .}}{{template "roleCard" .}}{{end}}
  </div>{{end}}
  {{with .UnconfiguredRoles}}<p class="server-note roles-unconfigured">○ Not configured: {{range $i, $r := .}}{{if $i}}, {{end}}{{$r.Title}}{{if $r.HasModel}} <span class="time-muted">({{$r.Health.Sub}}{{if $r.HumanDecided}} · decided by humans {{commafyPtr $r.HumanDecided}}{{if $r.JevDecided}} · by Jev {{commafy $r.JevDecided}}{{end}}{{end}})</span>{{end}}{{end}}</p>{{end}}
</section>

<section class="section" aria-labelledby="recipes-title">
  <h2 class="section-title" id="recipes-title">Recipes &amp; stale work</h2>
  {{if not .CountsKnown}}<p class="lib-empty">counts unavailable — see server logs</p>
  {{else}}
  {{if .Recipes}}
  <p class="server-note table-lead" id="recipes-note">Current recipe per tracked step and the output rows made under any other recipe. Legacy rows (pre-provenance) count as stale by design and converge as work is redone.</p>
  <div class="table-wrap">
  <table class="recipes-table" aria-labelledby="recipes-note">
    <thead><tr>
      <th scope="col">Step</th>
      <th scope="col">Role</th>
      <th scope="col" class="num">Stale rows</th>
      <th scope="col">Current recipe</th>
      <th scope="col">Model</th>
      <th scope="col" class="hide-sm">Prompt</th>
      <th scope="col" class="hide-sm">Since</th>
    </tr></thead>
    <tbody>
    {{range .Recipes}}
    <tr id="recipe-{{.Step}}">
      <th scope="row" class="mono">{{.Step}}</th>
      <td>{{if .RoleTitle}}{{.RoleTitle}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if not .StaleKnown}}<span class="time-muted">{{if $.StalePending}}counting…{{else}}—{{end}}</span>{{else if .Stale}}<strong{{with .ConvergeNote}} title="converges {{.}}"{{end}}>{{commafy64Ptr .Stale}}</strong>{{with .ConvergeNote}}<span class="sr-only"> — converges {{.}}</span>{{end}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="mono" title="{{.RecipeID}}">{{.RecipeShort}}</td>
      <td class="mono">{{or .Model "—"}}</td>
      <td class="hide-sm">{{or .PromptVersion "—"}}</td>
      <td class="time-muted hide-sm" title="{{formatTime .Since}}">{{relTime .Since}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p class="lib-empty">No current recipes yet. Current recipes are registered by <code>earmark monitor</code> at startup.</p>{{end}}
  {{with .UntrackedSteps}}<p class="server-note time-muted">Not tracked: {{range $i, $s := .}}{{if $i}}, {{end}}{{$s}}{{end}}</p>{{end}}
  {{end}}
</section>

{{if .Gateways}}
<section class="section" aria-labelledby="gateway-title">
  <h2 class="section-title" id="gateway-title">LiteLLM gateway{{if gt (len .Gateways) 1}}s ({{len .Gateways}}){{end}}</h2>
  <div class="panels role-panels">
  {{range .Gateways}}
    <div class="panel server-card {{.Class}}">
      <div class="server-head">
        <span class="server-name"><span class="dot {{.Dot}}"></span><span class="mono">{{.Host}}</span></span>
        {{with .Version}}<span class="step-chip" title="litellm_version">v{{.}}</span>{{end}}
      </div>
      <div class="server-state {{.Class}}">{{.StateLabel}}</div>
      <div class="server-sub">{{.Sub}}</div>
      {{range .Warnings}}<div class="server-sub err">{{.}}</div>{{end}}
      <dl class="role-kv">
        <dt>used by</dt><dd>{{range $i, $e := .UsedBy}}{{if $i}}, {{end}}{{$e}}{{end}}</dd>
        {{if .KeyInfoOK}}
        <dt>key</dt><dd><span class="mono">{{or .Key.KeyAlias "(no alias)"}}</span> · {{.KeyState}}{{with .ExpiresText}} · {{.}}{{end}}</dd>
        <dt>spend</dt><dd>{{.SpendText}}</dd>
        {{with .LimitsText}}<dt>limits</dt><dd>{{.}}</dd>{{end}}
        <dt>allowed</dt><dd>{{if .TeamModels}}<span class="time-muted">the team's models (not readable by earmark's key)</span>{{else if .AllowAll}}all models{{else}}{{range $i, $m := .Key.Models}}{{if $i}}, {{end}}<span class="mono">{{$m}}</span>{{end}}{{end}}</dd>
        {{else}}
        <dt>key</dt><dd class="time-muted">{{.KeyInfoErr}}</dd>
        {{end}}
        {{if .ShowRoleModels}}{{range .RoleModels}}<dt>{{.Role}}</dt><dd><span class="mono">{{.Model}}</span> <span class="{{.Class}}">{{.Mark}}</span></dd>{{end}}{{end}}
      </dl>
    </div>
  {{end}}
  </div>
</section>
{{end}}

<section class="section" aria-labelledby="endpoints-title">
  <h2 class="section-title" id="endpoints-title">AI endpoints ({{.EndpointCount}})</h2>
  {{if or .Endpoints .EvalEnvRow}}
  <p class="server-note table-lead" id="endpoints-note">The AI_ENDPOINTS registry. Liveness here is GET /models only; role health above includes call outcomes.</p>
  <div class="table-wrap">
  <table aria-labelledby="endpoints-note">
    <thead><tr>
      <th scope="col">ID</th>
      <th scope="col">Role</th>
      <th scope="col">Type</th>
      <th scope="col">Gateway</th>
      <th scope="col">Host</th>
      <th scope="col">Model</th>
      <th scope="col" title="GET /models only — not call success">Liveness</th>
      {{if .ShowOptions}}<th scope="col">Options</th>{{end}}
    </tr></thead>
    <tbody>
    {{range .Endpoints}}{{template "endpointRow" (endpointRow . $.ShowOptions)}}{{end}}
    {{with .EvalEnvRow}}{{template "endpointRow" (endpointRow . $.ShowOptions)}}{{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p class="lib-empty">No AI endpoints configured.</p>{{end}}
</section>

<section class="section" aria-labelledby="judge-output-title">
  <h2 class="section-title" id="judge-output-title">Judge output</h2>
  {{if not .CountsKnown}}<p class="lib-empty">counts unavailable — see server logs</p>
  {{else if not .JudgeOutput}}<p class="lib-empty">no judge findings yet</p>
  {{else}}
  <p class="server-note table-lead" id="judge-output-note">Judge findings by the model that answered.</p>
  <div class="table-wrap">
  <table aria-labelledby="judge-output-note">
    <thead><tr>
      <th scope="col">Answered by</th>
      <th scope="col" class="num">Proposed</th>
      <th scope="col" class="num">Unanchorable</th>
      <th scope="col" class="num" title="accepted + rejected + applied + reverted, decided by a person (mcp:, cli:, other, or unattributed)">Decided · human</th>
      <th scope="col" class="num" title="accepted + rejected + applied + reverted by the decide step (jev:&lt;recipe&gt;), incl. its undos (revert:jev:&lt;recipe&gt;)">Decided · Jev</th>
      <th scope="col" class="num" title="superseded by a re-transcribe, plus the patch state 'stale' (a replay quarantine — not stale_work)">Superseded / other</th>
      <th scope="col" class="num">Total</th>
    </tr></thead>
    <tbody>
    {{range .JudgeOutput}}
    <tr>
      <th scope="row" class="mono">{{.Model}}</th>
      <td class="num">{{commafy .Proposed}}</td>
      <td class="num">{{commafy .Unanchorable}}</td>
      <td class="num">{{commafy .DecidedHuman}}</td>
      <td class="num">{{commafy .DecidedJev}}</td>
      <td class="num">{{commafy .Other}}</td>
      <td class="num">{{commafy .Total}}</td>
    </tr>
    {{end}}
    </tbody>
    {{with .JudgeTotals}}
    <tfoot><tr>
      <th scope="row">total</th>
      <td class="num">{{commafy .Proposed}}</td>
      <td class="num">{{commafy .Unanchorable}}</td>
      <td class="num">{{commafy .DecidedHuman}}</td>
      <td class="num">{{commafy .DecidedJev}}</td>
      <td class="num">{{commafy .Other}}</td>
      <td class="num">{{commafy .Total}}</td>
    </tr></tfoot>
    {{end}}
  </table>
  </div>
  {{end}}
</section>

<section class="section" aria-labelledby="runners-title">
  <h2 class="section-title" id="runners-title">ASR runners ({{len .Runners}})</h2>
  {{if .Runners}}
  <div class="panels">
  {{range .Runners}}
    <div class="panel server-card {{.State.Class}}">
      <div class="server-head">
        <span class="server-name"><span class="dot {{.State.Dot}}"></span>{{.Name}}</span>
        {{if .Role}}<span class="badge role-{{.Role}}">{{.Role}}</span>{{end}}
        {{if not .Configured}}<span class="badge unconfigured" title="observed in the data but not in ASR_SERVERS">unconfigured</span>{{end}}
      </div>
      <div class="server-state {{.State.Class}}">{{.State.Glyph}} {{.State.Label}}</div>
      <div class="server-sub">{{.State.Sub}}</div>
      {{if and .Host (ne .Host .Name)}}<div class="server-host">{{.Host}}</div>{{end}}
    </div>
  {{end}}
  </div>
  {{else}}
  <p class="lib-empty">No ASR runners configured or observed yet. Set <code>ASR_SERVERS</code> to declare your transcription servers, or wait for a runner to claim its first job.</p>
  {{end}}
  {{with .RunnerUpdate}}
  <div class="server-sub runner-version">runner version: running <code>{{if .Running}}{{.Running}}{{else}}unknown{{end}}</code>{{if .Desired}} · requested <code>{{.Desired}}</code>{{if and .State (not .Available)}} · {{.State}}{{end}}{{end}}{{if .Available}} <span class="badge mismatch" title="a different version is requested">update {{if eq .State "updating"}}in progress{{else if eq .State "failed"}}failed{{else}}requested{{end}}</span>{{end}}</div>
  {{with .Error}}<div class="server-sub err mono err-clamp" title="{{.}}">{{.}}</div>{{end}}
  {{end}}

  {{if .Runners}}
  <p class="server-note table-lead" id="runners-note">Model, runtime, and capabilities per runner — observed values win over configured.</p>
  <div class="table-wrap">
  <table aria-labelledby="runners-note">
    <thead><tr>
      <th scope="col">Server</th>
      <th scope="col">Model</th>
      {{if .ShowRuntime}}<th scope="col" title="runtime — observed from run_metrics, else the configured expectation">Runtime</th>{{end}}
      {{if .ShowCaps}}<th scope="col" title="capabilities this backend applied (observed) or declares (configured); hover for the full list">Caps</th>{{end}}
      <th scope="col" title="compute precision the runner reported">Precision</th>
      <th scope="col" class="num" title="mean per-word confidence the model reported (blank when it emits no scores)">Conf</th>
      <th scope="col" class="num" title="transcripts this server has produced">Jobs</th>
      <th scope="col" class="num" title="mean transcription wall-clock per job">Avg / job</th>
      <th scope="col" title="last finished transcription">Last active</th>
    </tr></thead>
    <tbody>
    {{range .Runners}}{{template "serverModesRow" (runnerRow . $.ShowRuntime $.ShowCaps)}}{{end}}
    </tbody>
  </table>
  </div>
  {{end}}

  {{if not .CountsKnown}}<p class="lib-empty">transcript provenance: counts unavailable — see server logs</p>
  {{else if not .ASRGroups}}<p class="lib-empty">transcript provenance: no transcripts yet</p>
  {{else}}
  <p class="server-note table-lead" id="provenance-note">Which runner build and model file produced transcripts, newest first.</p>
  <div class="table-wrap">
  <table aria-labelledby="provenance-note">
    <thead><tr>
      <th scope="col">Model</th>
      <th scope="col">Runner</th>
      <th scope="col">.nemo sha256</th>
      <th scope="col" class="num">Transcripts</th>
      <th scope="col">First</th>
      <th scope="col">Last</th>
    </tr></thead>
    <tbody>
    {{range .ASRGroups}}
    <tr>
      <th scope="row" class="mono">{{.Model}}</th>
      {{if .Reported}}
      <td class="mono">{{.RunnerVersion}}</td>
      <td class="mono"{{if .SHA}} title="{{.SHA}}"{{end}}>{{or .SHAShort "—"}}</td>
      {{else}}
      <td colspan="2" class="time-muted"><em>not reported (pre-provenance runner)</em></td>
      {{end}}
      <td class="num">{{commafy .Count}}</td>
      <td class="time-muted" title="{{formatTime .First}}">{{relTime .First}}</td>
      <td class="time-muted" title="{{formatTime .Last}}">{{relTime .Last}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{end}}
</section>

{{define "roleCard"}}
<div class="panel server-card role-card {{.Health.Class}}" id="role-{{.Key}}">
  <div class="server-head">
    <span class="server-name"><span class="dot {{.Health.Dot}}"></span>{{.Title}}</span>
    {{if .Gateway}}<span class="badge gateway"{{if .GatewayInferred}} title="inferred from the host name; set gateway in AI_ENDPOINTS to confirm"{{end}}>via {{.GatewayLabel}}{{if .GatewayInferred}} (inferred){{end}}</span>{{end}}
    <span class="step-chip" title="recipe step">{{.Step}}</span>
  </div>
  <div class="server-state {{.Health.Class}}">{{.Health.Glyph}} {{.Health.Label}}</div>
  <div class="server-sub">{{.Health.Sub}}</div>
  {{if .AllowlistWarn}}<div class="server-sub err">✗ {{.Requested}} is not on earmark's LiteLLM key allowlist — every call 403s</div>{{end}}
  {{if and .LastErrorShort (eq .Health.Token "failing")}}<div class="server-sub err mono err-clamp" title="{{.LastError}}">{{.LastErrorShort}}</div>{{end}}
  <dl class="role-kv">
  {{if .HasModel}}
    <dt>requested</dt><dd>{{if .Requested}}<span class="mono">{{.Requested}}</span>{{if .EndpointID}} <span class="time-muted">@ {{.EndpointID}}</span>{{end}}{{else}}—{{end}}</dd>
    {{if .Expected}}<dt>pinned</dt><dd><span class="mono">{{.Expected}}</span>{{with .ExpectedRevisionShort}} <span class="time-muted">· rev {{.}}</span>{{end}}{{if eq .Key "asr"}} <span class="time-muted">· recorded only</span>{{end}}</dd>{{end}}
    <dt>answered</dt><dd>{{template "roleAnswered" .}}</dd>
    {{if .AlsoAnswered}}<dt><span class="sr-only">also answered</span></dt><dd class="time-muted">also: {{range $i, $m := .AlsoAnswered}}{{if $i}}, {{end}}<span class="mono">{{$m.Model}}</span> ×{{commafy $m.Count}}{{end}} (7d)</dd>{{end}}
    <dt>last ok</dt><dd>{{if not .LastKnown}}—{{else if .LastOK.IsZero}}<span class="time-muted">never</span>{{else}}<span title="{{formatTime .LastOK}}">{{relTime .LastOK}}</span>{{end}}{{if and (or (eq .Key "judge") .IsFnRole) .LastKnown}} · {{if .LastFail.IsZero}}<span class="time-muted">no failures</span>{{else}}last fail <span title="{{formatTime .LastFail}}">{{relTime .LastFail}}</span>{{end}}{{end}}</dd>
    {{if and .LastErrorShort (ne .Health.Token "failing")}}<dt>last error</dt><dd class="time-muted" title="{{.LastError}}">{{if not .LastFail.IsZero}}{{relTime .LastFail}}: {{end}}{{.LastErrorShort}}</dd>{{end}}
    {{if .FailingNow}}<dt>failing</dt><dd>{{commafy .FailingNow}} {{plural .FailingNow "transcript" "transcripts"}} currently failing</dd>{{end}}
    {{if eq .Key "judge"}}<dt>coverage</dt><dd>{{if .StatsKnown}}{{commafy .CoverageDone}} / {{commafy .CoverageTotal}} {{plural .CoverageTotal "transcript" "transcripts"}} judged{{else}}—{{end}}</dd>{{end}}
    {{if and .IsFnRole .FnKnown .CountsKnown}}<dt>calls</dt><dd>{{commafy .Calls}} <span class="mono">{{.Fn}}</span> {{plural .Calls "call" "calls"}}{{if .CacheHits}} + {{commafy .CacheHits}} cached{{end}}{{if .Failures24h}} · <span class="err">{{commafy .Failures24h}} failed (24h)</span>{{end}}{{if .Fallbacks}} · {{commafy .Fallbacks}} {{plural .Fallbacks "fallback" "fallbacks"}}{{end}} <span class="time-muted">· current recipe{{if not .RecipeSince.IsZero}}, since <span title="{{formatTime .RecipeSince}}">{{relTime .RecipeSince}}</span>{{end}}</span></dd>{{end}}
    {{if and (eq .Key "decide") .HumanDecided}}<dt>decided</dt><dd>{{commafy .JevDecided}} by Jev · {{commafyPtr .HumanDecided}} by humans{{if .JevUndone}} · {{commafy .JevUndone}} undone{{end}} · {{commafy .Proposed}} proposed awaiting</dd>{{end}}
    {{if and (eq .Key "scan") .Scanned}}<dt>scanned</dt><dd>{{commafy64Ptr .Scanned}}{{if .ChunksTotal}} / {{commafy .ChunksTotal}}{{end}} chunks <span class="time-muted">· current recipe</span></dd>{{end}}
    {{if eq .Key "embeddings"}}<dt>backlog</dt><dd>{{if .StatsKnown}}{{commafy .Backlog}} {{plural .Backlog "transcript" "transcripts"}} awaiting embedding{{else}}—{{end}}</dd>{{end}}
  {{end}}
    <dt>stale</dt><dd>{{if not .StaleKnown}}{{if .StalePending}}<span class="time-muted">counting…</span>{{else}}—{{end}}{{else if .StaleTracked}}{{if .CountsKnown}}<a href="#recipe-{{.Step}}">{{commafy64Ptr .Stale}} rows</a>{{else}}{{commafy64Ptr .Stale}} rows{{end}}{{else}}<span class="time-muted">not tracked</span>{{end}}</dd>
  </dl>
</div>
{{end}}

{{define "roleAnswered"}}{{if not .CountsKnown}}—{{else if eq .AnsweredMatch "none"}}<span class="time-muted">no runs yet</span>{{else if eq .AnsweredMatch "predates_recipe"}}<span class="time-muted">no calls since the {{if .PriorModelChanged}}model{{else}}recipe{{end}} changed{{with .CompareTarget}} (now {{.}}){{end}}{{if not .RecipeSince.IsZero}} <span title="{{formatTime .RecipeSince}}">{{relTime .RecipeSince}}</span>{{end}} · before: </span><span class="mono">{{.Answered}}</span>{{else if eq .AnsweredMatch "unreported"}}<span class="time-muted">not reported by endpoint</span>{{else}}<span class="mono">{{.Answered}}</span>{{if and (eq .Key "asr") .ASRRunnerVersion}} · runner <span class="mono">{{.ASRRunnerVersion}}</span>{{if .ASRModelSHA}} · .nemo <span class="mono" title="{{.ASRModelSHA}}">{{.ASRModelSHAShort}}</span>{{end}}{{end}}{{if eq .AnsweredMatch "match"}} <span class="match-ok">✓ matches {{if .Expected}}pin{{else}}request{{end}}</span>{{else if eq .AnsweredMatch "mismatch"}} <span class="badge mismatch">≠ expected {{.CompareTarget}}</span>{{end}}{{end}}{{end}}

{{define "endpointRow"}}
<tr>
  <th scope="row" class="mono">{{.V.ID}}</th>
  <td>{{if .V.Role}}<span title="role token: {{.V.Role}}">{{.V.RoleTitle}}</span>{{else}}<span class="time-muted">unbound</span>{{end}}</td>
  <td><span class="badge role-{{.V.Type}}">{{.V.Type}}</span></td>
  <td>{{if .V.Gateway}}<strong>{{.V.GatewayLabel}}</strong>{{if .V.GatewayInferred}} <span class="time-muted" title="inferred from the host name; set gateway in AI_ENDPOINTS to confirm">(inferred)</span>{{end}}{{else}}<span class="time-muted">direct</span>{{end}}</td>
  <td class="mono">{{or .V.HostOnly "—"}}</td>
  <td class="mono">{{.V.Model}}</td>
  <td><span class="server-state {{.V.State.Class}}" title="{{.V.State.Sub}}">{{.V.State.Glyph}} {{.V.State.Label}}</span></td>
  {{if .ShowOptions}}<td class="time-muted">{{if .V.FromEnv}}<em>from env, not in AI_ENDPOINTS</em>{{else}}{{.V.OptionsLine}}{{end}}</td>{{end}}
</tr>
{{end}}

{{define "serverModesRow"}}
<tr>
  <th scope="row">{{.V.Name}}{{if .V.Role}} <span class="time-muted">({{.V.Role}})</span>{{end}}</th>
  <td class="mono">{{if .V.Model}}{{.V.Model}}{{if eq .V.ModelSource "configured"}} <span class="time-muted" title="expected from ASR_SERVERS; no run has reported a model yet">(expected)</span>{{end}}{{else}}<span class="time-muted">—</span>{{end}}</td>
  {{if .ShowRuntime}}<td class="time-muted">{{if .V.Runtime}}<span{{if .V.RuntimeKnown}} class="family-known"{{end}}>{{.V.Runtime}}</span>{{if eq .V.RuntimeSource "configured"}} <span class="time-muted" title="expected from ASR_SERVERS; no run has reported a runtime yet">(expected)</span>{{end}}{{else}}<span title="no runtime observed or configured">unknown</span>{{end}}</td>{{end}}
  {{if .ShowCaps}}<td>
    {{if .V.Caps}}<span class="cap-strip" title="{{.V.CapsTitle}}">{{range .V.AppliedCaps}}<span class="cap-badge cap-on">{{.Label}}</span>{{else}}<span class="time-muted">none</span>{{end}}</span>{{else}}<span class="time-muted" title="no capabilities observed or configured">unknown</span>{{end}}
  </td>{{end}}
  <td class="time-muted">{{if .V.ComputeMode}}{{.V.ComputeMode}}{{else}}—{{end}}</td>
  <td class="time-muted">{{confPct .V.MeanConfidence}}</td>
  <td class="time-muted">{{commafy .V.JobsDone}}</td>
  <td class="time-muted">{{.V.AvgProc}}</td>
  <td class="time-muted"{{if not .V.LastActiveAt.IsZero}} title="{{formatTime .V.LastActiveAt}}"{{end}}>{{.V.LastActive}}</td>
</tr>
{{end}}
`))

// runnerTableRow is one runner-table row plus the table's column toggles
// (Runtime/Caps are hidden when no runner reports or declares them).
type runnerTableRow struct {
	V           serverView
	ShowRuntime bool
	ShowCaps    bool
}

// endpointTableRow is one endpoints-table row plus the Options column toggle
// (hidden when no row has options to show).
type endpointTableRow struct {
	V           endpointView
	ShowOptions bool
}

// modelsData backs the Models fragment (/servers/data).
type modelsData struct {
	// RenderedAt is the render clock ("15:04:05 UTC"), shown as-is: it is the
	// one header value that visibly freezes when polling stops. CountsAt /
	// StaleAt are absolute UTC stamps for the relative ages' title tooltips.
	RenderedAt string
	// CountsAge is how old the cached aggregate snapshot is ("12s ago"); ""
	// when none ever loaded. CountsErr is true when the latest refresh failed —
	// the page then shows the last good snapshot (if any) and says so.
	CountsAge   string
	CountsAt    string
	CountsErr   bool
	CountsKnown bool
	// StaleAge / StaleErr / StalePending are the same for the separately
	// cached (5 min) stale_work counts.
	StaleAge     string
	StaleAt      string
	StaleErr     bool
	StalePending bool
	// RoleCards are the roles rendered as cards; UnconfiguredRoles (state
	// not_configured) collapse into one muted line under them.
	RoleCards         []roleCard
	UnconfiguredRoles []roleCard
	// Recipes are the tracked steps; UntrackedSteps label the rest for the
	// one-line "Not tracked:" note.
	Recipes        []recipeRow
	UntrackedSteps []string
	Runners        []serverView
	ShowRuntime    bool
	ShowCaps       bool
	// RunnerUpdate is the read-only version-skew state (CONTRACT §2.12), nil
	// when the runner has never reported a version. The update form itself is
	// in the static shell so a poll never wipes what the operator is typing.
	RunnerUpdate *serversRunnerUpdate
	Gateways     []gatewayView
	Endpoints    []endpointView
	EvalEnvRow   *endpointView // non-nil when the judge comes from EVAL_CHAT_*
	ShowOptions  bool          // any endpoint row has options (or the env-row note)
	ASRGroups    []asrProvenanceRow
	JudgeOutput  []findingsModelRow
	JudgeTotals  *findingsModelRow
}

// EndpointCount is the number of endpoint table rows, including the
// synthetic EVAL_CHAT_* row.
func (d modelsData) EndpointCount() int {
	n := len(d.Endpoints)
	if d.EvalEnvRow != nil {
		n++
	}
	return n
}

// serversRunnerUpdate is the dashboard view of runner_control's version-skew +
// update state for the runner version panel.
type serversRunnerUpdate struct {
	Running   string
	Desired   string
	State     string
	Error     string
	Available bool
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *MCPServer) handleServersPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, serversPage, pageShell{Title: "models", Nav: "servers", ControlEnabled: s.controlToken != ""})
}

// handleServersData renders the Models fragment. Only a GetServerObservation
// failure is a 500 (the htmx region keeps its last swap and flags the
// connection); a failed or slow snapshot still renders 200 with "counts
// unavailable" for exactly the sections it backs.
func (s *MCPServer) handleServersData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	obs, err := s.db.GetServerObservation(ctx)
	if err != nil {
		s.logger.Error("GetServerObservation error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	stats, err := s.db.GetServiceStatus(ctx)
	if err != nil {
		s.logger.Warn("models: GetServiceStatus error; coverage/backlog/runner version degraded", "error", err)
		stats = nil
	}
	runners := buildServerViews(s.asrServers, obs, s.probeServers(ctx), now, s.runnerStaleAfter)
	eps := buildEndpointViews(s.cfg, s.probeEndpoints(ctx))
	gws, targets, gwByEndpoint := s.probeGateways(ctx)
	roles, ev := s.modelRoles(ctx, stats, runners, true, eps, gwByEndpoint, now)

	data := modelsData{
		RenderedAt:   now.UTC().Format("15:04:05 UTC"),
		CountsErr:    ev.SnapErr != nil,
		CountsKnown:  ev.Snap != nil,
		StaleErr:     ev.StaleErr != nil,
		StalePending: ev.StalePending,
		Runners:      runners,
		RunnerUpdate: runnerUpdateView(stats),
		Gateways:     buildGatewayViews(gws, targets, roles, now),
		Endpoints:    eps,
		EvalEnvRow:   envJudgeView(s.judgeConfig()),
	}
	for _, c := range roles {
		if c.Health.Token == roleNotConfigured {
			data.UnconfiguredRoles = append(data.UnconfiguredRoles, c)
		} else {
			data.RoleCards = append(data.RoleCards, c)
		}
	}
	for _, v := range runners {
		data.ShowRuntime = data.ShowRuntime || v.Runtime != ""
		data.ShowCaps = data.ShowCaps || len(v.Caps) > 0
	}
	data.ShowOptions = data.EvalEnvRow != nil
	for _, e := range eps {
		data.ShowOptions = data.ShowOptions || len(e.Options) > 0
	}
	if ev.Snap != nil {
		data.CountsAge, data.CountsAt = humanizeSince(now.Sub(ev.SnapAt)), absTime(ev.SnapAt)
		data.Recipes, data.UntrackedSteps = buildRecipeRows(ev.Snap, ev.Stale)
		data.ASRGroups = buildASRRows(ev.Snap.ASRGroups)
		rows, totals := buildFindingsRows(ev.Snap.Findings)
		data.JudgeOutput = rows
		if len(rows) > 1 {
			data.JudgeTotals = &totals
		}
	}
	if ev.Stale != nil {
		data.StaleAge, data.StaleAt = humanizeSince(now.Sub(ev.StaleAt)), absTime(ev.StaleAt)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := serversFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("models fragment render error", "error", err)
	}
}

// absTime is the absolute UTC stamp shown in a relative time's title tooltip.
func absTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

// staleFirstWait bounds how long a poll waits for the stale counts' very
// first load (a full-library scan) before rendering "counting…"; the load
// carries on in the background and the next poll picks it up.
const staleFirstWait = 1500 * time.Millisecond

// modelRoles builds the role cards from the cached snapshots. The page and
// GET /api/v1/status both call it, so they cannot disagree. Snapshot refresh
// errors are logged once per failed refresh by the caches themselves.
func (s *MCPServer) modelRoles(ctx context.Context, stats *db.QueueStats, runners []serverView, runnersKnown bool,
	eps []endpointView, gateways map[string]gatewayStatus, now time.Time,
) ([]roleCard, modelsEvidence) {
	ev := s.models.read(ctx, staleFirstWait)
	phase, err := s.db.GetPipelinePhase(ctx)
	if err != nil {
		s.logger.Warn("models: GetPipelinePhase error; ASR card treats a stopped runner as unintended", "error", err)
		phase = ""
	}
	roles := buildRoleCards(roleInputs{
		Phase:        phase,
		Cfg:          s.cfg,
		Judge:        s.judgeConfig(),
		ASRServers:   s.asrServers,
		Servers:      runners,
		ServersKnown: runnersKnown,
		Endpoints:    eps,
		Stats:        stats,
		Snap:         ev.Snap,
		Stale:        ev.Stale,
		StalePending: ev.StalePending,
		Gateways:     gateways,
		Now:          now,
	})
	return roles, ev
}

// judgeConfig reports what initEval resolved for the judge.
func (s *MCPServer) judgeConfig() judgeConfig {
	return judgeConfig{
		Configured: s.eval.configured,
		Source:     s.eval.source,
		Model:      s.eval.model,
		Host:       s.eval.host,
	}
}

// runnerUpdateView reads runner_control's version-skew state for the runner
// version panel. Returns nil (panel hidden) when stats are unavailable, or the
// runner has never reported a version and no update is pending, so the
// affordance only appears once it is meaningful.
func runnerUpdateView(stats *db.QueueStats) *serversRunnerUpdate {
	if stats == nil {
		return nil
	}
	running := derefStr(stats.RunnerVersion)
	desired := derefStr(stats.DesiredRunnerVersion)
	state := derefStr(stats.RunnerUpdateState)
	if running == "" && desired == "" && state == "" {
		return nil
	}
	return &serversRunnerUpdate{
		Running:   running,
		Desired:   desired,
		State:     state,
		Error:     derefStr(stats.RunnerUpdateError),
		Available: desired != "" && desired != running,
	}
}

// handleRunnerUpdate serves POST /actions/runner-update?version=<tag> (htmx): it
// records the operator's update intent (or clears it when version is empty) and
// re-renders the Models fragment. The runner — not this handler — performs the
// swap (CONTRACT §2.12). Fails closed when no control token is configured.
func (s *MCPServer) handleRunnerUpdate(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.controlToken == "" {
		writeActionError(w, "control token not configured — update disabled")
		return
	}
	version := strings.TrimSpace(runnerUpdateVersion(r))
	if version == "" {
		if err := s.db.ClearRunnerUpdate(r.Context(), "dashboard"); err != nil {
			s.logger.Error("runner-update clear error", "error", err)
			writeActionError(w, "clear update failed — see server logs")
			return
		}
		s.logger.Info("runner update request cleared via dashboard")
	} else {
		if err := s.db.SetDesiredRunnerVersion(r.Context(), version, "dashboard"); err != nil {
			s.logger.Error("runner-update error", "error", err)
			writeActionError(w, "runner update request failed — see server logs")
			return
		}
		s.logger.Info("runner update requested via dashboard", "version", version)
	}
	s.handleServersData(w, r)
}

// runnerUpdateVersion reads the requested version. A `version` field in the
// form body wins whenever it is PRESENT — even empty, which is an explicit
// clear (the Clear control posts version="") — so a stray ?version= in the URL
// can never turn a clear into a set. Only when the body carries no version
// field does the legacy query string apply. Empty means "clear the request".
func runnerUpdateVersion(r *http.Request) string {
	_ = r.ParseForm() // a malformed body leaves PostForm empty → query fallback
	if vs, ok := r.PostForm["version"]; ok {
		if len(vs) == 0 {
			return ""
		}
		return vs[0]
	}
	return r.URL.Query().Get("version")
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
