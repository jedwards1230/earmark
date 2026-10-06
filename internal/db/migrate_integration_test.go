package db

// Integration tests for schema migrations against a REAL Postgres with
// pgvector (CONTRACT §1.8). Skipped unless EARMARK_TEST_DATABASE_URL points at
// a server the test may create and drop databases on, e.g.
//
//	docker run -d --name earmark-it -e POSTGRES_PASSWORD=pw -p 55432:5432 pgvector/pgvector:pg16
//	EARMARK_TEST_DATABASE_URL='postgres://postgres:pw@localhost:55432/postgres?sslmode=disable' \
//	  go test -race -run Integration ./internal/db/
//
// CI runs them in the "Integration (Postgres)" job against a pgvector/pgvector:pg16
// service container. Each test gets its own throwaway database.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedwards1230/earmark/internal/log"
)

const testDatabaseEnv = "EARMARK_TEST_DATABASE_URL"

// integrationServer returns the admin URL, skipping the test when unset.
func integrationServer(t *testing.T) string {
	t.Helper()
	u := os.Getenv(testDatabaseEnv)
	if u == "" {
		t.Skipf("%s not set; skipping Postgres integration test", testDatabaseEnv)
	}
	return u
}

// newTestDatabase creates an empty database and returns its URL. It is dropped
// (with any lingering sessions) when the test ends.
func newTestDatabase(t *testing.T) string {
	t.Helper()
	admin := integrationServer(t)
	ctx := context.Background()

	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "earmark_it_" + hex.EncodeToString(b[:])

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseEnv, err)
	}
	u.Path = "/" + name
	return u.String()
}

