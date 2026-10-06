package patch

import (
	"strings"
	"unicode"
)

// Re-anchoring (CONTRACT §2.17 "Re-anchoring", `earmark reanchor`).
//
// A finding is anchored to ONE revision of ONE chunk: chunk_id, the chunk's
// pristine-text hash and a rune offset/occurrence. Re-chunking (requeue
// --reembed, a chunk-size change) regenerates every chunk under new ids and
// shifted boundaries, so every finding recorded before it describes text the
// projection no longer has — and Replay retires it as stale, which is terminal.
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
	// OutcomeUnique — exactly one word-bounded match in the chunk the finding
	// names (same chunk_index). Re-anchored in place.
	OutcomeUnique = "unique"
	// OutcomeMoved — no match in the named chunk, exactly one in the chunks
	// covering the finding's time window or, failing that, in the whole
	// transcript. Re-anchored to that chunk.
	OutcomeMoved = "moved"
	// OutcomeAmbiguous — more than one candidate at the first tier that has
	// any. Never guessed.
	OutcomeAmbiguous = "ambiguous"
	// OutcomeNone — the span is nowhere in the transcript's current text.
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
	// StartSec / EndSec are the recorded chunk's audio window. Audio time does
	// not move when the text is re-chunked, so this is what finds the finding's
	// new chunk when its index has drifted.
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
	// Candidates is how many word-bounded matches the deciding tier saw
	// (0 for none, ≥2 for ambiguous).
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
// The search is tiered, most specific first, and the first tier with ANY match
// decides — a match there is never second-guessed by a wider tier:
//
//  1. the chunk the finding names (same chunk_index), while it still covers
//     the finding's audio window or is unchanged since the judge saw it;
//  2. the other chunks overlapping the finding's [StartSec, EndSec) window;
//  3. every chunk of the transcript.
//
// One match at the deciding tier re-anchors; two or more is ambiguous; none at
// any tier is OutcomeNone. Tiers 2 and 3 exist because re-chunking moves text
// between chunks, so they are skipped when the named chunk is provably
// unchanged (its pristine text still has the recorded hash). Matching is case-sensitive and word-bounded (see
// WordOccurrences): case-sensitive because replay splices the exact span, so a
// case-folded anchor would only go stale later.
func Reanchor(f ReanchorFinding, chunks []ReanchorChunk) ReanchorResult {
	if len(chunks) == 0 {
		return ReanchorResult{Outcome: OutcomePending}
	}

	var named *ReanchorChunk
	if f.ChunkIndex != nil {
		for i := range chunks {
			if chunks[i].Index == *f.ChunkIndex {
				named = &chunks[i]
				break
			}
		}
	}

	if strings.TrimSpace(f.OriginalText) == "" {
		return ReanchorResult{Outcome: OutcomeNone}
	}

	if named != nil && isAnchored(f, *named) {
		span, _ := Locate(named.Text, Anchor{OriginalText: f.OriginalText, Offset: f.Offset, Occurrence: f.Occurrence})
		return ReanchorResult{
			Outcome: OutcomeAnchored, Chunk: *named,
			Offset: span.Start, Occurrence: f.Occurrence, Hash: f.ChunkHash,
		}
	}

	// Tier 1: the named chunk — but only while it is still the text the judge
	// saw. It is if it is byte-for-byte unchanged (the recorded hash), or if it
	// still covers the finding's audio window. After a re-chunk the same index
	// can name different audio, and a match there is the same words somewhere
	// else, not the span the judge flagged.
	if named != nil {
		unchanged := f.ChunkHash != "" && ChunkHash(named.Text) == f.ChunkHash
		if unchanged || !f.hasWindow() || f.overlaps(*named) {
			if starts := WordOccurrences(named.Text, f.OriginalText); len(starts) > 0 {
				return decide(OutcomeUnique, f.OriginalText, []hit{{chunk: *named, starts: starts}})
			}
		}
		// An unchanged chunk's text did not move: the span was never in it (a
		// misquoted span), and the same words elsewhere are a different place.
		if unchanged {
			return ReanchorResult{Outcome: OutcomeNone}
		}
	}

	// Tier 2: other chunks overlapping the finding's audio window.
	var window []hit
	for _, c := range chunks {
		if named != nil && c.Index == named.Index {
			continue
		}
		if f.overlaps(c) {
			if starts := WordOccurrences(c.Text, f.OriginalText); len(starts) > 0 {
				window = append(window, hit{chunk: c, starts: starts})
			}
		}
	}
	if len(window) > 0 {
		return decide(OutcomeMoved, f.OriginalText, window)
	}

	// Tier 3: the whole transcript.
	var all []hit
	for _, c := range chunks {
		if starts := WordOccurrences(c.Text, f.OriginalText); len(starts) > 0 {
			all = append(all, hit{chunk: c, starts: starts})
		}
	}
	if len(all) > 0 {
		return decide(OutcomeMoved, f.OriginalText, all)
	}
	return ReanchorResult{Outcome: OutcomeNone}
}

// hasWindow reports whether the finding carries a usable audio window. Every
// live row does; a degenerate one only loses the window check, never a tier.
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

// decide turns a tier's hits into a result: exactly one match re-anchors (with
// the given outcome), anything more is ambiguous.
func decide(outcome, span string, hits []hit) ReanchorResult {
	total := 0
	for _, h := range hits {
		total += len(h.starts)
	}
	if total != 1 {
		return ReanchorResult{Outcome: OutcomeAmbiguous, Candidates: total}
	}
	h := hits[0]
	start := h.starts[0]
	return ReanchorResult{
		Outcome:    outcome,
		Candidates: 1,
		Chunk:      h.chunk,
		Offset:     start,
		Occurrence: substringOccurrenceIndex(h.chunk.Text, span, start),
		Hash:       ChunkHash(h.chunk.Text),
	}
}

// isAnchored reports whether a finding's EXISTING anchor still resolves against
// the named chunk: it was recorded against this very chunk row, the chunk's
// pristine text still hashes to what was recorded, and Locate places the span.
// Exactly the checks Replay would make, so "already" means "would replay".
func isAnchored(f ReanchorFinding, c ReanchorChunk) bool {
	if f.ChunkHash == "" || f.ChunkID == "" || f.ChunkID != c.ID {
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
