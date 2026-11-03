package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The second-ASR pilot (CONTRACT §1.11, migration 00017): pilot transcripts
// written by runner/pilot.py, and the asr_vote evidence `earmark asr-pilot
// vote` records from them. Every read here is bounded — a page of at most
// MaxPilotPage rows, or one transcript's segments — because segments are the
// largest column in the schema. Nothing in decide reads these tables.

// MaxPilotPage caps every paged pilot read and every vote upsert batch.
const MaxPilotPage = 100

// Pilot scopes (pilot_transcripts.scope).
const (
	PilotScopeFull     = "full"     // the whole track
	PilotScopeFindings = "findings" // only the windows around held findings
)

// PilotTranscript is one pilot_transcripts row without its segments and raw
// text (load those with PilotSegments, one transcript at a time).
type PilotTranscript struct {
	ID                     int64
	FilePath               string
	ProductionTranscriptID string
	Backend                string
	ModelID                string
	ModelRevision          string
	Scope                  string
	WindowSec              *float64
	HasWordTimes           bool
	AudioSec               *float64
	WallSec                *float64
	RTFx                   *float64
	PeakVRAMMiB            *int64
	PeakReservedMiB        *int64
	CreatedAt              time.Time
}

// PilotWord is one word of a pilot segment. Start and End are nil when the
// model emits no word timestamps (pilot_transcripts.has_word_times = false).
type PilotWord struct {
	Word  string   `json:"word"`
	Start *float64 `json:"start,omitempty"`
	End   *float64 `json:"end,omitempty"`
}

// PilotSegment is one pilot segment (CONTRACT §1.2.1 shape; words may lack
// times). Times are track-relative seconds.
type PilotSegment struct {
	ID    int         `json:"id"`
	Start float64     `json:"start"`
	End   float64     `json:"end"`
	Text  string      `json:"text"`
	Words []PilotWord `json:"words"`
}

// PilotFilter selects pilot transcripts. Empty fields do not filter.
type PilotFilter struct {
	ModelID       string
	ModelRevision string
	// BookDirs restricts to tracks in these book directories
	// (filepath.Dir of file_path, as book_metadata.book_dir).
	BookDirs []string
	// AfterID pages by id: rows with id > AfterID, ascending.
	AfterID int64
	// Limit is the page size, 1..MaxPilotPage.
	Limit int
}

var pilotTranscriptsSQL = `
	SELECT id, file_path, production_transcript_id::text, backend, model_id, model_revision, scope,
	       window_sec, has_word_times, audio_sec, wall_sec, rtfx, peak_vram_mib, peak_reserved_mib, created_at
	  FROM pilot_transcripts
	 WHERE id > $1
	   AND ($2 = '' OR model_id = $2)
	   AND ($3 = '' OR model_revision = $3)
	   AND (cardinality($4::text[]) = 0 OR regexp_replace(file_path, '/[^/]+$', '') = ANY($4::text[]))
	 ORDER BY id
	 LIMIT $5`

// PilotTranscripts returns one page of pilot transcripts (metadata only).
func (db *DB) PilotTranscripts(ctx context.Context, f PilotFilter) ([]PilotTranscript, error) {
	return pilotTranscripts(ctx, db.pool, f)
}

func pilotTranscripts(ctx context.Context, q rowQuerier, f PilotFilter) ([]PilotTranscript, error) {
	if f.Limit < 1 || f.Limit > MaxPilotPage {
		return nil, fmt.Errorf("pilot transcripts: limit %d, want 1..%d", f.Limit, MaxPilotPage)
	}
	dirs := f.BookDirs
	if dirs == nil {
		dirs = []string{}
	}
	rows, err := q.Query(ctx, pilotTranscriptsSQL, f.AfterID, f.ModelID, f.ModelRevision, dirs, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("pilot transcripts: %w", err)
	}
	defer rows.Close()
	var out []PilotTranscript
	for rows.Next() {
		var p PilotTranscript
		var window, audio, wall, rtfx *float32
		if err := rows.Scan(&p.ID, &p.FilePath, &p.ProductionTranscriptID, &p.Backend, &p.ModelID,
			&p.ModelRevision, &p.Scope, &window, &p.HasWordTimes, &audio, &wall, &rtfx,
			&p.PeakVRAMMiB, &p.PeakReservedMiB, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("pilot transcripts: scan: %w", err)
		}
		p.WindowSec, p.AudioSec, p.WallSec, p.RTFx = f64(window), f64(audio), f64(wall), f64(rtfx)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pilot transcripts: %w", err)
	}
	return out, nil
}

