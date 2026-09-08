// Package cache is the short-lived cache of SPECIFICATIONS.md section 84.
//
// # What may be cached
//
// Only data a stale copy of which cannot make the system wrong. In practice
// that means data used to *answer a read*, never data used to *make a
// decision*: a patient record may be served from the cache to a client
// asking for it, and must not be read from the cache to decide whether a
// measurement may be written for that patient. Decisions always go to
// PostgreSQL, which is the record of truth.
//
// # Invalidation
//
// Every write to a cached entity deletes its entry explicitly. The time to
// live is the second line of defence, not the first: it bounds how long a
// stale entry can survive an invalidation that could not be delivered
// because Redis was unreachable at that moment. Configuration caps it at a
// few minutes for exactly that reason.
//
// # Degradation
//
// Every miss, every failure and a disabled cache are the same thing to a
// caller: read from the database. A cache that cannot be reached costs
// latency, never correctness.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
)

// Store is the cache port. Implementations never return an error a caller
// must handle: a failure is reported as a miss.
type Store interface {
	// Get reports whether the key held a usable value and unmarshals it
	// into dst.
	Get(ctx context.Context, key string, dst any) bool
	// Set stores value under key for ttl. Failures are silent by design.
	Set(ctx context.Context, key string, value any, ttl time.Duration)
	// Invalidate removes keys.
	Invalidate(ctx context.Context, keys ...string)
	// Key builds a namespaced key.
	Key(parts ...string) string
}

// Redis is a Store backed by the shared ephemeral store.
type Redis struct {
	client  *redisclient.Client
	logger  *slog.Logger
	enabled bool
}

// NewRedis returns a cache over client. A disabled configuration or a
// client without Redis yields a cache that never hits, which is a valid
// cache: every read then goes to the database.
func NewRedis(client *redisclient.Client, enabled bool, logger *slog.Logger) *Redis {
	return &Redis{client: client, logger: logger, enabled: enabled && client.Enabled()}
}

func (c *Redis) Key(parts ...string) string { return c.client.Key(parts...) }

// Get returns false on a miss, on an unusable entry and on any failure.
func (c *Redis) Get(ctx context.Context, key string, dst any) bool {
	if !c.enabled {
		return false
	}
	raw, err := c.client.Get(ctx, key)
	switch {
	case errors.Is(err, redisclient.ErrNotFound):
		return false
	case err != nil:
		// Redis logs the outage itself, once; a miss is all the caller
		// needs to know.
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		// An entry this build cannot read, most likely written by an older
		// one. Drop it so the next request does not pay for it again.
		c.logger.WarnContext(ctx, "cache entry discarded: unreadable", "key", key, "error", err)
		c.Invalidate(ctx, key)
		return false
	}
	return true
}

// Set stores value. A failure is not reported: the next read simply misses.
func (c *Redis) Set(ctx context.Context, key string, value any, ttl time.Duration) {
	if !c.enabled || ttl <= 0 {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		c.logger.ErrorContext(ctx, "cache entry not stored: unencodable", "key", key, "error", err)
		return
	}
	_ = c.client.Set(ctx, key, encoded, ttl)
}

// Invalidate removes entries. It runs on a context detached from the
// caller's cancellation: an invalidation is the thing that keeps the cache
// honest, and it matters most when the request that triggered it is being
// abandoned. The time to live bounds what a failure here can cost.
func (c *Redis) Invalidate(ctx context.Context, keys ...string) {
	if !c.enabled || len(keys) == 0 {
		return
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), invalidateTimeout)
	defer cancel()
	if err := c.client.Delete(detached, keys...); err != nil {
		c.logger.WarnContext(ctx, "cache entry not invalidated; it will expire on its own",
			"keys", keys, "error", err)
	}
}

// invalidateTimeout bounds a detached invalidation. It is short: the
// request is already finishing and the entry expires on its own anyway.
const invalidateTimeout = time.Second

// Disabled is a Store that holds nothing. It is what a gateway configured
// without Redis uses, and what tests use when the cache is not the subject.
type Disabled struct{}

func (Disabled) Get(context.Context, string, any) bool           { return false }
func (Disabled) Set(context.Context, string, any, time.Duration) {}
func (Disabled) Invalidate(context.Context, ...string)           {}
func (Disabled) Key(parts ...string) string {
	key := ""
	for i, p := range parts {
		if i > 0 {
			key += ":"
		}
		key += p
	}
	return key
}
