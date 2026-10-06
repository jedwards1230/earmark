package patch

import (
	"errors"
	"slices"
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

// TestApply and TestRevert lived here. Apply/Revert were deleted with the
// edit-the-chunk model (see anchor.go); every case they covered was migrated
// into TestApplyCorrections in overlay_test.go as a single-patch case, so the
// coverage moved rather than shrank. TestRevert had no successor on purpose:
// revert is now "flip patch_state and rebuild the projection", not a text swap.

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
		{StateProposed, StateUnanchorable},
		{StateUnanchorable, StateProposed},
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
		// unanchorable has no anchor to apply: it can only go back to proposed.
		{StateUnanchorable, StateAccepted},
		{StateUnanchorable, StateApplied},
		{StateUnanchorable, StateStale},
		{StateUnanchorable, StateRejected},
		// Only the review queue is re-anchored; a human decision is never parked.
		{StateAccepted, StateUnanchorable},
		{StateApplied, StateUnanchorable},
		{StateRejected, StateUnanchorable},
		{StateReverted, StateUnanchorable},
		{StateStale, StateUnanchorable},
	}
	for _, tc := range illegal {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("%s -> %s should be illegal", tc.from, tc.to)
		}
	}
}

// TestUnanchorableIsNeverAppliable walks every path: no sequence of legal
// transitions from unanchorable reaches applied without passing through
// proposed and then a human accept.
func TestUnanchorableIsNeverAppliable(t *testing.T) {
	if got := StatesAllowing(StateApplied); slices.Contains(got, StateUnanchorable) {
		t.Errorf("StatesAllowing(applied) = %v includes unanchorable", got)
	}
	if got := StatesAllowing(StateAccepted); slices.Contains(got, StateUnanchorable) {
		t.Errorf("StatesAllowing(accepted) = %v includes unanchorable", got)
	}
	var next []string
	for _, s := range AllStates() {
		if CanTransition(StateUnanchorable, s) {
			next = append(next, s)
		}
	}
	if !slices.Equal(next, []string{StateProposed}) {
		t.Errorf("unanchorable may move to %v, want only [proposed]", next)
	}
}

func TestIsMachineTransition(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{StateProposed, StateUnanchorable, true},
		{StateUnanchorable, StateProposed, true},
		{StateProposed, StateAccepted, false},
		{StateRejected, StateProposed, false},
		{StateReverted, StateProposed, false},
	} {
		if got := IsMachineTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("IsMachineTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
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
