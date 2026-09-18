package metrics

import (
	"strings"
	"testing"
	"time"
)

// releaseBuild is a complete build record, the shape app.New publishes.
func releaseBuild() Build {
	return Build{
		Version:          "89fc016",
		GoVersion:        "go1.25.14",
		Modules:          "sha256:0b1e5b4c1a2f3d4e5f607182",
		AlgorithmVersion: "1.0.0",
		ImageDigest:      "sha256:0b1e5b4c1a2f3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6",
	}
}

// build_info is how a running process says which build it is. Its value
// carries no information by design: the facts are the labels, so a scrape
// that dropped one would still look healthy.
func TestTheBuildIsPublishedWithEveryLabelFilled(t *testing.T) {
	p := NewPrometheus()
	p.SetBuildInfo(releaseBuild())

	line := seriesLine(t, scrape(t, p), "vitalmesh_build_info{")
	if !strings.HasSuffix(line, " 1") {
		t.Errorf("build_info should always be 1, its facts are labels: %s", line)
	}
	for label, want := range map[string]string{
		"version":           "89fc016",
		"go_version":        "go1.25.14",
		"dependencies":      "sha256:0b1e5b4c1a2f3d4e5f607182",
		"algorithm_version": "1.0.0",
		"image_digest":      "sha256:0b1e5b4c1a2f3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6",
	} {
		if !strings.Contains(line, label+`="`+want+`"`) {
			t.Errorf("build_info is missing %s=%q: %s", label, want, line)
		}
	}
}

// One series per process, whatever happens. A build_info that grew a series
// would mean a label was taking a value that varies, which is the one
// mistake this metric can make.
func TestTheBuildIsExactlyOneSeries(t *testing.T) {
	p := NewPrometheus()
	for range 5 {
		p.SetBuildInfo(releaseBuild())
	}
	if got := strings.Count(scrape(t, p), "vitalmesh_build_info{"); got != 1 {
		t.Errorf("build_info is %d series after five calls, want 1", got)
	}
}

// ------------------------------------------------------ schema version

// Zero is a real schema version: it is what an unmigrated database reports.
// So "could not read it" has to be absence, not zero, or an alert on a
// stalled migration would fire against every process that lost its
// database at start-up.
func TestTheSchemaVersionIsAbsentUntilItIsRead(t *testing.T) {
	p := NewPrometheus()
	if body := scrape(t, p); strings.Contains(body, "vitalmesh_database_schema_version") {
		t.Errorf("the schema version is published before it was read:\n%s", body)
	}

	p.SetSchemaVersion(11)
	body := scrape(t, p)
	if !strings.Contains(body, "vitalmesh_database_schema_version 11") {
		t.Errorf("the schema version was not published as 11:\n%s", body)
	}
}

// A version genuinely read as zero must be published, because "nothing has
// been applied here" is a fact worth alerting on.
func TestASchemaVersionOfZeroIsPublished(t *testing.T) {
	p := NewPrometheus()
	p.SetSchemaVersion(0)
	if body := scrape(t, p); !strings.Contains(body, "vitalmesh_database_schema_version 0") {
		t.Errorf("a schema version read as zero was not published:\n%s", body)
	}
}

// Registration happens on first use, so a second call must not panic on a
// duplicate collector. A process re-reading the version after reconnecting
// would otherwise crash the gateway.
func TestTheSchemaVersionCanBeSetMoreThanOnce(t *testing.T) {
	p := NewPrometheus()
	p.SetSchemaVersion(10)
	p.SetSchemaVersion(11)
	body := scrape(t, p)
	if !strings.Contains(body, "vitalmesh_database_schema_version 11") {
		t.Errorf("the second value did not replace the first:\n%s", body)
	}
	if got := strings.Count(body, "vitalmesh_database_schema_version{"); got != 0 {
		t.Errorf("the schema version grew labels: %d", got)
	}
}

// ------------------------------------------------------------ disclosure

// The exposition goes to whatever scrapes /metrics. Build metadata is the
// newest thing on it and the one most likely to carry something it should
// not, because it is assembled from the environment.
func TestTheExpositionDisclosesNoSecretOrInfrastructureDetail(t *testing.T) {
	p := NewPrometheus()
	p.SetBuildInfo(releaseBuild())
	p.SetSchemaVersion(11)
	p.HTTPRequest("GET", "/api/v1/patients/{patient_id}", 200, 5*time.Millisecond)

	body := strings.ToLower(scrape(t, p))
	for _, forbidden := range []string{
		"password", "secret", "token", "canary",
		"postgres://", "redis://", "amazonaws.com", "bearer ",
		".svc.cluster.local", "/home/", "/root/",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the exposition mentions %q:\n%s", forbidden, body)
		}
	}
}

// seriesLine returns the one exposition line starting with prefix.
func seriesLine(t *testing.T, body, prefix string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s series, got %d:\n%s", prefix, len(found), body)
	}
	return found[0]
}
