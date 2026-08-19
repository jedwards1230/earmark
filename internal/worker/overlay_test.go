package worker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/patch"
)

// ─── Correction overlay (CONTRACT §2.17) ─────────────────────────────────────

// capturingChat records the user prompt the judge was given, so a test can
// assert WHICH TEXT the judge actually saw.
type capturingChat struct {
	mu      sync.Mutex
	prompts []string
	resp    string
}

func (c *capturingChat) Complete(_ context.Context, _, user string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prompts = append(c.prompts, user)
	return c.resp, nil
}
func (c *capturingChat) Model() string { return "capturing-judge" }

func (c *capturingChat) seen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.prompts, "\n")
}

const overlayRawText = "the signal was sent from the auto sebo observatory"

func overlayTranscript() *db.Transcript {
	return &db.Transcript{
		ID:       "tid-overlay",
		JobID:    "job-overlay",
		FilePath: "/books/author/title/ch1.mp3",
		RawText:  overlayRawText,
	}
}

// correctionRow builds an accepted-correction row against `chunkText`. Offset is
// left unknown (-1) on purpose: the span is unique, so Locate recovers it, and
// the test does not have to hard-code a rune index.
func correctionRow(id string, chunkIndex int, chunkText, original, correction string) db.CorrectionRow {
	idx := chunkIndex
	corr := correction
	hash := patch.ChunkHash(chunkText)
	return db.CorrectionRow{
		ID:                  id,
		ChunkIndex:          &idx,
		OriginalText:        original,
		SuggestedCorrection: &corr,
		ChunkTextSHA256:     &hash,
	}
}

// pristineChunksFor regenerates the transcript's chunks the same way the worker
// does, so a test can build corrections against the exact text the projection
// will be rebuilt from.
func pristineChunksFor(t *testing.T, w *Worker, tr *db.Transcript, cfg *config.Config) []db.Chunk {
	t.Helper()
	chunks, err := w.chunkTranscript(tr, cfg.ChunkSize, false)
	require.NoError(t, err)
	require.NotEmpty(t, chunks)
	return chunks
}

// TestChunkTranscriptIsPristine: the regeneration step must produce source_text
// == text and query nothing. Corrections are layered on afterwards, by replay.
func TestChunkTranscriptIsPristine(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

	segmented := overlayTranscript()
	segmented.Segments = []db.Segment{
		{ID: 0, Start: 0, End: 2.5, Text: "the signal was sent"},
		{ID: 1, Start: 2.5, End: 5, Text: "from the auto sebo observatory"},
	}

	// Both chunking strategies (segment-derived and the raw-text fallback) must
	// produce pristine chunks.
	for name, tr := range map[string]*db.Transcript{
		"raw-text fallback": overlayTranscript(),
		"from segments":     segmented,
	} {
		t.Run(name, func(t *testing.T) {
			chunks := pristineChunksFor(t, w, tr, &config.Config{ChunkSize: 512})
			for _, c := range chunks {
				require.Equal(t, c.Text, c.SourceText,
					"a freshly regenerated chunk carries no corrections, so source_text == text")
			}
		})
	}
	require.Zero(t, fdb.overlayCalls, "chunkTranscript must not read the overlay")
}

