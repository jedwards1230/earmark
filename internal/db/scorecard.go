package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Model scorecards (CONTRACT §2.15 "Labels and scorecards", migration 14):
// read-only views over findings, judge passes, the decision log, fn_calls and
// finding_labels that say how each propose and decide recipe performed. The
// arithmetic lives in the views (recipe_scorecard, decide_scorecard) so SQL
// users and `earmark models report` see the same numbers.

// LabelTally counts the labelled targets a recipe proposed (or decided).
// Precision is good / (good + junk + partial); unsure is excluded.
type LabelTally struct {
	Good      int64    `json:"good"`
	Junk      int64    `json:"junk"`
	Partial   int64    `json:"partial"`
	Unsure    int64    `json:"unsure"`
	Precision *float64 `json:"precision"`
}

// Decided is the number of labels that count for precision.
func (l LabelTally) Decided() int64 { return l.Good + l.Junk + l.Partial }

// withPrecision fills Precision from the counts.
func (l LabelTally) withPrecision() LabelTally {
	if n := l.Decided(); n > 0 {
		p := float64(l.Good) / float64(n)
		l.Precision = &p
	}
	return l
}

// DecisionMix is how one decide recipe's latest live decisions on a propose
// recipe's findings came out.
type DecisionMix struct {
	DecideRecipeID string `json:"decide_recipe_id"`
	DecideModel    string `json:"decide_model,omitempty"`
	Decided        int64  `json:"decided"`
	Apply          int64  `json:"apply"`
	Hold           int64  `json:"hold"`
	Reject         int64  `json:"reject"`
	// Reasons counts "outcome:reason".
	Reasons map[string]int64 `json:"reasons"`
}

// ProposeScore is one recipe_scorecard row. Pointer fields are NULL when
// nothing recorded them; on a Legacy recipe (no recorded judge pass, no
// logged judge call) the pass- and call-derived ones always are.
type ProposeScore struct {
	RecipeID      string     `json:"recipe_id"` // "" = unattributed legacy findings
	ModelAlias    string     `json:"model_alias,omitempty"`
	ModelResolved string     `json:"model_resolved,omitempty"`
	PromptVersion string     `json:"prompt_version,omitempty"`
	StepVersion   *int       `json:"step_version,omitempty"`
	RecipeCreated *time.Time `json:"recipe_created_at,omitempty"`
	Legacy        bool       `json:"legacy"`

	Passes            int64    `json:"passes"`
	PassesComplete    int64    `json:"passes_complete"`
	PassesLegacy      int64    `json:"passes_legacy"`
	PassesIncomplete  int64    `json:"passes_incomplete"`
	PassesRunning     int64    `json:"passes_running"`
	TranscriptsJudged int64    `json:"transcripts_judged"`
	ChunksJudged      *int64   `json:"chunks_judged"`
	ChunksCached      *int64   `json:"chunks_cached"`
	ChunksFailed      *int64   `json:"chunks_failed"`
	CachedShare       *float64 `json:"cached_share"`

	FindingsTotal       int64            `json:"findings_total"`
	FindingsLive        int64            `json:"findings_live"`
	FindingsPer1kChunks *float64         `json:"findings_per_1k_chunks"`
	IssueMix            map[string]int64 `json:"issue_mix"`
	StateMix            map[string]int64 `json:"state_mix"`
	// Drops is nil when no pass recorded drop counts.
	Drops         map[string]int64 `json:"drops"`
	ParseFailures *int64           `json:"parse_failures"`

	Decisions   []DecisionMix `json:"decisions"`
	AppliedEver int64         `json:"applied_ever"`
	Reverted    int64         `json:"reverted"`
	RevertRate  *float64      `json:"revert_rate"`

	CorrectionKeys int64    `json:"correction_keys"`
	AgreedKeys     int64    `json:"agreed_keys"`
	AgreementRate  *float64 `json:"agreement_rate"`

	Labels           LabelTally `json:"labels"`
	ProperNounLabels LabelTally `json:"proper_noun_labels"`

	CostUSD         *float64 `json:"cost_usd"`
	CostPerChunkUSD *float64 `json:"cost_per_chunk_usd"`
	LatencyMsP50    *float64 `json:"latency_ms_p50"`
	LatencyMsP95    *float64 `json:"latency_ms_p95"`
	// LatencySource is "calls" (fn_calls), "passes" (judge_passes p50/p95
	// weighted by chunks) or "".
	LatencySource string `json:"latency_source,omitempty"`

	FirstActivityAt *time.Time `json:"first_activity_at,omitempty"`
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty"`
}

