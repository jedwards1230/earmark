package mcp

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// The Models page's Track record (CONTRACT §2.14 "Models page"): per model
// alias, the judge passes it ran and what it decided, read straight from
// judge_passes and finding_events (db.ModelTrackRecord). It is its own region
// below the polled Models fragment, fetched like the "Running now" panel is —
// stateless on the server, no snapshot cache — but every 60 s rather than 3 s:
// the decisions aggregate scans the whole decision log (≈0.7 s at ≈100k
// events), so the poll interval, not a server-side cache, keeps its cost down.

const (
	// trackRecordQueryTimeout is the deadline on the whole read, and the bound
	// that actually ends a slow one: the read runs two statements, each under
	// db.TrackRecordStatementTimeout, so the server alone could spend about
	// twice that. It sits inside the region's 6 s htmx timeout, so a read that
	// outgrows it renders "unavailable" instead of timing the request out.
	trackRecordQueryTimeout = db.TrackRecordStatementTimeout + time.Second

	// noAliasLabel names the row of recipes (and passes) that carry no model alias.
	noAliasLabel = "(no model alias)"
)

// trackRow is one model alias as the table renders it. Counts are final text;
// a side the alias never worked on is "—" and its Has flag is false.
type trackRow struct {
	Alias     string
	NoAlias   bool
	HasPasses bool
	Passes    string
	// PassesNote is the title of the total: what it includes.
	PassesNote string
	Complete   string
	Incomplete string
	// IncompleteNote is the title of Incomplete: its partial / failed split.
	IncompleteNote string
	Legacy         string
	HasDecisions   bool
	Apply          string
	Hold           string
	Reject         string
	Duplicate      string
	Undone         string
	Last           time.Time
}

// trackRecordData is the /servers/track-record fragment's model.
type trackRecordData struct {
	Rows []trackRow
	Err  bool
}

// buildTrackRows turns the aggregate into display rows, in the order read.
func buildTrackRows(ms []db.ModelTrack) []trackRow {
	rows := make([]trackRow, 0, len(ms))
	for _, m := range ms {
		r := trackRow{
			Alias: m.ModelAlias, NoAlias: m.ModelAlias == "",
			HasPasses: m.HasPasses(), HasDecisions: m.HasDecisions(), Last: m.Last,
			Passes: "—", Complete: "—", Incomplete: "—", Legacy: "—",
			Apply: "—", Hold: "—", Reject: "—", Duplicate: "—", Undone: "—",
		}
		if r.NoAlias {
			r.Alias = noAliasLabel
		}
		if r.HasPasses {
			r.Passes = commafy64(m.PassTotal())
			r.PassesNote = "all judge passes"
			if m.PassesRunning > 0 {
				r.PassesNote += fmt.Sprintf(", including %s still running", commafy64(m.PassesRunning))
			}
			r.Complete = commafy64(m.PassesComplete)
			r.Incomplete = commafy64(m.PassesPartial + m.PassesFailed)
			r.IncompleteNote = fmt.Sprintf("%s partial · %s failed", commafy64(m.PassesPartial), commafy64(m.PassesFailed))
			r.Legacy = commafy64(m.PassesLegacy)
		}
		if r.HasDecisions {
			r.Apply, r.Hold = commafy64(m.Apply), commafy64(m.Hold)
			r.Reject, r.Duplicate = commafy64(m.Reject), commafy64(m.Duplicate)
			r.Undone = commafy64(m.Undone)
		}
		rows = append(rows, r)
	}
	return rows
}

// handleTrackRecordData renders the Track record fragment. A failed or slow
// read renders 200 "track record unavailable" — the rest of the page is
// unaffected, so it must not flag the connection as lost.
func (s *MCPServer) handleTrackRecordData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), trackRecordQueryTimeout)
	defer cancel()
	ms, err := s.db.ModelTrackRecord(ctx)
	data := trackRecordData{Rows: buildTrackRows(ms)}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // the client went away
		}
		s.logger.Warn("models: track record read failed; section shows unavailable", "error", err)
		data = trackRecordData{Err: true}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := trackRecordFragmentTmpl.Execute(w, data); err != nil {
		s.logger.Error("track record template error", "error", err)
	}
}

