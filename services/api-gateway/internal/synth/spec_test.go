package synth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

var (
	testFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	testTo   = time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
)

// small is a spec that generates in milliseconds: two patients, two types,
// an hour at five-minute cadence.
func small(seed int64) Spec {
	s := DefaultSpec(seed, testFrom, testTo)
	s.Users = UsersSpec{Admins: 1, Operators: 1, Users: 1, Domain: "synthetic.invalid"}
	s.Patients.Count = 2
	s.Time.Interval = Duration{5 * time.Minute}
	s.Streams = map[domain.MeasurementType]StreamSpec{
		domain.HeartRate: s.Streams[domain.HeartRate],
		domain.SpO2:      s.Streams[domain.SpO2],
	}
	return s
}

func TestDefaultSpecIsValidAndCoversEveryType(t *testing.T) {
	s := DefaultSpec(1, testFrom, testTo)
	if err := s.Validate(); err != nil {
		t.Fatalf("default spec: %v", err)
	}
	if len(s.Streams) != 7 {
		t.Errorf("default spec has %d streams, want all 7 catalogue types", len(s.Streams))
	}
	if got := s.Types(); got[0] != domain.HeartRate || got[6] != domain.RespiratoryRate {
		t.Errorf("Types() = %v, want catalogue order", got)
	}
}

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Spec)
		wants string
	}{
		{"negative users", func(s *Spec) { s.Users.Admins = -1 }, "users: counts"},
		{"mail domain without a dot", func(s *Spec) { s.Users.Domain = "invalid" }, "users.domain"},
		{"negative patients", func(s *Spec) { s.Patients.Count = -1 }, "patients.count"},
		{"ages reversed", func(s *Spec) { s.Patients.AgeMin, s.Patients.AgeMax = 50, 20 }, "ages must satisfy"},
		{"unknown sex", func(s *Spec) { s.Patients.SexWeights[domain.Sex("X")] = 1 }, "unknown sex"},
		{"no sex weight", func(s *Spec) { s.Patients.SexWeights = map[domain.Sex]float64{} }, "at least one positive weight"},
		{"to before from", func(s *Spec) { s.Time.To = s.Time.From.Add(-time.Second) }, "to must be after from"},
		{"from before 1900", func(s *Spec) { s.Time.From = time.Date(1850, 1, 1, 0, 0, 0, 0, time.UTC) }, "time.from"},
		{"interval under a second", func(s *Spec) { s.Time.Interval = Duration{500 * time.Millisecond} }, "time.interval"},
		{"jitter of one", func(s *Spec) { s.Time.Jitter = 1 }, "time.jitter"},
		{"empty source", func(s *Spec) { s.Source = "" }, "source"},
		{"padded source", func(s *Spec) { s.Source = " x" }, "source"},
		{"long source", func(s *Spec) { s.Source = strings.Repeat("s", 65) }, "source"},
		{"no streams", func(s *Spec) { s.Streams = nil }, "streams: at least one"},
		{"unknown type", func(s *Spec) { s.Streams["PULSE"] = s.Streams[domain.HeartRate] }, "unknown measurement type"},
		{"negative noise", func(s *Spec) { st := s.Streams[domain.HeartRate]; st.Noise = -1; s.Streams[domain.HeartRate] = st }, "must not be negative"},
		{"too many decimals", func(s *Spec) { st := s.Streams[domain.HeartRate]; st.Decimals = 7; s.Streams[domain.HeartRate] = st }, "decimals"},
		{"rate over one", func(s *Spec) {
			st := s.Streams[domain.HeartRate]
			st.Anomalies.Rate = 1.5
			s.Streams[domain.HeartRate] = st
		}, "anomalies.rate"},
		{"rate without kinds", func(s *Spec) {
			st := s.Streams[domain.HeartRate]
			st.Anomalies.Kinds = nil
			s.Streams[domain.HeartRate] = st
		}, "anomalies.kinds: required"},
		{"unknown kind", func(s *Spec) {
			st := s.Streams[domain.HeartRate]
			st.Anomalies.Kinds = []AnomalyKind{"flatline"}
			s.Streams[domain.HeartRate] = st
		}, "unknown kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := small(1)
			tc.mut(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wants)
			}
		})
	}
}

func TestSpecRoundTripsThroughJSONAndRejectsUnknownFields(t *testing.T) {
	s := small(7)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"interval":"5m0s"`) {
		t.Errorf("interval should serialise as a duration string: %s", raw)
	}
	back, err := ParseSpec(raw)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if back.Seed != 7 || back.Time.Interval.Duration != 5*time.Minute || len(back.Streams) != 2 || !back.Time.To.Equal(testTo) {
		t.Errorf("round trip lost data: %+v", back)
	}

	if _, err := ParseSpec([]byte(`{"seed":1,"sed":2}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("a misspelt key must be an error, got %v", err)
	}
	if _, err := ParseSpec([]byte(`{"time":{"interval":60}}`)); err == nil || !strings.Contains(err.Error(), "duration must be a string") {
		t.Errorf("a numeric interval must be an error, got %v", err)
	}
}

func TestSamplesAndMeasurementCounts(t *testing.T) {
	s := small(1)
	if got := s.Time.Samples(); got != 12 {
		t.Errorf("Samples() = %d, want 12 (an hour at 5m)", got)
	}
	if got := s.Measurements(); got != 2*2*12 {
		t.Errorf("Measurements() = %d, want 48", got)
	}
	s.Time.To = s.Time.From.Add(11*time.Minute + time.Second)
	if got := s.Time.Samples(); got != 3 {
		t.Errorf("Samples() = %d, want 3 (partial interval rounds up)", got)
	}
}
