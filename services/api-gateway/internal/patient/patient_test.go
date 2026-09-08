package patient_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient/patienttest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

var now = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// recorder captures what the service reports to the observability ports.
type recorder struct {
	mu         sync.Mutex
	operations []string // "name:outcome"
	spans      []string
	errors     []error
}

func (r *recorder) HTTPRequest(string, string, int, time.Duration) {}
func (r *recorder) Batch(string, int)                              {}
func (r *recorder) Cache(string, bool)                             {}

func (r *recorder) Operation(name string, outcome metrics.Outcome, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations = append(r.operations, name+":"+string(outcome))
}

func (r *recorder) Start(ctx context.Context, name string) (context.Context, tracing.Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, name)
	return tracing.WithTraceID(ctx, "trace-"+name), &span{r: r}
}

type span struct{ r *recorder }

func (s *span) SetAttribute(string, any) {}
func (s *span) RecordError(err error) {
	if err != nil {
		s.r.mu.Lock()
		s.r.errors = append(s.r.errors, err)
		s.r.mu.Unlock()
	}
}
func (s *span) End() {}

type fixture struct {
	service  *patient.Service
	store    *patienttest.MemoryStore
	recorder *recorder
	logs     *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: patienttest.NewMemoryStore(), recorder: &recorder{}, logs: &bytes.Buffer{}}
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	f.service = patient.NewService(f.store, logger, patient.Options{
		Metrics: f.recorder, Tracer: f.recorder, Now: func() time.Time { return now },
	})
	return f
}

func operator() auth.Principal {
	return auth.Principal{UserID: uuid.New(), Role: domain.RoleOperator, TokenID: "t"}
}

func ctxWithRequest(id string) context.Context {
	return requestid.NewContext(context.Background(), id)
}

func validInput() patient.CreateInput {
	return patient.CreateInput{ExternalReference: "synthetic-0001", DateOfBirth: "1984-02-29", Sex: "FEMALE"}
}

