//go:build integration

package postgres_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

// storeReading inserts one reading at the given recording time, using the
// package's own builder so this file does not restate the shape.
func storeReading(t *testing.T, pool *pgxpool.Pool, patientID uuid.UUID, recordedAt time.Time) domain.Measurement {
	t.Helper()
	m, err := postgres.NewMeasurements(pool).Create(ctx(t), reading(patientID, domain.HeartRate, "bpm", 72, recordedAt))
	if err != nil {
		t.Fatalf("store reading: %v", err)
	}
	return m
}

// The window decides what goes: a reading recorded before the cutoff is
// removed and one recorded after it is kept, to the row.
func TestRetentionRemovesOnlyWhatIsPastTheCutoff(t *testing.T) {
	pool, _, _ := postgrestest.New(t)
	patient, _ := seedPatientAndUser(t, pool)
	now := time.Now().UTC()

	old := storeReading(t, pool, patient.ID, now.AddDate(0, 0, -40))
	edge := storeReading(t, pool, patient.ID, now.AddDate(0, 0, -30).Add(time.Minute))
	recent := storeReading(t, pool, patient.ID, now.AddDate(0, 0, -1))

	cutoff := now.AddDate(0, 0, -30)
	removed, err := postgres.NewRetention(pool).DeleteMeasurementsBefore(ctx(t), cutoff, 100)
	if err != nil {
		t.Fatalf("DeleteMeasurementsBefore: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d readings, want only the one past the cutoff", removed)
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want int
	}{
		{"the reading past the window", old.ID, 0},
		{"the reading just inside it", edge.ID, 1},
		{"the recent reading", recent.ID, 1},
	} {
		if got := countRows(t, pool, "measurements", "id = $1", tc.id); got != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.name, got, tc.want)
		}
	}
}

