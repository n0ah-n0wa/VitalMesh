package postgres

import "github.com/jackc/pgx/v5"

type pgxConfig = pgx.ConnConfig

// parseConnConfig parses a connection URL into a pgx connection config with
// the session settings every gateway connection uses.
func parseConnConfig(databaseURL string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["timezone"] = "UTC"
	cfg.RuntimeParams["application_name"] = "api-gateway"
	return cfg, nil
}
