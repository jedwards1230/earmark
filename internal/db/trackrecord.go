package db

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// The Models page's Track record (CONTRACT §2.14 "Models page"): per model
// alias, how many judge passes it ran and what it decided. It is the audit
// trail read back as a summary — judge_passes and finding_events grouped by the
// alias of the recipe that wrote each row — with no table of its own.

const (
	// TrackRecordStatementTimeout is the server-side statement_timeout of each
	// of the read's two statements, so a single runaway statement is stopped
	// by the server. It is not the bound on the whole read: the two statements
	// can spend up to twice this, and the caller's context deadline (5 s on the
	// dashboard, trackRecordQueryTimeout) is what ends the read first. The
	// decisions aggregate scans every decision event and joins its finding
	// (≈100k rows, ≈0.7 s on production, growing linearly), so a log that has
	// outgrown that deadline shows "unavailable", not a hung page.
	TrackRecordStatementTimeout = 4 * time.Second

	// trackRecordLimit bounds each aggregate's result. A row is a (model alias,
	// status | outcome) pair, so this only guards a pathological recipes table.
	trackRecordLimit = 500

	// judgePassLegacy is the status of a pass backfilled by migration 13.
	judgePassLegacy = "legacy"

	// decisionUndone labels the revoked-decision rows of trackRecordDecisionsSQL.
	// It is not a finding_events.outcome.
	decisionUndone = "undone"
)

// ModelTrack is one model alias's row of the Track record. Judge passes come
// from the propose recipes that alias names (judge_passes.recipe_id), decisions
// from the decide recipes (finding_events.recipe_id); an alias that only judged,
// or only decided, has zeros on the other side — HasPasses / HasDecisions tell
// that apart from "ran and found nothing".
type ModelTrack struct {
	// ModelAlias is recipes.model_alias; "" is a recipe without one, or a pass
	// without a recipe.
	ModelAlias string

	// Judge passes by status (migration 13).
	PassesComplete, PassesPartial, PassesFailed, PassesLegacy, PassesRunning int64

	// Decisions: each finding's latest decision by a recipe of this alias that
	// has not been revoked, by outcome. A finding the alias decided again
	// (a retried hold, a later build) counts once, at its latest outcome;
	// findings a requeue superseded are not counted (as earmark_decisions).
	Apply, Hold, Reject, Duplicate int64
	// Undone is the alias's decisions since revoked (`earmark decide revert`):
	// events, not findings, and not counted above.
	Undone int64

	// Last is the newest judge pass started or live decision made; zero when
	// there is none.
	Last time.Time
}

// PassTotal is every judge pass of the alias, including any still running.
func (m ModelTrack) PassTotal() int64 {
	return m.PassesComplete + m.PassesPartial + m.PassesFailed + m.PassesLegacy + m.PassesRunning
}

// DecisionTotal is the findings the alias currently has a live decision on.
func (m ModelTrack) DecisionTotal() int64 { return m.Apply + m.Hold + m.Reject + m.Duplicate }

// HasPasses reports whether the alias ran any judge pass.
func (m ModelTrack) HasPasses() bool { return m.PassTotal() > 0 }

// HasDecisions reports whether the alias made any decision, undone or not.
func (m ModelTrack) HasDecisions() bool { return m.DecisionTotal() > 0 || m.Undone > 0 }

// trackRecordJudgeSQL: judge passes by the propose recipe's alias and status.
// $1 is the result bound.
const trackRecordJudgeSQL = `
	SELECT coalesce(r.model_alias, ''), p.status, count(*), max(p.started_at)
	  FROM judge_passes p
	  LEFT JOIN recipes r ON r.recipe_id = p.recipe_id
	 GROUP BY 1, 2
	 ORDER BY 1, 2
	 LIMIT $1`

