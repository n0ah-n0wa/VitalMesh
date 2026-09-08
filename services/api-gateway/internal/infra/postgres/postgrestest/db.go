//go:build integration || e2e

// Package postgrestest gives integration tests a private, migrated schema on
// the PostgreSQL database named by TEST_DATABASE_URL. Each call creates a
// fresh schema and drops it when the test ends, so tests are isolated and
// run in parallel. Schemas rather than databases keep this cheap: dropping
// a database forces a server-wide checkpoint, dropping a schema does not.
//
// Connections carry `search_path=<schema>`, so the unqualified names in the
// migrations, in trigger bodies and in repository SQL resolve to the test's
// schema.
package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
)

// NewSchema creates an empty schema and returns a connection URL whose
// search_path is that schema, together with the schema name. It does not
// run migrations.
func NewSchema(t *testing.T) (dbURL, schema string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL is required for integration tests (see make dev-db)")
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	schema = "vm_test_" + hex.EncodeToString(suffix[:])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", base, err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create schema %s: %v", schema, err)
	}
	_ = admin.Close(ctx)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Logf("cleanup: connect: %v", err)
			return
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), schema
}

// New creates a schema, applies every migration into it and returns a pool
// bound to it together with the URL and the schema name.
func New(t *testing.T) (pool *pgxpool.Pool, dbURL, schema string) {
	t.Helper()
	dbURL, schema = NewSchema(t)

	migrator, err := postgres.NewMigrator(dbURL)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := migrator.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("close migrator: %v", err)
	}

	pool, err = postgres.Connect(context.Background(), config.Database{
		URL:            dbURL,
		MaxConns:       4,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, dbURL, schema
}
