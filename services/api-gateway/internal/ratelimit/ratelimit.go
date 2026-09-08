// Package ratelimit implements the distributed rate limiting of
// SPECIFICATIONS.md section 29.
//
// Limits are per role, per window, and come from configuration; nothing
// here is hard-coded. The counter lives in Redis so that every replica
// shares one budget, and it is maintained by a Lua script so that reading,
// incrementing and expiring cannot interleave between replicas.
//
// # Degradation
//
// Redis is not a record of truth (section 23), and rate limiting must not
// stop working when it is gone. When Redis cannot answer, the limiter falls
// back to a per-replica in-memory counter with the same limits. That is
// weaker, because N replicas then allow up to N times the limit, but it is
// far better than the alternatives: failing open removes the protection
// entirely, and failing closed turns a Redis outage into an outage of the
// whole API. Which backend served a decision is reported on the [Decision]
// so that logs and metrics can tell the two apart.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
)

// Backend names which counter decided a request, for logs and metrics.
type Backend string

const (
	// BackendShared is the Redis counter every replica shares.
	BackendShared Backend = "shared"
	// BackendLocal is this replica's own counter, used while Redis is
	// unavailable.
	BackendLocal Backend = "local"
)

// Decision is the outcome of one rate-limit check.
type Decision struct {
	// Allowed is whether the request may proceed.
	Allowed bool
	// Limit is the budget for the window.
	Limit int
	// Remaining is how much of it is left, never negative.
	Remaining int
	// Reset is how long until the window rolls over.
	Reset time.Duration
	// Backend says which counter decided.
	Backend Backend
}

// RetryAfter is the delay to advertise on a refusal, at least one second so
// that a client never reads it as "retry immediately".
func (d Decision) RetryAfter() time.Duration {
	if d.Reset < time.Second {
		return time.Second
	}
	return d.Reset.Round(time.Second)
}

// Identity is who a request is rate-limited as.
type Identity struct {
	// Key identifies the caller: the subject for an authenticated request,
	// the client address otherwise.
	Key string
	// Limit is the budget that applies to them.
	Limit int
	// Role names the limit that was chosen, for logs.
	Role string
}

