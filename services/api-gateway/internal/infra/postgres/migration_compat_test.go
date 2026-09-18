//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
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

// Rolling-deployment compatibility, checked migration by migration.
//
// Migrations run as a Job *before* the new version is rolled out, and the
// rollout replaces pods one at a time while traffic continues. For the
// length of that rollout the previous application version is serving
// against the new schema. So the property that matters is not that the new
// code works -- every other test covers that -- but that the *old* code
// still does.
//
// Four changes break it, and all four look harmless in review:
//
//   - a table or column removed or renamed: the old version still selects
//     and inserts it by name;
//   - a column whose type changed: the old version scans it into the type
//     it was;
//   - an existing nullable column made NOT NULL: the old version inserts
//     rows without it;
//   - a new NOT NULL column with no default, added to a table that already
//     existed: the old version's INSERT does not name the column, so every
//     write it makes fails.
//
// Each is legitimate as the contract half of an expand/contract pair, once
// no running version depends on the thing being removed. A migration that
// does one deliberately says so in its own file with a
// `-- rolling-deployment:` note and a reason, and this test then allows it.
// A migration that does one by accident fails here rather than in
// production. See docs/DATABASE.md, "Expand and contract".
const rollingDeploymentWaiver = "-- rolling-deployment:"

type columnFacts struct {
	dataType   string
	isNullable string
	hasDefault bool
}

// schemaAt is every column of every table in the schema, keyed "table.column".
func schemaAt(t *testing.T, dbURL, schema string) map[string]columnFacts {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `
		SELECT table_name, column_name, data_type, is_nullable, column_default IS NOT NULL
		FROM information_schema.columns
		WHERE table_schema = $1
		ORDER BY table_name, column_name`, schema)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	out := map[string]columnFacts{}
	for rows.Next() {
		var table, column, dataType, nullable string
		var hasDefault bool
		if err := rows.Scan(&table, &column, &dataType, &nullable, &hasDefault); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[table+"."+column] = columnFacts{dataType: dataType, isNullable: nullable, hasDefault: hasDefault}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns: %v", err)
	}
	return out
}

func tablesOf(schema map[string]columnFacts) map[string]bool {
	out := map[string]bool{}
	for key := range schema {
		table, _, _ := strings.Cut(key, ".")
		out[table] = true
	}
	return out
}

// upFiles lists the embedded migrations in version order.
func upFiles(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("reading migrations: %v (%d found)", err, len(names))
	}
	sort.Strings(names)
	return names
}

func versionOf(t *testing.T, name string) uint {
	t.Helper()
	number, _, ok := strings.Cut(name, "_")
	if !ok {
		t.Fatalf("migration %q is not named NNNNNN_description.up.sql", name)
	}
	value, err := strconv.ParseUint(number, 10, 32)
	if err != nil {
		t.Fatalf("migration %q has a non-numeric version: %v", name, err)
	}
	return uint(value)
}

