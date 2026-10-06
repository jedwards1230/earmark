package metaprovider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// fakeEmbedded is an EmbeddedASINSource returning one tag for every file and
// counting how often it is asked.
type fakeEmbedded struct {
	tag   string
	err   error
	calls atomic.Int32
}

func (f *fakeEmbedded) EmbeddedASIN(context.Context, string) (string, error) {
	f.calls.Add(1)
	return f.tag, f.err
}

// absServer serves one library item ("Project Hail Mary", asin B08G9PRS1K) and
// counts list requests.
func absServer(t *testing.T, lists *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/libraries/lib/items":
			lists.Add(1)
			_, _ = w.Write(absItemsFixture("item", "B08G9PRS1K"))
		case "/api/items/item":
			_, _ = w.Write(absItemDetailFixture("item"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestABSProvider_EmbeddedTag_ThirdSource: with no ASIN in the directory or
// filename, the embedded tag is used — and only once its record's title
// matches the book's path title (identity "exact", source "embedded_tag").
func TestABSProvider_EmbeddedTag_ThirdSource(t *testing.T) {
	var lists atomic.Int32
	srv := absServer(t, &lists)
	emb := &fakeEmbedded{tag: "b08g9prs1k"}
	path := metaprovider.NewPathProvider("", "/books")
	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client()).WithEmbeddedASIN(emb, path)

	meta, err := p.Lookup(context.Background(), "/books/Andy Weir/Project Hail Mary/01.m4b", "01.m4b")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.Source != "abs" || meta.ASIN != "B08G9PRS1K" || len(meta.Chapters) != 3 {
		t.Fatalf("meta = %+v, want the ABS record via the embedded tag", meta)
	}
	if meta.ASINSource != "embedded_tag" || meta.IdentityStatus != metaprovider.IdentityExact {
		t.Errorf("identity = (%q, %q), want (embedded_tag, exact)", meta.ASINSource, meta.IdentityStatus)
	}
}

// TestABSProvider_EmbeddedTag_TitleConflict: a tag naming a record whose title
// does not match the book is NOT used: the book keeps its path metadata, no
// ASIN, and is marked conflict.
func TestABSProvider_EmbeddedTag_TitleConflict(t *testing.T) {
	var lists atomic.Int32
	srv := absServer(t, &lists)
	emb := &fakeEmbedded{tag: "B08G9PRS1K"} // Project Hail Mary's ASIN…
	path := metaprovider.NewPathProvider("", "/books")
	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client()).WithEmbeddedASIN(emb, path)

	// …on a different book.
	meta, err := p.Lookup(context.Background(), "/books/Frank Herbert/Children of Dune/01.m4b", "01.m4b")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.IdentityStatus != metaprovider.IdentityConflict {
		t.Fatalf("IdentityStatus = %q, want conflict (meta %+v)", meta.IdentityStatus, meta)
	}
	if meta.ASIN != "" || meta.Description != "" || len(meta.Chapters) != 0 || meta.Source != "path" {
		t.Errorf("conflicting record leaked into the book: %+v", meta)
	}
	if meta.Title != "Children of Dune" || meta.Author != "Frank Herbert" {
		t.Errorf("path identity = %q / %q, want Children of Dune / Frank Herbert", meta.Title, meta.Author)
	}
}

// TestABSProvider_EmbeddedTag_PathASINWins: the tag is never consulted when the
// path has an ASIN (the directory and filename sources come first).
func TestABSProvider_EmbeddedTag_PathASINWins(t *testing.T) {
	var lists atomic.Int32
	srv := absServer(t, &lists)
	emb := &fakeEmbedded{tag: "B0TAGTAG01"}
	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client()).
		WithEmbeddedASIN(emb, metaprovider.NewPathProvider("", "/books"))

	meta, err := p.Lookup(context.Background(), "/books/Andy Weir/Project Hail Mary [B08G9PRS1K]/01.m4b", "01.m4b")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.ASINSource != "dir" || meta.IdentityStatus != metaprovider.IdentityExact {
		t.Errorf("identity = (%q, %q), want (dir, exact)", meta.ASINSource, meta.IdentityStatus)
	}
	if n := emb.calls.Load(); n != 0 {
		t.Errorf("embedded source asked %d times, want 0", n)
	}
}

// TestABSProvider_EmbeddedTag_Unset: without WithEmbeddedASIN (the read
// paths) a path with no ASIN never reaches ABS — today's behavior.
func TestABSProvider_EmbeddedTag_Unset(t *testing.T) {
	var lists atomic.Int32
	srv := absServer(t, &lists)
	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client())
	meta, err := p.Lookup(context.Background(), "/books/Andy Weir/Project Hail Mary/01.m4b", "01.m4b")
	if err != nil || meta.Source != "" {
		t.Errorf("Lookup = %+v, %v; want empty (not found)", meta, err)
	}
	if lists.Load() != 0 {
		t.Error("ABS queried without any ASIN source")
	}
}

// TestABSProvider_EmbeddedTag_SourceError: a failing tag read degrades to
// "no ASIN" rather than failing the lookup.
func TestABSProvider_EmbeddedTag_SourceError(t *testing.T) {
	var lists atomic.Int32
	srv := absServer(t, &lists)
	emb := &fakeEmbedded{err: errors.New("db down")}
	p := metaprovider.NewABSProvider(srv.URL, "tok", "lib", srv.Client()).
		WithEmbeddedASIN(emb, metaprovider.NewPathProvider("", "/books"))
	meta, err := p.Lookup(context.Background(), "/books/A/B/01.m4b", "01.m4b")
	if err != nil || meta.Source != "" {
		t.Errorf("Lookup = %+v, %v; want empty, no error", meta, err)
	}
}

// TestNew_WithEmbeddedASINSource: the factory threads the option into the ABS
// slot of a chain, so the third source works end to end through the chain.
func TestNew_WithEmbeddedASINSource(t *testing.T) {
	var lists atomic.Int32
	srv := absServer(t, &lists)
	cfg := fakeConfig{provider: "chain:abs,path", absURL: srv.URL, absToken: "tok", absLibID: "lib", booksDir: "/books"}
	emb := &fakeEmbedded{tag: "B08G9PRS1K"}
	prov := metaprovider.New(cfg, metaprovider.WithEmbeddedASINSource(emb))
	meta, err := prov.Lookup(context.Background(), "/books/Andy Weir/Project Hail Mary/01.m4b", "01.m4b")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ASINSource != "embedded_tag" || meta.Source != "abs" {
		t.Errorf("meta = %+v, want ABS via embedded tag", meta)
	}
}
