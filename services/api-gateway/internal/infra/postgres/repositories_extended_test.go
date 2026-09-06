//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

func countRows(t *testing.T, db postgres.DB, table string, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(ctx(t), `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func reading(patient uuid.UUID, kind domain.MeasurementType, unit string, value float64, at time.Time) postgres.NewMeasurement {
	return postgres.NewMeasurement{PatientID: patient, Type: kind, Value: value, Unit: unit, RecordedAt: at, Source: "batch"}
}

func TestMeasurementsCreateBatchIsAllOrNothing(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, _ := seedPatientAndUser(t, pool)
	measurements := postgres.NewMeasurements(pool)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	// Three valid readings and one out of range: nothing must be stored.
	mixed := []postgres.NewMeasurement{
		reading(patient.ID, domain.HeartRate, "bpm", 70, base),
		reading(patient.ID, domain.HeartRate, "bpm", 71, base.Add(time.Minute)),
		reading(patient.ID, domain.SpO2, "%", 101, base),
		reading(patient.ID, domain.HeartRate, "bpm", 72, base.Add(2*time.Minute)),
	}
	_, err := measurements.CreateBatch(c, mixed)
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindValidation || domErr.Code != "MEASUREMENTS_VALUE_RANGE_CHECK" {
		t.Fatalf("mixed batch: got %v, want the range violation", err)
	}
	if n := countRows(t, pool, "measurements", "patient_id = $1", patient.ID); n != 0 {
		t.Fatalf("%d readings stored from a rejected batch, want 0", n)
	}

	// A duplicate inside the batch is a conflict and also stores nothing.
	dup := []postgres.NewMeasurement{mixed[0], mixed[0]}
	if _, err := measurements.CreateBatch(c, dup); !isKind(err, domain.KindConflict) {
		t.Fatalf("duplicate in batch: got %v, want conflict", err)
	}
	if n := countRows(t, pool, "measurements", "patient_id = $1", patient.ID); n != 0 {
		t.Fatalf("%d readings stored from a conflicting batch, want 0", n)
	}

	valid := []postgres.NewMeasurement{mixed[0], mixed[1], mixed[3]}
	stored, err := measurements.CreateBatch(c, valid)
	if err != nil {
		t.Fatalf("valid batch: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("stored %d, want 3", len(stored))
	}
	for i, m := range stored {
		if m.Value != valid[i].Value || !m.RecordedAt.Equal(valid[i].RecordedAt) || m.ID == uuid.Nil {
			t.Errorf("stored[%d] = %+v does not match input %+v", i, m, valid[i])
		}
	}
	if empty, err := measurements.CreateBatch(c, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty batch = %v, %v", empty, err)
	}

	// Inside an explicit transaction the batch follows the transaction.
	err = postgres.WithTx(c, pool, func(tx pgx.Tx) error {
		if _, err := postgres.NewMeasurements(tx).CreateBatch(c, []postgres.NewMeasurement{
			reading(patient.ID, domain.HeartRate, "bpm", 80, base.Add(time.Hour)),
		}); err != nil {
			return err
		}
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("transaction should have been aborted")
	}
	if n := countRows(t, pool, "measurements", "patient_id = $1", patient.ID); n != 3 {
		t.Fatalf("%d readings after rolled-back batch, want 3", n)
	}
}

func TestMeasurementsListFilters(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, _ := seedPatientAndUser(t, pool)
	other, _ := seedPatientAndUser(t, pool)
	measurements := postgres.NewMeasurements(pool)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	var batch []postgres.NewMeasurement
	for i := range 6 {
		batch = append(batch, reading(patient.ID, domain.HeartRate, "bpm", 60+float64(i), base.Add(time.Duration(i)*time.Minute)))
	}
	for i := range 3 {
		batch = append(batch, reading(patient.ID, domain.SpO2, "%", 95+float64(i), base.Add(time.Duration(i)*time.Minute)))
	}
	batch = append(batch, reading(other.ID, domain.HeartRate, "bpm", 99, base))
	if _, err := measurements.CreateBatch(c, batch); err != nil {
		t.Fatalf("seed: %v", err)
	}

	spo2 := domain.SpO2
	from := base.Add(1 * time.Minute)
	to := base.Add(4 * time.Minute)
	cases := []struct {
		name   string
		filter postgres.MeasurementFilter
		want   int
	}{
		{"none", postgres.MeasurementFilter{}, 9},
		{"type", postgres.MeasurementFilter{Type: &spo2}, 3},
		{"from inclusive", postgres.MeasurementFilter{From: &from}, 5 + 2},
		{"to exclusive", postgres.MeasurementFilter{To: &to}, 4 + 3},
		{"range", postgres.MeasurementFilter{From: &from, To: &to}, 3 + 2},
		{"type and range", postgres.MeasurementFilter{Type: &spo2, From: &from, To: &to}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := measurements.ListByPatient(c, patient.ID, tc.filter, nil, 100)
			if err != nil {
				t.Fatalf("ListByPatient: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d readings, want %d", len(got), tc.want)
			}
			for i := 1; i < len(got); i++ {
				if got[i].RecordedAt.Before(got[i-1].RecordedAt) {
					t.Fatal("not in recording order")
				}
			}
		})
	}

	// Filter and cursor combine: page through the heart-rate readings two at a time.
	hr := domain.HeartRate
	var seen int
	var cursor *postgres.MeasurementCursor
	for {
		page, err := measurements.ListByPatient(c, patient.ID, postgres.MeasurementFilter{Type: &hr}, cursor, 2)
		if err != nil {
			t.Fatalf("paged: %v", err)
		}
		seen += len(page)
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = &postgres.MeasurementCursor{RecordedAt: last.RecordedAt, ID: last.ID}
	}
	if seen != 6 {
		t.Fatalf("paged through %d heart-rate readings, want 6", seen)
	}
}

func TestUsersListAndStatus(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	users := postgres.NewUsers(pool)

	var ids []uuid.UUID
	for i := range 5 {
		u, err := users.Create(c, "user"+string(rune('a'+i))+"@example.com", "h", domain.RoleUser)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, u.ID)
	}

	var seen []uuid.UUID
	var cursor *postgres.UserCursor
	for {
		page, err := users.List(c, cursor, 2)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, u := range page {
			seen = append(seen, u.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = &postgres.UserCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d users, want 5", len(seen))
	}
	for i := range ids {
		if seen[i] != ids[i] {
			t.Fatalf("order differs at %d", i)
		}
	}

	disabled, err := users.SetStatus(c, ids[0], domain.UserDisabled)
	if err != nil || disabled.Status != domain.UserDisabled {
		t.Fatalf("SetStatus = %+v, %v", disabled, err)
	}
	if _, err := users.SetStatus(c, ids[0], "FROZEN"); !isKind(err, domain.KindValidation) {
		t.Fatalf("invalid status: got %v, want validation", err)
	}
	if _, err := users.SetStatus(c, uuid.New(), domain.UserActive); !isKind(err, domain.KindNotFound) {
		t.Fatalf("unknown user: got %v, want not found", err)
	}
}

func TestJobsListByPatientAndPending(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)
	other, _ := seedPatientAndUser(t, pool)
	jobs := postgres.NewJobs(pool)

	create := func(p uuid.UUID) domain.ProcessingJob {
		j, err := jobs.Create(c, postgres.NewJob{PatientID: p, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return j
	}
	var mine []domain.ProcessingJob
	for range 3 {
		mine = append(mine, create(patient.ID))
	}
	theirs := create(other.ID)
	if _, err := jobs.Start(c, mine[1].ID, mine[1].RequestedAt, mine[1].RequestedAt.Add(time.Minute), "svc"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first, err := jobs.ListByPatient(c, patient.ID, nil, 2)
	if err != nil || len(first) != 2 || first[0].ID != mine[0].ID || first[1].ID != mine[1].ID {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	rest, err := jobs.ListByPatient(c, patient.ID, &postgres.JobCursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID}, 2)
	if err != nil || len(rest) != 1 || rest[0].ID != mine[2].ID {
		t.Fatalf("second page = %+v, %v", rest, err)
	}

	pending, err := jobs.ListPending(c, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	var pendingIDs []uuid.UUID
	for _, j := range pending {
		if j.Status != domain.JobPending {
			t.Errorf("non-pending job listed: %+v", j)
		}
		pendingIDs = append(pendingIDs, j.ID)
	}
	want := []uuid.UUID{mine[0].ID, mine[2].ID, theirs.ID}
	if len(pendingIDs) != len(want) {
		t.Fatalf("pending = %v, want %v", pendingIDs, want)
	}
	for i := range want {
		if pendingIDs[i] != want[i] {
			t.Fatalf("pending order differs at %d: %v vs %v", i, pendingIDs, want)
		}
	}
}

func TestResultsBatchAndListByPatient(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)
	other, _ := seedPatientAndUser(t, pool)
	jobs := postgres.NewJobs(pool)
	results := postgres.NewResults(pool)

	job, err := jobs.Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	otherJob, err := jobs.Create(c, postgres.NewJob{PatientID: other.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("other job: %v", err)
	}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	result := func(j uuid.UUID, hour int) postgres.NewResult {
		return postgres.NewResult{JobID: j, MeasurementType: domain.HeartRate, Window: "1h", WindowStart: start.Add(time.Duration(hour) * time.Hour),
			Statistics: json.RawMessage(`{"count":1}`), AlgorithmVersion: "1.0.0", ServiceVersion: "v"}
	}

	// A duplicate window in the batch stores nothing.
	if _, err := results.CreateBatch(c, []postgres.NewResult{result(job.ID, 0), result(job.ID, 1), result(job.ID, 0)}); !isKind(err, domain.KindConflict) {
		t.Fatalf("duplicate window: got %v, want conflict", err)
	}
	if n := countRows(t, pool, "processing_results", "job_id = $1", job.ID); n != 0 {
		t.Fatalf("%d results stored from a conflicting batch, want 0", n)
	}

	stored, err := results.CreateBatch(c, []postgres.NewResult{result(job.ID, 0), result(job.ID, 1), result(job.ID, 2)})
	if err != nil || len(stored) != 3 {
		t.Fatalf("batch = %d, %v", len(stored), err)
	}
	if _, err := results.Create(c, result(otherJob.ID, 0)); err != nil {
		t.Fatalf("other patient's result: %v", err)
	}

	var seen int
	var cursor *postgres.ResultCursor
	for {
		page, err := results.ListByPatient(c, patient.ID, cursor, 2)
		if err != nil {
			t.Fatalf("ListByPatient: %v", err)
		}
		for _, r := range page {
			if r.JobID != job.ID || r.PatientID != patient.ID {
				t.Fatalf("result of another patient listed: %+v", r)
			}
		}
		seen += len(page)
		if len(page) < 2 {
			break
		}
		last := postgres.CursorAfter(page[len(page)-1])
		cursor = &last
	}
	if seen != 3 {
		t.Fatalf("listed %d results for the patient, want 3", seen)
	}
}

// Creating a job and its audit entry is one unit of work: if the audit
// entry cannot be written the job must not exist either.
func TestTransactionComposesJobAndAudit(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)

	createWithAudit := func(action domain.AuditAction) (uuid.UUID, error) {
		var jobID uuid.UUID
		err := postgres.WithTx(c, pool, func(tx pgx.Tx) error {
			job, err := postgres.NewJobs(tx).Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
			if err != nil {
				return err
			}
			jobID = job.ID
			_, err = postgres.NewAudit(tx).Append(c, postgres.NewAuditEntry{
				ActorID: &user.ID, ActorType: domain.ActorUser, Action: action,
				ResourceType: "processing_job", ResourceID: &job.ID, RequestID: "req-tx",
			})
			return err
		})
		return jobID, err
	}

	jobID, err := createWithAudit(domain.AuditProcessingJobCreated)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if _, err := postgres.NewJobs(pool).GetByID(c, jobID); err != nil {
		t.Fatalf("job should exist: %v", err)
	}
	if entries, err := postgres.NewAudit(pool).ListByResource(c, jobID, 5); err != nil || len(entries) != 1 {
		t.Fatalf("audit entries = %d, %v; want 1", len(entries), err)
	}

	rolledBackID, err := createWithAudit("NOT_AN_ACTION")
	if !isKind(err, domain.KindValidation) {
		t.Fatalf("invalid audit action: got %v, want validation", err)
	}
	if _, err := postgres.NewJobs(pool).GetByID(c, rolledBackID); !isKind(err, domain.KindNotFound) {
		t.Fatalf("job should have been rolled back with the failed audit entry, got %v", err)
	}
}

// Concurrent replays of the same request must yield exactly one record.
func TestConcurrentIdempotencyBeginCreatesOneRecord(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	_, user := seedPatientAndUser(t, pool)
	idem := postgres.NewIdempotency(pool)
	key := postgres.NewIdempotencyKey{UserID: user.ID, Method: "POST", Path: "/api/v1/measurements", Key: "race", RequestFingerprint: "fp", ExpiresAt: time.Now().UTC().Add(time.Hour)}

	const workers = 8
	var wg sync.WaitGroup
	created := make(chan bool, workers)
	ids := make(chan uuid.UUID, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, wasCreated, err := idem.Begin(c, key)
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			created <- wasCreated
			ids <- rec.ID
		}()
	}
	wg.Wait()
	close(created)
	close(ids)

	var creators int
	for c := range created {
		if c {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d workers believed they created the record, want 1", creators)
	}
	first := <-ids
	for id := range ids {
		if id != first {
			t.Fatalf("workers saw different records: %s vs %s", first, id)
		}
	}
	if n := countRows(t, pool, "idempotency_keys", "user_id = $1", user.ID); n != 1 {
		t.Fatalf("%d records stored, want 1", n)
	}
}

func TestInvalidInputIsClassified(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)

	_, err := postgres.NewJobs(pool).Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: json.RawMessage(`not json`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindInvalid || domErr.Code != "INVALID_INPUT" {
		t.Fatalf("malformed JSON parameters: got %v, want INVALID_INPUT", err)
	}
}
