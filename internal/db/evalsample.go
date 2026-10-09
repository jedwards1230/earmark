package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// sampleEvalChunksSeededSQL orders the library by md5(seed || ':' || id) —
// the same seeded order `earmark scan --sample --seed` uses (scanCandidatesSQL)
// — so the same seed over the same library picks the same chunks, and two
// judge runs (an old and a new prompt) can be compared on identical input.
// $1 seed, $2 limit.
var sampleEvalChunksSeededSQL = evalChunkSelectSQL + `
	ORDER BY md5($1 || ':' || c.id::text), c.id
	LIMIT $2
`

// SampleEvalChunksSeeded returns up to limit chunks across the whole library
// in an order fixed by seed (read-only). Used by `earmark eval --sample N
// --seed s` (CONTRACT §2.15 "Measuring a prompt change"). A blank seed is an
// error: the unseeded sample is SampleEvalChunks.
func (db *DB) SampleEvalChunksSeeded(ctx context.Context, limit int, seed string) ([]EvalChunk, error) {
	if strings.TrimSpace(seed) == "" {
		return nil, errors.New("eval sample seed is required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.pool.Query(ctx, sampleEvalChunksSeededSQL, seed, limit)
	if err != nil {
		return nil, fmt.Errorf("seeded sample eval chunks query: %w", err)
	}
	defer rows.Close()
	return scanEvalChunks(rows)
}
