package synth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
)

// User is a synthetic account. The password is not part of the record: it
// is derived from the seed and the address by [Password], so a fixture on
// disk never holds a credential.
type User struct {
	Email string      `json:"email"`
	Role  domain.Role `json:"role"`
}

// Patient is a synthetic patient in the shape `POST /api/v1/patients`
// accepts.
type Patient struct {
	ExternalReference string     `json:"external_reference"`
	DateOfBirth       string     `json:"date_of_birth"`
	Sex               domain.Sex `json:"sex"`
}

// Measurement is one synthetic reading in the shape a batch item takes,
// except that it names the patient by external reference: the patient's id
// is assigned by the server, so the loader fills it in.
type Measurement struct {
	PatientReference string                 `json:"patient_reference"`
	Type             domain.MeasurementType `json:"type"`
	Value            float64                `json:"value"`
	Unit             string                 `json:"unit"`
	RecordedAt       time.Time              `json:"recorded_at"`
	Source           string                 `json:"source"`
	// Metadata marks anomalous samples ({"anomaly":"spike","episode":3}) so
	// that a consumer can check what a detector found against what was
	// planted. Normal samples carry none.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Sink receives what [Generate] produces, in a fixed order: every user,
// then every patient, then the measurements patient by patient and stream
// by stream, each stream in time order. A sink that returns an error stops
// the generation.
type Sink interface {
	User(User) error
	Patient(Patient) error
	Measurement(Measurement) error
}

// Counts is what a generation produced.
type Counts struct {
	Users        int `json:"users"`
	Patients     int `json:"patients"`
	Measurements int `json:"measurements"`
	// Anomalous is the number of readings inside an anomaly episode, gaps
	// excluded since they emit nothing.
	Anomalous int `json:"anomalous"`
	// Gaps is the number of samples an anomaly gap removed.
	Gaps int `json:"gaps"`
}

// Generate produces the data set described by spec and hands it to sink.
// It is deterministic: the same spec produces the same calls in the same
// order. Every random choice for a patient's stream comes from a generator
// seeded from the spec's seed, the patient's index and the type, so
// changing the number of patients or the set of types does not change the
// data of the patients and types that stay.
func Generate(spec Spec, sink Sink) (Counts, error) {
	if err := spec.Validate(); err != nil {
		return Counts{}, err
	}
	var counts Counts

	for _, u := range Users(spec) {
		if err := sink.User(u); err != nil {
			return counts, err
		}
		counts.Users++
	}

	patients := Patients(spec)
	for _, p := range patients {
		if err := sink.Patient(p); err != nil {
			return counts, err
		}
		counts.Patients++
	}

	types := spec.Types()
	for i, p := range patients {
		for _, code := range types {
			c, err := stream(spec, i, p, code, sink)
			counts.Measurements += c.Measurements
			counts.Anomalous += c.Anomalous
			counts.Gaps += c.Gaps
			if err != nil {
				return counts, err
			}
		}
	}
	return counts, nil
}

// Users invents the accounts of a spec: `synth-<seed tag>-admin-1@…`,
// `synth-<seed tag>-operator-1@…` and so on, admins first. The tag is the
// same eight hex digits that start the patient references, so fixtures made
// from different seeds create different accounts and each one signs in
// with its own derived password; without it a second seed would meet the
// first seed's accounts and be refused by them.
func Users(spec Spec) []User {
	users := make([]User, 0, spec.Users.Total())
	tag := seedTag(spec.Seed)
	add := func(n int, role domain.Role) {
		for i := 1; i <= n; i++ {
			users = append(users, User{
				Email: fmt.Sprintf("synth-%s-%s-%d@%s", tag, strings.ToLower(string(role)), i, spec.Users.Domain),
				Role:  role,
			})
		}
	}
	add(spec.Users.Admins, domain.RoleAdmin)
	add(spec.Users.Operators, domain.RoleOperator)
	add(spec.Users.Users, domain.RoleUser)
	return users
}

// Password derives the password of a synthetic account from the seed and
// the address. It is 30 characters from a keyed hash, so it meets the
// password rules, and it can be recomputed by anyone holding the fixture,
// which is the point: these accounts exist for local and staging
// environments and are never a secret. The loader refuses production.
func Password(seed int64, email string) string {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], uint64(seed))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("vitalmesh-synth-password\n"))
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(email))))
	sum := mac.Sum(nil)
	return "synth-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:15]))
}

// ReferencePrefix is the external-reference prefix a spec uses: its own,
// or `synth-<seed tag>`.
func ReferencePrefix(spec Spec) string {
	if spec.Patients.ReferencePrefix != "" {
		return spec.Patients.ReferencePrefix
	}
	return "synth-" + seedTag(spec.Seed)
}

// seedTag is eight hex digits that identify a seed in names: it is not the
// seed itself, so a name does not reveal what to regenerate the data from,
// but it is the same for the same seed everywhere.
func seedTag(seed int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("vitalmesh-synth-prefix:%d", seed)))
	return fmt.Sprintf("%x", sum[:4])
}

