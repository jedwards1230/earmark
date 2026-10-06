package db

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// Re-anchoring the findings backlog (`earmark reanchor`, CONTRACT §2.17
// "Re-anchoring").
//
// A finding's anchor (chunk_id, chunk_text_sha256, anchor_offset,
// anchor_occurrence) names one revision of one chunk. Re-chunking regenerates
// every chunk: the same index can now hold different text, under the same
// deterministic id or a new one — the hash, not the id, proves an anchor
// current. Every finding recorded before it points at text the projection no
// longer has, and the first rebuild that replays it
// retires it as stale — terminally. This pass finds each finding's span in the
// transcript's CURRENT pristine chunks (patch.Reanchor) and records a fresh
// anchor there, or parks the finding as `unanchorable` (non-terminal) when the
// span is missing or ambiguous.
//
// It writes ONLY transcript_findings: anchor columns, the chunk it now names
// (chunk_id, chunk_index, read FROM the chunk row — the judged window
// start_sec/end_sec is kept), patch_state between proposed and unanchorable, and the
// reanchored_at/unanchorable_reason audit columns. It never touches chunk text,
// embedding_stale or the transcripts table, and it never moves a finding a
// human has decided: only proposed and unanchorable rows are in scope.

// reanchorStates are the states the pass reads and may rewrite. proposed is the
// review queue; unanchorable is the parking lot it may now be able to place.
// Every human-decided state (accepted, applied, rejected, reverted) and the
// terminal stale are deliberately out of scope.
var reanchorStates = []string{patch.StateProposed, patch.StateUnanchorable}

// DefaultReanchorBatch is how many transcripts one batch covers. A batch is
// one transaction and loads those transcripts' chunk text into the process, so
// it is the knob that bounds both lock time and memory (~10 chunks × ~2.3 KB
// per transcript on the live library).
const DefaultReanchorBatch = 25

// ReanchorScope selects what one run covers.
type ReanchorScope struct {
	// Book is a case-insensitive file_path substring ("" = every book).
	Book string
	// Limit caps the number of findings examined (0 = no cap).
	Limit int
	// BatchSize is transcripts per transaction (0 = DefaultReanchorBatch).
	BatchSize int
}

// ReanchorTally counts one model era's findings by outcome.
type ReanchorTally struct {
	Total     int
	Already   int // anchor already resolves; untouched
	Unique    int // one match in the named chunk; re-anchored in place
	Moved     int // one match in another chunk; re-anchored there
	Ambiguous int // several candidates; never guessed
	None      int // span not in the transcript's current text
	Pending   int // transcript has no chunks right now; not judged
}

func (t *ReanchorTally) add(outcome string) {
	t.Total++
	switch outcome {
	case patch.OutcomeAnchored:
		t.Already++
	case patch.OutcomeUnique:
		t.Unique++
	case patch.OutcomeMoved:
		t.Moved++
	case patch.OutcomeAmbiguous:
		t.Ambiguous++
	case patch.OutcomeNone:
		t.None++
	case patch.OutcomePending:
		t.Pending++
	}
}

// ReanchorReport is the outcome of one run. ByModel is the classification
// (identical in dry-run and apply); the write counters are only non-zero when
// the run applied.
type ReanchorReport struct {
	ByModel map[string]*ReanchorTally
	// Reanchored — rows whose anchor was rewritten (including unanchorable
	// rows returned to proposed).
	Reanchored int
	// MarkedUnanchorable — rows moved to unanchorable (or whose reason changed).
	MarkedUnanchorable int
	// Conflicts — rows whose guarded UPDATE matched nothing: the finding was
	// decided, or its chunk rebuilt, between the read and the write. Left for
	// the next run.
	Conflicts int
}

