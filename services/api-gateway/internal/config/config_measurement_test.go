package config

import (
	"strings"
	"testing"
	"time"
)

func TestMeasurementAndIdempotencySettings(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Measurements.MaxBatchSize != 1000 || cfg.Measurements.MaxMetadataBytes != 2048 || cfg.Measurements.MaxFutureSkew != 5*time.Minute || cfg.Idempotency.TTL != 24*time.Hour {
		t.Errorf("defaults = %+v %+v", cfg.Measurements, cfg.Idempotency)
	}
	cfg, err = Load(lookupFrom(map[string]string{
		"MEASUREMENT_MAX_BATCH_SIZE": "250", "MEASUREMENT_MAX_METADATA_BYTES": "4096", "MEASUREMENT_MAX_FUTURE_SKEW": "1m", "IDEMPOTENCY_TTL": "48h",
	}))
	if err != nil || cfg.Measurements.MaxBatchSize != 250 || cfg.Measurements.MaxMetadataBytes != 4096 || cfg.Measurements.MaxFutureSkew != time.Minute || cfg.Idempotency.TTL != 48*time.Hour {
		t.Errorf("overrides = %+v %+v, %v", cfg.Measurements, cfg.Idempotency, err)
	}
	for name, tc := range map[string]struct{ key, value, want string }{
		"batch zero":       {"MEASUREMENT_MAX_BATCH_SIZE", "0", "must be positive"},
		"batch too big":    {"MEASUREMENT_MAX_BATCH_SIZE", "10001", "must be at most 10000"},
		"metadata too big": {"MEASUREMENT_MAX_METADATA_BYTES", "4097", "must be at most 4096"},
		"skew too big":     {"MEASUREMENT_MAX_FUTURE_SKEW", "2h", "must be at most 1h0m0s"},
		"ttl too short":    {"IDEMPOTENCY_TTL", "30s", "must be between 1m0s and 168h0m0s"},
		"ttl too long":     {"IDEMPOTENCY_TTL", "200h", "must be between"},
	} {
		_, err := Load(lookupFrom(map[string]string{tc.key: tc.value}))
		if err == nil || !strings.Contains(err.Error(), tc.key+": "+tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
