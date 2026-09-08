//go:build integration

package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

// Idempotency under concurrency, against a real PostgreSQL and a real
// Redis. The property under test is always the same one: whatever the
// interleaving, the operation happens at most once and no caller is given a
// wrong answer.
//
// Each test runs in three configurations, because the Redis lock is only a
// fast path. PostgreSQL's unique constraint on (user, method, path, key) is
// what actually serialises replays, so the outcomes must be identical when
// Redis is live, unreachable, or absent (SPECIFICATIONS.md sections 23 and
// 24, OQ-10).

// redisConfigurations is the three ways a deployment can be placed relative
// to Redis.
func redisConfigurations(t *testing.T) []struct {
	name     string
	redisURL string
} {
	t.Helper()
	return []struct {
		name     string
		redisURL string
	}{
		{"with redis", redistest.URL(t)},
		{"with redis unreachable", "redis://127.0.0.1:1"},
		{"with no redis configured", ""},
	}
}

func newPatientBody(reference string) string {
	return `{"external_reference":"` + reference + `","date_of_birth":"1990-01-01","sex":"UNKNOWN"}`
}

// patientsWithReference counts the rows a write actually created, which is
// the only claim about duplicate work worth making.
func patientsWithReference(t *testing.T, h *redisHarness, reference string) int {
	t.Helper()
	var count int
	err := h.pool.QueryRow(h.ctx,
		`SELECT count(*) FROM patients WHERE external_reference = $1`, reference).Scan(&count)
	if err != nil {
		t.Fatalf("counting patients: %v", err)
	}
	return count
}

// concurrentPosts fires n identical requests at once and returns their
// responses. They are released together so that they genuinely contend.
func concurrentPosts(h *redisHarness, n int, path, body, key string) []*httptest.ResponseRecorder {
	responses := make([]*httptest.ResponseRecorder, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses[i] = h.do(http.MethodPost, path, body, h.token,
				map[string]string{idempotency.Header: key})
		}()
	}
	close(start)
	wg.Wait()
	return responses
}

// ------------------------------------------------- concurrent duplicates

// The case idempotency exists for: a client retries because it did not hear
// back, and both attempts are in flight at once. Exactly one may do the
// work; every other caller gets either that same answer or a plain "still
// running", and never a second resource.
func TestConcurrentDuplicateRequestsCreateExactlyOneResource(t *testing.T) {
	for _, tc := range redisConfigurations(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := newRedisHarness(t, tc.redisURL, map[string]string{
				"REDIS_NAMESPACE": "conc-" + strconv.FormatInt(time.Now().UnixNano(), 36),
			})
			reference := "conc-" + uuid.NewString()
			body := newPatientBody(reference)

			const callers = 12
			responses := concurrentPosts(h, callers, "/api/v1/patients", body, "concurrent-key")

			executed, replayed, inProgress := 0, 0, 0
			for i, rec := range responses {
				switch {
				case rec.Code == http.StatusCreated && rec.Header().Get(middleware.ReplayedHeader) == "true":
					replayed++
				case rec.Code == http.StatusCreated:
					executed++
				case rec.Code == http.StatusConflict:
					inProgress++
					if code := decodeCode(t, rec); code != idempotency.CodeInProgress {
						t.Errorf("caller %d: 409 with code %s, want %s", i, code, idempotency.CodeInProgress)
					}
					if rec.Header().Get("Retry-After") == "" {
						t.Errorf("caller %d: a 409 without Retry-After leaves the client guessing", i)
					}
				default:
					t.Errorf("caller %d: %d %s", i, rec.Code, rec.Body.String())
				}
			}

			if executed != 1 {
				t.Errorf("%d callers ran the operation, want exactly 1", executed)
			}
			if executed+replayed+inProgress != callers {
				t.Errorf("accounted for %d of %d responses", executed+replayed+inProgress, callers)
			}
			// The claim that matters: the work happened once.
			if got := patientsWithReference(t, h, reference); got != 1 {
				t.Errorf("%d patients created, want exactly 1", got)
			}
		})
	}
}

