// Command synth generates synthetic e-health data and loads it into a
// gateway (SPECIFICATIONS.md sections 106 and 107).
//
// Usage:
//
//	synth spec     [flags]              print a spec to edit: the defaults with the flags applied
//	synth generate [flags] --out DIR    write a fixture: users, patients and measurement streams
//	synth load --from DIR --target URL --environment local|staging [--jobs] [--users]
//	                                    store a fixture through the gateway's API
//	synth password --from DIR --email ADDRESS
//	                                    print the password of one synthetic account
//
// A fixture is reproducible: the same seed and spec give the same bytes.
// Everything is invented; no real person is described.
//
// Accounts and patient references carry a tag of the seed
// (synth-<tag>-operator-1@synthetic.invalid, synth-<tag>-0001), so fixtures
// from different seeds never collide in one database.
//
// Loading refuses production unless --allow-production is passed and
// VITALMESH_SYNTH_ALLOW_PRODUCTION names the exact target host; see
// docs/SYNTHETIC_DATA.md. Credentials come from the environment
// (SYNTH_EMAIL, SYNTH_PASSWORD; DATABASE_URL for --users), never from flags.
//
// Exit codes: 0 on success, 1 on a runtime failure, 2 on invalid usage or a
// refused target.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/synth"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr, time.Now))
}

const usage = `usage:
  synth spec     [flags]                print a spec (JSON) to edit and pass back with --spec
  synth generate [flags] --out DIR      write a fixture
  synth load     --from DIR --target URL --environment local|staging|production
                 [--jobs] [--users] [--batch-size N] [--allow-production]
  synth password --from DIR --email ADDRESS
  synth version

Run a subcommand with -h for its flags. docs/SYNTHETIC_DATA.md explains the rest.`

// run is main without the process: tests call it with their own
// environment and streams.
func run(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, now func() time.Time) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "spec":
		return runSpec(args[1:], stdout, stderr, now)
	case "generate":
		return runGenerate(args[1:], stdout, stderr, now)
	case "load":
		return runLoad(ctx, args[1:], lookup, stdout, stderr)
	case "password":
		return runPassword(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "synth %s (fixture format %s)\n", buildinfo.Version, synth.Version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// specFlags are the adjustments generate and spec share. Each one overrides
// the corresponding part of the spec, whether that came from --spec or from
// the defaults.
type specFlags struct {
	spec                   string
	seed                   int64
	admins, operators      int
	users, patients        int
	from, to               string
	days                   float64
	interval               time.Duration
	types                  string
	source                 string
	noiseScale             float64
	anomalyRate            float64
	fs                     *flag.FlagSet
	setFlags               map[string]bool
	referencePrefix        string
	ageMin, ageMax, jitter float64
}

func newSpecFlags(name string, stderr io.Writer) *specFlags {
	f := &specFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError), setFlags: map[string]bool{}}
	f.fs.SetOutput(stderr)
	f.fs.StringVar(&f.spec, "spec", "", "spec file (JSON, see `synth spec`) to start from instead of the defaults")
	f.fs.Int64Var(&f.seed, "seed", 1, "seed of every random choice; the same seed and spec give the same fixture")
	f.fs.IntVar(&f.admins, "admins", -1, "synthetic ADMIN accounts")
	f.fs.IntVar(&f.operators, "operators", -1, "synthetic OPERATOR accounts")
	f.fs.IntVar(&f.users, "users", -1, "synthetic USER accounts")
	f.fs.IntVar(&f.patients, "patients", -1, "synthetic patients")
	f.fs.StringVar(&f.from, "from", "", "start of every stream (RFC 3339); default: --to minus --days")
	f.fs.StringVar(&f.to, "to", "", "end of every stream, exclusive (RFC 3339); default: now, to the minute")
	f.fs.Float64Var(&f.days, "days", 7, "length of the time range when --from is not given")
	f.fs.DurationVar(&f.interval, "interval", 0, "spacing between samples of one stream, at least 1s (default 1m)")
	f.fs.StringVar(&f.types, "types", "", "comma-separated measurement types to generate (default: all seven)")
	f.fs.StringVar(&f.source, "source", "", "the source field of every reading (default synthetic-monitor)")
	f.fs.Float64Var(&f.noiseScale, "noise-scale", 1, "multiply every stream's noise: 0 is a clean signal, 3 is a noisy one")
	f.fs.Float64Var(&f.anomalyRate, "anomaly-rate", -1, "set every stream's anomaly rate (probability per sample that an episode starts); 0 disables anomalies")
	f.fs.StringVar(&f.referencePrefix, "reference-prefix", "", "prefix of patient external references (default: synth-<seed tag>)")
	f.fs.Float64Var(&f.ageMin, "age-min", -1, "youngest patient age in years")
	f.fs.Float64Var(&f.ageMax, "age-max", -1, "oldest patient age in years")
	f.fs.Float64Var(&f.jitter, "jitter", -1, "fraction of the interval by which a sample may be late, in [0, 1)")
	return f
}

