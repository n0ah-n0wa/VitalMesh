# Synthetic data

VitalMesh only ever holds synthetic data (SPECIFICATIONS.md sections 3 and
103). `synth` is the tool that makes it: accounts, patients and measurement
streams invented from a seed, written as a fixture and loaded into a local
or staging gateway through its public API. Sections 106 and 107 of the
specification ask for it; this page is how it works.

Nothing it produces describes a person. Names are not generated at all,
addresses are under the reserved `.invalid` domain, which cannot resolve,
patient references are `synth-<seed tag>-<number>`, and every value comes
from a distribution the spec names.

```bash
make up                                   # the local environment
make synth-generate                       # .synth/default: 10 patients, 7 days, every type
make synth-load                           # accounts, patients, readings, one job per patient
make synth-generate SYNTH_ARGS="--seed 7 --patients 50 --days 30 --types HEART_RATE,SPO2"
```

The `bin/synth` binary (`make build`) does the same anywhere:

```bash
synth generate --seed 7 --patients 50 --days 30 --out fixtures/seed7
synth load --from fixtures/seed7 --target http://localhost:8080 --environment local --users --jobs
```

## What is generated

| Entity | How |
|---|---|
| Users | `synth-<tag>-admin-1@synthetic.invalid`, `synth-<tag>-operator-1@…`, `synth-<tag>-user-1@…`, as many of each role as asked, where `<tag>` is eight hex digits of the seed (the same ones that start the patient references), so two seeds never share an account. The password of each is derived from the seed and the address (`synth password`); it is never written to a file |
| Patients | `synth-<tag>-0001` onwards, a date of birth from a uniform age range, a sex from weights |
| Measurement streams | one stream per patient per type, sampled at the interval over the time range, with a per-patient baseline, a daily rhythm, per-sample noise and anomaly episodes |

A stream's value at a sample is

```text
baseline (per patient, drawn from Normal(mean, sd))
+ circadian × sin(daily, lowest at midnight, highest at noon)
+ Normal(0, noise)
+ the anomaly episode in progress, if any
```

rounded to the type's decimals and clamped to the API's technical range,
so every reading is one `POST /api/v1/measurements/batch` accepts. Samples
may be late by up to `jitter` of the interval, so a series is not
perfectly regular, but it is always strictly increasing and never holds
two readings with the same timestamp.

Anomaly episodes start at any sample with probability `rate`, last
`duration` samples and take one of four shapes:

| Kind | Shape |
|---|---|
| `spike` | every sample lifted by the magnitude |
| `dip` | every sample lowered by the magnitude |
| `drift` | a linear ramp from no offset up to the magnitude, the way a failing sensor looks |
| `gap` | no samples at all: a device that stopped reporting |

Anomalous readings carry `metadata: {"anomaly":"spike","episode":3}`, so a
detector's findings can be compared with what was planted. Gaps carry
nothing, since nothing is emitted.

The defaults are resting-adult distributions for all seven types, with
rare anomalies (`synth spec` prints them). They are plausibility, not
medicine: the numbers exist so the data looks like data.

## The spec

Every knob is a field of a JSON spec. `synth spec` prints one; edit it and
pass it back with `--spec`. Flags override the file, so a spec can be a
base and a command line the variation:

```bash
synth spec --seed 3 --patients 20 > spec.json
synth generate --spec spec.json --days 14 --noise-scale 2 --out fixtures/noisy
```

| Flag | Spec field | Meaning |
|---|---|---|
| `--seed` | `seed` | every random choice; same seed and spec, same bytes |
| `--admins`, `--operators`, `--users` | `users.*` | accounts per role |
| `--patients` | `patients.count` | how many patients |
| `--age-min`, `--age-max` | `patients.age_*` | age range, uniform, at the end of the time range |
| `--reference-prefix` | `patients.reference_prefix` | start of every external reference (default: derived from the seed) |
| `--from`, `--to`, `--days` | `time.from`, `time.to` | the range, half-open; `--to` defaults to now, `--from` to `--days` before it |
| `--interval` | `time.interval` | spacing of samples, at least `1s` |
| `--jitter` | `time.jitter` | how late a sample may be, as a fraction of the interval, in [0, 1) |
| `--types` | `streams` (keys) | which measurement types to generate |
| `--source` | `source` | the `source` field of every reading |
| `--noise-scale` | `streams.*.noise` | multiplies every stream's noise: 0 is a clean signal, 3 a noisy one |
| `--anomaly-rate` | `streams.*.anomalies.rate` | sets every stream's episode rate; 0 turns anomalies off |

Per-stream distributions (`baseline`, `circadian`, `noise`, `decimals`,
`anomalies.kinds`, `anomalies.magnitude`, `anomalies.duration`) are set in
the file. Unknown fields are rejected, so a typo cannot pass silently.

