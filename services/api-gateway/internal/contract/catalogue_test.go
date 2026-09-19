package contract

import (
	"encoding/json"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
)

// The measurement catalogue — which unit each type takes and what values are
// plausible for it — is written out twice: here in
// internal/measurement/catalog.go and again in the processor's
// src/domain/measurement.rs, because neither service can import the other's.
// Both validate against their own copy, so the two must agree.
//
// Nothing else keeps them in step. The enums are already gated — the types,
// the units, the windows and the statuses each have a test on both sides —
// and the pairing of type to unit, and the bounds, were the part of the same
// catalogue that was not. Drift there is quiet in the worst way: the gateway
// accepts a reading the processor then rejects, so every job carrying one
// fails with PROCESSING_REJECTED and the client is told only that the
// processing service refused the data.
//
// The contract publishes the catalogue, and this test and its Rust
// counterpart (`value_ranges_and_units_match_the_contract`) each confront
// one implementation with it.
func TestCatalogueMatchesTheContract(t *testing.T) {
	doc := load(t)

	components, _ := doc.Root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	measurementType, ok := schemas["MeasurementType"].(map[string]any)
	if !ok {
		t.Fatal("components.schemas.MeasurementType is missing")
	}
	published, ok := measurementType["x-value-ranges"].(map[string]any)
	if !ok {
		t.Fatal("components.schemas.MeasurementType.x-value-ranges is missing")
	}

	if len(published) != len(measurement.Catalog) {
		t.Errorf("the contract publishes %d types, the catalogue has %d",
			len(published), len(measurement.Catalog))
	}

	for _, entry := range measurement.Catalog {
		name := string(entry.Code)
		raw, ok := published[name].(map[string]any)
		if !ok {
			t.Errorf("x-value-ranges has no entry for %s", name)
			continue
		}
		if unit, _ := raw["unit"].(string); unit != entry.Unit {
			t.Errorf("%s: the contract publishes unit %q, the catalogue uses %q", name, unit, entry.Unit)
		}
		if got, ok := number(raw["min"]); !ok || got != entry.Min {
			t.Errorf("%s: the contract publishes minimum %v, the catalogue uses %v", name, raw["min"], entry.Min)
		}
		if got, ok := number(raw["max"]); !ok || got != entry.Max {
			t.Errorf("%s: the contract publishes maximum %v, the catalogue uses %v", name, raw["max"], entry.Max)
		}
	}

	// The other direction: a type published in the contract that this
	// gateway does not know would be one it could never accept a reading
	// for, however valid the processor found it.
	known := make(map[string]bool, len(measurement.Catalog))
	for _, entry := range measurement.Catalog {
		known[string(entry.Code)] = true
	}
	for name := range published {
		if !known[name] {
			t.Errorf("the contract publishes %s, which this gateway's catalogue does not have", name)
		}
	}
}

// number reads a JSON number, which the contract loader keeps as a
// json.Number so a value keeps the spelling it had in the file.
func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

// The enum and the catalogue describe the same set, so a type added to one
// and not the other is caught whichever side it is added to.
func TestCatalogueCoversTheContractEnum(t *testing.T) {
	doc := load(t)
	components, _ := doc.Root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	measurementType, _ := schemas["MeasurementType"].(map[string]any)
	values, _ := measurementType["enum"].([]any)
	if len(values) == 0 {
		t.Fatal("components.schemas.MeasurementType.enum is missing")
	}

	inCatalogue := make(map[string]bool, len(measurement.Catalog))
	for _, entry := range measurement.Catalog {
		inCatalogue[string(entry.Code)] = true
	}
	for _, value := range values {
		name, _ := value.(string)
		if !inCatalogue[name] {
			t.Errorf("the contract's MeasurementType enum has %s, the catalogue does not", name)
		}
	}
	if len(values) != len(measurement.Catalog) {
		t.Errorf("the enum lists %d types, the catalogue has %d", len(values), len(measurement.Catalog))
	}
}

// The public contract publishes the same set to clients. A type added to the
// catalogue and not there is one no generated client can send, and one
// published there and not in the catalogue is one the gateway would refuse.
func TestPublicContractMeasurementTypesMatchTheCatalogue(t *testing.T) {
	doc := loadPublic(t)
	components, _ := doc.Root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	measurementType, _ := schemas["MeasurementType"].(map[string]any)
	values, _ := measurementType["enum"].([]any)
	if len(values) == 0 {
		t.Fatal("the public contract has no MeasurementType enum")
	}

	inCatalogue := make(map[string]bool, len(measurement.Catalog))
	for _, entry := range measurement.Catalog {
		inCatalogue[string(entry.Code)] = true
	}
	published := make(map[string]bool, len(values))
	for _, value := range values {
		name, _ := value.(string)
		published[name] = true
		if !inCatalogue[name] {
			t.Errorf("the public contract publishes %s, which the catalogue does not have", name)
		}
	}
	for name := range inCatalogue {
		if !published[name] {
			t.Errorf("the catalogue has %s, which the public contract does not publish", name)
		}
	}
}
