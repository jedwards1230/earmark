package decide

import (
	"html"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/phonetic"
)

// Text-evidence kinds (Outcome.Evidence). Persisted with the decision, so a
// data contract: add new ones, never rename.
const (
	// EvidenceASINVerbatim — the words the correction introduces appear,
	// contiguously and word-bounded, in the book's catalogue record.
	EvidenceASINVerbatim = "asin_verbatim"
	// EvidenceExactRepeat — a repeated_text fix rung 0 verified as the removal
	// of an exact adjacent repeat: the text itself is the evidence.
	EvidenceExactRepeat = "exact_repeat"
	// EvidenceNone — no text evidence.
	EvidenceNone = "none"
)

// EvidenceRule names the rule TextEvidence implements (user decision D2(a)).
// It is a recipe param: changing the rule must change this name.
const EvidenceRule = "d2a_new_tokens_verbatim@v1"

// Relevance caps on the record sentences shown to the model.
const (
	MaxRelevantSentences = 4
	MaxRelevantRunes     = 600
	// MaxSentenceRunes clips one long sentence to a window around its match so
	// a single blurb paragraph cannot take the whole budget.
	MaxSentenceRunes = 200
	// ClipWords is the ±window kept around the match when clipping.
	ClipWords = 12
)

// minEvidenceRunes is the shortest non-stopword that can carry asin_verbatim
// evidence on its own (D2: ≥1 non-stopword of ≥4 characters).
const minEvidenceRunes = 4

// RecordSentence is one line of a book's catalogue record.
type RecordSentence struct {
	// Field is the column it came from: title, author, narrator, series,
	// chapter or description.
	Field string
	Text  string
}

// RecordSentences splits a catalogue record into sentences: title, author,
// narrator, each series and each chapter title are one sentence each; the
// description (the publisher's blurb, which may carry HTML) is stripped of
// markup and split into sentences. A record with no ASIN yields nothing —
// without a catalogue match there is no reference text to trust.
func RecordSentences(r *db.BookRecord) []RecordSentence {
	if r == nil || strings.TrimSpace(r.ASIN) == "" {
		return nil
	}
	var out []RecordSentence
	add := func(field, text string) {
		if text = collapseSpace(text); text != "" {
			out = append(out, RecordSentence{Field: field, Text: text})
		}
	}
	add("title", r.Title)
	add("author", r.Author)
	add("narrator", r.Narrator)
	for _, s := range r.Series {
		if s.Sequence != "" {
			add("series", s.Name+" #"+s.Sequence)
		} else {
			add("series", s.Name)
		}
	}
	for _, c := range r.Chapters {
		add("chapter", c.Title)
	}
	for _, s := range SplitSentences(StripHTML(r.Description)) {
		add("description", s)
	}
	return out
}

// blockTags end a line when stripped: a paragraph or list item is a sentence
// boundary even when the blurb has no full stop.
var blockTags = map[string]bool{
	"p": true, "br": true, "div": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "blockquote": true,
}

// StripHTML removes tags (block-level ones become line breaks, inline ones a
// space) and the content of script/style elements, then decodes entities.
func StripHTML(s string) string {
	var b strings.Builder
	skip := "" // inside <script> or <style>: the closing tag to wait for
	for i := 0; i < len(s); {
		if s[i] != '<' {
			if skip == "" {
				b.WriteByte(s[i])
			}
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 { // a stray '<' — keep it as text
			if skip == "" {
				b.WriteString(s[i:])
			}
			break
		}
		tag := strings.ToLower(strings.TrimSpace(s[i+1 : i+end]))
		i += end + 1
		closing := strings.HasPrefix(tag, "/")
		name := strings.TrimLeft(tag, "/")
		if j := strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == '/' }); j >= 0 {
			name = name[:j]
		}
		if skip != "" {
			if closing && name == skip {
				skip = ""
			}
			continue
		}
		switch {
		case !closing && (name == "script" || name == "style"):
			skip = name
		case blockTags[name]:
			b.WriteByte('\n')
		default:
			b.WriteByte(' ')
		}
	}
	return html.UnescapeString(b.String())
}

