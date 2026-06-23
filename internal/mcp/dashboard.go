package mcp

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/predict"
)

//go:embed layout.html htmx.min.js
var dashboardFS embed.FS

// htmxJS is the vendored, version-pinned htmx library (htmx.org v2.0.4), served
// from the binary so the dashboard needs no external CDN at runtime.
var htmxJS = func() []byte {
	b, err := dashboardFS.ReadFile("htmx.min.js")
	if err != nil {
		panic("embedded htmx.min.js missing: " + err.Error())
	}
	return b
}()

// libraryPageSize is the DEFAULT number of books per page in the library list.
// The reader can raise it via the ?per= selector to one of libraryPageSizes.
const libraryPageSize = 20

// libraryPageSizes are the selectable library page sizes (the ?per= allow-list),
// letting the reader expand the list in chunks up to 200. The first entry is the
// default; any ?per= value outside this set falls back to libraryPageSize.
var libraryPageSizes = []int{20, 50, 100, 200}

// segmentPageSize is the number of transcript segments rendered per page in the
// track-detail reader; beyond this an htmx "load more" button appends the next
// page (P7), so a multi-thousand-segment transcript doesn't render all at once.
const segmentPageSize = 30

// embedStallThreshold is the embed backlog above which the dashboard *considers*
// warning. A few transcripts always sit briefly between transcription and the
// worker's embed poll, so a small backlog is normal. NOTE: crossing this alone is
// NOT a stall — with in-pipeline eval enabled the worker runs the eval judge
// before embedding, so an active run legitimately builds a transient backlog that
// drains on its own. A real stall ALSO requires no embed progress for
// embedStallWindow (see newStatusData / EmbedStall).
const embedStallThreshold = 10

// embedStallWindow is how long the embed worker must make NO progress (no new
// chunk written) — while a backlog exists — before the dashboard calls it a
// genuine stall rather than normal eval/embed catch-up.
const embedStallWindow = 10 * time.Minute

// tmplFuncs are shared across every dashboard template.
var tmplFuncs = template.FuncMap{
	"embedStallMins": func() int { return int(embedStallWindow.Minutes()) },
	"shortName":      func(fp string) string { return path.Base(fp) },
	"relTime":        func(t time.Time) string { return humanizeSince(time.Since(t)) },
	"formatTime":     func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"formatTimePtr": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	},
	"derefStr": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
	"commafy": commafy,
	// statusLabel maps the internal job status to the operator-facing word. The
	// DB/API/CSS value stays "claimed"; humans see "transcribing".
	"statusLabel": statusLabel,
	// statusBadge renders the inner HTML of a status badge — a small glyph plus the
	// operator-facing word — so the badge never conveys state by color alone (1.4.1).
	// Returns template.HTML (entities are fixed literals, not user input).
	"statusBadge": statusBadge,
	// bookDir is the book directory for a track path. Used to build the
	// book-detail href: passed as a plain string into an href="…?dir={{…}}"
	// URL-context interpolation, which html/template auto-escapes (no template.URL
	// taint, no gosec G203).
	"bookDir": func(fp string) string { return path.Dir(fp) },
	// procTime renders a run_metrics processing duration (seconds) as a compact
	// human string, or an em dash when the metric is absent.
	"procTime": func(secs *float64) string {
		if secs == nil || *secs <= 0 {
			return "—"
		}
		return humanizeSeconds(*secs)
	},
	// commafyPtr renders a nullable integer count with thousands separators, or
	// an em dash when absent.
	"commafyPtr": func(n *int) string {
		if n == nil {
			return "—"
		}
		return commafy(*n)
	},
	// durTime renders a nullable audio duration (seconds) as a compact human
	// string (transcripts.duration_seconds), or an em dash when absent.
	"durTime": func(secs *float64) string {
		if secs == nil || *secs <= 0 {
			return "—"
		}
		return humanizeSeconds(*secs)
	},
	// codecLabel renders the audio codec + channel layout as "aac · stereo",
	// degrading gracefully when either run_metrics field is NULL: just "aac",
	// just "stereo", or an em dash when both are absent.
	"codecLabel": codecLabel,
	// commafy64Ptr renders a nullable 64-bit count (e.g. SUM of token counts)
	// with thousands separators, or an em dash when absent.
	"commafy64Ptr": func(n *int64) string {
		if n == nil {
			return "—"
		}
		return commafy64(*n)
	},
	// compactNum / compactNum64Ptr abbreviate millions+ (14.8M) so big stat-card
	// values don't overflow; pair with commafy in a title for the exact value.
	"compactNum": compactNum,
	"compactNum64Ptr": func(n *int64) string {
		if n == nil {
			return "—"
		}
		return compactNum64(*n)
	},
	// timestamp renders a float seconds offset as mm:ss (or h:mm:ss past an hour)
	// for the transcript reader and chunk list.
	"timestamp": timestamp,
	// strPtr renders a nullable string, or an em dash when nil/empty.
	"strPtr": func(s *string) string {
		if s == nil || *s == "" {
			return "—"
		}
		return *s
	},
	// boolPtr renders a nullable bool as "yes"/"no", or an em dash when nil.
	"boolPtr": func(b *bool) string {
		if b == nil {
			return "—"
		}
		if *b {
			return "yes"
		}
		return "no"
	},
	// bytesPtr renders a nullable byte count as a human size (e.g. "12.4 MB"), or
	// an em dash when nil.
	"bytesPtr": func(n *int64) string {
		if n == nil {
			return "—"
		}
		return humanizeBytes(*n)
	},
	// hzPtr renders a nullable sample rate in Hz as "44.1 kHz", or an em dash.
	"hzPtr": func(n *int) string {
		if n == nil {
			return "—"
		}
		return fmt.Sprintf("%.1f kHz", float64(*n)/1000)
	},
	// sub subtracts b from a (small arithmetic for the "N remaining" reader label).
	"sub": func(a, b int) int { return a - b },
	// add adds a+b (segment anchor index = page start offset + loop index, for
	// the "load more" continuation page).
	"add": func(a, b int) int { return a + b },
	// deref returns the value of an *int (0 when nil); used to compare the
	// deep-jump target index inside the reader loop.
	"deref": func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	},
	// pct renders an integer percentage (done/total) for a progress-bar width,
	// guarding division by zero (total == 0 → 0). Mirrors the handler's DonePct
	// computation so the pipeline-panel bars match the library list's progress bar.
	"pct": func(done, total int) int {
		if total <= 0 {
			return 0
		}
		return done * 100 / total
	},
	// confPct renders a nullable mean word-confidence (0–1) as a percentage, or an
	// em dash when the backend emitted no scores (NULL). Used by the Servers table.
	"confPct": func(c *float64) string {
		if c == nil {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", *c*100)
	},
	// confPctF renders a plain (non-nullable) confidence (0–1) as a percentage.
	// Used by the Findings per-book table where the mean is always present.
	"confPctF": func(c float64) string { return fmt.Sprintf("%.0f%%", c*100) },
	// worklistURL builds a /findings/data URL with one facet (conf|issue|book)
	// changed to val, carrying the other two active facets — so clicking an
	// Issue-Types or Per-Book row filters the worklist without losing the rest of
	// the filter state. An empty val clears that facet. The output is a fixed-shape
	// query string with url.QueryEscape'd values, returned as template.URL for
	// interpolation into an hx-get attribute.
	"worklistURL": worklistURL,
	// vramLabel renders a "used / total GB" VRAM string from nullable MB values
	// reported by gpu-arbiter, or an em dash when either value is absent.
	"vramLabel": vramLabel,
}

// mustPage parses the shared layout plus a page-specific {{define "content"}}.
func mustPage(content string) *template.Template {
	t := template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFS(dashboardFS, "layout.html"))
	return template.Must(t.Parse(content))
}

// ─── Page shells (layout + content) ──────────────────────────────────────────

// pipelinePage is the Pipeline ops page (GET /pipeline): the auto-refreshing
// status fragment (counts, pipeline state, pause + run-budget controls) plus the
// Failed view folded in as a second htmx region loading /failed/data.
var pipelinePage = mustPage(`{{define "content"}}
<p class="subtitle">pipeline status &nbsp;·&nbsp; auto-refreshes every 3 s</p>
<div id="conn" class="conn-lost" role="status" aria-live="polite" hidden>&#9888;&#xFE0F;&nbsp;connection lost — data below may be stale</div>
<div id="action-error" aria-live="assertive"></div>
<div id="data-region"
     hx-get="/status/data" hx-trigger="load, every 3s" hx-swap="innerHTML"
     hx-sync="this:drop" hx-request='{"timeout": 5000}'
     hx-on::response-error="document.getElementById('conn').hidden = false"
     hx-on::send-error="document.getElementById('conn').hidden = false"
     hx-on::timeout="document.getElementById('conn').hidden = false"
     hx-on::after-request="if (event.detail.successful) document.getElementById('conn').hidden = true">
  <p class="htmx-indicator">loading…</p>
</div>
<div id="failed-region" class="section" hx-get="/failed/data" hx-trigger="load" hx-swap="innerHTML">
  <p class="htmx-indicator">loading failed jobs…</p>
</div>
{{end}}`)

var libraryPage = mustPage(`{{define "content"}}
<div id="action-error" aria-live="assertive"></div>
<div id="library-region" hx-get="/library/data{{.DataQuery}}" hx-trigger="load" hx-swap="innerHTML">
  <p class="htmx-indicator">loading library…</p>
</div>
{{end}}`)

var bookPage = mustPage(`{{define "content"}}
<a class="back-link" href="/library">&#8592;&nbsp;Library</a>
<div id="action-error" aria-live="assertive"></div>
<div id="book-region" hx-get="/book/data?dir={{.DirQuery}}" hx-trigger="load" hx-swap="innerHTML">
  <p class="htmx-indicator">loading…</p>
</div>
{{end}}`)

var trackPage = mustPage(`{{define "content"}}
<div id="action-error" aria-live="assertive"></div>
<div id="track-region" hx-get="/track/data?id={{.IDQuery}}{{.TQuery}}" hx-trigger="load" hx-swap="innerHTML">
  <p class="htmx-indicator">loading…</p>
</div>
{{end}}`)

// failedFragmentTmpl is the track-level failures view: full error, retry count,
// and which runner last claimed each job, with per-row requeue.
var failedFragmentTmpl = template.Must(template.New("failed").Funcs(tmplFuncs).Parse(`
<div class="section-title">Failed jobs ({{len .Jobs}})</div>
{{if .Jobs}}
<div class="table-wrap">
<table>
  <caption>Failed transcription jobs — each shows its error and a retry</caption>
  <thead><tr><th scope="col">Track</th><th scope="col">Error</th><th scope="col" class="num">Attempts</th><th scope="col">Runner</th><th scope="col">Updated</th><th scope="col"><span class="sr-only">Actions</span></th></tr></thead>
  <tbody>
  {{range .Jobs}}
    <tr>
      <td><a class="file-name" href="/book?dir={{bookDir .FilePath}}" title="{{.FilePath}}">{{shortName .FilePath}}</a></td>
      <td>{{if .Error}}<details class="error-row"><summary>show error</summary><pre>{{derefStr .Error}}</pre></details>{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="time-muted">{{.Attempts}}/3</td>
      <td class="time-muted">{{if .ClaimedBy}}{{derefStr .ClaimedBy}}{{else}}—{{end}}</td>
      <td class="time-muted" title="{{formatTime .UpdatedAt}}">{{relTime .UpdatedAt}}</td>
      <td class="actions">
        <button class="btn" hx-post="/actions/requeue?id={{.ID}}&failed=1" hx-target="#failed-region" hx-swap="innerHTML"
                hx-confirm="Re-transcribe this track? It is reset to pending and re-run.">requeue</button>
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>
{{else}}
<p class="lib-empty">No failed jobs. &#127881;</p>
{{end}}
`))

// ─── Status fragment (Overview) ──────────────────────────────────────────────

