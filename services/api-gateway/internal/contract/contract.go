// Package contract loads and checks the machine-readable API contracts kept
// in the repository's contracts/ directory.
//
// The gateway is the client of the internal processing contract
// (SPECIFICATIONS.md section 8), so the checks live here: a contract that
// cannot be parsed, that describes an operation incompletely, or whose
// examples do not satisfy their own schemas would be discovered by the
// client at integration time otherwise. Section 49 requires the contract to
// be tested and requires an incompatible change to fail CI without an
// intentional version change; [Fingerprint] and the lock file provide that
// gate.
//
// The documents are OpenAPI 3.0.3 written as JSON rather than YAML. JSON is
// an equally valid OpenAPI serialization that every OpenAPI tool consumes,
// and it is parseable by the standard library, so this gate needs no
// dependency in either service and runs wherever Go runs. See
// contracts/internal-api/README.md.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Document is a parsed OpenAPI document. It keeps the raw decoded tree
// rather than a typed model: the checks below are about the document's own
// structure, and a typed model would silently drop anything it does not
// know about, which is precisely what must not happen here.
type Document struct {
	// Path the document was read from, for error messages.
	Path string
	// Root is the decoded document. Numbers are json.Number, so a value
	// keeps the exact spelling it had in the file.
	Root map[string]any
}

// Load reads and decodes an OpenAPI document.
func Load(path string) (*Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read contract: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", path, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%s: trailing content after the document", path)
	}
	return &Document{Path: path, Root: root}, nil
}

// Version reports the contract version from info.version.
func (d *Document) Version() (string, error) {
	info, ok := d.Root["info"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s: info is missing", d.Path)
	}
	version, ok := info["version"].(string)
	if !ok {
		return "", fmt.Errorf("%s: info.version is missing", d.Path)
	}
	return version, nil
}

// Operation is one path/method pair.
type Operation struct {
	Path   string
	Method string
	// Fields of the operation object itself.
	Fields map[string]any
	// Parameters declared on the operation merged with those declared on
	// the path item, references resolved.
	Parameters []map[string]any
}

// Operations lists every operation in path and then method order.
func (d *Document) Operations() ([]Operation, error) {
	paths, ok := d.Root["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: paths is missing", d.Path)
	}
	var out []Operation
	for _, path := range sortedKeys(paths) {
		item, ok := paths[path].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: paths[%s] is not an object", d.Path, path)
		}
		shared, err := d.parameters(item["parameters"], fmt.Sprintf("paths[%s]", path))
		if err != nil {
			return nil, err
		}
		for _, method := range sortedKeys(item) {
			if !isMethod(method) {
				continue
			}
			fields, ok := item[method].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: paths[%s].%s is not an object", d.Path, path, method)
			}
			own, err := d.parameters(fields["parameters"], fmt.Sprintf("paths[%s].%s", path, method))
			if err != nil {
				return nil, err
			}
			out = append(out, Operation{
				Path:       path,
				Method:     method,
				Fields:     fields,
				Parameters: append(append([]map[string]any{}, shared...), own...),
			})
		}
	}
	return out, nil
}

func (d *Document) parameters(value any, where string) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s.parameters is not an array", d.Path, where)
	}
	out := make([]map[string]any, 0, len(list))
	for i, entry := range list {
		object, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %s.parameters[%d] is not an object", d.Path, where, i)
		}
		resolved, err := d.Resolve(object)
		if err != nil {
			return nil, fmt.Errorf("%s: %s.parameters[%d]: %w", d.Path, where, i, err)
		}
		out = append(out, resolved)
	}
	return out, nil
}

