//go:build integration || e2e

// Package redistest gives integration tests a real Redis, isolated from
// every other test.
//
// Isolation is by key namespace rather than by database number: a namespace
// is what the production client already uses, so tests exercise the same key
// shapes the gateway builds, and several packages can run in parallel
// against one server. Every namespace a test creates is deleted when it
// ends.
package redistest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
)

// URLEnv names the environment variable holding the test server's address.
const URLEnv = "TEST_REDIS_URL"

// URL returns the configured address, skipping the test when there is none
// so that a developer without Redis still gets a green run and CI, which
// provides one, gets the real thing.
func URL(t *testing.T) string {
	t.Helper()
	url := os.Getenv(URLEnv)
	if url == "" {
		t.Skipf("%s is not set; start Redis with `make dev-redis` to run this test", URLEnv)
	}
	return url
}

// Config returns a Redis configuration pointing at the test server, in a
// namespace private to this test.
func Config(t *testing.T) config.Redis {
	t.Helper()
	return config.Redis{
		URL:              URL(t),
		DialTimeout:      2 * time.Second,
		Timeout:          2 * time.Second,
		PoolSize:         4,
		Namespace:        namespace(t),
		RecoveryInterval: 50 * time.Millisecond,
	}
}

// New returns a client on a private namespace, cleaned up when the test
// ends. It fails the test if the server cannot be reached, because a test
// that asked for real Redis must not quietly pass without it.
func New(t *testing.T) (*redisclient.Client, config.Redis) {
	t.Helper()
	cfg := Config(t)
	client, err := redisclient.New(cfg, testLogger(), redisclient.Options{})
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("redis at %s is not reachable: %v", cfg.URL, err)
	}
	t.Cleanup(func() {
		Flush(t, cfg)
		_ = client.Close()
	})
	return client, cfg
}

// Flush removes every key of the namespace. It uses SCAN rather than KEYS
// so that a shared server is never blocked by a test's cleanup.
func Flush(t *testing.T, cfg config.Redis) {
	t.Helper()
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		t.Fatalf("redis url: %v", err)
	}
	rdb := redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, cfg.Namespace+":*", 256).Result()
		if err != nil {
			t.Logf("redis cleanup scan: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := rdb.Del(ctx, keys...).Err(); err != nil {
				t.Logf("redis cleanup delete: %v", err)
			}
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

// Unreachable returns a configuration pointing at an address nothing
// listens on, for the failure-path tests. The port is in the range IANA
// keeps for dynamic use and the address is the loopback, so the connection
// is refused immediately rather than timing out on a route to nowhere.
func Unreachable() config.Redis {
	return config.Redis{
		URL:              "redis://127.0.0.1:1",
		DialTimeout:      200 * time.Millisecond,
		Timeout:          200 * time.Millisecond,
		PoolSize:         2,
		Namespace:        "vitalmesh-unreachable",
		RecoveryInterval: 50 * time.Millisecond,
	}
}

// testLogger discards output: a test asserts on behaviour, not on logs.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var namespaceCounter int

// namespace builds a name unique to this test and process.
func namespace(t *testing.T) string {
	t.Helper()
	namespaceCounter++
	return fmt.Sprintf("vmtest.%d.%d", os.Getpid(), namespaceCounter)
}
