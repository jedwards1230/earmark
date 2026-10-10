package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Step runs (CONTRACT §1.10): one step_runs row per ad-hoc batch step
// (`earmark decide`, `decide revert`, `scan`, `eval`), written by
// internal/runs while the step runs. The writes are single short statements
// on the pool — never inside a decide write transaction — and their callers
// treat any error as non-fatal. The reads back the dashboard's "Running now"
// panel, GET /api/v1/runs and the earmark_step_run_* metrics.

// Step run modes.
const (
	StepRunDryRun = "dry_run"
	StepRunWrite  = "write"
)

// Step run statuses. A run whose process died stays StepRunRunning with a
// heartbeat that stopped advancing; readers call it stale (StepRun.Stale).
const (
	StepRunRunning   = "running"
	StepRunDone      = "done"
	StepRunFailed    = "failed"
	StepRunCancelled = "cancelled"
)

// StepRunStaleAfter is how old a running row's heartbeat may be before
// readers report it stale. The recorder heartbeats at least every 30 s, so
// four missed heartbeats in a row mean the process is gone.
const StepRunStaleAfter = 2 * time.Minute

// StepRunAbandonAfter is how long a stale running row stays in the active
// list. Older ones (a pod killed days ago) are listed with the recent runs.
const StepRunAbandonAfter = 24 * time.Hour

// MaxStepRunList caps one StepRuns list.
const MaxStepRunList = 100

// Column bounds: a step run row is a status line, not a log.
const (
	maxStepRunText  = 256  // args, model, host, recipe id
	maxStepRunMsg   = 512  // last_message
	maxStepRunError = 2000 // error
)

var stepTokenRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// StepRunStart is the row a run inserts when it starts.
type StepRunStart struct {
	Step     string
	Mode     string // StepRunDryRun | StepRunWrite
	RecipeID string
	Model    string
	Args     string
	Host     string
	Total    *int64
	Message  string
}

// StepRunProgress is a run's mutable state, written by every heartbeat and
// by the final write. An empty RecipeID/Model keeps the stored value; Message
// "" keeps the last message.
type StepRunProgress struct {
	RecipeID string
	Model    string
	Total    *int64
	Done     *int64
	Counters map[string]int64
	CostUSD  *float64
	Message  string
}

// StepRun is one step_runs row as read back. ObservedAt is the database's
// now() at read time, so Stale and Elapsed compare the database clock with
// itself (the heartbeats are stamped with it too).
type StepRun struct {
	ID          int64
	Step        string
	Mode        string
	RecipeID    string
	Model       string
	Args        string
	Host        string
	StartedAt   time.Time
	HeartbeatAt time.Time
	FinishedAt  *time.Time
	Status      string
	Total       *int64
	Done        *int64
	Counters    map[string]int64
	CostUSD     *float64
	Error       string
	Message     string
	ObservedAt  time.Time
}

// Stale reports a running row whose heartbeat is older than after: the
// process that owned it is gone.
func (r StepRun) Stale(after time.Duration) bool {
	return r.Status == StepRunRunning && r.ObservedAt.Sub(r.HeartbeatAt) > after
}

// Elapsed is how long the run has run (running) or ran (closed). A stale
// run's elapsed time ends at its last heartbeat.
func (r StepRun) Elapsed(staleAfter time.Duration) time.Duration {
	end := r.ObservedAt
	switch {
	case r.FinishedAt != nil:
		end = *r.FinishedAt
	case r.Stale(staleAfter):
		end = r.HeartbeatAt
	}
	if d := end.Sub(r.StartedAt); d > 0 {
		return d
	}
	return 0
}

// StepRunList is the dashboard's view of step_runs: the open runs (live and
// recently stale, newest first) and the newest other runs.
type StepRunList struct {
	Active []StepRun
	Recent []StepRun
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s StepRunStart) validate() error {
	switch {
	case !stepTokenRe.MatchString(s.Step):
		return fmt.Errorf("step run: step %q is not a lowercase token", s.Step)
	case s.Mode != StepRunDryRun && s.Mode != StepRunWrite:
		return fmt.Errorf("step run: mode %q is not dry_run or write", s.Mode)
	case s.Total != nil && *s.Total < 0:
		return errors.New("step run: negative total")
	}
	return nil
}

func (p StepRunProgress) validate() error {
	switch {
	case p.Total != nil && *p.Total < 0, p.Done != nil && *p.Done < 0:
		return errors.New("step run: negative total or done")
	case p.CostUSD != nil && (*p.CostUSD < 0 || math.IsNaN(*p.CostUSD) || math.IsInf(*p.CostUSD, 0)):
		return errors.New("step run: cost must be a finite value >= 0")
	}
	return nil
}

func countersJSON(c map[string]int64) (string, error) {
	if c == nil {
		return "{}", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("step run counters: %w", err)
	}
	return string(b), nil
}

const insertStepRunSQL = `
	INSERT INTO step_runs (step, mode, recipe_id, model, args, host, total, last_message)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	RETURNING id`

