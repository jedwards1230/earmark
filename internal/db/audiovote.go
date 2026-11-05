package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/config"
)

// The audio-vote rung's reads (CONTRACT §2.19 "Audio-vote rung"): the held
// findings that have a current second-ASR vote, their current votes, and the
// latest decisions of the findings they conflict with. Every statement here is
// a SELECT; the rung writes through ApplyDecisions like the decide step.

// AudioVoteScope selects the held findings an audio-vote run considers.
type AudioVoteScope struct {
	// Rule is the vote rule counted (finding_asr_votes.detail->>'rule');
	// Models the (model_id, model_revision) pairs counted. A vote under
	// another rule or by another model or revision does not exist for the
	// rung.
	Rule   string
	Models []config.AudioVoteModel
	Book   string
	// IssueType keeps one issue type ("" = all).
	IssueType string
	// Shard / Shards partition the work by transcript, as DecideWorkScope.
	Shard, Shards int
	// After is the keyset cursor ("" = start); Limit the page size,
	// 1..MaxDecideWorkPage.
	After string
	Limit int
}

func (s AudioVoteScope) validate() error {
	if err := (config.AudioVoteConfig{Rule: s.Rule, Models: s.Models}).Validate(); err != nil {
		return fmt.Errorf("audio vote work: %w", err)
	}
	switch {
	case s.Shards < 0 || (s.Shards > 0 && (s.Shard < 0 || s.Shard >= s.Shards)):
		return fmt.Errorf("audio vote work: shard %d/%d out of range", s.Shard, s.Shards)
	case s.Limit < 1 || s.Limit > MaxDecideWorkPage:
		return fmt.Errorf("audio vote work: limit must be 1..%d", MaxDecideWorkPage)
	case s.After != "" && !uuidRe.MatchString(s.After):
		return fmt.Errorf("audio vote work: cursor %q is not a uuid", s.After)
	}
	return nil
}

func (s AudioVoteScope) modelArrays() (ids, revs []string) {
	for _, m := range s.Models {
		ids, revs = append(ids, m.ModelID), append(revs, m.Revision)
	}
	return ids, revs
}

// AudioVoteFinding is one held finding in the rung's scope, with the hold it
// is held by: its latest live (unrevoked) decision.
type AudioVoteFinding struct {
	DecideFinding
	// HoldEventID, HoldRecipeID, HoldReason, HoldP and HoldRelated are that
	// decision's event id, recipe_id, reason, p (nil = none recorded) and
	// related_finding_id ("" = none).
	HoldEventID  int64
	HoldRecipeID string
	HoldReason   string
	HoldP        *float64
	HoldRelated  string
}

// currentVoteWhere keeps a vote v (of pilot transcript p, on finding f) the
// rung counts: under rule $R, by one of the configured (model_id, revision)
// pairs, against the chunk revision the finding records. A vote on another
// revision of the chunk is stale: it does not exist for the rung.
func currentVoteWhere(rule, ids, revs string) string {
	return `v.detail->>'rule' = ` + rule + `
	   AND v.chunk_text_sha256 = f.chunk_text_sha256
	   AND (p.model_id, p.model_revision) IN (SELECT * FROM unnest(` + ids + `::text[], ` + revs + `::text[]))`
}

