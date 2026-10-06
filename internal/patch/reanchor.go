package patch

import (
	"slices"
	"strings"
	"unicode"
)

// Re-anchoring (CONTRACT §2.17 "Re-anchoring", `earmark reanchor`).
//
// A finding is anchored to ONE revision of ONE chunk: chunk_id, the chunk's
// pristine-text hash and a rune offset/occurrence. Re-chunking (requeue
// --reembed, a chunk-size change) regenerates every chunk: the same index can
// now hold different text (sometimes under the same deterministic id,
// sometimes a new one), so every finding recorded before it describes text the
// projection no longer has — and Replay retires it as stale, which is
// terminal. The HASH, not the id, is what proves an anchor current.
//
// Reanchor looks for the finding's span in the transcript's CURRENT pristine
// chunks and, when exactly one place fits, records a fresh anchor there. It
// refuses rather than guesses: two equally good places is an ambiguity, not a
// coin toss, because anchoring a correction to the wrong occurrence is corpus
// corruption that reads as a successful edit.

// Reanchor outcomes. Persisted values are the unanchorable reasons below; these
// are the classification the CLI reports.
const (
	// OutcomeAnchored — the existing anchor already resolves against the
	// current chunk (same chunk id, hash match, Locate succeeds). Nothing to do.
	OutcomeAnchored = "already"
	// OutcomeUnique — the one candidate is in the chunk the finding names
	// (same chunk_index). Re-anchored in place.
	OutcomeUnique = "unique"
	// OutcomeMoved — the one candidate is in another chunk covering the
	// finding's judged audio window. Re-anchored to that chunk.
	OutcomeMoved = "moved"
	// OutcomeAmbiguous — more than one candidate. Never guessed.
	OutcomeAmbiguous = "ambiguous"
	// OutcomeNone — no candidate that a single chunk can hold: the span is not
	// in the judged text, or its only occurrence straddles a chunk boundary.
	OutcomeNone = "none"
	// OutcomePending — the transcript has no chunks right now (mid re-embed).
	// Not judged at all: "no text yet" is not "the span is gone".
	OutcomePending = "pending"
)

// Unanchorable reasons, persisted verbatim into
// transcript_findings.unanchorable_reason. Deliberately the same words as the
// matching stale reasons so one vocabulary covers "why this can't be placed".
const (
	UnanchorableNotFound  = StaleReasonAnchorNotFound
	UnanchorableAmbiguous = StaleReasonAnchorAmbiguous
)

// ReanchorChunk is one current chunk of a transcript. Text is the PRISTINE
// text — COALESCE(source_text, text) — the coordinate system every anchor and
// hash is recorded against, never the corrected surface.
type ReanchorChunk struct {
	ID       string
	Index    int
	StartSec float64
	EndSec   float64
	Text     string
}

// ReanchorFinding is what Reanchor needs from a finding row.
type ReanchorFinding struct {
	OriginalText string
	// ChunkID / ChunkIndex name the chunk the finding was recorded against.
	// Either may be unset (ChunkIndex nil, ChunkID "") on very old rows.
	ChunkID    string
	ChunkIndex *int
	// StartSec / EndSec are the audio window of the chunk the judge was shown.
	// Audio time does not move when the text is re-chunked, so the judged
	// occurrence always lies in the current chunks overlapping this window.
	// Re-anchoring never rewrites it: it is the evidence the next re-anchor
	// needs.
	StartSec float64
	EndSec   float64
	// The existing anchor. Empty hash / negative offset+occurrence = none.
	ChunkHash  string
	Offset     int
	Occurrence int
}

// ReanchorResult is the classification of one finding plus, for
// OutcomeUnique/OutcomeMoved, the new anchor.
type ReanchorResult struct {
	Outcome string
	// Candidates is how many word-bounded matches the deciding search saw
	// (0 for none, ≥2 for ambiguous; a boundary-straddling one counts).
	Candidates int
	// Chunk is the chunk the anchor now points at (OutcomeUnique/Moved/Anchored).
	Chunk ReanchorChunk
	// Offset is the rune index of the span in Chunk.Text; Occurrence is its
	// index among Locate's (plain substring) occurrences, so Locate's
	// offset-then-occurrence ladder lands on exactly this span.
	Offset     int
	Occurrence int
	// Hash is ChunkHash(Chunk.Text).
	Hash string
}

