package metaprovider

import (
	"reflect"
	"testing"
)

// chaptersFromTitles builds a chapter list with sequential 0-based Index values
// (the shape ABS media.chapters returns) from bare titles.
func chaptersFromTitles(titles ...string) []Chapter {
	out := make([]Chapter, len(titles))
	for i, t := range titles {
		out[i] = Chapter{Index: i, Title: t, StartSec: float64(i * 100), EndSec: float64((i + 1) * 100)}
	}
	return out
}

func titlesOf(chapters []Chapter) []string {
	out := make([]string, len(chapters))
	for i, c := range chapters {
		out[i] = c.Title
	}
	return out
}

// TestCleanChapterTitles uses chapter titles captured verbatim from the live
// book_metadata.chapters column (2026-10-05). Each case is the head of a real
// book's list; the numbered form needs the list to start at position 1.
func TestCleanChapterTitles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "numbered + shared book title",
			in:   []string{"1 - Project Hail Mary: Dedication", "2 - Project Hail Mary: Chapter 1", "3 - Project Hail Mary: Chapter 2"},
			want: []string{"Dedication", "Chapter 1", "Chapter 2"},
		},
		{
			name: "book title starting with digits keeps its real chapter text",
			in:   []string{"1 - 1984: Part One: Chapter 1", "2 - 1984: Part One: Chapter 2", "3 - 1984: Part One: Chapter 3"},
			want: []string{"Part One: Chapter 1", "Part One: Chapter 2", "Part One: Chapter 3"},
		},
		{
			name: "purely numeric real chapter titles survive",
			in:   []string{"1 - Count Zero: 1", "2 - Count Zero: 2", "3 - Count Zero: 3"},
			want: []string{"1", "2", "3"},
		},
		{
			name: "zero-padded numeric real title survives",
			in:   []string{"1 - Ready Player Two: Cutscene", "2 - Ready Player Two: 0000", "3 - Ready Player Two: Level Four"},
			want: []string{"Cutscene", "0000", "Level Four"},
		},
		{
			name: "repeated book title stripped, bare title chapter kept",
			in: []string{
				"1 - Children of Dune: Children of Dune",
				"2 - Children of Dune: Children of Dune: Chapter 1",
				"3 - Children of Dune: Children of Dune: Chapter 2",
			},
			want: []string{"Children of Dune", "Chapter 1", "Chapter 2"},
		},
		{
			name: "repeated short title before a subtitle",
			in: []string{
				"1 - A World Appears: A World Appears: A Journey into Consciousness: Intro",
				"2 - A World Appears: Dedication",
				"3 - A World Appears: Epigraph",
			},
			want: []string{"A Journey into Consciousness: Intro", "Dedication", "Epigraph"},
		},
		{
			name: "chapter repeating the book title later is untouched",
			in:   []string{"1 - Dune: Book One: Dune", "2 - Dune: Book One: Dune: Chapter 1", "3 - Dune: Book One: Dune: Chapter 2"},
			want: []string{"Book One: Dune", "Book One: Dune: Chapter 1", "Book One: Dune: Chapter 2"},
		},
		{
			name: "stray whitespace after the separator is trimmed",
			in:   []string{"1 - Catch-22: Epigraph", "2 - Catch-22: 1. The Texan", "3 - Catch-22:  Joseph Heller Reads Selections from Catch-22"},
			want: []string{"Epigraph", "1. The Texan", "Joseph Heller Reads Selections from Catch-22"},
		},
		{
			name: "inner dash in a real title is kept",
			in:   []string{"1 - Dungeon Crawler Carl: Chapter 1", "2 - Dungeon Crawler Carl: Part I - Chapter 2", "3 - Dungeon Crawler Carl: Chapter 3"},
			want: []string{"Chapter 1", "Part I - Chapter 2", "Chapter 3"},
		},
		{
			name: "raw filename with catalogue id and split counter",
			in: []string{
				"Harry Potter and the Order of the Phoenix, Book 5 [B017V4NMX4] - 01 - Chapter 1: Dudley Demented (1)",
				"Harry Potter and the Order of the Phoenix, Book 5 [B017V4NMX4] - 01 - Chapter 1: Dudley Demented",
				"Harry Potter and the Order of the Phoenix, Book 5 [B017V4NMX4] - 39 - The Story Continues in Harry Potter and the Half-Blood Prince",
			},
			want: []string{
				"Chapter 1: Dudley Demented",
				"Chapter 1: Dudley Demented",
				"The Story Continues in Harry Potter and the Half-Blood Prince",
			},
		},
		{
			name: "raw filename whose title contains a colon",
			in: []string{
				"The Gutenberg Parenthesis: The Age of Print and Its Lessons for the Age of the Internet [B0CQ5RFM9D] - 01 - Chapter 1 (1)",
				"The Gutenberg Parenthesis: The Age of Print and Its Lessons for the Age of the Internet [B0CQ5RFM9D] - 01 - Chapter 1",
				"The Gutenberg Parenthesis: The Age of Print and Its Lessons for the Age of the Internet [B0CQ5RFM9D] - 12 - Chapter 12",
			},
			want: []string{"Chapter 1", "Chapter 1", "Chapter 12"},
		},
		{
			name: "raw filename whose chapter is the book title",
			in: []string{
				"SEX: Will technology ruin sex and intimacy — or make it even better? [B09TDNN24X] - 1 - SEX: Will technology ruin sex and intimacy — or make it even better? (1)",
				"SEX: Will technology ruin sex and intimacy — or make it even better? [B09TDNN24X] - 1 - SEX: Will technology ruin sex and intimacy — or make it even better?",
			},
			want: []string{
				"SEX: Will technology ruin sex and intimacy — or make it even better?",
				"SEX: Will technology ruin sex and intimacy — or make it even better?",
			},
		},
		{
			name: "clean list sharing a real 'Part I:' segment is untouched",
			in: []string{
				"Part I: Intuitions Come First, Strategic Reasoning Second",
				"Part I: Intuitions Come First, Strategic Reasoning Second: 1. Where Does Morality Come From?",
				"Part I: Intuitions Come First, Strategic Reasoning Second: 2. The Intuitive Dog and Its Rational Tail",
			},
			want: []string{
				"Part I: Intuitions Come First, Strategic Reasoning Second",
				"Part I: Intuitions Come First, Strategic Reasoning Second: 1. Where Does Morality Come From?",
				"Part I: Intuitions Come First, Strategic Reasoning Second: 2. The Intuitive Dog and Its Rational Tail",
			},
		},
		{
			name: "clean roman-numeral list untouched",
			in:   []string{"One: I", "One: II", "One: III"},
			want: []string{"One: I", "One: II", "One: III"},
		},
		{
			name: "single clean chapter untouched",
			in:   []string{"Chapter 1"},
			want: []string{"Chapter 1"},
		},
		{
			name: "numbering that does not follow position is not debris",
			in:   []string{"1 - Dune: Chapter 1", "3 - Dune: Chapter 2"},
			want: []string{"1 - Dune: Chapter 1", "3 - Dune: Chapter 2"},
		},
		{
			name: "one unnumbered title disables the numbered form",
			in:   []string{"1 - Dune: Chapter 1", "Dune: Chapter 2"},
			want: []string{"1 - Dune: Chapter 1", "Dune: Chapter 2"},
		},
		{
			name: "numbered without a shared segment strips only the number",
			in:   []string{"1 - Prologue", "2 - Part One: Arrival", "3 - Part Two: Departure"},
			want: []string{"Prologue", "Part One: Arrival", "Part Two: Departure"},
		},
		{
			name: "single numbered chapter keeps its leading segment",
			in:   []string{"1 - Part One: Intro"},
			want: []string{"Part One: Intro"},
		},
		{
			name: "bracketed non-id text is not a filename",
			in:   []string{"Prelude [Remastered] - 01 - Overture"},
			want: []string{"Prelude [Remastered] - 01 - Overture"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CleanChapterTitles(chaptersFromTitles(tc.in...))
			if g := titlesOf(got); !reflect.DeepEqual(g, tc.want) {
				t.Fatalf("titles =\n  %q\nwant\n  %q", g, tc.want)
			}
		})
	}
}

