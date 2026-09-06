package measurement_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement/measurementtest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

var now = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

var limits = config.Measurements{MaxBatchSize: 5, MaxMetadataBytes: 64, MaxFutureSkew: 5 * time.Minute}

type recorder struct {
	mu      sync.Mutex
	ops     []string
	batches []int
}

func (r *recorder) HTTPRequest(string, string, int, time.Duration) {}
func (r *recorder) Operation(name string, outcome metrics.Outcome, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, name+":"+string(outcome))
}
func (r *recorder) Batch(_ string, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, size)
}

type fixture struct {
	service  *measurement.Service
	store    *measurementtest.MemoryStore
	recorder *recorder
	logs     *bytes.Buffer
	patient  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: measurementtest.NewMemoryStore(), recorder: &recorder{}, logs: &bytes.Buffer{}}
	f.patient = f.store.AddPatient(domain.PatientActive)
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	f.service = measurement.NewService(f.store, limits, logger, measurement.Options{Metrics: f.recorder, Now: func() time.Time { return now }})
	return f
}

func operator() auth.Principal {
	return auth.Principal{UserID: uuid.New(), Role: domain.RoleOperator}
}

func f64(v float64) *float64 { return &v }

func (f *fixture) valid() measurement.Input {
	return measurement.Input{
		PatientID: f.patient.String(), Type: "HEART_RATE", Value: f64(72), Unit: "bpm",
		RecordedAt: "2026-09-06T11:59:00Z", Source: "synthetic-monitor", Metadata: json.RawMessage(`{"lead":"II"}`),
	}
}

func fieldMessages(t *testing.T, err error, wantCode string) map[string][]string {
	t.Helper()
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindValidation {
		t.Fatalf("err = %v, want validation error", err)
	}
	if domErr.Code != wantCode {
		t.Errorf("code = %q, want %q", domErr.Code, wantCode)
	}
	out := map[string][]string{}
	for _, d := range domErr.Details {
		out[d.Field] = append(out[d.Field], d.Message)
	}
	return out
}

func expectField(t *testing.T, fields map[string][]string, field, want string) {
	t.Helper()
	for _, m := range fields[field] {
		if strings.Contains(m, want) {
			return
		}
	}
	t.Errorf("field %q: messages %v lack %q (all: %v)", field, fields[field], want, fields)
}

func TestCreateStoresReadingWithAudit(t *testing.T) {
	f := newFixture(t)
	actor := operator()
	ctx := requestid.NewContext(context.Background(), "req-m1")

	m, err := f.service.Create(ctx, actor, f.valid())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.PatientID != f.patient || m.Type != domain.HeartRate || m.Value != 72 || m.Unit != "bpm" || m.Source != "synthetic-monitor" {
		t.Errorf("m = %+v", m)
	}
	if !m.RecordedAt.Equal(time.Date(2026, 9, 6, 11, 59, 0, 0, time.UTC)) || m.RecordedAt.Location() != time.UTC {
		t.Errorf("recorded_at = %s", m.RecordedAt)
	}
	if string(m.Metadata) != `{"lead":"II"}` {
		t.Errorf("metadata = %s", m.Metadata)
	}
	if len(f.store.Audit) != 1 || f.store.Audit[0].MeasurementID != m.ID || f.store.Audit[0].Event.Action != domain.AuditMeasurementCreated || f.store.Audit[0].Event.ActorID != actor.UserID || f.store.Audit[0].Event.RequestID != "req-m1" {
		t.Errorf("audit = %+v", f.store.Audit)
	}
	if !strings.Contains(f.logs.String(), "measurement created") || f.recorder.ops[0] != "measurement.create:ok" {
		t.Errorf("observability: %s %v", f.logs.String(), f.recorder.ops)
	}
}

