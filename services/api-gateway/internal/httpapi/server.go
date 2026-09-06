package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

// Run listens on cfg.Addr and serves handler until ctx is cancelled.
func Run(ctx context.Context, handler http.Handler, cfg config.HTTP) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	return Serve(ctx, ln, handler, cfg)
}

// Serve serves handler on ln until ctx is cancelled, then stops accepting
// connections and waits up to cfg.ShutdownTimeout for in-flight requests.
// If they do not finish in time the remaining connections are closed and the
// timeout is reported as an error.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, cfg config.HTTP) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", errors.Join(err, srv.Close()))
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
