package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProcessingAndProcessorDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Processing.MaxJobMeasurements != 100000 {
		t.Errorf("MaxJobMeasurements = %d", cfg.Processing.MaxJobMeasurements)
	}
	if cfg.Processing.AlgorithmVersion != DefaultAlgorithmVersion {
		t.Errorf("AlgorithmVersion = %q, want %q", cfg.Processing.AlgorithmVersion, DefaultAlgorithmVersion)
	}
	if cfg.Processing.FailureRecordTimeout != 5*time.Second {
		t.Errorf("FailureRecordTimeout = %s", cfg.Processing.FailureRecordTimeout)
	}
	if cfg.Processor.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d", cfg.Processor.MaxAttempts)
	}
	if cfg.Processor.Timeout != 5*time.Second {
		t.Errorf("Timeout = %s", cfg.Processor.Timeout)
	}
	if cfg.Processor.Backoff != 100*time.Millisecond || cfg.Processor.MaxBackoff != 2*time.Second {
		t.Errorf("backoff = %s/%s", cfg.Processor.Backoff, cfg.Processor.MaxBackoff)
	}
	if cfg.Processor.ContractVersion != InternalContractVersion {
		t.Errorf("ContractVersion = %q", cfg.Processor.ContractVersion)
	}
	// A client that gives up before the gateway can answer turns a clear
	// failure into a mystery, so the defaults must already be ordered.
	if cfg.Processor.Timeout >= cfg.HTTP.RequestTimeout {
		t.Errorf("the default processor timeout %s is not below the request timeout %s",
			cfg.Processor.Timeout, cfg.HTTP.RequestTimeout)
	}
}

func TestProcessingOverrides(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		"PROCESSING_MAX_JOB_MEASUREMENTS":   "250",
		"PROCESSING_ALGORITHM_VERSION":      "2.1.0",
		"PROCESSING_SERVICE_VERSION":        "gateway-x",
		"PROCESSING_FAILURE_RECORD_TIMEOUT": "1s",
		"PROCESSOR_URL":                     "https://processor.internal:9443",
		"PROCESSOR_TOKEN":                   "an-override-token",
		"PROCESSOR_TIMEOUT":                 "2s",
		"PROCESSOR_MAX_ATTEMPTS":            "5",
		"PROCESSOR_BACKOFF":                 "50ms",
		"PROCESSOR_MAX_BACKOFF":             "1s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Processing.MaxJobMeasurements != 250 || cfg.Processing.AlgorithmVersion != "2.1.0" {
		t.Errorf("processing = %+v", cfg.Processing)
	}
	if cfg.Processing.ServiceVersion != "gateway-x" || cfg.Processing.FailureRecordTimeout != time.Second {
		t.Errorf("processing = %+v", cfg.Processing)
	}
	if cfg.Processor.BaseURL != "https://processor.internal:9443" || cfg.Processor.MaxAttempts != 5 {
		t.Errorf("processor = %+v", cfg.Processor)
	}
	if string(cfg.Processor.Token) != "an-override-token" {
		t.Error("the token did not survive loading")
	}
}

func TestProcessorSettingsAreValidated(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a processor address that is not a URL", map[string]string{"PROCESSOR_URL": "processor:8081"}, "PROCESSOR_URL"},
		{"a processor address with a path", map[string]string{"PROCESSOR_URL": "http://processor:8081/internal"}, "PROCESSOR_URL"},
		{"a processor address with no host", map[string]string{"PROCESSOR_URL": "http://"}, "PROCESSOR_URL"},
		{"a timeout at the request timeout", map[string]string{"PROCESSOR_TIMEOUT": "10s"}, "PROCESSOR_TIMEOUT"},
		{"a timeout above the request timeout", map[string]string{"PROCESSOR_TIMEOUT": "30s"}, "PROCESSOR_TIMEOUT"},
		{"no attempts", map[string]string{"PROCESSOR_MAX_ATTEMPTS": "0"}, "PROCESSOR_MAX_ATTEMPTS"},
		{"more attempts than the bound", map[string]string{"PROCESSOR_MAX_ATTEMPTS": "6"}, "PROCESSOR_MAX_ATTEMPTS"},
		{"a cap below the base delay", map[string]string{"PROCESSOR_BACKOFF": "5s", "PROCESSOR_MAX_BACKOFF": "1s"}, "PROCESSOR_MAX_BACKOFF"},
		{"no job bound", map[string]string{"PROCESSING_MAX_JOB_MEASUREMENTS": "0"}, "PROCESSING_MAX_JOB_MEASUREMENTS"},
		{"a job bound above the ceiling", map[string]string{"PROCESSING_MAX_JOB_MEASUREMENTS": "2000000"}, "PROCESSING_MAX_JOB_MEASUREMENTS"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(lookupFrom(tc.env))
			if err == nil {
				t.Fatalf("the configuration was accepted; want %s to be rejected", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// The processor must not be reachable without a credential where it carries
// real traffic (SPECIFICATIONS.md sections 30 and 31).
func TestADeployedEnvironmentRequiresTheProcessorCredential(t *testing.T) {
	for _, env := range []string{"staging", "production"} {
		_, err := Load(lookupFrom(map[string]string{
			"ENVIRONMENT":  env,
			"DATABASE_URL": "postgres://u:p@db:5432/vm?sslmode=require",
		}))
		if err == nil || !strings.Contains(err.Error(), "PROCESSOR_TOKEN: required in "+env) {
			t.Errorf("%s: err = %v, want the credential to be required", env, err)
		}
	}
	for _, env := range []string{"local", "test"} {
		if _, err := Load(lookupFrom(map[string]string{"ENVIRONMENT": env})); err != nil {
			t.Errorf("%s: a developer may run without a processor credential: %v", env, err)
		}
	}
}

// A dump of the configuration must not reveal the processor credential.
func TestTheProcessorTokenIsRedacted(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{"PROCESSOR_TOKEN": "a-token-that-must-not-appear"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, rendered := range []string{
		cfg.Processor.Token.String(),
		fmt.Sprintf("%v", cfg.Processor),
		fmt.Sprintf("%+v", cfg.Processor),
		fmt.Sprintf("%#v", cfg.Processor),
		fmt.Sprintf("%+v", cfg),
	} {
		if strings.Contains(rendered, "a-token-that-must-not-appear") {
			t.Errorf("the token appeared in %q", rendered)
		}
	}
	if string(cfg.Processor.Token) != "a-token-that-must-not-appear" {
		t.Error("the token must still be usable through its bytes")
	}
}
