package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/jedwards1230/earmark/internal/log"
)

// Schema migrations (CONTRACT §1.8).
//
// The schema is owned by goose (github.com/pressly/goose/v3) running the
// numbered SQL files embedded from migrations/. Version 1 is the baseline — the
// DDL the old inline initialize() ran on every boot — and is special: it is a Go
// migration that EXECUTES the baseline only on a database that does not have it
// yet, and merely RECORDS version 1 on a database the old code already built.
// Everything after it is an ordinary goose SQL migration.

//go:embed migrations/*.sql
var migrationFiles embed.FS

// baselineFileName is migration version 1. It lives beside the numbered goose
// files so the sequence reads naturally, but goose's own scan excludes it: it is
// registered as a Go migration (see baselineMigration) because it needs a
// decision no SQL file can make.
const baselineFileName = "00001_baseline.sql"

// migrateAppName tags the dedicated migration connection in pg_stat_activity, so
// a lingering session (which would mean a leaked lock) is identifiable — and so
// the integration test can assert that none survives migrate().
const migrateAppName = "earmark-migrate"

// schemaInitLockKey is the advisory-lock key serializing schema migration.
//
// The value is arbitrary; only its stability matters. Every process that
// migrates must use the SAME key or the serialization does not happen. Chosen
// from "earmark schema init" so a stray lock is identifiable in pg_locks rather
// than looking like a random number. It is also the key the pre-goose
// initialize() took with pg_advisory_xact_lock; session and transaction advisory
// locks share one lock space, so an old pod and a new pod rolling together still
// serialize against each other.
const schemaInitLockKey int64 = 0x4541524D_5343484D // "EARM","SCHM"

// schemaLockSQL / schemaUnlockSQL take and release that lock. Package vars (like
// insertFindingSQL) so a test can assert their properties without a database.
//
// Session-scoped on purpose — and only safe because of WHERE it runs. Goose
// commits each migration in its own transaction, so a transaction-scoped
// pg_advisory_xact_lock would be released after the first one and could not
// span the run. A session lock can, but a session lock survives COMMIT and is
// released only by an explicit unlock or the connection closing — which is why
// migrate() takes it on a DEDICATED database/sql handle (never the service's
// pgxpool) that keeps no idle connections and is closed before migrate()
// returns. Even if the unlock fails, the session ends and the lock goes with it.
//
// pg_advisory_lock WAITS. A try-lock would let the loser skip the lock and race
// the DDL anyway — the exact deadlock this exists to prevent.
var (
	schemaLockSQL   = `SELECT pg_advisory_lock($1)`
	schemaUnlockSQL = `SELECT pg_advisory_unlock($1)`
)

// migrate brings the database at databaseURL to the latest schema version.
//
// # Why the advisory lock
//
// earmark runs two processes against one database (earmark-ingest and
// earmark-mcp), and both migrate on startup. In Kubernetes they are rolled
// together, so they routinely run at the same moment — and concurrent DDL
// deadlocks:
//
//	Process A waits for AccessExclusiveLock on relation X
//	Process B waits for ShareLock on A's transaction
//
// Observed in production on 2026-08-14 (earmark-mcp, during CREATE FUNCTION,
// "while updating tuple in relation pg_proc") and again on 2026-08-19
// (earmark-ingest, during DROP TRIGGER), when the schema was still applied
// inline. The lock makes the second process WAIT until the first has finished;
// it then finds nothing pending and returns.
//
// # Why not goose's own session locker
//
// goose.WithSessionLocker does NOT cover the whole run: Provider.Up first calls
// HasPending, which creates goose_db_version WITHOUT the locker (goose relies on
// retrying that CREATE when two instances race it). That is unserialized DDL —
// the class of bug the lock exists to remove. So the lock is held here, on its
// own connection, around EVERYTHING goose does, and goose is given no locker
// (one on another connection with the same key would wait on this one forever).
// TestMigrateCreatesNothingBeforeLock (integration) pins this.
func migrate(ctx context.Context, databaseURL string, logger log.Logger) error {
	return migrateTo(ctx, databaseURL, logger, math.MaxInt64)
}

