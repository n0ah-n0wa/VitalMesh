// Package redisclient is the gateway's adapter onto Redis, the shared
// ephemeral store of SPECIFICATIONS.md section 23.
//
// Redis holds three kinds of thing here, none of which is a record of
// truth: rate-limit counters, short-lived cache entries and idempotency
// locks. Everything the gateway needs to be correct lives in PostgreSQL, so
// losing Redis costs performance and precision, never data.
//
// # Degradation
//
// Every operation returns [ErrUnavailable] when Redis cannot serve it, and
// every caller is expected to carry on without it. The client makes that
// cheap: after a failure it stops dialling for [config.Redis.RecoveryInterval]
// and fails calls locally instead, so an outage costs one timeout rather
// than one timeout per request. It reports the transition in each direction
// exactly once, so an outage is one log line and one recovery line rather
// than a flood.
//
// # Timeouts
//
// Every call is bounded twice: by the client's own per-command timeout and
// by the caller's context, whichever is shorter. Configuration refuses a
// Redis timeout that is not shorter than the HTTP request timeout, so a
// slow Redis can never be what makes a request slow.
package redisclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

// ErrUnavailable means Redis could not serve the operation. Callers degrade
// rather than fail: it is never a reason to reject a request.
var ErrUnavailable = errors.New("redis unavailable")

// ErrNotFound means the key does not exist. It is an ordinary outcome, not
// a failure, and does not mark Redis unavailable.
var ErrNotFound = errors.New("redis: key not found")

// Client is a Redis connection pool plus the degradation policy around it.
// The zero value is not usable; use [New] or [Disabled].
type Client struct {
	rdb       *redis.Client
	cfg       config.Redis
	logger    *slog.Logger
	now       func() time.Time
	namespace string

	mu sync.Mutex
	// downUntil is when the client will next try Redis. Zero means it is
	// believed healthy.
	downUntil time.Time
	// reported is whether the current outage has been logged.
	reported bool
}

// Options tune a Client. Nil fields take safe defaults.
type Options struct {
	Now func() time.Time
}

// New returns a client for cfg. It does not connect: go-redis dials lazily,
// so a gateway starts even when Redis is down and picks it up when it comes
// back (SPECIFICATIONS.md sections 23 and 90).
//
// A configuration with no URL returns a disabled client, which is not an
// error: Redis is optional by design.
func New(cfg config.Redis, logger *slog.Logger, opts Options) (*Client, error) {
	if !cfg.Enabled() {
		return Disabled(logger), nil
	}
	parsed, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	parsed.DialTimeout = cfg.DialTimeout
	parsed.ReadTimeout = cfg.Timeout
	parsed.WriteTimeout = cfg.Timeout
	parsed.PoolSize = cfg.PoolSize
	// One attempt per call: a retry inside the client would spend another
	// timeout on a request that is already waiting, and the caller's
	// degradation path is cheaper than a second attempt.
	parsed.MaxRetries = -1

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		rdb:       redis.NewClient(parsed),
		cfg:       cfg,
		logger:    logger,
		now:       now,
		namespace: cfg.Namespace,
	}, nil
}

// Disabled returns a client that is always unavailable, for a deployment
// configured without Redis. Callers take their degraded path and nothing
// dials anything.
func Disabled(logger *slog.Logger) *Client {
	return &Client{logger: logger, now: time.Now}
}

// Enabled reports whether Redis is configured at all.
func (c *Client) Enabled() bool { return c != nil && c.rdb != nil }

// Available reports whether the client currently believes Redis is usable.
// It is advisory: a call may still fail, and a call may still succeed after
// this returns false once the recovery interval has passed.
func (c *Client) Available() bool {
	if !c.Enabled() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.now().Before(c.downUntil)
}

// Close releases the connection pool. It is safe on a disabled client and
// safe to call more than once.
func (c *Client) Close() error {
	if !c.Enabled() {
		return nil
	}
	return c.rdb.Close()
}

// Ping checks that Redis answers, for diagnostics and for tests that need
// to know a server is live. It is deliberately not wired into the readiness
// report: Redis being down degrades rate limiting, caching and idempotency
// locks, and a gateway in that state still serves every request correctly,
// so failing readiness would take a healthy gateway out of rotation
// (SPECIFICATIONS.md section 90, OPEN_QUESTIONS OQ-27).
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, func(ctx context.Context) error {
		return c.rdb.Ping(ctx).Err()
	})
}

// Key returns the namespaced form of a key, so that several environments
// can share one server without colliding.
func (c *Client) Key(parts ...string) string {
	return c.namespace + ":" + strings.Join(parts, ":")
}

