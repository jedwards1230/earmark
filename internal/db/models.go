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
	// EvalLastModelAt is when EvalLastModel answered (its eval_finished_at);
	// nil when unknown. The Models page compares the answer with the expected
	// model only when it is newer than the current propose recipe.
	EvalLastModelAt *time.Time
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
// judge answer and the newest transcript via LATERAL joins (so empty tables
// still yield one row).
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
	  em.eval_resolved_model, em.eval_finished_at,
	  (SELECT embed_model FROM run_metrics
	     WHERE embed_model IS NOT NULL
	     ORDER BY embed_finished_at DESC NULLS LAST LIMIT 1),
	  t.model_name, t.asr_runner_version, t.asr_model_sha256, t.created_at
	FROM (SELECT 1) AS one
	LEFT JOIN LATERAL (
	  SELECT eval_resolved_model, eval_finished_at FROM run_metrics
	   WHERE eval_resolved_model IS NOT NULL
	   ORDER BY eval_finished_at DESC NULLS LAST LIMIT 1
	) AS em ON true
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
		&evalModel, &a.EvalLastModelAt, &embed,
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

// FindingsModelCount is one (answering model, patch_state, decider) bucket of
// judge findings. Model is coalesce(resolved_model, model); Decider classifies
// decided_by (see the Decider* constants).
type FindingsModelCount struct {
	Model      string
	PatchState string
	Decider    string
	Count      int
}

// Decider classes of transcript_findings.decided_by (CONTRACT §2.17, §2.19).
// A decide recipe writes "jev:<recipe_id>" and its undo "revert:jev:<recipe_id>"
// (the only two deciders the bulk path accepts); anything else — mcp:, cli:,
// another tool — is a person. DeciderNone is a NULL/empty decided_by: an
// undecided finding, or a decision recorded before attribution (counted as a
// person's by the Models page, as it always was).
const (
	DeciderNone      = ""
	DeciderHuman     = "human"
	DeciderJev       = "jev"
	DeciderJevRevert = "jev_revert"
)

// findingsByModelLimit bounds the GROUP BY result: distinct models × the
// patch_state enum × the four decider classes is small, so this only guards a
// pathological table.
const findingsByModelLimit = 500

// findingsByModelSQL is one pass over the judge findings. The decider CASE
// mirrors findingevents.go's automatedDeciderRe loosely (LIKE, not the
// 64-hex check): a malformed jev-looking decider is still not a person.
const findingsByModelSQL = `
	SELECT coalesce(resolved_model, model), patch_state,
	       CASE WHEN decided_by LIKE 'jev:%'        THEN 'jev'
	            WHEN decided_by LIKE 'revert:jev:%' THEN 'jev_revert'
	            WHEN coalesce(decided_by, '') = ''  THEN ''
	            ELSE 'human' END,
	       count(*)
	  FROM transcript_findings
	 WHERE origin = 'judge'
	 GROUP BY 1, 2, 3
	 ORDER BY 1, 2, 3
	 LIMIT $1`