// Reanchor classifies one finding against a transcript's current chunks and,
// where it can, returns a fresh anchor. Pure: no I/O, deterministic for a given
// input regardless of chunk order.
//
// Where it looks depends on what is known about the judged text:
//
//  1. The named chunk is UNCHANGED (its pristine text still has the recorded
//     hash): it IS the judged text, so it alone decides. A span missing from it
//     was misquoted; the same words elsewhere are a different place.
//  2. Otherwise, with an audio window: the judged occurrence lies in the
//     current chunks overlapping [StartSec, EndSec), and nowhere else. Those
//     chunks are joined in chunk_index order with " " (how the worker joins
//     segments) and searched as ONE text, so an occurrence straddling a chunk
//     boundary is still a candidate. Exactly one candidate that lies wholly
//     inside one chunk re-anchors there; more is ambiguous; a lone straddler
//     cannot be anchored to any one chunk and is none. The named chunk gets no
//     preference: after a re-chunk it is just one of the window's chunks.
//  3. No usable window (none on the live library): the named chunk, then the
//     whole transcript.
//
// Matching is case-sensitive and word-bounded (see WordOccurrences):
// case-sensitive because replay splices the exact span, so a case-folded
// anchor would only go stale later.
func Reanchor(f ReanchorFinding, chunks []ReanchorChunk) ReanchorResult {
	if len(chunks) == 0 {
		return ReanchorResult{Outcome: OutcomePending}
	}
	if strings.TrimSpace(f.OriginalText) == "" {
		return ReanchorResult{Outcome: OutcomeNone}
	}

	named := f.namedChunk(chunks)

	if named != nil && isAnchored(f, *named) {
		span, _ := Locate(named.Text, Anchor{OriginalText: f.OriginalText, Offset: f.Offset, Occurrence: f.Occurrence})
		return ReanchorResult{
			Outcome: OutcomeAnchored, Chunk: *named,
			Offset: span.Start, Occurrence: f.Occurrence, Hash: f.ChunkHash,
		}
	}

	// 1. Unchanged named chunk: decisive.
	if named != nil && f.ChunkHash != "" && ChunkHash(named.Text) == f.ChunkHash {
		return decide(f, named, []hit{{chunk: *named, starts: WordOccurrences(named.Text, f.OriginalText)}})
	}

	// 2. The judged audio window.
	if f.hasWindow() {
		return f.searchWindow(named, chunks)
	}

	// 3. No window: named chunk, then the whole transcript.
	if named != nil {
		if starts := WordOccurrences(named.Text, f.OriginalText); len(starts) > 0 {
			return decide(f, named, []hit{{chunk: *named, starts: starts}})
		}
	}
	var all []hit
	for _, c := range chunks {
		if starts := WordOccurrences(c.Text, f.OriginalText); len(starts) > 0 {
			all = append(all, hit{chunk: c, starts: starts})
		}
	}
	return decide(f, named, all)
}

// searchWindow searches the chunks overlapping the finding's audio window as
// one text joined with " ", so a boundary-straddling occurrence is counted.
func (f ReanchorFinding) searchWindow(named *ReanchorChunk, chunks []ReanchorChunk) ReanchorResult {
	var window []ReanchorChunk
	for _, c := range chunks {
		if f.overlaps(c) {
			window = append(window, c)
		}
	}
	if len(window) == 0 {
		return ReanchorResult{Outcome: OutcomeNone}
	}
	slices.SortFunc(window, func(a, b ReanchorChunk) int { return a.Index - b.Index })

	texts := make([]string, len(window))
	bases := make([]int, len(window)) // rune offset of each chunk in the joined text
	base := 0
	for i, c := range window {
		texts[i] = c.Text
		bases[i] = base
		base += len([]rune(c.Text)) + 1 // + the " " separator
	}
	joined := WordOccurrences(strings.Join(texts, " "), f.OriginalText)
	switch {
	case len(joined) == 0:
		return ReanchorResult{Outcome: OutcomeNone}
	case len(joined) > 1:
		return ReanchorResult{Outcome: OutcomeAmbiguous, Candidates: len(joined)}
	}

	// One candidate: it must lie wholly inside one chunk to be anchorable.
	start, n := joined[0], len([]rune(f.OriginalText))
	for i, c := range window {
		local := start - bases[i]
		if local >= 0 && local+n <= len([]rune(c.Text)) {
			return decide(f, named, []hit{{chunk: c, starts: []int{local}}})
		}
	}
	return ReanchorResult{Outcome: OutcomeNone, Candidates: 1} // straddles a boundary
}

// hasWindow reports whether the finding carries a usable audio window. Every
// live row does.
func (f ReanchorFinding) hasWindow() bool { return f.EndSec > f.StartSec }

// overlaps reports whether a chunk covers any of the finding's audio window.
func (f ReanchorFinding) overlaps(c ReanchorChunk) bool {
	return c.StartSec < f.EndSec && c.EndSec > f.StartSec
}