Volume is patients × types × samples, where samples is the range divided
by the interval: ten patients, seven types, seven days at one minute is
about 700,000 readings, written in a few seconds. The generator streams to
disk and the loader streams from it, so size is bounded by the disk, not
by memory.

## Reproducibility

The same seed and spec produce the same fixture on any machine, byte for
byte. Two things follow.

A fixture need not be stored: its manifest is enough to make it again, and
a bug report can say "seed 7, the default spec" instead of attaching a
file. The manifest (`manifest.json`) records the spec, the counts, the
generator version and the SHA-256 of each data file; `synth load` checks
the digests before it starts and refuses a fixture that was edited or
truncated.

Changing a stream's parameters changes only that stream. Each patient's
stream is generated from the seed, the patient's index and the type, so
adding patients or types leaves the existing ones untouched, and a fixture
can grow without its earlier readings moving.

The contract is held by a golden fixture in
`services/api-gateway/internal/synth/testdata/golden`: a test regenerates
it from its manifest and compares the bytes. A deliberate change to what a
spec produces bumps the generator version and regenerates it with
`go test ./internal/synth -update`; an accidental one fails CI.

Explicit `--from` and `--to` make a fixture fully reproducible. With the
defaults, `--to` is the current minute, so two runs an hour apart differ
in their time range and therefore in their bytes; the manifest records
which range was used.

## Loading

`synth load` is a client of the public API and nothing more: it signs in,
creates the patients, stores the readings in batches and, with `--jobs`,
runs one processing job per patient over the fixture's types and range.
Every write carries an `Idempotency-Key` derived from the fixture, so the
same fixture loaded twice stores nothing twice; the second run reports
everything as already there. The loader only creates. No call it makes
deletes or changes what was in the database before it ran.

Rate limits are respected: a `429` is waited out for `Retry-After` and
retried, as are `502`, `503` and `504` and a busy idempotency record, a
bounded number of times. A validation error is reported with its field
details and stops the load.

Accounts are the exception to "through the API": there is no endpoint
that creates them, by design, so `--users` creates them directly in the
database named by `DATABASE_URL`, the way `api-gateway users create` does.
That works locally (`make synth-load` sets it) and from a job inside the
cluster, not from a workstation against staging. Against staging, sign in
with an existing operator account instead:

```bash
SYNTH_EMAIL=… SYNTH_PASSWORD=… \
  synth load --from fixtures/seed7 --target https://<staging host> --environment staging --jobs
```

Credentials are read from the environment, never from flags, so they are
in no process list or shell history. The loader prints nothing it was
given.

`synth password --from DIR --email ADDRESS` prints the password of one of
the fixture's own accounts, which is the only time one is printed. The
derivation is deliberately public (a keyed hash of the seed and the
address): these accounts are for local and staging environments and are
never a secret, which is one more reason the loader refuses production.

## The production safeguard

Loading synthetic data into production is refused unless it is asked for
twice, in two different ways, and the target agrees. Three checks run
before anything is sent, and only `GET /health` is called while they run:

1. **The claim.** `--environment` is required: `local`, `staging` or
   `production`. There is no default.
2. **The address.** For `local`, the host must look local: a loopback
   address, `localhost`, a name under `.local`, `.localhost` or
   `.internal`, Docker's host alias, or a single-label name such as a
   Compose service. A remote name with `--environment local` is refused.
   For `staging` and `production`, a remote host must be `https`, so that
   the credentials do not travel in the clear; a port-forward to a local
   address is fine.
3. **The gateway's own word.** `GET /health` reports the environment the
   gateway was configured with. If it disagrees with the claim, the load is
   refused: a gateway that says `production` is never loaded under
   `--environment staging`, whatever the address looks like. A gateway that
   reports nothing (an older build) passes on the first two checks alone.

For `production` two more keys are needed, on top of the three checks:
the `--allow-production` flag, and the `VITALMESH_SYNTH_ALLOW_PRODUCTION`
environment variable set to exactly the target's host name. Either alone
is refused with a message naming the other. With both, the load proceeds
and says so in capitals on its first line.

Exit code 2 means the target was refused and nothing was written.

The safeguard is tested against a fake gateway for every combination of
claim, address and answer (`internal/synth/target_test.go`,
`cmd/synth/main_test.go`), and the checks that a refused load makes no
call but `/health` are part of those tests.

## In the pipeline

The unit tests of the generator, the fixture format, the loader and the
safeguard run in `make test-go`. `make integration-test-postgres` also
loads a fixture into the real gateway, in process over the real database:
it creates the accounts as `--users` does, signs in with a derived
password, loads twice and reads every patient's readings back.

Staging is loaded by hand, from a workstation with a port-forward or
through the ingress, as above; the deployment pipeline does not seed
staging by itself, because a deployment should not grow the database each
time it runs. SPECIFICATIONS.md section 103 requires staging to hold
synthetic data only, and this tool is how that data gets there.
