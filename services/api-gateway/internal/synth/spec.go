// Package synth generates synthetic e-health data (SPECIFICATIONS.md
// section 106) and loads it into a running gateway (section 107).
//
// Everything it produces is invented from a seed: accounts, patients and
// readings. No real person is described, and the same seed and spec give
// byte-identical output on every machine, so a fixture can be regenerated
// rather than stored and a bug report can name a seed instead of a file.
//
// The package has three parts. A [Spec] says what to generate and with which
// distributions; [Generate] turns a spec into users, patients and
// measurement streams and hands them to a [Sink]; [Loader] pushes a written
// fixture through the public API of a gateway that has first been checked
// (see [CheckTarget]) not to be production.
package synth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
)

// Version names the generator's output format. It is recorded in every
// manifest and bumps when a change would alter what a given spec produces,
// so that two fixtures can only be compared when they were made the same way.
const Version = "1"

// Spec is the complete description of a synthetic data set. It is loaded
// from JSON (see [LoadSpec]) or built from [DefaultSpec] and adjusted.
type Spec struct {
	// Seed drives every random choice. Two runs with the same seed and the
	// same spec produce the same output.
	Seed int64 `json:"seed"`

	Users    UsersSpec    `json:"users"`
	Patients PatientsSpec `json:"patients"`
	Time     TimeSpec     `json:"time"`

	// Source is the `source` field of every reading. The generator marks
	// its output so that it can be told from anything else in a database.
	Source string `json:"source"`

	// Streams describes one measurement stream per type, keyed by the
	// type's code. Only the types listed are generated.
	Streams map[domain.MeasurementType]StreamSpec `json:"streams"`
}

// UsersSpec says how many synthetic accounts to invent, per role.
type UsersSpec struct {
	Admins    int `json:"admins"`
	Operators int `json:"operators"`
	Users     int `json:"users"`
	// Domain is the mail domain of every account. The default is under the
	// reserved `.invalid` top-level domain (RFC 2606), which can never
	// resolve, so a synthetic address can never reach a real mailbox.
	Domain string `json:"domain"`
}

// Total is the number of accounts a spec asks for.
func (u UsersSpec) Total() int { return u.Admins + u.Operators + u.Users }

// PatientsSpec says how many synthetic patients to invent and how their
// demographics are distributed.
type PatientsSpec struct {
	Count int `json:"count"`
	// ReferencePrefix starts every external reference. Empty means a
	// prefix derived from the seed, so that fixtures made from different
	// seeds can be loaded into one database without colliding.
	ReferencePrefix string `json:"reference_prefix"`
	// AgeMin and AgeMax bound the age, in whole years at the end of the
	// time range, drawn uniformly.
	AgeMin int `json:"age_min"`
	AgeMax int `json:"age_max"`
	// SexWeights are relative weights for the four accepted values; a
	// value that is absent has weight zero.
	SexWeights map[domain.Sex]float64 `json:"sex_weights"`
}

// TimeSpec bounds the streams in time and sets their cadence.
type TimeSpec struct {
	// From and To bound every stream, half-open: the first sample is at
	// From and no sample is at or after To. Both are required and are
	// recorded in the manifest, so a fixture is reproducible from it.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Interval is the nominal spacing between samples of one stream.
	Interval Duration `json:"interval"`
	// Jitter is the fraction of the interval, in [0, 1), by which each
	// sample may be late; 0 gives a perfectly regular series. Samples stay
	// strictly increasing whatever the value.
	Jitter float64 `json:"jitter"`
}

// Samples is the number of samples a stream nominally holds.
func (t TimeSpec) Samples() int {
	if t.Interval.Duration <= 0 || !t.To.After(t.From) {
		return 0
	}
	return int(math.Ceil(float64(t.To.Sub(t.From)) / float64(t.Interval.Duration)))
}

