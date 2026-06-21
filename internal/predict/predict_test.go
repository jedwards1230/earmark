package predict

import (
	"math"
	"strings"
	"testing"
)

func TestCompute(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		// expectations
		wantWork     float64
		wantHasWork  bool
		wantCalKnown bool
		wantCalendar float64
		wantEvalIncl bool
	}{
		{
			name: "normal with eval and availability",
			in: Inputs{
				RemainingChunks:      100,
				Rates:                Rates{TranscribeSecPerChunk: 7, EmbedSecPerChunk: 0.5, EvalSecPerChunk: 1, EvalKnown: true},
				AvailabilityFraction: 0.5,
			},
			wantWork:     100 * 8.5,
			wantHasWork:  true,
			wantCalKnown: true,
			wantCalendar: 100 * 8.5 / 0.5,
			wantEvalIncl: true,
		},
		{
			name: "eval unknown excludes eval",
			in: Inputs{
				RemainingChunks:      10,
				Rates:                Rates{TranscribeSecPerChunk: 7, EmbedSecPerChunk: 0.5, EvalSecPerChunk: 0, EvalKnown: false},
				AvailabilityFraction: 1,
			},
			wantWork:     10 * 7.5,
			wantHasWork:  true,
			wantCalKnown: true,
			wantCalendar: 10 * 7.5,
			wantEvalIncl: false,
		},
		{
			name: "zero history → no work, no calendar",
			in: Inputs{
				RemainingChunks: 0,
				Rates:           Rates{},
			},
			wantWork:     0,
			wantHasWork:  false,
			wantCalKnown: false,
			wantEvalIncl: false,
		},
		{
			name: "chunks but no rates → no work (structural zero)",
			in: Inputs{
				RemainingChunks: 500,
				Rates:           Rates{}, // all zero
			},
			wantWork:     0,
			wantHasWork:  false,
			wantCalKnown: false,
		},
		{
			name: "no availability → calendar unknown (work-time fallback)",
			in: Inputs{
				RemainingChunks:      20,
				Rates:                Rates{TranscribeSecPerChunk: 7, EmbedSecPerChunk: 0.5},
				AvailabilityFraction: 0, // divide-by-zero guard
			},
			wantWork:     20 * 7.5,
			wantHasWork:  true,
			wantCalKnown: false,
		},
		{
			name: "availability > 1 is clamped (calendar never < work)",
			in: Inputs{
				RemainingChunks:      4,
				Rates:                Rates{TranscribeSecPerChunk: 10},
				AvailabilityFraction: 2.5,
			},
			wantWork:     40,
			wantHasWork:  true,
			wantCalKnown: true,
			wantCalendar: 40, // work / 1
		},
		{
			name: "negative remaining is clamped to zero",
			in: Inputs{
				RemainingChunks: -5,
				Rates:           Rates{TranscribeSecPerChunk: 10},
			},
			wantWork:    0,
			wantHasWork: false,
		},
		{
			name: "NaN/Inf rates are ignored",
			in: Inputs{
				RemainingChunks: 10,
				Rates: Rates{
					TranscribeSecPerChunk: math.NaN(),
					EmbedSecPerChunk:      math.Inf(1),
				},
			},
			wantWork:    0,
			wantHasWork: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Compute(c.in)
			if got.WorkSeconds != c.wantWork {
				t.Errorf("WorkSeconds = %v, want %v", got.WorkSeconds, c.wantWork)
			}
			if got.HasWork != c.wantHasWork {
				t.Errorf("HasWork = %v, want %v", got.HasWork, c.wantHasWork)
			}
			if got.CalendarKnown != c.wantCalKnown {
				t.Errorf("CalendarKnown = %v, want %v", got.CalendarKnown, c.wantCalKnown)
			}
			if c.wantCalKnown && got.CalendarSeconds != c.wantCalendar {
				t.Errorf("CalendarSeconds = %v, want %v", got.CalendarSeconds, c.wantCalendar)
			}
			if got.EvalIncluded != c.wantEvalIncl {
				t.Errorf("EvalIncluded = %v, want %v", got.EvalIncluded, c.wantEvalIncl)
			}
		})
	}
}

func TestEstimateLabel(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want []string // substrings the label must contain
		not  []string // substrings the label must NOT contain
	}{
		{
			name: "no work → em dash",
			in:   Inputs{},
			want: []string{"—"},
		},
		{
			name: "calendar known → 'left'",
			in: Inputs{
				RemainingChunks:      1000,
				Rates:                Rates{TranscribeSecPerChunk: 7.1, EmbedSecPerChunk: 0.6, EvalSecPerChunk: 1.4, EvalKnown: true},
				AvailabilityFraction: 0.45,
			},
			want: []string{"left"},
			not:  []string{"work time", "excl. eval"},
		},
		{
			name: "no availability → labeled work-time, not a calendar figure",
			in: Inputs{
				RemainingChunks:      1000,
				Rates:                Rates{TranscribeSecPerChunk: 7.1, EmbedSecPerChunk: 0.6, EvalKnown: false},
				AvailabilityFraction: 0,
			},
			want: []string{"work time", "calendar depends on runner availability", "excl. eval"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label := Compute(c.in).Label()
			for _, w := range c.want {
				if !strings.Contains(label, w) {
					t.Errorf("label %q missing %q", label, w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(label, n) {
					t.Errorf("label %q must not contain %q", label, n)
				}
			}
		})
	}
}

// TestComputePerBookVsCatalog confirms the model scales linearly with remaining
// chunks, so a single book's estimate and the catalog estimate (sum of books)
// are consistent: catalog(N) == book(N).
func TestComputePerBookVsCatalog(t *testing.T) {
	rates := Rates{TranscribeSecPerChunk: 5, EmbedSecPerChunk: 1, EvalKnown: false}
	book := Compute(Inputs{RemainingChunks: 200, Rates: rates, AvailabilityFraction: 0.5})
	catalog := Compute(Inputs{RemainingChunks: 1000, Rates: rates, AvailabilityFraction: 0.5})
	if catalog.WorkSeconds != 5*book.WorkSeconds {
		t.Errorf("catalog work %v should be 5x book work %v", catalog.WorkSeconds, book.WorkSeconds)
	}
	if catalog.CalendarSeconds != 5*book.CalendarSeconds {
		t.Errorf("catalog calendar %v should be 5x book calendar %v", catalog.CalendarSeconds, book.CalendarSeconds)
	}
}
