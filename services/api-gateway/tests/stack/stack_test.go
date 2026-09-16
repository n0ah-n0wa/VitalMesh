//go:build stack

package stack

import (
	"testing"
)

// TestStack runs every flow in a fixed order against the running stack.
// The order matters twice: later flows build on state earlier ones create
// (a patient with readings, a completed job), and the failure cases come
// last so that a dependency taken away cannot disturb a flow that assumes
// the stack is whole.
func TestStack(t *testing.T) {
	s := newStack(t)

	flows := []struct {
		name string
		run  func(*testing.T)
	}{
		{"authentication", s.testAuthentication},
		{"patient creation", s.testPatientCreation},
		{"measurement ingestion", s.testMeasurementIngestion},
		{"batch ingestion", s.testBatchIngestion},
		{"processing job, Go to Rust, results", s.testProcessing},
		{"authorization", s.testAuthorization},
		{"idempotency", s.testIdempotency},
		{"audit logging", s.testAuditLogging},
		{"rate limiting", s.testRateLimiting},
		{"failure: invalid input", s.testInvalidInput},
		{"failure: duplicate request", s.testDuplicateRequest},
		{"failure: Rust unavailable", s.testProcessorUnavailable},
		{"failure: timeout", s.testProcessorTimeout},
		{"failure: database failure", s.testDatabaseFailure},
		{"failure: Redis failure", s.testRedisFailure},
	}
	for _, f := range flows {
		if !t.Run(f.name, f.run) && t.Failed() {
			// Keep going: every flow reports its own verdict, and the
			// failure cases restore what they took away.
			continue
		}
	}
}
