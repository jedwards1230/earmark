package patch

import (
	"errors"
	"strings"
	"testing"
)

// TestPlanDirectEdit_HappyPath asserts the span, occurrence, chunk hash, and
// preview are all correctly computed for a clean, unambiguous edit.
func TestPlanDirectEdit_HappyPath(t *testing.T) {
	const pristine = "the signal came from auto sebo in the jungle"
	edit := Patch{
		ID:         "(new)",
		Anchor:     Anchor{OriginalText: "auto sebo", Offset: -1, Occurrence: -1},
		Correction: "Arecibo",
	}

	plan, err := PlanDirectEdit(pristine, nil, edit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantStart := strings.Index(pristine, "auto sebo")
	if plan.Span.Start != wantStart || plan.Span.End != wantStart+len("auto sebo") {
		t.Errorf("span: want [%d,%d), got %+v", wantStart, wantStart+len("auto sebo"), plan.Span)
	}
	if plan.Occurrence != 0 {
		t.Errorf("occurrence: want 0, got %d", plan.Occurrence)
	}
	if plan.ChunkHash != ChunkHash(pristine) {
		t.Errorf("chunk hash: want %s, got %s", ChunkHash(pristine), plan.ChunkHash)
	}
	want := "the signal came from Arecibo in the jungle"
	if plan.Preview != want {
		t.Errorf("preview: want %q, got %q", want, plan.Preview)
	}
	if len(plan.PreviewStale) != 0 {
		t.Errorf("no existing patches: PreviewStale should be empty, got %+v", plan.PreviewStale)
	}
}

// TestPlanDirectEdit_OffsetIsServerComputedNotTrusted mirrors
// anchor_live_test.go's TestLiveModelOffsetIsUntrusted: a real model once
// reported offset 27 for a span that actually starts at 29. A caller-supplied
// offset is only ever a hint fed into Locate's resolution ladder — what gets
// persisted in the EditPlan is what Locate actually resolved, never the
// caller's claim.
func TestPlanDirectEdit_OffsetIsServerComputedNotTrusted(t *testing.T) {
	const pristine = "the signal was sent from the auto sebo observatory in nineteen seventy four"
	const original = "auto sebo"
	const trueOffset = 29
	const wrongOffset = 27

	if got := strings.Index(pristine, original); got != trueOffset {
		t.Fatalf("fixture drift: %q starts at %d, expected %d", original, got, trueOffset)
	}

	edit := Patch{
		ID: "(new)",
		Anchor: Anchor{
			OriginalText: original,
			Offset:       wrongOffset,
			Occurrence:   0,
		},
		Correction: "arecibo",
	}

	plan, err := PlanDirectEdit(pristine, nil, edit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Span.Start != trueOffset {
		t.Errorf("PlanDirectEdit trusted the caller's offset: want start %d, got %d", trueOffset, plan.Span.Start)
	}
}

// TestPlanDirectEdit_EmptyCorrection asserts an empty or whitespace-only
// correction is refused.
func TestPlanDirectEdit_EmptyCorrection(t *testing.T) {
	const pristine = "the quick brown fox"
	for _, correction := range []string{"", "   ", "\t\n"} {
		edit := Patch{ID: "(new)", Anchor: Anchor{OriginalText: "fox"}, Correction: correction}
		_, err := PlanDirectEdit(pristine, nil, edit)
		if !errors.Is(err, ErrEmptyCorrection) {
			t.Errorf("correction %q: want ErrEmptyCorrection, got %v", correction, err)
		}
	}
}

// TestPlanDirectEdit_ChunkChanged asserts a caller-pinned ChunkHash that no
// longer matches the pristine text is refused rather than anchored blind.
func TestPlanDirectEdit_ChunkChanged(t *testing.T) {
	const pristine = "the quick brown fox"
	edit := Patch{
		ID:         "(new)",
		Anchor:     Anchor{OriginalText: "fox"},
		Correction: "wolf",
		ChunkHash:  "not-the-real-hash",
	}
	_, err := PlanDirectEdit(pristine, nil, edit)
	if !errors.Is(err, ErrChunkChanged) {
		t.Fatalf("want ErrChunkChanged, got %v", err)
	}
}

// TestPlanDirectEdit_AmbiguousSpan asserts a span occurring twice with no
// offset/occurrence hint refuses rather than guessing, and that supplying
// Occurrence: 1 resolves the second occurrence.
func TestPlanDirectEdit_AmbiguousSpan(t *testing.T) {
	const pristine = "the fox ran and the fox sat"

	t.Run("no occurrence refuses", func(t *testing.T) {
		edit := Patch{
			ID:         "(new)",
			Anchor:     Anchor{OriginalText: "fox", Offset: -1, Occurrence: -1},
			Correction: "wolf",
		}
		_, err := PlanDirectEdit(pristine, nil, edit)
		if !errors.Is(err, ErrAnchorAmbiguous) {
			t.Fatalf("want ErrAnchorAmbiguous, got %v", err)
		}
	})

	t.Run("occurrence 1 resolves the second span", func(t *testing.T) {
		edit := Patch{
			ID:         "(new)",
			Anchor:     Anchor{OriginalText: "fox", Offset: -1, Occurrence: 1},
			Correction: "wolf",
		}
		plan, err := PlanDirectEdit(pristine, nil, edit)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.Occurrence != 1 {
			t.Errorf("occurrence: want 1, got %d", plan.Occurrence)
		}
		wantStart := strings.LastIndex(pristine, "fox")
		if plan.Span.Start != wantStart {
			t.Errorf("span start: want %d (the second occurrence), got %d", wantStart, plan.Span.Start)
		}
	})
}

// TestPlanDirectEdit_SpanAbsent asserts a span that does not occur at all in
// the pristine text is refused with ErrAnchorNotFound.
func TestPlanDirectEdit_SpanAbsent(t *testing.T) {
	const pristine = "the quick brown fox"
	edit := Patch{
		ID:         "(new)",
		Anchor:     Anchor{OriginalText: "badger"},
		Correction: "wolf",
	}
	_, err := PlanDirectEdit(pristine, nil, edit)
	if !errors.Is(err, ErrAnchorNotFound) {
		t.Fatalf("want ErrAnchorNotFound, got %v", err)
	}
}

// TestPlanDirectEdit_OverlapsExisting asserts an edit overlapping an already
// applied/accepted correction is refused with both ErrOverlappingPatches and
// ErrEditConflict, and that the message names the conflicting patch.
func TestPlanDirectEdit_OverlapsExisting(t *testing.T) {
	const pristine = "the quick brown fox jumped"
	existing := []Patch{
		{ID: "finding-1", Anchor: Anchor{OriginalText: "brown fox"}, Correction: "red wolf"},
	}
	// "brown" overlaps "brown fox" (both start within the existing span).
	edit := Patch{ID: "(new)", Anchor: Anchor{OriginalText: "brown"}, Correction: "orange"}

	_, err := PlanDirectEdit(pristine, existing, edit)
	if !errors.Is(err, ErrOverlappingPatches) {
		t.Errorf("want ErrOverlappingPatches, got %v", err)
	}
	if !errors.Is(err, ErrEditConflict) {
		t.Errorf("want ErrEditConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "finding-1") {
		t.Errorf("error should name the conflicting patch id, got: %v", err)
	}
}

// TestPlanDirectEdit_NonOverlappingExisting asserts a non-overlapping existing
// patch does not block the new edit, and that the Preview replays BOTH
// replacements — proof the preview replays the whole overlay, not just the
// new edit.
func TestPlanDirectEdit_NonOverlappingExisting(t *testing.T) {
	const pristine = "the quick brown fox jumped over the lazy dog"
	existing := []Patch{
		{ID: "finding-1", Anchor: Anchor{OriginalText: "lazy dog"}, Correction: "sleepy cat"},
	}
	edit := Patch{ID: "(new)", Anchor: Anchor{OriginalText: "brown fox"}, Correction: "red wolf"}

	plan, err := PlanDirectEdit(pristine, existing, edit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(plan.Preview, "red wolf") {
		t.Errorf("preview missing the new edit's replacement: %q", plan.Preview)
	}
	if !strings.Contains(plan.Preview, "sleepy cat") {
		t.Errorf("preview missing the existing patch's replacement — preview must replay the whole overlay: %q", plan.Preview)
	}
	if len(plan.PreviewStale) != 0 {
		t.Errorf("existing patch is valid, should not be stale: %+v", plan.PreviewStale)
	}
}

// TestPlanDirectEdit_UnusableExistingPatchDoesNotBlock asserts an existing
// patch that cannot itself replay (wrong ChunkHash) does not block the new
// edit, and shows up in PreviewStale rather than silently vanishing.
func TestPlanDirectEdit_UnusableExistingPatchDoesNotBlock(t *testing.T) {
	const pristine = "the quick brown fox jumped over the lazy dog"
	existing := []Patch{
		{
			ID:         "stale-finding",
			Anchor:     Anchor{OriginalText: "lazy dog"},
			Correction: "sleepy cat",
			ChunkHash:  "wrong-hash-does-not-match-current-pristine",
		},
	}
	edit := Patch{ID: "(new)", Anchor: Anchor{OriginalText: "brown fox"}, Correction: "red wolf"}

	plan, err := PlanDirectEdit(pristine, existing, edit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.PreviewStale) != 1 || plan.PreviewStale[0].ID != "stale-finding" {
		t.Errorf("want existing patch reported stale, got %+v", plan.PreviewStale)
	}
	if !strings.Contains(plan.Preview, "red wolf") {
		t.Errorf("new edit should still land: %q", plan.Preview)
	}
	if strings.Contains(plan.Preview, "sleepy cat") {
		t.Errorf("the unusable existing patch must not be applied: %q", plan.Preview)
	}
}

// TestPlanDirectEdit_MultibyteRuneCorrectness asserts the located span is a
// RUNE index (not a byte index) and the preview is intact, using text a
// byte-indexed implementation would slice incorrectly.
func TestPlanDirectEdit_MultibyteRuneCorrectness(t *testing.T) {
	const pristine = "café — the naïve 🐉 fox"
	// "fox" begins at a rune index well below its byte index, thanks to é, —,
	// ï, and the 4-byte emoji.
	byteIdx := strings.Index(pristine, "fox")
	runeIdx := len([]rune(pristine[:byteIdx]))
	if byteIdx == runeIdx {
		t.Fatalf("fixture does not exercise the rune/byte difference (byte idx %d == rune idx %d)", byteIdx, runeIdx)
	}

	edit := Patch{ID: "(new)", Anchor: Anchor{OriginalText: "fox", Offset: -1, Occurrence: -1}, Correction: "wolf"}
	plan, err := PlanDirectEdit(pristine, nil, edit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Span.Start != runeIdx {
		t.Errorf("span start: want rune index %d, got %d", runeIdx, plan.Span.Start)
	}
	want := "café — the naïve 🐉 wolf"
	if plan.Preview != want {
		t.Errorf("preview corrupted by non-ASCII text: want %q, got %q", want, plan.Preview)
	}
}

// TestOccurrences covers Occurrences directly: all rune indices, overlapping
// targets, and the empty/oversized-target degenerate cases.
func TestOccurrences(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		target string
		want   []int
	}{
		{
			name:   "multiple non-overlapping occurrences",
			text:   "the fox ran and the fox sat and the fox slept",
			target: "fox",
			want:   []int{4, 20, 36},
		},
		{
			name:   "overlapping occurrences are all reported",
			text:   "aaaa",
			target: "aa",
			want:   []int{0, 1, 2},
		},
		{
			name:   "no occurrences",
			text:   "the quick brown fox",
			target: "badger",
			want:   nil,
		},
		{
			name:   "empty target returns nil",
			text:   "the quick brown fox",
			target: "",
			want:   nil,
		},
		{
			name:   "target longer than text returns nil",
			text:   "fox",
			target: "the quick brown fox",
			want:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Occurrences(tc.text, tc.target)
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: want %d, got %d (full: want %v, got %v)", i, tc.want[i], got[i], tc.want, got)
				}
			}
		})
	}
}