// Models returns the model eras in the report, sorted.
func (r ReanchorReport) Models() []string {
	out := make([]string, 0, len(r.ByModel))
	for m := range r.ByModel {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// Sum totals every era.
func (r ReanchorReport) Sum() ReanchorTally {
	var s ReanchorTally
	for _, t := range r.ByModel {
		s.Total += t.Total
		s.Already += t.Already
		s.Unique += t.Unique
		s.Moved += t.Moved
		s.Ambiguous += t.Ambiguous
		s.None += t.None
		s.Pending += t.Pending
	}
	return s
}

// reanchorTranscriptsSQL pages the transcripts that have in-scope findings, by
// keyset on transcript_id, so a run walks the library in bounded batches and
// never holds more than one batch's chunk text.
//
// A superseded finding (requeue's archive, migration 5) is never in scope:
// patch_state = ANY($1) is only proposed/unanchorable, and superseded is
// terminal. transcript_id IS NOT NULL says the same thing a second way — a
// NULL transcript_id is only ever a superseded row (the
// transcript_findings_null_transcript_superseded CHECK), and a NULL would
// otherwise page as a transcript id.
var reanchorTranscriptsSQL = `
	SELECT DISTINCT transcript_id
	FROM transcript_findings
	WHERE patch_state = ANY($1)
	  AND transcript_id IS NOT NULL
	  AND ($2::uuid IS NULL OR transcript_id > $2::uuid)
	  AND file_path ILIKE $3
	ORDER BY transcript_id
	LIMIT $4
`

// reanchorFindingsSQL reads one batch's in-scope findings (dry-run).
var reanchorFindingsSQL = `
	SELECT id, transcript_id, model, patch_state, original_text,
	       chunk_id, chunk_index, start_sec, end_sec,
	       chunk_text_sha256, anchor_offset, anchor_occurrence,
	       COALESCE(unanchorable_reason, '')
	FROM transcript_findings
	WHERE transcript_id = ANY($1)
	  AND patch_state = ANY($2)
	  AND file_path ILIKE $3
	ORDER BY transcript_id, id
	LIMIT $4
`

// reanchorFindingsLockSQL is the apply-mode read: the same rows, locked
// FOR UPDATE SKIP LOCKED. A row a reviewer (or a second reanchor run) is
// deciding right now is skipped, not waited on, and picked up next run; a row
// this run holds cannot be decided underneath it.
var reanchorFindingsLockSQL = reanchorFindingsSQL + `	FOR UPDATE SKIP LOCKED
`

// reanchorChunksSQL reads one batch's current chunks, PRISTINE text only —
// COALESCE(source_text, text) is the coordinate system every anchor and hash
// is recorded against (the corrected surface would give every anchor a hash no
// rebuild can ever match).
var reanchorChunksSQL = `
	SELECT id, transcript_id, chunk_index, start_sec, end_sec,
	       COALESCE(source_text, text)
	FROM transcript_chunks
	WHERE transcript_id = ANY($1)
	ORDER BY transcript_id, chunk_index
`

// reanchorChunksLockSQL is the apply-mode chunk read: FOR SHARE, so the
// worker's rebuild (an upsert over these rows) waits for this batch to commit
// instead of changing the text between the read and the anchor write. Taken
// AFTER the finding locks, in chunk order — the same order the rebuild upserts
// in — so the two cannot deadlock.
//
// This is the repo-wide lock order, recipes → findings → chunks (InsertChunks,
// SetPatchState): re-anchoring takes no recipes lock at all, its finding locks
// are SKIP LOCKED (it never waits on a finding a rebuild's lockTailFindingsSQL
// or a reviewer holds), and it only ever waits on chunk rows, which a rebuild
// takes after every finding lock it needs.
var reanchorChunksLockSQL = reanchorChunksSQL + `	FOR SHARE
`

// reanchorWriteSQL records a fresh anchor.
//
// start_sec/end_sec are deliberately NOT rewritten: they are the audio window
// of the chunk the judge saw, the evidence the next re-anchor (after the next
// re-chunk) searches by. The chunk's own timing is one join away via chunk_id.
//
// The chunk's identity comes FROM THE CHUNK ROW, and the statement
// re-checks the chunk's pristine-text hash in SQL: if the chunk was rebuilt
// after it was read, zero rows match and the finding is left for the next run
// rather than anchored to text nobody verified. `patch_state = $2` is the
// compare-and-swap on the state the row was read in.
var reanchorWriteSQL = `
	UPDATE transcript_findings f
	SET chunk_id            = c.id,
	    chunk_index         = c.chunk_index,
	    chunk_text_sha256   = $4,
	    anchor_offset       = $5,
	    anchor_occurrence   = $6,
	    patch_state         = $7,
	    unanchorable_reason = NULL,
	    reanchored_at       = now()
	FROM transcript_chunks c
	WHERE f.id = $1
	  AND f.patch_state = $2
	  AND c.id = $3
	  AND c.transcript_id = f.transcript_id
	  AND encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex') = $4
`

// markUnanchorableSQL parks a finding the pass could not place. The original
// anchor columns are left as they were — they are the only record of where the
// judge saw the span, and a later re-anchor starts from them.
var markUnanchorableSQL = `
	UPDATE transcript_findings
	SET patch_state         = $3,
	    unanchorable_reason = $4,
	    reanchored_at       = now()
	WHERE id = $1 AND patch_state = $2
`

// reanchorRow is one in-scope finding as read.
type reanchorRow struct {
	ID           string
	TranscriptID string
	Model        string
	State        string
	// Reason is unanchorable_reason ("" unless State is unanchorable).
	Reason  string
	Finding patch.ReanchorFinding
}

// Reanchor runs the re-anchor pass over scope. With apply=false it is a
// read-only dry-run that only classifies; with apply=true each batch is one
// transaction that locks its findings (FOR UPDATE SKIP LOCKED) and chunks
// (FOR SHARE) and writes the outcome. Idempotent: a re-anchored finding reads
// as "already" next run, and an unanchorable one is only rewritten if its
// reason changed or it can now be placed.
func (db *DB) Reanchor(ctx context.Context, scope ReanchorScope, apply bool) (ReanchorReport, error) {
	return reanchor(ctx, db.pool, scope, apply)
}

func reanchor(ctx context.Context, b txBeginner, scope ReanchorScope, apply bool) (ReanchorReport, error) {
	rep := ReanchorReport{ByModel: map[string]*ReanchorTally{}}
	batch := scope.BatchSize
	if batch <= 0 {
		batch = DefaultReanchorBatch
	}
	like := likePattern(scope.Book)
	var after *string
	seen := 0

	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		remaining := -1
		if scope.Limit > 0 {
			remaining = scope.Limit - seen
			if remaining <= 0 {
				return rep, nil
			}
		}
		last, n, done, err := reanchorBatch(ctx, b, &rep, after, like, batch, remaining, apply)
		if err != nil {
			return rep, err
		}
		seen += n
		if done {
			return rep, nil
		}
		after = &last
	}
}

// reanchorBatch processes one batch of transcripts in one transaction. It
// returns the last transcript id (the keyset cursor), how many findings it
// examined, and whether the walk is finished.
func reanchorBatch(ctx context.Context, b txBeginner, rep *ReanchorReport,
	after *string, like string, batch, remaining int, apply bool,
) (last string, examined int, done bool, err error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return "", 0, false, fmt.Errorf("begin reanchor batch: %w", err)
	}
	// Dry-run batches are never committed; an apply batch commits explicitly.
	defer func() { _ = tx.Rollback(ctx) }()

	ids, err := collectStrings(ctx, tx, reanchorTranscriptsSQL, reanchorStates, after, like, batch)
	if err != nil {
		return "", 0, false, fmt.Errorf("page reanchor transcripts: %w", err)
	}
	if len(ids) == 0 {
		return "", 0, true, nil
	}
	last = ids[len(ids)-1]

	limit := any(nil) // LIMIT NULL = no limit
	if remaining >= 0 {
		limit = remaining
	}
	findingsSQL, chunksSQL := reanchorFindingsSQL, reanchorChunksSQL
	if apply {
		findingsSQL, chunksSQL = reanchorFindingsLockSQL, reanchorChunksLockSQL
	}
	rows, err := readReanchorFindings(ctx, tx, findingsSQL, ids, like, limit)
	if err != nil {
		return "", 0, false, err
	}
	chunks, err := readReanchorChunks(ctx, tx, chunksSQL, ids)
	if err != nil {
		return "", 0, false, err
	}

	for _, r := range rows {
		res := patch.Reanchor(r.Finding, chunks[r.TranscriptID])
		t := rep.ByModel[r.Model]
		if t == nil {
			t = &ReanchorTally{}
			rep.ByModel[r.Model] = t
		}
		t.add(res.Outcome)
		if apply {
			if err := writeReanchor(ctx, tx, rep, r, res); err != nil {
				return "", 0, false, err
			}
		}
	}

	if apply {
		if err := tx.Commit(ctx); err != nil {
			return "", 0, false, fmt.Errorf("commit reanchor batch: %w", err)
		}
	}
	return last, len(rows), len(ids) < batch, nil
}

