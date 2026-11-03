// Package asrpilot is the pure logic of the second-ASR pilot (CONTRACT §1.11):
// the asr_vote — what a pilot model heard at a held finding's span — and the
// pilot measurement report (word disagreement vs the production model,
// casing/punctuation, speed, VRAM, vote agreement). No database, no I/O: the
// cmd/asrpilot command loads one track at a time and calls in here.
//
// Nothing here feeds decide. A vote is evidence recorded for a later,
// versioned decide recipe; the apply policy is unchanged.
package asrpilot

import (
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/db"
)

// Token is one comparison word of a transcript, normalised, with its time.
type Token struct {
	// Norm is the normalised word (Normalize): lowercase, no punctuation.
	Norm string
	// Raw is the source word as written (casing and punctuation kept). A raw
	// word that normalises to several tokens ("well-known") repeats on each.
	Raw string
	// RawIdx numbers the raw words of the sequence, so heard text can be
	// rebuilt from distinct raw words.
	RawIdx int
	// Start and End are track seconds. Timed is false when they were
	// interpolated inside the segment rather than read from word timestamps.
	Start, End float64
	Timed      bool
	// Seg is the index of the segment the token came from; RuneStart and
	// RuneEnd its rune range within that segment's text (production tokens
	// only: pilot tokens leave them zero).
	Seg                int
	RuneStart, RuneEnd int
}

// Mid is the token's time midpoint.
func (t Token) Mid() float64 { return (t.Start + t.End) / 2 }

// Normalize lowercases s, turns dashes and slashes into spaces, drops every
// other punctuation and symbol rune, and collapses whitespace. The result is
// the space-joined comparison form ("Dorian's well-known" → "dorians well
// known").
func Normalize(s string) string {
	return strings.Join(normFields(s), " ")
}

func normFields(s string) []string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r), r == '-', r == '/', unicode.Is(unicode.Pd, r):
			b.WriteByte(' ')
		case unicode.IsPunct(r), unicode.IsSymbol(r):
			// dropped: "don't" → "dont", "Mr." → "mr"
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.Fields(b.String())
}

// fieldSpan is one whitespace-separated field of a text with its rune range.
type fieldSpan struct {
	text       string
	start, end int // runes
}

func fieldsWithRunes(s string) []fieldSpan {
	var out []fieldSpan
	start := -1
	i := 0
	runes := []rune(s)
	for ; i < len(runes); i++ {
		if unicode.IsSpace(runes[i]) {
			if start >= 0 {
				out = append(out, fieldSpan{string(runes[start:i]), start, i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, fieldSpan{string(runes[start:i]), start, i})
	}
	return out
}

// ProductionTokens flattens a production transcript's segments into tokens.
// The tokens are the fields of each segment's TEXT (the chunk text is the
// segments' texts joined by one space, so spans map onto them exactly); a
// field takes the time of the word at the same position when the segment has
// exactly one word per field, and otherwise a time interpolated by rune
// position across the segment (Timed false). A segment with no duration
// (raw-text fallback, start = end) yields tokens with Start = End.
func ProductionTokens(segs []db.Segment) []Token {
	var out []Token
	raw := 0
	for si, s := range segs {
		fields := fieldsWithRunes(s.Text)
		timed := len(s.Words) == len(fields) && len(fields) > 0
		n := len([]rune(s.Text))
		for fi, f := range fields {
			var start, end float64
			if timed {
				start, end = s.Words[fi].Start, s.Words[fi].End
			} else if n > 0 {
				d := s.End - s.Start
				start = s.Start + d*float64(f.start)/float64(n)
				end = s.Start + d*float64(f.end)/float64(n)
			}
			for _, p := range normFields(f.text) {
				out = append(out, Token{Norm: p, Raw: f.text, RawIdx: raw, Start: start, End: end,
					Timed: timed, Seg: si, RuneStart: f.start, RuneEnd: f.end})
			}
			raw++
		}
	}
	return out
}

// PilotTokens flattens pilot segments into tokens. Words with timestamps keep
// them; a segment whose words carry none (Canary) gets times interpolated
// evenly across the segment by word position (Timed false). A segment with an
// empty words list falls back to the fields of its text.
func PilotTokens(segs []db.PilotSegment) []Token {
	var out []Token
	raw := 0
	for si, s := range segs {
		type w struct {
			text       string
			start, end *float64
		}
		var words []w
		for _, x := range s.Words {
			words = append(words, w{x.Word, x.Start, x.End})
		}
		if len(words) == 0 {
			for _, f := range strings.Fields(s.Text) {
				words = append(words, w{text: f})
			}
		}
		n := float64(len(words))
		for wi, x := range words {
			start := s.Start + (s.End-s.Start)*float64(wi)/n
			end := s.Start + (s.End-s.Start)*float64(wi+1)/n
			timed := x.start != nil && x.end != nil
			if timed {
				start, end = *x.start, *x.end
			}
			for _, p := range normFields(x.text) {
				out = append(out, Token{Norm: p, Raw: x.text, RawIdx: raw, Start: start, End: end,
					Timed: timed, Seg: si})
			}
			raw++
		}
	}
	return out
}

// rawText rebuilds the as-written text of toks (one raw word per RawIdx).
func rawText(toks []Token) string {
	var parts []string
	last := -1
	for _, t := range toks {
		if t.RawIdx != last {
			parts = append(parts, t.Raw)
			last = t.RawIdx
		}
	}
	return strings.Join(parts, " ")
}

func norms(toks []Token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.Norm
	}
	return out
}

// TextStats counts a transcript's raw words with an uppercase letter and with
// a punctuation rune — the casing and punctuation rates of the report.
type TextStats struct {
	Words, Cased, Punctuated int64
}

// Add counts toks' raw words (once per RawIdx).
func (s *TextStats) Add(toks []Token) {
	last := -1
	for _, t := range toks {
		if t.RawIdx == last {
			continue
		}
		last = t.RawIdx
		s.Words++
		var up, punct bool
		for _, r := range t.Raw {
			up = up || unicode.IsUpper(r)
			punct = punct || unicode.IsPunct(r)
		}
		if up {
			s.Cased++
		}
		if punct {
			s.Punctuated++
		}
	}
}

// CasingRate is the share of words carrying an uppercase letter.
func (s TextStats) CasingRate() float64 { return ratio(s.Cased, s.Words) }

// PunctuationRate is the share of words carrying a punctuation rune.
func (s TextStats) PunctuationRate() float64 { return ratio(s.Punctuated, s.Words) }

// HasCasing reports whether a model writes case: at least 1% of its words
// carry an uppercase letter (a lowercase-only model has none).
func (s TextStats) HasCasing() bool { return s.Words > 0 && s.CasingRate() >= 0.01 }

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
