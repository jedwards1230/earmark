// Package predict computes an empirical, arithmetic (non-ML) ETA for the
// transcription pipeline from persisted history (CONTRACT §4 / the auditability
// plan). It is a PURE package: all inputs are passed in (the DB query that
// gathers them lives in internal/db), so the model is fully unit-testable with no
// DB, no clock, and no HTTP.
//
// Two estimates are produced:
//
//   - Work time — busy-seconds to process the remaining chunks at the observed
//     per-stage rates (transcribe + embed [+ eval]). This is what the old naive
//     dashboard ETA approximated, badly.
//   - Calendar time — work time divided by the runner's availability fraction
//     (the share of wall-clock the GPU host is actually available to
//     transcribe, from runner_availability windows). When availability history
//     is thin/absent the calendar estimate is omitted and the result is LABELED
//     as work-time only — never a misleading calendar figure, never a
//     divide-by-zero.
package predict

import "math"

// Rates are the per-chunk processing rates (seconds per chunk) for each stage,
// derived as a median over a trailing window of done jobs (segmented by
// asr_family/runner_host upstream). A non-positive rate means "no data for this
// stage" and is treated as 0 contribution (with EvalKnown/… flags carrying the
// "unknown" distinction where it matters).
type Rates struct {
	// TranscribeSecPerChunk is the median transcribe wall-clock per chunk.
	TranscribeSecPerChunk float64
	// EmbedSecPerChunk is the median embed wall-clock per chunk.
	EmbedSecPerChunk float64
	// EvalSecPerChunk is the median eval wall-clock per chunk. Zero is valid
	// ("eval not measured yet" — see EvalKnown); when unknown the ETA excludes
	// eval and the caller's label should say so.
	EvalSecPerChunk float64
	// EvalKnown reports whether any eval timing has been observed. When false the
	// ETA excludes eval and the result notes it (the plan: "until then ETAs
	// exclude eval and SHOULD say so").
	EvalKnown bool
}

// Inputs are everything the model needs, all measured upstream.
type Inputs struct {
	// RemainingChunks is the estimated number of chunks still to process across
	// all not-done books (Σ estimated_chunks(book)). Must be >= 0.
	RemainingChunks int
	// Rates are the per-stage per-chunk rates.
	Rates Rates
	// AvailabilityFraction is the share of wall-clock the runner is available to
	// transcribe (0..1], from runner_availability windows over the trailing
	// window. <= 0 means "no/insufficient availability history" → the calendar
	// estimate is omitted and the result falls back to work-time (labeled).
	AvailabilityFraction float64
}

// Estimate is the model output. WorkSeconds is always meaningful (0 when there
// is no remaining work or no rate data). CalendarSeconds is only meaningful when
// CalendarKnown is true; otherwise consumers should present WorkSeconds and the
// "calendar depends on runner availability" caveat.
type Estimate struct {
	// RemainingChunks echoes the input (for display).
	RemainingChunks int
	// WorkSeconds is the busy-time estimate: remaining_chunks × Σ stage rates.
	WorkSeconds float64
	// CalendarSeconds is WorkSeconds / AvailabilityFraction. Only valid when
	// CalendarKnown is true.
	CalendarSeconds float64
	// CalendarKnown is true iff a calendar estimate could be computed (positive
	// availability fraction AND positive work seconds).
	CalendarKnown bool
	// EvalIncluded reports whether eval time is part of WorkSeconds (false when
	// eval timing is unknown — the caller should label the ETA as excluding eval).
	EvalIncluded bool
	// HasWork is true when there is remaining work AND at least one positive rate
	// (so WorkSeconds is a real estimate, not a structural zero).
	HasWork bool
}

// Compute runs the model over the inputs. It never divides by zero and never
// panics: a missing rate contributes 0, a non-positive availability fraction
// yields CalendarKnown=false (work-time fallback). RemainingChunks < 0 is clamped
// to 0.
func Compute(in Inputs) Estimate {
	chunks := in.RemainingChunks
	if chunks < 0 {
		chunks = 0
	}

	perChunk := nonNeg(in.Rates.TranscribeSecPerChunk) + nonNeg(in.Rates.EmbedSecPerChunk)
	evalIncluded := false
	if in.Rates.EvalKnown && in.Rates.EvalSecPerChunk > 0 {
		perChunk += in.Rates.EvalSecPerChunk
		evalIncluded = true
	}

	work := float64(chunks) * perChunk
	hasWork := chunks > 0 && perChunk > 0

	est := Estimate{
		RemainingChunks: chunks,
		WorkSeconds:     work,
		EvalIncluded:    evalIncluded,
		HasWork:         hasWork,
	}

	// Calendar estimate only when there is real work AND a positive, sane
	// availability fraction. A fraction > 1 (clock skew / overlapping windows) is
	// clamped to 1 so calendar never under-reports below work time.
	frac := in.AvailabilityFraction
	if frac > 1 {
		frac = 1
	}
	if hasWork && frac > 0 {
		est.CalendarSeconds = work / frac
		est.CalendarKnown = true
	}
	return est
}

// nonNeg returns v when positive (and finite), else 0 — so a NaN/negative rate
// (e.g. from a bad median over no rows) never corrupts the sum.
func nonNeg(v float64) float64 {
	if v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
		return v
	}
	return 0
}