// FindingsByModel counts judge findings by the model that answered, their
// patch state and who decided them. Human-origin corrections are excluded (no
// model made them).
func (db *DB) FindingsByModel(ctx context.Context) ([]FindingsModelCount, error) {
	rows, err := db.pool.Query(ctx, findingsByModelSQL, findingsByModelLimit)
	if err != nil {
		return nil, fmt.Errorf("findings by model: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FindingsModelCount, error) {
		var f FindingsModelCount
		err := r.Scan(&f.Model, &f.PatchState, &f.Decider, &f.Count)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan findings by model: %w", err)
	}
	return out, nil
}

// FnRoleActivity is the call evidence for one pure-function role (decide's
// should_apply, scan's scan_chunk) under its CURRENT recipe: the fn_calls rows
// with the recipe's fn, model alias and prompt hash — the request identity the
// cache is keyed on, so errored calls (which carry no recipe_id) and fallback
// replies (stamped with the fallback's recipe) are counted too.
type FnRoleActivity struct {
	Step       string // recipe step: "decide" | "scan"
	Fn         string // the recipe's params.fn
	RecipeID   string
	ModelAlias string
	// Calls are model requests (cache hits excluded); CacheHits are calls
	// served from an earlier row.
	Calls     int
	CacheHits int
	// Fallbacks are replies from another model (error_class model_fallback):
	// stored, never served.
	Fallbacks int
	// Failures24h are failed calls (any error class but model_fallback) in the
	// last 24 hours.
	Failures24h int
	// LastOK is the newest successful model request; LastFail the newest
	// failed one (fallbacks excluded) and LastErrorClass its class.
	LastOK         *time.Time
	LastFail       *time.Time
	LastErrorClass string
	// LastModel is the newest reply's model_resolved (fallbacks included,
	// cache hits excluded); "" when no request got a reply.
	LastModel string
	// Outputs is the step's output rows under the recipe or one equivalent to
	// it (a format-1 id differing only in its build, CONTRACT §1.9) — scan:
	// chunk_scan rows; nil for decide (its decisions are counted from the
	// findings).
	Outputs *int64
}

// fnRoleActivitySQL is one round trip: the current decide/scan recipes and an
// aggregate over their calls. fn_calls has no index on fn alone, so this is a
// scan of the call log — bounded output (≤ 2 rows), run on the page's 30 s
// snapshot timer. $1 is ErrorClassModelFallback.
const fnRoleActivitySQL = `
	WITH cur AS (
	  SELECT cr.step, r.recipe_id, coalesce(r.model_alias, '') AS model_alias,
	         coalesce(r.prompt_sha256, '') AS prompt_sha256, coalesce(r.params->>'fn', '') AS fn,
	         CASE WHEN cr.step = 'scan'
	              THEN (SELECT count(*) FROM chunk_scan s JOIN recipes o ON o.recipe_id = s.recipe_id
	                     WHERE (o.step, o.step_version, o.model_alias, o.model_resolved, o.model_revision,
	                            o.prompt_version, o.prompt_sha256, o.params)
	                           IS NOT DISTINCT FROM
	                           (r.step, r.step_version, r.model_alias, r.model_resolved, r.model_revision,
	                            r.prompt_version, r.prompt_sha256, r.params)) END AS outputs
	    FROM current_recipes cr
	    JOIN recipes r ON r.recipe_id = cr.recipe_id
	   WHERE cr.step IN ('decide', 'scan')
	)
	SELECT cur.step, cur.fn, cur.recipe_id, cur.model_alias,
	       count(c.id) FILTER (WHERE NOT c.cache_hit),
	       count(c.id) FILTER (WHERE c.cache_hit),
	       count(c.id) FILTER (WHERE c.error_class = $1),
	       count(c.id) FILTER (WHERE c.error_class <> $1 AND c.created_at > now() - interval '24 hours'),
	       max(c.created_at) FILTER (WHERE c.error_class IS NULL AND NOT c.cache_hit),
	       max(c.created_at) FILTER (WHERE c.error_class <> $1),
	       (array_agg(c.error_class ORDER BY c.created_at DESC, c.id DESC)
	          FILTER (WHERE c.error_class <> $1))[1],
	       (array_agg(c.model_resolved ORDER BY c.created_at DESC, c.id DESC)
	          FILTER (WHERE c.model_resolved IS NOT NULL AND NOT c.cache_hit))[1],
	       cur.outputs
	  FROM cur
	  LEFT JOIN fn_calls c ON c.fn = cur.fn AND c.model_alias = cur.model_alias
	                      AND c.prompt_sha256 = cur.prompt_sha256
	 GROUP BY cur.step, cur.fn, cur.recipe_id, cur.model_alias, cur.outputs
	 ORDER BY cur.step`

// FnRoleActivity reads the decide and scan roles' call evidence; a step with
// no current recipe has no entry.
func (db *DB) FnRoleActivity(ctx context.Context) ([]FnRoleActivity, error) {
	rows, err := db.pool.Query(ctx, fnRoleActivitySQL, ErrorClassModelFallback)
	if err != nil {
		return nil, fmt.Errorf("fn role activity: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FnRoleActivity, error) {
		var (
			a              FnRoleActivity
			errClass, last *string
		)
		err := r.Scan(&a.Step, &a.Fn, &a.RecipeID, &a.ModelAlias,
			&a.Calls, &a.CacheHits, &a.Fallbacks, &a.Failures24h,
			&a.LastOK, &a.LastFail, &errClass, &last, &a.Outputs)
		a.LastErrorClass, a.LastModel = derefString(errClass), derefString(last)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan fn role activity: %w", err)
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
