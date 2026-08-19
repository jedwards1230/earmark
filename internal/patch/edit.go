package patch

import (
	"fmt"
	"strings"
)

// Direct (human-authored) edits.
//
// A reviewer sometimes wants a correction the judge never proposed. That edit
// cannot be a text rewrite: transcript_chunks is a disposable projection, so
// text written there is destroyed by the next rebuild (CONTRACT §2.17). A
// direct edit is therefore expressed as exactly the same thing an accepted
// finding is — a durable, replayable correction anchored to a chunk revision —
// and it must clear exactly the same gates before it is recorded:
//
//	non-empty correction → chunk hash matches → anchor resolves unambiguously
//	→ span does not overlap an already-accepted correction
//
// PlanDirectEdit runs all four, purely, so the write path can refuse before it
// inserts anything. Skipping any of them is the silent-corruption class the
// overlay exists to prevent: an unverified hand edit anchors to text nobody
// checked, and either desyncs the embedding from the text or quietly vanishes
// on the next rebuild.

// EditPlan is the verified, ready-to-record form of a direct edit.
//
// Span and Occurrence are SERVER-COMPUTED from the pristine chunk text — never
// the caller's word for where the span is. A caller-reported offset is only
// ever a hint fed into Locate's resolution ladder (a real model once reported
// 27 for a span at 29), so what gets persisted is what Locate actually found.
type EditPlan struct {
	// Span is the located target region, in rune indices into the pristine text.
	Span Span
	// Occurrence is the 0-based index of Span among identical spans in the
	// pristine text. Persisted so a later rebuild can re-resolve the anchor even
	// if offsets shift.
	Occurrence int
	// ChunkHash fingerprints the pristine text the edit was verified against.
	// Persisted as chunk_text_sha256, so a rebuild refuses to replay onto a
	// revision this edit never saw.
	ChunkHash string
	// Preview is the chunk text as the next rebuild would produce it: the
	// pristine text with the existing overlay AND this edit replayed onto it.
	Preview string
	// PreviewStale lists existing corrections that would NOT replay onto the
	// current pristine text. They are already in that condition — this edit does
	// not cause it — but surfacing them stops a reviewer reading Preview as
	// "every correction on this chunk landed".
	PreviewStale []StaleRef
}

// ErrEditConflict means the edit's span overlaps a correction that is already
// accepted or applied on this chunk. Composing two edits over the same
// characters has no well-defined meaning (ErrOverlappingPatches is the same
// refusal on the replay side), so the edit is refused rather than recorded and
// left to quarantine both at rebuild time.
var ErrEditConflict = fmt.Errorf("%w: direct edit overlaps an existing correction", ErrOverlappingPatches)

// PlanDirectEdit verifies a human-authored edit against the pristine chunk text
// and the corrections already accepted on that chunk.
//
// pristine MUST be the chunk's pristine text — COALESCE(source_text, text) —
// never the corrected surface. Anchoring an edit to corrected text records a
// hash the projection's input can never match, so the edit would be born stale.
//
// existing is the chunk's current overlay (its accepted + applied findings).
// Pure: no database, no I/O, no clock.
func PlanDirectEdit(pristine string, existing []Patch, edit Patch) (EditPlan, error) {
	if strings.TrimSpace(edit.Correction) == "" {
		return EditPlan{}, ErrEmptyCorrection
	}

	hash := ChunkHash(pristine)
	// A caller may pin the revision it read. Refusing here turns a concurrent
	// rebuild into a retry rather than an edit anchored to text that moved.
	if edit.ChunkHash != "" && edit.ChunkHash != hash {
		return EditPlan{}, ErrChunkChanged
	}

	span, err := Locate(pristine, edit.Anchor)
	if err != nil {
		return EditPlan{}, err
	}

	occ := occurrenceOf(pristine, edit.Anchor.OriginalText, span.Start)
	if occ < 0 {
		// Unreachable: Locate only returns spans it matched in this text.
		return EditPlan{}, ErrAnchorNotFound
	}

	// Overlap is checked against the corrections that would ACTUALLY replay onto
	// this text, not every stored row: a correction already quarantined by the
	// hash or anchor checks occupies no span, so treating it as a conflict would
	// block an edit for no reason.
	prior := Replay(pristine, existing)
	for _, a := range prior.Applied {
		if a.Span.Start < span.End && span.Start < a.Span.End {
			return EditPlan{}, fmt.Errorf("%w (finding %s covers runes %d-%d)",
				ErrEditConflict, a.ID, a.Span.Start, a.Span.End)
		}
	}

	// Preview what the rebuild will produce: the same Replay the worker runs,
	// over the existing overlay plus this edit.
	planned := edit
	planned.ChunkHash = hash
	combined := make([]Patch, 0, len(existing)+1)
	combined = append(combined, existing...)
	combined = append(combined, planned)
	after := Replay(pristine, combined)

	return EditPlan{
		Span:         span,
		Occurrence:   occ,
		ChunkHash:    hash,
		Preview:      after.Text,
		PreviewStale: prior.Stale,
	}, nil
}

// Occurrences returns the 0-based rune indices at which target occurs in text.
//
// Exported so a caller that has to PERSIST an occurrence index (a direct edit
// records the anchor it resolved) derives it from the same matcher Locate uses,
// rather than reimplementing rune-accurate substring search and eventually
// disagreeing with it.
func Occurrences(text, target string) []int {
	return occurrences([]rune(text), []rune(target))
}

// occurrenceOf reports which occurrence of target begins at rune index start,
// or -1 when none does.
func occurrenceOf(text, target string, start int) int {
	for i, s := range Occurrences(text, target) {
		if s == start {
			return i
		}
	}
	return -1
}
