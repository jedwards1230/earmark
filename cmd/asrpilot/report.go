package asrpilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/asrpilot"
	"github.com/jedwards1230/earmark/internal/db"
)

// reportStore is the slice of the database `asr-pilot report` needs. Every
// method is a read.
type reportStore interface {
	pilotReader
	PilotVoteCounts(ctx context.Context, bookDirs []string) ([]db.PilotVoteCount, error)
	PilotVotePairs(ctx context.Context, bookDirs []string) ([]db.PilotVotePair, error)
}

type reportOptions struct {
	asins   string
	bookDir string
	format  string
}

var reportOpts reportOptions

// ReportCmd is `earmark asr-pilot report`.
var ReportCmd = &cobra.Command{
	Use:   "report [--asin A,... | --book-dir D] [--format text|json]",
	Short: "Measure the pilot ASR models against production (read-only)",
	Long: `Per pilot model (model_id@revision, per scope):

  tracks, audio hours      pilot_transcripts rows and their audio_sec
  RTFx                     sum(audio_sec) / sum(wall_sec)
  peak VRAM                max(peak_vram_mib) / max(peak_reserved_mib)
  WER vs ` + asrpilot.ProductionModel + `
                           word-level edit distance of the pilot text against the
                           production transcript, both normalised (lowercase, no
                           punctuation), summed over tracks / production words.
                           Production is a reference, not ground truth: this is
                           disagreement with the production model. For scope
                           'findings' only the covered windows (the union of the
                           pilot's segment start/end) are compared, each window
                           on its own — labelled "covered windows only"
  casing / punctuation     share of the pilot's words with an uppercase letter /
                           a punctuation mark (production is ~0 for both)

then, per pair of pilot models on the same tracks, their WER against each
other (over the windows both covered), and from finding_asr_votes the held
findings voted and resolved (heard_replacement + heard_original) per model
and the two models' vote agreement.

Tracks are read one at a time (the production segments through the bounded
segment loader, then each pilot transcript of that track).

` + migrateWarning + `

Examples:
  earmark asr-pilot report
  earmark asr-pilot report --asin B075VF1R27,B00FQRCM9O --format json`,
	Args: cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		connect(func(ctx context.Context, d *db.DB) error { return runReport(ctx, os.Stdout, d, reportOpts) })
	},
}

func init() {
	f := ReportCmd.Flags()
	f.StringVar(&reportOpts.asins, "asin", "", "comma-separated catalogue ASINs (default: every pilot transcript)")
	f.StringVar(&reportOpts.bookDir, "book-dir", "", "one book directory (book_metadata.book_dir)")
	f.StringVar(&reportOpts.format, "format", "text", "text or json")
}

// Report is the JSON form of `asr-pilot report`.
type Report struct {
	Reference  string        `json:"reference"`
	Models     []ModelRow    `json:"models"`
	Production TextRow       `json:"production_text"`
	Cross      []CrossRow    `json:"cross"`
	Votes      []VoteRow     `json:"votes"`
	Agreement  []AgreeRow    `json:"agreement"`
	Skipped    SkippedTracks `json:"skipped"`
}

// ModelRow is one model × scope.
type ModelRow struct {
	Model           string  `json:"model"`
	ModelID         string  `json:"model_id"`
	ModelRevision   string  `json:"model_revision"`
	Backend         string  `json:"backend"`
	Scope           string  `json:"scope"`
	Tracks          int     `json:"tracks"`
	AudioHours      float64 `json:"audio_hours"`
	RTFx            float64 `json:"rtfx"`
	PeakVRAMMiB     int64   `json:"peak_vram_mib"`
	PeakReservedMiB int64   `json:"peak_reserved_mib"`
	// WERScope is "full" or "covered_windows_only".
	WERScope     string  `json:"wer_scope"`
	CoveredHours float64 `json:"covered_hours"`
	WER          float64 `json:"wer_vs_reference"`
	Errors       int64   `json:"word_errors"`
	RefWords     int64   `json:"reference_words"`
	HypWords     int64   `json:"pilot_words"`
	TextRow
}

// TextRow is the casing/punctuation of a transcript set.
type TextRow struct {
	Words           int64   `json:"words"`
	CasingRate      float64 `json:"casing_rate"`
	PunctuationRate float64 `json:"punctuation_rate"`
}

