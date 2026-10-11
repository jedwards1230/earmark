package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/labels"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Scorecards is the slice of the DB `models report` needs (*db.DB implements it).
type Scorecards interface {
	RecipeScorecard(ctx context.Context, since time.Time) ([]db.ProposeScore, error)
	DecideScorecard(ctx context.Context, since time.Time) ([]db.DecideScore, error)
}

// ReportOptions are the `models report` flags.
type ReportOptions struct {
	Since time.Time
	// Step is "" (both), recipe.StepPropose or recipe.StepDecide.
	Step string
	JSON bool
}

var (
	reportSince string
	reportStep  string
)

// naLegacy marks a number a legacy (pre-judge-pass) recipe never recorded.
const naLegacy = "n/a (legacy)"

var reportCmd = &cobra.Command{
	Use:   "report [--since <date>] [--step propose|decide] [--json]",
	Short: "Track record per recipe: how each propose and decide model performed",
	Long: `report prints the model scorecards (CONTRACT §2.15 "Labels and scorecards"):
the recipe_scorecard and decide_scorecard views, one block per recipe, oldest
first.

Propose recipes: coverage (judge passes: transcripts and chunks judged,
cached share), findings per 1k chunks judged, issue-type and state mix,
pre-filter drops, parse failures, the outcome of each decide recipe's latest
live decision on the recipe's findings, revert rate, agreement (share of its
fixes another recipe configuration also proposed), labelled precision
overall and for misheard_proper_noun (good / (good + junk + partial)), cost
and latency per chunk.

Decide recipes: outcome × evidence, reasons, labelled precision per outcome,
p calibration against labels and against human (mcp:) decisions, the flip
rate against the previous decide recipe, and cost/latency from fn_calls.

Recipes from before judge passes were recorded (gemma, haiku, qwen eras)
show "n/a (legacy)" for what they never recorded — per-chunk cost and
latency, chunk counts, drop counts, parse failures — never 0.

--since keeps recipes with activity (a finding, a pass, a decision or a
call) at or after the date.

Examples:
  earmark models report
  earmark models report --since 2026-10-01 --step decide
  earmark models report --json > scorecard.json`,
	Args: cobra.NoArgs,
	Run:  runReport,
}

func init() {
	reportCmd.Flags().StringVar(&reportSince, "since", "", "only recipes active at or after this time (RFC 3339 or YYYY-MM-DD)")
	reportCmd.Flags().StringVar(&reportStep, "step", "", "propose or decide (default: both)")
	ModelsCmd.AddCommand(reportCmd)
}

// ParseReportOptions validates the report flags.
func ParseReportOptions(since, step string, asJSON bool) (ReportOptions, error) {
	o := ReportOptions{JSON: asJSON}
	if since != "" {
		t, err := labels.ParseTime(since)
		if err != nil {
			return o, fmt.Errorf("--since: %w", err)
		}
		o.Since = t
	}
	switch s := strings.ToLower(strings.TrimSpace(step)); s {
	case "":
	case recipe.StepPropose, "eval", "judge":
		o.Step = recipe.StepPropose
	case recipe.StepDecide:
		o.Step = recipe.StepDecide
	default:
		return o, fmt.Errorf("--step %q: want propose or decide", step)
	}
	return o, nil
}

func runReport(cmd *cobra.Command, _ []string) {
	o, err := ParseReportOptions(reportSince, reportStep, jsonOut)
	exitOn(err, 2)
	_, database := open()
	defer database.Close()
	exitOn(Report(cmd.Context(), os.Stdout, database, o), 1)
}

// ScorecardReport is the --json shape.
type ScorecardReport struct {
	Since   *time.Time        `json:"since,omitempty"`
	Propose []db.ProposeScore `json:"propose,omitempty"`
	Decide  []db.DecideScore  `json:"decide,omitempty"`
}

