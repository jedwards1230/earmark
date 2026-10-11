package decide

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/metaprovider"
)

func TestStripHTML(t *testing.T) {
	in := `<p>Ryland Grace wakes up <b>alone</b>.</p><p>Earth&#39;s sun is dying &amp; he must act<br/>now</p>` +
		`<script>alert("x")</script><style>p{}</style><div class="x">The end</div> 3 < 4`
	got := SplitSentences(StripHTML(in))
	want := []string{
		"Ryland Grace wakes up alone .",
		"Earth's sun is dying & he must act",
		"now",
		"The end",
		"3 < 4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSplitSentences(t *testing.T) {
	got := SplitSentences(`Dr. Grace woke up. He was alone! Was he? J. R. R. Tolkien wrote "it." Then... more`)
	want := []string{"Dr. Grace woke up.", "He was alone!", "Was he?", `J. R. R. Tolkien wrote "it."`, "Then...", "more"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func hailMary() *db.BookRecord {
	return &db.BookRecord{
		ASIN:     "B08GB58KD5",
		Title:    "Project Hail Mary",
		Author:   "Andy Weir",
		Narrator: "Ray Porter",
		Series:   []metaprovider.SeriesRef{{Name: "Standalone", Sequence: "1"}},
		Chapters: []metaprovider.Chapter{{Title: "Chapter 1"}, {Title: "The Arecibo Message"}},
		Description: `<p>Ryland Grace is the sole survivor on a desperate mission.</p>` +
			`<p>He listens to the dish at Arecibo for a reply. The Holevo bound limits what he can learn. ` +
			`Unrelated sentence about lunch.</p>`,
	}
}

func TestRecordSentences(t *testing.T) {
	if got := RecordSentences(nil); got != nil {
		t.Errorf("nil record: %v", got)
	}
	noASIN := hailMary()
	noASIN.ASIN = " "
	if got := RecordSentences(noASIN); got != nil {
		t.Errorf("record without ASIN: %v", got)
	}
	got := RecordSentences(hailMary())
	var fields []string
	for _, s := range got {
		fields = append(fields, s.Field+"|"+s.Text)
	}
	want := []string{
		"title|Project Hail Mary", "author|Andy Weir", "narrator|Ray Porter", "series|Standalone #1",
		"chapter|Chapter 1", "chapter|The Arecibo Message",
		"description|Ryland Grace is the sole survivor on a desperate mission.",
		"description|He listens to the dish at Arecibo for a reply.",
		"description|The Holevo bound limits what he can learn.",
		"description|Unrelated sentence about lunch.",
	}
	if !slices.Equal(fields, want) {
		t.Errorf("got  %q\nwant %q", fields, want)
	}
}

func TestRelevant(t *testing.T) {
	recs := RecordSentences(hailMary())
	texts := func(rs []RecordSentence) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Text)
		}
		return out
	}

	got := texts(Relevant(recs, "auto sebo", "Arecibo"))
	// The sentences naming the new word first, record order within a rank.
	want := []string{"The Arecibo Message", "He listens to the dish at Arecibo for a reply."}
	if !slices.Equal(got, want) {
		t.Errorf("arecibo: got %q want %q", got, want)
	}

	// A sound-alike span word finds the sentence even when the replacement
	// is absent from it ("holovo" ~ "Holevo").
	got = texts(Relevant(recs, "holovo", "Holvo"))
	if !slices.Equal(got, []string{"The Holevo bound limits what he can learn."}) {
		t.Errorf("phonetic: got %q", got)
	}

	// Stopword-only overlap never makes a sentence relevant.
	if got := Relevant(recs, "the", "a"); len(got) != 0 {
		t.Errorf("stopwords: got %q", texts(got))
	}

	// Caps: sentence count and rune budget, long sentences clipped.
	many := make([]RecordSentence, 20)
	for i := range many {
		many[i] = RecordSentence{Field: "description", Text: "arecibo " + strings.Repeat("filler ", 100)}
	}
	capped := Relevant(many, "auto sebo", "Arecibo")
	if len(capped) > MaxRelevantSentences {
		t.Errorf("%d sentences, cap %d", len(capped), MaxRelevantSentences)
	}
	total := 0
	for _, s := range capped {
		n := utf8.RuneCountInString(s.Text)
		if n > MaxSentenceRunes {
			t.Errorf("sentence of %d runes not clipped", n)
		}
		if !strings.HasPrefix(s.Text, "arecibo") {
			t.Errorf("clip lost the match: %q", s.Text)
		}
		total += n
	}
	if total > MaxRelevantRunes {
		t.Errorf("%d runes, cap %d", total, MaxRelevantRunes)
	}
}

