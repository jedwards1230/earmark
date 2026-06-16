package mcp

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/db"
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

// libraryPageSize is the number of books per page in the library list.
const libraryPageSize = 20

// segmentPageSize is the number of transcript segments rendered per page in the
// track-detail reader; beyond this an htmx "load more" button appends the next
// page (P7), so a multi-thousand-segment transcript doesn't render all at once.
const segmentPageSize = 30

// embedStallThreshold is the embed backlog above which the dashboard warns that
// embeddings are not draining. A few transcripts always sit briefly between
// transcription and the worker's 30s embed poll, so a small backlog is normal;
// a sustained large one means Ollama is down or the model isn't pulled.
const embedStallThreshold = 10

// tmplFuncs are shared across every dashboard template.
var tmplFuncs = template.FuncMap{
	"shortName":  func(fp string) string { return path.Base(fp) },
	"relTime":    func(t time.Time) string { return humanizeSince(time.Since(t)) },
	"formatTime": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
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
}

// mustPage parses the shared layout plus a page-specific {{define "content"}}.
func mustPage(content string) *template.Template {
	t := template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFS(dashboardFS, "layout.html"))
	return template.Must(t.Parse(content))
}

// ─── Page shells (layout + content) ──────────────────────────────────────────

var overviewPage = mustPage(`{{define "content"}}
<p class="subtitle">pipeline status &nbsp;·&nbsp; auto-refreshes every 3 s</p>
<div id="conn" class="conn-lost" role="status" aria-live="polite" hidden>&#9888;&#xFE0F;&nbsp;connection lost — data below may be stale</div>
<div id="action-error" aria-live="assertive"></div>
<div id="data-region"
     hx-get="/status/data" hx-trigger="load, every 3s" hx-swap="innerHTML"
     hx-sync="this:replace" hx-request='{"timeout": 5000}'
     hx-on::response-error="document.getElementById('conn').hidden = false"
     hx-on::send-error="document.getElementById('conn').hidden = false"
     hx-on::timeout="document.getElementById('conn').hidden = false"
     hx-on::after-request="if (event.detail.successful) document.getElementById('conn').hidden = true">
  <p class="htmx-indicator">loading…</p>
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

var failedPage = mustPage(`{{define "content"}}
<div id="action-error" aria-live="assertive"></div>
<div id="failed-region" hx-get="/failed/data" hx-trigger="load" hx-swap="innerHTML">
  <p class="htmx-indicator">loading…</p>
</div>
{{end}}`)

var trackPage = mustPage(`{{define "content"}}
<div id="action-error" aria-live="assertive"></div>
<div id="track-region" hx-get="/track/data?id={{.IDQuery}}" hx-trigger="load" hx-swap="innerHTML">
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
  <thead><tr><th>Track</th><th>Error</th><th>Attempts</th><th>Runner</th><th>Updated</th><th></th></tr></thead>
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
<div class="updated">updated {{.RenderedAt}}</div>

<!-- unified pipeline state: combines the pause flag AND runner liveness into one
     honest line, so it can never say "running" while no runner is connected. -->
<div class="pipeline {{.StateClass}}">
  <div class="pipe-main">
    <span class="dot {{.DotClass}}"></span>
    <span class="pipe-label">{{.StateLabel}}</span>
    <span class="pipe-sub">{{.SubText}}</span>
    {{if .MetaText}}<div class="pipe-meta">{{.MetaText}}</div>{{end}}
  </div>
  {{if .Stats.Paused}}
  <button class="btn btn-go" hx-post="/actions/resume" hx-target="#data-region" hx-swap="innerHTML"
          hx-confirm="Resume the pipeline? The runner will start claiming pending jobs.">&#9654;&nbsp;Resume pipeline</button>
  {{else}}
  <button class="btn btn-warn" hx-post="/actions/pause" hx-target="#data-region" hx-swap="innerHTML"
          hx-confirm="Pause the pipeline? The runner finishes its current job, then stops claiming new work.">&#10073;&#10073;&nbsp;Pause pipeline</button>
  {{end}}
</div>

{{if .ShowProgress}}
<div class="overview-progress">
  <div class="op-bar"><div class="op-fill" style="width:{{.DonePct}}%"></div></div>
  <div class="op-meta">
    <span class="op-main">{{.ProgressText}}</span>
    <span>{{.ThroughputText}}</span>
    <span>{{.ETAText}}</span>
  </div>
</div>
{{end}}

<div class="card-group">
  <div class="group-label">transcription</div>
  <div class="grid">
    <a class="card card-click" href="/library?status=pending" title="show pending books">
      <div class="card-label">Pending</div><div class="card-value blue">{{commafy .Stats.Pending}}</div></a>
    <a class="card card-click" href="/library?status=claimed" title="show books being transcribed">
      <div class="card-label">Transcribing</div><div class="card-value yellow">{{commafy .Stats.Claimed}}</div></a>
    <a class="card card-click" href="/library?status=done" title="show completed books">
      <div class="card-label">Done</div><div class="card-value green">{{commafy .Stats.Done}}</div></a>
    <a class="card card-click" href="/library?status=failed" title="show books with failures">
      <div class="card-label">Failed</div><div class="card-value{{if gt .Stats.Failed 0}} red{{end}}">{{commafy .Stats.Failed}}</div></a>
  </div>
</div>

<div class="card-group">
  <div class="group-label">embedding</div>
  <div class="grid">
    <div class="card"><div class="card-label">Chunks</div><div class="card-value">{{commafy .Stats.Chunks}}</div></div>
    <div class="card" title="completed transcripts not yet embedded (worker backlog)">
      <div class="card-label">Unembedded</div><div class="card-value{{if .EmbedStall}} amber{{end}}">{{commafy .Stats.EmbedBacklog}}</div></div>
    <div class="card" title="total embedding tokens (local tokenizer) across all embedded runs">
      <div class="card-label">Embed tokens</div><div class="card-value purple">{{commafy64Ptr .Stats.TotalEmbedTokens}}</div></div>
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
      <div class="card-label">Words indexed</div><div class="card-value">{{commafy64Ptr .Stats.TotalWords}}</div></div>
    <div class="card" title="book directories whose every track is done">
      <div class="card-label">Books complete</div><div class="card-value green">{{commafy .Stats.BooksFullyDone}}</div></div>
  </div>
</div>

{{if .EmbedStall}}
<div class="stall-callout">
  &#9888;&#xFE0F;&nbsp;{{commafy .Stats.EmbedBacklog}} completed transcripts are waiting to be embedded and not draining.
  Embedding (not transcription) is stalled — check Ollama{{if .EmbedURL}} at {{.EmbedURL}}{{end}} and that the embeddings model is pulled.
  Job rows stay <em>done</em> during an embed stall, so this is the only place it shows.
</div>
{{end}}

{{if gt .Stats.Failed 0}}
<div class="failed-callout">
  <span>&#9888;&#xFE0F;&nbsp;{{commafy .Stats.Failed}} failed job{{if gt .Stats.Failed 1}}s{{end}} &nbsp;<a href="/failed">view failed jobs &#8250;</a></span>
  <button class="btn btn-warn" hx-post="/actions/retry-failed" hx-target="#data-region" hx-swap="innerHTML"
          hx-confirm="Retry all {{.Stats.Failed}} failed job(s)? Each is reset to pending and re-transcribed.">retry all failed</button>
</div>
{{end}}

<div class="section">
  <div class="section-title">Recent Activity (last {{len .Jobs}})</div>
  {{if .Jobs}}
  <div class="table-wrap">
  <table>
    <thead><tr><th>File</th><th>Status</th><th title="transcription wall-clock time (runner)">Proc</th><th title="embedding tokens (local tokenizer)">Tokens</th><th>Updated</th><th></th></tr></thead>
    <tbody>
    {{range .Jobs}}
      <tr>
        <td>
          <a class="file-name" href="/book?dir={{bookDir .FilePath}}" title="{{.FilePath}}">{{shortName .FilePath}}</a>
          {{if .Error}}<details class="error-row"><summary>show error</summary><pre>{{derefStr .Error}}</pre></details>{{end}}
        </td>
        <td><span class="badge {{.Status}}">{{statusLabel .Status}}</span></td>
        <td class="time-muted" title="{{if .Chunked}}chunked{{if .NWindows}} ({{commafyPtr .NWindows}} windows){{end}}{{else}}single-pass{{end}}">{{procTime .ProcessingSeconds}}</td>
        <td class="time-muted">{{commafyPtr .EmbedTotalTokens}}</td>
        <td class="time-muted" title="{{formatTime .UpdatedAt}}">{{relTime .UpdatedAt}}</td>
        <td class="actions">
          {{if or (eq .Status "done") (eq .Status "failed")}}
          <button class="btn" hx-post="/actions/requeue?id={{.ID}}" hx-target="#data-region" hx-swap="innerHTML"
                  hx-confirm="Re-transcribe {{shortName .FilePath}}? This deletes its transcript + embeddings and re-runs the runner.">requeue</button>
          {{end}}
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p class="lib-empty">No jobs yet.</p>{{end}}
</div>
`))

// ─── Library fragment ────────────────────────────────────────────────────────

var libraryFragmentTmpl = template.Must(template.New("library").Funcs(tmplFuncs).Parse(`
<div class="lib-bar">
  <form class="lib-search" hx-get="/library/data" hx-target="#library-region" hx-swap="innerHTML">
    <input type="hidden" name="status" value="{{.Status}}">
    <input type="search" name="q" value="{{.Query}}" placeholder="search author / title / track…" autocomplete="off">
    <button type="submit" class="btn">search</button>
    {{if or .Query .Status}}<a class="lib-clear" hx-get="/library/data" hx-target="#library-region" hx-swap="innerHTML">clear</a>{{end}}
  </form>
  <div class="lib-chips">
    <a class="chip{{if eq .Status ""}} active{{end}}"        hx-get="/library/data?q={{.QueryEscaped}}"                hx-target="#library-region" hx-swap="innerHTML">all</a>
    <a class="chip{{if eq .Status "pending"}} active{{end}}" hx-get="/library/data?status=pending&q={{.QueryEscaped}}" hx-target="#library-region" hx-swap="innerHTML">pending</a>
    <a class="chip{{if eq .Status "claimed"}} active{{end}}" hx-get="/library/data?status=claimed&q={{.QueryEscaped}}" hx-target="#library-region" hx-swap="innerHTML">transcribing</a>
    <a class="chip{{if eq .Status "done"}} active{{end}}"    hx-get="/library/data?status=done&q={{.QueryEscaped}}"    hx-target="#library-region" hx-swap="innerHTML">done</a>
    <a class="chip{{if eq .Status "failed"}} active{{end}}"  hx-get="/library/data?status=failed&q={{.QueryEscaped}}"  hx-target="#library-region" hx-swap="innerHTML">failed</a>
  </div>
</div>

{{if .Books}}
<div class="table-wrap">
<table>
  <thead><tr><th>Book</th><th>Author</th><th>Progress</th>
    <th title="total audio duration across done tracks">Duration</th>
    <th title="total transcript words across done tracks">Words</th>
    <th title="total embedded chunks across done tracks">Chunks</th>
    <th title="suspected-error findings recorded for this book (read-only eval)">Findings</th>
    <th>Breakdown</th><th>Updated</th><th></th></tr></thead>
  <tbody>
  {{range .Books}}
    <tr class="clickable" onclick="window.location=this.querySelector('a.file-name').href">
      <td><a class="file-name" href="/book?dir={{.Dir}}" title="{{.Dir}}">{{.Title}}</a></td>
      <td class="time-muted">{{if .Author}}{{.Author}}{{else}}—{{end}}</td>
      <td>
        <div class="progress" title="{{.Done}}/{{.Total}} tracks done">
          <div class="progress-bar{{if gt .Failed 0}} has-failed{{end}}" style="width:{{.DonePct}}%"></div>
        </div>
        <span class="progress-text">{{commafy .Done}}/{{commafy .Total}}</span>
      </td>
      <td class="time-muted">{{durTime .DurationSeconds}}</td>
      <td class="time-muted">{{commafyPtr .WordCount}}</td>
      <td class="time-muted">{{commafyPtr .EmbedChunkCount}}</td>
      <td class="time-muted">{{if gt .FindingCount 0}}<a class="findings-link" href="/book?dir={{.Dir}}#book-findings" onclick="event.stopPropagation()" title="{{.FindingCount}} suspected-error finding(s)">&#9873; {{commafy .FindingCount}}</a>{{else}}—{{end}}</td>
      <td class="mini-badges">
        {{if gt .Pending 0}}<span class="badge pending">{{commafy .Pending}} pend</span>{{end}}
        {{if gt .Claimed 0}}<span class="badge claimed">{{commafy .Claimed}} transcribing</span>{{end}}
        {{if gt .Done 0}}<span class="badge done">{{commafy .Done}} done</span>{{end}}
        {{if gt .Failed 0}}<span class="badge failed">{{commafy .Failed}} fail</span>{{end}}
      </td>
      <td class="time-muted" title="{{formatTime .LastUpdated}}">{{relTime .LastUpdated}}</td>
      <td class="actions"><a class="btn" href="/book?dir={{.Dir}}">open&nbsp;&#8250;</a></td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>

<div class="lib-pager">
  {{if .HasPrev}}<a class="btn" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}&offset={{.PrevOffset}}" hx-target="#library-region" hx-swap="innerHTML">&#8592;&nbsp;prev</a>{{end}}
  <span class="lib-meta">{{commafy .TotalBooks}} book{{if ne .TotalBooks 1}}s{{end}} &nbsp;·&nbsp; page {{.Page}} / {{.TotalPages}}</span>
  {{if .HasNext}}<a class="btn" hx-get="/library/data?status={{.Status}}&q={{.QueryEscaped}}&offset={{.NextOffset}}" hx-target="#library-region" hx-swap="innerHTML">next&nbsp;&#8594;</a>{{end}}
