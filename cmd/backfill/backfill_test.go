package backfill

import (
	"bytes"
	"context"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/metaprovider"
)

// fakeBackfiller is a test double for Backfiller.
type fakeBackfiller struct {
	books     []db.BookSummary
	upserts   map[string]metaprovider.BookMeta
	upsertErr error
}

func (f *fakeBackfiller) GetBookSummaries(_ context.Context, fil db.BookFilter) ([]db.BookSummary, int, error) {
	var out []db.BookSummary
	for _, b := range f.books {
		if fil.Query == "" || contains(b.Dir, fil.Query) {
			out = append(out, b)
		}
	}
	// Apply pagination.
	if fil.Offset >= len(out) {
		return nil, len(out), nil
	}
	end := fil.Offset + fil.Limit
	if end > len(out) {
		end = len(out)
	}
	return out[fil.Offset:end], len(out), nil
}

func (f *fakeBackfiller) UpsertBookMetadata(_ context.Context, bookDir string, meta metaprovider.BookMeta) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if f.upserts == nil {
		f.upserts = map[string]metaprovider.BookMeta{}
	}
	f.upserts[bookDir] = meta
	return nil
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// absStub is a MetadataProvider that returns a fixed BookMeta.
type absStub struct {
	result metaprovider.BookMeta
}

func (a *absStub) Lookup(_ context.Context, _, _ string) (metaprovider.BookMeta, error) {
	return a.result, nil
}

// TestBackfill_DryRun verifies that without --yes nothing is written.
func TestBackfill_DryRun(t *testing.T) {
	t.Parallel()

	q := &fakeBackfiller{
		books: []db.BookSummary{
			{Dir: "/books/Andy Weir/Project Hail Mary [B08G9PRS1K]", SamplePath: "/books/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b"},
		},
	}
	prov := &absStub{result: metaprovider.BookMeta{
		Title:    "Project Hail Mary",
		Author:   "Andy Weir",
		ASIN:     "B08G9PRS1K",
		Chapters: []metaprovider.Chapter{{Index: 0, Title: "Ch1", StartSec: 0, EndSec: 100}},
		Source:   "abs",
	}}

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, q, prov, options{yes: false}); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if len(q.upserts) != 0 {
		t.Errorf("dry-run should not upsert, got %d upserts", len(q.upserts))
	}
	out := buf.String()
	if out == "" {
		t.Error("dry-run should print preview output")
	}
}

// TestBackfill_Apply verifies that with --yes, books are upserted.
func TestBackfill_Apply(t *testing.T) {
	t.Parallel()

	q := &fakeBackfiller{
		books: []db.BookSummary{
			{Dir: "/books/Andy Weir/Project Hail Mary [B08G9PRS1K]", SamplePath: "/books/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b"},
			{Dir: "/books/Andy Weir/The Martian [B00B5HQ0LQ]", SamplePath: "/books/Andy Weir/The Martian [B00B5HQ0LQ]/01.m4b"},
		},
	}
	prov := &absStub{result: metaprovider.BookMeta{
		Title:    "A Book",
		Author:   "An Author",
		ASIN:     "B0000000XX",
		Chapters: []metaprovider.Chapter{{Index: 0, Title: "Ch1", StartSec: 0, EndSec: 100}},
		Source:   "abs",
	}}

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, q, prov, options{yes: true}); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if len(q.upserts) != 2 {
		t.Errorf("expected 2 upserts, got %d", len(q.upserts))
	}
}

// TestBackfill_NoBooks verifies the empty-library case.
func TestBackfill_NoBooks(t *testing.T) {
	t.Parallel()

	q := &fakeBackfiller{books: nil}
	prov := &absStub{}

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, q, prov, options{yes: true}); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if len(q.upserts) != 0 {
		t.Errorf("expected 0 upserts for empty library, got %d", len(q.upserts))
	}
}

// TestBackfill_ProviderReturnsEmpty verifies that books with no metadata are
// reported as skipped.
func TestBackfill_ProviderReturnsEmpty(t *testing.T) {
	t.Parallel()

	q := &fakeBackfiller{
		books: []db.BookSummary{
			{Dir: "/books/Author/No ASIN Book", SamplePath: "/books/Author/No ASIN Book/01.mp3"},
		},
	}
	prov := &absStub{result: metaprovider.BookMeta{}} // empty = not found

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, q, prov, options{yes: true}); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if len(q.upserts) != 0 {
		t.Errorf("expected 0 upserts when provider returns empty, got %d", len(q.upserts))
	}
	out := buf.String()
	if !containsStr(out, "SKIP") {
		t.Errorf("expected SKIP in output, got: %s", out)
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || contains(s, sub)
}
