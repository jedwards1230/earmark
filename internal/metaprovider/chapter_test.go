package metaprovider

// chapterForSec is an internal function tested here via white-box access
// (same package, _test suffix via file naming convention is not used so we
// stay in package metaprovider to access the unexported function directly).

import (
	"math"
	"testing"
)

// TestChapterForSec covers the boundary conditions documented in chapterForSec.
func TestChapterForSec(t *testing.T) {
	t.Parallel()

	chapters := []Chapter{
		{Index: 0, Title: "Dedication", StartSec: 0, EndSec: 17.18},
		{Index: 1, Title: "Chapter 1", StartSec: 17.18, EndSec: 2221.01},
		{Index: 2, Title: "Chapter 2", StartSec: 2221.01, EndSec: 3942.49},
	}

	cases := []struct {
		name      string
		sec       float64
		wantIdx   int
		wantTitle string
		wantOK    bool
	}{
		{
			name:      "start of first chapter",
			sec:       0,
			wantIdx:   0,
			wantTitle: "Dedication",
			wantOK:    true,
		},
		{
			name:      "middle of second chapter",
			sec:       1000,
			wantIdx:   1,
			wantTitle: "Chapter 1",
			wantOK:    true,
		},
		{
			name:      "exactly at second chapter boundary",
			sec:       17.18,
			wantIdx:   1,
			wantTitle: "Chapter 1",
			wantOK:    true,
		},
		{
			name:      "start of third chapter",
			sec:       2221.01,
			wantIdx:   2,
			wantTitle: "Chapter 2",
			wantOK:    true,
		},
		{
			name:      "at last chapter end (inclusive)",
			sec:       3942.49,
			wantIdx:   2,
			wantTitle: "Chapter 2",
			wantOK:    true,
		},
		{
			name:   "before first chapter start",
			sec:    -1,
			wantOK: false,
		},
		{
			name:   "past last chapter end",
			sec:    99999,
			wantOK: false,
		},
		{
			name:   "no chapters",
			sec:    100,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var chaps []Chapter
			if tc.name != "no chapters" {
				chaps = chapters
			}
			idx, title, ok := chapterForSec(chaps, tc.sec)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (sec=%v)", ok, tc.wantOK, tc.sec)
				return
			}
			if !ok {
				return
			}
			if idx != tc.wantIdx {
				t.Errorf("index = %d, want %d", idx, tc.wantIdx)
			}
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
		})
	}
}

// multiTrackChapters is a book-absolute chapter timeline over a three-track book
// whose tracks are 1000 s each (see multiTrackDurations). Chapter 2 starts
// inside track 2 and Chapter 3 inside track 3, so a track-relative second passed
// straight to chapterForSec would always land in "Front Matter"/"Chapter 1" —
// the bug ChapterForTrackSec exists to prevent.
var (
	multiTrackChapters = []Chapter{
		{Index: 0, Title: "Front Matter", StartSec: 0, EndSec: 60},
		{Index: 1, Title: "Chapter 1", StartSec: 60, EndSec: 1200},
		{Index: 2, Title: "Chapter 2", StartSec: 1200, EndSec: 2400},
		{Index: 3, Title: "Chapter 3", StartSec: 2400, EndSec: 3000},
	}
	multiTrackDurations = []float64{1000, 1000, 1000}
)

