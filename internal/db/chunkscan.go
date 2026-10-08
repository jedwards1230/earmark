package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Chunk scan (CONTRACT §1.9 "Chunk scan"): `earmark scan` asks a decision
// model six questions about each chunk's pristine text and stores the answers
// in chunk_scan, one row per (chunk position, chunk text hash, recipe). The
// scan READS transcript_chunks / transcripts / book_metadata and WRITES only
// chunk_scan (plus fn_calls, through internal/fn); insertChunkScanSQL is the
// one write and TestChunkScanWritesOnlyChunkScan pins it.

// DefaultScanContextSegments is how many neighbouring transcript segments on
// each side of a chunk the scan shows the model as context.
const DefaultScanContextSegments = 2

// MaxScanContextSegments bounds the neighbours per side.
const MaxScanContextSegments = 5

// defaultScanPage is the candidate page size of an unsampled scan.
const defaultScanPage = 500

// ScanCandidate is one chunk to scan: its position, its pristine text (the
// text the model judges, and whose sha256 the result records), and the text
// of up to N transcript segments just before and just after it (context the
// model is told not to judge).
type ScanCandidate struct {
	ChunkID       string
	TranscriptID  string
	FilePath      string
	ChunkIndex    int
	Text          string // COALESCE(source_text, text)
	TextSHA256    string // sha256 of Text, computed by Postgres the way the write re-checks it
	ContextBefore []string
	ContextAfter  []string
}

// ScanScope selects the chunks a scan visits.
type ScanScope struct {
	// RecipeID excludes chunks whose CURRENT text already has a scan by this
	// recipe. "" excludes nothing.
	RecipeID string
	// Book restricts to chunks whose file_path contains it (case-insensitive).
	Book string
	// Sample > 0 picks at most Sample chunks, in an order fixed by Seed (the
	// same seed over the same library picks the same chunks). 0 = every
	// candidate, in (transcript_id, chunk_index) order, paged with After.
	Sample int
	Seed   string
	// ContextSegments is the neighbour count per side (0 → none).
	ContextSegments int
	// After is the keyset cursor of an unsampled walk (zero = from the start);
	// Limit its page size (≤0 → a default).
	After ScanCursor
	Limit int
}

// ScanCursor is a keyset position over chunks ordered by (transcript_id,
// chunk_index). Keyset paging lets a dry run (which writes nothing, so its
// candidates never drop out) walk the library in bounded pages.
type ScanCursor struct {
	TranscriptID string
	ChunkIndex   int
}

// Cursor is the position just past c.
func (c ScanCandidate) Cursor() ScanCursor {
	return ScanCursor{TranscriptID: c.TranscriptID, ChunkIndex: c.ChunkIndex}
}

// scanCandidatesSQL selects chunks to scan. $1 recipe ("" = no exclusion),
// $2 book pattern ("" = all), $3 context segments per side, $4 sample (0 =
// keyset walk), $5 seed, $6/$7 keyset cursor (NULL = start), $8 limit.
//
// Context comes from the transcript's segments (CONTRACT §1.2.1): a chunk is
// whole segments joined by a space, so its neighbours are the segments ending
// at or before its start and starting at or after its end (a 1 ms tolerance
// absorbs float noise). Only the segment text is read, never the word list.
const scanCandidatesSQL = `
	WITH c AS (
		SELECT c.id, c.transcript_id, c.file_path, c.chunk_index, c.start_sec, c.end_sec,
		       COALESCE(c.source_text, c.text) AS text,
		       encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex') AS sha
		  FROM transcript_chunks c
		 WHERE ($2 = '' OR c.file_path ILIKE $2)
		   AND ($4 > 0 OR $6::uuid IS NULL OR (c.transcript_id, c.chunk_index) > ($6::uuid, $7::int))
	)
	SELECT c.id, c.transcript_id, c.file_path, c.chunk_index, c.text, c.sha,
	       COALESCE((SELECT array_agg(x.txt ORDER BY x.st)
	                   FROM (SELECT s->>'text' AS txt, (s->>'start')::float8 AS st
	                           FROM transcripts t, jsonb_array_elements(t.segments) s
	                          WHERE t.id = c.transcript_id AND $3 > 0
	                            AND (s->>'end')::float8 <= c.start_sec + 0.001
	                          ORDER BY (s->>'start')::float8 DESC
	                          LIMIT $3) x), '{}') AS before,
	       COALESCE((SELECT array_agg(x.txt ORDER BY x.st)
	                   FROM (SELECT s->>'text' AS txt, (s->>'start')::float8 AS st
	                           FROM transcripts t, jsonb_array_elements(t.segments) s
	                          WHERE t.id = c.transcript_id AND $3 > 0
	                            AND (s->>'start')::float8 >= c.end_sec - 0.001
	                          ORDER BY (s->>'start')::float8
	                          LIMIT $3) x), '{}') AS after
	  FROM c
	 WHERE $1 = '' OR NOT EXISTS (
	         SELECT 1 FROM chunk_scan s
	          WHERE s.transcript_id = c.transcript_id AND s.chunk_index = c.chunk_index
	            AND s.chunk_text_sha256 = c.sha AND s.recipe_id = $1)
	 ORDER BY CASE WHEN $4 > 0 THEN md5($5 || ':' || c.id::text) END,
	          c.transcript_id, c.chunk_index
	 LIMIT $8
`

