//go:build integration

package postgres_test

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/migrations"
)

var requiredTables = []string{
	"users", "patients", "measurement_types", "measurements",
	"processing_jobs", "processing_results", "audit_logs", "idempotency_keys",
}

func tableNames(t *testing.T, dbURL, schema string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = $1`, schema)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect tables: %v", err)
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// latestMigration is the highest version the embedded migrations define.
// Reading it rather than pinning a number means adding a migration does not
// need this test edited, while Up reaching anything else still fails.
func latestMigration(t *testing.T) uint {
	t.Helper()
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil || len(entries) == 0 {
		t.Fatalf("reading migrations: %v (%d found)", err, len(entries))
	}
	var highest uint
	for _, name := range entries {
		number, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("migration %q is not named NNNNNN_description.up.sql", name)
		}
		value, err := strconv.ParseUint(number, 10, 32)
		if err != nil {
			t.Fatalf("migration %q has a non-numeric version: %v", name, err)
		}
		if uint(value) > highest {
			highest = uint(value)
		}
	}
	return highest
}

func TestMigrateUpFromEmptyThenDownToEmpty(t *testing.T) {
	t.Parallel()
	dbURL, schema := postgrestest.NewSchema(t)

	m, err := postgres.NewMigrator(dbURL)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	defer m.Close()

	version, dirty, err := m.Version()
	if err != nil || version != 0 || dirty {
		t.Fatalf("fresh database: version=%d dirty=%v err=%v", version, dirty, err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("Up: %v", err)
	}
	version, dirty, err = m.Version()
	if want := latestMigration(t); err != nil || version != want || dirty {
		t.Fatalf("after Up: version=%d dirty=%v err=%v, want version %d", version, dirty, err, want)
	}
	tables := tableNames(t, dbURL, schema)
	for _, want := range requiredTables {
		if !tables[want] {
			t.Errorf("table %s missing after Up", want)
		}
	}

	if err := m.Up(); err != nil {
		t.Fatalf("second Up must be a no-op, got %v", err)
	}

	// One step back lands on the previous version, whichever migration is
	// currently last. DownAll below is what checks that every migration
	// undoes itself.
	if err := m.Down(1); err != nil {
		t.Fatalf("Down(1): %v", err)
	}
	version, _, _ = m.Version()
	if want := latestMigration(t) - 1; version != want {
		t.Fatalf("after Down(1): version=%d, want %d", version, want)
	}

	if err := m.DownAll(); err != nil {
		t.Fatalf("DownAll: %v", err)
	}
	version, dirty, err = m.Version()
	if err != nil || version != 0 || dirty {
		t.Fatalf("after DownAll: version=%d dirty=%v err=%v", version, dirty, err)
	}
	tables = tableNames(t, dbURL, schema)
	for _, gone := range requiredTables {
		if tables[gone] {
			t.Errorf("table %s survived DownAll", gone)
		}
	}
	if !tables["schema_migrations"] {
		t.Error("schema_migrations bookkeeping table should remain")
	}

	if err := m.Up(); err != nil {
		t.Fatalf("Up after DownAll: %v", err)
	}
}

// A migration whose second statement fails must leave nothing of that
// migration behind: PostgreSQL runs each file in one implicit transaction.
func TestFailingMigrationIsRolledBackAtomically(t *testing.T) {
	t.Parallel()
	dbURL, schema := postgrestest.NewSchema(t)

	fsys := fstest.MapFS{
		"000001_good.up.sql":   {Data: []byte(`CREATE TABLE good (id int PRIMARY KEY);`)},
		"000001_good.down.sql": {Data: []byte(`DROP TABLE good;`)},
		"000002_bad.up.sql": {Data: []byte(`
			CREATE TABLE partial (id int PRIMARY KEY);
			CREATE TABLE broken (id nosuchtype);`)},
		"000002_bad.down.sql": {Data: []byte(`DROP TABLE partial;`)},
	}
	m, err := postgres.NewMigratorFromFS(fsys, dbURL)
	if err != nil {
		t.Fatalf("NewMigratorFromFS: %v", err)
	}
	defer m.Close()

	err = m.Up()
	if err == nil || !strings.Contains(err.Error(), "nosuchtype") {
		t.Fatalf("Up should fail on the broken migration, got %v", err)
	}

	tables := tableNames(t, dbURL, schema)
	if !tables["good"] {
		t.Error("the migration before the failure should have been applied")
	}
	if tables["partial"] {
		t.Error("the failed migration's first statement was not rolled back")
	}

	version, dirty, err := m.Version()
	if err != nil || version != 2 || !dirty {
		t.Fatalf("after failure: version=%d dirty=%v err=%v, want 2/dirty", version, dirty, err)
	}

	if err := m.Force(1); err != nil {
		t.Fatalf("Force(1): %v", err)
	}
	if version, dirty, _ = m.Version(); version != 1 || dirty {
		t.Fatalf("after Force(1): version=%d dirty=%v", version, dirty)
	}
}

// A migration that cannot take the lock it needs must fail quickly rather
// than queue for it. PostgreSQL's lock queue is ordered, so a migration
// waiting for ACCESS EXCLUSIVE on a busy table puts every query that
// arrives after it behind itself: the table stops serving for as long as
// the migration waits. postgres.MigrationLockTimeout is what turns that
// outage into a failed deployment that the Job retries.
func TestAMigrationThatCannotTakeItsLockFailsFastInsteadOfBlocking(t *testing.T) {
	t.Parallel()
	dbURL, _ := postgrestest.NewSchema(t)

	first := fstest.MapFS{
		"000001_table.up.sql":   {Data: []byte(`CREATE TABLE lockme (id int PRIMARY KEY);`)},
		"000001_table.down.sql": {Data: []byte(`DROP TABLE lockme;`)},
	}
	m1, err := postgres.NewMigratorFromFS(first, dbURL)
	if err != nil {
		t.Fatalf("NewMigratorFromFS: %v", err)
	}
	if err := m1.Up(); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	m1.Close()

	// Another session holds the lock the next migration needs, the way a
	// long-running query on a hot table would.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	holder, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect the lock holder: %v", err)
	}
	defer holder.Close(ctx)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE lockme IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("take the conflicting lock: %v", err)
	}

	second := fstest.MapFS{
		"000001_table.up.sql":   {Data: []byte(`CREATE TABLE lockme (id int PRIMARY KEY);`)},
		"000001_table.down.sql": {Data: []byte(`DROP TABLE lockme;`)},
		"000002_alter.up.sql":   {Data: []byte(`ALTER TABLE lockme ADD COLUMN extra int;`)},
		"000002_alter.down.sql": {Data: []byte(`ALTER TABLE lockme DROP COLUMN extra;`)},
	}
	m2, err := postgres.NewMigratorFromFS(second, dbURL)
	if err != nil {
		t.Fatalf("NewMigratorFromFS: %v", err)
	}
	defer m2.Close()

	started := time.Now()
	err = m2.Up()
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("the migration should have failed: another session holds the lock it needs")
	}
	// The point of the timeout: it gave up rather than waiting for the
	// holder, which is still holding.
	if elapsed > 30*time.Second {
		t.Errorf("the migration waited %s for its lock; the lock timeout did not apply", elapsed)
	}
	if !strings.Contains(err.Error(), "lock timeout") && !strings.Contains(err.Error(), "55P03") {
		t.Errorf("failure was not a lock timeout: %v", err)
	}
	t.Logf("gave up after %s: %v", elapsed.Round(time.Millisecond), err)

	// The schema was not changed: the statement never ran.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release the conflicting lock: %v", err)
	}

	// Recovery is not automatic, and a deployment runbook has to say so:
	// golang-migrate marks the version dirty on any failure, so retrying
	// refuses until the marker is cleared -- even though nothing was
	// applied and the lock is now free.
	version, dirty, err := m2.Version()
	if err != nil || version != 2 || !dirty {
		t.Fatalf("after the lock timeout: version=%d dirty=%v err=%v, want 2/dirty", version, dirty, err)
	}
	if err := m2.Up(); err == nil || !strings.Contains(err.Error(), "Dirty") {
		t.Errorf("a retry over a dirty version should refuse, got %v", err)
	}
	if err := m2.Force(1); err != nil {
		t.Fatalf("Force(1) to clear the marker: %v", err)
	}
	if err := m2.Up(); err != nil {
		t.Errorf("after clearing the marker the migration should apply: %v", err)
	}
}
