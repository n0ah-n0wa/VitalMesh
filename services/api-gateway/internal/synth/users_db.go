package synth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
)

// CreateUsers creates the fixture's accounts directly in the database, the
// way `api-gateway users create` does: there is no public endpoint that
// creates accounts, by design. It needs a database URL, which only a local
// environment or a job inside the cluster has, and it is the only part of
// a load that does not go through the API.
//
// Each password is [Password] of the fixture's seed and the address. An
// account that exists already is left exactly as it is, password included.
// It returns how many were created and how many existed.
func CreateUsers(ctx context.Context, dbURL string, seed int64, users []User, lookup config.Lookup, log io.Writer) (created, existing int, err error) {
	if len(users) == 0 {
		return 0, 0, nil
	}
	hashCfg, err := config.LoadPasswordHash(lookup)
	if err != nil {
		return 0, 0, err
	}
	hasher := auth.NewHasher(hashCfg)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pool, err := postgres.Connect(ctx, config.Database{URL: dbURL, MaxConns: 1, ConnectTimeout: 5 * time.Second}, postgres.Options{})
	if err != nil {
		return 0, 0, fmt.Errorf("connect to the database: %w", err)
	}
	defer pool.Close()

	for _, u := range users {
		email := auth.NormalizeEmail(u.Email)
		hash, err := hasher.Hash(ctx, Password(seed, email))
		if err != nil {
			return created, existing, err
		}
		err = postgres.WithTx(ctx, pool, func(tx pgx.Tx) error {
			acct, err := postgres.NewUsers(tx).Create(ctx, email, hash, u.Role)
			if err != nil {
				return err
			}
			_, err = postgres.NewAudit(tx).Append(ctx, postgres.NewAuditEntry{
				ActorType:    domain.ActorSystem,
				Action:       domain.AuditUserCreated,
				ResourceType: "user",
				ResourceID:   &acct.ID,
				RequestID:    "cli:synth-load",
			})
			return err
		})
		var domErr *domain.Error
		switch {
		case err == nil:
			created++
			if log != nil {
				fmt.Fprintf(log, "user %s (%s): created\n", email, u.Role)
			}
		case errors.As(err, &domErr) && domErr.Kind == domain.KindConflict:
			existing++
			if log != nil {
				fmt.Fprintf(log, "user %s: already there, left as is\n", email)
			}
		default:
			return created, existing, fmt.Errorf("user %s: %w", email, err)
		}
	}
	return created, existing, nil
}
