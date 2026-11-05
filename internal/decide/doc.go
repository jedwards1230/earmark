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
// The spelling pass (spelling.go, CONTRACT §2.19 "One spelling per book")
// runs after should_apply and before conflicts are ranked, in DryRun and
// Apply: it groups the book's live findings by the sound of their differing
// words (internal/spelling) and holds an apply or an audio-liftable hold that
// contradicts the book's other spellings (spelling_conflict) or targets a
// spelling other than the one the book established (spelling_off_established);
// a no_evidence hold whose exact pair an applied finding of the book
// established is applied (spelling_established). Findings the run itself
// rejects do not count toward a contradiction; a book over a read cap is
// skipped and counted. EstablishedSpellings exposes a book's
// strong spellings for a future context consumer; nothing feeds them to a
// model yet.
//
// DryRun (`earmark decide`) runs the whole step over a deterministic sample
// and builds a Report — outcomes, reasons, a p histogram, cost and a backlog
// projection — and, in calibration mode, agreement with decisions people
// already made. It writes only the decide recipe and the fn_calls log.
//
// Apply (`earmark decide --yes`) decides every finding in scope — proposed
// ones, and re-checks of another decide recipe's accepts — computing with no
// lock held and writing the decisions in short transactions through
// db.ApplyDecisions and db.ApplyRecheckDecisions. db.RevertDecisions undoes
// them.
//
// AudioVote (`earmark decide --audio-vote`) is a second, separately
// registered decide recipe: it re-runs rung 0 on held findings and decides
// them from the second-ASR pilot votes (AudioVoteRung.Evaluate), asking no
// model and writing only applies and rejects.
package decide