// Resolve follows a $ref, if the object is one, to the object it names.
// Only local references into this document are supported; anything else is
// an error rather than a silently unchecked value.
func (d *Document) Resolve(object map[string]any) (map[string]any, error) {
	seen := 0
	for {
		ref, ok := object["$ref"]
		if !ok {
			return object, nil
		}
		pointer, ok := ref.(string)
		if !ok {
			return nil, fmt.Errorf("$ref is not a string")
		}
		seen++
		if seen > 32 {
			return nil, fmt.Errorf("$ref %q: reference cycle", pointer)
		}
		target, err := d.pointer(pointer)
		if err != nil {
			return nil, err
		}
		object = target
	}
}

func (d *Document) pointer(ref string) (map[string]any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("$ref %q: only local references are supported", ref)
	}
	current := any(d.Root)
	for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q: %q is not inside an object", ref, token)
		}
		next, ok := object[token]
		if !ok {
			return nil, fmt.Errorf("$ref %q: %q does not exist", ref, token)
		}
		current = next
	}
	object, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q: target is not an object", ref)
	}
	return object, nil
}

// Fingerprint is a stable digest of everything in the document that a client
// can observe: paths, operations, parameters, schemas, security and the
// declared status codes. Purely descriptive fields are excluded, so
// rewording a description does not require the lock file to be updated,
// while any change to the shape of a request or a response does.
//
// It detects that the interface changed. It does not decide whether the
// change was compatible; that judgement belongs to the reviewer who updates
// the lock file, and to the version rules in the contract's README. A
// dedicated diffing tool replaces this when one is available in CI.
func (d *Document) Fingerprint() (string, error) {
	canonical, err := canonicalize(stripDescriptive(d.Root))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// descriptive keys carry no interface obligation.
var descriptive = map[string]bool{
	"description":  true,
	"summary":      true,
	"example":      true,
	"examples":     true,
	"externalDocs": true,
	"contact":      true,
	"license":      true,
	"tags":         true,
	"title":        true,
}

func stripDescriptive(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if descriptive[key] {
				continue
			}
			out[key] = stripDescriptive(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = stripDescriptive(item)
		}
		return out
	default:
		return value
	}
}

// canonicalize renders a decoded document with object keys in sorted order
// and numbers spelled exactly as they were written, so that the same file
// always produces the same bytes.
func canonicalize(value any) (string, error) {
	var b strings.Builder
	if err := writeCanonical(&b, value); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeCanonical(b *strings.Builder, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		b.WriteByte('{')
		for i, key := range sortedKeys(typed) {
			if i > 0 {
				b.WriteByte(',')
			}
			encoded, err := json.Marshal(key)
			if err != nil {
				return err
			}
			b.Write(encoded)
			b.WriteByte(':')
			if err := writeCanonical(b, typed[key]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case json.Number:
		b.WriteString(typed.String())
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		b.Write(encoded)
	}
	return nil
}

// Lock is the recorded fingerprint of a contract.
type Lock struct {
	Contract    string `json:"contract"`
	Version     string `json:"version"`
	Fingerprint string `json:"fingerprint"`
	Note        string `json:"note"`
}

// LoadLock reads a lock file.
func LoadLock(path string) (*Lock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read lock: %w", err)
	}
	var lock Lock
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", path, err)
	}
	return &lock, nil
}

