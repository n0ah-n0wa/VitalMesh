# Security

The application security posture of VitalMesh: what protects each part, what
a hardening pass found, what was fixed, and which scanner findings are
deliberately not acted on and why. It is written to be re-run, not just
read — every claim below has a command under "Running the scans" that
reproduces it.

Nothing here is silenced to make a gate green. Where a scanner is wrong
about this repository the reason sits beside the code, and this document
collects those reasons in one place.

## Running the scans

```bash
make sast          # gosec: this repository's own Go code
make deps-verify   # both lockfiles are intact and describe what is imported
make deps-scan     # trivy on the lock files + govulncheck on the Go call graph
make secret-scan   # gitleaks over the whole git history and the working tree
make docker-scan   # trivy on both images and both service Dockerfiles
make sbom          # a CycloneDX bill of materials per image
make k8s-validate  # kustomize, kubeconform, kube-linter, trivy, checkov over the base and every overlay
make tf-validate   # terraform fmt, validate, test (against a mocked AWS), trivy, checkov over every root
make policy-check  # the gates still block what this document says they block
```

All nine run in CI, `deps-verify` as its two per-language halves
(`deps-verify-go` in the `go` job, `deps-verify-rust` in the `rust` job).
`make ci-local` runs them together with the test suites.

## What was reviewed, and the verdict

