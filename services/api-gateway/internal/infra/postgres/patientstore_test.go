//go:build integration

package postgres_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
)

func TestPatientStoreWritesStateAndAuditAtomically(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	_, user := seedPatientAndUser(t, pool)
	store := postgres.NewPatientStore(pool)
	audit := postgres.NewAudit(pool)

	in := patient.NewPatient{ExternalReference: "store-" + uuid.NewString(), DateOfBirth: time.Date(1975, 5, 5, 0, 0, 0, 0, time.UTC), Sex: domain.SexMale}

	// An audit event that violates the schema (empty request id) must
	// roll the patient back with it.
	_, err := store.Create(c, in, patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientCreated, RequestID: ""})
	if !isKind(err, domain.KindValidation) {
		t.Fatalf("create with invalid audit: err = %v, want validation failure", err)
	}
	for _, p := range mustList(t, pool) {
		if p.ExternalReference == in.ExternalReference {
			t.Fatal("patient stored although its audit event was rejected")
		}
	}

	created, err := store.Create(c, in, patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientCreated, RequestID: "req-create"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	entries, err := audit.ListByResource(c, created.ID, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != domain.AuditPatientCreated || entries[0].RequestID != "req-create" || *entries[0].ActorID != user.ID {
		t.Fatalf("audit after create = %+v, %v", entries, err)
	}

	// Same reference again: a conflict the service maps to PATIENT_ALREADY_EXISTS.
	_, err = store.Create(c, in, patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientCreated, RequestID: "req-dup"})
	if !isKind(err, domain.KindConflict) {
		t.Errorf("duplicate reference: err = %v, want conflict", err)
	}

	got, err := store.GetByID(c, created.ID)
	if err != nil || got != created {
		t.Errorf("GetByID = %+v, %v", got, err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	deleted, err := store.SoftDelete(c, created.ID, now, patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientDeleted, RequestID: "req-delete"})
	if err != nil || deleted.Status != domain.PatientDeleted || deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(now) {
		t.Fatalf("SoftDelete = %+v, %v", deleted, err)
	}
	entries, _ = audit.ListByResource(c, created.ID, 10)
	if len(entries) != 2 || entries[0].Action != domain.AuditPatientDeleted || entries[0].RequestID != "req-delete" {
		t.Errorf("audit after delete = %+v", entries)
	}

	// The deleted patient stays readable by id but leaves the list.
	if got, err := store.GetByID(c, created.ID); err != nil || got.Status != domain.PatientDeleted {
		t.Errorf("GetByID after delete = %+v, %v", got, err)
	}
	for _, p := range mustList(t, pool) {
		if p.ID == created.ID {
			t.Error("deleted patient listed")
		}
	}

	_, err = store.SoftDelete(c, created.ID, now, patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientDeleted, RequestID: "req-again"})
	if !isKind(err, domain.KindConflict) {
		t.Errorf("second delete: err = %v, want conflict", err)
	}
	if entries, _ = audit.ListByResource(c, created.ID, 10); len(entries) != 2 {
		t.Errorf("failed delete was audited: %+v", entries)
	}
	_, err = store.SoftDelete(c, uuid.New(), now, patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientDeleted, RequestID: "req-none"})
	if !isKind(err, domain.KindNotFound) {
		t.Errorf("unknown id: err = %v, want not found", err)
	}
}

func TestPatientStoreListFollowsCursor(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	_, user := seedPatientAndUser(t, pool)
	store := postgres.NewPatientStore(pool)

	for i := range 4 {
		_, err := store.Create(c, patient.NewPatient{ExternalReference: "cursor-" + uuid.NewString(), DateOfBirth: time.Date(1990, 1, 1+i, 0, 0, 0, 0, time.UTC), Sex: domain.SexUnknown},
			patient.AuditEvent{ActorID: user.ID, Action: domain.AuditPatientCreated, RequestID: "seed"})
		if err != nil {
			t.Fatal(err)
		}
	}
	all := mustList(t, pool)
	if len(all) < 5 {
		t.Fatalf("expected the seed patient plus four, got %d", len(all))
	}
	first, err := store.List(c, nil, 2)
	if err != nil || len(first) != 2 || first[0].ID != all[0].ID {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	rest, err := store.List(c, &patient.Cursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID}, 10)
	if err != nil || len(rest) != len(all)-2 || rest[0].ID != all[2].ID {
		t.Fatalf("after cursor = %d items, %v", len(rest), err)
	}
}

func mustList(t *testing.T, pool *pgxpool.Pool) []domain.Patient {
	t.Helper()
	items, err := postgres.NewPatients(pool).List(ctx(t), nil, 1000)
	if err != nil {
		t.Fatalf("list patients: %v", err)
	}
	return items
}
