package mcp

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/db"
)

//go:embed dashboard.html htmx.min.js
var dashboardFS embed.FS

// dashboardPage is parsed once at init time; the file is embedded so there is
// no runtime filesystem dependency.
var dashboardPage = template.Must(
	template.ParseFS(dashboardFS, "dashboard.html"),
)

// htmxJS is the vendored, version-pinned htmx library (htmx.org v2.0.4).
// Serving it from the binary instead of a CDN keeps the dashboard working
// offline / air-gapped and avoids a third-party runtime dependency.
var htmxJS = func() []byte {
	b, err := dashboardFS.ReadFile("htmx.min.js")
	if err != nil {
		panic("embedded htmx.min.js missing: " + err.Error())
	}
	return b
}()

// handleHTMX serves the embedded htmx library (GET /static/htmx.min.js).
func (s *MCPServer) handleHTMX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(htmxJS)
}

// fragmentTmpl is the htmx-refreshed inner fragment (no <html>/<head> wrapper).
// It is defined inline to avoid a second embedded file.
var fragmentTmpl = template.Must(template.New("fragment").Funcs(template.FuncMap{
	"shortName": func(fp string) string {
		return filepath.Base(fp)
	},
	"formatTime": func(t time.Time) string {
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	},
	"relTime": func(t time.Time) string {
		return humanizeSince(time.Since(t))
	},
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
}).Parse(`
<!-- updated stamp: server-rendered so even a successful refresh shows recency -->
<div class="updated">updated {{.RenderedAt}}</div>

<!-- stat cards -->
<div class="grid">
  <div class="card">
    <div class="card-label">Pending</div>
    <div class="card-value blue">{{commafy .Stats.Pending}}</div>
  </div>
  <div class="card">
    <div class="card-label">Claimed</div>
    <div class="card-value yellow">{{commafy .Stats.Claimed}}</div>
  </div>
  <div class="card">
    <div class="card-label">Done</div>
    <div class="card-value green">{{commafy .Stats.Done}}</div>
  </div>
  <div class="card">
    <div class="card-label">Failed</div>
    <div class="card-value{{if gt .Stats.Failed 0}} red{{end}}">{{commafy .Stats.Failed}}</div>
  </div>
  <div class="card">
    <div class="card-label">Transcripts</div>
    <div class="card-value purple">{{commafy .Stats.Transcripts}}</div>
  </div>
  <div class="card">
    <div class="card-label">Chunks</div>
    <div class="card-value">{{commafy .Stats.Chunks}}</div>
  </div>
</div>

<!-- runner status -->
<div class="section">
  <div class="section-title">ASR Runner</div>
  <div class="runner-box">
    {{if and .Stats.RunnerActive (not .RunnerStale)}}
    <div class="runner-row">
      <div class="runner-item">
        <div class="runner-key">Status</div>
        <div class="runner-val"><span class="dot green"></span>active</div>
      </div>
      <div class="runner-item">
        <div class="runner-key">Runner ID</div>
        <div class="runner-val">{{.Stats.RunnerID}}</div>
      </div>
      <div class="runner-item">
        <div class="runner-key">Last heartbeat</div>
        <div class="runner-val" title="{{formatTimePtr .Stats.LastHeartbeat}}">{{.HeartbeatRel}}</div>
      </div>
    </div>
    {{else if .RunnerStale}}
    <div class="runner-row">
      <div class="runner-item">
        <div class="runner-key">Status</div>
        <div class="runner-val"><span class="dot amber"></span>stale — last seen {{.HeartbeatRel}}</div>
      </div>
      <div class="runner-item">
        <div class="runner-key">Runner ID</div>
        <div class="runner-val">{{.Stats.RunnerID}}</div>
      </div>
      <div class="runner-item">
        <div class="runner-key">Last heartbeat</div>
        <div class="runner-val" title="{{formatTimePtr .Stats.LastHeartbeat}}">{{.HeartbeatRel}}</div>
      </div>
    </div>
    {{else}}
    <div class="runner-row">
      <div class="runner-item">
        <div class="runner-key">Status</div>
        <div class="runner-val"><span class="dot {{if .Empty}}grey{{else if gt .Stats.Pending 0}}yellow{{else}}red{{end}}"></span>{{if .Empty}}no jobs queued yet{{else}}idle / not running{{end}}</div>
      </div>
    </div>
    {{end}}
    {{if gt .Stats.Failed 0}}
    <div class="failed-callout">
      <span>&#9888;&#xFE0F;&nbsp;{{commafy .Stats.Failed}} failed job{{if gt .Stats.Failed 1}}s{{end}} — check recent activity below</span>
      <button class="btn btn-warn"
              hx-post="/actions/retry-failed"
              hx-target="#data-region" hx-swap="innerHTML"
              hx-confirm="Retry all {{.Stats.Failed}} failed job(s)? Each is reset to pending and re-transcribed.">retry all failed</button>
    </div>
    {{end}}
  </div>
</div>

<!-- recent activity -->
<div class="section">
  <div class="section-title">Recent Activity (last {{len .Jobs}})</div>
  {{if .Jobs}}
  <!-- table-wrap scrolls horizontally on narrow viewports so the actions
       column is never clipped (the card's rounded corners still clip the y). -->
  <div class="card table-wrap">
  <table>
    <thead>
      <tr>
        <th>File</th>
        <th>Status</th>
        <th>Updated</th>
        <th></th>
      </tr>
    </thead>
    <tbody>
    {{range .Jobs}}
      <tr>
        <td>
          <div class="file-name" title="{{.FilePath}}">{{shortName .FilePath}}</div>
          {{if .Error}}<div class="error-row">{{derefStr .Error}}</div>{{end}}
        </td>
        <td><span class="badge {{.Status}}">{{.Status}}</span></td>
        <td class="time-muted" title="{{formatTime .UpdatedAt}}">{{relTime .UpdatedAt}}</td>
        <td class="actions">
          {{if or (eq .Status "done") (eq .Status "failed")}}
          <button class="btn"
                  hx-post="/actions/requeue?id={{.ID}}"
                  hx-target="#data-region" hx-swap="innerHTML"
                  hx-confirm="Re-transcribe {{shortName .FilePath}}? This deletes its transcript + embeddings and re-runs the runner.">requeue</button>
          {{end}}
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}
  <p style="color:var(--muted)">No jobs yet.</p>
  {{end}}
</div>
`))