| Area | Verdict | The controls that matter |
|---|---|---|
| **Authentication** | sound | Argon2id verify on every login; an unknown email is verified against a throwaway hash so the response time does not reveal whether an account exists; the disabled-account check runs *after* the password check, so a disabled account is not enumerable either; no email is ever logged, only a classification and a user id |
| **Authorization** | sound | Deny by default. A route table (`internal/authz/routes.go`) binds every operation to one permission, the HTTP router is built from it, so an operation with no entry cannot be served. The middleware answers 401 when no principal is present, so a route registered without authentication still fails closed. Denials name neither the role nor the permission |
| **Password hashing** | sound | Argon2id, 16-byte random salt, constant-time compare, parameters carried in the PHC string and re-checked against bounds on parse so a hostile hash cannot make verification arbitrarily expensive; concurrent hashes are bounded so a burst of logins cannot exhaust memory; hashes are transparently upgraded when the cost parameters change |
| **JWT** | sound | HS256 fixed — a token naming any other algorithm, `none` included, is rejected before its key is looked up; strict canonical base64; signature verified with `hmac.Equal` *before* the claims are parsed; issuer, subject, role, `jti`, `iat`/`exp` all validated with a bounded clock skew; `kid` allows key rotation with retired keys still verifying; 15-minute default lifetime with a 24-hour ceiling; a 32-byte minimum secret enforced at startup; tokens are never logged |
| **Request validation** | sound | Content-Type enforced, unknown fields rejected, exactly one JSON value per body, rune-based length limits, allowlists and numeric ranges; every field error is reported at once rather than one per round trip |
| **Request size limits** | sound | A declared `Content-Length` over the limit is refused before a byte is read, and the body is wrapped in `MaxBytesReader` so a lying header is caught too. The processor applies its own body limit and refuses an oversized job before parsing |
| **Rate limiting** | sound | Authenticated callers are counted by user id, so changing address does not buy a fresh budget. Anonymous callers are counted by address, and `X-Forwarded-For` is consulted **only** for as many hops as are configured to be trusted, counting from the right — the default is zero, which ignores the header entirely. A caller that writes their own header entries cannot shift the index onto one of them |
| **Idempotency** | sound | The claim is a unique insert in PostgreSQL, so two identical requests serialise there whether or not Redis is reachable; Redis only adds a fast in-progress refusal. The key is scoped to account, method and path; a different body under the same key is refused; a 5xx or a panic releases the claim so a legitimate retry can run |
| **SQL** | sound | Every query is parameterised. Column lists are package-level constants, and the one statement assembled at run time (`ListByPatient`) appends fixed predicate fragments with generated `$n` placeholders; no user-controlled value is ever formatted into a statement. Verified by scanning every `Query`/`QueryRow`/`Exec` call site |
| **Redis** | sound | TLS (`rediss://`) is required in staging and production, refused at startup otherwise; one attempt per call with a short timeout; every dependent feature degrades rather than failing the request; commands are metered by name, never by key |
| **Service-to-service** | sound | The processor requires a bearer credential, compares it in **constant time**, and answers identically whether the credential was absent, malformed or wrong. A deployment without the credential refuses to start. The gateway never repeats the processor's own error text to a client |
| **Logs** | **one fix** | Redaction is applied at the exit — a `ReplaceAttr` hook in Go, the event formatter in Rust — so it cannot be forgotten at a call site. Names and suffixes are matched, JWT shapes scrubbed and values bounded. See the fix below |
| **Metrics** | sound | The HTTP method is folded to a closed set inside the recorder; routes are the patterns the router matched rather than paths, and SQL is reduced to its leading verb at the call site. No port takes a key or an identifier, and the remaining labels are bounded by a convention documented on the interface. No patient id, user id, request id or trace id can reach a label |
| **Traces** | sound | Span attributes carry the route pattern, not the path; the processor's address without a path; database system and operation, never the statement. The Go attribute setter renders an unrecognised type as `<type>` rather than serialising it, so a struct holding a payload structurally cannot reach a span |
| **Secrets** | sound | A `Secret` type in both services refuses to render itself through every common path (`String`, `GoString`, `LogValue`, `MarshalText` in Go; `Debug`/`Display` in Rust). No secret is a Terraform variable or in state — they are ephemeral resources with write-only arguments. Kubernetes ships shape-only placeholders that the deployed component deletes, so an empty object cannot overwrite a live credential |
| **Docker** | sound | Both service images pin their bases **by digest** at every stage, run as an explicit non-root uid, are distroless with no shell and no package manager, carry no build secret, and are checked as *running containers* by `make docker-verify` rather than by reading the Dockerfile |
| **Kubernetes** | sound | Pod Security `restricted` enforced at the namespace, so the pod-level security contexts are backed by admission rather than being a promise; `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `drop: [ALL]`, `seccompProfile` on every container; `automountServiceAccountToken: false` on both ServiceAccounts, on every pod spec and on the namespace's `default` account; RBAC is `rules: []` for both services; NetworkPolicy default-deny with link-local excluded from every egress rule, blocking instance-metadata access |
| **Terraform** | sound | Verified directly: RDS is `publicly_accessible = false`, `storage_encrypted = true`, with `rds.force_ssl` and a TLS 1.2 floor; every S3 bucket has the full public-access block; no security group admits `0.0.0.0/0`; IMDSv2 is `required` with a hop limit of 1, so a pod cannot read node credentials |
| **GitHub Actions** | **one fix** | Every third-party action is pinned to a full commit SHA; top-level `permissions: {}` in all five workflows with each job raising only what it needs; `persist-credentials: false` on every checkout; no `pull_request_target`. See the fix below |

## What this pass found and fixed

### HIGH — a fork's pull request could reach the image-push role and the staging deploy

`.github/workflows/release.yml`

Release is triggered by `workflow_run` on CI completing, filtered
`branches: [main]`. That filter matches `workflow_run.head_branch`, which
for a CI run started by a *pull request* is the head branch of that pull
request — including a fork's, and a fork's default branch is called `main`.
CI runs on a bare `pull_request:` trigger, so a fork's pull request produces
a completed CI run whose `head_branch` is `main`, and the filter matched it.

The `images` job then checked out `workflow_run.head_sha` (a fork commit is
fetchable from the base repository), ran `make docker-build`,
`docker-verify` and `docker-scan` — the Makefile, Dockerfiles and scripts
all coming from that checkout — and only then assumed the ECR push role. The
OIDC trust policy does not stop this: a `workflow_run` workflow runs in the
default-branch context, so its subject is
`repo:<owner>/<repo>:ref:refs/heads/main`, exactly what the push role
trusts, while the code being executed is the fork's. Every downstream job
(`deploy-staging`, the smoke and end-to-end runs) depends on `images`, so
the same commit reached the staging cluster.

The repository's own comments asserted the opposite in three places, each
true only of a directly pull-request-triggered workflow. The "require
approval for outside collaborators" repository setting blunts the
unauthenticated version of this, but it is not enforced by anything in the
tree, and approving a fork's workflow run reads as "run the tests", not
"build, push to the registry and deploy to staging".

**Fixed** by gating the job on the two things the branch filter does not
say, with the reasoning recorded beside the condition:

```yaml
(github.event.workflow_run.conclusion == 'success' &&
 github.event.workflow_run.event == 'push' &&
 github.event.workflow_run.head_repository.full_name == github.repository)
