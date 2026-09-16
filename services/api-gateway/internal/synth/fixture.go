package synth

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// A fixture is a directory of four files:
//
//	manifest.json        the spec, the counts, the generator version and
//	                     the digest of each data file
//	users.ndjson         one User per line
//	patients.ndjson      one Patient per line
//	measurements.ndjson  one Measurement per line, in generation order
//
// NDJSON streams: a fixture of millions of readings is written and read a
// line at a time and never held in memory.
const (
	ManifestFile     = "manifest.json"
	UsersFile        = "users.ndjson"
	PatientsFile     = "patients.ndjson"
	MeasurementsFile = "measurements.ndjson"
)

// Manifest describes a written fixture.
type Manifest struct {
	// Generator is [Version], the output format the fixture was made with.
	Generator string `json:"generator"`
	// GeneratedAt is when the fixture was written. It is informational and
	// is the one field that differs between two runs of the same spec.
	GeneratedAt time.Time `json:"generated_at"`
	Spec        Spec      `json:"spec"`
	Counts      Counts    `json:"counts"`
	// Digests holds the SHA-256 of each data file, so a fixture can be
	// checked for tampering or truncation before it is loaded.
	Digests map[string]string `json:"digests"`
}

// Write generates spec into dir as a fixture and returns its manifest. The
// directory is created; if it already holds a fixture the call fails unless
// overwrite is set, so that a fixture is not replaced by accident.
func Write(spec Spec, dir string, overwrite bool) (Manifest, error) {
	if err := spec.Validate(); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Manifest{}, fmt.Errorf("create %s: %w", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil && !overwrite {
		return Manifest{}, fmt.Errorf("%s already holds a fixture; pass --overwrite to replace it", dir)
	}

	w := &fileSink{dir: dir, digests: map[string]string{}}
	if err := w.open(); err != nil {
		return Manifest{}, err
	}
	counts, err := Generate(spec, w)
	if cerr := w.close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Manifest{}, err
	}

	m := Manifest{
		Generator:   Version,
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
		Spec:        spec,
		Counts:      counts,
		Digests:     w.digests,
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), append(raw, '\n'), 0o644); err != nil {
		return Manifest{}, fmt.Errorf("write manifest: %w", err)
	}
	return m, nil
}

// fileSink streams each entity kind to its file, digesting as it goes.
type fileSink struct {
	dir     string
	files   map[string]*os.File
	writers map[string]*bufio.Writer
	hashes  map[string]hashWriter
	digests map[string]string
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func (w *fileSink) open() error {
	w.files = map[string]*os.File{}
	w.writers = map[string]*bufio.Writer{}
	w.hashes = map[string]hashWriter{}
	for _, name := range []string{UsersFile, PatientsFile, MeasurementsFile} {
		f, err := os.Create(filepath.Join(w.dir, name))
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		w.files[name] = f
		w.hashes[name] = sha256.New()
		w.writers[name] = bufio.NewWriterSize(io.MultiWriter(f, w.hashes[name]), 1<<20)
	}
	return nil
}

func (w *fileSink) line(name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.writers[name].Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func (w *fileSink) User(u User) error               { return w.line(UsersFile, u) }
func (w *fileSink) Patient(p Patient) error         { return w.line(PatientsFile, p) }
func (w *fileSink) Measurement(m Measurement) error { return w.line(MeasurementsFile, m) }

func (w *fileSink) close() error {
	var errs []error
	for name, bw := range w.writers {
		if err := bw.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("flush %s: %w", name, err))
		}
		if err := w.files[name].Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", name, err))
		}
		w.digests[name] = "sha256:" + hex.EncodeToString(w.hashes[name].Sum(nil))
	}
	return errors.Join(errs...)
}

// ReadManifest reads and checks a fixture's manifest: the generator version
// must be this package's, the spec must be valid, and every data file must
// exist with the recorded digest.
func ReadManifest(dir string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("%s is not a fixture directory: %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Generator != Version {
		return Manifest{}, fmt.Errorf("fixture was made by generator version %q, this tool is version %q: regenerate it", m.Generator, Version)
	}
	if err := m.Spec.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("manifest spec: %w", err)
	}
	for _, name := range []string{UsersFile, PatientsFile, MeasurementsFile} {
		want, ok := m.Digests[name]
		if !ok {
			return Manifest{}, fmt.Errorf("manifest has no digest for %s", name)
		}
		got, err := digestFile(filepath.Join(dir, name))
		if err != nil {
			return Manifest{}, err
		}
		if got != want {
			return Manifest{}, fmt.Errorf("%s does not match its manifest digest (the fixture was edited or truncated): regenerate it", name)
		}
	}
	return m, nil
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ReadUsers reads a fixture's users.
func ReadUsers(dir string) ([]User, error) {
	var users []User
	err := readLines(filepath.Join(dir, UsersFile), func(raw []byte) error {
		var u User
		if err := json.Unmarshal(raw, &u); err != nil {
			return err
		}
		users = append(users, u)
		return nil
	})
	return users, err
}

// ReadPatients reads a fixture's patients.
func ReadPatients(dir string) ([]Patient, error) {
	var patients []Patient
	err := readLines(filepath.Join(dir, PatientsFile), func(raw []byte) error {
		var p Patient
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		patients = append(patients, p)
		return nil
	})
	return patients, err
}

// EachMeasurement streams a fixture's measurements to fn in file order.
func EachMeasurement(dir string, fn func(Measurement) error) error {
	return readLines(filepath.Join(dir, MeasurementsFile), func(raw []byte) error {
		var m Measurement
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		return fn(m)
	})
}

func readLines(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", filepath.Base(path), line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return nil
}
