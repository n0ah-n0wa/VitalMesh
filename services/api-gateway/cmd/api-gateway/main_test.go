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
