package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/synth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/synth/synthtest"
)

var fixedNow = func() time.Time { return time.Date(2026, 3, 1, 12, 0, 30, 0, time.UTC) }

func env(pairs ...string) func(string) (string, bool) {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func exec(t *testing.T, lookup func(string) (string, bool), args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, lookup, &stdout, &stderr, fixedNow)
	return code, stdout.String(), stderr.String()
}

func TestUsageAndUnknownCommands(t *testing.T) {
	if code, _, err := exec(t, env()); code != 2 || !strings.Contains(err, "usage") {
		t.Errorf("no args: %d %q", code, err)
	}
	if code, _, err := exec(t, env(), "frobnicate"); code != 2 || !strings.Contains(err, "unknown command") {
		t.Errorf("unknown: %d %q", code, err)
	}
	if code, out, _ := exec(t, env(), "help"); code != 0 || !strings.Contains(out, "synth generate") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, out, _ := exec(t, env(), "version"); code != 0 || !strings.Contains(out, "fixture format "+synth.Version) {
		t.Errorf("version: %d %q", code, out)
	}
	if code, _, _ := exec(t, env(), "generate", "-h"); code != 0 {
		t.Errorf("-h should exit 0, got %d", code)
	}
}

func TestSpecPrintsTheDefaultsWithFlagsApplied(t *testing.T) {
	code, out, errOut := exec(t, env(), "spec", "--seed", "7", "--patients", "3", "--types", "heart_rate,spo2", "--days", "1", "--interval", "30s", "--anomaly-rate", "0")
	if code != 0 {
		t.Fatalf("spec: %d %s", code, errOut)
	}
	spec, err := synth.ParseSpec([]byte(out))
	if err != nil {
		t.Fatalf("output is not a valid spec: %v\n%s", err, out)
	}
	if spec.Seed != 7 || spec.Patients.Count != 3 || len(spec.Streams) != 2 || spec.Time.Interval.Duration != 30*time.Second {
		t.Errorf("flags not applied: %+v", spec)
	}
	if !spec.Time.To.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) || !spec.Time.From.Equal(spec.Time.To.Add(-24*time.Hour)) {
		t.Errorf("range = %s..%s, want the day before now to the minute", spec.Time.From, spec.Time.To)
	}
	for _, st := range spec.Streams {
		if st.Anomalies.Rate != 0 {
			t.Errorf("--anomaly-rate 0 not applied: %+v", st.Anomalies)
		}
	}

	if code, _, errOut := exec(t, env(), "spec", "--types", "pulse"); code != 2 || !strings.Contains(errOut, "unknown measurement type") {
		t.Errorf("bad type: %d %q", code, errOut)
	}
	if code, _, errOut := exec(t, env(), "spec", "--from", "2026-03-02T00:00:00Z"); code != 2 || !strings.Contains(errOut, "to must be after from") {
		t.Errorf("from after to: %d %q", code, errOut)
	}
	if code, _, errOut := exec(t, env(), "spec", "extra"); code != 2 || !strings.Contains(errOut, "unexpected argument") {
		t.Errorf("stray argument: %d %q", code, errOut)
	}
}

