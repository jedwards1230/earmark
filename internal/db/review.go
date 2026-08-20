package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// Correction REVIEW surface (CONTRACT §2.17).
//
// corrections.go is the projection's read/write path — what the embed worker
// needs to replay an overlay. This file is the REVIEWER's path: the queries and
// writes behind the MCP review tools, which let a human (or an agent acting for
// one) see a proposed correction in context, decide it, or author one the judge
// never proposed.
//
// Same invariants, no exceptions:
//
//   - transcripts.segments / transcripts.raw_text are never written here.
//   - A reviewer is always shown the chunk's PRISTINE text
//     (COALESCE(source_text, text)) — the projection's input, and the only text
//     an anchor may be resolved against.
//   - A recorded decision flags exactly one chunk embedding_stale, so the
//     rebuild pass re-embeds it.

// Finding provenance (transcript_findings.origin).
//
// The judge and a human author findings that are structurally identical but
// mean different things: one is a machine's guess whose precision we measure,
// the other is a decision already made. Recording that in the row — not just in
// a log line — is what lets a quality metric say "judge precision" and mean it.
const (
	// OriginJudge marks a finding proposed by the eval layer's LLM judge. It is
	// the column default, so every pre-existing row reads correctly.
	OriginJudge = "judge"
	// OriginHuman marks a finding authored directly by a reviewer — a correction
	// no model proposed.
	OriginHuman = "human"

	// ManualEditModel is the transcript_findings.model value for a human-authored
	// correction. The column is NOT NULL and means "which judge said this"; a
	// direct edit had no judge, so it gets a reserved sentinel rather than a
	// borrowed model id that would corrupt per-model attribution.
	ManualEditModel = "manual"

	// ManualEditConfidence is the confidence recorded for a human-authored
	// correction. confidence is the judge's SELF-SCORE and is NOT NULL; a
	// decision a person already made carries no uncertainty, so it is 1. Metrics
	// that measure judge precision must filter origin = 'judge' rather than read
	// this as a judge score.
	ManualEditConfidence = 1.0
)

// ErrChunkNotFound means the addressed chunk row does not exist (wrong id, or
// the transcript was requeued and its chunks dropped).
var ErrChunkNotFound = errors.New("chunk not found")

// CorrectionDetail is one finding as a REVIEWER needs to see it: the proposal
// itself, its decision state, and the pristine chunk text its anchor resolves
// against. The chunk text is joined in because a correction cannot be judged
// without the surrounding sentence — "and" → "an" is unreviewable in isolation.
type CorrectionDetail struct {
	ID                  string
	TranscriptID        string
	FilePath            string
	BookDir             string
	ChunkID             *string
	ChunkIndex          *int
	StartSec            float64
	EndSec              float64
	OriginalText        string
	SuggestedCorrection *string
	IssueType           string
	Confidence          float64
	Model               string
	Origin              string
	PatchState          string
	StaleReason         *string
	DecidedAt           *time.Time
	DecidedBy           *string
	AppliedAt           *time.Time
	CreatedAt           time.Time
	AnchorOffset        *int
	AnchorOccurrence    *int
	ChunkTextSHA256     *string
	// ChunkText is the chunk's PRISTINE text (COALESCE(source_text, text)), or
	// empty when the finding's chunk no longer exists. Never the corrected
	// surface: an anchor resolved against corrected text records a hash the
	// projection's input can never match, so the correction would be born stale.
	ChunkText string
}

// CorrectionFilter scopes a review worklist query. The zero value lists the
// whole library at the default limit.
type CorrectionFilter struct {
	// ID selects exactly one finding (empty = no id filter).
	ID string
	// Path scopes to a book directory or a single track file — matched as the
	// exact path OR anything beneath it, the same way the scoped findings list
	// and the scoped clear match.
	Path string
	// States filters on patch_state (nil/empty = every state).
	States []string
	// MinConfidence drops findings below a judge self-score. 0 keeps everything.
	MinConfidence float64
	// Limit caps the rows (<= 0 → defaultCorrectionListLimit); Offset pages.
	Limit  int
	Offset int
}

