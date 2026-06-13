// Package requeue implements the `earmark requeue` command: a scoped way to
// redo work without the blunt DEBUG_DB_RESET. It can re-transcribe books, retry
// failed jobs, or re-embed transcripts (e.g. after changing the embedding model).
package requeue

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/spf13/cobra"
)

// Requeuer is the slice of the DB this command needs (kept small for testing).
type Requeuer interface {
	FindJobs(ctx context.Context, substr string) ([]db.JobMatch, error)
	FindFailedJobs(ctx context.Context) ([]db.JobMatch, error)
	RequeueJobs(ctx context.Context, substr string) ([]string, error)
	RequeueFailed(ctx context.Context) ([]string, error)
	ReembedJobs(ctx context.Context, substr string) ([]string, error)
}

type options struct {
	reembed bool // re-embed only (delete chunks; keep transcript) — no re-transcription
	failed  bool // target all failed jobs instead of a file-path substring
	yes     bool // actually apply; without it the command is a dry-run preview
}

var opts options

var RequeueCmd = &cobra.Command{
	Use:   "requeue [flags] [book-substring]",
	Short: "Re-transcribe, retry, or re-embed books (dry-run unless --yes)",
	Long: `Redo work for specific books without wiping the database.

Modes:
  requeue <substr>            re-transcribe books whose path contains <substr>
                              (deletes the transcript + its embeddings, resets
                              the job to pending so the runner redoes it)
  requeue --failed            re-transcribe every job currently in 'failed'
  requeue --reembed <substr>  re-embed only: drop the embeddings and let the
                              worker rebuild them (keeps the transcript; use
                              after changing the embedding model or chunk size)

All modes preview matches and do nothing unless you pass --yes.

Examples:
  earmark requeue "Project Hail Mary"          # preview
  earmark requeue "Project Hail Mary" --yes    # re-transcribe it
  earmark requeue --failed --yes               # retry all failures
  earmark requeue --reembed "" --yes           # re-embed everything`,
	Run: runRequeue,
}

func init() {
	RequeueCmd.Flags().BoolVar(&opts.reembed, "reembed", false, "re-embed only (drop chunks, keep transcript)")
	RequeueCmd.Flags().BoolVar(&opts.failed, "failed", false, "target all failed jobs")
	RequeueCmd.Flags().BoolVar(&opts.yes, "yes", false, "apply changes (otherwise dry-run preview)")
}

func runRequeue(cmd *cobra.Command, args []string) {
	substr := ""
	if len(args) > 0 {
		substr = args[0]
	}

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

	if err := run(context.Background(), os.Stdout, database, substr, opts); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// run holds the testable logic: validate flags, preview matches, and (with --yes)
// apply the selected operation.
func run(ctx context.Context, out io.Writer, q Requeuer, substr string, o options) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	if o.reembed && o.failed {
		return fmt.Errorf("--reembed and --failed cannot be combined (failed jobs have no transcript to re-embed)")
	}
	if !o.failed && substr == "" && !o.reembed {
		return fmt.Errorf("provide a book substring, or use --failed (use --reembed \"\" to re-embed everything)")
	}

	// Preview the affected jobs (the reembed path previews by job too — a job and
	// its transcript share the same file_path).
	var matches []db.JobMatch
	var err error
	if o.failed {
		matches, err = q.FindFailedJobs(ctx)
	} else {
		matches, err = q.FindJobs(ctx, substr)
	}
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		p("No matching jobs.\n")
		return nil
	}

	action := "re-transcribe"
	if o.reembed {
		action = "re-embed"
	}
	p("%d job(s) would %s:\n", len(matches), action)
	for _, m := range matches {
		p("  [%s] %s\n", m.Status, filepath.Base(m.FilePath))
	}

	if !o.yes {
		p("\n(dry-run) pass --yes to %s these %d job(s).\n", action, len(matches))
		return nil
	}

	var affected []string
	switch {
	case o.reembed:
		affected, err = q.ReembedJobs(ctx, substr)
	case o.failed:
		affected, err = q.RequeueFailed(ctx)
	default:
		affected, err = q.RequeueJobs(ctx, substr)
	}
	if err != nil {
		return err
	}
	p("\n%sd %d job(s).\n", capitalize(action), len(affected))
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}
