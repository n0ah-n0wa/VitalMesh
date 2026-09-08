//go:build integration

package redisclient_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
)

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ------------------------------------------------------ normal operation

func TestValuesRoundTripAndExpire(t *testing.T) {
	client, _ := redistest.New(t)
	ctx := testContext(t)
	key := client.Key("test", "roundtrip")

	if _, err := client.Get(ctx, key); !errors.Is(err, redisclient.ErrNotFound) {
		t.Fatalf("a key never written = %v, want ErrNotFound", err)
	}
	if err := client.Set(ctx, key, []byte("value"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := client.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "value" {
		t.Errorf("got %q", got)
	}

	if err := client.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := client.Get(ctx, key); !errors.Is(err, redisclient.ErrNotFound) {
		t.Errorf("after delete = %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not a failure.
	if err := client.Delete(ctx, key); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
}

// Nothing this gateway writes to Redis may outlive its purpose, so a write
// without a time to live is refused rather than left for ever.
func TestAWriteWithoutATimeToLiveIsRefused(t *testing.T) {
	client, _ := redistest.New(t)
	ctx := testContext(t)

	if err := client.Set(ctx, client.Key("test", "forever"), []byte("v"), 0); err == nil {
		t.Error("Set accepted a zero time to live")
	}
	if _, err := client.Acquire(ctx, client.Key("test", "lock"), 0); err == nil {
		t.Error("Acquire accepted a zero time to live")
	}
}

func TestAnEntryExpiresOnItsOwn(t *testing.T) {
	client, _ := redistest.New(t)
	ctx := testContext(t)
	key := client.Key("test", "short")

	if err := client.Set(ctx, key, []byte("v"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Get(ctx, key); errors.Is(err, redisclient.ErrNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the entry outlived its time to live")
}

// A lock is the ephemeral coordination of section 23: one holder at a time,
// released explicitly, and expiring on its own if a holder dies.
func TestOnlyOneHolderTakesALock(t *testing.T) {
	client, _ := redistest.New(t)
	ctx := testContext(t)
	key := client.Key("test", "exclusive")

	taken, err := client.Acquire(ctx, key, time.Minute)
	if err != nil || !taken {
		t.Fatalf("first Acquire = %v, %v", taken, err)
	}
	again, err := client.Acquire(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if again {
		t.Error("two holders took the same lock")
	}

	if err := client.Release(ctx, key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	retaken, err := client.Acquire(ctx, key, time.Minute)
	if err != nil || !retaken {
		t.Errorf("after release = %v, %v; the lock should be free", retaken, err)
	}
}

func TestALockIsHeldByExactlyOneOfManyRacers(t *testing.T) {
	client, _ := redistest.New(t)
	ctx := testContext(t)
	key := client.Key("test", "race")

	const racers = 16
	var wg sync.WaitGroup
	results := make([]bool, racers)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			taken, err := client.Acquire(ctx, key, time.Minute)
			results[i] = err == nil && taken
		}()
	}
	wg.Wait()

	held := 0
	for _, taken := range results {
		if taken {
			held++
		}
	}
	if held != 1 {
		t.Errorf("%d racers took the lock, want exactly 1", held)
	}
}

func TestKeysAreNamespaced(t *testing.T) {
	client, cfg := redistest.New(t)
	key := client.Key("a", "b")
	if want := cfg.Namespace + ":a:b"; key != want {
		t.Errorf("Key = %q, want %q", key, want)
	}
}

func TestPingSucceedsAgainstARealServer(t *testing.T) {
	client, _ := redistest.New(t)
	ctx := testContext(t)

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !client.Available() {
		t.Error("a client that just answered reports itself unavailable")
	}
}

// ---------------------------------------------------- connection failure

// Every operation must report unavailability rather than blocking or
// panicking, so that a caller can take its degraded path.
func TestEveryOperationDegradesWhenRedisCannotBeReached(t *testing.T) {
	client, err := redisclient.New(redistest.Unreachable(), logger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("New must succeed without connecting: %v", err)
	}
	defer func() { _ = client.Close() }()
	ctx := testContext(t)

	started := time.Now()

	if err := client.Ping(ctx); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Ping = %v, want ErrUnavailable", err)
	}
	if _, err := client.Get(ctx, client.Key("k")); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Get = %v, want ErrUnavailable", err)
	}
	if err := client.Set(ctx, client.Key("k"), []byte("v"), time.Minute); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Set = %v, want ErrUnavailable", err)
	}
	if err := client.Delete(ctx, client.Key("k")); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Delete = %v, want ErrUnavailable", err)
	}
	if _, err := client.Acquire(ctx, client.Key("k"), time.Minute); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Acquire = %v, want ErrUnavailable", err)
	}

	// A dead dependency must not become a slow one: after the first
	// failure the client answers locally for the recovery interval.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("five operations against a dead Redis took %v", elapsed)
	}
	if client.Available() {
		t.Error("the client still believes a refused server is available")
	}
}

// A client with no URL is a valid configuration, not a broken one.
func TestADisabledClientIsAlwaysUnavailableAndNeverDials(t *testing.T) {
	client, err := redisclient.New(config.Redis{}, logger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("New with no URL: %v", err)
	}
	ctx := testContext(t)

	if client.Enabled() {
		t.Error("a client with no URL reports itself enabled")
	}
	if _, err := client.Get(ctx, "k"); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Get = %v, want ErrUnavailable", err)
	}
	if err := client.Ping(ctx); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Ping = %v, want ErrUnavailable", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// An address that cannot be parsed is a configuration mistake, which must
// stop start-up rather than degrade silently.
func TestAnUnusableAddressIsAnError(t *testing.T) {
	if _, err := redisclient.New(config.Redis{URL: "://nonsense"}, logger(), redisclient.Options{}); err == nil {
		t.Error("New accepted an unusable address")
	}
}

// After the recovery interval the client tries again, so an outage that
// ends is picked up without restarting the gateway.
func TestTheClientRecoversWhenRedisComesBack(t *testing.T) {
	_, cfg := redistest.New(t)
	// A client that starts out pointed at nothing.
	broken := redistest.Unreachable()
	broken.Namespace = cfg.Namespace
	client, err := redisclient.New(broken, logger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = client.Close() }()
	ctx := testContext(t)

	if err := client.Ping(ctx); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Fatalf("Ping against a dead server = %v", err)
	}
	if client.Available() {
		t.Fatal("the client should have marked itself down")
	}

	// Once the recovery interval passes it must be willing to try again.
	time.Sleep(2 * broken.RecoveryInterval)
	if !client.Available() {
		t.Error("the client never became willing to retry")
	}

	// And a client pointed at the live server recovers in fact, not just in
	// willingness.
	live, err := redisclient.New(cfg, logger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = live.Close() }()
	if err := live.Ping(ctx); err != nil {
		t.Errorf("Ping against the live server: %v", err)
	}
	if !live.Available() {
		t.Error("a client that just succeeded reports itself unavailable")
	}
}

// The caller's deadline bounds a Redis call, so a request that has given up
// does not wait on a dependency it no longer needs.
func TestTheCallersDeadlineBoundsACall(t *testing.T) {
	client, _ := redistest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Get(ctx, client.Key("k")); !errors.Is(err, redisclient.ErrUnavailable) {
		t.Errorf("Get on a cancelled context = %v, want ErrUnavailable", err)
	}
	// A caller giving up says nothing about Redis, so the client must not
	// have marked it down.
	if !client.Available() {
		t.Error("a cancelled caller marked Redis unavailable")
	}
}
