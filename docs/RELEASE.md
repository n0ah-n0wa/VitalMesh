# Release identification

When something is wrong in production the first question is "which build is
this, exactly?", and every answer that follows depends on it. Which commit,
so the code can be read. Which dependencies, so an advisory can be matched
against them. Which schema, so a query can be trusted. Which algorithm, so a
result can be reproduced.

This document is what VitalMesh answers with, where each answer comes from,
and what stops a build that cannot answer from being released.

## What every build identifies

Seven facts, each derived from the build rather than written down beside it
(SPECIFICATIONS.md section 98).

| Fact | Where it comes from | Looks like |
|---|---|---|
| Git commit | `-ldflags -X …buildinfo.Version` for Go, `VITALMESH_VERSION` for Rust, both set by the Makefile and both Dockerfiles from `git rev-parse --short HEAD` | `89fc016` |
| Go version | `runtime.Version()` | `go1.25.14` |
| Rust version | `rustc -V` at compile time, captured by `services/processor/build.rs` | `rustc 1.89.0 (29483883e 2025-08-04)` |
| Dependency lockfiles | a digest per service, below | `sha256:900885fb…`, `fnv64:72f8f9e7…` |
| Container image digest | the deployment passes `IMAGE_DIGEST`; a process cannot read its own | `sha256:0b1e5b…` |
| Database migration version | the highest migration embedded in the gateway binary | `11` |
| Processing algorithm version | `PROCESSING_ALGORITHM_VERSION` for the gateway, `anomaly::ALGORITHM_VERSION` for the processor | `1.0.0` |

The processor also reports the internal-API contract version it implements,
because a release that pairs two services has to say which protocol they
agreed on.

## Reading it

Both binaries print their release record and exit:

```sh
api-gateway version
processor version
```

```json
{
  "service": "api-gateway",
  "version": "89fc016",
  "go_version": "go1.25.14",
  "dependencies": "sha256:900885fb77484aab1cfda735",
  "migration_version": 11,
  "algorithm_version": "1.0.0",
  "image_digest": ""
}
```

Neither reads configuration or opens a connection, so both answer inside a
container whose database is unreachable, which is when the question is most
often asked:

```sh
kubectl -n vitalmesh-production exec deploy/vitalmesh-api-gateway -- api-gateway version
```

A binary refuses to print a record it cannot stand behind: it prints what it
has, names the fields that identify nothing on standard error, and exits 1.
An unstamped build reports the version `dev`, which is treated as a missing
version rather than as a version.

## Where it is published

**`/metrics`, as `build_info`.** Both services publish the conventional
gauge whose value is always 1 and whose labels carry the facts:

```
vitalmesh_build_info{version="89fc016",go_version="go1.25.14",dependencies="sha256:900885fb77484aab1cfda735",algorithm_version="1.0.0",image_digest="sha256:0b1e5b…"} 1
vitalmesh_processor_build_info{version="89fc016",rust_version="rustc 1.89.0 (29483883e 2025-08-04)",dependencies="fnv64:72f8f9e7dc4f8160",algorithm_version="1.0.0",contract_version="1.1.2",image_digest="sha256:…"} 1
```

Cardinality is one series per process, because every label is fixed for the
life of the build. `count by (version) (vitalmesh_build_info)` says which
builds are running, which during a rollout says how far it has got.

The gateway also publishes `vitalmesh_database_schema_version`, the
migration version it read from the database at start-up. It is deliberately
**absent** rather than zero when it could not be read, because zero is a
real schema version: it is what an unmigrated database reports.

**`release.json`**, the release artifact, carries both records alongside the
image digests and what staging ran. It is written only after every staging
stage has passed and is what a production promotion deploys from; see
docs/DEPLOYMENT.md.

### Why not `/health`

Build metadata is published through metrics rather than through a new
endpoint, for three reasons. `/health` is unauthenticated, and the
toolchain a service was built with is a detail worth keeping off a public
endpoint even though it is not a secret. The internal health response is
fixed by `contracts/internal-api/processor-v1.json`, so adding fields there
is a contract change. And `/metrics` is where a scraper already looks, and
is already restricted to the `monitoring` namespace by the
`allow-metrics-scrape` NetworkPolicy.