```

`event == 'push'` excludes every pull-request-triggered run;
`head_repository` excludes every run whose code came from a fork. A full
checkout plus `git merge-base --is-ancestor` then asserts the commit is
actually on `main`, the same assertion `promote.yml` already makes before
production.

### MEDIUM — the processor's text log format bypassed redaction entirely

`services/processor/src/telemetry.rs`

Log redaction runs inside the JSON event formatter. The `LOG_FORMAT=text`
arm used the stock layer, so with that documented, supported setting **none**
of the redaction applied: no name matching, no JWT scrubbing, no value
bound. A span field named `authorization`, or a credential formatted into a
message, would have been written verbatim. The Go gateway does not have this
problem — it hands one options value carrying the redaction hook to both its
text and JSON handlers, so redaction there is format-independent.

**Fixed** by refusing the setting where it matters: configuration now rejects
`LOG_FORMAT=text` in staging and production, the same shape as the existing
`INTERNAL_TOKEN` check, with a regression test covering both the refusal and
the fact that a developer may still use it locally. Section 42 asks for
structured JSON in any case. The text path keeps a comment saying why it is
not redacted and where it is refused.

The residual is deliberate and bounded: on a developer's machine `text`
still bypasses redaction, and the data there is synthetic.

### MEDIUM — file permissions in the fixture and contract tooling

`gosec` G301/G306. The synthetic-fixture generator created its output
directory `0o755` and wrote its manifest `0o644`, and the contract lock was
written `0o644`. **Fixed** to `0o750` and `0o600`: these files are written
and read by one account and nothing needs to share them by default. Git
records only the executable bit, so the committed lock file is unaffected.

### LOW — an unvalidated input reaching a shell in the deploy workflow

`.github/workflows/deploy.yml` regex-validated `image_tag` and both digests
before any credential was obtained, but `commit` — the one input
interpolated directly into `run:` blocks — was not checked. Not exploitable
today, because every caller supplies a GitHub-derived commit id, but it was
the asymmetry in an otherwise disciplined file. **Fixed**: `commit` is now
validated as 40 hex characters alongside the others, and both uses go
through `env:` and `"$COMMIT"` rather than expression interpolation.

## Vulnerability and dependency policy

What blocks a build, what only reports, and what to do about a finding that
cannot be fixed today. `make policy-check` asserts that the gates below
still implement this section, because every threshold here is one deleted
flag away from passing everything.

### What blocks

| Finding | Scanner | Threshold | Blocks |
|---|---|---|---|
| Vulnerability, embedded secret or misconfiguration in an image | trivy image (`--scanners vuln,secret,misconfig`) | HIGH, CRITICAL | yes |
| Vulnerability in a lockfile (`go.sum`, `Cargo.lock`) | trivy fs | HIGH, CRITICAL | yes |
| **Reachable** vulnerability in Go code | govulncheck | any severity | yes |
| Weakness in this repository's own Go code | gosec | MEDIUM severity and above, at MEDIUM confidence and above | yes |
| Secret, in the working tree or anywhere in history | gitleaks | any | yes |
| Dockerfile, Kubernetes or Terraform misconfiguration | trivy config, checkov, kube-linter | HIGH and CRITICAL for trivy; any failed check for checkov and kube-linter | yes |
| Lockfile drift, or a module that no longer hashes to `go.sum` | `go mod verify`, `go mod tidy -diff`, `cargo --locked` | any | yes |
| MEDIUM or LOW vulnerability in a third-party image or lockfile | trivy | reported | no |
| SBOM contents | `make sbom` | reported | no |

Two thresholds are deliberately stricter than a severity number:

- **govulncheck blocks at any severity**, because it reports only
  vulnerabilities whose vulnerable function is actually reachable from this
  code. Reachable is the useful signal: a reachable LOW is more actionable
  than an unreachable CRITICAL.
- **gosec blocks at MEDIUM**, because this is code we wrote and can fix,
  rather than a transitive dependency we can only upgrade. It is also
  filtered to MEDIUM confidence and above (`-confidence=medium`), so a
  high-severity guess does not block; that filter is a deliberate trade and
  `make policy-check` asserts only the severity half of it.

### An unfixed vulnerability still blocks

There is no `--ignore-unfixed` anywhere. `make policy-check` fails if one
appears **in the Makefile**; trivy is also invoked from
`scripts/k8s-validate.sh` and `scripts/tf-validate.sh`, which that check
does not read. A HIGH with no patch available stops a release, which is the
intended behaviour: shipping anyway is a decision someone should have to
make and record, not one a flag makes silently for every finding at once.

The escape hatch is a **scoped, argued suppression**, never a lowered
threshold:

- it names one rule and one path, so every other occurrence still fires;
- it states why the finding is wrong about this repository, or why the risk
  is accepted and what compensates for it;
- it is reviewed when the pin it concerns is next updated.

`make policy-check` fails if any suppression lacks a reason: a `#nosec`
without `-- <reason>`, a trivy entry without a `statement:`, or a
`#checkov:skip` without text after the rule id. A suppression that cannot be
reviewed is not a suppression.

