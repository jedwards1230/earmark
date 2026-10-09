package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

type revertOptions struct {
	recipe  string
	finding string
	since   string
	yes     bool
	json    bool
}

var ropts revertOptions

// RevertCmd is `earmark decide revert`.
var RevertCmd = &cobra.Command{
	Use:   "revert [flags]",
	Short: "Undo decide-recipe decisions by recipe, finding or time (dry-run unless --yes)",
	Long: `revert undoes what a decide recipe did (CONTRACT §2.19 "earmark decide
revert"). Scope with any of --recipe, --finding and --since; together they
narrow (AND). Only findings a decide recipe last moved (decided_by
jev:<recipe_id>) are touched — a person's decision never is:

  applied  → reverted   (the fix comes out on the next rebuild)
  accepted → rejected   (never replayed)
  rejected → proposed   (decided again)
  reverted → proposed   (a re-check's undo; decided again)

Each move is attributed to revert:jev:<recipe_id> and recorded in
finding_events; every live decision in scope gets a revoke event. Afterwards
every moved finding's chunk is flagged for rebuild. Without --yes it only
counts. Running it twice is harmless: the second run finds nothing to undo.

Examples:
  earmark decide revert --recipe 3f…a1                    # preview
  earmark decide revert --recipe 3f…a1 --yes
  earmark decide revert --finding 0b5c…-…-… --yes          # one finding
  earmark decide revert --since 2026-10-09T14:00:00Z --yes # everything since`,
	Run: runRevert,
}

func init() {
	f := RevertCmd.Flags()
	f.StringVar(&ropts.recipe, "recipe", "", "undo this decide recipe (full recipe id)")
	f.StringVar(&ropts.finding, "finding", "", "undo this finding (uuid)")
	f.StringVar(&ropts.since, "since", "", "undo decisions made at or after this time (RFC 3339)")
	f.BoolVar(&ropts.yes, "yes", false, "write the undo (otherwise dry-run)")
	f.BoolVar(&ropts.json, "json", false, "print the report as JSON")
}

// scope parses the options into a db.RevertScope.
func (o revertOptions) scope() (db.RevertScope, error) {
	s := db.RevertScope{RecipeID: strings.ToLower(strings.TrimSpace(o.recipe)), FindingID: strings.ToLower(strings.TrimSpace(o.finding))}
	if o.since != "" {
		t, err := time.Parse(time.RFC3339, o.since)
		if err != nil {
			return s, fmt.Errorf("--since %q is not an RFC 3339 time", o.since)
		}
		s.Since = t
	}
	if s.RecipeID == "" && s.FindingID == "" && s.Since.IsZero() {
		return s, errors.New("give --recipe, --finding or --since")
	}
	return s, nil
}

// Reverter is the store revert needs (*db.DB implements it).
type Reverter interface {
	RevertDecisions(ctx context.Context, s db.RevertScope, apply bool) (db.RevertReport, error)
}

func runRevert(_ *cobra.Command, _ []string) {
	if _, err := ropts.scope(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := revert(ctx, os.Stdout, database, ropts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func revert(ctx context.Context, out io.Writer, store Reverter, o revertOptions) error {
	s, err := o.scope()
	if err != nil {
		return err
	}
	rep, err := store.RevertDecisions(ctx, s, o.yes)
	if err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("decide revert · %s\n", describeScope(s))
	keys := make([]string, 0, len(rep.Transitions))
	for k := range rep.Transitions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		p("no finding to move\n")
	}
	for _, k := range keys {
		p("  %-22s %d\n", strings.Replace(k, "->", " → ", 1), rep.Transitions[k])
	}
	if rep.DryRun {
		p("decisions to revoke: %d\n\n(dry run) nothing changed — pass --yes to undo.\n", rep.Revoked)
	} else {
		p("moved %d · skipped %d (changed by another writer since listed) · decisions revoked %d · chunks re-flagged %d\n",
			rep.Moved, rep.Skipped, rep.Revoked, rep.Reflagged)
	}
	_, err = io.WriteString(out, b.String())
	return err
}

func describeScope(s db.RevertScope) string {
	var parts []string
	if s.RecipeID != "" {
		parts = append(parts, "recipe "+s.RecipeID)
	}
	if s.FindingID != "" {
		parts = append(parts, "finding "+s.FindingID)
	}
	if !s.Since.IsZero() {
		parts = append(parts, "since "+s.Since.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, " · ")
}
