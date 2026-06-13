// Package metaprovider defines the MetadataProvider seam used by the Go
// service to derive book metadata (author, title, chapters, etc.) from a
// file path. Today the only implementation is PathProvider, which wraps the
// existing library.Resolver to preserve byte-identical output from path
// parsing. Future PRs will add ABSProvider (Audiobookshelf REST API) and
// ChainProvider (try providers in order, merge results).
//
// # Not-found semantics
//
// A provider must return an empty BookMeta (not an error) when the book is
// simply not in its catalog. Reserve errors for real transport/IO failures so
// callers can log-and-fall-back safely.
package metaprovider

import (
	"context"
	"path/filepath"

	"github.com/jedwards1230/earmark/internal/library"
)

// Chapter holds timestamp-bounded chapter information in seconds (matching
// the transcript_chunks schema). Index is the ordinal within the book; Title,
// StartSec, and EndSec are populated only when a provider has chapter data.
type Chapter struct {
	Index    int
	Title    string
	StartSec float64
	EndSec   float64
}

// BookMeta is the normalised metadata record returned by every provider.
// Fields are left zero when a provider cannot determine their value (e.g.
// PathProvider never populates Narrator, Series, ASIN, or Chapters).
//
// Source records the provenance, e.g. "path" or "abs", so callers can log
// which provider populated a result and later-PR ChainProvider can merge.
type BookMeta struct {
	Title    string
	Author   string
	Narrator string
	Series   string
	ASIN     string
	Chapters []Chapter
	Source   string
}

// MetadataProvider looks up metadata for one book given its canonical file
// path and one representative sample filename from that directory.
//
// Contract:
//   - Return an empty BookMeta (all zero values) when the book is not in this
//     provider's catalog — NOT an error. Callers treat an empty result as "not
//     found" and may fall back to the next provider in a chain.
//   - Return a non-nil error only for genuine transport failures (network,
//     database, I/O) so callers know to log and retry rather than silently
//     skip.
type MetadataProvider interface {
	Lookup(ctx context.Context, filePath, sampleName string) (BookMeta, error)
}

// PathProvider implements MetadataProvider by wrapping the existing
// library.Resolver. It derives (author, title) from the directory structure
// of filePath according to the configured LIBRARY_COLLECTIONS layout, with a
// generic fallback for unmatched roots — exactly the behaviour the resolver
// has always provided.
//
// Lookup never returns an error (path parsing is purely local) and never
// populates Chapters, Narrator, Series, or ASIN; those fields are reserved
// for future providers (ABS, Audible).
type PathProvider struct {
	resolver *library.Resolver
}

// NewPathProvider constructs a PathProvider from the LIBRARY_COLLECTIONS JSON
// string and the booksDir root. Mirrors the construction pattern previously
// used at each call site (library.ParseCollections + fallback to
// library.NewResolver on error).
//
// A parse error in collections falls back to the generic resolver — labels are
// cosmetic and must never block startup.
func NewPathProvider(libraryCollections, booksDir string) *PathProvider {
	resolver, err := library.ParseCollections(libraryCollections, booksDir)
	if err != nil {
		resolver = library.NewResolver(booksDir, nil)
	}
	return &PathProvider{resolver: resolver}
}

// Lookup derives author and title from the book directory by calling
// library.Resolver.Resolve. The directory is filepath.Dir(filePath) so
// callers pass any track path from the book and get consistent labels.
//
// Source is always "path". An empty BookMeta is never returned (the resolver
// always produces some label via its generic fallback), so the "not-found"
// contract only applies to providers that query external catalogs.
func (p *PathProvider) Lookup(_ context.Context, filePath, sampleName string) (BookMeta, error) {
	bookDir := filepath.Dir(filePath)
	author, title := p.resolver.Resolve(bookDir, sampleName)
	return BookMeta{
		Title:  title,
		Author: author,
		Source: "path",
	}, nil
}
