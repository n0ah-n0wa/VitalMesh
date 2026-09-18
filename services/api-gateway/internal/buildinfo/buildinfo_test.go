package buildinfo

import (
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// complete is the record a released build produces, as `api-gateway
// version` prints it. It is a literal rather than NewRecord() because a
// test binary cannot produce a real dependency digest; see
// TestTheModulesDigestDescribesTheLinkedDependencies.
func complete() Record {
	return Record{
		Service:          "api-gateway",
		Version:          "89fc016",
		GoVersion:        "go1.25.14",
		Dependencies:     "sha256:900885fb77484aab1cfda735",
		MigrationVersion: 11,
		AlgorithmVersion: "1.0.0",
		ImageDigest:      "sha256:0b1e5b4c1a2f3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6",
	}
}

// The purpose of build metadata is to identify the build. A field that is
// empty identifies nothing, and the failure is silent: a metric with an
// empty label still scrapes and a JSON document with an empty string still
// parses.
func TestCurrentIdentifiesTheBuild(t *testing.T) {
	build := Current()

	if build.Version == "" {
		t.Error("Version is empty; the Makefile and both Dockerfiles set it at link time")
	}
	if !strings.HasPrefix(build.GoVersion, "go1.") {
		t.Errorf("GoVersion = %q, want the toolchain runtime reports", build.GoVersion)
	}
	// Whatever happens, the digest is either a digest that names its
	// algorithm or the documented Unknown. Anything else would be published
	// as a metric label and read as a version.
	switch {
	case build.Modules == Unknown:
	case strings.HasPrefix(build.Modules, "sha256:") && len(build.Modules) == len("sha256:")+24:
	default:
		t.Errorf("Modules = %q, want a sha256: digest of 24 hex characters or %q", build.Modules, Unknown)
	}
}

// The digest has to describe what was actually linked, which is the whole
// reason it comes from the build information rather than from go.sum.
//
// A test binary records no dependency list -- `go test` builds a synthetic
// main package and debug.ReadBuildInfo reports no Deps for it -- so this
// skips there rather than asserting something it cannot see. The real
// binary is checked end to end by scripts/release-metadata-check.sh, which
// runs `api-gateway version` and rejects an unknown digest. That gate, not
// this test, is what holds the released artefact.
func TestTheModulesDigestDescribesTheLinkedDependencies(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok || len(info.Deps) == 0 {
		t.Skip("a test binary records no dependency list; scripts/release-metadata-check.sh checks the real one")
	}
	got := ModulesDigest()
	if got == Unknown {
		t.Fatalf("this binary links %d modules but the digest is %q", len(info.Deps), got)
	}

	// Reproduce it from the same source the function reads. An algorithm
	// that stopped deriving the digest from the module list would diverge.
	want := make([]string, 0, len(info.Deps))
	for _, dep := range info.Deps {
		module := dep
		for module.Replace != nil {
			module = module.Replace
		}
		want = append(want, module.Path+"@"+module.Version)
	}
	if expected := digest(want); got != expected {
		t.Errorf("ModulesDigest = %q, want %q derived from the linked modules", got, expected)
	}
}

// Two calls in one process describe the same binary, so they must agree. A
// digest that moved would make "which build is this?" unanswerable.
func TestTheModulesDigestIsStable(t *testing.T) {
	if first, second := ModulesDigest(), ModulesDigest(); first != second {
		t.Errorf("ModulesDigest is not stable: %q then %q", first, second)
	}
}

// The digest must distinguish dependency sets, and must not depend on the
// order the toolchain happened to record them in.
func TestTheDigestDistinguishesDependencySetsAndIgnoresOrder(t *testing.T) {
	if digest([]string{"example.com/a@v1.0.0"}) == digest([]string{"example.com/a@v1.0.1"}) {
		t.Error("two different dependency sets produced the same digest")
	}
	unsorted := digest([]string{"example.com/b@v1.0.0", "example.com/a@v1.0.0"})
	sorted := digest([]string{"example.com/a@v1.0.0", "example.com/b@v1.0.0"})
	if unsorted != sorted {
		t.Errorf("the digest depends on module order: %q vs %q", unsorted, sorted)
	}
	if !strings.HasPrefix(sorted, "sha256:") {
		t.Errorf("digest = %q, want it to name its algorithm", sorted)
	}
}

// A process cannot read its own image digest, so the deployment supplies
// it. An unset variable must leave the field empty rather than inventing a
// value: there genuinely is no image for a local build.
func TestTheImageDigestComesFromTheDeployment(t *testing.T) {
	t.Setenv(ImageDigestEnv, "")
	if got := Current().ImageDigest; got != "" {
		t.Errorf("ImageDigest = %q with the variable unset, want empty", got)
	}

	const want = "sha256:0b1e5b4c1a2f3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6"
	t.Setenv(ImageDigestEnv, "  "+want+"\n")
	if got := Current().ImageDigest; got != want {
		t.Errorf("ImageDigest = %q, want the value trimmed to %q", got, want)
	}

	// The manifests' placeholder is not a digest. Reporting it would be
	// worse than reporting nothing, because it looks like an answer.
	t.Setenv(ImageDigestEnv, DigestPlaceholder)
	if got := Current().ImageDigest; got != "" {
		t.Errorf("ImageDigest = %q for the unreplaced placeholder, want empty", got)
	}
}

// ---------------------------------------------------------- the record

func TestACompleteRecordIdentifiesEveryRequiredItem(t *testing.T) {
	if missing := complete().Incomplete(); len(missing) > 0 {
		t.Errorf("a complete release record was said not to identify: %s", strings.Join(missing, ", "))
	}
}

// Incomplete is what the CI gate calls, so it has to name the missing field
// rather than merely returning false.
func TestIncompleteNamesWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Record)
		want string
	}{
		{"no version", func(r *Record) { r.Version = "" }, "version"},
		// A plain `go build` produces this, and it is the one failure that
		// looks like success: the binary runs, serves and reports a version
		// that identifies nothing.
		{"unstamped version", func(r *Record) { r.Version = Unstamped }, "version"},
		{"no service", func(r *Record) { r.Service = "" }, "service"},
		{"no go version", func(r *Record) { r.GoVersion = "" }, "go_version"},
		{"unknown dependencies", func(r *Record) { r.Dependencies = Unknown }, "dependencies"},
		{"blank dependencies", func(r *Record) { r.Dependencies = "   " }, "dependencies"},
		{"no migration", func(r *Record) { r.MigrationVersion = 0 }, "migration_version"},
		{"blank algorithm", func(r *Record) { r.AlgorithmVersion = "  " }, "algorithm_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := complete()
			tc.edit(&record)
			missing := record.Incomplete()
			if len(missing) != 1 || missing[0] != tc.want {
				t.Errorf("Incomplete() = %v, want exactly [%s]", missing, tc.want)
			}
		})
	}
}