// Get reads a value. A missing key is [ErrNotFound]; an unusable Redis is
// [ErrUnavailable].
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	var out []byte
	err := c.do(ctx, func(ctx context.Context) error {
		value, err := c.rdb.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			// A miss is an answer, not a failure: it must not count
			// against Redis's health.
			return errNotFoundSentinel
		}
		out = value
		return err
	})
	if errors.Is(err, errNotFoundSentinel) {
		return nil, ErrNotFound
	}
	return out, err
}

// Set writes a value with a time to live. A zero or negative ttl is a
// programming error and is refused rather than writing a key that never
// expires: nothing in Redis here is allowed to outlive its purpose.
func (c *Client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("redis set %q: a time to live is required", key)
	}
	return c.do(ctx, func(ctx context.Context) error {
		return c.rdb.Set(ctx, key, value, ttl).Err()
	})
}

// Delete removes keys. Deleting a key that does not exist is not an error.
func (c *Client) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.do(ctx, func(ctx context.Context) error {
		return c.rdb.Del(ctx, keys...).Err()
	})
}

// Acquire takes a lock for ttl, returning whether it was taken. It is the
// ephemeral coordination of SPECIFICATIONS.md section 23: a lock is a hint
// that lets a caller fail fast, never the thing that makes an operation
// safe. Every caller must stay correct when the lock cannot be taken
// because Redis is unavailable.
func (c *Client) Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("redis lock %q: a time to live is required", key)
	}
	var acquired bool
	err := c.do(ctx, func(ctx context.Context) error {
		ok, err := c.rdb.SetNX(ctx, key, lockValue, ttl).Result()
		acquired = ok
		return err
	})
	if err != nil {
		return false, err
	}
	return acquired, nil
}

// Release drops a lock this process holds.
func (c *Client) Release(ctx context.Context, key string) error {
	return c.Delete(ctx, key)
}

const lockValue = "held"

// Eval runs a Lua script, which Redis executes atomically. The rate limiter
// uses it so that reading a counter, incrementing it and setting its expiry
// cannot interleave with another replica doing the same.
func (c *Client) Eval(ctx context.Context, script *redis.Script, keys []string, args ...any) (any, error) {
	var out any
	err := c.do(ctx, func(ctx context.Context) error {
		value, err := script.Run(ctx, c.rdb, keys, args...).Result()
		if errors.Is(err, redis.Nil) {
			// A script that returns nothing is a result, not a failure.
			out = nil
			return nil
		}
		out = value
		return err
	})
	return out, err
}

// errNotFoundSentinel distinguishes a miss from a failure inside do, so
// that a miss never marks Redis unavailable.
var errNotFoundSentinel = errors.New("redis: miss")

// do runs one operation under the client's timeout and the caller's
// context, and maintains the availability state around it.
func (c *Client) do(caller context.Context, fn func(context.Context) error) error {
	if !c.Enabled() {
		return ErrUnavailable
	}
	if !c.Available() {
		// Inside the recovery interval, fail locally rather than spending
		// this request's time discovering what the last one already found.
		return ErrUnavailable
	}
	// Bounded by the client's own timeout and by the caller's deadline,
	// whichever comes first.
	ctx, cancel := context.WithTimeout(caller, c.cfg.Timeout)
	defer cancel()

	err := fn(ctx)
	switch {
	case err == nil:
		c.markUp()
		return nil
	case errors.Is(err, errNotFoundSentinel):
		c.markUp()
		return err
	case caller.Err() != nil:
		// The caller gave up or ran out of time. That says nothing about
		// Redis, so it must not count against its health: the next request
		// would then be denied a working dependency because this one was
		// abandoned. Note this tests the *caller's* context, not the
		// derived one, which also ends when the client's own timeout fires.
		return ErrUnavailable
	default:
		c.markDown(err)
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// markDown records a failure and opens the recovery interval, logging the
// first failure of an outage only.
func (c *Client) markDown(cause error) {
	c.mu.Lock()
	first := !c.reported
	c.reported = true
	c.downUntil = c.now().Add(c.cfg.RecoveryInterval)
	c.mu.Unlock()

	if first {
		c.logger.Warn("redis unavailable; rate limiting, caching and idempotency locks are degraded",
			"error", cause, "retry_in", c.cfg.RecoveryInterval)
	}
}

// markUp records a success, logging the recovery only if an outage was
// reported.
func (c *Client) markUp() {
	c.mu.Lock()
	recovered := c.reported
	c.reported = false
	c.downUntil = time.Time{}
	c.mu.Unlock()

	if recovered {
		c.logger.Info("redis is available again")
	}
}