// abbreviations do not end a sentence.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "st": true, "jr": true, "sr": true,
	"vs": true, "prof": true, "gen": true, "col": true, "lt": true, "capt": true, "sgt": true,
	"mt": true, "no": true, "vol": true, "inc": true, "ltd": true, "co": true,
}

// SplitSentences splits text on line breaks and on . ! ? … followed by
// whitespace, except after a common abbreviation ("Dr. Grace") or a single
// initial ("J. R. R."). Sentences are space-collapsed; empty ones dropped.
func SplitSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		runes := []rune(line)
		start := 0
		for i := 0; i < len(runes); i++ {
			if !isTerminal(runes[i]) {
				continue
			}
			j := i
			for j+1 < len(runes) && (isTerminal(runes[j+1]) || isClosingQuote(runes[j+1])) {
				j++
			}
			if j+1 < len(runes) && !unicode.IsSpace(runes[j+1]) {
				i = j
				continue
			}
			if runes[i] == '.' && endsWithAbbreviation(runes[start:i]) {
				i = j
				continue
			}
			if s := collapseSpace(string(runes[start : j+1])); s != "" {
				out = append(out, s)
			}
			start, i = j+1, j
		}
		if s := collapseSpace(string(runes[start:])); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isTerminal(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '…' }

func isClosingQuote(r rune) bool {
	return r == '"' || r == '\'' || r == '”' || r == '’' || r == ')'
}

func endsWithAbbreviation(rs []rune) bool {
	i := len(rs)
	for i > 0 && unicode.IsLetter(rs[i-1]) {
		i--
	}
	word := strings.ToLower(string(rs[i:]))
	if word == "" {
		return false
	}
	return abbreviations[word] || utf8.RuneCountInString(word) == 1
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// Relevant keeps the record sentences that bear on a finding: those sharing a
// non-stopword with the original span or the replacement, or holding a word
// that sounds like a span word (Double Metaphone primary codes match, also
// against the span read as one word, so "auto sebo" finds "Arecibo").
// Sentences containing the replacement's new words come first, then record
// order; at most MaxRelevantSentences sentences and MaxRelevantRunes runes,
// each long sentence clipped around its first match.
//
// Filtering matters beyond cost: unrelated context can flip a decision
// model's answer, so the model sees only what bears on the edit.
func Relevant(sentences []RecordSentence, original, replacement string) []RecordSentence {
	words := map[string]bool{}
	for _, t := range append(tokens(original), tokens(replacement)...) {
		if !stopwords[t] {
			words[t] = true
		}
	}
	codes := map[string]bool{}
	addCode := func(s string) {
		if c := phonetic.DoubleMetaphone(s, 0); c.Primary != "" {
			codes[c.Primary] = true
			codes[c.Alternate] = true
		}
	}
	spanTokens := tokens(original)
	for _, t := range spanTokens {
		if !stopwords[t] && utf8.RuneCountInString(t) >= 3 {
			addCode(t)
		}
	}
	if len(spanTokens) > 1 {
		addCode(strings.Join(spanTokens, ""))
	}
	added := newTokens(original, replacement)

	type hit struct {
		s     RecordSentence
		rank  int // 0 = contains the new words
		match int // token index of the first match
	}
	var hits []hit
	for _, s := range sentences {
		ts := tokens(s.Text)
		match := -1
		for i, t := range ts {
			if words[t] {
				match = i
				break
			}
			if !stopwords[t] && utf8.RuneCountInString(t) >= 3 && codes[phonetic.DoubleMetaphone(t, 0).Primary] {
				match = i
				break
			}
		}
		if match < 0 {
			continue
		}
		rank := 1
		if at := indexRun(ts, added); len(added) > 0 && at >= 0 {
			rank, match = 0, at
		}
		hits = append(hits, hit{s: s, rank: rank, match: match})
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return a.rank - b.rank })

	var out []RecordSentence
	budget := MaxRelevantRunes
	for _, h := range hits {
		if len(out) == MaxRelevantSentences {
			break
		}
		s := h.s
		s.Text = clip(s.Text, h.match)
		n := utf8.RuneCountInString(s.Text)
		if n > budget {
			continue
		}
		budget -= n
		out = append(out, s)
	}
	return out
}

