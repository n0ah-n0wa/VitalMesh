//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

func isKind(err error, kind domain.Kind) bool {
	var domErr *domain.Error
	return errors.As(err, &domErr) && domErr.Kind == kind
}

func TestUsersCRUD(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	users := postgres.NewUsers(pool)

	created, err := users.Create(c, "alice@example.com", "argon2id$hash", domain.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || created.Status != domain.UserActive || created.Role != domain.RoleUser {
		t.Fatalf("created = %+v", created)
	}

	got, err := users.GetByID(c, created.ID)
	if err != nil || got != created {
		t.Fatalf("GetByID = %+v, %v; want %+v", got, err, created)
	}
	if byEmail, err := users.GetByEmail(c, "alice@example.com"); err != nil || byEmail.ID != created.ID {
		t.Fatalf("GetByEmail = %+v, %v", byEmail, err)
	}

	time.Sleep(10 * time.Millisecond)
	promoted, err := users.UpdateRole(c, created.ID, domain.RoleAdmin)
	if err != nil || promoted.Role != domain.RoleAdmin {
		t.Fatalf("UpdateRole = %+v, %v", promoted, err)
	}
	if !promoted.UpdatedAt.After(created.UpdatedAt) {
		t.Error("updated_at was not advanced by the trigger")
	}

	if _, err := users.GetByID(c, uuid.New()); !isKind(err, domain.KindNotFound) {
		t.Fatalf("unknown id: got %v, want not found", err)
	}
	if _, err := users.UpdateRole(c, uuid.New(), domain.RoleUser); !isKind(err, domain.KindNotFound) {
		t.Fatalf("update unknown: got %v, want not found", err)
	}
}

func TestPatientsCRUDAndKeysetPagination(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patients := postgres.NewPatients(pool)
	dob := time.Date(1990, 2, 29, 0, 0, 0, 0, time.UTC)

	var ids []uuid.UUID
	for i := range 5 {
		p, err := patients.Create(c, "ref-"+string(rune('a'+i)), dob, domain.SexOther)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, p.ID)
	}

	first, err := patients.GetByID(c, ids[0])
	if err != nil || !first.DateOfBirth.Equal(dob) || first.Status != domain.PatientActive || first.DeletedAt != nil {
		t.Fatalf("GetByID = %+v, %v", first, err)
	}

	// Walk the collection two at a time; every patient appears exactly once.
	var seen []uuid.UUID
	var cursor *postgres.PatientCursor
	for {
		page, err := patients.List(c, cursor, 2)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, p := range page {
			seen = append(seen, p.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = &postgres.PatientCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d patients across pages, want 5: %v", len(seen), seen)
	}
	for i, id := range ids {
		if seen[i] != id {
			t.Fatalf("page order differs from creation order at %d", i)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	deleted, err := patients.SoftDelete(c, ids[2], now)
	if err != nil || deleted.Status != domain.PatientDeleted || deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(now) {
		t.Fatalf("SoftDelete = %+v, %v", deleted, err)
	}
	if _, err := patients.SoftDelete(c, ids[2], now); !isKind(err, domain.KindConflict) {
		t.Fatalf("second delete: got %v, want conflict", err)
	}
	if _, err := patients.SoftDelete(c, uuid.New(), now); !isKind(err, domain.KindNotFound) {
		t.Fatalf("delete unknown: got %v, want not found", err)
	}
	remaining, err := patients.List(c, nil, 10)
	if err != nil || len(remaining) != 4 {
		t.Fatalf("List after delete = %d, %v; want 4", len(remaining), err)
	}
}

func TestMeasurementsCRUDAndPagination(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, _ := seedPatientAndUser(t, pool)
	measurements := postgres.NewMeasurements(pool)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	created, err := measurements.Create(c, postgres.NewMeasurement{
		PatientID: patient.ID, Type: domain.BodyTemperature, Value: 36.6, Unit: "C",
		RecordedAt: base, Source: "thermometer", Metadata: json.RawMessage(`{"device":"t-1"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Value != 36.6 || string(created.Metadata) != `{"device": "t-1"}` || !created.RecordedAt.Equal(base) {
		t.Fatalf("created = %+v", created)
	}
	if got, err := measurements.GetByID(c, created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("GetByID = %+v, %v", got, err)
	}

	// Insert out of order; listing must come back in recording order.
	for _, offset := range []int{3, 1, 2} {
		if _, err := measurements.Create(c, postgres.NewMeasurement{
			PatientID: patient.ID, Type: domain.BodyTemperature, Value: 36.5, Unit: "C",
			RecordedAt: base.Add(time.Duration(offset) * time.Minute), Source: "thermometer",
		}); err != nil {
			t.Fatalf("Create offset %d: %v", offset, err)
		}
	}
	var all []domain.Measurement
	var cursor *postgres.MeasurementCursor
	for {
		page, err := measurements.ListByPatient(c, patient.ID, postgres.MeasurementFilter{}, cursor, 3)
		if err != nil {
			t.Fatalf("ListByPatient: %v", err)
		}
		all = append(all, page...)
		if len(page) < 3 {
			break
		}
		last := page[len(page)-1]
		cursor = &postgres.MeasurementCursor{RecordedAt: last.RecordedAt, ID: last.ID}
	}
	if len(all) != 4 {
		t.Fatalf("listed %d measurements, want 4", len(all))
	}
	for i := 1; i < len(all); i++ {
		if !all[i].RecordedAt.After(all[i-1].RecordedAt) {
			t.Fatalf("not in recording order at %d", i)
		}
	}

	// Validation failures surface as domain validation errors with the constraint as code.
	_, err = measurements.Create(c, postgres.NewMeasurement{PatientID: patient.ID, Type: domain.SpO2, Value: 101, Unit: "%", RecordedAt: base, Source: "s"})
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindValidation || domErr.Code != "MEASUREMENTS_VALUE_RANGE_CHECK" {
		t.Fatalf("out of range: got %v", err)
	}
	if _, err := measurements.Create(c, postgres.NewMeasurement{PatientID: uuid.New(), Type: domain.SpO2, Value: 98, Unit: "%", RecordedAt: base, Source: "s"}); !isKind(err, domain.KindValidation) {
		t.Fatalf("unknown patient: got %v, want validation", err)
	}

	if err := measurements.Delete(c, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := measurements.Delete(c, created.ID); !isKind(err, domain.KindNotFound) {
		t.Fatalf("second delete: got %v, want not found", err)
	}
}

func TestJobsLifecycle(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)
	jobs := postgres.NewJobs(pool)
	requestID := "req-42"

	job, err := jobs.Create(c, postgres.NewJob{
		PatientID: patient.ID, Parameters: json.RawMessage(`{"windows":["1h"]}`),
		AlgorithmVersion: "1.0.0", CreatedBy: user.ID, RequestID: &requestID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if job.Status != domain.JobPending || job.AttemptCount != 0 || job.StartedAt != nil || *job.RequestID != requestID {
		t.Fatalf("created = %+v", job)
	}

	t0 := job.RequestedAt.Add(time.Second)
	if _, err := jobs.Complete(c, job.ID, t0); !isKind(err, domain.KindConflict) {
		t.Fatalf("complete a PENDING job: got %v, want conflict", err)
	}

	started, err := jobs.Start(c, job.ID, t0, t0.Add(time.Minute), "svc-1")
	if err != nil || started.Status != domain.JobProcessing || started.AttemptCount != 1 || started.LeaseExpiresAt == nil {
		t.Fatalf("Start = %+v, %v", started, err)
	}
	if _, err := jobs.Start(c, job.ID, t0, t0.Add(time.Minute), "svc-1"); !isKind(err, domain.KindConflict) {
		t.Fatalf("second start: got %v, want conflict", err)
	}

	done, err := jobs.Complete(c, job.ID, t0.Add(2*time.Second))
	if err != nil || done.Status != domain.JobCompleted || done.CompletedAt == nil || done.LeaseExpiresAt != nil {
		t.Fatalf("Complete = %+v, %v", done, err)
	}
	if _, err := jobs.Cancel(c, job.ID, t0.Add(3*time.Second)); !isKind(err, domain.KindConflict) {
		t.Fatalf("cancel a COMPLETED job: got %v, want conflict", err)
	}

	// Failure keeps the diagnostic metadata.
	failing, err := jobs.Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("Create failing: %v", err)
	}
	if _, err := jobs.Start(c, failing.ID, t0, t0.Add(time.Minute), "svc-1"); err != nil {
		t.Fatalf("Start failing: %v", err)
	}
	failed, err := jobs.Fail(c, failing.ID, t0.Add(time.Second), "PROCESSING_TIMEOUT", "The work exceeded its time limit.")
	if err != nil || failed.Status != domain.JobFailed || failed.ErrorCode == nil || *failed.ErrorCode != "PROCESSING_TIMEOUT" {
		t.Fatalf("Fail = %+v, %v", failed, err)
	}

	// Cancellation from PENDING.
	pending, err := jobs.Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("Create pending: %v", err)
	}
	cancelled, err := jobs.Cancel(c, pending.ID, pending.RequestedAt)
	if err != nil || cancelled.Status != domain.JobCancelled || cancelled.CancelledAt == nil {
		t.Fatalf("Cancel = %+v, %v", cancelled, err)
	}

	if _, err := jobs.GetByID(c, uuid.New()); !isKind(err, domain.KindNotFound) {
		t.Fatalf("unknown job: got %v", err)
	}
	if _, err := jobs.Start(c, uuid.New(), t0, t0, "svc"); !isKind(err, domain.KindNotFound) {
		t.Fatalf("start unknown job: got %v, want not found", err)
	}
}

func TestResultsCreateAndList(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)

	job, err := postgres.NewJobs(pool).Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: []byte(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	results := postgres.NewResults(pool)
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	input := postgres.NewResult{
		JobID: job.ID, MeasurementType: domain.HeartRate, Window: "1h", WindowStart: start,
		Statistics: json.RawMessage(`{"count":10,"mean":72.5}`), AlgorithmVersion: "1.0.0", ServiceVersion: "abc123",
	}
	created, err := results.Create(c, input)
	if err != nil || string(created.Anomalies) != "[]" || !created.WindowStart.Equal(start) {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	if created.PatientID != patient.ID {
		t.Fatalf("PatientID = %s, want the job's patient %s (set by trigger)", created.PatientID, patient.ID)
	}
	if _, err := results.Create(c, input); !isKind(err, domain.KindConflict) {
		t.Fatalf("duplicate window: got %v, want conflict", err)
	}
	input.WindowStart = start.Add(time.Hour)
	input.Anomalies = json.RawMessage(`[{"severity":"WARNING"}]`)
	if _, err := results.Create(c, input); err != nil {
		t.Fatalf("second window: %v", err)
	}

	list, err := results.ListByJob(c, job.ID, nil, 10)
	if err != nil || len(list) != 2 || !list[0].WindowStart.Equal(start) || !list[1].WindowStart.Equal(start.Add(time.Hour)) {
		t.Fatalf("ListByJob = %+v, %v", list, err)
	}
	cursor := postgres.CursorAfter(list[0])
	rest, err := results.ListByJob(c, job.ID, &cursor, 10)
	if err != nil || len(rest) != 1 || rest[0].ID != list[1].ID {
		t.Fatalf("ListByJob after cursor = %+v, %v", rest, err)
	}
	if string(list[1].Anomalies) != `[{"severity": "WARNING"}]` {
		t.Errorf("anomalies round trip = %s", list[1].Anomalies)
	}
}

func TestAuditAppendAndList(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)
	audit := postgres.NewAudit(pool)

	for i := range 3 {
		if _, err := audit.Append(c, postgres.NewAuditEntry{
			ActorID: &user.ID, ActorType: domain.ActorUser, Action: domain.AuditPatientCreated,
			ResourceType: "patient", ResourceID: &patient.ID, RequestID: "req-" + string(rune('0'+i)),
			Metadata: json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	entries, err := audit.ListByResource(c, patient.ID, 2)
	if err != nil || len(entries) != 2 || entries[0].RequestID != "req-2" || entries[0].ActorID == nil || *entries[0].ActorID != user.ID {
		t.Fatalf("ListByResource = %+v, %v", entries, err)
	}

	if _, err := audit.Append(c, postgres.NewAuditEntry{ActorType: domain.ActorUser, Action: domain.AuditLogin, ResourceType: "user", RequestID: "r"}); !isKind(err, domain.KindValidation) {
		t.Fatalf("user actor without id: got %v, want validation", err)
	}
	if _, err := audit.Append(c, postgres.NewAuditEntry{ActorType: domain.ActorSystem, Action: domain.AuditRetentionRun, ResourceType: "retention", RequestID: "r"}); err != nil {
		t.Fatalf("system actor: %v", err)
	}
}

func TestIdempotencyBeginReplayCompleteExpire(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	_, user := seedPatientAndUser(t, pool)
	idem := postgres.NewIdempotency(pool)
	key := postgres.NewIdempotencyKey{UserID: user.ID, Method: "POST", Path: "/api/v1/patients", Key: "k-1", RequestFingerprint: "fp-1", ExpiresAt: time.Now().UTC().Add(time.Hour)}

	first, created, err := idem.Begin(c, key)
	if err != nil || !created || first.Status != domain.IdempotencyInProgress {
		t.Fatalf("Begin = %+v, %v, %v", first, created, err)
	}
	replay, created, err := idem.Begin(c, key)
	if err != nil || created || replay.ID != first.ID {
		t.Fatalf("replay Begin = %+v, %v, %v", replay, created, err)
	}

	completed, err := idem.Complete(c, first.ID, 201, json.RawMessage(`{"id":"x"}`))
	if err != nil || completed.Status != domain.IdempotencyCompleted || *completed.ResponseStatus != 201 {
		t.Fatalf("Complete = %+v, %v", completed, err)
	}
	if _, err := idem.Complete(c, first.ID, 201, nil); !isKind(err, domain.KindNotFound) {
		t.Fatalf("completing twice: got %v, want not found", err)
	}
	got, err := idem.Get(c, user.ID, "POST", "/api/v1/patients", "k-1")
	if err != nil || string(got.ResponseBody) != `{"id": "x"}` {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	// Same key on another path is independent.
	other := key
	other.Path = "/api/v1/measurements"
	if _, created, err := idem.Begin(c, other); err != nil || !created {
		t.Fatalf("other path: created=%v err=%v", created, err)
	}

	n, err := idem.DeleteExpired(c, time.Now().UTC(), 100)
	if err != nil || n != 0 {
		t.Fatalf("DeleteExpired now = %d, %v", n, err)
	}
	later := time.Now().UTC().Add(2 * time.Hour)
	if n, err = idem.DeleteExpired(c, later, 1); err != nil || n != 1 {
		t.Fatalf("DeleteExpired limit 1 = %d, %v; want 1", n, err)
	}
	if n, err = idem.DeleteExpired(c, later, 100); err != nil || n != 1 {
		t.Fatalf("DeleteExpired remainder = %d, %v; want 1", n, err)
	}
	if n, err = idem.DeleteExpired(c, later, 100); err != nil || n != 0 {
		t.Fatalf("DeleteExpired when empty = %d, %v; want 0", n, err)
	}
}
