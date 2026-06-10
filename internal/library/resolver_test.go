package library

import "testing"

func TestResolveConfiguredLayouts(t *testing.T) {
	cols := []Collection{
		{Root: "audio-libation", Layout: "author/title"},
		{Root: "audio-libro", Layout: "author"},
		{Root: "audio-custom", Layout: "author"},
	}
	r := NewResolver("/books", cols)

	cases := []struct {
		name        string
		dir, sample string
		wantAuthor  string
		wantTitle   string
	}{
		{
			name:       "libation author/title dir",
			dir:        "/books/audio-libation/William Gibson/Neuromancer [B0057HR4E6]",
			sample:     "/books/audio-libation/William Gibson/Neuromancer [B0057HR4E6]/01 - Chapter 1.mp3",
			wantAuthor: "William Gibson",
			wantTitle:  "Neuromancer [B0057HR4E6]",
		},
		{
			name:       "libro author-only, title from filename",
			dir:        "/books/audio-libro/Daniel Kahneman",
			sample:     "/books/audio-libro/Daniel Kahneman/Thinking Fast and Slow - Track 202.mp3",
			wantAuthor: "Daniel Kahneman",
			wantTitle:  "Thinking Fast and Slow",
		},
		{
			name:       "custom author-only single file",
			dir:        "/books/audio-custom/George Orwell",
			sample:     "/books/audio-custom/George Orwell/1984.m4b",
			wantAuthor: "George Orwell",
			wantTitle:  "1984",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ti := r.Resolve(tc.dir, tc.sample)
			if a != tc.wantAuthor {
				t.Errorf("author = %q, want %q", a, tc.wantAuthor)
			}
			if ti != tc.wantTitle {
				t.Errorf("title = %q, want %q", ti, tc.wantTitle)
			}
		})
	}
}

func TestResolveGenericFallback(t *testing.T) {
	r := NewResolver("/books", nil) // no collections configured

	// Two dir levels below BOOKS_DIR → author/title.
	a, ti := r.Resolve("/books/Some Author/Some Book", "/books/Some Author/Some Book/01.mp3")
	if a != "Some Author" || ti != "Some Book" {
		t.Errorf("two-level fallback = (%q,%q), want (Some Author, Some Book)", a, ti)
	}

	// One dir level → author from dir, title from filename.
	a, ti = r.Resolve("/books/Solo Author", "/books/Solo Author/My Book - Part 3.mp3")
	if a != "Solo Author" || ti != "My Book" {
		t.Errorf("one-level fallback = (%q,%q), want (Solo Author, My Book)", a, ti)
	}
}

func TestRelativeRootResolvesAgainstBooksDir(t *testing.T) {
	r := NewResolver("/data/books", []Collection{{Root: "lib", Layout: "author/title"}})
	a, ti := r.Resolve("/data/books/lib/Author X/Book Y", "/data/books/lib/Author X/Book Y/1.mp3")
	if a != "Author X" || ti != "Book Y" {
		t.Errorf("relative-root = (%q,%q), want (Author X, Book Y)", a, ti)
	}
}

func TestTitleFromFilename(t *testing.T) {
	cases := map[string]string{
		"Thinking Fast and Slow - Track 202.mp3": "Thinking Fast and Slow",
		"My Book - Part 3.m4b":                   "My Book",
		"Chapter 01.mp3":                         "Chapter 01", // all-marker name → keep original, don't strip to empty
		"1984.m4b":                               "1984",
		"Dune Disc 2.mp3":                        "Dune",
	}
	for in, want := range cases {
		if got := titleFromFilename(in); got != want {
			t.Errorf("titleFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractASIN(t *testing.T) {
	cases := map[string]string{
		"Project Hail Mary [B08GB58KD5]":      "B08GB58KD5",
		"Neuromancer [B0057HR4E6]":            "B0057HR4E6",
		"Noise [1984832069]":                  "1984832069", // numeric catalogue id
		"[b08gb58kd5]":                        "B08GB58KD5", // case-insensitive
		"1984":                                "",           // a bare title, no bracket
		"Plain Title":                         "",
		"/books/audio/A/Title [B0011UGNDG]/x": "B0011UGNDG",
	}
	for in, want := range cases {
		if got := ExtractASIN(in); got != want {
			t.Errorf("ExtractASIN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripASIN(t *testing.T) {
	cases := map[string]string{
		"Project Hail Mary [B08GB58KD5]": "Project Hail Mary",
		"Noise [1984832069]":             "Noise",
		"1984":                           "1984", // no bracket → unchanged
		"Plain Title":                    "Plain Title",
	}
	for in, want := range cases {
		if got := StripASIN(in); got != want {
			t.Errorf("StripASIN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCollectionsEmpty(t *testing.T) {
	r, err := ParseCollections("", "/books")
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	// Falls back generically.
	a, ti := r.Resolve("/books/A/B", "/books/A/B/1.mp3")
	if a != "A" || ti != "B" {
		t.Errorf("empty-config fallback = (%q,%q), want (A,B)", a, ti)
	}
}

func TestParseCollectionsInvalidJSON(t *testing.T) {
	if _, err := ParseCollections("{not json", "/books"); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}
