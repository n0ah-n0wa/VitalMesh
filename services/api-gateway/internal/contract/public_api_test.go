package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The public contract and its lock file, relative to this package.
const (
	publicAPIPath  = "../../../../contracts/openapi/vitalmesh-public-v1.json"
	publicLockPath = "../../../../contracts/openapi/vitalmesh-public-v1.lock.json"
	publicAPIName  = "vitalmesh-public-v1"
)

// The operations the public API serves (SPECIFICATIONS.md sections 9 to 13
// and 27). This list is the contract's side of the agreement; the
// implementation's side is checked in internal/httpapi, which confronts the
// same document with the routes the gateway actually mounts. An endpoint
// added or removed without updating both fails.
var publicOperations = map[string]string{
	"GET /health":                                          "health",
	"GET /ready":                                           "readiness",
	"POST /api/v1/auth/login":                              "login",
	"GET /api/v1/auth/me":                                  "currentUser",
	"POST /api/v1/patients":                                "createPatient",
	"GET /api/v1/patients":                                 "listPatients",
	"GET /api/v1/patients/{patient_id}":                    "getPatient",
	"DELETE /api/v1/patients/{patient_id}":                 "deletePatient",
	"GET /api/v1/patients/{patient_id}/measurements":       "listPatientMeasurements",
	"GET /api/v1/patients/{patient_id}/processing-results": "listPatientProcessingResults",
	"POST /api/v1/measurements":                            "createMeasurement",
	"POST /api/v1/measurements/batch":                      "createMeasurementBatch",
	"GET /api/v1/measurements/{measurement_id}":            "getMeasurement",
	"DELETE /api/v1/measurements/{measurement_id}":         "deleteMeasurement",
	"POST /api/v1/processing/jobs":                         "createProcessingJob",
	"GET /api/v1/processing/jobs/{job_id}":                 "getProcessingJob",
}

// Operations that answer without a credential: the platform probes, which
// must stay reachable so a probe never needs one, and login, which is how a
// credential is obtained in the first place.
var publicallyReachable = map[string]bool{
	"health":    true,
	"readiness": true,
	"login":     true,
}

func loadPublic(t *testing.T) *Document {
	t.Helper()
	doc, err := Load(publicAPIPath)
	if err != nil {
		t.Fatalf("load public contract: %v", err)
	}
	return doc
}

func TestPublicContractParsesAndDeclaresItsVersion(t *testing.T) {
	doc := loadPublic(t)

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

func TestPublicContractDefinesExactlyTheServedOperations(t *testing.T) {
	doc := loadPublic(t)
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

	for key, want := range publicOperations {
		id, ok := got[key]
		if !ok {
			t.Errorf("%s is missing from the public contract", key)
			continue
		}
		if id != want {
			t.Errorf("%s: operationId = %q, want %q", key, id, want)
		}
	}
	for key := range got {
		if _, expected := publicOperations[key]; !expected {
			t.Errorf("%s is not a served operation; publishing an endpoint is a contract "+
				"change that must be reviewed and reflected in this test", key)
		}
	}
}

// Every operation must be usable from the document alone: named, described,
// able to carry the correlation headers, and honest about how it can fail.
func TestPublicContractOperationsAreCompletelySpecified(t *testing.T) {
	doc := loadPublic(t)
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

		// Section 85 requires the correlation identifiers to propagate
		// through every hop, so every operation must accept them.
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
		if _, present := responses["500"]; !present {
			t.Errorf("%s: no 500 response is declared; every operation can fail unexpectedly", key)
		}
		success := 0
		for status := range responses {
			if strings.HasPrefix(status, "2") {
				success++
			}
		}
		if success == 0 {
			t.Errorf("%s: declares no successful response", key)
		}
		checkPublicResponses(t, doc, key, responses)
	}
}

// checkPublicResponses asserts that every declared response carries a
// description, echoes the correlation headers, and that every failure that
// returns the error envelope says which codes it can produce and whether it
// is worth retrying (SPECIFICATIONS.md sections 25, 27 and 93).
func checkPublicResponses(t *testing.T, doc *Document, operation string, responses map[string]any) {
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

		// 204 says there is nothing to return, so it declares no content.
		if status == "204" {
			if _, present := resolved["content"]; present {
				t.Errorf("%s: a 204 must not declare content", where)
			}
			continue
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
		if strings.HasPrefix(status, "2") {
			continue
		}

		// GET /ready reports a failing dependency in the same document it
		// reports a healthy one, so its 503 is not an error envelope. It is
		// the one failure in this API that is not, and the exception is
		// narrow on purpose: anything else returning a non-envelope failure
		// is a bug in the document or in the handler.
		ref, _ := schema["$ref"].(string)
		if ref != "#/components/schemas/Error" {
			if operation == "GET /ready" && status == "503" &&
				ref == "#/components/schemas/Readiness" {
				continue
			}
			t.Errorf("%s: failures must use the shared error envelope, got %v", where, schema)
			continue
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
			t.Errorf("%s: x-retryable is missing; retry classification is required by "+
				"SPECIFICATIONS.md section 93", where)
		}
	}
}