// migrateTo is migrate stopping at version target (tests compare the schema at
// the baseline). Everything about the lock described on migrate applies here.
func migrateTo(ctx context.Context, databaseURL string, logger log.Logger, target int64) (retErr error) {
	connCfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse database URL for migrations: %w", err)
	}
	connCfg.RuntimeParams["application_name"] = migrateAppName

	// A dedicated handle, NOT the service pool: the session lock must never be parked on
	// a connection something else will borrow. MaxIdleConns(0) closes every
	// connection the moment it is returned, and Close (deferred) closes the
	// rest — so the lock cannot outlive this function.
	sqlDB := stdlib.OpenDB(*connCfg)
	defer func() { _ = sqlDB.Close() }()
	sqlDB.SetMaxIdleConns(0)
	// One connection holds the lock; goose runs on the other.
	sqlDB.SetMaxOpenConns(2)

	lockConn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open schema-lock connection: %w", err)
	}
	defer func() { _ = lockConn.Close() }()

	// FIRST, before goose touches the database at all.
	if _, err := lockConn.ExecContext(ctx, schemaLockSQL, schemaInitLockKey); err != nil {
		return fmt.Errorf("acquire schema advisory lock: %w", err)
	}
	defer func() {
		// Detached: a cancelled ctx must not skip the unlock. (Closing the
		// connection would release it anyway; this just does it promptly and
		// surfaces a lock that was somehow not held.)
		var released bool
		err := lockConn.QueryRowContext(context.WithoutCancel(ctx), schemaUnlockSQL, schemaInitLockKey).Scan(&released)
		switch {
		case err != nil:
			retErr = errors.Join(retErr, fmt.Errorf("release schema advisory lock: %w", err))
		case !released:
			retErr = errors.Join(retErr, errors.New("release schema advisory lock: lock was not held"))
		}
	}()

	provider, err := newMigrationProvider(sqlDB, logger)
	if err != nil {
		return err
	}

	results, err := provider.UpTo(ctx, target)
	if err != nil {
		return fmt.Errorf("apply schema migrations: %w", err)
	}
	for _, r := range results {
		logger.Info("schema migration applied",
			"version", r.Source.Version, "source", r.Source.Path,
			"duration", r.Duration.String())
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	logger.Info("schema up to date", "version", version, "applied", len(results))
	return nil
}

// newMigrationProvider builds the goose provider over the embedded migrations.
func newMigrationProvider(sqlDB *sql.DB, logger log.Logger) (*goose.Provider, error) {
	fsys, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys,
		goose.WithExcludeNames([]string{baselineFileName}),
		goose.WithGoMigrations(baselineMigration(logger)),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, fmt.Errorf("build migration provider: %w", err)
	}
	return provider, nil
}

// baselineMigration is goose version 1.
//
// It runs inside goose's transaction, under the schema lock, so the "is this a
// pre-goose database?" question and the action taken on its answer cannot race
// another process.
func baselineMigration(logger log.Logger) *goose.Migration {
	up := &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
		present, missing, err := inspectLegacySchema(ctx, tx)
		if err != nil {
			return err
		}
		switch decideBaseline(present, missing) {
		case baselineStamp:
			logger.Info("pre-goose schema detected; recording baseline version 1 without executing it")
			return nil
		case baselineCatchUp:
			logger.Warn("pre-goose schema is missing baseline objects; running the idempotent baseline to finish it",
				"missing", strings.Join(missing, ","))
		case baselineCreate:
			logger.Info("empty database; creating the baseline schema")
		}
		if _, err := tx.ExecContext(ctx, baselineSQL()); err != nil {
			return fmt.Errorf("execute baseline schema: %w", err)
		}
		return nil
	}}
	// The baseline cannot be undone: there is no "before" to go back to.
	down := &goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error {
		return errors.New("the baseline migration cannot be reverted")
	}}
	return goose.NewGoMigration(1, up, down)
}

// baselineAction is what version 1 does to a given database.
type baselineAction int

const (
	// baselineCreate: an empty database — execute the baseline.
	baselineCreate baselineAction = iota
	// baselineStamp: the old inline initialize() already built every baseline
	// object — record version 1 and execute NOTHING.
	baselineStamp
	// baselineCatchUp: a pre-goose database last booted by an OLDER earmark,
	// missing objects a newer inline initialize() would have added. The
	// baseline is idempotent (IF NOT EXISTS / guarded DO blocks), so executing
	// it finishes the job exactly as booting that newer version would have.
	baselineCatchUp
)

func (a baselineAction) String() string {
	switch a {
	case baselineCreate:
		return "create"
	case baselineStamp:
		return "stamp"
	case baselineCatchUp:
		return "catch-up"
	}
	return fmt.Sprintf("baselineAction(%d)", int(a))
}

// decideBaseline is the pure decision behind version 1. legacyPresent reports
// whether transcription_jobs exists (the first table the old code created, in
// the same transaction as every other one); missing lists baseline objects the
// database lacks.
func decideBaseline(legacyPresent bool, missing []string) baselineAction {
	switch {
	case !legacyPresent:
		return baselineCreate
	case len(missing) > 0:
		return baselineCatchUp
	default:
		return baselineStamp
	}
}

