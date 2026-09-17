package processorclient

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/contract"
)

// The retry classification is this gateway's reading of the contract, so it
// is checked against the contract: for every failure POST /process
// declares, the status and each code it can carry must be classified here
// the way the contract's x-retryable says (SPECIFICATIONS.md section 93).
// The two services then cannot drift apart on what is worth repeating: the
// processor's own contract test proves it emits these codes with these
// classifications, and this one proves the gateway acts on them the same
// way.
func TestRetryClassificationMatchesTheContract(t *testing.T) {
	doc, err := contract.Load(filepath.Join("..", "..", "..", "..", "..", "contracts", "internal-api", "processor-v1.json"))
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	ops, err := doc.Operations()
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	var process *contract.Operation
	for i := range ops {
		if ops[i].Path == processPath && ops[i].Method == "post" {
			process = &ops[i]
		}
	}
	if process == nil {
		t.Fatalf("the contract declares no POST %s", processPath)
	}
	responses, ok := process.Fields["responses"].(map[string]any)
	if !ok {
		t.Fatal("POST /process declares no responses")
	}

	// Where one status carries codes of both classifications, x-retryable
	// is the broader answer and the body's flag decides per code; the
	// contract names the exceptions in prose, and so does this table.
	narrowed := map[string]bool{"PROCESSING_CANCELLED": false}

	checked := 0
	for status, raw := range responses {
		code, err := strconv.Atoi(status)
		if err != nil || code < 400 {
			continue
		}
		object, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("response %s is not an object", status)
		}
		resolved, err := doc.Resolve(object)
		if err != nil {
			t.Fatalf("response %s: %v", status, err)
		}
		retryable, ok := resolved["x-retryable"].(bool)
		if !ok {
			t.Fatalf("response %s declares no x-retryable", status)
		}
		codes, ok := resolved["x-error-codes"].([]any)
		if !ok {
			t.Fatalf("response %s declares no x-error-codes", status)
		}
		for _, c := range codes {
			name, _ := c.(string)
			want := retryable
			if v, ok := narrowed[name]; ok {
				want = v
			}
			if got := classify(code, name).Retryable; got != want {
				t.Errorf("%d %s: the gateway classifies it retryable=%v, the contract says %v", code, name, got, want)
			}
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("checked only %d codes; the contract was not read as expected", checked)
	}
}