// A published error code that no longer exists in the gateway is a contract
// that lies. This walks the document's codes and requires each to appear in
// the gateway's own source, which is what makes the catalogue in docs/API.md
// and this document verifiable rather than aspirational.
func TestPublicContractErrorCodesExistInTheGateway(t *testing.T) {
	doc := loadPublic(t)

	codes := map[string]bool{}
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, item := range typed {
				if key == "x-error-codes" {
					list, _ := item.([]any)
					for _, entry := range list {
						if text, ok := entry.(string); ok {
							codes[text] = true
						}
					}
					continue
				}
				walk(item)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(doc.Root)
	if len(codes) == 0 {
		t.Fatal("the document declares no error codes; the walk is not working")
	}

	source := gatewaySource(t)
	for code := range codes {
		if !strings.Contains(source, `"`+code+`"`) {
			t.Errorf("the contract publishes %q but no non-test Go source defines it; "+
				"either the code was renamed and the contract was not, or the contract "+
				"invented it", code)
		}
	}
	t.Logf("checked %d published error codes against the gateway source", len(codes))
}

// gatewaySource concatenates the gateway's non-test Go sources. Tests are
// excluded deliberately: a code that appears only in a test is a code no
// client can ever receive.
func gatewaySource(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- walking this repository's own source tree in a test
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("read gateway source: %v", err)
	}
	if b.Len() == 0 {
		t.Fatal("read no gateway source; the walk is not working")
	}
	return b.String()
}

// Every example the contract publishes must satisfy the schema it is
// published against. This is what catches a schema and its documentation
// drifting apart, which is how a hand-written contract starts lying.
func TestPublicContractExamplesSatisfyTheirSchemas(t *testing.T) {
	doc := loadPublic(t)
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

// The error code an example returns must be one its response declares, so
// the catalogue and the examples cannot drift apart.
func TestPublicContractExamplesUseADeclaredErrorCode(t *testing.T) {
	doc := loadPublic(t)
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
			if len(declared) == 0 {
				continue
			}
			content, _ := resolved["content"].(map[string]any)
			media, _ := content["application/json"].(map[string]any)
			if media == nil {
				continue
			}
			body, ok := media["example"].(map[string]any)
			if !ok {
				continue
			}
			errorBody, ok := body["error"].(map[string]any)
			if !ok {
				continue
			}
			code, _ := errorBody["code"].(string)
			if !declared[code] {
				t.Errorf("%s %s %s: the example returns %q, which the response does not declare",
					strings.ToUpper(op.Method), op.Path, status, code)
			}
		}
	}
}

