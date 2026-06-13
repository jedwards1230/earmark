package metaprovider_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/earmark/internal/library"
	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// TestPathProviderLookup_MatchesResolver verifies that PathProvider.Lookup
// returns the same author/title that library.Resolver.Resolve produces for
// the same inputs. This is the load-bearing correctness test for the seam:
// if it passes, the provider introduces no behaviour change.
func TestPathProviderLookup_MatchesResolver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		collections string // LIBRARY_COLLECTIONS JSON
		booksDir    string
		filePath    string // track path passed to Lookup
		sampleName  string // same value used for both resolver and provider
		wantAuthor  string
		wantTitle   string
	}{
		{
			name:        "libation author/title layout",
			collections: `[{"root":"audio-libation","layout":"author/title"}]`,
			booksDir:    "/books",
			filePath:    "/books/audio-libation/William Gibson/Neuromancer [B0057HR4E6]/01 - Chapter 1.mp3",
			sampleName:  "/books/audio-libation/William Gibson/Neuromancer [B0057HR4E6]/01 - Chapter 1.mp3",
			wantAuthor:  "William Gibson",
			wantTitle:   "Neuromancer [B0057HR4E6]",
		},
		{
			name:        "libro author-only, title from filename",
			collections: `[{"root":"audio-libro","layout":"author"}]`,
			booksDir:    "/books",
			filePath:    "/books/audio-libro/Daniel Kahneman/Thinking Fast and Slow - Track 202.mp3",
			sampleName:  "/books/audio-libro/Daniel Kahneman/Thinking Fast and Slow - Track 202.mp3",
			wantAuthor:  "Daniel Kahneman",
			wantTitle:   "Thinking Fast and Slow",
		},
		{
			name:        "generic fallback two-level",
			collections: "",
			booksDir:    "/books",
			filePath:    "/books/George Orwell/1984/01.mp3",
			sampleName:  "/books/George Orwell/1984/01.mp3",
			wantAuthor:  "George Orwell",
			wantTitle:   "1984",
		},
		{
			name:        "generic fallback one-level title from filename",
			collections: "",
			booksDir:    "/books",
			filePath:    "/books/Solo Author/My Book - Part 3.mp3",
			sampleName:  "/books/Solo Author/My Book - Part 3.mp3",
			wantAuthor:  "Solo Author",
			wantTitle:   "My Book",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build the reference resolver directly — the same construction logic
			// that existed before this PR — so the test is self-verifying: it proves
			// PathProvider.Lookup == library.Resolver.Resolve for every case.
			var ref *library.Resolver
			if tc.collections == "" {
				ref = library.NewResolver(tc.booksDir, nil)
			} else {
				var err error
				ref, err = library.ParseCollections(tc.collections, tc.booksDir)
				if err != nil {
					t.Fatalf("ParseCollections: %v", err)
				}
			}

			// Reference result from the resolver ("before" behaviour).
			// bookDir mirrors the filepath.Dir call inside PathProvider.Lookup.
			bookDir := filepath.Dir(tc.filePath)
			wantAuthor, wantTitle := ref.Resolve(bookDir, tc.sampleName)

			// Sanity-check the test case's expected values against the resolver.
			// If these fail the test case itself has a typo, not the provider.
			if wantAuthor != tc.wantAuthor {
				t.Fatalf("test case author mismatch: resolver=%q want=%q", wantAuthor, tc.wantAuthor)
			}
			if wantTitle != tc.wantTitle {
				t.Fatalf("test case title mismatch: resolver=%q want=%q", wantTitle, tc.wantTitle)
			}

			// Now verify PathProvider.Lookup returns the same values.
			p := metaprovider.NewPathProvider(tc.collections, tc.booksDir)
			got, err := p.Lookup(context.Background(), tc.filePath, tc.sampleName)
			if err != nil {
				t.Fatalf("Lookup returned unexpected error: %v", err)
			}
			if got.Author != wantAuthor {
				t.Errorf("Author = %q, want %q", got.Author, wantAuthor)
			}
			if got.Title != wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, wantTitle)
			}
			if got.Source != "path" {
				t.Errorf("Source = %q, want %q", got.Source, "path")
			}
			// No-chapter contract: PathProvider never populates Chapters.
			if len(got.Chapters) != 0 {
				t.Errorf("Chapters = %v, want empty", got.Chapters)
			}
		})
	}
}

// TestPathProviderLookup_NoError verifies that PathProvider.Lookup always
// returns nil error (path parsing is local; no transport can fail).
func TestPathProviderLookup_NoError(t *testing.T) {
	t.Parallel()

	p := metaprovider.NewPathProvider("", "/books")
	_, err := p.Lookup(context.Background(), "/books/A/B/track.mp3", "/books/A/B/track.mp3")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestPathProviderImplementsInterface is a compile-time assertion that
// *PathProvider satisfies MetadataProvider. If the interface changes
// incompatibly this file will fail to compile.
func TestPathProviderImplementsInterface(t *testing.T) {
	t.Parallel()

	var _ metaprovider.MetadataProvider = metaprovider.NewPathProvider("", "/books")
}