var statusFragmentTmpl = template.Must(template.New("status").Funcs(tmplFuncs).Parse(`
{{- /* bookRow renders a single book row in the activity feed or queue section.
       It takes an activityBook as its data. Defined once here and called from
       both the Queue loop and the Recent Activity loop to avoid duplicating
       ~45 lines of markup. */ -}}
{{define "bookRow"}}
<details class="activity-book" data-book-id="{{.Dir}}">
  <summary class="activity-summary">
    <span class="activity-title">{{if .Title}}{{.Title}}{{else}}{{.Dir}}{{end}}{{if .Author}}<span class="activity-author time-muted"> · {{.Author}}</span>{{end}}</span>
    <span class="activity-counters">
      <span class="activity-tracks">{{commafy .Done}} / {{commafy .Total}} tracks done</span>
      {{if gt .Claimed 0}}<span class="badge claimed"> · {{commafy .Claimed}} transcribing</span>{{end}}
      {{if gt .Failed 0}}<span class="badge failed"> · {{commafy .Failed}} failed</span>{{end}}
    </span>
    <span class="activity-bar-wrap" title="{{.DonePct}}% done">
      <span class="progress activity-bar"><span class="progress-bar{{if gt .Failed 0}} has-failed{{end}}" style="width:{{.DonePct}}%"></span></span>
    </span>
    <span class="time-muted activity-time" title="{{formatTime .LastUpdated}}">{{relTime .LastUpdated}}</span>
  </summary>
  <div class="activity-tracks-body">
    {{if .Tracks}}
    <div class="table-wrap">
    <table>
      <caption>Tracks for {{if .Title}}{{.Title}}{{else}}{{.Dir}}{{end}}</caption>
      <thead><tr>
        <th scope="col">Track</th>
        <th scope="col">Status</th>
        <th scope="col" class="num" title="audio duration">Duration</th>
        <th scope="col" class="num" title="transcription wall-clock time (runner)">Proc</th>
        <th scope="col" class="num" title="embedded chunks">Chunks</th>
        <th scope="col">Updated</th>
      </tr></thead>
      <tbody>
      {{range .Tracks}}
        <tr>
          <td><a class="file-name" href="/track?id={{.ID}}" title="{{.FilePath}}">{{.ShortName}}</a></td>
          <td><span class="badge {{.Status}}">{{statusBadge .Status}}</span></td>
          <td class="time-muted">{{durTime .DurationSeconds}}</td>
          <td class="time-muted">{{procTime .ProcessingSeconds}}</td>
          <td class="time-muted">{{commafyPtr .EmbedChunkCount}}</td>
          <td class="time-muted" title="{{formatTime .UpdatedAt}}">{{relTime .UpdatedAt}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    </div>
    {{else}}<p class="lib-empty">No tracks found.</p>{{end}}
    <div class="activity-book-link"><a href="/book?dir={{.Dir}}">Open book &rarr;</a></div>
  </div>
</details>
{{end}}

<div class="updated">updated {{.RenderedAt}}</div>

<!-- unified pipeline state: combines the pause flag AND runner liveness into one
     honest line, so it can never say "running" while no runner is active. Runner
     liveness here is claim ACTIVITY (claimed-job heartbeats), not socket
     connectivity — live host/GPU connectivity lives on the Servers page. -->
<div class="pipeline {{.StateClass}}">
  <div class="pipe-main">
    <span class="dot {{.DotClass}}"></span>
    <span class="pipe-label">{{.StateLabel}}</span>
    <span class="phase-badge {{.PhaseClass}}" title="coordinator phase (read-only — the earmark batch coordinator owns phase; the dashboard never writes it)">phase: {{.Phase}}</span>
    <span class="pipe-sub">{{.SubText}}</span>
    {{if .MetaText}}<div class="pipe-meta">{{.MetaText}}</div>{{end}}
  </div>
  {{if .ControlEnabled}}
  {{if .Stats.Paused}}
  <button class="btn btn-primary" hx-post="/actions/resume" hx-target="#data-region" hx-swap="innerHTML"
          hx-sync="#data-region:replace"
          hx-confirm="Resume the pipeline? The runner will start claiming pending jobs.">&#9654;&nbsp;Resume pipeline</button>
  {{else}}
  <button class="btn btn-warn" hx-post="/actions/pause" hx-target="#data-region" hx-swap="innerHTML"
          hx-sync="#data-region:replace"
          hx-confirm="Pause the pipeline? The runner finishes its current job, then stops claiming new work.">&#10073;&#10073;&nbsp;Pause pipeline</button>
  {{end}}
  {{else}}
  <span class="ctrl-disabled" title="Pipeline controls are disabled: CONTROL_API_TOKEN is not configured on this deployment.">controls disabled — no CONTROL_API_TOKEN</span>
  {{end}}
</div>

<div class="run-budget">
  <div class="rb-state">
    <span class="rb-label">run budget</span>
    <span class="rb-value">{{.RunBudgetText}}</span>
  </div>
  {{if .ControlEnabled}}
  <form class="rb-form" hx-post="/actions/run" hx-target="#data-region" hx-swap="innerHTML"
        hx-sync="#data-region:replace"
        hx-confirm="Arm a bounded run? The runner claims that many jobs then auto-pauses.">
    <input id="rb-n" hx-preserve="true" type="number" name="n" min="1" value="1" aria-label="number of jobs to run" required>
    <button type="submit" class="btn btn-go">&#9654;&nbsp;run N then pause</button>
  </form>
  {{if .RunLimit}}
  <button class="btn" hx-post="/actions/run-clear" hx-target="#data-region" hx-swap="innerHTML"
          hx-sync="#data-region:replace"
          hx-confirm="Clear the bounded run (back to unlimited)? The pause flag is left unchanged.">clear budget</button>
  {{end}}
  {{else}}
  <span class="ctrl-disabled" title="Run controls are disabled: CONTROL_API_TOKEN is not configured.">disabled — no CONTROL_API_TOKEN</span>
  {{end}}
</div>

{{if .ShowProgress}}
<div class="overview-progress">
  <div class="op-bar"><div class="op-fill" style="width:{{.DonePct}}%"></div></div>
  <div class="op-meta">
    <span class="op-main">{{.ProgressText}}</span>
    {{if .Stats.BooksTotal}}<span title="a book is a directory of tracks; track counts above are per audio file">{{commafy .Stats.BooksFullyDone}} / {{commafy .Stats.BooksTotal}} books</span>{{end}}
    <span>{{.ThroughputText}}</span>
    <span>{{.ETAText}}</span>
  </div>
</div>
{{end}}

{{- /* ─── 3-stage lifecycle strip ─────────────────────────────────────────────
     Shows the honest Transcribe → Eval → Embed pipeline so an operator can see
     exactly which stage owns work / the GPU right now. The strip is always
     shown when there is any progress (ShowProgress) or the pipeline is still
     winding down. A11y: each stage cell uses glyph + word, not color alone.
*/ -}}
{{if or .ShowProgress (.Lifecycle.WindingDown)}}
<div class="lc-strip" aria-label="3-stage pipeline lifecycle">
  {{- $lc := .Lifecycle -}}
  {{- $done := $lc.TranscribeDone -}}
  {{- $total := $lc.TranscribeTotal -}}
  {{- $transcribePct := pct $done $total -}}

  <div class="lc-stage{{if eq (print $lc.Activity) "transcribing"}} lc-active{{end}}">
    <div class="lc-stage-hd">
      <span class="lc-glyph" aria-hidden="true">&#9658;</span>
      <span class="lc-name">Transcribe</span>
      {{if eq (print $lc.Activity) "transcribing"}}<span class="lc-gpu-marker" title="runner is actively using the GPU">&#9679; active on GPU</span>{{end}}
    </div>
    <div class="lc-count">{{commafy $done}} / {{commafy $total}}</div>
    <div class="lc-bar-wrap"><div class="lc-bar"><div class="lc-bar-fill" style="width:{{$transcribePct}}%"></div></div></div>
  </div>

  <div class="lc-stage{{if eq (print $lc.Activity) "evaluating"}} lc-active{{end}}{{if not $lc.EvalInPipeline}} lc-na{{end}}">
    <div class="lc-stage-hd">
      <span class="lc-glyph" aria-hidden="true">&#9673;</span>
      <span class="lc-name">Eval</span>
      {{if eq (print $lc.Activity) "evaluating"}}<span class="lc-gpu-marker" title="eval judge is actively using the GPU">&#9679; active on GPU</span>{{end}}
    </div>
    {{if $lc.EvalInPipeline}}
    <div class="lc-count">{{$lc.EvalCoverageDisplay $done}}</div>
    <div class="lc-bar-wrap"><div class="lc-bar"><div class="lc-bar-fill" style="width:{{$lc.EvalCoveragePct $done}}%"></div></div></div>
    {{else}}
    <div class="lc-count lc-na-text">not in pipeline</div>
    {{end}}
  </div>

  <div class="lc-stage{{if eq (print $lc.Activity) "embedding"}} lc-active{{end}}">
    <div class="lc-stage-hd">
      <span class="lc-glyph" aria-hidden="true">&#8857;</span>
      <span class="lc-name">Embed</span>
    </div>
    {{if gt $lc.EmbedBacklog 0}}
    <div class="lc-count">{{commafy $lc.EmbedBacklog}} backlog</div>
    <div class="lc-bar-wrap"><div class="lc-bar lc-bar-indeterminate"></div></div>
    {{else}}
    <div class="lc-count">&#10003; clear</div>
    {{end}}
  </div>
</div>
{{end}}

{{- /* ─── GPU / VRAM card group ───────────────────────────────────────────────
     Only shown when a gpu-arbiter probe is configured and reachable (GPUProbed).
     Shows GPU STATE, VRAM, and a MODELS RESIDENT list. A11y: glyph + word.
*/ -}}
{{if .Lifecycle.GPUProbed}}
<div class="card-group gpu-card-group">
  <div class="group-label">GPU <span class="group-unit">· <a href="/servers">details on Servers page</a></span></div>
  <div class="grid">
    <div class="card" title="gpu-arbiter state: available = GPU free for pipeline; gaming = evicted by game">
      <div class="card-label">GPU State</div>
      <div class="card-value{{if .Lifecycle.GPUCommitted}} green{{end}}">
        {{if .Lifecycle.GPUCommitted}}&#9679; committed{{else if .PrimaryArbiter.Reachable}}&#9675; free{{else}}&#8212; offline{{end}}
      </div>
    </div>
    <div class="card" title="VRAM usage reported by gpu-arbiter (used / total)">
      <div class="card-label">VRAM</div>
      <div class="card-value">{{vramLabel .PrimaryArbiter.VRAMUsedMB .PrimaryArbiter.VRAMTotalMB}}</div>
    </div>
    {{if .PrimaryArbiter.ResidentUnits}}
    <div class="card gpu-resident-card" title="model services resident on the GPU (&#9658; running, &#9679; loaded)">
      <div class="card-label">Models Resident</div>
      <div class="gpu-resident-list">
        {{range .PrimaryArbiter.ResidentUnits}}
        <div class="gpu-resident-unit">
          <span class="gpu-unit-glyph" aria-hidden="true">{{if .Running}}&#9658;{{else}}&#9679;{{end}}</span>
          <span class="gpu-unit-name">{{.Unit}}</span>
          <span class="gpu-unit-state">{{if .Running}}active{{else}}resident{{end}}</span>
        </div>
        {{end}}
      </div>
    </div>
    {{end}}
  </div>
</div>
{{end}}

<div class="card-group">
  <div class="group-label">transcription <span class="group-unit">· per track (audio file)</span></div>
  <div class="grid">
    <a class="card card-click" href="/library?status=pending" title="tracks pending — opens the books that contain them">
      <div class="card-label">Pending tracks</div><div class="card-value blue">{{commafy .Stats.Pending}}</div></a>
    <a class="card card-click" href="/library?status=claimed" title="tracks being transcribed — opens the books that contain them">
      <div class="card-label">Transcribing</div><div class="card-value yellow">{{commafy .Stats.Claimed}}</div></a>
    <a class="card card-click" href="/library?status=done" title="tracks done — opens the books that contain them">
      <div class="card-label">Done tracks</div><div class="card-value green">{{commafy .Stats.Done}}</div></a>
    <a class="card card-click" href="/library?status=failed" title="tracks with failures — opens the books that contain them">
      <div class="card-label">Failed tracks</div><div class="card-value{{if gt .Stats.Failed 0}} red{{end}}">{{commafy .Stats.Failed}}</div></a>
  </div>
</div>

<div class="card-group">
  <div class="group-label">embedding</div>
  <div class="grid">
    <div class="card"><div class="card-label">Chunks</div><div class="card-value" title="{{commafy .Stats.Chunks}}" aria-label="{{commafy .Stats.Chunks}} chunks">{{compactNum .Stats.Chunks}}</div></div>
    <div class="card" title="completed transcripts not yet embedded (worker backlog)">
      <div class="card-label">Unembedded</div><div class="card-value{{if .EmbedStall}} amber{{end}}">{{commafy .Stats.EmbedBacklog}}</div></div>
    <div class="card" title="total embedding tokens (local tokenizer) across all embedded runs">
      <div class="card-label">Embed tokens</div><div class="card-value purple" title="{{commafy64Ptr .Stats.TotalEmbedTokens}}" aria-label="{{commafy64Ptr .Stats.TotalEmbedTokens}} embed tokens">{{compactNum64Ptr .Stats.TotalEmbedTokens}}</div></div>
  </div>
</div>

<div class="card-group">
  <div class="group-label">throughput</div>
  <div class="grid">
    <div class="card" title="mean transcription wall-clock time over runs the runner has timed">
      <div class="card-label">Avg proc / track</div><div class="card-value">{{procTime .Stats.AvgProcessingSeconds}}</div></div>
    <div class="card" title="jobs completed in the last hour">
      <div class="card-label">Done / hour</div><div class="card-value">{{commafy .Stats.DoneLastHour}}</div></div>
  </div>
</div>

<div class="card-group">
  <div class="group-label">library</div>
  <div class="grid">
    <div class="card" title="total audio duration across all indexed transcripts">
      <div class="card-label">Duration indexed</div><div class="card-value">{{durTime .Stats.TotalDurationSeconds}}</div></div>
    <div class="card" title="total transcript words across all indexed transcripts">
      <div class="card-label">Words indexed</div><div class="card-value" title="{{commafy64Ptr .Stats.TotalWords}}" aria-label="{{commafy64Ptr .Stats.TotalWords}} words indexed">{{compactNum64Ptr .Stats.TotalWords}}</div></div>
    <div class="card" title="book directories whose every track is done, out of all books">
      <div class="card-label">Books complete</div><div class="card-value green">{{commafy .Stats.BooksFullyDone}}{{if .Stats.BooksTotal}} <span class="card-denom">/ {{commafy .Stats.BooksTotal}}</span>{{end}}</div></div>
  </div>
</div>

{{if .EmbedStall}}
<div class="stall-callout">
  &#9888;&#xFE0F;&nbsp;{{commafy .Stats.EmbedBacklog}} completed transcripts are waiting to be embedded and the worker has made no progress for {{embedStallMins}}+ min — this is a real stall, not normal catch-up.
  Embedding (not transcription) is stuck. Check two things: (1) the embeddings endpoint — Ollama{{if .EmbedURL}} at {{.EmbedURL}}{{end}} reachable and the model pulled; and (2) if in-pipeline eval is enabled, the <strong>eval judge</strong>, which runs <em>before</em> embedding and shares the GPU — a wedged/slow judge blocks the queue too.
  Job rows stay <em>done</em> during an embed stall, so this is the only place it shows.
</div>
{{end}}

{{if gt .Stats.Failed 0}}
<div class="failed-callout">
  <span>&#9888;&#xFE0F;&nbsp;{{commafy .Stats.Failed}} failed job{{if gt .Stats.Failed 1}}s{{end}} &nbsp;<a href="#failed-region">view failed jobs &#8250;</a></span>
  <button class="btn btn-warn" hx-post="/actions/retry-failed" hx-target="#data-region" hx-swap="innerHTML"
          hx-confirm="Retry all {{.Stats.Failed}} failed job(s)? Each is reset to pending and re-transcribed.">retry all failed</button>
</div>
{{end}}

<!-- Queue section: the PRIMARY section showing books with remaining work, ordered
     active-first. This is the forward-looking view of what is in the pipeline. -->
<div class="section">
  {{- $queueLen := len .QueueBooks -}}
  {{- $queuePending := 0 -}}
  {{- $queueClaimed := 0 -}}
  {{- range .QueueBooks -}}
    {{- $queuePending = add $queuePending .Pending -}}
    {{- $queueClaimed = add $queueClaimed .Claimed -}}
  {{- end -}}
  <div class="section-title">Queue
    <span class="section-meta"> · {{commafy $queueLen}} book{{if ne $queueLen 1}}s{{end}}
    {{- if gt $queuePending 0}} · {{commafy $queuePending}} track{{if ne $queuePending 1}}s{{end}} pending{{end -}}
    {{- if gt $queueClaimed 0}} · {{commafy $queueClaimed}} transcribing{{end -}}
    </span>
  </div>
  {{if .QueueBooks}}
  <div class="activity-feed">
  {{range .QueueBooks}}{{template "bookRow" .}}{{end}}
  </div>
  {{if gt .QueueTotalBooks $queueLen}}
  <p class="lib-empty queue-truncated">+ {{commafy (sub .QueueTotalBooks $queueLen)}} more queued (showing first {{commafy $queueLen}})</p>
  {{end}}
  {{else}}<p class="lib-empty">Queue is empty — all caught up.</p>{{end}}
</div>

<!-- Recent Activity section: SECONDARY subsection showing recently-updated books
     that are NOT already in the queue above (deduped). Shows recently finished
     books and other recent changes. -->
<div class="section section-secondary">
  <div class="section-title section-title-secondary">Recent Activity (last {{len .ActivityBooks}} books)</div>
  {{if .ActivityBooks}}
  <div class="activity-feed">
  {{range .ActivityBooks}}{{template "bookRow" .}}{{end}}
  </div>
  {{else}}<p class="lib-empty">No activity yet.</p>{{end}}
</div>
`))

// ─── Library fragment ────────────────────────────────────────────────────────

