package ratelimit_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/ratelimit"
)

// Everything here runs without Redis, which is both the unconfigured
// deployment and the degraded one: the limiter must keep limiting from this
// replica's own counter. The shared counter is covered by the integration
// tests against a real Redis.

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func settings() config.RateLimit {
	return config.RateLimit{
		Enabled:       true,
		Window:        time.Minute,
		Anonymous:     3,
		Authenticated: 5,
		Admin:         7,
	}
}

func caller(key string, limit int) ratelimit.Identity {
	return ratelimit.Identity{Key: key, Limit: limit, Role: "test"}
}

// A nil client is the "no Redis configured" deployment; it must not panic
// and must not disable the limit.
func TestTheLocalCounterAllowsExactlyTheLimit(t *testing.T) {
	limiter := ratelimit.New(settings(), nil, discard(), ratelimit.Options{})
	ctx := context.Background()
	id := caller("user:"+uuid.NewString(), 3)

	for i := 1; i <= 3; i++ {
		d := limiter.Allow(ctx, id)
		if !d.Allowed {
			t.Fatalf("request %d was refused with %d of %d left", i, d.Remaining, d.Limit)
		}
		if d.Backend != ratelimit.BackendLocal {
			t.Errorf("request %d was decided by %s, want the local counter", i, d.Backend)
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
	if over.RetryAfter() < time.Second {
		t.Errorf("RetryAfter = %s, want at least a second", over.RetryAfter())
	}
}

// A disabled client behaves the same as no client: this is what an
// unreachable Redis degrades to once the recovery interval has opened.
func TestADisabledClientDegradesToTheLocalCounter(t *testing.T) {
	client, err := redisclient.New(config.Redis{}, discard(), redisclient.Options{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	limiter := ratelimit.New(settings(), client, discard(), ratelimit.Options{})
	ctx := context.Background()
	id := caller("ip:198.51.100.9", 2)

	for i := 1; i <= 2; i++ {
		if d := limiter.Allow(ctx, id); !d.Allowed || d.Backend != ratelimit.BackendLocal {
			t.Fatalf("request %d = %+v", i, d)
		}
	}
	if d := limiter.Allow(ctx, id); d.Allowed {
		t.Error("the limit did not apply without Redis")
	}
}

// The window is fixed, so a caller's budget returns when it rolls over.
func TestTheLocalWindowRollsOver(t *testing.T) {
	now := time.Now()
	cfg := settings()
	cfg.Anonymous = 1
	limiter := ratelimit.New(cfg, nil, discard(), ratelimit.Options{Now: func() time.Time { return now }})
	ctx := context.Background()
	id := caller("ip:198.51.100.10", 1)

	if d := limiter.Allow(ctx, id); !d.Allowed {
		t.Fatal("the first request was refused")
	}
	if d := limiter.Allow(ctx, id); d.Allowed {
		t.Fatal("the second request was allowed inside the window")
	}

	now = now.Add(cfg.Window + time.Millisecond)
	if d := limiter.Allow(ctx, id); !d.Allowed {
		t.Error("the budget did not return after the window rolled over")
	}
}

func TestLocalBudgetsAreSeparatePerCaller(t *testing.T) {
	limiter := ratelimit.New(settings(), nil, discard(), ratelimit.Options{})
	ctx := context.Background()

	if d := limiter.Allow(ctx, caller("user:a", 1)); !d.Allowed {
		t.Fatal("the first caller was refused")
	}
	if d := limiter.Allow(ctx, caller("user:b", 1)); !d.Allowed {
		t.Error("the second caller was refused; the budgets are shared")
	}
}

// The local counter is reached concurrently by every request on the
// replica, so it must count each one exactly once.
func TestTheLocalCounterIsSafeUnderConcurrency(t *testing.T) {
	cfg := settings()
	cfg.Authenticated = 20
	limiter := ratelimit.New(cfg, nil, discard(), ratelimit.Options{})
	ctx := context.Background()
	id := caller("user:"+uuid.NewString(), 20)

	const callers = 50
	allowed := make([]bool, callers)
	var wg sync.WaitGroup
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

// Expired windows must not accumulate: a replica that sees a stream of
// one-off callers would otherwise grow without bound.
func TestExpiredLocalWindowsAreReleased(t *testing.T) {
	now := time.Now()
	cfg := settings()
	cfg.Window = time.Second
	limiter := ratelimit.New(cfg, nil, discard(), ratelimit.Options{Now: func() time.Time { return now }})
	ctx := context.Background()

	for i := 0; i < 500; i++ {
		limiter.Allow(ctx, caller("ip:"+uuid.NewString(), 10))
	}
	// Every window above has expired by now, so the next call sweeps them.
	now = now.Add(10 * cfg.Window)
	survivor := caller("ip:survivor", 1)
	if d := limiter.Allow(ctx, survivor); !d.Allowed {
		t.Fatalf("the sweep refused a fresh caller: %+v", d)
	}
	// The swept-out callers get a full budget back, which is the observable
	// consequence of their windows having been released.
	if d := limiter.Allow(ctx, survivor); d.Allowed {
		t.Error("the surviving window was swept while still live")
	}
}

func TestRateLimitingCanBeTurnedOffEntirely(t *testing.T) {
	cfg := settings()
	cfg.Enabled = false
	limiter := ratelimit.New(cfg, nil, discard(), ratelimit.Options{})
	ctx := context.Background()
	id := caller("user:"+uuid.NewString(), 1)

	for i := 0; i < 10; i++ {
		if d := limiter.Allow(ctx, id); !d.Allowed {
			t.Fatalf("request %d was refused with limiting disabled", i)
		}
	}
}

// -------------------------------------------------------- identity

func TestIdentifyUsesTheAccountWhenThereIsOneAndTheAddressOtherwise(t *testing.T) {
	limiter := ratelimit.New(settings(), nil, discard(), ratelimit.Options{})

	anonymous := httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil)
	anonymous.RemoteAddr = "203.0.113.7:5555"
	id := limiter.Identify(anonymous)
	if id.Key != "ip:203.0.113.7" {
		t.Errorf("anonymous key = %q", id.Key)
	}
	if id.Limit != settings().Anonymous {
		t.Errorf("anonymous limit = %d", id.Limit)
	}

	userID := uuid.New()
	authenticated := httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil)
	authenticated.RemoteAddr = "203.0.113.7:5555"
	authenticated = authenticated.WithContext(auth.NewContext(authenticated.Context(),
		auth.Principal{UserID: userID, Role: domain.RoleOperator}))
	id = limiter.Identify(authenticated)
	if want := "user:" + userID.String(); id.Key != want {
		t.Errorf("authenticated key = %q, want %q", id.Key, want)
	}
	if id.Limit != settings().Authenticated {
		t.Errorf("authenticated limit = %d", id.Limit)
	}

	admin := httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil)
	admin = admin.WithContext(auth.NewContext(admin.Context(),
		auth.Principal{UserID: uuid.New(), Role: domain.RoleAdmin}))
	if got := limiter.Identify(admin).Limit; got != settings().Admin {
		t.Errorf("admin limit = %d, want %d", got, settings().Admin)
	}
}

// A client must not be able to choose its own rate-limit identity, so
// X-Forwarded-For counts only for as many hops as are trusted.
func TestTheForwardedHeaderIsOnlyTrustedAsFarAsConfigured(t *testing.T) {
	request := func(forwarded string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		return r
	}

	cases := []struct {
		name      string
		forwarded string
		hops      int
		want      string
	}{
		{"no proxies trusted, header ignored", "1.2.3.4", 0, "10.0.0.1"},
		{"no proxies trusted, spoof ignored", "evil", 0, "10.0.0.1"},
		{"one proxy trusted", "1.2.3.4", 1, "1.2.3.4"},
		{"one proxy trusted, client entry ignored", "9.9.9.9, 1.2.3.4", 1, "1.2.3.4"},
		{"two proxies trusted", "9.9.9.9, 1.2.3.4", 2, "9.9.9.9"},
		{"more hops trusted than present", "1.2.3.4", 5, "1.2.3.4"},
		{"unparseable entry falls back to the socket", "not-an-ip", 1, "10.0.0.1"},
		{"no header at all", "", 2, "10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratelimit.ClientIP(request(tc.forwarded), tc.hops); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
