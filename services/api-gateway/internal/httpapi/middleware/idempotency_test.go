package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency/idempotencytest"
)

type countingHandler struct {
	calls  int
	status int
	body   string
	panics bool
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls++
	if h.panics {
		panic("boom")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(h.body))
}

func idempotentRequest(body, key string, user uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/things", strings.NewReader(body))
	if key != "" {
		req.Header.Set(idempotency.Header, key)
	}
	return req.WithContext(auth.NewContext(context.Background(), auth.Principal{UserID: user, Role: domain.RoleOperator}))
}

func TestIdempotencyReplaysCompletedResponses(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	h := &countingHandler{status: http.StatusCreated, body: `{"id":"1"}`}
	mw := Chain(h, RequestID(), Idempotency(store, time.Hour, nil, logger))
	user := uuid.New()

	first := httptest.NewRecorder()
	mw.ServeHTTP(first, idempotentRequest(`{"a":1}`, "k1", user))
	second := httptest.NewRecorder()
	mw.ServeHTTP(second, idempotentRequest(`{"a":1}`, "k1", user))

	if h.calls != 1 {
		t.Errorf("handler ran %d times, want 1", h.calls)
	}
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || second.Body.String() != `{"id":"1"}` {
		t.Errorf("responses: %d %q / %d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if first.Header().Get(ReplayedHeader) != "" || second.Header().Get(ReplayedHeader) != "true" {
		t.Errorf("replayed headers: %q / %q", first.Header().Get(ReplayedHeader), second.Header().Get(ReplayedHeader))
	}

	// The handler still sees the body the client sent.
	echo := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		n, _ := r.Body.Read(b)
		_, _ = w.Write(b[:n])
	}), Idempotency(idempotencytest.NewMemoryStore(), time.Hour, nil, logger))
	rec := httptest.NewRecorder()
	echo.ServeHTTP(rec, idempotentRequest(`{"a":1}`, "k2", user))
	if rec.Body.String() != `{"a":1}` {
		t.Errorf("handler saw body %q", rec.Body.String())
	}
}

func TestIdempotencyRefusesMismatchAndInProgress(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	h := &countingHandler{status: http.StatusCreated, body: `{}`}
	mw := Chain(h, Idempotency(store, time.Hour, nil, logger))
	user := uuid.New()
	mw.ServeHTTP(httptest.NewRecorder(), idempotentRequest(`{"a":1}`, "k", user))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{"a":2}`, "k", user))
	if rec.Code != http.StatusUnprocessableEntity || decodeErrorCode(t, rec) != idempotency.CodeKeyReused {
		t.Errorf("mismatch: %d %s", rec.Code, rec.Body.String())
	}

	// An in-progress record: claim a key without completing it.
	_, _, _ = store.Begin(context.Background(), idempotency.Request{
		UserID: user, Method: "POST", Path: "/api/v1/things", Key: "busy",
		Fingerprint: idempotency.Fingerprint("POST", "/api/v1/things", []byte(`{}`)), ExpiresAt: time.Now().Add(time.Hour),
	})
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{}`, "busy", user))
	if rec.Code != http.StatusConflict || decodeErrorCode(t, rec) != idempotency.CodeInProgress || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("in progress: %d %s Retry-After=%q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	if h.calls != 1 {
		t.Errorf("handler ran %d times", h.calls)
	}
}

func TestIdempotencyServerErrorsAndPanicsLeaveNoRecord(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	failing := &countingHandler{status: http.StatusInternalServerError, body: `{"error":{}}`}
	mw := Chain(failing, Idempotency(store, time.Hour, nil, logger))
	user := uuid.New()

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{}`, "k", user))
	if rec.Code != http.StatusInternalServerError || store.Len() != 0 {
		t.Errorf("5xx: status %d, records %d", rec.Code, store.Len())
	}
	failing.status = http.StatusCreated
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{}`, "k", user))
	if rec.Code != http.StatusCreated || failing.calls != 2 || store.Len() != 1 {
		t.Errorf("retry after 5xx: %d, calls %d, records %d", rec.Code, failing.calls, store.Len())
	}

	panicking := &countingHandler{panics: true}
	mw = Chain(panicking, Recover(logger), Idempotency(store, time.Hour, nil, logger))
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{}`, "p", user))
	if rec.Code != http.StatusInternalServerError || store.Len() != 1 {
		t.Errorf("panic: status %d, records %d (want only k)", rec.Code, store.Len())
	}
}

func TestIdempotencyExpiredRecordsAreReplaced(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	h := &countingHandler{status: http.StatusCreated, body: `{"n":1}`}
	mw := Chain(h, Idempotency(store, -time.Second, nil, logger)) // records expire immediately
	user := uuid.New()
	mw.ServeHTTP(httptest.NewRecorder(), idempotentRequest(`{}`, "k", user))
	h.body = `{"n":2}`
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{}`, "k", user))
	if h.calls != 2 || rec.Body.String() != `{"n":2}` || rec.Header().Get(ReplayedHeader) != "" {
		t.Errorf("expired record not replaced: calls %d body %s", h.calls, rec.Body.String())
	}
}

