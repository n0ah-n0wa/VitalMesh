//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// pgCode returns the SQLSTATE and constraint name of a driver error.
func pgCode(t *testing.T, err error) (string, string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %v", err)
	}
	return pgErr.Code, pgErr.ConstraintName
}

func TestRequiredIndexesExist(t *testing.T) {
	t.Parallel()
	pool, _, schema := postgrestest.New(t)

	rows, err := pool.Query(ctx(t), `SELECT tablename, indexdef FROM pg_indexes WHERE schemaname = $1`, schema)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Table string
		Def   string
	}])
	if err != nil {
		t.Fatalf("collect indexes: %v", err)
	}
	// An access path is served by any index whose leading columns are the
	// required ones, in order.
	has := func(table, columns string) bool {
		for _, d := range defs {
			// pg_indexes quotes reserved words such as "window".
			def := strings.ReplaceAll(d.Def, `"`, "")
			if d.Table == table && (strings.Contains(def, "("+columns+")") || strings.Contains(def, "("+columns+", ")) {
				return true
			}
		}
		return false
	}

	required := [][2]string{
		{"measurements", "patient_id, recorded_at"},
		{"measurements", "patient_id, type, recorded_at"},
		{"processing_jobs", "patient_id, created_at"},
		{"processing_jobs", "status, created_at"},
		{"processing_results", "job_id"},
		{"processing_results", "patient_id, measurement_type, window, window_start"},
		{"audit_logs", "resource_id, created_at"},
		{"patients", "created_at, id"},
		{"idempotency_keys", "expires_at"},
		{"users", "email"},
		{"users", "created_at, id"},
	}
	for _, r := range required {
		if !has(r[0], r[1]) {
			t.Errorf("missing index on %s(%s)", r[0], r[1])
		}
	}
}

func TestTimestampsAreTimezoneAwareAndScannedAsUTC(t *testing.T) {
	t.Parallel()
	pool, _, schema := postgrestest.New(t)

	rows, err := pool.Query(ctx(t), `
		SELECT table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = $1 AND column_name LIKE '%\_at'`, schema)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Table, Column, Type string }])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("no *_at columns found")
	}
	for _, c := range cols {
		if c.Type != "timestamp with time zone" {
			t.Errorf("%s.%s is %q, want timestamp with time zone", c.Table, c.Column, c.Type)
		}
	}

	user, err := postgres.NewUsers(pool).Create(ctx(t), "tz@example.com", "hash", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", user.CreatedAt.Location())
	}
}

