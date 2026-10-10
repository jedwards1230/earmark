package mcp

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// Step runs (CONTRACT §1.10): the "Running now" panel at the top of the
// Pipeline and Models pages, the Decide/Scan/Judge role cards' link to their
// active run, and GET /api/v1/runs. Everything reads db.StepRuns — two
// index-backed LIMIT queries — under its own short deadline, so a slow
// database can never hold a page poll past its htmx timeout.

const (
	runsActiveLimit = 10
	runsRecentLimit = 10
	// runsQueryTimeout is the hard deadline on the runs read: well inside the
	// 5 s htmx timeout of the regions that poll it.
	runsQueryTimeout = 2 * time.Second
	// roleRunsTimeout bounds the role cards' read inside /servers/data, which
	// has other work to do in the same 5 s.
	roleRunsTimeout = time.Second
)

// runCount is one counter of a run, for display.
type runCount struct {
	Key string
	N   string
}

// runView is one step run as the panel renders it. Every string is final
// text; the template only lays it out.
type runView struct {
	ID          int64
	Step        string // the step token
	StepLabel   string // "decide revert"
	Mode        string // dry_run | write
	ModeLabel   string // "dry run" | "write"
	Status      string // running | stale | done | failed | cancelled
	StatusLabel string // "✓ done", "⚠ stale"…
	Stale       bool
	Model       string
	RecipeID    string
	RecipeShort string
	Args        string
	Host        string
	Phase       string
	Error       string
	// Progress: DoneText always; with a total, TotalText, Pct and the bar.
	DoneText  string
	TotalText string
	HasTotal  bool
	Pct       int
	Counts    []runCount
	Cost      string // "$1.2345", "" when none
	Elapsed   string
	ETA       string // "" unless running live with a total and some progress
	// HeartbeatAgo is the age of the last heartbeat ("7m ago").
	HeartbeatAgo string
	StartedAt    time.Time
	StartedAgo   string
}

// runsPanelData is the /runs/data fragment's model.
type runsPanelData struct {
	Active []runView
	Recent []runView
	Err    bool
}

// stepLabels names the recorded steps; an unknown token renders as itself
// with underscores spaced.
var stepLabels = map[string]string{
	"decide":        "decide",
	"decide_revert": "decide revert",
	"scan":          "scan",
	"eval_sample":   "eval sample",
	"eval_book":     "eval book",
	"eval_backfill": "eval backfill",
}

func stepLabel(step string) string {
	if l, ok := stepLabels[step]; ok {
		return l
	}
	return strings.ReplaceAll(step, "_", " ")
}

// runRoleKey maps a step to the role card that links to its active run.
func runRoleKey(step string) string {
	switch {
	case step == "decide" || step == "decide_revert":
		return "decide"
	case step == "scan":
		return "scan"
	case strings.HasPrefix(step, "eval"):
		return "judge"
	}
	return ""
}

// runStatus is the displayed status: a running row whose heartbeat stopped is
// "stale", whatever its stored status says.
func runStatus(r db.StepRun) string {
	if r.Stale(db.StepRunStaleAfter) {
		return "stale"
	}
	return r.Status
}

var runStatusLabels = map[string]string{
	"running":   "● running",
	"stale":     "⚠ stale",
	"done":      "✓ done",
	"failed":    "✗ failed",
	"cancelled": "■ cancelled",
}

// counterOrder puts the decision outcomes first, in pipeline order; any other
// counter follows alphabetically.
var counterOrder = map[string]int{
	"apply": 0, "hold": 1, "reject": 2, "reanchor": 3, "rung0_pass": 4,
	"scanned": 0, "evaluated": 0, "latched": 0, "moved": 0,
}

func sortedCounts(c map[string]int64) []runCount {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, iok := counterOrder[keys[i]]
		oj, jok := counterOrder[keys[j]]
		switch {
		case iok && jok && oi != oj:
			return oi < oj
		case iok != jok:
			return iok
		}
		return keys[i] < keys[j]
	})
	out := make([]runCount, 0, len(keys))
	for _, k := range keys {
		out = append(out, runCount{Key: strings.ReplaceAll(k, "_", " "), N: commafy64(c[k])})
	}
	return out
}

