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
// or is this recipe's retryable hold ($8, jev_unavailable), or its
// overlap_pending hold unless its winner (related_finding_id) is still
// proposed AND held by this recipe — so the conflict is re-ranked once the
// winner is applied, rejected or archived, and also when the winner was never
// decided under this recipe (its accept capped or skipped, or a competitor
// outside the run's scope). Every other verdict of this recipe is final, so
// a re-run does not ask again (a loser pending on a winner this recipe holds
// stays put: no flip-flop).
//
// "This recipe" is the recipe and every recipe equivalent to it
// (equivalentRecipesCTE): a format-1 recipe id that differs from the run's
// only by the build that registered it is not "another recipe", so upgrading
// to ID format 2 does not re-check every accept or re-ask every hold.
//
// The shard is hashtext(transcript_id) mod $4, folded to be non-negative
// (hashtext is a signed int4; abs() would overflow on its minimum).
//
//	$1 recipe  $2 book pattern ('' = all)  $3 issue type ('' = all)
//	$4 shards (0 = all)  $5 shard  $6 cursor (NULL = start)  $7 limit  $8 retry reason
var decideWorkSQL = `
	WITH` + equivalentRecipesCTE + `
	SELECT f.id::text, f.transcript_id::text, f.file_path, f.issue_type, f.original_text,
	       COALESCE(f.suggested_correction, ''), f.confidence, f.chunk_index,
	       f.anchor_offset, COALESCE(f.anchor_occurrence, -1), f.chunk_text_sha256,
	       f.patch_state, COALESCE(f.decided_by, '')
	  FROM transcript_findings f
	  LEFT JOIN LATERAL (
	        SELECT e.recipe_id, e.outcome, e.reason, e.related_finding_id
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
	            AND f.decided_by ~ '^jev:[0-9a-f]{64}$'
	            AND f.decided_by NOT IN (SELECT 'jev:' || eq.recipe_id FROM eq)))
	   AND (d.recipe_id IS NULL OR d.recipe_id NOT IN (SELECT eq.recipe_id FROM eq)
	        OR (d.outcome = 'hold' AND d.reason = $8)
	        OR (d.outcome = 'hold' AND d.reason = '` + ReasonOverlapPending + `'
	            AND NOT EXISTS (
	                SELECT 1 FROM transcript_findings w
	                 CROSS JOIN LATERAL (
	                       SELECT e2.recipe_id, e2.outcome FROM finding_events e2
	                        WHERE e2.finding_id = w.id AND e2.kind = 'decision'
	                          AND NOT EXISTS (SELECT 1 FROM finding_events v2
	                                           WHERE v2.kind = 'revoke' AND v2.revokes_event_id = e2.id)
	                        ORDER BY e2.id DESC
	                        LIMIT 1) lw
	                 WHERE w.id = d.related_finding_id AND w.patch_state = 'proposed'
	                   AND lw.outcome = 'hold' AND lw.recipe_id IN (SELECT eq.recipe_id FROM eq))))
	   AND ($2 = '' OR f.file_path ILIKE $2)
	   AND ($3 = '' OR f.issue_type = $3)
	   AND ($4 = 0 OR mod(mod(hashtext(f.transcript_id::text)::bigint, $4) + $4, $4) = $5)
	   AND ($6::uuid IS NULL OR f.id > $6::uuid)
	 ORDER BY f.id
	 LIMIT $7`

// decideWorkCountSQL counts the whole work list: decideWorkSQL itself, with
// no cursor ($6 NULL) and no limit ($7 NULL → LIMIT ALL), so the count can
// never disagree with what the pages return.
var decideWorkCountSQL = `SELECT count(*) FROM (` + decideWorkSQL + `) w`