func (f *specFlags) parse(args []string) error {
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	f.fs.Visit(func(fl *flag.Flag) { f.setFlags[fl.Name] = true })
	if f.fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", f.fs.Arg(0))
	}
	return nil
}

// build resolves the spec: the file or the defaults, then the flags.
func (f *specFlags) build(now func() time.Time) (synth.Spec, error) {
	to := now().UTC().Truncate(time.Minute)
	if f.to != "" {
		t, err := time.Parse(time.RFC3339, f.to)
		if err != nil {
			return synth.Spec{}, fmt.Errorf("--to: %w", err)
		}
		to = t.UTC()
	}
	from := to.Add(-time.Duration(f.days * float64(24*time.Hour)))
	if f.from != "" {
		t, err := time.Parse(time.RFC3339, f.from)
		if err != nil {
			return synth.Spec{}, fmt.Errorf("--from: %w", err)
		}
		from = t.UTC()
	}

	var spec synth.Spec
	if f.spec != "" {
		loaded, err := synth.LoadSpec(f.spec)
		if err != nil {
			return synth.Spec{}, err
		}
		spec = loaded
		if f.setFlags["from"] || f.setFlags["days"] {
			spec.Time.From = from
		}
		if f.setFlags["to"] {
			spec.Time.To = to
		}
	} else {
		spec = synth.DefaultSpec(f.seed, from, to)
	}
	if f.setFlags["seed"] {
		spec.Seed = f.seed
	}
	if f.admins >= 0 {
		spec.Users.Admins = f.admins
	}
	if f.operators >= 0 {
		spec.Users.Operators = f.operators
	}
	if f.users >= 0 {
		spec.Users.Users = f.users
	}
	if f.patients >= 0 {
		spec.Patients.Count = f.patients
	}
	if f.interval > 0 {
		spec.Time.Interval = synth.Duration{Duration: f.interval}
	}
	if f.jitter >= 0 {
		spec.Time.Jitter = f.jitter
	}
	if f.source != "" {
		spec.Source = f.source
	}
	if f.referencePrefix != "" {
		spec.Patients.ReferencePrefix = f.referencePrefix
	}
	if f.ageMin >= 0 {
		spec.Patients.AgeMin = int(f.ageMin)
	}
	if f.ageMax >= 0 {
		spec.Patients.AgeMax = int(f.ageMax)
	}
	if f.types != "" {
		keep := map[domain.MeasurementType]synth.StreamSpec{}
		defaults := synth.DefaultSpec(spec.Seed, from, to).Streams
		for _, raw := range strings.Split(f.types, ",") {
			code := domain.MeasurementType(strings.ToUpper(strings.TrimSpace(raw)))
			if code == "" {
				continue
			}
			st, ok := spec.Streams[code]
			if !ok {
				st, ok = defaults[code]
			}
			if !ok {
				return synth.Spec{}, fmt.Errorf("--types: unknown measurement type %q", code)
			}
			keep[code] = st
		}
		spec.Streams = keep
	}
	if f.noiseScale != 1 {
		for code, st := range spec.Streams {
			st.Noise *= f.noiseScale
			spec.Streams[code] = st
		}
	}
	if f.anomalyRate >= 0 {
		for code, st := range spec.Streams {
			st.Anomalies.Rate = f.anomalyRate
			spec.Streams[code] = st
		}
	}
	if err := spec.Validate(); err != nil {
		return synth.Spec{}, err
	}
	return spec, nil
}