func TestConstraintsRejectInvalidRows(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)

	patient, err := postgres.NewPatients(pool).Create(c, "p-constraints", time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexOther)
	if err != nil {
		t.Fatalf("create patient: %v", err)
	}
	user, err := postgres.NewUsers(pool).Create(c, "c@example.com", "hash", domain.RoleOperator)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()

	cases := []struct {
		name       string
		sql        string
		args       []any
		code       string
		constraint string
	}{
		{"not null", `INSERT INTO measurements (patient_id, type, value, unit, source) VALUES ($1, 'SPO2', 98, '%', 's')`, []any{patient.ID}, "23502", ""},
		{"foreign key patient", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'SPO2', 98, '%', $2, 's')`, []any{uuid.New(), now}, "23503", "measurements_patient_id_fkey"},
		{"foreign key type", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'PULSE', 98, '%', $2, 's')`, []any{patient.ID, now}, "23503", "measurements_type_fkey"},
		{"unit mismatch (trigger)", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'SPO2', 98, 'bpm', $2, 's')`, []any{patient.ID, now}, "23514", "measurements_unit_check"},
		{"value above range (trigger)", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'SPO2', 100.5, '%', $2, 's')`, []any{patient.ID, now}, "23514", "measurements_value_range_check"},
		{"value below range (trigger)", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'BODY_TEMPERATURE', 19.9, 'C', $2, 's')`, []any{patient.ID, now}, "23514", "measurements_value_range_check"},
		// PostgreSQL orders NaN and Infinity above every number, so the
		// BEFORE trigger's range check rejects them first; the finite CHECK
		// constraint stays as defence in depth behind it.
		{"value NaN", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'SPO2', 'NaN'::float8, '%', $2, 's')`, []any{patient.ID, now}, "23514", "measurements_value_range_check"},
		{"value Infinity", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'SPO2', 'Infinity'::float8, '%', $2, 's')`, []any{patient.ID, now}, "23514", "measurements_value_range_check"},
		{"metadata not an object", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source, metadata) VALUES ($1, 'SPO2', 98, '%', $2, 's', '[1]'::jsonb)`, []any{patient.ID, now}, "23514", "measurements_metadata_object_check"},
		{"empty source", `INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) VALUES ($1, 'SPO2', 98, '%', $2, '')`, []any{patient.ID, now}, "23514", "measurements_source_check"},
		{"invalid role", `INSERT INTO users (email, password_hash, role) VALUES ('r@example.com', 'h', 'ROOT')`, nil, "23514", "users_role_check"},
		{"upper case email", `INSERT INTO users (email, password_hash, role) VALUES ('Upper@example.com', 'h', 'USER')`, nil, "23514", "users_email_check"},
		{"invalid sex", `INSERT INTO patients (external_reference, date_of_birth, sex) VALUES ('x', '1990-01-01', 'N/A')`, nil, "23514", "patients_sex_check"},
		{"deleted without deleted_at", `INSERT INTO patients (external_reference, date_of_birth, sex, status) VALUES ('x', '1990-01-01', 'OTHER', 'DELETED')`, nil, "23514", "patients_deleted_at_check"},
		{"job completed without timestamps", `INSERT INTO processing_jobs (patient_id, status, parameters, algorithm_version, created_by) VALUES ($1, 'COMPLETED', '{}', '1.0.0', $2)`, []any{patient.ID, user.ID}, "23514", "processing_jobs_completed_check"},
		{"job parameters not object", `INSERT INTO processing_jobs (patient_id, parameters, algorithm_version, created_by) VALUES ($1, '[]', '1.0.0', $2)`, []any{patient.ID, user.ID}, "23514", "processing_jobs_parameters_check"},
		{"job unknown creator", `INSERT INTO processing_jobs (patient_id, parameters, algorithm_version, created_by) VALUES ($1, '{}', '1.0.0', $2)`, []any{patient.ID, uuid.New()}, "23503", "processing_jobs_created_by_fkey"},
		{"result unknown job", `INSERT INTO processing_results (job_id, measurement_type, "window", window_start, statistics, algorithm_version, service_version) VALUES ($1, 'SPO2', '1h', $2, '{}', '1.0.0', 'v')`, []any{uuid.New(), now}, "23503", "processing_results_job_id_fkey"},
		{"result invalid window", `INSERT INTO processing_results (job_id, measurement_type, "window", window_start, statistics, algorithm_version, service_version) SELECT id, 'SPO2', '2h', $1, '{}', '1.0.0', 'v' FROM processing_jobs LIMIT 1`, []any{now}, "23514", "processing_results_window_check"},
		{"audit user actor without id", `INSERT INTO audit_logs (actor_type, action, resource_type, request_id) VALUES ('USER', 'LOGIN', 'user', 'r')`, nil, "23514", "audit_logs_actor_check"},
		{"audit unknown action", `INSERT INTO audit_logs (actor_type, action, resource_type, request_id) VALUES ('SYSTEM', 'REBOOT', 'system', 'r')`, nil, "23514", "audit_logs_action_check"},
		{"idempotency completed without status", `INSERT INTO idempotency_keys (user_id, method, path, key, request_fingerprint, status, expires_at) VALUES ($1, 'POST', '/x', 'k', 'f', 'COMPLETED', now() + interval '1 day')`, []any{user.ID}, "23514", "idempotency_keys_completed_check"},
		{"idempotency expires in the past", `INSERT INTO idempotency_keys (user_id, method, path, key, request_fingerprint, expires_at) VALUES ($1, 'POST', '/x', 'k', 'f', now() - interval '1 day')`, []any{user.ID}, "23514", "idempotency_keys_expires_check"},
	}
	// The result-window case needs a job to exist.
	if _, err := postgres.NewJobs(pool).Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID}); err != nil {
		t.Fatalf("create job: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(c, tc.sql, tc.args...)
			if err == nil {
				t.Fatal("insert succeeded, want a constraint violation")
			}
			code, constraint := pgCode(t, err)
			if code != tc.code {
				t.Errorf("SQLSTATE = %s, want %s (%v)", code, tc.code, err)
			}
			if tc.constraint != "" && constraint != tc.constraint {
				t.Errorf("constraint = %q, want %q", constraint, tc.constraint)
			}
		})
	}
}

func TestUniquenessConstraints(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	users := postgres.NewUsers(pool)

	if _, err := users.Create(c, "dup@example.com", "h", domain.RoleUser); err != nil {
		t.Fatalf("first user: %v", err)
	}
	_, err := users.Create(c, "dup@example.com", "h", domain.RoleUser)
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindConflict || domErr.Code != "USERS_EMAIL_KEY" {
		t.Fatalf("duplicate email: got %v, want conflict USERS_EMAIL_KEY", err)
	}

	patients := postgres.NewPatients(pool)
	dob := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	patient, err := patients.Create(c, "ref-1", dob, domain.SexFemale)
	if err != nil {
		t.Fatalf("patient: %v", err)
	}
	if _, err := patients.Create(c, "ref-1", dob, domain.SexMale); !errors.As(err, &domErr) || domErr.Code != "PATIENTS_EXTERNAL_REFERENCE_KEY" {
		t.Fatalf("duplicate external reference: got %v", err)
	}

	measurements := postgres.NewMeasurements(pool)
	at := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	reading := postgres.NewMeasurement{PatientID: patient.ID, Type: domain.SpO2, Value: 98, Unit: "%", RecordedAt: at, Source: "device-1"}
	if _, err := measurements.Create(c, reading); err != nil {
		t.Fatalf("first reading: %v", err)
	}
	if _, err := measurements.Create(c, reading); !errors.As(err, &domErr) || domErr.Code != "MEASUREMENTS_PATIENT_TYPE_TIME_SOURCE_KEY" {
		t.Fatalf("duplicate reading: got %v", err)
	}
	reading.Source = "device-2"
	if _, err := measurements.Create(c, reading); err != nil {
		t.Fatalf("same reading from another source must be allowed: %v", err)
	}
}

