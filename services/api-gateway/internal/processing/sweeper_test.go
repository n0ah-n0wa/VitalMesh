package processing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeExpirer struct {
	calls   int
	limits  []int
	failure Failure
	// answers are returned in order; the last one repeats.
	answers []int64
	err     error
	seen    time.Time
}

func (f *fakeExpirer) FailExpiredJobs(_ context.Context, now time.Time, failure Failure, limit int) (int64, error) {
	f.calls++
	f.limits = append(f.limits, limit)
	f.failure, f.seen = failure, now
	if f.err != nil {
		return 0, f.err
	}
	i := min(f.calls-1, len(f.answers)-1)
	return f.answers[i], nil
}

func TestSweepFailsExpiredJobsInBatchesUntilNoneAreLeft(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	expirer := &fakeExpirer{answers: []int64{SweepBatch, SweepBatch, 3}}
	sweeper := NewSweeper(expirer, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)), SweeperOptions{Now: func() time.Time { return now }})

	total, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if total != 2*SweepBatch+3 {
		t.Errorf("failed %d jobs, want %d", total, 2*SweepBatch+3)
	}
	if expirer.calls != 3 {
		t.Errorf("made %d statements, want 3: two full batches and one short one", expirer.calls)
	}
	for _, limit := range expirer.limits {
		if limit != SweepBatch {
			t.Errorf("a batch asked for %d rows, want %d", limit, SweepBatch)
		}
	}
	if expirer.failure.Code != CodeProcessingInterrupted || expirer.failure.Message == "" {
		t.Errorf("swept jobs carry %+v, want %s with a message", expirer.failure, CodeProcessingInterrupted)
	}
	if !expirer.seen.Equal(now) {
		t.Errorf("the sweep judged leases at %s, want the injected clock %s", expirer.seen, now)
	}
}

func TestSweepIsBoundedAndReportsAFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	endless := &fakeExpirer{answers: []int64{SweepBatch}}
	if _, err := NewSweeper(endless, time.Minute, logger, SweeperOptions{}).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if endless.calls != maxBatchesPerSweep {
		t.Errorf("a pass over an endless backlog made %d statements, want the bound %d", endless.calls, maxBatchesPerSweep)
	}

	broken := &fakeExpirer{err: errors.New("database is gone")}
	if _, err := NewSweeper(broken, time.Minute, logger, SweeperOptions{}).Sweep(context.Background()); err == nil {
		t.Error("a sweep whose statement failed reported no error")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	untouched := &fakeExpirer{answers: []int64{1}}
	if _, err := NewSweeper(untouched, time.Minute, logger, SweeperOptions{}).Sweep(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("a sweep on a done context = %v, want context.Canceled", err)
	}
	if untouched.calls != 0 {
		t.Error("a sweep on a done context still ran a statement")
	}
}

func TestRunSweepsOnEveryTickAndStopsWithTheContext(t *testing.T) {
	expirer := &fakeExpirer{answers: []int64{0}}
	sweeper := NewSweeper(expirer, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), SweeperOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { sweeper.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if expirer.calls == 0 {
		t.Error("Run never swept")
	}
}
