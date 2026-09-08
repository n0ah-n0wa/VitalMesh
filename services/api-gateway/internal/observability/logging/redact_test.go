package logging

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/redact"
)

// fakeJWT has a real token's exact shape and is a credential for nothing.
// It is assembled here rather than written out as a literal so that the
// source file contains no token-shaped string: a secret scanner should flag
// every one it finds in this repository, and a fixture that had to be
// allow-listed would blunt that for the next real one.
var fakeJWT = buildFakeJWT()

func buildFakeJWT() string {
	segment := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	return segment(`{"alg":"HS256","typ":"JWT"}`) + "." +
		segment(`{"sub":"not-a-real-user"}`) + "." +
		segment("not-a-real-signature")
}

func jsonLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return New(&buf, config.Log{Format: config.LogFormatJSON, Level: slog.LevelDebug},
		Service{Name: "test", Version: "v0", Environment: "test"}), &buf
}

// record decodes the single line the logger wrote.
func record(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("expected one record, got:\n%s", line)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, line)
	}
	return out
}

// ------------------------------------------------------- the schema

// Every record carries the fields an operator needs to place it: when, how
// bad, which service, which deployment, and what happened.
func TestEveryRecordCarriesTheSchemaFields(t *testing.T) {
	logger, buf := jsonLogger()
	logger.Info("something happened", "count", 3)

	got := record(t, buf)
	for _, field := range []string{"timestamp", "level", "service", "version", "environment", "message"} {
		if _, ok := got[field]; !ok {
			t.Errorf("record has no %q field:\n%v", field, got)
		}
	}
	if got["message"] != "something happened" {
		t.Errorf("message = %v", got["message"])
	}
	if got["service"] != "test" || got["environment"] != "test" {
		t.Errorf("identity = %v/%v", got["service"], got["environment"])
	}
	// Structured fields stay structured: a count is a number, not text.
	if got["count"] != float64(3) {
		t.Errorf("count = %#v, want the number 3", got["count"])
	}
}

// ------------------------------------------------- credentials by name

func TestCredentialsAreRedactedByAttributeName(t *testing.T) {
	const value = "hunter2-should-never-appear"
	names := []string{
		"password", "passwd", "pass", "password_hash", "secret", "token", "jwt",
		"authorization", "credential", "credentials", "cookie", "set_cookie",
		"session", "signature",
		// qualified forms
		"internal_token", "access_token", "refresh_token", "processor_token",
		"jwt_secret", "redis_password", "client_secret", "user_email",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			logger, buf := jsonLogger()
			logger.Info("attempt", name, value)

			got := record(t, buf)
			if got[name] != redact.Redacted {
				t.Errorf("%s = %v, want %q", name, got[name], redact.Redacted)
			}
			if strings.Contains(buf.String(), value) {
				t.Errorf("the value survived in the record:\n%s", buf.String())
			}
		})
	}
}

// Personal facts are not operational metadata. Identifiers are, and must
// survive, or a log is useless during an incident.
func TestPersonalFactsAreRedactedButIdentifiersSurvive(t *testing.T) {
	logger, buf := jsonLogger()
	logger.Info("patient touched",
		"email", "someone@example.com",
		"date_of_birth", "1970-01-01",
		"phone", "+1-555-0100",
		"external_reference", "MRN-88231",
		"patient_id", "6f1c8a2e-0000-4000-8000-000000000001",
		"user_id", "9b2d7c4a-0000-4000-8000-000000000002",
		"token_id", "jti-1234",
	)

	got := record(t, buf)
	for _, redacted := range []string{"email", "date_of_birth", "phone", "external_reference"} {
		if got[redacted] != redact.Redacted {
			t.Errorf("%s = %v, want it redacted", redacted, got[redacted])
		}
	}
	for _, kept := range []string{"patient_id", "user_id", "token_id"} {
		if got[kept] == redact.Redacted || got[kept] == nil {
			t.Errorf("%s = %v, want the identifier kept", kept, got[kept])
		}
	}
	for _, leaked := range []string{"someone@example.com", "1970-01-01", "555-0100", "MRN-88231"} {
		if strings.Contains(buf.String(), leaked) {
			t.Errorf("%q survived in the record:\n%s", leaked, buf.String())
		}
	}
}

// ------------------------------------------------- credentials by shape

// A token logged under an innocent name is still a token. It is recognised
// by its shape wherever it appears, including inside a message or an error.
func TestATokenIsRemovedWhateverItIsCalled(t *testing.T) {
	cases := map[string]func(l *slog.Logger){
		"under a harmless attribute name": func(l *slog.Logger) {
			l.Info("callback", "value", fakeJWT)
		},
		"inside an authorization header": func(l *slog.Logger) {
			l.Info("upstream call", "header", "Bearer "+fakeJWT)
		},
		"inside a message": func(l *slog.Logger) {
			l.Info("rejected token " + fakeJWT)
		},
		"inside an error": func(l *slog.Logger) {
			l.Error("verify", "error", fmt.Errorf("bad token %s: %w", fakeJWT, errors.New("signature")))
		},
		"inside a URL": func(l *slog.Logger) {
			l.Info("redirect", "location", "https://example.test/cb?access="+fakeJWT)
		},
	}
	for name, log := range cases {
		t.Run(name, func(t *testing.T) {
			logger, buf := jsonLogger()
			log(logger)

			line := buf.String()
			if strings.Contains(line, fakeJWT) {
				t.Errorf("the token survived:\n%s", line)
			}
			// Only the token goes; the record around it stays useful.
			if !strings.Contains(line, redact.Redacted) {
				t.Errorf("nothing was marked as redacted:\n%s", line)
			}
		})
	}
}

