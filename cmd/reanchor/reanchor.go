// Package reanchor implements `earmark reanchor`: re-anchor review findings to
// the transcript's current chunks after a re-chunk (CONTRACT §2.17
// "Re-anchoring"). Dry-run unless --yes, like requeue.
package reanchor

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

// Reanchorer is the slice of the DB this command needs (kept small for testing).
type Reanchorer interface {
	Reanchor(ctx context.Context, scope db.ReanchorScope, apply bool) (db.ReanchorReport, error)
}

type options struct {
	book  string
	limit int
	batch int
	yes   bool
}

var opts options

// ReanchorCmd is `earmark reanchor`.
var ReanchorCmd = &cobra.Command{
	Use:   "reanchor [flags]",
	Short: "Re-anchor proposed findings to the current chunks (dry-run unless --yes)",
	Long: `Re-chunking regenerates every chunk, so the same index can hold different
text and findings recorded before it describe text the projection no longer
has; replayed as they are, they go stale, which is terminal. reanchor finds
each proposed (or unanchorable) finding's span in the transcript's current
pristine chunks — word-bounded, case-sensitive — searching the named chunk if
it is unchanged, otherwise every chunk covering the finding's judged audio
window as one text (so a span straddling two chunks still counts), and:

  one candidate   re-anchors it (chunk_id, chunk hash, rune offset, occurrence)
  several         marks it unanchorable (anchor_ambiguous) — never guessed
  none, or only a boundary-straddling one
                  marks it unanchorable (anchor_not_found)

unanchorable is not terminal: run reanchor again after the next re-chunk and
anything it can now place returns to proposed. Findings a human has decided
(accepted, applied, rejected, reverted) and stale ones are never touched.

Without --yes no finding is written; the preview counts each outcome by model
era. (Like every command it applies pending schema migrations on connect.)
With --yes each batch of transcripts is one transaction that locks its findings
FOR UPDATE SKIP LOCKED and its chunks FOR SHARE, so it cannot race a review
decision or the worker's rebuild. Safe to re-run.

Examples:
  earmark reanchor                         # preview the whole library
  earmark reanchor --book "Children of Dune"
  earmark reanchor --limit 500 --yes       # apply to the first 500 findings
  earmark reanchor --yes                   # apply everywhere`,
	Run: runReanchor,
}

func init() {
	ReanchorCmd.Flags().StringVar(&opts.book, "book", "", "only findings whose file path contains this (case-insensitive)")
	ReanchorCmd.Flags().IntVar(&opts.limit, "limit", 0, "examine at most N findings (0 = all)")
	ReanchorCmd.Flags().IntVar(&opts.batch, "batch", db.DefaultReanchorBatch, "transcripts per transaction")
	ReanchorCmd.Flags().BoolVar(&opts.yes, "yes", false, "apply changes (otherwise dry-run preview)")
}

func runReanchor(_ *cobra.Command, _ []string) {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Printf("Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	// Ctrl-C stops between batches; a committed batch stays committed and the
	// next run picks up where this one left off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Stdout, database, opts); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// run holds the testable logic: validate flags, run the pass, print the report.
func run(ctx context.Context, out io.Writer, r Reanchorer, o options) error {
	if o.limit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	if o.batch < 1 {
		return fmt.Errorf("--batch must be >= 1")
	}
	rep, err := r.Reanchor(ctx, db.ReanchorScope{Book: o.book, Limit: o.limit, BatchSize: o.batch}, o.yes)
	// A cancelled run still reports what it got through.
	printReport(out, rep, o.yes)
	return err
}

func printReport(out io.Writer, rep db.ReanchorReport, applied bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	if len(rep.ByModel) == 0 {
		p("No proposed or unanchorable findings in scope.\n")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "model\ttotal\talready\tunique\tmoved\tambiguous\tnone\tpending\tsurvive\t")
	row := func(name string, t db.ReanchorTally) {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t\n", name,
			t.Total, t.Already, t.Unique, t.Moved, t.Ambiguous, t.None, t.Pending, survival(t))
	}
	for _, m := range rep.Models() {
		row(m, *rep.ByModel[m])
	}
	row("all", rep.Sum())
	_ = tw.Flush()

	p("\nalready = anchor still resolves · unique = one match in the named chunk · " +
		"moved = one match in another chunk\nambiguous / none → unanchorable · " +
		"pending = transcript has no chunks yet (not judged) · survive = (already+unique+moved)/total\n")

	if !applied {
		s := rep.Sum()
		p("\n(dry-run) pass --yes to re-anchor %d and park %d as unanchorable.\n",
			s.Unique+s.Moved, s.Ambiguous+s.None)
		return
	}
	p("\nRe-anchored %d, marked unanchorable %d, skipped %d (decided or rebuilt mid-run; re-run to retry).\n",
		rep.Reanchored, rep.MarkedUnanchorable, rep.Conflicts)
}

func survival(t db.ReanchorTally) string {
	if t.Total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(t.Already+t.Unique+t.Moved)/float64(t.Total))
}
