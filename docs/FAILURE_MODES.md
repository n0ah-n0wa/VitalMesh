# Failure modes

What the system does when a dependency goes away, observed on a real
cluster rather than reasoned about (SPECIFICATIONS.md sections 52 and 90).

```bash
sh scripts/k8s-local-test.sh --keep    # deploy
sh scripts/k8s-failure-test.sh         # break things
```

The suite runs against the local overlay on kind with Calico, so
NetworkPolicies are enforced and an outage can be simulated by cutting a
path rather than deleting a workload.

## Observed behaviour

| Failure | Expected (section 90) | Observed |
|---|---|---|
| Gateway pod deleted | replaced, nothing lost | new pod Ready in seconds; `/ready` 200; data intact |
| Processor pod deleted | replaced, processing resumes | new pod Ready; next job COMPLETED |
| PostgreSQL unreachable | API degraded or unavailable | `/health` 200, `/ready` 503, pod withdrawn from the Service, uncached reads and writes 504, **no restart**, recovery in ~1s |
| Redis unreachable | rate limiting and cache degrade | `/ready` stays 200, reads and sign-in work, rate limiting still returns 429 |
| Processor unavailable | processing unavailable | `/ready` stays 200, call bounded at ~10s, job recorded FAILED, one row |
| Processor slower than its budget | timeout, job not lost | attempt cut short, retry collects the result, one row, terminal state |
| Rolling deployment | no disruption | 40 requests across a restart, none dropped |

Across every scenario: **zero container restarts**, **no job left PENDING
or PROCESSING**, no duplicate rows, and the measurement count unchanged.

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
