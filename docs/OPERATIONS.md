# Operations

How VitalMesh behaves on Kubernetes when the ground moves under it —
deployments, pod and node loss, autoscaling, and dependency outages — and
what an operator does in each case. It is the observed companion to
[RESILIENCE.md](RESILIENCE.md) (every retry, timeout and lease, classified)
and [FAILURE_MODES.md](FAILURE_MODES.md) (each dependency outage at the
application level). Where those two reason about one process, this one is
about the cluster: more than one replica and more than one node, which is
where PodDisruptionBudgets, topology spread and the HorizontalPodAutoscaler
first mean anything.

Everything below was observed, not asserted. It is produced by
`make k8s-resilience-test`, which builds a throwaway three-node cluster and
runs the scenarios in this document against it; the section "How this is
kept honest" at the end says exactly what that covers.

## The environments

Three overlays, each a real target with its own `kustomize` base
(`infrastructure/kubernetes/overlays/`):

| Overlay | PostgreSQL / Redis | Ingress | Secrets | HPA floor / ceiling |
|---|---|---|---|---|
| `local` | in-cluster fixtures (emptyDir) | none | generated, non-secret | 1 / 2 |
| `staging` | managed (RDS, ElastiCache) | TLS ingress (ALB) | AWS Secrets Manager | 1 / 3 |
| `production` | managed (RDS, ElastiCache) | TLS ingress (ALB) | AWS Secrets Manager | 2 / 20 gateway, 2 / 12 processor |

`staging` and `production` reach for managed data services and a secret
manager, so they cannot be stood up on a developer's machine. The resilience
test therefore runs the **production replica counts and disruption
policies** — two replicas of each service, `PodDisruptionBudget`
`maxUnavailable: 1`, the `maxUnavailable: 0` rollout strategy, topology
spread — against the `local` overlay's in-cluster fixtures, on a multi-node
cluster. It is the production posture with stand-in data services, which is
the most of production that runs without a cloud account.

## The test cluster

`make k8s-resilience-test` (script `scripts/k8s-resilience-test.sh`, kind
config `infrastructure/kubernetes/kind/cluster-ha.yaml`) builds:

```
control-plane   PostgreSQL + Redis fixtures (pinned here, tainted off for the app)
worker-1        api-gateway + processor replicas
worker-2        api-gateway + processor replicas
```

The control-plane is tainted `NoSchedule` and the two stateful fixtures are
pinned to it, so **draining a worker can never destroy a fixture's emptyDir
data** — the node-disruption scenario is about the application rescheduling,
not about losing the database. Calico enforces the NetworkPolicies (kind's
default CNI does not), and a metrics-server gives the HPA a real CPU source.
Two application replicas of each service run on the two workers, spread by
`topologySpreadConstraints`.

Run it with `--keep` to leave the cluster up for a look around; without it
the cluster is deleted at the end.

## What was verified

Each row is one scenario, the disruption it injects, and what was observed.
The four properties every row also holds to: readiness stays truthful, no
healthy pod restarts, the database and job state stay correct, and no
request that should have been served was dropped.

