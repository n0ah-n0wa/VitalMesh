# Rollback

Going back to the release before this one, with a record of having done it.

**The rollback is a deployment of an earlier release, not an undo.** It
redeploys a commit and two image digests that a previous run already built,
tested through staging and recorded. Nothing is rebuilt, so nothing can come
out different from what ran before; and because the references are immutable,
"the release we were on last Tuesday" is a thing that can be named exactly
rather than approximately.

`kubectl rollout undo` also exists, and this document is partly about why it
is the emergency stop rather than the procedure. It restores the pod
template. It does not restore the configuration those pods read, it reaches
back only five revisions, and what it reaches back to lives in the cluster
rather than in an artifact. The rehearsal below demonstrates all three.

- [What a rollback is made of](#what-a-rollback-is-made-of)
- [Rolling production back](#rolling-production-back)
- [Rolling staging back](#rolling-staging-back)
- [Verifying it](#verifying-it)
- [The emergency stop](#the-emergency-stop)
- [The database](#the-database)
- [How far back you can go](#how-far-back-you-can-go)
- [What a rollback does not undo](#what-a-rollback-does-not-undo)
- [The rehearsal](#the-rehearsal)

## What a rollback is made of

Three immutable references, all of them read out of the release record of
the release being returned to (`release.json`, docs/RELEASE.md). None of
them is a name that can be moved.

| Reference | Where it comes from | Why it cannot drift |
|---|---|---|
| the commit | `.commit` | the manifests are rendered from that commit, because `deploy.yml` checks it out (`ref: ${{ inputs.commit }}`) |
| the gateway image | `.images["vitalmesh/api-gateway"]` | a `sha256:` digest; the deployment names the digest, never the tag |
| the processor image | `.images["vitalmesh/processor"]` | the same |

The record also carries what each image says about itself: the toolchains it
was built with, its dependency digests, the schema version it carries and the
processing algorithm it implements. That is what makes it possible to answer
"what exactly am I going back to?" before going there.

Because the commit is one of the three, a rollback returns the **Kubernetes
manifests** as well as the images. This is the difference that matters most
in practice and the one most easily assumed wrongly: a release is code plus
configuration, and a mechanism that restores only the code leaves half the
change in place.

## Rolling production back

Production is promoted, never deployed to directly, and a rollback is a
promotion of an older release. The approval gate, the digest re-check
against the registry and the smoke test all apply exactly as they do going
forwards.

**1. Find the release to go back to.** Release runs that passed staging are
the ones with a `release` artifact; a run without one cannot be promoted.

```sh
gh run list --workflow Release --branch main --limit 10 \
    --json databaseId,headSha,conclusion,createdAt \
    --jq '.[] | select(.conclusion=="success") | [.databaseId, .headSha[0:7], .createdAt] | @tsv'
```

**2. Read its record before you deploy it.** This is the step that turns a
guess into a decision: it says which commit, which digests, and which schema
version that release expects.

```sh
gh run download <run-id> --name release --dir /tmp/rollback
jq . /tmp/rollback/release.json
```

Expect a document whose `.commit` is the release you mean, whose
`.images` are two `sha256:` digests, and whose `.staging` says `smoke` and
`e2e` both passed. If there is no artifact, that run did not pass staging
and is not a rollback candidate.

**3. Promote it.** The same command as any promotion.

```sh
gh workflow run promote.yml --ref main -f release_run_id=<run-id>
gh run watch
```

A reviewer then approves it on the run's page, as for any production
deployment; docs/DEPLOYMENT.md gives the expected wall-clock for a
promotion, which a rollback shares because it is the same path. The workflow re-checks that the registry still holds exactly
those digests under that commit's tag before it touches the cluster, and
refuses if the two images disagree about the commit or the algorithm
version.

**4. Verify.** See [Verifying it](#verifying-it). Do not treat the green run
as the answer on its own: the run says the rollout finished, not that the
bytes you meant are the bytes serving.

## Rolling staging back

Staging has no promotion workflow, and that is a real asymmetry worth
stating rather than working around: `deploy.yml` is `workflow_call` only,
`promote.yml` targets production, and `release.yml` releases whatever
`main` points at. **There is no one-command way to put an older release
back into staging.** The two ways back are the revert and the emergency
stop.

```sh
gh workflow run release.yml --ref main          # releases main's head
```

is therefore only a rollback once `main` has been reverted, which is the
normal path for staging:

```sh
git revert --no-commit <bad commit>
git commit -m "revert: <what and why>"
# open a pull request; CI, then Release, run of themselves
```

This is also the right answer for production **when the problem has to stay
fixed**. A promotion of an older release leaves `main` still carrying the
bad commit, so the next release re-deploys it. Roll back first, revert
second; the revert is the durable half.

## Verifying it

Two commands, and both of them look at what is running rather than at what
was asked for.

```sh
aws eks update-kubeconfig --name vitalmesh-production --region eu-central-1

NAMESPACE=vitalmesh-production \
EXPECTED_VERSION="$(jq -r .version /tmp/rollback/release.json)" \
EXPECTED_GATEWAY_DIGEST="$(jq -r '.images["vitalmesh/api-gateway"]' /tmp/rollback/release.json)" \
EXPECTED_PROCESSOR_DIGEST="$(jq -r '.images["vitalmesh/processor"]' /tmp/rollback/release.json)" \
GATEWAY_URL=https://api.vitalmesh.example \
    sh scripts/rollback-verify.sh

GATEWAY_URL=https://api.vitalmesh.example \
EXPECTED_VERSION="$(jq -r .version /tmp/rollback/release.json)" \
    sh scripts/smoke.sh
```

The expected values are read out of the record rather than typed, so the
thing being verified against is the artifact, not a memory of it.

`rollback-verify.sh` reports, per service:

```
Rollouts in vitalmesh-production
  ok    vitalmesh-api-gateway: all 2 replicas are the current revision and Ready
  ok    vitalmesh-processor: all 2 replicas are the current revision and Ready

Go deployment (api-gateway)
  ok    gateway: all 2 pod(s) reference 2fd3d298846d
  ok    gateway: all 2 node(s) resolved it to the same digest, so the bytes running are the bytes released

Rust deployment (processor)
  ok    processor: all 2 pod(s) reference c30e605c8e6d
  ok    processor: all 2 node(s) resolved it to the same digest, so the bytes running are the bytes released

Migration Job
  ok    the migration Job ran the same image as the gateway

What the services say about themselves
  ok    the gateway reports version a1b2c3d, the release rolled back to

Database schema
  --    the schema was NOT rolled back: migrations are forward-only.
        The previous release runs against the current schema, which is
        safe for as long as no migration has declared a breaking change
        (docs/ROLLBACK.md, "How far back you can go").

rollback-verify: vitalmesh-production is running the expected release
```

It exits 0 only when every `ok` above is an `ok`. A `--` is a check that
did not apply, not one that passed: with no `GATEWAY_URL` the in-service
checks are skipped and say so, which is worth noticing rather than reading
as agreement.

Three of those checks exist because of things that can go wrong quietly:

- **every pod, not one.** A rollout that half-finished leaves pods from both
  releases serving, and the first pod you look at may be either. This is
  also why the rehearsal waits for the replaced pods to finish terminating
  before it reads anything: until they do, they are still answering.
- **the reference and the resolved digest.** A pod's `image` is what it
  asked for and `imageID` is what the node pulled. When the reference is a
  digest the two agree by construction, so what this really catches is a
  reference that is *not* a digest, and pods on different nodes that
  resolved the same tag to different bytes.
- **the service's own answer.** The cluster's account and the process's own
  can differ. The clearest case is configuration: a ConfigMap change alone
  does not change the pod template, so nothing rolls, and the pods keep the
  values they started with until something restarts them.

Smoke covers the outside: TLS, the redirect, unauthenticated requests
refused, and the released version in service.

## The emergency stop

When the pipeline itself is the problem -- GitHub is down, the workflow is
broken, the registry is unreachable -- `kubectl rollout undo` is faster and
needs nothing but cluster access:

```sh
kubectl -n vitalmesh-production rollout history deployment/vitalmesh-api-gateway
kubectl -n vitalmesh-production rollout undo deployment/vitalmesh-api-gateway   # or --to-revision=N
kubectl -n vitalmesh-production rollout undo deployment/vitalmesh-processor
kubectl -n vitalmesh-production rollout status deployment/vitalmesh-api-gateway --timeout=10m
kubectl -n vitalmesh-production rollout status deployment/vitalmesh-processor --timeout=10m
```

Roll both services back together. They are released together, and a gateway
from one commit against a processor from another is a combination nothing
has tested.

**What it restores:** the pod template, so the image digest and anything else
in the template.

**What it does not restore, and this is the important part: the
ConfigMaps.** The ConfigMaps here are named objects (`vitalmesh-api-gateway`,
`vitalmesh-processor`), not generated ones with a content hash in the name,
and the pods read them with `envFrom`. A restored pod template therefore
points at the same ConfigMap *name* and picks up whatever that ConfigMap
holds now -- which is the new release's configuration. The rehearsal below
measures this: after `rollout undo`, the gateway ran the previous image
while using the new release's configuration, a combination that has never
been tested and that neither release would produce on its own.

Two smaller limits, both real:

- it reaches back **five** revisions (`revisionHistoryLimit: 5`), and no
  further;
- what it reaches back to is ReplicaSet history in the cluster, not an
  artifact. A cluster that was rebuilt has none of it.

So: use it to stop the bleeding, then redeploy a release properly, so that
the environment's state is one a run explains.

## The database

**Migrations are never rolled back.** Not by the deployment, not by the
promotion, not by `rollout undo`. A rollback returns the code and the
configuration and leaves the schema exactly where it is, so the previous
release runs against the newer schema.

This is a deliberate choice, not a missing feature, and it has limits worth
stating plainly.

### Why the schema is not rolled back

A down migration undoes a schema change by destroying what the schema change
created. Dropping a column drops its data; there is no version of that
operation that preserves it. For a service holding patient measurements, a
rollback that silently deleted a day of readings would be a worse outcome
than the bug being rolled back from, in every case anyone could describe.

So the schema only moves forwards, and the code is required to tolerate
that. `TestEveryMigrationKeepsThePreviousVersionWorking` holds every
migration to it: each one is applied against a real database and the schema
before and after are compared, and the migration fails the build if it
removes or renames a table or column, changes a column's type, makes an
existing nullable column `NOT NULL`, or adds a `NOT NULL` column with no
default to a table that already existed. Any of those would break the
release that is still running -- during a rolling deployment going forwards,
and equally after a rollback going back. See docs/DATABASE.md, "Expand and
contract".

### What exists but is not used

The down migrations are real: every migration has one, and the integration
suite applies them down to an empty schema. The command exists too:

```sh
api-gateway migrate down [steps]      # NOT part of any rollback procedure
```

It is there for development and for a deliberate, supervised repair. It is
not in any runbook step above, and running it against production is a
data-loss operation that should be treated as one: take a snapshot first
(docs/DISASTER_RECOVERY.md), and expect to restore rather than to undo.

### If a migration itself is the problem

A migration that fails leaves golang-migrate's version marked **dirty**, and
every later attempt refuses with "Dirty database version N" rather than
retrying. That is a deliberate stop, and clearing it is a manual step:

```sh
kubectl -n vitalmesh-production exec deploy/vitalmesh-api-gateway -- \
    api-gateway migrate force <previous version>
```

against a schema that was never modified, because each migration file is one
implicit transaction. Then deploy again. A deployment that stops and says so
beats a schema half-applied, but a pager needs to know the step exists.

### What the schema says about itself

Each gateway publishes the migration version it read at start-up:

```
vitalmesh_database_schema_version 11
```

It is **absent**, not zero, when it could not be read, because zero is a
real schema version: it is what an unmigrated database reports. During a
rollout, two replicas reporting different values is the signal that a
migration landed between their starts.

## How far back you can go

Three limits bound a proper rollback and the shortest one wins. The fourth
applies only to the emergency stop.

| Limit | Today | What happens past it |
|---|---|---|
| the release record | 90 days (artifact retention) | no `release.json`, so nothing to promote; the digests would have to be found by hand |
| the images | newest 200 per repository (`ecr_keep_images`) | the digest cannot be pulled; the rollback fails at the registry check, before the cluster |
| schema compatibility | every release since the last migration that declared a breaking change | the older code meets a schema it cannot read |
| `rollout undo` only | 5 revisions (`revisionHistoryLimit`), held in the cluster | there is no such revision; and a rebuilt cluster has none at all |

**The schema limit is currently not binding: no migration in this repository
declares a breaking change**, so every release back to the first is
schema-compatible with the current schema. That is a property of the
migrations as they stand, not a promise about the future -- the moment a
migration carries a `-- rolling-deployment:` note, every release older than
it stops being a rollback target, and the note says so in the migration
itself:

```sh
grep -l 'rolling-deployment:' services/api-gateway/migrations/*.up.sql
```

An empty result means the whole history is available. A result names the
oldest release you can still return to: the one deployed immediately after
that migration.

## What a rollback does not undo

| Not undone | Why, and what to do instead |
|---|---|
| the database schema | above; forward-only by design |
| data written by the newer release | it stays. A column the older code does not select is ignored, not lost |
| rotated secrets | a Secret rotated since the release being returned to stays rotated. This is correct: rolling a credential back would re-expose it |
| the migration Job | it has already completed. The next deployment replaces it, because a Job's pod template is immutable |
| `main` | the bad commit is still there and the next release deploys it again. Revert it |

## The rehearsal

`scripts/rollback-test.sh` runs the procedure on a real Kubernetes cluster
and checks every part of it. It is a rehearsal rather than a production
drill, and the differences are stated below so the result is not read for
more than it is worth.

```sh
make rollback-images      # two builds of each service, told apart by version
make rollback-test        # create a cluster and a registry, rehearse, destroy
```

**What it does.** It pushes two releases to a registry beside the cluster --
so that real digests exist, because an image that has never been pushed has
no repository digest and a rehearsal between two moveable tags would prove
nothing about a procedure whose whole claim is immutability. It deploys
release A, deploys release B, and rolls back to A by digest. A and B differ
in both halves of a release: the binaries carry different versions, and B's
ConfigMap asks the gateway for a processing algorithm the processor does not
implement, which is a plausible reason to roll back and a change that
restoring the image alone would not undo.

**What it verifies**, after the rollback:

| Dimension | Checked by |
|---|---|
| Go deployment | the gateway pods' image reference and resolved digest are A's, and the gateway reports A's version |
| Rust deployment | the same, asked at the processor itself rather than through the gateway |
| Kubernetes manifests | the ConfigMap in the cluster is A's *and* the running gateway is using A's value |
| container image versions | both pods name their image by digest, and it is A's |
| the database | the schema version is the same before and after |

It also runs `scripts/rollback-verify.sh` -- the same script this document
tells an operator to run -- against the cluster, and checks that it both
accepts the release that is running and refuses the one that is not. A
verification that cannot say no is not a verification.

### What it found, and what it measured

Run on 18 September 2026, single-node kind cluster, Kubernetes v1.31.0,
one replica of each service. Run twice: once against a cluster that was
already up, and once from nothing (`sh scripts/rollback-test.sh` creating
the registry, the cluster and Calico, and destroying them afterwards). Both
passed every check.

The two releases, as the registry assigned their digests:

| Release | Gateway | Processor | Algorithm in its ConfigMap |
|---|---|---|---|
| A | `sha256:2fd3d298846d…` | `sha256:c30e605c8e6d…` | 1.0.0 |
| B | `sha256:adec4800b0f7…` | `sha256:e20c3a922a5b…` | 1.1.0 |

**The rollback restored everything it claims to.** After redeploying A by
digest, over release B:

| Dimension | Observed |
|---|---|
| Go deployment | the gateway pod referenced `2fd3d298846d…`, the node had resolved that digest, and the gateway reported version `a1b2c3d` |
| Rust deployment | the processor pod referenced `c30e605c8e6d…`, likewise resolved, and the processor reported `a1b2c3d` |
| Kubernetes manifests | the ConfigMap read 1.0.0 in the cluster **and** the running gateway was using 1.0.0 |
| container image versions | both pods named their image by digest, neither by tag |
| database schema | version 11 before the rollback and 11 after |

`scripts/rollback-verify.sh` agreed the environment was running release A,
and refused release B when told to expect it.

**Measured: 13 and 23 seconds** across two runs, from the apply to both
rollouts complete. Both are one node, one replica of each service, and
images already on the node, so they are a floor rather than an estimate for
production, where there are more replicas, a real registry to pull from and
a load balancer to register with. The spread between two runs of the same
thing on the same machine is itself the reason no single number is quoted
as *the* duration.

**What it found.** Two things this rehearsal established that were not
obvious from reading the manifests, and one of which the documentation had
wrong:

1. **`kubectl rollout undo` restored the image and left the
   configuration.** Measured: after the undo, the gateway reported version
   `a1b2c3d` -- release A's binary -- while both the ConfigMap and the
   running process reported algorithm version 1.1.0, release B's
   configuration. Neither release ever produced that pairing.
   docs/DEPLOYMENT.md previously said a hand rollback rolls the ConfigMaps
   back "since they are part of the applied set and the previous ReplicaSet
   keeps its references"; the previous ReplicaSet keeps a reference to the
   ConfigMap's *name*, which is not the same thing. That sentence has been
   corrected.

2. **A release cannot be applied over the previous one's migration Job.** A
   Job's pod template is immutable, so applying a release whose Job names a
   different image fails on that object -- and because the Job is rendered
   before the Deployments, the apply stops there, leaving the previous
   release running while reporting only a Job error. `deploy.yml` already
   deletes the Job first, which is why this has never bitten a real
   deployment; the rehearsal reproduced it by not doing so, which is how
   the reason for that step became legible.


**What the rehearsal is not.** It is not a drill against the staging
cluster. Staging is EKS, and its overlay expects Secrets written from AWS
Secrets Manager, an RDS endpoint and an ElastiCache endpoint, none of which
exist outside the account; the `local` overlay is the only one that stands
up on its own, so that is the one this deploys. The other differences: one
node, not three; in-cluster PostgreSQL and Redis fixtures, not RDS and
ElastiCache; a plain HTTP registry on the same machine, not ECR with
immutable tags, scan-on-push and an OIDC-scoped push role; no load
balancer, no TLS, no approval gate, and no traffic during the rollback.

What it does establish is the part that is the same everywhere: that
redeploying an earlier release by digest restores the images, the versions
and the configuration, that `rollout undo` does not restore the
configuration, and that the verification detects when a rollback has not
done what it claimed. It does not establish a production duration, and none
is claimed here.

**What is still untested**, and should be the next thing rehearsed: the
promotion path itself (`promote.yml` with an older run id, through the
approval gate, against a real registry), and a rollback with traffic
arriving throughout. Both need the AWS account.
