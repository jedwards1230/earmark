// Package backfill implements the `earmark backfill-metadata` command.
// It iterates over every known book directory (via GetBookSummaries) and runs
// the configured MetadataProvider.Lookup for each, UPSERTing any new metadata
// (chapters, narrator, series, ASIN) into book_metadata — so already-transcribed
// books gain ABS chapter data without requiring re-transcription.
//
// Usage:
//
//	earmark backfill-metadata              # preview (dry-run): show matches
//	earmark backfill-metadata --yes        # apply UPSERTs
//	earmark backfill-metadata --book "Hail Mary" --yes  # scope to one book
package backfill

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/spf13/cobra"
)

// Backfiller is the subset of db.DB needed by this command (kept narrow for testing).
type Backfiller interface {
	GetBookSummaries(ctx context.Context, f db.BookFilter) ([]db.BookSummary, int, error)
	UpsertBookMetadata(ctx context.Context, bookDir string, meta metaprovider.BookMeta) error
}

type options struct {
	book string // optional substring filter for book path/title
	yes  bool   // apply (otherwise dry-run preview)
}

var opts options

// BackfillCmd is the cobra command registered in main.
var BackfillCmd = &cobra.Command{
	Use:   "backfill-metadata",
	Short: "Backfill ABS metadata (chapters, narrator, series, ASIN) for already-transcribed books",
	Long: `Walk every book directory known to the service and re-run the configured
MetadataProvider to populate narrator, series, ASIN, and chapters into
book_metadata — without re-transcribing audio.

This is primarily useful after enabling the ABS provider for a library that
was transcribed before ABS integration: run once to seed all book_metadata rows
with chapter data so search results display chapter labels immediately.

The command is a dry-run by default — pass --yes to apply the UPSERTs.

The METADATA_PROVIDER, ABS_URL, ABS_TOKEN, and ABS_LIBRARY_ID environment
variables are read exactly as they are by the monitor service. Set them the
same way as you would for the running container.

Examples:
  earmark backfill-metadata                               # preview all books
  earmark backfill-metadata --yes                         # backfill all books
  earmark backfill-metadata --book "Project Hail Mary"    # preview one book
  earmark backfill-metadata --book "Project Hail Mary" --yes  # backfill one book`,
	Run: runBackfill,
}

func init() {
	BackfillCmd.Flags().StringVar(&opts.book, "book", "", "optional substring filter for book path/title")
	BackfillCmd.Flags().BoolVar(&opts.yes, "yes", false, "apply UPSERTs (otherwise dry-run preview)")
}

func runBackfill(cmd *cobra.Command, args []string) {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	database, err := db.New(cfg)
	if err != nil {
		fmt.Printf("connect to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	provider := metaprovider.New(cfg)

	if err := run(context.Background(), os.Stdout, database, provider, opts); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

// run holds the testable logic. It paginates through all book summaries,
// calls Lookup for each, and either previews or applies the UPSERT.
func run(ctx context.Context, out io.Writer, q Backfiller, prov metaprovider.MetadataProvider, o options) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	// Collect all matching books (paginate in chunks of 100).
	var books []db.BookSummary
	const pageSize = 100
	offset := 0
	for {
		page, _, err := q.GetBookSummaries(ctx, db.BookFilter{
			Query:  o.book,
			Limit:  pageSize,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("list books: %w", err)
		}
		books = append(books, page...)
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}

	if len(books) == 0 {
		p("No matching books.\n")
		return nil
	}

	if !o.yes {
		p("%d book(s) would be backfilled (dry-run — pass --yes to apply):\n", len(books))
		for _, b := range books {
			p("  %s\n", b.Dir)
		}
		return nil
	}

	p("Backfilling metadata for %d book(s)...\n", len(books))
	var succeeded, skipped, failed int

	for _, book := range books {
		// Pick any sample file for the provider — the provider only needs the
		// directory (for ASIN extraction) but PathProvider also uses the filename.
		sampleName := filepath.Base(book.SamplePath)
		// Reconstruct a file path inside the book dir so Lookup gets the full path.
		filePath := filepath.Join(book.Dir, sampleName)

		meta, err := prov.Lookup(ctx, filePath, sampleName)
		if err != nil {
			p("  WARN  %s — provider error: %v\n", book.Dir, err)
			failed++
			continue
		}
		if !isNonEmpty(meta) {
			p("  SKIP  %s — provider returned no metadata\n", book.Dir)
			skipped++
			continue
		}

		var info []string
		if meta.ASIN != "" {
			info = append(info, "asin:"+meta.ASIN)
		}
		if len(meta.Chapters) > 0 {
			info = append(info, fmt.Sprintf("%d chapters", len(meta.Chapters)))
		}
		if meta.Narrator != "" {
			info = append(info, "narrator:"+meta.Narrator)
		}

		if err := q.UpsertBookMetadata(ctx, book.Dir, meta); err != nil {
			p("  ERROR %s — DB write failed: %v\n", book.Dir, err)
			failed++
			continue
		}
		infoStr := ""
		if len(info) > 0 {
			infoStr = " (" + strings.Join(info, ", ") + ")"
		}
		p("  OK    %s%s\n", book.Dir, infoStr)
		succeeded++
	}

	p("\nDone: %d updated, %d skipped (no metadata), %d errors.\n", succeeded, skipped, failed)
	return nil
}

// isNonEmpty mirrors the chain provider's non-empty check.
func isNonEmpty(m metaprovider.BookMeta) bool {
	return m.Title != "" || m.Author != "" || len(m.Chapters) > 0
}