// CrossRow is two pilot models compared with each other (A = reference).
type CrossRow struct {
	ModelA       string  `json:"model_a"`
	ModelB       string  `json:"model_b"`
	WERScope     string  `json:"wer_scope"`
	Tracks       int     `json:"tracks"`
	CoveredHours float64 `json:"covered_hours,omitempty"`
	WER          float64 `json:"wer"`
	Errors       int64   `json:"word_errors"`
	RefWords     int64   `json:"reference_words"`
}

// VoteRow is one model's held-finding votes.
type VoteRow struct {
	Model            string `json:"model"`
	Voted            int64  `json:"voted"`
	Resolved         int64  `json:"resolved"`
	HeardReplacement int64  `json:"heard_replacement"`
	HeardOriginal    int64  `json:"heard_original"`
	Neither          int64  `json:"neither"`
	Unmappable       int64  `json:"unmappable"`
}

// AgreeRow is two models' agreement on the findings both voted on.
type AgreeRow struct {
	ModelA                string  `json:"model_a"`
	ModelB                string  `json:"model_b"`
	Both                  int64   `json:"both_voted"`
	Mapped                int64   `json:"both_mapped"`
	Agree                 int64   `json:"agree"`
	AgreementRate         float64 `json:"agreement_rate"`
	Resolved              int64   `json:"both_resolved"`
	ResolvedAgree         int64   `json:"resolved_agree"`
	ResolvedAgreementRate float64 `json:"resolved_agreement_rate"`
}

// SkippedTracks counts tracks left out of the WER.
type SkippedTracks struct {
	NoProductionSegments int `json:"no_production_segments"`
	NoPilotSegments      int `json:"no_pilot_segments"`
}

func runReport(ctx context.Context, out io.Writer, st reportStore, o reportOptions) error {
	if o.format != "text" && o.format != "json" {
		return fmt.Errorf("--format must be text or json")
	}
	dirs, err := bookDirs(ctx, st, o.asins, o.bookDir, false)
	if err != nil {
		return err
	}
	pilots, err := listPilots(ctx, st, db.PilotFilter{BookDirs: dirs})
	if err != nil {
		return err
	}
	rep, err := buildReport(ctx, st, pilots, dirs)
	if err != nil {
		return err
	}
	if o.format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(out, rep)
	return nil
}

type loadedPilot struct {
	meta db.PilotTranscript
	key  string // model@rev
	toks []asrpilot.Token
	cov  asrpilot.Coverage
}

