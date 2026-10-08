// Package decide is the decide step: it turns the judge's proposed findings
// (CONTRACT §2.15) into decisions about which corrections to apply.
//
// Rung 0 is deterministic checks that need no model call, run before
// anything is asked of a model:
//
//   - the finding still describes the chunk revision it was judged against,
//     and its anchor resolves to exactly one span (patch.ChunkHash,
//     patch.Locate);
//   - the edit changes words, not just case, punctuation, hyphens or spacing;
//   - the edit has the shape its issue type claims: a substitution must sound
//     like what it replaces (phonetic.SoundAlike), a repeated_text fix must
//     remove an exact adjacent repeat, a dropped_word fix must insert one or
//     two words and change nothing else;
//   - per chunk, overlapping candidates are deduplicated, and a candidate that
//     overlaps a correction already accepted or applied is refused.
//
// A rung-0 failure is final for that finding under the current recipe. A
// rung-0 pass is NOT a decision to apply — it only makes a finding eligible for
// the later rungs. Rung 0 is pure: no database, no network, no clock.
//
// should_apply (CONTRACT §2.19) is the next rung: Evaluator.Evaluate builds
// the finding's context from its ASR segments (BuildContext), finds text
// evidence in the book's catalogue record (RecordSentences, Relevant,
// TextEvidence), asks the pinned decision model one question through
// internal/fn, and maps the answer to apply, hold or reject (Decide). It fails
// closed: any failure to get a usable answer is a retryable hold, never an
// apply. Nothing here persists a decision or changes a finding's state.
//
// DryRun (`earmark decide`) runs the whole step over a deterministic sample
// and builds a Report — outcomes, reasons, a p histogram, cost and a backlog
// projection — and, in calibration mode, agreement with decisions people
// already made. It writes only the decide recipe and the fn_calls log.
package decide
