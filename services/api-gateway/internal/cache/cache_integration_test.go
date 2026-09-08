//go:build integration

package cache_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/cache"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
)

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type record struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func TestAValueRoundTripsThroughTheCache(t *testing.T) {
	client, _ := redistest.New(t)
	store := cache.NewRedis(client, true, logger())
	ctx := context.Background()

	key := store.Key("record", "one")
	want := record{ID: uuid.New(), Name: "first"}

	var miss record
	if store.Get(ctx, key, &miss) {
		t.Fatal("an empty cache reported a hit")
	}

	store.Set(ctx, key, want, time.Minute)

	var got record
	if !store.Get(ctx, key, &got) {
		t.Fatal("the value did not come back")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Invalidation is the mechanism, not the time to live: a write must remove
// the entry now.
func TestInvalidationRemovesTheEntryImmediately(t *testing.T) {
	client, _ := redistest.New(t)
	store := cache.NewRedis(client, true, logger())
	ctx := context.Background()
	key := store.Key("record", "two")

	store.Set(ctx, key, record{Name: "before"}, time.Hour)
	var got record
	if !store.Get(ctx, key, &got) {
		t.Fatal("the entry was not stored")
	}

	store.Invalidate(ctx, key)
	if store.Get(ctx, key, &got) {
		t.Error("the entry survived invalidation")
	}
}

// An invalidation matters most when the request that triggered it is being
// abandoned, so it must not be tied to the caller's cancellation.
func TestInvalidationRunsEvenWhenTheCallerHasGivenUp(t *testing.T) {
	client, _ := redistest.New(t)
	store := cache.NewRedis(client, true, logger())
	key := store.Key("record", "detached")

	store.Set(context.Background(), key, record{Name: "stale"}, time.Hour)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	store.Invalidate(cancelled, key)

	var got record
	if store.Get(context.Background(), key, &got) {
		t.Error("the entry survived an invalidation on a cancelled context")
	}
}

func TestAnEntryExpiresOnItsOwn(t *testing.T) {
	client, _ := redistest.New(t)
	store := cache.NewRedis(client, true, logger())
	ctx := context.Background()
	key := store.Key("record", "short")

	store.Set(ctx, key, record{Name: "brief"}, 50*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got record
		if !store.Get(ctx, key, &got) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the entry outlived its time to live")
}

// An entry this build cannot read must be dropped rather than served or
// retried for ever.
func TestAnUnreadableEntryIsDiscarded(t *testing.T) {
	client, _ := redistest.New(t)
	store := cache.NewRedis(client, true, logger())
	ctx := context.Background()
	key := store.Key("record", "corrupt")

	if err := client.Set(ctx, key, []byte("{not json"), time.Hour); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var got record
	if store.Get(ctx, key, &got) {
		t.Fatal("an unreadable entry was reported as a hit")
	}
	if _, err := client.Get(ctx, key); err == nil {
		t.Error("the unreadable entry was left behind")
	}
}

// ------------------------------------------------------- degradation

// Every failure and every disabled cache look the same to a caller: a miss,
// which sends the read to the database.
func TestACacheWithoutRedisAlwaysMisses(t *testing.T) {
	client, err := redisclient.New(redistest.Unreachable(), logger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer func() { _ = client.Close() }()

	store := cache.NewRedis(client, true, logger())
	ctx := context.Background()
	key := store.Key("record", "gone")

	// None of these may block, panic or return an error.
	store.Set(ctx, key, record{Name: "x"}, time.Minute)
	var got record
	if store.Get(ctx, key, &got) {
		t.Error("a cache with no Redis reported a hit")
	}
	store.Invalidate(ctx, key)
}

func TestADisabledCacheHoldsNothing(t *testing.T) {
	client, _ := redistest.New(t)
	store := cache.NewRedis(client, false, logger())
	ctx := context.Background()
	key := store.Key("record", "off")

	store.Set(ctx, key, record{Name: "x"}, time.Minute)
	var got record
	if store.Get(ctx, key, &got) {
		t.Error("a disabled cache served an entry")
	}
	// Nothing reached Redis either.
	if _, err := client.Get(ctx, key); err == nil {
		t.Error("a disabled cache wrote to Redis")
	}
}

func TestTheDisabledStoreIsUsableWithoutAnyClient(t *testing.T) {
	var store cache.Store = cache.Disabled{}
	ctx := context.Background()

	store.Set(ctx, store.Key("a", "b"), record{Name: "x"}, time.Minute)
	var got record
	if store.Get(ctx, store.Key("a", "b"), &got) {
		t.Error("the disabled store reported a hit")
	}
	store.Invalidate(ctx, store.Key("a", "b"))
	if want := "a:b"; store.Key("a", "b") != want {
		t.Errorf("Key = %q, want %q", store.Key("a", "b"), want)
	}
}

// A configuration without Redis is supported, and the cache must not try to
// reach anything.
func TestACacheOverADisabledClientIsInert(t *testing.T) {
	client, err := redisclient.New(config.Redis{}, logger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	store := cache.NewRedis(client, true, logger())
	ctx := context.Background()

	store.Set(ctx, "k", record{Name: "x"}, time.Minute)
	var got record
	if store.Get(ctx, "k", &got) {
		t.Error("an inert cache reported a hit")
	}
}