// subsetThrough is the real migrations up to and including version upTo,
// which is what the schema looked like at that release.
func subsetThrough(t *testing.T, upTo uint) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	for _, name := range entries {
		if versionOf(t, name) > upTo {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

func TestEveryMigrationKeepsThePreviousVersionWorking(t *testing.T) {
	t.Parallel()
	dbURL, schema := postgrestest.NewSchema(t)

	names := upFiles(t)
	previous := map[string]columnFacts{} // the empty schema, before migration 1
	violations := 0

	for _, name := range names {
		version := versionOf(t, name)
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		waived := strings.Contains(string(body), rollingDeploymentWaiver)

		// Apply exactly this one migration: the migrator is already at
		// version-1, so Up over the subset through `version` advances by one.
		m, err := postgres.NewMigratorFromFS(subsetThrough(t, version), dbURL)
		if err != nil {
			t.Fatalf("%s: migrator: %v", name, err)
		}
		if err := m.Up(); err != nil {
			m.Close()
			t.Fatalf("%s: up: %v", name, err)
		}
		if got, _, _ := m.Version(); got != version {
			m.Close()
			t.Fatalf("%s: expected to be at version %d, at %d", name, version, got)
		}
		m.Close()

		current := schemaAt(t, dbURL, schema)
		before, after := tablesOf(previous), tablesOf(current)

		var found []string
		for table := range before {
			if !after[table] {
				found = append(found, fmt.Sprintf("table %q was removed or renamed", table))
			}
		}
		for key, was := range previous {
			is, still := current[key]
			if !still {
				table, _, _ := strings.Cut(key, ".")
				if after[table] { // a dropped table is already reported once
					found = append(found, fmt.Sprintf("column %q was removed or renamed", key))
				}
				continue
			}
			if is.dataType != was.dataType {
				found = append(found, fmt.Sprintf("column %q changed type from %s to %s", key, was.dataType, is.dataType))
			}
			if was.isNullable == "YES" && is.isNullable == "NO" {
				found = append(found, fmt.Sprintf("column %q became NOT NULL; the previous version writes rows without it", key))
			}
		}
		for key, is := range current {
			if _, existed := previous[key]; existed {
				continue
			}
			table, _, _ := strings.Cut(key, ".")
			if !before[table] {
				continue // a brand new table: the previous version does not use it
			}
			if is.isNullable == "NO" && !is.hasDefault {
				found = append(found, fmt.Sprintf("column %q is NOT NULL with no default on an existing table; every INSERT the previous version makes would fail", key))
			}
		}

		switch {
		case len(found) == 0 && waived:
			t.Errorf("%s carries a %s note but makes no breaking change; remove the note", name, rollingDeploymentWaiver)
			violations++
		case len(found) == 0:
			t.Logf("%s: compatible with the previous version", name)
		case waived:
			t.Logf("%s: breaking, and declared: %s", name, strings.Join(found, "; "))
		default:
			t.Errorf("%s breaks the previous application version:\n    %s\n  If this is the contract half of an expand/contract pair, say so in the migration with a %s note and a reason (docs/DATABASE.md).",
				name, strings.Join(found, "\n    "), rollingDeploymentWaiver)
			violations++
		}
		previous = current
	}

	if violations == 0 {
		t.Logf("all %d migrations keep the previous application version working", len(names))
	}
}

// The deployment applies migrations and then rolls pods, so the schema the
// old version meets is the newest one. This checks the other half of that
// promise at the only version pair that exists today in production terms:
// the newest schema still serves the reads and writes the application makes.
func TestTheNewestSchemaServesTheApplicationsOwnStatements(t *testing.T) {
	t.Parallel()
	dbURL, _ := postgrestest.NewSchema(t)
	m, err := postgres.NewMigrator(dbURL)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Every table the application writes, inserted without naming any
	// column added after its table was created. A column added later with
	// no default, or made NOT NULL, would fail here exactly as it would for
	// a pod still running the previous version.
	if _, err := conn.Exec(ctx, `INSERT INTO users (email, password_hash, role) VALUES ('compat@vitalmesh.local', 'x', 'OPERATOR')`); err != nil {
		t.Errorf("the previous version's user insert no longer works: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO patients (external_reference, date_of_birth, sex) VALUES ('compat-1', DATE '1990-01-01', 'FEMALE')`); err != nil {
		t.Errorf("the previous version's patient insert no longer works: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source)
		SELECT id, 'HEART_RATE', 72, 'bpm', now(), 'compat' FROM patients WHERE external_reference = 'compat-1'`); err != nil {
		t.Errorf("the previous version's measurement insert no longer works: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, method, path, key, request_fingerprint, expires_at)
		SELECT id, 'POST', '/api/v1/patients', 'compat-key', 'fingerprint', now() + interval '1 hour'
		FROM users WHERE email = 'compat@vitalmesh.local'`); err != nil {
		t.Errorf("the previous version's idempotency claim no longer works: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO processing_jobs (patient_id, parameters, algorithm_version, created_by)
		SELECT p.id, '{}'::jsonb, '1.0.0', u.id FROM patients p, users u
		WHERE p.external_reference = 'compat-1' AND u.email = 'compat@vitalmesh.local'`); err != nil {
		t.Errorf("the previous version's job insert no longer works: %v", err)
	}
}
