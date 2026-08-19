package patch

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrOverlappingPatches means two accepted corrections claim overlapping spans
// of the same chunk. There is no well-defined composition of two edits to the
// same characters — whichever landed second would be editing text the judge
// never saw — so the overlay refuses instead of picking one.
var ErrOverlappingPatches = errors.New("patches overlap in the same chunk")

// Stale reasons. These strings are persisted verbatim into
// transcript_findings.stale_reason, so they are part of the data contract:
// change one and existing rows become unreadable.
const (
	// StaleReasonChunkChanged — the chunk no longer hashes to what the judge
	// reviewed (or the chunk it referenced no longer exists at all).
	StaleReasonChunkChanged = "chunk_changed"
	// StaleReasonAnchorNotFound — the span the finding describes is not present
	// in the regenerated chunk text.
	StaleReasonAnchorNotFound = "anchor_not_found"
	// StaleReasonAnchorAmbiguous — the span occurs several times and neither the
	// offset nor the occurrence index picked one.
	StaleReasonAnchorAmbiguous = "anchor_ambiguous"
	// StaleReasonOverlapping — the patch overlaps another accepted patch.
	StaleReasonOverlapping = "overlapping_patch"
	// StaleReasonEmptyCorrection — the correction is empty/whitespace, so
	// applying it would silently delete text.
	StaleReasonEmptyCorrection = "empty_correction"
)

// Patch is one human-accepted correction, ready to replay onto a chunk.
//
// It is a value, not a row: everything needed to reproduce the edit travels
// with it, so replay is a pure function of (pristine text, patches).
type Patch struct {
	// ID is transcript_findings.id. It gives replay a stable tiebreak for
	// ordering and lets a failure name the offending row.
	ID string
	// Anchor locates the span in the chunk text (rune-indexed).
	Anchor Anchor
	// Correction is the replacement text.
	Correction string
	// ChunkHash is chunk_text_sha256 as recorded when the judge reviewed the
	// chunk. Empty means "legacy row, no hash recorded" and skips the check.
	ChunkHash string
}

// Overlay is a transcript's accepted corrections, keyed by chunk index.
//
// The overlay is the ONLY place a correction lives. transcript_chunks is a
// disposable projection: it is regenerated from the immutable transcript source
// on every embed, so a correction written into transcript_chunks.text would be
// destroyed by the next re-chunk. Corrections are therefore replayed onto the
// regenerated text rather than stored in it (CONTRACT §2.17).
type Overlay map[int][]Patch

// AppliedPatch records one correction that landed, for the audit trail.
//
// Before/After are SPAN-level, not whole-chunk: a chunk may carry several
// patches, which makes a whole-chunk "before" ambiguous about which patch
// produced which difference.
type AppliedPatch struct {
	ID     string
	Span   Span
	Before string // the located span text, verbatim
	After  string // the correction
}

// StaleRef records one correction that could not be replayed, and why.
// A quarantined patch is never deleted — it stays visible for review.
type StaleRef struct {
	ID     string
	Reason string // one of the StaleReason* constants
}

// ReplayResult is the outcome of a lenient replay: the projected text plus a
// complete partition of the input patches into applied and stale. Every input
// patch appears in exactly one of the two slices — replay never silently drops
// one.
type ReplayResult struct {
	Text    string
	Applied []AppliedPatch
	Stale   []StaleRef
}

// resolvedPatch is a patch whose span has been located against the pristine text.
type resolvedPatch struct {
	patch Patch
	span  Span
}

