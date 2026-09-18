package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/migrations"
)

// Migrator applies the embedded SQL migrations. It holds a PostgreSQL
// advisory lock while running, so concurrent deployments serialise. Each
// migration file is sent as one command and therefore runs in a single
// implicit transaction. See docs/DATABASE.md.
type Migrator struct {
	m  *migrate.Migrate
	db *sql.DB
}

// NewMigrator targets databaseURL with the repository's migrations.
func NewMigrator(databaseURL string) (*Migrator, error) {
	return NewMigratorFromFS(migrations.FS, databaseURL)
}

// NewMigratorFromFS targets databaseURL with migrations read from fsys,
// which must contain the files at its root.
func NewMigratorFromFS(fsys fs.FS, databaseURL string) (*Migrator, error) {
	source, err := iofs.New(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	connConfig, err := pgxConnConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*connConfig)
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migrator: %w", err)
	}
	return &Migrator{m: m, db: db}, nil
}

// Up applies every pending migration. It is a no-op when none is pending.
func (m *Migrator) Up() error {
	if err := m.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls back the most recent `steps` migrations.
func (m *Migrator) Down(steps int) error {
	if steps <= 0 {
		return errors.New("migrate down: steps must be positive")
	}
	if err := m.m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// DownAll rolls back every migration.
func (m *Migrator) DownAll() error {
	if err := m.m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Version reports the current version and whether the last run left the
// database dirty. A fresh database reports version 0.
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return version, dirty, nil
}

// Force sets the recorded version without running migrations and clears
// the dirty flag. Use it only after a failed migration has been inspected.
func (m *Migrator) Force(version int) error {
	if err := m.m.Force(version); err != nil {
		return fmt.Errorf("migrate force: %w", err)
	}
	return nil
}

// Close releases the database connection.
func (m *Migrator) Close() error {
	sourceErr, dbErr := m.m.Close()
	return errors.Join(sourceErr, dbErr, m.db.Close())
}

// MigrationLockTimeout bounds how long a migration waits for a lock it
// needs before giving up.
//
// This is the difference between a slow deployment and an outage. A
// migration that takes ACCESS EXCLUSIVE on a busy table -- adding a
// constraint, rewriting a column -- queues behind the queries already
// running on it. While it queues, every query that arrives afterwards
// queues behind the migration, because PostgreSQL's lock queue is ordered.
// A migration that would have finished in milliseconds therefore stalls
// the whole table for as long as the slowest query in front of it, and the
// application sees a table it cannot read.
//
// With a lock timeout the migration fails instead, having changed nothing.
// That is the cheap outcome; blocking the table is the expensive one.
//
// It is not self-healing, and the difference matters when reading a failed
// deployment: golang-migrate marks the version dirty on any failure, this
// one included, so the Job's remaining retries all refuse with "Dirty
// database version N" rather than succeeding once the lock frees. Clearing
// it is one command against a schema that was never changed --
// `api-gateway migrate force <previous>` -- and then the deployment runs
// again (docs/DATABASE.md, docs/PRODUCTION_READINESS.md). A deployment that
// stops and says so beats a table that stopped serving.
//
// It bounds only the wait for a lock, never the work: a legitimately long
// migration such as an index build still runs to completion once it holds
// what it needs, which is why statement_timeout is deliberately not set
// here.
const MigrationLockTimeout = "5s"

func pgxConnConfig(databaseURL string) (*pgxConfig, error) {
	cfg, err := parseConnConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.RuntimeParams["lock_timeout"] = MigrationLockTimeout
	// So that a migration holding or waiting on a lock is identifiable in
	// pg_stat_activity as a migration, rather than as the application.
	cfg.RuntimeParams["application_name"] = "vitalmesh-migrate"
	return cfg, nil
}

// SchemaVersion reads the applied migration version and whether the last
// run left it dirty. It is a plain query rather than a migrator, so a
// process that only wants to report the version does not open a second
// connection pool or take the advisory lock.
//
// A database with no schema_migrations table has had no migration applied;
// that is reported as not found rather than as version zero, because zero
// is a version a real database can be at.
func SchemaVersion(ctx context.Context, db DB) (version uint, dirty bool, found bool, err error) {
	var v int64
	row := db.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`)
	switch err := row.Scan(&v, &dirty); {
	case err == nil:
		if v < 0 {
			return 0, dirty, false, fmt.Errorf("schema version %d is negative", v)
		}
		return uint(v), dirty, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, false, nil
	default:
		var pgErr *pgconn.PgError
		// 42P01 undefined_table: nothing has been migrated here yet.
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return 0, false, false, nil
		}
		return 0, false, false, err
	}
}