var libraryFragmentTmpl = template.Must(template.New("library").Funcs(tmplFuncs).Parse(`
{{if .Overview}}{{template "overviewBand" .Overview}}{{end}}
<p class="subtitle">Your audiobook library — transcription progress per book. Open a book for tracks, transcript, and eval findings.</p>
<div class="lib-bar">
  <form class="lib-search" hx-get="/library/data" hx-target="#library-region" hx-swap="innerHTML">
    <input type="hidden" name="status" value="{{.Status}}">
    <input type="hidden" name="sort" value="{{.Sort}}">
    {{if .HasFindings}}<input type="hidden" name="findings" value="1">{{end}}
    <input type="search" name="q" value="{{.Query}}" placeholder="search author / title / track…" autocomplete="off">
    <button type="submit" class="btn">search</button>
    {{if or .Query .Status}}<a class="lib-clear" hx-get="/library/data?sort={{.Sort}}{{if .HasFindings}}&findings=1{{end}}{{if ne .PageSize 20}}&per={{.PageSize}}{{end}}" hx-target="#library-region" hx-swap="innerHTML">clear</a>{{end}}
  </form>
  <div class="lib-chips">
    <a class="chip{{if eq .Status ""}} active{{end}}"        hx-get="/library/data?q={{.QueryEscaped}}{{.FilterParams}}"                hx-target="#library-region" hx-swap="innerHTML">all</a>
    <a class="chip{{if eq .Status "pending"}} active{{end}}" hx-get="/library/data?status=pending&q={{.QueryEscaped}}{{.FilterParams}}" hx-target="#library-region" hx-swap="innerHTML">pending</a>
    <a class="chip{{if eq .Status "claimed"}} active{{end}}" hx-get="/library/data?status=claimed&q={{.QueryEscaped}}{{.FilterParams}}" hx-target="#library-region" hx-swap="innerHTML">transcribing</a>
    <a class="chip{{if eq .Status "done"}} active{{end}}"    hx-get="/library/data?status=done&q={{.QueryEscaped}}{{.FilterParams}}"    hx-target="#library-region" hx-swap="innerHTML">done</a>
    <a class="chip{{if eq .Status "failed"}} active{{end}}"  hx-get="/library/data?status=failed&q={{.QueryEscaped}}{{.FilterParams}}"  hx-target="#library-region" hx-swap="innerHTML">failed</a>
    <span class="chip-sep">·</span>
    <a class="chip chip-findings{{if .HasFindings}} active{{end}}" title="show only books with recorded findings"
       hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{if not .HasFindings}}&findings=1{{end}}{{if ne .Sort "recent"}}&sort={{.Sort}}{{end}}{{if ne .PageSize 20}}&per={{.PageSize}}{{end}}"
       hx-target="#library-region" hx-swap="innerHTML">&#9873; has findings</a>
  </div>
  <div class="lib-sort">
    <span class="lib-sort-label">sort</span>
    <a class="chip{{if eq .Sort "recent"}} active{{end}}"   hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{if .HasFindings}}&findings=1{{end}}{{if ne .PageSize 20}}&per={{.PageSize}}{{end}}"             hx-target="#library-region" hx-swap="innerHTML">recent</a>
    <a class="chip{{if eq .Sort "title"}} active{{end}}"    hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{if .HasFindings}}&findings=1{{end}}&sort=title{{if ne .PageSize 20}}&per={{.PageSize}}{{end}}"    hx-target="#library-region" hx-swap="innerHTML">title</a>
    <a class="chip{{if eq .Sort "progress"}} active{{end}}" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{if .HasFindings}}&findings=1{{end}}&sort=progress{{if ne .PageSize 20}}&per={{.PageSize}}{{end}}" hx-target="#library-region" hx-swap="innerHTML">progress</a>
    <a class="chip{{if eq .Sort "findings"}} active{{end}}" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{if .HasFindings}}&findings=1{{end}}&sort=findings{{if ne .PageSize 20}}&per={{.PageSize}}{{end}}" hx-target="#library-region" hx-swap="innerHTML">findings</a>
  </div>
</div>

{{if .Books}}
<div class="lib-colbar">
  {{if .MoreCols}}
  <a class="lib-clear" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{.FilterParams}}" hx-target="#library-region" hx-swap="innerHTML">&#8722;&nbsp;fewer columns</a>
  {{else}}
  <a class="lib-clear" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{.FilterParams}}&cols=more" hx-target="#library-region" hx-swap="innerHTML">+&nbsp;more columns (duration · words · chunks)</a>
  {{end}}
  <span class="lib-pagesize" title="books per page">
    <span class="lib-sort-label">show</span>
    {{range $sz := .PageSizes}}
    <a class="chip{{if eq $.PageSize $sz}} active{{end}}" hx-get="/library/data?status={{$.Status}}&q={{$.QueryEscaped}}{{if $.HasFindings}}&findings=1{{end}}{{if ne $.Sort "recent"}}&sort={{$.Sort}}{{end}}{{if ne $sz 20}}&per={{$sz}}{{end}}" hx-target="#library-region" hx-swap="innerHTML">{{$sz}}</a>
    {{end}}
  </span>
</div>
<div class="table-wrap">
<table>
  <caption>Library — {{commafy .TotalBooks}} book{{if ne .TotalBooks 1}}s{{end}}, newest activity first</caption>
  <thead><tr>
    <th scope="col">Book</th>
    <th scope="col">Author</th>
    <th scope="col">Progress</th>
    {{if .MoreCols}}
    <th scope="col" class="num" title="total audio duration across done tracks">Duration</th>
    <th scope="col" class="num" title="total transcript words across done tracks">Words</th>
    <th scope="col" class="num" title="total embedded chunks across done tracks">Chunks</th>
    {{end}}
    <th scope="col">State</th>
    <th scope="col">Updated</th>
    <th scope="col"><span class="sr-only">Open</span></th>
  </tr></thead>
  <tbody>
  {{range .Books}}
    <tr class="row-link">
      <td><a class="row-a file-name" href="/book?dir={{.Dir}}" title="{{.Dir}}">{{.Title}}{{if gt .FindingCount 0}} <span class="flag-badge" title="{{.FindingCount}} suspected-error finding(s) recorded">&#9873;&nbsp;{{commafy .FindingCount}}</span>{{end}}</a></td>
      <td class="time-muted">{{if .Author}}{{.Author}}{{else}}—{{end}}</td>
      <td>
        <div class="progress" title="{{.Done}}/{{.Total}} tracks done">
          <div class="progress-bar{{if gt .Failed 0}} has-failed{{end}}" style="width:{{.DonePct}}%"></div>
        </div>
        <span class="progress-text">{{.DonePct}}% &middot; {{commafy .Done}}/{{commafy .Total}}</span>
      </td>
      {{if $.MoreCols}}
      <td class="time-muted num">{{durTime .DurationSeconds}}</td>
      <td class="time-muted num">{{commafyPtr .WordCount}}</td>
      <td class="time-muted num">{{commafyPtr .EmbedChunkCount}}</td>
      {{end}}
      <td class="mini-badges">
        {{if gt .Pending 0}}<span class="badge pending">{{commafy .Pending}} pend</span>{{end}}
        {{if gt .Claimed 0}}<span class="badge claimed">{{commafy .Claimed}} transcribing</span>{{end}}
        {{if and (gt .Done 0) (eq .Pending 0) (eq .Claimed 0) (eq .Failed 0)}}<span class="badge done">{{statusBadge "done"}}</span>{{end}}
        {{if gt .Failed 0}}<span class="badge failed">{{commafy .Failed}} fail</span>{{end}}
      </td>
      <td class="time-muted" title="{{formatTime .LastUpdated}}">{{relTime .LastUpdated}}</td>
      <td class="actions"><span class="open-cue" aria-hidden="true">&#8250;</span></td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>

<div class="lib-pager">
  {{if .HasPrev}}<a class="btn" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{.FilterParams}}&offset={{.PrevOffset}}" hx-target="#library-region" hx-swap="innerHTML">&#8592;&nbsp;prev</a>{{end}}
  <span class="lib-meta">{{commafy .TotalBooks}} book{{if ne .TotalBooks 1}}s{{end}} &nbsp;·&nbsp; page {{.Page}} / {{.TotalPages}}</span>
  {{if .HasNext}}<a class="btn" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}{{.FilterParams}}&offset={{.NextOffset}}" hx-target="#library-region" hx-swap="innerHTML">next&nbsp;&#8594;</a>{{end}}
</div>
{{else}}
<p class="lib-empty">No books match this filter{{if .Query}} for &ldquo;{{.Query}}&rdquo;{{end}}.</p>
{{end}}

{{define "overviewBand"}}
<section class="overview" aria-label="Library status overview">
  <div class="ov-cards">
    <a class="ov-card accent-blue" href="/library" title="all indexed books">
      <span class="ov-label">Books</span>
      <span class="ov-value">{{commafy .TotalBooks}}</span>
    </a>
    <a class="ov-card accent-green" href="/library?status=done" title="books whose every track is transcribed">
      <span class="ov-label">Transcribed</span>
      <span class="ov-value green">{{.TranscribedPct}}%</span>
      <span class="ov-sub">{{commafy .FullyTranscribed}} of {{commafy .TotalBooks}} books</span>
    </a>
    <a class="ov-card{{if gt .FailedJobs 0}} accent-red{{else}} accent-green{{end}}" href="/library?status=failed" title="tracks that failed transcription">
      <span class="ov-label">Failed</span>
      <span class="ov-value{{if gt .FailedJobs 0}} red{{end}}"><span class="ov-glyph" aria-hidden="true">{{if gt .FailedJobs 0}}&#10007;{{else}}&#10003;{{end}}</span>{{commafy .FailedJobs}}</span>
      <span class="ov-sub">{{if gt .FailedJobs 0}}{{commafy .FailedBooks}} book{{if ne .FailedBooks 1}}s{{end}} affected{{else}}no failures{{end}}</span>
    </a>
    <a class="ov-card accent-purple" href="/findings" title="suspected transcription errors (read-only eval)">
      <span class="ov-label">Findings</span>
      <span class="ov-value purple">{{commafy .Findings}}</span>
      {{if gt .HighFindings 0}}<span class="ov-sub">{{commafy .HighFindings}} high-confidence</span>{{end}}
    </a>
    <a class="ov-card accent-{{.QueueAccent}}" href="/pipeline" title="pipeline queue + coordinator phase">
      <span class="ov-label">Queue</span>
      <span class="ov-value {{.QueueAccent}}">{{commafy .Queued}}</span>
      <span class="ov-sub">{{.PhaseLabel}}</span>
    </a>
  </div>
  <div class="ov-phase-line{{if .Attention}} attn{{end}}">
    <span class="ov-phase-ico" aria-hidden="true">{{.StatusGlyph}}</span>
    <span>{{.PlainStatus}}</span>
  </div>
  {{if .FailedBooks}}
  <div class="ov-attn">
    <span class="ov-attn-ico" aria-hidden="true">&#9888;&#xFE0F;</span>
    <span>Needs attention: {{commafy .FailedBooks}} book{{if ne .FailedBooks 1}}s{{end}} with failed tracks ({{commafy .FailedJobs}} track{{if ne .FailedJobs 1}}s{{end}}). <a href="/library?status=failed">Review failed &#8250;</a> — each failed track shows its error and a retry.</span>
  </div>
  {{end}}
  {{if and (not .FailedBooks) (gt .HighFindings 0)}}
  <div class="ov-attn attn-soft">
    <span class="ov-attn-ico" aria-hidden="true">&#9873;</span>
    <span>{{commafy .HighFindings}} high-confidence finding{{if ne .HighFindings 1}}s{{end}} flagged by the eval judge (advisory). <a href="/findings">View findings &#8250;</a></span>
  </div>
  {{end}}
</section>
{{end}}
`))

// ─── Book detail fragment ────────────────────────────────────────────────────

var bookFragmentTmpl = template.Must(template.New("book").Funcs(tmplFuncs).Parse(`
<div class="book-head">
  <div class="book-title">{{.Title}}</div>
  {{if .Author}}<div class="book-author">{{.Author}}</div>{{end}}
  <div class="book-stats">
    <span>{{commafy .Total}} track{{if ne .Total 1}}s{{end}}</span>
    <span class="time-muted">{{commafy .Done}} done</span>
    {{if gt .Pending 0}}<span class="time-muted">{{commafy .Pending}} pending</span>{{end}}
    {{if gt .Claimed 0}}<span class="time-muted">{{commafy .Claimed}} transcribing</span>{{end}}
    {{if gt .Failed 0}}<span style="color:var(--red)">{{commafy .Failed}} failed</span>{{end}}
  </div>
  <div class="book-path">{{.Dir}}</div>
  <div class="book-actions">
    <button class="btn btn-warn" hx-post="/actions/book-requeue?dir={{.DirQuery}}" hx-target="#book-region" hx-swap="innerHTML"
            hx-confirm="Re-transcribe all {{.Total}} track(s) of this book? Deletes their transcripts + embeddings and re-runs the runner.">requeue entire book</button>
    {{if .EvalConfigured}}
    <button class="btn btn-primary" hx-post="/actions/eval?dir={{.DirQuery}}" hx-target="#book-region" hx-swap="innerHTML"
            hx-confirm="Run the read-only LLM judge over this book's chunks? It flags suspected transcription errors (advisory only — transcripts are never edited) and may take a minute.">run eval</button>
    {{else}}
    <span class="eval-disabled" title="No eval chat endpoint is configured (AI_ROLES.eval / EVAL_CHAT_*).">run eval (no endpoint — <a href="/servers">configure</a>)</span>
    {{end}}
  </div>
  {{if .EvalNotice}}<div class="eval-notice" role="status">{{.EvalNotice}}</div>{{end}}
</div>

<div class="pipeline-panel">
  <div class="panel-title">Pipeline</div>
  <div class="pipeline-row">
    <span class="pipeline-label">Transcribe</span>
    <div class="progress" title="{{commafy .Done}}/{{commafy .Total}} tracks transcribed">
      <div class="progress-bar{{if gt .Failed 0}} has-failed{{end}}" style="width:{{pct .Done .Total}}%"></div>
    </div>
    <span class="progress-text">{{commafy .Done}}/{{commafy .Total}}</span>
  </div>
  <div class="pipeline-row">
    <span class="pipeline-label">Embed</span>
    {{if gt .EmbedTotal 0}}
    <div class="progress" title="{{commafy .EmbedDone}}/{{commafy .EmbedTotal}} transcribed tracks embedded">
      <div class="progress-bar" style="width:{{pct .EmbedDone .EmbedTotal}}%"></div>
    </div>
    <span class="progress-text">{{commafy .EmbedDone}}/{{commafy .EmbedTotal}}</span>
    {{else}}
    <span class="progress-text" title="no transcribed tracks to embed yet">—</span>
    {{end}}
  </div>
  <div class="pipeline-row">
    <span class="pipeline-label">Judge</span>
    {{if gt .FindingsCount 0}}
    <a class="judge-chip" href="#book-findings" title="jump to this book's findings">&#9873; {{commafy .FindingsCount}} finding{{if ne .FindingsCount 1}}s{{end}}</a>
    {{else}}
    <span class="judge-chip judge-clean" title="no suspected-error findings recorded; the eval judge records no &quot;evaluated&quot; marker, so this is not a claim that the book was judged clean">no findings yet</span>
    {{end}}
  </div>
</div>

<form class="lib-search" hx-post="/search/book?dir={{.DirQuery}}" hx-target="#book-search-results" hx-swap="innerHTML">
  <input type="search" name="q" placeholder="search this book's transcript…" autocomplete="off">
  <button type="submit" class="btn">search</button>
</form>
<div id="book-search-results"></div>

{{if .Tracks}}
<div class="table-wrap">
<table>
  <caption>Tracks in this book</caption>
  <thead><tr><th scope="col">Track</th><th scope="col">Status</th>
    <th scope="col" class="num" title="audio duration">Duration</th>
    <th scope="col" class="num" title="transcript word count (runner)">Words</th>
    <th scope="col" class="num" title="transcription wall-clock time (runner)">Proc</th>
    <th scope="col" title="audio codec · channel layout (runner)">Codec</th>
    <th scope="col" class="num" title="embedded chunks">Chunks</th>
    <th scope="col" class="num" title="suspected-error findings recorded for this track (read-only eval)">Flags</th>
    <th scope="col">Updated</th><th scope="col"><span class="sr-only">Actions</span></th></tr></thead>
  <tbody>
  {{range .Tracks}}
    <tr>
      <td><a class="file-name" href="/track?id={{.ID}}" title="{{.FilePath}}">{{shortName .FilePath}}</a>
          {{if .Error}}<details class="error-row"><summary>show error</summary><pre>{{derefStr .Error}}</pre></details>{{end}}</td>
      <td><span class="badge {{.Status}}">{{statusBadge .Status}}</span></td>
      <td class="time-muted">{{durTime .DurationSeconds}}</td>
      <td class="time-muted">{{commafyPtr .WordCount}}</td>
      <td class="time-muted">{{procTime .ProcessingSeconds}}</td>
      <td class="time-muted">{{codecLabel .AudioCodec .AudioChannels}}</td>
      <td class="time-muted">{{commafyPtr .EmbedChunkCount}}</td>
      <td class="time-muted">{{with index $.FindingsByTrack .FilePath}}<a href="#book-findings" title="{{.}} finding(s)">&#9873; {{.}}</a>{{else}}—{{end}}</td>
      <td class="time-muted" title="{{formatTime .UpdatedAt}}">{{relTime .UpdatedAt}}</td>
      <td class="actions">
        {{if or (eq .Status "done") (eq .Status "failed")}}
        <button class="btn" hx-post="/actions/requeue?id={{.ID}}&book={{$.DirQuery}}" hx-target="#book-region" hx-swap="innerHTML"
                hx-confirm="Re-transcribe this track?">requeue</button>
        {{end}}
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>
{{else}}<p class="lib-empty">No tracks found for this book.</p>{{end}}

<div id="book-findings" class="section">
  <div class="section-title">Findings</div>
  {{if .FindingsSummary}}
  <p class="findings-summary">{{commafy .FindingsSummary.Count}} suspected error{{if ne .FindingsSummary.Count 1}}s{{end}}
     &nbsp;·&nbsp; mean conf {{confPctF .FindingsSummary.MeanConfidence}}
     &nbsp;·&nbsp; top issue {{.FindingsSummary.TopIssueType}}</p>
  {{else}}
  <p class="lib-empty">No findings recorded for this book. — Run the read-only LLM judge with <strong>run eval</strong> above to flag suspected transcription errors (advisory only).</p>
  {{end}}
  {{if and .ControlEnabled .Findings}}
  <div class="findings-actions">
    <button class="btn btn-danger" hx-post="/actions/findings-clear?dir={{.DirQuery}}" hx-target="#book-region" hx-swap="innerHTML"
            hx-confirm="Delete this book's recorded findings? Advisory data only — transcripts are untouched and findings can be regenerated by re-running eval.">clear book findings</button>
  </div>
  {{end}}
  {{if .Findings}}
  <div class="table-wrap">
  <table>
    <caption>Suspected errors in this book — advisory only, transcripts are never edited</caption>
    <thead><tr>
      <th scope="col" class="num" title="judge self-scored confidence (triage highest-first)">Conf</th>
      <th scope="col">Issue</th>
      <th scope="col" title="suspected span → suggested correction (advisory only)">Correction</th>
      <th scope="col" title="track · timestamp">Where</th>
    </tr></thead>
    <tbody>
    {{range .Findings}}
      <tr>
        <td>{{confPctF .Confidence}}</td>
        <td>{{.IssueType}}</td>
        <td>{{.OriginalText}} &#8594; {{strPtr .SuggestedCorrection}}</td>
        <td>{{if .JobID}}<a class="file-name" href="/track?id={{derefStr .JobID}}&t={{.StartSec}}" title="open the transcript at this point">{{shortName .FilePath}}</a>{{else}}<span class="file-name" title="{{.FilePath}}">{{shortName .FilePath}}</span>{{end}} &#183; {{timestamp .StartSec}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{end}}
</div>
`))

