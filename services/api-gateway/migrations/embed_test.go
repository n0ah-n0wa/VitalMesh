package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

// The schema version a build migrates to is part of what identifies a
// release, so it has to come from the binary rather than from a constant
// someone has to remember to bump.
func TestLatestIsTheHighestMigrationCarried(t *testing.T) {
	latest, err := Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == 0 {
		t.Fatal("Latest is zero, which means 'no migration applied'")
	}

	names, err := fs.Glob(FS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// Every carried migration must be at or below the reported version, and
	// one of them must be it.
	var found bool
	for _, name := range names {
		number, _, _ := strings.Cut(name, "_")
		switch {
		case len(number) == 0:
			t.Errorf("migration %q has no version prefix", name)
		case number > pad(latest):
			t.Errorf("migration %q is above the reported latest %d", name, latest)
		case number == pad(latest):
			found = true
		}
	}
	if !found {
		t.Errorf("no migration matches the reported latest version %d", latest)
	}
}

// pad renders a version the way the filenames do, so they compare as text.
func pad(version uint) string {
	digits := []byte("000000")
	for i := len(digits) - 1; i >= 0 && version > 0; i-- {
		digits[i] = byte('0' + version%10)
		version /= 10
	}
	return string(digits)
}

// Every up migration needs its down counterpart, or a rollback stops
// halfway. This is the release-metadata test's neighbour rather than its
// subject, but Latest is the first thing that would notice a stray file.
func TestEveryMigrationIsAPair(t *testing.T) {
	ups, err := fs.Glob(FS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, up := range ups {
		down := strings.TrimSuffix(up, ".up.sql") + ".down.sql"
		if _, err := fs.Stat(FS, down); err != nil {
			t.Errorf("%s has no down migration: %v", up, err)
		}
	}
	downs, err := fs.Glob(FS, "*.down.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(ups) != len(downs) {
		t.Errorf("%d up migrations but %d down", len(ups), len(downs))
	}
}
