package db

import (
	"os"
	"strings"
	"testing"
)

// earmark runs two processes against one database (earmark-ingest and
// earmark-mcp) and both call initialize() on startup. Concurrent DDL from two
// sessions deadlocks — observed in production 2026-08-14 (CREATE FUNCTION,
// "while updating tuple in relation pg_proc") and 2026-08-19 (DROP TRIGGER).
//
// The advisory lock is what serializes them. These tests assert its properties
// at the source level, the same way findings_test.go guards the eval layer's
// read-only SQL, because the real failure needs two concurrent connections to a
// live Postgres and so cannot be reproduced in a unit test.

// TestSchemaInitLockIsTransactionScoped is the important one.
//
// pg_advisory_xact_lock is released automatically on COMMIT or ROLLBACK.
// pg_advisory_lock is NOT — it is session-scoped and survives the transaction.
// Since initialize() runs on a POOLED connection, a session-scoped lock would
// be released only when that connection closes; the pool would hand the still-
// locked connection to the next borrower and every subsequent initialize()
// would block forever.
//
// Swapping one for the other looks like a harmless rename and would deadlock
// the service permanently instead of transiently. Hence the test.
func TestSchemaInitLockIsTransactionScoped(t *testing.T) {
	if !strings.Contains(schemaInitLockSQL, "pg_advisory_xact_lock") {
		t.Errorf("schema-init lock must be transaction-scoped, got: %s", schemaInitLockSQL)
	}

	// Reject the session-scoped variants. Checked as a whole-token prefix so
	// "pg_advisory_xact_lock" does not trip the "pg_advisory_lock" check.
	for _, banned := range []string{
		"pg_advisory_lock(",
		"pg_advisory_lock_shared(",
		"pg_try_advisory_lock(",
	} {
		if strings.Contains(schemaInitLockSQL, banned) {
			t.Errorf("schema-init lock uses session-scoped %s — it runs on a pooled "+
				"connection and would leak the lock to the next borrower:\n%s",
				banned, schemaInitLockSQL)
		}
	}

	// A try-lock returns false instead of waiting, which would let the loser
	// skip the lock and race anyway — the exact bug this is meant to prevent.
	if strings.Contains(schemaInitLockSQL, "pg_try_advisory") {
		t.Errorf("schema-init lock must WAIT, not try-and-continue: %s", schemaInitLockSQL)
	}
}

// TestSchemaInitLockKeyIsStable guards the other way this silently stops
// working: the key is only meaningful if every process uses the same one. A
// per-process or randomized key would acquire a lock nobody contends for, and
// the deadlock would return with the lock still apparently "in place".
func TestSchemaInitLockKeyIsStable(t *testing.T) {
	if schemaInitLockKey == 0 {
		t.Error("schema-init lock key must be a fixed non-zero constant")
	}
	// Reading it twice must give the same value — trivially true for a const,
	// which is the point: this fails to compile if someone makes it a variable
	// computed at startup (e.g. from a hostname or PID).
	if schemaInitLockKey != 0x4541524D_5343484D {
		t.Errorf("schema-init lock key changed to %#x — every earmark process must "+
			"use the SAME key or initialization is not serialized at all",
			schemaInitLockKey)
	}
}

// TestSchemaInitAcquiresLockFirst asserts ordering: the lock must be taken
// before any DDL. A statement executed before it takes locks outside the
// serialized region, which reintroduces the deadlock while looking fixed.
//
// Verified against the source because the ordering lives in a function body
// rather than in an inspectable package var.
func TestSchemaInitAcquiresLockFirst(t *testing.T) {
	src, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatalf("read db.go: %v", err)
	}
	text := string(src)

	const marker = "func (db *DB) initialize(ctx context.Context) error {"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatal("could not find initialize() — this test needs updating")
	}
	body := text[start:]

	lockAt := strings.Index(body, "schemaInitLockSQL")
	if lockAt < 0 {
		t.Fatal("initialize() does not acquire the schema-init advisory lock")
	}

	// The first DDL in the body must come after the lock.
	for _, ddl := range []string{"CREATE EXTENSION", "CREATE TABLE", "ALTER TABLE", "DROP TRIGGER"} {
		if at := strings.Index(body, ddl); at >= 0 && at < lockAt {
			t.Errorf("%q is executed before the advisory lock is acquired — "+
				"it takes locks outside the serialized region and can still deadlock", ddl)
		}
	}
}
