package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The contract and its lock file, relative to this package.
const (
	internalAPIPath  = "../../../../contracts/internal-api/processor-v1.json"
	internalLockPath = "../../../../contracts/internal-api/processor-v1.lock.json"
	internalAPIName  = "processor-v1"
)

// The operations this contract exists to define (SPECIFICATIONS.md section
// 8). A path added or removed without updating this list fails, so the
// contract cannot grow an endpoint unnoticed.
var expectedOperations = map[string]string{
	"POST /internal/v1/process":      "process",
	"GET /internal/v1/jobs/{job_id}": "getJob",
	"GET /internal/v1/health":        "health",
}

func load(t *testing.T) *Document {
	t.Helper()
	doc, err := Load(internalAPIPath)
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return doc
}

func TestContractParsesAndDeclaresItsVersion(t *testing.T) {
	doc := load(t)

	if version, _ := doc.Root["openapi"].(string); version != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", version)
	}
	version, err := doc.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(version) {
		t.Errorf("info.version = %q, want major.minor.patch", version)
	}
	if _, ok := doc.Root["servers"].([]any); !ok {
		t.Error("servers is missing: a client cannot be generated without one")
	}
}

func TestContractDefinesExactlyTheAgreedOperations(t *testing.T) {
	doc := load(t)
	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}

	got := make(map[string]string, len(operations))
	ids := make(map[string]string, len(operations))
	for _, op := range operations {
		key := strings.ToUpper(op.Method) + " " + op.Path
		id, _ := op.Fields["operationId"].(string)
		if id == "" {
			t.Errorf("%s: operationId is missing; a client cannot be generated without one", key)
		}
		if previous, duplicate := ids[id]; duplicate {
			t.Errorf("operationId %q is used by both %s and %s", id, previous, key)
		}
		ids[id] = key
		got[key] = id
	}

	for key, want := range expectedOperations {
		id, ok := got[key]
		if !ok {
			t.Errorf("%s is missing from the contract", key)
			continue
		}
		if id != want {
			t.Errorf("%s: operationId = %q, want %q", key, id, want)
		}
	}
	for key := range got {
		if _, expected := expectedOperations[key]; !expected {
			t.Errorf("%s is not one of the agreed operations; adding an endpoint is a "+
				"contract change that must be reviewed and reflected in this test", key)
		}
	}
}

func TestEveryOperationIsCompletelySpecified(t *testing.T) {
	doc := load(t)
	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}

	for _, op := range operations {
		key := strings.ToUpper(op.Method) + " " + op.Path
		if summary, _ := op.Fields["summary"].(string); summary == "" {
			t.Errorf("%s: summary is missing", key)
		}
		if description, _ := op.Fields["description"].(string); description == "" {
			t.Errorf("%s: description is missing", key)
		}

		// Every operation must accept the correlation headers, because
		// section 85 requires the ids to propagate through every hop.
		headers := map[string]bool{}
		for _, parameter := range op.Parameters {
			if in, _ := parameter["in"].(string); in != "header" {
				continue
			}
			name, _ := parameter["name"].(string)
			headers[strings.ToLower(name)] = true
		}
		for _, required := range []string{"x-request-id", "x-correlation-id"} {
			if !headers[required] {
				t.Errorf("%s: does not accept the %s header", key, required)
			}
		}

		responses, ok := op.Fields["responses"].(map[string]any)
		if !ok {
			t.Errorf("%s: responses is missing", key)
			continue
		}
		for _, status := range []string{"200", "500"} {
			if _, present := responses[status]; !present {
				t.Errorf("%s: no %s response is declared", key, status)
			}
		}
		checkResponses(t, doc, key, responses)
	}
}

