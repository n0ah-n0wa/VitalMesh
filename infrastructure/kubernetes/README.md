# Kubernetes

Application manifests for both services (SPECIFICATIONS.md sections 33–36
and 101–104). Kustomize, per OQ-17.

```text
base/                    every environment's shape
components/deployed/     what staging and production share and local does not
overlays/local/          kind: in-cluster PostgreSQL and Redis, no ingress
overlays/staging/        section 103
overlays/production/     section 104
kind/cluster.yaml        the local cluster
```

## Avoiding duplication

Three layers, each holding what it is the only place for.

**`base/`** is the shape: what every environment deploys, sized and
configured so that forgetting to override something is safe rather than
convenient. `ENVIRONMENT` is `production` there, the Secrets are empty and
the image tags cannot be pulled — an overlay that fails to override them
fails loudly.

**`components/deployed/`** is what is true of any environment reached from
outside and is identical between them: an Ingress, the NetworkPolicy
opening for the ingress controller, `TRUSTED_PROXY_HOPS=1`, and the
deletion of the base's placeholder Secrets. Written once, included by
staging and production, and deliberately not by local — which is what
makes local local.

**Each overlay** states only what genuinely differs. Between staging and
production that is: the domain, resource sizes, replica floor and ceiling,
log level, trace sampling, hash concurrency, and which Secret names to
read. The egress narrowing section 104 asks for lives in the component
rather than in production, so that staging exercises it first: a
restriction whose first real test is production is not a validated one.

## What differs, and where to change it

| | local | staging | production |
|---|---|---|---|
| namespace | `vitalmesh-local` | `vitalmesh-staging` | `vitalmesh-production` |
| replicas (HPA min/max) | 1 / 2 | 1 / 3 | 2 / 20 gateway, 2 / 12 processor |
| gateway memory limit | 256Mi | 384Mi | 1Gi |
| processor CPU limit | 500m | 1 | 4 |
| domain | none | `api.staging.vitalmesh.example` | `api.vitalmesh.example` |
| `ENVIRONMENT` | `local` | `staging` | `production` |
| `LOG_LEVEL` | debug | debug | info |
| trace sampling | not exported | 1.0 | 0.1 |
| Argon2id concurrency | 1 | 2 | 8 |
| secrets | generated, local values | `…-staging`, from CI | `…-production`, from a secret manager |
| egress | base (any address) | RFC 1918 only | RFC 1918 only |

Each memory limit follows from the hash concurrency and job concurrency in
the same overlay; the files show the arithmetic. Changing one without the
other is how a busy pod becomes an OOMKilled one.

## Apply these server-side

```bash
kubectl apply --server-side --force-conflicts -k <overlay>
```

Not a preference. The base declares the namespace's `default` ServiceAccount
so it can turn token automounting off, and on a namespace that does not
exist yet client-side apply reads it a moment after creating it, sees no
`default` account, decides to create one, and loses the race to the
ServiceAccount controller. The apply fails with `serviceaccounts "default"
already exists` — on the first deploy into a new namespace, which is the
worst possible time to find out.

Server-side apply states intent instead of making a create-or-update
decision from a stale read, so an object that already exists is not a
problem. `--force-conflicts` takes ownership of the automount field from
the controller that set it, which is the intent.

The migration Job is the other thing a pipeline must handle: a Job's spec
is immutable, so it has to be deleted before a re-apply.

```bash
kubectl -n <ns> delete job vitalmesh-migrate --ignore-not-found
kubectl -n <ns> apply --server-side --force-conflicts -k <overlay>
kubectl -n <ns> wait --for=condition=complete --timeout=15m job/vitalmesh-migrate
```

## No production secrets, anywhere

Nothing in this directory contains a credential, and nothing generates one
outside the local overlay. Staging and production delete the base's empty
placeholder Secrets and point each service at an environment-specific
name; the object itself is created out of band — by the deployment
pipeline in staging, and by a secrets operator in production (OQ-37 is
still open on which). Deleting rather than inheriting matters: applying an
empty Secret over a real one replaces working credentials with nothing,
and the deploy reports success.