// script is a fixed-window counter. It increments the key, sets its expiry
// on first use only, and returns the count with the remaining time to live.
// Running it as one script is what makes it correct across replicas: two
// gateways incrementing at once cannot both see the pre-increment value.
//
// The window is fixed rather than sliding, which is the simpler primitive
// and bounds a caller to `limit` per window at the cost of allowing up to
// twice that across a window boundary. The counters are tiny and expire on
// their own, so nothing accumulates.
var script = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  -- The key exists without an expiry, which only a foreign writer could
  -- cause. Give it one rather than letting it live for ever.
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {count, ttl}
`)

// Limiter decides whether a request may proceed.
type Limiter struct {
	cfg    config.RateLimit
	redis  *redisclient.Client
	local  *localLimiter
	logger *slog.Logger
	now    func() time.Time
}

// Options tune a Limiter. Nil fields take safe defaults.
type Options struct {
	Now func() time.Time
}

// New returns a limiter. A nil or disabled Redis client is not an error:
// the limiter then always uses its local counter.
func New(cfg config.RateLimit, client *redisclient.Client, logger *slog.Logger, opts Options) *Limiter {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		cfg:    cfg,
		redis:  client,
		local:  newLocalLimiter(cfg.Window, now),
		logger: logger,
		now:    now,
	}
}

// Identify returns who the request counts against and what budget applies.
// An authenticated request is limited by its subject, so a caller cannot
// escape their budget by changing address; an anonymous one is limited by
// client address.
func (l *Limiter) Identify(r *http.Request) Identity {
	if principal, ok := auth.FromContext(r.Context()); ok {
		limit := l.cfg.Authenticated
		role := string(principal.Role)
		if principal.Role == domain.RoleAdmin {
			limit = l.cfg.Admin
		}
		return Identity{Key: "user:" + principal.UserID.String(), Limit: limit, Role: role}
	}
	return Identity{
		Key:   "ip:" + ClientIP(r, l.cfg.TrustedProxyHops),
		Limit: l.cfg.Anonymous,
		Role:  "anonymous",
	}
}

// Allow counts one request against the identity's budget.
//
// It never returns an error: a limiter that cannot decide must not fail a
// request. When Redis cannot answer, the local counter decides instead and
// the decision says so.
func (l *Limiter) Allow(ctx context.Context, id Identity) Decision {
	if !l.cfg.Enabled {
		return Decision{Allowed: true, Limit: id.Limit, Remaining: id.Limit, Backend: BackendLocal}
	}
	if decision, ok := l.allowShared(ctx, id); ok {
		return decision
	}
	return l.local.allow(id)
}

// allowShared asks Redis. The second return is false when Redis could not
// answer, which is the caller's signal to fall back.
func (l *Limiter) allowShared(ctx context.Context, id Identity) (Decision, bool) {
	if !l.redis.Enabled() {
		return Decision{}, false
	}
	key := l.redis.Key("ratelimit", l.windowLabel(), id.Key)
	raw, err := l.redis.Eval(ctx, script, []string{key}, l.cfg.Window.Milliseconds())
	if err != nil {
		return Decision{}, false
	}
	count, ttl, ok := parseScriptResult(raw)
	if !ok {
		l.logger.WarnContext(ctx, "rate limiter: unusable reply from redis", "reply", fmt.Sprintf("%T", raw))
		return Decision{}, false
	}
	return decide(count, id.Limit, ttl, BackendShared), true
}

// windowLabel puts the window length in the key, so that changing the
// window starts fresh counters rather than reinterpreting old ones.
func (l *Limiter) windowLabel() string {
	return strconv.FormatInt(l.cfg.Window.Milliseconds(), 10) + "ms"
}

func parseScriptResult(raw any) (count int64, ttl time.Duration, ok bool) {
	values, isSlice := raw.([]any)
	if !isSlice || len(values) != 2 {
		return 0, 0, false
	}
	count, okCount := values[0].(int64)
	millis, okTTL := values[1].(int64)
	if !okCount || !okTTL {
		return 0, 0, false
	}
	if millis < 0 {
		millis = 0
	}
	return count, time.Duration(millis) * time.Millisecond, true
}

func decide(count int64, limit int, reset time.Duration, backend Backend) Decision {
	remaining := limit - int(count)
	if remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:   count <= int64(limit),
		Limit:     limit,
		Remaining: remaining,
		Reset:     reset,
		Backend:   backend,
	}
}

// localLimiter is the per-replica fallback: the same fixed window, held in
// memory. It is bounded by construction, because an entry lives at most one
// window and expired entries are swept as they are passed.
type localLimiter struct {
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	windows map[string]*window
	// sweptAt is when expired entries were last removed, so that a key
	// nobody visits again cannot keep its memory for ever.
	sweptAt time.Time
}

type window struct {
	count     int64
	expiresAt time.Time
}

func newLocalLimiter(size time.Duration, now func() time.Time) *localLimiter {
	return &localLimiter{window: size, now: now, windows: map[string]*window{}}
}

// maxLocalKeys bounds the memory the fallback can occupy. Reaching it means
// more distinct callers than a single replica can track in one window; the
// oldest entries are dropped, which can only be generous to a caller, never
// stricter than the limit.
const maxLocalKeys = 100_000

func (l *localLimiter) allow(id Identity) Decision {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)
	w, ok := l.windows[id.Key]
	if !ok || !now.Before(w.expiresAt) {
		w = &window{expiresAt: now.Add(l.window)}
		l.windows[id.Key] = w
	}
	w.count++
	return decide(w.count, id.Limit, w.expiresAt.Sub(now), BackendLocal)
}

// sweep drops expired windows, at most once per window so that a burst of
// requests does not walk the map on every call.
func (l *localLimiter) sweep(now time.Time) {
	if now.Sub(l.sweptAt) < l.window && len(l.windows) < maxLocalKeys {
		return
	}
	l.sweptAt = now
	for key, w := range l.windows {
		if !now.Before(w.expiresAt) {
			delete(l.windows, key)
		}
	}
	// Still too many live windows: this replica is seeing more distinct
	// callers than it can track, so drop the rest rather than grow without
	// bound. Dropping a counter only ever gives a caller more budget.
	if len(l.windows) >= maxLocalKeys {
		clear(l.windows)
	}
}

// ClientIP returns the address a request is rate-limited by.
//
// X-Forwarded-For is only consulted for as many hops as are configured to
// be trusted, counting from the right, because everything to the left of a
// trusted proxy is client-supplied and a client that could choose its own
// entry could choose a fresh budget for every request. With no trusted
// proxies the header is ignored entirely.
func ClientIP(r *http.Request, trustedHops int) string {
	remote := hostOnly(r.RemoteAddr)
	if trustedHops <= 0 {
		return remote
	}
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return remote
	}
	parts := strings.Split(forwarded, ",")
	// The rightmost entry was added by the nearest proxy. Stepping left by
	// the number of trusted hops lands on the address the outermost trusted
	// proxy saw.
	index := len(parts) - trustedHops
	if index < 0 {
		index = 0
	}
	candidate := strings.TrimSpace(parts[index])
	if ip := net.ParseIP(candidate); ip != nil {
		return ip.String()
	}
	return remote
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
