package contract

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

// Client-side conformance with the internal API contract (SPECIFICATIONS.md
// section 49).
//
// The processor's own tests confront the contract with the types it serves.
// These confront it with the types the gateway sends and expects, so a
// change to either side that the other has not seen fails here rather than
// in production. What is checked is the shape both services must agree on;
// the behaviour behind it is checked by the end-to-end test, which runs a
// real gateway against a real processor.

// decodeValue parses a JSON document the way the checker expects, with
// numbers kept as written.
func decodeValue(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// schemaRef returns a reference to one component schema.
func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// sampleRequest is a dispatch of the shape the gateway builds. The values
// come from the same code paths the service uses: identifiers as UUID
// strings, timestamps as RFC 3339 in UTC, parameters normalised.
func sampleRequest() processing.Request {
	return processing.Request{
		Job: processing.RequestJob{
			ID:        "6f1a8b0c-9a1e-4d2b-8f3c-2b7a5d6e1c40",
			PatientID: "b1c2d3e4-f5a6-4b7c-8d9e-0f1a2b3c4d5e",
			Parameters: processing.RequestParams{
				MeasurementTypes: []string{"HEART_RATE", "SPO2"},
				Windows:          []string{"1m", "1h"},
				Percentiles:      []int{50, 95},
			},
			AlgorithmVersion: config.DefaultAlgorithmVersion,
			Status:           domain.JobPending,
			RequestedAt:      "2026-09-06T12:00:00Z",
		},
		Readings: []processing.Reading{
			{
				ID: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", Type: "HEART_RATE",
				Value: 72, Unit: "bpm", RecordedAt: "2026-09-06T11:58:00Z",
			},
			{
				ID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", Type: "SPO2",
				Value: 97.5, Unit: "%", RecordedAt: "2026-09-06T11:59:00.500Z",
			},
		},
	}
}

// The request the gateway sends must satisfy the contract's ProcessRequest,
// which rejects unknown fields, so a field added on one side and not the
// other is caught here.
func TestTheGatewaysRequestSatisfiesTheContract(t *testing.T) {
	doc := load(t)

	encoded, err := json.Marshal(sampleRequest())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	value := decodeValue(t, encoded)
	if err := doc.ValidateExample(schemaRef("ProcessRequest"), value, "gateway request"); err != nil {
		t.Fatalf("the dispatch the gateway builds is not a valid ProcessRequest: %v", err)
	}
}

// A request the contract would reject must be rejected by the checker too,
// otherwise the test above proves nothing.
func TestTheConformanceCheckWouldNoticeADivergence(t *testing.T) {
	doc := load(t)

	cases := []struct {
		name   string
		break_ func(map[string]any)
	}{
		{"a window the processor does not implement", func(m map[string]any) {
			job := m["job"].(map[string]any)
			job["parameters"].(map[string]any)["windows"] = []any{"3h"}
		}},
		{"a measurement type the processor does not implement", func(m map[string]any) {
			readings := m["readings"].([]any)
			readings[0].(map[string]any)["type"] = "PULSE"
		}},
		{"a unit outside the catalogue", func(m map[string]any) {
			readings := m["readings"].([]any)
			readings[0].(map[string]any)["unit"] = "beats"
		}},
		{"a timestamp that is not RFC 3339", func(m map[string]any) {
			readings := m["readings"].([]any)
			readings[0].(map[string]any)["recorded_at"] = "yesterday"
		}},
		{"an algorithm version that is not major.minor.patch", func(m map[string]any) {
			m["job"].(map[string]any)["algorithm_version"] = "one"
		}},
		{"a field the contract does not define", func(m map[string]any) {
			m["extra"] = true
		}},
		{"no readings at all", func(m map[string]any) {
			m["readings"] = []any{}
		}},
		{"a status outside the state machine", func(m map[string]any) {
			m["job"].(map[string]any)["status"] = "STARTED"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(sampleRequest())
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			value := decodeValue(t, encoded).(map[string]any)
			tc.break_(value)
			if err := doc.ValidateExample(schemaRef("ProcessRequest"), value, "broken request"); err == nil {
				t.Fatal("the checker accepted a request the contract forbids")
			}
		})
	}
}

// The outcome the contract publishes must be one the gateway can read. The
// client refuses unknown fields, so this is where a field the processor
// starts sending, and the gateway has not been taught, is caught.
func TestTheGatewayCanReadTheContractsOutcome(t *testing.T) {
	doc := load(t)

	example := exampleAt(t, doc, "/internal/v1/process", "post", "200")
	encoded, err := json.Marshal(example)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var outcome processing.Outcome
	dec := json.NewDecoder(strings.NewReader(string(encoded)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&outcome); err != nil {
		t.Fatalf("the gateway cannot read the contract's own outcome: %v", err)
	}

	if outcome.JobID == "" {
		t.Error("the job id did not survive decoding")
	}
	if outcome.AlgorithmVersion != config.DefaultAlgorithmVersion {
		t.Errorf("the contract's example carries algorithm version %q but this gateway asks for %q",
			outcome.AlgorithmVersion, config.DefaultAlgorithmVersion)
	}
	if outcome.ServiceVersion == "" {
		t.Error("the service version did not survive decoding")
	}
	if len(outcome.Results) == 0 {
		t.Fatal("the example carries no results to check")
	}
	for i, r := range outcome.Results {
		if r.MeasurementType == "" || r.Window == "" {
			t.Errorf("result %d lost its identity: %+v", i, r)
		}
		if r.WindowStart.IsZero() {
			t.Errorf("result %d lost its window start", i)
		}
		if len(r.Statistics) == 0 || !json.Valid(r.Statistics) {
			t.Errorf("result %d lost its statistics", i)
		}
		if len(r.Anomalies) == 0 || !json.Valid(r.Anomalies) {
			t.Errorf("result %d lost its anomalies", i)
		}
	}
}

// The contract's own error envelope must be one the client can classify.
func TestTheGatewayCanReadTheContractsFailures(t *testing.T) {
	doc := load(t)
	operations, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}

	seen := 0
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
			content, _ := resolved["content"].(map[string]any)
			media, _ := content["application/json"].(map[string]any)
			if media == nil || media["example"] == nil {
				continue
			}
			encoded, err := json.Marshal(media["example"])
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			// The client reads only the code and the retry flag; it must
			// tolerate every other field the envelope carries.
			var body struct {
				Error struct {
					Code      string `json:"code"`
					Retryable *bool  `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Errorf("%s %s: the gateway cannot read the failure: %v", op.Path, status, err)
				continue
			}
			if body.Error.Code == "" {
				t.Errorf("%s %s: no code survived decoding", op.Path, status)
			}
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no failure examples were checked")
	}
}

// Every code the contract can return must be one the gateway classifies
// deliberately. A code nobody mapped would fall into the catch-all and be
// reported as a protocol failure, which is safe but uninformative.
func TestEveryContractErrorCodeIsClassifiedByTheGateway(t *testing.T) {
	doc := load(t)

	// The codes the client's classification names explicitly, plus the ones
	// it deliberately funnels into the protocol case.
	classified := map[string]bool{
		"PROCESSOR_OVERLOADED":          true,
		"PROCESSOR_SHUTTING_DOWN":       true,
		"PROCESSING_CANCELLED":          true,
		"PROCESSING_TIMEOUT":            true,
		"NO_VALID_MEASUREMENTS":         true,
		"JOB_TOO_LARGE":                 true,
		"UNSUPPORTED_ALGORITHM_VERSION": true,
		"INTERNAL_ERROR":                true,
		"UNAUTHENTICATED":               true,
		"INVALID_REQUEST":               true,
		"UNSUPPORTED_MEDIA_TYPE":        true,
		"REQUEST_BODY_TOO_LARGE":        true,
		"JOB_ALREADY_RUNNING":           true,
		"JOB_NOT_FOUND":                 true,
		"VALIDATION_FAILED":             true,
	}

	var declared []string
	collect(doc.Root, &declared)
	if len(declared) == 0 {
		t.Fatal("the contract declares no error codes")
	}
	for _, code := range declared {
		if !classified[code] {
			t.Errorf("the contract can return %q, which the gateway's client does not classify", code)
		}
	}
}

func collect(value any, out *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if codes, ok := typed["x-error-codes"].([]any); ok {
			for _, c := range codes {
				if text, ok := c.(string); ok {
					*out = append(*out, text)
				}
			}
		}
		for _, item := range typed {
			collect(item, out)
		}
	case []any:
		for _, item := range typed {
			collect(item, out)
		}
	}
}

// The version this gateway is built against must be the document's own.
func TestTheGatewayNamesTheContractVersionItWasBuiltAgainst(t *testing.T) {
	doc := load(t)
	version, err := doc.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if config.InternalContractVersion != version {
		t.Errorf("config.InternalContractVersion is %q but the contract is %q",
			config.InternalContractVersion, version)
	}
}

// exampleAt returns the example published for one response.
func exampleAt(t *testing.T, doc *Document, path, method, status string) any {
	t.Helper()
	paths, _ := doc.Root["paths"].(map[string]any)
	item, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("the contract has no path %s", path)
	}
	operation, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("the contract has no %s %s", method, path)
	}
	responses, _ := operation["responses"].(map[string]any)
	response, ok := responses[status].(map[string]any)
	if !ok {
		t.Fatalf("the contract has no %s response for %s %s", status, method, path)
	}
	resolved, err := doc.Resolve(response)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	content, _ := resolved["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	if media == nil || media["example"] == nil {
		t.Fatalf("%s %s %s publishes no example", method, path, status)
	}
	return media["example"]
}
