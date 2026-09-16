// Package synthtest is a fake gateway for testing the synthetic data
// loader: enough of the public API (sign-in, patients, measurements,
// processing jobs, idempotency, rate limiting) to exercise every path the
// loader takes, with the state kept in memory so a test can inspect it.
package synthtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Gateway is the fake. Fields are safe to read once the loader has
// returned.
type Gateway struct {
	Server *httptest.Server
	// Environment is what /health reports; "" imitates an older gateway.
	Environment string
	// Email and Password are the one account that can sign in.
	Email, Password string
	// RateLimitOnce makes the first batch answer 429 with Retry-After: 0,
	// once.
	RateLimitOnce bool
	// ForgetIdempotency drops every idempotency record, as an expired
	// retention would, so a repeated create meets the conflict path.
	ForgetIdempotency bool

	mu           sync.Mutex
	patients     []Patient
	measurements map[string]uuid.UUID
	idempotency  map[string]replay
	requests     []Request
	rateLimited  bool
	jobs         int
}

// Patient is a stored patient.
type Patient struct {
	ID                uuid.UUID `json:"id"`
	ExternalReference string    `json:"external_reference"`
	DateOfBirth       string    `json:"date_of_birth"`
	Sex               string    `json:"sex"`
	Status            string    `json:"status"`
}

// Request is one call the fake received.
type Request struct {
	Method, Path, IdempotencyKey string
}

type replay struct {
	status int
	body   []byte
}

const token = "synthtest-token"