### Keeping the pins current

A pinned dependency is only safe if something proposes the updates.
Dependabot covers all four ecosystems weekly — GitHub Actions, Go modules,
Cargo crates, and the base images of both services and the dev container —
grouped so that a week of bumps is one review. Each update is a pull request
through the same CI as any other change, so an update that breaks a gate is
rejected by the gate rather than noticed in production.

What Dependabot cannot see is recorded in `.github/dependabot.yml` and in
docs/DEVELOPMENT.md: the tool images named by tag in the Makefile and the
validation scripts (trivy, checkov, gitleaks, kustomize, kubeconform,
kube-linter, terraform, yq). The pinned Go tools (gosec, govulncheck) are Makefile variables that Dependabot does not see and are recorded in docs/DEVELOPMENT.md,
plus the Helm charts under `infrastructure/kubernetes/platform`. These are
reviewed by hand each quarter: bump the pin, run `make ci-local`, and triage
what the newer scanner finds. A scanner update that surfaces a real finding
is the update doing its job.

## Supply chain

Where each artifact comes from, and what proves it.

| Control | State |
|---|---|
| **Lockfiles** | `go.sum` and `Cargo.lock`, both committed. Every cargo invocation runs `--locked`, so a build that would change `Cargo.lock` fails instead of changing it. `make deps-verify` adds the Go equivalents: `go mod verify` re-hashes every module in the cache against `go.sum`, so a tampered or swapped module is refused, and `go mod tidy -diff` fails if `go.mod` and `go.sum` do not describe exactly what the code imports |
| **Dependency versions** | Pinned everywhere: actions by commit, base images by digest, Go modules by `go.sum`, crates by `Cargo.lock`, tool images by tag. Dependabot proposes the updates |
| **Vulnerable dependencies** | trivy over both lockfiles and both images, govulncheck over the Go call graph. Thresholds in the policy above |
| **Container provenance** | Both service images build from digest-pinned bases at every stage, are built only by the release workflow, and are pushed with a short-lived OIDC token to a role scoped to pushing. The three gates that decide whether an image may exist — build, container verification, vulnerability scan — all run *before* the registry login exists, so a failing image is never pushed |
| **Immutable references** | ECR repositories are `IMMUTABLE`, so a tag cannot be moved onto different bytes, with `scan_on_push` and KMS encryption; a Terraform test asserts all three. Deployments name images **by digest**, never by tag and never `latest` |
| **SBOM** | A CycloneDX bill of materials per image, generated by `make sbom` from the built image rather than from the lockfiles, so it lists what actually shipped, including the base layer's OS packages. It is also the only place the Rust binary's contents are visible, since a Rust binary carries no module list. CI uploads it for every run; the release attaches it named for the commit it describes, retained 90 days. Generated after the vulnerability gate, so an SBOM only ever describes an image that was allowed to exist |
| **GitHub Actions safety** | Top-level `permissions: {}` in all five workflows, each job raising only what it needs; `persist-credentials: false` on every checkout; no `pull_request_target`; untrusted values routed through `env:` rather than interpolated into `run:`; the release path gated on same-repository push runs |
| **Third-party action versioning** | Every third-party action is pinned to a full 40-character commit, with the release in a trailing comment. `make policy-check` fails on any `uses:` that is not, so a tag reference cannot be reintroduced |
| **Secret scanning** | gitleaks over the entire git history and the working tree, failing on any finding. History matters: a secret that was committed and later removed is still in the history, and still leaked |

### Provenance and signing: what is deferred, and why

Section 99 asks for "provenance where practical" and "signed artifacts where
practical". Both were assessed rather than assumed:

- **OCI attestations — SLSA provenance and an SBOM attached to the image —
  are not available at the pinned toolchain.** The image build is defined in
  `docker-compose.yml` deliberately, so that what CI builds and what a
  developer builds come from one definition. `docker compose build` at the
  pinned version exposes no attestation flags; `docker buildx bake` does
  (`--provenance`, `--sbom`), so adopting them means moving the release
  build to bake. That is a change to the one path that produces production
  images, and it is worth making deliberately rather than as a side effect
  of a review.