// PBucket is one p bucket of a decide recipe's calibration: [Lo, Hi).
type PBucket struct {
	Bucket int     `json:"bucket"`
	Lo     float64 `json:"lo"`
	Hi     float64 `json:"hi"`
	// Against labels.
	Good    int64 `json:"good,omitempty"`
	Junk    int64 `json:"junk,omitempty"`
	Partial int64 `json:"partial,omitempty"`
	Unsure  int64 `json:"unsure,omitempty"`
	// Against human (mcp:) decisions.
	Accepted int64 `json:"accepted,omitempty"`
	Rejected int64 `json:"rejected,omitempty"`
	// Rate is precision (labels) or the human accept rate.
	Rate *float64 `json:"rate"`
}

// DecideScore is one decide_scorecard row.
type DecideScore struct {
	RecipeID      string          `json:"recipe_id"`
	ModelAlias    string          `json:"model_alias,omitempty"`
	ModelResolved string          `json:"model_resolved,omitempty"`
	PromptVersion string          `json:"prompt_version,omitempty"`
	StepVersion   *int            `json:"step_version,omitempty"`
	Params        json.RawMessage `json:"params,omitempty"`
	RecipeCreated *time.Time      `json:"recipe_created_at,omitempty"`

	Decisions       int64    `json:"decisions"`
	Revoked         int64    `json:"revoked"`
	RevokeRate      *float64 `json:"revoke_rate"`
	FindingsDecided int64    `json:"findings_decided"`
	Live            int64    `json:"live"`
	Apply           int64    `json:"apply"`
	Hold            int64    `json:"hold"`
	Reject          int64    `json:"reject"`
	// OutcomeEvidence is outcome → evidence → count over live decisions.
	OutcomeEvidence map[string]map[string]int64 `json:"outcome_evidence"`
	Reasons         map[string]int64            `json:"reasons"`

	// LabelOutcomes is outcome → the labels of those decisions' fixes.
	LabelOutcomes    map[string]LabelTally `json:"label_outcomes"`
	LabelCalibration []PBucket             `json:"label_calibration"`
	HumanCalibration []PBucket             `json:"human_calibration"`
	HumanDecided     int64                 `json:"human_decided"`
	HumanAgree       int64                 `json:"human_agree"`
	HumanAgreement   *float64              `json:"human_agreement"`

	PrevRecipeID string   `json:"prev_recipe_id,omitempty"`
	FlipOverlap  int64    `json:"flip_overlap"`
	Flips        int64    `json:"flips"`
	FlipRate     *float64 `json:"flip_rate"`

	Calls          int64    `json:"calls"`
	CallsCached    int64    `json:"calls_cached"`
	CallErrors     int64    `json:"call_errors"`
	CostUSD        *float64 `json:"cost_usd"`
	CostPerCallUSD *float64 `json:"cost_per_call_usd"`
	LatencyMsP50   *float64 `json:"latency_ms_p50"`
	LatencyMsP95   *float64 `json:"latency_ms_p95"`

	FirstDecisionAt *time.Time `json:"first_decision_at,omitempty"`
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty"`
}

// recipeScorecardSQL reads the view; $1 (NULL = all) keeps recipes active
// at or after it.
var recipeScorecardSQL = `
	SELECT COALESCE(recipe_id, ''), COALESCE(model_alias, ''), COALESCE(model_resolved, ''),
	       COALESCE(prompt_version, ''), step_version, recipe_created_at, legacy,
	       passes, passes_complete, passes_legacy, passes_incomplete, passes_running,
	       transcripts_judged, chunks_judged, chunks_cached, chunks_failed, cached_share,
	       findings_total, findings_live, findings_per_1k_chunks, issue_mix, state_mix, drops,
	       parse_failures, decisions, applied_ever, reverted, revert_rate,
	       correction_keys, agreed_keys, agreement_rate,
	       labelled_good, labelled_junk, labelled_partial, labelled_unsure,
	       proper_noun_good, proper_noun_junk, proper_noun_partial, proper_noun_unsure,
	       cost_usd, cost_per_chunk_usd, latency_ms_p50, latency_ms_p95, COALESCE(latency_source, ''),
	       first_activity_at, last_activity_at
	  FROM recipe_scorecard
	 WHERE $1::timestamptz IS NULL OR last_activity_at >= $1::timestamptz
	 ORDER BY first_activity_at NULLS FIRST, recipe_id NULLS FIRST`

