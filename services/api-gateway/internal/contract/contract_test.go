package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schema decodes a schema written inline, the way the contract writes them.
func schema(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return out
}

func value(t *testing.T, raw string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("value: %v", err)
	}
	return out
}

func emptyDoc() *Document {
	return &Document{Path: "memory", Root: map[string]any{}}
}

// The validator's whole purpose is to reject. These cases prove it does,
// because a checker that accepts everything would make the contract gate
// worthless while looking green.
func TestValidatorRejectsWhatItShould(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		value  string
		want   string
	}{
		{
			name:   "missing required property",
			schema: `{"type":"object","required":["code"],"properties":{"code":{"type":"string"}}}`,
			value:  `{}`,
			want:   "required property \"code\" is missing",
		},
		{
			name:   "undeclared property when additionalProperties is false",
			schema: `{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}`,
			value:  `{"a":"x","b":1}`,
			want:   "not declared",
		},
		{
			name:   "wrong scalar type",
			schema: `{"type":"string"}`,
			value:  `7`,
			want:   "expected a string",
		},
		{
			name:   "number where an object belongs",
			schema: `{"type":"object","properties":{}}`,
			value:  `3`,
			want:   "expected an object",
		},
		{
			name:   "value outside an enum",
			schema: `{"type":"string","enum":["INFO","WARNING"]}`,
			value:  `"FATAL"`,
			want:   "is not one of the enumerated values",
		},
		{
			name:   "string shorter than minLength",
			schema: `{"type":"string","minLength":2}`,
			value:  `"a"`,
			want:   "shorter than the minimum",
		},
		{
			name:   "string longer than maxLength",
			schema: `{"type":"string","maxLength":2}`,
			value:  `"abc"`,
			want:   "longer than the maximum",
		},
		{
			name:   "string failing a pattern",
			schema: `{"type":"string","pattern":"^[A-Z]+$"}`,
			value:  `"abc"`,
			want:   "does not match",
		},
		{
			name:   "malformed timestamp",
			schema: `{"type":"string","format":"date-time"}`,
			value:  `"yesterday"`,
			want:   "is not an RFC 3339 instant",
		},
		{
			name:   "number below minimum",
			schema: `{"type":"number","minimum":0}`,
			value:  `-1`,
			want:   "below the minimum",
		},
		{
			name:   "number above maximum",
			schema: `{"type":"integer","maximum":99}`,
			value:  `100`,
			want:   "above the maximum",
		},
		{
			name:   "fraction where an integer belongs",
			schema: `{"type":"integer"}`,
			value:  `1.5`,
			want:   "is not an integer",
		},
		{
			name:   "array shorter than minItems",
			schema: `{"type":"array","minItems":1,"items":{"type":"string"}}`,
			value:  `[]`,
			want:   "fewer than the minimum",
		},
		{
			name:   "array longer than maxItems",
			schema: `{"type":"array","maxItems":1,"items":{"type":"string"}}`,
			value:  `["a","b"]`,
			want:   "more than the maximum",
		},
		{
			name:   "repeated item in a uniqueItems array",
			schema: `{"type":"array","uniqueItems":true,"items":{"type":"string"}}`,
			value:  `["a","a"]`,
			want:   "duplicate item",
		},
		{
			name:   "bad item inside an array",
			schema: `{"type":"array","items":{"type":"integer"}}`,
			value:  `[1,"two"]`,
			want:   "[1]: expected a number",
		},
		{
			name:   "null where null is not allowed",
			schema: `{"type":"string"}`,
			value:  `null`,
			want:   "null is not allowed",
		},
		{
			name:   "nested property failing deep in an object",
			schema: `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"integer"}}}}}`,
			value:  `{"a":{"b":"no"}}`,
			want:   "a.b: expected a number",
		},
		{
			name:   "value failing an allOf branch",
			schema: `{"allOf":[{"type":"string","minLength":3}]}`,
			value:  `"ab"`,
			want:   "shorter than the minimum",
		},
		{
			name:   "value matching no oneOf branch",
			schema: `{"oneOf":[{"type":"integer"},{"type":"boolean"}]}`,
			value:  `"x"`,
			want:   "matches no oneOf branch",
		},
		{
			name:   "additionalProperties as a schema is enforced",
			schema: `{"type":"object","additionalProperties":{"type":"number"}}`,
			value:  `{"50":"seventy"}`,
			want:   "expected a number",
		},
	}

	doc := emptyDoc()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := doc.ValidateExample(schema(t, tc.schema), value(t, tc.value), "x")
			if err == nil {
				t.Fatalf("accepted an invalid value; want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidatorAcceptsWhatItShould(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		value  string
	}{
		{"nullable field", `{"type":"string","nullable":true}`, `null`},
		{"nullable through allOf", `{"allOf":[{"type":"string","format":"date-time"}],"nullable":true}`, `null`},
		{"timestamp with an offset", `{"type":"string","format":"date-time"}`, `"2026-09-06T14:30:00+02:00"`},
		{"timestamp with a fraction", `{"type":"string","format":"date-time"}`, `"2026-09-06T12:00:00.184Z"`},
		{"integer at its bounds", `{"type":"integer","minimum":1,"maximum":99}`, `99`},
		{"zero where zero is the minimum", `{"type":"number","minimum":0}`, `0`},
		{"optional property absent", `{"type":"object","required":["a"],"properties":{"a":{"type":"string"},"b":{"type":"string"}}}`, `{"a":"x"}`},
		{"free-form object", `{"type":"object","additionalProperties":true}`, `{"anything":[1,2]}`},
		{"map of numbers", `{"type":"object","additionalProperties":{"type":"number"}}`, `{"50":74,"95":191.5}`},
	}

	doc := emptyDoc()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := doc.ValidateExample(schema(t, tc.schema), value(t, tc.value), "x"); err != nil {
				t.Fatalf("rejected a valid value: %v", err)
			}
		})
	}
}

