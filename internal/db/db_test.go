package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/library"
)

func TestComputeFileChecksum(t *testing.T) {
	tmp, err := os.CreateTemp("", "db_checksum_*.bin")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString("hello world"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tmp.Close()

	db := &DB{}
	sum1, err := db.ComputeFileChecksum(tmp.Name())
	if err != nil {
		t.Fatalf("ComputeFileChecksum: %v", err)
	}
	if sum1 == "" {
		t.Fatal("expected non-empty checksum")
	}

	// Idempotent
	sum2, err := db.ComputeFileChecksum(tmp.Name())
	if err != nil {
		t.Fatalf("ComputeFileChecksum (2nd): %v", err)
	}
	if sum1 != sum2 {
		t.Errorf("checksums differ: %q vs %q", sum1, sum2)
	}
}

func TestComputeFileChecksum_NonExistent(t *testing.T) {
	db := &DB{}
	_, err := db.ComputeFileChecksum("/nonexistent/path/file.m4b")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

// TestScanResultsMetadata validates that the Author/Title fields returned by
// scanResults (via DB.resolver) are correct for both 3-level (author/title/track)
// and 2-level (author/book.m4b) library layouts.
//
// We test the resolver used internally by scanResults rather than scanResults
// itself (which needs a live pgx.Rows), so the coverage is at the logic level
// that was the root cause of the mis-attribution bug.
func TestScanResultsMetadata(t *testing.T) {
	cols := []library.Collection{
		{Root: "audio-libation", Layout: "author/title"}, // 3-level
		{Root: "audio-custom", Layout: "author"},         // 2-level single-file
	}
	resolver := library.NewResolver("/books", cols)
	db := &DB{resolver: resolver}

	cases := []struct {
		name       string
		filePath   string
		wantAuthor string
		wantTitle  string
	}{
		{
			// 3-level: /books/audio-libation/Author/Book/Chapter.m4b
			// bookDir = .../Author/Book  → author=Author, title=Book
			name:       "libation 3-level author/title/track",
			filePath:   "/books/audio-libation/Jonathan Haidt/The Righteous Mind/01 - Chapter 1.m4b",
			wantAuthor: "Jonathan Haidt",
			wantTitle:  "The Righteous Mind",
		},
		{
			// 2-level: /books/audio-custom/Author/Book.m4b
			// bookDir = .../Author → layout=author, title from filename
			name:       "custom 2-level single-file",
			filePath:   "/books/audio-custom/Jonathan Haidt/The Coddling of the American Mind.m4b",
			wantAuthor: "Jonathan Haidt",
			wantTitle:  "The Coddling of the American Mind",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bookDir := filepath.Dir(tc.filePath)
			author, title := db.resolver.Resolve(bookDir, tc.filePath)
			if author != tc.wantAuthor {
				t.Errorf("author = %q, want %q", author, tc.wantAuthor)
			}
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
		})
	}
}

// TestTextSearchSQLShape validates that textSearchSQL uses the trigram similarity
// operator and ordering, and has an ILIKE fallback. This is a regression guard:
// if someone reverts to ILIKE-only the trigram GIN index is bypassed and this
// test will fail.
//
// The test does NOT require a live database — it inspects the SQL string that
// TextSearch sends to the pool. A live-DB integration test is deferred to M-8
// (testcontainers suite, needs Docker in CI).
func TestTextSearchSQLShape(t *testing.T) {
	sql := textSearchSQL

	// Must use the trigram similarity operator so the GIN index on text is hit.
	if !strings.Contains(sql, "c.text % $1") {
		t.Errorf("textSearchSQL missing trigram operator 'c.text %% $1': got:\n%s", sql)
	}

	// Must rank by similarity() descending — the core of the trigram rewrite.
	if !strings.Contains(sql, "similarity(c.text, $1) DESC") {
		t.Errorf("textSearchSQL missing 'ORDER BY similarity(c.text, $1) DESC': got:\n%s", sql)
	}

	// Must have a similarity() column in the SELECT list for scanning.
	if !strings.Contains(sql, "similarity(c.text, $1) AS similarity") {
		t.Errorf("textSearchSQL missing 'similarity(c.text, $1) AS similarity' in SELECT: got:\n%s", sql)
	}

	// Must retain the ILIKE fallback for short queries below the pg_trgm threshold.
	if !strings.Contains(sql, "ILIKE") {
		t.Errorf("textSearchSQL missing ILIKE fallback clause: got:\n%s", sql)
	}

	// Must NOT be ILIKE-only: confirm the trigram WHERE predicate is present.
	// An ILIKE-only rewrite would be: WHERE c.text ILIKE ... with no '% $1'.
	if strings.Contains(sql, "WHERE c.text ILIKE") && !strings.Contains(sql, "c.text % $1") {
		t.Errorf("textSearchSQL reverted to ILIKE-only — trigram operator missing: got:\n%s", sql)
	}
}

func TestComputeFileChecksum_DifferentFiles(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.bin")
	f2 := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(f1, []byte("content-a"), 0600); err != nil {
		t.Fatalf("write f1: %v", err)
	}
	if err := os.WriteFile(f2, []byte("content-b"), 0600); err != nil {
		t.Fatalf("write f2: %v", err)
	}

	db := &DB{}
	sum1, _ := db.ComputeFileChecksum(f1)
	sum2, _ := db.ComputeFileChecksum(f2)
	if sum1 == sum2 {
		t.Error("expected different checksums for different content")
	}
}
