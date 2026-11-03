package asrpilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/asrpilot"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/runs"
)

// voteStore is the slice of the database `asr-pilot vote` needs.
type voteStore interface {
	pilotReader
	HeldFindings(ctx context.Context, transcriptID, afterID string, limit int) ([]db.HeldFinding, error)
	UpsertFindingASRVotes(ctx context.Context, votes []db.FindingASRVote) (int64, error)
}

type voteOptions struct {
	model    string
	revision string
	asins    string
	bookDir  string
	limit    int
	yes      bool
}

var voteOpts voteOptions

// sampleSize is how many votes the summary prints.
const sampleSize = 12

// VoteCmd is `earmark asr-pilot vote`.
var VoteCmd = &cobra.Command{
	Use:   "vote --model <model_id> (--asin A,... | --book-dir D) [flags]",
	Short: "Record what a pilot ASR model heard at each held finding (dry-run unless --yes)",
	Long: `For every held finding (patch_state proposed, latest unrevoked decision a
hold) on the selected books' tracks that the pilot model transcribed, find
what the pilot heard at the finding's span (CONTRACT §1.11 "asr_vote"):

  1. the span is located in the chunk's pristine text — the
     judge's anchor_offset is a self-reported rune index and often wrong: it
     is used when the text is there, else original_text must occur exactly
     once in the chunk (several matches are unmappable, anchor_ambiguous);
     the chunk hash must still match the one recorded with the finding
  2. the span maps to production words through the segments the chunk was
     built from, giving its time window
  3. the pilot's words in that window (by word time, or for a pilot without
     word times the windows overlapping it) are aligned to the production
     words (a Myers diff, cheap for 540 s windows), and the words between
     the agreed neighbours are what it heard

  heard_replacement  the pilot heard the finding's correction
  heard_original     the pilot heard what production wrote
  neither            something else, or nothing
  unmappable         the span could not be placed (reason in detail)
  style_only         the correction only changes case, punctuation or a
                     spelling variant (grey/gray); what was heard is in
                     detail.style_vote, counted apart in the report

A heard_* vote needs an agreed word next to the span on at least one side
and word-timestamped production words; otherwise it is capped to neither.

Tracks are processed one at a time, findings in pages of 100. With a pilot
transcript in both scopes for a track, the full one is used. Without --yes
nothing is written: it prints the counts and a sample. With --yes each page
is upserted into finding_asr_votes (one row per finding and pilot
transcript; re-running replaces the vote). Findings, decisions and the decide
policy are untouched. The run is recorded in step_runs as asr_vote.

` + migrateWarning + `

Examples:
  earmark asr-pilot vote --model ibm-granite/granite-speech-4.1-2b --asin B075VF1R27
  earmark asr-pilot vote --model nvidia/canary-qwen-2.5b --asin B075VF1R27,B00FQRCM9O --limit 50
  earmark asr-pilot vote --model nvidia/canary-qwen-2.5b --book-dir "/books/Mary Shelley/Frankenstein" --yes`,
	Args: cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		connect(func(ctx context.Context, d *db.DB) error { return runVote(ctx, os.Stdout, d, voteOpts) })
	},
}

func init() {
	f := VoteCmd.Flags()
	f.StringVar(&voteOpts.model, "model", "", "pilot model_id (required)")
	f.StringVar(&voteOpts.revision, "revision", "", "pilot model_revision (required when the model has several)")
	f.StringVar(&voteOpts.asins, "asin", "", "comma-separated catalogue ASINs")
	f.StringVar(&voteOpts.bookDir, "book-dir", "", "one book directory (book_metadata.book_dir)")
	f.IntVar(&voteOpts.limit, "limit", 0, "vote on at most N findings (0 = all)")
	f.BoolVar(&voteOpts.yes, "yes", false, "write the votes (otherwise dry-run)")
}

// voteSummary is what a vote run prints.
type voteSummary struct {
	Tracks, SkippedTracks int
	Findings              int64
	Written               int64
	Votes                 map[string]int64
	Reasons               map[string]int64 // unmappable / neither reasons
	Anchor                map[string]int64 // offset | recovered
	Methods               map[string]int64
	Sample                []sampleVote
}

type sampleVote struct {
	FindingID, Original, Replacement, Vote, Heard, Reason string
	T0                                                    *float64
}

