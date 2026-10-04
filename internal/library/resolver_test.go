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
	cases := []struct {
		name, in, want string
	}{
		{"audible asin", "Project Hail Mary [B08GB58KD5]", "B08GB58KD5"},
		{"audible asin 2", "Neuromancer [B0057HR4E6]", "B0057HR4E6"},
		{"numeric catalogue id", "Noise [1984832069]", "1984832069"},
		{"case-insensitive asin", "[b08gb58kd5]", "B08GB58KD5"},
		{"inside a path", "/books/audio/A/Title [B0011UGNDG]/x", "B0011UGNDG"},
		// ISBN-10 with the X check digit — the two live books whose bracket
		// leaked into the title before X was accepted.
		{"isbn-10 X check digit (live 1)", "Some Title [059341635X]", "059341635X"},
		{"isbn-10 X check digit (live 2)", "Another Title [197733587X]", "197733587X"},
		{"isbn-10 lowercase x", "Title [059341635x]", "059341635X"},
		{"isbn-10 X in a dir path", "/books/audio-custom/Author/Title [197733587X]/01.m4b", "197733587X"},
		// Negatives: nothing that merely looks id-ish may match.
		{"bare title", "1984", ""},
		{"plain title", "Plain Title", ""},
		{"10-letter word", "Book [Remastered]", ""},
		{"10-letter word 2", "Book [Unabridged]", ""},
		{"X not last", "Book [05934163X5]", ""},
		{"X with only 8 digits", "Book [05934163X]", ""},
		{"X with 10 digits", "Book [0593416350X]", ""},
		{"double X", "Book [05934163XX]", ""},
		{"leading X", "Book [X059341635]", ""},
		{"unbracketed isbn-10", "Book 059341635X", ""},
		{"short numeric", "Book [12345]", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractASIN(tc.in); got != tc.want {
				t.Errorf("ExtractASIN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripASIN(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"audible asin", "Project Hail Mary [B08GB58KD5]", "Project Hail Mary"},
		{"numeric id", "Noise [1984832069]", "Noise"},
		{"isbn-10 X (live 1)", "Some Title [059341635X]", "Some Title"},
		{"isbn-10 X (live 2)", "Another Title [197733587X]", "Another Title"},
		{"isbn-10 lowercase x", "Some Title [059341635x]", "Some Title"},
		{"no bracket", "1984", "1984"},
		{"plain title", "Plain Title", "Plain Title"},
		{"non-id bracket kept", "Book [Remastered]", "Book [Remastered]"},
		{"malformed X kept", "Book [05934163X5]", "Book [05934163X5]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripASIN(tc.in); got != tc.want {
				t.Errorf("StripASIN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResolveISBNXTitleStripsClean is the end-to-end form of the live bug: a
// book directory carrying an ISBN-10-with-X id resolves to a title from which
// StripASIN removes the whole bracket (it previously survived as "[…X]"), and
// ExtractASIN yields the id the ABS lookup keys on.
func TestResolveISBNXTitleStripsClean(t *testing.T) {
	r := NewResolver("/books", []Collection{{Root: "audio-libation", Layout: "author/title"}})
	for _, tc := range []struct{ dir, wantASIN, wantTitle string }{
		{"/books/audio-libation/Some Author/Some Title [059341635X]", "059341635X", "Some Title"},
		{"/books/audio-libation/Other Author/Another Title [197733587X]", "197733587X", "Another Title"},
	} {
		_, title := r.Resolve(tc.dir, tc.dir+"/01.m4b")
		if got := ExtractASIN(title); got != tc.wantASIN {
			t.Errorf("ExtractASIN(%q) = %q, want %q", title, got, tc.wantASIN)
		}
		if got := StripASIN(title); got != tc.wantTitle {
			t.Errorf("StripASIN(%q) = %q, want clean %q", title, got, tc.wantTitle)
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