// ─── Per-book search results fragment ────────────────────────────────────────

// bookSearchFragmentTmpl renders the matching chunks for the per-book search
// box: each row shows the track filename, the chunk's timestamp range, and the
// matched text. Reuses the trigram TextSearch scoped to the book.
var bookSearchFragmentTmpl = template.Must(template.New("booksearch").Funcs(tmplFuncs).Parse(`
{{if .Query}}
<div class="section-title">Search results for &ldquo;{{.Query}}&rdquo; ({{len .Results}})</div>
{{if .Results}}
<div class="table-wrap">
<table>
  <caption>Matching transcript chunks in this book</caption>
  <thead><tr><th scope="col">Track</th><th scope="col">Time</th><th scope="col">Match</th></tr></thead>
  <tbody>
  {{range .Results}}
    <tr>
      <td><div class="file-name" title="{{.FilePath}}">{{shortName .FilePath}}</div></td>
      <td class="time-muted">{{timestamp .StartSec}} &#8594; {{timestamp .EndSec}}</td>
      <td>{{.Content}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>
{{else}}<p class="lib-empty">No matches in this book for &ldquo;{{.Query}}&rdquo;.</p>{{end}}
{{end}}
`))

// ─── Transcript segments page fragment (reader "load more") ──────────────────

// segmentsFragmentTmpl is the htmx "load more" response for the transcript
// reader: the next page of segment rows, followed by a fresh load-more button
// (which replaces itself via outerHTML) when more remain. The button the user
// clicked is swapped out (hx-swap="outerHTML"), so appending rows + a new button
// here continues the chain cleanly.
var segmentsFragmentTmpl = template.Must(template.New("segments").Funcs(tmplFuncs).Parse(`
{{$start := .StartOffset}}
{{range $i, $seg := .Segments}}
<div id="seg-{{add $start $i}}" class="seg">
  <span class="seg-time">[{{timestamp $seg.Start}} &#8594; {{timestamp $seg.End}}]</span>
  {{if $seg.Speaker}}<span class="seg-speaker">{{derefStr $seg.Speaker}}</span>{{end}}
  <span class="seg-text">{{$seg.Text}}</span>
</div>
{{end}}
{{if .HasMore}}
<button class="btn load-more" hx-get="/track/segments?id={{.IDQuery}}&offset={{.NextOffset}}"
        hx-swap="outerHTML" hx-target="this">load more</button>
{{end}}
`))

// ─── Track detail fragment ───────────────────────────────────────────────────

var trackFragmentTmpl = template.Must(template.New("track").Funcs(tmplFuncs).Parse(`
<a class="back-link" href="/book?dir={{.BackDir}}">&#8592;&nbsp;Book</a>
<div class="book-head">
  <div class="book-title">{{.Title}}</div>
  {{if .Author}}<div class="book-author">{{.Author}}</div>{{end}}
  <div class="book-stats">
    <span><span class="badge {{.Detail.Status}}">{{statusBadge .Detail.Status}}</span></span>
    {{if .Detail.HasTranscript}}<span class="time-muted">{{durTime .DurationPtr}}</span>{{end}}
    <span class="time-muted" title="{{formatTime .Detail.UpdatedAt}}">updated {{relTime .Detail.UpdatedAt}}</span>
  </div>
  <div class="book-path">{{.Detail.FilePath}}</div>
  {{if .Detail.Error}}<details class="error-row" style="margin-top:10px"><summary>show error</summary><pre>{{derefStr .Detail.Error}}</pre></details>{{end}}
</div>

<div class="panels">
  <div class="panel">
    <div class="panel-title">Audio</div>
    <dl class="kv">
      <dt>Size</dt><dd>{{bytesPtr .Detail.AudioBytes}}</dd>
      <dt>Codec</dt><dd>{{strPtr .Detail.AudioCodec}}</dd>
      <dt>Channels</dt><dd>{{commafyPtr .Detail.AudioChannels}}</dd>
      <dt>Sample rate</dt><dd>{{hzPtr .Detail.AudioSampleRate}}</dd>
      <dt>Format</dt><dd>{{strPtr .Detail.AudioFormat}}</dd>
      <dt>Duration</dt><dd>{{if .Detail.HasTranscript}}{{durTime .DurationPtr}}{{else}}—{{end}}</dd>
    </dl>
  </div>
  <div class="panel">
    <div class="panel-title">Transcription</div>
    <dl class="kv">
      <dt>Model</dt><dd>{{if .Detail.HasTranscript}}{{if .Detail.ModelName}}{{.Detail.ModelName}}{{else}}{{strPtr .Detail.ASRModel}}{{end}}{{else}}—{{end}}</dd>
      <dt>Language</dt><dd>{{if .Detail.Language}}{{.Detail.Language}}{{else}}—{{end}}</dd>
      <dt>Compute</dt><dd>{{strPtr .Detail.ComputeType}}</dd>
      <dt>Runner</dt><dd>{{strPtr .Detail.RunnerHost}}</dd>
      <dt>Proc time</dt><dd>{{procTime .Detail.ProcessingSeconds}}</dd>
      <dt>Chunked</dt><dd>{{boolPtr .Detail.Chunked}}{{if .Detail.NWindows}} ({{commafyPtr .Detail.NWindows}} windows){{end}}</dd>
      <dt>Words</dt><dd>{{commafyPtr .Detail.WordCount}}</dd>
      <dt>Segments</dt><dd>{{if .Detail.HasTranscript}}{{commafy (len .Detail.Segments)}}{{else}}{{commafyPtr .Detail.SegmentCount}}{{end}}</dd>
      <dt>Characters</dt><dd>{{commafyPtr .Detail.CharCount}}</dd>
      <dt>Speakers</dt><dd>{{commafyPtr .Detail.SpeakerCount}}</dd>
    </dl>
  </div>
  <div class="panel">
    <div class="panel-title">Embedding</div>
    <dl class="kv">
      <dt>Model</dt><dd>{{strPtr .Detail.EmbedModel}}</dd>
      <dt>Chunks</dt><dd>{{if .Detail.Chunks}}{{commafy (len .Detail.Chunks)}}{{else}}{{commafyPtr .Detail.EmbedChunkCount}}{{end}}</dd>
      <dt>Prompt tokens</dt><dd>{{commafyPtr .Detail.EmbedPromptTokens}}</dd>
      <dt>Total tokens</dt><dd>{{commafyPtr .Detail.EmbedTotalTokens}}</dd>
    </dl>
  </div>
</div>

{{if .Detail.HasTranscript}}
  <div class="section">
    <div class="section-title">Transcript ({{commafy .TotalSegments}} segment{{if ne .TotalSegments 1}}s{{end}})</div>
    {{if .PageSegments}}
    <div class="reader">
      {{$target := .TargetSeg}}
      {{/* Anchor id == global segment index. The initial reader preload always
           starts at global index 0 (handleTrackData loads [0, targetPage+1)), so
           the slice index $i IS the global index here. The /track/segments
           load-more fragment continues the sequence with {{add $start $i}}; if
           this initial preload is ever changed to start at a non-zero offset,
           switch this to the same $start+$i scheme. */}}
      {{range $i, $seg := .PageSegments}}
      <div id="seg-{{$i}}" class="seg{{if and $target (eq $i (deref $target))}} seg-active{{end}}">
        <span class="seg-time">[{{timestamp $seg.Start}} &#8594; {{timestamp $seg.End}}]</span>
        {{if $seg.Speaker}}<span class="seg-speaker">{{derefStr $seg.Speaker}}</span>{{end}}
        <span class="seg-text">{{$seg.Text}}</span>
      </div>
      {{end}}
      {{if .HasMore}}
      <button class="btn load-more" hx-get="/track/segments?id={{.IDQuery}}&offset={{.NextOffset}}"
              hx-swap="outerHTML" hx-target="this">load more ({{commafy (sub .TotalSegments .NextOffset)}} remaining)</button>
      {{end}}
    </div>
    {{if .TargetSeg}}
    <script>
      // Deep-jump: scroll the preloaded target segment into view after the
      // fragment settles. The #seg-N anchor is already in the initial DOM
      // (handleTrackData preloaded pages [0..target]), so no extra fetch.
      (function () {
        var el = document.getElementById('seg-{{deref .TargetSeg}}');
        if (el) { el.scrollIntoView({block: 'center'}); }
      })();
    </script>
    {{end}}
    {{else}}<p class="lib-empty">Transcript has no segments.</p>{{end}}
  </div>

  <div class="section">
    <div class="section-title">Chunks ({{commafy (len .Detail.Chunks)}})</div>
    {{if .Detail.Chunks}}
    <div class="table-wrap">
    <table>
      <caption>Embedding chunks for this track</caption>
      <thead><tr><th scope="col" class="num">#</th><th scope="col">Time range</th><th scope="col" class="num">Chars</th><th scope="col">Speaker</th></tr></thead>
      <tbody>
      {{range .Detail.Chunks}}
        <tr>
          <td class="time-muted">{{.ChunkIndex}}</td>
          <td class="time-muted">{{timestamp .StartSec}} &#8594; {{timestamp .EndSec}}</td>
          <td class="time-muted">{{commafy .CharCount}}</td>
          <td class="time-muted">{{if .Speaker}}{{derefStr .Speaker}}{{else}}—{{end}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    </div>
    {{else}}<p class="lib-empty">Not embedded yet — chunks appear after the worker's next embed pass.</p>{{end}}
  </div>
{{else}}
  <div class="section">
    <p class="lib-empty">Not transcribed yet — this track is <span class="badge {{.Detail.Status}}">{{statusBadge .Detail.Status}}</span>. The transcript reader and chunk list appear once the runner completes it.</p>
  </div>
{{end}}
`))

// ─── Template models ─────────────────────────────────────────────────────────

type pageShell struct {
	Title     string
	Nav       string
	DirQuery  string // book page: URL-escaped dir for the hx-get fragment load
	IDQuery   string // track page only: URL-escaped job id for the hx-get fragment load
	TQuery    string // track page only: "&t=<startSec>" deep-jump suffix (empty when absent)
	DataQuery string // library page only: "?status=…&q=…" for the initial fragment load

	// Topbar phase + paused badge (read-only — see statusData.Phase). Populated by
	// renderPage on every page so pipeline state is visible everywhere while the
	// live controls stay on the Pipeline page (D2). PhaseKnown is false when the
	// phase read failed, so the badge is suppressed rather than showing a stale
	// default.
	Phase      string
	PhaseClass string
	Paused     bool
	PhaseKnown bool
	// PhaseIcon is a small glyph paired with the phase word so the pill reads at a
	// glance without relying on the border tint alone (color-only avoidance, 1.4.1).
	// PhaseReason is a one-line plain-language reason string for the title tooltip.
	PhaseIcon   template.HTML
	PhaseReason string
}

// activityTrack is a condensed per-track row for the book-grouped activity feed
// (the inline expansion inside a <details> row).
type activityTrack struct {
	ID        string
	FilePath  string
	ShortName string // path.Base(FilePath)
	Status    string
	// Nullable metrics — nil renders as "—" in the template.
	DurationSeconds   *float64
	ProcessingSeconds *float64
	EmbedChunkCount   *int
	UpdatedAt         time.Time
}

// activityBook is one book row in the pipeline page's book-grouped activity feed.
// It carries both the per-book summary counters (for the <summary> line) and
// the full per-track list (for the inline expansion body). Both are rendered
// server-side inside the 3s-polled fragment, so the counters and track status
// tick live without re-fetching.
type activityBook struct {
	Dir     string // stable key written into data-book-id
	Title   string
	Author  string
	Total   int
	Pending int
	Claimed int
	Done    int
	Failed  int
	DonePct int
	// LastUpdated is the max(updated_at) across all of the book's tracks.
	LastUpdated time.Time
	Tracks      []activityTrack
}