// clip keeps ±ClipWords words around word index at when s is longer than
// MaxSentenceRunes. Word indices count whitespace-separated words, which
// match token indices for ordinary text and stay close otherwise.
func clip(s string, at int) string {
	if utf8.RuneCountInString(s) <= MaxSentenceRunes {
		return s
	}
	f := strings.Fields(s)
	lo, hi := max(0, at-ClipWords), min(len(f), at+ClipWords+1)
	out := strings.Join(f[lo:hi], " ")
	if lo > 0 {
		out = "… " + out
	}
	if hi < len(f) {
		out += " …"
	}
	if r := []rune(out); len(r) > MaxSentenceRunes {
		out = string(r[:MaxSentenceRunes-1]) + "…"
	}
	return out
}

// TextEvidence is the text evidence for a candidate that passed rung 0 (D2):
//
//   - exact_repeat: a repeated_text fix — rung 0 has already verified it
//     removes an exact adjacent repeat, so the text is its own evidence;
//   - asin_verbatim: the tokens the replacement introduces (its differing
//     window against the original) appear contiguously, word-bounded and
//     case-insensitively in one record sentence, and include at least one
//     non-stopword of minEvidenceRunes or more characters;
//   - none: anything else, including every candidate whose book has no
//     catalogue record (sentences is empty).
//
// Evidence never overrides the issue-type cap: a dropped_word or
// number_artifact fix can carry asin_verbatim and still only be held.
func TextEvidence(c Candidate, v Verdict, sentences []RecordSentence) string {
	if !v.Pass {
		return EvidenceNone
	}
	if c.IssueType == IssueRepeatedText {
		return EvidenceExactRepeat
	}
	added := newTokens(c.Original, c.Replacement)
	if !carriesEvidence(added) {
		return EvidenceNone
	}
	for _, s := range sentences {
		if indexRun(tokens(s.Text), added) >= 0 {
			return EvidenceASINVerbatim
		}
	}
	return EvidenceNone
}

// newTokens is the replacement's differing window against the original — the
// words the correction introduces — or nil for a pure deletion.
func newTokens(original, replacement string) []string {
	o, r := tokens(original), tokens(replacement)
	pre, suf := diffWindow(o, r)
	if len(r)-suf <= pre {
		return nil
	}
	return r[pre : len(r)-suf]
}

func carriesEvidence(ts []string) bool {
	for _, t := range ts {
		if !stopwords[t] && utf8.RuneCountInString(t) >= minEvidenceRunes {
			return true
		}
	}
	return false
}

// indexRun is the index of the first contiguous occurrence of run in ts, or
// -1. Tokens are whole words, so a match is word-bounded by construction.
func indexRun(ts, run []string) int {
	if len(run) == 0 {
		return -1
	}
	for i := 0; i+len(run) <= len(ts); i++ {
		if slices.Equal(ts[i:i+len(run)], run) {
			return i
		}
	}
	return -1
}

// stopwords are function words: they can neither make a sentence relevant nor
// carry evidence alone. Lower-case, apostrophes removed (tokens' form).
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a about above after again against all am an and any are as at be because
		been before being below between both but by can cannot could did do does doing down during each
		few for from further had has have having he her here hers herself him himself his how i if in
		into is it its itself just me more most my myself no nor not now of off on once only or other
		our ours ourselves out over own same she should so some such than that the their theirs them
		themselves then there these they this those through to too under until up very was we were
		what when where which while who whom why will with would you your yours yourself yourselves
		im ive id ill youre youve youd youll hes shes its were weve theyre theyve theyd theyll
		dont doesnt didnt cant couldnt wont wouldnt shouldnt isnt arent wasnt werent hasnt havent hadnt
		thats whats lets also may might must shall`) {
		m[w] = true
	}
	return m
}()
