package metaprovider

import "strings"

// SeriesRef is one series membership of a book: the series name plus this
// book's position in it.
//
// # Why Sequence is a string, not a number
//
// Series positions are NOT integers in practice. Audiobookshelf (and Audible
// before it) routinely reports novellas as fractional entries ("#1.5", "#0.5"),
// and non-numeric positions ("#1-3" for an omnibus, "#I") are legal too. Typing
// this as an int would silently truncate 1.5 → 1, and typing it as a float would
// reject the non-numeric forms outright. A string carries every real value
// without lying about it; consumers that want ordering parse it themselves and
// fall back to a string compare (see the list_books format=series renderer).
type SeriesRef struct {
	Name     string `json:"name"`
	Sequence string `json:"sequence,omitempty"`
}

// ParseSeries parses the raw book_metadata.series column into its parts.
//
// The stored format is a comma-joined list of "Name #Sequence" entries, because
// one book can belong to several series — e.g.
//
//	"Dune #2, The Dune Sequence #13"  →  [{Dune 2} {The Dune Sequence 13}]
//
// A part with no "#" yields a SeriesRef with only Name set (a real case: a
// series membership with no declared position). Whitespace around the name, the
// "#", and the sequence is trimmed, so "Name#2" and "Name  #  2" parse the same.
// Empty or whitespace-only input returns nil, as does input consisting only of
// separators; empty parts (a trailing comma) are skipped.
//
// Known limitation: a series name that itself contains a comma is
// indistinguishable from the separator and will be split into two entries. This
// is accepted rather than over-engineered — Audiobookshelf joins multi-series
// memberships with ", " and that is the only format the column takes in
// practice.
func ParseSeries(raw string) []SeriesRef {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	var refs []SeriesRef
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ref := SeriesRef{Name: part}
		// Split on the LAST '#' so a name containing one keeps it.
		if i := strings.LastIndex(part, "#"); i >= 0 {
			ref.Name = strings.TrimSpace(part[:i])
			ref.Sequence = strings.TrimSpace(part[i+1:])
		}
		if ref.Name == "" {
			continue
		}
		refs = append(refs, ref)
	}
	return refs
}