// Report reads the scorecards and prints them.
func Report(ctx context.Context, out io.Writer, store Scorecards, o ReportOptions) error {
	var r ScorecardReport
	if !o.Since.IsZero() {
		s := o.Since
		r.Since = &s
	}
	var err error
	if o.Step != recipe.StepDecide {
		if r.Propose, err = store.RecipeScorecard(ctx, o.Since); err != nil {
			return err
		}
		if r.Propose == nil {
			r.Propose = []db.ProposeScore{}
		}
	}
	if o.Step != recipe.StepPropose {
		if r.Decide, err = store.DecideScorecard(ctx, o.Since); err != nil {
			return err
		}
		if r.Decide == nil {
			r.Decide = []db.DecideScore{}
		}
	}
	if o.JSON {
		return writeJSONTo(out, r)
	}
	return RenderReport(out, r, o.Step)
}

func writeJSONTo(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// RenderReport prints the scorecards as text.
func RenderReport(out io.Writer, r ScorecardReport, step string) error {
	if r.Since != nil {
		_, _ = fmt.Fprintf(out, "recipes active since %s\n\n", r.Since.UTC().Format(time.DateOnly))
	}
	if step != recipe.StepDecide {
		if err := renderPropose(out, r.Propose); err != nil {
			return err
		}
	}
	if step == "" {
		_, _ = fmt.Fprintln(out)
	}
	if step != recipe.StepPropose {
		if err := renderDecide(out, r.Decide); err != nil {
			return err
		}
	}
	return nil
}

func renderPropose(out io.Writer, rows []db.ProposeScore) error {
	_, _ = fmt.Fprintf(out, "PROPOSE RECIPES (%d)\n", len(rows))
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "  no propose recipe has findings or judge passes")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RECIPE\tMODEL\tPROMPT\tFINDINGS\tPER 1K CHUNKS\tPRECISION\tPROPER NOUNS\tAGREE\tREVERTED\t$/CHUNK\tACTIVE")
	for _, s := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(s.RecipeID), dash(modelName(s.ModelAlias, s.ModelResolved)), dash(s.PromptVersion),
			num(s.FindingsTotal), legacyOr(s.Legacy, fltOr(s.FindingsPer1kChunks, "%.1f")),
			precision(s.Labels), precision(s.ProperNounLabels), pctOr(s.AgreementRate),
			fmt.Sprintf("%s/%s", num(s.Reverted), num(s.AppliedEver)),
			legacyOr(s.Legacy, usdOr(s.CostPerChunkUSD)), activeRange(s.FirstActivityAt, s.LastActivityAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range rows {
		renderProposeBlock(out, s)
	}
	return nil
}

func renderProposeBlock(out io.Writer, s db.ProposeScore) {
	era := "recorded"
	if s.Legacy {
		era = "legacy"
	}
	head := []string{dash(modelName(s.ModelAlias, s.ModelResolved))}
	if s.PromptVersion != "" {
		head = append(head, s.PromptVersion)
	}
	if s.StepVersion != nil {
		head = append(head, fmt.Sprintf("step %d", *s.StepVersion))
	}
	head = append(head, era)
	_, _ = fmt.Fprintf(out, "\n● %s  %s\n", idOrUnattributed(s.RecipeID), strings.Join(head, " · "))
	line := func(k, v string) { _, _ = fmt.Fprintf(out, "  %-11s %s\n", k, v) }
	line("active", activeRange(s.FirstActivityAt, s.LastActivityAt))
	cov := fmt.Sprintf("%s transcripts judged · passes %s (complete %s, legacy %s, partial/failed %s, running %s)",
		num(s.TranscriptsJudged), num(s.Passes), num(s.PassesComplete), num(s.PassesLegacy),
		num(s.PassesIncomplete), num(s.PassesRunning))
	line("coverage", cov)
	line("chunks", legacyOr(s.Legacy, fmt.Sprintf("judged %s · cached %s (%s) · failed %s",
		numOr(s.ChunksJudged), numOr(s.ChunksCached), pctOr(s.CachedShare), numOr(s.ChunksFailed))))
	line("findings", fmt.Sprintf("%s (live %s) · per 1k chunks judged %s", num(s.FindingsTotal), num(s.FindingsLive),
		legacyOr(s.Legacy, fltOr(s.FindingsPer1kChunks, "%.1f"))))
	line("issues", mix(s.IssueMix, 0))
	line("states", mix(s.StateMix, 0))
	drops := naLegacy
	if !s.Legacy {
		drops = "—"
		if s.Drops != nil {
			drops = mix(s.Drops, 0)
		}
	}
	line("drops", drops)
	line("parse fail", legacyOr(s.Legacy, numOr(s.ParseFailures)))
	if len(s.Decisions) == 0 {
		line("decided", "— (no decide recipe has decided these findings)")
	}
	for i, d := range s.Decisions {
		k := ""
		if i == 0 {
			k = "decided"
		}
		line(k, fmt.Sprintf("%s (%s): %s — apply %s · hold %s · reject %s [%s]",
			dash(d.DecideModel), shortID(d.DecideRecipeID), num(d.Decided), num(d.Apply), num(d.Hold), num(d.Reject),
			mix(d.Reasons, 6)))
	}
	line("reverts", fmt.Sprintf("%s of %s applied (%s)", num(s.Reverted), num(s.AppliedEver), pctOr(s.RevertRate)))
	line("agreement", fmt.Sprintf("%s of %s fixes also proposed by another recipe (%s)",
		num(s.AgreedKeys), num(s.CorrectionKeys), pctOr(s.AgreementRate)))
	line("labels", fmt.Sprintf("%s · proper nouns %s", tally(s.Labels), tally(s.ProperNounLabels)))
	lat := "—"
	if s.LatencyMsP50 != nil {
		lat = fmt.Sprintf("p50 %s ms · p95 %s ms (%s)", fltOr(s.LatencyMsP50, "%.0f"), fltOr(s.LatencyMsP95, "%.0f"), s.LatencySource)
	}
	line("cost", legacyOr(s.Legacy, fmt.Sprintf("%s total · %s per chunk · latency %s",
		usdOr(s.CostUSD), usdOr(s.CostPerChunkUSD), lat)))
}

func renderDecide(out io.Writer, rows []db.DecideScore) error {
	_, _ = fmt.Fprintf(out, "DECIDE RECIPES (%d)\n", len(rows))
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "  no decide recipe has decisions or calls")
		return nil
	}
	models := map[string]string{}
	for _, r := range rows {
		models[r.RecipeID] = modelName(r.ModelAlias, r.ModelResolved)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RECIPE\tMODEL\tDECISIONS\tAPPLY\tHOLD\tREJECT\tAPPLY PRECISION\tHUMAN AGREE\tFLIPS\t$/CALL\tACTIVE")
	for _, s := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(s.RecipeID), dash(modelName(s.ModelAlias, s.ModelResolved)), num(s.Decisions),
			num(s.Apply), num(s.Hold), num(s.Reject), precision(s.LabelOutcomes["apply"]),
			pctN(s.HumanAgreement, s.HumanDecided), pctN(s.FlipRate, s.FlipOverlap), usdOr(s.CostPerCallUSD),
			activeRange(s.FirstDecisionAt, s.LastActivityAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range rows {
		head := []string{dash(modelName(s.ModelAlias, s.ModelResolved))}
		if s.PromptVersion != "" {
			head = append(head, s.PromptVersion)
		}
		if s.StepVersion != nil {
			head = append(head, fmt.Sprintf("step %d", *s.StepVersion))
		}
		_, _ = fmt.Fprintf(out, "\n● %s  %s\n", shortID(s.RecipeID), strings.Join(head, " · "))
		line := func(k, v string) { _, _ = fmt.Fprintf(out, "  %-11s %s\n", k, v) }
		line("active", activeRange(s.FirstDecisionAt, s.LastActivityAt))
		line("decisions", fmt.Sprintf("%s on %s findings · revoked %s (%s) · live apply %s · hold %s · reject %s",
			num(s.Decisions), num(s.FindingsDecided), num(s.Revoked), pctOr(s.RevokeRate), num(s.Apply), num(s.Hold), num(s.Reject)))
		for _, o := range []string{"apply", "hold", "reject"} {
			if ev := s.OutcomeEvidence[o]; len(ev) > 0 {
				line("evidence", fmt.Sprintf("%s: %s", o, mix(ev, 0)))
			}
		}
		line("reasons", mix(s.Reasons, 8))
		var vs []string
		for _, o := range []string{"apply", "hold", "reject"} {
			if t, ok := s.LabelOutcomes[o]; ok {
				vs = append(vs, fmt.Sprintf("%s %s", o, tally(t)))
			}
		}
		if len(vs) == 0 {
			vs = []string{"— (no labelled fix decided)"}
		}
		line("vs labels", strings.Join(vs, " | "))
		line("p→precision", buckets(s.LabelCalibration, false))
		line("vs humans", fmt.Sprintf("agree %s · p→accept rate %s", pctN(s.HumanAgreement, s.HumanDecided), buckets(s.HumanCalibration, true)))
		flip := "— (no earlier decide recipe)"
		if s.PrevRecipeID != "" {
			flip = fmt.Sprintf("%s of %s findings both decided vs %s (%s)", pctOr(s.FlipRate), num(s.FlipOverlap),
				dash(models[s.PrevRecipeID]), shortID(s.PrevRecipeID))
		}
		line("flips", flip)
		lat := "—"
		if s.LatencyMsP50 != nil {
			lat = fmt.Sprintf("p50 %s ms · p95 %s ms", fltOr(s.LatencyMsP50, "%.0f"), fltOr(s.LatencyMsP95, "%.0f"))
		}
		line("cost", fmt.Sprintf("%s over %s calls (%s cached, %s errors) · %s per call · latency %s",
			usdOr(s.CostUSD), num(s.Calls), num(s.CallsCached), num(s.CallErrors), usdOr(s.CostPerCallUSD), lat))
	}
	return nil
}

// ─── formatting ──────────────────────────────────────────────────────────────

func shortID(id string) string {
	if id == "" {
		return "(unattributed)"
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func idOrUnattributed(id string) string {
	if id == "" {
		return "(unattributed: legacy findings with no recipe)"
	}
	return shortID(id)
}

func modelName(alias, resolved string) string {
	if alias != "" {
		return alias
	}
	return resolved
}

func legacyOr(legacy bool, s string) string {
	if legacy {
		return naLegacy
	}
	return s
}

// num renders an integer with thousands separators.
func num(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func numOr(n *int64) string {
	if n == nil {
		return "—"
	}
	return num(*n)
}

func fltOr(f *float64, format string) string {
	if f == nil {
		return "—"
	}
	return fmt.Sprintf(format, *f)
}

func pctOr(f *float64) string {
	if f == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", *f*100)
}

func pctN(f *float64, n int64) string {
	if f == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%% (n=%s)", *f*100, num(n))
}

func usdOr(f *float64) string {
	if f == nil {
		return "—"
	}
	switch v := *f; {
	case v == 0:
		return "$0"
	case v < 0.01:
		return fmt.Sprintf("$%.6f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}

func precision(t db.LabelTally) string {
	return pctN(t.Precision, t.Decided())
}

func tally(t db.LabelTally) string {
	return fmt.Sprintf("%s (good %s · junk %s · partial %s · unsure %s)", precision(t),
		num(t.Good), num(t.Junk), num(t.Partial), num(t.Unsure))
}

func activeRange(first, last *time.Time) string {
	switch {
	case first == nil && last == nil:
		return "—"
	case first == nil:
		return "→ " + last.UTC().Format(time.DateOnly)
	case last == nil:
		return first.UTC().Format(time.DateOnly) + " →"
	}
	return first.UTC().Format(time.DateOnly) + " → " + last.UTC().Format(time.DateOnly)
}

// mix renders a count map largest first ("k n · k n"); top > 0 keeps the
// largest top and sums the rest as "other".
func mix(m map[string]int64, top int) string {
	if len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var parts []string
	var rest int64
	for i, k := range keys {
		if top > 0 && i >= top {
			rest += m[k]
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", k, num(m[k])))
	}
	if rest > 0 {
		parts = append(parts, fmt.Sprintf("other %s", num(rest)))
	}
	return strings.Join(parts, " · ")
}

// buckets renders the five p buckets that have data.
func buckets(bs []db.PBucket, human bool) string {
	var parts []string
	for _, b := range bs {
		n := b.Good + b.Junk + b.Partial
		if human {
			n = b.Accepted + b.Rejected
		}
		if n == 0 {
			continue
		}
		closer := ")"
		if b.Bucket == len(bs) {
			closer = "]" // the top bucket includes p = 1
		}
		parts = append(parts, fmt.Sprintf("[%.1f,%.1f%s %s (n=%s)", b.Lo, b.Hi, closer, pctOr(b.Rate), num(n)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}