- **What stands in the meantime** is weaker than a signed attestation and
  stronger than nothing. The release record names the commit, both digests,
  the version and what staging tested; the promotion workflow refuses to
  deploy anything whose record disagrees with the run that produced it or
  whose commit is not an ancestor of `main`; and the registry is immutable,
  so a digest cannot come to mean different bytes later.
- **Signing (cosign keyless) is not wired.** The `images` job already holds
  an OIDC token, so the mechanism is available. What is missing is the
  verification side — an admission policy that refuses an unsigned image —
  and signing without verification is ceremony. Both halves belong to the
  same change.

## Justified exceptions

Each of these is a finding that is wrong about this repository, or right in
general and deliberately not acted on here. None is silenced to make a gate
pass.

Suppressions live in four configuration files plus inline annotations:

| File | Holds |
|---|---|
| `infrastructure/kubernetes/.trivyignore.yaml` | trivy rule ignores, scoped per rendered file, each with a `statement:` |
| `infrastructure/kubernetes/.checkov.yaml` | checkov `skip-check` for the manifests |
| `infrastructure/kubernetes/.kube-linter.yaml` | kube-linter `exclude` list |
| `infrastructure/terraform/.checkov.yaml` | one framework-wide checkov skip |
| inline | `#nosec` in Go, `#checkov:skip` and `#trivy:ignore` in Terraform |

**What `make policy-check` actually enforces**, so the rest is understood to
be reviewed by hand rather than by a gate: every `#nosec` under
`services/api-gateway` carries a reason; every `- id:` in the Kubernetes
`.trivyignore.yaml` has a matching `statement:`; every `#checkov:skip` under
`infrastructure/terraform` carries a reason. It does **not** check the
inline `#trivy:ignore` annotations, the `skip-check` lists in either
`.checkov.yaml`, or the `exclude` list in `.kube-linter.yaml`.

### gosec — 12 inline annotations, each with its reason

Ten are in developer tooling. Two are on the authentication path and are
intentional by design: a bounded integer conversion inside the Argon2id hash
parser, which runs on every sign-in, and an error-code constant.

| Rule | Where | Why |
|---|---|---|
| G304 (path from a variable) | the contract loader, the fixture generator, the spec loader | The path is a repository location supplied by this project's tooling, tests or a command-line flag. No request ever names a file |
| G306 / G301 (permissions) | remaining occurrences after the fixes above | Resolved rather than suppressed — see the fix above |
| G115 (integer conversion) | `config.go` `uint8(parallelism)` | The value is bounded to `MaxPasswordHashThreads` (64) on the line above, well inside `uint8`. Verified, not assumed |
| G115 | `generate.go` `uint64(seed)` | A bit-pattern conversion of the fixture seed into HMAC key material; every `int64` maps to a distinct `uint64` |
| G404 (weak randomness) | `generate.go` `rand.NewPCG` | Deliberately deterministic: a synthetic fixture must be reproducible from its seed. Nothing it generates is a secret, and the loader refuses to run against production |
| G115 | `password.go` `parseHash` `uint32(len(p.key))` | The key length is bounded to `[minKeyLength, maxKeyLength]` on the line above. On the sign-in path |
| G101 | the `INVALID_CREDENTIALS` error-code constant | An error code, not a credential. Matched on the word in the name. In the auth package, though it is only a constant |

### Kubernetes scanners

- **trivy AVD-KSV-0109** ("secrets in configMaps") — matches the word
  `PASSWORD` in `PASSWORD_HASH_MEMORY_KIB` and
  `PASSWORD_HASH_MAX_CONCURRENT`, which are Argon2id cost parameters in KiB
  and a concurrency count. Listed per rendered file rather than as a blanket
  rule, so a real secret placed in a ConfigMap in a new overlay would still
  be reported.
- **checkov CKV_K8S_15** (`imagePullPolicy: Always`) — staging and
  production do set it. The base and the local overlay cannot: local images
  are side-loaded into the kind node and exist in no registry, so `Always`
  would make the kubelet try to pull one and fail.
- **checkov CKV_K8S_35 / kube-linter `read-secret-from-env-var`** (prefer
  secret files over environment variables) — **correct, and accepted rather
  than fixed.** An environment variable is readable through
  `/proc/<pid>/environ` and is inherited by children. Acting on it is an
  application change, not a manifest one: neither service can read a secret
  from a file today. Recorded as owed work against the services'
  configuration layers.