// Patients invents the patients of a spec. Ages are uniform between the
// bounds, at the end of the time range; sexes follow the weights.
func Patients(spec Spec) []Patient {
	prefix := ReferencePrefix(spec)
	sexes, weights := sexTable(spec.Patients.SexWeights)
	asOf := spec.Time.To.UTC()
	patients := make([]Patient, 0, spec.Patients.Count)
	for i := 0; i < spec.Patients.Count; i++ {
		rng := newRNG(spec.Seed, "patient", i, "")
		years := spec.Patients.AgeMin
		if spec.Patients.AgeMax > spec.Patients.AgeMin {
			years += rng.IntN(spec.Patients.AgeMax - spec.Patients.AgeMin + 1)
		}
		// A birthday somewhere in the year before the age would change.
		dob := asOf.AddDate(-years, 0, 0).AddDate(0, 0, -rng.IntN(365))
		if dob.Before(time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)) {
			dob = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		patients = append(patients, Patient{
			ExternalReference: fmt.Sprintf("%s-%04d", prefix, i+1),
			DateOfBirth:       dob.Format(time.DateOnly),
			Sex:               sexes[pick(rng, weights)],
		})
	}
	return patients
}

// sexTable turns the weight map into parallel slices in a fixed order.
func sexTable(w map[domain.Sex]float64) ([]domain.Sex, []float64) {
	sexes := make([]domain.Sex, 0, len(w))
	for s := range w {
		sexes = append(sexes, s)
	}
	sort.Slice(sexes, func(i, j int) bool { return sexes[i] < sexes[j] })
	weights := make([]float64, len(sexes))
	for i, s := range sexes {
		weights[i] = w[s]
	}
	return sexes, weights
}

// pick draws an index with probability proportional to its weight.
func pick(rng *rand.Rand, weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	x := rng.Float64() * total
	for i, w := range weights {
		if x < w {
			return i
		}
		x -= w
	}
	return len(weights) - 1
}

// newRNG returns a generator whose state depends only on the seed and the
// named entity, so that every stream is independent of every other and of
// the order in which they are produced.
func newRNG(seed int64, kind string, index int, code string) *rand.Rand {
	h := sha256.New()
	fmt.Fprintf(h, "vitalmesh-synth:%d:%s:%d:%s", seed, kind, index, code)
	sum := h.Sum(nil)
	return rand.New(rand.NewPCG(binary.BigEndian.Uint64(sum[:8]), binary.BigEndian.Uint64(sum[8:16])))
}

// episode is an anomaly in progress.
type episode struct {
	kind      AnomalyKind
	magnitude float64
	length    int
	index     int
	number    int
}

// stream generates one patient's readings of one type.
func stream(spec Spec, patientIndex int, p Patient, code domain.MeasurementType, sink Sink) (Counts, error) {
	var counts Counts
	st := spec.Streams[code]
	cat, _ := measurement.Lookup(code)
	rng := newRNG(spec.Seed, "stream", patientIndex, string(code))

	baseline := st.Baseline.Mean + st.Baseline.SD*rng.NormFloat64()
	interval := spec.Time.Interval.Duration
	samples := spec.Time.Samples()
	scale := math.Pow(10, float64(st.Decimals))

	var ep *episode
	episodes := 0
	for i := 0; i < samples; i++ {
		at := spec.Time.From.Add(time.Duration(i) * interval)
		if spec.Time.Jitter > 0 {
			at = at.Add(time.Duration(rng.Float64() * spec.Time.Jitter * float64(interval)))
		}
		at = at.UTC().Truncate(time.Microsecond)
		if !at.Before(spec.Time.To) {
			break
		}

		// Daily rhythm: lowest at midnight, highest at noon.
		hour := float64(at.Hour()) + float64(at.Minute())/60
		value := baseline + st.Circadian*math.Sin(2*math.Pi*(hour-6)/24) + st.Noise*rng.NormFloat64()

		if ep == nil && st.Anomalies.Rate > 0 && rng.Float64() < st.Anomalies.Rate {
			a := st.Anomalies
			episodes++
			ep = &episode{
				kind:      a.Kinds[rng.IntN(len(a.Kinds))],
				magnitude: math.Abs(a.Magnitude.Mean + a.Magnitude.SD*rng.NormFloat64()),
				length:    max(1, int(math.Round(a.Duration.Mean+a.Duration.SD*rng.NormFloat64()))),
				number:    episodes,
			}
		}

		var metadata json.RawMessage
		if ep != nil {
			switch ep.kind {
			case AnomalySpike:
				value += ep.magnitude
			case AnomalyDip:
				value -= ep.magnitude
			case AnomalyDrift:
				value += ep.magnitude * float64(ep.index+1) / float64(ep.length)
			case AnomalyGap:
				counts.Gaps++
			}
			skip := ep.kind == AnomalyGap
			if !skip {
				metadata = json.RawMessage(fmt.Sprintf(`{"anomaly":%q,"episode":%d}`, ep.kind, ep.number))
				counts.Anomalous++
			}
			ep.index++
			if ep.index >= ep.length {
				ep = nil
			}
			if skip {
				continue
			}
		}

		value = math.Round(value*scale) / scale
		value = math.Min(math.Max(value, cat.Min), cat.Max)

		if err := sink.Measurement(Measurement{
			PatientReference: p.ExternalReference,
			Type:             code,
			Value:            value,
			Unit:             cat.Unit,
			RecordedAt:       at,
			Source:           spec.Source,
			Metadata:         metadata,
		}); err != nil {
			return counts, err
		}
		counts.Measurements++
	}
	return counts, nil
}