// StreamSpec describes how the values of one measurement type behave.
//
// A patient's readings are a per-patient baseline (drawn once from
// Baseline), plus a daily rhythm (a sine of amplitude Circadian, lowest at
// midnight and highest at noon), plus per-sample noise (normal, standard
// deviation Noise), plus anomaly episodes. The result is rounded to
// Decimals and clamped to the type's catalogue range, so every reading is
// one the API accepts.
type StreamSpec struct {
	Baseline  Normal    `json:"baseline"`
	Circadian float64   `json:"circadian"`
	Noise     float64   `json:"noise"`
	Decimals  int       `json:"decimals"`
	Anomalies Anomalies `json:"anomalies"`
}

// Normal is a normal distribution.
type Normal struct {
	Mean float64 `json:"mean"`
	SD   float64 `json:"sd"`
}

// Anomalies configures the anomaly episodes of a stream.
type Anomalies struct {
	// Rate is the probability, per sample outside an episode, that an
	// episode begins at that sample. 0 turns anomalies off.
	Rate float64 `json:"rate"`
	// Kinds are the episode kinds to draw from, uniformly.
	Kinds []AnomalyKind `json:"kinds"`
	// Magnitude is the size of an episode in the stream's unit; its
	// absolute value is used.
	Magnitude Normal `json:"magnitude"`
	// Duration is the length of an episode in samples, at least 1.
	Duration Normal `json:"duration"`
}

// AnomalyKind is the shape of an anomaly episode.
type AnomalyKind string

const (
	// AnomalySpike lifts every sample of the episode by the magnitude.
	AnomalySpike AnomalyKind = "spike"
	// AnomalyDip lowers every sample of the episode by the magnitude.
	AnomalyDip AnomalyKind = "dip"
	// AnomalyDrift ramps linearly from no offset to the magnitude over the
	// episode, the way a failing sensor or a slow change looks.
	AnomalyDrift AnomalyKind = "drift"
	// AnomalyGap emits no samples for the episode: a device that stopped
	// reporting.
	AnomalyGap AnomalyKind = "gap"
)

var anomalyKinds = map[AnomalyKind]bool{AnomalySpike: true, AnomalyDip: true, AnomalyDrift: true, AnomalyGap: true}

// Duration is a time.Duration that reads and writes as a string such as
// "30s" or "5m" in JSON.
type Duration struct{ time.Duration }