// writeReanchor persists one classification. Every state change is checked
// against patch.CanTransition before any SQL runs.
func writeReanchor(ctx context.Context, tx pgx.Tx, rep *ReanchorReport, r reanchorRow, res patch.ReanchorResult) error {
	switch res.Outcome {
	case patch.OutcomePending:
		return nil
	case patch.OutcomeAnchored:
		if r.State == patch.StateProposed {
			return nil
		}
		fallthrough // an unanchorable row whose old anchor resolves again
	case patch.OutcomeUnique, patch.OutcomeMoved:
		if r.State != patch.StateProposed && !patch.CanTransition(r.State, patch.StateProposed) {
			return fmt.Errorf("%w: %s -> %s (finding %s)", ErrIllegalTransition, r.State, patch.StateProposed, r.ID)
		}
		tag, err := tx.Exec(ctx, reanchorWriteSQL, r.ID, r.State, res.Chunk.ID,
			res.Hash, res.Offset, res.Occurrence, patch.StateProposed)
		if err != nil {
			return fmt.Errorf("reanchor finding %s: %w", r.ID, err)
		}
		if tag.RowsAffected() == 0 {
			rep.Conflicts++
			return nil
		}
		rep.Reanchored++
		return nil
	case patch.OutcomeAmbiguous, patch.OutcomeNone:
		reason := patch.UnanchorableNotFound
		if res.Outcome == patch.OutcomeAmbiguous {
			reason = patch.UnanchorableAmbiguous
		}
		if r.State == patch.StateUnanchorable && r.Reason == reason {
			return nil // already parked for this reason: idempotent no-op
		}
		if r.State != patch.StateUnanchorable && !patch.CanTransition(r.State, patch.StateUnanchorable) {
			return fmt.Errorf("%w: %s -> %s (finding %s)", ErrIllegalTransition, r.State, patch.StateUnanchorable, r.ID)
		}
		tag, err := tx.Exec(ctx, markUnanchorableSQL, r.ID, r.State, patch.StateUnanchorable, reason)
		if err != nil {
			return fmt.Errorf("mark finding %s unanchorable: %w", r.ID, err)
		}
		if tag.RowsAffected() == 0 {
			rep.Conflicts++
			return nil
		}
		rep.MarkedUnanchorable++
		return nil
	default:
		return fmt.Errorf("reanchor finding %s: unknown outcome %q", r.ID, res.Outcome)
	}
}

