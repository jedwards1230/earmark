package mcp

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// TestSegmentIndexForTime covers the deep-jump target resolution: the boundary
// (End>=t), clamps for t<=0 and t past the end, and the empty-slice guard.
func TestSegmentIndexForTime(t *testing.T) {
	segs := []db.Segment{
		{ID: 0, Start: 0, End: 10},
		{ID: 1, Start: 10, End: 20},
		{ID: 2, Start: 20, End: 30},
	}
	cases := []struct {
		name string
		t    float64
		want int
	}{
		{"zero", 0, 0},
		{"negative", -5, 0},
		{"inside first", 5, 0},
		{"boundary end-of-first (End>=t)", 10, 0},
		{"just past first", 10.1, 1},
		{"inside middle", 15, 1},
		{"inside last", 25, 2},
		{"past end clamps to last", 9999, 2},
	}
	for _, c := range cases {
		if got := segmentIndexForTime(segs, c.t); got != c.want {
			t.Errorf("%s: segmentIndexForTime(t=%v)=%d, want %d", c.name, c.t, got, c.want)
		}
	}
	if got := segmentIndexForTime(nil, 5); got != 0 {
		t.Errorf("empty slice: got %d, want 0", got)
	}
}

// (paginateSegments is covered by TestPaginateSegments in dashboard_test.go.)

// TestSortBookRows covers each sort key and the titleKey tie-break.
func TestSortBookRows(t *testing.T) {
	mk := func(dir, author, title string, total, done, findings int) bookRow {
		return bookRow{Dir: dir, Author: author, Title: title, Total: total, Done: done, FindingCount: findings}
	}
	first := func(rows []bookRow) string { return rows[0].Dir }

	// findings: highest count first.
	rows := []bookRow{mk("a", "", "A", 10, 1, 2), mk("b", "", "B", 10, 1, 9), mk("c", "", "C", 10, 1, 0)}
	sortBookRows(rows, "findings")
	if first(rows) != "b" {
		t.Errorf("findings sort: first=%s, want b (9 findings)", first(rows))
	}

	// progress: highest done-fraction first.
	rows = []bookRow{mk("a", "", "A", 10, 2, 0), mk("b", "", "B", 10, 9, 0), mk("c", "", "C", 10, 5, 0)}
	sortBookRows(rows, "progress")
	if first(rows) != "b" {
		t.Errorf("progress sort: first=%s, want b (0.9)", first(rows))
	}

	// title: case-insensitive author+title order.
	rows = []bookRow{mk("z", "Zadie", "Z", 1, 0, 0), mk("a", "alpha", "A", 1, 0, 0)}
	sortBookRows(rows, "title")
	if first(rows) != "a" {
		t.Errorf("title sort: first=%s, want a (alpha)", first(rows))
	}

	// findings tie-break falls back to titleKey.
	rows = []bookRow{mk("z", "Zoe", "Z", 1, 0, 5), mk("a", "Amy", "A", 1, 0, 5)}
	sortBookRows(rows, "findings")
	if first(rows) != "a" {
		t.Errorf("findings tie-break: first=%s, want a (titleKey)", first(rows))
	}

	// recent: no-op, preserves input (SQL) order.
	rows = []bookRow{mk("c", "", "C", 1, 0, 0), mk("a", "", "A", 1, 0, 0), mk("b", "", "B", 1, 0, 0)}
	sortBookRows(rows, "recent")
	if rows[0].Dir != "c" || rows[1].Dir != "a" || rows[2].Dir != "b" {
		t.Errorf("recent sort reordered rows: %s,%s,%s", rows[0].Dir, rows[1].Dir, rows[2].Dir)
	}
}

func TestValidSort(t *testing.T) {
	for _, s := range []string{"title", "progress", "findings"} {
		if got := validSort(s); got != s {
			t.Errorf("validSort(%q)=%q, want %q", s, got, s)
		}
	}
	for _, s := range []string{"", "recent", "bogus", "DROP TABLE", "Title"} {
		if got := validSort(s); got != "recent" {
			t.Errorf("validSort(%q)=%q, want recent", s, got)
		}
	}
}

func TestIsTruthy(t *testing.T) {
	for _, s := range []string{"1", "true", "on", "yes", "  TRUE  ", "Yes"} {
		if !isTruthy(s) {
			t.Errorf("isTruthy(%q)=false, want true", s)
		}
	}
	for _, s := range []string{"", "0", "false", "no", "off", "2", "maybe"} {
		if isTruthy(s) {
			t.Errorf("isTruthy(%q)=true, want false", s)
		}
	}
}

// TestDeepJumpHrefEscapesJobID proves the finding "Where" deep-link is safe
// against a malicious JobID: html/template auto-escapes the value in the href
// URL-query context (e.g. a quote becomes %22), so an attribute break-out like
// `" onclick=` cannot be injected. This is the rendering-layer guarantee that
// makes the plain-string JobID safe without manual escaping.
func TestDeepJumpHrefEscapesJobID(t *testing.T) {
	dir := "/books/A/B"
	evil := `j" onclick="alert(1)` // attribute-breakout attempt
	findings := []db.FindingRow{
		{ID: "f1", FilePath: dir + "/01.m4b", BookDir: dir, JobID: &evil,
			StartSec: 12.3, EndSec: 18.0, OriginalText: "x",
			IssueType: "misheard_word", Confidence: 0.9},
	}
	d := bookData{
		Dir: dir, DirQuery: "x", Title: "B", Author: "A", Total: 1, Done: 1,
		Tracks:          []db.RecentJob{{ID: "t1", FilePath: dir + "/01.m4b", Status: "done", UpdatedAt: time.Now()}},
		Findings:        findings,
		FindingsByTrack: map[string]int{dir + "/01.m4b": 1},
	}
	var buf bytes.Buffer
	if err := bookFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `onclick=`) {
		t.Errorf("XSS: injected onclick attribute survived escaping:\n%s", out)
	}
	if !strings.Contains(out, "%22") {
		t.Errorf("expected the quote in the JobID to be percent-escaped (%%22) in the href; output:\n%s", out)
	}
}
