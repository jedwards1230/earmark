// Package prunechunks implements `earmark prune-chunks`: a one-off cleanup of
// the orphan chunk rows a stale rebuild left behind before InsertChunks pruned
// them itself (CONTRACT §2.17).
//
// A rebuild re-chunks a transcript and upserts on (transcript_id, chunk_index).
// When the re-chunk yields FEWER chunks than the stored projection, the old
// tail (chunk_index >= the new count) used to survive: stale text that still
// matched searches and still carried findings. InsertChunks now prunes it in
// the same transaction; this command finds and prunes the tails written
// before that fix.
//
// It mirrors requeue's ergonomics: a dry-run preview unless --yes.
package prunechunks

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/worker"
	"github.com/spf13/cobra"
)

// Pruner is the slice of db.DB this command needs (kept small for testing).
type Pruner interface {
	GetChunkedTranscripts(ctx context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error)
	GetStoredChunkHashes(ctx context.Context, transcriptID string) ([]db.StoredChunkHash, error)
	PruneChunks(ctx context.Context, transcriptID string, keep int) (db.PruneResult, error)
}

type options struct {
	yes bool // apply; without it the command is a dry-run preview
	// pageSize is the keyset page size (transcripts loaded per query); 0 → default.
	pageSize int
}

var opts options

// PruneChunksCmd is the cobra command registered in main.
var PruneChunksCmd = &cobra.Command{
	Use:   "prune-chunks",
	Short: "Delete orphan chunk rows a re-chunk left behind (dry-run unless --yes)",
	Long: `Find transcripts whose stored chunks extend past what the current chunker
produces for them — the orphan tail a stale rebuild left behind when it
re-chunked into FEWER chunks — and delete that tail.

For every transcript with chunks, the transcript is re-chunked exactly as the
embed worker does (same CHUNK_SIZE, pristine text) and compared, by pristine
text hash, with the stored rows:

  match    stored rows == the re-chunk                    nothing to do
  orphans  rows 0..n-1 match the re-chunk, rows >= n exist   prune rows >= n
  drift    a row 0..n-1 differs (embedded under another
           chunking or CHUNK_SIZE)                         NOT pruned — use
                                                           requeue --reembed
  short    fewer rows than the re-chunk                    NOT pruned

Only "orphans" transcripts are ever touched. Pruning deletes the tail chunk
rows and moves the findings addressed to them (chunk_index >= the kept count;
proposed/accepted/applied) to patch_state 'stale' (reason chunk_changed) —
findings are never deleted, and rejected/reverted decisions are left as they
are. Each transcript is pruned in its own transaction.

Run it BEFORE any 'earmark eval --backfill-*': the backfill judges stored rows,
including an orphan tail.

Run it with the deployment's own environment (CHUNK_SIZE in particular): a
different chunk size classifies every transcript as drift and prunes nothing.

Examples:
  earmark prune-chunks          # preview
  earmark prune-chunks --yes    # prune`,
	Run: runPrune,
}

func init() {
	PruneChunksCmd.Flags().BoolVar(&opts.yes, "yes", false, "apply the prune (otherwise dry-run preview)")
}