// An unknown keyword must stop the check rather than be skipped, so the
// checker can never report success over a constraint it did not apply.
func TestValidatorRefusesKeywordsItDoesNotImplement(t *testing.T) {
	doc := emptyDoc()
	err := doc.ValidateExample(schema(t, `{"type":"string","multipleOf":2}`), value(t, `"x"`), "x")
	if err == nil {
		t.Fatal("accepted a schema with an unimplemented keyword")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Fatalf("error = %q, want it to name the unimplemented keyword", err)
	}
}

func TestValidatorFollowsReferences(t *testing.T) {
	doc := &Document{
		Path: "memory",
		Root: schema(t, `{"components":{"schemas":{
			"Id":{"type":"string","pattern":"^[a-z]+$"},
			"Wrapper":{"type":"object","properties":{"id":{"$ref":"#/components/schemas/Id"}}}
		}}}`),
	}
	target := schema(t, `{"$ref":"#/components/schemas/Wrapper"}`)
	if err := doc.ValidateExample(target, value(t, `{"id":"abc"}`), "x"); err != nil {
		t.Fatalf("rejected a valid value: %v", err)
	}
	err := doc.ValidateExample(target, value(t, `{"id":"ABC"}`), "x")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want the referenced pattern to be enforced", err)
	}
}

func TestResolveRejectsBrokenAndForeignReferences(t *testing.T) {
	doc := &Document{Path: "memory", Root: schema(t, `{"components":{"schemas":{"A":{"type":"string"}}}}`)}

	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{"missing target", "#/components/schemas/Missing", "does not exist"},
		{"remote document", "other.json#/X", "only local references"},
		{"through a non-object", "#/components/schemas/A/type/nope", "not inside an object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := doc.Resolve(map[string]any{"$ref": tc.ref})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCanonicalizationIsStableAndPreservesNumbers(t *testing.T) {
	first := value(t, `{"b":1,"a":{"d":2.50,"c":[1,2]}}`)
	second := value(t, `{"a":{"c":[1,2],"d":2.50},"b":1}`)

	left, err := canonicalize(first)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	right, err := canonicalize(second)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if left != right {
		t.Errorf("key order changed the canonical form:\n%s\n%s", left, right)
	}
	// A number keeps the spelling it had in the file, so a fingerprint is
	// reproducible without depending on float formatting.
	if !strings.Contains(left, "2.50") {
		t.Errorf("canonical form = %s, want the literal number spelling preserved", left)
	}
}

func TestLockRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	want := &Lock{Contract: "c", Version: "1.0.0", Fingerprint: "sha256:abc", Note: "n"}
	if err := want.Write(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadLock(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if *got != *want {
		t.Errorf("round trip changed the lock: %+v", got)
	}
}

func TestLoadReportsBadDocuments(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("loading a missing file succeeded")
	}
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte(`{"openapi": }`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("loading malformed JSON succeeded")
	}

	trailing := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"openapi":"3.0.3"} {}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(trailing); err == nil {
		t.Error("loading a file with trailing content succeeded")
	}
}