// TestApplyOverlay is the pure replay-onto-chunks helper: it rewrites Text only,
// never SourceText, and reports every patch as applied or stale.
func TestApplyOverlay(t *testing.T) {
	base := []db.Chunk{
		{ChunkIndex: 0, Text: "the auto sebo dish", SourceText: "the auto sebo dish"},
		{ChunkIndex: 1, Text: "a pin name appears", SourceText: "a pin name appears"},
	}

	tests := []struct {
		name        string
		overlay     patch.Overlay
		wantTexts   []string
		wantApplied []string
		wantStale   map[string]string
	}{
		{
			name:      "empty overlay leaves everything alone",
			overlay:   nil,
			wantTexts: []string{"the auto sebo dish", "a pin name appears"},
		},
		{
			name: "corrections land in their own chunks",
			overlay: patch.Overlay{
				0: {{ID: "a", Anchor: patch.Anchor{OriginalText: "auto sebo", Offset: -1, Occurrence: -1}, Correction: "arecibo"}},
				1: {{ID: "b", Anchor: patch.Anchor{OriginalText: "pin name", Offset: -1, Occurrence: -1}, Correction: "pen name"}},
			},
			wantTexts:   []string{"the arecibo dish", "a pen name appears"},
			wantApplied: []string{"a", "b"},
		},
		{
			name: "an unusable correction is quarantined, its chunk stays pristine",
			overlay: patch.Overlay{
				0: {{ID: "gone", Anchor: patch.Anchor{OriginalText: "badger", Offset: -1, Occurrence: -1}, Correction: "otter"}},
			},
			wantTexts: []string{"the auto sebo dish", "a pin name appears"},
			wantStale: map[string]string{"gone": patch.StaleReasonAnchorNotFound},
		},
		{
			// The transcript re-chunked into fewer chunks: a correction pointing
			// at a chunk index that no longer exists has nothing to replay onto.
			// It must be retired, not silently forgotten.
			name: "correction for a chunk index that no longer exists is retired",
			overlay: patch.Overlay{
				7: {{ID: "orphan", Anchor: patch.Anchor{OriginalText: "x", Offset: -1, Occurrence: -1}, Correction: "y"}},
			},
			wantTexts: []string{"the auto sebo dish", "a pin name appears"},
			wantStale: map[string]string{"orphan": patch.StaleReasonChunkChanged},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := make([]db.Chunk, len(base))
			copy(in, base)

			out, applied, stale := applyOverlay(in, tc.overlay)

			require.Len(t, out, len(tc.wantTexts))
			for i, want := range tc.wantTexts {
				require.Equal(t, want, out[i].Text, "chunk %d text", i)
				require.Equal(t, base[i].SourceText, out[i].SourceText,
					"source_text is the projection's input and must never be rewritten")
			}

			gotApplied := make([]string, 0, len(applied))
			for _, a := range applied {
				gotApplied = append(gotApplied, a.ID)
			}
			require.ElementsMatch(t, tc.wantApplied, gotApplied)

			require.Len(t, stale, len(tc.wantStale))
			for _, s := range stale {
				require.Equal(t, tc.wantStale[s.ID], s.Reason, "stale reason for %s", s.ID)
			}

			// The input slice must not be mutated — the caller still needs the
			// pristine chunks (the judge saw them).
			for i := range in {
				require.Equal(t, base[i].Text, in[i].Text, "applyOverlay mutated its input")
			}
		})
	}
}

// TestProcessTranscript_EmbedsCorrectedTextButJudgesPristine is the central
// end-to-end property of the design fork: ONE chunking pass feeds two consumers
// with different text. The judge must see pristine text (or its anchors and
// chunk hash would describe text the projection never produces, making every
// finding stale on arrival); the embedded/searched text must be corrected.
func TestProcessTranscript_EmbedsCorrectedTextButJudgesPristine(t *testing.T) {
	fdb := &fakeDB{}
	chat := &capturingChat{resp: `{"findings":[]}`}
	w := &Worker{
		ctx:   context.Background(),
		db:    fdb,
		log:   log.NewLogger("worker-test"),
		judge: eval.NewJudge(chat),
	}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	pristine := pristineChunksFor(t, w, tr, cfg)
	require.Len(t, pristine, 1, "fixture assumes a single chunk")
	fdb.overlay = []db.CorrectionRow{
		correctionRow("f1", 0, pristine[0].Text, "auto sebo", "arecibo"),
	}

	require.NoError(t, w.processTranscript(cfg, tr))

	// Embedded/searched surface: corrected.
	require.Len(t, fdb.chunks, 1)
	require.Contains(t, fdb.chunks[0].Text, "arecibo")
	require.NotContains(t, fdb.chunks[0].Text, "auto sebo")
	// Projection input: still pristine, so the next rebuild replays from the
	// same base and the judge keeps seeing untouched text.
	require.Equal(t, pristine[0].Text, fdb.chunks[0].SourceText)

	// Judge input: pristine.
	seen := chat.seen()
	require.Contains(t, seen, "auto sebo",
		"the judge must be shown the pristine chunk text")
	require.NotContains(t, seen, "arecibo",
		"the judge must never see the corrected surface — its anchors and "+
			"chunk_text_sha256 are recorded against what it was shown")

	// The applied correction is recorded with SPAN-level before/after.
	require.Len(t, fdb.appliedRecs, 1)
	require.Equal(t, db.AppliedFinding{ID: "f1", Before: "auto sebo", After: "arecibo"}, fdb.appliedRecs[0])
	require.Empty(t, fdb.staleMarks)
}

