// Package eval implements the `earmark eval` command: a read-only LLM-as-judge
// pass that records SUSPECTED transcription errors as advisory findings without
// ever editing the transcripts (CONTRACT §2.15, issue #49).
//
// It mirrors `requeue`'s ergonomics: dry-run by default (prints what it would
// record), persisting only with --write/--yes. Cost is operator-bounded — judge
// a single book or a random sample of N chunks, never the whole library at once.
package eval

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/spf13/cobra"
)

// runner is the slice of internal/eval the command drives (kept small + an
// interface so the dry-run gating is unit-testable without a DB or LLM).
type runner interface {
	Run(ctx context.Context, opts evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error)
}

type options struct {
	sample int  // judge a random sample of N chunks library-wide (instead of a book)
	limit  int  // cap chunks evaluated for a book (0 → package default)
	write  bool // persist findings; without it the command is a dry-run preview
}

var opts options

var EvalCmd = &cobra.Command{
	Use:   "eval [flags] [book-substring]",
	Short: "Read-only LLM judge: flag suspected transcript errors (dry-run unless --write)",
	Long: `Run a read-only LLM-as-judge over transcript chunks and record SUSPECTED
errors as advisory findings. The transcripts are NEVER edited — findings are
advisory metadata you triage by confidence (CONTRACT §2.15).

Cost is bounded: evaluate one book, or a random --sample of N chunks. Nothing is
recorded unless you pass --write (alias --yes).

Endpoint: bind AI_ROLES.eval to a chat AI_ENDPOINTS entry (preferred), or set
EVAL_CHAT_BASE_URL and EVAL_CHAT_MODEL (OpenAI-compatible chat endpoint, e.g.
vLLM) as a fallback. EVAL_CHAT_API_KEY is optional.

Examples:
  earmark eval "Project Hail Mary"           # preview findings for one book
  earmark eval "Project Hail Mary" --write   # record them
  earmark eval --sample 50                   # preview a 50-chunk library sample
  earmark eval --sample 50 --write           # record a 50-chunk sample`,
	Run: runEval,
}

func init() {
	EvalCmd.Flags().IntVar(&opts.sample, "sample", 0, "judge a random sample of N chunks library-wide")
	EvalCmd.Flags().IntVar(&opts.limit, "limit", 0, "max chunks to evaluate for a book (0 = default)")
	EvalCmd.Flags().BoolVar(&opts.write, "write", false, "persist findings (otherwise dry-run preview)")
	EvalCmd.Flags().BoolVar(&opts.write, "yes", false, "alias for --write")
}

func runEval(cmd *cobra.Command, args []string) {
	book := ""
	if len(args) > 0 {
		book = args[0]
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

	// Resolve the chat endpoint: AI_ROLES["eval"] from the registry when set,
	// else the standalone EVAL_CHAT_* env vars (#48 resolved).
	chat, err := evalpkg.ResolveChatClient(evalpkg.ConfigSource(cfg))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	judge := evalpkg.NewJudge(chat)

	r := &dbRunner{reader: database, judge: judge, writer: database}
	if err := run(context.Background(), os.Stdout, r, book, opts); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// dbRunner adapts the DB + judge to the runner interface.
type dbRunner struct {
	reader evalpkg.ChunkReader
	judge  *evalpkg.Judge
	writer evalpkg.FindingWriter
}

func (d *dbRunner) Run(ctx context.Context, o evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error) {
	return evalpkg.Run(ctx, d.reader, d.judge, d.writer, o)
}

// run holds the testable logic: validate flags, run the judge, and report.
// In dry-run (no --write) it prints what it would record and persists nothing.
func run(ctx context.Context, out io.Writer, r runner, book string, o options) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	if o.sample <= 0 && book == "" {
		return fmt.Errorf("provide a book substring, or use --sample N")
	}
	if o.sample > 0 && book != "" {
		return fmt.Errorf("--sample and a book substring cannot be combined")
	}

	runOpts := evalpkg.RunOptions{
		Book:   book,
		Sample: o.sample,
		Limit:  o.limit,
		Write:  o.write,
	}

	findings, stats, err := r.Run(ctx, runOpts)
	if err != nil {
		return err
	}

	scope := fmt.Sprintf("book %q", book)
	if o.sample > 0 {
		scope = fmt.Sprintf("a %d-chunk sample", o.sample)
	}
	p("Evaluated %d chunk(s) from %s — %d suspected error(s) found.\n",
		stats.ChunksEvaluated, scope, stats.FindingsFound)
	if stats.ChunksSkipped > 0 {
		p("(%d chunk(s) skipped due to transient judge errors — partial results below.)\n", stats.ChunksSkipped)
	}

	for _, f := range findings {
		conf := f.Confidence
		p("  [%.2f] %-22s %s — %q\n", conf, f.IssueType, filepath.Base(f.FilePath), truncate(f.OriginalText, 60))
	}

	if !o.write {
		p("\n(dry-run) pass --write to record these %d finding(s).\n", stats.FindingsFound)
		return nil
	}
	if stats.Persisted {
		p("\nRecorded %d finding(s).\n", stats.FindingsFound)
	} else {
		p("\nNo findings to record.\n")
	}
	return nil
}

// truncate shortens a string to n runes for the preview line, appending an
// ellipsis. It slices by rune (not byte) so a multi-byte codepoint — accented
// proper nouns, CJK, emoji — is never split mid-encoding.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
