//go:build integration

package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/app"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

// The real gateway, in process, over its real database: the accounts are
// created the way `--users` creates them, and the fixture is loaded through
// the API exactly as `synth load` does it.
func TestAFixtureLoadsIntoTheRealGateway(t *testing.T) {
	_, dbURL, _ := postgrestest.New(t)
	env := map[string]string{
		"DATABASE_URL":             dbURL,
		"JWT_SECRET":               "synth-secret-synth-secret-synth-secret",
		"PASSWORD_HASH_MEMORY_KIB": "8192",
		"PASSWORD_HASH_TIME":       "1",
		"ENVIRONMENT":              "test",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err := config.Load(lookup)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	gateway, err := app.New(context.Background(), cfg, logging.New(io.Discard, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "synth-test"}), "v-test")
	if err != nil {
		t.Fatalf("wire the gateway: %v", err)
	}
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	// The readings must be inside the gateway's future-skew window, so the
	// range ends now; the spec is otherwise the small test one.
	to := time.Now().UTC().Truncate(time.Minute)
	spec := small(99)
	spec.Time.From, spec.Time.To = to.Add(-time.Hour), to
	dir := t.TempDir()
	m, err := Write(spec, dir, false)
	if err != nil {
		t.Fatal(err)
	}

	users, err := ReadUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	created, existing, err := CreateUsers(ctx, dbURL, spec.Seed, users, lookup, log)
	if err != nil {
		t.Fatalf("CreateUsers: %v", err)
	}
	if created != 3 || existing != 0 {
		t.Errorf("created %d, existing %d", created, existing)
	}
	created, existing, err = CreateUsers(ctx, dbURL, spec.Seed, users, lookup, log)
	if err != nil || created != 0 || existing != 3 {
		t.Errorf("a second CreateUsers must leave the accounts alone: %d, %d, %v", created, existing, err)
	}
	if bytes.Contains(log.Bytes(), []byte("synth-")) && bytes.Contains(log.Bytes(), []byte(Password(spec.Seed, users[0].Email))) {
		t.Error("a password reached the log")
	}

	health, err := CheckTarget(ctx, server.Client(), Target{URL: server.URL, Environment: EnvLocal})
	if err != nil {
		t.Fatalf("CheckTarget: %v", err)
	}
	if health.Environment != "test" {
		t.Errorf("the real gateway reports environment %q", health.Environment)
	}

	var operator User
	for _, u := range users {
		if u.Role == domain.RoleOperator {
			operator = u
		}
	}
	l := &Loader{Client: server.Client(), BaseURL: server.URL, BatchSize: 7, Log: io.Discard, Sleep: func(time.Duration) {}}
	if err := l.Login(ctx, operator.Email, Password(spec.Seed, operator.Email)); err != nil {
		t.Fatalf("Login with the derived password: %v", err)
	}
	r, err := l.Load(ctx, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Patients.Created != 2 || r.Measurements.Stored != m.Counts.Measurements || r.Measurements.Existing != 0 {
		t.Errorf("first load: %+v", r)
	}
	again, err := l.Load(ctx, dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if again.Patients.Created != 0 || again.Measurements.Stored != 0 || again.Measurements.Existing != m.Counts.Measurements {
		t.Errorf("second load stored something: %+v", again)
	}

	// What the gateway holds is what the fixture says, reading by reading.
	patients, _ := ReadPatients(dir)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/patients?limit=10", nil)
	req.Header.Set("Authorization", "Bearer "+l.token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Items []struct {
			ID                string `json:"id"`
			ExternalReference string `json:"external_reference"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ExternalReference != patients[0].ExternalReference {
		t.Fatalf("patients in the gateway: %+v", page.Items)
	}
	perPatient := map[string]int{}
	_ = EachMeasurement(dir, func(x Measurement) error { perPatient[x.PatientReference]++; return nil })
	for _, p := range page.Items {
		count := 0
		cursor := ""
		for {
			url := server.URL + "/api/v1/patients/" + p.ID + "/measurements?limit=200"
			if cursor != "" {
				url += "&cursor=" + cursor
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+l.token)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var mp struct {
				Items      []json.RawMessage `json:"items"`
				NextCursor *string           `json:"next_cursor"`
			}
			err = json.NewDecoder(resp.Body).Decode(&mp)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			count += len(mp.Items)
			if mp.NextCursor == nil {
				break
			}
			cursor = *mp.NextCursor
		}
		if count != perPatient[p.ExternalReference] {
			t.Errorf("patient %s holds %d readings, fixture has %d", p.ExternalReference, count, perPatient[p.ExternalReference])
		}
	}
}
