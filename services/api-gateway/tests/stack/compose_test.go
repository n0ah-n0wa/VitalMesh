//go:build stack

package stack

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// compose drives the stack's docker compose project for the failure
// cases: stop, start, pause and unpause one service, and inspect a
// container. It never runs `up` or `down`; the script that starts the stack
// owns its life cycle, and every test that takes a service away puts it
// back before it returns.
type compose struct {
	file    string
	project string
}

func newCompose(t *testing.T) *compose {
	t.Helper()
	file := env("E2E_COMPOSE_FILE", filepath.Join("..", "..", "..", "..", "docker-compose.yml"))
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("compose file %s (E2E_COMPOSE_FILE): %v", file, err)
	}
	c := &compose{file: file, project: env("E2E_COMPOSE_PROJECT", "vitalmesh")}
	if _, err := c.run("version", "--short"); err != nil {
		t.Fatalf("docker compose is needed to take services away: %v", err)
	}
	return c
}

func (c *compose) run(args ...string) (string, error) {
	full := append([]string{"compose", "-f", c.file, "-p", c.project}, args...)
	cmd := exec.Command("docker", full...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", strings.Join(full, " "), err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (c *compose) must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := c.run(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *compose) stop(t *testing.T, service string)    { c.must(t, "stop", "-t", "5", service) }
func (c *compose) start(t *testing.T, service string)   { c.must(t, "start", service) }
func (c *compose) pause(t *testing.T, service string)   { c.must(t, "pause", service) }
func (c *compose) unpause(t *testing.T, service string) { c.must(t, "unpause", service) }
func (c *compose) restart(t *testing.T, service string) { c.must(t, "restart", "-t", "10", service) }

// kill sends SIGKILL: no drain, no shutdown sequence, the way a crash, an
// OOM kill or a lost node ends a process.
func (c *compose) kill(t *testing.T, service string) { c.must(t, "kill", service) }

// exec runs a command inside a service's container and returns its stdout.
func (c *compose) exec(t *testing.T, service string, args ...string) string {
	t.Helper()
	return c.must(t, append([]string{"exec", "-T", service}, args...)...)
}

// containerID is the id of a service's container.
func (c *compose) containerID(t *testing.T, service string) string {
	t.Helper()
	id := c.must(t, "ps", "-a", "-q", service)
	if id == "" {
		t.Fatalf("no container for service %s in project %s", service, c.project)
	}
	return strings.Fields(id)[0]
}

// containerState is what docker knows about a container: whether it is
// running, how often it was restarted, and when it last started.
type containerState struct {
	Running      bool
	Paused       bool
	RestartCount int
	StartedAt    time.Time
	Health       string
}

func (c *compose) state(t *testing.T, service string) containerState {
	t.Helper()
	id := c.containerID(t, service)
	cmd := exec.Command("docker", "inspect", "--format",
		"{{.State.Running}} {{.State.Paused}} {{.RestartCount}} {{.State.StartedAt}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", id)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", id, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 5 {
		t.Fatalf("docker inspect %s: unexpected output %q", id, out)
	}
	started, err := time.Parse(time.RFC3339Nano, fields[3])
	if err != nil {
		t.Fatalf("parse StartedAt %q: %v", fields[3], err)
	}
	var st containerState
	st.Running, st.Paused = fields[0] == "true", fields[1] == "true"
	fmt.Sscan(fields[2], &st.RestartCount)
	st.StartedAt, st.Health = started, fields[4]
	return st
}

// restore brings a service back to running and unpaused, whatever state a
// failed test left it in. It is registered as a cleanup by every failure
// case, so the stack is usable after the suite even when a case fails.
func (c *compose) restore(t *testing.T, service string) {
	t.Helper()
	st := c.state(t, service)
	if st.Paused {
		c.unpause(t, service)
	}
	if !st.Running {
		c.start(t, service)
	}
}
