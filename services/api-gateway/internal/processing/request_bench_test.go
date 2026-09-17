package processing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// The cost of turning a job's readings into the request the processor
// receives: building the wire structs and encoding them. Measured because
// the performance baseline put most of a large job's time in the gateway
// rather than the engine (docs/PERFORMANCE_OPTIMIZATIONS.md).
func benchReadings(n int) []domain.Measurement {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	patient := uuid.New()
	out := make([]domain.Measurement, n)
	for i := range out {
		out[i] = domain.Measurement{
			ID: uuid.New(), PatientID: patient, Type: domain.HeartRate, Value: 60 + float64(i%40), Unit: "bpm",
			RecordedAt: base.Add(time.Duration(i) * time.Second), Source: "bench",
		}
	}
	return out
}

// BenchmarkConvertReadings60k is what the store used to do after the scan:
// 60,000 domain rows into the wire shape. The store now scans into the wire
// shape directly, so this is the cost that was removed from the job path.
func BenchmarkConvertReadings60k(b *testing.B) {
	readings := benchReadings(60_000)
	b.ReportAllocs()
	for b.Loop() {
		out := make([]Reading, len(readings))
		for i, m := range readings {
			out[i] = ReadingFromMeasurement(m)
		}
	}
}

func BenchmarkEncodeRequest60k(b *testing.B) {
	stored := benchReadings(60_000)
	readings := make([]Reading, len(stored))
	for i, m := range stored {
		readings[i] = ReadingFromMeasurement(m)
	}
	job := domain.ProcessingJob{ID: uuid.New(), PatientID: stored[0].PatientID, Parameters: json.RawMessage(`{}`), AlgorithmVersion: "1.0.0", Status: domain.JobPending, RequestedAt: time.Now()}
	params := Parameters{MeasurementTypes: []domain.MeasurementType{domain.HeartRate}, Windows: []string{"1m", "5m"}, Percentiles: []int{50, 95}}
	var svc Service
	req := svc.request(job, NewJob{PatientID: job.PatientID, Parameters: params, AlgorithmVersion: "1.0.0"}, readings)
	b.ReportAllocs()
	for b.Loop() {
		body, err := json.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(body)))
	}
}
