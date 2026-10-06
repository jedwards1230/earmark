package metaprovider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// absItemsFixture returns the JSON body for the library items list, containing
// one item with the given ASIN.
func absItemsFixture(itemID, asin string) []byte {
	body, _ := json.Marshal(map[string]any{
		"results": []map[string]any{
			{
				"id": itemID,
				"media": map[string]any{
					"metadata": map[string]any{
						"title":        "Project Hail Mary",
						"authorName":   "Andy Weir",
						"narratorName": "Ray Porter",
						"seriesName":   "",
						"asin":         asin,
					},
					"chapters": []any{}, // chapters only in detail endpoint
				},
			},
		},
		"total": 1,
		"limit": 500,
		"page":  0,
	})
	return body
}

// absItemDetailFixture returns the JSON body for a full item detail response.
func absItemDetailFixture(itemID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": itemID,
		"media": map[string]any{
			"metadata": map[string]any{
				"title":        "Project Hail Mary",
				"authorName":   "Andy Weir",
				"narratorName": "Ray Porter",
				"seriesName":   "",
				"asin":         "B08G9PRS1K",
			},
			"chapters": []map[string]any{
				{"id": 0, "start": 0.0, "end": 17.18, "title": "Dedication"},
				{"id": 1, "start": 17.18, "end": 2221.01, "title": "Chapter 1"},
				{"id": 2, "start": 2221.01, "end": 3942.49, "title": "Chapter 2"},
			},
		},
	})
	return body
}