## What is deliberately not in a release record

Nothing that is a secret, a credential, an endpoint, a hostname or a
filesystem path. Neither record reads configuration at all, which is what
keeps it that way rather than a list of things to remember to leave out.
`scripts/release-metadata-check.sh` runs both binaries with a full set of
credentials in the environment and fails if any of them appears in the
output.

## The two dependency digests, and why they differ

Both answer "was this built from the same dependencies as that?", and
neither is a security control. The lockfiles themselves, `--locked` builds
and the scanners are what protect the supply chain; these identify.

**The gateway** hashes the module set the binary actually links, taken from
the build information the Go toolchain records: every dependency's path and
version, sorted, SHA-256, truncated to twelve bytes. It is taken from the
linked module list rather than from `go.sum` on purpose, because that
describes what was built rather than what a file said at the time.

**The processor** hashes `Cargo.lock` itself, with FNV-1a, in
`services/processor/build.rs`. Rust has no run-time equivalent of Go's build
information, so the digest has to be computed while compiling, and a build
script that hashes with SHA-256 needs a crypto crate as a build dependency.
That crate would enter `Cargo.lock`, be scanned as part of the supply chain
and need maintaining for ever, which is a poor trade for a hash whose job is
to tell two dependency sets apart. So the digest is FNV-1a and is prefixed
`fnv64:` rather than `sha256:`, so that nobody reads it as cryptographic.

A digest names its own algorithm for the same reason a release record
exists: it has to still mean something when it is read a year later.

## What enforces it

**`make release-metadata`** (in `make verify`, and its own step in the CI
`build` job) runs both binaries just built and checks each of the seven
items is identifiable, that both services agree on the commit and the
algorithm version, that the reported commit is `HEAD`, that the embedded
migrations are not stale, that both lockfiles are committed, that both
binaries report an image digest they are given, that both Deployments pass
`IMAGE_DIGEST`, and that neither record discloses anything sensitive.

It reads what the binaries say rather than what the source says, which is
the point: a record that had stopped being derived from the build would
still pass a source inspection.

**The release workflow** reads each record out of the image it just built,
before the registry login exists, so an image that cannot identify itself
is never pushed. Both records are folded into `release.json`.

**The promotion workflow** refuses a record whose two builds disagree about
the commit or about the algorithm version. That pairing is not cosmetic: the
processor refuses a job whose algorithm version it does not implement, so a
release whose halves disagree cannot process anything.

**The deployment workflow** stamps each pod with the digest of the image it
runs. The base manifests carry `IMAGE_DIGEST: set-by-ci`, replaced per
container at render time, and the deploy fails if any `set-by-ci` survives:
a pod reporting `set-by-ci` as its image digest would be worse than one
reporting none, because it looks like an answer.

## Reproducing a build

A commit plus the two lockfiles is enough, which is what makes the record
useful:

```sh
git checkout <commit>
make build                      # stamps the commit into both binaries
make release-metadata           # confirms the record matches this tree
```

The digests reproduce because `go build` resolves through the committed
`go.sum` and `cargo build --locked` refuses to move `Cargo.lock`. The two
toolchain versions reproduce because both are pinned: Go by `go.mod`, Rust
by `services/processor/rust-toolchain.toml`. Container images reproduce to
the same *contents* but not necessarily to the same digest, because a layer
carries timestamps; the digest is recorded rather than recomputed, which is
why the pipeline deploys by digest and passes it to the pod.

## The infrastructure version, which is the gap

SPECIFICATIONS.md section 98 also asks a release to identify the
infrastructure version. Today it does so only indirectly: the Terraform root
modules live in this repository, so the commit identifies the configuration,
and the `terraform plan` workflow records the plan for the commit it ran on.

What does not exist is a record of **which commit was last applied**.
Terraform is deliberately never applied by CI -- there is no apply role, and
none of these workflows can reach one -- so an apply is a human action and
nothing stamps the commit onto the resources or into the state. Reading it
back today means comparing the state against a plan.

Closing it means a `Revision` default tag fed from a variable the applier
passes, which is a one-line change to each root module and a change to how
applies are run. It is left open rather than half-done: a tag defaulted to
something convenient would be worse than no tag, because it would look like
the answer.