// DecideWorkCount counts the findings the --yes run over s would visit
// (s.After and s.Limit are ignored). Read-only; a progress total, so callers
// bound it with a deadline.
func (db *DB) DecideWorkCount(ctx context.Context, s DecideWorkScope) (int, error) {
	s.After, s.Limit = "", 1
	if err := s.validate(); err != nil {
		return 0, err
	}
	book := ""
	if strings.TrimSpace(s.Book) != "" {
		book = likePattern(s.Book)
	}
	var n int
	if err := db.pool.QueryRow(ctx, decideWorkCountSQL, s.RecipeID, book, s.IssueType, s.Shards, s.Shard,
		nil, nil, s.RetryReason).Scan(&n); err != nil {
		return 0, fmt.Errorf("decide work count: %w", err)
	}
	return n, nil
}

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
// directly; the finding is decided again. A duplicate the recipe grouped goes
// back to proposed (its grouping undone), unless its representative follows
// below.
//
// Duplicates follow their representative (CONTRACT §2.19 "Agreeing and
// conflicting findings"): when the undo takes a representative out of the
// overlay (accepted → rejected, applied → reverted) its duplicates are
// rejected too (reason duplicate_of_reverted, a decision naming the
// representative), so the undone fix cannot come back through a duplicate;
// when it returns a representative to proposed, its duplicates — and the
// findings an earlier undo rejected as duplicate_of_reverted — return to
// proposed with it.
var revertTransitions = map[string]string{
	patch.StateApplied:   patch.StateReverted,
	patch.StateAccepted:  patch.StateRejected,
	patch.StateRejected:  patch.StateProposed,
	patch.StateReverted:  patch.StateProposed,
	patch.StateDuplicate: patch.StateProposed,
}

// followsOut reports whether undoing a finding in state takes it out of the
// overlay (its duplicates are rejected); otherwise the undo returns it to
// proposed (its duplicates return too).
func followsOut(state string) bool {
	return state == patch.StateApplied || state == patch.StateAccepted
}

// ReasonDuplicateOfReverted is the reason of a duplicate rejected because the
// undo of its representative took the fix out of the overlay.
const ReasonDuplicateOfReverted = "duplicate_of_reverted"

// revertCandidatesSQL lists the findings a decide recipe last moved:
// decided_by 'jev:<recipe>' (a person's decision, or an undo already done —
// 'revert:jev:…' — never matches, which is what makes revert idempotent).
// Representatives first, duplicates last (then id order), so every
// representative a duplicate follows has moved before the duplicate's own
// batch, whatever the batching.
//
//	$1 recipe ('' = any)  $2 finding (NULL = any)  $3 since (NULL = any)
var revertCandidatesSQL = `
	SELECT f.id::text, f.patch_state, substring(f.decided_by FROM 5)
	  FROM transcript_findings f
	 WHERE f.decided_by ~ '^jev:[0-9a-f]{64}$'
	   AND f.patch_state IN ('applied', 'accepted', 'rejected', 'reverted', 'duplicate')
	   AND ($1 = '' OR f.decided_by = 'jev:' || $1)
	   AND ($2::uuid IS NULL OR f.id = $2::uuid)
	   AND ($3::timestamptz IS NULL OR f.decided_at >= $3::timestamptz)
	 ORDER BY f.patch_state = 'duplicate', f.id`

// revertScopeDecisions is the filter of the live decisions a revert revokes:
// decisions the recipe itself made (actor jev:<recipe>; the decisions an undo
// writes — duplicate_of_reverted — are not undone by undoing again), in scope.
const revertScopeDecisions = `
	 WHERE e.kind = 'decision'
	   AND e.actor = 'jev:' || e.recipe_id
	   AND ($1 = '' OR e.recipe_id = $1)
	   AND ($2::uuid IS NULL OR e.finding_id = $2::uuid)
	   AND ($3::timestamptz IS NULL OR e.created_at >= $3::timestamptz)
	   AND NOT EXISTS (SELECT 1 FROM finding_events v
	                    WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`

// revertDecisionsCountSQL counts the live decisions the scope would revoke.
var revertDecisionsCountSQL = `
	SELECT count(*) FROM finding_events e` + revertScopeDecisions

// revokeScopeSQL appends one revoke per live decision in scope, attributed to
// the undo of the decision's own recipe. The unique index on
// revokes_event_id makes a concurrent second revoke a no-op.
var revokeScopeSQL = `
	INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
	SELECT e.finding_id, e.transcript_id, 'revoke', 'decide revert', 'revert:jev:' || e.recipe_id,
	       e.recipe_id, e.id
	  FROM finding_events e` + revertScopeDecisions + `
	 ORDER BY e.id
	ON CONFLICT (revokes_event_id) WHERE kind = 'revoke' DO NOTHING`