// ApplyCorrections replays patches onto text and returns the corrected text.
//
// It is PURE: no database, no I/O, no clock, no randomness. Given the same
// pristine text and the same set of patches it returns a byte-identical result
// every time, in any input order. That property is what makes transcript_chunks
// safe to throw away and rebuild.
//
// Strict: any unusable patch fails the whole chunk. The projection layer wants
// Replay, which quarantines instead.
func ApplyCorrections(text string, patches []Patch) (string, error) {
	if len(patches) == 0 {
		return text, nil
	}

	// Resolve EVERY span against the pristine input text, never against a
	// partially-patched string. All patches on one chunk were judged against the
	// same revision, so they share one coordinate system.
	resolved := make([]resolvedPatch, 0, len(patches))
	for _, p := range patches {
		span, err := resolvePatch(text, p)
		if err != nil {
			return "", err
		}
		resolved = append(resolved, resolvedPatch{patch: p, span: span})
	}

	orderPatches(resolved)

	if i, j, ok := firstOverlap(resolved); ok {
		return "", fmt.Errorf("%w: %s and %s",
			ErrOverlappingPatches, resolved[i].patch.ID, resolved[j].patch.ID)
	}

	out, _ := splice(text, resolved)
	return out, nil
}

// Replay is the lenient wrapper the projection layer uses. It runs the same
// pipeline as ApplyCorrections but partitions instead of failing: a patch that
// fails the hash check, the empty-correction check, anchor resolution, or the
// overlap check is quarantined in Stale with a reason, and the survivors are
// applied.
//
// One bad correction must not cost a whole chunk its other corrections — but it
// must not disappear either, so the caller can mark it stale in the database and
// keep it visible for review.
func Replay(text string, patches []Patch) ReplayResult {
	if len(patches) == 0 {
		return ReplayResult{Text: text}
	}

	res := ReplayResult{Text: text}
	resolved := make([]resolvedPatch, 0, len(patches))
	for _, p := range patches {
		span, err := resolvePatch(text, p)
		if err != nil {
			res.Stale = append(res.Stale, StaleRef{ID: p.ID, Reason: staleReason(err)})
			continue
		}
		resolved = append(resolved, resolvedPatch{patch: p, span: span})
	}

	orderPatches(resolved)

	// Quarantine every member of an overlapping group, not an arbitrary winner:
	// there is no principled way to choose between two edits to the same
	// characters, and picking one would apply it to text the other patch's judge
	// never saw.
	conflicted := overlapSet(resolved)
	keep := make([]resolvedPatch, 0, len(resolved))
	for i, r := range resolved {
		if conflicted[i] {
			res.Stale = append(res.Stale, StaleRef{ID: r.patch.ID, Reason: StaleReasonOverlapping})
			continue
		}
		keep = append(keep, r)
	}

	out, applied := splice(text, keep)
	res.Text = out
	res.Applied = applied
	return res
}

// resolvePatch validates a single patch against the pristine chunk text and
// returns its span. Errors are wrapped with the patch ID so a failure names the
// offending finding row.
func resolvePatch(text string, p Patch) (Span, error) {
	if strings.TrimSpace(p.Correction) == "" {
		return Span{}, fmt.Errorf("patch %s: %w", p.ID, ErrEmptyCorrection)
	}
	// A finding describes a specific revision of a chunk. If the chunk no longer
	// hashes to that revision, replaying would edit text the judge never
	// reviewed. An empty hash is a legacy row recorded before the column existed.
	if p.ChunkHash != "" && ChunkHash(text) != p.ChunkHash {
		return Span{}, fmt.Errorf("patch %s: %w", p.ID, ErrChunkChanged)
	}
	span, err := Locate(text, p.Anchor)
	if err != nil {
		return Span{}, fmt.Errorf("patch %s: %w", p.ID, err)
	}
	return span, nil
}

// staleReason maps a resolve failure onto the persisted stale_reason vocabulary.
func staleReason(err error) string {
	switch {
	case errors.Is(err, ErrEmptyCorrection):
		return StaleReasonEmptyCorrection
	case errors.Is(err, ErrChunkChanged):
		return StaleReasonChunkChanged
	case errors.Is(err, ErrAnchorAmbiguous):
		return StaleReasonAnchorAmbiguous
	case errors.Is(err, ErrAnchorNotFound):
		return StaleReasonAnchorNotFound
	default:
		// Unreachable today; treated as "the text moved under us", which is the
		// conservative reading — it never applies the patch.
		return StaleReasonChunkChanged
	}
}

