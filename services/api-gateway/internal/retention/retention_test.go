package retention

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore records what it was asked to delete and how much it had.
type fakeStore struct {
	measurements int64
	results      int64

	measurementCalls []call
	resultCalls      []call
	audited          []json.RawMessage

	failMeasurements error
	failResults      error
	failAudit        error
}

type call struct {
	cutoff time.Time
	limit  int
}

func (f *fakeStore) DeleteMeasurementsBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	f.measurementCalls = append(f.measurementCalls, call{cutoff, limit})
	if f.failMeasurements != nil {
		return 0, f.failMeasurements
	}
	return f.take(&f.measurements, limit), nil
}

func (f *fakeStore) DeleteResultsBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	f.resultCalls = append(f.resultCalls, call{cutoff, limit})
	if f.failResults != nil {
		return 0, f.failResults
	}
	return f.take(&f.results, limit), nil
}

func (f *fakeStore) take(remaining *int64, limit int) int64 {
	n := int64(limit)
	if *remaining < n {
		n = *remaining
	}
	*remaining -= n
	return n
}

func (f *fakeStore) RecordRetentionRun(_ context.Context, metadata json.RawMessage) error {
	f.audited = append(f.audited, metadata)
	return f.failAudit
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func settings(measurementDays, resultDays, batch int) config.Retention {
	return config.Retention{
		MeasurementDays: measurementDays,
		ResultDays:      resultDays,
		Interval:        time.Hour,
		BatchLimit:      batch,
	}
}

// Nothing is removed unless an operator asked for it. Deleting data is
// irreversible, so the default must keep everything.
func TestDisabledByDefaultRemovesNothing(t *testing.T) {
	var zero config.Retention
	if zero.Enabled() {
		t.Error("the zero value enables retention")
	}

	store := &fakeStore{measurements: 500, results: 500}
	s := New(store, config.Retention{Interval: time.Hour, BatchLimit: 100}, discardLogger(), Options{})

	result, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Total() != 0 {
		t.Errorf("removed %d rows with retention disabled", result.Total())
	}
	if len(store.measurementCalls) != 0 || len(store.resultCalls) != 0 {
		t.Error("a delete was attempted with retention disabled")
	}
	if len(store.audited) != 0 {
		t.Error("a sweep that removed nothing was audited")
	}
}

// Run must return rather than tick over doing nothing, so a disabled
// sweeper costs a goroutine and a log line rather than a timer for the
// lifetime of the process.
func TestRunReturnsImmediatelyWhenDisabled(t *testing.T) {
	s := New(&fakeStore{}, config.Retention{Interval: time.Hour, BatchLimit: 100}, discardLogger(), Options{})
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with retention disabled")
	}
}

// The cutoff is what decides which rows go, so it is worth pinning: a
// window of N days removes what was recorded before now minus N days.
func TestCutoffIsTheConfiguredNumberOfDaysBack(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{measurements: 1, results: 1}
	s := New(store, settings(30, 7, 100), discardLogger(), Options{Now: fixedNow(now)})

	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.measurementCalls) == 0 || len(store.resultCalls) == 0 {
		t.Fatal("no delete was attempted")
	}
	if got, want := store.measurementCalls[0].cutoff, now.AddDate(0, 0, -30); !got.Equal(want) {
		t.Errorf("measurement cutoff = %s, want %s", got, want)
	}
	if got, want := store.resultCalls[0].cutoff, now.AddDate(0, 0, -7); !got.Equal(want) {
		t.Errorf("result cutoff = %s, want %s", got, want)
	}
}

// A backlog is removed in bounded batches rather than one statement, which
// is what keeps the locks off the write path.
func TestRemovesABacklogInBoundedBatches(t *testing.T) {
	store := &fakeStore{measurements: 250}
	s := New(store, settings(1, 0, 100), discardLogger(), Options{})

	result, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Measurements != 250 {
		t.Errorf("removed %d measurements, want 250", result.Measurements)
	}
	// 100, 100, 50: the short batch is how it knows nothing is left.
	if len(store.measurementCalls) != 3 {
		t.Errorf("made %d delete calls, want 3 batches of at most 100", len(store.measurementCalls))
	}
	for i, c := range store.measurementCalls {
		if c.limit != 100 {
			t.Errorf("call %d asked for %d rows, want the configured batch of 100", i, c.limit)
		}
	}
}

