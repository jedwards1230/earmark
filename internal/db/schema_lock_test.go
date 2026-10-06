package db

import (
	"os"
	"strings"
	"testing"
)

// earmark runs two processes against one database (earmark-ingest and
// earmark-mcp) and both migrate on startup. Concurrent DDL from two sessions
// deadlocks — observed in production 2026-08-14 (CREATE FUNCTION, "while
// updating tuple in relation pg_proc") and 2026-08-19 (DROP TRIGGER).
//
// The advisory lock is what serializes them. These tests assert its properties
// at the source level, the same way findings_test.go guards the eval layer's
// read-only SQL. The runtime behaviour — waiting, serializing, not leaking — is
// proven against a real Postgres by migrate_integration_test.go.

// TestSchemaLockWaits: a try-lock returns false instead of waiting, which would
// let the loser skip the lock and race the DDL anyway — the exact bug this is
// meant to prevent.
func TestSchemaLockWaits(t *testing.T) {
	if !strings.Contains(schemaLockSQL, "pg_advisory_lock($1)") {
		t.Errorf("schema lock must be a waiting pg_advisory_lock on the key, got: %s", schemaLockSQL)
	}
	for _, banned := range []string{"pg_try_advisory", "_shared("} {
		if strings.Contains(schemaLockSQL, banned) {
			t.Errorf("schema lock must be an exclusive WAITING lock, found %q: %s", banned, schemaLockSQL)
		}
	}
	if !strings.Contains(schemaUnlockSQL, "pg_advisory_unlock($1)") {
		t.Errorf("schema unlock must release the same session lock, got: %s", schemaUnlockSQL)
	}
}

// TestSchemaInitLockKeyIsStable guards the other way this silently stops
// working: the key is only meaningful if every process uses the same one. A
// per-process or randomized key would acquire a lock nobody contends for, and
// the deadlock would return with the lock still apparently "in place". The
// value is also the one the pre-goose initialize() used, so old and new pods
// rolling together still serialize.
func TestSchemaInitLockKeyIsStable(t *testing.T) {
	if schemaInitLockKey == 0 {
		t.Error("schema-init lock key must be a fixed non-zero constant")
	}
	if schemaInitLockKey != 0x4541524D_5343484D {
		t.Errorf("schema-init lock key changed to %#x — every earmark process (old and new) must "+
			"use the SAME key or migration is not serialized at all",
			schemaInitLockKey)
	}
}

// migrateBody returns the source of migrate().
func migrateBody(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("migrate.go")
	if err != nil {
		t.Fatalf("read migrate.go: %v", err)
	}
	text := string(src)
	const marker = "func migrateTo(ctx context.Context"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatal("could not find migrateTo() — this test needs updating")
	}
	body := text[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	return body
}

// TestSchemaLockIsOnADedicatedConnection guards the leak. The lock is
// session-scoped (it has to span goose's per-migration transactions), and a
// session lock left on a POOLED connection would be handed to the next borrower
// and block every later migration forever. So it must live on a handle that is
// not db.pool, keeps no idle connections, and is closed when migrate returns.
func TestSchemaLockIsOnADedicatedConnection(t *testing.T) {
	body := migrateBody(t)
	for _, want := range []string{
		"stdlib.OpenDB(",                       // its own database/sql handle
		"defer func() { _ = sqlDB.Close() }()", // physically closed on return
		"SetMaxIdleConns(0)",                   // a returned connection is closed, never parked
		"lockConn.Close()",                     // the lock connection is given back (and so closed)
		"schemaUnlockSQL",                      // and the lock is released explicitly first
		"context.WithoutCancel(",               // even when ctx was cancelled
	} {
		if !strings.Contains(body, want) {
			t.Errorf("migrate() no longer contains %q — the session lock could outlive it", want)
		}
	}
	if strings.Contains(body, ".pool") || strings.Contains(body, "pgxpool.") {
		t.Error("migrate() references a pool — the session lock must never run on the service's pgxpool")
	}
}

// TestSchemaLockPrecedesGoose asserts ordering: the lock must be taken before
// goose does anything, because goose's Up creates its version table (DDL)
// before any locker it is given would run. And goose must NOT be handed a
// session locker of its own: on its own connection with the same key it would
// wait for this lock forever.
func TestSchemaLockPrecedesGoose(t *testing.T) {
	body := migrateBody(t)
	lockAt := strings.Index(body, "schemaLockSQL")
	if lockAt < 0 {
		t.Fatal("migrate() does not acquire the schema advisory lock")
	}
	for _, call := range []string{"newMigrationProvider(", ".UpTo(ctx", "GetDBVersion("} {
		at := strings.Index(body, call)
		if at < 0 {
			t.Errorf("migrate() no longer calls %s — this test needs updating", call)
			continue
		}
		if at < lockAt {
			t.Errorf("%s runs before the schema lock is acquired — goose would create "+
				"goose_db_version outside the serialized region", call)
		}
	}

	src, err := os.ReadFile("migrate.go")
	if err != nil {
		t.Fatalf("read migrate.go: %v", err)
	}
	if strings.Contains(string(src), "goose.WithSessionLocker(") || strings.Contains(string(src), "goose.WithLocker(") {
		t.Error("goose must not get its own locker: it would self-deadlock against migrate()'s lock")
	}
}

// TestInitializeOnlyMigrates: the inline DDL is gone. initialize() is the lock +
// goose path and nothing else; schema changes are numbered migrations.
func TestInitializeOnlyMigrates(t *testing.T) {
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
	body = body[:strings.Index(body, "\n}\n")]
	if !strings.Contains(body, "migrate(") {
		t.Error("initialize() must run the goose migrations")
	}
	for _, ddl := range []string{"CREATE ", "ALTER ", "DROP ", "Exec("} {
		if strings.Contains(body, ddl) {
			t.Errorf("initialize() contains %q — schema changes belong in internal/db/migrations", ddl)
		}
	}
}