// activityBookFeedLimit is the maximum number of books shown in the pipeline
// activity feed (bounded N+1: one GetBookTracks call per book).
const activityBookFeedLimit = 12

// queueBookFeedLimit is the maximum number of books shown in the queue section
// (bounded N+1: one GetBookTracks call per book).
const queueBookFeedLimit = 20

type statusData struct {
	Stats *db.QueueStats
	Jobs  []db.RecentJob

	// QueueBooks is the forward-looking queue section (primary). It holds up to
	// queueBookFeedLimit books that have remaining work (pending or claimed tracks),
	// ordered by active-first (claimed > 0, then most-claimed, then most-pending,
	// then longest-waiting). This is the primary focus of the Pipeline page.
	QueueBooks []activityBook

	// QueueTotalBooks is the total number of books in the queue (from the DB count),
	// used to detect truncation when len(QueueBooks) < QueueTotalBooks.
	QueueTotalBooks int

	// ActivityBooks is the book-grouped activity feed (secondary subsection). It
	// holds up to activityBookFeedLimit books ordered by most-recently updated,
	// excluding books already shown in QueueBooks (so it reads as "recently
	// finished / other recent changes" rather than duplicating the live queue).
	ActivityBooks []activityBook

	RenderedAt string
	EmbedStall bool
	EmbedURL   string

	// Backfill progress (derived).
	DonePct        int
	ProgressText   string // "317 / 4,069 (8%)"
	ThroughputText string // "~22 done in the last hour"
	ETAText        string // "~6.9 days left" / "—"
	ShowProgress   bool   // false on a fresh/empty install

	// Unified pipeline state (derived from paused + runner liveness).
	StateLabel string
	StateClass string
	DotClass   string
	SubText    string
	MetaText   string

	// Coordinator phase (read-only — the dashboard READS runner_control.phase but
	// never writes it; the `earmark batch` coordinator owns transitions, CONTRACT
	// §1.4). Phase is the raw word ("idle"|"transcribe"|"analyze") and PhaseClass
	// the matching CSS modifier ("phase-idle" etc.) for the badge tint.
	Phase      string
	PhaseClass string

	// Run-budget control state (the Pipeline page's bounded-run controls). RunLimit
	// is the current bound (nil = unlimited); RunBudgetText is the human label.
	// ControlEnabled gates the live control writes: true only when a
	// CONTROL_API_TOKEN is configured, so the controls render honestly disabled
	// (with an explanation) on an unauthenticated deployment instead of POSTing
	// into a guaranteed 503.
	RunLimit       *int
	RunBudgetText  string
	ControlEnabled bool

	// Lifecycle is the derived 3-stage pipeline view (Transcribe → Eval → Embed).
	// It is computed in renderStatusFragment by calling computePipelineLifecycle
	// after the arbiter probe, and threaded into the template so the status strip
	// and GPU/VRAM cards can render with a single data source.
	Lifecycle pipelineLifecycle

	// PrimaryArbiter is the gpu-arbiter probe result for the primary ASR server.
	// Zero-value (Reachable=false) when no probe is configured or the probe failed.
	// It carries the VRAM metrics and resident unit list for the GPU card group.
	PrimaryArbiter arbiterStatus
}

type bookRow struct {
	Dir         string
	Title       string
	Author      string
	DonePct     int
	Total       int
	Pending     int
	Claimed     int
	Done        int
	Failed      int
	LastUpdated time.Time

	// Per-book aggregates over done tracks (nullable — em dash when none done).
	DurationSeconds *float64
	WordCount       *int
	EmbedChunkCount *int

	// FindingCount is this book's recorded findings count (the ⚑ column), looked
	// up from the one-shot GetFindingsCountByBook aggregate by the book's Dir; 0
	// when the book has no findings.
	FindingCount int
}

// overviewData backs the home status-overview band (the "is everything okay?"
// rollup at the top of the library page). All counts are whole-library — the band
// only renders on the unfiltered home view, so it answers the health question
// independent of the active library filter. Cards link to their drill-down.
type overviewData struct {
	TotalBooks       int
	FullyTranscribed int
	TranscribedPct   int
	FailedJobs       int    // failed transcription jobs (tracks)
	FailedBooks      int    // distinct books with ≥1 failed track
	Findings         int    // total recorded findings
	HighFindings     int    // confidence ≥ 0.8 (the triage-first bucket)
	Queued           int    // pending + claimed
	QueueAccent      string // "blue" (work queued) / "green" (drained) — never red for paused
	PhaseLabel       string // short queue-card sub: "idle" / "transcribing" / "paused"
	PlainStatus      string // one-line plain-language status under the cards
	StatusGlyph      template.HTML
	Attention        bool // true → the phase line reads as needs-attention (failures only)
}

type libraryData struct {
	// Overview is the home status-overview band, populated only on the unfiltered
	// home view (nil on a filtered/searched library so it doesn't contradict the
	// active filter).
	Overview *overviewData
	// MoreCols toggles the opt-in jargon columns (Duration/Words/Chunks) — off by
	// default so the persona-essential columns lead (?cols=more turns them on).
	MoreCols bool

	Books        []bookRow
	Status       string
	Query        string
	QueryEscaped string
	Page         int
	TotalPages   int
	TotalBooks   int
	HasPrev      bool
	HasNext      bool
	PrevOffset   int
	NextOffset   int
	// PageSize is the active per-page count (one of PageSizes); PageSizes are the
	// selectable options rendered as the "show N" control.
	PageSize  int
	PageSizes []int

	// Sort is the active sort mode ("recent"|"title"|"progress"|"findings");
	// HasFindings is the ⚑ has-findings filter chip state. Both are threaded
	// through every pager + chip link (FilterParams) so the controls persist
	// across navigation.
	Sort        string
	HasFindings bool
	// FilterParams is the "&sort=…&findings=…" suffix appended to every chip and
	// pager link so the active sort + findings filter survive a status/page change.
	FilterParams template.URL
}

type bookData struct {
	Dir      string
	DirQuery string
	Title    string
	Author   string
	Total    int
	Pending  int
	Claimed  int
	Done     int
	Failed   int
	Tracks   []db.RecentJob
	// EvalConfigured gates the "run eval" button: true only when a judge chat
	// endpoint resolves (CONTRACT §2.15). When false the button is hidden with a
	// pointer to the Models/Services page rather than POSTing into a failure.
	EvalConfigured bool
	// EvalNotice is a transient post-action banner ("evaluating N chunks…" or
	// "an eval run is already in flight"). Empty on a normal fragment load.
	EvalNotice string

	// FindingsSummary is this book's findings roll-up (the matching ByBook entry
	// from GetFindingsSummary), nil when the book has no findings.
	FindingsSummary *db.BookFindings
	// Findings is this book's individual findings (highest-confidence first).
	Findings []db.FindingRow
	// ControlEnabled gates the per-book clear-findings button: it renders only
	// when a CONTROL_API_TOKEN is configured (mirrors findingsData) and findings
	// exist, so it never appears as a button that would fail-close (503) on click.
	ControlEnabled bool
	// FindingsByTrack maps a track file_path → its finding count, for the ⚑ N cell
	// on the per-book Tracks table (built from Findings — no extra query).
	FindingsByTrack map[string]int

	// Pipeline panel (the three honest pipeline elements above the tracks table).
	// Transcribe and Embed are real progress bars derived from the per-track list;
	// Judge is an honest status/count, not a bar — the schema has no "evaluated"
	// marker, so a judged-clean book is indistinguishable from a never-judged one.
	//
	//   - Transcribe: TranscribeDone/Total (== Done/Total; tracks status=="done").
	//   - Embed:      EmbedDone/EmbedTotal, where EmbedTotal == count(done tracks)
	//                 and EmbedDone == count(done tracks with EmbedChunkCount set).
	//                 When EmbedTotal == 0 (no done tracks) the bar is omitted (—).
	//   - Judge:      FindingsCount findings (chip). 0 → "no findings yet" (NOT a
	//                 claim of "evaluated" — we cannot know).
	EmbedDone     int
	EmbedTotal    int
	FindingsCount int
}

type failedData struct {
	Jobs []db.FailedJob
}

type bookSearchData struct {
	Query   string
	Results []db.SearchResultWithMetadata
}

type trackData struct {
	Detail  *db.TrackDetail
	Title   string
	Author  string
	BackDir string // URL-escaped book dir (dirname of file_path) for use in href query params
	// DurationPtr adapts the non-pointer Detail.DurationSeconds to the *float64
	// the durTime helper expects (nil → em dash when no transcript).
	DurationPtr *float64

	// Transcript reader pagination (P7): the first page of segments plus a "load
	// more" affordance when there are more than segmentPageSize total.
	TotalSegments int
	PageSegments  []db.Segment
	HasMore       bool
	NextOffset    int
	IDQuery       string // URL-escaped job id for the "load more" hx-get

	// Deep-jump (D3): when a finding "Where" link carried &t=<startSec>,
	// TargetSeg is the slice index of the segment to scroll to / highlight, and
	// the reader preloads pages [0..target page] so the #seg-N anchor is in the
	// initial DOM. Nil when no t param was given (the normal first-page load).
	// Keyed off slice position (NOT db.Segment.ID, which is the ASR runner's
	// JSON id and not guaranteed contiguous), so it matches the offset-based
	// pagination cursor.
	TargetSeg *int
}

// segmentsData backs the segment-page fragment (the htmx "load more" target):
// one page of segments plus the link to the next page. StartOffset is the
// absolute slice index of this page's first segment, so each row's #seg-N anchor
// stays consistent with the initial-page anchors (a deep-jump target may live in
// a later page if the operator pages forward manually).
type segmentsData struct {
	Segments    []db.Segment
	HasMore     bool
	NextOffset  int
	StartOffset int
	IDQuery     string
}