// A replay is the same response, so every caller that got one must have got
// the same document and the same Location as the caller that did the work.
func TestConcurrentReplaysAllReturnTheSameResponse(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"REDIS_NAMESPACE": "same-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	body := newPatientBody("same-" + uuid.NewString())

	responses := concurrentPosts(h, 8, "/api/v1/patients", body, "same-answer-key")
	// Once the first request has finished, every later one replays.
	for i := 0; i < 4; i++ {
		responses = append(responses, h.do(http.MethodPost, "/api/v1/patients", body, h.token,
			map[string]string{idempotency.Header: "same-answer-key"}))
	}

	var reference *httptest.ResponseRecorder
	for _, rec := range responses {
		if rec.Code == http.StatusCreated {
			reference = rec
			break
		}
	}
	if reference == nil {
		t.Fatal("no caller succeeded")
	}
	if reference.Header().Get("Location") == "" {
		t.Fatal("the created response has no Location; the test premise is wrong")
	}

	for i, rec := range responses {
		if rec.Code != http.StatusCreated {
			continue
		}
		if !sameJSON(t, reference.Body.Bytes(), rec.Body.Bytes()) {
			t.Errorf("caller %d got a different document: %s", i, rec.Body.String())
		}
		if got := rec.Header().Get("Location"); got != reference.Header().Get("Location") {
			t.Errorf("caller %d got Location %q, want %q", i, got, reference.Header().Get("Location"))
		}
	}
}

// Concurrent requests that share a key but not a body are a client error,
// not a race: at most one may do work and the rest must be told the key is
// taken, never quietly given someone else's result.
func TestConcurrentRequestsWithDifferentBodiesNeverBothSucceed(t *testing.T) {
	for _, tc := range redisConfigurations(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := newRedisHarness(t, tc.redisURL, map[string]string{
				"REDIS_NAMESPACE": "clash-" + strconv.FormatInt(time.Now().UnixNano(), 36),
			})

			const callers = 10
			references := make([]string, callers)
			responses := make([]*httptest.ResponseRecorder, callers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range responses {
				references[i] = "clash-" + uuid.NewString()
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					responses[i] = h.do(http.MethodPost, "/api/v1/patients", newPatientBody(references[i]),
						h.token, map[string]string{idempotency.Header: "one-key-many-bodies"})
				}()
			}
			close(start)
			wg.Wait()

			created := 0
			for i, rec := range responses {
				switch rec.Code {
				case http.StatusCreated:
					created++
					if rec.Header().Get(middleware.ReplayedHeader) == "true" {
						t.Errorf("caller %d was served another request's response as its own", i)
					}
				case http.StatusUnprocessableEntity:
					if code := decodeCode(t, rec); code != idempotency.CodeKeyReused {
						t.Errorf("caller %d: 422 with code %s, want %s", i, code, idempotency.CodeKeyReused)
					}
				case http.StatusConflict:
					// Still running: a legitimate answer under contention.
				default:
					t.Errorf("caller %d: %d %s", i, rec.Code, rec.Body.String())
				}
			}
			if created != 1 {
				t.Errorf("%d callers created a resource, want exactly 1", created)
			}

			// Under contention most losers are simply told the key is busy,
			// so the conflict itself is checked once the winner has settled:
			// a different body under a used key is refused, never answered
			// with the other request's result.
			late := h.do(http.MethodPost, "/api/v1/patients", newPatientBody("clash-late-"+uuid.NewString()),
				h.token, map[string]string{idempotency.Header: "one-key-many-bodies"})
			if late.Code != http.StatusUnprocessableEntity {
				t.Errorf("a different body under a used key = %d, want 422: %s", late.Code, late.Body.String())
			} else if code := decodeCode(t, late); code != idempotency.CodeKeyReused {
				t.Errorf("conflicting reuse reported %s, want %s", code, idempotency.CodeKeyReused)
			}

			// Whatever happened, only one body was ever acted on.
			total := 0
			for _, reference := range references {
				total += patientsWithReference(t, h, reference)
			}
			if total != 1 {
				t.Errorf("%d patients created, want exactly 1", total)
			}
		})
	}
}