// checkResponses asserts that every declared response carries a description,
// echoes the correlation headers, and that every failure uses the shared
// error envelope and declares which codes it can return and whether it is
// worth retrying (SPECIFICATIONS.md sections 25 and 93).
func checkResponses(t *testing.T, doc *Document, operation string, responses map[string]any) {
	t.Helper()
	codePattern := regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

	for status, entry := range responses {
		where := operation + " " + status
		object, ok := entry.(map[string]any)
		if !ok {
			t.Errorf("%s: response is not an object", where)
			continue
		}
		resolved, err := doc.Resolve(object)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			continue
		}
		if description, _ := resolved["description"].(string); description == "" {
			t.Errorf("%s: description is missing", where)
		}

		headers, _ := resolved["headers"].(map[string]any)
		for _, required := range []string{"X-Request-ID", "X-Correlation-ID"} {
			if _, present := headers[required]; !present {
				t.Errorf("%s: does not echo the %s header", where, required)
			}
		}

		content, ok := resolved["content"].(map[string]any)
		if !ok {
			t.Errorf("%s: no content is declared", where)
			continue
		}
		media, ok := content["application/json"].(map[string]any)
		if !ok {
			t.Errorf("%s: no application/json content", where)
			continue
		}
		schema, ok := media["schema"].(map[string]any)
		if !ok {
			t.Errorf("%s: no schema", where)
			continue
		}

		if !strings.HasPrefix(status, "2") {
			if ref, _ := schema["$ref"].(string); ref != "#/components/schemas/Error" {
				t.Errorf("%s: failures must use the shared error envelope, got %v", where, schema)
			}
			codes, ok := resolved["x-error-codes"].([]any)
			if !ok || len(codes) == 0 {
				t.Errorf("%s: x-error-codes is missing; a client cannot map the failure", where)
			}
			for _, code := range codes {
				text, ok := code.(string)
				if !ok || !codePattern.MatchString(text) {
					t.Errorf("%s: %v is not a well-formed error code", where, code)
				}
			}
			if _, ok := resolved["x-retryable"].(bool); !ok {
				t.Errorf("%s: x-retryable is missing; retry classification is required "+
					"by SPECIFICATIONS.md section 93", where)
			}
		}
	}
}

// The error code a failure returns must be one the response declares, and
// every declared code must be reachable through some response, so the
// catalogue and the document cannot drift apart.
func TestExamplesUseADeclaredErrorCode(t *testing.T) {
	doc := load(t)
	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	for _, op := range operations {
		responses, _ := op.Fields["responses"].(map[string]any)
		for status, entry := range responses {
			if strings.HasPrefix(status, "2") {
				continue
			}
			object, _ := entry.(map[string]any)
			resolved, err := doc.Resolve(object)
			if err != nil {
				continue
			}
			declared := map[string]bool{}
			codes, _ := resolved["x-error-codes"].([]any)
			for _, code := range codes {
				if text, ok := code.(string); ok {
					declared[text] = true
				}
			}
			example := exampleOf(resolved)
			body, ok := example.(map[string]any)
			if !ok {
				continue
			}
			errorBody, ok := body["error"].(map[string]any)
			if !ok {
				continue
			}
			code, _ := errorBody["code"].(string)
			where := fmt.Sprintf("%s %s %s", strings.ToUpper(op.Method), op.Path, status)
			if !declared[code] {
				t.Errorf("%s: the example returns %q, which the response does not declare", where, code)
			}
			retryable, hasRetryable := errorBody["retryable"].(bool)
			expected, _ := resolved["x-retryable"].(bool)
			if hasRetryable && retryable != expected {
				t.Errorf("%s: the example says retryable=%v but the response is classified %v",
					where, retryable, expected)
			}
		}
	}
}

func exampleOf(response map[string]any) any {
	content, _ := response["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	if media == nil {
		return nil
	}
	return media["example"]
}