// StartStepRun inserts a running row and returns its id.
func (db *DB) StartStepRun(ctx context.Context, s StepRunStart) (int64, error) {
	return startStepRun(ctx, db.pool, s)
}

func startStepRun(ctx context.Context, q rowScanner, s StepRunStart) (int64, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	var id int64
	if err := q.QueryRow(ctx, insertStepRunSQL, s.Step, s.Mode,
		nullIfEmpty(clip(s.RecipeID, maxStepRunText)), nullIfEmpty(clip(s.Model, maxStepRunText)),
		nullIfEmpty(clip(s.Args, maxStepRunText)), nullIfEmpty(clip(s.Host, maxStepRunText)),
		s.Total, nullIfEmpty(clip(s.Message, maxStepRunMsg))).Scan(&id); err != nil {
		return 0, fmt.Errorf("start step run: %w", err)
	}
	return id, nil
}

// updateStepRunSQL is one heartbeat. It only touches an open row, so a late
// heartbeat can never reopen a closed run. $9/$10 are set by the final write:
// the closing status and error (NULL on a heartbeat).
const updateStepRunSQL = `
	UPDATE step_runs
	   SET heartbeat_at = now(),
	       recipe_id    = COALESCE($2, recipe_id),
	       model        = COALESCE($3, model),
	       total        = $4,
	       done         = $5,
	       counters     = $6::jsonb,
	       cost_usd     = $7,
	       last_message = COALESCE($8, last_message),
	       status       = COALESCE($9, status),
	       finished_at  = CASE WHEN $9::text IS NULL THEN finished_at ELSE now() END,
	       error        = COALESCE($10, error)
	 WHERE id = $1 AND status = 'running'`

// ErrStepRunClosed is returned when a heartbeat or finish targets a row that
// is no longer running (or does not exist).
var ErrStepRunClosed = errors.New("step run is not running")

// UpdateStepRun writes one heartbeat: the run's progress, and heartbeat_at =
// now().
func (db *DB) UpdateStepRun(ctx context.Context, id int64, p StepRunProgress) error {
	return writeStepRun(ctx, db.pool, id, p, "", "")
}

// FinishStepRun closes a running row with status (done, failed or
// cancelled), its final progress and, for a failure, the error text.
func (db *DB) FinishStepRun(ctx context.Context, id int64, status string, p StepRunProgress, errText string) error {
	switch status {
	case StepRunDone, StepRunFailed, StepRunCancelled:
	default:
		return fmt.Errorf("step run: %q is not a closing status", status)
	}
	return writeStepRun(ctx, db.pool, id, p, status, errText)
}

