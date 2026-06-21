package mcp

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
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
	// EvalConfigured gates the "run sample eval" trigger and the honest
	// empty-state (CONTRACT §2.15): true only when a judge chat endpoint resolves.
	// When false the empty-state points at the Models/Services page instead of
	// printing an unrunnable CLI command.
	EvalConfigured bool
	// EvalSampleN is the chunk count the sample-eval button requests.
	EvalSampleN int
	// ControlEnabled gates the "clear findings" button: it only renders when a
	// CONTROL_API_TOKEN is configured, so it never appears as a button that
	// would fail-close (503) on click — same honesty rule the eval triggers
	// follow. Clearing needs the token but NOT a configured eval endpoint.
	ControlEnabled bool
	// Groups is the deduplicated triage worklist (highest-confidence first): exact
	// repeats of the same correction (same original span → same suggestion → same
	// issue type) collapse into one row carrying an occurrence count and every
	// location, so a 243-row list of mostly-identical fixes reads as a handful of
	// distinct corrections. Built from the filtered FindingRow set.
	Groups []findingGroup
	// Filter state (faceting): the active confidence floor, issue-type, and book
	// scope. Threaded through the controls so they persist, and used to mark the
	// matching Issue-Types / Per-Book rows active.
	Filter findingsFilter
	// MatchedCount / TotalCount are the worklist sizes after / before the active
	// filters (individual findings, not groups) — for the "showing N of M" label.
	MatchedCount int
	TotalCount   int
	// IssueTypeOptions / BookOptions back the filter <select> controls (drawn from
	// the summary roll-up so they list exactly the present values).
	IssueTypeOptions []db.IssueTypeCount
	BookOptions      []db.BookFindings
}

// findingsFilter is the active faceting state for the worklist.
type findingsFilter struct {
	MinConfidence int    // percent floor (0/40/60/80); 0 = no floor
	IssueType     string // exact issue_type, or "" = any
	BookDir       string // exact book dir, or "" = any
	BookLabel     string // resolved short label for the active book (for the chip)
}

// Active reports whether any facet is narrowing the worklist.
func (f findingsFilter) Active() bool {
	return f.MinConfidence > 0 || f.IssueType != "" || f.BookDir != ""
}

// worklistURL builds a /findings/data URL with one facet (which ∈ conf|issue|book)
// set to val, carrying the other two active facets through so toggling one filter
// preserves the rest. An empty val clears that facet. Values are url.QueryEscaped;
// the result is template.URL for interpolation into an hx-get (a fixed-shape query
// string built from allow-listed keys, no raw user markup).
func worklistURL(f findingsFilter, which, val string) template.URL {
	conf := f.MinConfidence
	issue := f.IssueType
	book := f.BookDir
	switch which {
	case "conf":
		conf = atoiOr0(val)
	case "issue":
		issue = val
	case "book":
		book = val
	}
	q := url.Values{}
	if conf > 0 {
		q.Set("conf", strconv.Itoa(conf))
	}
	if issue != "" {
		q.Set("issue", issue)
	}
	if book != "" {
		q.Set("book", book)
	}
	// Point at the full page (/findings), not the fragment: the facet controls do a
	// real navigation so the auto-refreshing region (whose hx-get carries the same
	// params via DataQuery) keeps the filter instead of the 5s refresh wiping it.
	u := "/findings"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	// #nosec G203 -- u is a fixed path plus a url.Values.Encode() of allow-listed
	// keys with escaped values; no unescaped user markup reaches the attribute.
	return template.URL(u)
}

