// Command api-gateway runs the VitalMesh public API gateway.
//
// Exit codes: 0 on clean shutdown, 1 on a runtime failure, 2 on invalid
// configuration.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/app"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		// The logger is configured from cfg, so this is the one message that
		// cannot be structured.
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	logger := logging.New(os.Stdout, cfg.Log, logging.Service{
		Name:        app.ServiceName,
		Version:     buildinfo.Version,
		Environment: string(cfg.Environment),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.New(cfg, logger, buildinfo.Version).Run(ctx); err != nil {
		logger.Error("gateway exited", "error", err)
		return 1
	}
	return 0
}
