// Package postgres is the PostgreSQL implementation of the gateway's
// persistence: the connection pool, migrations, transactions and one
// repository per table. Every query is explicit SQL; nothing hides how the
// database behaves.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

// DB is the subset of pgx shared by a pool and a transaction, so that a
// repository can run inside either. A batch sent through a pool runs in an
// implicit transaction (all statements succeed or none is kept); a batch
// sent through a transaction is part of it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults
}

// Connect builds a connection pool. Connections are established lazily.
// Every connection runs in UTC and scans timestamps as UTC.
func Connect(ctx context.Context, cfg config.Database) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pc.ConnConfig.RuntimeParams["application_name"] = "api-gateway"
	pc.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name:  "timestamptz",
			OID:   pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
		})
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// Checker reports PostgreSQL readiness.
type Checker struct {
	pool *pgxpool.Pool
}

// NewChecker returns a readiness checker for pool.
func NewChecker(pool *pgxpool.Pool) *Checker {
	return &Checker{pool: pool}
}

// Name implements health.Checker.
func (c *Checker) Name() string { return "postgres" }

// Check implements health.Checker by acquiring a connection and pinging.
func (c *Checker) Check(ctx context.Context) error {
	return c.pool.Ping(ctx)
}
