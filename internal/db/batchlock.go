package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// batchLockKey is the advisory-lock key that admits ONE `earmark batch`
// coordinator at a time (CONTRACT §1.4, "Single instance").
//
// Like schemaInitLockKey the value is arbitrary and only its stability
// matters: every coordinator must use the same key or two can run at once. It
// spells "EARM","BTCH" so a held lock is identifiable in pg_locks (classid /
// objid = 0x4541524D / 0x42544348). It must differ from schemaInitLockKey: the
// two share Postgres's single advisory-lock space, and a coordinator holding
// the schema key for hours would stall every pod's startup migration.
const batchLockKey int64 = 0x4541524D_42544348 // "EARM","BTCH"

// batchTryLockSQL / batchUnlockSQL take and release that lock. Package vars
// (like schemaLockSQL) so a test can pin their properties without a database.
//
// pg_TRY_advisory_lock on purpose: a second coordinator must not queue behind
// the first (a CronJob run would sit for hours and then start a stale batch);
// it learns at once that one is running and exits without writing anything.
//
// Session-scoped on purpose: the lock must span the whole run, across many
// transactions. A session lock is released only by an explicit unlock or the
// session ending, which is why TryBatchLock pins ONE pooled connection for the
// lock's lifetime and never hands a still-locked session back to the pool.
var (
	batchTryLockSQL = `SELECT pg_try_advisory_lock($1)`
	batchUnlockSQL  = `SELECT pg_advisory_unlock($1)`
)

// batchUnlockTimeout bounds the unlock round-trip. The release usually runs
// after the run's context is cancelled (SIGTERM), so it uses its own deadline.
var batchUnlockTimeout = 10 * time.Second

// TryBatchLock tries to take the single-coordinator lock without waiting.
//
// acquired=false (err nil) means another coordinator holds it. When acquired,
// the caller MUST call release exactly when the run is over — after its
// restore-idle cleanup — and release is safe to call more than once.
//
// The lock lives on a dedicated connection acquired from the pool and held
// until release, so the pool can never give the locked session to another
// query (which could then unlock it, or leak it to an unrelated caller). On
// release the lock is dropped with pg_advisory_unlock and the connection goes
// back to the pool; if the unlock fails or reports the lock was not held, the
// connection is closed instead, which ends the session and drops the lock with
// it. If the connection dies mid-run the server drops the lock too, so a
// crashed coordinator never wedges the next one.
func (db *DB) TryBatchLock(ctx context.Context) (release func() error, acquired bool, err error) {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("batch lock: acquire connection: %w", err)
	}
	if err := conn.QueryRow(ctx, batchTryLockSQL, batchLockKey).Scan(&acquired); err != nil {
		// The try-lock may or may not have run; close the session rather than
		// risk pooling one that holds the lock.
		_ = conn.Hijack().Close(context.Background())
		return nil, false, fmt.Errorf("batch lock: try lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	var (
		once   sync.Once
		relErr error
		unlock = func() {
			ctx, cancel := context.WithTimeout(context.Background(), batchUnlockTimeout)
			defer cancel()
			var unlocked bool
			uerr := conn.QueryRow(ctx, batchUnlockSQL, batchLockKey).Scan(&unlocked)
			if uerr == nil && unlocked {
				conn.Release()
				return
			}
			if uerr == nil {
				uerr = errors.New("pg_advisory_unlock reported the lock was not held")
			}
			// Never pool a session that may still hold the lock: closing it ends
			// the session, and Postgres drops its advisory locks with it.
			cerr := conn.Hijack().Close(ctx)
			relErr = errors.Join(fmt.Errorf("batch lock: unlock: %w", uerr), cerr)
		}
	)
	return func() error {
		once.Do(unlock)
		return relErr
	}, true, nil
}