func connect(t *testing.T, dbURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// applyLegacyInitialize builds a database the way the pre-goose initialize()
// did: the frozen testdata/legacy_initialize.sql, in one transaction, under
// the old transaction-scoped lock.
func applyLegacyInitialize(t *testing.T, dbURL string) {
	t.Helper()
	src, err := os.ReadFile("testdata/legacy_initialize.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn := connect(t, dbURL)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaInitLockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(src)); err != nil {
		t.Fatalf("legacy initialize: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// schemaSnapshotSQL describes every user-visible schema object (columns with
// position, type, nullability and default; constraints; indexes; triggers;
// views; non-extension functions; extensions), one line each. goose's own
// bookkeeping table is excluded: it is the one thing a migrated database is
// supposed to have in addition.
const schemaSnapshotSQL = `
	SELECT format('column %s.%s #%s %s null=%s default=%s', c.table_name, c.column_name,
	              c.ordinal_position, c.udt_name, c.is_nullable, coalesce(c.column_default, '-'))
	  FROM information_schema.columns c
	 WHERE c.table_schema = current_schema() AND c.table_name <> 'goose_db_version'
	UNION ALL
	SELECT format('constraint %s on %s: %s', con.conname, con.conrelid::regclass,
	              pg_get_constraintdef(con.oid))
	  FROM pg_constraint con
	 WHERE con.connamespace = current_schema()::regnamespace
	   AND con.conrelid <> coalesce(to_regclass('goose_db_version'), 0)
	UNION ALL
	SELECT 'index ' || indexdef FROM pg_indexes
	 WHERE schemaname = current_schema() AND tablename <> 'goose_db_version'
	UNION ALL
	SELECT 'trigger ' || pg_get_triggerdef(t.oid) FROM pg_trigger t WHERE NOT t.tgisinternal
	UNION ALL
	SELECT format('view %s: %s', viewname, definition) FROM pg_views WHERE schemaname = current_schema()
	UNION ALL
	SELECT 'function ' || pg_get_functiondef(p.oid) FROM pg_proc p
	 WHERE p.pronamespace = current_schema()::regnamespace AND p.prokind = 'f'
	   AND p.proname NOT LIKE 'zz_test_%'
	   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
	UNION ALL
	SELECT format('extension %s %s', extname, extversion) FROM pg_extension
	UNION ALL
	SELECT 'runner_control seed ' || row_to_json(r)::text
	  FROM (SELECT id, paused, run_limit, phase FROM runner_control) r
`

func schemaSnapshot(t *testing.T, dbURL string) []string {
	t.Helper()
	ctx := context.Background()
	conn := connect(t, dbURL)
	rows, err := conn.Query(ctx, schemaSnapshotSQL)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	slices.Sort(lines)
	return lines
}

// diffSnapshots reports lines only in a and only in b.
func diffSnapshots(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, l := range b {
		inB[l] = true
	}
	inA := map[string]bool{}
	for _, l := range a {
		inA[l] = true
		if !inB[l] {
			onlyA = append(onlyA, l)
		}
	}
	for _, l := range b {
		if !inA[l] {
			onlyB = append(onlyB, l)
		}
	}
	return onlyA, onlyB
}

func requireSameSchema(t *testing.T, nameA string, a []string, nameB string, b []string) {
	t.Helper()
	onlyA, onlyB := diffSnapshots(a, b)
	for _, l := range onlyA {
		t.Errorf("only in %s: %s", nameA, l)
	}
	for _, l := range onlyB {
		t.Errorf("only in %s: %s", nameB, l)
	}
}

func gooseVersion(t *testing.T, dbURL string) int64 {
	t.Helper()
	var v int64
	if err := connect(t, dbURL).QueryRow(context.Background(),
		`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	return v
}

func latestVersion(t *testing.T) int64 {
	t.Helper()
	srcs := mustProvider(t).ListSources()
	return srcs[len(srcs)-1].Version
}

// itCtx bounds a migrate call: a leaked or never-released schema lock makes
// the next migrate wait forever, and that must fail the test, not hang it.
func itCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testLog() log.Logger { return log.NewLogger("db-it") }

// TestIntegrationBaselineMatchesLegacy is proofs (a)+(c): an empty database
// migrated to version 1 has exactly the schema the old inline initialize()
// built.
func TestIntegrationBaselineMatchesLegacy(t *testing.T) {
	fresh := newTestDatabase(t)
	if err := migrateTo(itCtx(t), fresh, testLog(), 1); err != nil {
		t.Fatalf("migrate empty database to v1: %v", err)
	}
	legacy := newTestDatabase(t)
	applyLegacyInitialize(t, legacy)

	requireSameSchema(t, "baseline (new)", schemaSnapshot(t, fresh), "legacy initialize (old)", schemaSnapshot(t, legacy))
	if v := gooseVersion(t, fresh); v != 1 {
		t.Errorf("goose version = %d, want 1", v)
	}
}

// installDDLRecorder makes the database record every DDL command it executes
// (an event trigger), so a test can prove a migration ran none.
func installDDLRecorder(t *testing.T, dbURL string) {
	t.Helper()
	_, err := connect(t, dbURL).Exec(context.Background(), `
		CREATE TABLE zz_test_ddl_log (tag TEXT, object_type TEXT, identity TEXT);
		CREATE FUNCTION zz_test_record_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $$
		DECLARE r record;
		BEGIN
			FOR r IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
				INSERT INTO zz_test_ddl_log VALUES (r.command_tag, r.object_type, r.object_identity);
			END LOOP;
		END $$;
		CREATE EVENT TRIGGER zz_test_ddl ON ddl_command_end EXECUTE FUNCTION zz_test_record_ddl();
	`)
	if err != nil {
		t.Fatalf("install DDL recorder (needs a superuser test role): %v", err)
	}
}

// recordedDDL returns the DDL recorded since installDDLRecorder, minus the
// recorder's own objects.
func recordedDDL(t *testing.T, dbURL string) []string {
	t.Helper()
	rows, err := connect(t, dbURL).Query(context.Background(),
		`SELECT tag || ' ' || object_type || ' ' || identity FROM zz_test_ddl_log`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func removeDDLRecorder(t *testing.T, dbURL string) {
	t.Helper()
	if _, err := connect(t, dbURL).Exec(context.Background(), `
		DROP EVENT TRIGGER zz_test_ddl;
		DROP FUNCTION zz_test_record_ddl();
		DROP TABLE zz_test_ddl_log;
	`); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationBaselineStampsLegacyWithoutDDL is proof (b): migrating a
// database the old code built records version 1 and executes no DDL of its
// own — the only DDL is goose creating its version table — and the schema is
// unchanged.
func TestIntegrationBaselineStampsLegacyWithoutDDL(t *testing.T) {
	dbURL := newTestDatabase(t)
	applyLegacyInitialize(t, dbURL)
	before := schemaSnapshot(t, dbURL)

	installDDLRecorder(t, dbURL)
	if err := migrateTo(itCtx(t), dbURL, testLog(), 1); err != nil {
		t.Fatalf("migrate legacy database to v1: %v", err)
	}
	for _, ddl := range recordedDDL(t, dbURL) {
		if !strings.Contains(ddl, "goose_db_version") {
			t.Errorf("baseline executed DDL on a pre-goose database: %s", ddl)
		}
	}
	removeDDLRecorder(t, dbURL)

	requireSameSchema(t, "before", before, "after", schemaSnapshot(t, dbURL))
	if v := gooseVersion(t, dbURL); v != 1 {
		t.Errorf("goose version = %d, want 1", v)
	}
}

// TestIntegrationBaselineCatchesUpOlderLegacy: a pre-goose database last booted
// by an older earmark (missing columns a later inline ALTER added) is finished
// by the idempotent baseline rather than stamped with the gap.
func TestIntegrationBaselineCatchesUpOlderLegacy(t *testing.T) {
	dbURL := newTestDatabase(t)
	applyLegacyInitialize(t, dbURL)
	if _, err := connect(t, dbURL).Exec(context.Background(), `
		ALTER TABLE run_metrics DROP COLUMN eval_error;
		ALTER TABLE transcript_findings DROP COLUMN resolved_model;
	`); err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(itCtx(t), dbURL, testLog(), 1); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var n int
	if err := connect(t, dbURL).QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		 WHERE (table_name, column_name) IN (('run_metrics','eval_error'),('transcript_findings','resolved_model'))
	`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("catch-up restored %d of 2 missing columns", n)
	}
}

// TestIntegrationLatestFromEmptyMatchesLatestFromLegacy: whichever way a
// database arrives at the latest version — built fresh, or adopted from the
// old code — it ends up with the same schema.
func TestIntegrationLatestFromEmptyMatchesLatestFromLegacy(t *testing.T) {
	fresh := newTestDatabase(t)
	if err := migrate(itCtx(t), fresh, testLog()); err != nil {
		t.Fatalf("migrate empty: %v", err)
	}
	legacy := newTestDatabase(t)
	applyLegacyInitialize(t, legacy)
	if err := migrate(itCtx(t), legacy, testLog()); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	requireSameSchema(t, "from empty", schemaSnapshot(t, fresh), "from legacy", schemaSnapshot(t, legacy))
	want := latestVersion(t)
	for _, u := range []string{fresh, legacy} {
		if v := gooseVersion(t, u); v != want {
			t.Errorf("goose version = %d, want %d", v, want)
		}
	}
	// A second run is a no-op.
	if err := migrate(itCtx(t), fresh, testLog()); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}

// TestIntegrationMigrateConcurrent is the production scenario: ingest and mcp
// (here, four of them) migrate the same empty database at the same moment.
// Without the lock this deadlocks or fails on duplicate DDL.
func TestIntegrationMigrateConcurrent(t *testing.T) {
	dbURL := newTestDatabase(t)
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	ctx := itCtx(t)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = migrate(ctx, dbURL, testLog())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrator %d: %v", i, err)
		}
	}
	if v, want := gooseVersion(t, dbURL), latestVersion(t); v != want {
		t.Errorf("goose version = %d, want %d", v, want)
	}
}

// TestIntegrationMigrateWaitsForLock: while another session holds the schema
// lock — here as the OLD code held it, pg_advisory_xact_lock in an open
// transaction — migrate must WAIT, must not create anything (not even goose's
// version table), and must finish once the lock is released.
func TestIntegrationMigrateWaitsForLock(t *testing.T) {
	dbURL := newTestDatabase(t)
	ctx := context.Background()
	holder := connect(t, dbURL)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaInitLockKey); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	mctx := itCtx(t)
	go func() { done <- migrate(mctx, dbURL, testLog()) }()

	select {
	case err := <-done:
		t.Fatalf("migrate returned (%v) while another session held the schema lock — it must wait", err)
	case <-time.After(1500 * time.Millisecond):
	}
	var created bool
	if err := connect(t, dbURL).QueryRow(ctx,
		`SELECT to_regclass('goose_db_version') IS NOT NULL OR to_regclass('transcription_jobs') IS NOT NULL`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("migrate created schema objects before it held the lock")
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migrate after the lock was released: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("migrate did not finish after the lock was released")
	}
}

// TestIntegrationMigrateReleasesLock guards the leak: when migrate returns, the
// session lock is gone and so is the dedicated connection that held it — so
// nothing pooled can ever inherit it.
func TestIntegrationMigrateReleasesLock(t *testing.T) {
	dbURL := newTestDatabase(t)
	ctx := context.Background()
	for range 2 { // the first run creates, the second finds nothing pending
		if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		requireNoMigrationSession(t, dbURL)
	}

	// And a cancelled context while WAITING for the lock leaves nothing behind.
	holder := connect(t, dbURL)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, schemaInitLockKey); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := migrate(cctx, dbURL, testLog()); err == nil {
		t.Fatal("migrate succeeded while the lock was held and its context expired")
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, schemaInitLockKey); err != nil {
		t.Fatal(err)
	}
	requireNoMigrationSession(t, dbURL)
}

