package db

// Postgres integration test for the 2026-11-01 pool deadlock: every search path
// must complete on a pool smaller than the number of searches in flight. Before
// the fix scanResults issued its per-book reads while its result set still held
// a connection, so each search needed two connections at once — on a
// one-connection pool a single search hung, and on any pool a burst of more
// searches than connections drained it for good. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// smallPoolDB opens a DB over dbURL whose pool holds at most maxConns
// connections.
func smallPoolDB(t *testing.T, dbURL string, maxConns int32) *DB {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &DB{pool: pool, log: testLog(), meta: metaprovider.NewPathProvider("", "/b")}
}

func TestIntegrationSearchesCompleteOnSmallPool(t *testing.T) {
	dbURL, seeded := migratedTestDB(t)
	seedRequeueRows(t, dbURL)
	var chunkID string
	if err := seeded.pool.QueryRow(itCtx(t), `
		SELECT id FROM transcript_chunks WHERE transcript_id = $1 AND chunk_index = 0
	`, rqTranscriptA).Scan(&chunkID); err != nil {
		t.Fatalf("find seeded chunk: %v", err)
	}

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.1
	}
	// Each search path that ends in scanResults, with the hit count the seed
	// rows give it.
	searches := []struct {
		name string
		want int
		run  func(ctx context.Context, d *DB) ([]SearchResultWithMetadata, error)
	}{
		{"findSimilar", 3, func(ctx context.Context, d *DB) ([]SearchResultWithMetadata, error) {
			return d.findSimilar(ctx, vec, 10, 0)
		}},
		{"searchInBook", 2, func(ctx context.Context, d *DB) ([]SearchResultWithMetadata, error) {
			return d.searchInBook(ctx, d.pool, vec, "/b/Author/A", 10, 0)
		}},
		{"TextSearch", 1, func(ctx context.Context, d *DB) ([]SearchResultWithMetadata, error) {
			return d.TextSearch(ctx, "the the cat", 10)
		}},
		{"TextSearchInBook", 1, func(ctx context.Context, d *DB) ([]SearchResultWithMetadata, error) {
			return d.TextSearchInBook(ctx, "/b/Author/A", "Ghanima", 10)
		}},
		{"GetChunkContext", 2, func(ctx context.Context, d *DB) ([]SearchResultWithMetadata, error) {
			return d.GetChunkContext(ctx, chunkID, 1)
		}},
	}

	// One connection: each search alone. The old code hung here outright.
	t.Run("one connection", func(t *testing.T) {
		d := smallPoolDB(t, dbURL, 1)
		for _, s := range searches {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			got, err := s.run(ctx, d)
			cancel()
			if err != nil {
				t.Errorf("%s on a 1-connection pool: %v", s.name, err)
				continue
			}
			if len(got) != s.want {
				t.Errorf("%s: %d results, want %d", s.name, len(got), s.want)
			}
			if len(got) > 0 && got[0].FileChecksum == "" {
				t.Errorf("%s: per-book enrichment missing (no FileChecksum)", s.name)
			}
		}
	})

	// A burst of 8× more searches than connections — the incident's shape.
	t.Run("burst", func(t *testing.T) {
		const conns, perPath = 2, 4
		d := smallPoolDB(t, dbURL, conns)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		errs := make(chan error, len(searches)*perPath)
		for _, s := range searches {
			for i := range perPath {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got, err := s.run(ctx, d)
					switch {
					case err != nil:
						errs <- fmt.Errorf("%s #%d: %w", s.name, i, err)
					case len(got) != s.want:
						errs <- fmt.Errorf("%s #%d: %d results, want %d", s.name, i, len(got), s.want)
					}
				}()
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}
