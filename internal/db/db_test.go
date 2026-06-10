package db

import (
	"os"
	"path/filepath"
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
