package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
)

// TestProcessTranscript_ReplayOutcomeNotPersistedBeforeInsert is the ordering
// guard. The replay outcome describes a projection: if the embed or the insert
// fails, that projection was never written, so claiming corrections "applied"
// (with applied_at stamped) — or worse, retiring one to the TERMINAL `stale`
// state — would record something that never happened and cannot be walked back.
func TestProcessTranscript_ReplayOutcomeNotPersistedBeforeInsert(t *testing.T) {
	cfg := &config.Config{ChunkSize: 512}

	tests := []struct {
		name    string
		breakDB func(f *fakeDB)
	}{
		{"embedding fails", func(f *fakeDB) { f.embedErr = fmt.Errorf("embeddings endpoint down") }},
		{"chunk insert fails", func(f *fakeDB) { f.insertErr = fmt.Errorf("insert exploded") }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fdb := &fakeDB{}
			w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
			tr := overlayTranscript()

			pristine := pristineChunksFor(t, w, tr, cfg)
			fdb.overlay = []db.CorrectionRow{
				correctionRow("good", 0, pristine[0].Text, "auto sebo", "arecibo"),
				correctionRow("gone", 0, pristine[0].Text, "not present anywhere", "x"),
			}
			tc.breakDB(fdb)

			require.Error(t, w.processTranscript(cfg, tr))

			require.Empty(t, fdb.chunks, "premise: nothing was inserted")
			require.Empty(t, fdb.appliedRecs,
				"a correction must not be recorded applied against a projection that was never written")
			require.Empty(t, fdb.staleMarks,
				"stale is terminal — it must not be written for a rebuild that failed")
			require.Empty(t, fdb.clearCalls,
				"the rebuild flag is the only record that a rebuild is owed; it must survive a failure")
		})
	}
}

// TestProcessTranscript_ClearsStaleFlagWithOverlayWatermark: the flag is cleared
// after a successful insert, scoped to the transcript, and carries the watermark
// the overlay was READ at — that watermark is what stops the clear from
// swallowing a human decision that landed mid-rebuild.
func TestProcessTranscript_ClearsStaleFlagWithOverlayWatermark(t *testing.T) {
	cfg := &config.Config{ChunkSize: 512}
	watermark := time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC)

	t.Run("with corrections", func(t *testing.T) {
		fdb := &fakeDB{overlayReadAt: watermark}
		w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
		tr := overlayTranscript()

		pristine := pristineChunksFor(t, w, tr, cfg)
		fdb.overlay = []db.CorrectionRow{
			correctionRow("f1", 0, pristine[0].Text, "auto sebo", "arecibo"),
		}

		require.NoError(t, w.processTranscript(cfg, tr))
		require.Len(t, fdb.clearCalls, 1)
		require.Equal(t, tr.ID, fdb.clearCalls[0].transcriptID)
		require.Equal(t, watermark, fdb.clearCalls[0].watermark)
	})

	// Zero corrections is NOT "nothing to do": a revert removes the last
	// correction from a chunk and leaves it flagged, so the rebuild still has to
	// run and still has to clear the flag.
	t.Run("with no corrections at all", func(t *testing.T) {
		fdb := &fakeDB{overlayReadAt: watermark}
		w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

		require.NoError(t, w.processTranscript(cfg, overlayTranscript()))
		require.Len(t, fdb.clearCalls, 1,
			"a rebuild with an empty overlay must still clear the flag")
		require.Equal(t, watermark, fdb.clearCalls[0].watermark)
	})

	t.Run("not cleared when the overlay could not be read", func(t *testing.T) {
		fdb := &fakeDB{overlayErr: fmt.Errorf("overlay query exploded")}
		w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

		require.Error(t, w.processTranscript(cfg, overlayTranscript()))
		require.Empty(t, fdb.clearCalls,
			"an unknown overlay must leave the chunk flagged for another attempt")
	})
}

// TestRebuildStaleTranscripts covers the trigger that closes the
// accept → replay → re-embed loop. Without it an accepted correction is
// unreachable in production: every other selection requires a transcript to
// have NO chunks, and findings only exist for transcripts that already do.
func TestRebuildStaleTranscripts(t *testing.T) {
	cfg := &config.Config{ChunkSize: 512, EmbedBatchSize: 4}

	t.Run("re-embeds the corrected projection without re-judging", func(t *testing.T) {
		fdb := &fakeDB{}
		chat := &capturingChat{resp: `{"findings":[]}`}
		w := &Worker{
			ctx:   context.Background(),
			db:    fdb,
			log:   log.NewLogger("worker-test"),
			judge: eval.NewJudge(chat),
		}
		tr := overlayTranscript()
		fdb.staleTranscripts = []*db.Transcript{tr}

		pristine, err := w.chunkTranscript(tr, cfg.ChunkSize, true)
		require.NoError(t, err)
		fdb.overlay = []db.CorrectionRow{
			correctionRow("f1", 0, pristine[0].Text, "auto sebo", "arecibo"),
		}

		require.Equal(t, 1, w.rebuildStaleTranscripts(cfg))

		require.Len(t, fdb.chunks, 1)
		require.Contains(t, fdb.chunks[0].Text, "arecibo",
			"the rebuild must publish the corrected text")
		require.Equal(t, pristine[0].Text, fdb.chunks[0].SourceText,
			"the rebuild regenerates from source; source_text stays pristine")
		require.Len(t, fdb.appliedRecs, 1)
		require.Len(t, fdb.clearCalls, 1, "a successful rebuild clears the flag")

		// The rebuild path must not route through the judge: a stale-driven
		// re-embed does not require re-judging (CONTRACT §2.17), and judging the
		// corrected surface would poison every finding it produced.
		require.Empty(t, chat.seen(), "the rebuild pass must never call the judge")
		require.Empty(t, fdb.findings)
		require.Empty(t, fdb.evalMetrics, "a rebuild must not disturb the eval gate's latch")
	})

	t.Run("bounded by the embed batch size", func(t *testing.T) {
		fdb := &fakeDB{}
		w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
		for i := range 10 {
			tr := overlayTranscript()
			tr.ID = fmt.Sprintf("tid-%d", i)
			fdb.staleTranscripts = append(fdb.staleTranscripts, tr)
		}

		require.Equal(t, cfg.EmbedBatchSize, w.rebuildStaleTranscripts(cfg),
			"the rebuild backlog must be bounded like every other selection")
	})

	t.Run("selection error is logged, not fatal", func(t *testing.T) {
		fdb := &fakeDB{staleSelectErr: fmt.Errorf("select exploded")}
		w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

		require.Zero(t, w.rebuildStaleTranscripts(cfg))
		require.Empty(t, fdb.chunks)
	})

	t.Run("nothing flagged is a no-op", func(t *testing.T) {
		fdb := &fakeDB{}
		w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

		require.Zero(t, w.rebuildStaleTranscripts(cfg))
		require.Equal(t, 1, fdb.staleSelectCalls)
		require.Zero(t, fdb.overlayCalls, "no rebuild means no overlay read")
	})
}
