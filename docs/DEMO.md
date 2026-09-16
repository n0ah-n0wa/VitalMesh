# The demonstration

One command runs VitalMesh end to end on your machine and shows what
happened at every layer: the API, the Rust processing engine, the
database, the metrics, the trace and the logs (SPECIFICATIONS.md section
108).

```bash
git clone https://github.com/n0ah-n0wa/VitalMesh
cd VitalMesh
make demo            # or: sh scripts/demo.sh
```

You need Docker with the compose plugin, `curl` and a POSIX shell (Git
Bash on Windows). Nothing else is installed on your machine: both
services are built as images and run in containers. The first run builds
them, which takes a few minutes; later runs take about half a minute.

Everything the demo creates is synthetic. The account is
`demo@vitalmesh.invalid` on a domain that cannot exist, the patient is a
reference and a date of birth, and the readings are a fixed series of
sixty heart-rate values with one deliberate spike. No real person is
described, here or anywhere else in the system.

## The twelve steps

Each step prints the request it makes and what came back, so the output
is the explanation. What to expect:

| Step | What happens | What to look at |
|---|---|---|
| 1. Start the infrastructure | `docker compose up -d --wait`: the gateway, the processor, PostgreSQL, Redis, Prometheus, Grafana and the OpenTelemetry collector, started in dependency order and waited for | `/health` answers with the version and environment, `/ready` with each dependency check |
| 2. Create the demo account | the gateway's own `users create` command, in a one-off container: there is no self-registration | created once; a second run finds it and moves on |
| 3. Authenticate | `POST /api/v1/auth/login` | a Bearer token, its lifetime, the account's role. The token is never printed |
| 4. Create a synthetic patient | `POST /api/v1/patients` with an `Idempotency-Key` | the id the server assigned, the `ACTIVE` status |
| 5. Generate synthetic measurements | sixty `HEART_RATE` readings a minute apart, a resting rhythm ending an hour ago, reading 31 at 185 bpm | the series is the same on every run; only the timestamps move |
| 6. Submit the measurements | `POST /api/v1/measurements/batch` | 60 stored in one transaction, and the request id the rest of the demo follows |
| 7. Create a processing job | `POST /api/v1/processing/jobs`: heart rate, windows of 5 minutes and 1 hour, the 50th and 95th percentiles | the job comes back already `COMPLETED`, because the gateway hands the readings to the Rust engine and answers when it is done; the same request again returns the same job |
| 8. Wait for processing | `GET /api/v1/processing/jobs/{id}` until the job is terminal | the timestamps of the life cycle, the processor version that did the work, the attempt count |
| 9. Retrieve the results | `GET /api/v1/patients/{id}/processing-results` | one result per window, statistics per window, and the anomaly the detector found at 185 bpm |
| 10. Inspect the metrics | `/metrics` on both services, a Prometheus query | request and job counters; no patient, user or request id appears as a label |
| 11. Inspect the trace | the trace id from the gateway's log line for the job request, the processor's lines under the same id, the collector's confirmation | one id across two services |
| 12. Inspect the logs | the structured lines of both services for the request, and the audit trail in PostgreSQL | one JSON line per event, `request_id` and `trace_id` on each, never a credential or a reading value; the audit rows name the actor and the request |

The run ends with the patient id, the job id and the trace id, so any of
them can be looked up afterwards.

## After the run

The environment stays up. Some things worth doing with it:

- **Dashboards.** http://localhost:3000 (no login) has four: API traffic,
  latency and errors, processing and jobs, infrastructure. Run the demo a
  few more times and watch them move. http://localhost:9090 is Prometheus
  itself.
- **The trace.** `docker compose logs otel-collector` prints every trace
  the collector received. To see the attributes of each span, set the
  debug exporter's verbosity to `detailed` in
  `observability/otel/collector.yaml` and restart it.
- **The logs.** `docker compose logs -f api-gateway processor` follows both
  services. Every line carries `request_id` and `trace_id`, so
  `grep <request id>` gives one request's story across both.
- **The database.** `docker compose exec postgres psql -U vitalmesh` opens
  a shell on the data: `patients`, `measurements`, `processing_jobs`,
  `processing_results`, and `audit_logs`, which rejects every `UPDATE` and
  `DELETE`.
- **More data.** `make synth-generate` and `make synth-load` fill the
  environment with a seeded synthetic data set: many patients, every
  measurement type, noise and anomaly episodes over a configurable range
  ([SYNTHETIC_DATA.md](SYNTHETIC_DATA.md)).
- **The API.** [API.md](API.md) documents every endpoint the demo used and
  the ones it did not, with the error codes and the rules.

## Running it again, resetting, stopping

The demo is safe to run as often as you like: each run registers a new
patient and uses fresh idempotency keys, and the account is only created
the first time. `DEMO_NO_START=1 make demo` skips the first step when the
environment is already running.

```bash
docker compose down        # stop everything, keep the database
docker compose down -v     # stop everything and start from nothing next time
```

## If something goes wrong

**A port is in use.** Every port is published on `127.0.0.1` and start-up
fails rather than taking a port something else holds; a machine-wide
PostgreSQL on 5432 is the usual case. Move the container's host port:

```bash
POSTGRES_PORT=5433 make demo
```

`.env.example` lists every port variable; copying it to `.env` makes the
change stick. Only the host side moves.

**Windows.** Run the demo from Git Bash. The script sets
`MSYS_NO_PATHCONV=1` itself, so container paths are not rewritten.

**No anomaly was found.** The local processor is configured with a
development rule set in `docker-compose.yml` (`ANOMALY_RULES`) that marks
a heart rate above 150 bpm as critical. If the demo stops at step 9 saying
no anomaly was found, that configuration did not reach the processor:
`docker compose config` shows what it was started with.

**Sign-in failed.** The demo account's password is
`demo-password-demo-password` (a development value, not a secret). If
the account was created earlier with another password, set
`DEMO_PASSWORD` to it, or start over with `docker compose down -v`.

**The first step is slow.** The images are built from source on the
first run: the Go gateway and the Rust processor, each in a two-stage
build. Later runs reuse the layers. `docker compose up --build` by hand
shows the progress.

## Against a deployed environment

The same script is what the release pipeline runs against staging after
every deployment, as a real client from outside the cluster. Point it at
the gateway and tell it the account is provisioned:

```bash
GATEWAY_URL=https://<staging host> DEMO_EMAIL=… DEMO_PASSWORD=… DEMO_ACCOUNT_EXISTS=1 sh scripts/demo.sh
```

Steps 1 and 2 have nothing to do there, and steps 10 to 12 print the
request ids and where the metrics, the trace and the logs of a deployed
environment are found instead of showing them
([DEPLOYMENT.md](DEPLOYMENT.md)). Steps 3 to 9 are identical.

## What the demo is not

It is a tour, not a test. The behaviour it walks through is asserted by
the test suites: the unit and integration tests next to the code, the
in-process end-to-end suite (`make e2e-test`) and the suite against the
containerized stack with its failure cases (`make stack-test`), all of
which CI runs on every change ([DEVELOPMENT.md](DEVELOPMENT.md)).
