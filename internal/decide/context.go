package decide

import (
	"strings"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// Context sizes, in words. They shape what the model sees, so they are recipe
// params (ShouldApplyParams).
const (
	// ContextWords is how many words each side of the span a sentence keeps.
	ContextWords = 25
	// NeighbourWords is how many words Before and After keep next to the
	// sentence: the nearest ones, which bear most on the span.
	NeighbourWords = 20
	// FallbackWords is the ±window around the span used when the segments do
	// not reproduce the chunk.
	FallbackWords = 25
)

// Span markers in the sentences shown to the model. Transcript text has no
// brackets (it is lowercase with no punctuation), so they are unambiguous.
const (
	markOpen  = "[["
	markClose = "]]"
)

// segmentTimeSlack absorbs float noise when matching segment times to a
// chunk's start_sec (both come from the same JSON numbers, so it is generous).
const segmentTimeSlack = 0.001

// ChunkWindow is the chunk a finding was judged against: its pristine text
// (COALESCE(source_text, text)) and its track-relative time window.
type ChunkWindow struct {
	Text     string
	StartSec float64
	EndSec   float64
}

// Context is the text around a finding shown to the decision model.
//
// "Sentence" means ASR segment: transcript text is lowercase with no
// punctuation, so there are no written sentences, and segments are already
// sentence-sized (split on pauses, capped in length). A chunk is whole
// segments joined by one space, which is how the segments covering the span
// are recovered.
type Context struct {
	// Original is the segment(s) containing the span, with the span marked
	// [[like this]]; Corrected is the same text with the replacement marked.
	Original, Corrected string
	// Before and After are the neighbouring segments. They may come from the
	// previous or next chunk — context only, never edited. Empty at the ends
	// of the transcript, and always empty on the fallback.
	Before, After string
	// Reconstructed is true when the segments reproduced the chunk text; false
	// means the ±FallbackWords window of the chunk was used instead.
	Reconstructed bool
}

// BuildContext builds the context of span (rune indices into chunk.Text) for
// a finding replacing it with replacement. segs is the transcript's full,
// ordered segment list.
//
// The segments starting at the chunk's start_sec are joined with " " until
// they cover the chunk; if that reproduces chunk.Text exactly, the sentence is
// the segment(s) the span falls in and Before/After are their neighbours.
// Otherwise — no segments, a chunk rebuilt by another chunker, a span that
// does not fit — it falls back to a ±FallbackWords window of the chunk. The
// sentence keeps ContextWords words on each side of the span, Before and After
// their NeighbourWords words nearest it.
func BuildContext(segs []db.Segment, chunk ChunkWindow, span patch.Span, replacement string) Context {
	runes := []rune(chunk.Text)
	if span.Start < 0 || span.End > len(runes) || span.Start >= span.End {
		return Context{}
	}
	if c, ok := segmentContext(segs, chunk, runes, span, replacement); ok {
		return c
	}
	before, spanText, after := string(runes[:span.Start]), string(runes[span.Start:span.End]), string(runes[span.End:])
	pre, post := lastWords(before, FallbackWords), firstWords(after, FallbackWords)
	return Context{
		Original:  pre + markOpen + spanText + markClose + post,
		Corrected: pre + markOpen + replacement + markClose + post,
	}
}

func segmentContext(segs []db.Segment, chunk ChunkWindow, runes []rune, span patch.Span, replacement string) (Context, bool) {
	lo := -1
	for i, s := range segs {
		if s.Start >= chunk.StartSec-segmentTimeSlack {
			lo = i
			break
		}
	}
	if lo < 0 {
		return Context{}, false
	}
	// segs[lo+k] covers runes [starts[k], ends[k]) of the chunk; one space
	// separates consecutive segments.
	var starts, ends []int
	n, hi := 0, lo
	for ; hi < len(segs) && n < len(runes); hi++ {
		if hi > lo {
			n++
		}
		starts = append(starts, n)
		n += len([]rune(segs[hi].Text))
		ends = append(ends, n)
	}
	if n != len(runes) {
		return Context{}, false
	}
	var b strings.Builder
	for i := lo; i < hi; i++ {
		if i > lo {
			b.WriteByte(' ')
		}
		b.WriteString(segs[i].Text)
	}
	if b.String() != chunk.Text {
		return Context{}, false
	}

	// first/last: the segments (relative to lo) holding the span's first and
	// last runes.
	first, last := -1, -1
	for k := range starts {
		if first < 0 && span.Start < ends[k] {
			first = k
		}
		if span.End > starts[k] {
			last = k
		}
	}
	if first < 0 || last < first || span.Start < starts[first] || span.End > ends[last] {
		return Context{}, false
	}
	sentStart, sentEnd := starts[first], ends[last]
	pre := lastWords(string(runes[sentStart:span.Start]), ContextWords)
	post := firstWords(string(runes[span.End:sentEnd]), ContextWords)
	spanText := string(runes[span.Start:span.End])
	c := Context{
		Original:      pre + markOpen + spanText + markClose + post,
		Corrected:     pre + markOpen + replacement + markClose + post,
		Reconstructed: true,
	}
	if i := lo + first - 1; i >= 0 {
		c.Before = lastWords(segs[i].Text, NeighbourWords)
	}
	if i := lo + last + 1; i < len(segs) {
		c.After = firstWords(segs[i].Text, NeighbourWords)
	}
	return c, true
}

// lastWords keeps the last n words of s, preserving whether s ended in
// whitespace (so a span after it stays separated).
func lastWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return s
	}
	out := strings.Join(f[len(f)-n:], " ")
	if strings.TrimRight(s, " \t\n") != s {
		out += " "
	}
	return out
}

// firstWords keeps the first n words of s, preserving whether s started with
// whitespace.
func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return s
	}
	out := strings.Join(f[:n], " ")
	if strings.TrimLeft(s, " \t\n") != s {
		out = " " + out
	}
	return out
}