- **kube-linter** additionally excludes six checks in
  `.kube-linter.yaml`, each with its reason in that file:
  `minimum-three-replicas` and `hpa-minimum-three-replicas` (the floor is
  two in production and one in staging, which is a sizing decision),
  `required-label-owner` and `required-annotation-email` (this repository
  has no owner registry), `no-node-affinity` (spreading is done with
  `topologySpreadConstraints`) and `dnsconfig-options`.
- **checkov CKV_K8S_43** (use a digest) — the stricter identifier, and
  SPECIFICATIONS.md section 32 is the authority, asking for an immutable
  identifier and recommending `service:<git-sha>`, which the pipeline sets.
  Moving to digests is a deliberate pipeline change with the specification
  updated to match, not a scanner accommodation.

### Terraform

Every `#checkov:skip` sits inside the resource it applies to with its reason
attached, and there are about forty of them across a dozen rule families.
[infrastructure/terraform/README.md](../infrastructure/terraform/README.md)
carries the authoritative group-by-group account; the summary is:

| Group | Example | Why |
|---|---|---|
| KMS **key** policies | `CKV_AWS_109`, `CKV_AWS_111` | a resource wildcard means "this key"; the statement is AWS's own default delegation to IAM |
| S3 access-log bucket | `CKV_AWS_145` | AWS delivers logs only into a bucket encrypted with S3-managed keys, and it should not log its own deliveries |
| Cross-region replication | `CKV_AWS_144` | a compliance need this project does not have; the recovery target is stated as none |
| EKS public endpoint | `CKV_AWS_38`, `CKV_AWS_39` | the API endpoint is reachable, restricted to a validated CIDR list that refuses `0.0.0.0/0` and refuses to be empty |
| Per-environment durability | `CKV_AWS_157`, `CKV_AWS_293`, `CKV_AWS_118`, `CKV_AWS_353` | Multi-AZ, deletion protection, enhanced monitoring and Performance Insights are set in production and deliberately not in staging |
| Secret rotation | `CKV2_AWS_57` | secrets are rotated by Terraform rather than on a Secrets Manager timer |
| IAM wildcards scoped by condition | `CKV_AWS_356` | the cluster autoscaler's `Resource: "*"` is narrowed by a tag condition |
| Graph blind spots | `CKV2_AWS_5`, `CKV2_AWS_62`, `CKV2_AWS_10` | attachments and deliveries checkov's graph does not follow |

One skip is **framework-wide** rather than inline:
`infrastructure/terraform/.checkov.yaml` skips `CKV_AWS_338` (one year of log
retention) because retention is set per environment (14 days staging, 365
production); `CKV_AWS_66` stays on.

**trivy inline ignores.** Six `#trivy:ignore` annotations exist, in
`bootstrap/logging.tf`, `modules/eks/main.tf` and `modules/rds/main.tf`.
Each has its argument in a comment above it rather than on the annotation
line, and unlike the checkov skips, **nothing machine-checks that they carry
a reason.**

### Accepted LOW risks, not changed

- **The development container runs as root, pins its bases by tag rather
  than digest, and sets `git safe.directory '*'`.** It ships nothing, is
  never deployed, and is not part of any image that serves traffic. It does
  carry a Docker CLI intended for a mounted host socket, which would be
  host-root-equivalent if that socket were mounted; it is not mounted by the
  dev container definition today. Worth revisiting if that changes.
- **A constraint name is returned as a client-visible error code** (for
  example `MEASUREMENTS_PATIENT_ID_FKEY`). This is schema naming crossing
  the trust boundary, and it is deliberate: it gives clients something
  stable to branch on. Noted so the trade is visible rather than accidental.
- **No token revocation list.** An access token is valid until it expires.
  The lifetime is 15 minutes by default with a 24-hour ceiling, and the
  account's status is re-read from the database when a request loads the
  current user, so disabling an account takes effect without waiting for the
  token to expire.

## Where the controls are tested

Security properties are asserted by tests, not only by review: the token
verifier's rejection of a wrong algorithm and a tampered signature, the
timing-equalised login, the authorization table's completeness, the body
limit, the rate limiter's proxy-hop handling, the redaction hooks in both
services (including that redaction survives a derived logger), the metrics
ports' refusal of free-form labels, and the processor's constant-time
credential comparison. `make k8s-local-test` additionally proves the cluster
*refuses* a privileged pod and that the NetworkPolicies actually deny,
rather than that the objects merely parse.