// One pass is bounded even when the backlog is not, so a sweep cannot run
// unboundedly long; the next pass continues.
func TestOnePassIsBoundedByTheBatchBudget(t *testing.T) {
	store := &fakeStore{measurements: int64(maxBatchesPerSweep)*10 + 500}
	s := New(store, settings(1, 0, 10), discardLogger(), Options{})

	result, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if want := int64(maxBatchesPerSweep) * 10; result.Measurements != want {
		t.Errorf("removed %d, want the pass to stop at %d", result.Measurements, want)
	}
	if store.measurements == 0 {
		t.Error("the pass drained the whole backlog; it should have stopped at its budget")
	}
}

// Section 81: a retention job must be auditable. An entry is written when
// something was removed, and it carries counts rather than identifiers.
func TestARunThatRemovedDataIsAudited(t *testing.T) {
	store := &fakeStore{measurements: 3, results: 2}
	s := New(store, settings(30, 7, 100), discardLogger(), Options{})

	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.audited) != 1 {
		t.Fatalf("wrote %d audit entries, want 1", len(store.audited))
	}

	var entry map[string]any
	if err := json.Unmarshal(store.audited[0], &entry); err != nil {
		t.Fatalf("audit metadata is not JSON: %v", err)
	}
	for key, want := range map[string]float64{
		"measurements_removed":       3,
		"results_removed":            2,
		"measurement_retention_days": 30,
		"result_retention_days":      7,
	} {
		got, ok := entry[key].(float64)
		if !ok || got != want {
			t.Errorf("audit metadata %s = %v, want %v", key, entry[key], want)
		}
	}
	// A retention entry says what the policy did, never who it was about
	// (SPECIFICATIONS.md sections 41 and 43).
	for _, forbidden := range []string{"patient_id", "measurement_id", "user_id", "id"} {
		if _, present := entry[forbidden]; present {
			t.Errorf("audit metadata carries %s, which identifies what was removed", forbidden)
		}
	}
}

// A pass that removed nothing writes no audit entry: an hourly row saying
// "removed 0" would bury the entries that matter.
func TestARunThatRemovedNothingIsNotAudited(t *testing.T) {
	store := &fakeStore{}
	s := New(store, settings(30, 7, 100), discardLogger(), Options{})

	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.audited) != 0 {
		t.Errorf("wrote %d audit entries for a sweep that removed nothing", len(store.audited))
	}
}

// The data is gone whether or not the entry was written, so a failing
// audit is reported and does not become the sweep's result.
func TestAFailingAuditDoesNotHideWhatWasRemoved(t *testing.T) {
	store := &fakeStore{measurements: 4, failAudit: errors.New("audit unavailable")}
	s := New(store, settings(30, 0, 100), discardLogger(), Options{})

	result, err := s.Sweep(context.Background())
	if err != nil {
		t.Errorf("Sweep returned %v; a failed audit must not fail the sweep", err)
	}
	if result.Measurements != 4 {
		t.Errorf("reported %d removed, want 4", result.Measurements)
	}
}

// A failure part way through reports what was actually removed, so the
// caller is never told nothing happened when rows are already gone.
func TestAFailureReportsWhatWasRemovedBeforeIt(t *testing.T) {
	failure := errors.New("connection lost")
	store := &fakeStore{measurements: 10, results: 10, failResults: failure}
	s := New(store, settings(30, 7, 100), discardLogger(), Options{})

	result, err := s.Sweep(context.Background())
	if !errors.Is(err, failure) {
		t.Errorf("err = %v, want the store's failure", err)
	}
	if result.Measurements != 10 {
		t.Errorf("reported %d measurements removed, want the 10 that were", result.Measurements)
	}
	if result.Results != 0 {
		t.Errorf("reported %d results removed, want 0", result.Results)
	}
}

// Shutdown must not wait on a backlog.
func TestSweepStopsWhenTheContextEnds(t *testing.T) {
	store := &fakeStore{measurements: 1 << 20}
	s := New(store, settings(1, 0, 1), discardLogger(), Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if store.measurements != 1<<20 {
		t.Error("a delete ran after the context was cancelled")
	}
}

// Only the window that is set is swept: a deployment may keep readings for
// a month and results for ever, or the other way round.
func TestOnlyTheConfiguredWindowsAreSwept(t *testing.T) {
	store := &fakeStore{measurements: 5, results: 5}
	s := New(store, settings(30, 0, 100), discardLogger(), Options{})

	result, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Measurements != 5 {
		t.Errorf("removed %d measurements, want 5", result.Measurements)
	}
	if len(store.resultCalls) != 0 {
		t.Error("results were swept with RESULT_RETENTION_DAYS unset")
	}
	if store.results != 5 {
		t.Error("results were removed with no result window configured")
	}
}
