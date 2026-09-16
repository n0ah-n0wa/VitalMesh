//go:build stack

// Package stack is the end-to-end suite against the real containerized
// application stack: the gateway and processor images, PostgreSQL and
// Redis, started by docker compose (scripts/stack-test.sh, `make
// stack-test`). Nothing here is wired in process; the suite is a client of
// the gateway's public API, reads the database only to check what was
// persisted, and drives docker compose to take dependencies away for the
// failure cases.
//
// It is deterministic: every value it sends is fixed, every name it
// creates carries one run identifier, every wait polls for a condition
// with a deadline rather than sleeping a guessed time, and the flows run in
// a fixed order as subtests of one test. It expects a clean stack (the
// script starts one) but tolerates a reused one: accounts are created if
// missing and names never collide across runs.
//
// Configuration, all with local-stack defaults:
//
//	E2E_GATEWAY_URL      http://localhost:8080
//	E2E_DATABASE_URL     the stack's PostgreSQL, for persistence checks
//	E2E_COMPOSE_FILE     docker-compose.yml at the repository root
//	E2E_COMPOSE_PROJECT  vitalmesh (the compose project name)
package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
)

// password is the one password of every account the suite creates. It
// protects nothing: the accounts exist in a throwaway stack.
const password = "e2e-stack-password-not-a-secret"

// account is a signed-in principal.
type account struct {
	email string
	role  domain.Role
	id    uuid.UUID
	token string
}

// stack is the environment under test and the state the subtests share.
type stack struct {
	gateway string
	client  *http.Client
	db      *pgxpool.Pool
	compose *compose
	runID   string
	// base is the recording time of the first reading in every series: on
	// the minute, two hours ago, so that every series sits inside the
	// processor's acceptance window and the gateway's future-skew bound.
	base time.Time

	admin, operator, user, limited account
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newStack connects to the stack, waits for it to be ready, and creates
// the suite's accounts.
func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{
		gateway: strings.TrimRight(env("E2E_GATEWAY_URL", "http://localhost:8080"), "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
		runID:   fmt.Sprintf("%x", time.Now().Unix()),
		base:    time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute),
	}
	s.compose = newCompose(t)

	s.waitFor(t, 90*time.Second, "the gateway to be ready", func() bool {
		r, err := s.raw(http.MethodGet, "/ready", "", nil, nil)
		return err == nil && r.status == http.StatusOK
	})

	dbURL := env("E2E_DATABASE_URL", "postgres://vitalmesh:vitalmesh@localhost:5432/vitalmesh?sslmode=disable")
	pool, err := postgres.Connect(context.Background(), config.Database{URL: dbURL, MaxConns: 4, ConnectTimeout: 5 * time.Second}, postgres.Options{})
	if err != nil {
		t.Fatalf("connect to the stack's database (E2E_DATABASE_URL): %v", err)
	}
	t.Cleanup(pool.Close)
	s.db = pool

	s.admin = s.ensureAccount(t, "e2e-admin@vitalmesh.invalid", domain.RoleAdmin)
	s.operator = s.ensureAccount(t, "e2e-operator@vitalmesh.invalid", domain.RoleOperator)
	s.user = s.ensureAccount(t, "e2e-user@vitalmesh.invalid", domain.RoleUser)
	// Its budget is spent by the rate-limit flow, so nothing else uses it,
	// and its name carries the run so that a reused stack starts it fresh.
	s.limited = s.ensureAccount(t, "e2e-limited-"+s.runID+"@vitalmesh.invalid", domain.RoleUser)
	return s
}

// ensureAccount creates an account the way `api-gateway users create`
// does (there is no API for it, by design) and signs it in through the API.
func (s *stack) ensureAccount(t *testing.T, email string, role domain.Role) account {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hashCfg, err := config.LoadPasswordHash(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.NewHasher(hashCfg).Hash(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	_, err = postgres.NewUsers(s.db).Create(ctx, email, hash, role)
	var domErr *domain.Error
	if err != nil && !(errors.As(err, &domErr) && domErr.Kind == domain.KindConflict) {
		t.Fatalf("create %s: %v", email, err)
	}
	a := account{email: email, role: role}
	a.token, a.id = s.login(t, email, password)
	return a
}

// login signs in and returns the token and the account id.
func (s *stack) login(t *testing.T, email, pw string) (string, uuid.UUID) {
	t.Helper()
	r := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": email, "password": pw}, nil)
	if r.status != http.StatusOK {
		t.Fatalf("login %s: %s", email, r.describe())
	}
	var out struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID uuid.UUID `json:"id"`
		} `json:"user"`
	}
	r.decode(t, &out)
	return out.AccessToken, out.User.ID
}

