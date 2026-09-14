# Deployment

How a commit reaches staging, what checks it on the way, how to tell when
it has gone wrong, and how to go back; and how a release that passed
staging is promoted to production, by a person, behind a reviewer.

- [The pipeline](#the-pipeline)
- [Immutable references](#immutable-references)
- [Safe rollout](#safe-rollout)
- [Timeouts](#timeouts)
- [Failure detection](#failure-detection)
- [Logs and artifacts](#logs-and-artifacts)
- [Rollback](#rollback)
- [Production](#production)
- [Privileges and secrets, reviewed](#privileges-and-secrets-reviewed)
- [Reproducing the checks locally](#reproducing-the-checks-locally)

## The pipeline

```text
push to main
  └─ CI (ci.yml): go · rust · contracts · integration · build · containers ·
     dependency scan · secret scan · kubernetes manifests · terraform → ci
       └─ Release (release.yml), only if CI succeeded, for that exact commit
            images          build → verify → scan → tag sha-<commit> → push to ECR
            deploy-staging  secrets → render by digest → apply → migrations → rollout
            smoke-staging   no credentials: up, right version, TLS, refuses without a token
            e2e-staging     scripts/demo.sh against staging as a signed-in client
            record          release.json: the commit, tag and digests staging tested
                 └─ Promote to production (promote.yml), dispatched by hand with that run's ID
                      verify             the run passed staging; the record names a commit on main
                      deploy-production  waits for the production reviewers, then deploys the record's digests
                      smoke-production   no credentials: the promoted version is in service
```

Every stage is a job; each waits for the one before it and fails the run
if it fails. The commit released is the one CI tested
(`workflow_run.head_sha`), not whatever `main` points at when the release
starts. The AWS side of each job is a short-lived OIDC token exchanged
for a role scoped to that job (`docs/GITHUB_CONFIGURATION.md`): the image
push role can push and nothing else, the staging deploy role can touch
staging and nothing else, and the smoke test holds no credential at all.
Nothing in the release workflow names the production role.

| Stage | Where | Runs |
|---|---|---|
| build, verify, scan, publish | `release.yml`, job `images` | `make docker-build docker-verify docker-scan`, then push |
| deploy | `deploy.yml`, called with `environment: staging` | `kubectl apply --server-side`, migrations, rollout |
| smoke | `release.yml`, job `smoke-staging` | `scripts/smoke.sh` |
| end-to-end | `release.yml`, job `e2e-staging` | `scripts/demo.sh` with the staging account |

## Immutable references

- Images are tagged `sha-<40-hex commit>`. The ECR repositories are
  `IMMUTABLE`, so a tag can never be made to mean a different image.
- The images job outputs the **digests** the registry reports after the
  push, and the deploy job takes those digests as inputs: it refuses
  anything that is not `sha-<commit>` plus two `sha256:` digests, and
  before applying anything it checks the registry still holds exactly
  those digests under that tag.
- The overlay is rendered with the digests in place of the committed
  `set-by-ci` tags (`kustomize edit set image name@sha256:…`), and the
  rendered manifests are checked to contain no tag at all. `latest` does
  not exist in this system: it is never pushed and would be rejected.
- The smoke test asks the running gateway for its version and compares it
  to the released commit, so a rollout that reported success but left the
  old build in service is caught from the outside.

## Safe rollout

- Both Deployments roll with `maxUnavailable: 0`, `maxSurge: 1`: a new
  pod must be Ready before an old one is retired, so a build that cannot
  start never reduces capacity.
- The namespace has load balancer readiness gates
  (`components/deployed/namespace-readiness-gate-patch.yaml`): a pod is
  not Ready until the load balancer has registered it and its health check
  passes, so a rollout cannot outrun the load balancer.
- PodDisruptionBudgets keep a floor of pods through node operations.
- Migrations run first, in a Job, and the rollout waits for the Job to
  complete; pods of the new version never start against an unmigrated
  schema.
- The Kubernetes Secrets are rewritten from Secrets Manager on every
  deployment, so a rotated secret reaches the cluster with the next
  release rather than with the next incident.

## Timeouts

Nothing waits for longer than a person would.

| What | Limit | Where |
|---|---|---|
| the whole deploy job | 40 minutes | `deploy.yml`, `timeout-minutes` |
| migration Job | 15 minutes (the Job's own `activeDeadlineSeconds` is 900 too) | `kubectl wait --timeout=15m` |
| each Deployment's rollout | 10 minutes | `kubectl rollout status --timeout=10m` |
| rollback rollout (if any) | 5 minutes each | same step |
| smoke: first `/health` | up to 5 minutes of retries (a new load balancer target takes about a minute to pass its checks) | `RETRY_SECONDS` |
| smoke and E2E jobs | 15 minutes | `timeout-minutes` |
| any single HTTP call in the tests | 10 seconds | `curl --max-time` |

## Failure detection

What fails the run, in order of when it is found:

| Finding | Detected by |
|---|---|
| a HIGH or CRITICAL vulnerability in an image or Dockerfile | `docker-scan`, before any registry credential exists; the image is never pushed |
| the registry holds a different image under this commit's tag | the digest check at the start of the deploy |
| a migration that fails or hangs | the Job wait; its logs are printed and collected |
| pods that never become Ready (bad config, crash on start, failing readiness, image cannot be pulled) | `rollout status` with a timeout; the rollout is rolled back in staging |
| the load balancer not registering the new pods | the same, through the readiness gate |
| the released build not in service, the certificate wrong, the hostname unresolvable, plain HTTP not redirected, an endpoint answering unauthenticated | `scripts/smoke.sh` |
| a sign-in, a write, the Rust engine, the results, or idempotency broken end to end | `scripts/demo.sh` in `e2e-staging` |

The rollout step rolls back on failure and **still fails**: a rollback is
a recovery, not a success, and the run must be red for someone to look.

## Logs and artifacts

| Artifact | Contents | Retention |
|---|---|---|
| `deploy-staging-<run>-<attempt>` | `rendered.yaml` and `apply.yaml`: the manifests exactly as applied, with the digests; on failure also `diagnostics/`: pods, deployments, the last 200 events, `describe` of every pod that is not Running, and the last 300 lines of every container's logs (current and previous) | 14 days |
| `e2e-staging-<run>-<attempt>` | `e2e.log`: every request the demo made and every response, with the access token never printed | 14 days |
| `release` | `release.json`: the commit, version, tag, digests and what staging ran; exists only if every staging stage passed; what a promotion deploys from | 90 days |
| `deploy-production-<run>-<attempt>` | as the staging one, for a promotion | 14 days |
| the run summary | the digests, the version, the hostname, the load balancer address; for the images job, the digest table | with the run |
| CloudWatch | Container Insights ships the pods' logs in production; in staging, `kubectl logs` through the cluster (the platform README) | `log_retention_days` |

Nothing in any artifact is a secret: the rendered manifests carry
Secret *references* (the placeholder Secrets are deleted before render),
the diagnostics show environment variable names and `secretKeyRef`s, and
the services never log credentials.

## Rollback

There are three ways back, from fastest to most correct. All of them
leave the database as it is: **migrations are not rolled back** (they are
forward-only and, by the migration policy, additive), so a rollback runs
the previous code against the current schema. That is safe as long as
each migration keeps the previous version working, which the migration
policy requires and the E2E run after each deployment exercises.

**1. Automatic, in staging.** If a rollout does not complete within its
timeout, `deploy.yml` rolls both Deployments back to their previous
revision (`kubectl rollout undo`) and fails the run with a warning saying
so. The previous revision is the previous digests; nothing is rebuilt.

**2. By hand, in minutes.** As a cluster admin or with the environment's
deploy role:

```sh
aws eks update-kubeconfig --name vitalmesh-staging --region eu-central-1
kubectl -n vitalmesh-staging rollout history deployment/vitalmesh-api-gateway
kubectl -n vitalmesh-staging rollout undo deployment/vitalmesh-api-gateway    # or --to-revision=N
kubectl -n vitalmesh-staging rollout undo deployment/vitalmesh-processor
kubectl -n vitalmesh-staging rollout status deployment/vitalmesh-api-gateway --timeout=10m
kubectl -n vitalmesh-staging rollout status deployment/vitalmesh-processor --timeout=10m
GATEWAY_URL=https://api.staging.vitalmesh.example sh scripts/smoke.sh
```

`rollout history` lists revisions; the artifact of each run says which
digests each revision carries. Roll both services back together: they
are released together, and a gateway from one commit against a processor
from another is a combination nothing has tested.

**3. Through the pipeline, which is the record.** Revert the offending
commit on `main` (`git revert`, a pull request, CI, then the release runs
of itself). The environment ends up on images built from the reverted
tree, tagged and pinned like any release, with the smoke and E2E checks
run against it, and the history shows what was reverted and why. This is
the way for anything that has to stay rolled back.

What a hand rollback does **not** do: it does not change the Kubernetes
Secrets (a rotated secret stays rotated), the migration Job (already
complete), or the ConfigMaps (rolled back with the Deployment, since they
are part of the applied set and the previous ReplicaSet keeps its
references). A configuration change alone can be rolled back with a
revert; there is nothing to undo in the cluster for it beyond the next
apply.

## Production

Production is promoted, never released to. `.github/workflows/promote.yml`
is dispatched by hand with the **run ID of a Release run**, and:

1. **Verifies** (no credentials, no environment) that the run is a
   Release from `main` that concluded successfully, downloads its release
   record (`release.json`, written by the Release workflow only after
   staging's deploy, smoke and E2E all passed, so a run without one cannot
   be promoted), and checks that the record's commit is the run's commit
   and is on `main`, that the tag is `sha-<that commit>`, that the digests
   have the right shape, and that the version is that commit's short form.
   The candidate — commit, subject, digests, what staging ran — goes into
   the run summary.
2. **Waits for approval.** The `deploy-production` job declares
   `environment: production`; it does not start until a required reviewer
   (not the person who dispatched it: prevent-self-review is on) approves
   it on the run's page, having read the summary. Only then is an OIDC
   token for the production role issued. Rejecting cancels the run.
3. **Deploys the record's digests** with `deploy.yml`, which re-checks the
   registry holds exactly them under the commit's tag before touching the
   cluster, applies, waits for migrations and both rollouts with the
   timeouts above, rolls back to the previous revision if a rollout does
   not complete (and still fails), and uploads the manifests and
   diagnostics.
4. **Smoke-tests production** from the outside: version in service, TLS,
   redirect, unauthenticated requests refused. There is no end-to-end run
   in production (no test account exists there, by a tested Terraform
   floor); the E2E evidence is staging's, on the same digests.

Nothing is rebuilt. The images promoted are the bytes staging ran, by
digest; a commit that was never released, or whose release did not pass
staging, has no record and cannot be promoted.

To promote:

```sh
gh run list --workflow Release --branch main --limit 5          # find the run that passed staging
gh workflow run promote.yml --ref main -f release_run_id=<run id>
gh run watch                                                    # then approve on the run's page as a reviewer
```

### Rolling production back

The same three ways as staging, in the same order of speed, with the same
caveat: migrations are forward-only and additive, so a rollback runs the
previous code against the current schema.

**1. Automatic, on a failed rollout.** `deploy.yml` rolls both
Deployments back to their previous revision and fails the run. With
`maxUnavailable: 0` the previous pods were still serving throughout, so
this only retires the ReplicaSet that could not start.

**2. Promote the previous release.** The fastest deliberate rollback, and
the one that leaves the right record: dispatch `promote.yml` with the run
ID of the last good release. Its record still exists (90 days), its
digests are still in the registry (tags are immutable and the lifecycle
keeps the newest 200 images), the reviewer approves it as any promotion,
and production ends up on exactly what it ran before, with the smoke test
confirming the version. Ten to fifteen minutes.

**3. By hand, as a cluster admin**, when the pipeline itself is the
problem: the `kubectl rollout undo` sequence under [Rollback](#rollback),
against `vitalmesh-production`. Follow it with a promotion of the intended
release once the pipeline works again, so that the environment's state is
one a run explains.

Everything a rollback needs to know is in the promotion run: the summary
names the digests deployed, the artifact holds the manifests as applied,
and `gh run list --workflow "Promote to production"` is the history of
what production has run.

## Privileges and secrets, reviewed

What each job may do, and what it can see, checked against the workflow
files.

| Job | AWS role | GitHub token | Environment |
|---|---|---|---|
| `release.yml` `images` | `vitalmesh-ecr-push` (push two repositories; main only) | `contents: read`, `id-token: write` | none |
| `deploy.yml` (staging) | `vitalmesh-staging-deploy` | `contents: read`, `id-token: write` | `staging` |
| `release.yml` `smoke-staging` | none | `contents: read` | none |
| `release.yml` `e2e-staging` | `vitalmesh-staging-deploy` (reads one secret and one parameter) | `contents: read`, `id-token: write` | `staging` |
| `release.yml` `record` | none | `contents: read` | none |
| `promote.yml` `verify` | none | `contents: read`, `actions: read` (to read a run and its artifact) | none |
| `deploy.yml` (production) | `vitalmesh-production-deploy` | `contents: read`, `id-token: write` | `production`, behind reviewers |
| `promote.yml` `smoke-production` | none | `contents: read` | none |

- **No job holds both environments' roles**, and no job outside a
  `production` environment can obtain the production one: the role's trust
  policy names that subject and nothing else, and the environment's branch
  policy limits it to `main`. The `promote.yml` guard job fails a dispatch
  from any other branch in words before the environment does so silently.
- **Every workflow starts from `permissions: {}`** and each job asks for
  what it uses; `id-token: write` appears only on jobs that assume a role.
- **Inputs are untrusted.** `release_run_id` is checked to be a number
  before it reaches a shell, and every value read from the release record
  is checked for shape and cross-checked against the run itself, the git
  history, and (in `deploy.yml`) the registry. Inputs reach scripts as
  environment variables, never interpolated into the script text.
- **Secrets never appear.** The deploy job masks each secret value the
  moment it is read (`::add-mask::`), composes the URLs from masked parts
  and masks those too, writes them to a 0700 temporary directory rather
  than passing them as arguments, and deletes the directory on exit. The
  end-to-end password is masked before use and the demo prints "not
  printed" where a token would be. The artifacts hold rendered manifests
  (Secret *references* only), pod descriptions (variable names and
  `secretKeyRef`s) and service logs (which never log credentials). The
  release record holds digests and a hostname.
- **The record is not a credential.** Anyone who can read the repository's
  artifacts can read it; it only says what was tested. Deploying it still
  needs the production role, which needs the environment, which needs a
  reviewer.
- **What a compromised `main` could do:** push images (from a merged
  commit only), deploy staging, and queue a promotion that a reviewer must
  still approve. It could not push under another tag, alter a pushed
  image (immutable), reach production's secrets, or read Terraform state.

### Findings of the CI/CD review, and what changed

| Area | Finding | Fix |
|---|---|---|
| Verdict job | `ci` computed its list of failed jobs through a pipe without `pipefail`; a `jq` failure would have produced an empty list and a green verdict | `set -euo pipefail`; the verdict now fails unless every required job reports `success`, the count matches, **and** the required list equals the jobs in `ci.yml` itself (a job added to the file but not to `needs` fails the verdict). Rehearsed against the real file: success, failure, skipped, cancelled, a missing job and an empty list each give the right answer |
| Token exposure | every checkout persisted `GITHUB_TOKEN` into `.git/config` for the rest of the job; no job pushes | `persist-credentials: false` on all 17 checkouts |
| Shell pipelines | eighteen piped steps ran under `set -eu` only, so a failing left-hand command could pass unnoticed | `set -euo pipefail` in every step that pipes |
| Dispatch from a branch | a manual Release or Terraform plan from a non-`main` branch built for minutes and was then refused by STS | a `wrong-branch` job fails immediately in words; the real jobs are conditioned on `main` |
| Scanners | trivy 0.58.2 and checkov 3.2.334 were twenty and nine months old | updated to 0.74.0 and 3.3.17; every gate re-run and green |
| Dependency updates | pins age silently; nothing proposed updates | `.github/dependabot.yml`: weekly grouped PRs for actions, Go modules, crates and the Dockerfiles' base images, each through CI. Tool images named in the Makefile and scripts are outside Dependabot's reach and are listed for quarterly review in DEVELOPMENT.md |
| Cache churn | the trivy database was cached under a per-run key, so every run saved a new copy and only ever hit `restore-keys` | keyed by day |
| Test coverage | tests ran but nothing measured coverage or held a floor | Go coverage is measured with cross-package accounting in every suite; `test-go` holds the unit profile at 70% and `coverage-go` the merged unit + integration + E2E profile at 85% (measured 71.0% and 87.1% when introduced), both ratchets that only go up; the merged profile is a CI artifact. Rust coverage (`cargo llvm-cov`) is a follow-up |

Checked and left as they were: action pins (each SHA resolved to its tagged release); service base images (already digest-pinned); the OIDC trust policies (exact `sub`, `aud`, no wildcards, tested); IAM (least-privilege assertions in the Terraform tests); no `pull_request_target`, no `secrets: inherit`, no write permission beyond `id-token`; every job has a timeout; no untrusted context is interpolated into a script; the demo prints no credential, so the E2E log artifact holds none.

Accepted, with reasons: the runner's own `kubectl`, `jq` and `yq` are used unpinned (they come from the runner image, which is versioned by GitHub); the tool images in the Makefile and scripts are pinned by tag rather than digest (the projects treat tags as immutable, and a digest pin would need updating by hand for every release); no coverage floor for Rust yet; `golang.org/x/crypto` stays at v0.55.0 although v0.56.0 fixes two advisories in its `ssh` package (GO-2026-6354, GO-2026-6355), because v0.56.0 requires Go 1.26 and the gateway's toolchain is pinned at 1.25.14 across go.mod, the builder image and CI. The gateway imports only `argon2` from that module; govulncheck confirms neither advisory is reachable, and the deps gate would fail if that changed. The toolchain bump is its own change, which Dependabot will propose.

## Reproducing the checks locally

```sh
docker compose up -d --wait
GATEWAY_URL=http://localhost:8080 sh scripts/smoke.sh          # the smoke test against the local stack
sh scripts/demo.sh                                              # the E2E run, creating its own account locally
IMAGE_TAG="sha-$(git rev-parse HEAD)" make docker-build docker-verify docker-scan   # the images job's gates
```

`EXPECTED_VERSION=<short commit>` makes the smoke test assert the running
version, as the pipeline does; `RETRY_SECONDS` shortens its patience.
Against a deployed environment, `DEMO_ACCOUNT_EXISTS=1` makes the demo
skip creating its account and sign in with `DEMO_EMAIL` and
`DEMO_PASSWORD` instead, which is what `e2e-staging` does with the account
the deployment provisioned.
