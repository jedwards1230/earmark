// The golden vectors in this file and in testdata/ are taken from Apache
// Commons Codec's DoubleMetaphoneTest and DoubleMetaphone2Test (commit
// d7bff678de1efdf767f713c5a178ba9dd9e1562d), Copyright The Apache Software
// Foundation, licensed under the Apache License, Version 2.0 (see
// LICENSE-APACHE-2.0.txt and the repository NOTICE file). Modified: rewritten
// as Go table tests and tab-separated test data.

package phonetic

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// readTSV loads a commons-codec-derived fixture, skipping '#' comment lines.
func readTSV(t *testing.T, path string, cols int) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var rows [][]string
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		row := strings.Split(text, "\t")
		if len(row) != cols {
			t.Fatalf("%s:%d: want %d columns, got %d", path, line, cols, len(row))
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no rows", path)
	}
	return rows
}

// TestDoubleMetaphone2Data is commons-codec's DoubleMetaphone2Test: 1,221
// names with their expected primary and alternate codes at the default
// maximum length of 4.
func TestDoubleMetaphone2Data(t *testing.T) {
	rows := readTSV(t, "testdata/doublemetaphone2.tsv", 3)
	if len(rows) != 1221 {
		t.Fatalf("fixture has %d rows, want 1221", len(rows))
	}
	for _, r := range rows {
		got := DoubleMetaphone(r[0], DefaultMaxCodeLen)
		if got.Primary != r[1] || got.Alternate != r[2] {
			t.Errorf("DoubleMetaphone(%q) = %q/%q, want %q/%q", r[0], got.Primary, got.Alternate, r[1], r[2])
		}
	}
}

