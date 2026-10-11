package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// Judge passes (CONTRACT §2.15 "Lineage", migration 00013): one judge_passes
// row per (transcript × judge recipe) pass. A judging path opens the row as
// 'running' before its first model call — so a killed run leaves evidence —
// and closes it, with the pass's findings and the run_metrics latch, in ONE
// transaction (CommitJudgePass).

// Judge pass statuses written by the judge (legacy rows come only from the
// migration's backfill).
const (
	JudgePassRunning  = "running"
	JudgePassComplete = "complete"
	JudgePassPartial  = "partial"
	JudgePassFailed   = "failed"
)

// DropAlreadyRecorded is the judge_passes.dropped reason of a kept finding
// that was not inserted because the transcript already has the same finding
// (chunk, span, issue type, correction) from this recipe or one equivalent
// to it. With findings_kept it accounts for every finding the pass kept.
const DropAlreadyRecorded = "already_recorded"

// JudgePassOpen is a pass about to start.
type JudgePassOpen struct {
	TranscriptID string
	JobID        string
	FilePath     string
	// Recipe is the judge's current recipe; registered (insert-if-absent)
	// before the row references it.
	Recipe      recipe.Recipe
	StepRunID   int64 // 0 → NULL
	ChunksTotal int
}

// #nosec G101 -- SQL text, not a credential
const openJudgePassSQL = `
	INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, step_run_id,
	                          status, chunks_total)
	VALUES ($1, $2, $3, $4, NULLIF($5, 0), 'running', $6)
	RETURNING id`

// OpenJudgePass registers p.Recipe and inserts a 'running' pass, returning
// its id.
func (db *DB) OpenJudgePass(ctx context.Context, p JudgePassOpen) (int64, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin judge pass: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rid, err := registerRecipe(ctx, tx, p.Recipe)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, openJudgePassSQL, p.TranscriptID, p.JobID, p.FilePath, rid,
		p.StepRunID, p.ChunksTotal).Scan(&id); err != nil {
		return 0, fmt.Errorf("open judge pass: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit judge pass: %w", err)
	}
	return id, nil
}

// JudgePassClose is how a pass ended.
type JudgePassClose struct {
	Status         string // complete | partial | failed
	ChunksTotal    int
	ChunksJudged   int // judged, served from cache included
	ChunksFailed   int
	ChunksCached   int // served from fn_calls
	Dropped        map[string]int
	InputTokens    *int64
	OutputTokens   *int64
	CostUSD        *float64
	LatencyP50MS   *int
	LatencyP95MS   *int
	BookContextSHA string
	Error          string
}

// JudgePassCommit closes pass PassID of TranscriptID: its findings, its
// closing record, and (when Metrics is set) the run_metrics eval slice.
type JudgePassCommit struct {
	PassID       int64
	TranscriptID string
	Findings     []Finding
	Close        JudgePassClose
	Metrics      *EvalMetrics
}

// JudgePassResult is what a commit stored.
type JudgePassResult struct {
	Inserted   int // findings inserted (findings_kept)
	Duplicates int // kept findings already recorded by an equivalent recipe
}

// judgePassLockSQL serializes the writers of one transcript's findings for
// the commit's dedupe read → insert (a transaction-scoped advisory lock;
// concurrent rejudge shards are disjoint by transcript, so it never waits in
// the normal case).
// #nosec G101 -- SQL text, not a credential
const judgePassLockSQL = `SELECT pg_advisory_xact_lock(hashtext('earmark.judge_pass'), hashtext($1))`

// recipeFindingKeysSQL reads the dedupe keys of a transcript's live findings
// made by $2 or a recipe equivalent to it (equivalentRecipesCTE) — the
// recipe-scoped dedupe. A finding identical to one from a DIFFERENT recipe is
// not a key: two recipes agreeing on a fix is recorded as two rows, which
// share a correction_key.
const recipeFindingKeysSQL = `
	WITH` + equivalentRecipesCTE + `
	SELECT COALESCE(f.chunk_id::text, ''), f.original_text, f.issue_type,
	       COALESCE(f.suggested_correction, '')
	  FROM transcript_findings f
	 WHERE f.transcript_id = $2
	   AND f.patch_state <> 'superseded'
	   AND f.recipe_id IN (SELECT recipe_id FROM eq)`

// closeJudgePassSQL closes a running pass. The status guard makes a second
// close (or closing a pass someone else closed) an error, never a rewrite.
// #nosec G101 -- SQL text, not a credential
const closeJudgePassSQL = `
	UPDATE judge_passes
	   SET status = $2, chunks_total = $3, chunks_judged = $4, chunks_failed = $5,
	       chunks_cached = $6, findings_kept = $7, dropped = $8::jsonb,
	       input_tokens = $9, output_tokens = $10, cost_usd = $11,
	       latency_ms_p50 = $12, latency_ms_p95 = $13,
	       book_context_sha256 = NULLIF($14, ''), error = NULLIF($15, ''),
	       finished_at = now()
	 WHERE id = $1 AND status = 'running'`