// trackRecordRegion is the polled region the Models page embeds below its
// runner-update form. hx-sync drop: a slow read is not stacked on itself.
//
// The handler answers a failed read with a 200 fragment, so the only failures
// the client sees are transport ones: an HTTP error from a proxy
// (htmx:response:error; hx-status:5xx keeps its body out of the swap) or a
// refused, reset or timed-out request (htmx:error, incl. the 6 s timeout). On
// either, the initial placeholder swaps its "loading" line for the
// "unavailable" one — the same words as the handler's own — so a first load
// that never lands does not read "loading" forever. A poll that fails after a
// table has rendered leaves that table up (no placeholder left to swap), and
// the next successful poll replaces whatever is shown.
const trackRecordRegion = `<div id="track-record-region" class="track-record-region"
     hx-get="/servers/track-record" hx-trigger="load, every 60s" hx-swap="innerHTML"
     hx-sync="this:drop" hx-config='{"timeout": 6000}' hx-status:5xx="swap:none"
     hx-on::response:error="this.querySelector('[data-unavailable]')?.removeAttribute('hidden'); this.querySelector('[data-loading]')?.setAttribute('hidden', '')"
     hx-on::error="this.querySelector('[data-unavailable]')?.removeAttribute('hidden'); this.querySelector('[data-loading]')?.setAttribute('hidden', '')">
  <section class="section" aria-labelledby="track-record-title">
    <h2 class="section-title" id="track-record-title">Track record</h2>
    <p class="htmx-indicator" data-loading>loading track record…</p>
    <p class="lib-empty err" data-unavailable hidden>` + trackRecordUnavailable + `</p>
  </section>
</div>`

// trackRecordUnavailable is the section's text when it cannot be read.
const trackRecordUnavailable = "track record unavailable — see server logs"

var trackRecordFragmentTmpl = template.Must(template.New("track-record").Funcs(tmplFuncs).Parse(`
<section class="section" aria-labelledby="track-record-title" id="track-record">
  <h2 class="section-title" id="track-record-title">Track record</h2>
  {{if .Err}}<p class="lib-empty err">` + trackRecordUnavailable + `</p>
  {{else if not .Rows}}<p class="lib-empty">no judge passes or decisions recorded yet</p>
  {{else}}
  <p class="server-note table-lead" id="track-record-note">Per model alias, from the judge-pass and decision logs. <em>Judge passes</em>: every pass its propose recipes ran, and how they ended. <em>Decisions</em>: each finding counted once, at the latest decision a recipe of that alias made that has not since been undone; findings a requeue archived are left out. <em>Undone</em> counts decisions since revoked (<code>earmark decide revert</code>). A model that only judges or only decides shows — on the other side.</p>
  <div class="table-wrap">
  <table aria-labelledby="track-record-note">
    <thead>
    <tr>
      <th scope="col" rowspan="2">Model</th>
      <th scope="colgroup" colspan="4" class="group">Judge passes</th>
      <th scope="colgroup" colspan="5" class="group">Decisions</th>
      <th scope="col" rowspan="2" class="hide-sm">Last active</th>
    </tr>
    <tr>
      <th scope="col" class="num">Passes</th>
      <th scope="col" class="num">Complete</th>
      <th scope="col" class="num" title="passes that ended partial or failed">Partial / failed</th>
      <th scope="col" class="num" title="passes backfilled before passes were recorded">Legacy</th>
      <th scope="col" class="num">Apply</th>
      <th scope="col" class="num">Hold</th>
      <th scope="col" class="num">Reject</th>
      <th scope="col" class="num" title="agrees with another finding's fix, which carries the decision">Duplicate</th>
      <th scope="col" class="num" title="decisions since revoked">Undone</th>
    </tr>
    </thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <th scope="row" class="mono">{{if .NoAlias}}<span class="time-muted">{{.Alias}}</span>{{else}}{{.Alias}}{{end}}</th>
      <td class="num"{{if .HasPasses}} title="{{.PassesNote}}"{{end}}>{{if .HasPasses}}{{.Passes}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasPasses}}{{.Complete}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num"{{if .HasPasses}} title="{{.IncompleteNote}}"{{end}}>{{if .HasPasses}}{{.Incomplete}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasPasses}}{{.Legacy}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasDecisions}}{{.Apply}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasDecisions}}{{.Hold}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasDecisions}}{{.Reject}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasDecisions}}{{.Duplicate}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="num">{{if .HasDecisions}}{{.Undone}}{{else}}<span class="time-muted">—</span>{{end}}</td>
      <td class="time-muted hide-sm"{{if not .Last.IsZero}} title="{{formatTime .Last}}"{{end}}>{{if .Last.IsZero}}—{{else}}{{relTime .Last}}{{end}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{end}}
</section>
`))
