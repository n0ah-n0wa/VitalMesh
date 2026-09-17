# Load testing

`make load-test` runs a fixed workload against the local environment with
[k6](https://k6.io) in a pinned container and writes a report with
throughput, p50/p95/p99 latency per operation, the error rate and the
processing latency (SPECIFICATIONS.md section 50). This page says what the
workload is, what the numbers mean, where they were measured, and what
they do not say.

The one sentence to keep: **the numbers are measurements of one run in one
environment, and that environment is a laptop.** They compare runs on the
same machine against each other. They are not the capacity of the system,
not what a deployment would do, and not a promise of anything.

```bash
make up                              # the local environment
make load-test                       # the standard profile: about 70 s of load, a 3 minute run all told
make load-test LOAD_PROFILE=smoke    # 15 s, to see that everything works
```

The report is printed and written to `tests/load/results/` (git-ignored)
as `report-<profile>-<stamp>.md`, next to `summary-<profile>-<stamp>.json`
with every metric k6 collected. Neither file holds a credential: the
accounts' tokens live in k6's setup data, which is left out of the
summary on purpose, so `make secret-scan` stays clean with results on
disk.

## What runs

`scripts/load-test.sh` starts the services if they are not up, creates the
load accounts with the gateway's own `users create` command (there is no
API for that, by design), records the environment, and runs
`tests/load/k6/scenarios.js` in `grafana/k6:1.4.0` on the compose network.
Everything it creates is synthetic and stays in the local database:
accounts `load-<n>@vitalmesh.invalid` on a reserved domain, patients that
are a reference and a date of birth, readings that are a fixed pattern.

Five scenarios run at the same time, each an **open workload**: k6 keeps
sending at the scenario's rate whether or not the system keeps up, and
counts the iterations it could not start on time (`dropped iterations` in
the report). That is what a real client population does; a closed loop
that waits for each answer would flatter a slow system.

| Scenario | Each iteration | Standard profile | Smoke profile |
|---|---|---|---|
| `ingest` | `POST /api/v1/measurements`, one reading | 20/s over 8 accounts | 5/s over 4 |
| `batch` | `POST /api/v1/measurements/batch`, 100 readings (`BATCH_SIZE`) | 5/s over 4 accounts | 1/s over 2 |
| `processing` | a 60-reading batch, then `POST /api/v1/processing/jobs` over exactly those readings (windows 1m and 5m, percentiles 50 and 95), then `GET .../processing-results` | 2/s over 4 accounts | 1/s over 2 |
| `readers` | `GET /patients/{id}`, `GET .../measurements?limit=50`, `GET .../processing-results?limit=20` on a patient that is receiving jobs | 10/s over 10 accounts | 3/s over 4 |
| `ratelimit` | `GET /patients/{id}` on one account, deliberately over its budget | 10/s for 45 s | 25/s for 15 s |

The standard profile runs the first four for 60 seconds; `ratelimit`
starts after 5 seconds so that its refusals land in steady state.
`DURATION=120s` lengthens a run; the rates and account counts are in the
profile table at the top of `scenarios.js`.

## Workload assumptions

These choices shape every number in the report. Change them and the
numbers change.

- **Per-account rate budget.** The gateway allows an authenticated account
  300 requests a minute (five a second) and answers `429` beyond that.
  Every scenario has a pool of accounts of its own and rotates through it
  iteration by iteration, so a scenario's rate divided by its accounts stays
  under five a second: 2.5 for `ingest`, 1.25 for `batch`, 1.5 for
  `processing` (three requests an iteration), 3 for `readers`. The limit,
  not the server, is therefore what caps throughput per account, and the
  total rate is a function of how many accounts the profile has. Raising
  `RATE_LIMIT_AUTHENTICATED` in `docker-compose.yml` would measure a
  different system.
- **One rate-limit probe.** The `ratelimit` scenario's account is pushed to
  10 requests a second, twice its budget, so roughly a third of its
  requests must be refused once the window fills. Those `429`s are expected
  and are excluded from the error rate; they are counted separately, and
  each is checked for `Retry-After` and a zero remaining budget.
