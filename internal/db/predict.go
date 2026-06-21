package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jedwards1230/earmark/internal/predict"
)

// predictWindow is how many trailing done jobs the per-stage median rates are
// computed over (the plan: median over a trailing window, not lifetime mean —
// the runner/model mix changes).
const predictWindow = 50

// availabilityWindowDays is the trailing window over which the runner-availability
// fraction is computed for the calendar ETA (plan §4.3: last 7–14 days).
const availabilityWindowDays = 14

// GetPredictInputs gathers the empirical inputs for the ETA model
// (internal/predict): per-stage median per-chunk rates over the last
// predictWindow done jobs, the estimated remaining chunk count, and the runner
// availability fraction over the trailing window. The model itself is pure; this
// is the single DB query layer feeding it.
//
// All sub-queries are NULL-safe: a fresh install with no history yields zero
// rates (→ "—" ETA) and a zero availability fraction (→ work-time fallback),
// never an error and never a divide-by-zero.
func (db *DB) GetPredictInputs(ctx context.Context) (predict.Inputs, error) {
	var in predict.Inputs

	// Per-stage median per-chunk rates over the trailing window of done jobs.
	// Each rate is seconds-per-chunk = stage_wall_clock / chunk_count, taken only
	// over rows where both the timing and a positive chunk count are present.
	// percentile_cont(0.5) is the median; COALESCE keeps a no-rows result at 0.
	//
	// Chunk-count basis: embed_chunk_count is the authoritative chunk count for a
	// job (written by the embed worker); transcribe/eval rates reuse it so all
	// three rates are per the same unit.
	var trate, erate, evrate float64
	var evalKnown bool
	if err := db.pool.QueryRow(ctx, `
		WITH recent AS (
			SELECT m.transcribe_started_at, m.transcribe_finished_at,
			       m.embed_started_at, m.embed_finished_at,
			       m.eval_started_at, m.eval_finished_at,
			       m.embed_chunk_count, m.eval_chunks
			FROM run_metrics m
			JOIN transcription_jobs j ON j.id = m.job_id
			WHERE j.status = 'done'
			ORDER BY COALESCE(j.completed_at, j.updated_at) DESC
			LIMIT $1
		)
		SELECT
		  COALESCE(percentile_cont(0.5) WITHIN GROUP (
		    ORDER BY EXTRACT(EPOCH FROM (transcribe_finished_at - transcribe_started_at)) / embed_chunk_count
		  ) FILTER (WHERE transcribe_started_at IS NOT NULL
		              AND transcribe_finished_at IS NOT NULL
		              AND embed_chunk_count > 0), 0),
		  COALESCE(percentile_cont(0.5) WITHIN GROUP (
		    ORDER BY EXTRACT(EPOCH FROM (embed_finished_at - embed_started_at)) / embed_chunk_count
		  ) FILTER (WHERE embed_started_at IS NOT NULL
		              AND embed_finished_at IS NOT NULL
		              AND embed_chunk_count > 0), 0),
		  COALESCE(percentile_cont(0.5) WITHIN GROUP (
		    ORDER BY EXTRACT(EPOCH FROM (eval_finished_at - eval_started_at)) / eval_chunks
		  ) FILTER (WHERE eval_started_at IS NOT NULL
		              AND eval_finished_at IS NOT NULL
		              AND eval_chunks > 0), 0),
		  COUNT(*) FILTER (WHERE eval_finished_at IS NOT NULL) > 0
		FROM recent
	`, predictWindow).Scan(&trate, &erate, &evrate, &evalKnown); err != nil {
		return in, fmt.Errorf("predict rates query: %w", err)
	}
	in.Rates = predict.Rates{
		TranscribeSecPerChunk: trate,
		EmbedSecPerChunk:      erate,
		EvalSecPerChunk:       evrate,
		EvalKnown:             evalKnown,
	}

	// Remaining chunks: pending+claimed jobs × the median embedded-chunk count per
	// done job. When no done job has a chunk count yet the per-job estimate is 0
	// (→ remaining 0 → "—"), which is the honest "not enough history" answer.
	var remaining int
	if err := db.pool.QueryRow(ctx, `
		SELECT COALESCE(
		  (SELECT COUNT(*) FROM transcription_jobs WHERE status IN ('pending','claimed'))
		  * COALESCE((
		      SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY embed_chunk_count)
		      FROM run_metrics WHERE embed_chunk_count > 0
		    ), 0), 0)::int
	`).Scan(&remaining); err != nil {
		return in, fmt.Errorf("predict remaining-chunks query: %w", err)
	}
	in.RemainingChunks = remaining

	// Availability fraction over the trailing window, from runner_availability
	// transition events (CONTRACT §1.7). We stitch consecutive 'state' events into
	// [from, to) intervals and sum the seconds the runner was available (reason
	// 'idle'), divided by the window's elapsed seconds. With < 2 events in the
	// window there is no measurable window → 0 (→ work-time fallback, labeled).
	in.AvailabilityFraction = db.availabilityFraction(ctx)

	return in, nil
}

// availabilityFraction computes the share of the trailing window the runner was
// available (runner_availability reason='idle'), by stitching the ordered
// availability events into intervals. Best-effort: any query error returns 0
// (the model then falls back to work-time, which is the safe degradation).
func (db *DB) availabilityFraction(ctx context.Context) float64 {
	rows, err := db.pool.Query(ctx, `
		SELECT created_at, COALESCE(detail->>'reason', reason, '')
		FROM pipeline_events
		WHERE stage = 'runner_availability'
		  AND created_at > now() - ($1 * interval '1 day')
		ORDER BY created_at ASC
	`, availabilityWindowDays)
	if err != nil {
		db.log.Warn("availability fraction query failed; ETA falls back to work time", "error", err)
		return 0
	}
	defer rows.Close()

	type evt struct {
		t      time.Time
		reason string
	}
	var events []evt
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.t, &e.reason); err != nil {
			db.log.Warn("availability fraction scan failed; ETA falls back to work time", "error", err)
			return 0
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil || len(events) < 2 {
		return 0
	}

	// Sum the seconds spent in the 'idle' (available) state across consecutive
	// intervals. The last event has no successor in-window, so it is not counted
	// (we only measure closed [from,to) intervals).
	var availableSecs, totalSecs float64
	for i := 0; i+1 < len(events); i++ {
		dur := events[i+1].t.Sub(events[i].t).Seconds()
		if dur <= 0 {
			continue
		}
		totalSecs += dur
		if events[i].reason == "idle" {
			availableSecs += dur
		}
	}
	if totalSecs <= 0 {
		return 0
	}
	return availableSecs / totalSecs
}
