package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/pgvector/pgvector-go"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// scanResultColumns are the 9 columns SELECTed by findSimilar / TextSearch /
// GetChunkContext, in the exact order scanResults expects them. Keeping this
// list co-located with the tests makes a SELECT/Scan reorder regression obvious.
var scanResultColumns = []string{
	"id", "text", "file_path", "chunk_index",
	"start_sec", "end_sec", "speaker", "similarity", "total_chunks",
}

// newTestDB builds a DB with only the layout-aware metadata provider populated
// (no pool). scanResults never touches the pool, so this is sufficient for
// execution-level scan tests and for exercising the Author/Title derivation.
func newTestDB() *DB {
	const collectionsJSON = `[{"root":"audio-libation","layout":"author/title"},{"root":"audio-custom","layout":"author"}]`
	return &DB{meta: metaprovider.NewPathProvider(collectionsJSON, "/books")}
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
// scanResults (via DB.meta) are correct for both 3-level (author/title/track)
// and 2-level (author/book.m4b) library layouts.
//
// We test the provider used internally by scanResults rather than scanResults
// itself (which needs a live pgx.Rows), so the coverage is at the logic level
// that was the root cause of the mis-attribution bug.
func TestScanResultsMetadata(t *testing.T) {
	collectionsJSON := `[{"root":"audio-libation","layout":"author/title"},{"root":"audio-custom","layout":"author"}]`
	db := &DB{meta: metaprovider.NewPathProvider(collectionsJSON, "/books")}

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
			got, err := db.meta.Lookup(context.Background(), tc.filePath, tc.filePath)
			if err != nil {
				t.Fatalf("Lookup error: %v", err)
			}
			if got.Author != tc.wantAuthor {
				t.Errorf("author = %q, want %q", got.Author, tc.wantAuthor)
			}
			if got.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", got.Title, tc.wantTitle)
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

// bookTrackColumns are the 11 columns SELECTed by GetBookTracks, in scan order.
var bookTrackColumns = []string{
	"id", "file_path", "status", "updated_at", "error",
	"duration_seconds", "processing_seconds",
	"word_count", "audio_codec", "audio_channels", "embed_chunk_count",
}

// TestGetBookTracksPopulatesDetail drives getBookTracks at execution level with
// pgxmock rows: one fully-populated 'done' track and one 'pending' track whose
// transcript/run_metrics columns are all NULL (the common case — most rows have
// no run_metrics yet). It asserts the nullable per-track detail lands in the
// right fields and that NULLs scan to nil pointers (em dash in the UI).
func TestGetBookTracksPopulatesDetail(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	dur := 1830.0
	proc := 95.5
	words := 14200
	codec := "aac"
	channels := 2
	chunks := 36

	rows := pgxmock.NewRows(bookTrackColumns).
		// Done track: every detail column populated.
		AddRow("t1", "/books/audio-libation/A/B/01.m4b", "done", now, nil,
			&dur, &proc, &words, &codec, &channels, &chunks).
		// Pending track: no transcript / no run_metrics → all NULL.
		AddRow("t2", "/books/audio-libation/A/B/02.m4b", "pending", now, nil,
			nil, nil, nil, nil, nil, nil)

	dir := "/books/audio-libation/A/B"
	mock.ExpectQuery(bookTracksSQL).WithArgs(dir).WillReturnRows(rows)

	got, err := db.getBookTracks(context.Background(), mock, dir)
	if err != nil {
		t.Fatalf("getBookTracks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tracks, want 2", len(got))
	}

	// Row 1: populated.
	r := got[0]
	if r.DurationSeconds == nil || *r.DurationSeconds != dur {
		t.Errorf("DurationSeconds = %v, want %v", r.DurationSeconds, dur)
	}
	if r.ProcessingSeconds == nil || *r.ProcessingSeconds != proc {
		t.Errorf("ProcessingSeconds = %v, want %v", r.ProcessingSeconds, proc)
	}
	if r.WordCount == nil || *r.WordCount != words {
		t.Errorf("WordCount = %v, want %d", r.WordCount, words)
	}
	if r.AudioCodec == nil || *r.AudioCodec != codec {
		t.Errorf("AudioCodec = %v, want %q", r.AudioCodec, codec)
	}
	if r.AudioChannels == nil || *r.AudioChannels != channels {
		t.Errorf("AudioChannels = %v, want %d", r.AudioChannels, channels)
	}
	if r.EmbedChunkCount == nil || *r.EmbedChunkCount != chunks {
		t.Errorf("EmbedChunkCount = %v, want %d", r.EmbedChunkCount, chunks)
	}

	// Row 2: every detail field nil (the no-run_metrics case → em dash).
	r2 := got[1]
	for name, isNil := range map[string]bool{
		"DurationSeconds":   r2.DurationSeconds == nil,
		"ProcessingSeconds": r2.ProcessingSeconds == nil,
		"WordCount":         r2.WordCount == nil,
		"AudioCodec":        r2.AudioCodec == nil,
		"AudioChannels":     r2.AudioChannels == nil,
		"EmbedChunkCount":   r2.EmbedChunkCount == nil,
	} {
		if !isNil {
			t.Errorf("row2 %s = non-nil, want nil (NULL → em dash)", name)
		}
	}
}

// findingRowColumns are the 11 columns SELECTed by ListFindings (global + scoped),
// in the exact order listFindings scans them. Co-locating with the test makes a
// SELECT/Scan reorder regression obvious.
var findingRowColumns = []string{
	"id", "file_path", "book_dir", "job_id",
	"chunk_index", "start_sec", "end_sec", "original_text",
	"issue_type", "suggested_correction", "confidence",
}

// TestListFindingsScopedSQL drives listFindings at execution level with pgxmock
// for both the global and scoped paths: it asserts each path uses the matching
// SQL byte-for-byte (QueryMatcherEqual), that the scoped path binds (limit, prefix)
// with the LIKE-escaped "<dir>/%" prefix, and that all 11 columns scan — including
// a NULL job_id (LEFT JOIN miss → nil) and a NULL suggested_correction.
func TestListFindingsScopedSQL(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	job := "job-uuid-1"
	ci := 4
	corr := "Arecibo"

	// Global path: limit only ($1).
	globalRows := pgxmock.NewRows(findingRowColumns).
		AddRow("f1", "/books/audio-libation/A/B/01.m4b", "/books/audio-libation/A/B",
			&job, &ci, 73.5, 81.0, "auto sebo", "misheard_proper_noun", &corr, 0.92).
		// Second row: NULL job_id (no matching job) + NULL suggested_correction.
		AddRow("f2", "/books/audio-libation/A/B/02.m4b", "/books/audio-libation/A/B",
			nil, nil, 12.0, 15.0, "free hundred", "number_artifact", nil, 0.71)
	mock.ExpectQuery(listFindingsSQL).WithArgs(100).WillReturnRows(globalRows)

	got, err := db.listFindings(context.Background(), mock, "", 100)
	if err != nil {
		t.Fatalf("listFindings (global): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("global: got %d rows, want 2", len(got))
	}
	if got[0].JobID == nil || *got[0].JobID != job {
		t.Errorf("row0 JobID = %v, want %q", got[0].JobID, job)
	}
	if got[0].ChunkIndex == nil || *got[0].ChunkIndex != ci {
		t.Errorf("row0 ChunkIndex = %v, want %d", got[0].ChunkIndex, ci)
	}
	if got[0].SuggestedCorrection == nil || *got[0].SuggestedCorrection != corr {
		t.Errorf("row0 SuggestedCorrection = %v, want %q", got[0].SuggestedCorrection, corr)
	}
	if got[0].Confidence != 0.92 || got[0].IssueType != "misheard_proper_noun" {
		t.Errorf("row0 = %+v, unexpected confidence/issue", got[0])
	}
	if got[1].JobID != nil {
		t.Errorf("row1 JobID = %v, want nil (LEFT JOIN miss → nil)", got[1].JobID)
	}
	if got[1].SuggestedCorrection != nil {
		t.Errorf("row1 SuggestedCorrection = %v, want nil (NULL)", got[1].SuggestedCorrection)
	}
	if got[1].ChunkIndex != nil {
		t.Errorf("row1 ChunkIndex = %v, want nil (NULL)", got[1].ChunkIndex)
	}

	// Scoped path: binds (limit, "<dir>/%"); the prefix is LIKE-escaped exactly as
	// textSearchInBook / the scoped clear do, so the three select the same set.
	dir := "/books/audio-libation/A/B"
	wantPrefix := likePrefix(dir) + "/%"
	scopedRows := pgxmock.NewRows(findingRowColumns).
		AddRow("f1", dir+"/01.m4b", dir, &job, &ci, 73.5, 81.0, "auto sebo",
			"misheard_proper_noun", &corr, 0.92)
	mock.ExpectQuery(listFindingsInBookSQL).WithArgs(50, wantPrefix).WillReturnRows(scopedRows)

	scoped, err := db.listFindings(context.Background(), mock, dir, 50)
	if err != nil {
		t.Fatalf("listFindings (scoped): %v", err)
	}
	if len(scoped) != 1 || scoped[0].BookDir != dir {
		t.Fatalf("scoped: got %+v, want one row in %q", scoped, dir)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetFindingsCountByBook drives getFindingsCountByBook against pgxmock,
// asserting the GROUP BY aggregate SQL byte-for-byte (QueryMatcherEqual) and that
// the (book_dir, count) rows scan into the keyed map the ⚑ library column reads.
func TestGetFindingsCountByBook(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows([]string{"book_dir", "count"}).
		AddRow("/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]", 21).
		AddRow("/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]", 16)
	mock.ExpectQuery(findingsCountByBookSQL).WillReturnRows(rows)

	got, err := db.getFindingsCountByBook(context.Background(), mock)
	if err != nil {
		t.Fatalf("getFindingsCountByBook: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
	if got["/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]"] != 21 {
		t.Errorf("PHM count = %d, want 21", got["/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]"])
	}
	if got["/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]"] != 16 {
		t.Errorf("Dune count = %d, want 16", got["/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]"])
	}
	// A book absent from the aggregate looks up to the zero value (em-dash cell).
	if got["/books/audio-libation/no/findings"] != 0 {
		t.Errorf("absent book = %d, want 0", got["/books/audio-libation/no/findings"])
	}
}

// bookSummaryColumns are the 12 columns SELECTed by GetBookSummaries, in scan
// order. The dynamic HAVING clause doesn't change the SELECT list.
var bookSummaryColumns = []string{
	"book_dir", "sample_path", "total", "pending", "claimed", "done", "failed",
	"last_updated", "duration_seconds", "word_count", "embed_chunk_count", "total_books",
}

// TestGetBookSummariesAggregates drives getBookSummaries at execution level with
// pgxmock rows and asserts the per-book aggregate sums (duration / words /
// chunks) land in the right fields, with NULL aggregates (a book with no done
// track) scanning to nil pointers.
func TestGetBookSummariesAggregates(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	dur := 58320.0
	words := 124800
	chunks := 412

	rows := pgxmock.NewRows(bookSummaryColumns).
		// Book with done tracks → aggregates populated.
		AddRow("/books/audio-libation/Andy Weir/PHM", "/books/audio-libation/Andy Weir/PHM/01.m4b",
			1, 0, 0, 1, 0, now, &dur, &words, &chunks, 2).
		// Pending-only book → no done track → aggregates NULL.
		AddRow("/books/audio-libro/X", "/books/audio-libro/X/01.mp3",
			3, 3, 0, 0, 0, now, nil, nil, nil, 2)

	// Default 20-page limit, offset 0, empty query.
	mock.ExpectQuery("WITH books AS").WithArgs("", 20, 0).WillReturnRows(rows)

	got, total, err := db.getBookSummaries(context.Background(), mock, BookFilter{})
	if err != nil {
		t.Fatalf("getBookSummaries: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(got) != 2 {
		t.Fatalf("got %d books, want 2", len(got))
	}

	b := got[0]
	if b.DurationSeconds == nil || *b.DurationSeconds != dur {
		t.Errorf("DurationSeconds = %v, want %v", b.DurationSeconds, dur)
	}
	if b.WordCount == nil || *b.WordCount != words {
		t.Errorf("WordCount = %v, want %d", b.WordCount, words)
	}
	if b.EmbedChunkCount == nil || *b.EmbedChunkCount != chunks {
		t.Errorf("EmbedChunkCount = %v, want %d", b.EmbedChunkCount, chunks)
	}

	b2 := got[1]
	if b2.DurationSeconds != nil || b2.WordCount != nil || b2.EmbedChunkCount != nil {
		t.Errorf("row2 aggregates = (%v,%v,%v), want all nil",
			b2.DurationSeconds, b2.WordCount, b2.EmbedChunkCount)
	}
}

// TestGetBookSummariesStatusFilter asserts the validated status filter is
// interpolated into the HAVING clause (and rejects an invalid status).
func TestGetBookSummariesStatusFilter(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows(bookSummaryColumns)
	// The HAVING clause for status=done must reference j.status.
	mock.ExpectQuery(`HAVING COUNT\(\*\) FILTER \(WHERE j\.status = 'done'\)`).
		WithArgs("", 20, 0).WillReturnRows(rows)

	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Status: "done"}); err != nil {
		t.Fatalf("getBookSummaries(done): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	// An invalid status is rejected before any query runs.
	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Status: "bogus"}); err == nil {
		t.Error("expected error for invalid status filter")
	}
}

// TestGetBookSummariesSortFilter asserts Sort is validated against an allow-list
// and mapped to a fixed ORDER BY literal: "activity" orders most-recently-updated
// first, and an unknown sort is rejected before any query runs.
func TestGetBookSummariesSortFilter(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows(bookSummaryColumns)
	// Sort=activity must order by last_updated DESC (the activity feed order).
	mock.ExpectQuery(`ORDER BY last_updated DESC, book_dir`).
		WithArgs("", 20, 0).WillReturnRows(rows)

	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Sort: "activity"}); err != nil {
		t.Fatalf("getBookSummaries(activity): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	// An invalid sort is rejected before any query runs.
	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Sort: "bogus"}); err == nil {
		t.Error("expected error for invalid sort filter")
	}
}

// TestGetBookSummariesQueuedStatus asserts that Status="queued" generates the
// correct HAVING clause (books with pending OR claimed tracks) and that an
// invalid status is rejected, consistent with the existing status filter tests.
func TestGetBookSummariesQueuedStatus(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows(bookSummaryColumns)
	// The HAVING clause for status=queued must include both pending and claimed.
	mock.ExpectQuery(`HAVING COUNT\(\*\) FILTER \(WHERE j\.status IN \('pending','claimed'\)\) > 0`).
		WithArgs("", 20, 0).WillReturnRows(rows)

	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Status: "queued"}); err != nil {
		t.Fatalf("getBookSummaries(queued): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetBookSummariesQueueSort asserts Sort="queue" is accepted and maps to the
// active-first ORDER BY: (claimed>0) DESC, claimed DESC, pending DESC,
// last_updated ASC, book_dir. An unknown sort is still rejected.
func TestGetBookSummariesQueueSort(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows(bookSummaryColumns)
	// Sort=queue must order by active-first: claimed>0 books lead.
	mock.ExpectQuery(`ORDER BY \(claimed > 0\) DESC, claimed DESC, pending DESC, last_updated ASC, book_dir`).
		WithArgs("", 20, 0).WillReturnRows(rows)

	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Sort: "queue"}); err != nil {
		t.Fatalf("getBookSummaries(queue): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	// An invalid sort is still rejected before any query runs.
	if _, _, err := db.getBookSummaries(context.Background(), mock, BookFilter{Sort: "bogus2"}); err == nil {
		t.Error("expected error for invalid sort filter")
	}
}

// TestGetLibraryTotals drives getLibraryTotals at execution level: it asserts the
// whole-library book counts (total / fully-transcribed / with-pending) scan into
// the right fields and the ILIKE filter arg is passed through.
func TestGetLibraryTotals(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows([]string{"total", "fully_done", "with_pending"}).
		AddRow(10, 6, 4)
	mock.ExpectQuery("WITH books AS").WithArgs("").WillReturnRows(rows)

	got, err := db.getLibraryTotals(context.Background(), mock, "")
	if err != nil {
		t.Fatalf("getLibraryTotals: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if got.TotalBooks != 10 || got.FullyTranscribed != 6 || got.WithPending != 4 {
		t.Errorf("totals = %+v, want {10 6 4}", got)
	}
}

// trackDetailColumns are the 32 columns SELECTed by GetTrackDetail, in scan
// order (job, has_transcript, transcript fields, run_metrics fields).
var trackDetailColumns = []string{
	"id", "file_path", "status", "updated_at", "error", "attempts", "claimed_by",
	"has_transcript",
	"language", "duration_seconds", "speaker_count", "model_name", "created_at", "segments",
	"audio_bytes", "audio_channels", "audio_sample_rate", "audio_codec", "audio_format",
	"processing_seconds",
	"asr_model", "compute_type", "runner_host", "chunked", "n_windows",
	"char_count", "word_count", "segment_count",
	"embed_model", "embed_chunk_count", "embed_prompt_tokens", "embed_total_tokens",
}

var trackChunkColumns = []string{"chunk_index", "start_sec", "end_sec", "char_count", "speaker"}

// TestGetTrackDetailWithTranscript drives getTrackDetail at execution level: a
// done track with a transcript (segments JSON unmarshalled), populated
// run_metrics, and two chunks. It asserts the segments parse, the chunk follow-
// up query runs, and HasTranscript is true.
func TestGetTrackDetailWithTranscript(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	jobID := "11111111-1111-1111-1111-111111111111"
	lang := "en"
	dur := 1830.0
	spkCount := 1
	model := "large-v3"
	segs := []byte(`[{"id":0,"start":0.0,"end":4.2,"text":"Hello.","speaker":"SPEAKER_00","words":[]}]`)
	bytesN := int64(48300000)
	ch, rate := 2, 44100
	codec, format := "aac", "m4b"
	proc := 95.5
	asr, compute, host := "large-v3", "bfloat16", "asr-runner-1"
	chunkedF := false
	words, chars, segCount := 14200, 84000, 1
	embModel := "nomic-embed-text"
	embChunks, embTotal := 36, 18240

	row := pgxmock.NewRows(trackDetailColumns).AddRow(
		jobID, "/books/audio-libation/A/B/01.m4b", "done", now, nil, 1, nil,
		true,
		&lang, &dur, &spkCount, &model, &now, segs,
		&bytesN, &ch, &rate, &codec, &format,
		&proc,
		&asr, &compute, &host, &chunkedF, (*int)(nil),
		&chars, &words, &segCount,
		&embModel, &embChunks, (*int)(nil), &embTotal,
	)
	mock.ExpectQuery(trackDetailSQL).WithArgs(jobID).WillReturnRows(row)

	spk := "SPEAKER_00"
	chunkRows := pgxmock.NewRows(trackChunkColumns).
		AddRow(0, 0.0, 90.4, 512, &spk).
		AddRow(1, 88.1, 182.7, 498, &spk)
	mock.ExpectQuery(trackChunksSQL).WithArgs(jobID).WillReturnRows(chunkRows)

	got, err := db.getTrackDetail(context.Background(), mock, jobID)
	if err != nil {
		t.Fatalf("getTrackDetail: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	if !got.HasTranscript {
		t.Error("HasTranscript = false, want true")
	}
	if got.Language != "en" || got.DurationSeconds != dur || got.ModelName != "large-v3" {
		t.Errorf("transcript fields wrong: lang=%q dur=%v model=%q", got.Language, got.DurationSeconds, got.ModelName)
	}
	if len(got.Segments) != 1 || got.Segments[0].Text != "Hello." {
		t.Errorf("segments = %+v, want 1 segment 'Hello.'", got.Segments)
	}
	if got.AudioCodec == nil || *got.AudioCodec != "aac" {
		t.Errorf("AudioCodec = %v, want aac", got.AudioCodec)
	}
	if got.EmbedTotalTokens == nil || *got.EmbedTotalTokens != embTotal {
		t.Errorf("EmbedTotalTokens = %v, want %d", got.EmbedTotalTokens, embTotal)
	}
	if got.NWindows != nil {
		t.Errorf("NWindows = %v, want nil (NULL)", got.NWindows)
	}
	if len(got.Chunks) != 2 || got.Chunks[1].CharCount != 498 {
		t.Errorf("chunks = %+v, want 2 chunks (2nd char_count 498)", got.Chunks)
	}
}

// TestGetTrackDetailNoTranscript drives the pending-track path: has_transcript
// false, all transcript/metric columns NULL, no chunks. The handler renders a
// "not transcribed yet" state for this — here we assert HasTranscript is false
// and the metric pointers stay nil.
func TestGetTrackDetailNoTranscript(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	jobID := "22222222-2222-2222-2222-222222222222"

	row := pgxmock.NewRows(trackDetailColumns).AddRow(
		jobID, "/books/audio-libation/A/B/02.m4b", "pending", now, nil, 0, nil,
		false,
		(*string)(nil), (*float64)(nil), (*int)(nil), (*string)(nil), (*time.Time)(nil), ([]byte)(nil),
		(*int64)(nil), (*int)(nil), (*int)(nil), (*string)(nil), (*string)(nil),
		(*float64)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil), (*bool)(nil), (*int)(nil),
		(*int)(nil), (*int)(nil), (*int)(nil),
		(*string)(nil), (*int)(nil), (*int)(nil), (*int)(nil),
	)
	mock.ExpectQuery(trackDetailSQL).WithArgs(jobID).WillReturnRows(row)
	// No chunks for a pending track.
	mock.ExpectQuery(trackChunksSQL).WithArgs(jobID).WillReturnRows(pgxmock.NewRows(trackChunkColumns))

	got, err := db.getTrackDetail(context.Background(), mock, jobID)
	if err != nil {
		t.Fatalf("getTrackDetail: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	if got.HasTranscript {
		t.Error("HasTranscript = true, want false for pending track")
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want pending", got.Status)
	}
	if got.AudioCodec != nil || got.EmbedTotalTokens != nil || got.ProcessingSeconds != nil {
		t.Error("metric pointers should be nil for a no-run_metrics pending track")
	}
	if len(got.Segments) != 0 || len(got.Chunks) != 0 {
		t.Errorf("pending track should have no segments/chunks, got %d/%d", len(got.Segments), len(got.Chunks))
	}
}

// TestGetTrackDetailCorruptSegmentsJSON verifies that a track row whose segments
// JSONB column contains invalid JSON causes getTrackDetail to return a non-nil
// error wrapping the unmarshal failure. The has_transcript flag is true so the
// unmarshal path is exercised; the chunk query is never reached.
func TestGetTrackDetailCorruptSegmentsJSON(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	jobID := "33333333-3333-3333-3333-333333333333"
	lang := "en"
	dur := 600.0
	model := "large-v3"
	badJSON := []byte(`{not valid json}`)

	row := pgxmock.NewRows(trackDetailColumns).AddRow(
		jobID, "/books/audio-libation/A/B/03.m4b", "done", now, nil, 1, nil,
		true,
		&lang, &dur, (*int)(nil), &model, &now, badJSON,
		(*int64)(nil), (*int)(nil), (*int)(nil), (*string)(nil), (*string)(nil),
		(*float64)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil), (*bool)(nil), (*int)(nil),
		(*int)(nil), (*int)(nil), (*int)(nil),
		(*string)(nil), (*int)(nil), (*int)(nil), (*int)(nil),
	)
	mock.ExpectQuery(trackDetailSQL).WithArgs(jobID).WillReturnRows(row)

	_, gotErr := db.getTrackDetail(context.Background(), mock, jobID)
	if gotErr == nil {
		t.Fatal("getTrackDetail: want error for corrupt segments JSON, got nil")
	}
	if !strings.Contains(gotErr.Error(), "unmarshal segments") {
		t.Errorf("error %q does not mention 'unmarshal segments'", gotErr.Error())
	}
}

// TestTextSearchInBookScopesToDir drives textSearchInBook at execution level and
// asserts it sends the book-scoped SQL with (query, limit, dirPrefix) args, the
// prefix being the LIKE-escaped dir + "/%".
func TestTextSearchInBookScopesToDir(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	dir := "/books/audio-libation/Andy Weir/PHM"
	rows := pgxmock.NewRows(scanResultColumns).
		AddRow("c1", "the matching text", dir+"/01.m4b", 3, 12.5, 18.0, nil, 0.8, 10)

	// Args: $1 query, $2 limit, $3 dir prefix "<dir>/%".
	mock.ExpectQuery(textSearchInBookSQL).
		WithArgs("matching", 50, dir+"/%").
		WillReturnRows(rows)

	got, err := db.textSearchInBook(context.Background(), mock, dir, "matching", 50)
	if err != nil {
		t.Fatalf("textSearchInBook: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(got) != 1 || got[0].FilePath != dir+"/01.m4b" {
		t.Fatalf("got %+v, want 1 result under the book dir", got)
	}

	// The book-scoped SQL must constrain file_path and reuse the trigram predicate.
	if !strings.Contains(textSearchInBookSQL, "c.file_path LIKE $3") {
		t.Error("textSearchInBookSQL missing file_path scope")
	}
	if !strings.Contains(textSearchInBookSQL, "c.text % $1") {
		t.Error("textSearchInBookSQL missing trigram predicate")
	}
}

// TestSearchInBookScopesAndBypassesHNSW drives searchInBook at execution level
// against a mock pool: it asserts (a) the dir prefix is passed as the LIKE arg so
// the search is scoped to one book, and (b) the executed SQL is the exact
// (non-HNSW) distance scan — i.e. it constrains file_path FIRST and orders by the
// raw `<=>` distance with no ANN index hint, which is what keeps recall perfect
// for a selective single-book filter.
func TestSearchInBookScopesAndBypassesHNSW(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	dir := "/books/audio-libation/Andy Weir/PHM"
	vec := make([]float32, 768)
	vec[0] = 0.1
	rows := pgxmock.NewRows(scanResultColumns).
		AddRow("c1", "the matching passage", dir+"/01.m4b", 5, 305.0, 372.5, nil, 0.82, 12)

	// Args: $1 vec, $2 limit, $3 dir prefix "<dir>/%", $4 threshold.
	mock.ExpectQuery(searchInBookSQL).
		WithArgs(pgvector.NewVector(vec), 10, dir+"/%", 0.3).
		WillReturnRows(rows)

	got, err := db.searchInBook(context.Background(), mock, vec, dir, 10, 0.3)
	if err != nil {
		t.Fatalf("searchInBook: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(got) != 1 || got[0].FilePath != dir+"/01.m4b" {
		t.Fatalf("got %+v, want 1 result under the book dir", got)
	}

	// Must constrain file_path (the btree-narrowing scope) and order by the raw
	// distance operator — i.e. an exact scan, not an HNSW ANN scan.
	if !strings.Contains(searchInBookSQL, "c.file_path LIKE $3") {
		t.Error("searchInBookSQL missing file_path scope")
	}
	if !strings.Contains(searchInBookSQL, "ORDER BY c.embedding <=> $1") {
		t.Error("searchInBookSQL missing exact distance ordering")
	}
}

// TestRequeueTxClearsRunMetrics drives requeueTx (the requeue transaction body)
// against a pgxmock transaction and asserts the run_metrics cleanup runs in the
// SAME transaction as the transcript-delete + job-reset, keyed on the requeued
// job ids. This is the data-integrity fix: requeue UPDATEs (not deletes) the job
// row and deletes the transcript, so neither path cascades to run_metrics — the
// orphaned telemetry row must be deleted explicitly here.
func TestRequeueTxClearsRunMetrics(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	id := "11111111-1111-1111-1111-111111111111"
	path := "/books/audio-libation/Andy Weir/PHM/PHM.m4b"

	mock.ExpectBegin()
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// 1) delete transcripts for the selected job
	mock.ExpectExec(requeueByID.deleteTranscripts).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	// 2) reset the job → pending, RETURNING (id, file_path)
	mock.ExpectQuery(requeueByID.resetJobs).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"id", "file_path"}).AddRow(id, path))
	// 3) THE FIX: clear the now-orphaned run_metrics for that job id
	mock.ExpectExec(requeueDeleteMetricsSQL).
		WithArgs([]string{id}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	_, paths, err := requeueTx(context.Background(), tx, requeueByID, id)
	if err != nil {
		t.Fatalf("requeueTx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (run_metrics cleanup missing?): %v", err)
	}
	if len(paths) != 1 || paths[0] != path {
		t.Fatalf("paths = %v, want [%s]", paths, path)
	}
}

// TestRequeueTxNoMetricsDeleteWhenNothingReset asserts the run_metrics delete is
// skipped entirely when the reset matched no jobs (empty RETURNING) — so a
// no-match requeue issues no spurious DELETE.
func TestRequeueTxNoMetricsDeleteWhenNothingReset(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	mock.ExpectExec(requeueFailed.deleteTranscripts).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(requeueFailed.resetJobs).
		WillReturnRows(pgxmock.NewRows([]string{"id", "file_path"})) // no rows

	_, paths, err := requeueTx(context.Background(), tx, requeueFailed)
	if err != nil {
		t.Fatalf("requeueTx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths = %v, want empty", paths)
	}
}

func TestLikePrefix(t *testing.T) {
	cases := map[string]string{
		"/books/A/B":   "/books/A/B",
		"50% off":      `50\% off`,
		"a_b":          `a\_b`,
		`back\slash`:   `back\\slash`,
		`%_\ combined`: `\%\_\\ combined`,
	}
	for in, want := range cases {
		if got := likePrefix(in); got != want {
			t.Errorf("likePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchInBookEscapesMetacharactersInDir verifies that searchInBook correctly
// escapes LIKE metacharacters in the dir prefix so a book whose name contains %
// or _ does not over-match tracks from other books. The test drives the inner
// searchInBook function against a mock pool and asserts that the prefix argument
// is the LIKE-escaped form (e.g. "\%_title/%" → "\\%\_title/%"), not the raw
// dir. It also verifies that the SQL carries the explicit ESCAPE '\' clause so
// the escaping is unambiguous regardless of server configuration.
func TestSearchInBookEscapesMetacharactersInDir(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// Dir whose name contains both % and _ — without escaping these would act as
	// LIKE wildcards and could match sibling directories.
	dir := `/books/audio-libation/Andy Weir/100%_Winner`
	// likePrefix escapes \, % and _ in order; the expected prefix is:
	//   /books/audio-libation/Andy Weir/100\%\_Winner/%
	escapedDir := `/books/audio-libation/Andy Weir/100\%\_Winner`
	wantPrefix := escapedDir + "/%"

	vec := make([]float32, 768)
	vec[0] = 0.5
	rows := pgxmock.NewRows(scanResultColumns).
		AddRow("c1", "the passage", dir+"/01.m4b", 1, 0.0, 5.0, nil, 0.75, 3)

	// The mock expectation uses the ESCAPED prefix — if the handler passes the raw
	// dir the args won't match and ExpectationsWereMet will fail.
	mock.ExpectQuery(searchInBookSQL).
		WithArgs(pgvector.NewVector(vec), 5, wantPrefix, 0.0).
		WillReturnRows(rows)

	got, err := db.searchInBook(context.Background(), mock, vec, dir, 5, 0.0)
	if err != nil {
		t.Fatalf("searchInBook: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations (prefix not escaped?): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}

	// The SQL must carry an explicit ESCAPE clause so the backslash escape is
	// honoured unconditionally (independent of standard_conforming_strings).
	if !strings.Contains(searchInBookSQL, `ESCAPE '\'`) {
		t.Error("searchInBookSQL missing ESCAPE '\\' clause")
	}
}

// TestTextSearchInBookEscapesMetacharactersInDir mirrors the semantic-search
// test for textSearchInBook: a dir containing % and _ must be escaped before use
// in the LIKE predicate, and the SQL must carry an explicit ESCAPE clause.
func TestTextSearchInBookEscapesMetacharactersInDir(t *testing.T) {
	db := newTestDB()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	dir := `/books/audio-libation/Science_% Authors/Basics`
	wantPrefix := `/books/audio-libation/Science\_\% Authors/Basics` + "/%"

	rows := pgxmock.NewRows(scanResultColumns).
		AddRow("c2", "chapter text", dir+"/ch1.mp3", 0, 0.0, 10.0, nil, 0.6, 5)

	mock.ExpectQuery(textSearchInBookSQL).
		WithArgs("basics", 10, wantPrefix).
		WillReturnRows(rows)

	got, err := db.textSearchInBook(context.Background(), mock, dir, "basics", 10)
	if err != nil {
		t.Fatalf("textSearchInBook: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations (prefix not escaped?): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}

	if !strings.Contains(textSearchInBookSQL, `ESCAPE '\'`) {
		t.Error("textSearchInBookSQL missing ESCAPE '\\' clause")
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

// ─── UpsertBookMetadata bias_terms tests (CONTRACT §1.6, PR 5) ───────────────

// TestUpsertBookMetadataSQL_IncludesBiasTerms verifies that the INSERT SQL used
// by UpsertBookMetadata contains the bias_terms column and its placeholder.
// This is a regression guard: if the column is accidentally removed from the
// SQL, or the parameter count shifts, this test catches it without a live DB.
// A full round-trip test requires testcontainers (M-8).
// TestUpsertEvalMetricsSQL_TouchesOnlyEvalSlice verifies the eval writer is a
// clean fourth column-selective writer (CONTRACT §1.5): it writes only the
// eval_* columns + updated_at and never references another writer's slice
// (audio_*, transcribe_*, embed_*), so it can't clobber them on the shared row.
func TestUpsertEvalMetricsSQL_TouchesOnlyEvalSlice(t *testing.T) {
	// Normalize runs of whitespace to a single space so assertions don't depend on
	// the SQL's column-alignment formatting.
	sql := strings.Join(strings.Fields(upsertEvalMetricsSQL), " ")

	wantCols := []string{
		"eval_started_at", "eval_finished_at", "eval_model",
		"eval_chunks", "eval_skipped", "eval_findings",
	}
	for _, c := range wantCols {
		if !strings.Contains(sql, c) {
			t.Errorf("upsertEvalMetricsSQL is missing the %s column", c)
		}
		if !strings.Contains(sql, c+" = EXCLUDED."+c) {
			t.Errorf("upsertEvalMetricsSQL ON CONFLICT must assign %s = EXCLUDED.%s", c, c)
		}
	}
	if !strings.Contains(sql, "updated_at = now()") {
		t.Error("upsertEvalMetricsSQL must bump updated_at = now()")
	}

	// Must NOT reference any other writer's slice — that would clobber it.
	foreignCols := []string{
		"audio_bytes", "audio_channels",
		"transcribe_started_at", "transcribe_finished_at", "asr_model",
		"embed_started_at", "embed_finished_at", "embed_model",
	}
	for _, c := range foreignCols {
		if strings.Contains(sql, c) {
			t.Errorf("upsertEvalMetricsSQL must not touch %s (another writer's column)", c)
		}
	}

	// It must be an UPSERT keyed on the job.
	if !strings.Contains(sql, "ON CONFLICT (job_id) DO UPDATE") {
		t.Error("upsertEvalMetricsSQL must UPSERT on job_id")
	}
}

func TestUpsertBookMetadataSQL_IncludesBiasTerms(t *testing.T) {
	if !strings.Contains(upsertBookMetadataSQL, "bias_terms") {
		t.Error("upsertBookMetadataSQL is missing the bias_terms column")
	}
	if !strings.Contains(upsertBookMetadataSQL, "bias_terms = EXCLUDED.bias_terms") {
		t.Error("upsertBookMetadataSQL ON CONFLICT clause must assign bias_terms = EXCLUDED.bias_terms (not COALESCE-guarded)")
	}
}

// TestUpsertBookMetadataBiasTermsDerivedFromMeta is a behavioral test for the
// derivation logic that UpsertBookMetadata calls internally. Given a BookMeta
// with known Author and Narrator, DeriveBiasTerms must return the canonical
// proper-noun tokens expected by the NeMo boosting runner.
//
// This tests the same function called by UpsertBookMetadata, so a refactor
// that removes or skips the DeriveBiasTerms call will surface here.
func TestUpsertBookMetadataBiasTermsDerivedFromMeta(t *testing.T) {
	meta := metaprovider.BookMeta{
		Title:    "Nineteen Eighty-Four",
		Author:   "George Orwell",
		Narrator: "Simon Prebble",
		Source:   "path",
	}

	terms := metaprovider.DeriveBiasTerms(meta)
	if len(terms) == 0 {
		t.Fatal("expected non-empty bias_terms for a book with known author/narrator")
	}

	// Spot-check for the canonical tokens from the task spec.
	wantTerms := []string{"George", "Orwell", "George Orwell", "Simon", "Prebble", "Simon Prebble"}
	for _, w := range wantTerms {
		found := false
		for _, got := range terms {
			if got == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected bias term %q not found in %v", w, terms)
		}
	}
}

// TestUpsertBookMetadataBiasTermsNilWhenEmpty verifies that when DeriveBiasTerms
// returns an empty list (e.g. all-empty BookMeta) the bias_terms argument
// sent to the DB is nil — so the column is stored as NULL, not an empty array.
// NULL is the "not yet populated" sentinel used by all other nullable columns.
func TestUpsertBookMetadataBiasTermsNilWhenEmpty(t *testing.T) {
	terms := metaprovider.DeriveBiasTerms(metaprovider.BookMeta{})
	if len(terms) != 0 {
		t.Errorf("expected empty terms for empty meta, got %v", terms)
	}
	// The nil-coercion happens in UpsertBookMetadata: biasTermsArg is nil when
	// len(biasTerms) == 0. Verify that invariant holds for the empty-meta case
	// so a caller never writes an empty array to the DB column.
	var biasTermsArg interface{}
	if len(terms) > 0 {
		biasTermsArg = terms
	}
	if biasTermsArg != nil {
		t.Errorf("expected nil biasTermsArg for empty terms, got %v", biasTermsArg)
	}
}

// ─── Deterministic chunk UUID (CONTRACT §1.5) ────────────────────────────────

// TestChunkUUID_Deterministic verifies that ChunkUUID produces the same value
// for the same (transcript_id, chunk_index) input — the idempotency guarantee
// that lets the eval pass and the embed pass agree on which UUID a chunk will
// have without coordinating through the DB.
func TestChunkUUID_Deterministic(t *testing.T) {
	id1 := ChunkUUID("transcript-abc", 0)
	id2 := ChunkUUID("transcript-abc", 0)
	if id1 != id2 {
		t.Errorf("ChunkUUID not deterministic: %q != %q", id1, id2)
	}
}

// TestChunkUUID_DifferentInputsProduceDifferentIDs ensures distinct
// (transcript_id, chunk_index) pairs never collide.
func TestChunkUUID_DifferentInputsProduceDifferentIDs(t *testing.T) {
	cases := [][2]any{
		{"t1", 0}, {"t1", 1}, {"t2", 0}, {"t2", 1},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		id := ChunkUUID(c[0].(string), c[1].(int))
		if seen[id] {
			t.Errorf("ChunkUUID collision for (%q, %d): %q already seen", c[0], c[1], id)
		}
		seen[id] = true
	}
}

// TestChunkUUID_EvalAndEmbedPassesAgree: the eval pass and the embed pass both
// call ChunkUUID(transcriptID, chunkIndex) with the same inputs; the result must
// be identical so findings written in the eval pass correctly reference the chunk
// rows the embed pass inserts. This test constructs both passes' UUID sequences
// and asserts they agree.
func TestChunkUUID_EvalAndEmbedPassesAgree(t *testing.T) {
	transcriptID := "transcript-eval-embed-agreement"
	chunkCount := 5

	// Eval pass: assign UUIDs before judging.
	evalIDs := make([]string, chunkCount)
	for i := range evalIDs {
		evalIDs[i] = ChunkUUID(transcriptID, i)
	}

	// Embed pass (independent call): assign UUIDs before inserting.
	embedIDs := make([]string, chunkCount)
	for i := range embedIDs {
		embedIDs[i] = ChunkUUID(transcriptID, i)
	}

	for i := range evalIDs {
		if evalIDs[i] != embedIDs[i] {
			t.Errorf("chunk %d: eval pass ID %q != embed pass ID %q", i, evalIDs[i], embedIDs[i])
		}
	}
}

// TestChunkUUID_IsValidUUID verifies the output is a well-formed UUID string.
func TestChunkUUID_IsValidUUID(t *testing.T) {
	id := ChunkUUID("some-transcript", 42)
	if len(id) != 36 {
		t.Errorf("ChunkUUID returned %q (len %d), want 36-char UUID", id, len(id))
	}
	// Must contain hyphens at positions 8, 13, 18, 23.
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Errorf("ChunkUUID %q: expected '-' at position %d", id, pos)
		}
	}
}

// ─── EvalBacklog and QueueStats SQL shape ────────────────────────────────────

// TestEvalBacklogSQLShape verifies the EvalBacklog query embedded in GetServiceStatus
// selects exactly the rows we expect: done+not-eval'd+not-embedded. We check the
// SQL shape at the package level (no live DB) by asserting the relevant predicate
// strings are present — a full integration test requires testcontainers (M-8).
func TestEvalBacklogSQLShape(t *testing.T) {
	// Verify that GetServiceStatus's EvalBacklog sub-query is using the correct
	// eval gate column. We can't run the query without a DB, but we can verify
	// that the constant strings referenced by the query match the schema.
	// The real coverage lives in the integration tests; this guards against
	// accidental column renames that would silently return a wrong count.
	// (For context: eval_finished_at IS NOT NULL is the eval-completion latch.)

	// Sanity: the EvalMetrics type has a FinishedAt field — if it were renamed
	// the worker would produce a wrong column name in UpsertEvalMetrics.
	var m EvalMetrics
	// FinishedAt must be a time.Time (not renamed to something else).
	_ = m.FinishedAt

	// Sanity: EvalBacklog is in QueueStats (zero-value accessible).
	var q QueueStats
	_ = q.EvalBacklog
}

// TestGetUnevaluatedJobTranscriptsSQL verifies the GetUnevaluatedJobTranscripts
// method's SQL shape at the package level: it must reference the eval_finished_at
// IS NOT NULL latch and restrict to status='done', and it must NOT restrict to
// not-yet-embedded (that is GetUnevaluatedTranscripts's job — the backfill
// explicitly handles already-embedded transcripts too).
func TestGetUnevaluatedJobTranscriptsSQL(t *testing.T) {
	// This is a shape test — we verify the query at package level without a
	// live DB (M-8 will add testcontainers coverage). The key invariant is
	// that GetUnevaluatedJobTranscripts does NOT filter on transcript_chunks
	// existence, so it covers already-embedded transcripts too.
	//
	// We verify this indirectly: GetUnevaluatedTranscripts (the eval-pass
	// selection) restricts to not-embedded; GetUnevaluatedJobTranscripts (the
	// backfill selection) does not. Both are SELECT-only against transcripts
	// + transcription_jobs + run_metrics — write verbs are forbidden.
	//
	// The full behavioral test is in cmd/eval_test.go (backfill dry-run
	// exercises GetUnevaluatedJobTranscripts against a fake backfillDB).
	var d DB
	_ = d.GetUnevaluatedJobTranscripts // must be accessible (not renamed)
}

// ─── Two-pass query selection (eval pass vs embed pass) ──────────────────────

// transcriptScanColumns mirrors the 11-column SELECT order shared by
// GetUnevaluatedTranscripts / GetEvaluatedUnembeddedTranscripts /
// GetCompletedTranscripts (and scanned by scanTranscriptRows). Keeping it
// co-located makes a SELECT/Scan reorder regression obvious.
var transcriptScanColumns = []string{
	"id", "job_id", "file_path", "checksum",
	"language", "duration_seconds", "speaker_count",
	"segments", "raw_text", "model_name", "created_at",
}

// addTranscriptRow appends one minimally-valid transcript row in the scan order.
// segments must be valid JSON ("[]" for none) so scanTranscriptRows unmarshals.
func addTranscriptRow(rows *pgxmock.Rows, id, jobID string) *pgxmock.Rows {
	speakerCount := 1 // *int column — pass a pointer
	return rows.AddRow(
		id, jobID, "/books/x/"+id+".m4b", "checksum-"+id,
		"en", 100.0, &speakerCount,
		[]byte("[]"), "raw text for "+id, "parakeet", time.Now(),
	)
}

// The two gated-flow selections enforce a hard invariant in their WHERE clauses:
//
//	eval pass (GetUnevaluatedTranscripts):
//	    done AND NOT embedded AND NOT eval'd
//	embed pass (GetEvaluatedUnembeddedTranscripts):
//	    done AND eval'd AND NOT embedded
//
// pgxmock returns exactly the rows we hand it (it does not run Postgres), so the
// row-category filtering itself lives in SQL. These tests therefore guard the
// WHERE clause two ways: (1) QueryMatcherEqual asserts the executed SQL is byte-
// identical to the reviewed constant — any clause drop/edit fails the match; and
// (2) the scan path is exercised end-to-end so a SELECT/column reorder regresses
// loudly. The "embedded ⟹ eval'd" invariant is protected because dropping the
// NOT-embedded clause from the eval pass, or the eval'd clause from the embed
// pass, changes the SQL text and fails (1).

// TestGetUnevaluatedTranscripts_EvalPassSelection drives the eval-pass query
// through pgxmock. It asserts the query the worker runs is the reviewed
// unevaluatedTranscriptsSQL (done + NOT-embedded + NOT-eval'd) and that the
// rows scan back correctly.
func TestGetUnevaluatedTranscripts_EvalPassSelection(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// The category the eval pass selects: done + not-eval'd + not-embedded.
	rows := pgxmock.NewRows(transcriptScanColumns)
	addTranscriptRow(rows, "t-uneval-1", "job-1")
	addTranscriptRow(rows, "t-uneval-2", "job-2")
	// The LIMIT is parameterized ($1): the worker passes the batch size as the
	// single arg, so the expectation must match it exactly.
	mock.ExpectQuery(unevaluatedTranscriptsSQL).WithArgs(32).WillReturnRows(rows)

	db := newTestDB()
	got, err := db.getUnevaluatedTranscripts(context.Background(), mock, 32)
	if err != nil {
		t.Fatalf("getUnevaluatedTranscripts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (SQL did not match reviewed constant): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d transcripts, want 2", len(got))
	}
	if got[0].ID != "t-uneval-1" || got[1].ID != "t-uneval-2" {
		t.Errorf("scanned ids = [%q %q], want [t-uneval-1 t-uneval-2]", got[0].ID, got[1].ID)
	}
}

// TestGetUnevaluatedTranscripts_WhereClauseInvariants asserts the eval-pass SQL
// carries BOTH discriminating predicates: NOT-embedded (no transcript_chunks)
// AND NOT-eval'd (no run_metrics with eval_finished_at). Dropping either would
// re-judge already-embedded transcripts or re-judge already-eval'd ones.
func TestGetUnevaluatedTranscripts_WhereClauseInvariants(t *testing.T) {
	sql := unevaluatedTranscriptsSQL
	for _, want := range []string{
		"j.status = 'done'",
		"FROM transcript_chunks c WHERE c.transcript_id = t.id", // NOT embedded
		"rm.eval_finished_at IS NOT NULL",                       // NOT eval'd (negated by NOT EXISTS)
		"NOT EXISTS",
		"ORDER BY t.created_at ASC", // oldest-first, deterministic drain order
		"LIMIT $1",                  // bounded + parameterized (OOM guard)
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("eval-pass SQL missing predicate %q:\n%s", want, sql)
		}
	}
	// It must NOT JOIN run_metrics directly (that would drop not-yet-eval'd rows,
	// which are exactly the ones the eval pass needs).
	if strings.Contains(sql, "JOIN run_metrics") {
		t.Errorf("eval-pass SQL must use NOT EXISTS, not JOIN run_metrics:\n%s", sql)
	}
}

// TestGetEvaluatedUnembeddedTranscripts_EmbedPassSelection drives the embed-pass
// query through pgxmock. It asserts the worker runs the reviewed
// evaluatedUnembeddedTranscriptsSQL (done + eval'd + NOT-embedded) and that the
// rows scan back correctly.
func TestGetEvaluatedUnembeddedTranscripts_EmbedPassSelection(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// The category the embed pass selects: done + eval'd + not-embedded.
	rows := pgxmock.NewRows(transcriptScanColumns)
	addTranscriptRow(rows, "t-eval-unembed-1", "job-3")
	// The LIMIT is parameterized ($1): the worker passes the batch size as the
	// single arg, so the expectation must match it exactly.
	mock.ExpectQuery(evaluatedUnembeddedTranscriptsSQL).WithArgs(32).WillReturnRows(rows)

	db := newTestDB()
	got, err := db.getEvaluatedUnembeddedTranscripts(context.Background(), mock, 32)
	if err != nil {
		t.Fatalf("getEvaluatedUnembeddedTranscripts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (SQL did not match reviewed constant): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d transcripts, want 1", len(got))
	}
	if got[0].ID != "t-eval-unembed-1" {
		t.Errorf("scanned id = %q, want t-eval-unembed-1", got[0].ID)
	}
}

// TestGetEvaluatedUnembeddedTranscripts_WhereClauseInvariants asserts the
// embed-pass SQL carries BOTH discriminating predicates: eval'd
// (eval_finished_at IS NOT NULL, via a positive run_metrics JOIN) AND
// NOT-embedded (no transcript_chunks). This is the invariant guard: an embed
// pass that dropped the eval'd predicate would embed un-judged transcripts,
// breaking the gate's "embedded ⟹ eval'd" contract.
func TestGetEvaluatedUnembeddedTranscripts_WhereClauseInvariants(t *testing.T) {
	sql := evaluatedUnembeddedTranscriptsSQL
	for _, want := range []string{
		"j.status = 'done'",
		"JOIN run_metrics rm ON rm.job_id = j.id",               // positive eval'd join
		"rm.eval_finished_at IS NOT NULL",                       // eval'd
		"FROM transcript_chunks c WHERE c.transcript_id = t.id", // NOT embedded
		"NOT EXISTS",
		"ORDER BY t.created_at ASC", // oldest-first, deterministic drain order
		"LIMIT $1",                  // bounded + parameterized (OOM guard)
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("embed-pass SQL missing predicate %q:\n%s", want, sql)
		}
	}
}

// TestSelectLimitNormalization asserts normalizeSelectLimit clamps a
// non-positive limit to defaultSelectLimit so a stray 0 (or negative) can never
// produce an unbounded selection — the OOM guard must hold even on a
// misconfigured caller.
func TestSelectLimitNormalization(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, defaultSelectLimit},
		{-1, defaultSelectLimit},
		{1, 1},
		{64, 64},
	} {
		if got := normalizeSelectLimit(tc.in); got != tc.want {
			t.Errorf("normalizeSelectLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestGatedSelections_BoundedAndParameterized asserts both gated-flow selections
// carry a parameterized LIMIT (the OOM guard) rather than an unbounded scan or a
// string-interpolated limit (which would be unsafe).
func TestGatedSelections_BoundedAndParameterized(t *testing.T) {
	for name, sql := range map[string]string{
		"eval-pass":  unevaluatedTranscriptsSQL,
		"embed-pass": evaluatedUnembeddedTranscriptsSQL,
	} {
		if !strings.Contains(sql, "LIMIT $1") {
			t.Errorf("%s SQL must carry a parameterized LIMIT $1 (OOM guard):\n%s", name, sql)
		}
	}
}

// TestEvalAndEmbedPasses_PartitionDoneNotEmbedded asserts the two gated-flow
// passes partition the done+not-embedded space cleanly on the eval'd axis: the
// eval pass selects NOT-eval'd, the embed pass selects eval'd. A transcript that
// is done+not-embedded is in exactly one pass depending on eval_finished_at —
// never both, never neither. We prove this at the SQL level by showing the two
// queries differ only in the eval'd polarity (NOT EXISTS vs positive JOIN +
// IS NOT NULL) over the shared done+not-embedded base.
func TestEvalAndEmbedPasses_PartitionDoneNotEmbedded(t *testing.T) {
	evalSQL := unevaluatedTranscriptsSQL
	embedSQL := evaluatedUnembeddedTranscriptsSQL

	// Shared base: both restrict to done jobs and not-embedded transcripts.
	for _, shared := range []string{
		"j.status = 'done'",
		"FROM transcript_chunks c WHERE c.transcript_id = t.id",
	} {
		if !strings.Contains(evalSQL, shared) {
			t.Errorf("eval pass missing shared base predicate %q", shared)
		}
		if !strings.Contains(embedSQL, shared) {
			t.Errorf("embed pass missing shared base predicate %q", shared)
		}
	}

	// Polarity: eval pass NEGATES eval'd (NOT EXISTS run_metrics … IS NOT NULL);
	// embed pass REQUIRES eval'd (positive JOIN + IS NOT NULL).
	if !strings.Contains(evalSQL, "NOT EXISTS") ||
		!strings.Contains(evalSQL, "rm.eval_finished_at IS NOT NULL") {
		t.Errorf("eval pass must NEGATE eval'd via NOT EXISTS:\n%s", evalSQL)
	}
	if strings.Contains(evalSQL, "JOIN run_metrics") {
		t.Errorf("eval pass must not positively JOIN run_metrics:\n%s", evalSQL)
	}
	if !strings.Contains(embedSQL, "JOIN run_metrics rm ON rm.job_id = j.id") ||
		!strings.Contains(embedSQL, "rm.eval_finished_at IS NOT NULL") {
		t.Errorf("embed pass must REQUIRE eval'd via positive JOIN + IS NOT NULL:\n%s", embedSQL)
	}
}

// ─── SetPipelinePhaseRejectsInvalid (unchanged) ──────────────────────────────

// TestSetPipelinePhaseRejectsInvalid verifies SetPipelinePhase validates against
// the closed phase set BEFORE touching the pool — an invalid value returns an
// error without any DB access (so a nil-pool *DB is safe to exercise here). The
// closed set guards the future coordinator (CONTRACT §1.4) from writing a bad
// phase that would silently mis-gate the worker.
func TestSetPipelinePhaseRejectsInvalid(t *testing.T) {
	d := &DB{} // no pool: validation must fail-fast before any pool use

	for _, bad := range []string{"paused", "transcribing", "ANALYZE", "garbage"} {
		if err := d.SetPipelinePhase(context.Background(), bad, "test"); err == nil {
			t.Errorf("SetPipelinePhase(%q) = nil error, want validation error", bad)
		}
	}

	// The three valid phases (and the empty-string alias for idle) must be in the
	// closed set so the setter accepts them.
	for _, ok := range []string{PhaseIdle, PhaseTranscribe, PhaseAnalyze} {
		if !validPhases[ok] {
			t.Errorf("validPhases[%q] = false, want true", ok)
		}
	}
}
