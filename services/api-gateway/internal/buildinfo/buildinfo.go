// Package buildinfo identifies the build this process came from: the commit
// it was built at, the toolchain that built it, the dependency set compiled
// into it, and the image it is running as.
//
// Everything here is safe to publish. It names versions and digests, never
// a path, a host, a credential or anything about the infrastructure the
// process is running on (SPECIFICATIONS.md section 41).
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

// Version identifies the build, normally the git commit. It is overridden at
// link time by the Makefile and by both Dockerfiles:
//
//	-X github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo.Version=<git-sha>
var Version = "dev"

// ImageDigestEnv names the environment variable a deployment sets to the
// digest of the image it pinned. A process cannot read its own image digest
// portably, and the deployment is the one place that knows it: the pipeline
// deploys by digest, so it has the value at hand.
const ImageDigestEnv = "IMAGE_DIGEST"

// DigestPlaceholder is the value the base Kubernetes manifests carry, which
// the deployment replaces with the digest it is deploying. It is treated as
// no digest at all, because reporting it would be worse than reporting
// nothing: it looks like an answer. The deployment refuses to apply a
// manifest where it survives, so this only ever shows up somewhere the
// manifests were applied by hand, such as a local cluster.
const DigestPlaceholder = "set-by-ci"

// Unknown is reported for a field this build cannot determine, so that a
// missing value is never mistaken for an empty one.
const Unknown = "unknown"

// Unstamped is the Version an unstamped build carries. It is the default
// value above, so it is what a plain `go build` produces: a working binary
// that cannot say which commit it came from. Incomplete treats it as a
// missing version rather than as a version, because a release that reports
// it is a release nobody can trace.
const Unstamped = "dev"

// Build is everything this process can say about where it came from.
type Build struct {
	// Version is the git commit the binary was built at.
	Version string
	// GoVersion is the toolchain that compiled it, as runtime reports it.
	GoVersion string
	// Modules is a digest over the module set compiled in: every
	// dependency path and version, sorted, hashed. Two builds with the same
	// digest were built from the same dependencies, which is the question a
	// lockfile answers. The digest is published rather than the list
	// because the list is long and the digest is what identifies it.
	Modules string
	// ImageDigest is the digest of the container image, when the
	// deployment supplied one, otherwise empty. It is empty for a local
	// build, which is honest: there is no image.
	ImageDigest string
}

// Current reads the build metadata of the running process.
func Current() Build {
	return Build{
		Version:     Version,
		GoVersion:   runtime.Version(),
		Modules:     ModulesDigest(),
		ImageDigest: imageDigest(),
	}
}

// imageDigest is the digest the deployment supplied, or empty when it
// supplied nothing usable.
func imageDigest() string {
	digest := strings.TrimSpace(os.Getenv(ImageDigestEnv))
	if digest == DigestPlaceholder {
		return ""
	}
	return digest
}

// ModulesDigest is a stable digest over the dependency set compiled into
// this binary, taken from the build information the toolchain records.
//
// It identifies the same thing go.sum does and has one advantage over
// hashing that file: it describes what was actually linked, not what the
// file said at the time. A build with a stale or edited go.sum produces a
// different digest here.
func ModulesDigest() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return Unknown
	}
	lines := make([]string, 0, len(info.Deps))
	for _, dep := range info.Deps {
		if dep == nil {
			continue
		}
		// Follow a replacement: what was linked is what counts.
		module := dep
		for module.Replace != nil {
			module = module.Replace
		}
		lines = append(lines, module.Path+"@"+module.Version)
	}
	if len(lines) == 0 {
		return Unknown
	}
	return digest(lines)
}

// digest hashes a module list into the published form. It sorts, so the
// digest identifies the set rather than the order the toolchain happened
// to record it in.
func digest(modules []string) string {
	sorted := make([]string, len(modules))
	copy(sorted, modules)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	// Twelve bytes is plenty to tell two dependency sets apart and short
	// enough to read in a label or a log line.
	return "sha256:" + hex.EncodeToString(sum[:12])
}

// Record is the release record for this build: every fact that identifies
// what is running, in one JSON document. `api-gateway version` prints it,
// CI validates it, and an operator can read it out of a running container
// without a scraper.
//
// It is assembled by the caller rather than read here, because two of its
// fields belong to other packages: the schema version comes from the
// embedded migrations and the algorithm version from configuration. Keeping
// this package free of both keeps it importable from anywhere.
type Record struct {
	Service string `json:"service"`
	// Version is the git commit, the first question asked about a release.
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	// Dependencies is the digest over the module set; see ModulesDigest.
	Dependencies string `json:"dependencies"`
	// MigrationVersion is the schema version this build migrates to, which
	// is a property of the binary. What a given database is actually at is
	// a different question, answered by `migrate version` and published as
	// the schema-version metric.
	MigrationVersion uint   `json:"migration_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	// ImageDigest is empty for a build that is not running from an image.
	ImageDigest string `json:"image_digest"`
}

// NewRecord assembles the release record from this build plus the two facts
// its caller owns.
func NewRecord(service, algorithmVersion string, migrationVersion uint) Record {
	build := Current()
	return Record{
		Service:          service,
		Version:          build.Version,
		GoVersion:        build.GoVersion,
		Dependencies:     build.Modules,
		MigrationVersion: migrationVersion,
		AlgorithmVersion: algorithmVersion,
		ImageDigest:      build.ImageDigest,
	}
}

// Incomplete names the fields that do not identify anything, so that a
// release can be rejected for being unidentifiable rather than shipping
// with "dev" in it. The image digest is not required: a binary run outside
// a container has no image, and saying so is more honest than inventing
// one.
//
// A CI gate calls this; see scripts/release-metadata-check.sh.
func (r Record) Incomplete() []string {
	var missing []string
	add := func(field, value string) {
		if strings.TrimSpace(value) == "" || value == Unknown {
			missing = append(missing, field)
		}
	}
	add("service", r.Service)
	add("version", r.Version)
	if r.Version == Unstamped {
		missing = append(missing, "version")
	}
	add("go_version", r.GoVersion)
	add("dependencies", r.Dependencies)
	add("algorithm_version", r.AlgorithmVersion)
	if r.MigrationVersion == 0 {
		missing = append(missing, "migration_version")
	}
	return missing
}
