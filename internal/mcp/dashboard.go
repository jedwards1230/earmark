package mcp

import (
	"embed"
	"html/template"
	"net/http"
	"path/filepath"
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
}).Parse(`
<!-- stat cards -->
<div class="grid">
  <div class="card">
    <div class="card-label">Pending</div>
    <div class="card-value blue">{{.Stats.Pending}}</div>
  </div>
  <div class="card">
    <div class="card-label">Claimed</div>
    <div class="card-value yellow">{{.Stats.Claimed}}</div>
  </div>
  <div class="card">
    <div class="card-label">Done</div>
    <div class="card-value green">{{.Stats.Done}}</div>
  </div>
  <div class="card">
    <div class="card-label">Failed</div>
    <div class="card-value{{if gt .Stats.Failed 0}} red{{end}}">{{.Stats.Failed}}</div>
  </div>
  <div class="card">
    <div class="card-label">Transcripts</div>
    <div class="card-value purple">{{.Stats.Transcripts}}</div>
  </div>
  <div class="card">
    <div class="card-label">Chunks</div>
    <div class="card-value">{{.Stats.Chunks}}</div>
  </div>
</div>

<!-- runner status -->
<div class="section">
  <div class="section-title">ASR Runner</div>
  <div class="runner-box">
    {{if .Stats.RunnerActive}}
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
        <div class="runner-val">{{formatTimePtr .Stats.LastHeartbeat}}</div>
      </div>
    </div>
    {{else}}
    <div class="runner-row">
      <div class="runner-item">
        <div class="runner-key">Status</div>
        <div class="runner-val"><span class="dot {{if gt .Stats.Pending 0}}yellow{{else}}red{{end}}"></span>idle / not running</div>
      </div>
    </div>
    {{end}}
    {{if gt .Stats.Failed 0}}
    <div style="margin-top:10px;color:var(--red);font-size:12px;">
      &#9888;&#xFE0F;&nbsp;{{.Stats.Failed}} failed job{{if gt .Stats.Failed 1}}s{{end}} — check recent activity below
    </div>
    {{end}}
  </div>
</div>

<!-- recent activity -->
<div class="section">
  <div class="section-title">Recent Activity (last {{len .Jobs}})</div>
  {{if .Jobs}}
  <div class="card" style="padding:0;overflow:hidden;">
  <table>
    <thead>
      <tr>
        <th>File</th>
        <th>Status</th>
        <th>Updated</th>
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
        <td class="time-muted">{{formatTime .UpdatedAt}}</td>
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

// dashboardData is the template model for the refreshed fragment.
type dashboardData struct {
	Stats *db.QueueStats
	Jobs  []db.RecentJob
}

// handleDashboardPage serves the full HTML shell (GET /).
func (s *MCPServer) handleDashboardPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := dashboardPage.Execute(w, nil); err != nil {
		s.logger.Error("dashboard page render error", "error", err)
	}
}

// handleStatusData serves the htmx-refreshed inner fragment (GET /status/data).
func (s *MCPServer) handleStatusData(w http.ResponseWriter, r *http.Request) {
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := fragmentTmpl.Execute(w, dashboardData{Stats: stats, Jobs: jobs}); err != nil {
		s.logger.Error("fragment render error", "error", err)
	}
}