func runSpec(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	f := newSpecFlags("spec", stderr)
	if err := f.parse(args); err != nil {
		return usageError(err, stderr)
	}
	spec, err := f.build(now)
	if err != nil {
		return usageError(err, stderr)
	}
	raw, err := marshalSpec(spec)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func runGenerate(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	f := newSpecFlags("generate", stderr)
	var out string
	var overwrite bool
	f.fs.StringVar(&out, "out", "", "directory to write the fixture into (required)")
	f.fs.BoolVar(&overwrite, "overwrite", false, "replace a fixture already in --out")
	if err := f.parse(args); err != nil {
		return usageError(err, stderr)
	}
	if out == "" {
		return usageError(errors.New("--out is required"), stderr)
	}
	spec, err := f.build(now)
	if err != nil {
		return usageError(err, stderr)
	}
	if ahead := spec.Time.To.Sub(now().UTC()); ahead > 5*time.Minute {
		fmt.Fprintf(stderr, "note: the range ends %s in the future; the gateway rejects readings more than 5 minutes ahead of its clock, so such a fixture cannot be loaded until then\n", ahead.Round(time.Minute))
	}

	fmt.Fprintf(stdout, "generating seed %d: %d accounts, %d patients, %d streams (%s) at %s over %s..%s: about %d readings\n",
		spec.Seed, spec.Users.Total(), spec.Patients.Count, len(spec.Streams), joinTypes(spec), spec.Time.Interval,
		spec.Time.From.Format(time.RFC3339), spec.Time.To.Format(time.RFC3339), spec.Measurements())
	m, err := synth.Write(spec, out, overwrite)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	c := m.Counts
	fmt.Fprintf(stdout, "wrote %s: %d users, %d patients, %d measurements (%d in anomaly episodes, %d dropped by gaps)\n",
		filepath.Clean(out), c.Users, c.Patients, c.Measurements, c.Anomalous, c.Gaps)
	fmt.Fprintf(stdout, "patient references start with %s-", synth.ReferencePrefix(spec))
	if users := synth.Users(spec); len(users) > 0 {
		fmt.Fprintf(stdout, "; accounts are %s and so on (synth password prints one)", users[0].Email)
	}
	fmt.Fprintln(stdout)
	return 0
}

func runLoad(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		from, target, environment string
		allowProduction, jobs     bool
		users                     bool
		batchSize                 int
		timeout                   time.Duration
		windows                   string
	)
	fs.StringVar(&from, "from", "", "fixture directory written by `synth generate` (required)")
	fs.StringVar(&target, "target", "", "gateway base URL, such as http://localhost:8080 (required)")
	fs.StringVar(&environment, "environment", "", "what the target is: local, staging or production (required)")
	fs.BoolVar(&allowProduction, "allow-production", false, "with "+synth.ProductionConfirmationEnv+"=<host>, allow a production target")
	fs.BoolVar(&jobs, "jobs", false, "after loading, run one processing job per patient over the fixture's types and range")
	fs.BoolVar(&users, "users", false, "create the fixture's accounts first, directly in the database named by DATABASE_URL")
	fs.IntVar(&batchSize, "batch-size", 500, "readings per batch request (the gateway accepts at most 1000 by default)")
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "timeout of one request")
	fs.StringVar(&windows, "windows", "1h,24h", "comma-separated processing windows for --jobs")
	if err := fs.Parse(args); err != nil {
		return usageError(err, stderr)
	}
	if fs.NArg() > 0 {
		return usageError(fmt.Errorf("unexpected argument %q", fs.Arg(0)), stderr)
	}
	if from == "" || target == "" {
		return usageError(errors.New("--from and --target are required"), stderr)
	}
	if batchSize < 1 || batchSize > 1000 {
		return usageError(errors.New("--batch-size must be between 1 and 1000"), stderr)
	}

	confirmation, _ := lookup(synth.ProductionConfirmationEnv)
	client := &http.Client{Timeout: timeout}
	health, err := synth.CheckTarget(ctx, client, synth.Target{
		URL: target, Environment: environment,
		AllowProduction: allowProduction, ProductionConfirmation: confirmation,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, synth.ErrRefused) {
			return 2
		}
		return 1
	}
	said := health.Environment
	if said == "" {
		said = "an older gateway that does not say"
	}
	fmt.Fprintf(stdout, "target %s: %s %s, environment %s (you said %s)\n", target, health.Service, health.Version, said, environment)
	if environment == synth.EnvProduction {
		fmt.Fprintln(stdout, "PRODUCTION: loading synthetic data into a production environment, as explicitly confirmed")
	}

	m, err := synth.ReadManifest(from)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fixtureUsers, err := synth.ReadUsers(from)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "fixture %s: seed %d, %d users, %d patients, %d measurements\n",
		filepath.Clean(from), m.Spec.Seed, m.Counts.Users, m.Counts.Patients, m.Counts.Measurements)

	if users {
		dbURL, ok := lookup("DATABASE_URL")
		if !ok || dbURL == "" {
			fmt.Fprintln(stderr, "--users needs DATABASE_URL: accounts are created in the database, as `api-gateway users create` does")
			return 2
		}
		created, existing, err := synth.CreateUsers(ctx, dbURL, m.Spec.Seed, fixtureUsers, lookup, stdout)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "users: %d created, %d already there\n", created, existing)
	}

	email, password, err := credentials(lookup, m.Spec.Seed, fixtureUsers, users)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	loader := &synth.Loader{Client: client, BaseURL: target, BatchSize: batchSize, Jobs: jobs, Log: stdout}
	if jobs {
		for _, w := range strings.Split(windows, ",") {
			if w = strings.TrimSpace(w); w != "" {
				loader.Windows = append(loader.Windows, w)
			}
		}
	}
	if err := loader.Login(ctx, email, password); err != nil {
		fmt.Fprintln(stderr, err)
		if _, explicit := lookup("SYNTH_EMAIL"); !explicit {
			fmt.Fprintln(stderr, "the fixture's own account refused its derived password: it was created from another fixture, or its password was changed; sign in with SYNTH_EMAIL and SYNTH_PASSWORD instead")
		}
		return 1
	}
	fmt.Fprintf(stdout, "signed in as %s\n", email)
	report, err := loader.Load(ctx, from)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "done: patients %d created / %d existing; measurements %d stored / %d existing in %d batches",
		report.Patients.Created, report.Patients.Existing,
		report.Measurements.Stored, report.Measurements.Existing, report.Measurements.Batches)
	if jobs {
		fmt.Fprintf(stdout, "; jobs %d completed / %d failed", report.Jobs.Completed, report.Jobs.Failed)
	}
	fmt.Fprintln(stdout)
	if jobs && report.Jobs.Failed > 0 {
		return 1
	}
	return 0
}

