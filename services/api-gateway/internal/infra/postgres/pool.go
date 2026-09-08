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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
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

// Options tune a pool. Nil fields take safe defaults.
type Options struct {
	// Metrics receives the latency of every statement; nil records none.
	Metrics metrics.Recorder
	// Tracer opens one span per statement; nil records none.
	Tracer tracing.Tracer
}

// Connect builds a connection pool. Connections are established lazily.
// Every connection runs in UTC and scans timestamps as UTC.
func Connect(ctx context.Context, cfg config.Database, opts Options) (*pgxpool.Pool, error) {
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

	// Timing every statement here rather than in each repository means a
	// statement added later is measured without anyone remembering to.
	pc.ConnConfig.Tracer = queryTracer{
		recorder: metrics.OrNoop(opts.Metrics),
		tracer:   tracing.OrNoop(opts.Tracer),
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// PoolCollector reports the connection pool's saturation, which is what
// distinguishes a slow database from a gateway that has run out of
// connections to it. It is a collector rather than a set of gauges because
// the pool already keeps these numbers; copying them on a timer would only
// add a way for them to be stale.
func PoolCollector(pool *pgxpool.Pool) prometheus.Collector {
	return poolCollector{pool: pool, descriptions: map[string]*prometheus.Desc{
		"acquired": prometheus.NewDesc(metrics.Namespace+"_database_connections_acquired",
			"Connections currently in use.", nil, nil),
		"idle": prometheus.NewDesc(metrics.Namespace+"_database_connections_idle",
			"Connections open and unused.", nil, nil),
		"total": prometheus.NewDesc(metrics.Namespace+"_database_connections_total",
			"Connections open, in use or not.", nil, nil),
		"max": prometheus.NewDesc(metrics.Namespace+"_database_connections_max",
			"Largest number of connections the pool may open.", nil, nil),
	}}
}

type poolCollector struct {
	pool         *pgxpool.Pool
	descriptions map[string]*prometheus.Desc
}

func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descriptions {
		ch <- d
	}
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.pool.Stat()
	values := map[string]float64{
		"acquired": float64(stat.AcquiredConns()),
		"idle":     float64(stat.IdleConns()),
		"total":    float64(stat.TotalConns()),
		"max":      float64(stat.MaxConns()),
	}
	for name, value := range values {
		ch <- prometheus.MustNewConstMetric(c.descriptions[name], prometheus.GaugeValue, value)
	}
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
