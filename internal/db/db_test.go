package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/jedwards1230/lil-whisper/internal/library"
)

// scanResultColumns are the 9 columns SELECTed by findSimilar / TextSearch /
// GetChunkContext, in the exact order scanResults expects them. Keeping this
// list co-located with the tests makes a SELECT/Scan reorder regression obvious.
var scanResultColumns = []string{
	"id", "text", "file_path", "chunk_index",
	"start_sec", "end_sec", "speaker", "similarity", "total_chunks",
}

// newTestDB builds a DB with only the layout-aware resolver populated (no pool).
// scanResults never touches the pool, so this is sufficient for execution-level
// scan tests and for exercising the Author/Title derivation.
func newTestDB() *DB {
	cols := []library.Collection{
		{Root: "audio-libation", Layout: "author/title"}, // 3-level
		{Root: "audio-custom", Layout: "author"},         // 2-level single-file
	}
	return &DB{resolver: library.NewResolver("/books", cols)}
}

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

// TestScanResultsPopulatesFields drives db.scanResults() at execution level with
// pgxmock-produced rows and asserts that all 9 scanned columns AND the 4 derived
// fields (Author, Title, Chapter, ChunkID) land in the right struct fields.
//
// This is the test the resolver-only TestScanResultsMetadata could not provide:
// it actually exercises the rows.Scan() argument order, the *string speaker
// dereference (including a nil case), the r.ChunkID = r.ID assignment, and the
// layout-aware Author/Title resolution for both 3-level and 2-level layouts.
func TestScanResultsPopulatesFields(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	speaker := "SPEAKER_01"
	rows := pgxmock.NewRows(scanResultColumns).
		// 3-level libation layout, non-nil speaker.
		AddRow(
			"chunk-1",
			"the elephant and the rider",
			"/books/audio-libation/Jonathan Haidt/The Righteous Mind/01 - Chapter 1.m4b",
			3,
			12.5, 18.0,
			&speaker,
			0.91,
			42,
		).
		// 2-level custom single-file layout, nil speaker.
		AddRow(
			"chunk-2",
			"a different passage entirely",
			"/books/audio-custom/Jonathan Haidt/The Coddling of the American Mind.m4b",
			0,
			0.0, 5.0,
			nil,
			0.50,
			7,
		)

	mock.ExpectQuery("SELECT").WithArgs("elephant", 10).WillReturnRows(rows)

	// Run through the public query path so Query() is actually invoked.
	got, err := db.textSearch(context.Background(), mock, "elephant", 10)
	if err != nil {
		t.Fatalf("textSearch: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}

	// ── Row 1: all 9 scanned fields + 4 derived fields ────────────────────────
	r := got[0]
	if r.ID != "chunk-1" {
		t.Errorf("ID = %q, want chunk-1", r.ID)
	}
	if r.ChunkID != "chunk-1" {
		t.Errorf("ChunkID = %q, want chunk-1 (must mirror ID)", r.ChunkID)
	}
	if r.Content != "the elephant and the rider" {
		t.Errorf("Content = %q", r.Content)
	}
	if r.FilePath != "/books/audio-libation/Jonathan Haidt/The Righteous Mind/01 - Chapter 1.m4b" {
		t.Errorf("FilePath = %q", r.FilePath)
	}
	if r.ChunkIndex != 3 {
		t.Errorf("ChunkIndex = %d, want 3", r.ChunkIndex)
	}
	if r.StartSec != 12.5 {
		t.Errorf("StartSec = %v, want 12.5", r.StartSec)
	}
	if r.EndSec != 18.0 {
		t.Errorf("EndSec = %v, want 18.0", r.EndSec)
	}
	if r.Speaker == nil || *r.Speaker != "SPEAKER_01" {
		t.Errorf("Speaker = %v, want SPEAKER_01", r.Speaker)
	}
	if r.Similarity != 0.91 {
		t.Errorf("Similarity = %v, want 0.91", r.Similarity)
	}
	if r.TotalChunks != 42 {
		t.Errorf("TotalChunks = %d, want 42", r.TotalChunks)
	}
	// Derived (layout-aware) fields — the mis-attribution bug surface.
	if r.Author != "Jonathan Haidt" {
		t.Errorf("Author = %q, want Jonathan Haidt", r.Author)
	}
	if r.Title != "The Righteous Mind" {
		t.Errorf("Title = %q, want The Righteous Mind", r.Title)
	}
	if r.Chapter != "01 - Chapter 1.m4b" {
		t.Errorf("Chapter = %q, want 01 - Chapter 1.m4b", r.Chapter)
	}

	// ── Row 2: nil speaker + 2-level layout ──────────────────────────────────
	r2 := got[1]
	if r2.Speaker != nil {
		t.Errorf("Speaker = %v, want nil for row 2", r2.Speaker)
	}
	if r2.Author != "Jonathan Haidt" {
		t.Errorf("row2 Author = %q, want Jonathan Haidt", r2.Author)
	}
	if r2.Title != "The Coddling of the American Mind" {
		t.Errorf("row2 Title = %q, want The Coddling of the American Mind", r2.Title)
	}
	if r2.ChunkID != "chunk-2" {
		t.Errorf("row2 ChunkID = %q, want chunk-2", r2.ChunkID)
	}
}

// TestTextSearchExecutesAndRanks drives TextSearch's query path against a mock
// pool and asserts (a) the trigram-ranked SQL is actually executed with the
// (query, limit) args, and (b) results are returned in the similarity-ranked
// order the mock supplies — i.e. scanResults preserves row order, so the
// ORDER BY similarity DESC contract is surfaced end-to-end rather than only
// string-checked.
func TestTextSearchExecutesAndRanks(t *testing.T) {
	db := newTestDB()

	// QueryMatcherEqual matches the expected SQL byte-for-byte (the production
	// query contains regex metacharacters like %, $, (, ), || so the default
	// regexp matcher would be brittle here).
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// Mock returns rows already in similarity-DESC order (best match first),
	// mirroring what `ORDER BY similarity(c.text, $1) DESC` yields server-side.
	rows := pgxmock.NewRows(scanResultColumns).
		AddRow("hi", "exact elephant match", "/books/audio-custom/A/Best.m4b", 0, 0.0, 1.0, nil, 0.95, 3).
		AddRow("mid", "partial elephants here", "/books/audio-custom/A/Mid.m4b", 1, 1.0, 2.0, nil, 0.60, 3).
		AddRow("lo", "an ele... fragment", "/books/audio-custom/A/Low.m4b", 2, 2.0, 3.0, nil, 0.20, 3)

	// Assert the exact SQL the production code sends and the bound args.
	mock.ExpectQuery(textSearchSQL).
		WithArgs("elephant", 5).
		WillReturnRows(rows)

	got, err := db.textSearch(context.Background(), mock, "elephant", 5)
	if err != nil {
		t.Fatalf("textSearch: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}

	// Ranked order preserved: descending similarity.
	wantOrder := []struct {
		id  string
		sim float64
	}{
		{"hi", 0.95},
		{"mid", 0.60},
		{"lo", 0.20},
	}
	for i, w := range wantOrder {
		if got[i].ID != w.id {
			t.Errorf("result[%d].ID = %q, want %q (ranking order broken)", i, got[i].ID, w.id)
		}
		if got[i].Similarity != w.sim {
			t.Errorf("result[%d].Similarity = %v, want %v", i, got[i].Similarity, w.sim)
		}
	}

	// Confirm the executed SQL actually carried the trigram predicate + ranking,
	// proving the query that ran is the GIN-friendly one (not a silent fallback).
	if !strings.Contains(textSearchSQL, "c.text % $1") {
		t.Error("executed SQL missing trigram operator")
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