// credentials picks the account to sign in with: SYNTH_EMAIL and
// SYNTH_PASSWORD when set, otherwise the fixture's first OPERATOR (or ADMIN)
// with its derived password, which works when the accounts were created by
// --users or an earlier load.
func credentials(lookup func(string) (string, bool), seed int64, users []synth.User, created bool) (string, string, error) {
	email, _ := lookup("SYNTH_EMAIL")
	password, _ := lookup("SYNTH_PASSWORD")
	if email != "" && password != "" {
		return auth.NormalizeEmail(email), password, nil
	}
	if email != "" || password != "" {
		return "", "", errors.New("SYNTH_EMAIL and SYNTH_PASSWORD must be set together")
	}
	for _, role := range []domain.Role{domain.RoleOperator, domain.RoleAdmin} {
		for _, u := range users {
			if u.Role == role {
				if !created {
					return "", "", fmt.Errorf("no SYNTH_EMAIL/SYNTH_PASSWORD; set them to an existing OPERATOR account, or pass --users (with DATABASE_URL) to create the fixture's accounts and sign in as %s", u.Email)
				}
				return auth.NormalizeEmail(u.Email), synth.Password(seed, u.Email), nil
			}
		}
	}
	return "", "", errors.New("no SYNTH_EMAIL/SYNTH_PASSWORD, and the fixture has no OPERATOR or ADMIN account to sign in with")
}

func runPassword(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var from, email string
	fs.StringVar(&from, "from", "", "fixture directory (required)")
	fs.StringVar(&email, "email", "", "the synthetic account's address (required)")
	if err := fs.Parse(args); err != nil {
		return usageError(err, stderr)
	}
	if from == "" || email == "" {
		return usageError(errors.New("--from and --email are required"), stderr)
	}
	m, err := synth.ReadManifest(from)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	users, err := synth.ReadUsers(from)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	want := auth.NormalizeEmail(email)
	for _, u := range users {
		if auth.NormalizeEmail(u.Email) == want {
			fmt.Fprintln(stdout, synth.Password(m.Spec.Seed, u.Email))
			return 0
		}
	}
	fmt.Fprintf(stderr, "%s is not an account of this fixture\n", email)
	return 1
}

func usageError(err error, stderr io.Writer) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(stderr, err)
	return 2
}

func joinTypes(spec synth.Spec) string {
	parts := make([]string, 0, len(spec.Streams))
	for _, t := range spec.Types() {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ",")
}
