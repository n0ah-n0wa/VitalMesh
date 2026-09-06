package request

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
)

type payload struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

func jsonRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func assertDomainError(t *testing.T, err error, kind domain.Kind, code string) *domain.Error {
	t.Helper()
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		t.Fatalf("err = %v (%T), want *domain.Error", err, err)
	}
	if domErr.Kind != kind || domErr.Code != code {
		t.Fatalf("err = %s/%s, want %s/%s", domErr.Kind, domErr.Code, kind, code)
	}
	return domErr
}

func TestDecodeJSONAcceptsValidBody(t *testing.T) {
	var dst payload
	r := jsonRequest(`{"name":"x","size":3}`)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if err := DecodeJSON(r, &dst); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if dst != (payload{Name: "x", Size: 3}) {
		t.Errorf("dst = %+v", dst)
	}
}

func TestDecodeJSONRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		ctype string
		kind  domain.Kind
		code  string
		field string
	}{
		{"missing content type", `{}`, "", domain.KindUnsupportedMedia, CodeUnsupportedMedia, ""},
		{"wrong content type", `{}`, "text/plain", domain.KindUnsupportedMedia, CodeUnsupportedMedia, ""},
		{"empty body", ``, "application/json", domain.KindInvalid, CodeEmptyBody, ""},
		{"malformed", `{"name":`, "application/json", domain.KindInvalid, CodeInvalidJSON, ""},
		{"syntax error", `{"name" "x"}`, "application/json", domain.KindInvalid, CodeInvalidJSON, ""},
		{"wrong field type", `{"size":"big"}`, "application/json", domain.KindInvalid, CodeInvalidJSON, "size"},
		{"wrong top-level type", `[1,2]`, "application/json", domain.KindInvalid, CodeInvalidJSON, "body"},
		{"unknown field", `{"colour":"red"}`, "application/json", domain.KindInvalid, CodeInvalidJSON, "colour"},
		{"multiple values", `{} {}`, "application/json", domain.KindInvalid, CodeInvalidJSON, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.ctype != "" {
				r.Header.Set("Content-Type", tc.ctype)
			}
			var dst payload
			domErr := assertDomainError(t, DecodeJSON(r, &dst), tc.kind, tc.code)

			if tc.field == "" {
				if len(domErr.Details) != 0 {
					t.Errorf("unexpected details %+v", domErr.Details)
				}
			} else if len(domErr.Details) != 1 || domErr.Details[0].Field != tc.field {
				t.Errorf("details = %+v, want field %q", domErr.Details, tc.field)
			}
			if strings.Contains(domErr.Message, "json:") || strings.Contains(domErr.Message, "main.") {
				t.Errorf("message leaks decoder internals: %q", domErr.Message)
			}
		})
	}
}

func TestDecodeJSONReportsOversizedBody(t *testing.T) {
	rec := httptest.NewRecorder()
	r := jsonRequest(`{"name":"` + strings.Repeat("x", 100) + `"}`)
	r.Body = http.MaxBytesReader(rec, r.Body, 32)

	var dst payload
	domErr := assertDomainError(t, DecodeJSON(r, &dst), domain.KindTooLarge, CodeBodyTooLarge)
	if !strings.Contains(domErr.Message, "32 bytes") {
		t.Errorf("message = %q, want the limit", domErr.Message)
	}
}

func TestDecodeJSONPassesThroughProgrammingErrors(t *testing.T) {
	var notAPointer payload
	err := DecodeJSON(jsonRequest(`{}`), notAPointer)
	var domErr *domain.Error
	if err == nil || errors.As(err, &domErr) {
		t.Fatalf("err = %v, want a raw (internal) error", err)
	}
}

func TestPagination(t *testing.T) {
	limits := pagination.Limits{Default: 20, Max: 100}
	cases := []struct {
		name  string
		query string
		want  pagination.Request
		field string
	}{
		{"defaults", "", pagination.Request{Limit: 20}, ""},
		{"explicit", "limit=5&cursor=abc", pagination.Request{Limit: 5, Cursor: "abc"}, ""},
		{"max", "limit=100", pagination.Request{Limit: 100}, ""},
		{"not a number", "limit=five", pagination.Request{}, "limit"},
		{"zero", "limit=0", pagination.Request{}, "limit"},
		{"too big", "limit=101", pagination.Request{}, "limit"},
		{"cursor too long", "cursor=" + strings.Repeat("a", pagination.MaxCursorLength+1), pagination.Request{}, "cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			got, err := Pagination(r, limits)
			if tc.field != "" {
				domErr := assertDomainError(t, err, domain.KindValidation, "VALIDATION_FAILED")
				if len(domErr.Details) != 1 || domErr.Details[0].Field != tc.field {
					t.Errorf("details = %+v, want field %q", domErr.Details, tc.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pagination: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Guard against a body that is not fully consumed hiding a size failure.
func TestDecodeJSONWithUnexpectedEOF(t *testing.T) {
	r := jsonRequest(`{"name":"x"`)
	r.Body = io.NopCloser(strings.NewReader(`{"name":"x"`))
	var dst payload
	assertDomainError(t, DecodeJSON(r, &dst), domain.KindInvalid, CodeInvalidJSON)
}