The local overlay generates its Secrets from literals in
`overlays/local/kustomization.yaml`. Those are the same obviously-local
strings `docker-compose.yml` already carries and protect nothing.

## Validating

```bash
make k8s-validate      # no cluster needed
make k8s-local-test    # deploys to kind and exercises it
make k8s-failure-test  # breaks each dependency and checks the behaviour
```

`docs/FAILURE_MODES.md` records what the third one observes.

`k8s-validate` renders **every** overlay and checks each rendered result —
not just the base, because an overlay can produce something the base never
contained.

| Tool | Question |
|---|---|
| `kustomize` | does each overlay assemble |
| `kubeconform` | is every object valid against the API schema, strict, on Kubernetes 1.30 and 1.31 |
| `kube-linter` | are the objects consistent — does a Service select a pod that exists, does an HPA target a Deployment that exists, does every container carry probes, limits and a security context |
| `trivy config` | does anything trip a known Kubernetes misconfiguration |
| `checkov` | the same question from a different engine, which disagrees usefully |

Every built-in kube-linter check is enabled; `.kube-linter.yaml` lists the
exclusions and argues each one. `.trivyignore.yaml` carries a single
suppression, for a rule that reads the word `PASSWORD` in an Argon2id
tuning parameter and calls it a credential. `.checkov.yaml` skips three
checks and argues each; Checkov earned its place by being the only tool to
object to the local fixtures' low uids and to the image pull policy, both
of which were fixed rather than skipped.

`k8s-local-test` is the half that needs a cluster, and it finds different
things. It applies the file `k8s-validate` rendered — so what was checked
is what is deployed — and then asserts:

- every object is accepted with no admission warnings;
- a privileged pod is **refused**, so the namespace's `restricted`
  enforcement is real and not just a label;
- the NetworkPolicies **deny**: from a namespace outside the default-deny,
  neither service is reachable; labelling that namespace
  `vitalmesh.io/gateway-client=true` opens the gateway's port and nothing
  else — the processor, PostgreSQL and Redis stay closed;
- the migration Job completes;
- a request crosses the gateway into the Rust processor and comes back
  with anomalies flagged;
- raising the HPA floor actually scales the processor and the new replica
  becomes ready;
- 45 requests sent through the Service across a rolling restart are **all
  answered** — the graceful-shutdown path, `maxUnavailable: 0` and the
  preStop pause working together;
- a rollout and a rollback both complete.

The cluster runs Calico rather than kind's own CNI, which does not
implement NetworkPolicy at all. On the default CNI every policy check
above would pass with every policy deleted.

It needs `kind` and `kubectl` on PATH. That is the one place this
repository asks for a tool outside Docker, and it is unavoidable: kind
builds its node as a container on the host's Docker, so it cannot run
inside one.

## Seams still open

- **Domains** are `.example`, reserved by RFC 2606 and never resolvable,
  until a real one exists (OQ-18). Three values per overlay change it.
- **Ingress class** is `nginx` in both deployed overlays, with no
  controller-specific annotations. Which controller terminates TLS is
  OQ-18 as well.
- **Deployed egress** is narrowed to RFC 1918 rather than to the real
  database, cache and collector subnets, which are not known until the VPC
  exists. That last narrowing belongs with the Terraform that creates them.
- **`ANOMALY_RULES` is deliberately unset in production.** Staging carries
  development bounds so the path is exercised; copying them into
  production would put thresholds nobody chose in front of real
  measurements. Note that nothing in the metrics would reveal the
  omission — the processor exposes no anomaly counter — so it has to be
  set deliberately.
- **Secrets reach the containers as environment variables**, not as
  mounted files. Two scanners object and both are right: an environment
  variable is readable through `/proc/<pid>/environ`, is inherited by
  children, and cannot be rotated without restarting the pod. Fixing it is
  an application change — neither service can read a secret from a file —
  so it is recorded as accepted rather than silenced. It is the one finding
  from the security review that is not fixed.

## Not here yet

The retention `CronJob`, ECR image references, IRSA, and the AWS ingress
annotations. Nothing in these manifests is cloud-specific.