// MarshalJSON writes the duration in its string form.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"1m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// DefaultSpec is a small, plausible data set: a handful of accounts, ten
// patients, every measurement type at one-minute cadence over the given
// range, with adult resting-value distributions and rare anomalies. It is
// the starting point for `synth spec`, and what the flags of `synth
// generate` adjust.
//
// The ranges below are physiological plausibility for a resting adult, not
// medical thresholds; they exist so that the data looks like data. Nothing
// here describes any person.
func DefaultSpec(seed int64, from, to time.Time) Spec {
	return Spec{
		Seed: seed,
		Users: UsersSpec{
			Admins: 1, Operators: 2, Users: 2,
			Domain: "synthetic.invalid",
		},
		Patients: PatientsSpec{
			Count:  10,
			AgeMin: 18, AgeMax: 90,
			SexWeights: map[domain.Sex]float64{
				domain.SexFemale: 0.49, domain.SexMale: 0.49, domain.SexOther: 0.01, domain.SexUnknown: 0.01,
			},
		},
		Time: TimeSpec{
			From: from, To: to,
			Interval: Duration{time.Minute},
			Jitter:   0.1,
		},
		Source: "synthetic-monitor",
		Streams: map[domain.MeasurementType]StreamSpec{
			domain.HeartRate: {
				Baseline: Normal{72, 8}, Circadian: 4, Noise: 3, Decimals: 0,
				Anomalies: Anomalies{Rate: 0.002, Kinds: []AnomalyKind{AnomalySpike, AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{60, 10}, Duration: Normal{5, 2}},
			},
			domain.BloodPressureSystolic: {
				Baseline: Normal{118, 10}, Circadian: 4, Noise: 5, Decimals: 0,
				Anomalies: Anomalies{Rate: 0.002, Kinds: []AnomalyKind{AnomalySpike, AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{45, 8}, Duration: Normal{5, 2}},
			},
			domain.BloodPressureDiastolic: {
				Baseline: Normal{76, 7}, Circadian: 2, Noise: 4, Decimals: 0,
				Anomalies: Anomalies{Rate: 0.002, Kinds: []AnomalyKind{AnomalySpike, AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{30, 6}, Duration: Normal{5, 2}},
			},
			domain.SpO2: {
				Baseline: Normal{97, 1}, Circadian: 0.3, Noise: 0.7, Decimals: 0,
				Anomalies: Anomalies{Rate: 0.002, Kinds: []AnomalyKind{AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{10, 3}, Duration: Normal{6, 2}},
			},
			domain.BodyTemperature: {
				Baseline: Normal{36.8, 0.2}, Circadian: 0.3, Noise: 0.1, Decimals: 1,
				Anomalies: Anomalies{Rate: 0.001, Kinds: []AnomalyKind{AnomalySpike, AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{2.5, 0.5}, Duration: Normal{30, 10}},
			},
			domain.BloodGlucose: {
				Baseline: Normal{95, 10}, Circadian: 10, Noise: 8, Decimals: 0,
				Anomalies: Anomalies{Rate: 0.002, Kinds: []AnomalyKind{AnomalySpike, AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{150, 30}, Duration: Normal{10, 3}},
			},
			domain.RespiratoryRate: {
				Baseline: Normal{15, 2}, Circadian: 1, Noise: 1, Decimals: 0,
				Anomalies: Anomalies{Rate: 0.002, Kinds: []AnomalyKind{AnomalySpike, AnomalyDip, AnomalyDrift, AnomalyGap},
					Magnitude: Normal{12, 3}, Duration: Normal{5, 2}},
			},
		},
	}
}

// LoadSpec reads a spec from a JSON file. Unknown fields are an error, so a
// misspelt key cannot be silently ignored.
func LoadSpec(path string) (Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("read spec: %w", err)
	}
	return ParseSpec(raw)
}

// ParseSpec decodes a spec from JSON. See [LoadSpec].
func ParseSpec(raw []byte) (Spec, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("parse spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Types lists the stream types in catalogue order, so that output order
// does not depend on map iteration.
func (s Spec) Types() []domain.MeasurementType {
	types := make([]domain.MeasurementType, 0, len(s.Streams))
	for code := range s.Streams {
		types = append(types, code)
	}
	index := map[domain.MeasurementType]int{}
	for i, t := range measurement.Catalog {
		index[t.Code] = i
	}
	sort.Slice(types, func(i, j int) bool {
		a, aok := index[types[i]]
		b, bok := index[types[j]]
		if aok != bok {
			return aok
		}
		if a != b {
			return a < b
		}
		return types[i] < types[j]
	})
	return types
}

// Measurements is the number of readings the spec produces before anomaly
// gaps remove any: patients × streams × samples.
func (s Spec) Measurements() int {
	return s.Patients.Count * len(s.Streams) * s.Time.Samples()
}

// Validate checks that the spec can be generated and that everything it
// produces would be accepted by the API.
func (s Spec) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if s.Users.Admins < 0 || s.Users.Operators < 0 || s.Users.Users < 0 {
		fail("users: counts must not be negative")
	}
	if s.Users.Total() > 0 && !strings.Contains(s.Users.Domain, ".") {
		fail("users.domain: %q must be a domain name", s.Users.Domain)
	}
	if strings.ContainsAny(s.Users.Domain, "@ \t\r\n") {
		fail("users.domain: must not contain @ or white space")
	}

	if s.Patients.Count < 0 {
		fail("patients.count: must not be negative")
	}
	if s.Patients.AgeMin < 0 || s.Patients.AgeMax > 120 || s.Patients.AgeMin > s.Patients.AgeMax {
		fail("patients: ages must satisfy 0 <= age_min <= age_max <= 120")
	}
	if len(s.Patients.ReferencePrefix) > 64 || strings.ContainsAny(s.Patients.ReferencePrefix, " \t\r\n") {
		fail("patients.reference_prefix: at most 64 characters, no white space")
	}
	total := 0.0
	for sex, w := range s.Patients.SexWeights {
		switch sex {
		case domain.SexFemale, domain.SexMale, domain.SexOther, domain.SexUnknown:
		default:
			fail("patients.sex_weights: unknown sex %q", sex)
		}
		if w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			fail("patients.sex_weights[%s]: must be a finite, non-negative weight", sex)
		}
		total += w
	}
	if s.Patients.Count > 0 && total <= 0 {
		fail("patients.sex_weights: at least one positive weight is required")
	}

	if s.Time.From.IsZero() || s.Time.To.IsZero() {
		fail("time: from and to are required")
	} else if !s.Time.To.After(s.Time.From) {
		fail("time: to must be after from")
	}
	if s.Time.From.Before(measurement.EarliestRecordedAt) {
		fail("time.from: must not be before %s", measurement.EarliestRecordedAt.Format(time.RFC3339))
	}
	if s.Time.Interval.Duration < time.Second {
		fail("time.interval: must be at least 1s (readings are stored to the microsecond, and a second keeps jittered samples apart)")
	}
	if s.Time.Jitter < 0 || s.Time.Jitter >= 1 || math.IsNaN(s.Time.Jitter) {
		fail("time.jitter: must be in [0, 1)")
	}

	src := strings.TrimSpace(s.Source)
	if src == "" || len(src) > measurement.MaxSourceLength || src != s.Source {
		fail("source: required, 1-%d characters, no surrounding white space", measurement.MaxSourceLength)
	}

	if len(s.Streams) == 0 {
		fail("streams: at least one measurement type is required")
	}
	for code, st := range s.Streams {
		if _, ok := measurement.Lookup(code); !ok {
			fail("streams[%s]: unknown measurement type (known: %s)", code, strings.Join(measurement.TypeCodes(), ", "))
			continue
		}
		prefix := fmt.Sprintf("streams[%s]", code)
		for name, v := range map[string]float64{
			"baseline.mean": st.Baseline.Mean, "baseline.sd": st.Baseline.SD, "circadian": st.Circadian, "noise": st.Noise,
			"anomalies.rate": st.Anomalies.Rate, "anomalies.magnitude.mean": st.Anomalies.Magnitude.Mean,
			"anomalies.magnitude.sd": st.Anomalies.Magnitude.SD, "anomalies.duration.mean": st.Anomalies.Duration.Mean,
			"anomalies.duration.sd": st.Anomalies.Duration.SD,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				fail("%s.%s: must be a finite number", prefix, name)
			}
		}
		if st.Baseline.SD < 0 || st.Noise < 0 || st.Circadian < 0 {
			fail("%s: baseline.sd, noise and circadian must not be negative", prefix)
		}
		if st.Decimals < 0 || st.Decimals > 6 {
			fail("%s.decimals: must be between 0 and 6", prefix)
		}
		a := st.Anomalies
		if a.Rate < 0 || a.Rate > 1 {
			fail("%s.anomalies.rate: must be in [0, 1]", prefix)
		}
		if a.Rate > 0 && len(a.Kinds) == 0 {
			fail("%s.anomalies.kinds: required when rate > 0", prefix)
		}
		for _, k := range a.Kinds {
			if !anomalyKinds[k] {
				fail("%s.anomalies.kinds: unknown kind %q (spike, dip, drift, gap)", prefix, k)
			}
		}
		if a.Magnitude.SD < 0 || a.Duration.SD < 0 {
			fail("%s.anomalies: magnitude.sd and duration.sd must not be negative", prefix)
		}
	}
	return errors.Join(errs...)
}
