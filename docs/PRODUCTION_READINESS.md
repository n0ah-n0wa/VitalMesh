# Production readiness

A review of VitalMesh against the sixteen areas a launch decision rests on,
with the remaining risks ranked by what they would cost on the day. It is
written to be argued with: every claim names the file or the test behind it,
and anything not verified is marked as an assumption rather than a finding.

**Verdict.** The application and its infrastructure are in good shape: the
failure behaviour, the security posture, the deployment path and the backups
have each been reviewed and, where it was possible, tested. **The blocker
this review originally raised — that nothing collected the application's
metrics or delivered its alerts — has been closed**, and the section below
records what was built and what about it is still unproven. Everything else
on the list is fixed, accepted with a compensating control, or scheduled.

This document still does not declare the system production-ready. Closing
the blocker removes the reason a launch was impossible; it does not by
itself make one advisable, and nothing here has ever run in a cloud
account.

## Fixed in this review

### P1 — Nothing collected the application's metrics or delivered its alerts

**What was wrong.** Both services served `/metrics`,
`observability/prometheus/rules/alerts.yml` defined nine alerts, four Grafana
dashboards existed, the staging and production ConfigMaps pointed
`OTEL_EXPORTER_OTLP_ENDPOINT` at `otel-collector.monitoring.svc.cluster.local:4318`,
and `networkpolicy-baseline.yaml` admitted scraping from a namespace called
`monitoring`.

Nothing created any of it. There was no `monitoring` namespace, no collector,
no Prometheus, no Alertmanager. On launch: `/metrics` served and never read;
nine alert rules that nothing evaluated; traces exported to a name that did
not resolve, which the gateway degraded over silently and correctly.
Infrastructure was watched — CloudWatch alarms on RDS and ElastiCache, an RDS
event subscription, all to SNS — so the database and cache had an owner. The
application did not. The first sign of a bad deploy would have been a user
telling someone.

**Why this review did not build it, and what changed.** The original entry
argued: "It is a platform installation that needs a cluster and an AWS
account to validate, and an untested monitoring stack is worse than a
documented gap: it looks like coverage." The first half of that is right and
the second half is the standard it set for itself. What it missed is that a
cluster was already available. `kind` runs a real API server, a real
admission chain and a real CNI enforcing NetworkPolicies, which is enough to
exercise every hop that was missing. The AWS account is only needed for the
last one, SNS delivery, and that one is still called out below rather than
claimed.

**What was built.** `infrastructure/kubernetes/monitoring/`, a kustomization
of this repository's own manifests rather than a Helm chart, so it passes the
same kubeconform, kube-linter, Trivy and Checkov gates as everything else —
the component that watches the system should not be the one component nothing
checks. It contains the `monitoring` namespace under the restricted Pod
Security Standard, Prometheus with read-only Kubernetes discovery, the
recording and alerting rules, Alertmanager, the OpenTelemetry Collector, and
a default-deny NetworkPolicy with each opening argued.

`make k8s-validate` now also runs `promtool check rules` over both copies of
the rules and compares them: the recording rules must be identical, and the
alerts must have the same names, expressions and severities with deployed
`for` clauses at least as long as the local ones.

**What was tested, on a kind cluster running Calico so the policies are
enforced rather than accepted and ignored** (`make k8s-monitoring-test`):

1. both services discovered through the Kubernetes API and scraped, proven
   by the pod label a static target could not carry;
2. the collector's own metrics scraped;
3. recording rules producing series, which can only happen if the scrape,
   the parse and the group evaluation all worked;
4. all eleven alerting rules loaded, with no rule reporting an evaluation
   error;
5. spans arriving at the collector, which proves the service name resolves
   and the NetworkPolicy on both sides allows 4318;
6. a real outage — the gateway's Service repointed at a port nothing listens
   on, so the pod stays Ready and its endpoint stays in discovery while the
   scrape is refused — firing `ServiceDown` after its two-minute `for` clause
   and **arriving at Alertmanager**;
7. the alert clearing once the Service answered again.

