package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Read-only aggregates behind the Models dashboard page (CONTRACT §2.14): what
// answered each pipeline role, when it last succeeded or failed, and which
// runner build produced the transcripts. Every query returns scalars or a
// bounded GROUP BY — never row-level text, jsonb, or vectors — so the page's
// 30 s snapshot stays cheap at production size.

// ModelCount is one model id with how many rows it accounts for.
type ModelCount struct {
	Model string
	Count int
}

// ASRLatest is the newest transcript's ASR provenance (migration 00006). The
// runner version and sha are nil for transcripts from a runner that predates
// provenance reporting.
type ASRLatest struct {
	Model         string
	RunnerVersion *string
	ModelSHA256   *string
	At            time.Time
}

// ModelActivity is the "configured vs answered" evidence for the Models page.
type ModelActivity struct {
	// EvalLastOK is max(run_metrics.eval_finished_at): the newest successful
	// judge call from ANY source (backfill CronJob, in-pipeline, dashboard).
	EvalLastOK *time.Time
	// EvalLastFail is max(run_metrics.eval_failed_at). A later success on the
	// same job clears its failure, so this is the newest still-failing attempt.
	EvalLastFail *time.Time
	// EvalLastError is the eval_error of that newest failure ("" when none).
	EvalLastError string
	// EvalFailingNow counts transcripts whose latest judge attempt failed.
	EvalFailingNow int
	// EvalLastModel is the most recent eval_resolved_model ("" when none).
	EvalLastModel string
	// EvalModels7d is every eval_resolved_model that answered in the last 7
	// days with its count, most first (at most evalModels7dLimit).
	EvalModels7d []ModelCount
	// EmbedLastModel is the most recent run_metrics.embed_model ("" when none).
	EmbedLastModel string
	// ASRLatest is the newest transcript's provenance; nil with no transcripts.
	ASRLatest *ASRLatest
}

// evalModels7dLimit bounds the 7-day resolved-model tally. The page shows the
// latest model plus at most three others, so four rows always suffice.
const evalModels7dLimit = 4

// modelActivitySQL is one round trip of scalar subselects plus the newest
// transcript via a LATERAL join (so an empty table still yields one row).
// eval_error is already capped at write time; left() keeps the read bounded
// regardless.
const modelActivitySQL = `
	SELECT
	  (SELECT max(eval_finished_at) FROM run_metrics),
	  (SELECT max(eval_failed_at)   FROM run_metrics),
	  (SELECT left(eval_error, 500) FROM run_metrics
	     WHERE eval_failed_at IS NOT NULL
	     ORDER BY eval_failed_at DESC LIMIT 1),
	  (SELECT count(*) FROM run_metrics WHERE eval_failed_at IS NOT NULL),
	  (SELECT eval_resolved_model FROM run_metrics
	     WHERE eval_resolved_model IS NOT NULL
	     ORDER BY eval_finished_at DESC NULLS LAST LIMIT 1),
	  (SELECT embed_model FROM run_metrics
	     WHERE embed_model IS NOT NULL
	     ORDER BY embed_finished_at DESC NULLS LAST LIMIT 1),
	  t.model_name, t.asr_runner_version, t.asr_model_sha256, t.created_at
	FROM (SELECT 1) AS one
	LEFT JOIN LATERAL (
	  SELECT model_name, asr_runner_version, asr_model_sha256, created_at
	    FROM transcripts ORDER BY created_at DESC LIMIT 1
	) AS t ON true`

const evalModels7dSQL = `
	SELECT eval_resolved_model, count(*)
	  FROM run_metrics
	 WHERE eval_finished_at > now() - interval '7 days'
	   AND eval_resolved_model IS NOT NULL
	 GROUP BY 1
	 ORDER BY 2 DESC, 1
	 LIMIT $1`

// GetModelActivity reads the per-role activity evidence for the Models page.
func (db *DB) GetModelActivity(ctx context.Context) (ModelActivity, error) {
	var (
		a                           ModelActivity
		lastErr, evalModel, embed   *string
		asrModel, asrRunner, asrSHA *string
		asrAt                       *time.Time
	)
	if err := db.pool.QueryRow(ctx, modelActivitySQL).Scan(
		&a.EvalLastOK, &a.EvalLastFail, &lastErr, &a.EvalFailingNow,
		&evalModel, &embed,
		&asrModel, &asrRunner, &asrSHA, &asrAt,
	); err != nil {
		return ModelActivity{}, fmt.Errorf("model activity: %w", err)
	}
	a.EvalLastError = derefString(lastErr)
	a.EvalLastModel = derefString(evalModel)
	a.EmbedLastModel = derefString(embed)
	if asrAt != nil {
		a.ASRLatest = &ASRLatest{Model: derefString(asrModel), RunnerVersion: asrRunner, ModelSHA256: asrSHA, At: *asrAt}
	}

	rows, err := db.pool.Query(ctx, evalModels7dSQL, evalModels7dLimit)
	if err != nil {
		return ModelActivity{}, fmt.Errorf("eval models 7d: %w", err)
	}
	a.EvalModels7d, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ModelCount, error) {
		var m ModelCount
		err := r.Scan(&m.Model, &m.Count)
		return m, err
	})
	if err != nil {
		return ModelActivity{}, fmt.Errorf("scan eval models 7d: %w", err)
	}
	return a, nil
}

// FindingsModelCount is one (answering model, patch_state) bucket of judge
// findings. Model is coalesce(resolved_model, model).
type FindingsModelCount struct {
	Model      string
	PatchState string
	Count      int
}

// findingsByModelLimit bounds the GROUP BY result: distinct models × the
// patch_state enum is small, so this only guards a pathological table.
const findingsByModelLimit = 500

// FindingsByModel counts judge findings by the model that answered and their
// patch state. Human-origin corrections are excluded (no model made them).
func (db *DB) FindingsByModel(ctx context.Context) ([]FindingsModelCount, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT coalesce(resolved_model, model), patch_state, count(*)
		  FROM transcript_findings
		 WHERE origin = 'judge'
		 GROUP BY 1, 2
		 ORDER BY 1, 2
		 LIMIT $1`, findingsByModelLimit)
	if err != nil {
		return nil, fmt.Errorf("findings by model: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FindingsModelCount, error) {
		var f FindingsModelCount
		err := r.Scan(&f.Model, &f.PatchState, &f.Count)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan findings by model: %w", err)
	}
	return out, nil
}

// ASRProvenanceGroup is one (model, runner build, .nemo sha) combination and
// the transcripts it produced. RunnerVersion/SHA are nil for transcripts from
// a runner that reported no provenance (the pre-00006 legacy slice).
type ASRProvenanceGroup struct {
	Model         string
	RunnerVersion *string
	SHA           *string
	Count         int
	First, Last   time.Time
}

// ASRProvenanceGroups returns the transcript provenance groups, newest first,
// at most limit of them.
func (db *DB) ASRProvenanceGroups(ctx context.Context, limit int) ([]ASRProvenanceGroup, error) {
	if limit <= 0 {
		return nil, errors.New("asr provenance groups: limit must be positive")
	}
	rows, err := db.pool.Query(ctx, `
		SELECT model_name, asr_runner_version, asr_model_sha256,
		       count(*), min(created_at), max(created_at)
		  FROM transcripts
		 GROUP BY 1, 2, 3
		 ORDER BY max(created_at) DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("asr provenance groups: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ASRProvenanceGroup, error) {
		var g ASRProvenanceGroup
		err := r.Scan(&g.Model, &g.RunnerVersion, &g.SHA, &g.Count, &g.First, &g.Last)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan asr provenance groups: %w", err)
	}
	return out, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