func buildReport(ctx context.Context, st reportStore, pilots []db.PilotTranscript, dirs []string) (Report, error) {
	rep := Report{Reference: asrpilot.ProductionModel}
	byTrack := map[string][]db.PilotTranscript{}
	for _, p := range pilots {
		byTrack[p.ProductionTranscriptID] = append(byTrack[p.ProductionTranscriptID], p)
	}
	stats := map[string]*asrpilot.ModelStats{} // model@rev|scope
	cross := map[string]*asrpilot.CrossStats{} // a|b
	var prodText asrpilot.TextStats

	for _, tid := range sortedKeys(byTrack) {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		prodSegs, err := st.GetTranscriptSegments(ctx, []string{tid})
		if err != nil {
			return rep, err
		}
		var prod []asrpilot.Token
		if segs := prodSegs[tid]; len(segs) > 0 {
			prod = asrpilot.ProductionTokens(segs)
			prodText.Add(prod)
		} else {
			rep.Skipped.NoProductionSegments++
		}

		var loaded []loadedPilot
		for _, p := range byTrack[tid] {
			segs, err := st.PilotSegments(ctx, p.ID)
			if err != nil {
				return rep, err
			}
			k := modelKey(p.ModelID, p.ModelRevision)
			s := stats[k+"|"+p.Scope]
			if s == nil {
				s = &asrpilot.ModelStats{ModelID: p.ModelID, ModelRevision: p.ModelRevision, Backend: p.Backend, Scope: p.Scope}
				stats[k+"|"+p.Scope] = s
			}
			s.AddTrack(p)
			if len(segs) == 0 {
				rep.Skipped.NoPilotSegments++
				continue
			}
			lp := loadedPilot{meta: p, key: k, toks: asrpilot.PilotTokens(segs), cov: asrpilot.CoverageFor(p.Scope, segs)}
			s.Text.Add(lp.toks)
			if prod != nil {
				s.VsProduction.Add(asrpilot.Compare(prod, lp.toks, lp.cov))
				s.CoveredSec += coveredSec(lp.cov, p)
			}
			loaded = append(loaded, lp)
		}

		// Every pair of different models on this track, over what both cover.
		sort.Slice(loaded, func(i, j int) bool { return loaded[i].key < loaded[j].key })
		for i := range loaded {
			for j := i + 1; j < len(loaded); j++ {
				a, b := loaded[i], loaded[j]
				if a.key == b.key {
					continue
				}
				cov := asrpilot.Intersect(a.cov, b.cov)
				c := cross[a.key+"|"+b.key]
				if c == nil {
					c = &asrpilot.CrossStats{ModelA: a.key, ModelB: b.key}
					cross[a.key+"|"+b.key] = c
				}
				c.Tracks++
				c.Partial = c.Partial || !cov.Full
				if !cov.Full {
					c.CoveredSec += cov.Seconds()
				}
				c.Add(asrpilot.Compare(a.toks, b.toks, cov))
			}
		}
	}

	for _, k := range sortedKeys(stats) {
		rep.Models = append(rep.Models, modelRow(stats[k]))
	}
	rep.Production = textRow(prodText)
	for _, k := range sortedKeys(cross) {
		c := cross[k]
		row := CrossRow{ModelA: c.ModelA, ModelB: c.ModelB, WERScope: "full", Tracks: c.Tracks,
			WER: c.WER(), Errors: c.Errors, RefWords: c.RefWords}
		if c.Partial {
			row.WERScope, row.CoveredHours = "covered_windows_only", c.CoveredSec/3600
		}
		rep.Cross = append(rep.Cross, row)
	}

	counts, err := st.PilotVoteCounts(ctx, dirs)
	if err != nil {
		return rep, err
	}
	tallies := map[string]*asrpilot.VoteTally{}
	for _, c := range counts {
		k := modelKey(c.ModelID, c.ModelRevision)
		if tallies[k] == nil {
			tallies[k] = &asrpilot.VoteTally{}
		}
		tallies[k].Add(c.Vote, c.N)
	}
	for _, k := range sortedKeys(tallies) {
		t := tallies[k]
		rep.Votes = append(rep.Votes, VoteRow{Model: k, Voted: t.Voted(), Resolved: t.Resolved(),
			HeardReplacement: t.HeardReplacement, HeardOriginal: t.HeardOriginal, Neither: t.Neither, Unmappable: t.Unmappable})
	}

	pairs, err := st.PilotVotePairs(ctx, dirs)
	if err != nil {
		return rep, err
	}
	cells := make([]asrpilot.VoteCell, len(pairs))
	for i, p := range pairs {
		cells[i] = asrpilot.VoteCell{ModelA: modelKey(p.ModelA, p.RevisionA), ModelB: modelKey(p.ModelB, p.RevisionB),
			VoteA: p.VoteA, VoteB: p.VoteB, N: p.N}
	}
	for _, a := range asrpilot.Agreement(cells) {
		rep.Agreement = append(rep.Agreement, AgreeRow{ModelA: a.ModelA, ModelB: a.ModelB, Both: a.Both, Mapped: a.Mapped,
			Agree: a.Agree, AgreementRate: a.AgreementRate(), Resolved: a.Resolved, ResolvedAgree: a.ResolvedAgree,
			ResolvedAgreementRate: a.ResolvedAgreementRate()})
	}
	return rep, nil
}

// coveredSec is the audio a comparison covered: the windows for a partial
// scope, the track's audio otherwise.
func coveredSec(cov asrpilot.Coverage, p db.PilotTranscript) float64 {
	if !cov.Full {
		return cov.Seconds()
	}
	if p.AudioSec != nil {
		return *p.AudioSec
	}
	return 0
}

