package library

import "testing"

// TestResolveASIN pins the source precedence: directory, then filename, then
// the embedded tag — the tag is consulted only when the path has no ASIN.
func TestResolveASIN(t *testing.T) {
	tests := []struct {
		name, file, tag string
		wantASIN        string
		wantSource      string
	}{
		{"directory wins over filename and tag",
			"/b/Weir/Project Hail Mary [B08G9PRS1K]/Hail [B000000001].m4b", "B0TAGTAG01", "B08G9PRS1K", ASINSourceDir},
		{"filename wins over tag",
			"/b/Schumacher/The Science of Information [1629976067].m4b", "B0TAGTAG01", "1629976067", ASINSourceFilename},
		{"tag is the third source",
			"/b/Herbert/Children of Dune/01.m4b", "B002V57VRC", "B002V57VRC", ASINSourceEmbeddedTag},
		{"tag normalized (case, whitespace)",
			"/b/Herbert/Children of Dune/01.m4b", "  b002v57vrc\n", "B002V57VRC", ASINSourceEmbeddedTag},
		{"ISBN-10 X tag accepted",
			"/b/A/B/01.m4b", "059341635x", "059341635X", ASINSourceEmbeddedTag},
		{"junk tag rejected", "/b/A/B/01.m4b", "N/A", "", ""},
		{"free-text tag rejected", "/b/A/B/01.m4b", "Children of Dune", "", ""},
		{"nothing anywhere", "/b/A/B/01.m4b", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asin, src := ResolveASIN(tc.file, tc.tag)
			if asin != tc.wantASIN || src != tc.wantSource {
				t.Errorf("ResolveASIN(%q, %q) = (%q, %q), want (%q, %q)",
					tc.file, tc.tag, asin, src, tc.wantASIN, tc.wantSource)
			}
		})
	}
}

// TestTitlesMatch pins the cross-check that guards the embedded tag: two-way
// coverage over each title's forms (series groups removed, either side of a
// colon). Series siblings and other products must NOT match.
func TestTitlesMatch(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		// same book
		{"Children of Dune", "Children of Dune", true},
		{"Children of Dune: Dune Chronicles, Book 3", "Children of Dune", true},
		{"Leviathan Wakes", "Leviathan Wakes (The Expanse, Book 1)", true},
		{"Leviathan Wakes [The Expanse #1]", "Leviathan Wakes", true},
		{"Project Hail Mary", "project hail mary [B08G9PRS1K]", true},
		{"Muad'Dib's Legacy", "Muad’Dib’s Legacy", true},
		{"Golden Son", "Red Rising: Golden Son", true}, // series prefix before the colon
		{"The Way of Kings (Unabridged)", "The Way of Kings", true},
		// series siblings and other products (review B3)
		{"Dune", "Dune Messiah", false},
		{"Dune", "Children of Dune", false},
		{"Dune Messiah", "Children of Dune", false},
		{"Project Hail Mary", "Hail Mary", false},
		{"Red Rising", "Red Rising: Golden Son", false},
		{"Fourth Wing", "Fourth Wing (1 of 2) [Dramatized Adaptation]", false},
		{"Leviathan Wakes", "Caliban's War (The Expanse, Book 2)", false},
		// unrelated / degenerate
		{"Eragon", "Children of Dune", false},
		{"The Way of Kings", "The Name of the Wind", false},
		{"", "Children of Dune", false},
		{"The", "The", false}, // stopwords only: no identity
	}
	for _, tc := range tests {
		for _, pair := range [][2]string{{tc.a, tc.b}, {tc.b, tc.a}} {
			if got := TitlesMatch(pair[0], pair[1]); got != tc.want {
				t.Errorf("TitlesMatch(%q, %q) = %v, want %v", pair[0], pair[1], got, tc.want)
			}
		}
	}
}

// TestSameBook adds the author check and author-token removal.
func TestSameBook(t *testing.T) {
	ref := func(title, author string) BookRef { return BookRef{Title: title, Author: author} }
	tests := []struct {
		name        string
		record, own BookRef
		want        bool
	}{
		{"same title and author", ref("Children of Dune", "Frank Herbert"), ref("Children of Dune", "Frank Herbert"), true},
		{"co-author listed in the record", ref("Children of Dune", "Frank Herbert, Brian Herbert"), ref("Children of Dune", "Frank Herbert"), true},
		{"unknown path author", ref("Children of Dune", "Frank Herbert"), ref("Children of Dune", ""), true},
		{"author in the path title", ref("Children of Dune", "Frank Herbert"), ref("Frank Herbert - Children of Dune", "Frank Herbert"), true},
		{"same title, different author", ref("Shift", "Hugh Howey"), ref("Shift", "Tim Kring"), false},
		{"series sibling, same author", ref("Dune", "Frank Herbert"), ref("Dune Messiah", "Frank Herbert"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameBook(tc.record, tc.own); got != tc.want {
				t.Errorf("SameBook(%+v, %+v) = %v, want %v", tc.record, tc.own, got, tc.want)
			}
		})
	}
}