// A $ref that does not resolve is a document no generator can consume.
func TestPublicContractReferencesResolve(t *testing.T) {
	doc := loadPublic(t)
	refs := 0
	var walk func(value any, where string)
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
func TestPublicContractComponentsAreAllUsed(t *testing.T) {
	doc := loadPublic(t)
	components, _ := doc.Root["components"].(map[string]any)
	if len(components) == 0 {
		t.Fatal("the contract declares no components")
	}

	for _, section := range []string{"schemas", "parameters", "responses", "headers"} {
		entries, _ := components[section].(map[string]any)
		if len(entries) == 0 {
			t.Errorf("components.%s is empty", section)
			continue
		}
		prefix := "#/components/" + section + "/"
		used := map[string]bool{}
		var walk func(value any, from string)
		walk = func(value any, from string) {
			switch typed := value.(type) {
			case map[string]any:
				if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, prefix) {
					if name := strings.TrimPrefix(ref, prefix); name != from {
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
		for key, value := range doc.Root {
			if key != "components" {
				walk(value, "")
			}
		}
		for key, value := range components {
			if key != section {
				walk(value, "")
			}
		}
		// References from inside the section itself, except a component's
		// reference to itself.
		for name, value := range entries {
			walk(value, name)
		}
		for _, name := range sortedKeys(entries) {
			if !used[name] {
				t.Errorf("components.%s.%s is never referenced", section, name)
			}
		}
	}
}

// Every schema must document itself, so a generated client and a rendered
// contract page explain what each field means.
func TestPublicContractSchemasAreDocumented(t *testing.T) {
	doc := loadPublic(t)
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

// Authentication must be declared and applied by default, and exactly the
// operations that are meant to answer without a credential may opt out. An
// operation that quietly opts out is how an endpoint ends up unguarded.
func TestPublicContractSecurityIsDeclaredAndOnlyTheRightOperationsAreExempt(t *testing.T) {
	doc := loadPublic(t)
	components, _ := doc.Root["components"].(map[string]any)
	schemes, _ := components["securitySchemes"].(map[string]any)
	if len(schemes) == 0 {
		t.Fatal("no security scheme is declared for the public API")
	}
	if global, _ := doc.Root["security"].([]any); len(global) == 0 {
		t.Error("no default security requirement is declared, so every operation would be public")
	}

	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	for _, op := range operations {
		id, _ := op.Fields["operationId"].(string)
		override, hasOverride := op.Fields["security"].([]any)
		exempt := hasOverride && len(override) == 0
		if publicallyReachable[id] {
			if !exempt {
				t.Errorf("%s must override security with an empty requirement: it has to be "+
					"reachable without a credential", id)
			}
			continue
		}
		if exempt {
			t.Errorf("%s opts out of authentication", id)
		}
	}
}

// Every write operation that can create duplicate state must accept an
// Idempotency-Key (SPECIFICATIONS.md section 24), and no other operation
// should advertise one.
func TestPublicContractDeclaresIdempotencyOnExactlyTheWrites(t *testing.T) {
	doc := loadPublic(t)
	want := map[string]bool{
		"createPatient":          true,
		"createMeasurement":      true,
		"createMeasurementBatch": true,
		"createProcessingJob":    true,
	}

	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	for _, op := range operations {
		id, _ := op.Fields["operationId"].(string)
		accepts := false
		for _, parameter := range op.Parameters {
			if name, _ := parameter["name"].(string); strings.EqualFold(name, "Idempotency-Key") {
				accepts = true
			}
		}
		if want[id] && !accepts {
			t.Errorf("%s does not accept an Idempotency-Key, but it can create duplicate state", id)
		}
		if !want[id] && accepts {
			t.Errorf("%s accepts an Idempotency-Key but is not an idempotent write operation", id)
		}
	}
}

// The fingerprint gate. It does not decide whether a change is compatible;
// it makes an interface change impossible to land silently
// (SPECIFICATIONS.md section 49).
func TestPublicContractMatchesItsLock(t *testing.T) {
	doc := loadPublic(t)
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
			Contract:    publicAPIName,
			Version:     version,
			Fingerprint: fingerprint,
			Note: "Regenerate with `make contracts-lock` after reviewing the change. " +
				"A change that removes or narrows anything a client may rely on requires a new " +
				"major version and a new path prefix; see contracts/openapi/README.md.",
		}
		if err := lock.Write(publicLockPath); err != nil {
			t.Fatalf("write lock: %v", err)
		}
		t.Logf("updated %s to %s", filepath.Base(publicLockPath), fingerprint)
		return
	}

	lock, err := LoadLock(publicLockPath)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	if lock.Contract != publicAPIName {
		t.Errorf("lock is for %q, want %q", lock.Contract, publicAPIName)
	}
	if lock.Version != version {
		t.Errorf("lock records version %q but the contract declares %q", lock.Version, version)
	}
	if lock.Fingerprint != fingerprint {
		t.Errorf(`the public API contract changed but its lock file did not.

  recorded:  %s
  computed:  %s

Review the change against the compatibility rules in
contracts/openapi/README.md. A change that removes an operation or a field,
narrows a type, adds a required field, or changes the meaning of an existing
one is breaking and needs a new major version and path prefix. Then record
the reviewed contract with:

  make contracts-lock`, lock.Fingerprint, fingerprint)
	}
}

// The fingerprint must ignore prose and notice shape, otherwise it either
// nags on every reworded sentence or misses a real change.
func TestPublicFingerprintIgnoresProseAndNoticesShape(t *testing.T) {
	doc := loadPublic(t)
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

	widened := clone(t, doc.Root)
	components, _ := widened["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	role, _ := schemas["Role"].(map[string]any)
	role["enum"] = []any{"ADMIN", "OPERATOR", "USER", "SUPERUSER"}
	if got := fingerprintOf(t, widened); got == base {
		t.Error("adding an enum value did not change the fingerprint")
	}

	removed := clone(t, doc.Root)
	paths, _ := removed["paths"].(map[string]any)
	delete(paths, "/api/v1/patients")
	if got := fingerprintOf(t, removed); got == base {
		t.Error("removing an operation did not change the fingerprint")
	}
}
