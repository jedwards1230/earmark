package mcp

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/db"
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
	// bookHref builds the book-detail URL for a track file path. Returned as
	// template.URL so html/template emits the pre-encoded query verbatim.
	"bookHref":    func(fp string) template.URL { return bookURL(path.Dir(fp)) },
	"bookHrefDir": func(dir string) template.URL { return bookURL(dir) },
}

func bookURL(dir string) template.URL {
	return template.URL("/book?dir=" + url.QueryEscape(dir)) //nolint:gosec // dir is server-derived and query-escaped
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
<div id="library-region" hx-get="/library/data" hx-trigger="load" hx-swap="innerHTML">
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

<div class="grid">
  <a class="card card-click" href="/library?status=pending" title="show pending books">
    <div class="card-label">Pending</div><div class="card-value blue">{{commafy .Stats.Pending}}</div></a>
  <a class="card card-click" href="/library?status=claimed" title="show in-progress books">
    <div class="card-label">Claimed</div><div class="card-value yellow">{{commafy .Stats.Claimed}}</div></a>
  <a class="card card-click" href="/library?status=done" title="show completed books">
    <div class="card-label">Done</div><div class="card-value green">{{commafy .Stats.Done}}</div></a>
  <a class="card card-click" href="/library?status=failed" title="show books with failures">
    <div class="card-label">Failed</div><div class="card-value{{if gt .Stats.Failed 0}} red{{end}}">{{commafy .Stats.Failed}}</div></a>
  <div class="card"><div class="card-label">Transcripts</div><div class="card-value purple">{{commafy .Stats.Transcripts}}</div></div>
  <div class="card"><div class="card-label">Chunks</div><div class="card-value">{{commafy .Stats.Chunks}}</div></div>
  <div class="card" title="completed transcripts not yet embedded (worker backlog)">
    <div class="card-label">Unembedded</div><div class="card-value{{if .EmbedStall}} amber{{end}}">{{commafy .Stats.EmbedBacklog}}</div></div>
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
  <span>&#9888;&#xFE0F;&nbsp;{{commafy .Stats.Failed}} failed job{{if gt .Stats.Failed 1}}s{{end}}</span>
  <button class="btn btn-warn" hx-post="/actions/retry-failed" hx-target="#data-region" hx-swap="innerHTML"
          hx-confirm="Retry all {{.Stats.Failed}} failed job(s)? Each is reset to pending and re-transcribed.">retry all failed</button>
</div>
{{end}}

<div class="section">
  <div class="section-title">Recent Activity (last {{len .Jobs}})</div>
  {{if .Jobs}}
  <div class="table-wrap">
  <table>
    <thead><tr><th>File</th><th>Status</th><th>Updated</th><th></th></tr></thead>
    <tbody>
    {{range .Jobs}}
      <tr>
        <td>
          <a class="file-name" href="{{bookHref .FilePath}}" title="{{.FilePath}}">{{shortName .FilePath}}</a>
          {{if .Error}}<div class="error-row">{{derefStr .Error}}</div>{{end}}
        </td>
        <td><span class="badge {{.Status}}">{{.Status}}</span></td>
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
    <a class="chip{{if eq .Status "claimed"}} active{{end}}" hx-get="/library/data?status=claimed&q={{.QueryEscaped}}" hx-target="#library-region" hx-swap="innerHTML">claimed</a>
    <a class="chip{{if eq .Status "done"}} active{{end}}"    hx-get="/library/data?status=done&q={{.QueryEscaped}}"    hx-target="#library-region" hx-swap="innerHTML">done</a>
    <a class="chip{{if eq .Status "failed"}} active{{end}}"  hx-get="/library/data?status=failed&q={{.QueryEscaped}}"  hx-target="#library-region" hx-swap="innerHTML">failed</a>
  </div>
</div>

{{if .Books}}
<div class="table-wrap">
<table>
  <thead><tr><th>Book</th><th>Author</th><th>Progress</th><th>Breakdown</th><th>Updated</th><th></th></tr></thead>
  <tbody>
  {{range .Books}}
    <tr class="clickable" onclick="window.location='{{.Href}}'">
      <td><a class="file-name" href="{{.Href}}" title="{{.Dir}}">{{.Title}}</a></td>
      <td class="time-muted">{{if .Author}}{{.Author}}{{else}}—{{end}}</td>
      <td>
        <div class="progress" title="{{.Done}}/{{.Total}} tracks done">
          <div class="progress-bar{{if gt .Failed 0}} has-failed{{end}}" style="width:{{.DonePct}}%"></div>
        </div>
        <span class="progress-text">{{commafy .Done}}/{{commafy .Total}}</span>
      </td>
      <td class="mini-badges">
        {{if gt .Pending 0}}<span class="badge pending">{{commafy .Pending}} pend</span>{{end}}
        {{if gt .Claimed 0}}<span class="badge claimed">{{commafy .Claimed}} run</span>{{end}}
        {{if gt .Done 0}}<span class="badge done">{{commafy .Done}} done</span>{{end}}
        {{if gt .Failed 0}}<span class="badge failed">{{commafy .Failed}} fail</span>{{end}}
      </td>
      <td class="time-muted" title="{{formatTime .LastUpdated}}">{{relTime .LastUpdated}}</td>
      <td class="actions"><a class="btn" href="{{.Href}}">open&nbsp;&#8250;</a></td>
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
    {{if gt .Claimed 0}}<span class="time-muted">{{commafy .Claimed}} in progress</span>{{end}}
    {{if gt .Failed 0}}<span style="color:var(--red)">{{commafy .Failed}} failed</span>{{end}}
  </div>
  <div class="book-path">{{.Dir}}</div>
  <div class="book-actions">
    <button class="btn btn-warn" hx-post="/actions/book-requeue?dir={{.DirQuery}}" hx-target="#book-region" hx-swap="innerHTML"
            hx-confirm="Re-transcribe all {{.Total}} track(s) of this book? Deletes their transcripts + embeddings and re-runs the runner.">requeue entire book</button>
  </div>
</div>

{{if .Tracks}}
<div class="table-wrap">
<table>
  <thead><tr><th>Track</th><th>Status</th><th>Updated</th><th></th></tr></thead>
  <tbody>
  {{range .Tracks}}
    <tr>
      <td><div class="file-name" title="{{.FilePath}}">{{shortName .FilePath}}</div>
          {{if .Error}}<div class="error-row">{{derefStr .Error}}</div>{{end}}</td>
      <td><span class="badge {{.Status}}">{{.Status}}</span></td>
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
`))

// ─── Template models ─────────────────────────────────────────────────────────

type pageShell struct {
	Title    string
	Nav      string
	DirQuery string // book page only
}

type statusData struct {
	Stats *db.QueueStats
	Jobs  []db.RecentJob

	RenderedAt string
	EmbedStall bool
	EmbedURL   string

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
	Href        template.URL
	DonePct     int
	Total       int
	Pending     int
	Claimed     int
	Done        int
	Failed      int
	LastUpdated time.Time
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
	default:
		// Not paused, but no fresh runner heartbeat — enabled yet idle.
		d.StateLabel, d.StateClass, d.DotClass = "IDLE", "state-idle", "blue"
		if stats.RunnerActive {
			d.SubText = "enabled — runner heartbeat is stale (crashed?)"
		} else {
			d.SubText = "enabled — no runner is currently connected"
		}
	}
	return d
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func commafy(n int) string {
	s := strconv.Itoa(n)
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

func (s *MCPServer) handleLibraryPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, libraryPage, pageShell{Title: "library", Nav: "library"})
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
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	books, total, err := s.db.GetBookSummaries(r.Context(), db.BookFilter{
		Status: status, Query: query, Limit: libraryPageSize, Offset: offset,
	})
	if err != nil {
		s.logger.Error("GetBookSummaries error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rows := make([]bookRow, 0, len(books))
	for _, b := range books {
		author, title := s.resolver.Resolve(b.Dir, b.SamplePath)
		pct := 0
		if b.Total > 0 {
			pct = b.Done * 100 / b.Total
		}
		rows = append(rows, bookRow{
			Dir: b.Dir, Title: title, Author: author, Href: bookURL(b.Dir), DonePct: pct,
			Total: b.Total, Pending: b.Pending, Claimed: b.Claimed, Done: b.Done, Failed: b.Failed,
			LastUpdated: b.LastUpdated,
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
	tracks, err := s.db.GetBookTracks(r.Context(), dir)
	if err != nil {
		s.logger.Error("GetBookTracks error", "dir", dir, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	d := bookData{Dir: dir, DirQuery: url.QueryEscape(dir), Tracks: tracks, Total: len(tracks)}
	for _, t := range tracks {
		switch t.Status {
		case "pending":
			d.Pending++
		case "claimed":
			d.Claimed++
		case "done":
			d.Done++
		case "failed":
			d.Failed++
		}
	}
	sample := dir
	if len(tracks) > 0 {
		sample = tracks[0].FilePath
	}
	d.Author, d.Title = s.resolver.Resolve(dir, sample)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := bookFragmentTmpl.Execute(w, d); err != nil {
		s.logger.Error("book fragment render error", "error", err)
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
	if err := s.db.SetPaused(r.Context(), false, "dashboard"); err != nil {
		s.logger.Error("resume error", "error", err)
		writeActionError(w, "resume failed — see server logs")
		return
	}
	s.logger.Info("pipeline resumed via dashboard")
	s.renderStatusFragment(w, r)
}
