package patch

import (
	"errors"
	"strings"
	"testing"
)

func TestLocate(t *testing.T) {
	// "fox" appears three times — the ambiguity this whole design exists to
	// handle. Offsets: 4, 20, 36.
	repeated := "the fox ran and the fox sat and the fox slept"

	tests := []struct {
		name      string
		chunk     string
		anchor    Anchor
		wantStart int
		wantErr   error
	}{
		{
			name:      "offset hit is preferred",
			chunk:     repeated,
			anchor:    Anchor{OriginalText: "fox", Offset: 20, Occurrence: 0},
			wantStart: 20, // offset wins over the (contradictory) occurrence
		},
		{
			name:      "occurrence selects when offset is stale",
			chunk:     repeated,
			anchor:    Anchor{OriginalText: "fox", Offset: 999, Occurrence: 2},
			wantStart: 36,
		},
		{
			name:      "occurrence selects when offset unknown",
			chunk:     repeated,
			anchor:    Anchor{OriginalText: "fox", Offset: -1, Occurrence: 1},
			wantStart: 20,
		},
		{
			name:      "unique recovery for a legacy finding with no anchor data",
			chunk:     "the quick brown fox jumped",
			anchor:    Anchor{OriginalText: "fox", Offset: -1, Occurrence: -1},
			wantStart: 16,
		},
		{
			name:    "ambiguous refuses rather than guessing",
			chunk:   repeated,
			anchor:  Anchor{OriginalText: "fox", Offset: -1, Occurrence: -1},
			wantErr: ErrAnchorAmbiguous,
		},
		{
			name:    "out-of-range occurrence on an ambiguous span refuses",
			chunk:   repeated,
			anchor:  Anchor{OriginalText: "fox", Offset: -1, Occurrence: 9},
			wantErr: ErrAnchorAmbiguous,
		},
		{
			name:    "missing span is not found",
			chunk:   "the quick brown fox",
			anchor:  Anchor{OriginalText: "badger", Offset: 0, Occurrence: 0},
			wantErr: ErrAnchorNotFound,
		},
		{
			name:    "empty original text is not found",
			chunk:   "the quick brown fox",
			anchor:  Anchor{OriginalText: "", Offset: 0, Occurrence: 0},
			wantErr: ErrAnchorNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Locate(tc.chunk, tc.anchor)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Start != tc.wantStart {
				t.Errorf("start: want %d, got %d", tc.wantStart, got.Start)
			}
			if want := tc.wantStart + len([]rune(tc.anchor.OriginalText)); got.End != want {
				t.Errorf("end: want %d, got %d", want, got.End)
			}
		})
	}
}

// Multi-byte text is the case where byte offsets would silently corrupt the
// string. The offsets here are rune indices; a byte-based implementation would
// land mid-character.
func TestLocateIsRuneIndexedNotByteIndexed(t *testing.T) {
	chunk := "café — the naïve fox"
	// "fox" starts at rune 17, but at a higher byte index thanks to é, —, and ï.
	if byteIdx := strings.Index(chunk, "fox"); byteIdx == 17 {
		t.Fatalf("test is not exercising the rune/byte difference (byte index %d)", byteIdx)
	}

	got, err := Locate(chunk, Anchor{OriginalText: "fox", Offset: 17, Occurrence: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Start != 17 {
		t.Errorf("want rune start 17, got %d", got.Start)
	}
}

func TestApply(t *testing.T) {
	chunk := "the quick brown fox jumped"
	hash := ChunkHash(chunk)

	t.Run("applies at the anchored span", func(t *testing.T) {
		got, span, err := Apply(chunk, hash, Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, "folks")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "the quick brown folks jumped"; got != want {
			t.Errorf("want %q, got %q", want, got)
		}
		if span.Start != 16 || span.End != 19 {
			t.Errorf("unexpected span %+v", span)
		}
	})

	t.Run("edits only the anchored occurrence", func(t *testing.T) {
		repeated := "the fox ran and the fox sat"
		got, _, err := Apply(repeated, ChunkHash(repeated),
			Anchor{OriginalText: "fox", Offset: 20, Occurrence: 1}, "folks")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "the fox ran and the folks sat"; got != want {
			t.Errorf("want %q, got %q", want, got)
		}
		if strings.Count(got, "fox") != 1 {
			t.Errorf("the other occurrence should be untouched: %q", got)
		}
	})

	t.Run("refuses when the chunk changed underneath", func(t *testing.T) {
		_, _, err := Apply("something else entirely", hash,
			Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, "folks")
		if !errors.Is(err, ErrChunkChanged) {
			t.Fatalf("want ErrChunkChanged, got %v", err)
		}
	})

	t.Run("refuses an empty correction", func(t *testing.T) {
		_, _, err := Apply(chunk, hash, Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, "   ")
		if !errors.Is(err, ErrEmptyCorrection) {
			t.Fatalf("want ErrEmptyCorrection, got %v", err)
		}
	})

	t.Run("empty hash skips verification for legacy findings", func(t *testing.T) {
		if _, _, err := Apply(chunk, "", Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, "folks"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Applying twice must not compound. The second attempt is against text whose
	// hash no longer matches, so it is refused rather than double-edited.
	t.Run("is not idempotent-by-accident: reapply is refused", func(t *testing.T) {
		once, _, err := Apply(chunk, hash, Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, "folks")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, _, err := Apply(once, hash, Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, "folks"); !errors.Is(err, ErrChunkChanged) {
			t.Fatalf("want ErrChunkChanged on reapply, got %v", err)
		}
	})
}

func TestRevert(t *testing.T) {
	before := "the quick brown fox jumped"
	after := "the quick brown folks jumped"

	t.Run("restores the recorded before-text", func(t *testing.T) {
		got, err := Revert(after, after, before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != before {
			t.Errorf("want %q, got %q", before, got)
		}
	})

	t.Run("refuses when the chunk drifted after apply", func(t *testing.T) {
		if _, err := Revert("edited again by someone", after, before); !errors.Is(err, ErrChunkChanged) {
			t.Fatalf("want ErrChunkChanged, got %v", err)
		}
	})
}

func TestCanTransition(t *testing.T) {
	legal := []struct{ from, to string }{
		{StateProposed, StateAccepted},
		{StateProposed, StateRejected},
		{StateAccepted, StateApplied},
		{StateApplied, StateReverted},
		{StateRejected, StateProposed},
		{StateReverted, StateProposed},
		{StateProposed, StateStale},
		{StateApplied, StateStale},
	}
	for _, tc := range legal {
		if !CanTransition(tc.from, tc.to) {
			t.Errorf("%s -> %s should be legal", tc.from, tc.to)
		}
	}

	// The dangerous shortcuts: reaching "applied" without a human accept, and
	// resurrecting a stale patch.
	illegal := []struct{ from, to string }{
		{StateProposed, StateApplied},
		{StateRejected, StateApplied},
		{StateStale, StateProposed},
		{StateStale, StateApplied},
		{StateApplied, StateAccepted},
		{"nonsense", StateApplied},
	}
	for _, tc := range illegal {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("%s -> %s should be illegal", tc.from, tc.to)
		}
	}
}

func TestChunkHashIsStable(t *testing.T) {
	const text = "the quick brown fox"
	first, second := ChunkHash(text), ChunkHash(text)
	if first != second {
		t.Errorf("hash is not stable: %s vs %s", first, second)
	}
	if changed := ChunkHash(text + "!"); first == changed {
		t.Error("hash does not distinguish different text")
	}
}