// buildRunView turns a stored row into display text.
func buildRunView(r db.StepRun) runView {
	status := runStatus(r)
	v := runView{
		ID: r.ID, Step: r.Step, StepLabel: stepLabel(r.Step), Mode: r.Mode, ModeLabel: "write",
		Status: status, StatusLabel: runStatusLabels[status], Stale: status == "stale",
		Model: r.Model, RecipeID: r.RecipeID, RecipeShort: shortID(r.RecipeID), Args: r.Args, Host: r.Host,
		Phase: r.Message, Error: r.Error, Counts: sortedCounts(r.Counters),
		StartedAt: r.StartedAt, StartedAgo: humanizeSince(r.ObservedAt.Sub(r.StartedAt)),
		HeartbeatAgo: humanizeSince(r.ObservedAt.Sub(r.HeartbeatAt)),
	}
	if r.Mode == db.StepRunDryRun {
		v.ModeLabel = "dry run"
	}
	if v.StatusLabel == "" {
		v.StatusLabel = status
	}
	var done int64
	if r.Done != nil {
		done = *r.Done
	}
	v.DoneText = commafy64(done)
	if r.Total != nil && *r.Total > 0 {
		total := *r.Total
		v.HasTotal, v.TotalText = true, commafy64(total)
		v.Pct = int(min(100, done*100/total))
	}
	if r.CostUSD != nil && *r.CostUSD > 0 {
		v.Cost = fmt.Sprintf("$%.4f", *r.CostUSD)
	}
	elapsed := r.Elapsed(db.StepRunStaleAfter)
	v.Elapsed = humanizeSeconds(elapsed.Seconds())
	if status == db.StepRunRunning && v.HasTotal && done > 0 && done < *r.Total {
		perItem := elapsed.Seconds() / float64(done)
		v.ETA = humanizeSeconds(perItem * float64(*r.Total-done))
	}
	return v
}

// stepRuns reads the runs list under its own deadline.
func (s *MCPServer) stepRuns(ctx context.Context, timeout time.Duration) (db.StepRunList, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.db.StepRuns(ctx, runsActiveLimit, runsRecentLimit)
}

func buildRunsPanel(l db.StepRunList) runsPanelData {
	var d runsPanelData
	for _, r := range l.Active {
		d.Active = append(d.Active, buildRunView(r))
	}
	for _, r := range l.Recent {
		d.Recent = append(d.Recent, buildRunView(r))
	}
	return d
}

// handleRunsData renders the "Running now" + "Recent runs" fragment. A failed
// or slow read renders 200 with "run status unavailable" — the rest of the
// page is unaffected, so it must not flag the connection as lost.
func (s *MCPServer) handleRunsData(w http.ResponseWriter, r *http.Request) {
	l, err := s.stepRuns(r.Context(), runsQueryTimeout)
	data := buildRunsPanel(l)
	if err != nil {
		s.logger.Warn("runs: step_runs read failed; panel shows unavailable", "error", err)
		data = runsPanelData{Err: true}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := runsFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("runs template error", "error", err)
	}
}

// roleRunLink is a role card's pointer to its step's active run.
type roleRunLink struct {
	ID    int64
	Text  string
	Stale bool
}

// attachRoleRuns links each role card to the newest active run of its steps.
// Best-effort: a failed read leaves the cards without links.
func (s *MCPServer) attachRoleRuns(ctx context.Context, roles []roleCard) {
	l, err := s.stepRuns(ctx, roleRunsTimeout)
	if err != nil {
		s.logger.Warn("models: step_runs read failed; role cards show no active run", "error", err)
		return
	}
	linkRoleRuns(roles, l.Active)
}

// linkRoleRuns sets ActiveRun on each card from active (newest first): a live
// run wins over a stale one.
func linkRoleRuns(roles []roleCard, active []db.StepRun) {
	for i := range roles {
		for _, r := range active {
			if runRoleKey(r.Step) != roles[i].Key {
				continue
			}
			v := buildRunView(r)
			text := v.StepLabel + " (" + v.ModeLabel + ") · " + v.DoneText
			if v.HasTotal {
				text += " / " + v.TotalText
			}
			if v.Stale {
				text += " · stale"
			} else if v.Phase != "" {
				text += " · " + v.Phase
			}
			link := &roleRunLink{ID: r.ID, Text: text, Stale: v.Stale}
			if roles[i].ActiveRun == nil || (roles[i].ActiveRun.Stale && !link.Stale) {
				roles[i].ActiveRun = link
			}
		}
	}
}