// The shape test must not fire on ordinary text, or it would hollow out
// every log during an incident.
func TestOrdinaryValuesAreLeftAlone(t *testing.T) {
	kept := []string{
		"processor.vitalmesh.internal",
		"postgres://gateway@db:5432/vitalmesh?sslmode=require",
		"1.2.3",
		"6f1c8a2e-0000-4000-8000-000000000001",
		"POST /api/v1/patients",
		"connection refused: dial tcp 127.0.0.1:6379",
	}
	for _, value := range kept {
		logger, buf := jsonLogger()
		logger.Info("event", "detail", value)
		if got := record(t, buf)["detail"]; got != value {
			t.Errorf("detail = %v, want it kept as %q", got, value)
		}
	}
}

// ---------------------------------------------------------- payloads

// A payload logged by mistake must not be stored whole. The bound is what
// keeps a request body, or a batch of readings, out of the log store.
func TestAnOversizedValueIsTruncated(t *testing.T) {
	body := strings.Repeat("A", redact.MaxValueLength*3)
	logger, buf := jsonLogger()
	logger.Info("oops", "detail", body)

	got, _ := record(t, buf)["detail"].(string)
	if len(got) >= len(body) {
		t.Fatalf("the value was not truncated: %d bytes", len(got))
	}
	if !strings.Contains(got, "more bytes omitted") {
		t.Errorf("a truncated value must say so: %q", got[max(0, len(got)-60):])
	}
}

// A whole measurement batch under a payload name is removed outright,
// rather than merely shortened.
func TestAPayloadAttributeIsRemovedOutright(t *testing.T) {
	batch := `[{"patient_id":"6f1c8a2e","value":121.5,"recorded_at":"2026-01-01T00:00:00Z"}]`
	for _, name := range []string{"payload", "body", "request_body", "response_body"} {
		logger, buf := jsonLogger()
		logger.Info("measurement batch", name, batch)
		if got := record(t, buf)[name]; got != redact.Redacted {
			t.Errorf("%s = %v, want it redacted", name, got)
		}
		if strings.Contains(buf.String(), "121.5") {
			t.Errorf("a measurement value reached the log:\n%s", buf.String())
		}
	}
}

// ------------------------------------------------- no way around it

// Redaction has to survive every way a logger can be built up, or the rule
// is only as good as the call site that happened to skip it.
func TestRedactionSurvivesDerivedLoggers(t *testing.T) {
	t.Run("attributes added with With", func(t *testing.T) {
		logger, buf := jsonLogger()
		logger.With("password", "hunter2").Info("derived")
		if got := record(t, buf)["password"]; got != redact.Redacted {
			t.Errorf("password = %v", got)
		}
	})

	t.Run("attributes inside a group", func(t *testing.T) {
		logger, buf := jsonLogger()
		logger.WithGroup("request").Info("derived", "token", fakeJWT)
		line := buf.String()
		if strings.Contains(line, fakeJWT) {
			t.Errorf("a grouped token survived:\n%s", line)
		}
	})

	t.Run("a value that redacts itself", func(t *testing.T) {
		// config.Secret already refuses to render; the logger must not undo
		// that by reaching past it.
		logger, buf := jsonLogger()
		logger.Info("startup", "configured", config.Secret("a-real-signing-key"))
		if strings.Contains(buf.String(), "a-real-signing-key") {
			t.Errorf("a Secret leaked:\n%s", buf.String())
		}
	})

	t.Run("the text format too", func(t *testing.T) {
		var buf bytes.Buffer
		logger := New(&buf, config.Log{Format: config.LogFormatText}, Service{Name: "test"})
		logger.Info("derived", "password", "hunter2", "value", fakeJWT)
		if strings.Contains(buf.String(), "hunter2") || strings.Contains(buf.String(), fakeJWT) {
			t.Errorf("the text handler skipped redaction:\n%s", buf.String())
		}
	})
}

func TestSensitiveNamesAreMatchedCaseInsensitively(t *testing.T) {
	for _, name := range []string{"Password", "AUTHORIZATION", " Token ", "Internal_Token"} {
		if !redact.Sensitive(name) {
			t.Errorf("Sensitive(%q) = false", name)
		}
	}
	for _, name := range []string{"token_id", "key", "count", "patient_id", "request_id", "route"} {
		if redact.Sensitive(name) {
			t.Errorf("Sensitive(%q) = true, want the field kept", name)
		}
	}
}
