package asrpilot

import (
	"errors"
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// VoteRule names this vote procedure; it is recorded in every vote's detail
// so a later decide recipe can tell votes of different rules apart. Bump it
// on any change that can change a vote.
const VoteRule = "asr_vote@v1"

// Votes (finding_asr_votes.vote).
const (
	VoteHeardReplacement = "heard_replacement"
	VoteHeardOriginal    = "heard_original"
	VoteNeither          = "neither"
	VoteUnmappable       = "unmappable"
)

// Vote parameters (part of VoteRule).
const (
	// TimedPad is the context, in seconds each side of the span, read from a
	// pilot with word timestamps and from the production words to align it
	// against.
	TimedPad = 3.0
	// SegmentPad widens the span when picking the segments of a pilot
	// without word timestamps, so a span at a window edge takes both windows.
	SegmentPad = 0.5
	// MatchSim is the normalised similarity at which heard text counts as
	// one of the two readings, and MatchMargin how far ahead of the other
	// reading it must be.
	MatchSim    = 0.8
	MatchMargin = 0.1
	// maxAligned caps the alignment columns kept in a vote's detail.
	maxAligned = 40
	// segmentTimeSlack absorbs float noise matching a segment's start to the
	// chunk's start_sec (decide's segmentContext uses the same).
	segmentTimeSlack = 0.001
)

// Finding is a held finding with the chunk it was judged against.
type Finding struct {
	OriginalText string
	Replacement  string
	// ChunkText is the chunk's pristine text, COALESCE(source_text, text);
	// ChunkSHA256 the hash recorded with the finding ("" = legacy, unchecked).
	ChunkText     string
	ChunkSHA256   string
	ChunkFound    bool
	ChunkStartSec float64
	// AnchorOffset (rune index, as the judge reported it) and
	// AnchorOccurrence are -1 when unknown.
	AnchorOffset     int
	AnchorOccurrence int
}

// Track is a production transcript prepared for voting.
type Track struct {
	Segments []db.Segment
	Tokens   []Token
	segFirst []int // first token index of each segment (len(Segments)+1 entries)
}

// NewTrack tokenises a production transcript once for all its findings.
func NewTrack(segs []db.Segment) *Track {
	t := &Track{Segments: segs, Tokens: ProductionTokens(segs)}
	t.segFirst = make([]int, len(segs)+1)
	k := 0
	for si := range segs {
		for k < len(t.Tokens) && t.Tokens[k].Seg < si {
			k++
		}
		t.segFirst[si] = k
	}
	t.segFirst[len(segs)] = len(t.Tokens)
	return t
}

// Pilot is one pilot transcript prepared for voting.
type Pilot struct {
	Segments     []db.PilotSegment
	Tokens       []Token
	HasWordTimes bool
	// Cased: the model writes case (TextStats.HasCasing), so a case-only
	// correction can be voted on.
	Cased bool
}

// NewPilot tokenises a pilot transcript once for all its findings.
func NewPilot(segs []db.PilotSegment, hasWordTimes bool) *Pilot {
	p := &Pilot{Segments: segs, Tokens: PilotTokens(segs), HasWordTimes: hasWordTimes}
	var st TextStats
	st.Add(p.Tokens)
	p.Cased = st.HasCasing()
	return p
}

// Result is one vote.
type Result struct {
	Vote      string
	HeardText string
	// SpanStartSec/SpanEndSec are the span's production word times (nil when
	// the span could not be placed).
	SpanStartSec, SpanEndSec *float64
	Detail                   Detail
}

// Detail is the vote's finding_asr_votes.detail JSON.
type Detail struct {
	Rule string `json:"rule"`
	// Reason says why a vote is unmappable or neither.
	Reason string `json:"reason,omitempty"`
	// Anchor is how patch.Locate placed the span: "offset" when the judge's
	// anchor_offset held, "recovered" when the occurrence or unique-match
	// rung found it (the stored offset is wrong).
	Anchor string `json:"anchor,omitempty"`
	// Span is the production text of the span's words; ProdTimes "word" when
	// their times are word timestamps, "interpolated" when estimated.
	Span      string `json:"span,omitempty"`
	ProdTimes string `json:"prod_times,omitempty"`
	// Method: "time_align" (pilot word times + alignment), "time_only"
	// (pilot word times, alignment found no anchor), "segment_align" (pilot
	// segment times + alignment).
	Method string `json:"method,omitempty"`
	// WindowStart/WindowEnd bound the pilot tokens considered.
	WindowStart float64 `json:"window_start,omitempty"`
	WindowEnd   float64 `json:"window_end,omitempty"`
	// Anchored says which sides of the span an agreed word anchored:
	// both, left, right or none.
	Anchored string `json:"anchored,omitempty"`
	// Aligned is the alignment around the span: [production, pilot] per
	// column, "" for a gap; at most 40 columns.
	Aligned        [][2]string `json:"aligned,omitempty"`
	SimOriginal    *float64    `json:"sim_original,omitempty"`
	SimReplacement *float64    `json:"sim_replacement,omitempty"`
	CaseSensitive  bool        `json:"case_sensitive,omitempty"`
}

// Vote decides what the pilot heard at f's span:
//
//  1. Locate the span (patch.Locate: the judge's rune offset if it holds,
//     else the occurrence, else a unique match — never the raw offset alone)
//     in the chunk's pristine text, verified against the recorded chunk hash.
//  2. Map it to production words: the chunk is whole segments joined by one
//     space, so the span's rune range picks segments, then fields within them.
//  3. Take the span's time window [t0, t1] from those words.
//  4. Read the pilot there: by word time (± TimedPad), or for a pilot without
//     word times the segments overlapping the window. Align those pilot
//     tokens with the production tokens of the same range; the pilot tokens
//     between the agreed words either side of the span are what it heard.
//  5. Compare normalised (or, for a case/punctuation-only correction and a
//     cased pilot, case-sensitively) with the original and the replacement.
//
// Anything that cannot be placed in time is VoteUnmappable with a reason.
func Vote(tr *Track, f Finding, p *Pilot) Result {
	res := Result{Detail: Detail{Rule: VoteRule}}
	unmappable := func(reason string) Result {
		res.Vote, res.Detail.Reason = VoteUnmappable, reason
		return res
	}
	if !f.ChunkFound {
		return unmappable("chunk_missing")
	}
	if f.ChunkSHA256 != "" && patch.ChunkHash(f.ChunkText) != f.ChunkSHA256 {
		return unmappable("chunk_changed")
	}
	span, err := patch.Locate(f.ChunkText, patch.Anchor{
		OriginalText: f.OriginalText, Offset: f.AnchorOffset, Occurrence: f.AnchorOccurrence,
	})
	if err != nil {
		if errors.Is(err, patch.ErrAnchorAmbiguous) {
			return unmappable("anchor_ambiguous")
		}
		return unmappable("anchor_not_found")
	}
	res.Detail.Anchor = "recovered"
	if span.Start == f.AnchorOffset {
		res.Detail.Anchor = "offset"
	}

	first, last, reason := tr.spanTokens(f, span)
	if reason != "" {
		return unmappable(reason)
	}
	toks := tr.Tokens
	t0, t1 := toks[first].Start, toks[last].End
	if !(t1 > t0) {
		return unmappable("no_timing")
	}
	res.SpanStartSec, res.SpanEndSec = &t0, &t1
	res.Detail.Span = rawText(toks[first : last+1])
	res.Detail.ProdTimes = "word"
	for _, t := range toks[first : last+1] {
		if !t.Timed {
			res.Detail.ProdTimes = "interpolated"
			break
		}
	}

	heard, reason := p.heard(toks, first, last, t0, t1, &res.Detail)
	if reason != "" {
		return unmappable(reason)
	}
	res.HeardText = rawText(heard)
	res.Vote = decideVote(heard, f.OriginalText, f.Replacement, p.Cased, &res.Detail)
	if res.Vote == VoteUnmappable && res.Detail.Reason == "" {
		res.Detail.Reason = "unmappable"
	}
	return res
}

// spanTokens maps a rune span of the chunk to the indices of the production
// tokens it touches, the way decide's segmentContext maps it to segments: the
// segments from the first one starting at the chunk's start_sec, joined by one
// space, must reproduce the chunk text exactly.
func (tr *Track) spanTokens(f Finding, span patch.Span) (first, last int, reason string) {
	segs := tr.Segments
	lo := -1
	for i, s := range segs {
		if s.Start >= f.ChunkStartSec-segmentTimeSlack {
			lo = i
			break
		}
	}
	if lo < 0 {
		return 0, 0, "chunk_not_in_segments"
	}
	runes := []rune(f.ChunkText)
	var b strings.Builder
	segStart := make([]int, 0, 8) // chunk rune offset of segs[lo+k]
	n, hi := 0, lo
	for ; hi < len(segs) && n < len(runes); hi++ {
		if hi > lo {
			n++
			b.WriteByte(' ')
		}
		segStart = append(segStart, n)
		n += len([]rune(segs[hi].Text))
		b.WriteString(segs[hi].Text)
	}
	if n != len(runes) || b.String() != f.ChunkText {
		return 0, 0, "segments_do_not_reproduce_chunk"
	}
	first, last = -1, -1
	for si := lo; si < hi; si++ {
		base := segStart[si-lo]
		for i := tr.segFirst[si]; i < tr.segFirst[si+1]; i++ {
			t := tr.Tokens[i]
			if base+t.RuneStart < span.End && base+t.RuneEnd > span.Start {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
	}
	if first < 0 {
		return 0, 0, "span_has_no_words"
	}
	return first, last, ""
}

// heard returns the pilot tokens heard at production tokens [first, last]
// (times [t0, t1]), filling the window fields of d.
func (p *Pilot) heard(prod []Token, first, last int, t0, t1 float64, d *Detail) ([]Token, string) {
	var cand []Token
	var ws, we float64
	if p.HasWordTimes {
		d.Method = "time_align"
		ws, we = t0-TimedPad, t1+TimedPad
		covered := false
		for _, s := range p.Segments {
			if s.End > t0 && s.Start < t1 {
				covered = true
				break
			}
		}
		if !covered {
			return nil, "pilot_no_coverage"
		}
		for _, t := range p.Tokens {
			if t.End > ws && t.Start < we {
				cand = append(cand, t)
			}
		}
	} else {
		d.Method = "segment_align"
		sLo, sHi := -1, -1
		for i, s := range p.Segments {
			if s.End > t0-SegmentPad && s.Start < t1+SegmentPad {
				if sLo < 0 {
					sLo = i
				}
				sHi = i
			}
		}
		if sLo < 0 {
			return nil, "pilot_no_coverage"
		}
		ws, we = p.Segments[sLo].Start, p.Segments[sHi].End
		for _, t := range p.Tokens {
			if t.Seg >= sLo && t.Seg <= sHi {
				cand = append(cand, t)
			}
		}
	}
	d.WindowStart, d.WindowEnd = ws, we

	// The production tokens of the same window, always including the span.
	wlo, whi := first, last
	for wlo > 0 && prod[wlo-1].End > ws {
		wlo--
	}
	for whi < len(prod)-1 && prod[whi+1].Start < we {
		whi++
	}
	window := prod[wlo : whi+1]
	if len(window) > MaxAlignTokens || len(cand) > MaxAlignTokens {
		return nil, "window_too_large"
	}
	if len(cand) == 0 {
		d.Anchored = "none"
		return nil, ""
	}
	pn, qn := norms(window), norms(cand)
	pairs := Align(pn, qn)
	a, b := first-wlo, last-wlo
	lo, hi, anchored := gap(pairs, a, b, pn, qn)
	d.Anchored = anchored
	d.Aligned = alignedDetail(pairs[lo+1:hi], window, cand)

	if anchored == "none" && p.HasWordTimes {
		// No agreed neighbour: fall back to the pilot's own word times.
		d.Method = "time_only"
		var out []Token
		for _, t := range cand {
			if m := t.Mid(); m >= t0 && m <= t1 {
				out = append(out, t)
			}
		}
		return out, ""
	}
	var out []Token
	for _, pr := range pairs[lo+1 : hi] {
		if pr.B >= 0 {
			out = append(out, cand[pr.B])
		}
	}
	return out, ""
}

// gap finds the alignment columns (lo, hi) — exclusive — that hold the span's
// production tokens [a, b]: lo is the last exact match left of the span (the
// left anchor) and hi the first exact match right of it. A side with no
// anchor is bounded by the span's own columns instead.
func gap(pairs []Pair, a, b int, pn, qn []string) (lo, hi int, anchored string) {
	lo, hi = -1, len(pairs)
	left, right := false, false
	for k, pr := range pairs {
		if pr.A >= 0 && pr.A < a && pr.B >= 0 && pn[pr.A] == qn[pr.B] {
			lo, left = k, true
		}
	}
	for k := len(pairs) - 1; k >= 0; k-- {
		pr := pairs[k]
		if pr.A > b && pr.B >= 0 && pn[pr.A] == qn[pr.B] {
			hi, right = k, true
		}
	}
	if !left {
		for k, pr := range pairs {
			if pr.A >= a {
				lo = k - 1
				break
			}
		}
	}
	if !right {
		for k := len(pairs) - 1; k >= 0; k-- {
			if pr := pairs[k]; pr.A >= 0 && pr.A <= b {
				hi = k + 1
				break
			}
		}
	}
	switch {
	case left && right:
		anchored = "both"
	case left:
		anchored = "left"
	case right:
		anchored = "right"
	default:
		anchored = "none"
	}
	return lo, hi, anchored
}

func alignedDetail(cols []Pair, a, b []Token) [][2]string {
	if len(cols) > maxAligned {
		cols = cols[:maxAligned]
	}
	out := make([][2]string, 0, len(cols))
	for _, c := range cols {
		var x [2]string
		if c.A >= 0 {
			x[0] = a[c.A].Norm
		}
		if c.B >= 0 {
			x[1] = b[c.B].Norm
		}
		out = append(out, x)
	}
	return out
}

// decideVote compares what was heard with the two readings.
func decideVote(heard []Token, original, replacement string, cased bool, d *Detail) string {
	no, nr := Normalize(original), Normalize(replacement)
	if no == nr {
		// A case/punctuation-only correction: normalising erases it.
		if !cased {
			d.Reason = "case_only_uncased_pilot"
			return VoteUnmappable
		}
		d.CaseSensitive = true
		h := rawText(heard)
		form := keepCase
		if keepCase(original) == keepCase(replacement) {
			form = collapseSpace // punctuation-only: compare as written
		}
		switch fh := form(h); {
		case fh == "":
			d.Reason = "nothing_heard"
			return VoteNeither
		case fh == form(replacement):
			return VoteHeardReplacement
		case fh == form(original):
			return VoteHeardOriginal
		default:
			d.Reason = "heard_other"
			return VoteNeither
		}
	}

	hn := strings.Join(norms(heard), " ")
	if hn == "" {
		d.Reason = "nothing_heard"
		return VoteNeither
	}
	if hn == nr {
		return VoteHeardReplacement
	}
	if hn == no {
		return VoteHeardOriginal
	}
	ch, cr, co := compact(hn), compact(nr), compact(no)
	if cr != co {
		// "bell rose" heard for "belrose": same letters, other spacing.
		if ch == cr {
			return VoteHeardReplacement
		}
		if ch == co {
			return VoteHeardOriginal
		}
	}
	sr, so := Similarity(ch, cr), Similarity(ch, co)
	d.SimReplacement, d.SimOriginal = &sr, &so
	switch {
	case sr >= MatchSim && sr-so >= MatchMargin:
		return VoteHeardReplacement
	case so >= MatchSim && so-sr >= MatchMargin:
		return VoteHeardOriginal
	case sr >= MatchSim && so >= MatchSim:
		d.Reason = "no_margin"
	default:
		d.Reason = "heard_other"
	}
	return VoteNeither
}

func compact(s string) string { return strings.ReplaceAll(s, " ", "") }

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// keepCase strips punctuation and symbols and collapses whitespace, keeping
// case ("Gray." → "Gray").
func keepCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r), r == '-', unicode.Is(unicode.Pd, r):
			b.WriteByte(' ')
		case unicode.IsPunct(r), unicode.IsSymbol(r):
		default:
			b.WriteRune(r)
		}
	}
	return collapseSpace(b.String())
}