// TestABSProvider_Lookup_Hit verifies that a path containing a valid ASIN
// resolves to the correct BookMeta with chapters populated.
func TestABSProvider_Lookup_Hit(t *testing.T) {
	t.Parallel()

	const (
		itemID   = "test-item-id"
		asin     = "B08G9PRS1K"
		libID    = "lib-id"
		token    = "test-token"
		filePath = "/books/audio-libation/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer token is forwarded.
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(absItemsFixture(itemID, asin))
		case "/api/items/" + itemID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(absItemDetailFixture(itemID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, token, libID, srv.Client())
	got, err := p.Lookup(context.Background(), filePath, "01.m4b")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}

	if got.Title != "Project Hail Mary" {
		t.Errorf("Title = %q, want %q", got.Title, "Project Hail Mary")
	}
	if got.Author != "Andy Weir" {
		t.Errorf("Author = %q, want %q", got.Author, "Andy Weir")
	}
	if got.Narrator != "Ray Porter" {
		t.Errorf("Narrator = %q, want %q", got.Narrator, "Ray Porter")
	}
	if got.ASIN != asin {
		t.Errorf("ASIN = %q, want %q", got.ASIN, asin)
	}
	if got.Source != "abs" {
		t.Errorf("Source = %q, want %q", got.Source, "abs")
	}
	if len(got.Chapters) != 3 {
		t.Fatalf("len(Chapters) = %d, want 3", len(got.Chapters))
	}
	if got.Chapters[0].Title != "Dedication" {
		t.Errorf("Chapters[0].Title = %q, want %q", got.Chapters[0].Title, "Dedication")
	}
	if got.Chapters[1].StartSec != 17.18 {
		t.Errorf("Chapters[1].StartSec = %v, want 17.18", got.Chapters[1].StartSec)
	}
	if got.Chapters[2].Index != 2 {
		t.Errorf("Chapters[2].Index = %d, want 2", got.Chapters[2].Index)
	}
}

// TestABSProvider_Lookup_FilenameASIN verifies the author-only layout path:
// the book directory carries no ASIN, but the track filename does, so ABS still
// matches via the filename fallback (audio-custom / audio-libro layouts).
func TestABSProvider_Lookup_FilenameASIN(t *testing.T) {
	t.Parallel()

	const (
		itemID = "test-item-id"
		asin   = "B08G9PRS1K"
		libID  = "lib-id"
		token  = "test-token"
		// No ASIN on the directory; the ASIN lives only on the file itself.
		filePath = "/books/audio-custom/Andy Weir/Project Hail Mary [B08G9PRS1K].m4b"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer token is forwarded.
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(absItemsFixture(itemID, asin))
		case "/api/items/" + itemID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(absItemDetailFixture(itemID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, token, libID, srv.Client())
	got, err := p.Lookup(context.Background(), filePath, filepath.Base(filePath))
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if got.ASIN != asin {
		t.Errorf("ASIN = %q, want %q (filename fallback)", got.ASIN, asin)
	}
	if got.Title != "Project Hail Mary" {
		t.Errorf("Title = %q, want %q", got.Title, "Project Hail Mary")
	}
	if got.Author != "Andy Weir" {
		t.Errorf("Author = %q, want %q", got.Author, "Andy Weir")
	}
	if got.Narrator != "Ray Porter" {
		t.Errorf("Narrator = %q, want %q", got.Narrator, "Ray Porter")
	}
	if got.Source != "abs" {
		t.Errorf("Source = %q, want %q", got.Source, "abs")
	}
	if len(got.Chapters) != 3 {
		t.Fatalf("len(Chapters) = %d, want 3 (chapters should resolve via filename ASIN)", len(got.Chapters))
	}
	if got.Chapters[0].Title != "Dedication" {
		t.Errorf("Chapters[0].Title = %q, want %q", got.Chapters[0].Title, "Dedication")
	}
	if got.Chapters[1].StartSec != 17.18 {
		t.Errorf("Chapters[1].StartSec = %v, want 17.18", got.Chapters[1].StartSec)
	}
	if got.Chapters[2].Index != 2 {
		t.Errorf("Chapters[2].Index = %d, want 2", got.Chapters[2].Index)
	}
}

// TestABSProvider_Lookup_DirectoryASINWinsOnMismatch verifies the priority rule:
// when both the directory and the filename carry an ASIN and they differ, the
// directory ASIN is used (it identifies the book) and the filename ASIN is ignored.
func TestABSProvider_Lookup_DirectoryASINWinsOnMismatch(t *testing.T) {
	t.Parallel()

	const (
		itemID   = "test-item-id"
		dirASIN  = "B000000001" // on the directory — must win (valid 10-char ASIN)
		fileASIN = "B08G9PRS1K" // on the filename — must be ignored
		libID    = "lib-id"
		token    = "test-token"
		filePath = "/books/audio-libation/Author/Title [" + dirASIN + "]/track [" + fileASIN + "].m4b"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(absItemsFixture(itemID, dirASIN)) // library only has the dir ASIN
		case "/api/items/" + itemID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(absItemDetailFixture(itemID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, token, libID, srv.Client())
	got, err := p.Lookup(context.Background(), filePath, filepath.Base(filePath))
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if got.ASIN != dirASIN {
		t.Errorf("ASIN = %q, want %q (directory ASIN must win on mismatch)", got.ASIN, dirASIN)
	}
}

// TestABSProvider_Lookup_NoASIN verifies that a path without a bracketed ASIN
// returns empty BookMeta with no error (not-found signal).
func TestABSProvider_Lookup_NoASIN(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("ABS should not be called when there is no ASIN in the path")
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client())
	got, err := p.Lookup(context.Background(), "/books/author/My Book/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got.Title != "" || got.Author != "" || got.ASIN != "" || len(got.Chapters) != 0 {
		t.Errorf("expected empty BookMeta, got %+v", got)
	}
}

// TestABSProvider_Lookup_ASINNotInLibrary verifies that an ASIN present in the
// path but absent from ABS returns empty BookMeta (not-found, no error).
func TestABSProvider_Lookup_ASINNotInLibrary(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/libraries/lib/items" {
			http.NotFound(w, r)
			return
		}
		// Return a list with a different ASIN.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(absItemsFixture("other-id", "B000000000"))
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client())
	got, err := p.Lookup(context.Background(),
		"/books/audio-libation/Author/Book [B08G9PRS1K]/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("expected nil error for not-found, got %v", err)
	}
	if got.Title != "" || len(got.Chapters) != 0 {
		t.Errorf("expected empty BookMeta, got %+v", got)
	}
}

// TestABSProvider_Lookup_TransportError verifies that an HTTP error (connection
// refused) is returned as an error (so ChainProvider can log + fall back).
func TestABSProvider_Lookup_TransportError(t *testing.T) {
	t.Parallel()

	// Use a server that immediately closes the connection to trigger a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client())
	_, err := p.Lookup(context.Background(),
		"/books/audio-libation/Author/Book [B08G9PRS1K]/01.mp3", "01.mp3")
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

// TestABSProviderImplementsInterface is a compile-time assertion.
func TestABSProviderImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ metaprovider.MetadataProvider = metaprovider.NewABSProvider("http://x", "tok", "lib", nil)
}

// TestABSProvider_Lookup_ISBNXCheckDigit is the provider-level form of the live
// bug: a book directory keyed by an ISBN-10 ending in the X check digit
// ("[059341635X]") previously yielded no ASIN, so the ABS lookup was skipped and
// the path title — bracket and all — was used. It must now resolve via ABS and
// return the clean catalogue title.
func TestABSProvider_Lookup_ISBNXCheckDigit(t *testing.T) {
	t.Parallel()

	const (
		itemID = "isbn-x-item"
		asin   = "059341635X"
		libID  = "lib-id"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			_, _ = w.Write(absItemsFixture(itemID, asin))
		case "/api/items/" + itemID:
			_, _ = w.Write(absItemDetailFixture(itemID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, "tok", libID, srv.Client())
	got, err := p.Lookup(context.Background(),
		"/books/audio-libation/Andy Weir/Project Hail Mary [059341635X]/01.m4b", "01.m4b")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if got.ASIN != asin {
		t.Errorf("ASIN = %q, want %q (X check digit must be extracted)", got.ASIN, asin)
	}
	if got.Title != "Project Hail Mary" {
		t.Errorf("Title = %q, want the clean ABS title", got.Title)
	}
}

// absEnrichedItemsJSON is a verbatim-shaped ABS /api/libraries/{id}/items body
// (minified list shape) carrying the catalogue-enrichment fields earmark now
// stores: description, genres (string array) and isbn.
const absEnrichedItemsJSON = `{
  "results": [
    {
      "id": "li_enriched",
      "media": {
        "metadata": {
          "title": "Project Hail Mary",
          "authorName": "Andy Weir",
          "narratorName": "Ray Porter",
          "seriesName": "",
          "asin": "B08G9PRS1K",
          "isbn": " 9780593135204 ",
          "description": "  <p>Ryland Grace is the sole survivor on a desperate mission.</p> ",
          "genres": ["Science Fiction & Fantasy", " ", "Science Fiction"],
          "publishedYear": "2021"
        }
      }
    }
  ],
  "total": 1, "limit": 500, "page": 0
}`

// TestABSProvider_Lookup_DecodesEnrichment asserts description, genres and isbn
// are decoded from the ABS item metadata and normalized (trimmed; blank genres
// dropped).
func TestABSProvider_Lookup_DecodesEnrichment(t *testing.T) {
	t.Parallel()
	const libID = "lib-id"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			_, _ = w.Write([]byte(absEnrichedItemsJSON))
		case "/api/items/li_enriched":
			_, _ = w.Write(absItemDetailFixture("li_enriched"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, "tok", libID, srv.Client())
	got, err := p.Lookup(context.Background(),
		"/books/audio-libation/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b", "01.m4b")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if want := "<p>Ryland Grace is the sole survivor on a desperate mission.</p>"; got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}
	if got.ISBN != "9780593135204" {
		t.Errorf("ISBN = %q, want trimmed 9780593135204", got.ISBN)
	}
	wantGenres := []string{"Science Fiction & Fantasy", "Science Fiction"}
	if len(got.Genres) != len(wantGenres) {
		t.Fatalf("Genres = %q, want %q", got.Genres, wantGenres)
	}
	for i := range wantGenres {
		if got.Genres[i] != wantGenres[i] {
			t.Errorf("Genres[%d] = %q, want %q", i, got.Genres[i], wantGenres[i])
		}
	}
}

// TestABSProvider_Lookup_MissingEnrichmentIsZero asserts an item without the
// enrichment fields yields zero values (nil genres) so the upsert stores NULL
// and keeps any previously stored value.
func TestABSProvider_Lookup_MissingEnrichmentIsZero(t *testing.T) {
	t.Parallel()
	const (
		libID  = "lib-id"
		itemID = "plain-item"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			_, _ = w.Write(absItemsFixture(itemID, "B08G9PRS1K"))
		case "/api/items/" + itemID:
			_, _ = w.Write(absItemDetailFixture(itemID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := metaprovider.NewABSProvider(srv.URL, "tok", libID, srv.Client())
	got, err := p.Lookup(context.Background(),
		"/books/audio-libation/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b", "01.m4b")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if got.Description != "" || got.ISBN != "" || got.Genres != nil {
		t.Errorf("enrichment = (%q, %q, %v), want all zero", got.Description, got.ISBN, got.Genres)
	}
}

// TestABSProvider_Lookup_CleansChapterTitles proves ingest-time cleaning: ABS
// chapter titles carrying filename debris (the live "<N> - <Book>: <chapter>"
// shape) come back from Lookup clean, with the provider original preserved in
// RawTitle so the stored JSONB loses nothing.
func TestABSProvider_Lookup_CleansChapterTitles(t *testing.T) {
	t.Parallel()

	const (
		itemID   = "debris-item"
		asin     = "B08G9PRS1K"
		libID    = "lib-id"
		filePath = "/books/audio-libation/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b"
	)
	detail, _ := json.Marshal(map[string]any{
		"id": itemID,
		"media": map[string]any{
			"metadata": map[string]any{"title": "Project Hail Mary", "asin": asin},
			"chapters": []map[string]any{
				{"id": 0, "start": 0.0, "end": 17.18, "title": "1 - Project Hail Mary: Dedication"},
				{"id": 1, "start": 17.18, "end": 2221.01, "title": "2 - Project Hail Mary: Chapter 1"},
			},
		},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/libraries/" + libID + "/items":
			_, _ = w.Write(absItemsFixture(itemID, asin))
		case "/api/items/" + itemID:
			_, _ = w.Write(detail)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := metaprovider.NewABSProvider(srv.URL, "tok", libID, srv.Client()).
		Lookup(context.Background(), filePath, "01.m4b")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	want := []metaprovider.Chapter{
		{Index: 0, Title: "Dedication", RawTitle: "1 - Project Hail Mary: Dedication", StartSec: 0, EndSec: 17.18},
		{Index: 1, Title: "Chapter 1", RawTitle: "2 - Project Hail Mary: Chapter 1", StartSec: 17.18, EndSec: 2221.01},
	}
	if len(got.Chapters) != len(want) {
		t.Fatalf("len(Chapters) = %d, want %d", len(got.Chapters), len(want))
	}
	for i := range want {
		if got.Chapters[i] != want[i] {
			t.Errorf("Chapters[%d] = %+v, want %+v", i, got.Chapters[i], want[i])
		}
	}
}