</div>
{{else}}
<p class="lib-empty">No books match this filter{{if .Query}} for &ldquo;{{.Query}}&rdquo;{{end}}.</p>
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
    <button class="btn" hx-post="/actions/eval?dir={{.DirQuery}}" hx-target="#book-region" hx-swap="innerHTML"
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
  <thead><tr><th>Track</th><th>Status</th>
    <th title="audio duration">Duration</th>
    <th title="transcript word count (runner)">Words</th>
    <th title="transcription wall-clock time (runner)">Proc</th>
    <th title="audio codec · channel layout (runner)">Codec</th>
    <th title="embedded chunks">Chunks</th>
    <th title="suspected-error findings recorded for this track (read-only eval)">Flags</th>
    <th>Updated</th><th></th></tr></thead>
  <tbody>
  {{range .Tracks}}
    <tr>
      <td><a class="file-name" href="/track?id={{.ID}}" title="{{.FilePath}}">{{shortName .FilePath}}</a>
          {{if .Error}}<details class="error-row"><summary>show error</summary><pre>{{derefStr .Error}}</pre></details>{{end}}</td>
      <td><span class="badge {{.Status}}">{{statusLabel .Status}}</span></td>
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
    <thead><tr>
      <th title="judge self-scored confidence (triage highest-first)">Confidence</th>
      <th>Issue</th>
      <th title="suspected span → suggested correction (advisory only)">Correction</th>
      <th title="track · timestamp">Where</th>
    </tr></thead>
    <tbody>
    {{range .Findings}}
      <tr>
        <td>{{confPctF .Confidence}}</td>
        <td>{{.IssueType}}</td>
        <td>{{.OriginalText}} &#8594; {{strPtr .SuggestedCorrection}}</td>
        <td><span class="file-name" title="{{.FilePath}}">{{shortName .FilePath}}</span> &#183; {{timestamp .StartSec}}</td>
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
  <thead><tr><th>Track</th><th>Time</th><th>Match</th></tr></thead>
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
{{range .Segments}}
<div class="seg">
  <span class="seg-time">[{{timestamp .Start}} &#8594; {{timestamp .End}}]</span>
  {{if .Speaker}}<span class="seg-speaker">{{derefStr .Speaker}}</span>{{end}}
  <span class="seg-text">{{.Text}}</span>
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
    <span><span class="badge {{.Detail.Status}}">{{statusLabel .Detail.Status}}</span></span>
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
      {{range .PageSegments}}
      <div class="seg">
        <span class="seg-time">[{{timestamp .Start}} &#8594; {{timestamp .End}}]</span>
        {{if .Speaker}}<span class="seg-speaker">{{derefStr .Speaker}}</span>{{end}}
        <span class="seg-text">{{.Text}}</span>
      </div>
      {{end}}
      {{if .HasMore}}
      <button class="btn load-more" hx-get="/track/segments?id={{.IDQuery}}&offset={{.NextOffset}}"
              hx-swap="outerHTML" hx-target="this">load more ({{commafy (sub .TotalSegments .NextOffset)}} remaining)</button>
      {{end}}
    </div>
    {{else}}<p class="lib-empty">Transcript has no segments.</p>{{end}}
  </div>

  <div class="section">
    <div class="section-title">Chunks ({{commafy (len .Detail.Chunks)}})</div>
    {{if .Detail.Chunks}}
    <div class="table-wrap">
    <table>
      <thead><tr><th>#</th><th>Time range</th><th>Chars</th><th>Speaker</th></tr></thead>
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
    <p class="lib-empty">Not transcribed yet — this track is <span class="badge {{.Detail.Status}}">{{statusLabel .Detail.Status}}</span>. The transcript reader and chunk list appear once the runner completes it.</p>
  </div>
{{end}}
`))

// ─── Template models ─────────────────────────────────────────────────────────

type pageShell struct {
	Title     string
	Nav       string
	DirQuery  string // book page: URL-escaped dir for the hx-get fragment load
	IDQuery   string // track page only: URL-escaped job id for the hx-get fragment load
	DataQuery string // library page only: "?status=…&q=…" for the initial fragment load
}

type statusData struct {
	Stats *db.QueueStats
	Jobs  []db.RecentJob

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

type libraryData struct {
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
}

// segmentsData backs the segment-page fragment (the htmx "load more" target):
// one page of segments plus the link to the next page.
type segmentsData struct {
	Segments   []db.Segment
	HasMore    bool
	NextOffset int
	IDQuery    string
}

// newStatusData derives the unified pipeline state from the pause flag and the
// runner heartbeat freshness, so the banner is never self-contradictory.
func newStatusData(stats *db.QueueStats, jobs []db.RecentJob, now time.Time, staleAfter time.Duration, embedURL string) statusData {
	d := statusData{
		Stats:      stats,
		Jobs:       jobs,
		RenderedAt: now.UTC().Format("15:04:05 UTC"),
		EmbedURL:   embedURL,
		EmbedStall: stats.EmbedBacklog >= embedStallThreshold,
	}

	// Backfill progress / throughput / ETA.
	if stats.TotalJobs > 0 {
		d.ShowProgress = true
		d.DonePct = stats.Done * 100 / stats.TotalJobs
		d.ProgressText = fmt.Sprintf("%s / %s (%d%%)", commafy(stats.Done), commafy(stats.TotalJobs), d.DonePct)
		remaining := stats.Pending + stats.Claimed
		if stats.DoneLastHour > 0 {
			d.ThroughputText = fmt.Sprintf("~%s done in the last hour", commafy(stats.DoneLastHour))
			etaHours := float64(remaining) / float64(stats.DoneLastHour)
			d.ETAText = humanizeETA(etaHours)
		} else {
			d.ThroughputText = "no completions in the last hour"
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
		// Not paused, no fresh runner, and nothing waiting — genuinely idle
		// (queue drained) or a runner was never seen.
		d.StateLabel, d.StateClass, d.DotClass = "IDLE", "state-idle", "blue"
		if stats.RunnerActive {
			d.SubText = "enabled — runner heartbeat is stale (no work waiting)"
		} else {
			d.SubText = "enabled — no runner is currently connected"
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

// handleOverviewPage serves the Overview page shell (GET /). The "/" route is a
// catch-all, so 404 any other path.
func (s *MCPServer) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.renderPage(w, overviewPage, pageShell{Title: "overview", Nav: "overview"})
}

func (s *MCPServer) handleLibraryPage(w http.ResponseWriter, r *http.Request) {
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
	dataQuery := ""
	if enc := vals.Encode(); enc != "" {
		dataQuery = "?" + enc
	}
	s.renderPage(w, libraryPage, pageShell{Title: "library", Nav: "library", DataQuery: dataQuery})
}

func (s *MCPServer) handleBookPage(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "missing dir", http.StatusBadRequest)
		return
	}
	s.renderPage(w, bookPage, pageShell{Title: "book", Nav: "library", DirQuery: url.QueryEscape(dir)})
}

func (s *MCPServer) renderPage(w http.ResponseWriter, tmpl *template.Template, data pageShell) {
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
	data := newStatusData(stats, jobs, time.Now(), s.runnerStaleAfter, s.embedURL)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := statusFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("status fragment render error", "error", err)
	}
}

// ─── Library data handler ────────────────────────────────────────────────────

func (s *MCPServer) handleLibraryData(w http.ResponseWriter, r *http.Request) {
	status := validStatus(r.URL.Query().Get("status"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
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

	books, total, err := s.db.GetBookSummaries(r.Context(), db.BookFilter{
		Status: status, Query: query, Limit: libraryPageSize, Offset: offset,
	})
	if err != nil {
		s.logger.Error("GetBookSummaries error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// One whole-library findings-count aggregate, keyed by book dir, so the ⚑
	// column on each (paged) row is a map lookup rather than an N+1 per row. A
	// findings error must not break the library list, so it's logged and the
	// column just renders 0 (em dash) everywhere.
	findingsByBook, err := s.db.GetFindingsCountByBook(r.Context())
	if err != nil {
		s.logger.Error("GetFindingsCountByBook error", "error", err)
		findingsByBook = nil
	}

	rows := make([]bookRow, 0, len(books))
	for _, b := range books {
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
			FindingCount: findingsByBook[b.Dir],
		})
	}

	totalPages := (total + libraryPageSize - 1) / libraryPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	data := libraryData{
		Books: rows, Status: status, Query: query, QueryEscaped: url.QueryEscape(query),
		Page: offset/libraryPageSize + 1, TotalPages: totalPages, TotalBooks: total,
		HasPrev: offset > 0, HasNext: offset+libraryPageSize < total,
		PrevOffset: max(0, offset-libraryPageSize), NextOffset: offset + libraryPageSize,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := libraryFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("library render error", "error", err)
	}
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
	s.renderPage(w, trackPage, pageShell{
		Title: "track", Nav: "library", IDQuery: url.QueryEscape(id),
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
	page, hasMore, next := paginateSegments(detail.Segments, 0)
	d.PageSegments, d.HasMore, d.NextOffset = page, hasMore, next

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
	data := segmentsData{Segments: page, HasMore: hasMore, NextOffset: next, IDQuery: url.QueryEscape(id)}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := segmentsFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("segments fragment render error", "error", err)
	}
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

func (s *MCPServer) handleFailedPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, failedPage, pageShell{Title: "failed", Nav: "failed"})
}

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