// runsFragmentTmpl renders the panel. Run cards reuse the role-card styles.
var runsFragmentTmpl = template.Must(template.New("runs").Parse(`
<section class="section runs-panel" aria-labelledby="runs-now-title">
  <h2 class="section-title" id="runs-now-title">Running now</h2>
  {{if .Err}}<p class="lib-empty err">run status unavailable — see server logs</p>
  {{else if not .Active}}<p class="lib-empty runs-none">Nothing running. <span class="time-muted">decide, scan and eval runs started from a shell or a CronJob appear here while they run.</span></p>
  {{else}}<div class="panels runs-active">
  {{range .Active}}
  <div class="panel server-card run-card {{if .Stale}}state-busy{{else}}state-running{{end}}" id="run-{{.ID}}">
    <div class="server-head">
      <span class="server-name"><span class="dot {{if .Stale}}amber{{else}}green{{end}}"></span>{{.StepLabel}}</span>
      <span class="badge run-mode mode-{{.Mode}}">{{.ModeLabel}}</span>
      <span class="badge run-status run-{{.Status}}">{{.StatusLabel}}</span>
    </div>
    <div class="server-sub{{if .Stale}} stale-note{{end}}">{{if .Stale}}last heartbeat {{.HeartbeatAgo}} — the process is gone (killed exec, OOM, evicted pod); it will not finish{{else if .Phase}}{{.Phase}}{{else}}running{{end}}</div>
    <div class="run-progress">{{if .HasTotal}}<span class="progress" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="{{.Pct}}" aria-label="{{.StepLabel}} progress"><span class="progress-bar" style="width:{{.Pct}}%"></span></span><span class="progress-text">{{.DoneText}} / {{.TotalText}} · {{.Pct}}%</span>{{else}}<span class="progress-text">{{.DoneText}} done · total unknown</span>{{end}}</div>
    <dl class="role-kv">
      <dt>model</dt><dd class="mono">{{or .Model "—"}}</dd>
      <dt>recipe</dt><dd class="mono" title="{{.RecipeID}}">{{or .RecipeShort "—"}}</dd>
      <dt>counts</dt><dd>{{range $i, $c := .Counts}}{{if $i}} · {{end}}{{$c.Key}} {{$c.N}}{{else}}—{{end}}</dd>
      <dt>elapsed</dt><dd>{{.Elapsed}}{{if .ETA}} · ETA {{.ETA}}{{end}}</dd>
      {{if .Cost}}<dt>spend</dt><dd class="mono">{{.Cost}}</dd>{{end}}
      <dt>where</dt><dd><span class="mono">{{or .Host "—"}}</span>{{with .Args}} <span class="time-muted">· {{.}}</span>{{end}}</dd>
    </dl>
  </div>
  {{end}}
  </div>{{end}}
</section>
<section class="section runs-recent" aria-labelledby="runs-recent-title">
  <h2 class="section-title" id="runs-recent-title">Recent runs</h2>
  {{if .Err}}<p class="lib-empty err">run status unavailable</p>
  {{else if .Recent}}<div class="table-wrap">
  <table aria-labelledby="runs-recent-title">
    <thead><tr>
      <th scope="col">Step</th><th scope="col">Mode</th><th scope="col">Outcome</th>
      <th scope="col">Counts</th><th scope="col" class="num">Cost</th>
      <th scope="col" class="num">Duration</th><th scope="col">Started</th>
    </tr></thead>
    <tbody>
    {{range .Recent}}
    <tr id="run-{{.ID}}">
      <th scope="row">{{.StepLabel}}{{with .Args}} <span class="time-muted">{{.}}</span>{{end}}</th>
      <td>{{.ModeLabel}}</td>
      <td><span class="badge run-status run-{{.Status}}"{{with .Error}} title="{{.}}"{{end}}>{{.StatusLabel}}</span></td>
      <td class="time-muted">{{.DoneText}}{{if .HasTotal}} / {{.TotalText}}{{end}}{{range .Counts}} · {{.Key}} {{.N}}{{end}}</td>
      <td class="num">{{or .Cost "—"}}</td>
      <td class="num">{{.Elapsed}}</td>
      <td class="time-muted" title="{{.StartedAt.UTC.Format "2006-01-02 15:04:05 UTC"}}">{{.StartedAgo}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p class="lib-empty">No runs recorded yet.</p>{{end}}
</section>
`))