// hit is one chunk's word-bounded matches.
type hit struct {
	chunk  ReanchorChunk
	starts []int
}

// decide turns a set of hits into a result: exactly one match re-anchors
// (unique when it is in the named chunk, moved otherwise), more is ambiguous,
// none is none.
func decide(f ReanchorFinding, named *ReanchorChunk, hits []hit) ReanchorResult {
	total := 0
	for _, h := range hits {
		total += len(h.starts)
	}
	switch {
	case total == 0:
		return ReanchorResult{Outcome: OutcomeNone}
	case total > 1:
		return ReanchorResult{Outcome: OutcomeAmbiguous, Candidates: total}
	}
	var h hit
	for _, x := range hits {
		if len(x.starts) == 1 {
			h = x
		}
	}
	outcome := OutcomeMoved
	if named != nil && h.chunk.Index == named.Index {
		outcome = OutcomeUnique
	}
	start := h.starts[0]
	return ReanchorResult{
		Outcome:    outcome,
		Candidates: 1,
		Chunk:      h.chunk,
		Offset:     start,
		Occurrence: substringOccurrenceIndex(h.chunk.Text, f.OriginalText, start),
		Hash:       ChunkHash(h.chunk.Text),
	}
}

// namedChunk resolves the chunk a finding names with the repo-wide finding
// address (findingChunkAddressDoc in internal/db): by chunk_index whenever the
// finding recorded one, by chunk_id only when it did not. The overlay is keyed
// by chunk_index, so this is the chunk replay would splice into. nil if the
// address names no current chunk.
func (f ReanchorFinding) namedChunk(chunks []ReanchorChunk) *ReanchorChunk {
	for i := range chunks {
		if f.ChunkIndex != nil {
			if chunks[i].Index == *f.ChunkIndex {
				return &chunks[i]
			}
		} else if f.ChunkID != "" && chunks[i].ID == f.ChunkID {
			return &chunks[i]
		}
	}
	return nil
}

// isAnchored reports whether a finding's EXISTING anchor still resolves against
// the named chunk (namedChunk): the chunk's pristine text still hashes to what
// was recorded and Locate places the span. Exactly the checks Replay makes, so
// "already" means "would replay". The chunk's id is deliberately not compared:
// findings are addressed by chunk_index first, and most legacy judge findings
// carry a chunk_id that never named the row at their index — the hash, not the
// id, proves the anchor current.
func isAnchored(f ReanchorFinding, c ReanchorChunk) bool {
	if f.ChunkHash == "" {
		return false
	}
	if ChunkHash(c.Text) != f.ChunkHash {
		return false
	}
	_, err := Locate(c.Text, Anchor{OriginalText: f.OriginalText, Offset: f.Offset, Occurrence: f.Occurrence})
	return err == nil
}

// substringOccurrenceIndex returns the index of the occurrence starting at
// rune `start` among ALL plain-substring occurrences of span — the numbering
// Locate's occurrence step uses. Recording it this way keeps the anchor
// consistent with the replay ladder even when the word-bounded match is not
// the first raw substring hit (e.g. "the" inside "other the").
func substringOccurrenceIndex(text, span string, start int) int {
	for i, s := range occurrences([]rune(text), []rune(span)) {
		if s == start {
			return i
		}
	}
	return -1 // unreachable: every word-bounded match is a substring match
}

// WordOccurrences returns the rune start of every case-sensitive occurrence of
// span in text that sits on word boundaries, including overlapping ones.
//
// The boundary rule applies only at an edge where the span itself has a word
// rune: a span starting with a letter must not be preceded by one ("ganema"
// does not match inside "proganema"), but a span starting with punctuation
// ("...ganema") has no left-hand word to protect. A word rune is a Unicode
// letter, digit or underscore — on ASCII text (all live chunk text) exactly
// Postgres' C-locale [[:alnum:]_], which is what lets the server-side survival
// query mirror this function.
//
// Overlapping matches are kept ("ha ha" occurs twice in "ha ha ha"): two
// candidate placements are an ambiguity whether or not they overlap.
func WordOccurrences(text, span string) []int {
	if span == "" {
		return nil
	}
	runes := []rune(text)
	target := []rune(span)
	n := len(target)
	leftWord := isWordRune(target[0])
	rightWord := isWordRune(target[n-1])

	var out []int
	for _, s := range occurrences(runes, target) {
		if leftWord && s > 0 && isWordRune(runes[s-1]) {
			continue
		}
		if rightWord && s+n < len(runes) && isWordRune(runes[s+n]) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