// newStatusData derives the unified pipeline state from the pause flag and the
// runner heartbeat freshness, so the banner is never self-contradictory.
func newStatusData(stats *db.QueueStats, jobs []db.RecentJob, now time.Time, staleAfter time.Duration, embedURL string, est *predict.Estimate) statusData {
	d := statusData{
		Stats:      stats,
		Jobs:       jobs,
		RenderedAt: now.UTC().Format("15:04:05 UTC"),
		EmbedURL:   embedURL,
		// A genuine embed stall = a real backlog AND the worker has written no new
		// chunk for embedStallWindow. Crossing the threshold alone is normal during
		// an active run (in-pipeline eval runs before embedding, so the backlog
		// rides up and drains on its own). Only flag it when embeds have actually
		// stopped landing — a nil LastEmbedAt with a backlog means nothing has ever
		// embedded, which also counts as stalled.
		EmbedStall: stats.EmbedBacklog >= embedStallThreshold &&
			(stats.LastEmbedAt == nil || now.Sub(*stats.LastEmbedAt) > embedStallWindow),
	}

	// Backfill progress / throughput / ETA.
	if stats.TotalJobs > 0 {
		d.ShowProgress = true
		d.DonePct = stats.Done * 100 / stats.TotalJobs
		d.ProgressText = fmt.Sprintf("%s / %s tracks (%d%%)", commafy(stats.Done), commafy(stats.TotalJobs), d.DonePct)

		if stats.DoneLastHour > 0 {
			d.ThroughputText = fmt.Sprintf("~%s done in the last hour", commafy(stats.DoneLastHour))
		} else {
			d.ThroughputText = "no completions in the last hour"
		}

		// ETA: prefer the empirical predict model (CONTRACT §4) when supplied — it
		// degrades gracefully (calendar when availability history exists, else a
		// labeled work-time estimate) instead of collapsing to "—" during a stall.
		// Fall back to the legacy remaining/DoneLastHour calc only when no estimate
		// is provided (e.g. older unit tests).
		switch {
		case est != nil:
			d.ETAText = est.Label()
		case stats.DoneLastHour > 0:
			remaining := stats.Pending + stats.Claimed
			d.ETAText = humanizeETA(float64(remaining) / float64(stats.DoneLastHour))
		default:
			d.ETAText = "—"
		}
	}

	fresh := false
	if stats.LastHeartbeat != nil {
		age := now.Sub(*stats.LastHeartbeat)
		d.MetaText = "last heartbeat " + humanizeSince(age)
		fresh = age <= staleAfter
	}

	switch {
	case stats.Paused:
		d.StateLabel, d.StateClass, d.DotClass = "PAUSED", "state-paused", "amber"
		d.SubText = "runner is not claiming new work"
	case stats.RunnerActive && fresh:
		d.StateLabel, d.StateClass, d.DotClass = "RUNNING", "state-running", "green"
		if stats.RunnerID != "" {
			d.SubText = "runner " + stats.RunnerID + " is transcribing"
		} else {
			d.SubText = "runner is transcribing"
		}
	case stats.RunnerActive && (stats.Pending+stats.Claimed) > 0:
		// A runner claimed work but its heartbeat went stale while jobs are still
		// waiting — that's a crashed/wedged runner, an incident, not idleness.
		d.StateLabel, d.StateClass, d.DotClass = "STALLED", "state-stalled", "red"
		d.SubText = "runner heartbeat is stale and work is waiting — runner may have crashed"
	default:
		// Not paused and no fresh active runner. Two very different situations
		// hide here, so distinguish them — and note this branch only knows claim
		// ACTIVITY (claimed-job heartbeats), NOT socket connectivity, so it must
		// not assert "no runner connected" (that contradicts the Servers page,
		// which shows live gpu-arbiter connectivity).
		d.StateLabel, d.StateClass, d.DotClass = "IDLE", "state-idle", "blue"
		switch {
		case stats.RunnerActive:
			// A runner claimed a job but its heartbeat went stale; nothing waiting.
			d.SubText = "enabled — runner heartbeat is stale (no work waiting)"
		case stats.Pending > 0:
			// Work is queued but nothing is claiming it — the ASR runner is likely
			// stopped/parked. Point at the Servers page for live runner status.
			d.SubText = fmt.Sprintf("enabled — %s job%s queued, waiting for a runner to claim (see Servers for live runner status)",
				commafy(stats.Pending), plural(stats.Pending))
		default:
			// Queue drained — genuinely, benignly idle.
			d.SubText = "enabled — queue drained, nothing to transcribe"
		}
	}
	return d
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// statusLabel maps an internal job status to the operator-facing word. Only
// "claimed" differs — it reads as "transcribing" to a human watching the
// pipeline; the stored value, CSS class, and filter param stay "claimed".
func statusLabel(status string) string {
	if status == "claimed" {
		return "transcribing"
	}
	return status
}

// statusBadge renders a status badge's inner HTML: a leading glyph paired with the
// operator-facing label, so job state is conveyed by shape + text, not color alone
// (WCAG 1.4.1). The glyphs are fixed entities (no user input), so the template.HTML
// return is safe.
func statusBadge(status string) template.HTML {
	var ico string
	switch status {
	case "done":
		ico = "&#10003;" // ✓
	case "failed":
		ico = "&#10007;" // ✗
	case "claimed":
		ico = "&#9658;" // ►
	case "pending":
		ico = "&#9675;" // ○
	default:
		ico = "&#9679;" // ●
	}
	// #nosec G203 -- ico is one of a fixed set of literal HTML entities (no user
	// input) and the only variable part, statusLabel(status), is escaped with
	// template.HTMLEscapeString; the result is safe to emit unescaped.
	return template.HTML(`<span class="badge-ico" aria-hidden="true">` + ico + `</span>` + template.HTMLEscapeString(statusLabel(status)))
}

// timestamp renders a non-negative float seconds offset as a clock string:
// "mm:ss" below an hour, "h:mm:ss" at or above one. Used by the transcript
// reader and chunk list. Negative input is clamped to 0.
func timestamp(secs float64) string {
	if secs < 0 {
		secs = 0
	}
	total := int64(secs) // truncate — segment boundaries are already second-ish
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// humanizeBytes renders a byte count as a human-readable size (KB/MB/GB, base
// 1024). Used for the run_metrics audio_bytes field.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// channelLabel maps an audio channel count to a human layout word; for unusual
// counts it falls back to "Nch".
func channelLabel(n int) string {
	switch n {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	default:
		return strconv.Itoa(n) + "ch"
	}
}

// codecLabel renders "codec · channels" (e.g. "aac · stereo") from the nullable
// run_metrics audio fields, degrading to just the codec, just the channel
// layout, or an em dash when both are absent.
func codecLabel(codec *string, channels *int) string {
	var parts []string
	if codec != nil && *codec != "" {
		parts = append(parts, *codec)
	}
	if channels != nil && *channels > 0 {
		parts = append(parts, channelLabel(*channels))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

func commafy(n int) string { return commafy64(int64(n)) }

// vramLabel renders "N.N / M.M GB" from nullable MB values (gpu-arbiter probe),
// or an em dash when either pointer is nil.
func vramLabel(usedMB, totalMB *int) string {
	if usedMB == nil || totalMB == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f / %.1f GB", float64(*usedMB)/1024, float64(*totalMB)/1024)
}

func commafy64(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// compactNum64 renders large magnitudes compactly so wide stat-card values
// don't overflow their cards: below 1,000,000 it keeps the exact comma form;
// at/above 1M it abbreviates to one decimal with a M/B/T suffix (e.g.
// 14,826,533 → "14.8M", 13,000,000 → "13M"). Templates surface the exact value
// in a tooltip alongside it.
func compactNum64(n int64) string {
	// Below 1M (either sign) keep the exact comma form.
	if n > -1_000_000 && n < 1_000_000 {
		return commafy64(n)
	}
	// Work in float64 to avoid the int64 abs overflow at math.MinInt64.
	f := float64(n)
	neg := f < 0
	if neg {
		f = -f
	}
	// Escalate to the next unit when one-decimal rounding would otherwise hit
	// 1000 of the current unit (e.g. 999,999,999 must read "1B", not "1000M").
	units := []string{"M", "B", "T"} // 1e6, 1e9, 1e12
	div := 1e6
	i := 0
	for i < len(units)-1 && f/div >= 999.95 {
		div *= 1000
		i++
	}
	s := strings.TrimSuffix(strconv.FormatFloat(f/div, 'f', 1, 64), ".0") // 13.0M → 13M
	if neg {
		s = "-" + s
	}
	return s + units[i]
}

func compactNum(n int) string { return compactNum64(int64(n)) }

// humanizeETA renders a rough remaining-time estimate from a count of hours.
func humanizeETA(hours float64) string {
	switch {
	case hours <= 0:
		return "—"
	case hours < 1:
		return "<1h left"
	case hours < 48:
		return fmt.Sprintf("~%dh left", int(hours+0.5))
	default:
		return fmt.Sprintf("~%.1f days left", hours/24)
	}
}

func humanizeSince(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// humanizeSeconds renders a non-negative duration in seconds as a compact
// "1h1m2s", "3m4s", or "5s" string for the per-run processing-time column.
//
// The input is rounded to whole seconds (deliberate — sub-second precision is
// noise for a processing-time column), then decomposed from the total so no
// component is silently dropped: e.g. 3661.5s → "1h1m2s" (1h, 1m, 2s after
// rounding), not "1h1m". Zero-valued leading components are omitted; for the
// h/m branches the trailing seconds are kept even when zero ("1h0m0s") so the
// breakdown is unambiguous.
func humanizeSeconds(secs float64) string {
	total := int64(secs + 0.5) // round to whole seconds
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// phaseClass maps a coordinator phase word to its CSS badge modifier. An unknown
// phase falls back to the idle tint so a future phase value never renders
// unstyled.
func phaseClass(phase string) string {
	switch phase {
	case db.PhaseTranscribe:
		return "phase-transcribe"
	case db.PhaseAnalyze:
		return "phase-analyze"
	default:
		return "phase-idle"
	}
}

// phaseIcon returns the small glyph paired with a coordinator phase word so the
// topbar pill is legible without relying on the border tint alone (1.4.1). It is
// HTML (an entity) so the caller interpolates it unescaped; the values are fixed
// literals, not user input.
func phaseIcon(phase string) template.HTML {
	switch phase {
	case db.PhaseTranscribe:
		return template.HTML("&#9658;") // ► running/transcribe
	case db.PhaseAnalyze:
		return template.HTML("&#9673;") // ◉ analyze
	default:
		return template.HTML("&#9679;") // ● idle
	}
}

// phaseReason is the one-line plain-language reason string for the phase pill
// tooltip. Paused is surfaced separately (its own neutral badge), so this only
// describes the coordinator phase itself.
func phaseReason(phase string, paused bool) string {
	if paused {
		return "Paused by intent — the runner is not claiming new work (a normal resting state)."
	}
	switch phase {
	case db.PhaseTranscribe:
		return "Transcribing — the runner is claiming and processing audio."
	case db.PhaseAnalyze:
		return "Analyzing — the eval judge is reviewing transcripts."
	default:
		return "Idle — nothing queued for the runner right now."
	}
}

// runBudgetText renders the current bounded-run state for the Pipeline page: the
// remaining-claim count when a run_limit is set, or "unlimited" when nil. A
// run_limit of 0 means a bounded run has drained (the runner declines further
// claims until re-armed), so it reads "exhausted" rather than "0 jobs".
func runBudgetText(limit *int) string {
	if limit == nil {
		return "unlimited"
	}
	if *limit <= 0 {
		return "exhausted (0 left)"
	}
	if *limit == 1 {
		return "1 job left"
	}
	return commafy(*limit) + " jobs left"
}

// validStatus is the allow-list for the library status filter; anything else is
// treated as "no filter".
func validStatus(s string) string {
	switch s {
	case "pending", "claimed", "done", "failed":
		return s
	default:
		return ""
	}
}

// validSort is the allow-list for the library sort control; anything else is
// treated as the default "recent" (the SQL transcribed-first ordering).
func validSort(s string) string {
	switch s {
	case "title", "progress", "findings":
		return s
	default:
		return "recent"
	}
}

// validPageSize maps the ?per= query param to one of libraryPageSizes, falling
// back to the default (libraryPageSize) for empty/malformed/out-of-set values.
// Pagination is a UI control, not an API, so a bad value degrades to the default
// rather than erroring.
func validPageSize(s string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		for _, allowed := range libraryPageSizes {
			if n == allowed {
				return n
			}
		}
	}
	return libraryPageSize
}

// isTruthy parses a checkbox/flag query param ("1"/"true"/"on"/"yes") to a bool.
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// libraryFetchCap bounds the all-in-Go library sort/filter (D1): we fetch the
// full filtered book set so the sort + has-findings filter operate over the
// whole library (not just one SQL page), then paginate in Go. A library beyond
// this bound is logged as a warning and the excess rows are NOT silently
// dropped from correctness — they're simply not fetched in one pass; the warning
// tells an operator the cap needs raising. 5000 books is far above any real
// homelab library.
const libraryFetchCap = 5000

// isHTMX guards mutating endpoints against drive-by/CSRF posts (htmx sets the
// HX-Request header, which cross-origin forms cannot without a CORS preflight).
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// writeActionError surfaces a failed mutation: htmx ignores the body of a
// non-2xx response, so return 200 + HX-Retarget to swap a dismissible banner
// into #action-error.
func writeActionError(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#action-error")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<div class="action-err">&#9888;&#xFE0F;&nbsp;%s</div>`, template.HTMLEscapeString(msg))
}

// ─── Static + page handlers ──────────────────────────────────────────────────

func (s *MCPServer) handleHTMX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(htmxJS)
}

// handlePipelinePage serves the Pipeline ops page shell (GET /pipeline): the
// status fragment plus the folded-in Failed view.
func (s *MCPServer) handlePipelinePage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, pipelinePage, pageShell{Title: "pipeline", Nav: "pipeline"})
}

func (s *MCPServer) handleLibraryPage(w http.ResponseWriter, r *http.Request) {
	// The Library is now the home page (GET /), which is a catch-all route — so
	// 404 any unmatched path that falls through to "/". A real /library request
	// has Path=="/library" and passes this guard unchanged.
	if r.URL.Path != "/" && r.URL.Path != "/library" {
		http.NotFound(w, r)
		return
	}
	// Thread the status/search filter (e.g. from an Overview stat-card link) into
	// the initial fragment load, so /library?status=pending actually shows the
	// pending books instead of the whole library.
	vals := url.Values{}
	if status := validStatus(r.URL.Query().Get("status")); status != "" {
		vals.Set("status", status)
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		vals.Set("q", q)
	}
	if sort := validSort(r.URL.Query().Get("sort")); sort != "recent" {
		vals.Set("sort", sort)
	}
	if isTruthy(r.URL.Query().Get("findings")) {
		vals.Set("findings", "1")
	}
	if ps := validPageSize(r.URL.Query().Get("per")); ps != libraryPageSize {
		vals.Set("per", strconv.Itoa(ps))
	}
	dataQuery := ""
	if enc := vals.Encode(); enc != "" {
		dataQuery = "?" + enc
	}
	s.renderPage(w, r, libraryPage, pageShell{Title: "library", Nav: "library", DataQuery: dataQuery})
}

func (s *MCPServer) handleBookPage(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "missing dir", http.StatusBadRequest)
		return
	}
	s.renderPage(w, r, bookPage, pageShell{Title: "book", Nav: "library", DirQuery: url.QueryEscape(dir)})
}

func (s *MCPServer) renderPage(w http.ResponseWriter, r *http.Request, tmpl *template.Template, data pageShell) {
	// Populate the topbar phase + paused badge (read-only, every page — D2). The
	// dashboard never writes phase; the `earmark batch` coordinator owns it
	// (CONTRACT §1.4). A read error suppresses the badge (PhaseKnown stays false)
	// rather than showing a misleading default, and never blocks the page. The
	// pause flag comes from GetControl (same source the control API re-reads).
	ctx := r.Context()
	if phase, err := s.db.GetPipelinePhase(ctx); err != nil {
		s.logger.Error("GetPipelinePhase (shell) error", "error", err)
	} else {
		data.Phase = phase
		data.PhaseClass = phaseClass(phase)
		data.PhaseIcon = phaseIcon(phase)
		data.PhaseKnown = true
	}
	if paused, _, err := s.db.GetControl(ctx); err != nil {
		s.logger.Error("GetControl (shell) error", "error", err)
	} else {
		data.Paused = paused
	}
	data.PhaseReason = phaseReason(data.Phase, data.Paused)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := tmpl.Execute(w, data); err != nil {
		s.logger.Error("page render error", "error", err)
	}
}

// ─── Status fragment handler ─────────────────────────────────────────────────

func (s *MCPServer) handleStatusData(w http.ResponseWriter, r *http.Request) {
	s.renderStatusFragment(w, r)
}

func (s *MCPServer) renderStatusFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.db.GetServiceStatus(ctx)
	if err != nil {
		s.logger.Error("GetServiceStatus error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	jobs, err := s.db.GetRecentJobs(ctx, 15)
	if err != nil {
		s.logger.Error("GetRecentJobs error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Empirical ETA (CONTRACT §4). Best-effort: a predict-inputs read error
	// degrades to no estimate (newStatusData then falls back / shows "—") rather
	// than failing the fragment — the ETA is informational.
	var est *predict.Estimate
	if in, perr := s.db.GetPredictInputs(ctx); perr != nil {
		s.logger.Warn("GetPredictInputs error; ETA degraded", "error", perr)
	} else {
		e := predict.Compute(in)
		est = &e
	}

	data := newStatusData(stats, jobs, time.Now(), s.runnerStaleAfter, s.embedURL, est)

	// Read-only coordinator phase for the phase badge. The dashboard never writes
	// phase (the `earmark batch` coordinator owns it, CONTRACT §1.4); a read error
	// degrades to "idle" + a log line rather than failing the fragment.
	phase, perr := s.db.GetPipelinePhase(ctx)
	if perr != nil {
		s.logger.Error("GetPipelinePhase error", "error", perr)
		phase = db.PhaseIdle
	}
	data.Phase = phase
	data.PhaseClass = phaseClass(phase)
	// Run-budget control state. RunLimit comes from the same GetServiceStatus read
	// (no extra query). ControlEnabled gates the live writes so the controls render
	// honestly disabled on a token-less deployment.
	data.RunLimit = stats.RunLimit
	data.RunBudgetText = runBudgetText(stats.RunLimit)
	data.ControlEnabled = s.controlToken != ""

	// Pipeline lifecycle (3-stage: Transcribe → Eval → Embed). Computed here so
	// the status fragment can show the honest "winding down" state and the GPU/VRAM
	// card group. Arbiter probe: prefer the "primary" role server; fall back to the
	// first configured server with a probe URL.
	{
		probes := s.probeServers(ctx)
		primaryArbiter := arbiterStatus{}
		gpuProbed := false
		for _, c := range s.asrServers {
			if st, ok := probes[c.Name]; ok {
				gpuProbed = true
				primaryArbiter = st
				if c.Role == "primary" {
					break
				}
			}
		}
		data.Lifecycle = computePipelineLifecycle(stats, phase, primaryArbiter, gpuProbed, s.evalInPipeline)
		data.PrimaryArbiter = primaryArbiter
		// Override the default SubText with the lifecycle-aware message so the
		// Pipeline page never says "IDLE" while the GPU is still committed.
		if data.Lifecycle.WindingDown() && data.StateClass == "state-idle" {
			data.SubText = "Winding down — GPU still working (eval / embed)"
		} else if data.Lifecycle.FullyDone && !data.Lifecycle.GPUCommitted {
			data.SubText = "Idle — safe to walk away"
		}
	}

	// Build the queue and activity feeds. The queue is built first (primary section);
	// the activity feed then excludes books already in the queue to avoid duplication.
	// Both are best-effort: errors degrade to empty rather than failing the fragment.
	data.QueueBooks, data.QueueTotalBooks = s.buildQueueFeed(ctx)
	data.ActivityBooks = s.buildActivityFeed(ctx, data.QueueBooks)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := statusFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("status fragment render error", "error", err)
	}
}

// buildQueueFeed fetches books with remaining work (pending or claimed tracks)
// ordered active-first (claimed > 0 first, then most-claimed, most-pending,
// longest-waiting). Returns the book rows and the total DB count of queued books
// (which may exceed queueBookFeedLimit, surfacing truncation in the UI).
// It is best-effort: errors are logged and degrade the feed to empty.
func (s *MCPServer) buildQueueFeed(ctx context.Context) ([]activityBook, int) {
	summaries, total, err := s.db.GetBookSummaries(ctx, db.BookFilter{
		Status: "queued", Sort: "queue", Limit: queueBookFeedLimit,
	})
	if err != nil {
		s.logger.Error("queue feed: GetBookSummaries error", "error", err)
		return nil, 0
	}
	out := s.buildActivityBooks(ctx, summaries, "queue feed")
	return out, total
}

// buildActivityFeed fetches the most-recently-updated books and their tracks for
// the pipeline page's book-grouped activity feed. Books already present in the
// queue feed (passed as queueBooks) are excluded so the two sections do not
// repeat the same books. It is best-effort: errors are logged and degrade the
// feed to empty rather than failing the fragment.
func (s *MCPServer) buildActivityFeed(ctx context.Context, queueBooks []activityBook) []activityBook {
	// Build a set of dirs already shown in the queue so we can exclude them.
	queueDirs := make(map[string]struct{}, len(queueBooks))
	for _, b := range queueBooks {
		queueDirs[b.Dir] = struct{}{}
	}

	summaries, _, err := s.db.GetBookSummaries(ctx, db.BookFilter{
		Sort: "activity", Limit: activityBookFeedLimit,
	})
	if err != nil {
		s.logger.Error("activity feed: GetBookSummaries error", "error", err)
		return nil
	}

	// Exclude books already in the queue section.
	filtered := summaries[:0:0]
	for _, b := range summaries {
		if _, inQueue := queueDirs[b.Dir]; !inQueue {
			filtered = append(filtered, b)
		}
	}
	return s.buildActivityBooks(ctx, filtered, "activity feed")
}

// buildActivityBooks converts a slice of BookSummary into activityBook rows,
// fetching per-book tracks and resolving metadata for each. label is used in
// log messages to distinguish the caller (queue vs activity feed). It is
// best-effort: per-book errors are logged but do not abort the whole slice.
func (s *MCPServer) buildActivityBooks(ctx context.Context, summaries []db.BookSummary, label string) []activityBook {
	out := make([]activityBook, 0, len(summaries))
	for _, b := range summaries {
		bookMeta, err := s.meta.Lookup(ctx, b.SamplePath, b.SamplePath)
		if err != nil {
			// Non-fatal: the template falls back to rendering b.Dir when Title is
			// empty. Log at debug so metadata issues are diagnosable without
			// breaking the feed.
			s.logger.Debug(label+": meta lookup error", "dir", b.Dir, "error", err)
		}
		pct := 0
		if b.Total > 0 {
			pct = b.Done * 100 / b.Total
		}
		ab := activityBook{
			Dir: b.Dir, Title: bookMeta.Title, Author: bookMeta.Author,
			Total: b.Total, Pending: b.Pending, Claimed: b.Claimed, Done: b.Done, Failed: b.Failed,
			DonePct:     pct,
			LastUpdated: b.LastUpdated,
		}

		// Fetch per-book tracks for the inline expansion body.
		tracks, terr := s.db.GetBookTracks(ctx, b.Dir)
		if terr != nil {
			s.logger.Error(label+": GetBookTracks error", "dir", b.Dir, "error", terr)
		}
		ab.Tracks = make([]activityTrack, 0, len(tracks))
		for _, t := range tracks {
			ab.Tracks = append(ab.Tracks, activityTrack{
				ID:                t.ID,
				FilePath:          t.FilePath,
				ShortName:         path.Base(t.FilePath),
				Status:            t.Status,
				DurationSeconds:   t.DurationSeconds,
				ProcessingSeconds: t.ProcessingSeconds,
				EmbedChunkCount:   t.EmbedChunkCount,
				UpdatedAt:         t.UpdatedAt,
			})
		}
		out = append(out, ab)
	}
	return out
}

// ─── Library data handler ────────────────────────────────────────────────────

func (s *MCPServer) handleLibraryData(w http.ResponseWriter, r *http.Request) {
	status := validStatus(r.URL.Query().Get("status"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	sort := validSort(r.URL.Query().Get("sort"))
	hasFindings := isTruthy(r.URL.Query().Get("findings"))
	// offset is the htmx "load more" cursor. A missing or malformed value
	// intentionally falls back to the first page (0) rather than erroring —
	// this is a UI pagination control, not an API, so a bad cursor should
	// degrade gracefully instead of surfacing a 400.
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}
	// pageSize is the reader-selectable list length (?per=), validated against
	// libraryPageSizes with the default as the fallback.
	pageSize := validPageSize(r.URL.Query().Get("per"))

	// All-in-Go sort/filter (D1): fetch the FULL status/query-filtered book set in
	// one pass (up to the defensive cap), then merge findings, apply the
	// has-findings filter, sort, and paginate in Go. This keeps the sort + ⚑
	// filter operating over the whole library rather than just one SQL page. The
	// SQL already returns the "recent" (transcribed-first) order, so the default
	// sort is a no-op re-sort; the other modes re-order the fetched slice.
	books, total, err := s.db.GetBookSummaries(r.Context(), db.BookFilter{
		Status: status, Query: query, Limit: libraryFetchCap, Offset: 0,
	})
	if err != nil {
		s.logger.Error("GetBookSummaries error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if total > libraryFetchCap {
		// Never silently truncate: warn so an operator knows the cap needs raising.
		// The visible list is still correct for the fetched set; only books beyond
		// the cap are missing from this in-Go pass.
		s.logger.Warn("library exceeds the in-Go sort/filter fetch cap — raise libraryFetchCap",
			"total", total, "cap", libraryFetchCap)
	}

	// One whole-library findings-count aggregate, keyed by book dir, so the ⚑
	// column is a map lookup rather than an N+1 per row, and the has-findings
	// filter has the counts it needs. A findings error must not break the library
	// list, so it's logged and the column just renders 0 (em dash) everywhere
	// (with the has-findings filter then matching nothing — an honest empty list,
	// not a crash).
	findingsByBook, err := s.db.GetFindingsCountByBook(r.Context())
	if err != nil {
		s.logger.Error("GetFindingsCountByBook error", "error", err)
		findingsByBook = nil
	}

	rows := make([]bookRow, 0, len(books))
	for _, b := range books {
		fc := findingsByBook[b.Dir]
		if hasFindings && fc == 0 {
			continue // ⚑ filter: drop books with no recorded findings
		}
		bookMeta, _ := s.meta.Lookup(r.Context(), b.SamplePath, b.SamplePath)
		author, title := bookMeta.Author, bookMeta.Title
		pct := 0
		if b.Total > 0 {
			pct = b.Done * 100 / b.Total
		}
		rows = append(rows, bookRow{
			Dir: b.Dir, Title: title, Author: author, DonePct: pct,
			Total: b.Total, Pending: b.Pending, Claimed: b.Claimed, Done: b.Done, Failed: b.Failed,
			LastUpdated:     b.LastUpdated,
			DurationSeconds: b.DurationSeconds, WordCount: b.WordCount, EmbedChunkCount: b.EmbedChunkCount,
			FindingCount: fc,
		})
	}

	sortBookRows(rows, sort)

	// Paginate the filtered+sorted slice in Go.
	filteredTotal := len(rows)
	if offset > filteredTotal {
		offset = filteredTotal
	}
	end := offset + pageSize
	if end > filteredTotal {
		end = filteredTotal
	}
	pageRows := rows[offset:end]

	totalPages := (filteredTotal + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	data := libraryData{
		Books: pageRows, Status: status, Query: query, QueryEscaped: url.QueryEscape(query),
		Sort: sort, HasFindings: hasFindings, MoreCols: r.URL.Query().Get("cols") == "more",
		FilterParams: libraryFilterParams(sort, hasFindings, pageSize),
		Page:         offset/pageSize + 1, TotalPages: totalPages, TotalBooks: filteredTotal,
		PageSize: pageSize, PageSizes: libraryPageSizes,
		HasPrev: offset > 0, HasNext: end < filteredTotal,
		PrevOffset: max(0, offset-pageSize), NextOffset: offset + pageSize,
	}

	// Home status-overview band: only on the unfiltered first page (the "is it
	// okay?" home view). On any filter/search/paged view it's suppressed so the
	// whole-library rollup never contradicts the narrowed list. `rows` here is the
	// full library (status=="", query==""), so failed-book counting is exact.
	if status == "" && query == "" && !hasFindings && offset == 0 {
		data.Overview = s.buildOverview(r.Context(), rows, findingsByBook)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := libraryFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("library render error", "error", err)
	}
}

// buildOverview assembles the home status-overview band from the full library row
// set (already fetched for the unfiltered view), the per-book findings counts, and
// a fresh queue/phase read. It is best-effort: a queue or findings read error
// degrades the affected cards to zero rather than failing the whole library page.
// "Paused" is treated as a NORMAL resting state — the phase line and Queue card
// read neutral (never red/attention) when paused-by-intent; only real failures
// raise the attention flag.
func (s *MCPServer) buildOverview(ctx context.Context, rows []bookRow, findingsByBook map[string]int) *overviewData {
	o := &overviewData{TotalBooks: len(rows)}
	for _, b := range rows {
		if b.Total > 0 && b.Done == b.Total {
			o.FullyTranscribed++
		}
		if b.Failed > 0 {
			o.FailedBooks++
		}
	}
	if o.TotalBooks > 0 {
		o.TranscribedPct = o.FullyTranscribed * 100 / o.TotalBooks
	}

	stats, err := s.db.GetServiceStatus(ctx)
	if err != nil {
		s.logger.Error("GetServiceStatus (overview) error", "error", err)
		stats = &db.QueueStats{}
	}
	o.FailedJobs = stats.Failed
	o.Queued = stats.Pending + stats.Claimed

	phase, perr := s.db.GetPipelinePhase(ctx)
	if perr != nil {
		s.logger.Error("GetPipelinePhase (overview) error", "error", perr)
		phase = db.PhaseIdle
	}

	if summary, ferr := s.db.GetFindingsSummary(ctx); ferr != nil {
		s.logger.Error("GetFindingsSummary (overview) error", "error", ferr)
	} else if summary != nil {
		o.Findings = summary.TotalFindings
		o.HighFindings = summary.HighConfidence
	}

	o.fillStatusLine(stats.Paused, stats.Claimed > 0, phase)
	return o
}

// fillStatusLine sets the overview's queue-card accent + sub-label, the
// plain-language status line, and its glyph. Order of precedence in the prose:
// failures first (the only "needs attention" case), then paused (neutral), then
// active transcription, then idle.
func (o *overviewData) fillStatusLine(paused, transcribing bool, phase string) {
	// Queue card accent: blue when work is queued, green when drained. Never red
	// for paused — paused is intentional, not an alert.
	if o.Queued > 0 {
		o.QueueAccent = "blue"
	} else {
		o.QueueAccent = "green"
	}

	switch {
	case paused:
		o.PhaseLabel = "paused"
	case transcribing:
		o.PhaseLabel = "transcribing"
	default:
		o.PhaseLabel = phase
	}

	// Plain-language one-liner. Failures are the lead when present (attention);
	// otherwise the line is neutral.
	transcribed := "All books transcribed"
	if o.FullyTranscribed < o.TotalBooks {
		transcribed = commafy(o.FullyTranscribed) + " of " + commafy(o.TotalBooks) + " books transcribed"
	}
	switch {
	case o.FailedJobs > 0:
		o.Attention = true
		o.StatusGlyph = template.HTML("&#9888;&#xFE0F;") // ⚠️
		o.PlainStatus = commafy(o.FailedJobs) + " track" + plural(o.FailedJobs) + " failed and need attention — " + transcribed + "."
	case paused:
		o.StatusGlyph = template.HTML("&#9208;") // ⏸ neutral
		o.PlainStatus = "Paused (intentional) — runner is not claiming new work. " + transcribed + ", " + commafy(o.FailedJobs) + " failed."
	case o.Queued > 0:
		o.StatusGlyph = template.HTML("&#9658;") // ►
		o.PlainStatus = commafy(o.Queued) + " track" + plural(o.Queued) + " queued — " + transcribed + ", 0 failed."
	default:
		o.StatusGlyph = template.HTML("&#9679;") // ●
		o.PlainStatus = "Idle — nothing queued. " + transcribed + ", 0 failed."
	}
}

// plural returns "s" for any count != 1 (small pluralization helper for the
// overview status prose).
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sortBookRows orders the library rows for the chosen sort mode, in place:
//   - recent   — no-op: the SQL already returned transcribed-first/most-recent.
//   - title    — by resolved "Author — Title" label, case-insensitively.
//   - progress — by done fraction DESC (most-complete first), ties by title.
//   - findings — by recorded finding count DESC (most-flagged first), ties by title.
//
// A stable sort preserves the SQL "recent" order within ties.
func sortBookRows(rows []bookRow, mode string) {
	switch mode {
	case "title":
		sortStable(rows, func(a, b bookRow) bool { return titleKey(a) < titleKey(b) })
	case "progress":
		sortStable(rows, func(a, b bookRow) bool {
			ra, rb := doneFrac(a), doneFrac(b)
			if ra != rb {
				return ra > rb
			}
			return titleKey(a) < titleKey(b)
		})
	case "findings":
		sortStable(rows, func(a, b bookRow) bool {
			if a.FindingCount != b.FindingCount {
				return a.FindingCount > b.FindingCount
			}
			return titleKey(a) < titleKey(b)
		})
	default: // recent — keep the SQL order
	}
}

// sortStable stable-sorts rows by a less predicate (thin wrapper over
// sort.SliceStable so the callers read as a/b comparisons).
func sortStable(rows []bookRow, less func(a, b bookRow) bool) {
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
}

// titleKey is the case-insensitive sort key for the title sort: "author title"
// from the metadata-resolved labels, falling back to the dir when both are empty.
func titleKey(b bookRow) string {
	k := strings.ToLower(strings.TrimSpace(b.Author + " " + b.Title))
	if k == "" {
		return strings.ToLower(b.Dir)
	}
	return k
}

// doneFrac is the done/total fraction (0 when total==0), for the progress sort.
func doneFrac(b bookRow) float64 {
	if b.Total <= 0 {
		return 0
	}
	return float64(b.Done) / float64(b.Total)
}

// libraryFilterParams builds the "&sort=…&findings=1" suffix appended to chip and
// pager links so the active sort + findings filter persist across navigation.
// Returns template.URL since it is interpolated into an href that already has a
// leading "?…"; both components are fixed allow-listed tokens (no user text), so
// this is not an injection vector.
func libraryFilterParams(sort string, hasFindings bool, pageSize int) template.URL {
	var b strings.Builder
	if sort != "recent" {
		b.WriteString("&sort=")
		b.WriteString(sort)
	}
	if hasFindings {
		b.WriteString("&findings=1")
	}
	if pageSize != libraryPageSize {
		b.WriteString("&per=")
		b.WriteString(strconv.Itoa(pageSize))
	}
	// #nosec G203 -- sort is constrained to a fixed allow-list by validSort
	// (recent/title/progress/findings), findings is a fixed flag, and pageSize is
	// validated against libraryPageSizes by validPageSize; the string contains no
	// user-controlled input, so the unescaped template.URL is safe.
	return template.URL(b.String())
}

// ─── Book data handler ───────────────────────────────────────────────────────

func (s *MCPServer) handleBookData(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "missing dir", http.StatusBadRequest)
		return
	}
	s.renderBookFragment(w, r, dir)
}

func (s *MCPServer) renderBookFragment(w http.ResponseWriter, r *http.Request, dir string) {
	s.renderBookFragmentWithNotice(w, r, dir, "")
}

// renderBookFragmentWithNotice renders the book fragment with an optional
// transient eval notice (e.g. "evaluating N chunks…" after the run-eval button).
func (s *MCPServer) renderBookFragmentWithNotice(w http.ResponseWriter, r *http.Request, dir, evalNotice string) {
	tracks, err := s.db.GetBookTracks(r.Context(), dir)
	if err != nil {
		s.logger.Error("GetBookTracks error", "dir", dir, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	d := bookData{Dir: dir, DirQuery: url.QueryEscape(dir), Tracks: tracks, Total: len(tracks),
		EvalConfigured: s.eval.configured, EvalNotice: evalNotice}
	for _, t := range tracks {
		switch t.Status {
		case "pending":
			d.Pending++
		case "claimed":
			d.Claimed++
		case "done":
			d.Done++
			// Embed progress is measured over done tracks only: a track is embedded
			// once its run_metrics.embed_chunk_count is set (non-nil). EmbedTotal ==
			// done-track count so the bar reads "of the transcribed tracks, how many
			// are embedded" — never a fake fraction against pending/failed tracks.
			d.EmbedTotal++
			if t.EmbedChunkCount != nil {
				d.EmbedDone++
			}
		case "failed":
			d.Failed++
		}
	}
	// filePath must be a track file so that filepath.Dir(filePath) == dir.
	// When no tracks exist yet, synthesise one so the provider sees the correct
	// directory depth; the fictitious filename is never stored.
	filePath := dir + "/__"
	if len(tracks) > 0 {
		filePath = tracks[0].FilePath
	}
	bookMeta, _ := s.meta.Lookup(r.Context(), filePath, filePath)
	d.Author, d.Title = bookMeta.Author, bookMeta.Title

	// Per-book findings: the individual worklist rows for this book, plus the
	// matching ByBook roll-up entry (reused from GetFindingsSummary — no second
	// aggregate query). Both are read-only; a findings error must not break the
	// book page, so they're logged and the section just renders its empty state.
	if findings, ferr := s.db.ListFindings(r.Context(), dir, perBookFindingsLimit); ferr != nil {
		s.logger.Error("ListFindings error", "dir", dir, "error", ferr)
	} else {
		d.Findings = findings
		d.FindingsByTrack = make(map[string]int, len(findings))
		for _, f := range findings {
			d.FindingsByTrack[f.FilePath]++
		}
	}
	if summary, serr := s.db.GetFindingsSummary(r.Context()); serr != nil {
		s.logger.Error("GetFindingsSummary error (book)", "dir", dir, "error", serr)
	} else {
		for i := range summary.ByBook {
			if summary.ByBook[i].BookDir == dir {
				d.FindingsSummary = &summary.ByBook[i]
				break
			}
		}
	}
	// Judge element count: prefer the roll-up Count (whole-book total) over
	// len(Findings) (capped at perBookFindingsLimit), falling back to the worklist
	// length when no roll-up entry exists. This is a status/count, not a bar — see
	// the bookData pipeline-panel comment for why a judge % is not derivable.
	if d.FindingsSummary != nil {
		d.FindingsCount = d.FindingsSummary.Count
	} else {
		d.FindingsCount = len(d.Findings)
	}
	d.ControlEnabled = s.controlToken != ""

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := bookFragmentTmpl.Execute(w, d); err != nil {
		s.logger.Error("book fragment render error", "error", err)
	}
}

// perBookFindingsLimit caps the per-book findings worklist on the Book page.
const perBookFindingsLimit = 200

// ─── Per-book search handler ─────────────────────────────────────────────────

// bookSearchLimit caps the per-book search result count.
const bookSearchLimit = 50

// handleBookSearch runs a trigram text search scoped to one book and renders the
// matching chunk rows (POST /search/book?dir=…, q in the form or query). It's a
// read, but accepts POST (htmx form submit) as well as GET; both are fine since
// it only queries. The dir comes from the query string; q from the form (POST)
// or query (GET).
func (s *MCPServer) handleBookSearch(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "missing dir", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// r.Form merges query + POST body params, so this works for GET and POST.
	query := strings.TrimSpace(r.Form.Get("q"))

	var results []db.SearchResultWithMetadata
	if query != "" {
		var err error
		results, err = s.db.TextSearchInBook(r.Context(), dir, query, bookSearchLimit)
		if err != nil {
			s.logger.Error("TextSearchInBook error", "dir", dir, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := bookSearchFragmentTmpl.Execute(w, bookSearchData{Query: query, Results: results}); err != nil {
		s.logger.Error("book search render error", "error", err)
	}
}

// ─── Track detail handlers ───────────────────────────────────────────────────

func (s *MCPServer) handleTrackPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// The id goes into an hx-get attribute, which html/template does NOT
	// URL-filter, so it is pre-escaped. The "back to book" link is derived in the
	// fragment from the track's own file_path, so no dir param is threaded here.
	// Thread a valid &t=<startSec> deep-jump param into the fragment load so a
	// full-page navigation to /track?id=…&t=… still lands on the right segment; an
	// absent/invalid t is dropped.
	tQuery := ""
	if raw := strings.TrimSpace(r.URL.Query().Get("t")); raw != "" {
		if t, err := strconv.ParseFloat(raw, 64); err == nil && t >= 0 {
			tQuery = "&t=" + url.QueryEscape(raw)
		}
	}
	s.renderPage(w, r, trackPage, pageShell{
		Title: "track", Nav: "library", IDQuery: url.QueryEscape(id), TQuery: tQuery,
	})
}

func (s *MCPServer) handleTrackData(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	detail, err := s.db.GetTrackDetail(r.Context(), id)
	if err != nil {
		// A bad/unknown id is a 404, not a 500 — distinguishes "no such track"
		// from a real backend failure.
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "no such track", http.StatusNotFound)
			return
		}
		s.logger.Error("GetTrackDetail error", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	bookDir := path.Dir(detail.FilePath)
	d := trackData{Detail: detail, BackDir: url.QueryEscape(bookDir), IDQuery: url.QueryEscape(id)}
	trackMeta, _ := s.meta.Lookup(r.Context(), detail.FilePath, detail.FilePath)
	d.Author, d.Title = trackMeta.Author, trackMeta.Title
	if detail.HasTranscript {
		dur := detail.DurationSeconds
		d.DurationPtr = &dur
	}

	// Transcript reader pagination (P7): render only the first page of segments;
	// the rest load on demand via the htmx "load more" button.
	d.TotalSegments = len(detail.Segments)

	// Deep-jump (D3): a finding "Where" link carries &t=<startSec>. Resolve it to
	// the target segment's slice index and preload pages [0 .. target page] so the
	// #seg-N anchor is already in the initial DOM (no N+1, no extra round-trip):
	// the reader scrolls to it after htmx settles. Without t, render just the
	// first page. A malformed t is ignored (degrade to first page) — this is a UI
	// convenience link, not an API.
	preloadEnd := segmentPageSize
	if raw := strings.TrimSpace(r.URL.Query().Get("t")); raw != "" && len(detail.Segments) > 0 {
		if t, perr := strconv.ParseFloat(raw, 64); perr == nil && t >= 0 {
			idx := segmentIndexForTime(detail.Segments, t)
			d.TargetSeg = &idx
			targetPage := idx / segmentPageSize
			preloadEnd = (targetPage + 1) * segmentPageSize
		}
	}
	if preloadEnd > len(detail.Segments) {
		preloadEnd = len(detail.Segments)
	}
	d.PageSegments = detail.Segments[:preloadEnd]
	d.HasMore = preloadEnd < len(detail.Segments)
	d.NextOffset = preloadEnd

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := trackFragmentTmpl.Execute(w, d); err != nil {
		s.logger.Error("track fragment render error", "error", err)
	}
}

// handleTrackSegments serves one page of the transcript reader (the htmx "load
// more" target): segments [offset, offset+segmentPageSize) for the given job.
func (s *MCPServer) handleTrackSegments(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// offset is the htmx "load more" cursor. A missing or malformed value
	// intentionally falls back to the first page (0) rather than erroring —
	// this is a UI pagination control, not an API, so a bad cursor should
	// degrade gracefully instead of surfacing a 400.
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}

	detail, err := s.db.GetTrackDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "no such track", http.StatusNotFound)
			return
		}
		s.logger.Error("GetTrackDetail (segments) error", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page, hasMore, next := paginateSegments(detail.Segments, offset)
	// next == clampedStart + len(page); recover the clamped start so each row's
	// #seg-N anchor uses its absolute slice index (consistent with the first page).
	data := segmentsData{
		Segments: page, HasMore: hasMore, NextOffset: next,
		StartOffset: next - len(page), IDQuery: url.QueryEscape(id),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := segmentsFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("segments fragment render error", "error", err)
	}
}

// segmentIndexForTime returns the slice index of the first segment whose End is
// at or after t (the segment that contains, or first follows, time t). It is the
// deep-jump target: a finding's start_sec lands on the segment spanning it.
// Returns 0 for t<=0 or an empty slice, and the last index when t is past the
// end (clamp), so the result is always a valid index into a non-empty slice.
func segmentIndexForTime(segs []db.Segment, t float64) int {
	if len(segs) == 0 {
		return 0
	}
	if t <= 0 {
		return 0
	}
	for i, s := range segs {
		if s.End >= t {
			return i
		}
	}
	return len(segs) - 1
}

// paginateSegments returns the page of segments starting at offset (size
// segmentPageSize), whether more remain after it, and the next offset. A
// negative or out-of-range offset is clamped to the valid range.
func paginateSegments(segs []db.Segment, offset int) (page []db.Segment, hasMore bool, next int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(segs) {
		offset = len(segs)
	}
	end := offset + segmentPageSize
	if end > len(segs) {
		end = len(segs)
	}
	return segs[offset:end], end < len(segs), end
}

// ─── Failed jobs view ────────────────────────────────────────────────────────

func (s *MCPServer) handleFailedData(w http.ResponseWriter, r *http.Request) {
	s.renderFailedFragment(w, r)
}

func (s *MCPServer) renderFailedFragment(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.db.GetFailedJobs(r.Context())
	if err != nil {
		s.logger.Error("GetFailedJobs error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := failedFragmentTmpl.Execute(w, failedData{Jobs: jobs}); err != nil {
		s.logger.Error("failed fragment render error", "error", err)
	}
}

// ─── Mutating action handlers ────────────────────────────────────────────────

// handleRequeueJob re-transcribes a single job (POST /actions/requeue?id=…).
// When a "book" dir param is present (the book-detail page), it re-renders the
// book fragment; otherwise it re-renders the Overview status fragment.
func (s *MCPServer) handleRequeueJob(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if _, err := s.db.RequeueByID(r.Context(), id); err != nil {
		s.logger.Error("requeue job error", "id", id, "error", err)
		writeActionError(w, "requeue failed — see server logs")
		return
	}
	s.logger.Info("requeued job via dashboard", "id", id)
	if r.URL.Query().Get("failed") != "" {
		s.renderFailedFragment(w, r)
		return
	}
	if book := r.URL.Query().Get("book"); book != "" {
		s.renderBookFragment(w, r, book)
		return
	}
	s.renderStatusFragment(w, r)
}

// handleRetryFailed re-transcribes every failed job (POST /actions/retry-failed).
func (s *MCPServer) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	paths, err := s.db.RequeueFailed(r.Context())
	if err != nil {
		s.logger.Error("retry failed error", "error", err)
		writeActionError(w, "retry-all-failed failed — see server logs")
		return
	}
	s.logger.Info("retried failed jobs via dashboard", "count", len(paths))
	s.renderStatusFragment(w, r)
}

// handleBookRequeue re-transcribes every track in one book (POST
// /actions/book-requeue?dir=…) and re-renders the book fragment.
func (s *MCPServer) handleBookRequeue(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "missing dir", http.StatusBadRequest)
		return
	}
	paths, err := s.db.RequeueByDir(r.Context(), dir)
	if err != nil {
		s.logger.Error("book requeue error", "dir", dir, "error", err)
		writeActionError(w, "requeue book failed — see server logs")
		return
	}
	s.logger.Info("requeued book via dashboard", "dir", dir, "count", len(paths))
	s.renderBookFragment(w, r, dir)
}

func (s *MCPServer) handlePause(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.db.SetPaused(r.Context(), true, "dashboard"); err != nil {
		s.logger.Error("pause error", "error", err)
		writeActionError(w, "pause failed — see server logs")
		return
	}
	s.logger.Info("pipeline paused via dashboard")
	s.renderStatusFragment(w, r)
}

func (s *MCPServer) handleResume(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Resume = clear any bounded run (run_limit→NULL), then unpause. Limit first so
	// the runner stays gated by paused until the final write. A dashboard resume
	// means "run normally", matching the control API's resume semantics.
	if err := s.db.SetRunLimit(r.Context(), nil, "dashboard"); err != nil {
		s.logger.Error("resume (clear limit) error", "error", err)
		writeActionError(w, "resume failed — see server logs")
		return
	}
	if err := s.db.SetPaused(r.Context(), false, "dashboard"); err != nil {
		s.logger.Error("resume error", "error", err)
		writeActionError(w, "resume failed — see server logs")
		return
	}
	s.logger.Info("pipeline resumed via dashboard")
	s.renderStatusFragment(w, r)
}

// handleRunBudget starts a bounded run of N≥1 claims from the Pipeline page
// (POST /actions/run?n=N): set run_limit=N then unpause, so the runner processes
// exactly N jobs and then declines further claims. Mirrors the JSON control
// API's handleAPIRun (api.go) — limit before unpause, so the runner stays gated
// by paused until the final write and can never claim beyond N. It is
// htmx-guarded and fail-closed on an unset CONTROL_API_TOKEN (like the eval
// actions), since arming a run is a control mutation.
func (s *MCPServer) handleRunBudget(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.controlToken == "" {
		writeActionError(w, "run controls are disabled: CONTROL_API_TOKEN is not configured")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeActionError(w, "bad form")
		return
	}
	// r.Form merges query + POST body, so the htmx form (which posts n in the
	// body) and a ?n= query both work.
	n, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("n")))
	if err != nil || n < 1 {
		writeActionError(w, "run budget must be an integer ≥ 1")
		return
	}
	// Limit before unpause (mirror api.go:312-324): the runner stays gated by
	// paused until the final write, so it can never claim beyond N.
	if err := s.db.SetRunLimit(r.Context(), &n, "dashboard"); err != nil {
		s.logger.Error("run budget (set limit) error", "error", err)
		writeActionError(w, "set run budget failed — see server logs")
		return
	}
	if err := s.db.SetPaused(r.Context(), false, "dashboard"); err != nil {
		s.logger.Error("run budget (unpause) error", "error", err)
		writeActionError(w, "set run budget failed — see server logs")
		return
	}
	s.logger.Info("bounded run armed via dashboard", "limit", n)
	s.renderStatusFragment(w, r)
}

// handleClearBudget clears a bounded run (run_limit→NULL) without touching the
// pause flag (POST /actions/run-clear) — back to an unlimited run. htmx-guarded
// and fail-closed on an unset token like handleRunBudget.
func (s *MCPServer) handleClearBudget(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.controlToken == "" {
		writeActionError(w, "run controls are disabled: CONTROL_API_TOKEN is not configured")
		return
	}
	if err := s.db.SetRunLimit(r.Context(), nil, "dashboard"); err != nil {
		s.logger.Error("clear run budget error", "error", err)
		writeActionError(w, "clear run budget failed — see server logs")
		return
	}
	s.logger.Info("bounded run cleared via dashboard")
	s.renderStatusFragment(w, r)
}
