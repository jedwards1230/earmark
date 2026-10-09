package decide

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// ClassReanchor is the report's fourth outcome class: a rung-0 chunk_changed
// or anchor_missing is not a judgement on the correction but a finding whose
// anchor no longer resolves — `earmark reanchor`'s job, not a reject.
const ClassReanchor = "reanchor"

// reportClasses is the order outcome classes are printed in.
var reportClasses = []string{DecisionApply, DecisionHold, DecisionReject, ClassReanchor}

// pBuckets is the number of equal-width p histogram buckets over [0, 1].
const pBuckets = 10

// Report is a dry run's result (the --json output). Every count is an
// aggregate: no finding ids or text.
type Report struct {
	DryRun    bool   `json:"dry_run"`
	Calibrate bool   `json:"calibrate"`
	RecipeID  string `json:"recipe_id"`
	Model     string `json:"model"`
	Seed      string `json:"seed"`
	// Requested is --sample; Findings is how many the scope held (≤ Requested).
	Requested int `json:"requested"`
	Findings  int `json:"findings"`

	// Outcomes counts apply / hold / reject / reanchor.
	Outcomes map[string]int `json:"outcomes"`
	// Cells is outcome × issue_type × evidence.
	Cells []Cell `json:"cells"`
	// Reasons counts the decision rule's reasons (holds and model rejects).
	Reasons map[string]int `json:"reasons"`
	// Rung0Rejects counts rung-0 reject reasons; Reanchor the reasons that
	// mean "re-anchor needed" (chunk_changed, anchor_missing).
	Rung0Rejects map[string]int `json:"rung0_rejects"`
	Reanchor     map[string]int `json:"reanchor"`
	// PHistogram counts answered p in ten buckets [0,0.1) … [0.9,1.0].
	PHistogram []int    `json:"p_histogram"`
	Jev        JevStats `json:"jev"`
	// Projection scales the sample's rates to the whole scope.
	Projection Projection `json:"projection"`
	// Calibration is set by --calibrate.
	Calibration *Calibration `json:"calibration,omitempty"`
}

// Cell is one outcome × issue_type × evidence count.
type Cell struct {
	Outcome   string `json:"outcome"`
	IssueType string `json:"issue_type"`
	Evidence  string `json:"evidence"`
	Count     int    `json:"count"`
}

