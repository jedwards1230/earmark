package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// Demo fixture for the correction-review tools (CONTRACT §2.17).
//
// The three review tools are the only MCP tools that write, which makes them
// the ones most worth exercising WITHOUT a database — an agent (or a person)
// can point a client at `earmark mcp --demo` and drive the whole loop: list a
// proposed correction, accept it, hand-author another, and watch the anchor
// resolution and refusals behave exactly as they will against Postgres.
//
// The fixture is deliberately honest rather than canned: chunk fingerprints are
// computed with patch.ChunkHash over the fixture text, and every anchor is
// resolved by the real patch.Locate/Replay. A span that does not occur in the
// demo chunk text is refused here exactly as it would be in production.
//
// Mutations are held in a process-global store because demoDB is a value type
// (its production fields use the same heap-backed trick) and `--demo` is a
// single process serving one fixture. They are lost on restart, which is the
// correct lifetime for a fixture.

// demoChunk is one fixture chunk: the pristine text a correction anchors to.
type demoChunk struct {
	id           string
	transcriptID string
	filePath     string
	chunkIndex   int
	startSec     float64
	endSec       float64
	text         string
}

// demoCorrectionStore holds the fixture's mutable correction state.
type demoCorrectionStore struct {
	mu   sync.Mutex
	rows []db.CorrectionDetail
	next int
}

var (
	demoCorrectionsOnce sync.Once
	demoCorrectionState *demoCorrectionStore
)

// demoChunks are the fixture chunks the demo corrections anchor into. The text
// contains the flagged spans verbatim ("auto sebo" twice, so the ambiguous-
// anchor path is reachable), which is what makes the demo exercise the real
// resolution ladder instead of a happy path.
var demoChunks = []demoChunk{
	{
		id:           "11111111-1111-4111-8111-111111111111",
		transcriptID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa",
		filePath:     "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]/Project Hail Mary.m4b",
		chunkIndex:   0,
		startSec:     60,
		endSec:       120,
		text: "The signal came from auto sebo, the great dish in the Puerto Rican jungle. " +
			"Nobody had listened to auto sebo in years, and now it was talking back.",
	},
	{
		id:           "22222222-2222-4222-8222-222222222222",
		transcriptID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa",
		filePath:     "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]/Project Hail Mary.m4b",
		chunkIndex:   4,
		startSec:     240,
		endSec:       300,
		text: "He wrote under a pin name for most of his career, publishing three hundred " +
			"and twelve papers before anyone learned who he was.",
	},
}

func demoChunkByID(id string) *demoChunk {
	for i := range demoChunks {
		if demoChunks[i].id == id {
			return &demoChunks[i]
		}
	}
	return nil
}

// demoCorrections returns the process-wide fixture store, seeding it once.
func demoCorrections() *demoCorrectionStore {
	demoCorrectionsOnce.Do(func() {
		demoCorrectionState = &demoCorrectionStore{rows: seedDemoCorrections()}
	})
	return demoCorrectionState
}