// Write stores a lock file with a trailing newline and LF line endings.
func (l *Lock) Write(path string) error {
	encoded, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isMethod(name string) bool {
	switch name {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

// Validate checks the document against a JSON Schema subset: the keywords
// the contracts actually use. An unsupported keyword is reported as an
// error rather than ignored, so this can never pass a value it did not
// really check.
//
// It is deliberately not a general JSON Schema implementation. Its only job
// is to prove that the examples the contract publishes satisfy the schemas
// the contract publishes, which is where hand-written documents drift first.
type validator struct {
	doc *Document
}

var supportedKeywords = map[string]bool{
	"$ref": true, "type": true, "format": true, "description": true,
	"enum": true, "nullable": true, "default": true, "example": true,
	"required": true, "properties": true, "additionalProperties": true,
	"items": true, "minItems": true, "maxItems": true, "uniqueItems": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"minLength": true, "maxLength": true, "pattern": true,
	"allOf": true, "oneOf": true, "anyOf": true,
}

// ValidateExample checks one value against one schema, naming the location
// in any failure.
func (d *Document) ValidateExample(schema map[string]any, value any, where string) error {
	v := validator{doc: d}
	return v.check(schema, value, where, 0)
}

func (v validator) check(schema map[string]any, value any, where string, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%s: schema nests too deeply", where)
	}
	resolved, err := v.doc.Resolve(schema)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	for key := range resolved {
		if !supportedKeywords[key] && !strings.HasPrefix(key, "x-") {
			return fmt.Errorf("%s: schema uses %q, which this checker does not implement; "+
				"extend the checker or the contract is not really validated", where, key)
		}
	}

	if value == nil {
		if nullable, _ := resolved["nullable"].(bool); nullable {
			return nil
		}
		if _, hasType := resolved["type"]; !hasType && len(resolved) == 0 {
			return nil
		}
		return fmt.Errorf("%s: null is not allowed here", where)
	}

	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		list, ok := resolved[keyword].([]any)
		if !ok {
			continue
		}
		matches := 0
		var last error
		for i, entry := range list {
			sub, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: %s[%d] is not a schema", where, keyword, i)
			}
			err := v.check(sub, value, where, depth+1)
			if err == nil {
				matches++
				continue
			}
			last = err
			if keyword == "allOf" {
				return err
			}
		}
		if keyword != "allOf" && matches == 0 {
			return fmt.Errorf("%s: matches no %s branch: %w", where, keyword, last)
		}
		if keyword == "oneOf" && matches > 1 {
			return fmt.Errorf("%s: matches %d oneOf branches", where, matches)
		}
	}

	if enum, ok := resolved["enum"].([]any); ok {
		found := false
		for _, allowed := range enum {
			if sameJSON(allowed, value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of the enumerated values", where, value)
		}
	}

	declared, _ := resolved["type"].(string)
	switch declared {
	case "":
		// No type: any of the checks above still applied.
	case "object":
		return v.checkObject(resolved, value, where, depth)
	case "array":
		return v.checkArray(resolved, value, where, depth)
	case "string":
		return v.checkString(resolved, value, where)
	case "number", "integer":
		return v.checkNumber(resolved, value, where, declared)
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected a boolean, got %T", where, value)
		}
	default:
		return fmt.Errorf("%s: unknown type %q", where, declared)
	}
	return nil
}

