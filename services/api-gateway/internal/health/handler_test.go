package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEndpointsReportOK(t *testing.T) {
	h := NewHandler("api-gateway", "abc123")

	endpoints := map[string]http.HandlerFunc{
		"live":  h.Live,
		"ready": h.Ready,
	}

	for name, fn := range endpoints {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fn(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content-type = %q, want application/json", got)
			}

			var body Response
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			want := Response{Status: "ok", Service: "api-gateway", Version: "abc123"}
			if body != want {
				t.Fatalf("body = %+v, want %+v", body, want)
			}
		})
	}
}