**What is still not proven, and will not be until there is an account.** SNS
delivery. `scripts/eks-platform-install.sh` substitutes the environment's
`alarm_topic_arn` into `infrastructure/kubernetes/platform/alertmanager-sns.yml.template`,
annotates the service account with `alertmanager_role_arn` — an IRSA role
whose policy is `sns:Publish` on that one topic — and **refuses to install at
all if either output is missing**, because an Alertmanager with no receiver
looks exactly like a working one from the outside. Nothing in this repository
has published to a real topic. After the first install, publish a test
message to the topic and confirm a subscriber receives it; the script prints
that instruction on the way out.

Two further limits, stated so they are not discovered later: Prometheus keeps
fifteen days on an `emptyDir`, so history does not survive the pod, and the
collector exports spans to `debug` rather than to a trace store.


### P2 — A migration could stall a hot table for as long as the query in front of it

**What was wrong.** Migrations connected with no `lock_timeout`. A migration
needing `ACCESS EXCLUSIVE` on a busy table — adding a constraint, rewriting
a column — queues behind the queries already running on it, and because
PostgreSQL's lock queue is ordered, every query arriving afterwards queues
behind the migration. A migration that would have taken milliseconds stalls
the table for as long as the slowest query ahead of it, and the application
sees a table it cannot read. The RDS parameter group set
`idle_in_transaction_session_timeout` and `log_lock_waits` but no
`lock_timeout`, and nothing set one per session.

**Fixed.** The migrator now connects with `lock_timeout` (5s,
`postgres.MigrationLockTimeout`) and an `application_name` of
`vitalmesh-migrate`, so a migration waiting on or holding a lock is
identifiable in `pg_stat_activity` as a migration rather than as the
application. Only the wait for the lock is bounded, never the work: a
legitimately long migration such as an index build still runs to completion,
which is why `statement_timeout` is deliberately not set.

**Verified**, not assumed — `TestAMigrationThatCannotTakeItsLockFailsFastInsteadOfBlocking`
holds `ACCESS EXCLUSIVE` on a table from one session and runs a migration
needing it from another. Measured: the migration gives up after **5.02 s**
with `SQLSTATE 55P03`, having changed nothing.

**What the test also established, and the runbook must say.** Recovery is
not automatic. golang-migrate marks the version dirty on any failure, this
one included, so the migration Job's remaining retries all refuse with
"Dirty database version N" rather than succeeding once the lock frees.
Clearing it is one command against a schema that was never modified:

```sh
kubectl -n <ns> exec deploy/vitalmesh-api-gateway -- \
  api-gateway migrate force <previous version>
```

and then the deployment runs again. A deployment that stops and says so
beats a table that stopped serving, but it is a manual step and a pager
needs to know it.

## Accepted, with a compensating control

Each of these is a real limitation, deliberately not closed, with the reason
and what stands in its place.

| # | Risk | Compensating control | Where it is argued |
|---|---|---|---|
| P3 | **Region loss is not covered.** Backups stay in one region; so do the VPC, cluster, cache and secrets | The recovery target is stated as *none* rather than implied. Cross-region backup replication alone would not help, because nothing else exists in a second region | DISASTER_RECOVERY.md |
| P4 | **The RTO is an estimate.** The restore test ran against a local PostgreSQL, not RDS | The *procedure and its verification* are tested, including that a post-target change is absent and constraints survive. Only the duration and the RPO are assumptions, and both are labelled | DISASTER_RECOVERY.md |
| P5 | **Secrets reach the services as environment variables**, readable through `/proc/<pid>/environ` and inherited by children | Accepted rather than suppressed: neither service can read a secret from a file today, so mounting one would produce a file nothing reads. The fix belongs to the configuration layers | SECURITY.md, `.checkov.yaml` |
| P6 | **No image signing or OCI provenance** | Digest-pinned bases, immutable ECR tags, scan-on-push, and the three gates that decide whether an image may exist all run before the registry login exists. An SBOM is attached to every release. Signing without an admission policy that refuses unsigned images would be ceremony | SECURITY.md |
| P7 | **No access-token revocation list** | 15-minute default lifetime with a 24-hour ceiling, and the account's status is re-read from the database when a request loads the current user, so disabling an account takes effect without waiting for expiry | SECURITY.md |
| P8 | **The HPA scales on CPU only**, which describes the gateway well and the processor poorly | The processor's admission control bounds it independently: it refuses work beyond `MAX_CONCURRENT_JOBS` in a millisecond rather than queueing. Queue depth as a custom metric is a documented seam | `horizontalpodautoscaler.yaml` |
| P9 | **Redis is not backed up** | Nothing in it is a system of record: cache entries, rate-limit counters and idempotency locks all reconstruct, and the idempotency claim itself lives in PostgreSQL | DISASTER_RECOVERY.md |
| P10 | **A restored instance's master password can differ from what the cluster holds**, because the password rotates every 7 days | A deployment after a restore is a required step of the procedure, not an optional one | DISASTER_RECOVERY.md |

