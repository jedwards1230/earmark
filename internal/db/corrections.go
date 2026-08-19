package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jedwards1230/earmark/internal/patch"
)

// Correction overlay (CONTRACT §2.17).
//
// transcript_chunks is a DERIVED PROJECTION, not storage: the worker
// regenerates it from the immutable transcript source on every embed and
// upserts over the existing rows. A correction written into
// transcript_chunks.text is therefore destroyed by the next re-embed — which is
// why corrections live in transcript_findings and are REPLAYED onto the
// regenerated text:
//
//	regenerate from source → replay accepted corrections → embed
//
// This file is that overlay's read/write path. It writes transcript_findings
// (state + audit) and transcript_chunks.embedding_stale (invalidation). It
// NEVER writes transcripts.segments or transcripts.raw_text — those are
// immutable provenance, the record of what the recognizer actually produced.

var (
	// ErrIllegalTransition means the requested patch_state move is not in the
	// state machine (patch.CanTransition). The DB layer refuses rather than
	// letting a UI bug write a state a human never approved — notably
	// proposed → applied, which would skip the human gate entirely.
	ErrIllegalTransition = errors.New("illegal patch state transition")

	// ErrPatchStateConflict means the row was not in the expected `from` state
	// when the guarded UPDATE ran: someone else decided it first. The caller
	// must re-read rather than retry blindly.
	ErrPatchStateConflict = errors.New("patch is no longer in the expected state")
)

// txBeginner / execer are the narrow slices of the pool API this file needs.
// Both *pgxpool.Pool and pgxmock.PgxPoolIface satisfy them, which makes the
// correction writers execution-testable without a live Postgres.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// CorrectionRow is one accepted correction as stored in transcript_findings.
// It is the persisted form of a patch.Patch; BuildOverlay converts a set of
// them into the replayable overlay.
type CorrectionRow struct {
	ID                  string
	ChunkIndex          *int
	OriginalText        string
	SuggestedCorrection *string
	AnchorOffset        *int
	AnchorOccurrence    *int
	ChunkTextSHA256     *string
}

// AppliedFinding is one correction that landed during a replay.
//
// Before/After are SPAN-level, not whole-chunk (see MarkFindingsApplied).
type AppliedFinding struct {
	ID     string
	Before string
	After  string
}

// overlayStates are the patch states whose corrections belong in the projection:
// `accepted` (approved, not yet reflected in a rebuild) and `applied` (approved
// and already reflected). Both must replay on every rebuild — the projection is
// regenerated from scratch each time, so "already applied" does not mean
// "already present in the new text".
var overlayStates = []string{patch.StateAccepted, patch.StateApplied}

// appliedFromStates are the states MarkFindingsApplied may promote from:
// whatever the state machine says can reach `applied` (i.e. `accepted`, the
// human gate), plus `applied` itself so a repeat rebuild refreshes the audit
// row instead of silently failing its guard.
var appliedFromStates = append(patch.StatesAllowing(patch.StateApplied), patch.StateApplied)

// staleFromStates are the states MarkFindingsStale may retire from, DERIVED
// from patch.CanTransition rather than restated in SQL — a second copy of the
// state machine would eventually disagree with the first. Notably it excludes
// `stale` (terminal) and `rejected`/`reverted` (a human's decision is not
// invalidated by a chunk rebuild).
var staleFromStates = patch.StatesAllowing(patch.StateStale)

// correctionOverlaySQL selects a transcript's replayable corrections.
//
// Read-only. Ordered by (chunk_index, anchor_offset NULLS LAST, id) so the
// overlay is deterministic before replay even sorts it — the same rows always
// arrive in the same order, which keeps a rebuild reproducible end-to-end.
var correctionOverlaySQL = `
	SELECT id, chunk_index, original_text, suggested_correction,
	       anchor_offset, anchor_occurrence, chunk_text_sha256
	FROM transcript_findings
	WHERE transcript_id = $1
	  AND patch_state = ANY($2)
	ORDER BY chunk_index, anchor_offset NULLS LAST, id
`

// GetCorrectionOverlay returns the accepted/applied corrections for one
// transcript, in deterministic order. Read-only.
func (db *DB) GetCorrectionOverlay(ctx context.Context, transcriptID string) ([]CorrectionRow, error) {
	return db.getCorrectionOverlay(ctx, db.pool, transcriptID)
}