// audioVoteWorkSQL is the rung's work list, in id order after a keyset
// cursor: anchored judge findings that are held — proposed, and the latest
// live decision is a hold, by any recipe (heldJoin's rule) — with at least
// one vote the rung counts (currentVoteWhere). The shard is decideWorkSQL's.
//
//	$1 rule  $2 model ids  $3 revisions  $4 book pattern ('' = all)
//	$5 issue type ('' = all)  $6 shards (0 = all)  $7 shard
//	$8 cursor (NULL = start)  $9 limit
var audioVoteWorkSQL = `
	SELECT f.id::text, f.transcript_id::text, f.file_path, f.issue_type, f.original_text,
	       COALESCE(f.suggested_correction, ''), f.confidence, f.chunk_index,
	       f.anchor_offset, COALESCE(f.anchor_occurrence, -1), f.chunk_text_sha256,
	       f.patch_state, COALESCE(f.decided_by, ''),
	       ld.id, COALESCE(ld.recipe_id, ''), COALESCE(ld.reason, ''), ld.p, COALESCE(ld.related_finding_id::text, '')
	  FROM transcript_findings f
	  JOIN LATERAL (
	        SELECT e.id, e.recipe_id, e.outcome, e.reason, e.p, e.related_finding_id
	          FROM finding_events e
	         WHERE e.finding_id = f.id AND e.kind = 'decision'
	           AND NOT EXISTS (SELECT 1 FROM finding_events r
	                            WHERE r.kind = 'revoke' AND r.revokes_event_id = e.id)
	         ORDER BY e.id DESC
	         LIMIT 1) ld ON ld.outcome = 'hold'
	 WHERE f.origin = 'judge'
	   AND f.patch_state = 'proposed'
	   AND f.transcript_id IS NOT NULL
	   AND f.chunk_index IS NOT NULL
	   AND f.anchor_offset IS NOT NULL
	   AND f.chunk_text_sha256 IS NOT NULL
	   AND EXISTS (SELECT 1 FROM finding_asr_votes v
	                 JOIN pilot_transcripts p ON p.id = v.pilot_transcript_id
	                WHERE v.finding_id = f.id
	                  AND ` + currentVoteWhere("$1", "$2", "$3") + `)
	   AND ($4 = '' OR f.file_path ILIKE $4)
	   AND ($5 = '' OR f.issue_type = $5)
	   AND ($6 = 0 OR mod(mod(hashtext(f.transcript_id::text)::bigint, $6) + $6, $6) = $7)
	   AND ($8::uuid IS NULL OR f.id > $8::uuid)
	 ORDER BY f.id
	 LIMIT $9`

// AudioVoteWork returns one page of the rung's work list (read-only).
func (db *DB) AudioVoteWork(ctx context.Context, s AudioVoteScope) ([]AudioVoteFinding, error) {
	return audioVoteWork(ctx, db.pool, s)
}

