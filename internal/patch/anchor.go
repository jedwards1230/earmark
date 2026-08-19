// Package patch turns the eval layer's advisory findings into reviewable,
// human-confirmed edits to transcript text.
//
// # Why this is a separate package
//
// CONTRACT §2.15 binds the eval layer to read-only: the LLM judge's only write
// is INSERT INTO transcript_findings, and its suggested_correction is never
// applied. That asymmetry is deliberate and still correct — a wrong flag is
// cheap, an autonomous wrong correction would silently corrupt the corpus.
//
// This package does not relax that. The judge remains read-only; every write
// here is gated on an explicit human decision recorded against a specific
// finding. The model proposes, a person disposes. Keeping the apply path in its
// own package (rather than in internal/eval) is what keeps the §2.15 guarantee
// mechanically true instead of merely intended — internal/eval still contains
// no UPDATE against transcript text, and its SQL guard test still passes.
package patch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Patch lifecycle states. Stored in transcript_findings.patch_state.
//
//	proposed -> accepted -> applied -> reverted
//	proposed -> rejected
//	(any)    -> stale        (the underlying chunk changed underneath us)
const (
	StateProposed = "proposed"
	StateAccepted = "accepted"
	StateRejected = "rejected"
	StateApplied  = "applied"
	StateStale    = "stale"
	StateReverted = "reverted"
)

var (
	// ErrAnchorNotFound means the target span could not be located in the chunk
	// with enough confidence to edit it. Callers must treat this as stale, never
	// as "apply somewhere close" — a near-miss edit is corpus corruption.
	ErrAnchorNotFound = errors.New("patch anchor not found in chunk text")

	// ErrAnchorAmbiguous means the span occurs multiple times and neither the
	// offset nor the occurrence index resolved which one was meant.
	ErrAnchorAmbiguous = errors.New("patch anchor is ambiguous in chunk text")

	// ErrChunkChanged means the chunk's text no longer hashes to what the judge
	// reviewed, so the finding describes text that no longer exists.
	ErrChunkChanged = errors.New("chunk text changed since the finding was recorded")

	// ErrEmptyCorrection guards against a patch that would delete text without
	// saying so. The judge is required to supply a correction.
	ErrEmptyCorrection = errors.New("patch has an empty suggested correction")
)

// ChunkHash is the canonical fingerprint of a chunk's text, recorded when a
// finding is created and re-checked before the patch is applied. Hex-encoded
// SHA-256 of the raw UTF-8 bytes.
func ChunkHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Span is a located target region, expressed in RUNE indices into the chunk
// text. Runes rather than bytes so an offset can never split a multi-byte
// character, and so the offsets mean the same thing to the review UI as they do
// here.
type Span struct {
	Start int
	End   int
}

// Anchor identifies which span of a chunk a finding refers to.
//
// OriginalText is the verbatim span the judge copied. Offset and Occurrence
// disambiguate it: Offset is the rune index the judge reported, Occurrence is
// the 0-based index among identical spans in the chunk. Both are optional
// (negative = unknown) because findings recorded before this feature existed
// have neither — those fall back to unique-match recovery.
type Anchor struct {
	OriginalText string
	Offset       int // rune index; negative when unknown
	Occurrence   int // 0-based; negative when unknown
}

// Locate resolves an Anchor against the current chunk text.
//
// The resolution ladder is deliberately ordered most-precise first, and it
// refuses rather than guesses at the bottom:
//
//  1. Offset hit — the recorded offset still holds exactly this span. Cheapest
//     and most precise; the normal path.
//  2. Occurrence hit — the text shifted but the span still appears, and the
//     recorded occurrence index selects one.
//  3. Unique recovery — no usable offset/occurrence (e.g. a legacy finding), but
//     the span appears exactly once, so there is nothing to be ambiguous about.
//  4. Refuse — multiple candidates and no way to choose (ErrAnchorAmbiguous), or
//     no candidates at all (ErrAnchorNotFound).
//
// Step 4 is the important one. Applying a correction to the wrong occurrence is
// worse than not applying it, so an unresolvable anchor is always an error.
func Locate(chunkText string, a Anchor) (Span, error) {
	if a.OriginalText == "" {
		return Span{}, ErrAnchorNotFound
	}

	runes := []rune(chunkText)
	target := []rune(a.OriginalText)
	n := len(target)

	// 1. Offset hit.
	if a.Offset >= 0 && a.Offset+n <= len(runes) {
		if string(runes[a.Offset:a.Offset+n]) == a.OriginalText {
			return Span{Start: a.Offset, End: a.Offset + n}, nil
		}
	}

	starts := occurrences(runes, target)
	if len(starts) == 0 {
		return Span{}, ErrAnchorNotFound
	}

	// 2. Occurrence hit.
	if a.Occurrence >= 0 && a.Occurrence < len(starts) {
		s := starts[a.Occurrence]
		return Span{Start: s, End: s + n}, nil
	}

	// 3. Unique recovery.
	if len(starts) == 1 {
		return Span{Start: starts[0], End: starts[0] + n}, nil
	}

	// 4. Refuse.
	return Span{}, fmt.Errorf("%w: %d candidates for %q", ErrAnchorAmbiguous, len(starts), a.OriginalText)
}

// occurrences returns the rune indices at which target appears in runes.
func occurrences(runes, target []rune) []int {
	if len(target) == 0 || len(target) > len(runes) {
		return nil
	}
	var out []int
	for i := 0; i+len(target) <= len(runes); i++ {
		match := true
		for j := range target {
			if runes[i+j] != target[j] {
				match = false
				break
			}
		}
		if match {
			out = append(out, i)
		}
	}
	return out
}

// Applying lives in overlay.go, not here.
//
// There used to be an Apply(chunkText, …) that rewrote one chunk's stored text,
// and a Revert that swapped the whole text back. Both encoded a model that is
// now known to be wrong: transcript_chunks is a DERIVED PROJECTION, regenerated
// from the immutable transcript source on every embed, so a correction written
// into chunk text is destroyed by the next re-chunk. Corrections are replayed
// onto regenerated text instead (ApplyCorrections / Replay), and revert is
// "flip patch_state and rebuild the projection" rather than a text swap.
// CONTRACT §2.17.

// CanTransition reports whether a patch may move from one state to another.
// Centralised so the DB layer and the UI cannot disagree about what is legal.
func CanTransition(from, to string) bool {
	switch from {
	case StateProposed:
		return to == StateAccepted || to == StateRejected || to == StateStale
	case StateAccepted:
		return to == StateApplied || to == StateRejected || to == StateStale
	case StateApplied:
		return to == StateReverted || to == StateStale
	case StateRejected:
		// A rejected finding can be reconsidered, but never jump straight to applied.
		return to == StateProposed
	case StateReverted:
		return to == StateProposed
	case StateStale:
		// Terminal: the text it described is gone. Re-run the judge instead.
		return false
	default:
		return false
	}
}