// TestChapterForTrackSec covers the track-relative → book-absolute translation.
func TestChapterForTrackSec(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		chapters  []Chapter
		durations []float64
		trackIdx  int
		trackSec  float64
		wantIdx   int
		wantTitle string
		wantOK    bool
	}{
		{
			name:      "first track behaves like a plain book-absolute lookup",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  0,
			trackSec:  10,
			wantIdx:   0,
			wantTitle: "Front Matter",
			wantOK:    true,
		},
		{
			// 1000 + 300 = 1300 s → Chapter 2, not Chapter 1.
			name:      "second track adds the first track's duration",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  1,
			trackSec:  300,
			wantIdx:   2,
			wantTitle: "Chapter 2",
			wantOK:    true,
		},
		{
			// 2000 + 500 = 2500 s → Chapter 3.
			name:      "third track adds both preceding durations",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  2,
			trackSec:  500,
			wantIdx:   3,
			wantTitle: "Chapter 3",
			wantOK:    true,
		},
		{
			// The same small trackSec on a later track must NOT collapse to the
			// book's opening chapter: 0 s of track 3 is 2000 s into the book.
			name:      "small offset late in the book still resolves late",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  2,
			trackSec:  0,
			wantIdx:   2,
			wantTitle: "Chapter 2",
			wantOK:    true,
		},
		{
			name:      "single-file book",
			chapters:  multiTrackChapters,
			durations: []float64{3000},
			trackIdx:  0,
			trackSec:  1300,
			wantIdx:   2,
			wantTitle: "Chapter 2",
			wantOK:    true,
		},
		{
			name:      "unknown preceding duration yields no chapter",
			chapters:  multiTrackChapters,
			durations: []float64{0, 1000, 1000},
			trackIdx:  1,
			trackSec:  300,
			wantOK:    false,
		},
		{
			name:      "negative preceding duration yields no chapter",
			chapters:  multiTrackChapters,
			durations: []float64{-1, 1000},
			trackIdx:  1,
			trackSec:  10,
			wantOK:    false,
		},
		{
			name:      "NaN preceding duration yields no chapter",
			chapters:  multiTrackChapters,
			durations: []float64{math.NaN(), 1000},
			trackIdx:  1,
			trackSec:  10,
			wantOK:    false,
		},
		{
			name:      "Inf preceding duration yields no chapter",
			chapters:  multiTrackChapters,
			durations: []float64{math.Inf(1), 1000},
			trackIdx:  1,
			trackSec:  10,
			wantOK:    false,
		},
		{
			name:      "track index past the end yields no chapter",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  3,
			trackSec:  10,
			wantOK:    false,
		},
		{
			name:      "negative track index yields no chapter",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  -1,
			trackSec:  10,
			wantOK:    false,
		},
		{
			name:      "no track list yields no chapter",
			chapters:  multiTrackChapters,
			durations: nil,
			trackIdx:  0,
			trackSec:  10,
			wantOK:    false,
		},
		{
			name:      "no chapters yields no chapter",
			chapters:  nil,
			durations: multiTrackDurations,
			trackIdx:  1,
			trackSec:  10,
			wantOK:    false,
		},
		{
			// 2000 + 5000 is far past the last chapter's end.
			name:      "past the end of the last chapter yields no chapter",
			chapters:  multiTrackChapters,
			durations: multiTrackDurations,
			trackIdx:  2,
			trackSec:  5000,
			wantOK:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, title, ok := ChapterForTrackSec(tc.chapters, tc.durations, tc.trackIdx, tc.trackSec)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (trackIdx=%d trackSec=%v)", ok, tc.wantOK, tc.trackIdx, tc.trackSec)
				return
			}
			if !ok {
				return
			}
			if idx != tc.wantIdx {
				t.Errorf("index = %d, want %d", idx, tc.wantIdx)
			}
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
		})
	}
}

// TestChapterForTrackSecMatchesChapterForSecOnFirstTrack pins the
// single-file/first-track equivalence: with trackIdx 0 the offset is 0, so the
// result must be identical to the plain book-absolute lookup.
func TestChapterForTrackSecMatchesChapterForSecOnFirstTrack(t *testing.T) {
	t.Parallel()

	durations := []float64{3000, 1000}
	for _, sec := range []float64{0, 59.999, 60, 1199, 1200, 2400, 3000, -1, 99999} {
		wantIdx, wantTitle, wantOK := chapterForSec(multiTrackChapters, sec)
		gotIdx, gotTitle, gotOK := ChapterForTrackSec(multiTrackChapters, durations, 0, sec)
		if gotIdx != wantIdx || gotTitle != wantTitle || gotOK != wantOK {
			t.Errorf("sec=%v: ChapterForTrackSec = (%d, %q, %v), ChapterForSec = (%d, %q, %v)",
				sec, gotIdx, gotTitle, gotOK, wantIdx, wantTitle, wantOK)
		}
	}
}

// TestBookOffsetSec exercises the offset accumulator directly.
func TestBookOffsetSec(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		durations  []float64
		trackIdx   int
		wantOffset float64
		wantOK     bool
	}{
		{name: "first track", durations: []float64{100, 200, 300}, trackIdx: 0, wantOffset: 0, wantOK: true},
		{name: "second track", durations: []float64{100, 200, 300}, trackIdx: 1, wantOffset: 100, wantOK: true},
		{name: "last track", durations: []float64{100, 200, 300}, trackIdx: 2, wantOffset: 300, wantOK: true},
		{
			// The zero duration is AFTER trackIdx, so it cannot affect the offset.
			name: "unknown duration after the track is irrelevant", durations: []float64{100, 200, 0},
			trackIdx: 1, wantOffset: 100, wantOK: true,
		},
		{name: "unknown duration before the track", durations: []float64{100, 0, 300}, trackIdx: 2, wantOK: false},
		{name: "empty list", durations: nil, trackIdx: 0, wantOK: false},
		{name: "index out of range", durations: []float64{100}, trackIdx: 1, wantOK: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			off, ok := bookOffsetSec(tc.durations, tc.trackIdx)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
				return
			}
			if ok && off != tc.wantOffset {
				t.Errorf("offset = %v, want %v", off, tc.wantOffset)
			}
		})
	}
}
