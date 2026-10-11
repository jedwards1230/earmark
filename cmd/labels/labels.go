// Package labels implements `earmark labels import`: append quality labels
// on proposed fixes to finding_labels (CONTRACT §2.15 "Labels and
// scorecards"), where `earmark models report` scores every recipe against
// them.
package labels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/labels"
)

// Importer is the slice of the DB the command needs (*db.DB implements it).
type Importer interface {
	ImportLabels(ctx context.Context, imp db.LabelImport) (db.LabelImportReport, error)
}

// Options are the import flags.
type Options struct {
	Set, Labeler string
	Format       string
	LabelsFile   string
	LabelledAt   string
	DryRun       bool
	ParseOnly    bool
	MatchCurrent bool
	JSON         bool
	// ShowUnresolved caps the unresolved items listed (text output).
	ShowUnresolved int
}

var opts Options

// LabelsCmd is `earmark labels`.
var LabelsCmd = &cobra.Command{
	Use:   "labels",
	Short: "Quality labels on proposed fixes (finding_labels)",
	Args:  cobra.NoArgs,
}

var importCmd = &cobra.Command{
	Use:   "import --set <name> --labeler <kind:name> [flags] <file>",
	Short: "Append labels from a file to finding_labels (idempotent; --dry-run to preview)",
	Long: `import appends quality labels — good, junk, partial (right target, still
wrong: counts against precision) or unsure (excluded) — to finding_labels, an
append-only table (CONTRACT §2.15 "Labels and scorecards").

Each item names its target either by finding_id, or by chunk_id + original +
correction. A label is stored against a chunk REVISION, so it scores every
recipe that proposes that fix to that text — before or after the label:

  finding  a finding_id: the finding's recorded chunk hash and texts; or no
           hash: findings that proposed the same fix on the chunk recorded
           the chunk's current revision
  hash     the item's chunk_text_sha256, if it is the current chunk's (whose
           text must contain the span) or one a finding recorded
  chunk    --match-current-chunk only: no finding proposed the fix, and the
           current chunk text contains the span (and the item's context)

Anything else — a chunk that is gone or changed since the findings saw it, a
fix no finding proposed (without --match-current-chunk), a finding whose
texts disagree with the item, items of one file that label the same fix
differently — is reported unresolved and never stored.

Re-importing is a no-op: an item is skipped when the latest stored row of its
(set, labeler, fix) has the same label and reason. A changed label is a new
row, even back to an earlier one (good → junk → good); the label in force is
the newest by labelled_at (human:… labelers outrank all others).

Formats (--format, default auto by extension):
  jsonl        canonical: one {"id","finding_id","chunk_id","chunk_text_sha256",
               "original","correction","issue_type","context","label","reason",
               "labelled_at"} per line
  bakeoff      bake-off label dumps (.json) and verdict*.md appendices
  bakeoff-key  judge_items_key.json with --labels jlabels.py

--dry-run resolves against the database in a read-only transaction and
reports what would be stored; --parse-only reads the file without connecting.
(Like every command, connecting applies pending schema migrations.)

Examples:
  earmark labels import --set bakeoff-2026-10-qw2 --labeler opus:bakeoff-2026-10 --match-current-chunk --dry-run verdict-judges-qw2.md
  earmark labels import --set bakeoff-2026-10-qw1 --labeler opus:bakeoff-2026-10 \
      --format bakeoff-key --labels jlabels.py judge_items_key.json
  earmark labels import --set review-2026-11 --labeler human:justin labels.jsonl`,
	Args: cobra.ExactArgs(1),
	Run:  runImport,
}

func init() {
	f := importCmd.Flags()
	f.StringVar(&opts.Set, "set", "", "label set name, e.g. bakeoff-2026-10-qw2 (required)")
	f.StringVar(&opts.Labeler, "labeler", "", "who labelled, kind:name — human:justin, opus:bakeoff-2026-10 (required)")
	f.StringVar(&opts.Format, "format", labels.FormatAuto, "file format: "+strings.Join(labels.Formats, ", "))
	f.StringVar(&opts.LabelsFile, "labels", "", "bakeoff-key: the labels file (jlabels.py or a JSON object)")
	f.StringVar(&opts.LabelledAt, "labelled-at", "", "labelled_at for items without one (RFC 3339 or YYYY-MM-DD; default: the file's Date:, else now)")
	f.BoolVar(&opts.DryRun, "dry-run", false, "resolve and count without writing (read-only transaction)")
	f.BoolVar(&opts.ParseOnly, "parse-only", false, "parse the file and count; no database connection")
	f.BoolVar(&opts.MatchCurrent, "match-current-chunk", false, "let an item no finding proposed (no finding_id, no hash) match the CURRENT chunk when its text contains the span and context")
	f.BoolVar(&opts.JSON, "json", false, "print JSON")
	f.IntVar(&opts.ShowUnresolved, "show-unresolved", 20, "list at most N unresolved items")
	LabelsCmd.AddCommand(importCmd)
}

