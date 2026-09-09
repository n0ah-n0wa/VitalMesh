//go:build integration

package ratelimit_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/ratelimit"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sharedLimits() config.RateLimit {
	return config.RateLimit{
		Enabled:       true,
		Window:        time.Minute,
		Anonymous:     3,
		Authenticated: 5,
		Admin:         7,
	}
}

func sharedIdentity(key string, limit int) ratelimit.Identity {
	return ratelimit.Identity{Key: key, Limit: limit, Role: "test"}
}

// ------------------------------------------------------ shared counter

func TestTheSharedCounterAllowsExactlyTheLimit(t *testing.T) {
	client, _ := redistest.New(t)
	limiter := ratelimit.New(sharedLimits(), client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 3)

	for i := 1; i <= 3; i++ {
		d := limiter.Allow(ctx, id)
		if !d.Allowed {
			t.Fatalf("request %d was refused with %d of %d left", i, d.Remaining, d.Limit)
		}
		if d.Backend != ratelimit.BackendShared {
			t.Errorf("request %d was decided by %s, want the shared counter", i, d.Backend)
		}
		if want := 3 - i; d.Remaining != want {
			t.Errorf("request %d left %d, want %d", i, d.Remaining, want)
		}
	}

	over := limiter.Allow(ctx, id)
	if over.Allowed {
		t.Fatal("the fourth request was allowed past a limit of three")
	}
	if over.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", over.Remaining)
	}
	if over.Reset <= 0 || over.Reset > time.Minute {
		t.Errorf("reset = %s, want it inside the window", over.Reset)
	}
	if over.RetryAfter() < time.Second {
		t.Errorf("RetryAfter = %s, want at least a second", over.RetryAfter())
	}
}

// Two limiters are two replicas sharing one budget, which is the whole
// point of putting the counter in Redis.
func TestReplicasShareOneBudget(t *testing.T) {
	client, cfg := redistest.New(t)
	other, err := redisclient.New(cfg, discardLog(), redisclient.Options{})
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	defer func() { _ = other.Close() }()

	first := ratelimit.New(sharedLimits(), client, discardLog(), ratelimit.Options{})
	second := ratelimit.New(sharedLimits(), other, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 3)

	if d := first.Allow(ctx, id); !d.Allowed {
		t.Fatal("first replica refused the first request")
	}
	if d := second.Allow(ctx, id); !d.Allowed {
		t.Fatal("second replica refused the second request")
	}
	if d := first.Allow(ctx, id); !d.Allowed {
		t.Fatal("first replica refused the third request")
	}
	if d := second.Allow(ctx, id); d.Allowed {
		t.Error("the fourth request was allowed: the replicas are not sharing a budget")
	}
}

// The counter is maintained by a Lua script so that concurrent callers
// cannot both read the same pre-increment value.
func TestConcurrentRequestsAreCountedExactlyOnce(t *testing.T) {
	client, _ := redistest.New(t)
	cfg := sharedLimits()
	cfg.Authenticated = 20
	limiter := ratelimit.New(cfg, client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 20)

	const callers = 50
	var wg sync.WaitGroup
	allowed := make([]bool, callers)
	for i := range allowed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed[i] = limiter.Allow(ctx, id).Allowed
		}()
	}
	wg.Wait()

	count := 0
	for _, ok := range allowed {
		if ok {
			count++
		}
	}
	if count != 20 {
		t.Errorf("%d of %d concurrent requests were allowed, want exactly the limit of 20", count, callers)
	}
}

