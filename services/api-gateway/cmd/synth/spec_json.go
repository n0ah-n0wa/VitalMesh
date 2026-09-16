package main

import (
	"encoding/json"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/synth"
)

// marshalSpec renders a spec the way `synth spec` prints it: indented, so
// it can be edited by hand and passed back with --spec.
func marshalSpec(spec synth.Spec) ([]byte, error) {
	return json.MarshalIndent(spec, "", "  ")
}
