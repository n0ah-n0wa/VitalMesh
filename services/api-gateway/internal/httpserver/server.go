// Package httpserver wires the HTTP router and runs the server with graceful shutdown.
package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
)

// NewRouter returns the HTTP handler exposing the service's routes.
func NewRouter(h *health.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Live)
	mux.HandleFunc("GET /ready", h.Ready)
	return mux
}

// Run listens on addr and serves handler until ctx is cancelled.
func Run(ctx context.Context, addr string, handler http.Handler, shutdownTimeout time.Duration) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, ln, handler, shutdownTimeout)
}

// Serve serves handler on ln until ctx is cancelled, then shuts down
// gracefully, waiting up to shutdownTimeout for in-flight requests.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
