package idempotency

import (
	"strings"
	"testing"
)

func TestValidKey(t *testing.T) {
	for _, ok := range []string{"a", "order-2026-09-06:1", "A.b_c", strings.Repeat("k", MaxKeyLength)} {
		if !ValidKey(ok) {
			t.Errorf("ValidKey(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", " ", "a b", "a/b", "ключ", strings.Repeat("k", MaxKeyLength+1), "a\n", "a\x00"} {
		if ValidKey(bad) {
			t.Errorf("ValidKey(%q) = true", bad)
		}
	}
}

func TestFingerprintIsSensitiveToEveryPart(t *testing.T) {
	base := Fingerprint("POST", "/api/v1/measurements", []byte(`{"a":1}`))
	if len(base) != 64 {
		t.Errorf("fingerprint length = %d", len(base))
	}
	if base != Fingerprint("POST", "/api/v1/measurements", []byte(`{"a":1}`)) {
		t.Error("not deterministic")
	}
	for name, other := range map[string]string{
		"method": Fingerprint("PUT", "/api/v1/measurements", []byte(`{"a":1}`)),
		"path":   Fingerprint("POST", "/api/v1/measurements/batch", []byte(`{"a":1}`)),
		"body":   Fingerprint("POST", "/api/v1/measurements", []byte(`{"a":2}`)),
		"space":  Fingerprint("POST", "/api/v1/measurements", []byte(`{"a": 1}`)),
		"split":  Fingerprint("POST", "/api/v1/measurement", []byte(`s{"a":1}`)),
	} {
		if other == base {
			t.Errorf("%s change did not alter the fingerprint", name)
		}
	}
}

func TestErrorsAreExplicit(t *testing.T) {
	if e := ErrInvalidKey(); e.Code != CodeInvalidKey || len(e.Details) != 1 || e.Details[0].Field != Header {
		t.Errorf("ErrInvalidKey = %+v", e)
	}
	if e := ErrKeyReused(); e.Code != CodeKeyReused {
		t.Errorf("ErrKeyReused = %+v", e)
	}
	if e := ErrInProgress(); e.Code != CodeInProgress {
		t.Errorf("ErrInProgress = %+v", e)
	}
}
