package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
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

func pgxConnConfig(databaseURL string) (*pgxConfig, error) {
	cfg, err := parseConnConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	return cfg, nil
}
