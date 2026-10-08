package phonetic

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestNumberWords(t *testing.T) {
	tests := map[string]string{
		"0":                   "zero",
		"7":                   "seven",
		"13":                  "thirteen",
		"20":                  "twenty",
		"42":                  "forty two",
		"100":                 "one hundred",
		"240":                 "two hundred forty",
		"1000":                "one thousand",
		"1001":                "one thousand one",
		"1984":                "one thousand nine hundred eighty four",
		"20000":               "twenty thousand",
		"1000000":             "one million",
		"2500017":             "two million five hundred thousand seventeen",
		"999999999999":        "nine hundred ninety nine billion nine hundred ninety nine million nine hundred ninety nine thousand nine hundred ninety nine",
		"007":                 "zero zero seven",
		"1234567890123456789": "one two three four five six seven eight nine zero one two three four five six seven eight nine",
		"":                    "",
		"12a":                 "",
		"-3":                  "",
	}
	for in, want := range tests {
		if got := NumberWords(in); got != want {
			t.Errorf("NumberWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := map[string]string{
		"Auto Sebo":       "autosebo",
		"Lempel-Ziv":      "lempelziv",
		"240":             "twohundredforty",
		"1,000 miles":     "onethousandmiles",
		"10, 12":          "tentwelve",
		"route 66!":       "routesixtysix",
		"Ünïcode":         "ünïcode",
		"... ---":         "",
		"x1y":             "xoney",
		"o’brien's":       "obriens",
		"  \t\n":          "",
		"two hundred 40":  "twohundredforty",
		"3.5":             "threepointfive",
		"1,2,3":           "onetwothree",
		"1,0000":          "onezerozerozerozero",
		"12,345.67":       "twelvethousandthreehundredfortyfivepointsixseven",
		"1234,567":        "onethousandtwohundredthirtyfourfivehundredsixtyseven",
		"9,99,999 (typo)": "nineninetyninethousandninehundredninetyninetypo",
	}
	for in, want := range tests {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSoundAlike(t *testing.T) {
	tests := []struct {
		a, b  string
		score float64
		pass  bool
	}{
		// Judge-prompt examples (internal/eval/prompt.go).
		{"auto sebo", "Arecibo", 0.75, true},               // ATSP vs ARSP
		{"Holovo", "Holevo", 1, true},                      // HLF
		{"placenes", "place names", 5.0 / 6, true},         // PLSNS vs PLSNMS
		{"pin name", "pen name", 1, true},                  // PNM
		{"their", "there", 1, true},                        // 0R/TR
		{"thegreycourses", "thegreatcourses", 0.875, true}, // 0KRKRSS vs 0KRTKRSS
		{"Limpel Ziv", "Lempel-Ziv", 1, true},              // LMPLSF
		{"too forty", "240", 1, true},                      // TFRT: "240" read as "two forty"
		{"240", "two hundred forty", 1, true},              // same normalized text
		{"neumann", "newman", 1, true},                     // NMN
		{"cat", "dog", 0, false},                           // KT vs TK
		{"holevo", "shannon", 0, false},                    // HLF vs XNN
		{"arecibo", "puerto rico", 0.2, false},             // ARSP vs PRTRK
		{"auto sebo", "algebra", 0.4, false},               // ATSP vs ALJPR/ALKPR
		{"and", "an", 2.0 / 3, false},                      // ANT vs AN: 0.667 < 0.67
		{"240", "250", 8.0 / 9, true},                      // forty/fifty differ by one code letter
		{"", "", 0, false},                                 // nothing to compare
		{"!!!", "abc", 0, false},                           // one side empty after normalizing
		{"h", "w", 0, false},                               // both codes empty, texts differ
		{"hh", "HH", 1, true},                              // codes empty, texts identical
		{"Case", "case", 1, true},                          // identical once normalized
		{"tic tac toe", "tic-tac-toe", 1, true},            // identical once normalized
		{"seventeen", "seventy", 0.8, true},                // SFNTN vs SFNT
		{"nineteen eighty four", "1984", 1, true},          // paired (year) reading
		{"two oh five", "205", 1, true},                    // paired reading with "oh"
		{"twenty oh five", "2005", 1, true},
		{"nineteen hundred", "1900", 1, true},
		{"two four zero", "240", 1, true},                // digit by digit
		{"1,984", "nineteen eighty four", 0.4375, false}, // grouped: cardinal only
		{"Schwarzenegger", "shwartseneger", 0.875, true}, // XFRTSNKR vs XRTSNKR
		{"the cat", "a dog", 1.0 / 3, false},             // 0KT/TKT vs ATK
		{"Wasserman", "Vasserman", 1, true},              // alternate pairing AFSRMN
		{"Jose", "Hosay", 1, true},                       // H vs H
		{"Smith", "Schmidt", 1, true},                    // SM0/XMT alternate pairing
		{"Arecibo", "auto sebo", 0.75, true},             // order does not matter
		{"place names", "placenes", 5.0 / 6, true},       // order does not matter
		{"supercalifragilistic", "super cali fragilistic", 1, true},
	}
	for _, tc := range tests {
		ok, score := SoundAlike(tc.a, tc.b, DefaultSoundAlikeThreshold)
		if ok != tc.pass || math.Abs(score-tc.score) > 1e-9 {
			m := Compare(tc.a, tc.b)
			t.Errorf("SoundAlike(%q, %q) = %v %.4f [%s/%s vs %s/%s], want %v %.4f",
				tc.a, tc.b, ok, score, m.A.Codes.Primary, m.A.Codes.Alternate,
				m.B.Codes.Primary, m.B.Codes.Alternate, tc.pass, tc.score)
		}
	}
}

func TestNumberReadings(t *testing.T) {
	tests := map[string][]string{
		"7":     {"seven"},
		"42":    {"forty two", "four two"},
		"240":   {"two hundred forty", "two forty", "two four zero"},
		"205":   {"two hundred five", "two oh five", "two zero five"},
		"200":   {"two hundred", "two zero zero"},
		"1984":  {"one thousand nine hundred eighty four", "nineteen eighty four", "one nine eight four"},
		"2005":  {"two thousand five", "twenty oh five", "two zero zero five"},
		"1900":  {"one thousand nine hundred", "nineteen hundred", "one nine zero zero"},
		"2000":  {"two thousand", "two zero zero zero"},
		"1010":  {"one thousand ten", "ten ten", "one zero one zero"},
		"007":   {"zero zero seven"},
		"12345": {"twelve thousand three hundred forty five", "one two three four five"},
		"":      nil,
		"x":     nil,
	}
	for in, want := range tests {
		if got := NumberReadings(in); !slices.Equal(got, want) {
			t.Errorf("NumberReadings(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadings(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"auto sebo", []string{"auto sebo"}},
		{"", []string{""}},
		{"Route 66!", []string{"route sixty six", "route six six"}},
		{"about 240 miles", []string{"about two hundred forty miles", "about two forty miles", "about two four zero miles"}},
		{"1,984", []string{"one thousand nine hundred eighty four"}},
		// 2 × 3 = 6 combinations: within the cap, cardinal-first, first
		// numeral varying fastest.
		{"42 240", []string{
			"forty two two hundred forty", "four two two hundred forty",
			"forty two two forty", "four two two forty",
			"forty two two four zero", "four two two four zero",
		}},
	}
	for _, tc := range tests {
		if got := Readings(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("Readings(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadingsCap(t *testing.T) {
	// Three numerals with three readings each: 27 = MaxReadings, all kept.
	if n := len(Readings("240 205 1984")); n != 27 {
		t.Errorf("three numerals gave %d readings, want 27", n)
	}
	// A fourth numeral would make 81: it is pinned to its cardinal, the
	// first three keep every combination.
	got := Readings("240 205 1984 1990")
	if len(got) != 27 {
		t.Fatalf("four numerals gave %d readings, want 27", len(got))
	}
	for _, r := range got {
		if !strings.HasSuffix(r, " one thousand nine hundred ninety") {
			t.Errorf("fourth numeral not pinned to its cardinal: %q", r)
		}
	}
	if !slices.Contains(got, "two forty two oh five nineteen eighty four one thousand nine hundred ninety") {
		t.Errorf("earliest numerals lost a combination: %q", got)
	}
	// Input longer than MaxPhraseRunes gets the cardinal reading only.
	if n := len(Readings(strings.Repeat("12 ", 200))); n != 1 {
		t.Errorf("200 numerals gave %d readings, want 1", n)
	}
	for _, s := range []string{"1 2 3", "12 34 56", "10 20", "240", "1 22 333 4444 55555"} {
		if n := len(Readings(s)); n < 1 || n > MaxReadings {
			t.Errorf("Readings(%q) has %d readings", s, n)
		}
	}
}

func TestScanNumeral(t *testing.T) {
	tests := []struct {
		in   string
		want Numeral
	}{
		{"240 x", Numeral{Whole: "240", End: 3}},
		{"1,000 x", Numeral{Whole: "1000", Grouped: true, End: 5}},
		{"12,345.67", Numeral{Whole: "12345", Grouped: true, Fraction: "67", End: 9}},
		{"3.5.", Numeral{Whole: "3", Fraction: "5", End: 3}},
		{"1,2,3", Numeral{Whole: "1", End: 1}},
		{"1,0000", Numeral{Whole: "1", End: 1}},
		{"1234,567", Numeral{Whole: "1234", End: 4}},
		{"7.", Numeral{Whole: "7", End: 1}},
	}
	for _, tc := range tests {
		if got := ScanNumeral([]rune(tc.in), 0); got != tc.want {
			t.Errorf("ScanNumeral(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestCompareBounds(t *testing.T) {
	long := strings.Repeat("7", MaxPhraseRunes+1)
	if m := Compare(long, long); m.Score != 0 {
		t.Errorf("over-long input scored %v, want 0", m.Score)
	}
	// 50k digits must not take minutes.
	if ok, _ := SoundAlike(strings.Repeat("9", 50000), strings.Repeat("8", 50000), DefaultSoundAlikeThreshold); ok {
		t.Error("over-long input passed")
	}
}

func TestCompareReportsWinningReading(t *testing.T) {
	m := Compare("too forty", "240")
	if m.B.Reading != "two forty" || m.B.Readings != 3 || m.A.Reading != "too forty" || m.A.Readings != 1 {
		t.Errorf("Compare reported A %+v B %+v", m.A, m.B)
	}
	m = Compare("1984", "nineteen eighty four")
	if m.A.Reading != "nineteen eighty four" || m.Score != 1 {
		t.Errorf("Compare reported A %+v score %v", m.A, m.Score)
	}
}

func TestSoundAlikeZeroThresholdNeverPassesEmpty(t *testing.T) {
	if ok, _ := SoundAlike("", "abc", 0); ok {
		t.Error("an empty side passed at threshold 0")
	}
}

// FuzzSoundAlikeSymmetric checks the score is symmetric and within [0, 1].
// The seed corpus runs under plain `go test`.
func FuzzSoundAlikeSymmetric(f *testing.F) {
	for _, s := range [][2]string{
		{"auto sebo", "Arecibo"}, {"Holovo", "Holevo"}, {"the the cat", "the cat"},
		{"placenes", "place names"}, {"too forty", "240"}, {"tic tac toe", "tic-tac-toe"},
		{"french", "French"}, {"", ""}, {"", "a"}, {"h", "w"}, {"1,000,000", "a million"},
		{"Çedilla", "Ñandu"}, {"straße", "strasse"}, {"İstanbul", "istanbul"},
		{"san jose", "SAN JOSE"}, {"99999999999999999999", "x"},
		{"too forty", "240"}, {"1984 and 2005", "nineteen eighty four and twenty oh five"},
		{"1 2 3 4 5 6 7 8 9", "123456789"},
		{"1,000", "one thousand"}, {"3.5", "three point five"}, {"1,2,3", "1,234,567.89"},
		{"240 205 1984 1990", "two forty two oh five nineteen eighty four nineteen ninety"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		_, ab := SoundAlike(a, b, DefaultSoundAlikeThreshold)
		_, ba := SoundAlike(b, a, DefaultSoundAlikeThreshold)
		if ab != ba {
			t.Fatalf("asymmetric: score(%q,%q)=%v score(%q,%q)=%v", a, b, ab, b, a, ba)
		}
		if ab < 0 || ab > 1 || math.IsNaN(ab) {
			t.Fatalf("score(%q,%q)=%v out of [0,1]", a, b, ab)
		}
	})
}