func TestJobTransitionsAreEnforcedByTheDatabase(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)

	job, err := postgres.NewJobs(pool).Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	// Raw updates bypass the repository's CAS but not the trigger.
	_, err = pool.Exec(c, `UPDATE processing_jobs SET status = 'COMPLETED', started_at = now(), completed_at = now() WHERE id = $1`, job.ID)
	if code, constraint := pgCode(t, err); code != "23514" || constraint != "processing_jobs_transition_check" {
		t.Fatalf("PENDING -> COMPLETED: got %s/%s (%v)", code, constraint, err)
	}
	if _, err := pool.Exec(c, `UPDATE processing_jobs SET status = 'PROCESSING', started_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("PENDING -> PROCESSING should be allowed: %v", err)
	}
	_, err = pool.Exec(c, `UPDATE processing_jobs SET status = 'PENDING', started_at = NULL WHERE id = $1`, job.ID)
	if code, _ := pgCode(t, err); code != "23514" {
		t.Fatalf("PROCESSING -> PENDING: got %s (%v)", code, err)
	}
	_, err = pool.Exec(c, `UPDATE processing_jobs SET status = 'FAILED', failed_at = now() WHERE id = $1`, job.ID)
	if code, constraint := pgCode(t, err); code != "23514" || constraint != "processing_jobs_failed_check" {
		t.Fatalf("FAILED without error_code: got %s/%s (%v)", code, constraint, err)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)

	entry, err := postgres.NewAudit(pool).Append(c, postgres.NewAuditEntry{ActorType: domain.ActorSystem, Action: domain.AuditRetentionRun, ResourceType: "retention", RequestID: "req-1"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	for name, sql := range map[string]string{
		"update": `UPDATE audit_logs SET request_id = 'tampered' WHERE id = $1`,
		"delete": `DELETE FROM audit_logs WHERE id = $1`,
	} {
		_, err := pool.Exec(c, sql, entry.ID)
		if code, _ := pgCode(t, err); code != "42501" {
			t.Errorf("%s: SQLSTATE = %s, want 42501 (%v)", name, code, err)
		}
	}
	var n int
	if err := pool.QueryRow(c, `SELECT count(*) FROM audit_logs WHERE id = $1 AND request_id = 'req-1'`, entry.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("entry changed or vanished: n=%d err=%v", n, err)
	}
}

func TestTransactionsRollBackOnError(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)

	sentinel := errors.New("abort")
	err := postgres.WithTx(c, pool, func(tx pgx.Tx) error {
		if _, err := postgres.NewUsers(tx).Create(c, "rollback@example.com", "h", domain.RoleUser); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx should return the callback error, got %v", err)
	}
	if _, err := postgres.NewUsers(pool).GetByEmail(c, "rollback@example.com"); err == nil {
		t.Fatal("user should not exist after rollback")
	}

	err = postgres.WithTx(c, pool, func(tx pgx.Tx) error {
		_, err := postgres.NewUsers(tx).Create(c, "commit@example.com", "h", domain.RoleUser)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	if _, err := postgres.NewUsers(pool).GetByEmail(c, "commit@example.com"); err != nil {
		t.Fatalf("user should exist after commit: %v", err)
	}
}

// Two workers race to start the same job; the compare-and-set lets exactly
// one win and the loser gets a conflict, never a second start.
func TestConcurrentStartIsCompareAndSet(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)
	jobs := postgres.NewJobs(pool)

	job, err := jobs.Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	const workers = 8
	results := make(chan error, workers)
	now := time.Now().UTC()
	for i := range workers {
		go func() {
			_, err := jobs.Start(c, job.ID, now, now.Add(time.Minute), "worker-"+string(rune('a'+i)))
			results <- err
		}()
	}
	var wins, conflicts int
	for range workers {
		err := <-results
		var domErr *domain.Error
		switch {
		case err == nil:
			wins++
		case errors.As(err, &domErr) && domErr.Kind == domain.KindConflict:
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != workers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1/%d", wins, conflicts, workers-1)
	}
	started, err := jobs.GetByID(c, job.ID)
	if err != nil || started.Status != domain.JobProcessing || started.AttemptCount != 1 {
		t.Fatalf("job = %+v err=%v, want PROCESSING with attempt_count 1", started, err)
	}
}

func seedPatientAndUser(t *testing.T, pool *pgxpool.Pool) (domain.Patient, domain.User) {
	t.Helper()
	c := ctx(t)
	patient, err := postgres.NewPatients(pool).Create(c, "seed-"+uuid.NewString(), time.Date(1985, 6, 15, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatalf("seed patient: %v", err)
	}
	user, err := postgres.NewUsers(pool).Create(c, uuid.NewString()+"@example.com", "hash", domain.RoleOperator)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return patient, user
}
