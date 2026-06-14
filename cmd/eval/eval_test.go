package eval

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
)

// fakeRunner records the RunOptions it was called with and returns canned data.
type fakeRunner struct {
	findings []db.Finding
	stats    evalpkg.RunStats
	gotOpts  evalpkg.RunOptions
	called   bool
}

func (f *fakeRunner) Run(_ context.Context, o evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error) {
	f.called = true
	f.gotOpts = o
	return f.findings, f.stats, nil
}

func sampleFinding() db.Finding {
	return db.Finding{FilePath: "/b/Dune/01.m4b", IssueType: "misheard_proper_noun", OriginalText: "Paul Atreides", Confidence: 0.8}
}

func TestRun_DryRunDoesNotWrite(t *testing.T) {
	f := &fakeRunner{
		findings: []db.Finding{sampleFinding()},
		stats:    evalpkg.RunStats{ChunksEvaluated: 1, FindingsFound: 1, Persisted: false},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotOpts.Write {
		t.Error("dry-run must pass Write=false to the runner")
	}
	if s := out.String(); !strings.Contains(s, "(dry-run)") {
		t.Errorf("expected dry-run notice, got:\n%s", s)
	}
}

func TestRun_WritePassesWriteFlag(t *testing.T) {
	f := &fakeRunner{
		findings: []db.Finding{sampleFinding()},
		stats:    evalpkg.RunStats{ChunksEvaluated: 1, FindingsFound: 1, Persisted: true},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{write: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !f.gotOpts.Write {
		t.Error("--write must pass Write=true to the runner")
	}
	if s := out.String(); !strings.Contains(s, "Recorded 1 finding") {
		t.Errorf("expected recorded notice, got:\n%s", s)
	}
}

func TestRun_SamplePassesSampleSize(t *testing.T) {
	f := &fakeRunner{stats: evalpkg.RunStats{ChunksEvaluated: 0, FindingsFound: 0}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "", options{sample: 25}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotOpts.Sample != 25 {
		t.Errorf("Sample = %d, want 25", f.gotOpts.Sample)
	}
}

func TestRun_RejectsNoScope(t *testing.T) {
	f := &fakeRunner{}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "", options{}); err == nil {
		t.Fatal("expected error with neither book nor --sample")
	}
	if f.called {
		t.Error("runner should not be called when scope is invalid")
	}
}

func TestRun_RejectsBookAndSample(t *testing.T) {
	f := &fakeRunner{}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{sample: 10}); err == nil {
		t.Fatal("expected error when both book and --sample are given")
	}
	if f.called {
		t.Error("runner should not be called when scope is ambiguous")
	}
}

func TestTruncate_RuneSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"ascii under limit", "hello", 60, "hello"},
		{"ascii over limit", "abcdef", 3, "abc…"},
		// "Atréïdes Bʁöñ" is multi-byte; truncating at a rune boundary must not
		// split a codepoint (a byte slice at n=5 would corrupt the é/ï).
		{"multibyte not split", "Atréïdes Brön", 5, "Atréï…"},
		{"multibyte under limit", "café", 60, "café"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q,%d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q,%d) produced invalid UTF-8: %q", tc.in, tc.n, got)
			}
		})
	}
}

// TestRunOutput_FormatsFindingsCorrectly verifies the operator-facing preview
// line: confidence as [0.XX] (%.2f), the issue type and filename, and long
// original text truncated to 60 runes + ellipsis.
func TestRunOutput_FormatsFindingsCorrectly(t *testing.T) {
	longText := "Thufir Hawat says the spice must flow across the whole of Arrakis tonight" // > 60 runes
	f := &fakeRunner{
		findings: []db.Finding{{
			FilePath:     "/books/Dune/01.m4b",
			IssueType:    "misheard_proper_noun",
			OriginalText: longText,
			Confidence:   0.8456,
		}},
		stats: evalpkg.RunStats{ChunksEvaluated: 1, FindingsFound: 1},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	s := out.String()

	if !strings.Contains(s, "[0.85]") { // 0.8456 → %.2f → 0.85
		t.Errorf("output should contain confidence [0.85], got:\n%s", s)
	}
	if !strings.Contains(s, "misheard_proper_noun") {
		t.Errorf("output should contain issue_type, got:\n%s", s)
	}
	if !strings.Contains(s, "01.m4b") {
		t.Errorf("output should contain filename, got:\n%s", s)
	}
	if !strings.Contains(s, "…") {
		t.Errorf("long original text should be truncated with an ellipsis, got:\n%s", s)
	}
	// The full (untruncated) long text must NOT appear verbatim.
	if strings.Contains(s, longText) {
		t.Errorf("long original text should have been truncated, but appeared in full:\n%s", s)
	}
}

// TestRunOutput_ReportsSkippedChunks verifies the partial-results notice surfaces
// when transient judge errors skipped chunks (RunStats.ChunksSkipped > 0).
func TestRunOutput_ReportsSkippedChunks(t *testing.T) {
	f := &fakeRunner{
		findings: nil,
		stats:    evalpkg.RunStats{ChunksEvaluated: 0, ChunksSkipped: 3, FindingsFound: 0},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Book", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "3 chunk(s) skipped") {
		t.Errorf("output should report skipped chunks on transient errors, got:\n%s", out.String())
	}
}
