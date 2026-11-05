package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Per-book reads of the decide step's spelling pass (CONTRACT §2.19 "One
// spelling per book"). A book is a directory: the findings, pilot transcripts
// and chunks whose file_path lies under it (file_path LIKE <escaped dir>/%).
// Files in a sub-directory of dir belong to another book and are excluded.
// Every read is bounded: the findings by a LIMIT, the texts by streaming one
// row at a time to a callback that keeps no text, and both texts by a row cap.
// The callbacks run while the result set is open (holding a pool
// connection): they must not use the database.

// ErrSpellingCapExceeded is wrapped by a spelling loader whose book is over a
// read cap: the caller skips the book rather than building its model from a
// part of it.
var ErrSpellingCapExceeded = errors.New("spelling read cap exceeded")

// Read caps of the spelling loaders. Past a cap the read fails with
// ErrSpellingCapExceeded rather than building a model from part of a book.
const (
	// MaxSpellingFindings caps the live findings one book may have.
	MaxSpellingFindings = 50000
	// MaxSpellingPilotRows caps the pilot transcripts (tracks × models) read
	// for one book.
	MaxSpellingPilotRows = 5000
	// MaxSpellingChunkRows caps the production chunks read for one book.
	MaxSpellingChunkRows = 200000
)

// SpellingFinding is one live finding of a book, as the spelling model reads
// it.
type SpellingFinding struct {
	ID          string
	Original    string
	Replacement string
	// State is proposed, accepted or applied.
	State string
}

// spellingFindingsSQL: the book's proposed, accepted and applied findings, not
// archived (superseded_at) and not duplicates. $1 the book's LIKE pattern, $2 the
// row limit, $3 the pattern of its sub-directories (excluded).
var spellingFindingsSQL = `
	SELECT f.id::text, f.original_text, COALESCE(f.suggested_correction, ''), f.patch_state
	  FROM transcript_findings f
	 WHERE f.file_path LIKE $1 ESCAPE '\'
	   AND f.file_path NOT LIKE $3 ESCAPE '\'
	   AND f.patch_state IN ('proposed', 'accepted', 'applied')
	   AND f.superseded_at IS NULL
	   AND f.duplicate_of IS NULL
	 ORDER BY f.id
	 LIMIT $2`

// spellingPilotSQL: the newest full-track pilot transcript of each (track,
// model) under the book. Revisions of one model are not summed — only the
// newest row of a track counts. $1 the book's LIKE pattern, $2 the row limit, $3 the pattern of
// its sub-directories (excluded).
var spellingPilotSQL = `
	SELECT p.model_id, p.raw_text
	  FROM pilot_transcripts p
	 WHERE p.id IN (
	        SELECT DISTINCT ON (q.file_path, q.model_id) q.id
	          FROM pilot_transcripts q
	         WHERE q.file_path LIKE $1 ESCAPE '\'
	           AND q.file_path NOT LIKE $3 ESCAPE '\'
	           AND q.scope = 'full'
	         ORDER BY q.file_path, q.model_id, q.created_at DESC, q.id DESC)
	 ORDER BY p.id
	 LIMIT $2`

// spellingChunksSQL: the book's production chunk text (the pristine
// source_text, else text). $1 the book's LIKE pattern, $2 the row limit, $3 the pattern of
// its sub-directories (excluded).
var spellingChunksSQL = `
	SELECT COALESCE(c.source_text, c.text)
	  FROM transcript_chunks c
	 WHERE c.file_path LIKE $1 ESCAPE '\'
	   AND c.file_path NOT LIKE $3 ESCAPE '\'
	 LIMIT $2`

// bookPathPattern is the LIKE pattern of every file under dir; subDirPattern
// that of every file in a sub-directory of dir, which is another book
// (filepath.Dir of its file_path is not dir) and is excluded.
func bookPathPattern(dir string) string { return likePrefix(dir) + "/%" }

func subDirPattern(dir string) string { return likePrefix(dir) + "/%/%" }

// SpellingFindings returns the live findings of the book in dir (filepath.Dir
// of a file_path), by id. More than MaxSpellingFindings is an error.
func (db *DB) SpellingFindings(ctx context.Context, dir string) ([]SpellingFinding, error) {
	return spellingFindings(ctx, db.pool, dir, MaxSpellingFindings)
}

func spellingFindings(ctx context.Context, q rowQuerier, dir string, limit int) ([]SpellingFinding, error) {
	rows, err := q.Query(ctx, spellingFindingsSQL, bookPathPattern(dir), limit+1, subDirPattern(dir))
	if err != nil {
		return nil, fmt.Errorf("spelling findings of %s: %w", dir, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SpellingFinding, error) {
		var f SpellingFinding
		err := r.Scan(&f.ID, &f.Original, &f.Replacement, &f.State)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("spelling findings of %s: %w", dir, err)
	}
	if len(out) > limit {
		return nil, fmt.Errorf("spelling findings of %s: more than %d live findings: %w", dir, limit, ErrSpellingCapExceeded)
	}
	return out, nil
}

// StreamPilotText calls fn with each pilot transcript of the book in dir (the
// newest full-track row per track and model): its model id and raw text. The
// text is only valid during the call. More than MaxSpellingPilotRows rows is
// an error.
func (db *DB) StreamPilotText(ctx context.Context, dir string, fn func(modelID, text string) error) error {
	return streamPilotText(ctx, db.pool, dir, MaxSpellingPilotRows, fn)
}

func streamPilotText(ctx context.Context, q rowQuerier, dir string, limit int, fn func(modelID, text string) error) error {
	rows, err := q.Query(ctx, spellingPilotSQL, bookPathPattern(dir), limit+1, subDirPattern(dir))
	if err != nil {
		return fmt.Errorf("pilot text of %s: %w", dir, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if n++; n > limit {
			return fmt.Errorf("pilot text of %s: more than %d pilot transcripts: %w", dir, limit, ErrSpellingCapExceeded)
		}
		var model, text string
		if err := rows.Scan(&model, &text); err != nil {
			return fmt.Errorf("pilot text of %s: scan: %w", dir, err)
		}
		if err := fn(model, text); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pilot text of %s: %w", dir, err)
	}
	return nil
}

// StreamProductionText calls fn with the text of each production chunk of the
// book in dir. The text is only valid during the call. More than
// MaxSpellingChunkRows chunks is an error.
func (db *DB) StreamProductionText(ctx context.Context, dir string, fn func(text string) error) error {
	return streamProductionText(ctx, db.pool, dir, MaxSpellingChunkRows, fn)
}

func streamProductionText(ctx context.Context, q rowQuerier, dir string, limit int, fn func(text string) error) error {
	rows, err := q.Query(ctx, spellingChunksSQL, bookPathPattern(dir), limit+1, subDirPattern(dir))
	if err != nil {
		return fmt.Errorf("production text of %s: %w", dir, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if n++; n > limit {
			return fmt.Errorf("production text of %s: more than %d chunks: %w", dir, limit, ErrSpellingCapExceeded)
		}
		var text string
		if err := rows.Scan(&text); err != nil {
			return fmt.Errorf("production text of %s: scan: %w", dir, err)
		}
		if err := fn(text); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("production text of %s: %w", dir, err)
	}
	return nil
}
