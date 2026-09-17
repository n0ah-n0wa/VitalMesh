//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

// Connection-pool exhaustion, safely testable at the pool level
// (SPECIFICATIONS.md section 52). A pool of two connections has both held
// open; a third acquire must not hang forever. It waits only as long as
// the caller's context allows and then fails predictably, and once a
// connection is returned the pool serves again. That is the property that
// keeps a burst of slow queries from cascading into a stuck gateway: every
// database call is bounded by the request's own deadline.
func TestPoolExhaustionFailsPredictablyAndRecovers(t *testing.T) {
	t.Parallel()
	dbURL, _ := postgrestest.NewSchema(t)
	pool, err := postgres.Connect(context.Background(), config.Database{URL: dbURL, MaxConns: 2, ConnectTimeout: 5 * time.Second}, postgres.Options{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	c := context.Background()

	// Hold both connections in open transactions.
	held := make([]pgx.Tx, 0, 2)
	for i := 0; i < 2; i++ {
		tx, err := pool.Begin(c)
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		held = append(held, tx)
	}

	// A third acquire with a short deadline must return promptly, not hang.
	started := time.Now()
	waitCtx, cancel := context.WithTimeout(c, 500*time.Millisecond)
	defer cancel()
	_, err = pool.Query(waitCtx, "SELECT 1")
	waited := time.Since(started)
	if err == nil {
		t.Fatal("a query against an exhausted pool returned no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("exhausted-pool error = %v, want a deadline, not a hang or a panic", err)
	}
	if waited > 2*time.Second {
		t.Errorf("the acquire waited %s; it should have been bounded by the 500ms context", waited)
	}

	// Release one connection; the pool serves again at once.
	held[0].Rollback(c)
	okCtx, cancelOK := context.WithTimeout(c, 5*time.Second)
	defer cancelOK()
	var one int
	if err := pool.QueryRow(okCtx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("after releasing a connection: %v (got %d)", err, one)
	}
	held[1].Rollback(c)

	// The pool is whole: its stats show no leaked or dangling connections.
	if stat := pool.Stat(); stat.AcquiredConns() != 0 {
		t.Errorf("%d connections still acquired after the test, want 0", stat.AcquiredConns())
	}
}