func TestTheWindowExpiresOnItsOwn(t *testing.T) {
	client, _ := redistest.New(t)
	cfg := sharedLimits()
	cfg.Window = 300 * time.Millisecond
	cfg.Authenticated = 1
	limiter := ratelimit.New(cfg, client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 1)

	if d := limiter.Allow(ctx, id); !d.Allowed {
		t.Fatal("the first request was refused")
	}
	if d := limiter.Allow(ctx, id); d.Allowed {
		t.Fatal("the second request was allowed inside the window")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if limiter.Allow(ctx, id).Allowed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the window never rolled over")
}

// Two identities never share a budget, whatever they are.
func TestIdentitiesAreCountedSeparately(t *testing.T) {
	client, _ := redistest.New(t)
	limiter := ratelimit.New(sharedLimits(), client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	first := sharedIdentity("user:"+uuid.NewString(), 1)
	second := sharedIdentity("user:"+uuid.NewString(), 1)

	if d := limiter.Allow(ctx, first); !d.Allowed {
		t.Fatal("the first identity was refused")
	}
	if d := limiter.Allow(ctx, second); !d.Allowed {
		t.Error("the second identity was refused; the budgets are shared")
	}
}

// ------------------------------------------------------ key lifetime

// Nothing the limiter writes may outlive its window. A counter without an
// expiry would be a key that lives for ever, one per caller, so the script
// sets the expiry when it creates the counter and repairs a counter that
// somehow lacks one.
func TestEveryCounterKeyCarriesAnExpiry(t *testing.T) {
	client, _ := redistest.New(t)
	limits := sharedLimits()
	limiter := ratelimit.New(limits, client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 3)

	if d := limiter.Allow(ctx, id); !d.Allowed {
		t.Fatalf("the first request was refused: %+v", d)
	}

	key := counterKey(client, limits, id)
	ttl := pttl(ctx, t, client, key)
	if ttl <= 0 {
		t.Fatalf("the counter key has no expiry (PTTL = %s); it would live for ever", ttl)
	}
	if ttl > limits.Window {
		t.Errorf("PTTL = %s, want no more than the window of %s", ttl, limits.Window)
	}

	// The reported reset and the key's real expiry are the same fact, so a
	// client that waits for the advertised reset finds the window gone.
	if d := limiter.Allow(ctx, id); absDuration(d.Reset-pttl(ctx, t, client, key)) > time.Second {
		t.Errorf("Reset = %s but the key expires in %s", d.Reset, pttl(ctx, t, client, key))
	}
}

// A counter left without an expiry, which only a foreign writer could
// cause, must be given one rather than pinning a key for ever.
func TestACounterFoundWithoutAnExpiryIsGivenOne(t *testing.T) {
	client, _ := redistest.New(t)
	limits := sharedLimits()
	limiter := ratelimit.New(limits, client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 3)
	key := counterKey(client, limits, id)

	// Plant a counter with no expiry, as a stray writer would.
	if _, err := client.Eval(ctx, redis.NewScript(`return redis.call('SET', KEYS[1], '1')`), []string{key}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if ttl := pttl(ctx, t, client, key); ttl >= 0 {
		t.Fatalf("the seeded key already had an expiry (%s); the test proves nothing", ttl)
	}

	if d := limiter.Allow(ctx, id); !d.Allowed {
		t.Fatalf("the request was refused: %+v", d)
	}
	if ttl := pttl(ctx, t, client, key); ttl <= 0 {
		t.Errorf("PTTL = %s: the counter was left without an expiry", ttl)
	}
}

// counterKey is where the limiter keeps one identity's counter. The layout
// is the limiter's own; the test builds it the same way so that it can look
// at what was actually written.
func counterKey(client *redisclient.Client, cfg config.RateLimit, id ratelimit.Identity) string {
	window := strconv.FormatInt(cfg.Window.Milliseconds(), 10) + "ms"
	return client.Key("ratelimit", window, id.Key)
}

// pttl reports a key's remaining life. A key with no expiry reports a
// negative duration, which is exactly what these tests are looking for.
func pttl(ctx context.Context, t *testing.T, client *redisclient.Client, key string) time.Duration {
	t.Helper()
	raw, err := client.Eval(ctx, redis.NewScript(`return redis.call('PTTL', KEYS[1])`), []string{key})
	if err != nil {
		t.Fatalf("PTTL %s: %v", key, err)
	}
	millis, ok := raw.(int64)
	if !ok {
		t.Fatalf("PTTL %s returned %T", key, raw)
	}
	return time.Duration(millis) * time.Millisecond
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ------------------------------------------------------- degradation

// With Redis gone the limiter must keep limiting, using this replica's own
// counter, and must say which counter decided.
func TestTheLimiterFallsBackToALocalCounterWithoutRedis(t *testing.T) {
	client, err := redisclient.New(redistest.Unreachable(), discardLog(), redisclient.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer func() { _ = client.Close() }()

	limiter := ratelimit.New(sharedLimits(), client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 3)

	for i := 1; i <= 3; i++ {
		d := limiter.Allow(ctx, id)
		if !d.Allowed {
			t.Fatalf("request %d was refused while degraded", i)
		}
		if d.Backend != ratelimit.BackendLocal {
			t.Errorf("request %d was decided by %s, want the local counter", i, d.Backend)
		}
	}
	if d := limiter.Allow(ctx, id); d.Allowed {
		t.Error("the limit stopped applying when Redis went away")
	}
}

// Turning rate limiting off must allow everything, whatever Redis does.
func TestRateLimitingCanBeTurnedOff(t *testing.T) {
	client, _ := redistest.New(t)
	cfg := sharedLimits()
	cfg.Enabled = false
	limiter := ratelimit.New(cfg, client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 1)

	for i := 0; i < 10; i++ {
		if d := limiter.Allow(ctx, id); !d.Allowed {
			t.Fatalf("request %d was refused with limiting disabled", i)
		}
	}
}

// ---------------------------------------------------- outage and return

// An outage in the middle of a deployment's life is the case the fallback
// exists for: the limiter must keep limiting while Redis is gone and go
// back to the shared counter once it returns, without losing the count it
// had already taken.
func TestTheLimiterSurvivesAnOutageAndReturnsToTheSharedCounter(t *testing.T) {
	_, live := redistest.New(t)
	proxy := newRedisProxy(t, live.URL)

	cfg := live
	cfg.URL = proxy.url()
	client, err := redisclient.New(cfg, discardLog(), redisclient.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer func() { _ = client.Close() }()

	limits := sharedLimits()
	limits.Authenticated = 3
	limiter := ratelimit.New(limits, client, discardLog(), ratelimit.Options{})
	ctx := context.Background()
	id := sharedIdentity("user:"+uuid.NewString(), 3)

	first := limiter.Allow(ctx, id)
	if !first.Allowed || first.Backend != ratelimit.BackendShared {
		t.Fatalf("the first request = %+v, want an allowed shared decision", first)
	}

	// Redis goes away mid-flight.
	proxy.stop()
	degraded := limiter.Allow(ctx, id)
	if !degraded.Allowed {
		t.Fatalf("a request was refused because Redis went away: %+v", degraded)
	}
	if degraded.Backend != ratelimit.BackendLocal {
		t.Errorf("backend = %s during the outage, want the local counter", degraded.Backend)
	}
	// The limit still applies, from this replica's own counter.
	for i := 0; i < 3; i++ {
		limiter.Allow(ctx, id)
	}
	if d := limiter.Allow(ctx, id); d.Allowed {
		t.Error("the limit stopped applying during the outage")
	}

	// Redis comes back.
	proxy.start()
	// Wait for the proxy to be accepting again before asking anything of
	// the limiter. Without this the test races the listener coming back and
	// reports "the limiter never recovered" when what actually happened is
	// that Redis was still unreachable, which is a different bug entirely.
	proxy.waitUntilAccepting(t)

	// The client refuses to dial for a recovery interval after a failure,
	// so recovery is not instant even once Redis is reachable. The deadline
	// is generous because this runs alongside every other integration
	// package, on a machine that may be busy.
	deadline := time.Now().Add(30 * time.Second)
	var recovered ratelimit.Decision
	for time.Now().Before(deadline) {
		recovered = limiter.Allow(ctx, id)
		if recovered.Backend == ratelimit.BackendShared {
			break
		}
		time.Sleep(cfg.RecoveryInterval)
	}
	if recovered.Backend != ratelimit.BackendShared {
		t.Fatalf("the limiter never returned to the shared counter: %+v", recovered)
	}
	// The shared counter kept what it had counted before the outage, so the
	// caller does not get a fresh budget out of an outage.
	if recovered.Remaining >= limits.Authenticated {
		t.Errorf("remaining = %d of %d after recovery: the shared count was lost",
			recovered.Remaining, limits.Authenticated)
	}
}

// redisProxy is a TCP forwarder in front of Redis that a test can switch
// off and on, which is how an outage is simulated without touching the
// server other tests are using.
type redisProxy struct {
	t       *testing.T
	backend string

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn
	addr     string
}

func newRedisProxy(t *testing.T, backendURL string) *redisProxy {
	t.Helper()
	opts, err := redis.ParseURL(backendURL)
	if err != nil {
		t.Fatalf("redis url: %v", err)
	}
	p := &redisProxy{t: t, backend: opts.Addr}
	p.start()
	t.Cleanup(p.stop)
	return p
}

// url is the address to point a client at.
func (p *redisProxy) url() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return "redis://" + p.addr
}

// start begins forwarding, reusing the address of any previous run so that
// a client configured once keeps working across an outage.
func (p *redisProxy) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil {
		return
	}
	addr := p.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		p.t.Fatalf("proxy listen: %v", err)
	}
	p.listener = listener
	p.addr = listener.Addr().String()
	go p.accept(listener)
}

// waitUntilAccepting blocks until the proxy answers a connection, so a test
// never mistakes a listener that has not come back for a client that has
// not recovered.
func (p *redisProxy) waitUntilAccepting(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", p.url()[len("redis://"):], time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the proxy never started accepting again")
}

// stop closes the listener and every connection through it, so that calls
// in flight fail rather than hang.
func (p *redisProxy) stop() {
	p.mu.Lock()
	listener, conns := p.listener, p.conns
	p.listener, p.conns = nil, nil
	p.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (p *redisProxy) accept(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		backend, err := net.Dial("tcp", p.backend)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		open := p.listener == listener
		if open {
			p.conns = append(p.conns, client, backend)
		}
		p.mu.Unlock()
		if !open {
			_ = client.Close()
			_ = backend.Close()
			return
		}
		go pipe(client, backend)
		go pipe(backend, client)
	}
}

func pipe(from, to net.Conn) {
	defer func() { _ = to.Close() }()
	defer func() { _ = from.Close() }()
	buf := make([]byte, 4096)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			if _, werr := to.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
