package decide

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Print writes the audio-vote report as text, or as indented JSON.
func (r *AudioVoteReport) Print(w io.Writer, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	mode := "dry run"
	if r.Write != nil {
		mode = "run (--yes)"
	}
	p("decide --audio-vote %s · recipe %s · rule %s\n", mode, r.RecipeID, r.Rule)
	p("models: %s (every one must agree)\n", strings.Join(r.Models, ", "))
	p("held findings with a counted vote: %d", r.Findings)
	if r.Limit > 0 {
		p(" (--limit %d)", r.Limit)
	}
	p("\n")
	if r.Findings == 0 {
		p("\nNo held findings in scope have a counted vote.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	p("\noutcomes: %s\n", counts(r.Outcomes, reportClasses))
	p("reasons:  %s\n", counts(r.Reasons, sortedKeys(r.Reasons)))
	p("held by:  %s\n", counts(r.HoldReasons, sortedKeys(r.HoldReasons)))
	p("votes:    %s\n", counts(r.Votes, sortedKeys(r.Votes)))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\noutcome\treason\tissue_type\tcount")
	for _, c := range r.Cells {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", c.Outcome, c.Reason, c.IssueType, c.Count)
	}
	_ = tw.Flush()
	if len(r.Sample) > 0 {
		p("\nsample (%d, seed %q):\n", len(r.Sample), r.Seed)
		for _, s := range r.Sample {
			p("  %s %s %q → %q · held %s · votes %s → %s %s\n", shortID(s.FindingID), s.IssueType,
				s.Original, s.Replacement, s.HoldReason, strings.Join(s.Votes, "+"), s.Outcome, s.Reason)
		}
	}
	if ws := r.Write; ws != nil {
		ws.print(p)
		p("\nUndo with: earmark decide revert --recipe %s --yes (narrow with --finding / --since)\n", r.RecipeID)
	} else {
		if r.Capped > 0 {
			p("\nover --max-accepts: %d apply would not be written\n", r.Capped)
		}
		p("\n(dry run) nothing written: no recipe row, no finding_events, no state changes. Holds are never written; --yes writes the applies and rejects.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