// baselineSQL returns the embedded baseline DDL.
func baselineSQL() string {
	b, err := migrationFiles.ReadFile("migrations/" + baselineFileName)
	if err != nil {
		// The file is embedded at compile time; this is unreachable unless the
		// embed pattern stops matching it, which TestBaselineInventory catches.
		panic(fmt.Sprintf("embedded baseline missing: %v", err))
	}
	return string(b)
}

// baselineObject is one object the baseline guarantees, in the form
// inspectLegacySchema reports it: "table:x", "column:t.c", "index:x",
// "constraint:x", "function:x", "trigger:x".
type baselineObject = string

var (
	reTable      = regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS (\w+)`)
	reIndex      = regexp.MustCompile(`(?i)CREATE INDEX IF NOT EXISTS (\w+)`)
	reConstraint = regexp.MustCompile(`conname = '(\w+)'`)
	reFunction   = regexp.MustCompile(`(?i)CREATE OR REPLACE FUNCTION (\w+)`)
	reTrigger    = regexp.MustCompile(`(?i)CREATE TRIGGER (\w+)`)
	// An ALTER TABLE statement up to its terminating semicolon; the ADD COLUMN
	// clauses inside it are matched separately.
	reAlter     = regexp.MustCompile(`(?is)ALTER TABLE (\w+)\s+(ADD COLUMN IF NOT EXISTS .*?);`)
	reAddColumn = regexp.MustCompile(`(?i)ADD COLUMN IF NOT EXISTS (\w+)`)
)

// baselineInventory lists every named object the baseline creates, parsed from
// the baseline file itself so it cannot drift from it. Columns are only the
// ADD COLUMN ones: those are what an older pre-goose database can lack, since
// the old code added columns to existing tables but never dropped any.
func baselineInventory() []baselineObject {
	src := baselineSQL()
	var out []baselineObject
	add := func(kind string, re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			out = append(out, kind+":"+m[1])
		}
	}
	add("table", reTable)
	add("index", reIndex)
	add("constraint", reConstraint)
	add("function", reFunction)
	add("trigger", reTrigger)
	for _, m := range reAlter.FindAllStringSubmatch(src, -1) {
		for _, c := range reAddColumn.FindAllStringSubmatch(m[2], -1) {
			out = append(out, "column:"+m[1]+"."+c[1])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// legacyInventorySQL lists the baseline-relevant objects that exist in the
// current schema, in baselineInventory's notation.
const legacyInventorySQL = `
	SELECT 'table:' || c.relname FROM pg_class c
	 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind IN ('r','p')
	UNION ALL
	SELECT 'index:' || c.relname FROM pg_class c
	 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind = 'i'
	UNION ALL
	SELECT 'constraint:' || con.conname FROM pg_constraint con
	 WHERE con.connamespace = current_schema()::regnamespace
	UNION ALL
	SELECT 'function:' || p.proname FROM pg_proc p
	 WHERE p.pronamespace = current_schema()::regnamespace
	UNION ALL
	SELECT 'trigger:' || t.tgname FROM pg_trigger t
	  JOIN pg_class c ON c.oid = t.tgrelid
	 WHERE c.relnamespace = current_schema()::regnamespace AND NOT t.tgisinternal
	UNION ALL
	SELECT 'column:' || c.relname || '.' || a.attname FROM pg_attribute a
	  JOIN pg_class c ON c.oid = a.attrelid
	 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind = 'r'
	   AND a.attnum > 0 AND NOT a.attisdropped
`

// inspectLegacySchema reports whether a pre-goose schema is present and which
// baseline objects it lacks.
func inspectLegacySchema(ctx context.Context, tx *sql.Tx) (present bool, missing []string, err error) {
	if err := tx.QueryRowContext(ctx,
		`SELECT to_regclass('transcription_jobs') IS NOT NULL`).Scan(&present); err != nil {
		return false, nil, fmt.Errorf("detect pre-goose schema: %w", err)
	}
	if !present {
		return false, nil, nil
	}
	rows, err := tx.QueryContext(ctx, legacyInventorySQL)
	if err != nil {
		return true, nil, fmt.Errorf("inventory pre-goose schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return true, nil, fmt.Errorf("scan schema inventory: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return true, nil, fmt.Errorf("schema inventory rows: %w", err)
	}
	return true, missingObjects(baselineInventory(), have), nil
}

// missingObjects returns the entries of want absent from have, in want's order.
func missingObjects(want []baselineObject, have map[string]bool) []string {
	var missing []string
	for _, o := range want {
		if !have[o] {
			missing = append(missing, o)
		}
	}
	return missing
}