// TestCleanChapterTitlesKeepsRawTitle proves no information is lost: a changed
// title keeps its original in RawTitle, an unchanged one leaves RawTitle empty,
// and times/indexes are untouched.
func TestCleanChapterTitlesKeepsRawTitle(t *testing.T) {
	t.Parallel()

	in := []Chapter{
		{Index: 0, Title: "1 - Children of Dune: Children of Dune", StartSec: 0, EndSec: 10},
		{Index: 1, Title: "2 - Children of Dune: Children of Dune: Chapter 1", StartSec: 10, EndSec: 20},
	}
	orig := append([]Chapter(nil), in...)

	got := CleanChapterTitles(in)

	want := []Chapter{
		{Index: 0, Title: "Children of Dune", RawTitle: "1 - Children of Dune: Children of Dune", StartSec: 0, EndSec: 10},
		{Index: 1, Title: "Chapter 1", RawTitle: "2 - Children of Dune: Children of Dune: Chapter 1", StartSec: 10, EndSec: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("input mutated: %+v", in)
	}

	clean := CleanChapterTitles(chaptersFromTitles("Prologue", "Chapter 1"))
	for _, c := range clean {
		if c.RawTitle != "" {
			t.Errorf("unchanged title %q got RawTitle %q", c.Title, c.RawTitle)
		}
	}
}

// TestCleanChapterTitlesSecondPassOnLiveShapes: for every live debris shape a
// second pass over cleaned output changes nothing (RawTitle included).
func TestCleanChapterTitlesSecondPassOnLiveShapes(t *testing.T) {
	t.Parallel()

	lists := [][]string{
		{"1 - Count Zero: 1", "2 - Count Zero: 2"},
		{"1 - Children of Dune: Children of Dune", "2 - Children of Dune: Children of Dune: Chapter 1"},
		{"Harry Potter and the Chamber of Secrets, Book 2 [B017V4IWVG] - 01 - Opening Credits (1)"},
		{"Part I: Intuitions Come First", "Part I: Intuitions Come First: 1. Morality"},
	}
	for _, l := range lists {
		once := CleanChapterTitles(chaptersFromTitles(l...))
		twice := CleanChapterTitles(once)
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("second pass changed %q:\n once  %+v\n twice %+v", l, once, twice)
		}
	}
}