// runsRegion is the polled region both pages embed above their own data.
const runsRegion = `<div id="runs-region" class="runs-region"
     hx-get="/runs/data" hx-trigger="load, every 3s" hx-swap="innerHTML"
     hx-sync="this:drop" hx-config='{"timeout": 5000}' hx-status:5xx="swap:none">
  <p class="htmx-indicator">loading runs…</p>
</div>`

// ─── JSON API (CONTRACT §2.12) ───────────────────────────────────────────────

// apiRun is one step run in GET /api/v1/runs and GET /api/v1/status runs.
type apiRun struct {
	ID          int64            `json:"id"`
	Step        string           `json:"step"`
	Mode        string           `json:"mode"`
	Status      string           `json:"status"`
	Stale       bool             `json:"stale"`
	RecipeID    *string          `json:"recipeId"`
	Model       *string          `json:"model"`
	Args        *string          `json:"args"`
	Host        *string          `json:"host"`
	StartedAt   string           `json:"startedAt"`
	HeartbeatAt string           `json:"heartbeatAt"`
	FinishedAt  *string          `json:"finishedAt"`
	Total       *int64           `json:"total"`
	Done        *int64           `json:"done"`
	Progress    *float64         `json:"progress"`
	ElapsedSecs float64          `json:"elapsedSeconds"`
	ETASecs     *float64         `json:"etaSeconds"`
	Counters    map[string]int64 `json:"counters"`
	CostUSD     *float64         `json:"costUsd"`
	Message     *string          `json:"message"`
	Error       *string          `json:"error"`
}

// apiRuns is the body of GET /api/v1/runs and the status runs field.
type apiRuns struct {
	Active []apiRun `json:"active"`
	Recent []apiRun `json:"recent"`
	// StaleAfterSeconds is the heartbeat age past which a running run is
	// reported stale.
	StaleAfterSeconds int `json:"staleAfterSeconds"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func apiRunFrom(r db.StepRun) apiRun {
	status := runStatus(r)
	a := apiRun{
		ID: r.ID, Step: r.Step, Mode: r.Mode, Status: status, Stale: status == "stale",
		RecipeID: optStr(r.RecipeID), Model: optStr(r.Model), Args: optStr(r.Args), Host: optStr(r.Host),
		StartedAt: r.StartedAt.UTC().Format(time.RFC3339), HeartbeatAt: r.HeartbeatAt.UTC().Format(time.RFC3339),
		Total: r.Total, Done: r.Done, Counters: r.Counters, CostUSD: r.CostUSD,
		Message: optStr(r.Message), Error: optStr(r.Error),
	}
	if a.Counters == nil {
		a.Counters = map[string]int64{}
	}
	if r.FinishedAt != nil {
		f := r.FinishedAt.UTC().Format(time.RFC3339)
		a.FinishedAt = &f
	}
	elapsed := r.Elapsed(db.StepRunStaleAfter).Seconds()
	a.ElapsedSecs = elapsed
	if r.Total != nil && *r.Total > 0 {
		var done int64
		if r.Done != nil {
			done = *r.Done
		}
		p := min(1, float64(done)/float64(*r.Total))
		a.Progress = &p
		if status == db.StepRunRunning && done > 0 && done < *r.Total {
			eta := elapsed / float64(done) * float64(*r.Total-done)
			a.ETASecs = &eta
		}
	}
	return a
}

func apiRunsFrom(l db.StepRunList) apiRuns {
	out := apiRuns{Active: []apiRun{}, Recent: []apiRun{}, StaleAfterSeconds: int(db.StepRunStaleAfter / time.Second)}
	for _, r := range l.Active {
		out.Active = append(out.Active, apiRunFrom(r))
	}
	for _, r := range l.Recent {
		out.Recent = append(out.Recent, apiRunFrom(r))
	}
	return out
}

// handleAPIRuns serves GET /api/v1/runs[?recent=N]: the open runs and the
// newest N (default 10, max 100) others. 500 when the read fails.
func (s *MCPServer) handleAPIRuns(w http.ResponseWriter, r *http.Request) {
	recent := runsRecentLimit
	if v := r.URL.Query().Get("recent"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > db.MaxStepRunList {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("recent must be 1..%d", db.MaxStepRunList))
			return
		}
		recent = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), runsQueryTimeout)
	defer cancel()
	l, err := s.db.StepRuns(ctx, db.MaxStepRunList, recent)
	if err != nil {
		s.logger.Error("api runs error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, apiRunsFrom(l))
}