| Scenario | Injection | Observed |
|---|---|---|
| **Readiness, spread, PDB** | steady state | `/ready` 200 through the Service; the two api-gateway replicas spread across both workers; each PDB reports one allowed disruption; metrics-server reports pod CPU, so the HPA has a live source |
| **Rolling restart** (gateway, then processor) | `kubectl rollout restart` | the rollout completed and **not one of the requests sent through the Service during it was dropped** — `maxUnavailable: 0` brings the new pod to Ready before the old one is touched, and the 5s preStop pause holds the old one open while its endpoint is withdrawn |
| **Pod deletion** (gateway, then processor) | `kubectl delete pod` | the Service **never fell below one endpoint**; a replacement reached Ready and the Deployment returned to two; a processing job still completed end to end afterwards |
| **Node disruption** | `kubectl drain` one worker | the drain completed; the PDB **held the line at one ready replica throughout** (it never reached zero for either Deployment); no request was dropped; both Deployments rescheduled to two Ready on the remaining worker; the patient's measurements were unchanged; the node was uncordoned |
| **Processor scaling** | raise the HPA floor | the HPA scaled the processor from two to four Ready replicas, the new pods passing their probes and joining the Service |
| **Gateway scaling** | CPU load, then raise the floor | the HPA had **already scaled the gateway above its floor of two on CPU** during the load-heavy earlier scenarios — metric-driven scale-up, observed — and the floor change then drove it to four Ready |
| **Redis outage** | NetworkPolicy drops Redis's port | `/ready` stayed 200 (Redis is not a readiness dependency); reads, sign-in and writes kept working; the per-replica fallback limiter still refused excess sign-in attempts (15 of 75 at one replica, the sixty-per-minute anonymous limit doing its job per pod); the gateway did not restart |
| **Database outage** | NetworkPolicy drops PostgreSQL's port + `pg_terminate_backend` | `/health` kept answering (liveness consults no dependency); `/ready` turned 503 and the pod left the Service's endpoints; after 65 seconds without a database the pod had **not restarted** and was still `Running`; when the database returned, `/ready` was 200 again within about a second and the endpoint came back |
| **Integrity** | after every scenario above | the measurement count was unchanged; exactly one patient row (no retry made a duplicate); **no job left `PENDING` or `PROCESSING`**; every surviving pod's restart count was zero |

## The behaviours, in detail

### Readiness is two questions, not one

`/health` (liveness) asks only whether the process is answering and consults
no dependency; `/ready` (readiness) asks whether the dependencies a request
needs are usable. This split is what the database-outage scenario turns on:
liveness stays green so the kubelet does not restart a pod for something a
restart cannot fix, while readiness turns red so the pod leaves the
Service's endpoints and callers get no route rather than a slow error. The
processor's `/ready` is different again — it reflects lifecycle, turning
false the instant shutdown begins, which is what takes a draining pod out of
rotation before it stops accepting.

### Rolling deployments drop nothing, by construction

Three settings act together and the rolling-restart scenario fails if any is
missing: `maxUnavailable: 0` keeps the ready count from dipping below
desired during a rollout (the new pod is Ready before an old one is
removed); `maxSurge: 1` gives it somewhere to add that pod; and the 5-second
`preStop` sleep holds a terminating pod open while its endpoint removal
propagates, closing the race where a proxy still routes to a pod that has
stopped accepting. The gateway's `terminationGracePeriodSeconds` is 30 and
the processor's is 60 — long enough for `HTTP_SHUTDOWN_TIMEOUT` (gateway) or
a running job within `SHUTDOWN_TIMEOUT` (processor) to finish, plus the
preStop pause, without letting a wedged pod hold a rollout open for long.

### Involuntary loss is covered by running two of everything

Deleting a pod is the fast version of a node failure Kubernetes did not get
to drain. Because each Deployment runs at least two replicas spread across
nodes, the Service kept serving from the surviving replica while the deleted
one was replaced. This is the property that the single-replica `local`
overlay cannot demonstrate and this test can.

### A node drain is gated by the PodDisruptionBudget

`kubectl drain` evicts through the eviction API, which honours the PDB.
`maxUnavailable: 1` means the drain takes the workers' pods **one at a
time**: it evicted one replica, waited for its replacement to be Ready
elsewhere, then took the next — so neither Deployment ever had both replicas
down at once, and nothing was dropped. The budget is written
`maxUnavailable: 1` rather than `minAvailable: 1` on purpose: expressed as
`maxUnavailable` it scales with the HPA and always lets a drain make
progress, where `minAvailable: 1` against a single replica would block a
drain outright. `unhealthyPodEvictionPolicy: AlwaysAllow` is the other half:
during a database outage every gateway pod is not-ready at once, and the
default `IfHealthyBudget` would then refuse to evict any of them — blocking
a node drain on pods that are serving no traffic. `AlwaysAllow` keeps a pod
that cannot serve from also being undrainable.

Fixtures are pinned to the control-plane in this test so a worker drain is
safe; in staging and production the data services are managed and off-cluster,
so this concern does not arise there.

### Autoscaling scales on CPU, up fast and down slow