// orderPatches sorts resolved patches by Start DESCENDING, then End descending,
// then patch ID ascending.
//
// The descending order is the whole point. Splicing left-to-right invalidates
// every later offset the moment an earlier replacement changes length — a silent
// corruption that produces plausible-looking text and no error. Applying from
// the end backwards leaves every not-yet-applied span's indices untouched.
//
// The tiebreaks make the ordering total, which is what makes the rebuild
// byte-identical regardless of the order the rows arrived in.
func orderPatches(rs []resolvedPatch) {
	slices.SortFunc(rs, func(a, b resolvedPatch) int {
		if a.span.Start != b.span.Start {
			return b.span.Start - a.span.Start
		}
		if a.span.End != b.span.End {
			return b.span.End - a.span.End
		}
		return strings.Compare(a.patch.ID, b.patch.ID)
	})
}

// firstOverlap reports the first overlapping adjacent pair in a
// descending-ordered slice. Adjacent comparison is sufficient: if two spans
// overlap, every span ordered between them overlaps at least one of them too.
func firstOverlap(rs []resolvedPatch) (int, int, bool) {
	for i := 0; i+1 < len(rs); i++ {
		if rs[i+1].span.End > rs[i].span.Start {
			return i, i + 1, true
		}
	}
	return 0, 0, false
}

// overlapSet marks every index that participates in an overlap.
func overlapSet(rs []resolvedPatch) map[int]bool {
	out := make(map[int]bool)
	for i := 0; i+1 < len(rs); i++ {
		if rs[i+1].span.End > rs[i].span.Start {
			out[i] = true
			out[i+1] = true
		}
	}
	return out
}

// splice applies descending-ordered, non-overlapping patches to text.
//
// Spans are rune indices, so the splice runs on a []rune copy: a byte-indexed
// splice could cut a multi-byte character in half. The input string is never
// mutated (Go strings are immutable, and the rune slice is a copy).
func splice(text string, rs []resolvedPatch) (string, []AppliedPatch) {
	if len(rs) == 0 {
		return text, nil
	}
	pristine := []rune(text)
	out := make([]rune, len(pristine))
	copy(out, pristine)

	applied := make([]AppliedPatch, 0, len(rs))
	for _, r := range rs {
		repl := []rune(r.patch.Correction)
		next := make([]rune, 0, len(out)-(r.span.End-r.span.Start)+len(repl))
		next = append(next, out[:r.span.Start]...)
		next = append(next, repl...)
		next = append(next, out[r.span.End:]...)
		out = next

		applied = append(applied, AppliedPatch{
			ID:   r.patch.ID,
			Span: r.span,
			// Before is the located SPAN from the pristine text, verbatim — not
			// the whole chunk, which would be ambiguous with several patches.
			Before: string(pristine[r.span.Start:r.span.End]),
			After:  r.patch.Correction,
		})
	}
	return string(out), applied
}

// AllStates lists every legal value of transcript_findings.patch_state.
//
// Exported so the DB layer can DERIVE the set of states a transition is legal
// from (by filtering with CanTransition) rather than hard-coding a second copy
// of the state machine in SQL. Two copies would eventually disagree, and the
// disagreement would show up as a patch applied from a state a human never
// approved.
func AllStates() []string {
	return []string{
		StateProposed,
		StateAccepted,
		StateRejected,
		StateApplied,
		StateStale,
		StateReverted,
	}
}

// StatesAllowing returns every state that may legally transition to `to`,
// in AllStates order. Derived from CanTransition, never restated.
func StatesAllowing(to string) []string {
	var out []string
	for _, s := range AllStates() {
		if CanTransition(s, to) {
			out = append(out, s)
		}
	}
	return out
}