// JevStats describes the model calls.
type JevStats struct {
	// Asked: rung 0 passed and the model was asked. Answered: a usable answer
	// came back (from the model or the cache). Unavailable: jev_unavailable.
	Asked       int `json:"asked"`
	Answered    int `json:"answered"`
	Unavailable int `json:"unavailable"`
	CacheHits   int `json:"cache_hits"`
	// Errors counts jev_unavailable by bounded class.
	Errors map[string]int `json:"errors"`
	// Latency percentiles over calls that went to the model (cache hits
	// excluded), in milliseconds; nil with no such call.
	LatencyP50MS *int64  `json:"latency_p50_ms,omitempty"`
	LatencyP95MS *int64  `json:"latency_p95_ms,omitempty"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// Projection is the sample scaled to the scope's population, per issue type.
type Projection struct {
	// Backlog is the scope's size; ByIssue its per-issue-type counts.
	Backlog int            `json:"backlog"`
	ByIssue map[string]int `json:"by_issue"`
	// Outcomes is the projected count per outcome class (rounded).
	Outcomes map[string]int `json:"outcomes"`
	// Unprojected counts backlog findings of issue types the sample has none
	// of (no rate to scale).
	Unprojected int `json:"unprojected"`
	// Asked is the projected model calls; CostUSD their projected cost at the
	// sample's mean cost per uncached call (nil when no call was uncached).
	Asked   int      `json:"asked"`
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// classOf is the report class of an outcome.
func classOf(o Outcome) string {
	if o.Decision == DecisionReject && (o.Reason == ReasonChunkChanged || o.Reason == ReasonAnchorMissing) {
		return ClassReanchor
	}
	return o.Decision
}

func buildReport(o RunOptions, recipeID string, items []*item, backlog map[string]int) *Report {
	rep := &Report{
		DryRun: true, Calibrate: o.Scope.Calibrate, RecipeID: recipeID, Model: ShouldApplyModel,
		Seed: o.Scope.Seed, Requested: o.Scope.Sample, Findings: len(items),
		Outcomes: map[string]int{}, Reasons: map[string]int{}, Rung0Rejects: map[string]int{},
		Reanchor: map[string]int{}, PHistogram: make([]int, pBuckets),
		Jev: JevStats{Errors: map[string]int{}},
	}
	cells := map[Cell]int{}
	type issueTally struct {
		n, asked int
		classes  map[string]int
	}
	byIssue := map[string]*issueTally{}
	var latencies []time.Duration
	uncached := 0

	for _, it := range items {
		out := it.outcome
		class := classOf(out)
		rep.Outcomes[class]++
		cells[Cell{Outcome: class, IssueType: it.finding.IssueType, Evidence: out.Evidence}]++
		t := byIssue[it.finding.IssueType]
		if t == nil {
			t = &issueTally{classes: map[string]int{}}
			byIssue[it.finding.IssueType] = t
		}
		t.n++
		t.classes[class]++
		switch {
		case class == ClassReanchor:
			rep.Reanchor[out.Reason]++
		case !out.Rung0.Pass:
			rep.Rung0Rejects[out.Reason]++
		default:
			rep.Reasons[out.Reason]++
		}
		if !out.Asked {
			continue
		}
		t.asked++
		rep.Jev.Asked++
		if out.CacheHit {
			rep.Jev.CacheHits++
		} else if out.Latency > 0 {
			latencies = append(latencies, out.Latency)
		}
		if out.P != nil {
			rep.Jev.Answered++
			rep.PHistogram[min(int(*out.P*pBuckets), pBuckets-1)]++
		}
		if out.Reason == ReasonJevUnavailable {
			rep.Jev.Unavailable++
			rep.Jev.Errors[out.ErrorClass]++
		}
		if !out.CacheHit && out.ErrorClass == "" {
			uncached++
		}
		rep.Jev.InputTokens += out.InputTokens
		rep.Jev.OutputTokens += out.OutputTokens
		rep.Jev.CostUSD += out.CostUSD
	}
	for c, n := range cells {
		c.Count = n
		rep.Cells = append(rep.Cells, c)
	}
	sort.Slice(rep.Cells, func(i, j int) bool {
		a, b := rep.Cells[i], rep.Cells[j]
		if a.Outcome != b.Outcome {
			return classRank(a.Outcome) < classRank(b.Outcome)
		}
		if a.IssueType != b.IssueType {
			return a.IssueType < b.IssueType
		}
		return a.Evidence < b.Evidence
	})
	rep.Jev.LatencyP50MS = percentileMS(latencies, 0.50)
	rep.Jev.LatencyP95MS = percentileMS(latencies, 0.95)

	proj := Projection{ByIssue: backlog, Outcomes: map[string]int{}}
	projected := map[string]float64{}
	var asked float64
	for issue, n := range backlog {
		proj.Backlog += n
		t := byIssue[issue]
		if t == nil {
			proj.Unprojected += n
			continue
		}
		for class, k := range t.classes {
			projected[class] += float64(n) * float64(k) / float64(t.n)
		}
		asked += float64(n) * float64(t.asked) / float64(t.n)
	}
	for class, v := range projected {
		proj.Outcomes[class] = int(math.Round(v))
	}
	proj.Asked = int(math.Round(asked))
	if uncached > 0 {
		c := asked * rep.Jev.CostUSD / float64(uncached)
		proj.CostUSD = &c
	}
	rep.Projection = proj
	return rep
}

func classRank(c string) int {
	for i, k := range reportClasses {
		if k == c {
			return i
		}
	}
	return len(reportClasses)
}

// percentileMS is the nearest-rank percentile of ds in milliseconds.
func percentileMS(ds []time.Duration, q float64) *int64 {
	if len(ds) == 0 {
		return nil
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Ceil(q*float64(len(s)))) - 1
	ms := s[max(0, min(i, len(s)-1))].Milliseconds()
	return &ms
}

// Print writes the report as text, or as indented JSON.
func (r *Report) Print(w io.Writer, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	mode := "dry run"
	if r.Calibrate {
		mode = "calibration (human-decided findings)"
	}
	p("decide %s · recipe %s · model %s · seed %q\n", mode, short(r.RecipeID), r.Model, r.Seed)
	p("sampled %d of %d requested · scope %d\n", r.Findings, r.Requested, r.Projection.Backlog)
	if r.Findings == 0 {
		p("\nNo findings in scope.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	p("\noutcomes: %s\n", counts(r.Outcomes, reportClasses))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "outcome\tissue_type\tevidence\tcount")
	for _, c := range r.Cells {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", c.Outcome, c.IssueType, c.Evidence, c.Count)
	}
	_ = tw.Flush()

	p("\ndecision reasons: %s\n", counts(r.Reasons, sortedKeys(r.Reasons)))
	p("rung-0 rejects:   %s\n", counts(r.Rung0Rejects, sortedKeys(r.Rung0Rejects)))
	p("reanchor needed:  %s\n", counts(r.Reanchor, sortedKeys(r.Reanchor)))

	p("\np histogram (answered %d):\n", r.Jev.Answered)
	for i, n := range r.PHistogram {
		hi := "1.0]"
		if i < pBuckets-1 {
			hi = fmt.Sprintf("%.1f)", float64(i+1)/pBuckets)
		}
		p("  [%.1f,%s %5d", float64(i)/pBuckets, hi, n)
		if w := bar(n, r.PHistogram); w > 0 {
			p(" %s", strings.Repeat("#", w))
		}
		p("\n")
	}

	j := r.Jev
	p("\njev: asked %d · answered %d · unavailable %d · cache hits %d\n", j.Asked, j.Answered, j.Unavailable, j.CacheHits)
	if len(j.Errors) > 0 {
		p("jev errors: %s\n", counts(j.Errors, sortedKeys(j.Errors)))
	}
	if j.LatencyP50MS != nil {
		p("latency: p50 %dms · p95 %dms (uncached calls)\n", *j.LatencyP50MS, *j.LatencyP95MS)
	}
	p("tokens: %d in · %d out · cost $%.6f\n", j.InputTokens, j.OutputTokens, j.CostUSD)

	pr := r.Projection
	p("\nprojection to the scope (%d findings): %s\n", pr.Backlog, counts(pr.Outcomes, reportClasses))
	if pr.Unprojected > 0 {
		p("  %d findings of issue types not in the sample are not projected\n", pr.Unprojected)
	}
	if pr.CostUSD != nil {
		p("  ~%d model calls · ~$%.2f (cache hits are free)\n", pr.Asked, *pr.CostUSD)
	} else {
		p("  ~%d model calls · cost unknown (no uncached call in the sample)\n", pr.Asked)
	}

	if c := r.Calibration; c != nil {
		c.print(p)
	}
	p("\n(dry run) nothing decided: no finding_events, no state changes. Model answers are cached in fn_calls.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func counts(m map[string]int, keys []string) string {
	if len(keys) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// bar scales n to at most 40 characters against the largest bucket.
func bar(n int, all []int) int {
	top := 0
	for _, v := range all {
		top = max(top, v)
	}
	if top == 0 {
		return 0
	}
	return int(math.Round(40 * float64(n) / float64(top)))
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
