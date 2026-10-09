package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// Read-only selection for `earmark decide` (CONTRACT §2.19). Every statement
// here is a SELECT: the dry run writes nothing but fn_calls and recipes, and
// TestDecideSelectionSQLIsReadOnly pins that.

// MaxDecideSample caps how many findings one decide run may sample.
const MaxDecideSample = 10000

// MaxDecideChunkBatch caps the chunks one DecideChunks call may load.
const MaxDecideChunkBatch = 512

// DecideScope selects findings for a decide run.
type DecideScope struct {
	// Sample is how many findings to take (1..MaxDecideSample).
	Sample int
	// Seed orders the sample: md5(id || seed). The same seed and the same
	// backlog pick the same findings.
	Seed string
	// Book keeps findings whose file path contains it (case-insensitive).
	Book string
	// IssueType keeps one issue type ("" = all).
	IssueType string
	// Calibrate selects findings a human already decided — decided_by
	// '<actor>:%' for one of HumanActors, and patch_state accepted/applied
	// (accepted by a human) or rejected — instead of the proposed backlog.
	Calibrate bool
	// HumanActors are the decided_by prefixes (without the colon) that count
	// as a person in calibration: "mcp" (the review surface) and/or "cli".
	// Empty = DefaultHumanActors.
	HumanActors []string
}

// DefaultHumanActors is calibration's default: decisions made through the
// review surface.
var DefaultHumanActors = []string{"mcp"}

var humanActorRe = regexp.MustCompile(`^[a-z]+$`)

// actorPatterns is the decided_by LIKE patterns calibration matches (empty
// when not calibrating).
func (s DecideScope) actorPatterns() []string {
	if !s.Calibrate {
		return []string{}
	}
	actors := s.HumanActors
	if len(actors) == 0 {
		actors = DefaultHumanActors
	}
	out := make([]string, len(actors))
	for i, a := range actors {
		out[i] = a + ":%"
	}
	return out
}

// decideStates is the state scope of a run: the proposed backlog, or the
// human-decided set calibration compares against.
func (s DecideScope) states() []string {
	if s.Calibrate {
		return []string{patch.StateAccepted, patch.StateApplied, patch.StateRejected}
	}
	return []string{patch.StateProposed}
}

func (s DecideScope) validate() error {
	switch {
	case s.Sample < 1 || s.Sample > MaxDecideSample:
		return fmt.Errorf("decide sample must be 1..%d", MaxDecideSample)
	case strings.TrimSpace(s.Seed) == "":
		return errors.New("decide seed is required")
	}
	for _, a := range s.HumanActors {
		if !humanActorRe.MatchString(a) || a == "jev" || a == "revert" {
			return fmt.Errorf("decide: %q is not a human actor prefix", a)
		}
	}
	return nil
}

func (s DecideScope) bookPattern() string {
	if strings.TrimSpace(s.Book) == "" {
		return ""
	}
	return likePattern(s.Book)
}

// DecideFinding is one anchored judge finding in scope.
type DecideFinding struct {
	ID               string
	TranscriptID     string
	FilePath         string
	IssueType        string
	Original         string
	Replacement      string
	Confidence       float64
	ChunkIndex       int
	AnchorOffset     int
	AnchorOccurrence int // -1 when unknown
	ChunkTextSHA256  string
	PatchState       string
	DecidedBy        string // "" when undecided
}

// decideScopeWhere is the shared filter: anchored judge findings (an anchor
// offset and a chunk hash, so rung 0 can verify the revision), in the scope's
// states, optionally one book / issue type, and — for calibration — decided
// by a person (decided_by LIKE one of the actor patterns, e.g. 'mcp:%').
//
//	$1 states  $2 book pattern ('' = all)  $3 issue type ('' = all)
//	$4 actor patterns (empty = not calibrating)
const decideScopeWhere = `
	 WHERE f.origin = 'judge'
	   AND f.transcript_id IS NOT NULL
	   AND f.chunk_index IS NOT NULL
	   AND f.anchor_offset IS NOT NULL
	   AND f.chunk_text_sha256 IS NOT NULL
	   AND f.patch_state = ANY($1)
	   AND ($2 = '' OR f.file_path ILIKE $2)
	   AND ($3 = '' OR f.issue_type = $3)
	   AND (cardinality($4::text[]) = 0 OR f.decided_by LIKE ANY($4::text[]))`

