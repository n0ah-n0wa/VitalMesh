package main

import (
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
)

func TestReadPassword(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"plain":            {"hunter2-hunter2", "hunter2-hunter2"},
		"trailing newline": {"hunter2-hunter2\n", "hunter2-hunter2"},
		"crlf":             {"hunter2-hunter2\r\n", "hunter2-hunter2"},
		"inner spaces":     {"pass phrase here\n", "pass phrase here"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := readPassword(strings.NewReader(tc.in))
			if err != nil || got != tc.want {
				t.Errorf("readPassword = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := readPassword(strings.NewReader("\n")); err == nil {
		t.Error("empty password accepted")
	}
	if got, _ := readPassword(strings.NewReader(strings.Repeat("x", auth.MaxPasswordLength*2))); len(got) > auth.MaxPasswordLength+2 {
		t.Errorf("read %d bytes, want the input bounded", len(got))
	}
}

func TestUsageErrorsExitWithTwo(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{
		{"bogus"},
		{"users"},
		{"users", "create"},
		{"users", "create", "a@b.c"},
		{"users", "create", "a@b.c", "ROOT"},
		{"users", "delete", "a@b.c", "USER"},
		{"users", "create", "a@b.c", "USER"}, // valid shape, but no DATABASE_URL
		{"migrate"},
		{"migrate", "force"},
	} {
		if code := run(args, strings.NewReader("")); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}

// A migration run that prints nothing leaves no record of what it did. The
// one-shot migration container is the only place this text is read, so the
// wording is pinned here rather than left to be noticed as missing.
func TestSchemaMoveMessage(t *testing.T) {
	cases := map[string]struct {
		before, after uint
		want          string
	}{
		"fresh database":  {0, 7, "migrate: schema applied, version 0 to 7"},
		"already current": {7, 7, "migrate: schema already at version 7, nothing to apply"},
		"rolled back":     {7, 6, "migrate: schema rolled back, version 7 to 6"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := schemaMoveMessage(tc.before, tc.after); got != tc.want {
				t.Errorf("schemaMoveMessage(%d, %d) = %q, want %q", tc.before, tc.after, got, tc.want)
			}
		})
	}
}
