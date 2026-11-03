package asrpilot

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// lcsLen is the textbook DP longest common subsequence, the oracle for the
// Myers diff (whose matches are an LCS).
func lcsLen(a, b []string) int {
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] > cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev = cur
	}
	return prev[len(b)]
}

// TestAlignIsAnLCS: on random inputs the matches are strictly increasing,
// are equal tokens, number an LCS, and every token of each side appears in
// exactly one column, in order.
func TestAlignIsAnLCS(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	vocab := []string{"a", "b", "c", "d", "e"}
	for trial := range 500 {
		a := make([]string, r.IntN(30))
		b := make([]string, r.IntN(30))
		for i := range a {
			a[i] = vocab[r.IntN(len(vocab))]
		}
		for i := range b {
			b[i] = vocab[r.IntN(len(vocab))]
		}
		pairs := Align(a, b)
		matches, ni, nj := 0, 0, 0
		for _, p := range pairs {
			if p.A >= 0 {
				if p.A != ni {
					t.Fatalf("trial %d: a out of order in %v", trial, pairs)
				}
				ni++
			}
			if p.B >= 0 {
				if p.B != nj {
					t.Fatalf("trial %d: b out of order in %v", trial, pairs)
				}
				nj++
			}
			if p.A >= 0 && p.B >= 0 && a[p.A] == b[p.B] {
				matches++
			}
		}
		if ni != len(a) || nj != len(b) {
			t.Fatalf("trial %d: columns cover %d/%d and %d/%d tokens", trial, ni, len(a), nj, len(b))
		}
		ms, _ := diffMatches(a, b, len(a)+len(b))
		if want := lcsLen(a, b); len(ms) != want || matches < want {
			t.Fatalf("trial %d: %d diff matches (%d equal columns), LCS %d\na=%v\nb=%v", trial, len(ms), matches, want, a, b)
		}
	}
}

func TestAlignBounds(t *testing.T) {
	if Align(make([]string, MaxAlignTokens+1), nil) != nil {
		t.Error("Align must refuse past MaxAlignTokens")
	}
	a := make([]string, MaxAlignEdits+10)
	b := make([]string, MaxAlignEdits+10)
	for i := range a {
		a[i], b[i] = fmt.Sprint("x", i), fmt.Sprint("y", i)
	}
	if Align(a, b) != nil {
		t.Error("Align must refuse texts differing in more than MaxAlignEdits tokens")
	}
}

// granitePair builds a 1500-word production window and a pilot transcript of
// it with ~5% of the words changed — a 540 s Granite window.
func granitePair(n int) (prod, pilot []string) {
	r := rand.New(rand.NewPCG(7, 9))
	for range n {
		w := fmt.Sprintf("w%d", r.IntN(400))
		prod = append(prod, w)
		switch r.IntN(40) {
		case 0:
			pilot = append(pilot, "changed")
		case 1: // dropped
		case 2:
			pilot = append(pilot, w, "inserted")
		default:
			pilot = append(pilot, w)
		}
	}
	return prod, pilot
}

func BenchmarkAlign1500(b *testing.B) {
	prod, pilot := granitePair(1500)
	for b.Loop() {
		if Align(prod, pilot) == nil {
			b.Fatal("no alignment")
		}
	}
}

// TestVoteGraniteWindow: a 1500-word window-level pilot (no word times) is
// voted on — not unmappable — and quickly.
func TestVoteGraniteWindow(t *testing.T) {
	prodWords, _ := granitePair(1500)
	prodWords[750], prodWords[751] = "dorian", "grey"
	var segs []db.Segment
	for k := 0; k < 1500; k += 15 {
		segs = append(segs, prodSeg(k/15, float64(k)*0.36, strings.Join(prodWords[k:k+15], " ")))
	}
	pilotWords := append([]string(nil), prodWords...)
	pilotWords[751] = "Gray"
	pilotWords[100], pilotWords[1200] = "noise", "noise"
	ps := untimedPilot(0, 540, strings.Join(pilotWords, " "))
	chunk := chunkOf(segs[49:51]) // the segments holding words 735..764
	f := finding(chunk, "dorian grey", "dorian gray")
	f.ChunkStartSec = segs[49].Start
	start := time.Now()
	got := Vote(NewTrack(segs), f, NewPilot([]db.PilotSegment{ps}, false))
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("vote took %v", el)
	}
	if got.Vote != VoteStyleOnly || got.Detail.StyleVote != VoteHeardReplacement || got.HeardText != "dorian Gray" ||
		got.Detail.Method != "segment_align" || got.Detail.Anchored != "both" {
		t.Fatalf("got %s heard %q %+v", got.Vote, got.HeardText, got.Detail)
	}
}