// reflagChunksSQL marks the chunk of every listed finding embedding_stale
// (addressed like markChunkStaleForFindingSQL). It runs after the revert
// commits: a rebuild that read the overlay before the revert and clears the
// flag after it would otherwise keep the undone fix in the projection.
// ClearEmbeddingStale's replayed-set guard now closes that window on its own;
// this sweep (shared with ClearFindings) stays as defence in depth for a
// rebuild running an older build. Costs at most one extra re-embed per chunk.
var reflagChunksSQL = `
	UPDATE transcript_chunks c
	   SET embedding_stale = true
	  FROM transcript_findings f
	 WHERE f.id = ANY($1)
	   AND ((f.chunk_index IS NOT NULL AND c.transcript_id = f.transcript_id AND c.chunk_index = f.chunk_index)
	        OR (f.chunk_index IS NULL AND c.id = f.chunk_id))`

// followerDecisions is the filter of a follower's live decisions an undo
// revokes when it moves the follower: its duplicate decision, and an earlier
// undo's duplicate_of_reverted reject.
const followerDecisions = `e.kind = 'decision'
	   AND (e.outcome = 'duplicate' OR e.reason = '` + ReasonDuplicateOfReverted + `')
	   AND NOT EXISTS (SELECT 1 FROM finding_events v
	                    WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`

// revertFollowersSQL lists, for the representatives $1, the findings that
// follow them: their duplicates, and the findings an earlier undo rejected as
// duplicate_of_reverted naming them (still rejected, the decision live). It
// is the dry run's view of what the apply's follower moves will touch.
var revertFollowersSQL = `
	SELECT d.id::text, d.duplicate_of::text, d.patch_state
	  FROM transcript_findings d
	 WHERE d.patch_state = 'duplicate' AND d.duplicate_of = ANY($1)
	UNION
	SELECT f.id::text, e.related_finding_id::text, f.patch_state
	  FROM finding_events e
	  JOIN transcript_findings f ON f.id = e.finding_id
	 WHERE e.reason = '` + ReasonDuplicateOfReverted + `' AND e.related_finding_id = ANY($1)
	   AND f.patch_state = 'rejected' AND ` + followerDecisions

// followerRevokesCountSQL counts the live follower decisions of the findings
// $1 that the scope ($2 recipe, $3 finding, $4 since) does not already count,
// so a dry run's revoke total matches what --yes revokes.
var followerRevokesCountSQL = `
	SELECT count(*) FROM finding_events e
	 WHERE e.finding_id = ANY($1) AND ` + followerDecisions + `
	   AND NOT (e.actor = 'jev:' || e.recipe_id
	            AND ($2 = '' OR e.recipe_id = $2)
	            AND ($3::uuid IS NULL OR e.finding_id = $3::uuid)
	            AND ($4::timestamptz IS NULL OR e.created_at >= $4::timestamptz))`

// rejectFollowersSQL rejects the duplicates of representatives the undo took
// out of the overlay: $1 representatives, $2 the decider each move is
// attributed to (revert:jev:<recipe being reverted>), index-aligned.
// duplicate_of is cleared (the 00013 CHECK); the representative is named by
// the duplicate_of_reverted decision written next. Locks in id order.
var rejectFollowersSQL = `
	WITH w AS (
	    SELECT * FROM unnest($1::uuid[], $2::text[]) AS w(rep, decider)
	), lk AS (
	    SELECT f.id, w.rep, w.decider
	      FROM transcript_findings f JOIN w ON f.duplicate_of = w.rep
	     WHERE f.patch_state = 'duplicate'
	     ORDER BY f.id
	       FOR UPDATE OF f
	)
	UPDATE transcript_findings f
	   SET patch_state  = 'rejected',
	       duplicate_of = NULL,
	       decided_at   = clock_timestamp(),
	       decided_by   = lk.decider
	  FROM lk
	 WHERE f.id = lk.id AND f.patch_state = 'duplicate'
	RETURNING f.id::text, lk.rep::text, lk.decider`

