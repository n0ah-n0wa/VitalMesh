# Failure modes

What the system does when a dependency goes away, observed rather than
reasoned about (SPECIFICATIONS.md sections 37, 52 and 90 to 94). Two suites
observe it, and both are run, not read:

```bash
make stack-test                        # docker compose: the automated failure-injection tests
sh scripts/k8s-local-test.sh --keep    # kind: deploy
sh scripts/k8s-failure-test.sh         # kind: break things
```

The kind suite runs against the local overlay with Calico, so
NetworkPolicies are enforced and an outage can be simulated by cutting a
path rather than deleting a workload. The stack suite
(`services/api-gateway/tests/stack`, build tag `stack`) takes services away
with `docker compose stop`, `pause` and `restart`, severs the gateway's
database connections from inside PostgreSQL, and asserts the four
properties the specification requires of every failure (below); a Go
integration test (`pool_exhaustion_test.go`) covers connection-pool
exhaustion at the pool level, where it is safely testable.

## Observed behaviour

| Failure | Expected (section 90) | Observed |
|---|---|---|
| Gateway pod deleted | replaced, nothing lost | new pod Ready in seconds; `/ready` 200; data intact |
| Processor pod deleted | replaced, processing resumes | new pod Ready; next job COMPLETED |
| PostgreSQL unreachable | API degraded or unavailable | `/health` 200, `/ready` 503, pod withdrawn from the Service, uncached reads and writes `503 DATABASE_UNAVAILABLE` (or `504 REQUEST_TIMEOUT` when the network swallows the connection rather than refusing it), **no restart**, recovery in ~1s |
| Redis unreachable | rate limiting and cache degrade | `/ready` stays 200, reads and sign-in work, rate limiting still returns 429 |
| Processor unavailable | processing unavailable | `/ready` stays 200, call bounded at ~10s, job recorded FAILED, one row |
| Processor slower than its budget | timeout, job not lost | attempt cut short, retry collects the result, one row, terminal state |
| Rolling deployment | no disruption | 40 requests across a restart, none dropped |

Across every scenario: **zero container restarts** of anything not
deliberately restarted, **no job left PENDING or PROCESSING**, no duplicate
rows, and the measurement count unchanged.

## The four properties, and where each failure is asserted

Every failure the specification lists (section 52) has an automated test
that asserts, for that failure, the four things section 37 requires: the
request **fails predictably** (a documented status and error code, bounded
in time), nothing **cascades** (readiness and unrelated paths keep serving,
and no healthy container restarts), retries stay **bounded** (never
uncontrolled, never a duplicate write), and the **database and job state
stay correct** (one terminal job per dispatch, no row lost or invented).

| Failure (section 52) | Test | Injection | Asserted behaviour |
|---|---|---|---|
| PostgreSQL unavailable | stack `database failure` | `stop postgres` | `/health` 200, `/ready` 503, a write answers `503 DATABASE_UNAVAILABLE` or `504` within the request bound and names no host; on recovery readiness returns in ~1s, no gateway restart, the failed write created nothing |
| Redis unavailable | stack `Redis failure` | `stop redis` | `/ready` stays 200; reads, sign-in, writes and idempotent replays all work; rate-limit headers still present (the limiter falls back to a per-replica counter); no gateway restart |
| Rust processor unavailable | stack `Rust unavailable` | `stop processor` | `/ready` stays 200; the job answers a 5xx within ~10s; exactly one job row, FAILED, with an error code; recovery on `start`, no gateway restart |
| Rust timeout | stack `timeout` | `pause processor` (frozen, never answers) | the job answers `504` between the per-attempt and the request bound; one FAILED row; recovery on `unpause` |
| Transient network / slow downstream | stack `transient processor recovery` | `pause` then `unpause` mid-request | the gateway waits out the stall and the job COMPLETES with results, no client retry, no failure recorded, one job row, no restart |
| Bounded retries (no uncontrolled duplicates) | stack `bounded retries, no duplicate` | `pause processor`, one job | `attempt_count` within `PROCESSOR_MAX_ATTEMPTS` (3), no more "may retry" log lines than that for the request, exactly one job row |
| Transient network error at the database | stack `database connections severed` | `pg_terminate_backend` of the gateway's pooled connections | the pool reconnects on its own, reads and a write serve again within seconds, readiness recovers, the patient is intact, no gateway restart |
| Pod restart (downstream) | stack `processor restart` | `stop` then `start processor` | a job during the outage is FAILED and terminal, a job after it COMPLETES, the gateway does not restart |
| Pod restart (the service) | stack `gateway restart` | `restart api-gateway` | patient, measurement, job and result counts unchanged across the restart, no job left non-terminal, a write after it works, a job created before it still reads back COMPLETED |
| Database connection exhaustion | `pool_exhaustion_test.go` (integration) | a 2-connection pool with both held | a third acquire fails with a deadline within its bounded context rather than hanging, and the pool serves again the instant a connection is returned, with no leaked connection |
| Pod killed mid-job (a crash, an OOM kill, a lost node) | stack `gateway killed mid-job` | `kill api-gateway` (SIGKILL, no drain) while a job is in flight against a frozen processor | the job is PROCESSING across the restart and is not guessed dead; once its lease expires the restarted gateway's sweep fails it with `PROCESSING_INTERRUPTED`, `attempt_count` 1, no results invented, no job left non-terminal; the next job COMPLETES (docs/RESILIENCE.md) |