// ErrJudgePassNotRunning: the pass to close is not 'running' (already closed,
// or no such pass).
var ErrJudgePassNotRunning = errors.New("judge pass is not running")

// CommitJudgePass stores a pass's outcome in one transaction: it registers
// the findings' recipes, inserts every finding not already recorded for the
// transcript by the SAME recipe (or one equivalent to it) — stamped with the
// pass and its fn_calls row —, closes the pass (findings_kept = rows
// inserted; same-recipe duplicates are counted under dropped
// "already_recorded"), and writes the run_metrics eval slice when c.Metrics
// is set (its Findings set to the rows inserted). Nothing is half-written: an
// error rolls all of it back and leaves the pass 'running'.
func (db *DB) CommitJudgePass(ctx context.Context, c JudgePassCommit) (JudgePassResult, error) {
	var res JudgePassResult
	if c.PassID == 0 {
		return res, errors.New("commit judge pass: no pass id")
	}
	if c.Close.Status != JudgePassComplete && c.Close.Status != JudgePassPartial && c.Close.Status != JudgePassFailed {
		return res, fmt.Errorf("commit judge pass: invalid status %q", c.Close.Status)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin judge pass commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Recipes first, then findings: the lock order of InsertFindings and of
	// migration 00013 (recipes before transcript_findings).
	recipeIDs, err := registerFindingRecipes(ctx, tx, c.Findings)
	if err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, judgePassLockSQL, c.TranscriptID); err != nil {
		return res, fmt.Errorf("lock transcript findings: %w", err)
	}
	keys := map[string]map[FindingKey]bool{}
	fresh := make([]Finding, 0, len(c.Findings))
	freshIDs := make([]*string, 0, len(c.Findings))
	for i, f := range c.Findings {
		rid := ""
		if recipeIDs[i] != nil {
			rid = *recipeIDs[i]
		}
		if rid != "" {
			if _, ok := keys[rid]; !ok {
				k, err := recipeFindingKeys(ctx, tx, rid, c.TranscriptID)
				if err != nil {
					return res, err
				}
				keys[rid] = k
			}
			k := KeyOf(f)
			if keys[rid][k] {
				res.Duplicates++
				continue
			}
			keys[rid][k] = true // a repeat within this pass is a duplicate too
		}
		pass := c.PassID
		f.JudgePassID = &pass
		fresh = append(fresh, f)
		freshIDs = append(freshIDs, recipeIDs[i])
	}
	if err := insertFindingRows(ctx, tx, fresh, freshIDs); err != nil {
		return res, err
	}
	res.Inserted = len(fresh)

	dropped := map[string]int{}
	for k, v := range c.Close.Dropped {
		if v > 0 {
			dropped[k] = v
		}
	}
	if res.Duplicates > 0 {
		dropped[DropAlreadyRecorded] += res.Duplicates
	}
	droppedJSON, err := json.Marshal(dropped)
	if err != nil {
		return res, fmt.Errorf("encode judge pass drops: %w", err)
	}
	cl := c.Close
	tag, err := tx.Exec(ctx, closeJudgePassSQL, c.PassID, cl.Status, cl.ChunksTotal, cl.ChunksJudged,
		cl.ChunksFailed, cl.ChunksCached, res.Inserted, string(droppedJSON), cl.InputTokens, cl.OutputTokens,
		cl.CostUSD, cl.LatencyP50MS, cl.LatencyP95MS, cl.BookContextSHA, truncateRunes(cl.Error, maxEvalErrorLen))
	if err != nil {
		return res, fmt.Errorf("close judge pass %d: %w", c.PassID, err)
	}
	if tag.RowsAffected() != 1 {
		return res, fmt.Errorf("close judge pass %d: %w", c.PassID, ErrJudgePassNotRunning)
	}
	if c.Metrics != nil {
		m := *c.Metrics
		m.Findings = res.Inserted
		if err := upsertEvalMetrics(ctx, tx, m); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit judge pass %d: %w", c.PassID, err)
	}
	return res, nil
}

