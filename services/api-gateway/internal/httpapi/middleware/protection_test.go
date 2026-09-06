package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
)

func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v (body %q)", err, rec.Body.String())
	}
	return body.Error.Code
}

func TestSecureHeaders(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), SecureHeaders())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for key, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"Cache-Control":           "no-store",
	} {
		if got := rec.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must be left to the TLS terminator")
	}
}

func TestBodyLimitRejectsDeclaredLengthEarly(t *testing.T) {
	var bodyRead bool
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyRead = true
	}), BodyLimit(10))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 11)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if bodyRead {
		t.Error("handler ran for an oversized declared body")
	}
	if code := decodeErrorCode(t, rec); code != "REQUEST_BODY_TOO_LARGE" {
		t.Errorf("code = %q", code)
	}
}

func TestBodyLimitCapsUndeclaredBody(t *testing.T) {
	var readErr error
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}), BodyLimit(10))

	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(strings.NewReader(strings.Repeat("x", 11))))
	if req.ContentLength != -1 {
		t.Fatalf("test setup: ContentLength = %d, want unknown", req.ContentLength)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)

	var tooLarge *http.MaxBytesError
	if readErr == nil || !errorAs(readErr, &tooLarge) {
		t.Errorf("read error = %v, want *http.MaxBytesError", readErr)
	}
}

func TestBodyLimitAllowsSmallBody(t *testing.T) {
	var got string
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}), BodyLimit(10))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("tiny")))
	if got != "tiny" || rec.Code != http.StatusOK {
		t.Errorf("body = %q status = %d", got, rec.Code)
	}
}

func TestTimeoutPassesCompletedResponseThrough(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("made"))
	}), Timeout(time.Second))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusCreated || rec.Body.String() != "made" || rec.Header().Get("X-Custom") != "yes" {
		t.Errorf("response altered: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestTimeoutDefaultsStatusTo200(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), Timeout(time.Second))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestTimeoutRespondsWhenHandlerIsSlow(t *testing.T) {
	handlerDone := make(chan struct{})
	release := make(chan struct{}) // closed once the middleware has responded
	var deadlineSeen bool
	var lateWriteErr error
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		<-r.Context().Done()
		deadlineSeen = r.Context().Err() != nil
		<-release
		_, lateWriteErr = w.Write([]byte("too late"))
	}), Timeout(30*time.Millisecond))

	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	close(release) // ServeHTTP has returned, so the 504 is already written
	<-handlerDone

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("response took %s, timeout not enforced", elapsed)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != "REQUEST_TIMEOUT" {
		t.Errorf("code = %q", code)
	}
	if strings.Contains(rec.Body.String(), "too late") {
		t.Error("late handler output reached the client")
	}
	if !deadlineSeen {
		t.Error("handler did not observe the context deadline")
	}
	if lateWriteErr != http.ErrHandlerTimeout {
		t.Errorf("late write error = %v, want http.ErrHandlerTimeout", lateWriteErr)
	}
}

func TestTimeoutPropagatesPanics(t *testing.T) {
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), Timeout(time.Second))

	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