func runImport(cmd *cobra.Command, args []string) {
	if err := opts.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	if err := importFile(cmd.Context(), args[0], opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// importFile parses path and, unless --parse-only, imports it; the database
// is closed before it returns.
func importFile(ctx context.Context, path string, o Options) error {
	parsed, err := Parse(path, o)
	if err != nil {
		return err
	}
	if o.ParseOnly {
		return Import(ctx, os.Stdout, nil, path, parsed, o)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer database.Close()
	return Import(ctx, os.Stdout, database, path, parsed, o)
}

func (o Options) validate() error {
	imp := db.LabelImport{Set: o.Set, Labeler: o.Labeler}
	if err := imp.Validate(); err != nil {
		return err
	}
	if o.DryRun && o.ParseOnly {
		return fmt.Errorf("--dry-run and --parse-only are exclusive")
	}
	return nil
}

// Parse reads path with the flags' format options.
func Parse(path string, o Options) (labels.Parsed, error) {
	po := labels.Options{Format: o.Format, LabelsFile: o.LabelsFile}
	if o.LabelledAt != "" {
		at, err := labels.ParseTime(o.LabelledAt)
		if err != nil {
			return labels.Parsed{}, fmt.Errorf("--labelled-at: %w", err)
		}
		po.LabelledAt = at
	}
	p, err := labels.ParseFile(path, po)
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParseSummary describes a parsed file without the database.
type ParseSummary struct {
	File       string         `json:"file"`
	Format     string         `json:"format"`
	Set        string         `json:"set"`
	Labeler    string         `json:"labeler"`
	Items      int            `json:"items"`
	Unlabelled int            `json:"unlabelled,omitempty"`
	ByLabel    map[string]int `json:"by_label"`
	// ByTarget counts items by how they name their target: finding,
	// chunk+hash, chunk (span only).
	ByTarget   map[string]int `json:"by_target"`
	WithCtx    int            `json:"with_context"`
	LabelledAt string         `json:"labelled_at,omitempty"`
}

func summarize(path string, p labels.Parsed, o Options) ParseSummary {
	s := ParseSummary{File: path, Format: p.Format, Set: o.Set, Labeler: o.Labeler, Items: len(p.Items),
		Unlabelled: p.Unlabelled, ByLabel: map[string]int{}, ByTarget: map[string]int{}}
	for _, it := range p.Items {
		s.ByLabel[it.Label]++
		switch {
		case it.FindingID != "":
			s.ByTarget["finding"]++
		case it.ChunkTextSHA256 != "":
			s.ByTarget["chunk+hash"]++
		default:
			s.ByTarget["chunk"]++
		}
		if it.Context != "" {
			s.WithCtx++
		}
	}
	if len(p.Items) > 0 && !p.Items[0].LabelledAt.IsZero() {
		s.LabelledAt = p.Items[0].LabelledAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return s
}

// Import parses-only (store nil) or imports parsed into store, and prints
// the result.
func Import(ctx context.Context, out io.Writer, store Importer, path string, parsed labels.Parsed, o Options) error {
	sum := summarize(path, parsed, o)
	if store == nil {
		if o.JSON {
			return writeJSON(out, map[string]any{"parse_only": true, "parsed": sum})
		}
		printSummary(out, sum)
		_, _ = fmt.Fprintln(out, "parse only: nothing resolved or written (use --dry-run to resolve against the database)")
		return nil
	}
	rep, err := store.ImportLabels(ctx, db.LabelImport{
		Set: o.Set, Labeler: o.Labeler, Items: parsed.Items, DryRun: o.DryRun, MatchCurrentChunk: o.MatchCurrent,
	})
	if err != nil {
		return err
	}
	if o.JSON {
		return writeJSON(out, map[string]any{"parsed": sum, "import": rep})
	}
	printSummary(out, sum)
	printReport(out, rep, o.ShowUnresolved)
	return nil
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printSummary(out io.Writer, s ParseSummary) {
	_, _ = fmt.Fprintf(out, "%s: format %s · %d items · set %s · labeler %s\n", s.File, s.Format, s.Items, s.Set, s.Labeler)
	_, _ = fmt.Fprintf(out, "  labels   %s\n", counts(s.ByLabel, db.Labels))
	_, _ = fmt.Fprintf(out, "  targets  %s · %d with context\n", counts(s.ByTarget, []string{"finding", "chunk+hash", "chunk"}), s.WithCtx)
	if s.Unlabelled > 0 {
		_, _ = fmt.Fprintf(out, "  %d key items have no label (skipped)\n", s.Unlabelled)
	}
	if s.LabelledAt != "" {
		_, _ = fmt.Fprintf(out, "  labelled_at %s (items without their own)\n", s.LabelledAt)
	}
}

func printReport(out io.Writer, r db.LabelImportReport, show int) {
	verb := "inserted"
	if r.DryRun {
		verb = "would insert"
	}
	_, _ = fmt.Fprintf(out, "  %s %d · skipped %d (already stored or repeated) · unresolved %d\n", verb, r.Inserted, r.Skipped, r.Unresolved)
	_, _ = fmt.Fprintf(out, "  resolved by %s\n", counts(r.ByResolution, []string{db.LabelResolvedFinding, db.LabelResolvedHash, db.LabelResolvedChunk}))
	if r.Unresolved == 0 {
		return
	}
	reasons := map[string]int{}
	for _, u := range r.UnresolvedItems {
		reasons[u.Reason]++
	}
	_, _ = fmt.Fprintf(out, "  unresolved: %s\n", counts(reasons, nil))
	for i, u := range r.UnresolvedItems {
		if i >= show {
			_, _ = fmt.Fprintf(out, "    … %d more (--show-unresolved, or --json for all)\n", len(r.UnresolvedItems)-show)
			break
		}
		ref := u.SourceRef
		if ref == "" {
			ref = fmt.Sprintf("#%d", u.Index+1)
		}
		_, _ = fmt.Fprintf(out, "    %s  %s  %s\n", ref, u.Reason, u.Detail)
	}
}

// counts renders "k n · k n" in order's order, then the remaining keys
// sorted; zero counts in order are shown, unknown keys only when non-zero.
func counts(m map[string]int, order []string) string {
	var parts []string
	seen := map[string]bool{}
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
		seen[k] = true
	}
	var rest []string
	for k := range m {
		if !seen[k] && m[k] > 0 {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}