func TestGenerateWritesAFixtureFromFlagsOrASpecFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fx")
	code, stdout, errOut := exec(t, env(), "generate", "--seed", "5", "--patients", "2", "--admins", "0", "--operators", "1", "--users", "0",
		"--types", "HEART_RATE", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T00:30:00Z", "--interval", "1m", "--out", out)
	if code != 0 {
		t.Fatalf("generate: %d %s", code, errOut)
	}
	if !strings.Contains(stdout, "wrote") || !strings.Contains(stdout, "1 users, 2 patients, 60 measurements") {
		t.Errorf("summary: %q", stdout)
	}
	m, err := synth.ReadManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if m.Spec.Seed != 5 || m.Counts.Measurements != 60 {
		t.Errorf("manifest %+v", m.Counts)
	}

	if code, _, errOut := exec(t, env(), "generate", "--out", out, "--patients", "1"); code != 1 || !strings.Contains(errOut, "already holds a fixture") {
		t.Errorf("overwriting silently: %d %q", code, errOut)
	}
	if code, _, _ := exec(t, env(), "generate", "--out", out, "--patients", "1", "--types", "SPO2", "--overwrite", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T00:10:00Z"); code != 0 {
		t.Errorf("--overwrite refused: %d", code)
	}
	if code, _, errOut := exec(t, env(), "generate"); code != 2 || !strings.Contains(errOut, "--out is required") {
		t.Errorf("no --out: %d %q", code, errOut)
	}

	// A spec file is the starting point, and flags still adjust it.
	_, specJSON, _ := exec(t, env(), "spec", "--seed", "9", "--patients", "4", "--types", "SPO2", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T00:10:00Z")
	specPath := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(specPath, []byte(specJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	fromSpec := filepath.Join(t.TempDir(), "fromspec")
	if code, _, errOut := exec(t, env(), "generate", "--spec", specPath, "--patients", "1", "--out", fromSpec); code != 0 {
		t.Fatalf("generate from spec: %d %s", code, errOut)
	}
	m, _ = synth.ReadManifest(fromSpec)
	if m.Spec.Seed != 9 || m.Spec.Patients.Count != 1 || len(m.Spec.Streams) != 1 || m.Counts.Measurements != 10 {
		t.Errorf("spec file + flags: %+v %+v", m.Spec.Patients, m.Counts)
	}

	future := filepath.Join(t.TempDir(), "future")
	if _, _, errOut := exec(t, env(), "generate", "--out", future, "--patients", "1", "--types", "SPO2", "--to", "2026-03-02T00:00:00Z", "--days", "0.01"); !strings.Contains(errOut, "in the future") {
		t.Errorf("a future range should be noted: %q", errOut)
	}
}

func TestPasswordIsPrintedOnlyOnRequest(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fx")
	if code, _, errOut := exec(t, env(), "generate", "--out", out, "--patients", "1", "--types", "SPO2", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T00:05:00Z"); code != 0 {
		t.Fatal(errOut)
	}
	for _, name := range []string{synth.UsersFile, synth.ManifestFile} {
		raw, _ := os.ReadFile(filepath.Join(out, name))
		if strings.Contains(string(raw), "password") {
			t.Errorf("%s mentions a password", name)
		}
	}
	users, err := synth.ReadUsers(out)
	if err != nil || len(users) < 2 {
		t.Fatalf("users: %v %v", users, err)
	}
	operator := users[1].Email
	code, stdout, errOut := exec(t, env(), "password", "--from", out, "--email", strings.ToUpper(operator))
	if code != 0 || strings.TrimSpace(stdout) != synth.Password(1, operator) {
		t.Errorf("password: %d %q %q", code, stdout, errOut)
	}
	if code, _, errOut := exec(t, env(), "password", "--from", out, "--email", "nobody@synthetic.invalid"); code != 1 || !strings.Contains(errOut, "not an account of this fixture") {
		t.Errorf("unknown account: %d %q", code, errOut)
	}
}

func TestLoadAppliesTheSafeguardBeforeTouchingAnything(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fx")
	if code, _, errOut := exec(t, env(), "generate", "--out", out, "--patients", "1", "--types", "SPO2", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T00:05:00Z"); code != 0 {
		t.Fatal(errOut)
	}
	production := synthtest.New(t, synth.EnvProduction, "op@x.invalid", "pw-pw-pw-pw-pw")
	creds := env("SYNTH_EMAIL", "op@x.invalid", "SYNTH_PASSWORD", "pw-pw-pw-pw-pw")

	cases := []struct {
		name  string
		args  []string
		wants string
	}{
		{"no environment", []string{"--from", out, "--target", production.URL()}, "--environment is required"},
		{"claimed local, really production", []string{"--from", out, "--target", production.URL(), "--environment", "local"}, `says it is "production"`},
		{"claimed staging, really production", []string{"--from", out, "--target", production.URL(), "--environment", "staging"}, `says it is "production"`},
		{"production without the keys", []string{"--from", out, "--target", "https://api.vitalmesh.example.com", "--environment", "production"}, "--allow-production"},
		{"production with the flag only", []string{"--from", out, "--target", "https://api.vitalmesh.example.com", "--environment", "production", "--allow-production"}, synth.ProductionConfirmationEnv},
		{"missing target", []string{"--from", out, "--environment", "local"}, "--from and --target are required"},
		{"batch size", []string{"--from", out, "--target", production.URL(), "--environment", "local", "--batch-size", "5000"}, "--batch-size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(production.Requests())
			code, _, errOut := exec(t, creds, append([]string{"load"}, tc.args...)...)
			if code != 2 || !strings.Contains(errOut, tc.wants) {
				t.Fatalf("code %d, stderr %q, want 2 and %q", code, errOut, tc.wants)
			}
			for _, r := range production.Requests()[before:] {
				if r.Path != "/health" {
					t.Errorf("a refused load must make no call but /health, made %s %s", r.Method, r.Path)
				}
			}
			if production.MeasurementCount() != 0 || len(production.Patients()) != 0 {
				t.Error("a refused load wrote data")
			}
		})
	}
}

func TestLoadIntoALocalGateway(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fx")
	if code, _, errOut := exec(t, env(), "generate", "--out", out, "--patients", "2", "--types", "HEART_RATE,SPO2", "--from", "2026-01-01T00:00:00Z", "--to", "2026-01-01T00:20:00Z", "--interval", "1m"); code != 0 {
		t.Fatal(errOut)
	}
	g := synthtest.New(t, synth.EnvLocal, "op@x.invalid", "pw-pw-pw-pw-pw")

	code, stdout, errOut := exec(t, env("SYNTH_EMAIL", "op@x.invalid", "SYNTH_PASSWORD", "pw-pw-pw-pw-pw"),
		"load", "--from", out, "--target", g.URL(), "--environment", "local", "--jobs", "--batch-size", "7")
	if code != 0 {
		t.Fatalf("load: %d\n%s\n%s", code, stdout, errOut)
	}
	if !strings.Contains(stdout, "patients 2 created / 0 existing; measurements 80 stored / 0 existing") || !strings.Contains(stdout, "jobs 2 completed / 0 failed") {
		t.Errorf("summary: %s", stdout)
	}
	if strings.Contains(stdout, "pw-pw-pw-pw-pw") || strings.Contains(stdout, "Bearer") {
		t.Error("a credential reached the output")
	}
	if g.MeasurementCount() != 80 || len(g.Patients()) != 2 || g.Jobs() != 2 {
		t.Errorf("gateway: %d readings, %d patients, %d jobs", g.MeasurementCount(), len(g.Patients()), g.Jobs())
	}

	// Without credentials and without --users there is nothing to sign in
	// with, and the message says what to do.
	code, _, errOut = exec(t, env(), "load", "--from", out, "--target", g.URL(), "--environment", "local")
	if code != 2 || !strings.Contains(errOut, "SYNTH_EMAIL") || !strings.Contains(errOut, "--users") {
		t.Errorf("no credentials: %d %q", code, errOut)
	}
	code, _, errOut = exec(t, env("SYNTH_EMAIL", "x@y.invalid"), "load", "--from", out, "--target", g.URL(), "--environment", "local")
	if code != 2 || !strings.Contains(errOut, "together") {
		t.Errorf("half credentials: %d %q", code, errOut)
	}
	code, _, errOut = exec(t, env(), "load", "--from", out, "--target", g.URL(), "--environment", "local", "--users")
	if code != 2 || !strings.Contains(errOut, "DATABASE_URL") {
		t.Errorf("--users without a database: %d %q", code, errOut)
	}
	code, _, errOut = exec(t, env("SYNTH_EMAIL", "op@x.invalid", "SYNTH_PASSWORD", "nope-nope-nope"), "load", "--from", out, "--target", g.URL(), "--environment", "local")
	if code != 1 || !strings.Contains(errOut, "sign in as op@x.invalid") {
		t.Errorf("bad password: %d %q", code, errOut)
	}
}

func TestCredentialsFallBackToTheFixturesOperator(t *testing.T) {
	users := []synth.User{{Email: "synth-admin-1@synthetic.invalid", Role: "ADMIN"}, {Email: "synth-operator-1@synthetic.invalid", Role: "OPERATOR"}}
	email, password, err := credentials(env(), 3, users, true)
	if err != nil || email != "synth-operator-1@synthetic.invalid" || password != synth.Password(3, email) {
		t.Errorf("created accounts: %q %q %v", email, password, err)
	}
	if _, _, err := credentials(env(), 3, users, false); err == nil {
		t.Error("without created accounts the fallback must explain itself")
	}
	if _, _, err := credentials(env(), 3, nil, true); err == nil {
		t.Error("a fixture with no accounts has nothing to sign in with")
	}
	email, password, err = credentials(env("SYNTH_EMAIL", " Op@X.invalid ", "SYNTH_PASSWORD", "secret-secret-secret"), 3, users, true)
	if err != nil || email != "op@x.invalid" || password != "secret-secret-secret" {
		t.Errorf("explicit credentials: %q %q %v", email, password, err)
	}
	raw, _ := json.Marshal(users)
	if strings.Contains(string(raw), "password") {
		t.Error("users serialise with a password field")
	}
}
