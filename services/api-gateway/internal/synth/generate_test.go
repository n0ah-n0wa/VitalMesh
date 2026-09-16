package synth

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// memory collects everything a generation produces.
type memory struct {
	users        []User
	patients     []Patient
	measurements []Measurement
}

func (m *memory) User(u User) error       { m.users = append(m.users, u); return nil }
func (m *memory) Patient(p Patient) error { m.patients = append(m.patients, p); return nil }
func (m *memory) Measurement(x Measurement) error {
	m.measurements = append(m.measurements, x)
	return nil
}

func generate(t *testing.T, s Spec) (*memory, Counts) {
	t.Helper()
	m := &memory{}
	c, err := Generate(s, m)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return m, c
}

func TestTheSameSeedGivesTheSameData(t *testing.T) {
	a, ca := generate(t, small(42))
	b, cb := generate(t, small(42))
	if !reflect.DeepEqual(a, b) || ca != cb {
		t.Fatal("two generations from the same spec differ")
	}
	c, _ := generate(t, small(43))
	if reflect.DeepEqual(a.measurements, c.measurements) {
		t.Fatal("a different seed produced the same readings")
	}
	if reflect.DeepEqual(a.patients, c.patients) {
		t.Fatal("a different seed produced the same patients")
	}
}

func TestAddingPatientsOrTypesKeepsExistingStreams(t *testing.T) {
	base, _ := generate(t, small(5))

	more := small(5)
	more.Patients.Count = 4
	more.Streams[domain.BloodGlucose] = DefaultSpec(5, testFrom, testTo).Streams[domain.BloodGlucose]
	grown, _ := generate(t, more)

	key := func(m Measurement) string { return m.PatientReference + "|" + string(m.Type) }
	got := map[string][]Measurement{}
	for _, m := range grown.measurements {
		got[key(m)] = append(got[key(m)], m)
	}
	want := map[string][]Measurement{}
	for _, m := range base.measurements {
		want[key(m)] = append(want[key(m)], m)
	}
	for k, series := range want {
		if !reflect.DeepEqual(got[k], series) {
			t.Errorf("stream %s changed when patients and types were added", k)
		}
	}
	if len(grown.patients) != 4 || !reflect.DeepEqual(grown.patients[:2], base.patients) {
		t.Error("the first two patients changed when two were added")
	}
}

func TestEveryReadingIsOneTheAPIAccepts(t *testing.T) {
	s := DefaultSpec(9, testFrom, testTo)
	s.Patients.Count = 3
	s.Time.Interval = Duration{2 * time.Minute}
	for code, st := range s.Streams {
		st.Anomalies.Rate = 0.1 // plenty of episodes, to test the clamping too
		s.Streams[code] = st
	}
	m, c := generate(t, s)
	if c.Measurements != len(m.measurements) || c.Measurements == 0 {
		t.Fatalf("counts %+v disagree with %d readings", c, len(m.measurements))
	}

	refs := map[string]bool{}
	for _, p := range m.patients {
		refs[p.ExternalReference] = true
	}
	seen := map[string]bool{}
	last := map[string]time.Time{}
	for i, x := range m.measurements {
		cat, ok := measurement.Lookup(x.Type)
		if !ok {
			t.Fatalf("reading %d has unknown type %s", i, x.Type)
		}
		if x.Unit != cat.Unit {
			t.Errorf("reading %d: unit %q, want %q", i, x.Unit, cat.Unit)
		}
		if x.Value < cat.Min || x.Value > cat.Max {
			t.Errorf("reading %d: %s value %v outside [%v, %v]", i, x.Type, x.Value, cat.Min, cat.Max)
		}
		if !refs[x.PatientReference] {
			t.Errorf("reading %d names unknown patient %s", i, x.PatientReference)
		}
		if x.RecordedAt.Before(s.Time.From) || !x.RecordedAt.Before(s.Time.To) {
			t.Errorf("reading %d at %s is outside [%s, %s)", i, x.RecordedAt, s.Time.From, s.Time.To)
		}
		if x.RecordedAt.Location() != time.UTC || x.RecordedAt.Nanosecond()%1000 != 0 {
			t.Errorf("reading %d: recorded_at must be UTC at microsecond precision, got %s", i, x.RecordedAt)
		}
		if x.Source != s.Source {
			t.Errorf("reading %d: source %q", i, x.Source)
		}
		stream := x.PatientReference + "|" + string(x.Type)
		if prev, ok := last[stream]; ok && !x.RecordedAt.After(prev) {
			t.Errorf("reading %d: %s is not strictly increasing (%s after %s)", i, stream, x.RecordedAt, prev)
		}
		last[stream] = x.RecordedAt
		k := stream + "|" + x.RecordedAt.String() + "|" + x.Source
		if seen[k] {
			t.Errorf("reading %d duplicates another (same patient, type, recorded_at, source)", i)
		}
		seen[k] = true
		if x.Metadata != nil && !json.Valid(x.Metadata) {
			t.Errorf("reading %d: metadata is not JSON: %s", i, x.Metadata)
		}
	}
}