// Section 81 asks for removal "without breaking referential integrity".
// Readings are the one thing nothing holds a key to, so removing them must
// leave the job that processed them, and that job's results, untouched and
// readable.
func TestRemovingReadingsLeavesTheJobAndItsResultsIntact(t *testing.T) {
	pool, _, _ := postgrestest.New(t)
	patient, user := seedPatientAndUser(t, pool)
	now := time.Now().UTC()

	stored := storeReading(t, pool, patient.ID, now.AddDate(0, 0, -90))

	job, err := postgres.NewJobs(pool).Create(ctx(t), postgres.NewJob{
		PatientID:        patient.ID,
		Parameters:       json.RawMessage(`{"measurement_types":["HEART_RATE"],"windows":["1h"]}`),
		AlgorithmVersion: "1.0.0",
		CreatedBy:        user.ID,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	result, err := postgres.NewResults(pool).Create(ctx(t), postgres.NewResult{
		JobID:            job.ID,
		MeasurementType:  domain.HeartRate,
		Window:           "1h",
		WindowStart:      now.AddDate(0, 0, -90).Truncate(time.Hour),
		Statistics:       json.RawMessage(`{"count":1,"min":72,"max":72,"mean":72,"median":72,"variance":0,"std_dev":0}`),
		Anomalies:        json.RawMessage(`[]`),
		AlgorithmVersion: "1.0.0",
		ServiceVersion:   "test",
	})
	if err != nil {
		t.Fatalf("create result: %v", err)
	}

	// Remove the readings the result was computed from.
	if _, err := postgres.NewRetention(pool).DeleteMeasurementsBefore(ctx(t), now, 100); err != nil {
		t.Fatalf("DeleteMeasurementsBefore: %v", err)
	}
	if got := countRows(t, pool, "measurements", "id = $1", stored.ID); got != 0 {
		t.Fatalf("the reading was not removed")
	}

	// The job and its result survive, and are still readable.
	if _, err := postgres.NewJobs(pool).GetByID(ctx(t), job.ID); err != nil {
		t.Errorf("the job is unreadable after its readings were removed: %v", err)
	}
	if got := countRows(t, pool, "processing_results", "id = $1", result.ID); got != 1 {
		t.Errorf("the result did not survive the removal of its readings")
	}
}

// Results have their own window, and removing one must leave the job that
// produced it: section 94 requires a job to keep its record.
func TestRemovingResultsLeavesTheJob(t *testing.T) {
	pool, _, _ := postgrestest.New(t)
	patient, user := seedPatientAndUser(t, pool)
	now := time.Now().UTC()

	job, err := postgres.NewJobs(pool).Create(ctx(t), postgres.NewJob{
		PatientID:        patient.ID,
		Parameters:       json.RawMessage(`{"measurement_types":["HEART_RATE"],"windows":["1h"]}`),
		AlgorithmVersion: "1.0.0",
		CreatedBy:        user.ID,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := postgres.NewResults(pool).Create(ctx(t), postgres.NewResult{
		JobID:            job.ID,
		MeasurementType:  domain.HeartRate,
		Window:           "1h",
		WindowStart:      now.Truncate(time.Hour),
		Statistics:       json.RawMessage(`{"count":1,"min":72,"max":72,"mean":72,"median":72,"variance":0,"std_dev":0}`),
		Anomalies:        json.RawMessage(`[]`),
		AlgorithmVersion: "1.0.0",
		ServiceVersion:   "test",
	}); err != nil {
		t.Fatalf("create result: %v", err)
	}

	// Everything created in this schema is newer than now+time, so a
	// cutoff in the future removes it.
	removed, err := postgres.NewRetention(pool).DeleteResultsBefore(ctx(t), now.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("DeleteResultsBefore: %v", err)
	}
	if removed < 1 {
		t.Fatalf("removed %d results, want at least the one created here", removed)
	}
	if got := countRows(t, pool, "processing_results", "job_id = $1", job.ID); got != 0 {
		t.Error("the result was not removed")
	}
	if _, err := postgres.NewJobs(pool).GetByID(ctx(t), job.ID); err != nil {
		t.Errorf("the job did not survive the removal of its results: %v", err)
	}
}

// The limit is what bounds the locks a sweep takes, so it has to be
// respected rather than treated as a hint.
func TestRetentionRespectsTheBatchLimit(t *testing.T) {
	pool, _, _ := postgrestest.New(t)
	patient, _ := seedPatientAndUser(t, pool)
	now := time.Now().UTC()

	const total = 7
	for i := range total {
		storeReading(t, pool, patient.ID, now.AddDate(0, 0, -100).Add(time.Duration(i)*time.Second))
	}

	r := postgres.NewRetention(pool)
	removed, err := r.DeleteMeasurementsBefore(ctx(t), now, 3)
	if err != nil {
		t.Fatalf("DeleteMeasurementsBefore: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed %d readings, want the batch limit of 3", removed)
	}
	if got := countRows(t, pool, "measurements", "patient_id = $1", patient.ID); got != total-3 {
		t.Errorf("%d readings left, want %d", got, total-3)
	}

	// Repeating drains the rest, which is what the sweeper does.
	for {
		n, err := r.DeleteMeasurementsBefore(ctx(t), now, 3)
		if err != nil {
			t.Fatalf("DeleteMeasurementsBefore: %v", err)
		}
		if n < 3 {
			break
		}
	}
	if got := countRows(t, pool, "measurements", "patient_id = $1", patient.ID); got != 0 {
		t.Errorf("%d readings left after draining", got)
	}
}

// Nothing to remove must be cheap and silent rather than an error.
func TestRetentionOnAnEmptyWindowRemovesNothing(t *testing.T) {
	pool, _, _ := postgrestest.New(t)
	patient, _ := seedPatientAndUser(t, pool)
	storeReading(t, pool, patient.ID, time.Now().UTC())

	r := postgres.NewRetention(pool)
	cutoff := time.Now().UTC().AddDate(-10, 0, 0)
	for _, tc := range []struct {
		name string
		del  func() (int64, error)
	}{
		{"measurements", func() (int64, error) { return r.DeleteMeasurementsBefore(ctx(t), cutoff, 100) }},
		{"results", func() (int64, error) { return r.DeleteResultsBefore(ctx(t), cutoff, 100) }},
	} {
		removed, err := tc.del()
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if removed != 0 {
			t.Errorf("%s: removed %d rows, want none", tc.name, removed)
		}
	}
}
