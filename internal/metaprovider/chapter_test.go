package metaprovider

// chapterForSec is an internal function tested here via white-box access
// (same package, _test suffix via file naming convention is not used so we
// stay in package metaprovider to access the unexported function directly).

import (
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
