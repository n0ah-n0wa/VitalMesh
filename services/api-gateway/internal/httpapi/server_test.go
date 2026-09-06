package httpapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

func testHTTPConfig() config.HTTP {
	return config.HTTP{
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       time.Second,
		WriteTimeout:      time.Second,
		IdleTimeout:       time.Second,
		ShutdownTimeout:   2 * time.Second,
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	inflight := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inflight)
		<-release
		_, _ = w.Write([]byte("done"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, ln, handler, testHTTPConfig()) }()

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		resCh <- result{body: string(b), err: err}
	}()

	<-inflight
	cancel() // shutdown begins while a request is in flight
	close(release)

	res := <-resCh
	if res.err != nil || res.body != "done" {
		t.Errorf("in-flight request was not completed: body=%q err=%v", res.body, res.err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestServeForcesCloseWhenShutdownTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	inflight := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inflight)
		<-release
	})

	cfg := testHTTPConfig()
	cfg.ShutdownTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, ln, handler, cfg) }()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-inflight
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve returned %v, want a shutdown timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve hung on a request that never finished")
	}
}

func TestRunFailsOnUnusableAddress(t *testing.T) {
	cfg := testHTTPConfig()
	cfg.Addr = "127.0.0.1:-1"
	if err := Run(context.Background(), http.NotFoundHandler(), cfg); err == nil {
		t.Fatal("Run returned nil for an invalid address")
	}
}