- **Unique readings.** Every VU has a patient of its own and a three-hour
  time window of its own, two hours or more in the past, and numbers its
  readings 10 ms apart, so no request ever repeats a reading (which the
  API would refuse with `409`) and every reading is inside the processor's
  acceptance window.
- **Fixed job size.** Each processing iteration stores 60 readings and asks
  for a job over exactly that range, so every job is the same size and the
  processing latencies are comparable. The processor is configured with
  `MAX_CONCURRENT_JOBS=4` in the local environment; at 2 jobs a second of
  a few milliseconds each it is nowhere near that, and the report counts
  jobs refused as busy separately.
- **Readers read hot patients.** The `readers` scenario reads the patients
  of the `processing` VUs, which are receiving readings and results while
  it reads, rather than empty ones.
- **No think time, no TLS, no ingress.** Requests go straight to the
  gateway over plain HTTP on the compose network, back to back. A deployed
  environment adds a load balancer, TLS and network distance that this
  measurement does not include.
- **Data grows during the run.** Readings accumulate in the patients'
  tables as the run goes on (about 38,000 in a standard run), so the tail
  of a run reads and writes against more data than its start. A run
  against a database already holding many runs' worth of data is a
  different measurement again; `docker compose down -v` resets it.

## What is measured

| Figure | Definition |
|---|---|
| Throughput | requests completed divided by the run's length, per operation and in total; readings ingested a second across `ingest`, `batch` and `processing`; jobs completed a second |
| p50, p95, p99, max | k6's `http_req_duration` for the operation: from the first byte sent to the last byte of the response received, as seen by the load generator on the same host |
| Error rate | the share of requests that did not get the status the operation expected (`201` for writes, `200` for reads), with the rate-limit probe's `429`s excluded. `http_req_failed` is also printed: k6's own count, which includes those `429`s |
| Processing latency | the processor's time on a job, `completed_at` minus `started_at` from the job the API returns: the Rust engine's work without the HTTP round trip, validation or database writes. The end-to-end figure for a job is the `POST /processing/jobs` row |
| Dropped iterations | iterations the arrival-rate executors could not start on time; a run with many is a run where the load generator, not only the system, was behind |
| Rate limiting | requests and `429`s of the probe, and the checks on the refusal headers |

The thresholds in `scenarios.js` are **correctness gates only**: the run
fails if the error rate reaches 1% or any check fails. There is no latency
threshold, on purpose: a latency target would be a claim about what the
system should do, and this page makes none.

## Environment

The report records the environment it ran in, from the script:

```text
date, commit (and whether the tree was modified)
host kernel and architecture
Docker version, operating system, CPUs and memory available to containers
docker compose version
gateway and processor versions (from /health)
k6 image
service configuration: docker-compose.yml defaults (no CPU or memory limits, PostgreSQL 16,
  Redis 7, processor MAX_CONCURRENT_JOBS 4, gateway rate limit 300/min per account)
load generator: k6 in a container on the same Docker host, sharing its CPU with the services
```