var decideScorecardSQL = `
	SELECT recipe_id, COALESCE(model_alias, ''), COALESCE(model_resolved, ''), COALESCE(prompt_version, ''),
	       step_version, params, recipe_created_at,
	       decisions, revoked, revoke_rate, findings_decided, live, apply, hold, reject,
	       outcome_evidence, reasons, label_outcomes, label_calibration, human_calibration,
	       human_decided, human_agree, human_agreement,
	       COALESCE(prev_recipe_id, ''), flip_overlap, flips, flip_rate,
	       calls, calls_cached, call_errors, cost_usd, cost_per_call_usd, latency_ms_p50, latency_ms_p95,
	       first_decision_at, last_activity_at
	  FROM decide_scorecard
	 WHERE $1::timestamptz IS NULL OR last_activity_at >= $1::timestamptz
	 ORDER BY first_decision_at NULLS LAST, recipe_id`

// RecipeScorecard returns recipe_scorecard, oldest recipe first. A non-zero
// since keeps the recipes with activity at or after it.
func (db *DB) RecipeScorecard(ctx context.Context, since time.Time) ([]ProposeScore, error) {
	out, err := readScorecard(ctx, db.pool, recipeScorecardSQL, since, scanProposeScore)
	if err != nil {
		return nil, fmt.Errorf("recipe scorecard: %w", err)
	}
	return out, nil
}

// DecideScorecard returns decide_scorecard, oldest recipe first.
func (db *DB) DecideScorecard(ctx context.Context, since time.Time) ([]DecideScore, error) {
	out, err := readScorecard(ctx, db.pool, decideScorecardSQL, since, scanDecideScore)
	if err != nil {
		return nil, fmt.Errorf("decide scorecard: %w", err)
	}
	return out, nil
}

// scorecardJITOffSQL turns JIT off for the read. The views aggregate whole
// tables through CTEs whose row estimates are coarse, so the planner's cost
// crosses jit_above_cost and LLVM compilation dominates: at the live scale
// (~33k findings, ~67k finding events, ~135k fn_calls) recipe_scorecard took
// 1.3 s with JIT, ~1.0 s of it compiling, and 0.25 s without.
const scorecardJITOffSQL = `SET LOCAL jit = off`

// readScorecard runs one scorecard query in a read-only transaction with
// JIT off.
func readScorecard[T any](ctx context.Context, b txOptionsBeginner, sql string, since time.Time,
	scan pgx.RowToFunc[T]) ([]T, error) {
	tx, err := b.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, scorecardJITOffSQL); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, optTime(since))
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func scanProposeScore(row pgx.CollectableRow) (ProposeScore, error) {
	var s ProposeScore
	var issue, states, drops, decisions []byte
	var stepVersion *int32
	if err := row.Scan(&s.RecipeID, &s.ModelAlias, &s.ModelResolved, &s.PromptVersion, &stepVersion,
		&s.RecipeCreated, &s.Legacy,
		&s.Passes, &s.PassesComplete, &s.PassesLegacy, &s.PassesIncomplete, &s.PassesRunning,
		&s.TranscriptsJudged, &s.ChunksJudged, &s.ChunksCached, &s.ChunksFailed, &s.CachedShare,
		&s.FindingsTotal, &s.FindingsLive, &s.FindingsPer1kChunks, &issue, &states, &drops,
		&s.ParseFailures, &decisions, &s.AppliedEver, &s.Reverted, &s.RevertRate,
		&s.CorrectionKeys, &s.AgreedKeys, &s.AgreementRate,
		&s.Labels.Good, &s.Labels.Junk, &s.Labels.Partial, &s.Labels.Unsure,
		&s.ProperNounLabels.Good, &s.ProperNounLabels.Junk, &s.ProperNounLabels.Partial, &s.ProperNounLabels.Unsure,
		&s.CostUSD, &s.CostPerChunkUSD, &s.LatencyMsP50, &s.LatencyMsP95, &s.LatencySource,
		&s.FirstActivityAt, &s.LastActivityAt); err != nil {
		return s, err
	}
	if stepVersion != nil {
		v := int(*stepVersion)
		s.StepVersion = &v
	}
	s.Labels, s.ProperNounLabels = s.Labels.withPrecision(), s.ProperNounLabels.withPrecision()
	if err := unmarshalOpt(issue, &s.IssueMix); err != nil {
		return s, fmt.Errorf("issue_mix: %w", err)
	}
	if err := unmarshalOpt(states, &s.StateMix); err != nil {
		return s, fmt.Errorf("state_mix: %w", err)
	}
	if err := unmarshalOpt(drops, &s.Drops); err != nil {
		return s, fmt.Errorf("drops: %w", err)
	}
	if err := unmarshalOpt(decisions, &s.Decisions); err != nil {
		return s, fmt.Errorf("decisions: %w", err)
	}
	if s.Decisions == nil {
		s.Decisions = []DecisionMix{}
	}
	return s, nil
}