func TestTextEvidence(t *testing.T) {
	recs := RecordSentences(hailMary())
	pass := Verdict{Pass: true}
	tests := []struct {
		name       string
		issue      string
		orig, repl string
		verdict    Verdict
		sentences  []RecordSentence
		want       string
	}{
		{"new name present", IssueMisheardProperNoun, "auto sebo", "Arecibo", pass, recs, EvidenceASINVerbatim},
		{"case-insensitive", IssueMisheardProperNoun, "holovo", "HOLEVO", pass, recs, EvidenceASINVerbatim},
		{"multi-word new tokens contiguous", IssueMisheardWord, "holovo bounds", "Holevo bound", pass, recs, EvidenceASINVerbatim},
		{"multi-word new tokens not contiguous", IssueMisheardWord, "rye land race", "Ryland survivor", pass, recs, EvidenceNone},
		{"new tokens absent", IssueMisheardProperNoun, "holovo", "Holova", pass, recs, EvidenceNone},
		{"word boundary: prefix of a record word", IssueMisheardWord, "aresi", "Areci", pass, recs, EvidenceNone},
		{"word boundary: longer than the record word", IssueMisheardWord, "holevos", "Holevos", pass, recs, EvidenceNone},
		{"stopwords only", IssueHomophone, "add", "and", pass, recs, EvidenceNone},
		{"short non-stopword only", IssueHomophone, "rye", "Ray", pass, recs, EvidenceNone},
		{"shared context words are not new", IssueMisheardWord, "the dosh at", "the dish at", pass, recs, EvidenceASINVerbatim},
		{"no record", IssueMisheardProperNoun, "auto sebo", "Arecibo", pass, nil, EvidenceNone},
		{"rung-0 failure has none", IssueMisheardProperNoun, "auto sebo", "Arecibo", Verdict{Reason: ReasonNotSoundAlike}, recs, EvidenceNone},
		{"repeated_text is exact_repeat", IssueRepeatedText, "the the cat", "the cat", pass, nil, EvidenceExactRepeat},
		{"capped type still reports evidence", IssueDroppedWord, "the dish", "the Arecibo dish", pass, recs, EvidenceASINVerbatim},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Candidate{IssueType: tc.issue, Original: tc.orig, Replacement: tc.repl}
			if got := TextEvidence(c, tc.verdict, tc.sentences); got != tc.want {
				t.Errorf("TextEvidence = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDecodeErrorDegradesPerBook: db.GetBookRecords no longer fails a batch
// for one book whose chapters JSON does not decode — it returns that record
// with DecodeError set and Chapters nil. Decide then reads it without its
// chapters: that book loses the asin_verbatim evidence only its chapter
// titles carried, and never gains a match it would not have had intact,
// while another book in the same batch is unaffected.
func TestDecodeErrorDegradesPerBook(t *testing.T) {
	intact := hailMary()
	intact.Chapters = append(intact.Chapters, metaprovider.Chapter{Title: "Petrova Line"})
	other := &db.BookRecord{
		ASIN: "B002V1OF70", Title: "Children of Dune", Author: "Frank Herbert",
		Chapters:    []metaprovider.Chapter{{Title: "Sietch Tabr"}},
		Description: "<p>Ghanima and Leto face Alia.</p>",
	}
	// The batch as GetBookRecords returns it after a decode error on the
	// first book: same row, chapters dropped, the error recorded.
	degraded := *intact
	degraded.Chapters, degraded.DecodeError = nil, "chapters: unexpected end of JSON input"
	batch := map[string]db.BookRecord{"/b/PHM": degraded, "/b/Dune": *other}

	pass := Verdict{Pass: true}
	evidence := func(r db.BookRecord, orig, repl string) string {
		return TextEvidence(Candidate{IssueType: IssueMisheardProperNoun, Original: orig, Replacement: repl}, pass, RecordSentences(&r))
	}
	tests := []struct {
		name, dir, orig, repl string
		intact, degraded      string // evidence with the intact record vs the batch's record
	}{
		{"chapter-only name is lost", "/b/PHM", "pet rova line", "Petrova Line", EvidenceASINVerbatim, EvidenceNone},
		{"name also in the description survives", "/b/PHM", "auto sebo", "Arecibo", EvidenceASINVerbatim, EvidenceASINVerbatim},
		{"header field survives", "/b/PHM", "ray porta", "Ray Porter", EvidenceASINVerbatim, EvidenceASINVerbatim},
		{"no match stays none", "/b/PHM", "holovo", "Holova", EvidenceNone, EvidenceNone},
		{"other book's chapter is unaffected", "/b/Dune", "see etch tab", "Sietch Tabr", EvidenceASINVerbatim, EvidenceASINVerbatim},
		{"other book's description is unaffected", "/b/Dune", "galena", "Ghanima", EvidenceASINVerbatim, EvidenceASINVerbatim},
	}
	full := map[string]*db.BookRecord{"/b/PHM": intact, "/b/Dune": other}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidence(*full[tc.dir], tc.orig, tc.repl); got != tc.intact {
				t.Errorf("intact record: %s, want %s", got, tc.intact)
			}
			got := evidence(batch[tc.dir], tc.orig, tc.repl)
			if got != tc.degraded {
				t.Errorf("batch record: %s, want %s", got, tc.degraded)
			}
			if got == EvidenceASINVerbatim && tc.intact != EvidenceASINVerbatim {
				t.Error("a degraded record gained a match")
			}
		})
	}
	if batch["/b/Dune"].DecodeError != "" || batch["/b/PHM"].DecodeError == "" {
		t.Error("the decode error must stay on its own book")
	}
}
