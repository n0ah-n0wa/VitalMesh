//go:build integration

package postgres_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

// The batch insert sends the entries as parallel arrays; the optional
// columns must arrive as NULL, not as empty strings, and every entry must
// come back with what was sent.
func TestAuditAppendBatchKeepsOptionalColumnsNullable(t *testing.T) {
	t.Parallel()
	pool, _, _ := postgrestest.New(t)
	c := ctx(t)
	_, user := seedPatientAndUser(t, pool)
	audit := postgres.NewAudit(pool)
	resource := uuid.New()

	stored, err := audit.AppendBatch(c, []postgres.NewAuditEntry{
		{ActorType: domain.ActorSystem, Action: domain.AuditRetentionRun, ResourceType: "measurement", RequestID: "sys-1"},
		{ActorID: &user.ID, ActorType: domain.ActorUser, Action: domain.AuditMeasurementCreated, ResourceType: "measurement",
			ResourceID: &resource, RequestID: "req-1", Metadata: json.RawMessage(`{"type":"HEART_RATE"}`)},
	})
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d entries, want 2", len(stored))
	}
	var system, byUser domain.AuditEntry
	for _, e := range stored {
		if e.ActorType == domain.ActorSystem {
			system = e
		} else {
			byUser = e
		}
	}
	if system.ActorID != nil || system.ResourceID != nil || system.Action != domain.AuditRetentionRun || string(system.Metadata) != "{}" {
		t.Errorf("system entry = %+v, want no actor, no resource, empty metadata", system)
	}
	if byUser.ActorID == nil || *byUser.ActorID != user.ID || byUser.ResourceID == nil || *byUser.ResourceID != resource || byUser.RequestID != "req-1" {
		t.Errorf("user entry = %+v", byUser)
	}
	var meta map[string]string
	if err := json.Unmarshal(byUser.Metadata, &meta); err != nil || meta["type"] != "HEART_RATE" {
		t.Errorf("metadata = %s", byUser.Metadata)
	}

	entries, err := audit.ListByResource(c, resource, 10)
	if err != nil || len(entries) != 1 || entries[0].ID != byUser.ID {
		t.Errorf("ListByResource = %+v, %v", entries, err)
	}
	if empty, err := audit.AppendBatch(c, nil); err != nil || len(empty) != 0 {
		t.Errorf("empty batch = %v, %v", empty, err)
	}
}