func recipeFindingKeys(ctx context.Context, q rowQuerier, recipeID, transcriptID string) (map[FindingKey]bool, error) {
	rows, err := q.Query(ctx, recipeFindingKeysSQL, recipeID, transcriptID)
	if err != nil {
		return nil, fmt.Errorf("query recipe finding keys: %w", err)
	}
	defer rows.Close()
	keys := map[FindingKey]bool{}
	for rows.Next() {
		var k FindingKey
		if err := rows.Scan(&k.ChunkID, &k.OriginalText, &k.IssueType, &k.SuggestedCorrection); err != nil {
			return nil, fmt.Errorf("scan recipe finding key: %w", err)
		}
		keys[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipe finding keys: %w", err)
	}
	return keys, nil
}

// GetRecipeFindingKeys returns the dedupe keys of a transcript's live
// findings made by recipeID or a recipe equivalent to it. Read-only. A dry
// run uses it to report what a commit would skip.
func (db *DB) GetRecipeFindingKeys(ctx context.Context, transcriptID, recipeID string) (map[FindingKey]bool, error) {
	return recipeFindingKeys(ctx, db.pool, recipeID, transcriptID)
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// RecipesEquivalent reports whether recipes a and b are the same or
// equivalent (equivalentRecipesCTE: they differ at most in code_version).
func (db *DB) RecipesEquivalent(ctx context.Context, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	var ok bool
	if err := db.pool.QueryRow(ctx, `WITH`+equivalentRecipesCTE+`
		SELECT EXISTS (SELECT 1 FROM eq WHERE recipe_id = $2)`, a, b).Scan(&ok); err != nil {
		return false, fmt.Errorf("compare recipes: %w", err)
	}
	return ok, nil
}

// RejudgeTarget is one transcript the re-judge selects: just its identity —
// the chunks are read per transcript (GetEvalChunksForTranscript), so a page
// never holds text.
type RejudgeTarget struct {
	TranscriptID string
	JobID        string
	FilePath     string
	CreatedAt    time.Time
}

// Cursor is the keyset position just past t.
func (t RejudgeTarget) Cursor() TranscriptCursor {
	return TranscriptCursor{CreatedAt: t.CreatedAt, ID: t.TranscriptID}
}

// RejudgeQuery selects one page of re-judge targets.
type RejudgeQuery struct {
	// RecipeID is the judge's current recipe: a transcript with a 'complete'
	// pass under it, or under a recipe equivalent to it, is done.
	RecipeID string
	// Book, when set, keeps transcripts whose file_path contains it
	// (case-insensitive).
	Book string
	// Shard / Shards partition the selection: only transcripts with
	// hashtext(transcript_id) mod Shards = Shard. Shards 0 = unsharded. The
	// same partition as `earmark decide --shard`.
	Shard, Shards int
	After         TranscriptCursor
	Limit         int
}

// rejudgeTargetsSQL is the --rejudge selection: done transcripts that have
// stored chunks and no complete judge pass under the current recipe (or an
// equivalent one), in keyset order. $1 recipe, $2 book pattern (” = all),
// $3/$4 shard/shards, $5/$6 cursor, $7 limit. The shard folds hashtext (a
// signed int4) to be non-negative, as decide's does.
const rejudgeTargetsSQL = `
	WITH` + equivalentRecipesCTE + `
	SELECT t.id, t.job_id, t.file_path, t.created_at
	  FROM transcripts t
	  JOIN transcription_jobs j ON j.id = t.job_id
	 WHERE j.status = 'done'
	   AND EXISTS (SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id)
	   AND NOT EXISTS (
	         SELECT 1 FROM judge_passes p
	          WHERE p.transcript_id = t.id
	            AND p.status = 'complete'
	            AND p.recipe_id IN (SELECT recipe_id FROM eq))
	   AND ($2 = '' OR t.file_path ILIKE $2)
	   AND ($4 = 0 OR mod(mod(hashtext(t.id::text)::bigint, $4) + $4, $4) = $3)
	   AND ($5::timestamptz IS NULL OR (t.created_at, t.id) > ($5::timestamptz, $6::uuid))
	 ORDER BY t.created_at ASC, t.id ASC
	 LIMIT $7`

// RejudgeTargets returns one page of transcripts the current judge recipe has
// not completely judged (see rejudgeTargetsSQL). Read-only.
func (db *DB) RejudgeTargets(ctx context.Context, q RejudgeQuery) ([]RejudgeTarget, error) {
	if q.Shards < 0 || (q.Shards > 0 && (q.Shard < 0 || q.Shard >= q.Shards)) {
		return nil, fmt.Errorf("rejudge targets: shard %d/%d out of range", q.Shard, q.Shards)
	}
	book := ""
	if q.Book != "" {
		book = likePattern(q.Book)
	}
	c1, c2 := q.After.keysetArgs()
	rows, err := db.pool.Query(ctx, rejudgeTargetsSQL, q.RecipeID, book, q.Shard, q.Shards, c1, c2,
		normalizeSelectLimit(q.Limit))
	if err != nil {
		return nil, fmt.Errorf("query rejudge targets: %w", err)
	}
	defer rows.Close()
	var out []RejudgeTarget
	for rows.Next() {
		var t RejudgeTarget
		if err := rows.Scan(&t.TranscriptID, &t.JobID, &t.FilePath, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan rejudge target: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rejudge targets: %w", err)
	}
	return out, nil
}