// seedDemoCorrections builds the starting rows: one clean proposal, one already
// accepted (awaiting the rebuild), and one whose anchor is ambiguous — the row
// that teaches a reviewer why `occurrence` exists.
func seedDemoCorrections() []db.CorrectionDetail {
	now := time.Now().UTC()
	c0, c4 := demoChunks[0], demoChunks[1]
	hash0, hash4 := patch.ChunkHash(c0.text), patch.ChunkHash(c4.text)

	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }

	return []db.CorrectionDetail{
		{
			ID: "f0000000-0000-4000-8000-000000000001", TranscriptID: c0.transcriptID,
			FilePath: c0.filePath, BookDir: demoBookDir(c0.filePath),
			ChunkID: str(c0.id), ChunkIndex: num(c0.chunkIndex),
			StartSec: 73.5, EndSec: 81, OriginalText: "auto sebo",
			SuggestedCorrection: str("Arecibo"), IssueType: "misheard_proper_noun",
			Confidence: 0.92, Model: "qwen2.5-14b-instruct", Origin: db.OriginJudge,
			PatchState: patch.StateProposed, CreatedAt: now.Add(-2 * time.Hour),
			AnchorOffset: num(strings.Index(c0.text, "auto sebo")), AnchorOccurrence: num(0),
			ChunkTextSHA256: str(hash0), ChunkText: c0.text,
		},
		{
			ID: "f0000000-0000-4000-8000-000000000002", TranscriptID: c4.transcriptID,
			FilePath: c4.filePath, BookDir: demoBookDir(c4.filePath),
			ChunkID: str(c4.id), ChunkIndex: num(c4.chunkIndex),
			StartSec: 244, EndSec: 251, OriginalText: "pin name",
			SuggestedCorrection: str("pen name"), IssueType: "homophone",
			Confidence: 0.81, Model: "qwen2.5-14b-instruct", Origin: db.OriginJudge,
			PatchState: patch.StateAccepted, CreatedAt: now.Add(-90 * time.Minute),
			DecidedAt: timePtr(now.Add(-20 * time.Minute)), DecidedBy: str("mcp:agent"),
			AnchorOffset: num(strings.Index(c4.text, "pin name")), AnchorOccurrence: num(0),
			ChunkTextSHA256: str(hash4), ChunkText: c4.text,
		},
		{
			// No recorded anchor and two identical spans in the chunk: this row
			// resolves to anchor_ambiguous, so the demo shows a correction that can
			// never be applied as-is.
			ID: "f0000000-0000-4000-8000-000000000003", TranscriptID: c0.transcriptID,
			FilePath: c0.filePath, BookDir: demoBookDir(c0.filePath),
			ChunkID: str(c0.id), ChunkIndex: num(c0.chunkIndex),
			StartSec: 96, EndSec: 103, OriginalText: "auto sebo",
			SuggestedCorrection: str("Arecibo"), IssueType: "misheard_proper_noun",
			Confidence: 0.64, Model: "qwen2.5-14b-instruct", Origin: db.OriginJudge,
			PatchState: patch.StateProposed, CreatedAt: now.Add(-time.Hour),
			ChunkTextSHA256: str(hash0), ChunkText: c0.text,
		},
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// demoBookDir strips the track filename, matching the SQL the real query uses.
func demoBookDir(filePath string) string {
	if i := strings.LastIndex(filePath, "/"); i > 0 {
		return filePath[:i]
	}
	return filePath
}

// ListCorrections serves the demo review worklist, applying the same filters
// the SQL does so a client sees consistent paging and scoping.
func (d demoDB) ListCorrections(_ context.Context, f db.CorrectionFilter) ([]db.CorrectionDetail, error) {
	if d.scenario == "empty" {
		return nil, nil
	}
	s := demoCorrections()
	s.mu.Lock()
	defer s.mu.Unlock()

	states := make(map[string]bool, len(f.States))
	for _, st := range f.States {
		states[st] = true
	}

	var out []db.CorrectionDetail
	for _, r := range s.rows {
		if f.ID != "" && r.ID != f.ID {
			continue
		}
		if len(states) > 0 && !states[r.PatchState] {
			continue
		}
		if p := strings.TrimRight(f.Path, "/"); p != "" &&
			r.FilePath != p && !strings.HasPrefix(r.FilePath, p+"/") {
			continue
		}
		if r.Confidence < f.MinConfidence {
			continue
		}
		out = append(out, r)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if f.Offset >= len(out) {
		return nil, nil
	}
	out = out[f.Offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetCorrectionOverlay returns the demo transcript's replayable corrections —
// the accepted/applied rows, exactly the set the real query selects.
func (d demoDB) GetCorrectionOverlay(_ context.Context, transcriptID string) ([]db.CorrectionRow, time.Time, error) {
	s := demoCorrections()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []db.CorrectionRow
	for _, r := range s.rows {
		if r.TranscriptID != transcriptID {
			continue
		}
		if r.PatchState != patch.StateAccepted && r.PatchState != patch.StateApplied {
			continue
		}
		out = append(out, db.CorrectionRow{
			ID: r.ID, ChunkIndex: r.ChunkIndex, OriginalText: r.OriginalText,
			SuggestedCorrection: r.SuggestedCorrection,
			AnchorOffset:        r.AnchorOffset, AnchorOccurrence: r.AnchorOccurrence,
			ChunkTextSHA256: r.ChunkTextSHA256,
		})
	}
	return out, time.Now().UTC(), nil
}

// GetChunkForEdit returns a fixture chunk with its pristine text.
func (d demoDB) GetChunkForEdit(_ context.Context, chunkID string) (*db.ChunkTarget, error) {
	c := demoChunkByID(chunkID)
	if c == nil {
		return nil, fmt.Errorf("%w: %s", db.ErrChunkNotFound, chunkID)
	}
	return &db.ChunkTarget{
		ID: c.id, TranscriptID: c.transcriptID, FilePath: c.filePath,
		ChunkIndex: c.chunkIndex, StartSec: c.startSec, EndSec: c.endSec,
		Pristine: c.text, Corrected: c.text,
	}, nil
}

// SetPatchState mutates the fixture through the SAME gates the database
// enforces: the transition must be legal, and the row must still be in the
// expected state. A demo that accepted anything would teach an agent the wrong
// thing about the tool.
func (d demoDB) SetPatchState(_ context.Context, id, from, to, decidedBy string) error {
	if !patch.CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s (finding %s)", db.ErrIllegalTransition, from, to, id)
	}
	s := demoCorrections()
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.rows {
		if s.rows[i].ID != id {
			continue
		}
		if s.rows[i].PatchState != from {
			return fmt.Errorf("%w: finding %s is not in state %q", db.ErrPatchStateConflict, id, from)
		}
		s.rows[i].PatchState = to
		switch to {
		case patch.StateAccepted, patch.StateRejected, patch.StateReverted:
			now := time.Now().UTC()
			s.rows[i].DecidedAt = &now
			by := decidedBy
			s.rows[i].DecidedBy = &by
		}
		return nil
	}
	return fmt.Errorf("%w: finding %s not found", db.ErrPatchStateConflict, id)
}

// InsertManualCorrection appends a human-authored correction to the fixture,
// re-checking the chunk fingerprint the way the guarded INSERT does.
func (d demoDB) InsertManualCorrection(_ context.Context, m db.ManualCorrection) (string, error) {
	c := demoChunkByID(m.ChunkID)
	if c == nil || patch.ChunkHash(c.text) != m.ChunkSHA256 {
		return "", fmt.Errorf("%w: chunk %s moved under the edit", patch.ErrChunkChanged, m.ChunkID)
	}

	s := demoCorrections()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.next++
	now := time.Now().UTC()
	str := func(v string) *string { return &v }
	num := func(n int) *int { return &n }
	row := db.CorrectionDetail{
		ID:           fmt.Sprintf("f0000000-0000-4000-8000-0000000001%02d", s.next),
		TranscriptID: c.transcriptID, FilePath: c.filePath, BookDir: demoBookDir(c.filePath),
		ChunkID: str(c.id), ChunkIndex: num(c.chunkIndex),
		StartSec: c.startSec, EndSec: c.endSec,
		OriginalText: m.OriginalText, SuggestedCorrection: str(m.Correction),
		IssueType: m.IssueType, Confidence: db.ManualEditConfidence,
		Model: db.ManualEditModel, Origin: db.OriginHuman,
		PatchState: patch.StateAccepted, CreatedAt: now,
		DecidedAt: &now, DecidedBy: str(m.DecidedBy),
		AnchorOffset: num(m.AnchorOffset), AnchorOccurrence: num(m.AnchorOccurrence),
		ChunkTextSHA256: str(m.ChunkSHA256), ChunkText: c.text,
	}
	s.rows = append(s.rows, row)
	return row.ID, nil
}