func modelRow(s *asrpilot.ModelStats) ModelRow {
	r := ModelRow{
		Model: modelKey(s.ModelID, s.ModelRevision), ModelID: s.ModelID, ModelRevision: s.ModelRevision,
		Backend: s.Backend, Scope: s.Scope, Tracks: s.Tracks, AudioHours: s.AudioSec / 3600, RTFx: s.RTFx(),
		PeakVRAMMiB: s.PeakVRAMMiB, PeakReservedMiB: s.PeakReservedMiB,
		WERScope: "full", CoveredHours: s.CoveredSec / 3600, WER: s.VsProduction.WER(),
		Errors: s.VsProduction.Errors, RefWords: s.VsProduction.RefWords, HypWords: s.VsProduction.HypWords,
		TextRow: textRow(s.Text),
	}
	if s.Scope == db.PilotScopeFindings {
		r.WERScope = "covered_windows_only"
	}
	return r
}

func textRow(t asrpilot.TextStats) TextRow {
	return TextRow{Words: t.Words, CasingRate: t.CasingRate(), PunctuationRate: t.PunctuationRate()}
}

func printReport(out io.Writer, r Report) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	if len(r.Models) == 0 {
		p("No pilot transcripts in scope.\n")
		return
	}
	p("ASR pilot report · WER reference: %s (production; disagreement, not ground truth)\n\n", r.Reference)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tSCOPE\tTRACKS\tAUDIO h\tRTFx\tPEAK VRAM MiB (alloc/reserved)\tWER vs ref\tWER OVER\tCASED\tPUNCT")
	for _, m := range r.Models {
		over := "full tracks"
		if m.WERScope == "covered_windows_only" {
			over = fmt.Sprintf("covered windows only (%.2f h)", m.CoveredHours)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%.2f\t%.1f\t%d / %d\t%.2f%% (%d/%d)\t%s\t%.1f%%\t%.1f%%\n",
			m.Model, m.Scope, m.Tracks, m.AudioHours, m.RTFx, m.PeakVRAMMiB, m.PeakReservedMiB,
			100*m.WER, m.Errors, m.RefWords, over, 100*m.CasingRate, 100*m.PunctuationRate)
	}
	_ = tw.Flush()
	p("production text: %d words · cased %.1f%% · punctuated %.1f%%\n",
		r.Production.Words, 100*r.Production.CasingRate, 100*r.Production.PunctuationRate)
	if r.Skipped.NoProductionSegments+r.Skipped.NoPilotSegments > 0 {
		p("skipped from WER: %d track(s) without production segments, %d pilot transcript(s) without segments\n",
			r.Skipped.NoProductionSegments, r.Skipped.NoPilotSegments)
	}

	if len(r.Cross) > 0 {
		p("\nPilot vs pilot (A = reference):\n")
		tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "A\tB\tTRACKS\tWER\tOVER")
		for _, c := range r.Cross {
			over := "full tracks"
			if c.WERScope == "covered_windows_only" {
				over = fmt.Sprintf("covered windows only (%.2f h)", c.CoveredHours)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%.2f%% (%d/%d)\t%s\n", c.ModelA, c.ModelB, c.Tracks, 100*c.WER, c.Errors, c.RefWords, over)
		}
		_ = tw.Flush()
	}

	p("\nHeld findings (finding_asr_votes, current vote per model):\n")
	if len(r.Votes) == 0 {
		p("  none voted yet (earmark asr-pilot vote --yes)\n")
	} else {
		tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "MODEL\tVOTED\tRESOLVED\theard_replacement\theard_original\tneither\tunmappable")
		for _, v := range r.Votes {
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%d (%.1f%%)\t%d\t%d\t%d\t%d\n", v.Model, v.Voted, v.Resolved,
				100*ratio(v.Resolved, v.Voted), v.HeardReplacement, v.HeardOriginal, v.Neither, v.Unmappable)
		}
		_ = tw.Flush()
	}
	if len(r.Agreement) > 0 {
		p("\nTwo-model vote agreement:\n")
		tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "A\tB\tBOTH VOTED\tAGREE (both mapped)\tAGREE (both resolved)")
		for _, a := range r.Agreement {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d/%d (%.1f%%)\t%d/%d (%.1f%%)\n", a.ModelA, a.ModelB, a.Both,
				a.Agree, a.Mapped, 100*a.AgreementRate, a.ResolvedAgree, a.Resolved, 100*a.ResolvedAgreementRate)
		}
		_ = tw.Flush()
	}
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
