package phonetic

import (
	"math"
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
		"3.5":             "threefive",
		"9,99,999 (typo)": "ninehundredninetyninethousandninehundredninetyninetypo",
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
		{"too forty", "240", 4.0 / 9, false},               // TFRT vs THNTRTFRT: known gap
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
		{"nineteen eighty four", "1984", 0.4375, false},    // cardinal reading only
		{"Schwarzenegger", "shwartseneger", 0.875, true},   // XFRTSNKR vs XRTSNKR
		{"the cat", "a dog", 1.0 / 3, false},               // 0KT/TKT vs ATK
		{"Wasserman", "Vasserman", 1, true},                // alternate pairing AFSRMN
		{"Jose", "Hosay", 1, true},                         // H vs H
		{"Smith", "Schmidt", 1, true},                      // SM0/XMT alternate pairing
		{"Arecibo", "auto sebo", 0.75, true},               // order does not matter
		{"place names", "placenes", 5.0 / 6, true},         // order does not matter
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