// trackRecordDecisionsSQL: decisions by the decide recipe's alias.
//
// A decision is revoked when a 'revoke' event names it (the unique partial
// index finding_events_revokes_idx makes that anti-join a hash). live keeps,
// per (finding, alias), the newest decision that was not revoked — the same
// "latest unrevoked decision" decide_scorecard and earmark_decisions count,
// grouped by alias instead of recipe, so one alias's retried holds are one
// finding, not several. Findings a requeue superseded are left out of both
// arms, as earmark_decisions leaves them out (the archived finding is the same
// audio as its re-judged successor, which carries the decision now); the
// scorecard views keep them. The second arm counts the revoked events. $1 is
// the result bound.
const trackRecordDecisionsSQL = `
	WITH ev AS MATERIALIZED (
	  SELECT e.id, e.finding_id, e.outcome, e.created_at, coalesce(r.model_alias, '') AS alias,
	         (v.revokes_event_id IS NOT NULL) AS revoked
	    FROM finding_events e
	    JOIN recipes r ON r.recipe_id = e.recipe_id
	    JOIN transcript_findings f ON f.id = e.finding_id AND f.patch_state <> 'superseded'
	    LEFT JOIN finding_events v ON v.kind = 'revoke' AND v.revokes_event_id = e.id
	   WHERE e.kind = 'decision'
	), live AS (
	  SELECT DISTINCT ON (finding_id, alias) alias, outcome, created_at
	    FROM ev
	   WHERE NOT revoked
	   ORDER BY finding_id, alias, id DESC
	)
	SELECT alias, outcome, count(*), max(created_at) FROM live GROUP BY 1, 2
	UNION ALL
	SELECT alias, '` + decisionUndone + `', count(*), NULL::timestamptz FROM ev WHERE revoked GROUP BY 1
	ORDER BY 1, 2
	LIMIT $1`

// trackRecordSessionSQL scopes the read transaction: JIT off (a coarse row
// estimate over the event log can cross jit_above_cost and compilation then
// dominates, as it did for the scorecards) and a hard statement timeout.
// $1 is the timeout in milliseconds.
const trackRecordSessionSQL = `SELECT set_config('jit', 'off', true), set_config('statement_timeout', $1, true)`

// ModelTrackRecord returns one row per model alias that ran a judge pass or
// made a decision, alphabetical with the alias-less row last. Both aggregates
// read one snapshot in a read-only transaction.
func (db *DB) ModelTrackRecord(ctx context.Context) ([]ModelTrack, error) {
	return modelTrackRecord(ctx, db.pool)
}

func modelTrackRecord(ctx context.Context, b txOptionsBeginner) ([]ModelTrack, error) {
	tx, err := b.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("track record: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, trackRecordSessionSQL, strconv.FormatInt(TrackRecordStatementTimeout.Milliseconds(), 10)); err != nil {
		return nil, fmt.Errorf("track record: session settings: %w", err)
	}

	byAlias := map[string]*ModelTrack{}
	row := func(alias string) *ModelTrack {
		m, ok := byAlias[alias]
		if !ok {
			m = &ModelTrack{ModelAlias: alias}
			byAlias[alias] = m
		}
		return m
	}
	later := func(m *ModelTrack, at *time.Time) {
		if at != nil && at.After(m.Last) {
			m.Last = *at
		}
	}

	var (
		alias, status string
		n             int64
		at            *time.Time
	)
	rows, err := tx.Query(ctx, trackRecordJudgeSQL, trackRecordLimit)
	if err != nil {
		return nil, fmt.Errorf("track record: judge passes: %w", err)
	}
	if _, err := pgx.ForEachRow(rows, []any{&alias, &status, &n, &at}, func() error {
		m := row(alias)
		switch status {
		case JudgePassComplete:
			m.PassesComplete += n
		case JudgePassPartial:
			m.PassesPartial += n
		case JudgePassFailed:
			m.PassesFailed += n
		case JudgePassRunning:
			m.PassesRunning += n
		case judgePassLegacy:
			m.PassesLegacy += n
		}
		later(m, at)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("track record: judge passes: %w", err)
	}

	rows, err = tx.Query(ctx, trackRecordDecisionsSQL, trackRecordLimit)
	if err != nil {
		return nil, fmt.Errorf("track record: decisions: %w", err)
	}
	// The scan targets are reused: status holds the outcome (or "undone") here.
	if _, err := pgx.ForEachRow(rows, []any{&alias, &status, &n, &at}, func() error {
		m := row(alias)
		switch status {
		case OutcomeApply:
			m.Apply += n
		case OutcomeHold:
			m.Hold += n
		case OutcomeReject:
			m.Reject += n
		case OutcomeDuplicate:
			m.Duplicate += n
		case decisionUndone:
			m.Undone += n
		}
		later(m, at)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("track record: decisions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("track record: commit: %w", err)
	}

	out := make([]ModelTrack, 0, len(byAlias))
	for _, m := range byAlias {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ModelAlias == "") != (out[j].ModelAlias == "") {
			return out[j].ModelAlias == ""
		}
		return out[i].ModelAlias < out[j].ModelAlias
	})
	return out, nil
}