// TestProcessTranscript_RebuildIsByteIdentical: the projection is thrown away
// and rebuilt on every embed. If replay were not deterministic the searchable
// corpus would drift on each run with no error anywhere.
func TestProcessTranscript_RebuildIsByteIdentical(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	pristine := pristineChunksFor(t, w, tr, cfg)
	fdb.overlay = []db.CorrectionRow{
		correctionRow("f1", 0, pristine[0].Text, "auto sebo", "arecibo"),
		correctionRow("f2", 0, pristine[0].Text, "signal", "message"),
	}

	const rebuilds = 3
	var texts []string
	for i := range rebuilds {
		fdb.chunks = nil
		require.NoError(t, w.processTranscript(cfg, tr), "rebuild %d", i)
		require.Len(t, fdb.chunks, 1)
		texts = append(texts, fdb.chunks[0].Text)
	}
	for i := 1; i < len(texts); i++ {
		require.Equal(t, texts[0], texts[i],
			"rebuild %d is not byte-identical to the first", i)
	}
	require.Contains(t, texts[0], "arecibo")
	require.Contains(t, texts[0], "message")
}

// TestProcessTranscript_OverlayLoadFailureFailsClosed: if the overlay cannot be
// read we must NOT embed pristine text as though it were corrected. In the
// corpus that is indistinguishable from "there were no corrections", so the
// transcript is left un-embedded and retried.
func TestProcessTranscript_OverlayLoadFailureFailsClosed(t *testing.T) {
	fdb := &fakeDB{overlayErr: fmt.Errorf("overlay query exploded")}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

	err := w.processTranscript(&config.Config{ChunkSize: 512}, overlayTranscript())
	require.Error(t, err)
	require.Contains(t, err.Error(), "correction overlay")
	require.Empty(t, fdb.chunks, "nothing may be embedded when the overlay is unknown")
}

// TestProcessTranscript_QuarantinedCorrectionIsRecordedStale: a correction that
// can no longer be placed must be retired WITH A REASON and stay visible. The
// rest of the chunk still embeds.
func TestProcessTranscript_QuarantinedCorrectionIsRecordedStale(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	pristine := pristineChunksFor(t, w, tr, cfg)
	fdb.overlay = []db.CorrectionRow{
		correctionRow("good", 0, pristine[0].Text, "auto sebo", "arecibo"),
		correctionRow("gone", 0, pristine[0].Text, "a span that is not there", "whatever"),
	}

	require.NoError(t, w.processTranscript(cfg, tr))

	require.Len(t, fdb.chunks, 1)
	require.Contains(t, fdb.chunks[0].Text, "arecibo", "the good correction still applies")

	require.Len(t, fdb.appliedRecs, 1)
	require.Equal(t, "good", fdb.appliedRecs[0].ID)

	require.Len(t, fdb.staleMarks, 1)
	require.Equal(t, patch.StaleReasonAnchorNotFound, fdb.staleMarks[0].reason)
	require.Equal(t, []string{"gone"}, fdb.staleMarks[0].ids)
}

