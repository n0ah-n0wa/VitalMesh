// Command api-gateway runs the VitalMesh public API gateway.
//
// Usage:
//
//	api-gateway [serve]                 run the HTTP server (default)
//	api-gateway migrate up              apply pending migrations
//	api-gateway migrate down [steps]    roll back migrations (default 1)
//	api-gateway migrate version         print the schema version
//	api-gateway migrate force <version> reset a dirty version (see docs/DATABASE.md)
//	api-gateway users create <email> <role>
//	                                    create an account; the password is read
//	                                    from standard input
//
// Exit codes: 0 on success, 1 on a runtime failure, 2 on invalid
// configuration or usage.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/app"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin))
}

func run(args []string, stdin io.Reader) int {
	if len(args) == 0 {
		return serve()
	}
	switch args[0] {
	case "serve":
		return serve()
	case "migrate":
		return migrate(args[1:])
	case "users":
		return users(args[1:], stdin)
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

func databaseURL() (string, bool) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		return "", false
	}
	return url, true
}

func migrate(args []string) int {
	url, ok := databaseURL()
	if !ok {
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

// users implements `users create <email> <role>`. The password comes from
// standard input (one line) so that it appears in neither the process list
// nor the environment. This is how the first administrator is bootstrapped.
func users(args []string, stdin io.Reader) int {
	const usage = "usage: echo \"$PASSWORD\" | api-gateway users create <email> <ADMIN|OPERATOR|USER>"
	if len(args) != 3 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	email := auth.NormalizeEmail(args[1])
	role := domain.Role(strings.ToUpper(args[2]))
	switch role {
	case domain.RoleAdmin, domain.RoleOperator, domain.RoleUser:
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	url, ok := databaseURL()
	if !ok {
		return 2
	}
	hashCfg, err := config.LoadPasswordHash(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	password, err := readPassword(stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := auth.ValidateNewPassword(password); err != nil {
		var domErr *domain.Error
		if errors.As(err, &domErr) && len(domErr.Details) > 0 {
			fmt.Fprintf(os.Stderr, "password %s\n", domErr.Details[0].Message)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	hash, err := auth.NewHasher(hashCfg).Hash(context.Background(), password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := postgres.Connect(ctx, config.Database{URL: url, MaxConns: 1, ConnectTimeout: 5 * time.Second}, postgres.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	var created domain.User
	err = postgres.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		created, err = postgres.NewUsers(tx).Create(ctx, email, hash, role)
		if err != nil {
			return err
		}
		_, err = postgres.NewAudit(tx).Append(ctx, postgres.NewAuditEntry{
			ActorType:    domain.ActorSystem,
			Action:       domain.AuditUserCreated,
			ResourceType: "user",
			ResourceID:   &created.ID,
			RequestID:    "cli:users-create",
		})
		return err
	})
	if err != nil {
		var domErr *domain.Error
		if errors.As(err, &domErr) && domErr.Kind == domain.KindConflict {
			fmt.Fprintln(os.Stderr, "a user with that email already exists")
			return 1
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("created user %s (%s) with role %s\n", created.ID, created.Email, created.Role)
	return 0
}

// readPassword reads one line from r, bounded so a stray stream cannot be
// consumed indefinitely.
func readPassword(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, auth.MaxPasswordLength+2))
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if password == "" {
		return "", errors.New("a password is required on standard input")
	}
	return password, nil
}