// trimPath normalizes a book-dir / file-path filter the same way the scoped
// findings list and the scoped clear do, so "review this book" scopes to the
// identical set of rows.
func trimPath(p string) string {
	return strings.TrimRight(strings.TrimSpace(p), "/")
}

// defaultCorrectionListLimit bounds an unbounded review query. Deliberately
// smaller than the dashboard worklist's 200: each row here carries a whole
// chunk of text, and the consumer is an agent's context window.
const defaultCorrectionListLimit = 50

// maxCorrectionListLimit is the hard ceiling on one page of review rows.
const maxCorrectionListLimit = 200

// listCorrectionsSQL is the reviewer's worklist query.
//
// Read-only. Package var so a test can assert its shape (SELECT-only, pristine
// text, no transcript writes) without a live database.
//
// The LEFT JOIN mirrors markChunkStaleForFindingSQL's addressing exactly: by
// chunk_id when the finding recorded one, else by (transcript_id, chunk_index)
// for findings written before chunk_id was populated. The two arms are mutually
// exclusive (the second requires chunk_id IS NULL), so a finding can never
// match two chunks and fan out into duplicate rows. LEFT, not INNER, so a
// finding whose chunk is gone still appears — it is exactly the row a reviewer
// needs to see, with an empty chunk text saying why it cannot be replayed.
//
// It selects COALESCE(c.source_text, c.text): the PRISTINE chunk, the same text
// the judge was shown and the same text replay resolves anchors against.
var listCorrectionsSQL = `
	SELECT f.id, f.transcript_id, f.file_path,
	       regexp_replace(f.file_path, '/[^/]+$', '') AS book_dir,
	       f.chunk_id, f.chunk_index, f.start_sec, f.end_sec,
	       f.original_text, f.suggested_correction, f.issue_type, f.confidence,
	       f.model, f.origin, f.patch_state, f.stale_reason,
	       f.decided_at, f.decided_by, f.applied_at, f.created_at,
	       f.anchor_offset, f.anchor_occurrence, f.chunk_text_sha256,
	       COALESCE(c.source_text, c.text, '') AS chunk_text
	FROM transcript_findings f
	LEFT JOIN transcript_chunks c
	       ON c.id = f.chunk_id
	       OR (f.chunk_id IS NULL
	           AND c.transcript_id = f.transcript_id
	           AND c.chunk_index = f.chunk_index)
	WHERE ($1::uuid IS NULL OR f.id = $1::uuid)
	  AND ($2::text IS NULL OR f.file_path = $2 OR f.file_path LIKE $3 ESCAPE '\')
	  AND ($4::text[] IS NULL OR f.patch_state = ANY($4))
	  AND f.confidence >= $5
	ORDER BY f.confidence DESC, f.file_path, f.start_sec, f.id
	LIMIT $6 OFFSET $7
`

// ListCorrections returns findings with the chunk context needed to review
// them, highest-confidence first (the triage order the dashboard worklist uses).
// Read-only.
func (db *DB) ListCorrections(ctx context.Context, f CorrectionFilter) ([]CorrectionDetail, error) {
	return db.listCorrections(ctx, db.pool, f)
}