// dashboardData is the template model for the refreshed fragment. The derived
// fields (RunnerStale/HeartbeatRel/RenderedAt/Empty) are computed in the handler
// since html/template can't call time.Now() to judge freshness itself.
type dashboardData struct {
	Stats *db.QueueStats
	Jobs  []db.RecentJob

	RunnerStale  bool   // RunnerActive but heartbeat older than runnerStaleAfter
	HeartbeatRel string // e.g. "12s ago" / "2h ago" / "—"
	RenderedAt   string // server render time, so a successful refresh shows recency
	Empty        bool   // no jobs at all and no runner ever seen (fresh install)
}

// newDashboardData builds the template model, deriving freshness fields from the
// current time so the view can distinguish a live runner from a crashed one.
func newDashboardData(stats *db.QueueStats, jobs []db.RecentJob, now time.Time, staleAfter time.Duration) dashboardData {
	d := dashboardData{
		Stats:        stats,
		Jobs:         jobs,
		HeartbeatRel: "—",
		RenderedAt:   now.UTC().Format("15:04:05 UTC"),
		Empty: !stats.RunnerActive && stats.LastHeartbeat == nil &&
			stats.Pending == 0 && stats.Claimed == 0 &&
			stats.Done == 0 && stats.Failed == 0,
	}
	if stats.LastHeartbeat != nil {
		age := now.Sub(*stats.LastHeartbeat)
		d.HeartbeatRel = humanizeSince(age)
		if stats.RunnerActive && age > staleAfter {
			d.RunnerStale = true
		}
	}
	return d
}

// commafy renders an integer with thousands separators (18452 -> "18,452").
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

// humanizeSince renders a coarse, glanceable relative duration ("12s ago").
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

// handleDashboardPage serves the full HTML shell (GET /). The "/" route is a
// ServeMux catch-all, so reject any non-root path with 404 instead of serving
// the dashboard for e.g. /favicon.ico or a mistyped path.
func (s *MCPServer) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := dashboardPage.Execute(w, nil); err != nil {
		s.logger.Error("dashboard page render error", "error", err)
	}
}

// handleStatusData serves the htmx-refreshed inner fragment (GET /status/data).
func (s *MCPServer) handleStatusData(w http.ResponseWriter, r *http.Request) {
	s.renderStatusFragment(w, r)
}

// renderStatusFragment queries the live status and renders the htmx fragment.
// Shared by the periodic refresh and by the action handlers (which re-render the
// fragment after mutating so the table updates immediately).
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

	data := newDashboardData(stats, jobs, time.Now(), s.runnerStaleAfter)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := fragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("fragment render error", "error", err)
	}
}

// writeActionError surfaces a failed mutation to the user instead of letting it
// vanish silently: htmx does not swap the target on a non-2xx response, so a bare
// http.Error would leave the dashboard looking like nothing happened. We return
// 200 with HX-Retarget so htmx swaps a dismissible banner into #action-error,
// while the periodic poll still refreshes #data-region on its own.
func writeActionError(w http.ResponseWriter, msg string) {
	w.Header().Set("HX-Retarget", "#action-error")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<div class="action-err">&#9888;&#xFE0F;&nbsp;%s</div>`,
		template.HTMLEscapeString(msg))
}

// isHTMX reports whether the request came from htmx (which sets HX-Request:
// true). The mutating action endpoints require it — a lightweight guard against
// drive-by/CSRF form posts, which cannot set custom headers cross-origin without
// a CORS preflight the dashboard never grants.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// handleRequeueJob re-transcribes a single job (POST /actions/requeue?id=…) and
// returns the refreshed status fragment so htmx swaps in the updated table.
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
	s.renderStatusFragment(w, r)
}

// handleRetryFailed re-transcribes every failed job (POST /actions/retry-failed)
// and returns the refreshed status fragment.
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