// bucketRow is one decide_scorecard calibration element.
type bucketRow struct {
	Bucket   int   `json:"bucket"`
	Good     int64 `json:"good"`
	Junk     int64 `json:"junk"`
	Partial  int64 `json:"partial"`
	Unsure   int64 `json:"unsure"`
	Accepted int64 `json:"accepted"`
	Rejected int64 `json:"rejected"`
}

// pBuckets is the calibration buckets of the view: five of width 0.2.
const pBuckets = 5

// calibration expands the view's sparse buckets to all five, with rates:
// label precision (human == false) or the human accept rate.
func calibration(raw []bucketRow, human bool) []PBucket {
	out := make([]PBucket, pBuckets)
	for i := range out {
		out[i] = PBucket{Bucket: i + 1, Lo: float64(i) / pBuckets, Hi: float64(i+1) / pBuckets}
	}
	for _, r := range raw {
		if r.Bucket < 1 || r.Bucket > pBuckets {
			continue
		}
		b := &out[r.Bucket-1]
		b.Good, b.Junk, b.Partial, b.Unsure, b.Accepted, b.Rejected = r.Good, r.Junk, r.Partial, r.Unsure, r.Accepted, r.Rejected
	}
	for i := range out {
		b := &out[i]
		num, den := b.Good, b.Good+b.Junk+b.Partial
		if human {
			num, den = b.Accepted, b.Accepted+b.Rejected
		}
		if den > 0 {
			r := float64(num) / float64(den)
			b.Rate = &r
		}
	}
	return out
}

func scanDecideScore(row pgx.CollectableRow) (DecideScore, error) {
	var s DecideScore
	var params, oe, reasons, lo, lcal, hcal []byte
	var stepVersion *int32
	if err := row.Scan(&s.RecipeID, &s.ModelAlias, &s.ModelResolved, &s.PromptVersion, &stepVersion,
		&params, &s.RecipeCreated,
		&s.Decisions, &s.Revoked, &s.RevokeRate, &s.FindingsDecided, &s.Live, &s.Apply, &s.Hold, &s.Reject,
		&oe, &reasons, &lo, &lcal, &hcal,
		&s.HumanDecided, &s.HumanAgree, &s.HumanAgreement,
		&s.PrevRecipeID, &s.FlipOverlap, &s.Flips, &s.FlipRate,
		&s.Calls, &s.CallsCached, &s.CallErrors, &s.CostUSD, &s.CostPerCallUSD, &s.LatencyMsP50, &s.LatencyMsP95,
		&s.FirstDecisionAt, &s.LastActivityAt); err != nil {
		return s, err
	}
	if stepVersion != nil {
		v := int(*stepVersion)
		s.StepVersion = &v
	}
	if len(params) > 0 {
		s.Params = json.RawMessage(params)
	}
	if err := unmarshalOpt(oe, &s.OutcomeEvidence); err != nil {
		return s, fmt.Errorf("outcome_evidence: %w", err)
	}
	if err := unmarshalOpt(reasons, &s.Reasons); err != nil {
		return s, fmt.Errorf("reasons: %w", err)
	}
	var outcomes map[string]LabelTally
	if err := unmarshalOpt(lo, &outcomes); err != nil {
		return s, fmt.Errorf("label_outcomes: %w", err)
	}
	s.LabelOutcomes = make(map[string]LabelTally, len(outcomes))
	for k, v := range outcomes {
		s.LabelOutcomes[k] = v.withPrecision()
	}
	var lb, hb []bucketRow
	if err := unmarshalOpt(lcal, &lb); err != nil {
		return s, fmt.Errorf("label_calibration: %w", err)
	}
	if err := unmarshalOpt(hcal, &hb); err != nil {
		return s, fmt.Errorf("human_calibration: %w", err)
	}
	s.LabelCalibration, s.HumanCalibration = calibration(lb, false), calibration(hb, true)
	return s, nil
}

// unmarshalOpt decodes a nullable jsonb column; NULL leaves v untouched.
func unmarshalOpt(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}