// decideSampleSQL samples the scope deterministically: md5(id || seed).
// $5 seed, $6 limit.
var decideSampleSQL = `
	SELECT f.id::text, f.transcript_id::text, f.file_path, f.issue_type, f.original_text,
	       COALESCE(f.suggested_correction, ''), f.confidence, f.chunk_index,
	       f.anchor_offset, COALESCE(f.anchor_occurrence, -1), f.chunk_text_sha256,
	       f.patch_state, COALESCE(f.decided_by, '')
	  FROM transcript_findings f` + decideScopeWhere + `
	 ORDER BY md5(f.id::text || $5), f.id
	 LIMIT $6`

// decideBacklogSQL counts the whole scope per issue type — the population a
// sample's rates are projected onto.
var decideBacklogSQL = `
	SELECT f.issue_type, count(*)
	  FROM transcript_findings f` + decideScopeWhere + `
	 GROUP BY f.issue_type
	 ORDER BY f.issue_type`

// DecideSample returns the scope's sample in seed order (read-only).
func (db *DB) DecideSample(ctx context.Context, s DecideScope) ([]DecideFinding, error) {
	return decideSample(ctx, db.pool, s)
}

func decideSample(ctx context.Context, q rowQuerier, s DecideScope) ([]DecideFinding, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, decideSampleSQL, s.states(), s.bookPattern(), s.IssueType, s.actorPatterns(), s.Seed, s.Sample)
	if err != nil {
		return nil, fmt.Errorf("decide sample: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanDecideFinding)
	if err != nil {
		return nil, fmt.Errorf("decide sample: %w", err)
	}
	return out, nil
}

func scanDecideFinding(r pgx.CollectableRow) (DecideFinding, error) {
	var f DecideFinding
	err := r.Scan(&f.ID, &f.TranscriptID, &f.FilePath, &f.IssueType, &f.Original, &f.Replacement,
		&f.Confidence, &f.ChunkIndex, &f.AnchorOffset, &f.AnchorOccurrence, &f.ChunkTextSHA256,
		&f.PatchState, &f.DecidedBy)
	return f, err
}

// DecideBacklog counts the scope (ignoring Sample) per issue type.
func (db *DB) DecideBacklog(ctx context.Context, s DecideScope) (map[string]int, error) {
	return decideBacklog(ctx, db.pool, s)
}

func decideBacklog(ctx context.Context, q rowQuerier, s DecideScope) (map[string]int, error) {
	rows, err := q.Query(ctx, decideBacklogSQL, s.states(), s.bookPattern(), s.IssueType, s.actorPatterns())
	if err != nil {
		return nil, fmt.Errorf("decide backlog: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, fmt.Errorf("decide backlog: %w", err)
		}
		out[t] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decide backlog: %w", err)
	}
	return out, nil
}

// ChunkKey names one chunk.
type ChunkKey struct {
	TranscriptID string
	ChunkIndex   int
}

// DecideChunk is one chunk a decide run reads: its PRISTINE text (the
// coordinate system every anchor and hash is recorded against), its time
// window, and the findings on it that rung 0 must see together — proposed,
// anchored judge findings (dedupe competitors) and accepted/applied
// corrections of any origin (the overlay a candidate must not overlap).
type DecideChunk struct {
	Key      ChunkKey
	Text     string
	StartSec float64
	EndSec   float64
	// Competitors are the chunk's proposed, anchored judge findings.
	Competitors []DecideFinding
	// Overlay is the chunk's accepted + applied corrections.
	Overlay []patch.Patch
}

var decideChunksSQL = `
	SELECT c.transcript_id::text, c.chunk_index, COALESCE(c.source_text, c.text), c.start_sec, c.end_sec
	  FROM transcript_chunks c
	  JOIN unnest($1::uuid[], $2::int[]) AS k(tid, idx)
	    ON c.transcript_id = k.tid AND c.chunk_index = k.idx
	 ORDER BY c.transcript_id, c.chunk_index`

// decideChunkFindingsSQL reads every finding on the chunks that rung 0 has
// to weigh: $3 = proposed (competitors, judge-only, anchored) and $4 = the
// overlay states (any origin).
var decideChunkFindingsSQL = `
	SELECT f.id::text, f.transcript_id::text, f.file_path, f.issue_type, f.original_text,
	       COALESCE(f.suggested_correction, ''), f.confidence, f.chunk_index,
	       COALESCE(f.anchor_offset, -1), COALESCE(f.anchor_occurrence, -1), COALESCE(f.chunk_text_sha256, ''),
	       f.patch_state, COALESCE(f.decided_by, '')
	  FROM transcript_findings f
	  JOIN unnest($1::uuid[], $2::int[]) AS k(tid, idx)
	    ON f.transcript_id = k.tid AND f.chunk_index = k.idx
	 WHERE f.patch_state = ANY($4)
	    OR (f.patch_state = $3 AND f.origin = 'judge'
	        AND f.anchor_offset IS NOT NULL AND f.chunk_text_sha256 IS NOT NULL)
	 ORDER BY f.transcript_id, f.chunk_index, f.id`

// DecideChunks loads the listed chunks with their competitors and overlay.
// A chunk that no longer exists is absent from the result. At most
// MaxDecideChunkBatch keys per call.
func (db *DB) DecideChunks(ctx context.Context, keys []ChunkKey) (map[ChunkKey]*DecideChunk, error) {
	return decideChunks(ctx, db.pool, keys)
}

func decideChunks(ctx context.Context, q rowQuerier, keys []ChunkKey) (map[ChunkKey]*DecideChunk, error) {
	if len(keys) > MaxDecideChunkBatch {
		return nil, fmt.Errorf("decide chunks: %d keys, max %d per call", len(keys), MaxDecideChunkBatch)
	}
	out := make(map[ChunkKey]*DecideChunk, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	tids := make([]string, len(keys))
	idxs := make([]int, len(keys))
	for i, k := range keys {
		tids[i], idxs[i] = k.TranscriptID, k.ChunkIndex
	}
	rows, err := q.Query(ctx, decideChunksSQL, tids, idxs)
	if err != nil {
		return nil, fmt.Errorf("decide chunks: %w", err)
	}
	chunks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*DecideChunk, error) {
		c := &DecideChunk{}
		err := r.Scan(&c.Key.TranscriptID, &c.Key.ChunkIndex, &c.Text, &c.StartSec, &c.EndSec)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("decide chunks: %w", err)
	}
	for _, c := range chunks {
		out[c.Key] = c
	}

	rows, err = q.Query(ctx, decideChunkFindingsSQL, tids, idxs, patch.StateProposed, overlayStates)
	if err != nil {
		return nil, fmt.Errorf("decide chunk findings: %w", err)
	}
	fs, err := pgx.CollectRows(rows, scanDecideFinding)
	if err != nil {
		return nil, fmt.Errorf("decide chunk findings: %w", err)
	}
	for _, f := range fs {
		c, ok := out[ChunkKey{f.TranscriptID, f.ChunkIndex}]
		if !ok {
			continue
		}
		if f.PatchState == patch.StateProposed {
			c.Competitors = append(c.Competitors, f)
			continue
		}
		c.Overlay = append(c.Overlay, patch.Patch{
			ID:         f.ID,
			Anchor:     patch.Anchor{OriginalText: f.Original, Offset: f.AnchorOffset, Occurrence: f.AnchorOccurrence},
			Correction: f.Replacement,
			ChunkHash:  f.ChunkTextSHA256,
		})
	}
	return out, nil
}