func TestCreateNormalises(t *testing.T) {
	f := newFixture(t)
	in := f.valid()
	in.RecordedAt = "2026-09-06T13:59:00+02:00"
	in.Source = "  padded  "
	in.Metadata = json.RawMessage("  { \"a\" : [1, 2] }  ")
	m, err := f.service.Create(context.Background(), operator(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !m.RecordedAt.Equal(time.Date(2026, 9, 6, 11, 59, 0, 0, time.UTC)) || m.Source != "padded" || string(m.Metadata) != `{"a":[1,2]}` {
		t.Errorf("m = %+v", m)
	}
	in.Metadata = nil
	in.RecordedAt = "2026-09-06T11:58:00Z"
	if m, err := f.service.Create(context.Background(), operator(), in); err != nil || string(m.Metadata) != `{}` {
		t.Errorf("absent metadata: %+v, %v", m, err)
	}
	in.Metadata = json.RawMessage("null")
	in.RecordedAt = "2026-09-06T11:57:00Z"
	if m, err := f.service.Create(context.Background(), operator(), in); err != nil || string(m.Metadata) != `{}` {
		t.Errorf("null metadata: %+v, %v", m, err)
	}
}

func TestCreateValidationMatrix(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct {
		mutate func(*measurement.Input)
		field  string
		want   string
	}{
		"missing patient":        {func(in *measurement.Input) { in.PatientID = "" }, "patient_id", "is required"},
		"patient not uuid":       {func(in *measurement.Input) { in.PatientID = "42" }, "patient_id", "must be a UUID"},
		"patient nil uuid":       {func(in *measurement.Input) { in.PatientID = uuid.Nil.String() }, "patient_id", "must be a UUID"},
		"unknown patient":        {func(in *measurement.Input) { in.PatientID = uuid.NewString() }, "patient_id", "existing patient"},
		"missing type":           {func(in *measurement.Input) { in.Type = "" }, "type", "is required"},
		"unknown type":           {func(in *measurement.Input) { in.Type = "PULSE" }, "type", "must be one of"},
		"lowercase type":         {func(in *measurement.Input) { in.Type = "heart_rate" }, "type", "must be one of"},
		"missing value":          {func(in *measurement.Input) { in.Value = nil }, "value", "is required"},
		"nan value":              {func(in *measurement.Input) { in.Value = f64(math.NaN()) }, "value", "finite"},
		"infinite value":         {func(in *measurement.Input) { in.Value = f64(math.Inf(1)) }, "value", "finite"},
		"value below min":        {func(in *measurement.Input) { in.Value = f64(-0.001) }, "value", "between 0 and 300"},
		"value above max":        {func(in *measurement.Input) { in.Value = f64(300.001) }, "value", "between 0 and 300"},
		"missing unit":           {func(in *measurement.Input) { in.Unit = "" }, "unit", "is required"},
		"wrong unit":             {func(in *measurement.Input) { in.Unit = "BPM" }, "unit", `must be "bpm"`},
		"unit of other type":     {func(in *measurement.Input) { in.Unit = "mmHg" }, "unit", `must be "bpm"`},
		"missing timestamp":      {func(in *measurement.Input) { in.RecordedAt = "" }, "recorded_at", "is required"},
		"timestamp no offset":    {func(in *measurement.Input) { in.RecordedAt = "2026-09-06T11:59:00" }, "recorded_at", "RFC 3339"},
		"timestamp date only":    {func(in *measurement.Input) { in.RecordedAt = "2026-09-06" }, "recorded_at", "RFC 3339"},
		"timestamp epoch number": {func(in *measurement.Input) { in.RecordedAt = "1757159940" }, "recorded_at", "RFC 3339"},
		"timestamp too old":      {func(in *measurement.Input) { in.RecordedAt = "1899-12-31T23:59:59Z" }, "recorded_at", "before 1900"},
		"timestamp in future":    {func(in *measurement.Input) { in.RecordedAt = "2026-09-06T12:05:01Z" }, "recorded_at", "in the future"},
		"missing source":         {func(in *measurement.Input) { in.Source = "  " }, "source", "is required"},
		"source too long":        {func(in *measurement.Input) { in.Source = strings.Repeat("s", 65) }, "source", "at most 64"},
		"source control chars":   {func(in *measurement.Input) { in.Source = "mon\x01itor" }, "source", "control characters"},
		"metadata not json":      {func(in *measurement.Input) { in.Metadata = json.RawMessage(`{"a":`) }, "metadata", "valid JSON"},
		"metadata array":         {func(in *measurement.Input) { in.Metadata = json.RawMessage(`[1,2]`) }, "metadata", "JSON object"},
		"metadata string":        {func(in *measurement.Input) { in.Metadata = json.RawMessage(`"x"`) }, "metadata", "JSON object"},
		"metadata number":        {func(in *measurement.Input) { in.Metadata = json.RawMessage(`7`) }, "metadata", "JSON object"},
		"metadata too large":     {func(in *measurement.Input) { in.Metadata = json.RawMessage(`{"k":"` + strings.Repeat("v", 60) + `"}`) }, "metadata", "at most 64 bytes"},
		"metadata too deep": {func(in *measurement.Input) {
			in.Metadata = json.RawMessage(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":1}}}}}}}}}`)
		}, "metadata", "deeper than 8"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := f.valid()
			tc.mutate(&in)
			_, err := f.service.Create(context.Background(), operator(), in)
			expectField(t, fieldMessages(t, err, measurement.CodeValidationFailed), tc.field, tc.want)
		})
	}
	if f.store.Len() != 0 || len(f.store.Audit) != 0 {
		t.Errorf("invalid input changed state: %d items, %d audit events", f.store.Len(), len(f.store.Audit))
	}
	for _, op := range f.recorder.ops {
		if op != "measurement.create:invalid" {
			t.Errorf("op = %s", op)
		}
	}
}

func TestCreateBoundaryValuesAreInclusive(t *testing.T) {
	f := newFixture(t)
	for i, tc := range []struct {
		typ, unit string
		value     float64
	}{
		{"HEART_RATE", "bpm", 0}, {"HEART_RATE", "bpm", 300},
		{"BLOOD_PRESSURE_SYSTOLIC", "mmHg", 300}, {"BLOOD_PRESSURE_DIASTOLIC", "mmHg", 200},
		{"SPO2", "%", 100}, {"SPO2", "%", 0},
		{"BODY_TEMPERATURE", "C", 20}, {"BODY_TEMPERATURE", "C", 45},
		{"BLOOD_GLUCOSE", "mg/dL", 1000}, {"RESPIRATORY_RATE", "breaths/min", 0},
		{"HEART_RATE", "bpm", 72.5},
	} {
		in := f.valid()
		in.Type, in.Unit, in.Value = tc.typ, tc.unit, f64(tc.value)
		in.RecordedAt = fmt.Sprintf("2026-09-06T11:%02d:00Z", i)
		if _, err := f.service.Create(context.Background(), operator(), in); err != nil {
			t.Errorf("%s %g %s rejected: %v", tc.typ, tc.value, tc.unit, err)
		}
	}
	for _, tc := range []struct {
		typ, unit string
		value     float64
	}{
		{"BLOOD_PRESSURE_DIASTOLIC", "mmHg", 200.5}, {"BODY_TEMPERATURE", "C", 19.99}, {"SPO2", "%", 100.01}, {"BLOOD_GLUCOSE", "mg/dL", -1},
	} {
		in := f.valid()
		in.Type, in.Unit, in.Value = tc.typ, tc.unit, f64(tc.value)
		_, err := f.service.Create(context.Background(), operator(), in)
		expectField(t, fieldMessages(t, err, measurement.CodeValidationFailed), "value", "must be between")
	}
}

func TestCreateEveryCatalogTypeWithItsUnit(t *testing.T) {
	f := newFixture(t)
	for i, typ := range measurement.Catalog {
		in := f.valid()
		in.Type, in.Unit, in.Value = string(typ.Code), typ.Unit, f64((typ.Min+typ.Max)/2)
		in.RecordedAt = fmt.Sprintf("2026-09-06T10:%02d:00Z", i)
		if _, err := f.service.Create(context.Background(), operator(), in); err != nil {
			t.Errorf("%s: %v", typ.Code, err)
		}
	}
}

func TestCreateFutureSkewBoundary(t *testing.T) {
	f := newFixture(t)
	in := f.valid()
	in.RecordedAt = "2026-09-06T12:05:00Z" // exactly now + skew
	if _, err := f.service.Create(context.Background(), operator(), in); err != nil {
		t.Errorf("recorded_at at the skew bound rejected: %v", err)
	}
}

func TestCreateRejectsDeletedPatientAndDuplicates(t *testing.T) {
	f := newFixture(t)
	deleted := f.store.AddPatient(domain.PatientDeleted)
	in := f.valid()
	in.PatientID = deleted.String()
	_, err := f.service.Create(context.Background(), operator(), in)
	expectField(t, fieldMessages(t, err, measurement.CodeValidationFailed), "patient_id", "deleted patient")

	if _, err := f.service.Create(context.Background(), operator(), f.valid()); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.Create(context.Background(), operator(), f.valid())
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindConflict || domErr.Code != measurement.CodeAlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	if strings.Contains(domErr.Message, "MEASUREMENTS_PATIENT") {
		t.Error("message leaks the constraint name")
	}
	// Same reading from another source is a distinct reading.
	other := f.valid()
	other.Source = "second-device"
	if _, err := f.service.Create(context.Background(), operator(), other); err != nil {
		t.Errorf("different source rejected: %v", err)
	}
}

func TestBatchStoresAllOrNothing(t *testing.T) {
	f := newFixture(t)
	items := make([]measurement.Input, 3)
	for i := range items {
		items[i] = f.valid()
		items[i].RecordedAt = fmt.Sprintf("2026-09-06T11:%02d:00Z", i)
	}
	ctx := requestid.NewContext(context.Background(), "req-b1")
	created, err := f.service.CreateBatch(ctx, operator(), items)
	if err != nil || len(created) != 3 {
		t.Fatalf("CreateBatch = %d, %v", len(created), err)
	}
	for i, m := range created {
		if !m.RecordedAt.Equal(time.Date(2026, 9, 6, 11, i, 0, 0, time.UTC)) {
			t.Errorf("item %d out of input order: %s", i, m.RecordedAt)
		}
	}
	if len(f.store.Audit) != 3 || f.store.Audit[2].Event.RequestID != "req-b1" {
		t.Errorf("audit = %+v", f.store.Audit)
	}
	if f.recorder.batches[0] != 3 || f.recorder.ops[0] != "measurement.batch:ok" {
		t.Errorf("hooks = %v %v", f.recorder.batches, f.recorder.ops)
	}

	// One item already stored: nothing else is stored and the item is named.
	again := []measurement.Input{f.valid(), items[1]}
	again[0].RecordedAt = "2026-09-06T11:30:00Z"
	_, err = f.service.CreateBatch(context.Background(), operator(), again)
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Code != measurement.CodeAlreadyExists || len(domErr.Details) != 1 || domErr.Details[0].Field != "items[1]" {
		t.Errorf("duplicate in batch: %v", err)
	}
	if f.store.Len() != 3 || len(f.store.Audit) != 3 {
		t.Errorf("partial batch stored: %d items", f.store.Len())
	}
}

func TestBatchValidationReportsEveryItem(t *testing.T) {
	f := newFixture(t)
	bad := f.valid()
	bad.Unit = "mmHg"
	unknownPatient := f.valid()
	unknownPatient.PatientID = uuid.NewString()
	dup := f.valid()
	items := []measurement.Input{f.valid(), bad, unknownPatient, dup}
	_, err := f.service.CreateBatch(context.Background(), operator(), items)
	fields := fieldMessages(t, err, measurement.CodeValidationFailed)
	expectField(t, fields, "items[1].unit", `must be "bpm"`)
	expectField(t, fields, "items[2].patient_id", "existing patient")
	expectField(t, fields, "items[3]", "duplicates items[0]")
	if f.store.Len() != 0 {
		t.Error("invalid batch stored items")
	}

	_, err = f.service.CreateBatch(context.Background(), operator(), nil)
	expectField(t, fieldMessages(t, err, measurement.CodeValidationFailed), "items", "at least one")

	tooMany := make([]measurement.Input, limits.MaxBatchSize+1)
	for i := range tooMany {
		tooMany[i] = f.valid()
		tooMany[i].RecordedAt = fmt.Sprintf("2026-09-06T10:%02d:00Z", i)
	}
	_, err = f.service.CreateBatch(context.Background(), operator(), tooMany)
	expectField(t, fieldMessages(t, err, measurement.CodeValidationFailed), "items", "at most 5")

	exact := tooMany[:limits.MaxBatchSize]
	if _, err := f.service.CreateBatch(context.Background(), operator(), exact); err != nil {
		t.Errorf("batch at the size limit rejected: %v", err)
	}
}

func TestGetAndDelete(t *testing.T) {
	f := newFixture(t)
	actor := operator()
	m, err := f.service.Create(context.Background(), actor, f.valid())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.service.Get(context.Background(), actor, m.ID); err != nil || got.ID != m.ID {
		t.Errorf("Get = %+v, %v", got, err)
	}
	var domErr *domain.Error
	if _, err := f.service.Get(context.Background(), actor, uuid.New()); !errors.As(err, &domErr) || domErr.Code != measurement.CodeNotFound {
		t.Errorf("unknown get: %v", err)
	}

	ctx := requestid.NewContext(context.Background(), "req-d1")
	if err := f.service.Delete(ctx, actor, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.store.Len() != 0 || len(f.store.Audit) != 2 || f.store.Audit[1].Event.Action != domain.AuditMeasurementDeleted || f.store.Audit[1].Event.RequestID != "req-d1" {
		t.Errorf("after delete: %d items, audit %+v", f.store.Len(), f.store.Audit)
	}
	if err := f.service.Delete(context.Background(), actor, m.ID); !errors.As(err, &domErr) || domErr.Code != measurement.CodeNotFound {
		t.Errorf("second delete: %v", err)
	}
	if len(f.store.Audit) != 2 {
		t.Error("failed delete audited")
	}
}

func TestListFiltersAndPaginates(t *testing.T) {
	f := newFixture(t)
	types := []string{"HEART_RATE", "SPO2", "HEART_RATE", "HEART_RATE", "SPO2"}
	units := map[string]string{"HEART_RATE": "bpm", "SPO2": "%"}
	for i, typ := range types {
		in := f.valid()
		in.Type, in.Unit, in.Value = typ, units[typ], f64(50)
		in.RecordedAt = fmt.Sprintf("2026-09-06T11:%02d:00Z", i)
		if _, err := f.service.Create(context.Background(), operator(), in); err != nil {
			t.Fatal(err)
		}
	}
	list := func(in measurement.ListInput) measurement.Page {
		t.Helper()
		if in.Page.Limit == 0 {
			in.Page.Limit = 50
		}
		page, err := f.service.ListByPatient(context.Background(), operator(), f.patient, in)
		if err != nil {
			t.Fatalf("List %+v: %v", in, err)
		}
		return page
	}
	if page := list(measurement.ListInput{}); len(page.Items) != 5 || page.NextCursor != "" {
		t.Errorf("all = %d items", len(page.Items))
	}
	if page := list(measurement.ListInput{Type: "SPO2"}); len(page.Items) != 2 {
		t.Errorf("SPO2 = %d items", len(page.Items))
	}
	if page := list(measurement.ListInput{From: "2026-09-06T11:01:00Z", To: "2026-09-06T11:03:00Z"}); len(page.Items) != 2 || page.Items[0].RecordedAt.Minute() != 1 {
		t.Errorf("range = %+v", page.Items)
	}

	var seen []time.Time
	cursor := ""
	for {
		page := list(measurement.ListInput{Page: pagination.Request{Limit: 2, Cursor: cursor}})
		for _, m := range page.Items {
			seen = append(seen, m.RecordedAt)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d readings", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if !seen[i].After(seen[i-1]) {
			t.Errorf("not in recording order: %v", seen)
		}
	}

	for name, in := range map[string]measurement.ListInput{
		"bad type":   {Type: "PULSE"},
		"bad from":   {From: "yesterday"},
		"to before":  {From: "2026-09-06T11:03:00Z", To: "2026-09-06T11:01:00Z"},
		"bad cursor": {Page: pagination.Request{Limit: 10, Cursor: "!!!"}},
	} {
		if in.Page.Limit == 0 {
			in.Page.Limit = 10
		}
		if _, err := f.service.ListByPatient(context.Background(), operator(), f.patient, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	var domErr *domain.Error
	if _, err := f.service.ListByPatient(context.Background(), operator(), uuid.New(), measurement.ListInput{Page: pagination.Request{Limit: 10}}); !errors.As(err, &domErr) || domErr.Code != measurement.CodePatientNotFound {
		t.Errorf("unknown patient: %v", err)
	}
	deleted := f.store.AddPatient(domain.PatientDeleted)
	if _, err := f.service.ListByPatient(context.Background(), operator(), deleted, measurement.ListInput{Page: pagination.Request{Limit: 10}}); !errors.As(err, &domErr) || domErr.Code != measurement.CodePatientNotFound {
		t.Errorf("deleted patient for OPERATOR: %v", err)
	}
	if _, err := f.service.ListByPatient(context.Background(), auth.Principal{Role: domain.RoleAdmin}, deleted, measurement.ListInput{Page: pagination.Request{Limit: 10}}); err != nil {
		t.Errorf("deleted patient for ADMIN: %v", err)
	}
}

func TestCatalogIsComplete(t *testing.T) {
	want := []string{"HEART_RATE", "BLOOD_PRESSURE_SYSTOLIC", "BLOOD_PRESSURE_DIASTOLIC", "SPO2", "BODY_TEMPERATURE", "BLOOD_GLUCOSE", "RESPIRATORY_RATE"}
	if got := measurement.TypeCodes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("catalog = %v", got)
	}
	for _, typ := range measurement.Catalog {
		if typ.Unit == "" || typ.Min >= typ.Max {
			t.Errorf("bad catalogue entry %+v", typ)
		}
	}
	if _, ok := measurement.Lookup("PULSE"); ok {
		t.Error("unknown type found")
	}
}

func TestReadingsOfDeletedPatientsFollowPatientVisibility(t *testing.T) {
	f := newFixture(t)
	actor := operator()
	m, err := f.service.Create(context.Background(), actor, f.valid())
	if err != nil {
		t.Fatal(err)
	}
	f.store.Patients[f.patient] = domain.PatientDeleted

	var domErr *domain.Error
	if _, err := f.service.Get(context.Background(), actor, m.ID); !errors.As(err, &domErr) || domErr.Code != measurement.CodeNotFound {
		t.Errorf("OPERATOR get after patient deletion: %v, want not found", err)
	}
	if err := f.service.Delete(context.Background(), actor, m.ID); !errors.As(err, &domErr) || domErr.Code != measurement.CodeNotFound {
		t.Errorf("OPERATOR delete after patient deletion: %v, want not found", err)
	}
	if f.store.Len() != 1 || len(f.store.Audit) != 1 {
		t.Error("hidden reading was deleted or audited")
	}
	admin := auth.Principal{UserID: uuid.New(), Role: domain.RoleAdmin}
	if got, err := f.service.Get(context.Background(), admin, m.ID); err != nil || got.ID != m.ID {
		t.Errorf("ADMIN get: %+v, %v", got, err)
	}
	if err := f.service.Delete(context.Background(), admin, m.ID); err != nil {
		t.Errorf("ADMIN delete: %v", err)
	}
}

func TestRecordedAtIsStoredAtMicrosecondPrecision(t *testing.T) {
	f := newFixture(t)
	in := f.valid()
	in.RecordedAt = "2026-09-06T11:59:00.123456789Z"
	m, err := f.service.Create(context.Background(), operator(), in)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 6, 11, 59, 0, 123456000, time.UTC); !m.RecordedAt.Equal(want) {
		t.Errorf("recorded_at = %s, want %s", m.RecordedAt.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	// Two readings that differ only below a microsecond are the same reading.
	twin := f.valid()
	twin.RecordedAt = "2026-09-06T11:59:00.123456001Z"
	var domErr *domain.Error
	if _, err := f.service.Create(context.Background(), operator(), twin); !errors.As(err, &domErr) || domErr.Code != measurement.CodeAlreadyExists {
		t.Errorf("sub-microsecond twin: %v, want conflict", err)
	}
	batch := []measurement.Input{f.valid(), f.valid()}
	batch[0].RecordedAt = "2026-09-06T11:58:00.000000100Z"
	batch[1].RecordedAt = "2026-09-06T11:58:00.000000900Z"
	_, err = f.service.CreateBatch(context.Background(), operator(), batch)
	expectField(t, fieldMessages(t, err, measurement.CodeValidationFailed), "items[1]", "duplicates items[0]")
}

// writeFailingStore passes every read and rejects every write the way the
// database trigger would for a reading that slipped past validation.
type writeFailingStore struct{ *measurementtest.MemoryStore }

func (w *writeFailingStore) Create(context.Context, measurement.NewMeasurement, measurement.AuditEvent) (domain.Measurement, error) {
	return domain.Measurement{}, domain.Wrap(errors.New("check_violation"), domain.KindValidation, "MEASUREMENTS_VALUE_RANGE_CHECK", "The value violates a data constraint.")
}

func TestDatabaseRejectionIsReportedAndLogged(t *testing.T) {
	f := newFixture(t)
	svc := measurement.NewService(&writeFailingStore{MemoryStore: f.store}, limits, slog.New(slog.NewJSONHandler(f.logs, nil)), measurement.Options{Now: func() time.Time { return now }})
	_, err := svc.Create(context.Background(), operator(), f.valid())
	fields := fieldMessages(t, err, measurement.CodeValidationFailed)
	expectField(t, fields, "measurement", "data constraint")
	if strings.Contains(fmt.Sprint(fields), "MEASUREMENTS_VALUE_RANGE_CHECK") {
		t.Error("constraint name reached the client details")
	}
	if !strings.Contains(f.logs.String(), "database rejected a validated measurement") {
		t.Errorf("rejection not logged: %s", f.logs.String())
	}
}