func f64(v *float32) *float64 {
	if v == nil {
		return nil
	}
	x := float64(*v)
	return &x
}

// PilotSegments returns the segments of one pilot transcript (nil when the
// row does not exist).
func (db *DB) PilotSegments(ctx context.Context, id int64) ([]PilotSegment, error) {
	return pilotSegments(ctx, db.pool, id)
}

func pilotSegments(ctx context.Context, q rowQuerier, id int64) ([]PilotSegment, error) {
	rows, err := q.Query(ctx, `SELECT segments FROM pilot_transcripts WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("pilot segments %d: %w", id, err)
	}
	defer rows.Close()
	var segs []PilotSegment
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("pilot segments %d: scan: %w", id, err)
		}
		if err := json.Unmarshal(raw, &segs); err != nil {
			return nil, fmt.Errorf("pilot segments %d: %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pilot segments %d: %w", id, err)
	}
	return segs, nil
}

// BookDirsForASINs resolves catalogue ASINs to book directories
// (book_metadata.book_dir). At most MaxDecideBookBatch ASINs per call.
func (db *DB) BookDirsForASINs(ctx context.Context, asins []string) ([]string, error) {
	return bookDirsForASINs(ctx, db.pool, asins)
}

func bookDirsForASINs(ctx context.Context, q rowQuerier, asins []string) ([]string, error) {
	if len(asins) > MaxDecideBookBatch {
		return nil, fmt.Errorf("book dirs for asins: %d asins, max %d per call", len(asins), MaxDecideBookBatch)
	}
	if len(asins) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT book_dir FROM book_metadata WHERE asin = ANY($1::text[]) ORDER BY book_dir LIMIT $2`,
		asins, MaxDecideBookBatch)
	if err != nil {
		return nil, fmt.Errorf("book dirs for asins: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("book dirs for asins: scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("book dirs for asins: %w", err)
	}
	return out, nil
}

// HeldFinding is a held finding (patch_state proposed, latest unrevoked
// decision a hold) with the chunk it was judged against, as the asr_vote
// needs it. ChunkFound is false when the chunk no longer exists.
type HeldFinding struct {
	ID                  string
	ChunkIndex          int
	OriginalText        string
	SuggestedCorrection string
	AnchorOffset        int // -1 when unknown
	AnchorOccurrence    int // -1 when unknown
	ChunkTextSHA256     string
	ChunkFound          bool
	ChunkText           string // COALESCE(source_text, text): the pristine text
	ChunkStartSec       float64
	ChunkEndSec         float64
}

// heldFindingsSQL: held = proposed whose latest decision event (not revoked)
// is a hold — the same latest-decision rule as decide's reads. Keyset paged
// by finding id. $1 transcript, $2 after id, $3 limit.
var heldFindingsSQL = `
	SELECT f.id::text, f.chunk_index, f.original_text, COALESCE(f.suggested_correction, ''),
	       COALESCE(f.anchor_offset, -1), COALESCE(f.anchor_occurrence, -1),
	       COALESCE(f.chunk_text_sha256, ''),
	       c.chunk_index IS NOT NULL, COALESCE(c.source_text, c.text, ''),
	       COALESCE(c.start_sec, 0), COALESCE(c.end_sec, 0)
	  FROM transcript_findings f
	  JOIN LATERAL (
	        SELECT e.outcome FROM finding_events e
	         WHERE e.finding_id = f.id AND e.kind = 'decision'
	           AND NOT EXISTS (SELECT 1 FROM finding_events v
	                            WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
	         ORDER BY e.id DESC
	         LIMIT 1) ld ON ld.outcome = 'hold'
	  LEFT JOIN transcript_chunks c
	    ON c.transcript_id = f.transcript_id AND c.chunk_index = f.chunk_index
	 WHERE f.transcript_id = $1::uuid
	   AND f.patch_state = 'proposed'
	   AND f.chunk_index IS NOT NULL
	   AND f.id > $2::uuid
	 ORDER BY f.id
	 LIMIT $3`

// FirstFindingID is the keyset start for HeldFindings.
const FirstFindingID = "00000000-0000-0000-0000-000000000000"

// HeldFindings returns one page (1..MaxPilotPage) of a transcript's held
// findings with ids greater than afterID (FirstFindingID to start).
func (db *DB) HeldFindings(ctx context.Context, transcriptID, afterID string, limit int) ([]HeldFinding, error) {
	return heldFindings(ctx, db.pool, transcriptID, afterID, limit)
}

func heldFindings(ctx context.Context, q rowQuerier, transcriptID, afterID string, limit int) ([]HeldFinding, error) {
	if limit < 1 || limit > MaxPilotPage {
		return nil, fmt.Errorf("held findings: limit %d, want 1..%d", limit, MaxPilotPage)
	}
	rows, err := q.Query(ctx, heldFindingsSQL, transcriptID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("held findings: %w", err)
	}
	defer rows.Close()
	var out []HeldFinding
	for rows.Next() {
		var h HeldFinding
		if err := rows.Scan(&h.ID, &h.ChunkIndex, &h.OriginalText, &h.SuggestedCorrection,
			&h.AnchorOffset, &h.AnchorOccurrence, &h.ChunkTextSHA256,
			&h.ChunkFound, &h.ChunkText, &h.ChunkStartSec, &h.ChunkEndSec); err != nil {
			return nil, fmt.Errorf("held findings: scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("held findings: %w", err)
	}
	return out, nil
}

// FindingASRVote is one finding_asr_votes row to upsert.
type FindingASRVote struct {
	FindingID         string
	PilotTranscriptID int64
	ModelID           string
	ChunkTextSHA256   string // "" = NULL
	SpanStartSec      *float64
	SpanEndSec        *float64
	Vote              string
	HeardText         string
	Detail            json.RawMessage // a JSON object; nil = {}
}

var upsertFindingASRVotesSQL = `
	INSERT INTO finding_asr_votes (finding_id, pilot_transcript_id, model_id, chunk_text_sha256,
	                               span_start_sec, span_end_sec, vote, heard_text, detail)
	SELECT v.finding_id, v.pilot_id, v.model_id, NULLIF(v.sha, ''), v.t0, v.t1, v.vote, v.heard, v.detail::jsonb
	  FROM unnest($1::uuid[], $2::int8[], $3::text[], $4::text[], $5::float8[], $6::float8[],
	              $7::text[], $8::text[], $9::text[])
	       AS v(finding_id, pilot_id, model_id, sha, t0, t1, vote, heard, detail)
	ON CONFLICT (finding_id, pilot_transcript_id) DO UPDATE
	   SET model_id = EXCLUDED.model_id, chunk_text_sha256 = EXCLUDED.chunk_text_sha256,
	       span_start_sec = EXCLUDED.span_start_sec, span_end_sec = EXCLUDED.span_end_sec,
	       vote = EXCLUDED.vote, heard_text = EXCLUDED.heard_text, detail = EXCLUDED.detail,
	       updated_at = now()`

// UpsertFindingASRVotes writes at most MaxPilotPage votes in one statement,
// replacing an earlier vote of the same (finding, pilot transcript).
func (db *DB) UpsertFindingASRVotes(ctx context.Context, votes []FindingASRVote) (int64, error) {
	return upsertFindingASRVotes(ctx, db.pool, votes)
}

func upsertFindingASRVotes(ctx context.Context, ex execer, votes []FindingASRVote) (int64, error) {
	if len(votes) > MaxPilotPage {
		return 0, fmt.Errorf("upsert asr votes: %d votes, max %d per call", len(votes), MaxPilotPage)
	}
	if len(votes) == 0 {
		return 0, nil
	}
	n := len(votes)
	var (
		ids, models, shas, kinds, heard = make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		pilots                          = make([]int64, n)
		t0s, t1s                        = make([]*float64, n), make([]*float64, n)
		details                         = make([]string, n)
	)
	for i, v := range votes {
		ids[i], pilots[i], models[i], shas[i] = v.FindingID, v.PilotTranscriptID, v.ModelID, v.ChunkTextSHA256
		t0s[i], t1s[i], kinds[i], heard[i] = v.SpanStartSec, v.SpanEndSec, v.Vote, v.HeardText
		details[i] = "{}"
		if len(v.Detail) > 0 {
			details[i] = string(v.Detail)
		}
	}
	tag, err := ex.Exec(ctx, upsertFindingASRVotesSQL, ids, pilots, models, shas, t0s, t1s, kinds, heard, details)
	if err != nil {
		return 0, fmt.Errorf("upsert asr votes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PilotVoteCount is the number of findings whose current vote by one pilot
// model is Vote. "Current" = the most recently written vote of that finding
// by that model (a finding voted against two scopes counts once).
type PilotVoteCount struct {
	ModelID       string
	ModelRevision string
	Vote          string
	N             int64
}

var pilotVoteCountsSQL = `
	WITH cur AS (
	    SELECT DISTINCT ON (v.finding_id, p.model_id, p.model_revision)
	           v.finding_id, p.model_id, p.model_revision, v.vote
	      FROM finding_asr_votes v
	      JOIN pilot_transcripts p ON p.id = v.pilot_transcript_id
	     WHERE cardinality($1::text[]) = 0 OR regexp_replace(p.file_path, '/[^/]+$', '') = ANY($1::text[])
	     ORDER BY v.finding_id, p.model_id, p.model_revision, v.updated_at DESC, v.id DESC)
	SELECT model_id, model_revision, vote, count(*) FROM cur
	 GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`

// PilotVoteCounts tallies the current votes per model and vote (an aggregate:
// a few rows per model).
func (db *DB) PilotVoteCounts(ctx context.Context, bookDirs []string) ([]PilotVoteCount, error) {
	if bookDirs == nil {
		bookDirs = []string{}
	}
	rows, err := db.pool.Query(ctx, pilotVoteCountsSQL, bookDirs)
	if err != nil {
		return nil, fmt.Errorf("pilot vote counts: %w", err)
	}
	defer rows.Close()
	var out []PilotVoteCount
	for rows.Next() {
		var c PilotVoteCount
		if err := rows.Scan(&c.ModelID, &c.ModelRevision, &c.Vote, &c.N); err != nil {
			return nil, fmt.Errorf("pilot vote counts: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pilot vote counts: %w", err)
	}
	return out, nil
}

// PilotVotePair counts the findings that two pilot models (A < B by
// model_id, then revision) both voted on, by the pair of current votes.
type PilotVotePair struct {
	ModelA, RevisionA, VoteA string
	ModelB, RevisionB, VoteB string
	N                        int64
}

var pilotVotePairsSQL = `
	WITH cur AS (
	    SELECT DISTINCT ON (v.finding_id, p.model_id, p.model_revision)
	           v.finding_id, p.model_id, p.model_revision, v.vote
	      FROM finding_asr_votes v
	      JOIN pilot_transcripts p ON p.id = v.pilot_transcript_id
	     WHERE cardinality($1::text[]) = 0 OR regexp_replace(p.file_path, '/[^/]+$', '') = ANY($1::text[])
	     ORDER BY v.finding_id, p.model_id, p.model_revision, v.updated_at DESC, v.id DESC)
	SELECT a.model_id, a.model_revision, a.vote, b.model_id, b.model_revision, b.vote, count(*)
	  FROM cur a
	  JOIN cur b ON b.finding_id = a.finding_id
	            AND (a.model_id, a.model_revision) < (b.model_id, b.model_revision)
	 GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY 1, 2, 4, 5, 3, 6`

// PilotVotePairs is the two-model vote cross-tabulation (an aggregate).
func (db *DB) PilotVotePairs(ctx context.Context, bookDirs []string) ([]PilotVotePair, error) {
	if bookDirs == nil {
		bookDirs = []string{}
	}
	rows, err := db.pool.Query(ctx, pilotVotePairsSQL, bookDirs)
	if err != nil {
		return nil, fmt.Errorf("pilot vote pairs: %w", err)
	}
	defer rows.Close()
	var out []PilotVotePair
	for rows.Next() {
		var p PilotVotePair
		if err := rows.Scan(&p.ModelA, &p.RevisionA, &p.VoteA, &p.ModelB, &p.RevisionB, &p.VoteB, &p.N); err != nil {
			return nil, fmt.Errorf("pilot vote pairs: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pilot vote pairs: %w", err)
	}
	return out, nil
}
