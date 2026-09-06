package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeChecker struct {
	name  string
	check func(ctx context.Context) error
}

func (f fakeChecker) Name() string                    { return f.name }
func (f fakeChecker) Check(ctx context.Context) error { return f.check(ctx) }

func ok(name string) Checker {
	return fakeChecker{name, func(context.Context) error { return nil }}
}

func failing(name string, err error) Checker {
	return fakeChecker{name, func(context.Context) error { return err }}
}

func TestNoCheckersIsReady(t *testing.T) {
	rep := NewReadiness(time.Second).Check(context.Background())
	if !rep.Ready {
		t.Error("Ready = false with no checkers")
	}
	if len(rep.Checks) != 0 {
		t.Errorf("Checks = %v, want empty", rep.Checks)
	}
}

func TestAnyFailureMakesNotReady(t *testing.T) {
	boom := errors.New("boom")
	rep := NewReadiness(time.Second, ok("a"), failing("b", boom), ok("c")).Check(context.Background())

	if rep.Ready {
		t.Error("Ready = true despite a failing check")
	}
	if len(rep.Checks) != 3 {
		t.Fatalf("Checks = %d, want 3", len(rep.Checks))
	}
	for i, want := range []string{"a", "b", "c"} {
		if rep.Checks[i].Name != want {
			t.Errorf("Checks[%d].Name = %q, want %q (registration order)", i, rep.Checks[i].Name, want)
		}
	}
	if !errors.Is(rep.Checks[1].Err, boom) {
		t.Errorf("Checks[1].Err = %v, want boom", rep.Checks[1].Err)
	}
	if rep.Checks[0].Err != nil || rep.Checks[2].Err != nil {
		t.Error("passing checks reported an error")
	}
}

func TestSlowCheckerIsBoundedByTimeout(t *testing.T) {
	slow := fakeChecker{"slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}

	start := time.Now()
	rep := NewReadiness(50*time.Millisecond, slow).Check(context.Background())

	if rep.Ready {
		t.Error("Ready = true for a checker that timed out")
	}
	if !errors.Is(rep.Checks[0].Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want DeadlineExceeded", rep.Checks[0].Err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Check took %s, timeout was not enforced", elapsed)
	}
}

// Two checkers each wait for the other to start. That can only complete if
// they run concurrently; a sequential implementation would time out.
func TestCheckersRunConcurrently(t *testing.T) {
	aStarted := make(chan struct{})
	bStarted := make(chan struct{})

	a := fakeChecker{"a", func(ctx context.Context) error {
		close(aStarted)
		select {
		case <-bStarted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	b := fakeChecker{"b", func(ctx context.Context) error {
		close(bStarted)
		select {
		case <-aStarted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}

	rep := NewReadiness(2*time.Second, a, b).Check(context.Background())
	if !rep.Ready {
		t.Errorf("checks did not run concurrently: %+v", rep.Checks)
	}
}

func TestPanickingCheckerIsReportedNotFatal(t *testing.T) {
	panicking := fakeChecker{"buggy", func(context.Context) error {
		var m map[string]int
		m["x"] = 1 // nil map write
		return nil
	}}

	rep := NewReadiness(time.Second, ok("fine"), panicking).Check(context.Background())

	if rep.Ready {
		t.Error("Ready = true despite a panicking check")
	}
	if rep.Checks[0].Err != nil {
		t.Errorf("healthy check affected: %v", rep.Checks[0].Err)
	}
	if err := rep.Checks[1].Err; err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("Checks[1].Err = %v, want a panic report", err)
	}
}