// ScanCandidates returns the chunks scope selects (read-only).
func (db *DB) ScanCandidates(ctx context.Context, scope ScanScope) ([]ScanCandidate, error) {
	return scanCandidates(ctx, db.pool, scope)
}

func scanCandidates(ctx context.Context, q rowQuerier, scope ScanScope) ([]ScanCandidate, error) {
	if scope.ContextSegments < 0 || scope.ContextSegments > MaxScanContextSegments {
		return nil, fmt.Errorf("scan context segments must be 0..%d", MaxScanContextSegments)
	}
	if scope.Sample < 0 {
		return nil, errors.New("scan sample must be >= 0")
	}
	limit := scope.Limit
	if scope.Sample > 0 {
		limit = scope.Sample
	} else if limit <= 0 {
		limit = defaultScanPage
	}
	book := ""
	if strings.TrimSpace(scope.Book) != "" {
		book = likePattern(scope.Book)
	}
	var afterID *string
	if scope.After.TranscriptID != "" {
		afterID = &scope.After.TranscriptID
	}
	rows, err := q.Query(ctx, scanCandidatesSQL, scope.RecipeID, book, scope.ContextSegments,
		scope.Sample, scope.Seed, afterID, scope.After.ChunkIndex, limit)
	if err != nil {
		return nil, fmt.Errorf("scan candidates: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ScanCandidate, error) {
		var c ScanCandidate
		err := r.Scan(&c.ChunkID, &c.TranscriptID, &c.FilePath, &c.ChunkIndex, &c.Text, &c.TextSHA256,
			&c.ContextBefore, &c.ContextAfter)
		return c, err
	})
}

// ChunkScan is one chunk_scan row to insert. Probabilities are in [0,1],
// Quality in [1,5]; QualityConfidence is nil when the model gave none.
type ChunkScan struct {
	TranscriptID      string
	ChunkIndex        int
	ChunkTextSHA256   string
	RecipeID          string
	FnCallID          int64 // 0 → NULL (a concurrent identical call holds the cache row)
	PNeedsFix         float64
	Quality           float64
	QualityConfidence *float64
	PBoilerplate      float64
	PGarbled          float64
	PDialogue         float64
	IssueType         string
	IssueProbs        map[string]float64
}

// ChunkScanOutcome is what InsertChunkScan did.
type ChunkScanOutcome string

const (
	// ChunkScanInserted: the row was written.
	ChunkScanInserted ChunkScanOutcome = "inserted"
	// ChunkScanExists: this chunk text already has a scan by this recipe.
	ChunkScanExists ChunkScanOutcome = "exists"
	// ChunkScanChanged: the chunk is gone or its text no longer hashes to
	// ChunkTextSHA256 (a re-chunk or rebuild ran since it was read) —
	// nothing is written; the next scan picks the new text up.
	ChunkScanChanged ChunkScanOutcome = "changed"
)

// insertChunkScanSQL writes one scan result. It is a single statement, so it
// is its own short transaction: the chunk's pristine text is re-hashed in the
// same snapshot the row is inserted from, and a chunk whose text changed since
// the model saw it gets no row. ON CONFLICT on the unique key makes a re-run
// (or a concurrent scanner) a no-op. It writes to chunk_scan and nothing else.
var insertChunkScanSQL = `
	WITH cur AS (
		SELECT 1 FROM transcript_chunks c
		 WHERE c.transcript_id = $1 AND c.chunk_index = $2
		   AND encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex') = $3
	), ins AS (
		INSERT INTO chunk_scan (transcript_id, chunk_index, chunk_text_sha256, recipe_id, fn_call_id,
		                        p_needs_fix, quality, quality_confidence, p_boilerplate, p_garbled,
		                        p_dialogue, issue_type, issue_probs)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb FROM cur
		ON CONFLICT (transcript_id, chunk_index, chunk_text_sha256, recipe_id) DO NOTHING
		RETURNING id
	)
	SELECT EXISTS (SELECT 1 FROM cur), EXISTS (SELECT 1 FROM ins)
`

