package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The safeguard against loading synthetic data into production.
//
// Loading is refused unless three things agree: what the operator says the
// target is (--environment), what the address looks like, and what the
// gateway itself answers on /health. Production needs, on top of that, an
// explicit flag and an environment variable naming the exact host, so that
// no single mistake (a wrong URL in a shell history, a copied command, a
// misread kubeconfig) can seed a production database.

// Environment names of a target, as the operator states them and as the
// gateway reports itself.
const (
	EnvLocal      = "local"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// ProductionConfirmationEnv is the environment variable that, set to the
// exact host of the target, is the second key of a production load.
const ProductionConfirmationEnv = "VITALMESH_SYNTH_ALLOW_PRODUCTION"

// Target is a gateway to load into and the operator's claims about it.
type Target struct {
	// URL is the gateway's base URL, such as http://localhost:8080.
	URL string
	// Environment is what the operator says the target is: local, staging
	// or production.
	Environment string
	// AllowProduction is the --allow-production flag.
	AllowProduction bool
	// ProductionConfirmation is the value of ProductionConfirmationEnv.
	ProductionConfirmation string
}

// Health is what the gateway answers on /health.
type Health struct {
	Status      string `json:"status"`
	Service     string `json:"service"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
}

// ErrRefused marks a target the safeguard rejected. Its message says why
// and what would be needed; it is never a transient error.
var ErrRefused = errors.New("target refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// CheckTarget applies the safeguard and returns the gateway's health
// answer. It performs one unauthenticated GET /health and nothing else.
func CheckTarget(ctx context.Context, client *http.Client, t Target) (Health, error) {
	base, err := parseBase(t.URL)
	if err != nil {
		return Health{}, err
	}
	host := strings.ToLower(base.Hostname())
	local := LooksLocal(host)

	switch t.Environment {
	case EnvLocal:
		if !local {
			return Health{}, refuse("--environment local, but %s is not a local address; a deployed environment must be named explicitly (--environment staging)", host)
		}
	case EnvStaging:
		if base.Scheme != "https" && !local {
			return Health{}, refuse("%s is a remote address over plain http; credentials would travel in the clear. Use https, or a port-forward to a local address", host)
		}
	case EnvProduction:
		if !t.AllowProduction {
			return Health{}, refuse("loading synthetic data into production is refused. If that is really intended, pass --allow-production and set %s=%s", ProductionConfirmationEnv, host)
		}
		if strings.ToLower(strings.TrimSpace(t.ProductionConfirmation)) != host {
			return Health{}, refuse("--allow-production also needs %s set to exactly the target host (%s)", ProductionConfirmationEnv, host)
		}
		if base.Scheme != "https" {
			return Health{}, refuse("a production target must be https")
		}
	case "":
		return Health{}, refuse("--environment is required: local, staging or production")
	default:
		return Health{}, refuse("--environment %q is not one of local, staging, production", t.Environment)
	}

	h, err := fetchHealth(ctx, client, base)
	if err != nil {
		return Health{}, err
	}
	if h.Status != "ok" {
		return h, refuse("%s answered /health with status %q", host, h.Status)
	}
	switch reported := h.Environment; {
	case reported == "":
		// An older gateway that does not say what it is. The address and
		// the operator's claim have already been checked; production has
		// needed both keys.
	case reported == t.Environment:
	case t.Environment == EnvLocal && reported == "test":
	default:
		return h, refuse("the gateway at %s says it is %q, not %q (--environment): loading nothing", host, reported, t.Environment)
	}
	return h, nil
}

// LooksLocal reports whether a host name can only be a machine at hand: a
// loopback address, localhost, a name under .localhost, .local or
// .internal, Docker's host alias, or a bare single-label name such as a
// Compose service.
func LooksLocal(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return !strings.Contains(host, ".")
}

func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, refuse("--target %q: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, refuse("--target %q must be an http or https URL with a host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, refuse("--target %q must not carry a query or fragment", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func fetchHealth(ctx context.Context, client *http.Client, base *url.URL) (Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/health", nil)
	if err != nil {
		return Health{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Health{}, fmt.Errorf("the gateway at %s is not answering: %w", base.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Health{}, fmt.Errorf("read /health: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("%s answered /health with HTTP %d", base.Host, resp.StatusCode)
	}
	var h Health
	if err := json.Unmarshal(body, &h); err != nil {
		return Health{}, fmt.Errorf("%s did not answer /health with the gateway's JSON: %w", base.Host, err)
	}
	return h, nil
}
