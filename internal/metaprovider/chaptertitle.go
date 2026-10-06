package metaprovider

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/jedwards1230/earmark/internal/library"
)

// CleanChapterTitles strips filename debris from a provider chapter list and
// returns a cleaned copy (the input slice is never mutated).
//
// Audiobookshelf builds chapter titles from the track/file names when a book
// has no embedded chapter metadata, and the libation layout makes those names
// carry the book title, a track number, and sometimes the catalogue id. Two
// shapes were measured on the live library (CONTRACT §1.6):
//
//  1. Numbered + shared book title — "<N> - <Book>: <chapter>", where N is the
//     1-based position in the list and <Book> is the same leading segment on
//     every chapter, e.g. "12 - Project Hail Mary: Chapter 11" → "Chapter 11".
//     Some books repeat the title once more ("1 - Children of Dune: Children of
//     Dune: Chapter 1"); the repeat is stripped too.
//  2. Raw filename — "<Book> [<ASIN>] - <NN> - <chapter>[ (<k>)]", e.g.
//     "Harry Potter and the Order of the Phoenix, Book 5 [B017V4NMX4] - 01 -
//     Chapter 1: Dudley Demented (1)" → "Chapter 1: Dudley Demented". The
//     " (k)" suffix is ABS's de-duplication counter for a file split into
//     several chapters, so both halves end up with the same honest title.
//
// The rules are deliberately conservative, because a wrong strip destroys a
// real title:
//
//   - Form 1 fires only when EVERY chapter carries "<N> - " with N equal to its
//     1-based position, and the book segment is stripped only when every
//     chapter shares it (at least two chapters). A real title that merely
//     starts with digits ("1984: Part One", "Count Zero: 1", "0000") or a list
//     of genuine "Part I: ..." titles is never touched.
//   - Form 2 fires per title and only on a bracketed catalogue id followed by
//     " - <digits> - ", which no real chapter title contains.
//   - A strip that would leave an empty title is not applied.
//
// Nothing is lost: whenever a title changes, the original is kept in
// Chapter.RawTitle (unless RawTitle already holds an earlier original).
//
// Cleaning is NOT idempotent in general: a cleaned title can itself look like
// debris, e.g. ["1 - Book: 1 - Intro", "2 - Book: 2 - Body"] cleans to
// ["1 - Intro", "2 - Body"] and a second pass would strip again. Callers must
// therefore clean a list at most once. The read path (db.decodeChapters) keys
// on RawTitle: a stored list where any entry carries RawTitle was cleaned at
// ingest and is returned as stored. (A list that ingest left unchanged has no
// RawTitle, but a second pass over an unchanged list is a no-op by
// construction: its input is identical to the first pass's.)
func CleanChapterTitles(chapters []Chapter) []Chapter {
	if len(chapters) == 0 {
		return chapters
	}
	out := make([]Chapter, len(chapters))
	copy(out, chapters)

	titles := make([]string, len(out))
	for i, c := range out {
		titles[i] = stripFilenameForm(c.Title)
	}
	titles = stripNumberedForm(titles)

	for i := range out {
		t := strings.TrimSpace(titles[i])
		if t == "" || t == out[i].Title {
			continue
		}
		if out[i].RawTitle == "" {
			out[i].RawTitle = out[i].Title
		}
		out[i].Title = t
	}
	return out
}

// filenameTitle matches form 2: "<Book> [<catalogue id>] - <NN> - <chapter>"
// with an optional trailing " (<k>)" de-duplication counter. The id
// alternation is library.ASINIDPattern so it can never drift from ExtractASIN.
var filenameTitle = regexp.MustCompile(`(?i)^.+?\s*\[(?:` + library.ASINIDPattern + `)\]\s+-\s+\d+\s+-\s+(.+?)(?:\s+\(\d+\))?$`)

// stripFilenameForm reduces a raw-filename chapter title (form 2) to its
// chapter part; any other title is returned unchanged.
func stripFilenameForm(title string) string {
	m := filenameTitle.FindStringSubmatch(title)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return title
	}
	return m[1]
}

// numberedTitle matches form 1's "<N> - " track-number prefix.
var numberedTitle = regexp.MustCompile(`^(\d+) - (.+)$`)

// stripNumberedForm applies form 1 to the whole list. It returns titles
// unchanged unless every one carries a "<N> - " prefix whose N is its own
// 1-based position.
func stripNumberedForm(titles []string) []string {
	rest := make([]string, len(titles))
	for i, t := range titles {
		m := numberedTitle.FindStringSubmatch(t)
		if m == nil {
			return titles
		}
		if n, err := strconv.Atoi(m[1]); err != nil || n != i+1 {
			return titles
		}
		rest[i] = m[2]
	}
	return stripSharedBookSegment(rest)
}

// stripSharedBookSegment removes a leading "<Book>: " segment shared by every
// title. The segment is the text before the FIRST ": " — a known limitation:
// when the book title itself contains ": " (e.g. "1 - Star Wars: Thrawn:
// Chapter 1"), only "Star Wars: " is stripped and "Thrawn: Chapter 1" is left.
// That leaves residue but never damages a real title, and the live library's
// prefixes are always the short title before any colon ("Dune" for "Dune: The
// Butlerian Jihad"), so it does not occur today. A title equal to "<Book>"
// alone is allowed and kept; after the strip, one or more immediate repeats of
// the same segment are stripped too. It needs at least two titles —
// with one there is no consensus to tell a book title from a real "Part I:"
// prefix — and leaves titles unchanged when no common segment exists.
func stripSharedBookSegment(titles []string) []string {
	if len(titles) < 2 {
		return titles
	}
	book := ""
	for _, t := range titles {
		if seg, _, ok := strings.Cut(t, ": "); ok {
			book = seg
			break
		}
	}
	if strings.TrimSpace(book) == "" {
		return titles
	}
	prefix := book + ": "
	for _, t := range titles {
		if t != book && !strings.HasPrefix(t, prefix) {
			return titles
		}
	}
	out := make([]string, len(titles))
	for i, t := range titles {
		for strings.HasPrefix(t, prefix) && strings.TrimSpace(t[len(prefix):]) != "" {
			t = t[len(prefix):]
		}
		out[i] = t
	}
	return out
}
