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
