// Package migrations embeds the SQL migration files so that the binary
// carries the schema it needs and deployments never depend on files on disk.
//
// Files are named NNNNNN_description.up.sql and .down.sql and applied in
// numeric order by internal/infra/postgres. See docs/DATABASE.md.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
