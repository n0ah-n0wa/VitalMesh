# Final audit: VitalMesh against SPECIFICATIONS.md

An audit of the repository against the 115 sections of `SPECIFICATIONS.md`,
which is authoritative (§114). It was first written at commit `8a74e21` as a
read-only assessment; the two findings it rated high-severity and
production-blocking have since been resolved, and findings 1 and 2 now record
what was built for each, how it is verified, and what about it remains
unproven. Everything else is as first assessed.

**This document does not declare production readiness, and nothing in it
should be read as doing so.** It classifies what the specification asks for
against what the tree contains, and records the gaps with a recommended
action for each. The readiness decision is separate, belongs to a person,
and is discussed only in "[What a readiness decision would still
need](#what-a-readiness-decision-would-still-need)".

**The audit itself changed nothing.** Where the implementation and the
specification disagree, the disagreement is recorded rather than resolved,
and every classification below other than findings 1 and 2 describes the tree
as found. Two repairs were made while auditing: a broken line continuation
that made the documented `make synth-load` command fail to run at all
("[The one repair made](#the-one-repair-made)"), and two error codes the
draft public contract published that the gateway does not define.

**What came afterwards is separate work, and it is marked as such.** §27 and
§104 — the public API contract and deployed monitoring — were then
implemented, because they were the only findings rated High or
production-blocking. Findings 1 and 2 describe the result, including three
latent defects the new tests found and one new gap they opened (finding 12).

## How this was checked

Claims were verified against the code, not against other documents. Where a
requirement names a command, the command was run; where it names a field, a
constant or a migration, the file was read. The verification commands that
back the findings are in
"[Reproducing this audit](#reproducing-this-audit)".

---

## Summary

| # | Area | Specification | Classification |
|---|---|---|---|
| 1 | Go gateway | §6, §9–13, §25–26, §28, §66 | **COMPLETE** |
| 2 | Rust processor | §7, §14–18, §63 | **COMPLETE** |
| 3 | PostgreSQL | §19–22, §64 | **COMPLETE** |
| 4 | Redis | §23, §84 | **COMPLETE** |
| 5 | Authentication | §9.1, §30 | **COMPLETE** |
| 6 | Authorization | §9.1, §30 | **COMPLETE** |
| 7 | Idempotency | §24 | **COMPLETE** |
| 8 | Rate limiting | §29 | **COMPLETE** |
| 9 | API surface | §9–13, §26, §28, §65 | **COMPLETE** |
| 10 | API documentation | §27 | **COMPLETE** |
| 11 | Processing | §13–14, §92 | **COMPLETE** |
| 12 | Algorithms | §15–16, §96 | **COMPLETE** |
| 13 | Docker | §32 | **COMPLETE** |
| 14 | Kubernetes | §33–36, §101–102 | **COMPLETE** |
| 15 | Terraform | §57, §100 | **COMPLETE** |
| 16 | AWS | §58–59, §103 | **COMPLETE** |
| 17 | Production environment | §104 | **COMPLETE** |
| 18 | CI/CD | §60–63, §98 | **COMPLETE** |
| 19 | Observability | §39–42 | **COMPLETE** |
| 19a | Alerting coverage | §39–42 | **COMPLETE** |
| 20 | **Audit logging** | **§43** | **PARTIALLY COMPLETE** |
| 21 | Testing | §44–51 | **COMPLETE** |
| 22 | **Chaos / failure testing** | **§52** | **PARTIALLY COMPLETE** |
| 23 | Security | §30–31, §75 | **COMPLETE** |
| 24 | Resilience | §37–38, §90, §92–93 | **COMPLETE** |
| 25 | Performance | §50–51, §83, §97 | **COMPLETE** |
| 26 | Documentation | §76–78 | **COMPLETE** |
| 27 | Disaster recovery | §79–80 | **COMPLETE** |
| 28 | Data retention | §81 | **COMPLETE** |
| 29 | Privacy | §82 | **COMPLETE** |
| 30 | **Supply chain** | **§99** | **INTENTIONALLY DIFFERENT** |
| 31 | **Repository structure** | **§68** | **INTENTIONALLY DIFFERENT** |
| 32 | Release management | §98 | **COMPLETE** |
| 33 | Demo and dataset | §106, §108 | **COMPLETE** |
| 34 | Backpressure | §91 | **COMPLETE** |
| 35 | Dead letter / failed jobs | §94 | **COMPLETE** |
| 36 | Clock and timestamp validation | §87 | **COMPLETE** |
| 37 | **CLI / developer utilities** | **§107** | **PARTIALLY COMPLETE** |
| 38 | **Acceptance protocol** | **§110** | **PARTIALLY COMPLETE** |
| 39 | **Final quality gate** | **§111** | **PARTIALLY COMPLETE** |
| 40 | **Acceptance criteria / completion** | **§109, §115** | **PARTIALLY COMPLETE** |

Eight of the forty-one rows are not a plain COMPLETE, and **none of them is
an unmet `must`**. Seven are set out below as numbered findings, each with
the exact requirement, what exists, the gap, a severity and a recommended
action; the eighth, row 40, is a checklist over the other rows rather than
an area of its own and is tallied in "[§109 and §115
roll-up](#109-and-115-roll-up)".

Five rows changed after this audit was first written, and each is recorded
in its finding rather than quietly amended: rows 10 (§27) and 17 (§104) were
the High and the production blocker; row 28 (§81) was the last unmet
mandatory requirement; row 36 (§87) followed from it; and row 19a is an
alerting gap that only became visible once something was watching the
alerts fire.

Rows 34 and 35 are marked COMPLETE deliberately rather than by default:
claims of a defect in each were put to this audit and did not survive
verification, and both are set out in "[Claims tested and
rejected](#claims-tested-and-rejected)".

**Severity scale.** *High*: the specification states a hard requirement
(`must`) and nothing satisfies it. *Medium*: a hard requirement is partly
satisfied, or an operational property the system depends on is absent.
*Low*: a soft requirement (`should`), a cosmetic divergence, or something
whose absence is visible and harmless.

---

## 1. §27 API Documentation — COMPLETE

**Exact requirement.** "The public API must be described using OpenAPI",
with complete schemas, request and response examples, authentication, error
responses, pagination, idempotency and versioning. "OpenAPI specification
must be generated or validated automatically in CI."

**Current implementation.** `contracts/openapi/vitalmesh-public-v1.json` is
an OpenAPI 3.0.3 document describing all sixteen served operations: the
fourteen under `/api/v1` and the two unversioned platform probes. It carries
the schemas of every request and response, an example for each, the bearer
scheme with exactly `health`, `readiness` and `login` exempt from it, the
`x-error-codes` and `x-retryable` classification on every failure, the
pagination envelope, the `Idempotency-Key` parameter on the four write
operations that can create duplicate state, and the versioning rules in
`contracts/openapi/README.md`.

It is JSON rather than YAML, which is the deviation from this audit's own
earlier recommendation and follows the repository's existing convention: both
serialisations are valid OpenAPI and every tool reads both, and JSON is
parseable by the standard library, so the gate needs no dependency in either
service (`contracts/internal-api/README.md` sets out the same reasoning).

**How it is validated in CI.** `make contracts-check`, which the `contracts`
job runs, and which now covers both sides of the agreement:

- `internal/contract/public_api_test.go` checks the document against itself
  and against the gateway's source: every operation named, described and
  accepting the correlation headers; every envelope failure declaring its
  codes and retry classification; every example validating against its
  schema; every `$ref` resolving; every component referenced and documented;
  authentication applied by default with only the three intended exemptions;
  `Idempotency-Key` on exactly the four idempotent writes; and **every
  published error code present in the gateway's non-test Go source**, so a
  code renamed in the implementation and not in the contract fails.
- `internal/httpapi/openapi_test.go` checks it against the router: the
  contract's operations and the routes `NewHandler` mounts must be the same
  set, derived from the same `authz.Routes` and `Handlers.operations()`
  tables the router is built from. A second test states the five
  authorization rules that have no handler — the four `/users` operations and
  job cancellation — and requires them to stay absent from the contract, so
  serving one becomes a visible decision.
- A fingerprint lock (`vitalmesh-public-v1.lock.json`) makes any interface
  change fail until it is reviewed and re-recorded with `make contracts-lock`.

**What the gate caught while being written**, which is the evidence that it
is a gate rather than a formality: the first draft published two error codes
the gateway does not define, `NOT_READY` and `SERVICE_UNAVAILABLE`. Both were
inventions of the contract; the document was corrected rather than the code.
The gate was then proved to fail in both directions — an endpoint documented
but not served, an endpoint served but not documented, and a renamed error
code each fail the relevant test.

**Remaining deviation.** One response in the document is not the shared error
envelope: `GET /ready` reports a failing dependency in the same `Readiness`
body it reports a healthy one, because that is what the handler does. The
test encodes that as a narrow, named exception rather than a general
loophole, so any other non-envelope failure still fails.

---

## 2. §104 Production Environment — COMPLETE, with one clause configured and unproven

**Exact requirement.** Production configuration must use private managed
database infrastructure, managed Redis, HTTPS, restricted network access,
separate secrets, production-specific IAM roles, **enable monitoring**,
enable backups and enable auditability.

**Current implementation.** Eight of the nine were already satisfied and
verified: RDS private and encrypted with TLS enforced, ElastiCache managed
with transit encryption required, the ALB terminating TLS 1.2+ with an HTTP
redirect, security groups admitting only the cluster, per-environment secrets
in Secrets Manager, `vitalmesh-production-deploy` as a distinct role, RDS
automated backups with 14-day retention and a required final snapshot, and
CloudTrail plus five CloudWatch log groups for auditability.

"Enable monitoring" was the one that stopped, and it is the blocker
`docs/PRODUCTION_READINESS.md` raised. It is now built:
`infrastructure/kubernetes/monitoring/` creates the `monitoring` namespace
the application manifests have always assumed, under the restricted Pod
Security Standard, and runs Prometheus, Alertmanager and an OpenTelemetry
Collector in it. Prometheus discovers both services through the Kubernetes
API, evaluates the same recording rules and the same nine alerts the local
stack ships, and posts to Alertmanager. An IRSA role
(`alertmanager_role_arn`, `sns:Publish` on one topic) and
`scripts/eks-platform-install.sh` route delivery to the environment's alarm
topic — the same topic the RDS and ElastiCache alarms use.

It is this repository's own manifests rather than a Helm chart so that it
passes `make k8s-validate` like everything else; the component that watches
the system should not be the one component nothing checks.

**How it is verified.** `make k8s-monitoring-test` on a kind cluster running
Calico, so the NetworkPolicies are enforced rather than accepted and ignored:
both services discovered and scraped, proven by a pod label a static target
could not carry; the collector scraped per pod; recording rules producing
series; all nine alerting rules loaded with no evaluation error; spans
arriving at the collector; and then a real failure — the gateway's Service
repointed at a port nothing listens on, so the pod stays Ready and its
endpoint stays in discovery while the scrape is refused — firing
`ServiceDown` after its two-minute `for` clause and **arriving at
Alertmanager**, then clearing when the Service is restored. `make k8s-validate` additionally runs
`promtool check rules` over both copies of the rules and fails if they drift.

**Three latent defects the testing found**, none of which was visible while
nothing consumed the metrics:

1. the scrape config was pinned to a namespace called `vitalmesh`, which no
   overlay uses — every environment renames it — so it would have discovered
   nothing in staging or production;
2. the egress policy allowed the Kubernetes API on 443 only, but kube-proxy
   rewrites the destination to the API server's real port before the policy
   is applied, which is 6443 on kind;
3. staging and production pointed `OTEL_EXPORTER_OTLP_ENDPOINT` at
   `https://`, and the collector serves plaintext OTLP — as the gateway
   already reaches the processor over plain `http://` in-cluster.

**Gap.** SNS delivery itself has never been exercised. The receiver, the IRSA
role and the topic are configured, and the install script refuses to run when
either Terraform output is missing, so a cluster cannot end up with an
Alertmanager that notifies nobody. But no message has been published to a
real topic, because there is no account. Two further limits: Prometheus keeps
fifteen days on an `emptyDir`, so history does not survive the pod, and the
collector exports spans to `debug` rather than to a trace store.

**Severity.** **Low**, down from Medium. What §104 asks for in three words
exists and is tested to the edge of the account boundary; what remains is one
hop that cannot be tested without an account, and it is stated wherever it
matters rather than implied.

**Recommended action.** On the first real install, publish a test message to
the alarm topic and confirm a subscriber receives it —
`scripts/eks-platform-install.sh` prints that instruction on the way out.
Attach remote write to Amazon Managed Prometheus if history needs to outlive
the pod, and an exporter to the traces pipeline if a browsable trace is
wanted; neither is an application change.

---

## 3. §81 Data Retention — COMPLETE

**Exact requirement.** "Synthetic health measurements must have configurable
retention policies." The implementation "must support eventual
archival/deletion without breaking referential integrity", and "retention
jobs must be observable and auditable".

**Current implementation.** All three clauses are met.

*Configurable.* `MEASUREMENT_RETENTION_DAYS` and `RESULT_RETENTION_DAYS`
set the windows in days, with `RETENTION_INTERVAL` and
`RETENTION_BATCH_LIMIT` tuning the sweep. Both windows default to zero,
which removes nothing: deleting data is irreversible, so it happens because
an operator asked for it rather than because nobody set a variable. The
gateway logs which state it is in at startup, so a service quietly keeping
everything says so.

*Removal without breaking referential integrity.* `internal/retention`
sweeps readings and results in bounded batches, deleting by primary key so
the locks stay off the write path. Processing jobs are deliberately not
covered: §94 requires a failed job to keep its diagnosis, and a job row is
small and bounded by how many were requested. Nothing holds a foreign key
to a measurement, and a result keys to its job rather than to the readings
it was computed from, so neither delete can orphan a row. That is asserted
rather than argued —
`TestRemovingReadingsLeavesTheJobAndItsResultsIntact` removes the readings
under a completed job and reads the job and its results back afterwards,
and `TestRemovingResultsLeavesTheJob` does the reverse.

*Observable and auditable.* Each pass records
`vitalmesh_operation_*{operation="retention.sweep"}` with its outcome and
duration, plus the batch sizes `retention.measurements` and
`retention.results`. A pass that removed anything writes the `RETENTION_RUN`
audit entry — the constant that existed for this feature and had no caller
until now — carrying the counts and the windows in force and never an
identifier of anything removed (§41, §43). A pass that removed nothing writes
no entry, so the hourly no-ops do not bury the entries that matter.

**Verification.** Eight unit tests over the sweeper (cutoff arithmetic,
batching, the per-pass budget, audit content and its absence, failure
reporting, cancellation, per-window selection) and five integration tests
against a real PostgreSQL covering the cutoff boundary to the row, both
referential-integrity directions, the batch limit, and an empty window.

**Remaining note.** Retention is off by default, so a deployment that wants
it must set it. That is the intended direction to fail in, and
`docs/OPERATIONS.md` has the runbook for turning it on, including what the
first pass over a backlog does.

---

## 4. §43 Audit Logging — PARTIALLY COMPLETE

**Exact requirement.** Security-sensitive operations must generate audit
records. Nine example actions are named: `LOGIN`, `LOGOUT`,
`PATIENT_CREATED`, `PATIENT_DELETED`, `MEASUREMENT_CREATED`,
`MEASUREMENT_DELETED`, `PROCESSING_JOB_CREATED`,
`PROCESSING_JOB_CANCELLED`, `ROLE_CHANGED`. Records must carry actor,
action, resource, timestamp, request id and metadata, and must be
append-oriented.

**Current implementation.** The record shape is complete: `audit_logs`
carries `actor_id`, `actor_type`, `action`, `resource_type`, `resource_id`,
`request_id`, `metadata` and `created_at`. Append-orientation is enforced at
the database and proven — the stack suite issues an `UPDATE` and a `DELETE`
against `audit_logs` and asserts both are refused. All nine named actions
are defined as constants, plus `USER_CREATED` and `RETENTION_RUN`.

Six of the nine are actually emitted. **Three are not**: `LOGOUT`,
`PROCESSING_JOB_CANCELLED` and `ROLE_CHANGED` are defined and no code path
produces them, because the operations that would produce them are not
served. `/api/v1/auth/logout`, the user-management endpoints and job
cancellation have authorization rules in `internal/authz/routes.go` and no
handler in `internal/httpapi/handler.go`, so no route is registered and each
answers 404 — verified against a running gateway.

**Gap.** Three of the nine example actions are unreachable.

**Severity.** **Low.** §43 introduces the list with "Examples", not as a
required set, and §9–§13 — the sections that actually define the API — do
not require logout, user management or cancellation. The operations are
absent by decision, not by oversight, and `docs/API.md` marks them as
unserved. The gap is that the constants imply capability the system does not
have.

**Recommended action.** Leave the constants and keep the documentation
honest, which it now is; or, if the three operations are wanted, note that
the authorization rules and audit actions are already in place, so the work
is handlers plus tests rather than design. Do not delete the constants
merely to make the list match: they record an intended surface, and
`docs/OPEN_QUESTIONS.md` OQ-04 and OQ-06 explain why it was deferred.

---

## 5. §52 Chaos / Failure Testing — PARTIALLY COMPLETE

**Exact requirement.** "The project **should** include controlled failure
scenarios." Eight examples: PostgreSQL unavailable, Redis unavailable, Rust
processor unavailable, Rust processor timeout, pod restart, network latency,
processing failure, database connection exhaustion. "Expected behavior must
be documented."

**Current implementation.** Seven of the eight are covered by automated
tests that run and pass. The stack suite (`make stack-test`, 21 flows,
verified passing) injects: database failure, Redis failure, Rust processor
unavailable, processor timeout, transient processor recovery, bounded
retries with no duplicate write, database connections severed, processor
restart, gateway restart and gateway killed mid-job. Connection exhaustion
has its own test at
`services/api-gateway/internal/infra/postgres/pool_exhaustion_test.go`. Pod
restart and node disruption are covered again at cluster scale by
`make k8s-resilience-test`. Expected behaviour is documented in
`docs/FAILURE_MODES.md` and `docs/OPERATIONS.md`.

**Gap.** **Network latency injection is not implemented.** There is no
`netem`, `tc qdisc`, Toxiproxy or equivalent anywhere in the tree. Slow
dependencies are exercised only as timeouts, which is the limit case rather
than the interesting middle: a dependency at 300 ms does not time out but
does change queueing, pool occupancy and tail latency.

**Severity.** **Low.** §52 is a `should`, and seven of eight examples are
covered by tests that run in CI.

**Recommended action.** Add one latency scenario to the stack suite using a
Toxiproxy container in front of PostgreSQL or the processor, asserting that
the gateway's own timeouts still bound the request and that the pool does
not exhaust. Alternatively record the absence in `docs/FAILURE_MODES.md` as
a deliberate limit, which is the cheaper honest option and consistent with
how other limits in that document are handled.

---

## 6. §99 Supply Chain Security — INTENTIONALLY DIFFERENT

**Exact requirement.** "The project **should** support: dependency
lockfiles; SBOM generation; container vulnerability scanning; provenance
where practical; signed artifacts where practical. Production deployment
should prefer immutable artifact references."

**Current implementation.** Lockfiles are committed and verified
(`make deps-verify`, `--locked` everywhere). An SBOM is generated per image
in CycloneDX and attached to every release, 90-day retention. Images,
Dockerfiles and lockfiles are scanned by Trivy at HIGH and CRITICAL with
`--exit-code 1`, and the scan runs before the registry login exists, so an
image that fails is never pushed. Immutable references are used throughout:
ECR tags are `IMMUTABLE`, deployments name `sha256:` digests, and `latest`
is never a deployment reference.

**No image signing and no OCI provenance attestation.**

**Gap.** Two of the five bullets are not implemented.

**Severity.** **Low**, and classified INTENTIONALLY DIFFERENT rather than
PARTIALLY COMPLETE because the specification qualifies both with "where
practical" and the decision is recorded with its reasoning:
`docs/SECURITY.md` argues that signing without an admission policy that
refuses unsigned images is ceremony, and `docs/PRODUCTION_READINESS.md`
carries it as accepted risk P6 with its compensating controls.

**Recommended action.** No action required for specification compliance.
If signing is wanted later, the honest order is admission policy first,
then signing, so that a signature is something the cluster checks rather
than metadata nobody reads. Note that `docker compose build` exposes no
attestation flags; `buildx bake` does, which is a build-system change rather
than a workflow tweak.

---

## 7. §68 Repository Structure — INTENTIONALLY DIFFERENT

**Exact requirement.** A recommended tree is given. The section closes: "The
exact directory layout may be refined during implementation, but
architectural boundaries must remain intact."

**Current implementation.** The boundaries are intact — `services/`,
`contracts/`, `infrastructure/terraform/`, `infrastructure/kubernetes/`,
`observability/`, `scripts/`, `docs/`, `.github/workflows/`, the `Makefile`,
`docker-compose.yml`, `README.md` and `SPECIFICATIONS.md` are all where the
specification puts them. Three placements differ:

| Recommended | Actual | Why |
|---|---|---|
| `deployments/docker/`, `deployments/compose/` | `services/*/Dockerfile`, `docker-compose.yml` at the root | each Dockerfile sits with the service it builds; Compose at the root is where `docker compose` looks by default |
| `tests/e2e/`, `tests/integration/` | `services/api-gateway/tests/{e2e,stack}` | the suites are Go packages of that module and use its build tags |
| `tests/load/` | `tests/load/` | unchanged |

**Gap.** None against the specification, which permits refinement. What does
remain is untidiness: `deployments/` and `tests/` still contain placeholder
`README.md` files saying "Not yet populated", describing work that landed
elsewhere.

**Severity.** **Low.**

**Recommended action.** Both placeholder READMEs have since been replaced:
`contracts/openapi/README.md` documents the public contract and its gate,
and `deployments/README.md` now points at where each thing it once promised
actually lives. What remains under this heading is the directory layout
itself, which the specification explicitly permits refining.
`docs/DEVELOPMENT.md` already carries a table naming them as traps, so a
reader is warned; removing them would be better than warning about them.

---

## 8. §110 AI Agent Acceptance Protocol — PARTIALLY COMPLETE

**Exact requirement.** "Before declaring the project complete, an AI agent
must perform a final repository audit." The audit must verify seventeen
named items, "must identify any remaining deviations", and "a green build
alone is not sufficient evidence of completion".

**Current implementation.** Before this document there was no audit
artefact of any kind: `AI Agent Acceptance`, `Acceptance Protocol` and
`section 110` appear nowhere in the tree outside `SPECIFICATIONS.md`
itself, and there is no `docs/AUDIT.md`, `docs/ACCEPTANCE.md` or
equivalent. Two documents overlap the requirement without claiming to
discharge it: `docs/PRODUCTION_READINESS.md` is a sixteen-area review
organised by launch risk rather than by §110's items, and README
"Limitations and known gaps" lists deviations in prose.

**Gap.** This document discharges the audit and the deviation list. The
checklist is walked in "[§110 checklist](#110-checklist)" below. What
remains outstanding is that §110 is a gate on *declaring completion*, and
completion is not declared here.

**Severity.** **Low**, now that the checklist exists and the deviations are
recorded. It was Medium before this document, because the specification's
one concrete deliverable was entirely absent.

**Recommended action.** Keep this document current with the tree: it is
only evidence for the commit it names. Re-run "[Reproducing this
audit](#reproducing-this-audit)" before any completion claim, and record
the result rather than a green CI run, per §110's closing sentence.

---

## 9. §111 Final Quality Gate — PARTIALLY COMPLETE

**Exact requirement.** The project must be reproducible from a clean
checkout, with the local workflow `git clone` → install prerequisites →
`make setup` → `make verify` → `make dev` → run demo, and the cloud
workflow configure AWS → `terraform plan` → `terraform apply` → deploy
application → run smoke tests → verify observability. "No undocumented
manual steps may be required."

**Current implementation.** Eleven of the thirteen named nodes exist and
were exercised. `make setup`, `make verify`, `make demo`, the documented
prerequisites, `terraform plan` (also automated in `terraform-plan.yml`),
the deliberately-manual `terraform apply`, `deploy.yml` and
`scripts/smoke.sh` are all present, and every manual step is written down
in `infrastructure/terraform/README.md` steps 1–5 and its "Known gaps".

**Gap.** Two named nodes do not resolve.

1. **`make dev` now exists**, as an alias for `make up`, so the workflow
   the specification writes out can be followed literally. It was missing,
   and `docs/IMPLEMENTATION_PLAN.md` had promised it, so the repository
   contradicted itself.
2. **"verify observability" has something to verify now, and no step that
   does it.** This node was unsatisfiable when the audit was written:
   nothing scraped `/metrics` or evaluated `alerts.yml` in a deployed
   environment, so there was nothing to check. Finding 2 changed that — a
   cluster installed from `infrastructure/kubernetes/monitoring` has a
   Prometheus with targets and rules, and `scripts/eks-platform-install.sh`
   prints the commands to look at both. What is still missing is an
   automated step: `scripts/observability-smoke.sh` checks the *local*
   stack against `localhost:9090/3000/8888`, and `deploy.yml` has no
   observability check at all, so a deploy that silently stopped being
   scraped would not be noticed by the pipeline.

**Severity.** **Low** for both. `make dev` is a missing alias and a stale
planning document. The observability node is now a manual check where an
automated one would be better, rather than a check that cannot be made.

**Recommended action.** Add `dev: up` as an alias with a `##` help line, and
correct the two `IMPLEMENTATION_PLAN.md` references. For the second, give
`observability-smoke.sh` a target argument so it can point at a
port-forwarded Prometheus, and call it from `deploy.yml` after the rollout
with a check that both services are healthy scrape targets — which is what
`make k8s-monitoring-test` already asserts on kind.

---

## 10. §107 CLI / Developer Utilities — PARTIALLY COMPLETE

**Exact requirement.** "The project **should** provide a developer CLI or
scripts capable of": generate demo data; create test users; seed database;
submit processing jobs; inspect job status; run benchmarks; run smoke
tests. The tooling must be safe against local and staging environments, and
production destructive operations must require explicit safeguards.

**Current implementation.** Five of the seven capabilities are fully
served, and the two safety clauses are met strongly. `synth generate`,
`synth load --users`, `api-gateway users create`, `synth load` and `synth
load --jobs` cover generation, users, seeding and job submission.
`internal/synth/target.go` requires three-way agreement between
`--environment`, the URL shape and what the gateway reports on `/health`;
production additionally requires both `--allow-production` and
`VITALMESH_SYNTH_ALLOW_PRODUCTION` set to the exact target host, and
credentials come only from the environment, never from flags.

**Gap.** Three items, in descending order of consequence.

1. **`make synth-load` was broken.** `Makefile` line 391 carried a literal
   two-character `\n` where a line continuation belonged, so the recipe was
   one physical line and `sh` reduced the escape to a bare word `n`. The
   CLI rejected it — `unexpected argument "n"`, exit status 2 — meaning the
   documented seeding command (`README.md` line 698) failed outright. **This
   was repaired in this change**; see "[The one repair made](#the-one-repair-made)".
2. **No way to inspect an arbitrary job's status.** Neither `synth` nor
   `api-gateway` has a status subcommand. `synth load` reports each job it
   submitted, and `scripts/demo.sh` polls the endpoint inline, but an
   operator holding a job id has only `curl`.
3. **Benchmarks and smoke tests are now reachable from `make help`.**
   `make bench` runs the Go benchmarks and the Criterion suites, and
   `make smoke` runs `scripts/smoke.sh` against a deployed gateway. Both
   existed and neither was discoverable.

**Severity.** **Low.** §107 is a `should`, both safety clauses are met, and
every capability exists in some reachable form. The broken recipe would
have been Medium had it not been fixed: it was a documented command that
could not run.

**Recommended action.** Add `bench:` and `smoke:` targets wrapping the
commands already documented, so `make help` lists them. A job-status
subcommand is optional; if added, it belongs in `cmd/synth` beside the
existing target-safety checks rather than in the gateway binary.

---

## 11. §87 Clock and Timestamp Validation — COMPLETE

**Exact requirement.** The system must reject or explicitly handle
impossible timestamps, excessively future timestamps, and "timestamps
outside configured retention windows". Server-side timestamps must come
from trusted server clocks.

**Current implementation.** The first two bullets and the clock clause are
met. `internal/measurement/measurement.go` parses `recorded_at` with
`time.Parse(time.RFC3339Nano, …)`, which requires an explicit UTC offset,
then rejects anything before `EarliestRecordedAt` (1900-01-01) or beyond
the future skew allowance. `requested_at`, `started_at`, `completed_at`,
`failed_at` and the audit timestamps are all server-generated.

**Gap, as it now stands.** The third bullet asks the system to "reject or
explicitly handle" timestamps outside configured retention windows, and
since finding 3 it does the second: a reading older than
`MEASUREMENT_RETENTION_DAYS` is accepted, stored, and removed by the next
sweep, which is an explicit and audited disposal rather than a silent one.

Rejecting such a reading at ingest instead would fail faster, and was not
done: it would make the API refuse data whose acceptability depends on a
window an operator can change at any time, so the same request would
succeed or fail depending on configuration the client cannot see. What is
left is the smaller point that the lower bound is a package-level
`var EarliestRecordedAt` fixed at 1900-01-01 rather than configuration.

**Severity.** **Low**, and consequential rather than independent — it
resolves when §81 does.

**Recommended action.** Nothing further is required for the specification.
If ingest-time rejection is wanted later, it belongs at the same point that
already applies the 1900 bound, and it should be a deliberate API decision
rather than a side effect of a retention setting.

---

## 12. §39–§42 Alerting coverage — COMPLETE

**Exact requirement.** §40 requires metrics for request rate, error rate,
latency and saturation, and §42 requires the system to surface failures. The
alert rules are how a failure reaches a person rather than a dashboard
nobody is looking at.

**What was wrong.** `ServiceDown` is `up == 0`, which matches a target that
exists and fails its scrape. A service that has gone entirely takes its
endpoints with it, so the target leaves discovery and its `up` series stops
existing rather than becoming zero — and an `x == 0` expression matches
nothing when `x` is absent. The total outage, which is the worst case, was
the one case the availability alert could not see.

This was found while writing `make k8s-monitoring-test`, whose first
version injected its failure by scaling the processor to zero and waited
for an alert that never came.

**Current implementation.** Two `absent()` companions, `GatewayAbsent` and
`ProcessorAbsent`, both `critical`, in the availability group of both rule
sets: `for: 1m` locally and `for: 5m` deployed, long enough that a rolling
update or a node drain replacing every replica does not page. `absent()`
has to name each series explicitly, so a new service means a new clause;
that is the cost of covering the case at all, and it is why these are two
rules rather than one.

The repository now ships eleven alerts, and the count is asserted rather
than stated: `EXPECTED_ALERTS` in `scripts/k8s-monitoring-test.sh` lists all
eleven and the test fails if the running Prometheus has not loaded every
one of them.

**Verification.** `promtool check rules` over both copies, the drift check
confirming eleven alerts matched between them, and the kind test confirming
all eleven load and evaluate with no rule reporting an error.

---

## Claims tested and rejected

Three findings were put to this audit by a subagent sweep and did not
survive verification. They are recorded because a finding that was checked
and refuted is as much a result as one that held, and because each would
have been a serious defect had it been real.

**"A job stranded in `PENDING` is never swept."** The claim was that
`processing.go` creates a job `PENDING` and only sets `lease_expires_at` on
the move to `PROCESSING`, so a crash in that window leaves a row the sweep
predicate (`WHERE status = 'PROCESSING' AND lease_expires_at <= $1`) can
never match. The predicate is quoted correctly, but it is only the second
half of the sweep. `JobStore.FailExpiredJobs`
(`internal/infra/postgres/jobstore.go` lines 123–134) runs `ExpirePending`
and `FailExpired` in one transaction: `ExpirePending`
(`internal/infra/postgres/jobs.go` lines 129–144) moves `PENDING` rows
older than one lease to `PROCESSING` with a lease that has already expired,
precisely so `FailExpired` can then fail them — the state machine allows
`FAILED` only from `PROCESSING`, which is why the sweep takes two steps.
Both steps are bounded by `limit` and use `FOR UPDATE SKIP LOCKED`.
Covered by `internal/infra/postgres/jobsweep_test.go` and
`internal/processing/sweeper_test.go`. **No gap.**

**"§94 lacks dead-letter metrics and a replay path."** §94 requires
neither. Its text asks that failed jobs preserve diagnostic metadata, that
a failed job not disappear, and that the record include `error_code`, a
safe error message, attempt count, timestamps, service version, algorithm
version, request ID and trace ID, with stack traces kept server-side. All
eight are columns on `processing_jobs` (migration `000006`, lines 9–21) and
all eight are in `jobColumns`. `FAILED` is terminal and nothing deletes
job rows. **§94 is COMPLETE**; the suggested metrics and replay path are
new features, not specification gaps.

**"§91 has no gateway-side concurrent-job admission limiter."** True as
stated and not a gap. §91 requires bounded queues "or equivalent flow
control", with rejection, deferral or explicit queueing when capacity is
exhausted, and forbids unbounded in-memory queues. The processor bounds
concurrency with a `tokio` `Semaphore` whose `try_acquire` never waits
(`src/concurrency.rs`), rejecting at capacity; the gateway dispatches
synchronously inside the request — `internal/processing/processing.go`
contains no goroutine, channel or queue — so no in-memory queue exists to
be unbounded. `metrics.InFlight` is a gauge, not a limiter, which is what
prompted the claim. **§91 is COMPLETE** by the "reject" branch.

One further claim, that the Go side lacks DST-transition and leap-day tests
for `recorded_at` where Rust has them, is factually right
(`internal/measurement/measurement_test.go` tests only the 1900 bound;
`services/processor/src/domain/timestamp.rs` has
`leap_day_and_leap_second_handling`) but is not a gap: the gateway parses
RFC 3339 with a mandatory explicit offset, so there is no local-zone
ambiguity for a DST test to exercise. Such a test would assert the
behaviour of `time.Parse`, not of VitalMesh.

---

## §110 checklist

The seventeen items §110 requires an agent to verify, each marked against
the tree at `8a74e21`. "Verified" means checked in this audit against code,
not against another document.

| # | Item | Verified | Result |
|---|---|---|---|
| 1 | SPECIFICATIONS.md compliance | yes | 41 areas classified above; 11 are not a plain COMPLETE |
| 2 | Architecture consistency | yes | `docs/ARCHITECTURE.md` matches the boundary rules the code enforces |
| 3 | API contract consistency | yes | both contracts locked and tested; the public one is checked against the mounted routes and the gateway error codes (finding 1) |
| 4 | Database integrity | yes | constraints, triggers, state machine and migration up/down tested |
| 5 | Security controls | yes | gosec, trivy, govulncheck, gitleaks, checkov, kube-linter; `make policy-check` binds the gates to `docs/SECURITY.md` |
| 6 | Observability | **partial** | collection, rules and alert delivery built and tested on kind (finding 2); SNS delivery unproven, and `ServiceDown` cannot see a total outage (finding 12) |
| 7 | Test coverage | yes | floors enforced at 70% (changed) and 85% (merged), measured 71.8% / 86.6% |
| 8 | CI/CD | yes | 11 parallel jobs plus an aggregate `ci` gate; promotion is dispatch-only and reviewed |
| 9 | Docker | yes | every base pinned by digest; `make docker-verify` asserts non-root and no shell |
| 10 | Kubernetes | yes | `make k8s-validate`, `k8s-local-test`, `k8s-resilience-test` all run |
| 11 | Terraform | yes (code) | `make tf-validate` including mocked-apply tests; **never applied** |
| 12 | AWS deployment | **partial** | fully automated and never executed against a real account |
| 13 | Documentation | yes | audited command by command in the preceding phase |
| 14 | Performance benchmarks | yes | recorded runs in `tests/load/results/`; no `make bench` wrapper (finding 10) |
| 15 | Failure handling | yes | `docs/FAILURE_MODES.md` maps each §52 failure to an automated test; no latency injection (finding 5) |
| 16 | Dependency security | yes | `make deps-scan` (trivy + govulncheck), `make deps-verify`, Dependabot |
| 17 | Secret scanning | yes | `make secret-scan` (gitleaks, full history), CI job `secrets` |

Deviations are the twelve numbered findings above, the §109 and §115
partials, plus the repository-hygiene items
under "[Cross-cutting observations](#cross-cutting-observations)". Per
§110's closing sentence, none of this is discharged by CI being green: the
items marked partial are partial regardless of build status.

---

## §109 and §115 roll-up

These two sections are checklists over the areas already classified, so
they are tallied rather than given findings of their own.

**§109 Acceptance Criteria** — 44 bullets across application (11),
infrastructure (7), CI/CD (7), observability (6), testing (6) and
documentation (7). **41 are met outright; 3 are partial; none is unmet.**

Three of the six original partials closed with findings 1 and 2: metrics are
collected, alerts are evaluated and delivered to Alertmanager, and the public
API has a machine-readable contract that CI validates. The three that remain
are the same fact three times over — **nothing has run in an AWS account**:
staging deployment is automated and has never been executed, the Terraform is
valid and has never been applied, and production is deployable on paper with
its gaps recorded in `infrastructure/terraform/README.md`.

**§115 Completion Standard** — twelve capabilities a competent team should
be able to exercise. **All twelve now hold**, with one qualification.
Understand, run locally, test, inspect, deploy (mechanism), diagnose, scale,
secure, reproduce, evolve safely and the synthetic-data limitation were
already met; the last is stated at the top of the README and enforced
structurally (no generated names, `.invalid` e-mail domain, seeded
distributions).

**"Monitor it" now holds in a deployed environment**, which is what finding 2
changed: a cluster installed from `infrastructure/kubernetes/monitoring`
scrapes both services, evaluates every rule and delivers a firing alert to
Alertmanager, and that path is tested rather than asserted. The
qualification is the last hop — nothing has published to a real SNS topic,
because there is no account — and the narrower gap in finding 12, that the
availability alert cannot see a service that has vanished entirely.

---

## The one repair made

This audit changed exactly one line of the tree, and added no
functionality.

`Makefile` line 391 held a literal `\n` — backslash then `n`, two
characters — where a backslash-newline continuation was intended, so the
`synth-load` recipe was a single physical line and `sh` collapsed the
escape into a bare word `n` in the argument list. It was repaired to a real
continuation and proved in both directions:

```sh
# before: the stray argument is rejected outright
go run ./cmd/synth load --from ../../.synth/default n --target http://127.0.0.1:1 \
  --environment local --users --jobs
#   unexpected argument "n"   (exit status 2)

# after: parsing succeeds and the target check is reached
go run ./cmd/synth load --from ../../.synth/default --target http://127.0.0.1:1 \
  --environment local --users --jobs
#   the gateway at 127.0.0.1:1 is not answering: ... connection refused   (exit status 1)
```

It is recorded here rather than left for later because it is a defect in a
command the README advertises, not a gap in the specification, and because
an audit that left a documented command broken while reporting on
documentation accuracy would be reporting on itself incorrectly.

The repaired command was then run end to end against the Compose stack, which
is what §107's "seed database" actually asks for:

```text
target http://host.docker.internal:8080: api-gateway dev, environment local
users: 5 created, 0 already there
patients: 3 created, 0 already there
measurements: 144 stored, 0 already there, in 3 batches
jobs: 3 completed, 0 failed
```

All three jobs reached `COMPLETED` through the real processor, so the
seeding path exercises accounts, patients, batched readings and processing
in one run.

A sweep for the same defect across every `Makefile` and shell script found
no other instance: the remaining 169 occurrences of `\n` are all inside
quoted `printf` format strings, plus one intentional `sed` replacement in
`scripts/rollback-test.sh` line 262 that was exercised during the rollback
rehearsal.

---

## Cross-cutting observations

These are not specification gaps. They are properties of the repository that
an auditor should see stated.

**The process in the planning documents was not followed.** All 38 items in
`docs/OPEN_QUESTIONS.md` are still marked `OPEN`, no ADR was ever written,
and `docs/adr/` does not exist — yet the system was built through the final
planned phase. Both planning documents said each question must become an ADR
before the phase it affects begins. The decisions were made, and are
recorded in the code and in the documents that explain it, but not where the
process said they would be. Both files now carry a banner saying so.

**Nothing has ever been deployed.** The Terraform has never been applied;
CI runs `plan` only, under a read-only role, and there is no apply role
anywhere. The Kubernetes manifests have been exercised on local `kind`
clusters for deployment, disruption and rollback, and validated by
kubeconform, kube-linter, Trivy and Checkov, but no AWS environment exists.
Every AWS claim in this audit is a claim about configuration, not about a
running system.

**Two things in the tree are visibly unfinished and known.** The ingress
domain is a placeholder that never resolves (`api.vitalmesh.example`), and
production refuses to plan until it is set — OQ-18. There is no `LICENSE`
file, which matters for a repository presented as open source.

**Three placeholders outlived what they described.** `deployments/README.md` still says "Not yet populated; Dockerfiles and
Compose arrive with the first service that needs PostgreSQL and Redis",
though the Dockerfiles have long since landed in `services/*/` and the
Compose file at the repository root, leaving the directory an empty shell.
`docs/IMPLEMENTATION_PLAN.md` lines 354 and 658 still promise a `make dev`
target that was never written. The README documentation index omits six
documents that exist: `LOAD_TESTING.md`, `PERFORMANCE_BASELINE.md`,
`PERFORMANCE_OPTIMIZATIONS.md`, `GITHUB_CONFIGURATION.md`,
`IMPLEMENTATION_PLAN.md` and `OPEN_QUESTIONS.md`. None of these breaks
anything; all three send a reader somewhere that is not there.

**Commit hygiene is good but the commits are large.** §69 asks for focused,
reviewable, independently understandable commits and warns AI agents against
giant unrelated commits. The prefixes follow the recommended vocabulary. The
commits are thematically focused but individually large, each covering a
whole phase.

---

## What a readiness decision would still need

**This audit does not make that decision and does not recommend it.** For
whoever does, these are the things this audit found that bear on it,
in the order they would block:

1. **A first real deployment.** Nothing has been applied to AWS. This is now
   the largest item by some distance, and several smaller ones are only
   unproven because of it: SNS alert delivery has never published to a real
   topic, and the restore test ran against a local PostgreSQL rather than
   RDS, so the recovery time objective is an estimate and is labelled as one.
2. **`ServiceDown` cannot see a total outage** (finding 12). The alert most
   likely to matter is silent on the failure most worth catching. It is a
   small change and it is not made here.
3. **Data retention** (§81), if the deployment is meant to run unattended.

Monitoring collection and the public API contract were the first two entries
on this list when it was written. Both are now built and tested, which is why
they are not here; what remains of each is recorded in findings 1 and 2
rather than treated as closed.

Everything else this audit found is either accepted with a compensating
control, argued in `docs/PRODUCTION_READINESS.md`, or cosmetic.

---

## Reproducing this audit

The gate commands, all of which pass at `8a74e21`:

```sh
make verify              # every code gate, coverage floors included
make sast deps-scan secret-scan policy-check
make docker-build docker-verify docker-scan sbom
make k8s-validate tf-validate
make contracts-check      # both API contracts, against themselves and the code
make k8s-monitoring-test  # scrape, rules, traces and alert delivery, on kind
make stack-test          # 21 flows including the failure injections
make db-restore-test     # point-in-time recovery, verified
make rollback-test       # the rollback rehearsal, on a kind cluster
make demo                # the twelve-step end-to-end demonstration
```

The findings that are not gate output were checked directly:

```sh
# §27: no public contract document
ls contracts/openapi/

# §43: which audit actions are emitted, as opposed to defined
grep -rn "AuditLogout\|AuditRoleChanged\|AuditProcessingJobCancelled" \
  --include=*.go services/api-gateway/internal | grep -v entities.go

# §43/§10: the unserved operations answer 404
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/v1/users

# §52: no latency injection anywhere
grep -rli "netem\|toxiproxy\|tc qdisc" scripts/ services/ tests/

# §81: RETENTION_RUN is defined and never emitted; no retention config
grep -rn "RETENTION" services/api-gateway/internal/config/config.go

# §87: the lower time bound is a fixed var, not configuration
grep -rn "EarliestRecordedAt" services/api-gateway/internal/measurement/

# §94: every required failure-record field is a column
grep -n "jobColumns = " -A 3 services/api-gateway/internal/infra/postgres/jobs.go

# §94: stranded PENDING jobs are swept (two steps, one transaction)
grep -n "ExpirePending" -A 16 services/api-gateway/internal/infra/postgres/jobs.go
grep -n "func (s *JobStore) FailExpiredJobs" -A 12 services/api-gateway/internal/infra/postgres/jobstore.go

# §91: the processor rejects at capacity; the gateway never queues
grep -n "try_acquire" -B 4 services/processor/src/concurrency.rs
grep -nE "go func|chan |queue" services/api-gateway/internal/processing/processing.go   # no matches

# §107: the repaired recipe expands to a real continuation
make -n synth-load

# §110: no audit artefact predates this document
grep -rlE "AI Agent Acceptance|Acceptance Protocol" --exclude=SPECIFICATIONS.md --exclude=FINAL_AUDIT.md .   # no matches

# §111: there is no make dev target
grep -nE "^dev:" Makefile   # no matches; make up is the equivalent

# §27: the public contract describes exactly the mounted routes, and every
# code it publishes exists in the gateway
cd services/api-gateway && go test ./internal/contract/ ./internal/httpapi/   -run "PublicContract|UnservedAuthorizationRules" -v

# §104: the deployed and local rule sets agree, and both parse
sh scripts/k8s-validate.sh 2>&1 | sed -n "/Prometheus rules/,/^$/p"

# §104: what the monitoring stack is, before running anything
kustomize build infrastructure/kubernetes/monitoring | grep -E "^kind:" | sort | uniq -c

# finding 12: ServiceDown matches nothing when the series is absent
#   up == 0          fires when a target exists and fails
#   absent(up{...})  is what would fire when it has gone entirely
grep -n "alert: ServiceDown" -A 3 observability/prometheus/rules/alerts.yml
```
