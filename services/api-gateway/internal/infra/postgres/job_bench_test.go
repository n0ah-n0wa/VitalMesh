//go:build integration

package postgres_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

// The two database sides of a large job, measured against the real
// database: reading 60,000 readings for the processor and writing the
// 1,200 results it returns (docs/PERFORMANCE_OPTIMIZATIONS.md).
//
//	go test -tags integration -run xxx -bench Job -benchmem ./internal/infra/postgres/
func BenchmarkReadingsForJob60k(b *testing.B) {
	pool, _, _ := postgrestest.New(b)
	c := b.Context()
	patient, _ := seedPatientAndUser(b, pool)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := postgres.NewMeasurements(pool)
	for chunk := 0; chunk < 60; chunk++ {
		items := make([]postgres.NewMeasurement, 1000)
		for i := range items {
			items[i] = reading(patient.ID, domain.HeartRate, "bpm", 60+float64(i%40), base.Add(time.Duration(chunk*1000+i)*time.Second))
		}
		if _, err := repo.CreateBatch(c, items); err != nil {
			b.Fatal(err)
		}
	}
	store := postgres.NewJobStore(pool)
	from, to := base, base.Add(60000*time.Second)
	params := processing.Parameters{MeasurementTypes: []domain.MeasurementType{domain.HeartRate}, From: &from, To: &to}
	b.ResetTimer()
	for b.Loop() {
		got, err := store.ReadingsForJob(c, patient.ID, params, 100_001)
		if err != nil || len(got) != 60000 {
			b.Fatalf("%d readings, %v", len(got), err)
		}
	}
}

func BenchmarkResultsCreateBatch1200(b *testing.B) {
	pool, _, _ := postgrestest.New(b)
	c := b.Context()
	patient, user := seedPatientAndUser(b, pool)
	jobs := postgres.NewJobs(pool)
	results := postgres.NewResults(pool)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stats := json.RawMessage(`{"count":60,"mean":72.5,"min":60,"max":99,"median":72,"std_dev":11.2,"variance":125.4,"percentiles":{"50":72,"95":97}}`)
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		job, err := jobs.Create(c, postgres.NewJob{PatientID: patient.ID, Parameters: json.RawMessage(`{}`), AlgorithmVersion: "1.0.0", CreatedBy: user.ID})
		if err != nil {
			b.Fatal(err)
		}
		rows := make([]postgres.NewResult, 1200)
		for i := range rows {
			rows[i] = postgres.NewResult{JobID: job.ID, MeasurementType: domain.HeartRate, Window: "1m", WindowStart: start.Add(time.Duration(i) * time.Minute),
				Statistics: stats, Anomalies: json.RawMessage(`[]`), AlgorithmVersion: "1.0.0", ServiceVersion: "bench"}
		}
		b.StartTimer()
		if _, err := results.CreateBatch(c, rows); err != nil {
			b.Fatal(err)
		}
	}
	_ = uuid.Nil
}
