package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	cases := map[string]bool{
		"abc":                           true,
		"req-123_x.y":                   true,
		strings.Repeat("a", 128):        true,
		"":                              false,
		strings.Repeat("a", 129):        false,
		"has space":                     false,
		"semi;colon":                    false,
		"new\nline":                     false,
		"unicodé":                       false,
		"{\"json\":1}":                  false,
		"<script>alert(1)</script>":     false,
		"../../etc/passwd":              false,
		"ok-but-then\x00null":           false,
		"trailing-newline-is-invalid\n": false,
	}
	for id, want := range cases {
		if got := Valid(id); got != want {
			t.Errorf("Valid(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestGenerateProducesValidUniqueIDs(t *testing.T) {
	seen := make(map[string]struct{})
	for range 1000 {
		id := Generate()
		if !Valid(id) {
			t.Fatalf("Generate() = %q is not Valid", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("Generate() repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Errorf("FromContext(empty) = %q, want \"\"", got)
	}
	ctx := NewContext(context.Background(), "abc")
	if got := FromContext(ctx); got != "abc" {
		t.Errorf("FromContext = %q, want abc", got)
	}
}
