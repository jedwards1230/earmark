package db

import (
	"context"
	"errors"
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

	lockA, ok, err := a.TryBatchLock(ctx)
	if err != nil || !ok {
		t.Fatalf("first TryBatchLock = (%v, %v), want acquired", ok, err)
	}
	if err := lockA.Check(ctx); err != nil {
		t.Fatalf("Check while held: %v", err)
	}
	if _, ok, err := b.TryBatchLock(ctx); err != nil || ok {
		t.Fatalf("second TryBatchLock = (%v, %v), want not acquired while held", ok, err)
	}
	// Same pool, other connection: still excluded.
	if _, ok, err := a.TryBatchLock(ctx); err != nil || ok {
		t.Fatalf("same-pool TryBatchLock = (%v, %v), want not acquired while held", ok, err)
	}
	if err := lockA.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := lockA.Release(); err != nil {
		t.Fatalf("second release must be a no-op, got %v", err)
	}
	if err := lockA.Check(ctx); !errors.Is(err, ErrBatchLockLost) {
		t.Fatalf("Check after release = %v, want ErrBatchLockLost", err)
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

	lockB, ok, err := b.TryBatchLock(ctx)
	if err != nil || !ok {
		t.Fatalf("TryBatchLock after release = (%v, %v), want acquired", ok, err)
	}

	// The pinned session dies (as on a network drop): Check must report the
	// loss, and Postgres must have dropped the lock with the session.
	var pid int
	if err := lockB.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if _, err := a.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := lockB.Check(ctx); !errors.Is(err, ErrBatchLockLost) {
		t.Fatalf("Check after the session died = %v, want ErrBatchLockLost", err)
	}
	if err := lockB.Release(); err == nil {
		t.Fatal("Release on a dead session must report the unconfirmed unlock")
	}
	// Dead session, nobody else holding: no successor (restoring is safe).
	if other, err := lockB.Successor(ctx); err != nil || other {
		t.Fatalf("Successor with the lock free = (%v, %v), want (false, nil)", other, err)
	}
	lockC, ok, err := a.TryBatchLock(ctx)
	if err != nil || !ok {
		t.Fatalf("TryBatchLock after the holder died = (%v, %v), want acquired", ok, err)
	}
	// Now another session holds it: B sees a successor, C sees none.
	if other, err := lockB.Successor(ctx); err != nil || !other {
		t.Fatalf("Successor after C took the lock = (%v, %v), want (true, nil)", other, err)
	}
	if other, err := lockC.Successor(ctx); err != nil || other {
		t.Fatalf("holder's own Successor = (%v, %v), want (false, nil)", other, err)
	}
	if err := lockC.Release(); err != nil {
		t.Fatalf("release C: %v", err)
	}
}