// atoiOr0 parses an int, returning 0 on any error (the "no floor" sentinel).
func atoiOr0(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// filterFindings applies the confidence-floor and issue-type facets to the
// worklist in Go (the book facet is applied at the DB query). The input order
// (confidence DESC) is preserved. Returns a new slice; the input is unmodified.
func filterFindings(in []db.FindingRow, f findingsFilter) []db.FindingRow {
	if !f.Active() {
		return in
	}
	floor := float64(f.MinConfidence) / 100
	out := in[:0:0]
	for _, fr := range in {
		if f.MinConfidence > 0 && fr.Confidence < floor {
			continue
		}
		if f.IssueType != "" && fr.IssueType != f.IssueType {
			continue
		}
		// BookDir is already applied by the scoped DB query, but re-check defensively
		// so an in-memory/test fixture that ignores the scope still filters.
		if f.BookDir != "" && fr.BookDir != f.BookDir {
			continue
		}
		out = append(out, fr)
	}
	return out
}

// groupFindings collapses exact-duplicate corrections — same original span, same
// suggested correction, same issue type — into one findingGroup carrying the
// occurrence count and every location. The judge frequently flags the identical
// fix across many tracks (e.g. a recurring misheard proper noun), so a 243-row
// list of mostly-identical rows becomes a handful of distinct corrections. Groups
// are ordered by max confidence DESC (triage worst-first), ties by count DESC.
// Input order (already confidence DESC) seeds the first-seen order within ties.
func groupFindings(in []db.FindingRow) []findingGroup {
	if len(in) == 0 {
		return nil
	}
	type key struct {
		orig, corr, issue string
	}
	idx := map[key]int{}
	groups := make([]findingGroup, 0, len(in))
	for _, fr := range in {
		corr := ""
		if fr.SuggestedCorrection != nil {
			corr = *fr.SuggestedCorrection
		}
		k := key{fr.OriginalText, corr, fr.IssueType}
		loc := findingLocation{FilePath: fr.FilePath, BookDir: fr.BookDir, JobID: fr.JobID, StartSec: fr.StartSec}
		if i, ok := idx[k]; ok {
			groups[i].Count++
			groups[i].Locations = append(groups[i].Locations, loc)
			if fr.Confidence > groups[i].Confidence {
				groups[i].Confidence = fr.Confidence
			}
			continue
		}
		idx[k] = len(groups)
		groups = append(groups, findingGroup{
			OriginalText:        fr.OriginalText,
			SuggestedCorrection: fr.SuggestedCorrection,
			IssueType:           fr.IssueType,
			Confidence:          fr.Confidence,
			Count:               1,
			Locations:           []findingLocation{loc},
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Confidence != groups[j].Confidence {
			return groups[i].Confidence > groups[j].Confidence
		}
		return groups[i].Count > groups[j].Count
	})
	return groups
}

// findingLocation is one occurrence of a deduplicated correction: the track it
// was found in and the timestamp, with the deep-jump link parts.
type findingLocation struct {
	FilePath string
	BookDir  string
	JobID    *string
	StartSec float64
}

// findingGroup is a set of identical corrections (same original → suggestion →
// issue type) collapsed into one worklist row. Count is the occurrence count;
// Locations holds every place it was found (revealed via "show all locations").
// Confidence is the max across the group (triage by the worst case).
type findingGroup struct {
	OriginalText        string
	SuggestedCorrection *string
	IssueType           string
	Confidence          float64 // max confidence across the group
	Count               int
	Locations           []findingLocation
}

// findingsWorklistLimit caps the global findings worklist on the /findings page.
// Raised from 200 since dedup collapses the visible row count dramatically — the
// fetch needs headroom to find all occurrences of a repeated correction.
const findingsWorklistLimit = 1000

// findingsPage is the page shell (layout + content) for GET /findings.
var findingsPage = mustPage(`{{define "content"}}
<p class="subtitle">Suspected transcription errors from the read-only LLM judge — advisory only, transcripts are never edited. Filter by confidence, issue type, or book. Auto-refreshes every 5&thinsp;s.</p>
<div id="action-error" aria-live="assertive"></div>
<div id="eval-notice" role="status" aria-live="polite"></div>
<div id="conn" class="conn-lost" role="status" aria-live="polite" hidden>&#9888;&#xFE0F;&nbsp;connection lost — data below may be stale</div>
<div id="findings-region"
     hx-get="/findings/data{{.DataQuery}}" hx-trigger="load, every 5s" hx-swap="innerHTML"
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
{{if or .EvalConfigured (and .ControlEnabled (gt .Summary.TotalFindings 0))}}
<div class="findings-actions">
  {{if .EvalConfigured}}
  <button class="btn btn-primary" hx-post="/actions/eval-sample?n={{.EvalSampleN}}" hx-target="#findings-region" hx-swap="afterbegin"
          hx-confirm="Run the read-only LLM judge over a {{.EvalSampleN}}-chunk library sample? It flags suspected transcription errors (advisory only — transcripts are never edited) and may take a minute.">run sample eval ({{.EvalSampleN}} chunks)</button>
  {{end}}
  {{if and .ControlEnabled (gt .Summary.TotalFindings 0)}}
  <button class="btn btn-danger" hx-post="/actions/findings-clear" hx-target="#findings-region" hx-swap="innerHTML"
          hx-confirm="Delete all {{.Summary.TotalFindings}} recorded findings? Advisory data only — transcripts are untouched and findings can be regenerated by re-running eval.">clear findings</button>
  {{end}}
</div>
{{end}}
{{if eq .Summary.TotalFindings 0}}
{{if .EvalConfigured}}
<p class="lib-empty">No findings recorded yet. The eval layer is read-only and on-demand — use <strong>run sample eval</strong> above (or the per-book <strong>run eval</strong> on a book page) to record suspected transcription errors (advisory only; transcripts are never edited).</p>
{{else}}
<p class="lib-empty">Eval endpoint not configured. The LLM-as-judge needs a chat endpoint bound to the <code>eval</code> role — configure it on the <a href="/servers">Models &amp; Services</a> page (<code>AI_ROLES.eval</code> → a <code>chat</code> endpoint), then the <strong>run sample eval</strong> trigger appears here.</p>
{{end}}
{{else}}
{{$f := .Filter}}
<div class="card-group grid">
  <div class="card"><div class="card-label">Findings</div><div class="card-value">{{commafy .Summary.TotalFindings}}</div></div>
  <div class="card"><div class="card-label">Mean confidence</div><div class="card-value blue">{{confPct .Summary.MeanConfidence}}</div></div>
  <a class="card card-click" href="/findings?conf=80" title="filter the worklist to high-confidence findings"><div class="card-label">High &ge;0.8</div><div class="card-value red">{{commafy .Summary.HighConfidence}}</div></a>
  <a class="card card-click" href="/findings?conf=40" title="filter the worklist to medium-and-up findings"><div class="card-label">Medium 0.4–0.8</div><div class="card-value amber">{{commafy .Summary.MediumConfidence}}</div></a>
  <div class="card"><div class="card-label">Low &lt;0.4</div><div class="card-value">{{commafy .Summary.LowConfidence}}</div></div>
</div>

<div class="lib-bar findings-facets">
  <div class="lib-chips">
    <span class="lib-sort-label">confidence&ge;</span>
    <a class="chip{{if eq $f.MinConfidence 0}} active{{end}}"  href="{{worklistURL $f "conf" "0"}}">any</a>
    <a class="chip{{if eq $f.MinConfidence 40}} active{{end}}" href="{{worklistURL $f "conf" "40"}}">40%</a>
    <a class="chip{{if eq $f.MinConfidence 60}} active{{end}}" href="{{worklistURL $f "conf" "60"}}">60%</a>
    <a class="chip{{if eq $f.MinConfidence 80}} active{{end}}" href="{{worklistURL $f "conf" "80"}}">80%</a>
  </div>
  {{if $f.Active}}
  <a class="lib-clear" href="/findings">clear filters</a>
  {{end}}
</div>

{{if $f.Active}}
<div class="active-facets">
  <span class="lib-sort-label">filtered:</span>
  {{if gt $f.MinConfidence 0}}<span class="facet-pill">confidence &ge; {{$f.MinConfidence}}% <a class="facet-x" href="{{worklistURL $f "conf" "0"}}" title="remove">&#10007;</a></span>{{end}}
  {{if $f.IssueType}}<span class="facet-pill">issue: {{$f.IssueType}} <a class="facet-x" href="{{worklistURL $f "issue" ""}}" title="remove">&#10007;</a></span>{{end}}
  {{if $f.BookDir}}<span class="facet-pill">book: {{$f.BookLabel}} <a class="facet-x" href="{{worklistURL $f "book" ""}}" title="remove">&#10007;</a></span>{{end}}
</div>
{{end}}

{{if .Summary.ByIssueType}}
<div class="section">
  <div class="section-title">Issue types — click to filter the worklist</div>
  <div class="table-wrap">
  <table>
    <caption>Suspected-error types across the library</caption>
    <thead><tr><th scope="col">Issue type</th><th scope="col" class="num">Count</th></tr></thead>
    <tbody>
    {{range .Summary.ByIssueType}}
      <tr class="row-link{{if eq $f.IssueType .IssueType}} facet-active{{end}}">
        <td><a class="row-a" href="{{worklistURL $f "issue" .IssueType}}" title="filter the worklist to {{.IssueType}}">{{.IssueType}}</a></td>
        <td class="num">{{commafy .Count}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>
{{end}}

{{if .Summary.ByBook}}
<div class="section">
  <div class="section-title">Per book — click to filter the worklist</div>
  <div class="table-wrap">
  <table>
    <caption>Findings grouped by book</caption>
    <thead><tr>
      <th scope="col">Book</th>
      <th scope="col" class="num" title="suspected errors recorded for this book">Findings</th>
      <th scope="col" class="num" title="mean self-scored confidence of this book's findings">Mean conf</th>
      <th scope="col" title="most common issue type for this book">Top issue</th>
      <th scope="col"><span class="sr-only">Open book</span></th>
    </tr></thead>
    <tbody>
    {{range .Summary.ByBook}}
      <tr class="row-link{{if eq $f.BookDir .BookDir}} facet-active{{end}}">
        <td><a class="row-a" href="{{worklistURL $f "book" .BookDir}}" title="filter the worklist to this book">{{shortName .BookDir}}</a></td>
        <td class="num">{{commafy .Count}}</td>
        <td class="num">{{confPctF .MeanConfidence}}</td>
        <td>{{.TopIssueType}}</td>
        <td class="actions"><a class="open-cue" href="/book?dir={{.BookDir}}" title="open the book page" aria-label="open book">&#8250;</a></td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>
{{end}}

<div class="section">
  <div class="section-title">Worklist{{if .TotalCount}} — showing {{commafy .MatchedCount}} of {{commafy .TotalCount}} finding{{if ne .TotalCount 1}}s{{end}}{{if lt (len .Groups) .MatchedCount}} in {{commafy (len .Groups)}} distinct correction{{if ne (len .Groups) 1}}s{{end}}{{end}}{{end}}</div>
  {{if .Groups}}
  <div class="table-wrap">
  <table>
    <caption>Suspected transcription errors — advisory only, transcripts are never edited</caption>
    <thead><tr>
      <th scope="col" class="num" title="judge self-scored confidence (triage highest-first; max across repeats)">Conf</th>
      <th scope="col">Issue</th>
      <th scope="col" title="suspected span → suggested correction (advisory only)">Correction</th>
      <th scope="col" title="how many times this exact correction was suggested">Count</th>
      <th scope="col" title="where it was found (first location; expand for all)">Where</th>
    </tr></thead>
    <tbody>
    {{range $gi, $g := .Groups}}
      {{$loc := index $g.Locations 0}}
      <tr>
        <td class="num">{{confPctF $g.Confidence}}</td>
        <td>{{$g.IssueType}}</td>
        <td>{{$g.OriginalText}} &#8594; {{strPtr $g.SuggestedCorrection}}</td>
        <td class="num">{{if gt $g.Count 1}}&times;{{$g.Count}}{{else}}1{{end}}</td>
        <td>
          {{if $g.Count | eq 1}}
            {{template "findingWhere" $loc}}
          {{else}}
            <details class="loc-details">
              <summary>{{$g.Count}} locations</summary>
              <div class="loc-list">
                {{range $g.Locations}}<div class="loc-row">{{template "findingWhere" .}}</div>{{end}}
              </div>
            </details>
          {{end}}
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}
  <p class="lib-empty">No findings match the active filters. <a href="/findings" style="color:var(--blue)">Clear filters</a> to see all {{commafy .TotalCount}}.</p>
  {{end}}
</div>
{{end}}

{{define "findingWhere"}}{{if .JobID}}<a class="file-name" href="/track?id={{derefStr .JobID}}&t={{.StartSec}}" title="open the transcript at this point">{{shortName .FilePath}}</a>{{else}}<span class="file-name" title="{{.FilePath}}">{{shortName .FilePath}}</span>{{end}} &#183; {{timestamp .StartSec}} &#183; <a class="file-name" href="/book?dir={{.BookDir}}" title="{{.BookDir}}">{{shortName .BookDir}}</a>{{end}}
`))

// handleFindingsPage serves the Findings page shell (GET /findings). It threads
// the validated facet params (conf/issue/book) into the auto-refreshing region's
// hx-get (DataQuery) so the 5s refresh re-fetches the SAME filtered view instead
// of wiping the filter the user just applied.
func (s *MCPServer) handleFindingsPage(w http.ResponseWriter, r *http.Request) {
	f := parseFindingsFilter(r)
	s.renderPage(w, r, findingsPage, pageShell{Title: "findings", Nav: "findings", DataQuery: findingsDataQuery(f)})
}

// handleFindingsData serves the htmx findings fragment (GET /findings/data).
func (s *MCPServer) handleFindingsData(w http.ResponseWriter, r *http.Request) {
	s.renderFindingsFragment(w, r)
}

// parseFindingsFilter reads the worklist facet params (conf/issue/book) off the
// request, validating the confidence floor to the chip set {0,40,60,80}. issue
// and book are passed through verbatim (matched exactly against the summary, so an
// unknown value simply matches nothing — no injection risk, html/template escapes
// on render).
func parseFindingsFilter(r *http.Request) findingsFilter {
	f := findingsFilter{
		IssueType: strings.TrimSpace(r.URL.Query().Get("issue")),
		BookDir:   strings.TrimSpace(r.URL.Query().Get("book")),
	}
	switch atoiOr0(r.URL.Query().Get("conf")) {
	case 80:
		f.MinConfidence = 80
	case 60:
		f.MinConfidence = 60
	case 40:
		f.MinConfidence = 40
	default:
		f.MinConfidence = 0
	}
	return f
}

// findingsDataQuery builds the "?conf=…&issue=…&book=…" suffix for the region's
// hx-get from an active filter (empty when no facet is set).
func findingsDataQuery(f findingsFilter) string {
	q := url.Values{}
	if f.MinConfidence > 0 {
		q.Set("conf", strconv.Itoa(f.MinConfidence))
	}
	if f.IssueType != "" {
		q.Set("issue", f.IssueType)
	}
	if f.BookDir != "" {
		q.Set("book", f.BookDir)
	}
	if enc := q.Encode(); enc != "" {
		return "?" + enc
	}
	return ""
}

// renderFindingsFragment fetches the current findings summary, applies the active
// facet filter to the worklist, dedups identical corrections into groups, and
// renders the findings fragment. Shared by the GET refresh path and the
// clear-findings action so both produce the same up-to-date view.
func (s *MCPServer) renderFindingsFragment(w http.ResponseWriter, r *http.Request) {
	summary, err := s.db.GetFindingsSummary(r.Context())
	if err != nil {
		s.logger.Error("GetFindingsSummary error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	filter := parseFindingsFilter(r)
	// Scope the DB worklist fetch to the active book when one is set (uses the
	// existing book-scoped query); confidence + issue-type are applied in Go so the
	// facets compose without new SQL.
	findings, err := s.db.ListFindings(r.Context(), filter.BookDir, findingsWorklistLimit)
	if err != nil {
		// The worklist rows are advisory (read-only eval output), not load-bearing
		// for this page. Match the book page: a ListFindings error degrades the
		// worklist to its empty state rather than failing the whole page.
		s.logger.Error("ListFindings error", "error", err)
		findings = nil
	}
	total := len(findings)
	matched := filterFindings(findings, filter)
	// Resolve a short label for the active book facet (for the chip).
	if filter.BookDir != "" {
		filter.BookLabel = path.Base(filter.BookDir)
	}

	data := findingsData{
		Summary:          summary,
		RenderedAt:       time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		EvalConfigured:   s.eval.configured,
		EvalSampleN:      defaultEvalSampleN,
		ControlEnabled:   s.controlToken != "",
		Groups:           groupFindings(matched),
		Filter:           filter,
		MatchedCount:     len(matched),
		TotalCount:       total,
		IssueTypeOptions: summary.ByIssueType,
		BookOptions:      summary.ByBook,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := findingsFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("findings fragment render error", "error", err)
	}
}

// handleFindingsClear deletes recorded findings (POST /actions/findings-clear)
// and re-renders the findings fragment. It mirrors the eval-action gate: htmx
// origin + a configured CONTROL_API_TOKEN (fail-closed). Unlike the eval
// actions it needs NO eval endpoint — clearing only deletes advisory rows.
// An optional ?dir= scopes the clear to one book; absent → clear all. The
// delete touches only transcript_findings, so the read-only-transcripts
// invariant (§2.15) still holds and findings can be regenerated by re-running
// eval.
func (s *MCPServer) handleFindingsClear(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.controlToken == "" {
		// Same fail-closed posture as the eval/control surfaces: an
		// unauthenticated deployment cannot mutate state.
		writeActionError(w, "clear is disabled: CONTROL_API_TOKEN is not configured")
		return
	}
	dir := r.URL.Query().Get("dir")
	n, err := s.db.ClearFindings(r.Context(), dir)
	if err != nil {
		s.logger.Error("clear findings error", "dir", dir, "error", err)
		writeActionError(w, "clear findings failed — see server logs")
		return
	}
	s.logger.Info("cleared findings via dashboard", "dir", dir, "deleted", n)
	// A dir-scoped clear is reachable from the Book page (its clear-book-findings
	// button targets #book-region), so re-render the book fragment in that case;
	// an unscoped clear comes from the /findings page and re-renders it.
	if dir != "" {
		s.renderBookFragment(w, r, dir)
		return
	}
	s.renderFindingsFragment(w, r)
}

// ─── On-demand eval-run action handlers (CONTRACT §2.15) ─────────────────────
//
// These mirror the requeue action handlers (dashboard.go): htmx-guarded via the
// HX-Request header, and additionally fail-closed (503) when CONTROL_API_TOKEN is
// unset — running the judge issues real (billable) LLM calls, so the trigger is
// gated like the other mutating control surfaces rather than left open. Unlike
// requeue, the work is NOT done synchronously: the LLM calls run in a background
// goroutine so the HTTP request returns immediately with an "evaluating…"
// indicator, and the page's auto-refresh surfaces findings as they land.

const (
	// defaultEvalSampleN is the library-sample size the /findings button requests.
	defaultEvalSampleN = 50
	// maxEvalSampleN is the hard ceiling on a sample run (cost bound) — a larger
	// requested n is clamped down to this.
	maxEvalSampleN = 200
	// perBookEvalLimit caps the chunks judged in a per-book run (cost bound).
	perBookEvalLimit = 200
	// evalRunTimeout bounds a background run so a hung endpoint can't pin the
	// in-flight flag forever (the judge's per-request client also has its own
	// 120s backstop).
	evalRunTimeout = 30 * time.Minute
)

// evalGate enforces the shared preconditions for the eval-run actions: htmx
// origin, a configured endpoint, and a configured control token (fail-closed).
// It returns false (after writing the response) when the request must not run.
func (s *MCPServer) evalGate(w http.ResponseWriter, r *http.Request) bool {
	if !isHTMX(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if s.controlToken == "" {
		// Same fail-closed posture as the JSON control API: an unauthenticated
		// deployment cannot trigger billable LLM runs.
		writeActionError(w, "eval is disabled: CONTROL_API_TOKEN is not configured")
		return false
	}
	if !s.eval.configured || s.eval.run == nil {
		writeActionError(w, "no eval chat endpoint configured — see Models & Services")
		return false
	}
	return true
}

// startEvalRun kicks the judge off in the background (its own context, generous
// timeout) and reports whether it started. It guards against overlapping runs:
// a second trigger while one is in flight returns started=false so the caller
// can say so rather than doubling LLM cost.
func (s *MCPServer) startEvalRun(opts eval.RunOptions, scope string) (started bool) {
	if !s.eval.inFlight.CompareAndSwap(false, true) {
		return false
	}
	run := s.eval.run
	go func() {
		defer s.eval.inFlight.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), evalRunTimeout)
		defer cancel()
		stats, err := run(ctx, opts)
		if err != nil {
			s.logger.Error("eval run failed", "scope", scope, "error", err)
			return
		}
		s.logger.Info("eval run complete", "scope", scope,
			"chunksEvaluated", stats.ChunksEvaluated, "chunksSkipped", stats.ChunksSkipped,
			"findings", stats.FindingsFound, "persisted", stats.Persisted)
	}()
	return true
}

// handleEvalBook runs the judge over one book's chunks (POST /actions/eval?dir=…)
// and re-renders the book fragment with an "evaluating…" notice.
func (s *MCPServer) handleEvalBook(w http.ResponseWriter, r *http.Request) {
	if !s.evalGate(w, r) {
		return
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "missing dir", http.StatusBadRequest)
		return
	}
	notice := ""
	if s.startEvalRun(eval.RunOptions{Book: dir, Limit: perBookEvalLimit, Write: true}, "book "+dir) {
		s.logger.Info("eval run started via dashboard", "scope", "book", "dir", dir)
		notice = "Evaluating up to " + itoa(perBookEvalLimit) + " chunk(s)… findings will appear on the Findings page as they land."
	} else {
		notice = "An eval run is already in flight — wait for it to finish before starting another."
	}
	s.renderBookFragmentWithNotice(w, r, dir, notice)
}

// handleEvalSample runs the judge over a library sample (POST /actions/eval-sample?n=N)
// and returns a small banner prepended to the findings region.
func (s *MCPServer) handleEvalSample(w http.ResponseWriter, r *http.Request) {
	if !s.evalGate(w, r) {
		return
	}
	n := clampSampleN(r.URL.Query().Get("n"))
	var notice string
	if s.startEvalRun(eval.RunOptions{Sample: n, Write: true}, "sample") {
		s.logger.Info("eval run started via dashboard", "scope", "sample", "n", n)
		notice = "Evaluating a " + itoa(n) + "-chunk sample… findings will appear below as the page refreshes."
	} else {
		notice = "An eval run is already in flight — wait for it to finish before starting another."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<div class="eval-notice" role="status">` + template.HTMLEscapeString(notice) + `</div>`))
}

// clampSampleN parses the requested sample size and clamps it to [1, maxEvalSampleN].
// A missing/invalid value falls back to the default.
func clampSampleN(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		n = defaultEvalSampleN
	}
	if n > maxEvalSampleN {
		n = maxEvalSampleN
	}
	return n
}

// itoa is strconv.Itoa under a short name for the notice strings above.
func itoa(n int) string { return strconv.Itoa(n) }