func writeStepRun(ctx context.Context, ex execer, id int64, p StepRunProgress, status, errText string) error {
	if err := p.validate(); err != nil {
		return err
	}
	counters, err := countersJSON(p.Counters)
	if err != nil {
		return err
	}
	tag, err := ex.Exec(ctx, updateStepRunSQL, id,
		nullIfEmpty(clip(p.RecipeID, maxStepRunText)), nullIfEmpty(clip(p.Model, maxStepRunText)),
		p.Total, p.Done, counters, p.CostUSD, nullIfEmpty(clip(p.Message, maxStepRunMsg)),
		nullIfEmpty(status), nullIfEmpty(clip(errText, maxStepRunError)))
	if err != nil {
		return fmt.Errorf("write step run %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("write step run %d: %w", id, ErrStepRunClosed)
	}
	return nil
}

const stepRunColumns = `id, step, mode, COALESCE(recipe_id, ''), COALESCE(model, ''), COALESCE(args, ''),
	       COALESCE(host, ''), started_at, heartbeat_at, finished_at, status, total, done, counters,
	       cost_usd, COALESCE(error, ''), COALESCE(last_message, ''), now()`

// activeStepRunsSQL lists open rows whose heartbeat is within the abandon
// window ($1 seconds), newest first — step_runs_running_idx.
const activeStepRunsSQL = `
	SELECT ` + stepRunColumns + `
	  FROM step_runs
	 WHERE status = 'running' AND heartbeat_at > now() - $1 * interval '1 second'
	 ORDER BY started_at DESC
	 LIMIT $2`

// recentStepRunsSQL lists every other row, newest first —
// step_runs_started_at_idx. Open rows past the abandon window land here (as
// stale) rather than in the active list.
const recentStepRunsSQL = `
	SELECT ` + stepRunColumns + `
	  FROM step_runs
	 WHERE NOT (status = 'running' AND heartbeat_at > now() - $1 * interval '1 second')
	 ORDER BY started_at DESC, id DESC
	 LIMIT $2`

func scanStepRun(r pgx.CollectableRow) (StepRun, error) {
	var s StepRun
	var counters []byte
	if err := r.Scan(&s.ID, &s.Step, &s.Mode, &s.RecipeID, &s.Model, &s.Args, &s.Host, &s.StartedAt,
		&s.HeartbeatAt, &s.FinishedAt, &s.Status, &s.Total, &s.Done, &counters, &s.CostUSD, &s.Error,
		&s.Message, &s.ObservedAt); err != nil {
		return s, err
	}
	if len(counters) > 0 {
		if err := json.Unmarshal(counters, &s.Counters); err != nil {
			return s, fmt.Errorf("step run %d counters: %w", s.ID, err)
		}
	}
	return s, nil
}

// StepRuns returns at most activeLimit open runs and recentLimit other runs
// (each clamped to 1..MaxStepRunList), newest first. Two index-backed LIMIT
// queries; read-only.
func (db *DB) StepRuns(ctx context.Context, activeLimit, recentLimit int) (StepRunList, error) {
	return stepRuns(ctx, db.pool, activeLimit, recentLimit)
}

func clampStepRunLimit(n int) int {
	return min(max(n, 1), MaxStepRunList)
}

func stepRuns(ctx context.Context, q rowQuerier, activeLimit, recentLimit int) (StepRunList, error) {
	var out StepRunList
	abandon := int64(StepRunAbandonAfter / time.Second)
	rows, err := q.Query(ctx, activeStepRunsSQL, abandon, clampStepRunLimit(activeLimit))
	if err != nil {
		return out, fmt.Errorf("active step runs: %w", err)
	}
	if out.Active, err = pgx.CollectRows(rows, scanStepRun); err != nil {
		return out, fmt.Errorf("active step runs: %w", err)
	}
	rows, err = q.Query(ctx, recentStepRunsSQL, abandon, clampStepRunLimit(recentLimit))
	if err != nil {
		return out, fmt.Errorf("recent step runs: %w", err)
	}
	if out.Recent, err = pgx.CollectRows(rows, scanStepRun); err != nil {
		return out, fmt.Errorf("recent step runs: %w", err)
	}
	return out, nil
}

// StepRunActive is one earmark_step_run_active series.
type StepRunActive struct {
	Step, Mode string
	N          int64
}

// StepRunProgressRatio is one earmark_step_run_progress_ratio series: done /
// total of the step's newest live run that has a total.
type StepRunProgressRatio struct {
	Step  string
	Ratio float64
}

// StepRunItems is one earmark_step_run_items_total series: the sum of one
// counter over every recorded run of a step.
type StepRunItems struct {
	Step, Outcome string
	N             int64
}

// StepRunStats backs the earmark_step_run_* metrics.
type StepRunStats struct {
	Active   []StepRunActive
	Progress []StepRunProgressRatio
	Items    []StepRunItems
}

// stepRunActiveSQL counts live (not stale, $1 seconds) open runs.
const stepRunActiveSQL = `
	SELECT step, mode, count(*)
	  FROM step_runs
	 WHERE status = 'running' AND heartbeat_at > now() - $1 * interval '1 second'
	 GROUP BY 1, 2
	 ORDER BY 1, 2`

// stepRunProgressSQL: per step, the newest live open run with a total.
const stepRunProgressSQL = `
	SELECT DISTINCT ON (step) step,
	       LEAST(COALESCE(done, 0)::float8 / total, 1)
	  FROM step_runs
	 WHERE status = 'running' AND heartbeat_at > now() - $1 * interval '1 second'
	   AND total > 0
	 ORDER BY step, started_at DESC`

// stepRunItemsSQL sums every counter over all runs per step. step_runs grows
// by a handful of rows an hour, so the scan stays small.
const stepRunItemsSQL = `
	SELECT r.step, c.key, sum(c.value::bigint)::bigint
	  FROM step_runs r, jsonb_each_text(r.counters) c
	 WHERE c.value ~ '^[0-9]{1,18}$'
	 GROUP BY 1, 2
	 ORDER BY 1, 2`

// StepRunStats reads the earmark_step_run_* metric values (read-only).
func (db *DB) StepRunStats(ctx context.Context) (StepRunStats, error) {
	return stepRunStats(ctx, db.pool)
}

func stepRunStats(ctx context.Context, q rowQuerier) (StepRunStats, error) {
	var out StepRunStats
	stale := int64(StepRunStaleAfter / time.Second)
	rows, err := q.Query(ctx, stepRunActiveSQL, stale)
	if err != nil {
		return out, fmt.Errorf("step run active: %w", err)
	}
	if out.Active, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (StepRunActive, error) {
		var a StepRunActive
		return a, r.Scan(&a.Step, &a.Mode, &a.N)
	}); err != nil {
		return out, fmt.Errorf("step run active: %w", err)
	}
	if rows, err = q.Query(ctx, stepRunProgressSQL, stale); err != nil {
		return out, fmt.Errorf("step run progress: %w", err)
	}
	if out.Progress, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (StepRunProgressRatio, error) {
		var p StepRunProgressRatio
		return p, r.Scan(&p.Step, &p.Ratio)
	}); err != nil {
		return out, fmt.Errorf("step run progress: %w", err)
	}
	if rows, err = q.Query(ctx, stepRunItemsSQL); err != nil {
		return out, fmt.Errorf("step run items: %w", err)
	}
	if out.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (StepRunItems, error) {
		var i StepRunItems
		return i, r.Scan(&i.Step, &i.Outcome, &i.N)
	}); err != nil {
		return out, fmt.Errorf("step run items: %w", err)
	}
	return out, nil
}
