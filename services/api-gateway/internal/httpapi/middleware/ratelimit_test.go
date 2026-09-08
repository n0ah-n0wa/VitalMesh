package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/ratelimit"
)

// These tests use no Redis at all, which is also the degraded configuration:
// the limiter then decides from this replica's own counter and the
// middleware must behave identically.

func rateLimitConfig() config.RateLimit {
	return config.RateLimit{
		Enabled:       true,
		Window:        time.Minute,
		Anonymous:     2,
		Authenticated: 4,
		Admin:         6,
	}
}

func rateLimited(t *testing.T, cfg config.RateLimit) (http.Handler, *int) {
	t.Helper()
	logger, _ := testLogger()
	limiter := ratelimit.New(cfg, nil, logger, ratelimit.Options{})
	served := 0
	handler := RateLimit(limiter, logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))
	return handler, &served
}

func anonymousRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil)
	r.RemoteAddr = "198.51.100.4:4444"
	return r
}

func TestEveryResponseCarriesTheBudget(t *testing.T) {
	handler, _ := rateLimited(t, rateLimitConfig())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, anonymousRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(RateLimitLimitHeader); got != "2" {
		t.Errorf("%s = %q, want %q", RateLimitLimitHeader, got, "2")
	}
	if got := rec.Header().Get(RateLimitRemainingHeader); got != "1" {
		t.Errorf("%s = %q, want %q", RateLimitRemainingHeader, got, "1")
	}
	reset, err := strconv.Atoi(rec.Header().Get(RateLimitResetHeader))
	if err != nil {
		t.Fatalf("%s = %q: %v", RateLimitResetHeader, rec.Header().Get(RateLimitResetHeader), err)
	}
	if reset <= 0 || reset > 60 {
		t.Errorf("%s = %d, want it inside the window", RateLimitResetHeader, reset)
	}
}

func TestACallerOverBudgetIsRefusedWithRetryAfter(t *testing.T) {
	handler, served := rateLimited(t, rateLimitConfig())

	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, anonymousRequest())
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, anonymousRequest())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if *served != 2 {
		t.Errorf("the handler ran %d times, want 2: a refused request must not reach it", *served)
	}
	if got := rec.Header().Get(RateLimitRemainingHeader); got != "0" {
		t.Errorf("%s = %q, want %q", RateLimitRemainingHeader, got, "0")
	}
	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q: %v", rec.Header().Get("Retry-After"), err)
	}
	if retryAfter < 1 {
		t.Errorf("Retry-After = %d, want at least a second", retryAfter)
	}

	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("body is not an error document: %v", err)
	}
	if body.Error.Code != CodeRateLimited {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeRateLimited)
	}
}

// The limit follows the caller, not the address: an authenticated request
// spends its own budget even from an address that is already exhausted.
func TestAnAuthenticatedCallerHasItsOwnBudget(t *testing.T) {
	handler, _ := rateLimited(t, rateLimitConfig())

	for i := 0; i < 3; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), anonymousRequest())
	}

	authenticated := anonymousRequest()
	authenticated = authenticated.WithContext(auth.NewContext(authenticated.Context(),
		auth.Principal{UserID: uuid.New(), Role: domain.RoleOperator}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authenticated)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the address's exhausted budget was applied to an account", rec.Code)
	}
	if got := rec.Header().Get(RateLimitLimitHeader); got != "4" {
		t.Errorf("%s = %q, want the authenticated limit of 4", RateLimitLimitHeader, got)
	}
}

func TestARefusalIsLoggedWithoutTheCallersIdentifier(t *testing.T) {
	cfg := rateLimitConfig()
	cfg.Anonymous = 1
	logger, buf := testLogger()
	limiter := ratelimit.New(cfg, nil, logger, ratelimit.Options{})
	handler := RateLimit(limiter, logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), anonymousRequest())
	buf.Reset()
	handler.ServeHTTP(httptest.NewRecorder(), anonymousRequest())

	line := buf.String()
	if line == "" {
		t.Fatal("a refusal was not logged")
	}
	if got := lastLogLine(t, buf)["message"]; got != "rate limit exceeded" {
		t.Errorf("msg = %v", got)
	}
	// The address is what identifies an anonymous caller; it is not logged
	// as part of the rate-limit record.
	if strings.Contains(line, "198.51.100.4") {
		t.Errorf("the log carried the caller's address:\n%s", line)
	}
}

func TestDisabledRateLimitingAllowsEverything(t *testing.T) {
	cfg := rateLimitConfig()
	cfg.Enabled = false
	handler, served := rateLimited(t, cfg)

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, anonymousRequest())
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
	if *served != 10 {
		t.Errorf("the handler ran %d times, want 10", *served)
	}
}
