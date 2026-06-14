package mcp

import (
	"html/template"
	"net/http"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// ─── Findings page (read-only eval-layer surface, CONTRACT §2.15) ─────────────
//
// The Findings page surfaces the LLM-as-judge output recorded in
// transcript_findings: how many suspected transcription errors there are, their
// confidence spread (you triage by confidence — a wrong flag is harmless), the
// issue-type mix, and a per-book breakdown. It is purely observational; like the
// rest of the dashboard it never mutates anything, and it is self-contained so
// it merges cleanly alongside the parallel Servers/Models reshaping (#48).

// findingsData is the template model for the findings fragment.
type findingsData struct {
	Summary    *db.FindingsSummary
	RenderedAt string
}

// findingsPage is the page shell (layout + content) for GET /findings.
var findingsPage = mustPage(`{{define "content"}}
<p class="subtitle">transcript findings &nbsp;·&nbsp; read-only LLM judge &nbsp;·&nbsp; auto-refreshes every 5 s</p>
<div id="conn" class="conn-lost" role="status" aria-live="polite" hidden>&#9888;&#xFE0F;&nbsp;connection lost — data below may be stale</div>
<div id="findings-region"
     hx-get="/findings/data" hx-trigger="load, every 5s" hx-swap="innerHTML"
     hx-sync="this:replace" hx-request='{"timeout": 5000}'
     hx-on::response-error="document.getElementById('conn').hidden = false"
     hx-on::send-error="document.getElementById('conn').hidden = false"
     hx-on::timeout="document.getElementById('conn').hidden = false"
     hx-on::after-request="if (event.detail.successful) document.getElementById('conn').hidden = true">
  <p class="htmx-indicator">loading…</p>
</div>
{{end}}`)

// findingsFragmentTmpl is the htmx-refreshed data fragment.
var findingsFragmentTmpl = template.Must(template.New("findings").Funcs(tmplFuncs).Parse(`
<div class="updated">updated {{.RenderedAt}}</div>
{{if eq .Summary.TotalFindings 0}}
<p class="lib-empty">No findings recorded yet. The eval layer is read-only and on-demand — run <code>earmark eval "&lt;book&gt;"</code> or <code>earmark eval --sample N</code> to record suspected transcription errors (advisory only; transcripts are never edited).</p>
{{else}}
<div class="card-group grid">
  <div class="card"><div class="card-label">Findings</div><div class="card-value">{{commafy .Summary.TotalFindings}}</div></div>
  <div class="card"><div class="card-label">Mean confidence</div><div class="card-value blue">{{confPct .Summary.MeanConfidence}}</div></div>
  <div class="card"><div class="card-label">High &ge;0.8</div><div class="card-value red">{{commafy .Summary.HighConfidence}}</div></div>
  <div class="card"><div class="card-label">Medium 0.4–0.8</div><div class="card-value amber">{{commafy .Summary.MediumConfidence}}</div></div>
  <div class="card"><div class="card-label">Low &lt;0.4</div><div class="card-value">{{commafy .Summary.LowConfidence}}</div></div>
</div>

{{if .Summary.ByIssueType}}
<div class="section">
  <div class="section-title">Issue types</div>
  <div class="table-wrap">
  <table>
    <thead><tr><th>Issue type</th><th>Count</th></tr></thead>
    <tbody>
    {{range .Summary.ByIssueType}}
      <tr><td>{{.IssueType}}</td><td>{{commafy .Count}}</td></tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>
{{end}}

{{if .Summary.ByBook}}
<div class="section">
  <div class="section-title">Per book</div>
  <div class="table-wrap">
  <table>
    <thead><tr>
      <th>Book</th>
      <th title="suspected errors recorded for this book">Findings</th>
      <th title="mean self-scored confidence of this book's findings">Mean conf</th>
      <th title="most common issue type for this book">Top issue</th>
    </tr></thead>
    <tbody>
    {{range .Summary.ByBook}}
      <tr>
        <td>{{shortName .BookDir}}</td>
        <td>{{commafy .Count}}</td>
        <td>{{confPctF .MeanConfidence}}</td>
        <td>{{.TopIssueType}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>
{{end}}
{{end}}
`))

// handleFindingsPage serves the Findings page shell (GET /findings).
func (s *MCPServer) handleFindingsPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, findingsPage, pageShell{Title: "findings", Nav: "findings"})
}

// handleFindingsData serves the htmx findings fragment (GET /findings/data).
func (s *MCPServer) handleFindingsData(w http.ResponseWriter, r *http.Request) {
	summary, err := s.db.GetFindingsSummary(r.Context())
	if err != nil {
		s.logger.Error("GetFindingsSummary error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := findingsData{Summary: summary, RenderedAt: time.Now().UTC().Format("2006-01-02 15:04:05 UTC")}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := findingsFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("findings fragment render error", "error", err)
	}
}
