package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// Finding version history (CONTRACT §2.17 "Version history", migration 8).
//
// finding_events is the append-only patch log. Transitions are written by
// the database itself (a trigger on transcript_findings.patch_state), so no
// writer can change a finding without leaving a row; this file adds the rows
// only a caller knows — a decide recipe's verdicts and their revocation — the
// bulk state change the decide step uses, and the history reads behind blame
// and point-in-time reconstruction.

// Finding event kinds (finding_events.kind).
const (
	EventTransition = "transition"
	EventDecision   = "decision"
	EventRevoke     = "revoke"
)

// Decide outcomes (finding_events.outcome).
const (
	OutcomeApply  = "apply"
	OutcomeHold   = "hold"
	OutcomeReject = "reject"
)

// Evidence classes a decision may cite (finding_events.evidence).
const (
	EvidenceASINVerbatim = "asin_verbatim"
	EvidenceExactRepeat  = "exact_repeat"
	EvidenceNone         = "none"
)

// maxFindingHistory caps FindingHistory. A finding moves a handful of times;
// hitting the cap means something is looping, and the newest rows are kept.
const maxFindingHistory = 500

// automatedDeciderRe is the only decided_by the bulk path accepts: a decide
// recipe ("jev:<recipe_id>") or an undo of one ("revert:jev:<recipe_id>").
// Humans decide one finding at a time through SetPatchState, and the MCP
// surface prefixes every attribution with "mcp:", so it cannot match.
var automatedDeciderRe = regexp.MustCompile(`^(jev|revert:jev):[0-9a-f]{64}$`)

// ValidAutomatedDecider reports whether s is a decide-recipe attribution the
// bulk path accepts.
func ValidAutomatedDecider(s string) bool { return automatedDeciderRe.MatchString(s) }

// bulkTargets are the states a bulk change may write. Never applied — only a
// rebuild replaying an accepted finding reaches it — and never a maintenance
// state (stale, unanchorable, superseded).
var bulkTargets = []string{patch.StateAccepted, patch.StateRejected, patch.StateReverted, patch.StateProposed}

// BulkResult reports a bulk state change. Changed moved from the expected
// state; Skipped were not in it when the statement ran (someone decided them
// first, or the id does not exist) and were left alone. Both sorted.
type BulkResult struct {
	Changed []string
	Skipped []string
}

// validateBulk is the pre-SQL gate, the bulk twin of setPatchState's.
func validateBulk(from, to, decidedBy string) error {
	if !patch.CanTransition(from, to) || patch.IsMachineTransition(from, to) || !slices.Contains(bulkTargets, to) {
		return fmt.Errorf("%w: %s -> %s (bulk)", ErrIllegalTransition, from, to)
	}
	if !ValidAutomatedDecider(decidedBy) {
		return fmt.Errorf("%w: bulk decided_by %q is not jev:<recipe_id> or revert:jev:<recipe_id>",
			ErrIllegalTransition, decidedBy)
	}
	return nil
}

// setPatchStateBulkSQL is one guarded compare-and-swap over many findings.
//
//   - lk locks the candidates in id order — the order every bulk caller uses,
//     so two concurrent bulk changes cannot deadlock — and, under READ
//     COMMITTED, re-checks patch_state on a row another transaction changed
//     while it waited, dropping it.
//   - upd moves exactly the rows still in $2 and stamps the decision.
//     clock_timestamp(), not now(): decided_at is what ClearEmbeddingStale's
//     watermark compares against, and the statement's own time is closer to
//     its commit than the transaction's start.
//   - flag marks each changed finding's chunk embedding_stale when $5 (to is
//     accepted or reverted: the overlay gained or lost a correction), by the
//     same address as markChunkStaleForFindingSQL.
//
// The patch_state trigger records one transition event per changed row.
var setPatchStateBulkSQL = `
	WITH lk AS (
	    SELECT id FROM transcript_findings
	     WHERE id = ANY($1) AND patch_state = $2
	     ORDER BY id
	       FOR UPDATE
	), upd AS (
	    UPDATE transcript_findings f
	       SET patch_state = $3,
	           decided_at  = clock_timestamp(),
	           decided_by  = $4
	      FROM lk
	     WHERE f.id = lk.id AND f.patch_state = $2
	    RETURNING f.id, f.transcript_id, f.chunk_index, f.chunk_id
	), flag AS (
	    UPDATE transcript_chunks c
	       SET embedding_stale = true
	      FROM upd f
	     WHERE $5
	       AND ((f.chunk_index IS NOT NULL
	             AND c.transcript_id = f.transcript_id
	             AND c.chunk_index = f.chunk_index)
	            OR (f.chunk_index IS NULL AND c.id = f.chunk_id))
	    RETURNING c.id
	)
	SELECT id::text FROM upd
`