func collectStrings(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func readReanchorFindings(ctx context.Context, tx pgx.Tx, sql string, ids []string, like string, limit any) ([]reanchorRow, error) {
	rows, err := tx.Query(ctx, sql, ids, reanchorStates, like, limit)
	if err != nil {
		return nil, fmt.Errorf("read reanchor findings: %w", err)
	}
	defer rows.Close()
	var out []reanchorRow
	for rows.Next() {
		var (
			r                  reanchorRow
			chunkID, sha       *string
			offset, occurrence *int
		)
		if err := rows.Scan(&r.ID, &r.TranscriptID, &r.Model, &r.State, &r.Finding.OriginalText,
			&chunkID, &r.Finding.ChunkIndex, &r.Finding.StartSec, &r.Finding.EndSec,
			&sha, &offset, &occurrence, &r.Reason); err != nil {
			return nil, fmt.Errorf("scan reanchor finding: %w", err)
		}
		r.Finding.ChunkID = strOrEmpty(chunkID)
		r.Finding.ChunkHash = strOrEmpty(sha)
		r.Finding.Offset = intOrUnknown(offset)
		r.Finding.Occurrence = intOrUnknown(occurrence)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (reanchor findings): %w", err)
	}
	return out, nil
}

// readReanchorChunks returns the batch's chunks grouped by transcript id.
func readReanchorChunks(ctx context.Context, tx pgx.Tx, sql string, ids []string) (map[string][]patch.ReanchorChunk, error) {
	rows, err := tx.Query(ctx, sql, ids)
	if err != nil {
		return nil, fmt.Errorf("read reanchor chunks: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]patch.ReanchorChunk, len(ids))
	for rows.Next() {
		var (
			tid string
			c   patch.ReanchorChunk
		)
		if err := rows.Scan(&c.ID, &tid, &c.Index, &c.StartSec, &c.EndSec, &c.Text); err != nil {
			return nil, fmt.Errorf("scan reanchor chunk: %w", err)
		}
		out[tid] = append(out[tid], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error (reanchor chunks): %w", err)
	}
	return out, nil
}