// keyFormat is the gateway's rule for Idempotency-Key.
var keyFormat = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,255}$`)

// New starts a fake gateway that reports itself as environment and accepts
// the given sign-in. It stops with the test.
func New(t testing.TB, environment, email, password string) *Gateway {
	t.Helper()
	g := &Gateway{
		Environment: environment, Email: email, Password: password,
		measurements: map[string]uuid.UUID{}, idempotency: map[string]replay{},
	}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Server.Close)
	return g
}

// URL is the fake's base URL.
func (g *Gateway) URL() string { return g.Server.URL }

// Requests returns every call received so far.
func (g *Gateway) Requests() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Request(nil), g.requests...)
}

// Patients returns the stored patients in creation order.
func (g *Gateway) Patients() []Patient {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Patient(nil), g.patients...)
}

// MeasurementCount is how many distinct readings are stored.
func (g *Gateway) MeasurementCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.measurements)
}

// Jobs is how many processing jobs were created.
func (g *Gateway) Jobs() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.jobs
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, Request{r.Method, r.URL.Path, r.Header.Get("Idempotency-Key")})

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		body := map[string]string{"status": "ok", "service": "api-gateway", "version": "test"}
		if g.Environment != "" {
			body["environment"] = g.Environment
		}
		writeJSON(w, http.StatusOK, body)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/login":
		var in struct{ Email, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Email != g.Email || in.Password != g.Password {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid credentials", nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
		return
	}

	if r.Header.Get("Authorization") != "Bearer "+token {
		writeError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a token is required", nil)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !keyFormat.MatchString(key) {
		writeError(w, http.StatusUnprocessableEntity, "INVALID_IDEMPOTENCY_KEY", "The Idempotency-Key header is invalid.",
			[]map[string]string{{"field": "Idempotency-Key", "message": "must be 1 to 255 characters of [A-Za-z0-9._:-]"}})
		return
	}
	if key != "" && !g.ForgetIdempotency {
		if rep, ok := g.idempotency[key]; ok {
			w.Header().Set("Idempotency-Replayed", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rep.status)
			_, _ = w.Write(rep.body)
			return
		}
	}
	rec := httptest.NewRecorder()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/patients":
		g.createPatient(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/patients":
		g.listPatients(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/measurements/batch":
		g.storeBatch(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/measurements":
		g.storeOne(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/processing/jobs":
		g.createJob(rec, r)
	default:
		writeError(rec, http.StatusNotFound, "NOT_FOUND", "no such route", nil)
	}
	if key != "" && rec.Code < 500 && rec.Code != http.StatusTooManyRequests && !g.ForgetIdempotency {
		g.idempotency[key] = replay{rec.Code, rec.Body.Bytes()}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (g *Gateway) createPatient(w http.ResponseWriter, r *http.Request) {
	var in Patient
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ExternalReference == "" {
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "bad patient", nil)
		return
	}
	for _, p := range g.patients {
		if p.ExternalReference == in.ExternalReference {
			writeError(w, http.StatusConflict, "PATIENT_ALREADY_EXISTS", "another patient has that reference", nil)
			return
		}
	}
	in.ID, in.Status = uuid.New(), "ACTIVE"
	g.patients = append(g.patients, in)
	writeJSON(w, http.StatusCreated, in)
}

func (g *Gateway) listPatients(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	end := min(start+limit, len(g.patients))
	items := g.patients[start:end]
	var next *string
	if end < len(g.patients) {
		s := strconv.Itoa(end)
		next = &s
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next, "has_more": next != nil})
}

type item struct {
	PatientID  string          `json:"patient_id"`
	Type       string          `json:"type"`
	Value      *float64        `json:"value"`
	Unit       string          `json:"unit"`
	RecordedAt string          `json:"recorded_at"`
	Source     string          `json:"source"`
	Metadata   json.RawMessage `json:"metadata"`
}

func (g *Gateway) check(items []item) (string, string, []map[string]string) {
	for i, it := range items {
		if it.Value == nil || it.Type == "" || it.Unit == "" || it.RecordedAt == "" || it.Source == "" {
			return "MEASUREMENT_VALIDATION_FAILED", "missing field", []map[string]string{{"field": fmt.Sprintf("items[%d]", i), "message": "incomplete"}}
		}
		found := false
		for _, p := range g.patients {
			if p.ID.String() == it.PatientID {
				found = true
			}
		}
		if !found {
			return "MEASUREMENT_VALIDATION_FAILED", "unknown patient", []map[string]string{{"field": fmt.Sprintf("items[%d].patient_id", i), "message": "must be an existing patient"}}
		}
		if _, dup := g.measurements[readingKey(it)]; dup {
			return "MEASUREMENT_ALREADY_EXISTS", "the same reading is already stored", []map[string]string{{"field": fmt.Sprintf("items[%d]", i), "message": "already stored"}}
		}
	}
	return "", "", nil
}

func readingKey(it item) string {
	return strings.Join([]string{it.PatientID, it.Type, it.RecordedAt, it.Source}, "|")
}

func (g *Gateway) storeBatch(w http.ResponseWriter, r *http.Request) {
	if g.RateLimitOnce && !g.rateLimited {
		g.rateLimited = true
		w.Header().Set("Retry-After", "0")
		writeError(w, http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED", "slow down", nil)
		return
	}
	var in struct {
		Items []item `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Items) == 0 || len(in.Items) > 1000 {
		writeError(w, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED", "1 to 1000 items", nil)
		return
	}
	if code, msg, details := g.check(in.Items); code != "" {
		status := http.StatusUnprocessableEntity
		if code == "MEASUREMENT_ALREADY_EXISTS" {
			status = http.StatusConflict
		}
		writeError(w, status, code, msg, details)
		return
	}
	out := make([]map[string]any, len(in.Items))
	for i, it := range in.Items {
		id := uuid.New()
		g.measurements[readingKey(it)] = id
		out[i] = map[string]any{"id": id}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"items": out})
}

func (g *Gateway) storeOne(w http.ResponseWriter, r *http.Request) {
	var in item
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED", "bad reading", nil)
		return
	}
	if code, msg, details := g.check([]item{in}); code != "" {
		status := http.StatusUnprocessableEntity
		if code == "MEASUREMENT_ALREADY_EXISTS" {
			status = http.StatusConflict
		}
		writeError(w, status, code, msg, details)
		return
	}
	id := uuid.New()
	g.measurements[readingKey(in)] = id
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (g *Gateway) createJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PatientID string `json:"patient_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	for k := range g.measurements {
		if strings.HasPrefix(k, in.PatientID+"|") {
			g.jobs++
			writeJSON(w, http.StatusCreated, map[string]any{"id": uuid.New(), "patient_id": in.PatientID, "status": "COMPLETED"})
			return
		}
	}
	writeError(w, http.StatusUnprocessableEntity, "PROCESSING_NO_MEASUREMENTS", "no readings in range", nil)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, code, message string, details []map[string]string) {
	body := map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": "synthtest"}}
	if details != nil {
		body["error"].(map[string]any)["details"] = details
	}
	writeJSON(w, status, body)
}