// response is what the gateway answered.
type response struct {
	status int
	header http.Header
	body   []byte
}

// raw performs one request. body is marshalled as JSON when it is not
// nil and not already raw bytes.
func (s *stack) raw(method, path, token string, body any, headers map[string]string) (response, error) {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader, contentType = bytes.NewReader(b), "application/json"
	case string:
		reader, contentType = strings.NewReader(b), "application/json"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return response{}, err
		}
		reader, contentType = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, s.gateway+path, reader)
	if err != nil {
		return response{}, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// do is raw, failing the test on a transport error.
func (s *stack) do(t *testing.T, method, path, token string, body any, headers map[string]string) response {
	t.Helper()
	r, err := s.raw(method, path, token, body, headers)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode response: %v\n%s", err, r.body)
	}
}

// errorEnvelope is the API's error body.
type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Details   []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"details"`
	} `json:"error"`
}

func (r response) envelope() errorEnvelope {
	var e errorEnvelope
	_ = json.Unmarshal(r.body, &e)
	return e
}

// code is the error code of an error response, or "".
func (r response) code() string { return r.envelope().Error.Code }

// detail is the message of the named field in the error details, or "".
func (r response) detail(field string) string {
	for _, d := range r.envelope().Error.Details {
		if d.Field == field {
			return d.Message
		}
	}
	return ""
}

func (r response) requestID() string { return r.header.Get("X-Request-ID") }

func (r response) describe() string {
	body := string(r.body)
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	return fmt.Sprintf("HTTP %d %s", r.status, body)
}

// expect fails unless the response has the status and, when given, the
// error code.
func (r response) expect(t *testing.T, status int, code string) response {
	t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		t.Fatalf("got %s, want %d %s", r.describe(), status, code)
	}
	return r
}

// waitFor polls until cond holds or the deadline passes.
func (s *stack) waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ready reports the gateway's readiness status code, or 0 when it does not
// answer at all.
func (s *stack) ready() int {
	r, err := s.raw(http.MethodGet, "/ready", "", nil, nil)
	if err != nil {
		return 0
	}
	return r.status
}

// patient creates a patient as the operator and returns its id.
func (s *stack) patient(t *testing.T, suffix string) uuid.UUID {
	t.Helper()
	r := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, map[string]string{
		"external_reference": "e2e-" + s.runID + "-" + suffix, "date_of_birth": "1984-02-29", "sex": "FEMALE",
	}, nil).expect(t, http.StatusCreated, "")
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	r.decode(t, &out)
	return out.ID
}

// reading is one measurement in the API's shape.
func (s *stack) reading(patient uuid.UUID, typ string, value float64, unit string, minute int, source string) map[string]any {
	return map[string]any{
		"patient_id": patient.String(), "type": typ, "value": value, "unit": unit,
		"recorded_at": s.base.Add(time.Duration(minute) * time.Minute).Format(time.RFC3339),
		"source":      source,
	}
}

// heartRateSeries is sixty readings a minute apart with one at 185 bpm,
// above the critical bound the stack's processor is configured with, so
// that the anomaly detector has exactly one thing to find.
func (s *stack) heartRateSeries(patient uuid.UUID, source string) []map[string]any {
	items := make([]map[string]any, 60)
	for i := range items {
		value := 70.0 + float64(i%5)
		if i == 30 {
			value = 185
		}
		items[i] = s.reading(patient, "HEART_RATE", value, "bpm", i, source)
	}
	return items
}

// jobRequest asks for the heart-rate statistics of a patient over the
// suite's series.
func (s *stack) jobRequest(patient uuid.UUID) map[string]any {
	return map[string]any{
		"patient_id": patient.String(), "measurement_types": []string{"HEART_RATE"},
		"windows": []string{"5m", "1h"}, "percentiles": []int{50, 95},
	}
}

// query runs one row-returning statement against the stack's database.
func (s *stack) query(t *testing.T, sql string, args ...any) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		row := map[string]any{}
		for i, f := range rows.FieldDescriptions() {
			row[f.Name] = values[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// count is a scalar count query.
func (s *stack) count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	rows := s.query(t, sql, args...)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows", len(rows))
	}
	for _, v := range rows[0] {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	t.Fatalf("count query returned no integer: %v", rows[0])
	return 0
}
