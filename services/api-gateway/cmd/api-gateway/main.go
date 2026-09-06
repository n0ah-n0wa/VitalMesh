// Command api-gateway runs the VitalMesh public API gateway.
//
// Usage:
//
//	api-gateway [serve]                 run the HTTP server (default)
//	api-gateway migrate up              apply pending migrations
//	api-gateway migrate down [steps]    roll back migrations (default 1)
//	api-gateway migrate version         print the schema version
//	api-gateway migrate force <version> reset a dirty version (see docs/DATABASE.md)
//
// Exit codes: 0 on success, 1 on a runtime failure, 2 on invalid
// configuration or usage.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/app"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		return serve()
	}
	switch args[0] {
	case "serve":
		return serve()
	case "migrate":
		return migrate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; see the package documentation\n", args[0])
		return 2
	}
}

func serve() int {
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

	a, err := app.New(ctx, cfg, logger, buildinfo.Version)
	if err != nil {
		logger.Error("gateway failed to start", "error", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		logger.Error("gateway exited", "error", err)
		return 1
	}
	return 0
}

func migrate(args []string) int {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: api-gateway migrate up|down [steps]|version|force <version>")
		return 2
	}

	m, err := postgres.NewMigrator(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer m.Close()

	switch args[0] {
	case "up":
		err = m.Up()
	case "down":
		steps := 1
		if len(args) > 1 {
			if steps, err = strconv.Atoi(args[1]); err != nil {
				fmt.Fprintf(os.Stderr, "invalid step count %q\n", args[1])
				return 2
			}
		}
		err = m.Down(steps)
	case "version":
		version, dirty, verr := m.Version()
		if verr != nil {
			err = verr
			break
		}
		fmt.Printf("version=%d dirty=%v\n", version, dirty)
	case "force":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: api-gateway migrate force <version>")
			return 2
		}
		version, perr := strconv.Atoi(args[1])
		if perr != nil {
			fmt.Fprintf(os.Stderr, "invalid version %q\n", args[1])
			return 2
		}
		err = m.Force(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown migrate command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