func audioVoteWork(ctx context.Context, q rowQuerier, s AudioVoteScope) ([]AudioVoteFinding, error) {
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
	ids, revs := s.modelArrays()
	rows, err := q.Query(ctx, audioVoteWorkSQL, s.Rule, ids, revs, book, s.IssueType, s.Shards, s.Shard, after, s.Limit)
	if err != nil {
		return nil, fmt.Errorf("audio vote work: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AudioVoteFinding, error) {
		var f AudioVoteFinding
		err := r.Scan(&f.ID, &f.TranscriptID, &f.FilePath, &f.IssueType, &f.Original, &f.Replacement,
			&f.Confidence, &f.ChunkIndex, &f.AnchorOffset, &f.AnchorOccurrence, &f.ChunkTextSHA256,
			&f.PatchState, &f.DecidedBy, &f.HoldEventID, &f.HoldRecipeID, &f.HoldReason, &f.HoldP, &f.HoldRelated)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("audio vote work: %w", err)
	}
	return out, nil
}

// ASRVote is one current vote the rung counts: the vote row's id, the pilot
// model and revision that cast it (via its pilot transcript), and the vote.
// UpdatedAt is when the row was last written (votes are upserted in place).
type ASRVote struct {
	VoteID            int64     `json:"vote_id"`
	ModelID           string    `json:"model_id"`
	ModelRevision     string    `json:"model_revision"`
	PilotTranscriptID int64     `json:"pilot_transcript_id"`
	Vote              string    `json:"vote"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// currentASRVotesSQL is each finding's current vote per configured model and
// revision, among the votes the rung counts: the most recently written (then
// the highest id), as the pilot report's "current vote".
//
//	$1 finding ids  $2 rule  $3 model ids  $4 revisions
var currentASRVotesSQL = `
	SELECT DISTINCT ON (v.finding_id, p.model_id, p.model_revision)
	       v.finding_id::text, v.id, p.model_id, p.model_revision, v.pilot_transcript_id, v.vote, v.updated_at
	  FROM finding_asr_votes v
	  JOIN pilot_transcripts p ON p.id = v.pilot_transcript_id
	  JOIN transcript_findings f ON f.id = v.finding_id
	 WHERE v.finding_id = ANY($1::uuid[])
	   AND ` + currentVoteWhere("$2", "$3", "$4") + `
	 ORDER BY v.finding_id, p.model_id, p.model_revision, v.updated_at DESC, v.id DESC`

// CurrentASRVotes returns the current counted votes of at most
// MaxDecideWorkPage findings, by finding id; a finding with none is absent.
// Each finding's votes are in (model_id, model_revision) order.
func (db *DB) CurrentASRVotes(ctx context.Context, findingIDs []string, rule string, models []config.AudioVoteModel) (map[string][]ASRVote, error) {
	return currentASRVotes(ctx, db.pool, findingIDs, rule, models)
}

func currentASRVotes(ctx context.Context, q rowQuerier, findingIDs []string, rule string, models []config.AudioVoteModel) (map[string][]ASRVote, error) {
	if len(findingIDs) > MaxDecideWorkPage {
		return nil, fmt.Errorf("current asr votes: %d findings, max %d per call", len(findingIDs), MaxDecideWorkPage)
	}
	if strings.TrimSpace(rule) == "" || len(models) == 0 {
		return nil, errors.New("current asr votes: a rule and at least one model are required")
	}
	out := map[string][]ASRVote{}
	if len(findingIDs) == 0 {
		return out, nil
	}
	ids, revs := AudioVoteScope{Models: models}.modelArrays()
	rows, err := q.Query(ctx, currentASRVotesSQL, findingIDs, rule, ids, revs)
	if err != nil {
		return nil, fmt.Errorf("current asr votes: %w", err)
	}
	var fid string
	var v ASRVote
	if _, err := pgx.ForEachRow(rows, []any{&fid, &v.VoteID, &v.ModelID, &v.ModelRevision, &v.PilotTranscriptID, &v.Vote, &v.UpdatedAt},
		func() error {
			out[fid] = append(out[fid], v)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("current asr votes: %w", err)
	}
	return out, nil
}

// LatestDecision is a finding's latest live (unrevoked) decision event.
type LatestDecision struct {
	ID       int64
	RecipeID string
	Outcome  string
	Reason   string
	// Related is related_finding_id ("" = none).
	Related string
}

var latestDecisionsSQL = `
	SELECT f.id::text, ld.id, COALESCE(ld.recipe_id, ''), ld.outcome, COALESCE(ld.reason, ''), COALESCE(ld.related_finding_id::text, '')
	  FROM transcript_findings f
	  JOIN LATERAL (
	        SELECT e.id, e.recipe_id, e.outcome, e.reason, e.related_finding_id
	          FROM finding_events e
	         WHERE e.finding_id = f.id AND e.kind = 'decision'
	           AND NOT EXISTS (SELECT 1 FROM finding_events r
	                            WHERE r.kind = 'revoke' AND r.revokes_event_id = e.id)
	         ORDER BY e.id DESC
	         LIMIT 1) ld ON true
	 WHERE f.id = ANY($1::uuid[])`

// LatestDecisions returns the latest live decision of each of at most
// MaxDecideWorkPage findings; a finding never decided is absent.
func (db *DB) LatestDecisions(ctx context.Context, findingIDs []string) (map[string]LatestDecision, error) {
	return latestDecisions(ctx, db.pool, findingIDs)
}

func latestDecisions(ctx context.Context, q rowQuerier, findingIDs []string) (map[string]LatestDecision, error) {
	if len(findingIDs) > MaxDecideWorkPage {
		return nil, fmt.Errorf("latest decisions: %d findings, max %d per call", len(findingIDs), MaxDecideWorkPage)
	}
	out := map[string]LatestDecision{}
	if len(findingIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, latestDecisionsSQL, findingIDs)
	if err != nil {
		return nil, fmt.Errorf("latest decisions: %w", err)
	}
	var id string
	var d LatestDecision
	if _, err := pgx.ForEachRow(rows, []any{&id, &d.ID, &d.RecipeID, &d.Outcome, &d.Reason, &d.Related}, func() error {
		out[id] = d
		return nil
	}); err != nil {
		return nil, fmt.Errorf("latest decisions: %w", err)
	}
	return out, nil
}
