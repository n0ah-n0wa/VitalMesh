package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

func newTestLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	cfg := config.Log{Level: level, Format: config.LogFormatJSON}
	svc := Service{Name: "api-gateway", Version: "abc123", Environment: "test"}
	return New(&buf, cfg, svc), &buf
}

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	return rec
}

func TestRecordCarriesSchemaFields(t *testing.T) {
	logger, buf := newTestLogger(slog.LevelInfo)
	ctx := requestid.NewContext(context.Background(), "req-1")

	logger.InfoContext(ctx, "hello", "extra", 42)

	rec := decodeLine(t, buf)
	for key, want := range map[string]any{
		"level":       "INFO",
		"message":     "hello",
		"service":     "api-gateway",
		"version":     "abc123",
		"environment": "test",
		"request_id":  "req-1",
		"extra":       float64(42),
	} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
		}
	}
	if _, ok := rec["timestamp"]; !ok {
		t.Error("record has no timestamp field")
	}
	for _, old := range []string{"time", "msg"} {
		if _, ok := rec[old]; ok {
			t.Errorf("record still has standard %q key", old)
		}
	}
}

func TestRequestIDOmittedWithoutContextValue(t *testing.T) {
	logger, buf := newTestLogger(slog.LevelInfo)
	logger.Info("plain")
	if _, ok := decodeLine(t, buf)["request_id"]; ok {
		t.Error("request_id present without a request ID in context")
	}
}

func TestRequestIDSurvivesWithAndWithGroup(t *testing.T) {
	logger, buf := newTestLogger(slog.LevelInfo)
	ctx := requestid.NewContext(context.Background(), "req-2")

	logger.With("component", "x").WithGroup("g").InfoContext(ctx, "nested", "k", "v")

	rec := decodeLine(t, buf)
	if rec["request_id"] != "req-2" {
		t.Errorf("request_id = %v, want req-2 at top level", rec["request_id"])
	}
	if rec["component"] != "x" {
		t.Errorf("component = %v, want x", rec["component"])
	}
	group, ok := rec["g"].(map[string]any)
	if !ok {
		t.Fatalf("group g missing or not an object: %v", rec["g"])
	}
	if group["k"] != "v" {
		t.Errorf("g.k = %v, want v", group["k"])
	}
	if _, nested := group["request_id"]; nested {
		t.Error("request_id was nested inside the group")
	}
}

func TestLevelFilters(t *testing.T) {
	logger, buf := newTestLogger(slog.LevelWarn)
	logger.Info("dropped")
	logger.Warn("kept")
	if strings.Contains(buf.String(), "dropped") || !strings.Contains(buf.String(), "kept") {
		t.Errorf("level filtering failed: %s", buf.String())
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, config.Log{Format: config.LogFormatText}, Service{Name: "s"}).Info("hi")
	if !strings.Contains(buf.String(), "message=hi") || !strings.Contains(buf.String(), "service=s") {
		t.Errorf("unexpected text output: %s", buf.String())
	}
}