Both services carry an HPA targeting 70% CPU of the request, `minReplicas: 2`,
`maxReplicas` per environment. Scale-up has a zero stabilization window and
an aggressive policy (double, or +2 pods, per 30s): the cost of an extra pod
is small and the cost of queuing behind a saturated one is paid by every
caller. Scale-down is deliberately slow — a 300-second window for the
gateway, 600 for the processor, because removing a processor pod mid-job
cancels the work, so scale-down waits out a job rather than interrupting one.
The test observed the gateway's HPA raise the Deployment on real CPU under
load without any manual trigger. CPU is the only metric; memory was left out
on purpose (a Go runtime's resident memory is a ratchet, not a signal, and
the processor's memory is bounded by `MAX_CONCURRENT_JOBS` and near-constant)
— the reasoning and the seam for a custom queue-depth metric are in
`horizontalpodautoscaler.yaml`.

### Redis is a degradation, the database is an outage

Readiness ignores Redis on purpose: with it gone, reads, sign-in and writes
all kept working, and rate limiting fell back to a per-replica counter
(a weaker guarantee than the shared Redis window — the limit is enforced per
pod rather than across the deployment — but not an absent one). PostgreSQL is
the opposite: readiness depends on it, so an outage withdraws the pod from
rotation, and recovery needs no restart because the pool reconnects on its
own. Neither outage restarted a pod.

### Nothing is corrupted, and no job is stranded

Across every scenario the measurement count held, no duplicate patient row
appeared, and no job was left non-terminal. A job in flight when its gateway
is disrupted is covered by the lease sweep (see RESILIENCE.md): a job still
`PROCESSING` after its lease expires is failed with `PROCESSING_INTERRUPTED`
rather than stranded. Retries carry the job id the gateway created, so a
repeat re-runs rather than duplicates.

## Runbooks

### Deploy a new version

**In staging and production, do not deploy by hand.** `main` releases itself:
CI, then `release.yml` builds and scans the images, pushes them to ECR by
digest, deploys staging, and runs smoke and end-to-end against it. Production
is a separate manually dispatched promotion of a release that already passed
staging, gated on a reviewer. The commands and the gates are in
[DEPLOYMENT.md](DEPLOYMENT.md); nothing below replaces them.

The manual sequence here is for a **local or test cluster**, and for the case
where the pipeline itself is broken. A deploy is a rolling update; the
manifests are applied server-side.

```sh
kubectl -n <ns> apply --server-side --force-conflicts -f <rendered overlay>
kubectl -n <ns> rollout status deployment/vitalmesh-api-gateway --timeout=180s
kubectl -n <ns> rollout status deployment/vitalmesh-processor  --timeout=180s
```

`--server-side --force-conflicts` matters on the first apply into a new
namespace: the base declares the namespace's `default` ServiceAccount to turn
token automounting off, and a client-side apply races the ServiceAccount
controller for it. Watch that both rollouts report success; `maxUnavailable: 0`
means a rollout that cannot bring a new pod to Ready stalls rather than
dropping capacity, so a stuck rollout is safe but must be investigated (a bad
image tag surfaces as `ImagePullBackOff`, which is why the base pins an
unpullable placeholder tag that an overlay must override).

### Roll back

**The rollback is a deployment of an earlier release, not an undo.** The full
procedure, the verification commands and the limits on how far back you can
go are in [ROLLBACK.md](ROLLBACK.md), which also records the rehearsal that
exercises them. In production it is a promotion of the last good release; the
commit and both image digests come from that release's record, so the
Kubernetes manifests are restored along with the images.

`kubectl rollout undo` is the **emergency stop**, for when the pipeline
itself is the problem:

```sh
kubectl -n <ns> rollout undo deployment/vitalmesh-api-gateway
kubectl -n <ns> rollout undo deployment/vitalmesh-processor
kubectl -n <ns> rollout status deployment/vitalmesh-api-gateway --timeout=180s
kubectl -n <ns> rollout status deployment/vitalmesh-processor  --timeout=180s
```

Roll both services back together: they are released together, and a gateway
from one commit against a processor from another is a combination nothing has
tested.

**What `rollout undo` does not restore, and this is the trap.** It restores
the pod template and nothing else. The ConfigMaps here are named objects
rather than generated ones with a content hash, and the pods read them with
`envFrom`, so a restored pod template points at the same ConfigMap *name* and
picks up whatever it holds now — the configuration of the release you are
rolling back *from*. The rehearsal measures this: after an undo, the gateway
ran the previous image while using the new release's configuration, a pairing
neither release produced. It also reaches back only five revisions
(`revisionHistoryLimit`), and what it reaches back to is ReplicaSet history in
the cluster rather than an artifact, so a rebuilt cluster has none of it.

Use it to stop the bleeding, then redeploy a release properly so the
environment's state is one a run explains.

**The database is not rolled back either.** It is the system of record and
migrations are forward-only; each one must keep the previous version working,
which an integration test enforces migration by migration (see
[DATABASE.md](DATABASE.md)). So the previous code runs against the current
schema, which is safe for as long as no migration has declared a breaking
change — today none has.

### Inspect what a service is doing

Three signals, and one honest gap.

**Logs.** Structured JSON, one line per event, on stdout. Every request line
carries `request_id`, `trace_id`, `method`, `route`, `status` and
`duration_ms`; `route` is the matched pattern, never the path.

```sh
kubectl -n <ns> logs -l app.kubernetes.io/name=api-gateway --tail=200 -f
kubectl -n <ns> logs -l app.kubernetes.io/name=api-gateway --tail=500 |
  jq -r 'select(.status >= 500) | "\(.timestamp) \(.route) \(.status) \(.request_id)"'
kubectl -n <ns> logs -l app.kubernetes.io/name=processor --tail=200 |
  jq -r 'select(.trace_id == "<trace id>")'
```

Take a `trace_id` from a gateway line and grep the processor's logs for the
same value to follow one request across both services. Credentials, tokens,
hashes and reading values never appear; redaction is applied where logs exit,
so it cannot be forgotten at a call site.

**Metrics.** Both services expose Prometheus at `/metrics`, unauthenticated
but restricted at the network to a `monitoring` namespace. A port-forward
reaches it from a workstation:

```sh
kubectl -n <ns> port-forward deploy/vitalmesh-api-gateway 8080:8080
curl -s localhost:8080/metrics | grep -E '^vitalmesh_(build_info|database_schema_version)'
curl -s localhost:8080/metrics | grep '^vitalmesh_http_errors_total'
```

`vitalmesh_build_info` says which build is answering, and
`vitalmesh_database_schema_version` which migration version it read at
start-up. During a rollout, two replicas reporting different values is the
signal that a migration landed between their starts.

**Traces.** Both services propagate W3C trace context and export OTLP when
`OTEL_EXPORTER_OTLP_ENDPOINT` is set. **Nothing collects them in a deployed
environment today** — there is no collector, Prometheus or Alertmanager in the
manifests or in Terraform (see PRODUCTION_READINESS.md, the P1 blocker). So in
staging and production the trace id in the logs is what you have; a browsable
trace exists only in the local compose stack, where a collector runs. Export
to an unreachable endpoint degrades silently and never affects a request.

### Troubleshoot a failing rollout

`maxUnavailable: 0` means a rollout that cannot bring a pod to Ready stalls
rather than dropping capacity: the old pods keep serving, so this is safe but
must be investigated.

```sh
kubectl -n <ns> rollout status deployment/vitalmesh-api-gateway --timeout=60s
kubectl -n <ns> get pods -l app.kubernetes.io/name=api-gateway
kubectl -n <ns> describe pod <pod> | tail -30        # Events say why
kubectl -n <ns> logs <pod> --previous --tail=100      # the crashed container
```

| Symptom | Usual cause |
|---|---|
| `ImagePullBackOff` | the overlay did not override the base's unpullable placeholder tag, or the digest is not in the registry |
| `CreateContainerConfigError` | a `secretKeyRef` names a key the Secret does not have |
| Ready never true, no restarts | readiness is 503: the gateway cannot reach PostgreSQL. Check the database, not the pod |
| `CrashLoopBackOff` on start | configuration rejected at start-up; the first log line says which variable |
| Rollout stalls with the old pods healthy | the new ReplicaSet cannot schedule: check quota, `describe` the pending pod |

The migration Job is a separate failure: it runs before the rollout, and a
failure leaves the schema version marked **dirty**, so later attempts refuse
rather than retry. Clearing it is a manual step against a schema that was
never modified:

```sh
kubectl -n <ns> logs job/vitalmesh-migrate --tail=50
kubectl -n <ns> exec deploy/vitalmesh-api-gateway -- api-gateway migrate force <previous version>
```

### Drain a node for maintenance

```sh
kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
# ... maintenance ...
kubectl uncordon <node>
```

The PDB makes this safe and self-pacing: it evicts one application replica at
a time and waits for each replacement to be Ready before the next, so the
Service keeps serving. If a drain hangs, the cause is almost always a PDB
that cannot be satisfied — check `kubectl get pdb -n <ns>` and its
`ALLOWED DISRUPTIONS`. Pods do not rebalance back onto the node after
`uncordon` on their own; the next rollout or scale event redistributes them.

### Scale, or understand a scaling event

The HPA owns the replica count; do not `kubectl scale` a Deployment it
targets (the change is reverted within about fifteen seconds). To change the
floor or ceiling, patch the HPA:

```sh
kubectl -n <ns> patch hpa vitalmesh-processor --type=merge -p '{"spec":{"minReplicas":3}}'
kubectl -n <ns> get hpa            # TARGETS shows current vs 70% CPU
kubectl -n <ns> top pods           # per-pod CPU, needs metrics-server
```

`TARGETS: <unknown>/70%` means metrics-server is not reporting — the HPA
cannot scale without it, so treat it as an outage of the autoscaler.

### Respond to a Redis outage

Redis is a degradation, not an outage: `/ready` stays green and the API keeps
serving. Expect cache misses (more database load), rate limiting enforced
per-pod rather than across the fleet, and idempotency locks falling back to
the database's own serialisation. No action is required to keep serving;
restore Redis and the client picks it back up within `REDIS_RECOVERY_INTERVAL`
(5s). Do not restart gateway pods in response — it fixes nothing and drops
in-flight work.

### Respond to a database outage

The gateway withdraws itself from the Service (readiness 503) and keeps its
process alive. Callers get "no route", which is the intended degraded state.
Do not restart the pods: recovery needs no restart, and a database outage
that restarted every gateway would only add a cold-start to the recovery.
Fix the database; `/ready` returns to 200 and the endpoints come back within
about a second. If the pods are being evicted for a node drain during the
outage, `AlwaysAllow` lets the drain proceed rather than blocking on
not-ready pods.

## What this test does and does not cover

- **In-cluster fixtures, not managed data services.** PostgreSQL and Redis
  run as single-replica fixtures pinned to the control-plane. Their own
  failover is a property of the managed services in staging and production,
  not of these manifests, and is out of scope here.
- **Node disruption is a drain, not a kernel panic.** `kubectl drain` is
  graceful voluntary disruption, which is what a cluster upgrade or a
  descheduler does. Sudden node loss is covered in principle by running two
  replicas spread across nodes (the pod-deletion scenario is its fast
  analogue), but kind cannot pull the power on a node.
- **One machine.** kind runs every node as a container on one host, so this
  finds scheduling, disruption and autoscaling behaviour, not true
  cross-machine networking or a cloud load balancer's own health checking.
- **CPU-driven HPA.** The autoscaler scales on CPU; a queue-depth metric for
  the processor would describe its load better and is a documented seam
  (`horizontalpodautoscaler.yaml`), not built yet.

## How this is kept honest

`make k8s-resilience-test` runs every scenario above against a fresh
three-node cluster and fails if any check fails; it is the source of the
results table. It complements the two cluster tests that came before it:
`make k8s-local-test` (the manifests apply, admission and NetworkPolicies are
enforced, one rollout is clean) and `make k8s-failure-test` (each dependency
outage at single-replica scale) and `make rollback-test` (a release deployed,
replaced, and rolled back by digest, with every part of the result verified;
docs/ROLLBACK.md). `make k8s-validate` checks every overlay
against the API schema and four linters without a cluster, and runs in CI;
the four cluster tests need `kind` and `kubectl` and are run on demand,
because kind builds its nodes as containers on the host's Docker and so
cannot itself run inside CI's container.
