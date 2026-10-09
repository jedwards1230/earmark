package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// The decide step's full run and its undo (CONTRACT §2.19 "earmark decide
// --yes", "earmark decide revert"). Selection here is read-only; the writes
// go through ApplyDecisions / ApplyRecheckDecisions (decisions) and
// RevertDecisions (undo), each in short transactions.

// MaxDecideWorkPage caps one DecideWork page.
const MaxDecideWorkPage = 1000

// DecideWorkScope selects findings a --yes run must decide under RecipeID.
type DecideWorkScope struct {
	// RecipeID is the decide recipe the run decides under.
	RecipeID string
	// RetryReason is the hold reason that is retried under the same recipe
	// (decide.ReasonJevUnavailable); every other verdict of RecipeID is final.
	RetryReason string
	Book        string
	IssueType   string
	// Shard / Shards partition the work by transcript:
	// hashtext(transcript_id) mod Shards = Shard. Shards 0 = unsharded.
	Shard, Shards int
	// After is the keyset cursor: only ids greater than it ("" = start).
	After string
	// Limit is the page size, 1..MaxDecideWorkPage.
	Limit int
}

func (s DecideWorkScope) validate() error {
	switch {
	case !sha256HexRe.MatchString(s.RecipeID):
		return fmt.Errorf("decide work: recipe_id %q is not lowercase hex sha256", s.RecipeID)
	case s.RetryReason == "":
		return errors.New("decide work: retry reason is required")
	case s.Shards < 0 || (s.Shards > 0 && (s.Shard < 0 || s.Shard >= s.Shards)):
		return fmt.Errorf("decide work: shard %d/%d out of range", s.Shard, s.Shards)
	case s.Limit < 1 || s.Limit > MaxDecideWorkPage:
		return fmt.Errorf("decide work: limit must be 1..%d", MaxDecideWorkPage)
	}
	return nil
}

// decideWorkSQL is the --yes work list, in id order after a keyset cursor.
//
// In scope: anchored judge findings that are
//   - proposed, or
//   - accepted/applied by ANOTHER decide recipe (decided_by 'jev:<other>') —
//     the re-check under a new recipe (CONTRACT §2.19). A person's accept is
//     never re-checked;
//
// and whose latest live (unrevoked) decision is absent, is by another recipe,
// or is this recipe's retryable hold ($8, jev_unavailable). Every other
// verdict of this recipe is final, so a re-run does not ask again.
//
// The shard is hashtext(transcript_id) mod $4, folded to be non-negative
// (hashtext is a signed int4; abs() would overflow on its minimum).
//
//	$1 recipe  $2 book pattern ('' = all)  $3 issue type ('' = all)
//	$4 shards (0 = all)  $5 shard  $6 cursor (NULL = start)  $7 limit  $8 retry reason
var decideWorkSQL = `
	SELECT f.id::text, f.transcript_id::text, f.file_path, f.issue_type, f.original_text,
	       COALESCE(f.suggested_correction, ''), f.confidence, f.chunk_index,
	       f.anchor_offset, COALESCE(f.anchor_occurrence, -1), f.chunk_text_sha256,
	       f.patch_state, COALESCE(f.decided_by, '')
	  FROM transcript_findings f
	  LEFT JOIN LATERAL (
	        SELECT e.recipe_id, e.outcome, e.reason
	          FROM finding_events e
	         WHERE e.finding_id = f.id AND e.kind = 'decision'
	           AND NOT EXISTS (SELECT 1 FROM finding_events v
	                            WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
	         ORDER BY e.id DESC
	         LIMIT 1) d ON true
	 WHERE f.origin = 'judge'
	   AND f.transcript_id IS NOT NULL
	   AND f.chunk_index IS NOT NULL
	   AND f.anchor_offset IS NOT NULL
	   AND f.chunk_text_sha256 IS NOT NULL
	   AND (f.patch_state = 'proposed'
	        OR (f.patch_state IN ('accepted', 'applied')
	            AND f.decided_by ~ '^jev:[0-9a-f]{64}$' AND f.decided_by <> 'jev:' || $1))
	   AND (d.recipe_id IS NULL OR d.recipe_id <> $1 OR (d.outcome = 'hold' AND d.reason = $8))
	   AND ($2 = '' OR f.file_path ILIKE $2)
	   AND ($3 = '' OR f.issue_type = $3)
	   AND ($4 = 0 OR mod(mod(hashtext(f.transcript_id::text)::bigint, $4) + $4, $4) = $5)
	   AND ($6::uuid IS NULL OR f.id > $6::uuid)
	 ORDER BY f.id
	 LIMIT $7`