// TestProcessTranscript_StaleHashIsNotApplied: a correction recorded against an
// older revision of the chunk must not be replayed onto the current one — the
// judge never reviewed this text.
func TestProcessTranscript_StaleHashIsNotApplied(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	row := correctionRow("f1", 0, "some earlier revision of the chunk", "auto sebo", "arecibo")
	fdb.overlay = []db.CorrectionRow{row}

	require.NoError(t, w.processTranscript(cfg, tr))

	require.Len(t, fdb.chunks, 1)
	require.Contains(t, fdb.chunks[0].Text, "auto sebo", "the stale correction must not land")
	require.Empty(t, fdb.appliedRecs)
	require.Len(t, fdb.staleMarks, 1)
	require.Equal(t, patch.StaleReasonChunkChanged, fdb.staleMarks[0].reason)
}

// TestEmbedTranscript_AppliesOverlay covers the gated two-pass flow's embed
// half: it is the only pass that produces corrected text.
func TestEmbedTranscript_AppliesOverlay(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"), evalGatesEmbed: true}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	pristine, err := w.chunkTranscript(tr, cfg.ChunkSize, true)
	require.NoError(t, err)
	fdb.overlay = []db.CorrectionRow{
		correctionRow("f1", 0, pristine[0].Text, "auto sebo", "arecibo"),
	}

	require.NoError(t, w.embedTranscript(cfg, tr))
	require.Len(t, fdb.chunks, 1)
	require.Contains(t, fdb.chunks[0].Text, "arecibo")
	require.Equal(t, pristine[0].Text, fdb.chunks[0].SourceText)
	require.Equal(t, pristine[0].ID, fdb.chunks[0].ID,
		"the overlay must not disturb the deterministic chunk UUID the eval pass used")
}

// TestEvalTranscript_NeverAppliesOverlay: the eval pass is a judge path. It must
// not read or replay the overlay at all — doing so would hand the judge
// corrected text and make every finding it produces stale on arrival.
func TestEvalTranscript_NeverAppliesOverlay(t *testing.T) {
	fdb := &fakeDB{}
	chat := &capturingChat{resp: `{"findings":[]}`}
	w := &Worker{
		ctx:            context.Background(),
		db:             fdb,
		log:            log.NewLogger("worker-test"),
		judge:          eval.NewJudge(chat),
		evalGatesEmbed: true,
	}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	pristine, err := w.chunkTranscript(tr, cfg.ChunkSize, true)
	require.NoError(t, err)
	fdb.overlay = []db.CorrectionRow{
		correctionRow("f1", 0, pristine[0].Text, "auto sebo", "arecibo"),
	}

	require.NoError(t, w.evalTranscript(cfg, tr))

	require.Zero(t, fdb.overlayCalls, "the eval pass must not load the correction overlay")
	require.Contains(t, chat.seen(), "auto sebo")
	require.NotContains(t, chat.seen(), "arecibo")
	require.Empty(t, fdb.chunks, "the eval pass embeds nothing")
}

// TestPersistReplay_BestEffort: the projection is already correct once replay
// has run, and every rebuild replays the whole overlay from scratch — so a
// failed bookkeeping write must not fail the embed.
func TestPersistReplay_BestEffort(t *testing.T) {
	fdb := &fakeDB{
		appliedErr: fmt.Errorf("applied write failed"),
		staleErr:   fmt.Errorf("stale write failed"),
	}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	cfg := &config.Config{ChunkSize: 512}
	tr := overlayTranscript()

	pristine := pristineChunksFor(t, w, tr, cfg)
	fdb.overlay = []db.CorrectionRow{
		correctionRow("good", 0, pristine[0].Text, "auto sebo", "arecibo"),
		correctionRow("gone", 0, pristine[0].Text, "not present anywhere", "x"),
	}

	require.NoError(t, w.processTranscript(cfg, tr),
		"a bookkeeping failure must not block the embed")
	require.Len(t, fdb.chunks, 1)
	require.Contains(t, fdb.chunks[0].Text, "arecibo")
}
