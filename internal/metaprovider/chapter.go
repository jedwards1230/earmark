package metaprovider

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