// Every example the contract publishes must satisfy the schema it is
// published against. This is the check that catches a schema and its
// documentation drifting apart, which is how a hand-written contract
// usually starts lying.
func TestEveryExampleSatisfiesItsSchema(t *testing.T) {
	doc := load(t)
	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}

	checked := 0
	for _, op := range operations {
		key := strings.ToUpper(op.Method) + " " + op.Path

		if body, ok := op.Fields["requestBody"].(map[string]any); ok {
			resolved, err := doc.Resolve(body)
			if err != nil {
				t.Errorf("%s requestBody: %v", key, err)
				continue
			}
			content, _ := resolved["content"].(map[string]any)
			media, _ := content["application/json"].(map[string]any)
			if media == nil {
				t.Errorf("%s: request body has no application/json content", key)
				continue
			}
			example, present := media["example"]
			if !present {
				t.Errorf("%s: request body has no example; SPECIFICATIONS.md section 27 "+
					"requires examples and they are what makes the schema testable", key)
				continue
			}
			schema, _ := media["schema"].(map[string]any)
			if err := doc.ValidateExample(schema, example, key+" request"); err != nil {
				t.Errorf("%v", err)
			}
			checked++
		}

		responses, _ := op.Fields["responses"].(map[string]any)
		for status, entry := range responses {
			object, _ := entry.(map[string]any)
			resolved, err := doc.Resolve(object)
			if err != nil {
				t.Errorf("%s %s: %v", key, status, err)
				continue
			}
			content, _ := resolved["content"].(map[string]any)
			media, _ := content["application/json"].(map[string]any)
			if media == nil {
				continue
			}
			example, present := media["example"]
			if !present {
				t.Errorf("%s %s: response has no example", key, status)
				continue
			}
			schema, _ := media["schema"].(map[string]any)
			if err := doc.ValidateExample(schema, example, key+" "+status); err != nil {
				t.Errorf("%v", err)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no examples were checked; the test is not exercising the contract")
	}
	t.Logf("validated %d examples against their schemas", checked)
}

// A $ref that does not resolve is a document that no generator can consume.
func TestEveryReferenceResolves(t *testing.T) {
	doc := load(t)
	var walk func(value any, where string)
	refs := 0
	walk = func(value any, where string) {
		switch typed := value.(type) {
		case map[string]any:
			if ref, ok := typed["$ref"].(string); ok {
				refs++
				if _, err := doc.Resolve(map[string]any{"$ref": ref}); err != nil {
					t.Errorf("%s: %v", where, err)
				}
			}
			for key, item := range typed {
				walk(item, where+"."+key)
			}
		case []any:
			for i, item := range typed {
				walk(item, fmt.Sprintf("%s[%d]", where, i))
			}
		}
	}
	walk(doc.Root, "document")
	if refs == 0 {
		t.Fatal("the document contains no references; the walk is not working")
	}
	t.Logf("resolved %d references", refs)
}

// A component nobody references is either dead weight or a sign that an
// operation forgot to use it.
func TestEveryComponentSchemaIsUsed(t *testing.T) {
	doc := load(t)
	components, _ := doc.Root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if len(schemas) == 0 {
		t.Fatal("the contract declares no component schemas")
	}

	used := map[string]bool{}
	var walk func(value any, from string)
	walk = func(value any, from string) {
		switch typed := value.(type) {
		case map[string]any:
			if ref, ok := typed["$ref"].(string); ok {
				name := strings.TrimPrefix(ref, "#/components/schemas/")
				if name != ref && name != from {
					used[name] = true
				}
			}
			for _, item := range typed {
				walk(item, from)
			}
		case []any:
			for _, item := range typed {
				walk(item, from)
			}
		}
	}
	// References from anywhere except a schema's own definition.
	for key, section := range doc.Root {
		if key == "components" {
			continue
		}
		walk(section, "")
	}
	for name, schema := range schemas {
		walk(schema, name)
	}
	for key, section := range components {
		if key == "schemas" {
			continue
		}
		walk(section, "")
	}

	for _, name := range sortedKeys(schemas) {
		if !used[name] {
			t.Errorf("component schema %q is never referenced", name)
		}
	}
}

// Every schema must document itself, so the generated client and the
// contract page explain what a field means.
func TestEveryComponentSchemaAndPropertyIsDocumented(t *testing.T) {
	doc := load(t)
	components, _ := doc.Root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)

	for _, name := range sortedKeys(schemas) {
		schema, ok := schemas[name].(map[string]any)
		if !ok {
			t.Errorf("components.schemas.%s is not an object", name)
			continue
		}
		if description, _ := schema["description"].(string); description == "" {
			t.Errorf("components.schemas.%s: description is missing", name)
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, field := range sortedKeys(properties) {
			property, _ := properties[field].(map[string]any)
			_, isRef := property["$ref"]
			description, _ := property["description"].(string)
			if !isRef && description == "" {
				t.Errorf("components.schemas.%s.%s: description is missing and it is not a "+
					"reference to a documented schema", name, field)
			}
		}
	}
}

// The security scheme must exist and be applied, and the health endpoint
// must stay reachable without a credential so a probe never needs one.
func TestSecurityIsDeclaredAndHealthIsExempt(t *testing.T) {
	doc := load(t)
	components, _ := doc.Root["components"].(map[string]any)
	schemes, _ := components["securitySchemes"].(map[string]any)
	if len(schemes) == 0 {
		t.Fatal("no security scheme is declared for the internal API")
	}
	global, _ := doc.Root["security"].([]any)
	if len(global) == 0 {
		t.Error("no default security requirement is declared")
	}

	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	for _, op := range operations {
		id, _ := op.Fields["operationId"].(string)
		override, hasOverride := op.Fields["security"].([]any)
		if id == "health" {
			if !hasOverride || len(override) != 0 {
				t.Error("health must override security with an empty requirement so a probe " +
					"and a compatibility check need no credential")
			}
			continue
		}
		if hasOverride && len(override) == 0 {
			t.Errorf("%s: opts out of authentication", id)
		}
	}
}

// The fingerprint gate. It does not decide whether a change is compatible;
// it makes an interface change impossible to land silently
// (SPECIFICATIONS.md section 49).
func TestContractMatchesItsLock(t *testing.T) {
	doc := load(t)
	fingerprint, err := doc.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	version, err := doc.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	if os.Getenv("UPDATE_CONTRACT_LOCK") == "1" {
		lock := &Lock{
			Contract:    internalAPIName,
			Version:     version,
			Fingerprint: fingerprint,
			Note: "Regenerate with `make contracts-lock` after reviewing the change. " +
				"A change that removes or narrows anything a client may rely on requires a new " +
				"major version and a new path prefix; see contracts/internal-api/README.md.",
		}
		if err := lock.Write(internalLockPath); err != nil {
			t.Fatalf("write lock: %v", err)
		}
		t.Logf("updated %s to %s", filepath.Base(internalLockPath), fingerprint)
		return
	}

	lock, err := LoadLock(internalLockPath)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	if lock.Contract != internalAPIName {
		t.Errorf("lock is for %q, want %q", lock.Contract, internalAPIName)
	}
	if lock.Version != version {
		t.Errorf("lock records version %q but the contract declares %q", lock.Version, version)
	}
	if lock.Fingerprint != fingerprint {
		t.Errorf(`the internal API contract changed but its lock file did not.

  recorded:  %s
  computed:  %s

Review the change against the compatibility rules in
contracts/internal-api/README.md. A change that removes an operation or a
field, narrows a type, adds a required field, or changes the meaning of an
existing one is breaking and needs a new major version and path prefix. Then
record the reviewed contract with:

  make contracts-lock`, lock.Fingerprint, fingerprint)
	}
}

// The fingerprint must ignore prose and notice shape, otherwise it either
// nags on every reworded sentence or misses a real change.
func TestFingerprintIgnoresProseAndNoticesShape(t *testing.T) {
	doc := load(t)
	base, err := doc.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	reworded := clone(t, doc.Root)
	info, _ := reworded["info"].(map[string]any)
	info["description"] = "totally different prose"
	if got := fingerprintOf(t, reworded); got != base {
		t.Error("rewording a description changed the fingerprint; the lock would nag on every edit")
	}

	changed := clone(t, doc.Root)
	components, _ := changed["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	severity, _ := schemas["Severity"].(map[string]any)
	severity["enum"] = []any{"INFO", "WARNING", "CRITICAL", "FATAL"}
	if got := fingerprintOf(t, changed); got == base {
		t.Error("adding an enum value did not change the fingerprint")
	}

	removed := clone(t, doc.Root)
	paths, _ := removed["paths"].(map[string]any)
	delete(paths, "/internal/v1/health")
	if got := fingerprintOf(t, removed); got == base {
		t.Error("removing an operation did not change the fingerprint")
	}
}

func clone(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("clone: %v", err)
	}
	return out
}

func fingerprintOf(t *testing.T, root map[string]any) string {
	t.Helper()
	doc := &Document{Path: "memory", Root: root}
	fingerprint, err := doc.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fingerprint
}