// DecideWork returns one page of the --yes work list (read-only). A finding
// is a re-check when its PatchState is accepted or applied.
func (db *DB) DecideWork(ctx context.Context, s DecideWorkScope) ([]DecideFinding, error) {
	return decideWork(ctx, db.pool, s)
}

func decideWork(ctx context.Context, q rowQuerier, s DecideWorkScope) ([]DecideFinding, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	book := ""
	if strings.TrimSpace(s.Book) != "" {
		book = likePattern(s.Book)
	}
	var after *string
	if s.After != "" {
		after = &s.After
	}
	rows, err := q.Query(ctx, decideWorkSQL, s.RecipeID, book, s.IssueType, s.Shards, s.Shard, after, s.Limit, s.RetryReason)
	if err != nil {
		return nil, fmt.Errorf("decide work: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanDecideFinding)
	if err != nil {
		return nil, fmt.Errorf("decide work: %w", err)
	}
	return out, nil
}

// ─── Revert ──────────────────────────────────────────────────────────────────

// RevertScope selects the automated decisions to undo. At least one field is
// set; set fields narrow together (AND).
type RevertScope struct {
	// RecipeID undoes one decide recipe.
	RecipeID string
	// FindingID undoes one finding.
	FindingID string
	// Since undoes decisions made at or after it (zero = no bound).
	Since time.Time
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (s RevertScope) validate() error {
	switch {
	case s.RecipeID == "" && s.FindingID == "" && s.Since.IsZero():
		return errors.New("revert: give a recipe, a finding or a since time")
	case s.RecipeID != "" && !sha256HexRe.MatchString(s.RecipeID):
		return fmt.Errorf("revert: recipe_id %q is not lowercase hex sha256", s.RecipeID)
	case s.FindingID != "" && !uuidRe.MatchString(s.FindingID):
		return fmt.Errorf("revert: finding id %q is not a uuid", s.FindingID)
	}
	return nil
}

func (s RevertScope) args() []any {
	var finding *string
	if s.FindingID != "" {
		finding = &s.FindingID
	}
	var since *time.Time
	if !s.Since.IsZero() {
		since = &s.Since
	}
	return []any{s.RecipeID, finding, since}
}

// revertTransitions maps a state a decide recipe left a finding in to the
// state its undo moves it to: an applied fix is reverted, an accept not yet
// replayed is rejected, a reject goes back to proposed, and a re-check's
// revert (applied → reverted by a newer recipe) goes back to proposed too —
// proposed → applied is illegal, so the older recipe's fix is not restored
// directly; the finding is decided again.
var revertTransitions = map[string]string{
	patch.StateApplied:  patch.StateReverted,
	patch.StateAccepted: patch.StateRejected,
	patch.StateRejected: patch.StateProposed,
	patch.StateReverted: patch.StateProposed,
}

// revertCandidatesSQL lists the findings a decide recipe last moved, in id
// order: decided_by 'jev:<recipe>' (a person's decision, or an undo already
// done — 'revert:jev:…' — never matches, which is what makes revert
// idempotent).
//
//	$1 recipe ('' = any)  $2 finding (NULL = any)  $3 since (NULL = any)
var revertCandidatesSQL = `
	SELECT f.id::text, f.patch_state, substring(f.decided_by FROM 5)
	  FROM transcript_findings f
	 WHERE f.decided_by ~ '^jev:[0-9a-f]{64}$'
	   AND f.patch_state IN ('applied', 'accepted', 'rejected', 'reverted')
	   AND ($1 = '' OR f.decided_by = 'jev:' || $1)
	   AND ($2::uuid IS NULL OR f.id = $2::uuid)
	   AND ($3::timestamptz IS NULL OR f.decided_at >= $3::timestamptz)
	 ORDER BY f.id`

// revertDecisionsCountSQL counts the live decisions the scope would revoke.
var revertDecisionsCountSQL = `
	SELECT count(*) FROM finding_events e
	 WHERE e.kind = 'decision'
	   AND ($1 = '' OR e.recipe_id = $1)
	   AND ($2::uuid IS NULL OR e.finding_id = $2::uuid)
	   AND ($3::timestamptz IS NULL OR e.created_at >= $3::timestamptz)
	   AND NOT EXISTS (SELECT 1 FROM finding_events v
	                    WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`

// revokeScopeSQL appends one revoke per live decision in scope, attributed to
// the undo of the decision's own recipe. The unique index on
// revokes_event_id makes a concurrent second revoke a no-op.
var revokeScopeSQL = `
	INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
	SELECT e.finding_id, e.transcript_id, 'revoke', 'decide revert', 'revert:jev:' || e.recipe_id,
	       e.recipe_id, e.id
	  FROM finding_events e
	 WHERE e.kind = 'decision'
	   AND ($1 = '' OR e.recipe_id = $1)
	   AND ($2::uuid IS NULL OR e.finding_id = $2::uuid)
	   AND ($3::timestamptz IS NULL OR e.created_at >= $3::timestamptz)
	   AND NOT EXISTS (SELECT 1 FROM finding_events v
	                    WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
	 ORDER BY e.id
	ON CONFLICT (revokes_event_id) WHERE kind = 'revoke' DO NOTHING`

// reflagChunksSQL marks the chunk of every listed finding embedding_stale
// (addressed like markChunkStaleForFindingSQL). It runs after the revert
// commits: a rebuild that read the overlay before the revert and clears the
// flag after it would otherwise keep the undone fix in the projection
// (ClearEmbeddingStale's watermark only sees decided_at). Costs at most one
// extra re-embed per chunk.
var reflagChunksSQL = `
	UPDATE transcript_chunks c
	   SET embedding_stale = true
	  FROM transcript_findings f
	 WHERE f.id = ANY($1)
	   AND ((f.chunk_index IS NOT NULL AND c.transcript_id = f.transcript_id AND c.chunk_index = f.chunk_index)
	        OR (f.chunk_index IS NULL AND c.id = f.chunk_id))`

// maxRevertBatch caps the findings one revert transaction moves.
const maxRevertBatch = 500

// RevertReport counts an undo. Transitions is keyed "from->to".
type RevertReport struct {
	DryRun      bool           `json:"dry_run"`
	Transitions map[string]int `json:"transitions"`
	// Moved counts findings moved (apply) or that would move (dry run).
	Moved int `json:"moved"`
	// Skipped counts candidates another writer changed first (apply only).
	Skipped int `json:"skipped"`
	// Revoked counts decisions revoked (or that would be).
	Revoked int64 `json:"revoked"`
	// Reflagged counts chunks flagged embedding_stale by the sweep.
	Reflagged int64 `json:"reflagged"`
}

// RevertDecisions undoes automated decisions in scope (CONTRACT §2.19
// "earmark decide revert"). Dry run unless apply: it counts the transitions
// and the decisions it would revoke. With apply, in batches of at most
// maxRevertBatch findings, it moves applied → reverted, accepted → rejected,
// rejected → proposed and (re-check) reverted → proposed with decided_by
// 'revert:jev:<recipe>' — each a transition event — then revokes every live
// decision in scope, then re-flags the moved findings' chunks. Idempotent: an
// undone finding no longer carries a jev decider, and a revoked decision is
// not revoked twice. A person's decision is never touched.
func (db *DB) RevertDecisions(ctx context.Context, s RevertScope, apply bool) (RevertReport, error) {
	return revertDecisions(ctx, db.pool, s, apply)
}

type revertCandidate struct {
	id, state, recipe string
}

// revertPool is the pool slice revertDecisions needs (*pgxpool.Pool and
// pgxmock satisfy it).
type revertPool interface {
	txBeginner
	rowScanner
	execer
}

func revertDecisions(ctx context.Context, b revertPool, s RevertScope, apply bool) (RevertReport, error) {
	if err := s.validate(); err != nil {
		return RevertReport{}, err
	}
	rep := RevertReport{DryRun: !apply, Transitions: map[string]int{}}
	rows, err := b.Query(ctx, revertCandidatesSQL, s.args()...)
	if err != nil {
		return rep, fmt.Errorf("revert candidates: %w", err)
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (revertCandidate, error) {
		var c revertCandidate
		err := r.Scan(&c.id, &c.state, &c.recipe)
		return c, err
	})
	if err != nil {
		return rep, fmt.Errorf("revert candidates: %w", err)
	}
	if !apply {
		for _, c := range cands {
			rep.Transitions[c.state+"->"+revertTransitions[c.state]]++
		}
		rep.Moved = len(cands)
		if err := b.QueryRow(ctx, revertDecisionsCountSQL, s.args()...).Scan(&rep.Revoked); err != nil {
			return rep, fmt.Errorf("count decisions to revoke: %w", err)
		}
		return rep, nil
	}

	for start := 0; start < len(cands); start += maxRevertBatch {
		batch := cands[start:min(start+maxRevertBatch, len(cands))]
		moved, skipped, err := revertBatch(ctx, b, batch, rep.Transitions)
		if err != nil {
			return rep, err
		}
		rep.Moved += len(moved)
		rep.Skipped += skipped
		if len(moved) > 0 {
			tag, err := b.Exec(ctx, reflagChunksSQL, moved)
			if err != nil {
				return rep, fmt.Errorf("re-flag reverted chunks: %w", err)
			}
			rep.Reflagged += tag.RowsAffected()
		}
	}
	tag, err := b.Exec(ctx, revokeScopeSQL, s.args()...)
	if err != nil {
		return rep, fmt.Errorf("revoke decisions: %w", err)
	}
	rep.Revoked = tag.RowsAffected()
	return rep, nil
}

// revertBatch moves one batch in one transaction, grouped by (state, recipe):
// each group is one guarded bulk change that skips rows another writer moved
// since they were listed.
func revertBatch(ctx context.Context, b txBeginner, batch []revertCandidate, tally map[string]int) (moved []string, skipped int, err error) {
	type group struct{ state, recipe string }
	groups := map[group][]string{}
	for _, c := range batch {
		g := group{c.state, c.recipe}
		groups[g] = append(groups[g], c.id)
	}
	keys := make([]group, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	slices.SortFunc(keys, func(a, b group) int {
		if c := strings.Compare(a.state, b.state); c != 0 {
			return c
		}
		return strings.Compare(a.recipe, b.recipe)
	})

	tx, err := b.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("begin revert tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	counts := map[string]int{}
	for _, g := range keys {
		to := revertTransitions[g.state]
		br, err := setPatchStateBulkTx(ctx, tx, g.state, to, "revert:jev:"+g.recipe, groups[g])
		if err != nil {
			return nil, 0, err
		}
		moved = append(moved, br.Changed...)
		skipped += len(br.Skipped)
		counts[g.state+"->"+to] += len(br.Changed)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit revert: %w", err)
	}
	for k, n := range counts {
		tally[k] += n
	}
	return moved, skipped, nil
}

// ─── Metric ──────────────────────────────────────────────────────────────────

// DecisionCount is one earmark_decisions series: findings whose latest live
// decision has these labels.
type DecisionCount struct {
	Outcome   string
	IssueType string
	Evidence  string
	Recipe    string
	N         int64
}

// decisionCountsSQL groups each finding's latest unrevoked decision (findings
// a requeue superseded excluded). One aggregate, run on a slow timer.
var decisionCountsSQL = `
	SELECT d.outcome, COALESCE(d.issue_type, ''), COALESCE(d.evidence, ''), d.recipe_id, count(*)
	  FROM (SELECT DISTINCT ON (e.finding_id) e.finding_id, e.outcome, e.issue_type, e.evidence, e.recipe_id
	          FROM finding_events e
	         WHERE e.kind = 'decision'
	           AND NOT EXISTS (SELECT 1 FROM finding_events v
	                            WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
	         ORDER BY e.finding_id, e.id DESC) d
	  JOIN transcript_findings f ON f.id = d.finding_id AND f.patch_state <> 'superseded'
	 GROUP BY 1, 2, 3, 4
	 ORDER BY 4, 1, 2, 3`

// DecisionCounts backs earmark_decisions (CONTRACT §2.16).
func (db *DB) DecisionCounts(ctx context.Context) ([]DecisionCount, error) {
	return decisionCounts(ctx, db.pool)
}

func decisionCounts(ctx context.Context, q rowQuerier) ([]DecisionCount, error) {
	rows, err := q.Query(ctx, decisionCountsSQL)
	if err != nil {
		return nil, fmt.Errorf("count decisions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DecisionCount, error) {
		var c DecisionCount
		err := r.Scan(&c.Outcome, &c.IssueType, &c.Evidence, &c.Recipe, &c.N)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("count decisions: %w", err)
	}
	return out, nil
}