// releaseDuplicatesSQL returns duplicates to proposed: the listed ones ($1,
// duplicates the reverted recipe grouped, deciders $2) and every duplicate of
// the representatives the undo returned to proposed ($3, deciders $4). A
// listed duplicate whose representative moved is reported as following it
// (listed false), as the dry run counts it.
// duplicate_of is cleared in the same UPDATE (the 00013 CHECK); the decider is
// revert:jev:<the recipe being reverted>, so the move is never a revert
// candidate again. Locks in id order.
var releaseDuplicatesSQL = `
	WITH w AS (
	    SELECT f.id, x.decider, true AS listed
	      FROM unnest($1::uuid[], $2::text[]) AS x(id, decider)
	      JOIN transcript_findings f ON f.id = x.id
	    UNION ALL
	    SELECT f.id, x.decider, false
	      FROM unnest($3::uuid[], $4::text[]) AS x(rep, decider)
	      JOIN transcript_findings f ON f.duplicate_of = x.rep
	), pick AS (
	    SELECT DISTINCT ON (id) id, decider, listed FROM w ORDER BY id, listed
	), lk AS (
	    SELECT f.id, pick.decider, pick.listed
	      FROM transcript_findings f JOIN pick ON pick.id = f.id
	     WHERE f.patch_state = 'duplicate'
	     ORDER BY f.id
	       FOR UPDATE OF f
	)
	UPDATE transcript_findings f
	   SET patch_state  = 'proposed',
	       duplicate_of = NULL,
	       decided_at   = clock_timestamp(),
	       decided_by   = lk.decider
	  FROM lk
	 WHERE f.id = lk.id AND f.patch_state = 'duplicate'
	RETURNING f.id::text, lk.listed, lk.decider`

// restoreFollowersSQL returns to proposed the findings an earlier undo
// rejected as duplicate_of_reverted naming a representative the undo now
// returns to proposed ($1, deciders $2).
var restoreFollowersSQL = `
	WITH x AS (
	    SELECT * FROM unnest($1::uuid[], $2::text[]) AS x(rep, decider)
	), w AS (
	    SELECT DISTINCT ON (e.finding_id) e.finding_id AS id, x.decider
	      FROM finding_events e JOIN x ON e.related_finding_id = x.rep
	     WHERE e.reason = '` + ReasonDuplicateOfReverted + `' AND ` + followerDecisions + `
	     ORDER BY e.finding_id, e.id DESC
	), lk AS (
	    SELECT f.id, w.decider
	      FROM transcript_findings f JOIN w ON w.id = f.id
	     WHERE f.patch_state = 'rejected'
	     ORDER BY f.id
	       FOR UPDATE OF f
	)
	UPDATE transcript_findings f
	   SET patch_state = 'proposed',
	       decided_at  = clock_timestamp(),
	       decided_by  = lk.decider
	  FROM lk
	 WHERE f.id = lk.id AND f.patch_state = 'rejected'
	RETURNING f.id::text, lk.decider`

// revokeFollowerDecisionsSQL revokes the live follower decisions of the moved
// followers ($1, actors $2 index-aligned), reason $3, so the work list decides
// a returned follower again and a rejected one carries only the undo's
// decision.
var revokeFollowerDecisionsSQL = `
	INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
	SELECT e.finding_id, e.transcript_id, 'revoke', $3, x.actor, e.recipe_id, e.id
	  FROM unnest($1::uuid[], $2::text[]) AS x(id, actor)
	  JOIN finding_events e ON e.finding_id = x.id
	 WHERE ` + followerDecisions + `
	 ORDER BY e.id
	ON CONFLICT (revokes_event_id) WHERE kind = 'revoke' DO NOTHING`

// insertFollowerRejectsSQL records the undo's verdict on a rejected follower:
// a reject decision, reason duplicate_of_reverted, naming the representative,
// attributed to the undo (actor revert:jev:<recipe>, recipe_id that recipe).
var insertFollowerRejectsSQL = `
	INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id,
	                            chunk_text_sha256, issue_type, related_finding_id)
	SELECT f.id, f.transcript_id, 'decision', 'reject', '` + ReasonDuplicateOfReverted + `', x.actor,
	       substring(x.actor FROM '[0-9a-f]{64}$'), f.chunk_text_sha256, f.issue_type, x.rep
	  FROM unnest($1::uuid[], $2::uuid[], $3::text[]) WITH ORDINALITY AS x(id, rep, actor, ord)
	  JOIN transcript_findings f ON f.id = x.id
	 ORDER BY x.ord`

// maxRevertBatch caps the findings one revert transaction moves (a var so a
// test can force several batches).
var maxRevertBatch = 500