func runVote(ctx context.Context, out io.Writer, st voteStore, o voteOptions) (err error) {
	if o.model == "" {
		return fmt.Errorf("--model is required")
	}
	if o.limit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	dirs, err := bookDirs(ctx, st, o.asins, o.bookDir, true)
	if err != nil {
		return err
	}
	pilots, err := listPilots(ctx, st, db.PilotFilter{ModelID: o.model, ModelRevision: o.revision, BookDirs: dirs})
	if err != nil {
		return err
	}
	pilots, err = onePerTrack(pilots, o.revision)
	if err != nil {
		return err
	}
	if len(pilots) == 0 {
		_, _ = fmt.Fprintf(out, "No pilot transcripts of %s for the selected books.\n", o.model)
		return nil
	}

	mode := db.StepRunDryRun
	if o.yes {
		mode = db.StepRunWrite
	}
	rec := runs.StartIfStore(ctx, st, runs.Spec{
		Step: runs.StepASRVote, Mode: mode, Model: o.model,
		Args: fmt.Sprintf("--model %s --limit %d tracks=%d", o.model, o.limit, len(pilots)),
	})
	defer func() { rec.Finish(err) }()

	sum := voteSummary{Votes: map[string]int64{}, Reasons: map[string]int64{}, Anchor: map[string]int64{}, Methods: map[string]int64{}}
	defer func() { printVoteSummary(out, o, sum) }()

	for i, p := range pilots {
		if o.limit > 0 && sum.Findings >= int64(o.limit) {
			break
		}
		rec.Phase(fmt.Sprintf("track %d/%d", i+1, len(pilots)))
		if err := voteTrack(ctx, st, o, p, &sum, rec); err != nil {
			return err
		}
	}
	return nil
}

// onePerTrack keeps one pilot transcript per track: scope full over
// findings, then the newest. Without --revision the model must have one.
func onePerTrack(pilots []db.PilotTranscript, revision string) ([]db.PilotTranscript, error) {
	revs := map[string]bool{}
	best := map[string]db.PilotTranscript{}
	for _, p := range pilots {
		revs[p.ModelRevision] = true
		cur, ok := best[p.FilePath]
		if !ok || better(p, cur) {
			best[p.FilePath] = p
		}
	}
	if revision == "" && len(revs) > 1 {
		return nil, fmt.Errorf("model has %d revisions (%s): pass --revision", len(revs), strings.Join(sortedKeys(revs), ", "))
	}
	out := make([]db.PilotTranscript, 0, len(best))
	for _, k := range sortedKeys(best) {
		out = append(out, best[k])
	}
	return out, nil
}

func better(p, cur db.PilotTranscript) bool {
	if (p.Scope == db.PilotScopeFull) != (cur.Scope == db.PilotScopeFull) {
		return p.Scope == db.PilotScopeFull
	}
	return p.ID > cur.ID
}

