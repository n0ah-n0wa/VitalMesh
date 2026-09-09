package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

// healthCheckTimeout bounds the probe. It is short because a probe that
// takes longer than this has already told the orchestrator what it needs to
// know.
const healthCheckTimeout = 2 * time.Second

// healthcheck asks this process's own HTTP server whether it is alive and
// reports the answer as an exit code.
//
// It exists because the runtime image contains no shell, no curl and no
// wget: a container built on a distroless base has nothing to run a health
// check with except the binary already in it. Adding a shell to the image
// so that Docker can run `curl` would enlarge the attack surface far more
// than this costs.
//
// It probes liveness (/health), not readiness. Readiness depends on
// PostgreSQL, and a container that is up but waiting for its database is
// not one Docker should restart; Kubernetes distinguishes the two with
// separate probes against /health and /ready.
func healthcheck() int {
	// The address comes from the same configuration the server listens on,
	// so the two cannot disagree about the port.
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = config.DefaultHTTPAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP_ADDR is not an address\n")
		return 2
	}
	// A server listening on every interface is reached over the loopback,
	// which is the only interface a probe inside the container can rely on.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	url := "http://" + net.JoinHostPort(host, port) + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// The message names the failure, never the address: the address is
		// this container's own and saying it adds nothing.
		fmt.Fprintln(os.Stderr, "healthcheck: the server is not answering")
		return 1
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: the server answered %d\n", res.StatusCode)
		return 1
	}
	return 0
}