func TestVolumeIsExactWithoutAnomalies(t *testing.T) {
	s := small(3)
	for code, st := range s.Streams {
		st.Anomalies.Rate = 0
		s.Streams[code] = st
	}
	_, c := generate(t, s)
	if c.Measurements != s.Measurements() || c.Anomalous != 0 || c.Gaps != 0 {
		t.Errorf("counts = %+v, want exactly %d readings and no anomalies", c, s.Measurements())
	}
}

func TestACleanSignalIsFlat(t *testing.T) {
	s := small(11)
	st := s.Streams[domain.HeartRate]
	st.Noise, st.Circadian, st.Anomalies.Rate = 0, 0, 0
	s.Streams = map[domain.MeasurementType]StreamSpec{domain.HeartRate: st}
	m, _ := generate(t, s)
	byPatient := map[string]map[float64]bool{}
	for _, x := range m.measurements {
		if byPatient[x.PatientReference] == nil {
			byPatient[x.PatientReference] = map[float64]bool{}
		}
		byPatient[x.PatientReference][x.Value] = true
	}
	for ref, values := range byPatient {
		if len(values) != 1 {
			t.Errorf("patient %s: a signal with no noise, rhythm or anomalies has %d distinct values, want 1", ref, len(values))
		}
	}
	if len(byPatient) != 2 {
		t.Errorf("%d patients, want 2", len(byPatient))
	}
}

func TestNoiseAndCircadianRhythmMoveTheValues(t *testing.T) {
	s := small(11)
	st := s.Streams[domain.HeartRate]
	st.Anomalies.Rate = 0
	s.Time.To = testFrom.Add(24 * time.Hour)
	s.Time.Interval = Duration{time.Hour}

	st.Noise, st.Circadian = 0, 10
	s.Streams = map[domain.MeasurementType]StreamSpec{domain.HeartRate: st}
	rhythm, _ := generate(t, s)
	var midnight, noon float64
	for _, x := range rhythm.measurements {
		if x.PatientReference != rhythm.patients[0].ExternalReference {
			continue
		}
		switch x.RecordedAt.Hour() {
		case 0:
			midnight = x.Value
		case 12:
			noon = x.Value
		}
	}
	if noon-midnight < 15 {
		t.Errorf("circadian amplitude 10 should lift noon over midnight by about 20, got %v - %v", noon, midnight)
	}

	st.Noise, st.Circadian = 5, 0
	s.Streams = map[domain.MeasurementType]StreamSpec{domain.HeartRate: st}
	noisy, _ := generate(t, s)
	distinct := map[float64]bool{}
	for _, x := range noisy.measurements {
		distinct[x.Value] = true
	}
	if len(distinct) < 10 {
		t.Errorf("noise 5 over 48 readings gave only %d distinct values", len(distinct))
	}
}