// voteTrack votes on one track's held findings, page by page.
func voteTrack(ctx context.Context, st voteStore, o voteOptions, p db.PilotTranscript, sum *voteSummary, rec *runs.Run) error {
	prod, err := st.GetTranscriptSegments(ctx, []string{p.ProductionTranscriptID})
	if err != nil {
		return err
	}
	segs := prod[p.ProductionTranscriptID]
	pilotSegs, err := st.PilotSegments(ctx, p.ID)
	if err != nil {
		return err
	}
	if len(segs) == 0 || len(pilotSegs) == 0 {
		sum.SkippedTracks++
		rec.Count("tracks_skipped", 1)
		return nil
	}
	sum.Tracks++
	tr := asrpilot.NewTrack(segs)
	pl := asrpilot.NewPilot(pilotSegs, p.HasWordTimes)

	after := db.FirstFindingID
	for {
		size := db.MaxPilotPage
		if o.limit > 0 {
			size = min(size, o.limit-int(sum.Findings))
			if size <= 0 {
				return nil
			}
		}
		page, err := st.HeldFindings(ctx, p.ProductionTranscriptID, after, size)
		if err != nil {
			return err
		}
		batch := make([]db.FindingASRVote, 0, len(page))
		for _, h := range page {
			res := asrpilot.Vote(tr, asrpilot.Finding{
				OriginalText: h.OriginalText, Replacement: h.SuggestedCorrection,
				ChunkText: h.ChunkText, ChunkSHA256: h.ChunkTextSHA256, ChunkFound: h.ChunkFound,
				ChunkStartSec: h.ChunkStartSec, AnchorOffset: h.AnchorOffset, AnchorOccurrence: h.AnchorOccurrence,
			}, pl)
			sum.add(h, res)
			rec.Tick(res.Vote, 0)
			detail, err := json.Marshal(res.Detail)
			if err != nil {
				return fmt.Errorf("vote detail for finding %s: %w", h.ID, err)
			}
			batch = append(batch, db.FindingASRVote{
				FindingID: h.ID, PilotTranscriptID: p.ID, ModelID: p.ModelID,
				ChunkTextSHA256: h.ChunkTextSHA256, SpanStartSec: res.SpanStartSec, SpanEndSec: res.SpanEndSec,
				Vote: res.Vote, HeardText: res.HeardText, Detail: detail,
			})
		}
		if o.yes && len(batch) > 0 {
			n, err := st.UpsertFindingASRVotes(ctx, batch)
			if err != nil {
				return err
			}
			sum.Written += n
			rec.Count("written", n)
		}
		if len(page) < size {
			return nil
		}
		after = page[len(page)-1].ID
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (s *voteSummary) add(h db.HeldFinding, r asrpilot.Result) {
	s.Findings++
	s.Votes[r.Vote]++
	if r.Detail.Reason != "" {
		s.Reasons[r.Vote+":"+r.Detail.Reason]++
	}
	if r.Detail.Anchor != "" {
		s.Anchor[r.Detail.Anchor]++
	}
	if r.Detail.Method != "" {
		s.Methods[r.Detail.Method]++
	}
	// The sample prefers decisive votes, keeping a few of the rest.
	decisive := r.Vote == asrpilot.VoteHeardReplacement || r.Vote == asrpilot.VoteHeardOriginal
	if len(s.Sample) < sampleSize && (decisive || len(s.Sample) < sampleSize/2) {
		s.Sample = append(s.Sample, sampleVote{
			FindingID: h.ID, Original: h.OriginalText, Replacement: h.SuggestedCorrection,
			Vote: r.Vote, Heard: r.HeardText, Reason: r.Detail.Reason, T0: r.SpanStartSec,
		})
	}
}

func printVoteSummary(out io.Writer, o voteOptions, s voteSummary) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	mode := "DRY RUN (nothing written; --yes to write)"
	if o.yes {
		mode = fmt.Sprintf("wrote %d vote(s)", s.Written)
	}
	p("asr-pilot vote · %s · %s\n", o.model, mode)
	p("tracks voted %d · skipped %d (no production or pilot segments) · held findings %d\n",
		s.Tracks, s.SkippedTracks, s.Findings)
	for _, v := range []string{asrpilot.VoteHeardReplacement, asrpilot.VoteHeardOriginal, asrpilot.VoteNeither, asrpilot.VoteUnmappable, asrpilot.VoteStyleOnly} {
		p("  %-18s %d\n", v, s.Votes[v])
	}
	if len(s.Anchor) > 0 {
		p("span located by: anchor_offset %d · unique match (offset wrong) %d · ambiguous %d\n", s.Anchor["offset"], s.Anchor["unique"], s.Reasons[asrpilot.VoteUnmappable+":anchor_ambiguous"])
	}
	if len(s.Methods) > 0 {
		var parts []string
		for _, k := range sortedKeys(s.Methods) {
			parts = append(parts, fmt.Sprintf("%s %d", k, s.Methods[k]))
		}
		p("methods: %s\n", strings.Join(parts, " · "))
	}
	if len(s.Reasons) > 0 {
		p("reasons:\n")
		keys := sortedKeys(s.Reasons)
		sort.SliceStable(keys, func(i, j int) bool { return s.Reasons[keys[i]] > s.Reasons[keys[j]] })
		for _, k := range keys {
			p("  %-44s %d\n", k, s.Reasons[k])
		}
	}
	if len(s.Sample) > 0 {
		p("sample:\n")
		for _, v := range s.Sample {
			at := "      -"
			if v.T0 != nil {
				at = fmt.Sprintf("%7.1fs", *v.T0)
			}
			extra := ""
			if v.Reason != "" {
				extra = " (" + v.Reason + ")"
			}
			p("  %s %s  %q → %q  heard %q  %s%s\n", at, shortID(v.FindingID), v.Original, v.Replacement, v.Heard, v.Vote, extra)
		}
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