func runPrune(_ *cobra.Command, _ []string) {
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

	// Ctrl-C / SIGTERM stops the walk between transcripts. Each prune is its
	// own transaction, so an interrupted run leaves no half-pruned transcript.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Stdout, database, cfg.ChunkSize, opts); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// layout is how a transcript's stored chunks compare with its re-chunk.
type layout int

const (
	layoutMatch   layout = iota // stored rows are exactly the re-chunk
	layoutOrphans               // rows 0..n-1 match; rows >= n are orphans
	layoutDrift                 // a row 0..n-1 differs or is missing
	layoutShort                 // fewer rows than the re-chunk, prefix matches
)

// classify compares the re-chunk's pristine-text hashes (expected[i] is chunk
// i's hash) with the stored rows. It returns the verdict and how many stored
// rows sit at chunk_index >= len(expected).
//
// Orphans are declared ONLY when every row 0..n-1 is present and hashes to the
// re-chunk: that is the signature of a rebuild that rewrote the head and left
// the tail. Any mismatch in the head means the transcript was embedded under a
// different chunking, and its "extra" rows are real coverage, not orphans.
func classify(expected []string, stored []db.StoredChunkHash) (layout, int) {
	n := len(expected)
	head := make(map[int]string, n)
	tail := 0
	for _, s := range stored {
		if s.ChunkIndex >= n {
			tail++
			continue
		}
		head[s.ChunkIndex] = s.SHA256
	}
	matched := 0
	for i := 0; i < n; i++ {
		h, ok := head[i]
		if !ok {
			continue
		}
		if h != expected[i] {
			return layoutDrift, tail
		}
		matched++
	}
	switch {
	case matched < n && tail > 0:
		return layoutDrift, tail // a hole in the head plus a tail: not a clean rebuild
	case matched < n:
		return layoutShort, 0
	case tail > 0:
		return layoutOrphans, tail
	default:
		return layoutMatch, 0
	}
}

// expectedHashes re-chunks t exactly as the embed worker does and returns each
// chunk's pristine-text hash, in chunk_index order.
func expectedHashes(t *db.Transcript, chunkSize int) ([]string, error) {
	chunks, err := worker.PristineChunks(t, chunkSize)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = patch.ChunkHash(c.SourceText)
	}
	return out, nil
}

const defaultPageSize = 32

// run walks every chunked transcript in keyset pages and prunes (under --yes)
// the orphan tails.
func run(ctx context.Context, out io.Writer, p Pruner, chunkSize int, o options) error {
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	pageSize := o.pageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}

	var scanned, matched, orphanT, orphanC, driftT, shortT, unchunkable int
	var prunedC, retiredF int
	var cursor db.TranscriptCursor
	for {
		page, err := p.GetChunkedTranscripts(ctx, cursor, pageSize)
		if err != nil {
			return fmt.Errorf("query chunked transcripts: %w", err)
		}
		for _, t := range page {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cursor = db.CursorAfter(t)
			scanned++
			name := filepath.Base(t.FilePath)

			expected, err := expectedHashes(t, chunkSize)
			if err != nil {
				// Never prune against an empty re-chunk.
				unchunkable++
				pf("  skip %s: %v\n", name, err)
				continue
			}
			stored, err := p.GetStoredChunkHashes(ctx, t.ID)
			if err != nil {
				return fmt.Errorf("read stored chunks of %s: %w", t.FilePath, err)
			}
			verdict, tail := classify(expected, stored)
			switch verdict {
			case layoutMatch:
				matched++
			case layoutShort:
				shortT++
			case layoutDrift:
				driftT++
				pf("  drift %s: %d stored vs %d re-chunked, content differs — not pruned (requeue --reembed)\n",
					name, len(stored), len(expected))
			case layoutOrphans:
				orphanT++
				orphanC += tail
				if !o.yes {
					pf("  [dry-run] %s: keep %d, prune %d orphan chunk(s)\n", name, len(expected), tail)
					continue
				}
				r, err := p.PruneChunks(ctx, t.ID, len(expected))
				if err != nil {
					return fmt.Errorf("prune %s: %w", t.FilePath, err)
				}
				prunedC += r.Chunks
				retiredF += r.Findings
				pf("  pruned %s: kept %d, deleted %d chunk(s), retired %d finding(s) to stale\n",
					name, len(expected), r.Chunks, r.Findings)
			}
		}
		if len(page) < pageSize {
			break
		}
	}

	pf("\nScanned %d chunked transcript(s) at CHUNK_SIZE=%d: %d match, %d with orphans (%d orphan chunk(s)), %d drift, %d short, %d unchunkable.\n",
		scanned, chunkSize, matched, orphanT, orphanC, driftT, shortT, unchunkable)
	if orphanT == 0 {
		pf("No orphan chunks — nothing to prune.\n")
		return nil
	}
	if !o.yes {
		pf("(dry-run) pass --yes to delete %d orphan chunk(s) across %d transcript(s).\n", orphanC, orphanT)
		return nil
	}
	pf("Pruned %d chunk(s); retired %d finding(s) to stale (chunk_changed).\n", prunedC, retiredF)
	return nil
}