// Different keys must not contend: the lock and the record are per key, so
// concurrent unrelated writes all go through.
func TestConcurrentRequestsUnderDifferentKeysAllSucceed(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"REDIS_NAMESPACE": "distinct-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})

	const callers = 10
	responses := make([]*httptest.ResponseRecorder, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses[i] = h.do(http.MethodPost, "/api/v1/patients",
				newPatientBody("distinct-"+uuid.NewString()), h.token,
				map[string]string{idempotency.Header: "distinct-key-" + strconv.Itoa(i)})
		}()
	}
	close(start)
	wg.Wait()

	for i, rec := range responses {
		if rec.Code != http.StatusCreated {
			t.Errorf("caller %d under its own key = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
}

// ------------------------------------------------------- failed requests

// A request that failed must leave nothing behind: the key it claimed has
// to be usable again, or the retry the failure invites is refused for the
// whole time to live.
func TestAFailedRequestLeavesItsKeyUsable(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"REDIS_NAMESPACE": "failed-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	key := map[string]string{idempotency.Header: "reusable-after-failure"}

	// A body the handler rejects: a client error, which is a real outcome
	// and is stored, so it must replay rather than run again.
	bad := `{"external_reference":"","date_of_birth":"1990-01-01","sex":"UNKNOWN"}`
	first := h.do(http.MethodPost, "/api/v1/patients", bad, h.token, key)
	if first.Code < 400 || first.Code >= 500 {
		t.Fatalf("the invalid request = %d, want a 4xx: %s", first.Code, first.Body.String())
	}
	replay := h.do(http.MethodPost, "/api/v1/patients", bad, h.token, key)
	if replay.Code != first.Code {
		t.Errorf("the replay of a client error = %d, want %d", replay.Code, first.Code)
	}
	if replay.Header().Get(middleware.ReplayedHeader) != "true" {
		t.Error("a stored client error was not replayed")
	}

	// The record is COMPLETED, so it is not holding the key open.
	record, err := postgres.NewIdempotency(h.pool).Get(h.ctx, userIDFromToken(t, h),
		http.MethodPost, "/api/v1/patients", "reusable-after-failure")
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if record.Status != domain.IdempotencyCompleted {
		t.Errorf("record status = %s, want COMPLETED: the key is still held", record.Status)
	}
}

// ------------------------------------------------------------- retention

// Records must expire. The request path only discards an expired record for
// a key a client happens to present again, which for unique keys is never,
// so the sweep is what actually bounds the table.
func TestTheRetentionSweepRemovesExpiredRecordsAndKeepsLiveOnes(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	repo := postgres.NewIdempotency(h.pool)
	user := userIDFromToken(t, h)

	expired := make([]uuid.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		record, _, err := repo.Begin(h.ctx, postgres.NewIdempotencyKey{
			UserID: user, Method: http.MethodPost, Path: "/api/v1/patients",
			Key: "stale-" + strconv.Itoa(i), RequestFingerprint: "fp",
			ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("seeding a record: %v", err)
		}
		age(t, h, record.ID)
		expired = append(expired, record.ID)
	}
	live, _, err := repo.Begin(h.ctx, postgres.NewIdempotencyKey{
		UserID: user, Method: http.MethodPost, Path: "/api/v1/patients",
		Key: "fresh", RequestFingerprint: "fp", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("seeding a live record: %v", err)
	}

	store := postgres.NewIdempotencyStore(h.pool)
	collector := idempotency.NewCollector(store, time.Hour, testLoggerFor(t), idempotency.CollectorOptions{})
	removed, err := collector.Sweep(h.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed < int64(len(expired)) {
		t.Errorf("the sweep removed %d records, want at least the %d expired ones", removed, len(expired))
	}

	for _, id := range expired {
		if recordExists(t, h, id) {
			t.Errorf("an expired record survived the sweep: %s", id)
		}
	}
	if !recordExists(t, h, live.ID) {
		t.Error("the sweep removed a record that had not expired")
	}
}

// A sweep with nothing to do must be cheap and silent, since it runs on
// every replica for the life of the process.
func TestASweepWithNothingExpiredRemovesNothing(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	collector := idempotency.NewCollector(postgres.NewIdempotencyStore(h.pool), time.Hour,
		testLoggerFor(t), idempotency.CollectorOptions{})

	if removed, err := collector.Sweep(h.ctx); err != nil || removed != 0 {
		t.Errorf("Sweep on an empty table = %d, %v", removed, err)
	}
}

// Retention stops when the gateway does, rather than holding shutdown open.
func TestTheCollectorStopsWithItsContext(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	collector := idempotency.NewCollector(postgres.NewIdempotencyStore(h.pool),
		10*time.Millisecond, testLoggerFor(t), idempotency.CollectorOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		collector.Run(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the collector did not stop when its context ended")
	}
}

// After a record expires its key is free again, so a client reusing a key
// long afterwards gets a fresh execution rather than a stale answer.
func TestAKeyIsUsableAgainOnceItsRecordHasBeenSwept(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"IDEMPOTENCY_TTL": "1m",
		"REDIS_NAMESPACE": "reuse-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	key := map[string]string{idempotency.Header: "recycled-key"}

	first := h.do(http.MethodPost, "/api/v1/patients", newPatientBody("recycled-"+uuid.NewString()), h.token, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}

	// Age the record past its expiry, then sweep as the retention job does.
	var id uuid.UUID
	if err := h.pool.QueryRow(h.ctx,
		`SELECT id FROM idempotency_keys WHERE key = $1`, "recycled-key").Scan(&id); err != nil {
		t.Fatalf("finding the record: %v", err)
	}
	age(t, h, id)
	collector := idempotency.NewCollector(postgres.NewIdempotencyStore(h.pool), time.Hour,
		testLoggerFor(t), idempotency.CollectorOptions{})
	if _, err := collector.Sweep(h.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	second := h.do(http.MethodPost, "/api/v1/patients", newPatientBody("recycled-"+uuid.NewString()), h.token, key)
	if second.Code != http.StatusCreated {
		t.Fatalf("reusing a swept key = %d: %s", second.Code, second.Body.String())
	}
	if second.Header().Get(middleware.ReplayedHeader) == "true" {
		t.Error("a swept record was replayed")
	}
}

// ---------------------------------------------------------------- helpers

// age moves a record wholly into the past. Both timestamps move, because
// the schema insists a record expires after it was created, which is also
// why an expired record can only ever come from the passage of time.
func age(t *testing.T, h *redisHarness, id uuid.UUID) {
	t.Helper()
	tag, err := h.pool.Exec(h.ctx, `
		UPDATE idempotency_keys
		SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'
		WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("ageing a record: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("ageing a record touched %d rows", tag.RowsAffected())
	}
}

func recordExists(t *testing.T, h *redisHarness, id uuid.UUID) bool {
	t.Helper()
	var count int
	if err := h.pool.QueryRow(h.ctx,
		`SELECT count(*) FROM idempotency_keys WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("counting records: %v", err)
	}
	return count > 0
}

func userIDFromToken(t *testing.T, h *redisHarness) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.pool.QueryRow(h.ctx,
		`SELECT id FROM users WHERE email = $1`, "redis-op@example.com").Scan(&id); err != nil {
		t.Fatalf("reading the operator: %v", err)
	}
	return id
}

func decodeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error document: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

func testLoggerFor(t *testing.T) *slog.Logger {
	t.Helper()
	return logging.New(io.Discard, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"})
}
