// Package asrpilot implements `earmark asr-pilot`: the second-ASR pilot's
// asr_vote recorder (`vote`) and measurement report (`report`), CONTRACT
// §1.11. The pure logic is internal/asrpilot; this package only loads one
// track at a time, in bounded pages, and prints.
package asrpilot

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

// migrateWarning is repeated in every subcommand's help: these commands are
// for a pilot, and a binary built from an unreleased branch must never touch
// the production database.
const migrateWarning = `Like every earmark command it connects through db.New, which APPLIES ANY
PENDING SCHEMA MIGRATION on connect — even for a dry run or the read-only
report. Never run a binary built from an unmerged branch against the
production database; use the deployed release.`

// ASRPilotCmd is `earmark asr-pilot`.
var ASRPilotCmd = &cobra.Command{
	Use:   "asr-pilot",
	Short: "Second-ASR pilot: record asr_vote evidence and report pilot measurements",
	Long: `The second-ASR pilot (CONTRACT §1.11) transcribes tracks that already have a
production transcript with another model (runner/pilot.py writes
pilot_transcripts). These subcommands read those transcripts:

  vote     what each pilot model heard at every held finding's span
           (finding_asr_votes; dry-run unless --yes)
  report   WER vs the production model, casing/punctuation, RTFx, peak VRAM,
           held findings resolved per model and two-model vote agreement

Nothing here changes a finding, a decision or the decide policy; decide does
not read these tables.

` + migrateWarning,
}

func init() {
	ASRPilotCmd.AddCommand(VoteCmd)
	ASRPilotCmd.AddCommand(ReportCmd)
}

// pilotReader is the read slice both subcommands share (*db.DB implements it).
type pilotReader interface {
	PilotTranscripts(ctx context.Context, f db.PilotFilter) ([]db.PilotTranscript, error)
	PilotSegments(ctx context.Context, id int64) ([]db.PilotSegment, error)
	GetTranscriptSegments(ctx context.Context, ids []string) (map[string][]db.Segment, error)
	BookDirsForASINs(ctx context.Context, asins []string) ([]string, error)
}

// connect is the shared Run prologue: config, database, signal context.
func connect(run func(ctx context.Context, d *db.DB) error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, database)
	stop()
	database.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// splitList parses a comma-separated flag value, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// bookDirs resolves --asin / --book-dir to book directories. required makes
// one of them mandatory.
func bookDirs(ctx context.Context, st pilotReader, asins, bookDir string, required bool) ([]string, error) {
	list := splitList(asins)
	switch {
	case len(list) > 0 && bookDir != "":
		return nil, fmt.Errorf("--asin and --book-dir are mutually exclusive")
	case bookDir != "":
		return []string{strings.TrimRight(bookDir, "/")}, nil
	case len(list) == 0:
		if required {
			return nil, fmt.Errorf("one of --asin or --book-dir is required")
		}
		return nil, nil
	}
	dirs, err := st.BookDirsForASINs(ctx, list)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no book_metadata row for asin(s) %s", strings.Join(list, ","))
	}
	return dirs, nil
}

// listPilots pages through every pilot transcript matching f (metadata only;
// a pilot has at most a few hundred rows).
func listPilots(ctx context.Context, st pilotReader, f db.PilotFilter) ([]db.PilotTranscript, error) {
	f.Limit = db.MaxPilotPage
	var out []db.PilotTranscript
	for {
		page, err := st.PilotTranscripts(ctx, f)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < f.Limit {
			return out, nil
		}
		f.AfterID = page[len(page)-1].ID
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// modelKey names a model row: id@revision (revision shortened to 12 chars,
// HF commit shas are 40).
func modelKey(modelID, revision string) string {
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return modelID + "@" + revision
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
