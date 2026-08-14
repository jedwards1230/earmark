package metaprovider

import "math"

// chapterForSec returns the chapter that contains the given second offset.
// A chapter contains sec when chapter.StartSec <= sec, and we pick the last
// (highest-index) chapter that satisfies that condition (i.e. we track the
// best-so-far as we iterate).
//
// After selecting the best candidate we verify that sec does not overshoot
// the chapter's declared end by more than a small epsilon (0.001 s) to guard
// against obviously out-of-range values.  When EndSec is 0 the check is
// skipped, which handles providers that omit EndSec on the final chapter.
//
// When chapters is empty, or sec is before the first chapter's StartSec, or
// sec is past the last chapter's EndSec+epsilon, ok is false and the zero
// values for index and title are returned.
// ChapterForSec is exported for use from other packages (e.g. internal/db).
// The unexported alias chapterForSec is kept for white-box tests in this package.
//
// sec must be BOOK-absolute. Callers holding a track-relative offset (every
// transcript_chunks.start_sec is one) must use ChapterForTrackSec instead —
// passing track time here silently resolves every track to the book's opening
// chapters.
func ChapterForSec(chapters []Chapter, sec float64) (index int, title string, ok bool) {
	return chapterForSec(chapters, sec)
}

func chapterForSec(chapters []Chapter, sec float64) (index int, title string, ok bool) {
	if len(chapters) == 0 {
		return 0, "", false
	}
	best := -1
	for i, c := range chapters {
		if sec >= c.StartSec {
			best = i
		}
	}
	if best < 0 {
		return 0, "", false
	}
	c := chapters[best]
	// Reject if clearly past the chapter's end (allow a small epsilon for
	// floating-point boundary exactly at EndSec).
	if c.EndSec > 0 && sec > c.EndSec+0.001 {
		return 0, "", false
	}
	return c.Index, c.Title, true
}

// ChapterForTrackSec maps a TRACK-RELATIVE second offset onto a BOOK-ABSOLUTE
// chapter list.
//
// The two time bases differ for multi-track books and this is load-bearing:
//   - transcript_chunks.start_sec is relative to the start of its own audio file
//     (one ASR transcript per track), so track 7 restarts at ~0.
//   - Provider chapter lists (e.g. Audiobookshelf media.chapters) are absolute
//     over the whole book, i.e. over all tracks concatenated in play order.
//
// Passing a track-relative second straight into ChapterForSec therefore resolves
// every track to the book's opening chapters. This function first adds the
// track's book offset — the sum of the durations of all preceding tracks — and
// only then does the chapter lookup.
//
// trackDurations must be the per-track durations in play order and trackIdx the
// index of the chunk's own track within it. trackIdx == 0 yields an offset of 0,
// so single-file books behave exactly as a plain ChapterForSec call.
//
// ok is false when the offset cannot be established (trackIdx out of range, or a
// preceding track with a non-positive/NaN/Inf duration) — callers must then leave
// the chapter fields unset. A missing chapter label is honest; a plausible but
// wrong chapter title is worse than none.
func ChapterForTrackSec(chapters []Chapter, trackDurations []float64, trackIdx int, trackSec float64) (index int, title string, ok bool) {
	offset, ok := bookOffsetSec(trackDurations, trackIdx)
	if !ok {
		return 0, "", false
	}
	return chapterForSec(chapters, offset+trackSec)
}

// bookOffsetSec returns the book-absolute start time of track trackIdx: the sum
// of the durations of every preceding track. ok is false when trackIdx is out of
// range or any preceding duration is unusable (<= 0, NaN, or Inf) — an unknown
// duration makes every later track's offset unknowable, and guessing would
// mislabel chapters.
func bookOffsetSec(trackDurations []float64, trackIdx int) (offset float64, ok bool) {
	if trackIdx < 0 || trackIdx >= len(trackDurations) {
		return 0, false
	}
	for _, d := range trackDurations[:trackIdx] {
		if d <= 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			return 0, false
		}
		offset += d
	}
	return offset, true
}
