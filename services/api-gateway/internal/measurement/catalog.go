// Package measurement is the application service behind the Measurement
// API (SPECIFICATIONS.md section 12). It validates readings strictly against
// the type catalogue, checks the patient, writes audit records with every
// state change and reports to the observability ports. Persistence is
// behind the Store port, implemented in infra/postgres.
package measurement

import (
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Type describes one accepted kind of reading: its canonical unit and the
// technical plausibility range, inclusive. The bounds reject values that
// cannot be a reading at all; they are data validation, not medical
// thresholds (OQ-08).
type Type struct {
	Code domain.MeasurementType
	Unit string
	Min  float64
	Max  float64
}

// Catalog lists the supported types in specification order. It mirrors the
// measurement_types table, which the database enforces independently; an
// integration test keeps the two identical.
var Catalog = []Type{
	{domain.HeartRate, "bpm", 0, 300},
	{domain.BloodPressureSystolic, "mmHg", 0, 300},
	{domain.BloodPressureDiastolic, "mmHg", 0, 200},
	{domain.SpO2, "%", 0, 100},
	{domain.BodyTemperature, "C", 20, 45},
	{domain.BloodGlucose, "mg/dL", 0, 1000},
	{domain.RespiratoryRate, "breaths/min", 0, 100},
}

var catalogByCode = func() map[domain.MeasurementType]Type {
	m := make(map[domain.MeasurementType]Type, len(Catalog))
	for _, t := range Catalog {
		m[t.Code] = t
	}
	return m
}()

// Lookup returns the catalogue entry for code.
func Lookup(code domain.MeasurementType) (Type, bool) {
	t, ok := catalogByCode[code]
	return t, ok
}

// TypeCodes lists the supported type codes as strings, in catalogue order.
func TypeCodes() []string {
	out := make([]string, len(Catalog))
	for i, t := range Catalog {
		out[i] = string(t.Code)
	}
	return out
}