// TestDoubleMetaphone is commons-codec's testDoubleMetaphone.
func TestDoubleMetaphone(t *testing.T) {
	primary := []struct{ in, want string }{
		{"testing", "TSTN"}, {"The", "0"}, {"quick", "KK"}, {"brown", "PRN"},
		{"fox", "FKS"}, {"jumped", "JMPT"}, {"over", "AFR"}, {"the", "0"},
		{"lazy", "LS"}, {"dogs", "TKS"}, {"MacCafferey", "MKFR"},
		{"Stephan", "STFN"}, {"Kuczewski", "KSSK"}, {"McClelland", "MKLL"},
		{"san jose", "SNHS"}, {"xenophobia", "SNFP"},
	}
	for _, tc := range primary {
		if got := DoubleMetaphone(tc.in, DefaultMaxCodeLen).Primary; got != tc.want {
			t.Errorf("primary(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	alternate := []struct{ in, want string }{
		{"testing", "TSTN"}, {"The", "T"}, {"quick", "KK"}, {"brown", "PRN"},
		{"fox", "FKS"}, {"jumped", "AMPT"}, {"over", "AFR"}, {"the", "T"},
		{"lazy", "LS"}, {"dogs", "TKS"}, {"MacCafferey", "MKFR"},
		{"Stephan", "STFN"}, {"Kutchefski", "KXFS"}, {"McClelland", "MKLL"},
		{"san jose", "SNHS"}, {"xenophobia", "SNFP"}, {"Fokker", "FKR"},
		{"Joqqi", "AK"}, {"Hovvi", "HF"}, {"Czerny", "XRN"},
	}
	for _, tc := range alternate {
		if got := DoubleMetaphone(tc.in, DefaultMaxCodeLen).Alternate; got != tc.want {
			t.Errorf("alternate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// equalCodes is commons-codec's isDoubleMetaphoneEqual at the default length.
func equalCodes(a, b string, alternate bool) bool {
	ca, cb := DoubleMetaphone(a, DefaultMaxCodeLen), DoubleMetaphone(b, DefaultMaxCodeLen)
	if alternate {
		return ca.Alternate == cb.Alternate
	}
	return ca.Primary == cb.Primary
}

// TestDoubleMetaphoneEqual covers testIsDoubleMetaphoneEqualBasic,
// testIsDoubleMetaphoneEqualExtended2, testCCedilla, testNTilde, testCodec184,
// testCodec320 and testIsDoubleMetaphoneNotEqual.
func TestDoubleMetaphoneEqual(t *testing.T) {
	basic := [][2]string{
		{"", ""}, {"Case", "case"}, {"CASE", "Case"}, {"caSe", "cAsE"},
		{"cookie", "quick"}, {"quick", "cookie"}, {"Brian", "Bryan"},
		{"Auto", "Otto"}, {"Steven", "Stefan"}, {"Philipowitz", "Filipowicz"},
	}
	for _, p := range basic {
		for _, alt := range []bool{false, true} {
			if !equalCodes(p[0], p[1], alt) || !equalCodes(p[1], p[0], alt) {
				t.Errorf("expected match %q/%q (alternate %v)", p[0], p[1], alt)
			}
		}
	}
	if !equalCodes("Jablonski", "Yablonsky", true) {
		t.Error("expected alternate match Jablonski/Yablonsky")
	}
	if !equalCodes("ç", "S", false) {
		t.Error("c-cedilla should encode as S")
	}
	if !equalCodes("ñ", "N", false) {
		t.Error("n-tilde should encode as N")
	}
	if !equalCodes("ANGHELINA", "ANKL", false) {
		t.Error("CODEC-320: ANGHELINA should match ANKL")
	}
	for _, alt := range []bool{false, true} {
		if equalCodes("aa", "", alt) || equalCodes("", "aa", alt) {
			t.Errorf("CODEC-184: aa must not match empty (alternate %v)", alt)
		}
		if equalCodes("Brain", "Band", alt) || equalCodes("Band", "Brain", alt) {
			t.Errorf("Brain must not match Band (alternate %v)", alt)
		}
	}
}

// TestDoubleMetaphoneMatches is commons-codec's
// testIsDoubleMetaphoneEqualWithMATCHES: each pair matches on the primary or
// the alternate code.
func TestDoubleMetaphoneMatches(t *testing.T) {
	rows := readTSV(t, "testdata/matches.tsv", 2)
	for _, r := range rows {
		if !equalCodes(r[0], r[1], false) && !equalCodes(r[0], r[1], true) {
			t.Errorf("expected a primary or alternate match for %q/%q", r[0], r[1])
		}
	}
}

// TestDoubleMetaphoneEmpty is commons-codec's testEmpty; null becomes empty
// Codes here.
func TestDoubleMetaphoneEmpty(t *testing.T) {
	for _, in := range []string{"", " ", "\t\n\r "} {
		if got := DoubleMetaphone(in, DefaultMaxCodeLen); got != (Codes{}) {
			t.Errorf("DoubleMetaphone(%q) = %+v, want empty", in, got)
		}
	}
}

// TestDoubleMetaphoneMaxCodeLen is commons-codec's testSetMaxCodeLength plus
// this port's unbounded mode.
func TestDoubleMetaphoneMaxCodeLen(t *testing.T) {
	tests := []struct {
		in      string
		max     int
		primary string
		alt     string
	}{
		{"jumped", 4, "JMPT", "AMPT"},
		{"jumped", 3, "JMP", "AMP"},
		{"jumped", 0, "JMPT", "AMPT"},
		// Unbounded (this port only — not commons-codec vectors): the whole
		// phrase is encoded, not its first four sounds.
		{"thegreatcourses", 0, "0KRTKRSS", "TKRTKRSS"},
		{"thegreatcourses", 4, "0KRT", "TKRT"},
		{"thegreatcourses", -1, "0KRTKRSS", "TKRTKRSS"},
		// Hand-traced: SCH+W → X (alternate keeps F for W after SCH), Z → S/TS
		// (slavo-germanic), GG → K.
		{"Schwarzenegger", 0, "XRSNKR", "XFRTSNKR"},
	}
	for _, tc := range tests {
		got := DoubleMetaphone(tc.in, tc.max)
		if got.Primary != tc.primary || got.Alternate != tc.alt {
			t.Errorf("DoubleMetaphone(%q, %d) = %q/%q, want %q/%q",
				tc.in, tc.max, got.Primary, got.Alternate, tc.primary, tc.alt)
		}
	}
}