// TestCleanChapterTitlesNotIdempotentInGeneral pins the counterexample that
// makes "clean at most once" (db.decodeChapters' RawTitle guard) necessary: a
// cleaned list can itself look like numbered debris.
func TestCleanChapterTitlesNotIdempotentInGeneral(t *testing.T) {
	t.Parallel()

	once := CleanChapterTitles(chaptersFromTitles("1 - Book: 1 - Intro", "2 - Book: 2 - Body"))
	if g, w := titlesOf(once), []string{"1 - Intro", "2 - Body"}; !reflect.DeepEqual(g, w) {
		t.Fatalf("first pass = %q, want %q", g, w)
	}
	twice := CleanChapterTitles(once)
	if g, w := titlesOf(twice), []string{"Intro", "Body"}; !reflect.DeepEqual(g, w) {
		t.Fatalf("second pass = %q, want %q (the over-strip the read-path guard prevents)", g, w)
	}
	if twice[0].RawTitle != "1 - Book: 1 - Intro" {
		t.Errorf("RawTitle overwritten on second pass: %q", twice[0].RawTitle)
	}
}

func TestCleanChapterTitlesEmpty(t *testing.T) {
	t.Parallel()
	if got := CleanChapterTitles(nil); got != nil {
		t.Fatalf("CleanChapterTitles(nil) = %v, want nil", got)
	}
}
