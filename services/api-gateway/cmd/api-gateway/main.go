// Command api-gateway runs the VitalMesh public API gateway.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpserver"
)

const (
	serviceName     = "api-gateway"
	defaultAddr     = ":8080"
	shutdownTimeout = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"service", serviceName,
		"version", buildinfo.Version,
	)

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	router := httpserver.NewRouter(health.NewHandler(serviceName, buildinfo.Version))

	logger.Info("starting", "addr", addr)
	if err := httpserver.Run(ctx, addr, router, shutdownTimeout); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
	logger.Info("stopped")
}