// RevertReport counts an undo. Transitions is keyed "from->to".
type RevertReport struct {
	DryRun      bool           `json:"dry_run"`
	Transitions map[string]int `json:"transitions"`
	// Moved counts candidates moved in their own right (apply) or that would
	// move (dry run).
	Moved int `json:"moved"`
	// Cascaded counts followers moved with their representative: duplicates
	// rejected with it or returned to proposed with it, and earlier
	// duplicate_of_reverted rejects restored (also in Transitions).
	Cascaded int `json:"cascaded"`
	// Skipped counts candidates another writer changed first (apply only).
	Skipped int `json:"skipped"`
	// Revoked counts decisions revoked (or that would be).
	Revoked int64 `json:"revoked"`
	// Reflagged counts chunks flagged embedding_stale by the sweep.
	Reflagged int64 `json:"reflagged"`
}

// RevertDecisions undoes automated decisions in scope (CONTRACT §2.19
// "earmark decide revert"). Dry run unless apply: it counts the transitions
// and the decisions it would revoke, the followers included. With apply, in
// batches of at most maxRevertBatch findings (representatives first), it
// moves applied → reverted, accepted → rejected, rejected → proposed,
// (re-check) reverted → proposed and duplicate → proposed with decided_by
// 'revert:jev:<recipe>' — each a transition event — and moves each moved
// representative's followers with it (revertTransitions); then it revokes
// every live decision the recipe made in scope, then re-flags the moved
// findings' chunks. Idempotent: an undone finding no longer carries a jev
// decider, and a revoked decision is not revoked twice. A person's decision is
// never touched.
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
		return rep, previewRevert(ctx, b, s, cands, &rep)
	}

	// done: every follower an earlier batch moved, so a later batch's
	// candidate duplicate is neither moved again nor counted as skipped.
	done := map[string]bool{}
	for start := 0; start < len(cands); start += maxRevertBatch {
		batch := cands[start:min(start+maxRevertBatch, len(cands))]
		br, err := revertBatch(ctx, b, batch, rep.Transitions, done)
		if err != nil {
			return rep, err
		}
		rep.Moved += len(br.moved) + br.released
		rep.Cascaded += br.cascaded
		rep.Skipped += br.skipped
		rep.Revoked += br.revoked
		if len(br.moved) > 0 {
			tag, err := b.Exec(ctx, reflagChunksSQL, br.moved)
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
	rep.Revoked += tag.RowsAffected()
	return rep, nil
}

// previewRevert is the dry run: the candidates' own transitions, their
// followers' (revertFollowersSQL, judged the way revertBatch moves them), and
// the decisions --yes would revoke — the scope's plus the followers' outside
// it.
func previewRevert(ctx context.Context, b revertPool, s RevertScope, cands []revertCandidate, rep *RevertReport) error {
	var reps []string
	out := map[string]bool{} // representative → its undo takes it out of the overlay
	for _, c := range cands {
		if c.state != patch.StateDuplicate {
			reps = append(reps, c.id)
			out[c.id] = followsOut(c.state)
		}
	}
	followers := map[string]bool{}
	var moved []string
	if len(reps) > 0 {
		rows, err := b.Query(ctx, revertFollowersSQL, reps)
		if err != nil {
			return fmt.Errorf("revert followers: %w", err)
		}
		var id, of, state string
		if _, err := pgx.ForEachRow(rows, []any{&id, &of, &state}, func() error {
			if followers[id] {
				return nil
			}
			to := patch.StateProposed
			if state == patch.StateDuplicate && out[of] {
				to = patch.StateRejected
			}
			if state == patch.StateRejected && out[of] {
				return nil // already rejected with it
			}
			followers[id] = true
			moved = append(moved, id)
			rep.Transitions[state+"->"+to]++
			rep.Cascaded++
			return nil
		}); err != nil {
			return fmt.Errorf("revert followers: %w", err)
		}
	}
	for _, c := range cands {
		if followers[c.id] {
			continue
		}
		rep.Transitions[c.state+"->"+revertTransitions[c.state]]++
		rep.Moved++
		if c.state == patch.StateDuplicate {
			moved = append(moved, c.id)
		}
	}
	if err := b.QueryRow(ctx, revertDecisionsCountSQL, s.args()...).Scan(&rep.Revoked); err != nil {
		return fmt.Errorf("count decisions to revoke: %w", err)
	}
	if len(moved) > 0 {
		var extra int64
		if err := b.QueryRow(ctx, followerRevokesCountSQL, append([]any{moved}, s.args()...)...).Scan(&extra); err != nil {
			return fmt.Errorf("count follower decisions to revoke: %w", err)
		}
		rep.Revoked += extra
	}
	return nil
}

// revertBatchResult is one revert transaction: the non-duplicates it moved,
// how many listed duplicates it released in their own right and how many
// followers it moved, candidates another writer moved first, and the
// decisions revoked with the followers.
type revertBatchResult struct {
	moved                       []string
	released, cascaded, skipped int
	revoked                     int64
}

// revertBatch moves one batch in one transaction, grouped by (state, recipe):
// each group is one guarded bulk change that skips rows another writer moved
// since they were listed. Then, in the same transaction, each moved
// representative's followers move with it — rejected when it left the
// overlay, returned to proposed when it returned there — and the batch's
// remaining duplicates return to proposed. done holds the followers earlier
// batches moved; they are left out, and the ones this batch moves are added.
func revertBatch(ctx context.Context, b txBeginner, batch []revertCandidate, tally map[string]int, done map[string]bool) (revertBatchResult, error) {
	type group struct{ state, recipe string }
	groups := map[group][]string{}
	var dups, dupDeciders []string
	for _, c := range batch {
		if c.state == patch.StateDuplicate {
			if !done[c.id] {
				dups, dupDeciders = append(dups, c.id), append(dupDeciders, "revert:jev:"+c.recipe)
			}
			continue
		}
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

	var res revertBatchResult
	tx, err := b.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin revert tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	counts := map[string]int{}
	var outReps, outDeciders, inReps, inDeciders []string
	for _, g := range keys {
		to := revertTransitions[g.state]
		decider := "revert:jev:" + g.recipe
		br, err := setPatchStateBulkTx(ctx, tx, g.state, to, decider, groups[g])
		if err != nil {
			return res, err
		}
		res.moved = append(res.moved, br.Changed...)
		res.skipped += len(br.Skipped)
		counts[g.state+"->"+to] += len(br.Changed)
		for _, id := range br.Changed {
			if followsOut(g.state) {
				outReps, outDeciders = append(outReps, id), append(outDeciders, decider)
			} else {
				inReps, inDeciders = append(inReps, id), append(inDeciders, decider)
			}
		}
	}
	fr, err := moveFollowersTx(ctx, tx, outReps, outDeciders, inReps, inDeciders, dups, dupDeciders, done)
	if err != nil {
		return res, err
	}
	res.released, res.cascaded, res.revoked = fr.released, fr.cascaded, fr.revoked
	res.skipped += len(dups) - fr.released - fr.listedCascaded
	for k, n := range fr.counts {
		counts[k] += n
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit revert: %w", err)
	}
	for _, id := range fr.ids {
		done[id] = true
	}
	for k, n := range counts {
		if n > 0 {
			tally[k] += n
		}
	}
	return res, nil
}

// followerMoves is what moveFollowersTx did.
type followerMoves struct {
	released       int // listed duplicates returned to proposed in their own right
	cascaded       int // followers moved with their representative
	listedCascaded int // of those, listed duplicates
	revoked        int64
	counts         map[string]int
	ids            []string // every finding moved
}

// moveFollowersTx moves, inside the revert's transaction: the duplicates of
// representatives that left the overlay (out) to rejected, each with a
// duplicate_of_reverted decision naming its representative; the duplicates of
// representatives returned to proposed (in) and the listed duplicates
// (dups) to proposed; and the earlier duplicate_of_reverted rejects of the in
// representatives back to proposed. Each follower's live duplicate (and
// duplicate_of_reverted) decisions are revoked by the same actor. Rows in
// done are already handled.
func moveFollowersTx(ctx context.Context, tx pgx.Tx, out, outDeciders, in, inDeciders, dups, dupDeciders []string,
	done map[string]bool) (followerMoves, error) {
	fm := followerMoves{counts: map[string]int{}}
	listed := map[string]bool{}
	for _, d := range dups {
		listed[d] = true
	}
	revoke := func(ids, actors []string, reason string) error {
		if len(ids) == 0 {
			return nil
		}
		tag, err := tx.Exec(ctx, revokeFollowerDecisionsSQL, ids, actors, reason)
		if err != nil {
			return fmt.Errorf("revoke follower decisions: %w", err)
		}
		fm.revoked += tag.RowsAffected()
		return nil
	}
	note := func(id, from, to string) {
		fm.ids = append(fm.ids, id)
		fm.counts[from+"->"+to]++
	}

	if len(out) > 0 {
		if err := SetEventContext(ctx, tx, EventContext{Reason: ReasonDuplicateOfReverted}); err != nil {
			return fm, err
		}
		rows, err := tx.Query(ctx, rejectFollowersSQL, out, outDeciders)
		if err != nil {
			return fm, fmt.Errorf("reject duplicates of reverted representatives: %w", err)
		}
		var ids, reps, actors []string
		var id, rep, actor string
		if _, err := pgx.ForEachRow(rows, []any{&id, &rep, &actor}, func() error {
			ids, reps, actors = append(ids, id), append(reps, rep), append(actors, actor)
			return nil
		}); err != nil {
			return fm, fmt.Errorf("reject duplicates of reverted representatives: %w", err)
		}
		if err := revoke(ids, actors, ReasonDuplicateOfReverted); err != nil {
			return fm, err
		}
		if len(ids) > 0 {
			if _, err := tx.Exec(ctx, insertFollowerRejectsSQL, ids, reps, actors); err != nil {
				return fm, fmt.Errorf("record duplicate_of_reverted decisions: %w", err)
			}
		}
		for _, id := range ids {
			note(id, patch.StateDuplicate, patch.StateRejected)
			fm.cascaded++
			if listed[id] {
				fm.listedCascaded++
			}
		}
	}

	var relIDs, relDeciders []string
	for i, d := range dups {
		if !slices.Contains(fm.ids, d) && !done[d] {
			relIDs, relDeciders = append(relIDs, d), append(relDeciders, dupDeciders[i])
		}
	}
	if len(relIDs) > 0 || len(in) > 0 {
		if err := SetEventContext(ctx, tx, EventContext{Reason: ReasonDuplicateReleased}); err != nil {
			return fm, err
		}
		if relIDs == nil {
			relIDs, relDeciders = []string{}, []string{}
		}
		inArg, inDec := in, inDeciders
		if inArg == nil {
			inArg, inDec = []string{}, []string{}
		}
		rows, err := tx.Query(ctx, releaseDuplicatesSQL, relIDs, relDeciders, inArg, inDec)
		if err != nil {
			return fm, fmt.Errorf("release duplicates: %w", err)
		}
		var ids, actors []string
		var id, actor string
		var isListed bool
		if _, err := pgx.ForEachRow(rows, []any{&id, &isListed, &actor}, func() error {
			ids, actors = append(ids, id), append(actors, actor)
			note(id, patch.StateDuplicate, patch.StateProposed)
			switch {
			case isListed:
				fm.released++
			case listed[id]:
				fm.cascaded++
				fm.listedCascaded++
			default:
				fm.cascaded++
			}
			return nil
		}); err != nil {
			return fm, fmt.Errorf("release duplicates: %w", err)
		}
		if err := revoke(ids, actors, ReasonDuplicateReleased); err != nil {
			return fm, err
		}
	}
	if len(in) > 0 {
		rows, err := tx.Query(ctx, restoreFollowersSQL, in, inDeciders)
		if err != nil {
			return fm, fmt.Errorf("restore duplicate_of_reverted rejects: %w", err)
		}
		var ids, actors []string
		var id, actor string
		if _, err := pgx.ForEachRow(rows, []any{&id, &actor}, func() error {
			ids, actors = append(ids, id), append(actors, actor)
			note(id, patch.StateRejected, patch.StateProposed)
			fm.cascaded++
			return nil
		}); err != nil {
			return fm, fmt.Errorf("restore duplicate_of_reverted rejects: %w", err)
		}
		if err := revoke(ids, actors, ReasonDuplicateReleased); err != nil {
			return fm, err
		}
	}
	if len(fm.ids) > 0 || len(out) > 0 {
		if err := SetEventContext(ctx, tx, EventContext{}); err != nil {
			return fm, err
		}
	}
	return fm, nil
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