func (v validator) checkObject(schema map[string]any, value any, where string, depth int) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: expected an object, got %T", where, value)
	}
	properties, _ := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, entry := range required {
			name, ok := entry.(string)
			if !ok {
				return fmt.Errorf("%s: required lists a non-string", where)
			}
			if _, present := object[name]; !present {
				return fmt.Errorf("%s: required property %q is missing", where, name)
			}
		}
	}
	for _, name := range sortedKeys(object) {
		sub, declared := properties[name]
		if !declared {
			switch extra := schema["additionalProperties"].(type) {
			case bool:
				if !extra {
					return fmt.Errorf("%s: property %q is not declared and additional properties are not allowed", where, name)
				}
			case map[string]any:
				if err := v.check(extra, object[name], where+"."+name, depth+1); err != nil {
					return err
				}
			case nil:
				// Unconstrained by default in OpenAPI 3.0.
			default:
				return fmt.Errorf("%s: additionalProperties is neither a boolean nor a schema", where)
			}
			continue
		}
		schemaObject, ok := sub.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: properties[%q] is not a schema", where, name)
		}
		if err := v.check(schemaObject, object[name], where+"."+name, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (v validator) checkArray(schema map[string]any, value any, where string, depth int) error {
	list, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s: expected an array, got %T", where, value)
	}
	if minimum, ok := intKeyword(schema, "minItems"); ok && len(list) < minimum {
		return fmt.Errorf("%s: has %d items, fewer than the minimum %d", where, len(list), minimum)
	}
	if maximum, ok := intKeyword(schema, "maxItems"); ok && len(list) > maximum {
		return fmt.Errorf("%s: has %d items, more than the maximum %d", where, len(list), maximum)
	}
	if unique, _ := schema["uniqueItems"].(bool); unique {
		seen := make(map[string]bool, len(list))
		for i, item := range list {
			encoded, err := canonicalize(item)
			if err != nil {
				return err
			}
			if seen[encoded] {
				return fmt.Errorf("%s[%d]: duplicate item in a uniqueItems array", where, i)
			}
			seen[encoded] = true
		}
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		if _, present := schema["items"]; present {
			return fmt.Errorf("%s: items is not a schema", where)
		}
		return nil
	}
	for i, item := range list {
		if err := v.check(items, item, fmt.Sprintf("%s[%d]", where, i), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (v validator) checkString(schema map[string]any, value any, where string) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("%s: expected a string, got %T", where, value)
	}
	if minimum, ok := intKeyword(schema, "minLength"); ok && len(text) < minimum {
		return fmt.Errorf("%s: is %d characters, shorter than the minimum %d", where, len(text), minimum)
	}
	if maximum, ok := intKeyword(schema, "maxLength"); ok && len(text) > maximum {
		return fmt.Errorf("%s: is %d characters, longer than the maximum %d", where, len(text), maximum)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("%s: pattern %q does not compile: %w", where, pattern, err)
		}
		if !expression.MatchString(text) {
			return fmt.Errorf("%s: %q does not match %q", where, text, pattern)
		}
	}
	if format, ok := schema["format"].(string); ok && format == "date-time" {
		if err := checkRFC3339(text); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

func (v validator) checkNumber(schema map[string]any, value any, where, declared string) error {
	number, ok := value.(json.Number)
	if !ok {
		return fmt.Errorf("%s: expected a number, got %T", where, value)
	}
	asFloat, err := number.Float64()
	if err != nil {
		return fmt.Errorf("%s: %q is not a number: %w", where, number, err)
	}
	if declared == "integer" {
		if _, err := number.Int64(); err != nil {
			return fmt.Errorf("%s: %q is not an integer", where, number)
		}
	}
	if minimum, ok := floatKeyword(schema, "minimum"); ok {
		exclusive, _ := schema["exclusiveMinimum"].(bool)
		if asFloat < minimum || (exclusive && asFloat == minimum) {
			return fmt.Errorf("%s: %v is below the minimum %v", where, asFloat, minimum)
		}
	}
	if maximum, ok := floatKeyword(schema, "maximum"); ok {
		exclusive, _ := schema["exclusiveMaximum"].(bool)
		if asFloat > maximum || (exclusive && asFloat == maximum) {
			return fmt.Errorf("%s: %v is above the maximum %v", where, asFloat, maximum)
		}
	}
	return nil
}

// checkRFC3339 accepts the profile the contract declares: an RFC 3339
// instant. The processor normalizes any offset to UTC on the way in, so an
// offset is accepted here too.
func checkRFC3339(text string) error {
	pattern := `^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$`
	if !regexp.MustCompile(pattern).MatchString(text) {
		return fmt.Errorf("%q is not an RFC 3339 instant", text)
	}
	return nil
}

func intKeyword(schema map[string]any, name string) (int, bool) {
	number, ok := schema[name].(json.Number)
	if !ok {
		return 0, false
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, false
	}
	return value, true
}

func floatKeyword(schema map[string]any, name string) (float64, bool) {
	number, ok := schema[name].(json.Number)
	if !ok {
		return 0, false
	}
	value, err := number.Float64()
	if err != nil {
		return 0, false
	}
	return value, true
}

func sameJSON(a, b any) bool {
	left, err := canonicalize(a)
	if err != nil {
		return false
	}
	right, err := canonicalize(b)
	if err != nil {
		return false
	}
	return left == right
}
