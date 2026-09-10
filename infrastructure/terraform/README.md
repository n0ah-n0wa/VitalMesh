# VitalMesh on AWS: the Terraform

The AWS infrastructure for staging and production. This covers SPECIFICATIONS.md sections 57 to 59 and 100 to 105.

Nothing here has been applied. Everything in this directory can be checked without an AWS account by running `make tf-validate`. Planning and applying need an account, and are described under [Planning](#planning).

- [What it builds](#what-it-builds)
- [Layout, and why these modules](#layout-and-why-these-modules)
- [Remote state](#remote-state)
- [Environment isolation](#environment-isolation)
- [Security properties](#security-properties)
- [Secrets](#secrets)
- [Outputs](#outputs)
- [Validating without AWS](#validating-without-aws)
- [Planning](#planning)
- [Recovery](#recovery)
- [Cost](#cost)
- [Scaling down and destroying](#scaling-down-and-destroying)
- [Known gaps](#known-gaps)

## What it builds

Each environment gets its own VPC, EKS cluster, PostgreSQL, Redis, keys, secrets, alarms and deploy role:

```text
                           Internet
                              │
   ┌──────────────────────────┴───────────────────────────┐
   │ public subnets, a /24 per zone                       │  NAT gateways
   │                                                      │  (load balancers later, OQ-18)
   └──────────────────────────┬───────────────────────────┘
                              │
   ┌──────────────────────────┴───────────────────────────┐
   │ private subnets, a /19 per zone                      │  EKS nodes and pods; the VPC CNI
   │                                                      │  gives every pod a VPC address
   └──────────────────────────┬───────────────────────────┘
                              │  5432 and 6379, from the cluster security group only
   ┌──────────────────────────┴───────────────────────────┐
   │ database subnets, a /24 per zone, no route out       │  RDS PostgreSQL, ElastiCache Redis
   └──────────────────────────────────────────────────────┘
```

| Tier | Staging (`10.20.0.0/16`, 2 zones) | Production (`10.30.0.0/16`, 3 zones) | Route out |
|---|---|---|---|
| public | `10.20.0.0/24`, `10.20.1.0/24` | `10.30.0.0/24` to `10.30.2.0/24` | internet gateway |
| private | `10.20.32.0/19`, `10.20.64.0/19` | `10.30.32.0/19`, `10.30.64.0/19`, `10.30.96.0/19` | NAT gateway: one in staging, one per zone in production |
| database | `10.20.10.0/24`, `10.20.11.0/24` | `10.30.10.0/24` to `10.30.12.0/24` | none |

These pieces are created once per AWS account rather than once per environment, and are shared:
- the state bucket and its KMS key
- GitHub's OIDC provider
- the CI roles
- the ECR repositories

| | Staging | Production |
|---|---|---|
| Kubernetes | 1.34; 1 to 3 `t3.medium` nodes (2 desired) | 1.34; 3 to 8 `m6i.xlarge` nodes (3 desired, one per zone) |
| PostgreSQL 16 | `db.t4g.micro`; 20 GiB, growing to 50; single zone | `db.m6g.large`; 100 GiB, growing to 500; Multi-AZ |
| Backups | 3 days of point-in-time recovery | 14 days of point-in-time recovery, and a final snapshot on delete |
| Redis 7.1 | one `cache.t4g.micro` | `cache.m6g.large`, primary and replica, with automatic failover |
| Logs | 14 days | 365 days, plus Container Insights |
| Deleted secrets | removed at once | recoverable for 30 days |

## Layout, and why these modules

```text
infrastructure/terraform/
├── bootstrap/          once per account: state bucket and key, CloudTrail and
│                       access logs, GitHub OIDC, the plan and image-push
│                       roles, ECR
├── modules/
│   ├── network/        VPC, three subnet tiers, NAT, S3 endpoint, flow logs
│   ├── eks/            cluster, node group, add-ons, IRSA roles, access entries
│   ├── rds/            PostgreSQL, parameter group, security group, logs, alarms
│   ├── elasticache/    Redis, its AUTH token and secret, security group, logs, alarms
│   └── platform/       one whole environment: the four above, plus KMS keys,
│                       application secrets, the alarm topic and the deploy role
├── environments/
│   ├── staging/        values only: a single call to modules/platform
│   └── production/     values only: a single call to modules/platform
└── tests/mocks/        what the mocked AWS provider answers in terraform test;
                        each root keeps its own test files in its tests/
```

Section 57 asks for both environments "without duplicating large amounts of configuration". `modules/platform` is how: everything that must be the same in both is written once there. Each environment root is a single module call containing only the values that differ, and in each file those values sit next to comments explaining them.

The other four modules each hold something that travels together. The database's security group, parameter group, log groups and alarms belong to the database. The Redis AUTH token must be written to the cache and to its secret in the same run. The add-ons, IRSA roles and access entries belong to the cluster. Splitting any of these further would only add inputs and outputs to wire together.

Some things are deliberately not modules:

- **Bootstrap.** It exists once per account, not per environment.
- **ECR.** It lives in bootstrap and is shared by both environments. An image is built once on `main`, and the same digest is deployed to staging and then promoted to production. A registry per environment would mean rebuilding for production, which would then run something staging never tested.
- **Community registry modules** such as `terraform-aws-modules`. Every resource is in this repository, where review can see it. The only external code is the two HashiCorp providers, pinned in `.terraform.lock.hcl` with checksums for five platforms.

## Remote state

```text
s3://vitalmesh-tfstate-<account>-<region>/
├── bootstrap/terraform.tfstate                 once migrated (below)
├── environments/staging/terraform.tfstate
├── environments/staging/terraform.tfstate.tflock      only while a run holds it
├── environments/production/terraform.tfstate
└── environments/production/terraform.tfstate.tflock
```

The state bucket:
- **Is versioned.** It keeps 90 days of history, so a state file damaged by a bad apply can be restored from an earlier version.
- **Is encrypted** with a KMS key of its own (`alias/vitalmesh-terraform-state`).
- **Blocks public access** at all four S3 levels, and has ACLs disabled.
- **Refuses any request not made over TLS.**
- **Logs every request.** The log goes to `vitalmesh-access-logs-<account>-<region>` under `state/`, kept for 365 days, so you can see who read, wrote or locked an environment's state.
- **Is guarded by `prevent_destroy`,** as are its key and the log bucket.

**Locking** uses S3 itself: `use_lockfile = true`, available from Terraform 1.10. A run writes `<key>.tflock` beside the state and deletes it when done. No DynamoDB table is involved.

**What state contains.** State holds no secret values, as described under [Secrets](#secrets). It is still a complete map of each environment — endpoints, ARNs and configuration — so it is encrypted, access-logged, and readable only by administrators and the read-only plan role.

**Partial backend configuration.** Each environment's `versions.tf` names its own key and region. The bucket and KMS key are specific to one account, so they are passed at `init` from bootstrap's outputs rather than committed:

```sh
cd infrastructure/terraform/bootstrap
BUCKET=$(terraform output -raw state_bucket_name)
KMS=$(terraform output -raw state_kms_key_arn)

cd ../environments/staging
terraform init -backend-config="bucket=$BUCKET" -backend-config="kms_key_id=$KMS"
```

**Bootstrap's own state.** Bootstrap creates the bucket, so on its first run there is nowhere remote to keep its state. It starts local. Afterwards, move it into the bucket with `bootstrap/backend.tf.example`; that file contains the three commands.

### First-time order in an account

1. Apply `bootstrap/`, then migrate its state into the bucket as above.
2. In the GitHub repository settings, create the environments `staging` and `production`. Give `production` required reviewers. The deploy roles trust exactly these names, so a job outside them cannot assume the roles.
3. Apply `environments/staging`, then `environments/production`. Production refuses to plan without `ingress_domain_name` and `route53_zone_id`: it needs a certificate to serve HTTPS.
4. A cluster administrator installs the platform components (`sh scripts/eks-platform-install.sh <env>`: the AWS Load Balancer Controller and the Cluster Autoscaler, see `infrastructure/kubernetes/platform/`), and applies each namespace's `Namespace`, `ResourceQuota` and `LimitRange` once. The deploy role's access is scoped to inside the namespace, so it can do neither.
5. After the first deployment, the load balancer exists and has an address (`kubectl -n vitalmesh-<env> get ingress`). Create the Route 53 alias record for `ingress_domain_name` pointing at it. external-dns would do this; it is not installed yet.

## Environment isolation

Section 100 requires isolated environment state. Beyond that, staging and production share nothing that one could use to reach the other:

| | How it is separated |
|---|---|
| State | A separate object with its own lock. A run in one root never opens the other's state. |
| Network | Separate VPCs on non-overlapping ranges, with no peering. |
| Compute | Separate EKS clusters. |
| Data | Separate RDS instances and Redis replication groups. |
| Keys | Two KMS keys per environment, `data` and `logs`. Disabling one of staging's keys cannot touch production. |
| Secrets | Named `vitalmesh/<env>/…`, and each is encrypted with that environment's key. |
| IAM | A deploy role per environment, trusted only from the GitHub environment of the same name, which reads only its own secrets and manages only its own namespace. |
| Wrong account | `allowed_account_ids` makes the provider refuse to run anywhere else. |

In a single AWS account, administrators — and the read-only plan role — can still see both environments. Giving each environment its own account is the stronger form, and the stacks support it: run bootstrap in each account and set that account's ID in each environment's `terraform.tfvars`.

The only shared piece is ECR. With separate accounts it would live in one of them, with a repository policy letting the other account's node role pull from it. That policy is not built here.

## EKS: what runs where, and why

The cluster is the piece with the most choices, so they are written down.

| Concern | Choice | Instead of |
|---|---|---|
| Nodes | One managed node group of one instance type across every zone, with a launch template (IMDSv2, hop limit 1, encrypted gp3 root), EKS node auto-repair, and `max_unavailable = 1` on updates | EKS Auto Mode, which bundles nodes, autoscaling, load balancing and storage at a premium on every instance hour and without the launch template the IMDS and volume controls need; or Karpenter, which needs an SQS queue, EventBridge rules, a node role and CRDs |
| Node autoscaling | The Cluster Autoscaler, resizing the group between `node_min_size` and `node_max_size`. Terraform creates its IRSA role, scoped by the `k8s.io/cluster-autoscaler/<cluster>` tag EKS puts on the group; the autoscaler is installed from `infrastructure/kubernetes/platform` | Karpenter (the upgrade path once one group of one type is the bottleneck) |
| Load balancing | An Application Load Balancer per environment, created by the AWS Load Balancer Controller from the application's Ingress, with pods as targets and readiness gates. Terraform creates its IRSA role with the project's published policy, verbatim; the controller is installed from `infrastructure/kubernetes/platform` | ingress-nginx behind a Network Load Balancer: one more Deployment to size, patch and watch, in front of a two-service application |
| TLS | An ACM certificate for `ingress_domain_name`, DNS-validated in `route53_zone_id`, renewed by ACM, found by the controller by host. No key ever exists outside ACM. Production refuses to plan without one (section 104) | A certificate Secret in the cluster |
| Cluster IAM | The control-plane role with `AmazonEKSClusterPolicy` and the KMS grant for Secrets encryption; the node role with only `AmazonEKSWorkerNodePolicy` and `AmazonEC2ContainerRegistryReadOnly` | The usual three node policies, which put the CNI's network rights on every pod that can reach node credentials |
| Workload IAM | IRSA, one role per platform service account: the VPC CNI, the load balancer controller, the autoscaler, and the CloudWatch agent where Container Insights is on. The application has no role: it calls no AWS API | EKS Pod Identity, which would drop the OIDC provider but is not the documented path for two of the four, and needs its agent on nodes the CNI has to bring up first |
| ECR | Nodes pull with `AmazonEC2ContainerRegistryReadOnly`; the S3 gateway endpoint keeps the layers off the NAT gateway; in production, `ecr.api` and `ecr.dkr` interface endpoints keep the API calls off it too, so a pull never leaves AWS | Pulls through the NAT gateway in production (about USD 14 a month per zone-pair saved against per-GB NAT charges) |
| Add-ons | EKS-managed: VPC CNI with NetworkPolicy enforcement on and its own role, kube-proxy, CoreDNS, metrics-server (the HPAs' source), and CloudWatch Observability where enabled; versions are EKS's default for the Kubernetes version, not "latest" | Self-managed add-ons, which nothing here could configure or upgrade |
| Logging | All five control-plane log types; Container Insights (metrics and container logs) in production; every log group created by Terraform with retention and the logs key | Log groups created on first write, with neither |
| Access | Access entries only; no implicit admin for whoever applies; the deploy role admin inside its namespace and nothing else; the API endpoint public to listed addresses only, with the private endpoint always on | The `aws-auth` ConfigMap |
| Resilience | ARC zonal shift registration in production (one call evacuates an impaired zone, and the load balancer follows); node auto-repair; cluster deletion protection in production | Nothing, and a person watching |
| Storage | None: state lives in RDS and ElastiCache. The EBS CSI add-on is one line in `addons.tf` when something needs a volume | Installing a driver nothing uses |

The Kubernetes side of the same decisions (the Ingress annotations, the NetworkPolicy that admits the load balancer by the public subnets' ranges, the Helm values) is in `infrastructure/kubernetes/platform/README.md`.

## Security properties

**Network**

- The database tier has no route to anywhere.
- PostgreSQL and Redis admit their port from the EKS cluster security group only. They have no egress rules at all.
- Neither database is publicly accessible.
- The VPC's default security group is adopted with no rules.
- Public subnets do not hand out public addresses.
- VPC flow logs record all traffic at 60-second aggregation.
- The Kubernetes API endpoint:
  - The private endpoint is always on.
  - The public endpoint is also on, but open only to `cluster_endpoint_public_access_cidrs`.
  - That list is validated to be non-empty and never `0.0.0.0/0`.
  - The public endpoint stays on until something inside the VPC can reach the private one (see [Known gaps](#known-gaps)).

**IAM** (all least-privilege)

| Role | Assumable by | May |
|---|---|---|
| `vitalmesh-terraform-plan` | GitHub: pull requests and `main` of this repository | `ReadOnlyAccess`, read state, write only `*.tflock`, and use the state key. `ReadOnlyAccess` reads data as well as configuration, so an explicit deny takes back log contents, secret values, S3 objects outside the state bucket, image layers, private SSM parameters, and decryption with any key but the state key. Plan needs none of those: the provider refreshes write-only secret versions with `ListSecretVersionIds`. |
| `vitalmesh-ecr-push` | GitHub: `main` only | Push to the two VitalMesh repositories, and nothing else. |
| `vitalmesh-<env>-deploy` | GitHub: the `<env>` environment only | Describe its cluster; read its environment's three secrets; decrypt only through Secrets Manager; administer inside the `vitalmesh-<env>` namespace (an EKS access entry). |
| cluster admins | the principals in `cluster_admin_principal_arns` | Cluster-admin through an access entry. Whoever runs `apply` is not made admin implicitly. |
| cluster, nodes | EKS and EC2 | Only AWS's managed policies for each role. The CNI's permissions are moved off the node role onto the CNI's own IRSA role. |
| IRSA roles | one service account each | The VPC CNI, and the CloudWatch agent where Container Insights is on. |
| flow logs, RDS monitoring | the service, with confused-deputy conditions | Write to their own log group, and nothing else. |
| `<env>-load-balancer-controller` | the `kube-system/aws-load-balancer-controller` service account only | The controller project's published policy, verbatim: create and delete load balancers, target groups and security groups, every write conditioned on the `elbv2.k8s.aws/cluster` tag it sets itself. |
| `<env>-cluster-autoscaler` | the `kube-system/cluster-autoscaler` service account only | Describe Auto Scaling groups and instance types; resize only groups tagged `k8s.io/cluster-autoscaler/<cluster> = owned`. |

The nodes require IMDSv2 with a hop limit of 1, so pods cannot reach the node's credentials. Cluster access uses EKS access entries only (`authentication_mode = "API"`) rather than the `aws-auth` ConfigMap, so who can reach the cluster shows up in plan and in CloudTrail.

**Logging and audit**

| What | Where | Kept |
|---|---|---|
| Every management API call in the account, every region (CloudTrail, with digests) | `vitalmesh-cloudtrail-<account>-<region>` | 365 days |
| Every request to the state and trail buckets (S3 access logs) | `vitalmesh-access-logs-<account>-<region>` | 365 days |
| EKS control plane: API, audit, authenticator, controller manager, scheduler | `/aws/eks/<name>/cluster` | `log_retention_days` |
| VPC flow logs, all traffic, 60-second aggregation | `/aws/vpc/<name>/flow-logs` | `log_retention_days` |
| PostgreSQL: connections, disconnections, statements slower than a second, upgrades | `/aws/rds/instance/<name>/…` | `log_retention_days` |
| Redis slow log and engine log | `/aws/elasticache/<name>/…` | `log_retention_days` |
| Container Insights, where enabled | `/aws/containerinsights/<name>/…` | `log_retention_days` |

Production keeps logs for 365 days; the platform module refuses less. Every log group is created by Terraform, with its retention and key, before anything writes to it.

PostgreSQL logs slow statements without their bound parameters (`log_parameter_max_length = 0`, and the same on error). The gateway sends every value as a parameter, so with the default a slow login lookup would have written an email address into CloudWatch, and a slow credential check the password hash beside it.

**Monitoring of the data stores**

Every alarm notifies the environment's SNS topic, and so the address in `alarm_email` once its subscription is confirmed.

| Store | Alarm | Fires when |
|---|---|---|
| PostgreSQL | `cpu-high` | CPU above 80% for ten minutes |
| | `storage-low` | free storage below 5% of the initial allocation (autoscaling has not kept up, or has hit `db_max_allocated_storage`) |
| | `connections-high` | connections above `db_connections_alarm_threshold` (90 in staging, 700 in production: a little under the class's `max_connections`) |
| | `memory-low` | freeable memory below `db_freeable_memory_alarm_mib` (100 in staging, 800 in production: a tenth of the class) |
| | RDS events | failover, failure, low storage, maintenance, backup, configuration change, deletion, recovery, availability: what RDS itself reports, to the same topic |
| Redis, each member | `engine-cpu-high` | engine CPU above 80% for ten minutes (Redis is single-threaded; host CPU is not the ceiling) |
| | `memory-high` | memory above 80% |
| | `evictions` | any key evicted in five minutes: the idempotency locks are the shortest-lived keys and go first, so an eviction is a correctness question |
| Redis, each replica | `replication-lag` | more than thirty seconds behind for three minutes: a failover would lose that much |

Enhanced Monitoring (60-second OS metrics) and Performance Insights (per-query load, with `pg_stat_statements` preloaded) are on in production. PostgreSQL logs connections, disconnections, statements over a second, and lock waits over a second — never bound parameters.

**Encryption**

| What | Key |
|---|---|
| RDS storage, snapshots and Performance Insights | environment `data` key |
| RDS master password, application secret, Redis AUTH secret | environment `data` key |
| ElastiCache at rest | environment `data` key |
| Kubernetes Secrets in etcd (envelope encryption) | environment `data` key |
| Every CloudWatch log group, and the alarm topic | environment `logs` key |
| Node volumes | AWS-managed EBS key. A customer-managed key would need a grant for the Auto Scaling service-linked role. |
| ECR images | `alias/vitalmesh-ecr` |
| Terraform state | `alias/vitalmesh-terraform-state` |
| CloudTrail | `alias/vitalmesh-cloudtrail` |
| Access logs (state and trail buckets) | S3-managed. AWS supports no other kind of key on an access-log destination. |

All keys rotate yearly.

The logs have a key of their own because CloudWatch Logs can only use a key whose policy names the Logs service. Granting that on the key that protects the database would be a grant with no business being there.

In transit:
- PostgreSQL has `rds.force_ssl = 1` and `ssl_min_protocol_version = TLSv1.2`.
- Redis has `transit_encryption_mode = "required"`.
- The Kubernetes API is TLS only.

## Secrets

No secret is hard-coded, and none is in Terraform state, in a plan, or in this repository:

| Secret | Generated by | Stored in |
|---|---|---|
| `JWT_SECRET`, `PROCESSOR_TOKEN` | an ephemeral `random_password`, written with `secret_string_wo` | `vitalmesh/<env>/app`, as JSON |
| Redis AUTH token | an ephemeral `random_password`, written with `auth_token_wo` to the cache and with `secret_string_wo` to the secret, in the same run | `vitalmesh/<env>/redis` |
| PostgreSQL master password | RDS (`manage_master_user_password`) | the RDS-managed secret, rotated by RDS |

Ephemeral resources exist only for the length of one run, and write-only arguments are sent to AWS without being recorded. That is why this needs Terraform 1.11 or later.

**Into the pods.** The deployment pipeline assumes the environment's deploy role, reads the three secrets, and writes the Kubernetes Secrets that the overlays reference:

| Kubernetes Secret | Key | Built from |
|---|---|---|
| `vitalmesh-api-gateway-<env>` | `DATABASE_URL` | `postgres://<master user>:<RDS secret password>@<database_address>:5432/vitalmesh?sslmode=require` (`verify-full` once the image carries the RDS CA bundle) |
| | `REDIS_URL` | `rediss://:<AUTH token>@<redis_primary_endpoint>:6379` |
| | `JWT_SECRET`, `PROCESSOR_TOKEN` | `vitalmesh/<env>/app` |
| `vitalmesh-processor-<env>` | `INTERNAL_TOKEN` | `PROCESSOR_TOKEN` from `vitalmesh/<env>/app` |

**Rotating.** Increase `secret_version` in the environment's module call and apply. That generates a new JWT key, a new processor token and a new Redis AUTH token.

Redis is changed with the `ROTATE` strategy, which keeps the previous token valid alongside the new one, so pods not yet rolled keep working. The gateway accepts one JWT key at a time, so rotating it signs everyone out.

The RDS master password is rotated by RDS itself, every seven days by default. Nothing yet carries the new password into the Kubernetes Secret: see [Known gaps](#known-gaps).

## Outputs

Each environment root re-exports these from `modules/platform`. None of them is a credential: each secret is identified by the ARN of the Secrets Manager entry that holds it.

| Output | What it is |
|---|---|
| `cluster_name` | EKS cluster name, for `aws eks update-kubeconfig` |
| `cluster_endpoint` | Kubernetes API endpoint |
| `kubernetes_namespace` | `vitalmesh-<env>`, the overlay's namespace |
| `deploy_role_arn` | the role the CD job assumes |
| `database_address`, `database_port`, `database_name` | where PostgreSQL is, reachable only from the cluster |
| `database_master_username`, `database_master_secret_arn` | the master user, and the RDS-managed secret holding its password |
| `database_instance_identifier` | the instance's identifier, which a restore changes and a point-in-time restore names as its source |
| `redis_primary_endpoint`, `redis_port` | where Redis is, reachable only from the cluster, over TLS |
| `redis_auth_secret_arn` | the secret holding the AUTH token |
| `app_secret_arn` | the secret holding `JWT_SECRET` and `PROCESSOR_TOKEN` |
| `alarm_topic_arn` | the SNS topic every alarm notifies |
| `nat_public_ips` | the addresses outbound traffic comes from |
| `load_balancer_controller_role_arn`, `cluster_autoscaler_role_arn` | the IRSA roles `scripts/eks-platform-install.sh` installs the controllers with |
| `vpc_id` | the VPC, which the load balancer controller is told at install |
| `public_subnet_cidrs` | where the load balancer connects from; the overlay's NetworkPolicy must name exactly these |
| `ingress_certificate_arn` | the validated ACM certificate, or null while no domain is set |

`modules/platform` also outputs the cluster CA data, its OIDC provider ARN, the VPC and private subnet IDs, and both KMS key ARNs, for stacks that build on an environment later.

Bootstrap outputs:
- `aws_region`
- `state_bucket_name`
- `state_kms_key_arn`
- `access_log_bucket_name`
- `cloudtrail_bucket_name`
- `cloudtrail_arn`
- `github_oidc_provider_arn`
- `terraform_plan_role_arn`
- `ecr_push_role_arn`
- `ecr_repository_urls`

Every output has a description in its `outputs.tf`.

## Validating without AWS

```sh
make tf-validate
```

This runs `scripts/tf-validate.sh`, in pinned containers like every other tool here, and CI runs it as the `terraform` job. It needs no credentials and is given none.

- **`terraform fmt -check`** checks formatting.
- **`terraform init -backend=false -lockfile=readonly`** runs for each root. A provider that doesn't match the committed checksums fails the check rather than quietly rewriting the lock file.
- **`terraform validate`** checks each root together with the modules it calls.
- **`terraform test`** plans and mock-applies each root against a mocked AWS provider (`tests/*.tftest.hcl`, mock data in `tests/mocks/`): no account and no credentials, and a mocked apply creates nothing, but every value Terraform would compute is evaluated. It catches what validate cannot, notably the validation rules on a module's inputs. The mutation test for this gate showed validate alone passing production with a single-AZ database.
- **`environments/production/tests/security.tftest.hcl`** asserts the security review's invariants on what Terraform would build, module by module: the databases are not public and admit their port from named security groups only; TLS is required on both; every log group and secret uses the right key; the cluster has no implicit admin, all five log types, and never `0.0.0.0/0`; nodes require IMDSv2 and have no SSH; the deploy role is trusted from exactly one GitHub environment and its policy has no wildcard; secrets reach AWS through write-only arguments only. The bootstrap test does the same for the buckets, the trail, and the CI roles' trust and permissions.
- **`environments/production/tests/floors.tftest.hcl`** breaks each production floor in `modules/platform/variables.tf` in turn, and expects exactly that variable to refuse it. Production refuses to run the database single-AZ, keep fewer than 7 days of backups, skip the final snapshot, run Redis without a replica, drop flow logs, or keep logs for less than 365 days.
- **`trivy config`** fails on any HIGH or CRITICAL finding.
- **`checkov`** fails on any finding.

Each suppression sits in the resource it concerns, as a `#trivy:ignore` or `#checkov:skip` comment with its reason. They fall into these groups:
- **Staging's deliberate choices.** Single-AZ, no deletion protection, one Redis node, no Performance Insights, 14-day logs. The production floors cover the ones that protect data or availability. Deletion protection deliberately has no floor, because turning it off is the first step of a deliberate teardown; the final-snapshot floor stands behind it.
- **Things the scanners cannot see.** The AUTH token set through a write-only argument, the validated endpoint CIDRs, the conditional Performance Insights key, and a Kubernetes version newer than checkov's list.
- **Key policies**, where `"*"` means the key itself.
- **The access-log bucket's S3-managed encryption**, which is an AWS requirement.
- **The public API endpoint.**
- **No cross-region replica or event notifications** on the state, trail and access-log buckets.
- **CloudTrail not mirrored into CloudWatch Logs.** Worth adding once there are alarms to define on it; until then a second copy with no reader.
- **Rotating secrets through Terraform** rather than on a timer.

The only policy-wide skip is the one-year log retention check, in `.checkov.yaml`.

What this cannot tell you:
- whether AWS accepts an instance type in a given zone
- whether an add-on exists for Kubernetes 1.34
- whether bootstrap has been applied

The environment roots look up GitHub's OIDC provider, so they cannot plan in an account without bootstrap. All three are `plan`'s job.

## Planning

Planning reads the account and changes nothing. You need credentials for an administrator role in the target account.

Terraform runs from its container here, as in `scripts/tf-validate.sh`. From the repository root:

```sh
# Git Bash on Windows: export MSYS_NO_PATHCONV=1, and use $(pwd -W) for $(pwd)
# and a C:/Users/... path for $HOME/.aws.
tf() {
  dir="$1"; shift
  docker run --rm -it \
    -v "$(pwd):/repo" -w "/repo/infrastructure/terraform/$dir" \
    -v "$HOME/.aws:/root/.aws:ro" -e AWS_PROFILE \
    -v vitalmesh-tf-plugins:/plugins -e TF_PLUGIN_CACHE_DIR=/plugins \
    hashicorp/terraform:1.16.2 "$@"
}

cp infrastructure/terraform/environments/staging/terraform.tfvars.example \
   infrastructure/terraform/environments/staging/terraform.tfvars   # then edit: git ignores it

tf environments/staging init -backend-config="bucket=<state_bucket_name>" -backend-config="kms_key_id=<state_kms_key_arn>"
tf environments/staging plan -out=staging.tfplan                    # git ignores *.tfplan
```

`terraform.tfvars` holds the account ID, the cluster admins, the addresses allowed to reach the Kubernetes API, and optionally an alarm address. None of these is a secret. The example files ship with a documentation address range (`203.0.113.0/24`) that routes nowhere, so until it is replaced nobody can reach the endpoint.

`apply` works the same way, run by a person. There is no CI apply role yet (see below).

## Recovery

[docs/DISASTER_RECOVERY.md](../../docs/DISASTER_RECOVERY.md) is the recovery document (sections 79 and 80): what holds state, the RPO and RTO targets per scenario, the backup mechanics, and the restore procedures. The short version:

- **PostgreSQL** is the only store that must be restored. Daily snapshots plus transaction logs every five minutes give point-in-time recovery to any second in the window: 14 days in production (the platform module refuses fewer than 7), 3 in staging. Production's instance leaves a final snapshot when deleted and keeps its automated backups after deletion.
- **A restore is a Terraform change**, not console work: `db_instance_suffix` plus either `db_restore_snapshot_identifier` or `db_restore_to_point_in_time` in the environment's tfvars create a new instance from the backup beside the old one; the module refuses a restore without a suffix. Verify, then a deployment repoints the application. The `database_instance_identifier` output is the source a point-in-time restore names.
- **Redis** is not backed up by design; everything in it has a TTL and a degradation path.
- **Secrets** stay recoverable for 30 days in production; **state** is versioned for 90.
- **A zone failure** is survived without a restore: Multi-AZ PostgreSQL, a Redis replica, nodes in three zones, zonal shift.

The restore test section 80 requires has not been run; the document has the procedure and the table it is recorded in.

## Cost

Section 105 asks for cost awareness. This is a portfolio project, and production is sized as production would be, not as a demo needs. The cheapest honest way to show the platform is to run staging and destroy it when idle.

Rough list prices in `eu-central-1`, rounded, in September 2026. Use the AWS pricing calculator before relying on them.

| Driver | Billed while idle | Staging | Production |
|---|---|---|---|
| EKS control plane | yes, $0.10 an hour | about $73 a month | about $73 a month |
| NAT gateways | yes, about $0.05 an hour each, plus per GB processed | 1 | 3 |
| Public IPv4 addresses (on the NATs) | yes, $0.005 an hour each | 1 | 3 |
| Application Load Balancer (created by the controller from the Ingress) | yes, about $0.025 an hour plus capacity units | 1 | 1 |
| ECR interface endpoints | yes, about $0.01 an hour each per zone | none | 2 × 3 zones, about $44 a month |
| EC2 nodes and their volumes | yes | 2 × `t3.medium` | 3 × `m6i.xlarge`, the largest item |
| RDS | yes | `db.t4g.micro` | `db.m6g.large` Multi-AZ, billed as two instances; Enhanced Monitoring adds CloudWatch Logs ingestion |
| ElastiCache | yes | 1 × `cache.t4g.micro` | 2 × `cache.m6g.large` |
| CloudWatch Logs | ingestion (about $0.63 a GB); storage is cheap | 5 control-plane log types, flow logs | the same, plus Container Insights |
| KMS keys | yes, $1 a month each | 2, plus 3 in bootstrap | 2 |
| CloudTrail | the first copy of management events is free; the bucket is cents | account-wide | account-wide |
| Secrets Manager | yes, $0.40 a month each | 3 | 3 |

With no traffic, staging comes to roughly $250 a month and production to roughly $1,500.

**Optional expensive components**, each a single value in the environment root:

| Component | Setting |
|---|---|
| Container Insights | `enable_container_insights`; per metric and per GB of logs |
| Multi-AZ PostgreSQL | doubles the database cost |
| Redis replica | doubles the Redis cost |
| NAT per zone | three times the NAT cost |
| Enhanced Monitoring | `db_monitoring_interval` |
| Scheduler and controller-manager logs | chatty; kept because they explain Pending pods |
| ECR interface endpoints | `enable_ecr_endpoints`; off in staging |
| Node autoscaling | the Cluster Autoscaler adds nodes up to `node_max_size` under load; lower the ceiling to cap the bill |

EKS's six-times-dearer extended support is avoided on purpose: `upgrade_policy = STANDARD` makes EKS upgrade the cluster instead.

## Scaling down and destroying

Nothing in an environment can be scaled to zero:
- The EKS control plane and NAT gateways bill by the hour regardless.
- ElastiCache cannot be stopped.
- A stopped RDS instance starts itself again after seven days.

An idle staging should therefore be destroyed. One apply recreates it, generating fresh secrets.

Delete the application's Ingress first (`kubectl -n vitalmesh-<env> delete ingress vitalmesh-api-gateway`) and wait for the controller to remove the load balancer: its network interfaces and security group would otherwise stop the VPC from being deleted. In production, the cluster's own deletion protection then has to be turned off (`cluster_deletion_protection = false`) in its own change, as the database's does.

**Staging.** Everything that would block a destroy is already off: deletion protection, the final snapshot, and the secret recovery window. So:

```sh
tf environments/staging destroy
```

**Production.** Four steps, in order:

1. Set `db_deletion_protection = false` in `environments/production/main.tf`, and apply that as its own reviewed change.
2. Keep what must outlive the environment:
   - **The final snapshot's key.** The snapshot is encrypted with the environment's `data` key, which destroy schedules for deletion after 30 days. Once the key is gone the snapshot cannot be restored. Either copy the snapshot under a key that stays (`aws rds copy-db-snapshot --kms-key-id …`), or cancel the key's deletion (`aws kms cancel-key-deletion`).
   - **The logs.** Destroy deletes the log groups along with their contents. Export anything the retention obligation still covers first.
3. Run `tf environments/production destroy`.
4. The secrets now sit in their 30-day recovery window. Recreating production within that window means restoring them first.

**Bootstrap.** Destroy it last, and only after every environment is gone. The state bucket, its key and the log bucket carry `prevent_destroy`, and the ECR repositories refuse to delete while they hold images. So:

1. Remove those `lifecycle` blocks deliberately.
2. Empty the buckets and repositories.
3. Destroy.

## Known gaps

Each gap is deliberate for now, and each is written down so it is not mistaken for finished work.

- **The domain is still a placeholder** (OQ-18). The overlays serve `api.vitalmesh.example` and `api.staging.vitalmesh.example`, which never resolve; `ingress_domain_name` must be set to the real host, and the overlay host changed to match, once a domain exists. Production refuses to plan until then. The DNS record itself is created by hand after the first deployment; external-dns is not installed. OQ-18's decision record is still to be written.
- **The platform components are installed by a script, not by Terraform.** The controllers are Helm charts, and a `helm_release` in Terraform would need cluster credentials at plan time and tie two lifecycles together. `scripts/eks-platform-install.sh` pins both charts and is the documented step; CI cannot exercise it without a cluster.
- **The Kubernetes API is public, restricted by address.** GitHub-hosted runners have no fixed addresses, so the CD job needs either GitHub's larger runners with static IPs or a self-hosted runner inside the VPC. The in-VPC runner would also let `cluster_endpoint_public_access` become `false`.
- **No CI apply role.** Applying needs broad rights, and a CI role holding them would be the most valuable credential in the account. Until an apply pipeline is designed around a permissions boundary, a person applies.
- **CI checks the Terraform statically only.** There is no `plan` job yet: it needs the plan role, which needs an account with bootstrap applied. There is no tflint either; its AWS ruleset would catch an instance type that does not exist, without an account. Both are listed in the implementation plan's Phase 9.
- **The services connect to PostgreSQL as the master user.** IAM database authentication is already on, for a least-privileged application role later.
- **The restore test has not been run** (section 80, OQ-32). The procedure and the table for its result are in docs/DISASTER_RECOVERY.md; it needs a staging environment applied.
- **The RDS master password rotates every seven days, and nothing carries the new one into the cluster.** RDS rotates the secret it manages on that schedule by default. Connections already open survive a rotation; new ones fail once the pool needs them, until the deployment pipeline rewrites the Kubernetes Secret. Resolve it with the application role above (IAM authentication needs no password at all) or an External Secrets Operator (OQ-37); until then, a deploy after each rotation, or turning the schedule off on the secret, is the operational answer.
- **No alarms on the audit trail.** CloudTrail lands in S3 and is read when a question arises. Alarms on root sign-in and IAM changes need the trail mirrored into CloudWatch Logs first.
- **Namespaces are applied by a cluster admin**, once. The deploy role is scoped to inside the namespace.
- **Kubernetes 1.34** leaves EKS standard support about 14 months after its release, around the end of 2026. EKS then upgrades the cluster itself. At first apply, choose the newest version EKS offers, and extend the manifests' kubeconform matrix (currently 1.30 and 1.31) to match.
- **Secrets rotate by a reviewed change, not on a schedule.** The gateway accepts one JWT key at a time, so automatic rotation would sign everyone out on a timer.
- **One AWS account, both environments.** This works and is isolated as described above. Separate accounts would be stronger, and would need the cross-account ECR policy that is not written yet.