func TestAnomaliesArePlantedAndMarked(t *testing.T) {
	s := small(21)
	s.Time.To = testFrom.Add(24 * time.Hour)
	s.Time.Interval = Duration{time.Minute}
	st := s.Streams[domain.HeartRate]
	st.Noise, st.Circadian = 0, 0
	st.Anomalies = Anomalies{Rate: 0.01, Kinds: []AnomalyKind{AnomalySpike}, Magnitude: Normal{60, 0}, Duration: Normal{3, 0}}
	s.Streams = map[domain.MeasurementType]StreamSpec{domain.HeartRate: st}
	m, c := generate(t, s)

	if c.Anomalous == 0 || c.Anomalous%3 != 0 {
		t.Fatalf("anomalous = %d, want a positive multiple of the episode length 3", c.Anomalous)
	}
	marked := 0
	for _, x := range m.measurements {
		if x.Metadata == nil {
			continue
		}
		marked++
		var meta struct {
			Anomaly AnomalyKind `json:"anomaly"`
			Episode int         `json:"episode"`
		}
		if err := json.Unmarshal(x.Metadata, &meta); err != nil || meta.Anomaly != AnomalySpike || meta.Episode < 1 {
			t.Errorf("metadata %s does not name the episode", x.Metadata)
		}
	}
	if marked != c.Anomalous {
		t.Errorf("%d readings marked, counts say %d", marked, c.Anomalous)
	}
	// With no noise, a spiked reading sits exactly 60 above the baseline.
	var baseline, spiked float64
	for _, x := range m.measurements {
		if x.PatientReference != m.patients[0].ExternalReference {
			continue
		}
		if x.Metadata == nil && baseline == 0 {
			baseline = x.Value
		}
		if x.Metadata != nil && spiked == 0 {
			spiked = x.Value
		}
	}
	if spiked-baseline != 60 {
		t.Errorf("spike lifted %v to %v, want +60", baseline, spiked)
	}
}

func TestEachAnomalyKindHasItsShape(t *testing.T) {
	series := func(kind AnomalyKind) ([]float64, Counts) {
		s := small(8)
		s.Patients.Count = 1
		s.Time.To = testFrom.Add(10 * time.Minute)
		s.Time.Interval = Duration{time.Minute}
		s.Time.Jitter = 0
		st := s.Streams[domain.HeartRate]
		st.Baseline, st.Noise, st.Circadian = Normal{70, 0}, 0, 0
		st.Anomalies = Anomalies{Rate: 1, Kinds: []AnomalyKind{kind}, Magnitude: Normal{40, 0}, Duration: Normal{4, 0}}
		s.Streams = map[domain.MeasurementType]StreamSpec{domain.HeartRate: st}
		m, c := generate(t, s)
		values := make([]float64, 0, len(m.measurements))
		for _, x := range m.measurements {
			values = append(values, x.Value)
		}
		return values, c
	}
	if v, _ := series(AnomalySpike); v[0] != 110 || v[3] != 110 {
		t.Errorf("spike: %v", v)
	}
	if v, _ := series(AnomalyDip); v[0] != 30 || v[3] != 30 {
		t.Errorf("dip: %v", v)
	}
	if v, _ := series(AnomalyDrift); v[0] != 80 || v[1] != 90 || v[2] != 100 || v[3] != 110 {
		t.Errorf("drift should ramp 80, 90, 100, 110: %v", v)
	}
	if v, c := series(AnomalyGap); c.Gaps == 0 || len(v)+c.Gaps != 10 || c.Anomalous != 0 {
		t.Errorf("gap should drop samples: %d readings, %d gaps, %d anomalous", len(v), c.Gaps, c.Anomalous)
	}
}

