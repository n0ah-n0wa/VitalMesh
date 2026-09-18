// Package migrations embeds the SQL migration files so that the binary
// carries the schema it needs and deployments never depend on files on disk.
//
// Files are named NNNNNN_description.up.sql and .down.sql and applied in
// numeric order by internal/infra/postgres. See docs/DATABASE.md.
package migrations

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS

// Latest is the highest migration version this binary carries: the schema
// version a deployment of this build migrates the database to. It is a
// property of the build, not of any database, and it is part of what
// identifies a release (docs/RELEASE.md).
//
// It returns 0 with an error only if the embedded files are malformed,
// which the build would have to be broken to produce; a test holds it.
func Latest() (uint, error) {
	names, err := fs.Glob(FS, "*.up.sql")
	if err != nil {
		return 0, err
	}
	if len(names) == 0 {
		return 0, errors.New("no migrations are embedded in this binary")
	}
	var latest uint64
	for _, name := range names {
		number, _, ok := strings.Cut(name, "_")
		if !ok {
			return 0, fmt.Errorf("migration %q is not named NNNNNN_description.up.sql", name)
		}
		version, perr := strconv.ParseUint(number, 10, 32)
		if perr != nil {
			return 0, fmt.Errorf("migration %q has a non-numeric version: %w", name, perr)
		}
		if version == 0 {
			return 0, fmt.Errorf("migration %q is version zero, which means 'no migration applied'", name)
		}
		if version > latest {
			latest = version
		}
	}
	return uint(latest), nil
}
