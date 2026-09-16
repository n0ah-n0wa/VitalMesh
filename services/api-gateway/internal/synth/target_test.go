package synth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/synth/synthtest"
)

func TestLooksLocal(t *testing.T) {
	local := []string{"localhost", "LOCALHOST", "127.0.0.1", "::1", "api-gateway", "postgres", "gateway.local", "svc.cluster.internal", "host.docker.internal", "dev.localhost", "127.0.0.1."}
	remote := []string{"api.vitalmesh-staging.example.com", "10.0.0.5", "192.168.1.20", "api.example.com", "vitalmesh.io", "", "2001:db8::1"}
	for _, h := range local {
		if !LooksLocal(h) {
			t.Errorf("%q should look local", h)
		}
	}
	for _, h := range remote {
		if LooksLocal(h) {
			t.Errorf("%q should not look local", h)
		}
	}
}

func TestTheSafeguardRefusesWhatItShould(t *testing.T) {
	staging := synthtest.New(t, EnvStaging, "e@x.invalid", "pw-pw-pw-pw-pw")
	production := synthtest.New(t, EnvProduction, "e@x.invalid", "pw-pw-pw-pw-pw")
	local := synthtest.New(t, EnvLocal, "e@x.invalid", "pw-pw-pw-pw-pw")
	older := synthtest.New(t, "", "e@x.invalid", "pw-pw-pw-pw-pw")
	testEnv := synthtest.New(t, "test", "e@x.invalid", "pw-pw-pw-pw-pw")
	// A remote-looking name that resolves to the fake: the test servers
	// listen on 127.0.0.1, so the URL's host is rewritten and the request
	// is steered back by a custom transport.
	remoteOf := func(g *synthtest.Gateway) string {
		return "http://api.vitalmesh.example.com" + strings.TrimPrefix(g.URL(), "http://127.0.0.1")
	}
	steer := func(g *synthtest.Gateway) *http.Client {
		return &http.Client{Transport: rewriteHost{to: strings.TrimPrefix(g.URL(), "http://")}}
	}

	cases := []struct {
		name   string
		target Target
		client *http.Client
		wants  string // "" means accepted
	}{
		{"local target, local claim", Target{URL: local.URL(), Environment: EnvLocal}, nil, ""},
		{"test gateway passes for a local claim", Target{URL: testEnv.URL(), Environment: EnvLocal}, nil, ""},
		{"older gateway, local claim", Target{URL: older.URL(), Environment: EnvLocal}, nil, ""},
		{"staging through a port-forward", Target{URL: staging.URL(), Environment: EnvStaging}, nil, ""},
		{"no environment", Target{URL: local.URL()}, nil, "--environment is required"},
		{"unknown environment", Target{URL: local.URL(), Environment: "qa"}, nil, "not one of"},
		{"local claim for a remote address", Target{URL: "http://api.vitalmesh.example.com", Environment: EnvLocal}, nil, "not a local address"},
		{"staging over plain http remotely", Target{URL: "http://api.vitalmesh.example.com", Environment: EnvStaging}, nil, "plain http"},
		{"staging gateway that is really production", Target{URL: production.URL(), Environment: EnvStaging}, nil, `says it is "production"`},
		{"local gateway claimed as staging", Target{URL: local.URL(), Environment: EnvStaging}, nil, `says it is "local"`},
		{"production without the flag", Target{URL: "https://api.vitalmesh.example.com", Environment: EnvProduction}, nil, "pass --allow-production"},
		{"production with the flag but no variable", Target{URL: "https://api.vitalmesh.example.com", Environment: EnvProduction, AllowProduction: true}, nil, "set to exactly the target host"},
		{"production with the wrong host in the variable", Target{URL: "https://api.vitalmesh.example.com", Environment: EnvProduction, AllowProduction: true, ProductionConfirmation: "api.vitalmesh-staging.example.com"}, nil, "set to exactly the target host"},
		{"production over http", Target{URL: "http://api.vitalmesh.example.com", Environment: EnvProduction, AllowProduction: true, ProductionConfirmation: "api.vitalmesh.example.com"}, nil, "must be https"},
		{"production with both keys", Target{URL: remoteOf(production), Environment: EnvProduction, AllowProduction: true, ProductionConfirmation: "API.vitalmesh.example.com "}, steer(production), "must be https"},
		{"bad URL", Target{URL: "localhost:8080", Environment: EnvLocal}, nil, "must be an http or https URL"},
		{"URL with a query", Target{URL: "http://localhost:8080/?x=1", Environment: EnvLocal}, nil, "query"},
		{"nothing listening", Target{URL: "http://127.0.0.1:1", Environment: EnvLocal}, nil, "not answering"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.client
			if client == nil {
				client = http.DefaultClient
			}
			_, err := CheckTarget(context.Background(), client, tc.target)
			if tc.wants == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wants)
			}
			if tc.wants != "not answering" && !errors.Is(err, ErrRefused) {
				t.Errorf("a refusal must be ErrRefused, got %v", err)
			}
		})
	}
}

func TestTheSafeguardAcceptsProductionOnlyWithBothKeysOverTLS(t *testing.T) {
	production := synthtest.New(t, EnvProduction, "e@x.invalid", "pw-pw-pw-pw-pw")
	tls := httptest.NewTLSServer(production.Server.Config.Handler)
	t.Cleanup(tls.Close)
	client := tls.Client()
	client.Transport = rewriteHost{to: strings.TrimPrefix(tls.URL, "https://"), base: client.Transport}
	target := Target{URL: "https://api.vitalmesh.example.com", Environment: EnvProduction, AllowProduction: true, ProductionConfirmation: "api.vitalmesh.example.com"}

	h, err := CheckTarget(context.Background(), client, target)
	if err != nil {
		t.Fatalf("production with both keys over https must be accepted: %v", err)
	}
	if h.Environment != EnvProduction {
		t.Errorf("health = %+v", h)
	}
	target.ProductionConfirmation = ""
	if _, err := CheckTarget(context.Background(), client, target); !errors.Is(err, ErrRefused) {
		t.Errorf("without the variable: %v", err)
	}
}

// rewriteHost sends every request to `to` whatever host the URL names, so
// a test can use a remote-looking address against a local fake. The TLS
// server name is left as the fake's, which its certificate covers.
type rewriteHost struct {
	to   string
	base http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Host = r.to
	clone.Host = r.to
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