The last line matters most. The load generator, both services, PostgreSQL
and Redis all run on one machine, inside one Docker Desktop virtual
machine on Windows in the runs below, and compete for the same eight
virtual CPUs and four gigabytes. Everything the machine does during a run
(a browser, a build, Docker Desktop's own housekeeping) shows up in the
tail latencies.

## An example: two consecutive standard runs

Measured on 2026-09-16 at commit `4140694` with a modified working tree,
on a Windows 11 laptop, Docker Desktop 26.1.4 with 8 CPUs and 3.8 GiB for
containers, k6 1.4.0, the environment above. Two runs, three minutes apart,
nothing else running.

| Operation | Requests | req/s | run 1 p50 | p95 | p99 | max | run 2 p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|---|---|---|
| POST /measurements (one reading) | 1194 / 1201 | 16.7 / 17.2 | 7.4 | 30.0 | 178.3 | 1141.4 | 5.7 | 9.7 | 15.8 | 37.0 |
| POST /measurements/batch (100 readings) | 301 / 300 | 4.2 / 4.3 | 54.1 | 199.6 | 517.6 | 1064.0 | 40.2 | 59.7 | 81.5 | 136.8 |
| POST /processing/jobs (60 readings, end to end) | 120 / 121 | 1.7 / 1.7 | 22.9 | 65.2 | 118.9 | 233.9 | 17.1 | 23.0 | 54.7 | 92.6 |
| GET …/processing-results (after a job) | 120 / 121 | 1.7 / 1.7 | 3.5 | 10.9 | 13.4 | 13.6 | 2.5 | 3.4 | 4.0 | 4.1 |
| GET /patients/{id} | 601 / 601 | 8.4 / 8.6 | 1.8 | 6.6 | 12.8 | 47.9 | 1.5 | 2.4 | 4.1 | 9.5 |
| GET /patients/{id}/measurements | 601 / 601 | 8.4 / 8.6 | 3.1 | 11.7 | 32.1 | 763.4 | 2.4 | 4.0 | 5.9 | 17.0 |
| GET …/processing-results (reader) | 601 / 601 | 8.4 / 8.6 | 2.5 | 9.6 | 16.3 | 832.2 | 2.0 | 3.2 | 5.0 | 26.7 |
| GET /patients/{id} (rate-limit probe) | 450 / 451 | 6.3 / 6.5 | 1.7 | 5.4 | 13.0 | 17.1 | 1.5 | 2.4 | 4.1 | 5.0 |

Latencies in milliseconds. Totals: about 4,200 requests at 59 req/s
overall, about 38,500 readings ingested at 540 readings/s, 120 jobs at
1.7 jobs/s, error rate 0.000% in both runs, every check passed, 7 dropped
iterations in run 1 and none in run 2, 152 and 153 of the probe's 450
requests refused with `429` (33.8%, 33.9%), each carrying `Retry-After`.

Processing latency (the processor's own time on a 60-reading job): run 1
p50 5.5 ms, p95 20.0 ms, p99 39.4 ms, max 88 ms; run 2 p50 4.0 ms, p95
6.0 ms, p99 8.8 ms, max 41 ms.

What the two runs show, and it is the reason there are two: the medians
agree within a couple of milliseconds, and the tails do not. Run 1 has a
one-second stall that reaches every write operation and the two reader
operations at once; run 2 has no such stall. That stall is the host, not a
property of the code path, and a single run would have reported it or not
by luck. Read the p50 as the cost of the operation on this machine, and
the p99 as a property of the machine on that day. Quote either with the
environment line attached, or not at all.

What the runs do not show: the maximum throughput (the rates are fixed,
well under the point where anything saturates, and the per-account limit
would refuse more before the server did), how the system scales (one
replica of each service, no limits, no ingress), or what a deployment
would measure (different hardware, a real network, TLS, a load balancer,
PostgreSQL and Redis as managed services).

## Reproducing and comparing

A run is reproducible in what it sends: the profile fixes the rates, the
mix, the batch and job sizes, the account pools and the reading pattern,
and the k6 version is pinned. It is not reproducible in what it measures,
because the environment is not: two runs on one machine differ in their
tails, and two machines differ in everything.

To compare two states of the code, run both on the same machine, back to
back, from the same database state (`docker compose down -v` between
them), more than once, and compare medians before tails. Keep the
`report-*.md` and the environment block that heads it; a number without
its environment block is a rumour.

To measure something else, change one thing at a time: `DURATION` for a
longer run, `BATCH_SIZE` up to 1000 for larger batches, a profile edit for
other rates or more accounts (the script creates `ACCOUNTS` accounts,
32 by default; a profile that needs more says so and stops).

## Limitations, plainly

- One machine runs everything, load generator included.
- Short runs (60 s) with no warm-up phase; connection pools, caches and
  the JIT of nothing (both services are compiled) are cold at the start.
- Fixed, moderate rates chosen to stay under the per-account limit; the
  test does not search for a breaking point.
- The processor is fed 60-reading jobs. Larger jobs, more measurement
  types, more windows and percentiles cost more, and are not measured.
- No TLS, no ingress, no network latency, no managed database.
- The report is one run's summary. Distributions over time (a rate that
  climbs as the tables grow, a stall at minute two) are in the raw
  `summary-*.json` only as totals; run with `--out` for a time series.
