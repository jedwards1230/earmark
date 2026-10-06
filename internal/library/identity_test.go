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

// TestTitlesMatch pins the cross-check that guards the embedded tag:
// normalized token overlap against the smaller title.
func TestTitlesMatch(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"Children of Dune", "Children of Dune", true},
		{"Children of Dune: Dune Chronicles, Book 3", "Children of Dune", true},
		{"Project Hail Mary", "project hail mary [B08G9PRS1K]", true},
		{"Muad'Dib's Legacy", "Muad’Dib’s Legacy", true},
		{"Eragon", "Children of Dune", false},
		{"The Way of Kings", "The Name of the Wind", false},
		{"Dune Messiah", "Children of Dune", false}, // 1 of 2 identity tokens
		{"", "Children of Dune", false},
		{"The", "The", false}, // stopwords only: no identity
	}
	for _, tc := range tests {
		if got := TitlesMatch(tc.a, tc.b); got != tc.want {
			t.Errorf("TitlesMatch(%q, %q) = %v (overlap %.2f), want %v",
				tc.a, tc.b, got, TitleOverlap(tc.a, tc.b), tc.want)
		}
	}
}
