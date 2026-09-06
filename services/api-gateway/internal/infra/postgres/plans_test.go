//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

// The tables large enough that a sequential scan in a request path is a
// defect. Small lookup tables (users, patients, measurement_types) are
// legitimately scanned when they hold a few rows.
var largeTables = map[string]bool{
	"measurements":       true,
	"processing_jobs":    true,
	"processing_results": true,
	"audit_logs":         true,
	"idempotency_keys":   true,
}

// queryRecorder captures every statement a pool executes, so the plans
// asserted below are the repositories' real SQL and arguments.
type queryRecorder struct {
	mu      sync.Mutex
	queries []recordedQuery
}

type recordedQuery struct {
	sql  string
	args []any
}

func (r *queryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, recordedQuery{sql: data.SQL, args: data.Args})
	return ctx
}

func (r *queryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *queryRecorder) drain() []recordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.queries
	r.queries = nil
	return out
}

// seedRealisticData fills the schema with enough rows for the planner to
// prefer indexes: 40 patients with 250 readings each, 20 jobs per patient
// with 10 results each, 50 audit entries per patient, 2000 idempotency keys.
func seedRealisticData(t *testing.T, pool *pgxpool.Pool) (domain.Patient, domain.User) {
	t.Helper()
	// Seeding is bulk DDL/DML that shares the server with the other
	// parallel tests; give it more room than a single query gets.
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	statements := []string{
		`INSERT INTO users (email, password_hash, role)
		 SELECT 'plan' || g || '@example.com', 'hash', 'OPERATOR' FROM generate_series(1, 5) g`,
		`INSERT INTO patients (external_reference, date_of_birth, sex)
		 SELECT 'plan-' || g, DATE '1980-01-01' + g, 'OTHER' FROM generate_series(1, 40) g`,
		// The seed values are valid by construction; skipping the per-row
		// validation trigger keeps this test fast.
		`ALTER TABLE measurements DISABLE TRIGGER measurements_validate`,
		`INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source)
		 SELECT p.id, 'HEART_RATE', 60 + (r % 40), 'bpm', TIMESTAMPTZ '2026-08-01' + (r * interval '1 minute'), 'seed'
		 FROM patients p CROSS JOIN generate_series(0, 249) r`,
		`ALTER TABLE measurements ENABLE TRIGGER measurements_validate`,
		`INSERT INTO processing_jobs (patient_id, status, parameters, algorithm_version, created_by, requested_at, cancelled_at, created_at)
		 SELECT p.id, CASE WHEN g % 5 = 0 THEN 'PENDING' ELSE 'CANCELLED' END, '{}', '1.0.0',
		        (SELECT id FROM users ORDER BY email LIMIT 1),
		        TIMESTAMPTZ '2026-08-01' + (g * interval '1 minute'),
		        CASE WHEN g % 5 = 0 THEN NULL ELSE TIMESTAMPTZ '2026-08-01' + (g * interval '1 minute') END,
		        TIMESTAMPTZ '2026-08-01' + (g * interval '1 minute')
		 FROM patients p CROSS JOIN generate_series(1, 20) g`,
		`INSERT INTO processing_results (job_id, measurement_type, "window", window_start, statistics, algorithm_version, service_version)
		 SELECT j.id, 'HEART_RATE', '1h', TIMESTAMPTZ '2026-08-01' + (w * interval '1 hour'), '{"count":60}', '1.0.0', 'v'
		 FROM processing_jobs j CROSS JOIN generate_series(0, 9) w`,
		`INSERT INTO audit_logs (actor_type, action, resource_type, resource_id, request_id)
		 SELECT 'SYSTEM', 'RETENTION_RUN', 'patient', p.id, 'req-' || g
		 FROM patients p CROSS JOIN generate_series(1, 50) g`,
		`INSERT INTO idempotency_keys (user_id, method, path, key, request_fingerprint, created_at, expires_at)
		 SELECT u.id, 'POST', '/api/v1/measurements', 'k-' || g, 'fp', now() - interval '2 days',
		        CASE WHEN g % 2 = 0 THEN now() - interval '1 day' ELSE now() + interval '1 day' END
		 FROM users u CROSS JOIN generate_series(1, 400) g`,
		// Only this schema's tables: a bare ANALYZE would walk every test
		// schema on the server.
		`ANALYZE users, patients, measurements, processing_jobs, processing_results, audit_logs, idempotency_keys`,
	}
	for _, sql := range statements {
		if _, err := pool.Exec(c, sql); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	patient, err := postgres.NewPatients(pool).GetByID(c, firstID(t, pool, "patients"))
	if err != nil {
		t.Fatalf("seed patient: %v", err)
	}
	user, err := postgres.NewUsers(pool).GetByID(c, firstID(t, pool, "users"))
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return patient, user
}

func firstID(t *testing.T, pool *pgxpool.Pool, table string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx(t), `SELECT id FROM `+pgx.Identifier{table}.Sanitize()+` ORDER BY created_at, id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("first id of %s: %v", table, err)
	}
	return id
}

// explainNodes returns every plan node of a statement as (node type, relation).
func explainNodes(t *testing.T, pool *pgxpool.Pool, q recordedQuery) []string {
	t.Helper()
	var raw string
	if err := pool.QueryRow(ctx(t), "EXPLAIN (FORMAT JSON) "+q.sql, q.args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, q.sql)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	var nodes []string
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		relation, _ := node["Relation Name"].(string)
		nodes = append(nodes, fmt.Sprintf("%s on %s", node["Node Type"], relation))
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if m, ok := child.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(plans[0].Plan)
	return nodes
}

// Every repository read path must reach large tables through an index.
// This is the specification's "index decisions validated using realistic
// query plans", automated: the queries are the repositories' own, captured
// through a pgx tracer and explained against a seeded schema.
func TestRepositoryQueriesAvoidSequentialScansOnLargeTables(t *testing.T) {
	t.Parallel()
	seedPool, dbURL, _ := postgrestest.New(t)
	patient, user := seedRealisticData(t, seedPool)

	recorder := &queryRecorder{}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	c := ctx(t)

	hr := domain.HeartRate
	from := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	to := from.Add(3 * time.Hour)
	mCursor := &postgres.MeasurementCursor{RecordedAt: from, ID: uuid.Nil}
	jCursor := &postgres.JobCursor{CreatedAt: from, ID: uuid.Nil}
	rCursor := &postgres.ResultCursor{MeasurementType: hr, Window: "1h", WindowStart: from, JobID: uuid.Nil}
	someJob := firstID(t, pool, "processing_jobs")

	reads := map[string]func() error{
		"measurements first page": func() error {
			_, err := postgres.NewMeasurements(pool).ListByPatient(c, patient.ID, postgres.MeasurementFilter{}, nil, 50)
			return err
		},
		"measurements cursor page": func() error {
			_, err := postgres.NewMeasurements(pool).ListByPatient(c, patient.ID, postgres.MeasurementFilter{}, mCursor, 50)
			return err
		},
		"measurements type and range with cursor": func() error {
			_, err := postgres.NewMeasurements(pool).ListByPatient(c, patient.ID, postgres.MeasurementFilter{Type: &hr, From: &from, To: &to}, mCursor, 50)
			return err
		},
		"measurement by id": func() error {
			_, err := postgres.NewMeasurements(pool).GetByID(c, uuid.New())
			return err
		},
		"jobs by patient with cursor": func() error {
			_, err := postgres.NewJobs(pool).ListByPatient(c, patient.ID, jCursor, 20)
			return err
		},
		"pending jobs": func() error {
			_, err := postgres.NewJobs(pool).ListPending(c, 50)
			return err
		},
		"results by job with cursor": func() error {
			_, err := postgres.NewResults(pool).ListByJob(c, someJob, rCursor, 50)
			return err
		},
		"results by patient first page": func() error {
			_, err := postgres.NewResults(pool).ListByPatient(c, patient.ID, nil, 50)
			return err
		},
		"results by patient with cursor": func() error {
			_, err := postgres.NewResults(pool).ListByPatient(c, patient.ID, rCursor, 50)
			return err
		},
		"audit by resource": func() error {
			_, err := postgres.NewAudit(pool).ListByResource(c, patient.ID, 20)
			return err
		},
		"idempotency lookup": func() error {
			_, err := postgres.NewIdempotency(pool).Get(c, user.ID, "POST", "/api/v1/measurements", "k-7")
			return err
		},
	}

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			recorder.drain()
			var domErr *domain.Error
			if err := read(); err != nil && !(errors.As(err, &domErr) && domErr.Kind == domain.KindNotFound) {
				t.Fatalf("query failed: %v", err)
			}
			queries := recorder.drain()
			if len(queries) == 0 {
				t.Fatal("no query recorded")
			}
			for _, q := range queries {
				nodes := explainNodes(t, pool, q)
				for _, n := range nodes {
					if strings.HasPrefix(n, "Seq Scan on ") && largeTables[strings.TrimPrefix(n, "Seq Scan on ")] {
						t.Errorf("sequential scan in plan:\n  %s\nplan nodes: %v\nsql: %s", n, nodes, q.sql)
					}
				}
			}
		})
	}

	// The expiry sweep is the one write path that touches many rows; it
	// must walk the expires_at index, not the table.
	t.Run("expiry sweep", func(t *testing.T) {
		recorder.drain()
		if _, err := postgres.NewIdempotency(pool).DeleteExpired(c, time.Now().UTC(), 100); err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		for _, q := range recorder.drain() {
			for _, n := range explainNodes(t, pool, q) {
				if n == "Seq Scan on idempotency_keys" {
					t.Errorf("expiry sweep scans the table: %v", n)
				}
			}
		}
	})
}