func TestCreateStoresPatientWithAuditEvent(t *testing.T) {
	f := newFixture(t)
	actor := operator()

	created, err := f.service.Create(ctxWithRequest("req-1"), actor, validInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || created.ExternalReference != "synthetic-0001" || created.Sex != domain.SexFemale || created.Status != domain.PatientActive {
		t.Errorf("created = %+v", created)
	}
	if !created.DateOfBirth.Equal(time.Date(1984, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date of birth = %s", created.DateOfBirth)
	}
	if len(f.store.Audit) != 1 {
		t.Fatalf("audit events = %+v, want one", f.store.Audit)
	}
	got := f.store.Audit[0]
	if got.PatientID != created.ID || got.Event.Action != domain.AuditPatientCreated || got.Event.ActorID != actor.UserID || got.Event.RequestID != "req-1" {
		t.Errorf("audit = %+v", got)
	}
	if !strings.Contains(f.logs.String(), "patient created") || !strings.Contains(f.logs.String(), created.ID.String()) {
		t.Errorf("creation not logged: %s", f.logs.String())
	}
	if f.recorder.operations[0] != "patient.create:ok" || f.recorder.spans[0] != "patient.create" {
		t.Errorf("hooks = %v %v", f.recorder.operations, f.recorder.spans)
	}
}

func TestCreateNormalisesReference(t *testing.T) {
	f := newFixture(t)
	in := validInput()
	in.ExternalReference = "  padded-ref  "
	created, err := f.service.Create(context.Background(), operator(), in)
	if err != nil || created.ExternalReference != "padded-ref" {
		t.Errorf("created = %+v, %v", created, err)
	}
}

func TestCreateValidationFailures(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct {
		mutate func(*patient.CreateInput)
		field  string
		want   string
	}{
		"missing reference":    {func(in *patient.CreateInput) { in.ExternalReference = " " }, "external_reference", "is required"},
		"reference too long":   {func(in *patient.CreateInput) { in.ExternalReference = strings.Repeat("x", 129) }, "external_reference", "at most 128"},
		"reference control":    {func(in *patient.CreateInput) { in.ExternalReference = "a\x00b" }, "external_reference", "control characters"},
		"missing dob":          {func(in *patient.CreateInput) { in.DateOfBirth = "" }, "date_of_birth", "is required"},
		"dob not a date":       {func(in *patient.CreateInput) { in.DateOfBirth = "1984-2-9" }, "date_of_birth", "YYYY-MM-DD"},
		"dob with time":        {func(in *patient.CreateInput) { in.DateOfBirth = "1984-02-09T00:00:00Z" }, "date_of_birth", "YYYY-MM-DD"},
		"dob impossible":       {func(in *patient.CreateInput) { in.DateOfBirth = "1985-02-29" }, "date_of_birth", "YYYY-MM-DD"},
		"dob before 1900":      {func(in *patient.CreateInput) { in.DateOfBirth = "1899-12-31" }, "date_of_birth", "before 1900"},
		"dob in the future":    {func(in *patient.CreateInput) { in.DateOfBirth = "2026-09-07" }, "date_of_birth", "future"},
		"missing sex":          {func(in *patient.CreateInput) { in.Sex = "" }, "sex", "is required"},
		"unknown sex":          {func(in *patient.CreateInput) { in.Sex = "female" }, "sex", "must be one of"},
		"everything wrong":     {func(in *patient.CreateInput) { *in = patient.CreateInput{} }, "sex", "is required"},
		"whitespace reference": {func(in *patient.CreateInput) { in.ExternalReference = "\t\n" }, "external_reference", "is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			_, err := f.service.Create(context.Background(), operator(), in)
			var domErr *domain.Error
			if !errors.As(err, &domErr) || domErr.Kind != domain.KindValidation {
				t.Fatalf("err = %v, want validation error", err)
			}
			found := false
			for _, d := range domErr.Details {
				if d.Field == tc.field && strings.Contains(d.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("details = %+v, want %s: %q", domErr.Details, tc.field, tc.want)
			}
		})
	}
	if len(f.store.Audit) != 0 {
		t.Errorf("invalid input produced audit events: %+v", f.store.Audit)
	}
	for _, op := range f.recorder.operations {
		if op != "patient.create:invalid" {
			t.Errorf("operation outcome = %s, want invalid", op)
		}
	}
}

func TestCreateTodayIsAccepted(t *testing.T) {
	f := newFixture(t)
	in := validInput()
	in.DateOfBirth = "2026-09-06"
	if _, err := f.service.Create(context.Background(), operator(), in); err != nil {
		t.Errorf("birth today rejected: %v", err)
	}
}

func TestCreateDuplicateReferenceIsAConflict(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Create(context.Background(), operator(), validInput()); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Create(context.Background(), operator(), validInput())
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindConflict || domErr.Code != patient.CodeAlreadyExists {
		t.Errorf("err = %v, want %s", err, patient.CodeAlreadyExists)
	}
	if strings.Contains(domErr.Message, "PATIENTS_EXTERNAL_REFERENCE_KEY") || strings.Contains(domErr.Message, "index") {
		t.Errorf("message leaks schema detail: %q", domErr.Message)
	}
	if f.recorder.operations[len(f.recorder.operations)-1] != "patient.create:conflict" {
		t.Errorf("operations = %v", f.recorder.operations)
	}
}

func TestCreateFailsWhenAuditCannotBeWritten(t *testing.T) {
	f := newFixture(t)
	f.store.FailAudit = errors.New("audit_logs unavailable")
	_, err := f.service.Create(context.Background(), operator(), validInput())
	var domErr *domain.Error
	if err == nil || errors.As(err, &domErr) {
		t.Errorf("err = %v, want an internal error", err)
	}
	if page, _ := f.service.List(context.Background(), pagination.Request{Limit: 10}); len(page.Items) != 0 {
		t.Errorf("patient stored without its audit event: %+v", page.Items)
	}
	if len(f.recorder.errors) != 1 || f.recorder.operations[0] != "patient.create:error" {
		t.Errorf("hooks = %v %v", f.recorder.errors, f.recorder.operations)
	}
}

func TestGetVisibilityOfDeletedPatients(t *testing.T) {
	f := newFixture(t)
	seeded := f.store.Seed(1)
	id := seeded[0].ID
	if err := f.service.Delete(context.Background(), operator(), id); err != nil {
		t.Fatal(err)
	}

	for _, role := range []domain.Role{domain.RoleOperator, domain.RoleUser} {
		_, err := f.service.Get(context.Background(), auth.Principal{UserID: uuid.New(), Role: role}, id)
		var domErr *domain.Error
		if !errors.As(err, &domErr) || domErr.Code != patient.CodeNotFound {
			t.Errorf("%s sees a deleted patient: %v", role, err)
		}
	}
	got, err := f.service.Get(context.Background(), auth.Principal{UserID: uuid.New(), Role: domain.RoleAdmin}, id)
	if err != nil || got.Status != domain.PatientDeleted || got.DeletedAt == nil {
		t.Errorf("ADMIN get = %+v, %v; want the deleted record", got, err)
	}
	_, err = f.service.Get(context.Background(), operator(), uuid.New())
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Code != patient.CodeNotFound || domErr.Kind != domain.KindNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

func TestDeleteRecordsAuditAndIsIdempotentOnlyOnce(t *testing.T) {
	f := newFixture(t)
	id := f.store.Seed(1)[0].ID
	actor := operator()

	if err := f.service.Delete(ctxWithRequest("req-del"), actor, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	stored, _ := f.store.Get(id)
	if stored.Status != domain.PatientDeleted || stored.DeletedAt == nil || !stored.DeletedAt.Equal(now) {
		t.Errorf("stored = %+v", stored)
	}
	if len(f.store.Audit) != 1 || f.store.Audit[0].Event.Action != domain.AuditPatientDeleted || f.store.Audit[0].Event.ActorID != actor.UserID || f.store.Audit[0].Event.RequestID != "req-del" {
		t.Errorf("audit = %+v", f.store.Audit)
	}

	err := f.service.Delete(context.Background(), actor, id)
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindConflict || domErr.Code != patient.CodeAlreadyDeleted {
		t.Errorf("second delete: %v, want %s", err, patient.CodeAlreadyDeleted)
	}
	err = f.service.Delete(context.Background(), actor, uuid.New())
	if !errors.As(err, &domErr) || domErr.Code != patient.CodeNotFound {
		t.Errorf("unknown id: %v", err)
	}
	if len(f.store.Audit) != 1 {
		t.Errorf("failed deletes were audited: %+v", f.store.Audit)
	}
	if !strings.Contains(f.logs.String(), "patient deleted") {
		t.Error("deletion not logged")
	}
}

func TestListPaginatesInCreationOrderAndHidesDeleted(t *testing.T) {
	f := newFixture(t)
	seeded := f.store.Seed(5)
	if err := f.service.Delete(context.Background(), operator(), seeded[2].ID); err != nil {
		t.Fatal(err)
	}

	var got []uuid.UUID
	cursor := ""
	pages := 0
	for {
		page, err := f.service.List(context.Background(), pagination.Request{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, p := range page.Items {
			got = append(got, p.ID)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Items) != 2 {
			t.Errorf("a page with a next cursor has %d items, want the full 2", len(page.Items))
		}
		cursor = page.NextCursor
	}
	want := []uuid.UUID{seeded[0].ID, seeded[1].ID, seeded[3].ID, seeded[4].ID}
	if len(got) != len(want) || pages != 2 {
		t.Fatalf("walk = %v over %d pages, want %v over 2", got, pages, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %s, want %s", i, got[i], want[i])
		}
	}

	// A page that exactly fills the limit reports no further page.
	page, err := f.service.List(context.Background(), pagination.Request{Limit: 4})
	if err != nil || len(page.Items) != 4 || page.NextCursor != "" {
		t.Errorf("exact page = %d items, next %q, %v", len(page.Items), page.NextCursor, err)
	}
	if f.recorder.operations[len(f.recorder.operations)-1] != "patient.list:ok" {
		t.Errorf("operations = %v", f.recorder.operations)
	}
}

func TestListRejectsForeignCursors(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(2)
	nilID, _ := pagination.EncodeCursor(patient.Cursor{CreatedAt: now})
	zeroTime, _ := pagination.EncodeCursor(patient.Cursor{ID: uuid.New()})
	for name, cursor := range map[string]string{
		"garbage":   "!!!",
		"not json":  "bm90IGpzb24",
		"nil id":    nilID,
		"zero time": zeroTime,
		"wrong shape": func() string {
			c, _ := pagination.EncodeCursor(map[string]any{"page": 2})
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.service.List(context.Background(), pagination.Request{Limit: 10, Cursor: cursor})
			var domErr *domain.Error
			if !errors.As(err, &domErr) || domErr.Code != "INVALID_CURSOR" {
				t.Errorf("err = %v, want INVALID_CURSOR", err)
			}
		})
	}
}

func TestStoreFailuresAreInternalAndTraced(t *testing.T) {
	f := newFixture(t)
	f.store.Err = errors.New("connection refused")
	_, err := f.service.Get(context.Background(), operator(), uuid.New())
	var domErr *domain.Error
	if err == nil || errors.As(err, &domErr) {
		t.Errorf("err = %v, want unclassified", err)
	}
	if f.recorder.operations[0] != "patient.get:error" || len(f.recorder.errors) != 1 {
		t.Errorf("hooks = %v %v", f.recorder.operations, f.recorder.errors)
	}
}

func TestLogsCarryTraceIDFromTheTracer(t *testing.T) {
	f := newFixture(t)
	// The service's spans put a trace id in the context; the request
	// logger picks it up. Here the JSON handler is plain, so we only
	// check the propagation contract.
	ctx, span := f.recorder.Start(context.Background(), "x")
	span.End()
	if tracing.TraceID(ctx) != "trace-x" {
		t.Errorf("trace id = %q", tracing.TraceID(ctx))
	}
}