func requireNoMigrationSession(t *testing.T, dbURL string) {
	t.Helper()
	conn := connect(t, dbURL)
	ctx := context.Background()
	// Backend exit is asynchronous to the client closing its socket.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var locks, sessions int
		if err := conn.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM pg_locks
			         WHERE locktype = 'advisory' AND granted
			           AND ((classid::bigint << 32) | objid::bigint) = $1),
			       (SELECT count(*) FROM pg_stat_activity
			         WHERE application_name = $2 AND datname = current_database())
		`, schemaInitLockKey, migrateAppName).Scan(&locks, &sessions); err != nil {
			t.Fatal(err)
		}
		if locks == 0 && sessions == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after migrate: %d schema lock(s) still held, %d %s session(s) still open",
				locks, sessions, migrateAppName)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestIntegrationResetRebuildsFreshSchema: DEBUG_DB_RESET's drop list covers
// every migrated object. If a migration adds a table Reset does not drop, the
// re-migration either fails or leaves a different schema.
func TestIntegrationResetRebuildsFreshSchema(t *testing.T) {
	ctx := context.Background()
	fresh := newTestDatabase(t)
	if err := migrate(itCtx(t), fresh, testLog()); err != nil {
		t.Fatal(err)
	}
	reset := newTestDatabase(t)
	applyLegacyInitialize(t, reset)
	if err := migrate(itCtx(t), reset, testLog()); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, reset)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE runner_control SET paused = true`); err != nil {
		t.Fatal(err)
	}
	if err := resetSchema(ctx, pool, reset, testLog()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	requireSameSchema(t, "fresh", schemaSnapshot(t, fresh), "after reset", schemaSnapshot(t, reset))
	leftovers, freshRels := relations(t, reset), relations(t, fresh)
	if !slices.Equal(leftovers, freshRels) {
		t.Errorf("relations after reset %v, fresh %v", leftovers, freshRels)
	}
}

func relations(t *testing.T, dbURL string) []string {
	t.Helper()
	rows, err := connect(t, dbURL).Query(context.Background(), `
		SELECT relname FROM pg_class
		 WHERE relnamespace = current_schema()::regnamespace AND relkind IN ('r','v')
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}