func TestIdempotencyWithoutKeyOrPrincipal(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	h := &countingHandler{status: http.StatusCreated, body: `{}`}
	mw := Chain(h, Idempotency(store, time.Hour, nil, logger))

	mw.ServeHTTP(httptest.NewRecorder(), idempotentRequest(`{}`, "", uuid.New()))
	mw.ServeHTTP(httptest.NewRecorder(), idempotentRequest(`{}`, "", uuid.New()))
	if h.calls != 2 || store.Len() != 0 {
		t.Errorf("without key: calls %d, records %d", h.calls, store.Len())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/things", strings.NewReader(`{}`))
	req.Header.Set(idempotency.Header, "k")
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || h.calls != 2 {
		t.Errorf("without principal: %d, calls %d", rec.Code, h.calls)
	}

	store.Err = errors.New("db down")
	rec = httptest.NewRecorder()
	mw.ServeHTTP(rec, idempotentRequest(`{}`, "k", uuid.New()))
	if rec.Code != http.StatusInternalServerError || h.calls != 2 {
		t.Errorf("store failure: %d, calls %d", rec.Code, h.calls)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Error("store failure leaked to the client")
	}
}

// ------------------------------------------- the client going away

// cancellableRequest is an idempotent request whose context a handler can
// end, which is what a client hanging up or the request timeout firing does
// to a real request.
func cancellableRequest(body, key string, user uuid.UUID) (*http.Request, context.CancelFunc) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/things", strings.NewReader(body))
	req.Header.Set(idempotency.Header, key)
	ctx, cancel := context.WithCancel(auth.NewContext(context.Background(),
		auth.Principal{UserID: user, Role: domain.RoleOperator}))
	return req.WithContext(ctx), cancel
}

// A request that failed must release its key even if the client has already
// gone: otherwise the retry the failure invites is refused as in progress
// for the whole time to live.
func TestAFailedRequestReleasesItsKeyEvenWhenTheClientHasGoneAway(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	user := uuid.New()
	req, cancel := cancellableRequest(`{"a":1}`, "abandoned", user)

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel() // the client hangs up while the handler is running
		w.WriteHeader(http.StatusBadGateway)
	})
	Chain(handler, Idempotency(store, time.Hour, nil, logger)).ServeHTTP(httptest.NewRecorder(), req)

	if store.Len() != 0 {
		t.Fatalf("%d records left after a failure; the key stays claimed and every retry is refused as in progress", store.Len())
	}
}

// The operation happened, so its response must be stored even if nobody is
// left to read it: the retry that follows a lost connection is exactly the
// case idempotency exists for.
func TestASuccessfulResponseIsStoredEvenWhenTheClientHasGoneAway(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	user := uuid.New()
	req, cancel := cancellableRequest(`{"a":1}`, "hung-up", user)

	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		cancel()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	})
	mw := Chain(handler, Idempotency(store, time.Hour, nil, logger))
	mw.ServeHTTP(httptest.NewRecorder(), req)

	// The client retries on a fresh connection.
	retry := httptest.NewRecorder()
	mw.ServeHTTP(retry, idempotentRequest(`{"a":1}`, "hung-up", user))

	if calls != 1 {
		t.Errorf("the handler ran %d times, want 1: the operation was repeated", calls)
	}
	if retry.Code != http.StatusCreated {
		t.Fatalf("the retry got %d (%s), want the stored 201", retry.Code, retry.Body.String())
	}
	if retry.Body.String() != `{"id":"1"}` {
		t.Errorf("the retry got body %q", retry.Body.String())
	}
	if retry.Header().Get(ReplayedHeader) != "true" {
		t.Error("the retry was not marked as a replay")
	}
}

// A replay must be the same response, not merely the same status and body:
// a client that follows Location on the first response must be able to
// follow it on the replay.
func TestAReplayCarriesTheHeadersOfTheFirstResponse(t *testing.T) {
	logger, _ := testLogger()
	store := idempotencytest.NewMemoryStore()
	user := uuid.New()
	const location = "/api/v1/things/9f1c"

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"9f1c"}`))
	})
	mw := Chain(handler, Idempotency(store, time.Hour, nil, logger))

	first := httptest.NewRecorder()
	mw.ServeHTTP(first, idempotentRequest(`{"a":1}`, "located", user))
	if first.Header().Get("Location") != location {
		t.Fatalf("the first response has Location %q; the test premise is wrong", first.Header().Get("Location"))
	}

	replayed := httptest.NewRecorder()
	mw.ServeHTTP(replayed, idempotentRequest(`{"a":1}`, "located", user))
	if got := replayed.Header().Get("Location"); got != location {
		t.Errorf("the replay has Location %q, want %q: it is not the same response", got, location)
	}
}