func (s ChunkScan) validate() error {
	unit := func(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 1 }
	switch {
	case s.TranscriptID == "" || s.RecipeID == "":
		return errors.New("chunk scan: transcript id and recipe id are required")
	case s.ChunkIndex < 0:
		return errors.New("chunk scan: negative chunk index")
	case !sha256HexRe.MatchString(s.ChunkTextSHA256):
		return errors.New("chunk scan: chunk_text_sha256 is not lowercase hex sha256")
	case !unit(s.PNeedsFix) || !unit(s.PBoilerplate) || !unit(s.PGarbled) || !unit(s.PDialogue):
		return errors.New("chunk scan: probability outside [0,1]")
	case math.IsNaN(s.Quality) || s.Quality < 1 || s.Quality > 5:
		return errors.New("chunk scan: quality outside [1,5]")
	case s.QualityConfidence != nil && !unit(*s.QualityConfidence):
		return errors.New("chunk scan: quality confidence outside [0,1]")
	case s.IssueType == "" || len(s.IssueProbs) == 0:
		return errors.New("chunk scan: issue type and probabilities are required")
	}
	return nil
}

// InsertChunkScan writes s unless the chunk's text changed or the scan
// already exists (see insertChunkScanSQL).
func (db *DB) InsertChunkScan(ctx context.Context, s ChunkScan) (ChunkScanOutcome, error) {
	return insertChunkScan(ctx, db.pool, s)
}

func insertChunkScan(ctx context.Context, q rowScanner, s ChunkScan) (ChunkScanOutcome, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	probs, err := json.Marshal(s.IssueProbs)
	if err != nil {
		return "", fmt.Errorf("chunk scan issue probs: %w", err)
	}
	var fnCall *int64
	if s.FnCallID != 0 {
		fnCall = &s.FnCallID
	}
	var found, inserted bool
	if err := q.QueryRow(ctx, insertChunkScanSQL, s.TranscriptID, s.ChunkIndex, s.ChunkTextSHA256,
		s.RecipeID, fnCall, s.PNeedsFix, s.Quality, s.QualityConfidence, s.PBoilerplate, s.PGarbled,
		s.PDialogue, s.IssueType, string(probs)).Scan(&found, &inserted); err != nil {
		return "", fmt.Errorf("insert chunk scan: %w", err)
	}
	switch {
	case inserted:
		return ChunkScanInserted, nil
	case found:
		return ChunkScanExists, nil
	default:
		return ChunkScanChanged, nil
	}
}

// QualityGroup is the scan results of one recipe over one side of the ASIN
// split: how many chunks and the sum of their quality (1..5).
type QualityGroup struct {
	RecipeID    string
	ASINMatched bool
	Chunks      int64
	QualitySum  float64
}

// qualityGroupsSQL aggregates the scans of every chunk's CURRENT text
// (a scan of text a re-chunk replaced no longer describes the library),
// excluding chunks the model judged boilerplate ($1 = the p_boilerplate cut;
// credits, ads, chapter announcements are not transcription quality). Per
// recipe, split by whether the chunk's book has a catalogue (ASIN) record.
// The unique key admits one row per (chunk text, recipe), so there is no
// "latest" to pick.
const qualityGroupsSQL = `
	SELECT s.recipe_id,
	       COALESCE(bm.asin, '') <> '' AS asin_matched,
	       count(*), sum(s.quality)
	  FROM chunk_scan s
	  JOIN transcript_chunks c
	    ON c.transcript_id = s.transcript_id AND c.chunk_index = s.chunk_index
	   AND encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex') = s.chunk_text_sha256
	  LEFT JOIN book_metadata bm ON bm.book_dir = regexp_replace(c.file_path, '/[^/]+$', '')
	 WHERE s.p_boilerplate <= $1
	 GROUP BY 1, 2
	 ORDER BY 1, 2
`

// QualityGroups returns the per-recipe, per-ASIN-side quality sums behind
// earmark_quality_index, excluding chunks with p_boilerplate above
// boilerplateCut (read-only).
func (db *DB) QualityGroups(ctx context.Context, boilerplateCut float64) ([]QualityGroup, error) {
	rows, err := db.pool.Query(ctx, qualityGroupsSQL, boilerplateCut)
	if err != nil {
		return nil, fmt.Errorf("quality groups: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (QualityGroup, error) {
		var g QualityGroup
		err := r.Scan(&g.RecipeID, &g.ASINMatched, &g.Chunks, &g.QualitySum)
		return g, err
	})
}