The pod-restart rows restart the named container on purpose; "no gateway
restart" there means the *gateway* was not restarted as a side effect of a
*downstream* failure. Rolling deployments and pod deletion are covered by
the kind suite above; the stack suite covers container restart, which is
what docker compose can inject.

## The details worth knowing

### A database outage takes the API out of rotation, and that is the design

`/health` keeps answering because liveness consults no dependency — which
is what stops Kubernetes restarting a healthy process because something
else is broken (section 35). `/ready` turns 503, the pod leaves the
Service's endpoints, and callers get no route at all rather than a slow
error. The pod stays `Running`, and after 65 seconds without a database
its restart count was still zero.

When the database came back, `/ready` returned 200 within a second and the
pod was re-added to the endpoints. No restart was needed, and no data was
lost.

One nuance: a patient that had been read recently still answered 200 at the
pod's own address during the outage, served from the Redis cache. That does
not keep the API available, because readiness has already withdrawn the pod
from the Service — the cache shortens a database blip for in-flight work,
it does not extend availability.

### Redis is a degradation, not an outage

Readiness ignores Redis on purpose. With it unreachable, reads and sign-in
worked, and rate limiting still refused 16 of 75 rapid sign-in attempts —
the limiter falls back to a per-replica counter, so the limit is enforced
per pod rather than across the deployment. That is a weaker guarantee, not
an absent one.

### A failed dispatch always leaves a terminal job

With the processor scaled to zero, the call returned in about ten seconds
and the client received `REQUEST_TIMEOUT`. Exactly one `processing_jobs`
row was written and it was `FAILED`. Nothing was left `PENDING` or
`PROCESSING`, which is what section 92 requires.

Worth noting for tuning: `PROCESSOR_MAX_ATTEMPTS` (3) × `PROCESSOR_TIMEOUT`
(5s) is 15s, which is longer than `HTTP_REQUEST_TIMEOUT` (10s). The retry
budget therefore cannot be spent inside one request, and the caller sees
the handler's generic timeout rather than the more specific
`PROCESSOR_UNAVAILABLE`. Configuration validation checks that
`PROCESSOR_TIMEOUT < HTTP_REQUEST_TIMEOUT` but not the product of attempts
and timeout. The outcome is correct either way — the job is FAILED and the
call is bounded — so this is a diagnosability question, not a correctness
one.

### A timeout does not mean the work was lost

Given a deliberately absurd 1ms budget, the gateway's log shows what
actually happens:

```
WARN  processor call failed, may retry
INFO  processor call succeeded after a retry
```

The first attempt is cut off, the processor finishes the work regardless,
and the retry a hundred milliseconds later collects the result. The job
completes with one row. Retrying is safe because the request carries the
job id the gateway created, so a repeat cannot become a second job — which
is the property section 37 asks for.

### A job the gateway dies under is failed, not forgotten

Graceful shutdown drains a dispatch, but a crash cannot. A job in flight
when the gateway is killed stays `PROCESSING` in the database with nobody
working on it. Every dispatch therefore holds the job under a lease
(`PROCESSING_JOB_LEASE`, 15 minutes, far longer than the request timeout
plus the failure record's own), and every gateway sweeps expired leases
(`PROCESSING_LEASE_SWEEP_INTERVAL`, 1 minute): a job still `PROCESSING`
after its lease has expired is moved to `FAILED` with
`PROCESSING_INTERRUPTED`, keeping its attempt count and timestamps. The
same sweep starts and fails a job left `PENDING` by a request that died
between creating and dispatching it. A job whose lease is valid is never
touched, however slow its gateway is, so the sweep cannot fail work that
is still being done. The `gateway killed mid-job` case above watches this
happen with a short lease. The related case of a processor that finished
but a results write that failed is handled in the request itself: the job
is failed with `PROCESSING_RESULTS_NOT_STORED` rather than left
`PROCESSING`.

## Simulating an outage without causing a different one

Two mistakes are easy to make here, and both were made before the results
above were trustworthy.

**Scaling a fixture to zero is not an outage.** The local PostgreSQL keeps
its data in an `emptyDir`, so scaling it to zero destroys the database
rather than making it unavailable. Every later scenario then runs against
an empty schema and reports failures that are really the fixture being
gone. The suite narrows the `fixtures` NetworkPolicy instead: the pod keeps
running with its data, and nothing can reach it.

**Cutting the network does not close open connections.** A NetworkPolicy
governs new connections; Calico leaves established flows alone, so blocking
port 5432 left the gateway reading and writing quite happily through the
pool it already held. The suite also calls `pg_terminate_backend` to sever
them. Signalling the server does not work either — the kernel will not
deliver SIGKILL to PID 1 from inside its own namespace, so `kill -9 1`
returns success and changes nothing.

**Probe the pod, not the Service, while readiness is false.** A pod that
fails readiness is removed from its Service, so every request through the
Service returns a connection failure and "no route" becomes
indistinguishable from "the application refused". The suite resolves the
pod's address first and asks it directly, and asserts the endpoint
withdrawal separately.

## What was found

No correctness problems in the application. Every scenario behaved as
sections 37, 90 and 92 describe, and the failures seen along the way were
in the test harness: destroying the database instead of isolating it,
probing through a route that readiness had withdrawn, and asserting that a
1ms budget must make a job fail when the retry is designed to recover it.
