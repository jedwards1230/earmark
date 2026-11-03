package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBatchLockIntegration: one holder at a time across processes (two pools),
// the lock frees on release, and a released connection goes back to the pool
// without the lock.
func TestBatchLockIntegration(t *testing.T) {
	dbURL := newTestDatabase(t)
	ctx := context.Background()
	open := func() *DB {
		pool, err := pgxpool.New(ctx, dbURL)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		t.Cleanup(pool.Close)
		return &DB{pool: pool, log: testLog()}
	}
	a, b := open(), open()

	releaseA, ok, err := a.TryBatchLock(ctx)
	if err != nil || !ok {
		t.Fatalf("first TryBatchLock = (%v, %v), want acquired", ok, err)
	}
	if _, ok, err := b.TryBatchLock(ctx); err != nil || ok {
		t.Fatalf("second TryBatchLock = (%v, %v), want not acquired while held", ok, err)
	}
	// Same pool, other connection: still excluded.
	if _, ok, err := a.TryBatchLock(ctx); err != nil || ok {
		t.Fatalf("same-pool TryBatchLock = (%v, %v), want not acquired while held", ok, err)
	}
	if err := releaseA(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := releaseA(); err != nil {
		t.Fatalf("second release must be a no-op, got %v", err)
	}

	var held int
	if err := a.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		   AND ((classid::bigint << 32) | objid::bigint) = $1`,
		batchLockKey).Scan(&held); err != nil {
		t.Fatalf("pg_locks: %v", err)
	}
	if held != 0 {
		t.Fatalf("lock still held after release (%d rows)", held)
	}

	releaseB, ok, err := b.TryBatchLock(ctx)
	if err != nil || !ok {
		t.Fatalf("TryBatchLock after release = (%v, %v), want acquired", ok, err)
	}
	if err := releaseB(); err != nil {
		t.Fatalf("release B: %v", err)
	}
}