// The image digest is deliberately not required: a binary run outside a
// container has no image, and saying so is more honest than inventing one.
func TestAMissingImageDigestDoesNotMakeARecordIncomplete(t *testing.T) {
	record := complete()
	record.ImageDigest = ""
	if missing := record.Incomplete(); len(missing) > 0 {
		t.Errorf("a build with no image was called incomplete for: %s", strings.Join(missing, ", "))
	}
}

// NewRecord must carry through the two facts its caller owns, because they
// are the two that no amount of build information can supply.
func TestNewRecordCarriesTheCallersFacts(t *testing.T) {
	record := NewRecord("api-gateway", "2.1.0", 11)
	if record.Service != "api-gateway" {
		t.Errorf("Service = %q", record.Service)
	}
	if record.AlgorithmVersion != "2.1.0" {
		t.Errorf("AlgorithmVersion = %q, want 2.1.0", record.AlgorithmVersion)
	}
	if record.MigrationVersion != 11 {
		t.Errorf("MigrationVersion = %d, want 11", record.MigrationVersion)
	}
}

// The record is printed by `api-gateway version` and parsed by CI, so its
// field names are an interface. Renaming one silently would make the gate
// pass over a field it is no longer reading.
func TestTheRecordSerialisesUnderTheNamesCIReads(t *testing.T) {
	data, err := json.Marshal(complete())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, name := range []string{
		"service", "version", "go_version", "dependencies",
		"migration_version", "algorithm_version", "image_digest",
	} {
		if _, ok := fields[name]; !ok {
			t.Errorf("the record has no %q field: %s", name, data)
		}
	}
	if len(fields) != 7 {
		t.Errorf("the record has %d fields, want 7: %s", len(fields), data)
	}
}

// The record is published. Anything sensitive that reached it would be
// published too, so nothing in this package may read configuration or any
// environment variable beyond the one it documents.
func TestTheRecordDisclosesNothingSensitive(t *testing.T) {
	// Every value is a canary: low-entropy words, so the secret scanner
	// does not read the fixture itself as a leak, and all three carry the
	// same marker so one assertion below catches any of them.
	t.Setenv("DATABASE_URL", "postgres://vitalmesh:canary-value-not-a-secret@db.internal:5432/vitalmesh")
	t.Setenv("JWT_SECRET", "canary-value-not-a-secret")
	t.Setenv("REDIS_URL", "redis://cache.internal:6379")
	t.Setenv("PROCESSOR_TOKEN", "canary-value-not-a-secret")

	data, err := json.Marshal(NewRecord("api-gateway", "1.0.0", 11))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := strings.ToLower(string(data))
	for _, forbidden := range []string{
		"canary", "password", "secret", "token",
		"postgres://", "redis://", "amazonaws.com", ".internal",
		"/home/", "/root/", ".svc.cluster.local",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the release record discloses %q: %s", forbidden, data)
		}
	}
}