func TestUsersAreSyntheticAndTheirPasswordsDerived(t *testing.T) {
	s := small(2)
	s.Users = UsersSpec{Admins: 1, Operators: 2, Users: 3, Domain: "synthetic.invalid"}
	users := Users(s)
	if len(users) != 6 {
		t.Fatalf("%d users, want 6", len(users))
	}
	roles := map[domain.Role]int{}
	for _, u := range users {
		roles[u.Role]++
		if !strings.HasSuffix(u.Email, "@synthetic.invalid") || !validate.IsEmail(u.Email) {
			t.Errorf("email %q is not a valid address under the reserved .invalid domain", u.Email)
		}
		p := Password(s.Seed, u.Email)
		if len(p) < 12 || p != Password(s.Seed, strings.ToUpper(u.Email)) || p == Password(s.Seed+1, u.Email) {
			t.Errorf("password for %s: %q must be long, case-insensitive in the address and seed-dependent", u.Email, p)
		}
	}
	if roles[domain.RoleAdmin] != 1 || roles[domain.RoleOperator] != 2 || roles[domain.RoleUser] != 3 {
		t.Errorf("roles = %v", roles)
	}
	tag := strings.TrimPrefix(ReferencePrefix(s), "synth-")
	if users[0].Email != "synth-"+tag+"-admin-1@synthetic.invalid" || users[1].Email != "synth-"+tag+"-operator-1@synthetic.invalid" {
		t.Errorf("addresses are not the documented ones: %v", users[:2])
	}
	other := s
	other.Seed++
	if Users(other)[0].Email == users[0].Email {
		t.Error("two seeds share an account address")
	}
	if Password(1, "a@b.invalid") == Password(1, "c@b.invalid") {
		t.Error("two accounts share a password")
	}
}

func TestPatientsAreWithinTheAgeRangeAndUnique(t *testing.T) {
	s := small(4)
	s.Patients.Count = 200
	s.Patients.AgeMin, s.Patients.AgeMax = 30, 40
	s.Patients.SexWeights = map[domain.Sex]float64{domain.SexFemale: 1, domain.SexMale: 1}
	patients := Patients(s)
	refs := map[string]bool{}
	sexes := map[domain.Sex]int{}
	prefix := ReferencePrefix(s)
	for _, p := range patients {
		if refs[p.ExternalReference] || !strings.HasPrefix(p.ExternalReference, prefix+"-") {
			t.Errorf("reference %q is duplicated or not under %s-", p.ExternalReference, prefix)
		}
		refs[p.ExternalReference] = true
		dob, err := time.Parse(time.DateOnly, p.DateOfBirth)
		if err != nil {
			t.Fatalf("date of birth %q: %v", p.DateOfBirth, err)
		}
		age := s.Time.To.Sub(dob).Hours() / 24 / 365.25
		if age < 30 || age > 41.1 {
			t.Errorf("patient %s is %.1f years old, want 30..41", p.ExternalReference, age)
		}
		sexes[p.Sex]++
	}
	if sexes[domain.SexFemale] < 60 || sexes[domain.SexMale] < 60 || sexes[domain.SexOther]+sexes[domain.SexUnknown] != 0 {
		t.Errorf("sexes = %v, want roughly even FEMALE/MALE and nothing else", sexes)
	}
	if got := ReferencePrefix(s); !strings.HasPrefix(got, "synth-") || len(got) != len("synth-")+8 {
		t.Errorf("derived prefix %q", got)
	}
	s.Patients.ReferencePrefix = "trial"
	if Patients(s)[0].ExternalReference != "trial-0001" {
		t.Errorf("explicit prefix ignored: %s", Patients(s)[0].ExternalReference)
	}
}

func TestGenerateStopsWhenTheSinkFails(t *testing.T) {
	boom := fmt.Errorf("disk full")
	_, err := Generate(small(1), failing{boom})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Generate = %v, want the sink's error", err)
	}
	if _, err := Generate(Spec{}, &memory{}); err == nil {
		t.Error("an invalid spec must be refused")
	}
}

type failing struct{ err error }

func (f failing) User(User) error               { return nil }
func (f failing) Patient(Patient) error         { return nil }
func (f failing) Measurement(Measurement) error { return f.err }
