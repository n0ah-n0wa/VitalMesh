// Package health is the application-level readiness service. Infrastructure
// packages register a Checker for each required dependency; the service runs
// them concurrently under a single timeout and reports the outcome.
package health

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Checker verifies that one required dependency can serve requests.
type Checker interface {
	// Name identifies the dependency in readiness reports, e.g. "postgres".
	Name() string
	// Check returns nil when the dependency is usable. It must honour ctx.
	Check(ctx context.Context) error
}

// CheckResult is the outcome of one Checker.
type CheckResult struct {
	Name string
	Err  error
}

// Report is the outcome of a readiness evaluation.
type Report struct {
	// Ready is true when every check passed.
	Ready bool
	// Checks holds one result per registered checker, in registration order.
	Checks []CheckResult
}

// Readiness evaluates whether every required dependency is usable.
type Readiness struct {
	timeout  time.Duration
	checkers []Checker
}

// NewReadiness returns a Readiness that bounds each evaluation by timeout.
func NewReadiness(timeout time.Duration, checkers ...Checker) *Readiness {
	return &Readiness{timeout: timeout, checkers: checkers}
}

// Check runs every checker concurrently and reports the results. A checker
// that panics is reported as failed rather than crashing the process: a bug
// in one dependency adapter must not turn into pod restarts.
func (r *Readiness) Check(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	results := make([]CheckResult, len(r.checkers))
	var wg sync.WaitGroup
	for i, c := range r.checkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runCheck(ctx, c)
		}()
	}
	wg.Wait()

	ready := true
	for _, res := range results {
		if res.Err != nil {
			ready = false
			break
		}
	}
	return Report{Ready: ready, Checks: results}
}

func runCheck(ctx context.Context, c Checker) (res CheckResult) {
	res.Name = c.Name()
	defer func() {
		if p := recover(); p != nil {
			res.Err = fmt.Errorf("check panicked: %v", p)
		}
	}()
	res.Err = c.Check(ctx)
	return res
}