// listCorrections is the querier-parameterized core of ListCorrections, split
// out so the query + scan path is testable against a mock pool (mirrors
// listFindings).
func (db *DB) listCorrections(ctx context.Context, q rowQuerier, f CorrectionFilter) ([]CorrectionDetail, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultCorrectionListLimit
	}
	if limit > maxCorrectionListLimit {
		limit = maxCorrectionListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// NULL rather than an empty string for the unset filters: the query tests
	// each with `$n IS NULL`, so an empty string would filter on "" and match
	// nothing.
	var id, path, prefix *string
	if f.ID != "" {
		v := f.ID
		id = &v
	}
	if p := trimPath(f.Path); p != "" {
		v := p
		path = &v
		lp := likePrefix(p) + "/%"
		prefix = &lp
	}
	var states []string
	if len(f.States) > 0 {
		states = f.States
	}

	rows, err := q.Query(ctx, listCorrectionsSQL, id, path, prefix, states, f.MinConfidence, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list corrections query: %w", err)
	}
	defer rows.Close()

	var out []CorrectionDetail
	for rows.Next() {
		var c CorrectionDetail
		if err := rows.Scan(&c.ID, &c.TranscriptID, &c.FilePath, &c.BookDir,
			&c.ChunkID, &c.ChunkIndex, &c.StartSec, &c.EndSec,
			&c.OriginalText, &c.SuggestedCorrection, &c.IssueType, &c.Confidence,
			&c.Model, &c.Origin, &c.PatchState, &c.StaleReason,
			&c.DecidedAt, &c.DecidedBy, &c.AppliedAt, &c.CreatedAt,
			&c.AnchorOffset, &c.AnchorOccurrence, &c.ChunkTextSHA256,
			&c.ChunkText,
		); err != nil {
			return nil, fmt.Errorf("scan correction detail: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (list corrections): %w", err)
	}
	return out, nil
}

// ChunkTarget is a chunk addressed for a direct edit: its identity, its place
// in the transcript, and its PRISTINE text.
//
// Corrected is the current projected surface (transcript_chunks.text). It is
// returned for display only — an anchor is never resolved against it.
type ChunkTarget struct {
	ID           string
	TranscriptID string
	FilePath     string
	ChunkIndex   int
	StartSec     float64
	EndSec       float64
	Pristine     string
	Corrected    string
}

// chunkTargetSQL fetches one chunk as the direct-edit path needs it. Read-only.
var chunkTargetSQL = `
	SELECT c.id, c.transcript_id, c.file_path, c.chunk_index,
	       c.start_sec, c.end_sec,
	       COALESCE(c.source_text, c.text) AS pristine, c.text
	FROM transcript_chunks c
	WHERE c.id = $1
`

// GetChunkForEdit returns one chunk by id, with the pristine text a direct edit
// must anchor against. Read-only. ErrChunkNotFound when there is no such row.
func (db *DB) GetChunkForEdit(ctx context.Context, chunkID string) (*ChunkTarget, error) {
	return db.getChunkForEdit(ctx, db.pool, chunkID)
}

func (db *DB) getChunkForEdit(ctx context.Context, q rowScanner, chunkID string) (*ChunkTarget, error) {
	var c ChunkTarget
	err := q.QueryRow(ctx, chunkTargetSQL, chunkID).Scan(&c.ID, &c.TranscriptID, &c.FilePath,
		&c.ChunkIndex, &c.StartSec, &c.EndSec, &c.Pristine, &c.Corrected)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrChunkNotFound, chunkID)
	}
	if err != nil {
		return nil, fmt.Errorf("get chunk %s for edit: %w", chunkID, err)
	}
	return &c, nil
}

// ManualCorrection is a human-authored correction, already verified against the
// chunk's pristine text by patch.PlanDirectEdit.
//
// The caller supplies WHAT to change; every field describing WHERE the chunk
// sits (transcript, path, index, timings) is read from the chunk row by the
// INSERT itself, so a caller cannot file a correction against one chunk while
// labelling it another.
type ManualCorrection struct {
	// ChunkID addresses the chunk. Required.
	ChunkID string
	// OriginalText is the verbatim span being replaced, ChunkSHA256 fingerprints
	// the pristine text it was located in, and AnchorOffset/AnchorOccurrence are
	// the RESOLVED position — patch.PlanDirectEdit's output, never a caller's
	// claim about where the span is.
	OriginalText     string
	Correction       string
	AnchorOffset     int
	AnchorOccurrence int
	ChunkSHA256      string
	// IssueType classifies the correction, from the same closed vocabulary the
	// judge uses.
	IssueType string
	// DecidedBy records who made the decision. Stored as decided_by; empty
	// becomes NULL.
	DecidedBy string
}