// getCorrectionOverlay is the querier-parameterized core of GetCorrectionOverlay
// (mirrors listFindings) so the query + scan path is testable against a mock
// pool.
func (db *DB) getCorrectionOverlay(ctx context.Context, q rowQuerier, transcriptID string) ([]CorrectionRow, error) {
	rows, err := q.Query(ctx, correctionOverlaySQL, transcriptID, overlayStates)
	if err != nil {
		return nil, fmt.Errorf("correction overlay query for transcript %s: %w", transcriptID, err)
	}
	defer rows.Close()

	var out []CorrectionRow
	for rows.Next() {
		var c CorrectionRow
		if err := rows.Scan(&c.ID, &c.ChunkIndex, &c.OriginalText, &c.SuggestedCorrection,
			&c.AnchorOffset, &c.AnchorOccurrence, &c.ChunkTextSHA256); err != nil {
			return nil, fmt.Errorf("scan correction row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (correction overlay): %w", err)
	}
	return out, nil
}

// BuildOverlay converts persisted correction rows into the replayable overlay,
// keyed by chunk index. Pure.
//
// It also returns the IDs of rows that CANNOT be placed at all — a finding with
// a NULL chunk_index has no chunk to replay onto. Those are returned rather than
// dropped so the caller can retire them explicitly; a silently discarded
// correction would sit in `accepted` forever while never appearing in the text.
//
// A NULL/empty suggested_correction is deliberately NOT filtered here: it is
// carried into the overlay so Replay quarantines it with an explicit
// empty_correction reason instead of it vanishing.
func BuildOverlay(rows []CorrectionRow) (patch.Overlay, []string) {
	if len(rows) == 0 {
		return nil, nil
	}
	overlay := make(patch.Overlay)
	var unplaceable []string
	for _, r := range rows {
		if r.ChunkIndex == nil {
			unplaceable = append(unplaceable, r.ID)
			continue
		}
		p := patch.Patch{
			ID: r.ID,
			Anchor: patch.Anchor{
				OriginalText: r.OriginalText,
				Offset:       intOrUnknown(r.AnchorOffset),
				Occurrence:   intOrUnknown(r.AnchorOccurrence),
			},
			Correction: strOrEmpty(r.SuggestedCorrection),
			ChunkHash:  strOrEmpty(r.ChunkTextSHA256),
		}
		overlay[*r.ChunkIndex] = append(overlay[*r.ChunkIndex], p)
	}
	return overlay, unplaceable
}

// intOrUnknown maps a NULL anchor column to patch's "unknown" sentinel (-1),
// which is what makes Locate fall back down its resolution ladder instead of
// trusting a zero it was never given.
func intOrUnknown(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

func strOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// markFindingsAppliedSQL records that one correction landed in the projection.
//
// The guard (`patch_state = ANY($4)`) is what keeps the human gate honest: a
// `proposed` finding can never reach `applied` through this path, only an
// `accepted` one (or an `applied` one being refreshed by a later rebuild).
var markFindingsAppliedSQL = `
	UPDATE transcript_findings
	SET patch_state         = 'applied',
	    applied_at          = now(),
	    applied_before_text = $2,
	    applied_after_text  = $3
	WHERE id = $1 AND patch_state = ANY($4)
`

// MarkFindingsApplied records the corrections that landed during a rebuild.
//
// SEMANTIC NARROWING (CONTRACT §2.17): applied_before_text / applied_after_text
// now hold the SPAN-level before/after, not the whole chunk. Whole-chunk
// before/after became ill-defined once a chunk can carry several corrections —
// there is no single "before" that attributes a difference to one finding. The
// columns are an audit trail, not a revert mechanism: revert is now "flip
// patch_state and rebuild the projection", so nothing needs the old whole-text
// swap.
//
// Idempotent by construction: every rebuild replays the whole overlay, so a
// still-correct patch is re-marked `applied` with identical span text.
func (db *DB) MarkFindingsApplied(ctx context.Context, recs []AppliedFinding) error {
	return db.markFindingsApplied(ctx, db.pool, recs)
}

func (db *DB) markFindingsApplied(ctx context.Context, b txBeginner, recs []AppliedFinding) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin applied-findings tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, r := range recs {
		if _, err := tx.Exec(ctx, markFindingsAppliedSQL,
			r.ID, r.Before, r.After, appliedFromStates); err != nil {
			return fmt.Errorf("mark finding %s applied: %w", r.ID, err)
		}
	}
	return tx.Commit(ctx)
}

// markFindingsStaleSQL retires corrections that can no longer be replayed.
//
// Stale findings STAY VISIBLE — this is an UPDATE, never a DELETE. The row is
// the audit trail of a divergence the judge spotted; losing it would hide that
// a correction silently stopped being applied.
var markFindingsStaleSQL = `
	UPDATE transcript_findings
	SET patch_state  = 'stale',
	    stale_reason = $2
	WHERE id = ANY($1) AND patch_state = ANY($3)
`

// MarkFindingsStale retires findings whose corrections could not be replayed,
// recording why (one of the patch.StaleReason* constants). Findings already in
// a state that may not transition to `stale` (including `stale` itself, which is
// terminal) are left untouched by the guard.
func (db *DB) MarkFindingsStale(ctx context.Context, ids []string, reason string) error {
	return db.markFindingsStale(ctx, db.pool, ids, reason)
}

func (db *DB) markFindingsStale(ctx context.Context, e execer, ids []string, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := e.Exec(ctx, markFindingsStaleSQL, ids, reason, staleFromStates); err != nil {
		return fmt.Errorf("mark %d finding(s) stale (%s): %w", len(ids), reason, err)
	}
	return nil
}

// setPatchStateSQL / setPatchStateDecidedSQL are the guarded state transition.
//
// `WHERE id = $1 AND patch_state = $2` is a compare-and-swap: if another
// reviewer decided the finding first, the UPDATE matches zero rows and the
// caller gets ErrPatchStateConflict instead of clobbering that decision.
//
// The decided_* variant is used for the human decisions (accept/reject) — the
// machine-driven transitions (applied/stale) must not overwrite the record of
// who approved the patch in the first place.
var (
	setPatchStateSQL = `
		UPDATE transcript_findings
		SET patch_state = $3
		WHERE id = $1 AND patch_state = $2
	`
	setPatchStateDecidedSQL = `
		UPDATE transcript_findings
		SET patch_state = $3,
		    decided_at  = now(),
		    decided_by  = NULLIF($4, '')
		WHERE id = $1 AND patch_state = $2
	`
)

// markChunkStaleForFindingSQL flags the ONE chunk a finding belongs to as
// needing a re-embed.
//
// Invalidation is exactly one chunk, and that is a property of the schema
// rather than luck: chunks store their text denormalized and carry no absolute
// offsets into the transcript, so a correction in one chunk never shifts
// another's anchors.
//
// The chunk is addressed by chunk_id when the finding recorded one, falling
// back to (transcript_id, chunk_index) for findings written before chunk_id was
// populated. It writes ONLY transcript_chunks.embedding_stale — never the
// chunk text, and never transcripts.
var markChunkStaleForFindingSQL = `
	UPDATE transcript_chunks c
	SET embedding_stale = true
	FROM transcript_findings f
	WHERE f.id = $1
	  AND (c.id = f.chunk_id
	       OR (f.chunk_id IS NULL
	           AND c.transcript_id = f.transcript_id
	           AND c.chunk_index = f.chunk_index))
`

// SetPatchState moves one finding through the patch state machine. This is the
// human gate (CONTRACT §2.17): the judge proposes, a person disposes, and no
// text changes without an explicit recorded decision on a specific finding.
//
// The transition is validated against patch.CanTransition BEFORE any SQL runs,
// so an illegal move (notably proposed → applied, which would skip the human
// accept) never reaches the database. decidedBy is recorded for accept/reject.
//
// Accepting or reverting also flags the affected chunk `embedding_stale`: the
// projection for that chunk no longer reflects the overlay, so it must be
// rebuilt and re-embedded. Both happen in ONE transaction — a decision that was
// recorded without its invalidation would leave the corpus permanently showing
// text that disagrees with its own findings.
func (db *DB) SetPatchState(ctx context.Context, id, from, to, decidedBy string) error {
	return db.setPatchState(ctx, db.pool, id, from, to, decidedBy)
}

func (db *DB) setPatchState(ctx context.Context, b txBeginner, id, from, to, decidedBy string) error {
	if !patch.CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s (finding %s)", ErrIllegalTransition, from, to, id)
	}

	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin patch-state tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tag pgconn.CommandTag
	if to == patch.StateAccepted || to == patch.StateRejected {
		tag, err = tx.Exec(ctx, setPatchStateDecidedSQL, id, from, to, decidedBy)
	} else {
		tag, err = tx.Exec(ctx, setPatchStateSQL, id, from, to)
	}
	if err != nil {
		return fmt.Errorf("set patch state %s -> %s (finding %s): %w", from, to, id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: finding %s is not in state %q", ErrPatchStateConflict, id, from)
	}

	// The projection for this chunk is now out of date: an accept adds a
	// correction to the overlay, a revert removes one.
	if to == patch.StateAccepted || to == patch.StateReverted {
		if _, err := tx.Exec(ctx, markChunkStaleForFindingSQL, id); err != nil {
			return fmt.Errorf("flag chunk stale for finding %s: %w", id, err)
		}
	}

	return tx.Commit(ctx)
}
