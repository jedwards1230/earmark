package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// openRowsGuard is a rowScanner that fails the test when a query is issued
// while a result set it handed out is still open. Against a pool, an open
// pgx.Rows pins one connection and every Query/QueryRow acquires another, so a
// query under an open result set holds two connections at once — the shape
// that deadlocked earmark-mcp's pool on 2026-11-01 once more searches ran at
// once than the pool had connections.
//
// A result set counts as released the way pgx releases its connection: on
// Close, or when Next reports no more rows.
type openRowsGuard struct {
	t    *testing.T
	q    rowScanner
	open int
}

func (g *openRowsGuard) check(what, sql string) {
	g.t.Helper()
	if g.open > 0 {
		g.t.Errorf("%s issued while %d result set(s) still hold a connection: %.60q", what, g.open, sql)
	}
}

func (g *openRowsGuard) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	g.check("Query", sql)
	rows, err := g.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	g.open++
	return &guardedRows{Rows: rows, g: g}, nil
}

func (g *openRowsGuard) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	g.check("QueryRow", sql)
	return g.q.QueryRow(ctx, sql, args...)
}

// guardedRows tracks when its result set gives its connection back.
type guardedRows struct {
	pgx.Rows
	g        *openRowsGuard
	released bool
}

func (r *guardedRows) release() {
	if !r.released {
		r.released = true
		r.g.open--
	}
}

func (r *guardedRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.release()
	return false
}

func (r *guardedRows) Close() {
	r.Rows.Close()
	r.release()
}

// guardedMeta is a MetadataProvider that fails the test when it is consulted
// while a result set is open: a provider may do I/O (the ABS provider is an
// HTTP call), and doing it under an open result set keeps a pool connection
// pinned for the length of that call.
type guardedMeta struct {
	g    *openRowsGuard
	next metaprovider.MetadataProvider
}

func (m guardedMeta) Lookup(ctx context.Context, filePath, sampleName string) (metaprovider.BookMeta, error) {
	m.g.check("metadata Lookup", filePath)
	return m.next.Lookup(ctx, filePath, sampleName)
}

// TestScanResultsClosesRowsBeforeEnrichment is the regression test for the
// 2026-11-01 pool deadlock: scanResults must read its result set to the end
// and release it BEFORE any per-book enrichment read (loadBookContext) or
// metadata lookup, on every query path that shares it — and the enrichment
// must still land on the right rows, in the order the query returned them.
func TestScanResultsClosesRowsBeforeEnrichment(t *testing.T) {
	type run func(d *DB, q rowScanner) ([]SearchResultWithMetadata, error)
	ctx := context.Background()
	vec := make([]float32, 768)
	paths := []struct {
		name string
		args []any
		run  run
	}{
		{
			name: "textSearch",
			args: []any{"q", 10},
			run: func(d *DB, q rowScanner) ([]SearchResultWithMetadata, error) {
				return d.textSearch(ctx, q, "q", 10)
			},
		},
		{
			name: "textSearchInBook",
			args: []any{"q", 10, likePrefix(testLibationDir) + "/%"},
			run: func(d *DB, q rowScanner) ([]SearchResultWithMetadata, error) {
				return d.textSearchInBook(ctx, q, testLibationDir, "q", 10)
			},
		},
		{
			name: "searchInBook",
			args: []any{pgvector.NewVector(vec), 10, likePrefix(testLibationDir) + "/%", 0.0},
			run: func(d *DB, q rowScanner) ([]SearchResultWithMetadata, error) {
				return d.searchInBook(ctx, q, vec, testLibationDir, 10, 0)
			},
		},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("new mock pool: %v", err)
			}
			defer mock.Close()

			// Two books, interleaved, so a lazy per-row enrichment would have
			// to query mid-scan for each.
			rows := pgxmock.NewRows(scanResultColumns).
				AddRow("c1", "one two three", testLibationTrk1, 0, 12.5, 20.0, nil, 0.9, 4).
				AddRow("c2", "four", testCustomTrack, 0, 0.0, 5.0, nil, 0.8, 1).
				AddRow("c3", "five six", testLibationTrk2, 1, 12.5, 20.0, nil, 0.7, 4)
			mock.ExpectQuery("SELECT").WithArgs(p.args...).WillReturnRows(rows)
			expectBookContext(mock, testLibationDir, chaptersJSONFixture(t, testBookChapters), [][]any{
				{testLibationTrk1, fp(1000), testTrackCheck1},
				{testLibationTrk2, fp(1000), testTrackCheck2},
			})
			expectBookContext(mock, testCustomDir, nil, [][]any{
				{testCustomTrack, fp(3600), testCustomChecksm},
			})

			d := newTestDB()
			guard := &openRowsGuard{t: t, q: mock}
			d.meta = guardedMeta{g: guard, next: d.meta}

			got, err := p.run(d, guard)
			if err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
			if guard.open != 0 {
				t.Errorf("%d result set(s) left open after the search returned", guard.open)
			}

			// Behaviour identity: same rows, same order, enrichment applied.
			want := []struct {
				id, checksum, title string
				words               int
			}{
				{"c1", testTrackCheck1, "The Righteous Mind", 3},
				{"c2", testCustomChecksm, "The Coddling of the American Mind", 1},
				{"c3", testTrackCheck2, "The Righteous Mind", 2},
			}
			if len(got) != len(want) {
				t.Fatalf("got %d results, want %d", len(got), len(want))
			}
			for i, w := range want {
				r := got[i]
				if r.ID != w.id || r.FileChecksum != w.checksum || r.Title != w.title || r.WordCount != w.words {
					t.Errorf("result %d = {ID:%q FileChecksum:%q Title:%q WordCount:%d}, want %+v",
						i, r.ID, r.FileChecksum, r.Title, r.WordCount, w)
				}
			}
			if got[0].TotalChapters != len(testBookChapters) || got[1].TotalChapters != 0 {
				t.Errorf("TotalChapters = %d, %d; want %d, 0",
					got[0].TotalChapters, got[1].TotalChapters, len(testBookChapters))
			}
			if got[2].ChapterTitle == "" || got[2].ChapterTitle == got[0].ChapterTitle {
				t.Errorf("track 2 chapter = %q (track 1 %q): track offset not applied",
					got[2].ChapterTitle, got[0].ChapterTitle)
			}
		})
	}
}

// TestScanResultRowsClosesOnScanError: a scan failure mid-result-set still
// gives the connection back before scanResults returns its error.
func TestScanResultRowsClosesOnScanError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows(scanResultColumns).
		AddRow("c1", "ok", testLibationTrk1, 0, 0.0, 1.0, nil, 0.9, 1).
		AddRow("c2", "bad", testLibationTrk1, "not-an-int", 0.0, 1.0, nil, 0.9, 1)
	mock.ExpectQuery("SELECT").WithArgs("q", 10).WillReturnRows(rows)

	d := newTestDB()
	guard := &openRowsGuard{t: t, q: mock}
	if _, err := d.textSearch(context.Background(), guard, "q", 10); err == nil {
		t.Fatal("textSearch over a malformed row returned no error")
	}
	if guard.open != 0 {
		t.Errorf("%d result set(s) left open after a scan error", guard.open)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