// insertManualCorrectionSQL records a human-authored correction.
//
// Three properties are load-bearing, and all three come from it being an
// INSERT … SELECT over the chunk row rather than a VALUES list:
//
//  1. transcript_id, file_path, chunk_index and the timings are taken FROM THE
//     CHUNK, so the row cannot describe a chunk it is not attached to.
//  2. The hash guard (`… = $12`, the same value stored as chunk_text_sha256) is
//     evaluated against the chunk's CURRENT pristine text inside the write. A
//     rebuild that changed the chunk between verification and insert matches
//     zero rows, so the edit is refused instead of being recorded against a
//     revision nobody verified. Hex-encoded SHA-256 of the UTF-8 bytes — the
//     same fingerprint patch.ChunkHash computes in Go.
//  3. patch_state is bound to 'accepted' (not 'proposed'): a direct edit IS the
//     human decision, so decided_at/decided_by are stamped in the same row. It
//     never reaches 'applied' here — only a successful rebuild promotes it, so
//     the audit trail still distinguishes "approved" from "in the corpus".
var insertManualCorrectionSQL = `
	INSERT INTO transcript_findings
	       (transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
	        original_text, issue_type, suggested_correction, confidence, model,
	        origin, patch_state, decided_at, decided_by,
	        anchor_offset, anchor_occurrence, chunk_text_sha256)
	SELECT c.transcript_id, c.file_path, c.id, c.chunk_index, c.start_sec, c.end_sec,
	       $2, $3, $4, $5, $6,
	       $7, $8, now(), NULLIF($9, ''),
	       $10, $11, $12
	FROM transcript_chunks c
	WHERE c.id = $1
	  AND encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex') = $12
	RETURNING id
`

// markChunkStaleByIDSQL flags the edited chunk for re-embed. Writes ONLY
// transcript_chunks.embedding_stale — never the chunk text, which the rebuild
// regenerates, and never transcripts.
var markChunkStaleByIDSQL = `
	UPDATE transcript_chunks
	SET embedding_stale = true
	WHERE id = $1
`

// InsertManualCorrection records a direct, human-authored correction and flags
// its chunk for re-embed, in ONE transaction. Returns the new finding id.
//
// This is the escape hatch for an edit no model proposed, and it deliberately
// takes the SAME path as an accepted judge finding: same table, same anchor
// columns, same replay, same invalidation. It writes no transcript text — a
// correction is never stored in the projection, because the projection is
// regenerated and replayed on every embed (CONTRACT §2.17). What makes it
// distinguishable in the audit trail is origin = 'human' plus the reserved
// model/confidence sentinels, not the mechanism.
//
// Returns patch.ErrChunkChanged when the chunk no longer hashes to the revision
// the edit was verified against (or has disappeared) — the caller must re-read
// and re-verify rather than retry blindly.
func (db *DB) InsertManualCorrection(ctx context.Context, m ManualCorrection) (string, error) {
	return db.insertManualCorrection(ctx, db.pool, m)
}

func (db *DB) insertManualCorrection(ctx context.Context, b txBeginner, m ManualCorrection) (string, error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin manual-correction tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx, insertManualCorrectionSQL,
		m.ChunkID, m.OriginalText, m.IssueType, m.Correction,
		ManualEditConfidence, ManualEditModel,
		OriginHuman, patch.StateAccepted, m.DecidedBy,
		m.AnchorOffset, m.AnchorOccurrence, m.ChunkSHA256,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The guarded INSERT matched no chunk: the row is gone, or its pristine
		// text no longer hashes to what was verified.
		return "", fmt.Errorf("%w: chunk %s moved under the edit", patch.ErrChunkChanged, m.ChunkID)
	}
	if err != nil {
		return "", fmt.Errorf("insert manual correction for chunk %s: %w", m.ChunkID, err)
	}

	// Same transaction as the decision: a correction recorded without its
	// invalidation would leave the corpus permanently showing text that
	// disagrees with its own findings.
	if _, err := tx.Exec(ctx, markChunkStaleByIDSQL, m.ChunkID); err != nil {
		return "", fmt.Errorf("flag chunk %s stale after manual correction: %w", m.ChunkID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit manual correction for chunk %s: %w", m.ChunkID, err)
	}
	return id, nil
}