// SetPatchStateBulk moves every finding in ids from `from` to `to` in one
// transaction, attributed to an automated decider (CONTRACT §2.17
// "Automated decisions"). It is the decide step's path; a person deciding
// one finding uses SetPatchState.
//
// The transition is validated before any SQL: legal in patch.CanTransition,
// not a maintenance move, `to` one of accepted/rejected/reverted/proposed
// (never applied), and decidedBy jev:<recipe_id> or revert:jev:<recipe_id>.
// Rows not in `from` are reported in Skipped rather than failing the batch —
// per row it is the same compare-and-swap as SetPatchState.
func (db *DB) SetPatchStateBulk(ctx context.Context, from, to, decidedBy string, ids []string) (BulkResult, error) {
	return setPatchStateBulk(ctx, db.pool, from, to, decidedBy, ids)
}

func setPatchStateBulk(ctx context.Context, b txBeginner, from, to, decidedBy string, ids []string) (BulkResult, error) {
	if err := validateBulk(from, to, decidedBy); err != nil {
		return BulkResult{}, err
	}
	if len(ids) == 0 {
		return BulkResult{}, nil
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return BulkResult{}, fmt.Errorf("begin bulk patch-state tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := setPatchStateBulkTx(ctx, tx, from, to, decidedBy, ids)
	if err != nil {
		return BulkResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BulkResult{}, fmt.Errorf("commit bulk patch-state: %w", err)
	}
	return res, nil
}

// setPatchStateBulkTx is SetPatchStateBulk inside a caller's transaction —
// the decide write, which locks findings (id order) then chunks, re-checks
// them, and records its decision events alongside the state change.
func setPatchStateBulkTx(ctx context.Context, q rowQuerier, from, to, decidedBy string, ids []string) (BulkResult, error) {
	if err := validateBulk(from, to, decidedBy); err != nil {
		return BulkResult{}, err
	}
	want := slices.Clone(ids)
	slices.Sort(want)
	want = slices.Compact(want)
	if len(want) == 0 {
		return BulkResult{}, nil
	}

	flag := to == patch.StateAccepted || to == patch.StateReverted
	rows, err := q.Query(ctx, setPatchStateBulkSQL, want, from, to, decidedBy, flag)
	if err != nil {
		return BulkResult{}, fmt.Errorf("bulk patch state %s -> %s (%d findings): %w", from, to, len(want), err)
	}
	changed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return BulkResult{}, fmt.Errorf("bulk patch state %s -> %s: %w", from, to, err)
	}
	slices.Sort(changed)

	res := BulkResult{Changed: changed}
	for _, id := range want {
		if _, ok := slices.BinarySearch(changed, id); !ok {
			res.Skipped = append(res.Skipped, id)
		}
	}
	return res, nil
}

// EventContext attributes the transitions a transaction makes that are not
// decisions (CONTRACT §2.17 "Version history"): a stamped decision is
// attributed to its decided_by, anything else to Actor, else 'system'.
// RecipeID, when set, is recorded on every transition of the transaction.
type EventContext struct {
	Actor    string
	RecipeID string
}

// setEventContextSQL sets the transaction-local settings the transition
// trigger reads. is_local = true: they end with the transaction, so a pooled
// connection never carries one transaction's attribution into the next.
var setEventContextSQL = `SELECT set_config('earmark.actor', $1, true), set_config('earmark.recipe_id', $2, true)`

// SetEventContext attributes the rest of tx's transitions. Call it inside the
// transaction that changes the findings.
func SetEventContext(ctx context.Context, tx execer, ec EventContext) error {
	if _, err := tx.Exec(ctx, setEventContextSQL, ec.Actor, ec.RecipeID); err != nil {
		return fmt.Errorf("set finding event context: %w", err)
	}
	return nil
}

// DecisionEvent is one decide-recipe verdict on one finding. A hold is a
// decision whose finding stays proposed.
type DecisionEvent struct {
	FindingID       string
	RecipeID        string // the decide recipe; the event's actor is "jev:" + RecipeID
	Outcome         string // OutcomeApply | OutcomeHold | OutcomeReject
	Reason          string
	P               *float64 // the model's probability, when one was asked
	Evidence        string   // Evidence*; "" = not assessed
	FnCallID        *int64   // the fn_calls row that answered
	ChunkTextSHA256 string   // the chunk revision the decider saw
}

func (e DecisionEvent) validate() error {
	switch {
	case e.FindingID == "":
		return errors.New("decision event: finding id is empty")
	case !sha256HexRe.MatchString(e.RecipeID):
		return fmt.Errorf("decision event %s: recipe_id %q is not lowercase hex sha256", e.FindingID, e.RecipeID)
	case !slices.Contains([]string{OutcomeApply, OutcomeHold, OutcomeReject}, e.Outcome):
		return fmt.Errorf("decision event %s: unknown outcome %q", e.FindingID, e.Outcome)
	case e.Reason == "":
		return fmt.Errorf("decision event %s: reason is empty", e.FindingID)
	case e.P != nil && (math.IsNaN(*e.P) || *e.P < 0 || *e.P > 1):
		return fmt.Errorf("decision event %s: p %v is outside [0,1]", e.FindingID, *e.P)
	case e.Evidence != "" && !slices.Contains([]string{EvidenceASINVerbatim, EvidenceExactRepeat, EvidenceNone}, e.Evidence):
		return fmt.Errorf("decision event %s: unknown evidence %q", e.FindingID, e.Evidence)
	}
	return nil
}

// insertDecisionEventsSQL appends the decisions in input order. transcript_id
// and issue_type come from the finding itself, so they cannot disagree with
// it; a finding id that does not exist yields no row (InsertDecisionEvents
// refuses that).
var insertDecisionEventsSQL = `
	INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor,
	                            recipe_id, p, evidence, fn_call_id, chunk_text_sha256, issue_type)
	SELECT f.id, f.transcript_id, 'decision', d.outcome, d.reason, 'jev:' || d.recipe_id,
	       d.recipe_id, d.p, NULLIF(d.evidence, ''), d.fn_call_id, NULLIF(d.chunk_sha, ''), f.issue_type
	  FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::float8[], $6::text[],
	              $7::bigint[], $8::text[])
	       WITH ORDINALITY AS d(finding_id, recipe_id, outcome, reason, p, evidence, fn_call_id, chunk_sha, ord)
	  JOIN transcript_findings f ON f.id = d.finding_id
	 ORDER BY d.ord
	RETURNING id
`

// InsertDecisionEvents appends one decision event per element, in order, and
// returns their ids. All or nothing: an invalid event, or one naming a
// finding that does not exist, fails the call (and, the caller's
// transaction being aborted, the batch).
func InsertDecisionEvents(ctx context.Context, tx rowQuerier, evs []DecisionEvent) ([]int64, error) {
	if len(evs) == 0 {
		return nil, nil
	}
	var (
		findings  = make([]string, len(evs))
		recipes   = make([]string, len(evs))
		outcomes  = make([]string, len(evs))
		reasons   = make([]string, len(evs))
		ps        = make([]*float64, len(evs))
		evidence  = make([]string, len(evs))
		fnCalls   = make([]*int64, len(evs))
		chunkSHAs = make([]string, len(evs))
	)
	for i, e := range evs {
		if err := e.validate(); err != nil {
			return nil, err
		}
		findings[i], recipes[i], outcomes[i], reasons[i] = e.FindingID, e.RecipeID, e.Outcome, e.Reason
		ps[i], evidence[i], fnCalls[i], chunkSHAs[i] = e.P, e.Evidence, e.FnCallID, e.ChunkTextSHA256
	}
	rows, err := tx.Query(ctx, insertDecisionEventsSQL,
		findings, recipes, outcomes, reasons, ps, evidence, fnCalls, chunkSHAs)
	if err != nil {
		return nil, fmt.Errorf("insert %d decision events: %w", len(evs), err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("insert %d decision events: %w", len(evs), err)
	}
	if len(ids) != len(evs) {
		return nil, fmt.Errorf("insert decision events: %d of %d findings exist", len(ids), len(evs))
	}
	return ids, nil
}

// revokeEventsSQL appends one revoke event per live decision of a recipe.
// The partial unique index finding_events_revokes_idx makes a concurrent
// second revoke of the same decision a no-op rather than a duplicate.
var revokeEventsSQL = `
	INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
	SELECT e.finding_id, e.transcript_id, 'revoke', 'revoke decide recipe ' || e.recipe_id, $2,
	       e.recipe_id, e.id
	  FROM finding_events e
	 WHERE e.kind = 'decision' AND e.recipe_id = $1
	   AND NOT EXISTS (SELECT 1 FROM finding_events v
	                    WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
	 ORDER BY e.id
	ON CONFLICT (revokes_event_id) WHERE kind = 'revoke' DO NOTHING
`

// RevokeEvents withdraws every live decision recipeID made, by appending
// revoke events attributed to by; the decisions themselves are never edited.
// It records the undo only — moving the findings back is SetPatchStateBulk
// with a revert:jev:<recipe_id> decider, in the same transaction. Returns how
// many decisions it revoked.
func RevokeEvents(ctx context.Context, tx execer, recipeID, by string) (int64, error) {
	if !sha256HexRe.MatchString(recipeID) {
		return 0, fmt.Errorf("revoke events: recipe_id %q is not lowercase hex sha256", recipeID)
	}
	if by == "" {
		return 0, errors.New("revoke events: by is empty")
	}
	tag, err := tx.Exec(ctx, revokeEventsSQL, recipeID, by)
	if err != nil {
		return 0, fmt.Errorf("revoke decisions of recipe %s: %w", recipeID, err)
	}
	return tag.RowsAffected(), nil
}

// FindingEvent is one finding_events row. Optional columns are "" / nil when
// NULL.
type FindingEvent struct {
	ID              int64
	FindingID       string
	TranscriptID    string
	Kind            string
	FromState       string
	ToState         string
	Outcome         string
	Reason          string
	Actor           string
	RecipeID        string
	P               *float64
	Evidence        string
	FnCallID        *int64
	ChunkTextSHA256 string
	IssueType       string
	RevokesEventID  *int64
	Revoked         bool // a decision a later revoke event withdrew
	CreatedAt       time.Time
}

// findingHistorySQL is a finding's log, oldest first: the newest
// maxFindingHistory rows by the (finding_id, id DESC) index, re-ordered.
var findingHistorySQL = `
	SELECT * FROM (
	    SELECT e.id, e.finding_id::text, COALESCE(e.transcript_id::text, ''), e.kind,
	           COALESCE(e.from_state, ''), COALESCE(e.to_state, ''), COALESCE(e.outcome, ''),
	           COALESCE(e.reason, ''), e.actor, COALESCE(e.recipe_id, ''), e.p,
	           COALESCE(e.evidence, ''), e.fn_call_id, COALESCE(e.chunk_text_sha256, ''),
	           COALESCE(e.issue_type, ''), e.revokes_event_id,
	           EXISTS (SELECT 1 FROM finding_events v
	                    WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id) AS revoked,
	           e.created_at
	      FROM finding_events e
	     WHERE e.finding_id = $1
	     ORDER BY e.id DESC
	     LIMIT $2
	) h ORDER BY id
`

// FindingHistory returns a finding's events, oldest first — the blame for
// one patch: every state it passed through, who moved it, and every verdict
// a decide recipe gave on it (and whether that verdict was revoked).
func (db *DB) FindingHistory(ctx context.Context, findingID string) ([]FindingEvent, error) {
	return findingHistory(ctx, db.pool, findingID)
}

func findingHistory(ctx context.Context, q rowQuerier, findingID string) ([]FindingEvent, error) {
	rows, err := q.Query(ctx, findingHistorySQL, findingID, maxFindingHistory)
	if err != nil {
		return nil, fmt.Errorf("finding %s history: %w", findingID, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FindingEvent, error) {
		var e FindingEvent
		err := r.Scan(&e.ID, &e.FindingID, &e.TranscriptID, &e.Kind, &e.FromState, &e.ToState,
			&e.Outcome, &e.Reason, &e.Actor, &e.RecipeID, &e.P, &e.Evidence, &e.FnCallID,
			&e.ChunkTextSHA256, &e.IssueType, &e.RevokesEventID, &e.Revoked, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan finding %s history: %w", findingID, err)
	}
	return out, nil
}

// PatchSetEntry is one finding in a transcript's patch set at a point in
// time: its state then, and the transition that put it there.
type PatchSetEntry struct {
	FindingID string
	State     string // accepted or applied
	Since     time.Time
	Actor     string
}

// patchSetAtSQL reconstructs which findings were in a transcript's overlay
// at $2: each finding's latest transition at or before $2, kept when it was
// into an overlay state ($3). A finding with no transition by then was
// proposed (findings are inserted proposed; anything else records an insert
// transition), so it was not in the set.
var patchSetAtSQL = `
	SELECT finding_id, to_state, created_at, actor FROM (
	    SELECT DISTINCT ON (e.finding_id) e.finding_id::text AS finding_id, e.to_state,
	           e.created_at, e.actor
	      FROM finding_events e
	     WHERE e.transcript_id = $1 AND e.kind = 'transition' AND e.created_at <= $2
	     ORDER BY e.finding_id, e.created_at DESC, e.id DESC
	) last
	 WHERE to_state = ANY($3)
	 ORDER BY finding_id
`

// TranscriptPatchSetAt returns the findings that were accepted or applied on
// transcriptID at time t, derived from the event log alone — the patch set
// that, replayed onto the pristine text, gives the transcript as it read
// then. Bounded by one transcript's findings. Event times are the moment
// each transition ran (clock_timestamp), not its transaction's commit.
func (db *DB) TranscriptPatchSetAt(ctx context.Context, transcriptID string, t time.Time) ([]PatchSetEntry, error) {
	return transcriptPatchSetAt(ctx, db.pool, transcriptID, t)
}

func transcriptPatchSetAt(ctx context.Context, q rowQuerier, transcriptID string, t time.Time) ([]PatchSetEntry, error) {
	rows, err := q.Query(ctx, patchSetAtSQL, transcriptID, t, overlayStates)
	if err != nil {
		return nil, fmt.Errorf("transcript %s patch set at %s: %w", transcriptID, t.Format(time.RFC3339), err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PatchSetEntry, error) {
		var e PatchSetEntry
		err := r.Scan(&e.FindingID, &e.State, &e.Since, &e.Actor)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan transcript %s patch set: %w", transcriptID, err)
	}
	return out, nil
}

// ApplyResult reports one ApplyDecisions write. Every finding id given lands
// in exactly one list, each sorted.
type ApplyResult struct {
	Accepted []string // apply: proposed → accepted
	Rejected []string // reject: proposed → rejected
	Held     []string // hold: recorded, left proposed
	Skipped  []string // not written: locked by someone else, no longer proposed, or its chunk changed
}

// applyDecisionsLockSQL locks the findings first (the repo-wide order:
// findings, then chunks) in id order, SKIP LOCKED: a finding a reviewer, a
// reanchor run or another decide shard holds right now is skipped and
// re-decided next run, never waited on. Only proposed findings are decided.
var applyDecisionsLockSQL = `
	SELECT id::text FROM transcript_findings
	 WHERE id = ANY($1) AND patch_state = 'proposed'
	 ORDER BY id
	   FOR UPDATE SKIP LOCKED
`

// applyDecisionsChunksSQL reads, FOR SHARE, the pristine-text hash of each
// locked finding's chunk (addressed like markChunkStaleForFindingSQL), so a
// rebuild cannot change the text between this check and the commit. Chunks
// are locked in chunk-id order, after every finding lock.
var applyDecisionsChunksSQL = `
	SELECT f.id::text,
	       encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex')
	  FROM transcript_findings f
	  JOIN transcript_chunks c
	    ON (f.chunk_index IS NOT NULL AND c.transcript_id = f.transcript_id AND c.chunk_index = f.chunk_index)
	    OR (f.chunk_index IS NULL AND c.id = f.chunk_id)
	 WHERE f.id = ANY($1)
	 ORDER BY c.id
	   FOR SHARE OF c
`

// ApplyDecisions is the decide step's write (CONTRACT §2.17 "Automated
// decisions"): in one short transaction it locks the decided findings then
// their chunks, drops any finding that is locked elsewhere, no longer
// proposed, or whose chunk no longer hashes to the event's ChunkTextSHA256
// (the decider saw other text), then records the remaining decision events
// and moves apply → accepted and reject → rejected as "jev:<recipeID>". Holds
// are recorded and stay proposed. Model calls belong before this call, never
// inside it.
//
// Every event must belong to recipeID, the recipe must be registered, and a
// finding may appear once.
func (db *DB) ApplyDecisions(ctx context.Context, recipeID string, evs []DecisionEvent) (ApplyResult, error) {
	return applyDecisions(ctx, db.pool, recipeID, evs)
}

func applyDecisions(ctx context.Context, b txBeginner, recipeID string, evs []DecisionEvent) (ApplyResult, error) {
	if !sha256HexRe.MatchString(recipeID) {
		return ApplyResult{}, fmt.Errorf("apply decisions: recipe_id %q is not lowercase hex sha256", recipeID)
	}
	ids := make([]string, 0, len(evs))
	seen := make(map[string]bool, len(evs))
	for _, e := range evs {
		if err := e.validate(); err != nil {
			return ApplyResult{}, err
		}
		if e.RecipeID != recipeID {
			return ApplyResult{}, fmt.Errorf("apply decisions: event for %s is by recipe %s, not %s", e.FindingID, e.RecipeID, recipeID)
		}
		if seen[e.FindingID] {
			return ApplyResult{}, fmt.Errorf("apply decisions: finding %s decided twice", e.FindingID)
		}
		seen[e.FindingID] = true
		ids = append(ids, e.FindingID)
	}
	if len(ids) == 0 {
		return ApplyResult{}, nil
	}
	slices.Sort(ids)

	tx, err := b.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin apply-decisions tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, applyDecisionsLockSQL, ids)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("lock decided findings: %w", err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return ApplyResult{}, fmt.Errorf("lock decided findings: %w", err)
	}
	rows, err = tx.Query(ctx, applyDecisionsChunksSQL, locked)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("lock decided chunks: %w", err)
	}
	chunkSHA := make(map[string]string, len(locked))
	var fid, sha string
	if _, err := pgx.ForEachRow(rows, []any{&fid, &sha}, func() error {
		chunkSHA[fid] = sha
		return nil
	}); err != nil {
		return ApplyResult{}, fmt.Errorf("lock decided chunks: %w", err)
	}

	var (
		res     ApplyResult
		write   []DecisionEvent
		isReady = make(map[string]bool, len(locked))
	)
	for _, id := range locked {
		isReady[id] = true
	}
	for _, e := range evs {
		cur, hasChunk := chunkSHA[e.FindingID]
		// A decision about text the chunk no longer holds is not written; an
		// event that names no hash still needs the chunk to exist.
		if !isReady[e.FindingID] || !hasChunk || (e.ChunkTextSHA256 != "" && e.ChunkTextSHA256 != cur) {
			res.Skipped = append(res.Skipped, e.FindingID)
			continue
		}
		write = append(write, e)
		switch e.Outcome {
		case OutcomeApply:
			res.Accepted = append(res.Accepted, e.FindingID)
		case OutcomeReject:
			res.Rejected = append(res.Rejected, e.FindingID)
		default:
			res.Held = append(res.Held, e.FindingID)
		}
	}
	if len(write) > 0 {
		if _, err := InsertDecisionEvents(ctx, tx, write); err != nil {
			return ApplyResult{}, err
		}
	}
	decider := "jev:" + recipeID
	for _, mv := range []struct {
		to  string
		ids []string
	}{{patch.StateAccepted, res.Accepted}, {patch.StateRejected, res.Rejected}} {
		if len(mv.ids) == 0 {
			continue
		}
		br, err := setPatchStateBulkTx(ctx, tx, patch.StateProposed, mv.to, decider, mv.ids)
		if err != nil {
			return ApplyResult{}, err
		}
		// The findings are locked and were proposed, so nothing can be skipped.
		if len(br.Skipped) > 0 {
			return ApplyResult{}, fmt.Errorf("apply decisions: %d locked finding(s) did not move to %s", len(br.Skipped), mv.to)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit apply decisions: %w", err)
	}
	for _, l := range [][]string{res.Accepted, res.Rejected, res.Held, res.Skipped} {
		slices.Sort(l)
	}
	return res, nil
}