## Reviewed and sound

These were examined in this review or earlier in the same pass, found
adequate, and are listed so the absence of a finding is explicit rather than
an omission.

| Area | What backs it |
|---|---|
| **Failure modes** | Every dependency outage has an automated test: PostgreSQL, Redis and processor unavailability, processor timeout, transient recovery, bounded retries with no duplicate write, severed database connections, container restarts, and a gateway killed mid-job. 21 flows in `make stack-test` |
| **Deployments and rollback** | `maxUnavailable: 0` with a surge, a preStop pause covering endpoint withdrawal, and a rehearsed `rollout undo`. A rolling restart and a node drain each dropped zero requests in the Kubernetes resilience run |
| **Kubernetes** | Pod Security `restricted` enforced at the namespace, full security contexts, `automountServiceAccountToken: false` in three places, empty RBAC, default-deny NetworkPolicies with link-local excluded. 62 checks passed against a three-node cluster |
| **Autoscaling** | HPAs on both services observed scaling — the processor floor-driven from 2 to 4, the gateway driven above its floor by real CPU — with PDBs holding at least one replica through a drain |
| **Resource limits** | Requests and limits on every container, a namespace `LimitRange` for containers added later, and per-environment `ResourceQuota` derived from the HPA ceilings |
| **Security** | A full hardening pass: authentication, authorization, JWT, password hashing, validation, size limits, rate limiting, idempotency, SQL, Redis and service-to-service all verified directly. One HIGH (a fork pull request reaching the image-push role) and two MEDIUM findings fixed |
| **Secrets** | A `Secret` type in both services that refuses to render itself; no secret in Terraform state or variables; Kubernetes placeholders deleted by the deployed component so an empty object cannot overwrite a live credential |
| **Networking** | Default-deny both ways, DNS the single explicit exception, instance metadata blocked at the NetworkPolicy and again by IMDSv2 hop limit 1 |
| **AWS** | RDS private, encrypted, TLS-enforced with a 1.2 floor; every S3 bucket fully public-access-blocked; no security group admits `0.0.0.0/0`; OIDC trust scoped to exact subjects; no Terraform apply role exists |
| **CI/CD** | Eleven security gates, all green, plus a policy-drift gate that fails if a threshold is lowered, a gate dropped, or a suppression left unargued — proven by running it against a deliberately weakened configuration |
| **Backups** | Automated with configurable retention (14 days production, 3 staging), PITR implemented as code, a final snapshot required, backup failures alarmed through RDS events, and a restore tested and verified row by row |
| **Logging** | Redaction applied at the exit rather than at call sites, so it cannot be forgotten; the one format that bypassed it is now refused in deployed environments |
| **Observability (traces, metrics cardinality)** | No metric label can take a patient, user, request or trace id; span attributes carry route patterns rather than paths. Collection and alert evaluation are installed and tested (P1 above); SNS delivery is configured and unproven |
| **Database migrations** | Applied by a Job before the deployment, each file one implicit transaction, an advisory lock serialising concurrent deploys, expand/contract across two releases for destructive changes, and now a lock timeout |

## How to re-run this review

```sh
make verify              # every code gate, including lockfile integrity
make policy-check        # the security gates still match the documented policy
make sast deps-scan secret-scan docker-scan sbom k8s-validate tf-validate
make db-restore-test     # point-in-time recovery, verified
make stack-test          # 21 flows including every failure injection
make k8s-resilience-test # 3 nodes, 2 replicas: drain, PDB, HPA, outages
make k8s-monitoring-test # scrape, rules, traces, and an alert reaching Alertmanager
make rollback-images rollback-test # the rollback procedure, rehearsed and verified
```

The first three run in CI on every change. The rest need Docker, and the
three Kubernetes ones need `kind`.
