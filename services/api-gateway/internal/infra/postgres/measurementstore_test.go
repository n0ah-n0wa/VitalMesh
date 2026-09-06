//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
)

func newReading(patient uuid.UUID, minute int) measurement.NewMeasurement {
	return measurement.NewMeasurement{
		PatientID: patient, Type: domain.HeartRate, Value: 70, Unit: "bpm",
		RecordedAt: time.Date(2026, 9, 6, 11, minute, 0, 0, time.UTC), Source: "store-test", Metadata: json.RawMessage(`{"n":1}`),
	}
}

func TestMeasurementStoreBatchIsAtomicAndNamesTheItem(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	patient, user := seedPatientAndUser(t, pool)
	store := postgres.NewMeasurementStore(pool)
	audit := postgres.NewAudit(pool)
	event := measurement.AuditEvent{ActorID: user.ID, Action: domain.AuditMeasurementCreated, RequestID: "req-batch"}

	first, err := store.Create(c, newReading(patient.ID, 0), event)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	entries, _ := audit.ListByResource(c, first.ID, 5)
	if len(entries) != 1 || entries[0].Action != domain.AuditMeasurementCreated || entries[0].RequestID != "req-batch" {
		t.Fatalf("audit after create = %+v", entries)
	}

	// Item 2 duplicates the stored reading: nothing from the batch is kept.
	batch := []measurement.NewMeasurement{newReading(patient.ID, 1), newReading(patient.ID, 2), newReading(patient.ID, 0), newReading(patient.ID, 3)}
	_, err = store.CreateBatch(c, batch, event)
	var item *measurement.ItemError
	if !errors.As(err, &item) || item.Index != 2 || !isKind(err, domain.KindConflict) {
		t.Fatalf("batch with duplicate: err = %v, want ItemError{Index: 2} wrapping a conflict", err)
	}
	all, err := postgres.NewMeasurements(pool).ListByPatient(c, patient.ID, postgres.MeasurementFilter{}, nil, 100)
	if err != nil || len(all) != 1 {
		t.Fatalf("after failed batch: %d readings, %v; want only the first", len(all), err)
	}

	// A reading the trigger rejects (unit mismatch) is reported by index too,
	// and the whole batch rolls back.
	badUnit := newReading(patient.ID, 4)
	badUnit.Unit = "BPM"
	_, err = store.CreateBatch(c, []measurement.NewMeasurement{newReading(patient.ID, 5), badUnit}, event)
	if !errors.As(err, &item) || item.Index != 1 || !isKind(err, domain.KindValidation) {
		t.Fatalf("batch with bad unit: err = %v", err)
	}

	created, err := store.CreateBatch(c, []measurement.NewMeasurement{newReading(patient.ID, 6), newReading(patient.ID, 7)}, event)
	if err != nil || len(created) != 2 || created[0].RecordedAt.Minute() != 6 {
		t.Fatalf("good batch = %+v, %v", created, err)
	}
	for _, m := range created {
		entries, _ := audit.ListByResource(c, m.ID, 5)
		if len(entries) != 1 || entries[0].ResourceType != measurement.ResourceType {
			t.Errorf("audit for %s = %+v", m.ID, entries)
		}
	}

	if err := store.Delete(c, first.ID, measurement.AuditEvent{ActorID: user.ID, Action: domain.AuditMeasurementDeleted, RequestID: "req-del"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entries, _ = audit.ListByResource(c, first.ID, 5)
	if len(entries) != 2 || entries[0].Action != domain.AuditMeasurementDeleted || entries[0].RequestID != "req-del" {
		t.Errorf("audit after delete = %+v", entries)
	}
	if _, err := store.GetByID(c, first.ID); !isKind(err, domain.KindNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	if err := store.Delete(c, first.ID, event); !isKind(err, domain.KindNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if status, err := store.PatientStatus(c, patient.ID); err != nil || status != domain.PatientActive {
		t.Errorf("PatientStatus = %s, %v", status, err)
	}
	if _, err := store.PatientStatus(c, uuid.New()); !isKind(err, domain.KindNotFound) {
		t.Errorf("unknown patient status: %v", err)
	}
}

// The Go catalogue and the measurement_types table must agree, so that a
// reading the service accepts is one the database accepts and vice versa.
func TestCatalogMatchesMeasurementTypesTable(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	rows, err := pool.Query(ctx(t), `SELECT code, canonical_unit, min_value, max_value FROM measurement_types ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	table := map[domain.MeasurementType]measurement.Type{}
	for rows.Next() {
		var typ measurement.Type
		if err := rows.Scan(&typ.Code, &typ.Unit, &typ.Min, &typ.Max); err != nil {
			t.Fatal(err)
		}
		table[typ.Code] = typ
	}
	if len(table) != len(measurement.Catalog) {
		t.Fatalf("table has %d types, catalogue %d", len(table), len(measurement.Catalog))
	}
	for _, typ := range measurement.Catalog {
		if got := table[typ.Code]; got != typ {
			t.Errorf("%s: table %+v, catalogue %+v", typ.Code, got, typ)
		}
	}
}

func TestIdempotencyStoreRoundTrip(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	_, user := seedPatientAndUser(t, pool)
	store := postgres.NewIdempotencyStore(pool)
	req := idempotency.Request{UserID: user.ID, Method: "POST", Path: "/api/v1/measurements", Key: "k-1", Fingerprint: "fp", ExpiresAt: time.Now().Add(time.Hour)}

	rec, created, err := store.Begin(c, req)
	if err != nil || !created || rec.Status != domain.IdempotencyInProgress {
		t.Fatalf("Begin = %+v, %v, %v", rec, created, err)
	}
	again, created, err := store.Begin(c, req)
	if err != nil || created || again.ID != rec.ID {
		t.Fatalf("Begin again = %+v, %v, %v", again, created, err)
	}
	if err := store.Complete(c, rec.ID, 201, json.RawMessage(`{"id":"x"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	done, _, _ := store.Begin(c, req)
	if done.Status != domain.IdempotencyCompleted || *done.ResponseStatus != 201 || string(done.ResponseBody) != `{"id": "x"}` && string(done.ResponseBody) != `{"id":"x"}` {
		t.Errorf("completed = %+v", done)
	}
	if err := store.Delete(c, rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, created, _ := store.Begin(c, req); !created {
		t.Error("key not reusable after Delete")
	}
}
