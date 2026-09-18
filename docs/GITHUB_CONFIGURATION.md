# GitHub ↔ AWS: the exact repository configuration

How GitHub Actions authenticates to AWS, what it may do once it has, and
every setting the repository needs for that to work. Nothing in this
document is a secret, and nothing here is stored in a repository file:
the values below live in GitHub's settings, and the trust that makes them
mean anything lives in Terraform.

- [How authentication works](#how-authentication-works)
- [The roles](#the-roles)
- [Repository variables](#repository-variables)
- [Environments](#environments)
- [Branch protection](#branch-protection)
- [Actions settings](#actions-settings)
- [Verifying it](#verifying-it)
- [What is deliberately not configured](#what-is-deliberately-not-configured)

## How authentication works

There are no AWS access keys: not in GitHub secrets, not in files, not in
anyone's home directory for CI's sake. Each job that needs AWS asks GitHub
for an OIDC token, hands it to AWS STS, and gets back credentials that last
one hour and name the job that asked for them:

```text
GitHub Actions job ──(id-token: write)──► GitHub OIDC provider issues a JWT
      │                                    sub = repo:n0ah-n0wa/VitalMesh:<what triggered it>
      │                                    aud = sts.amazonaws.com
      ▼
aws-actions/configure-aws-credentials ──► sts:AssumeRoleWithWebIdentity
      │                                    IAM checks the role's trust policy
      │                                    against sub and aud, exactly
      ▼
temporary credentials for one role, one hour, one session named
gha-<what>-<run id>: what CloudTrail records
```

The subject (`sub`) is what makes trust narrow. GitHub sets it from the
event, and it cannot be chosen by the workflow:

| Job triggered by | `sub` claim | Role that trusts it |
|---|---|---|
| a pull request | `repo:n0ah-n0wa/VitalMesh:pull_request` | `vitalmesh-terraform-plan` |
| a push to `main` (or a dispatch from it) | `repo:n0ah-n0wa/VitalMesh:ref:refs/heads/main` | `vitalmesh-terraform-plan`, `vitalmesh-ecr-push` |
| a job with `environment: staging` | `repo:n0ah-n0wa/VitalMesh:environment:staging` | `vitalmesh-staging-deploy` |
| a job with `environment: production` | `repo:n0ah-n0wa/VitalMesh:environment:production` | `vitalmesh-production-deploy` |

Every trust policy uses `StringEquals` on the full subject: no wildcard,
no prefix match. A branch other than `main`, a tag, a fork, another
repository, or a job outside the named environment gets
`AccessDenied` from STS before any AWS API is reached. GitHub does not
issue OIDC tokens to workflows triggered by pull requests from forks at
all.

The IAM OIDC provider (`token.actions.githubusercontent.com`, audience
`sts.amazonaws.com`) is created once per account by
`infrastructure/terraform/bootstrap/github.tf`; AWS validates GitHub's
signing keys against its own trust store, so no thumbprint is pinned.

## The roles

All four are Terraform, so a change to what CI may do is a reviewed diff,
and `make tf-validate` asserts the properties below on every change
(`bootstrap/tests/plan.tftest.hcl`,
`environments/production/tests/security.tftest.hcl`).

| Role | Trusted from | May | May not |
|---|---|---|---|
| `vitalmesh-terraform-plan` | pull requests; `main` | `ReadOnlyAccess`; read Terraform state; write the state lock (`*.tflock`); use the state key | read log contents, secret values, S3 objects outside the state bucket, image layers, private SSM parameters, or decrypt with any other key (explicit denies); change anything |
| `vitalmesh-ecr-push` | `main` only | push to the two `vitalmesh/*` repositories | delete images, change repositories, push anywhere else |
| `vitalmesh-staging-deploy` | the `staging` environment only | describe its cluster; read `/vitalmesh/staging/deploy` (SSM) and staging's four secrets (the application secret, the Redis auth token, the RDS master credential, and the end-to-end account's, the last only because staging sets `create_e2e_account`); resolve image digests; administer the `vitalmesh-staging` namespace | read state; push images; anything in production; any write to AWS |
| `vitalmesh-production-deploy` | the `production` environment only | the same, for production, with three secrets: production creates no end-to-end account | the same, plus anything in staging |

The deploy roles' Kubernetes rights are EKS access entries scoped to one
namespace (`modules/eks/access.tf`). The Namespace itself, its
ResourceQuota and LimitRange are applied once by a cluster admin; the
deploy job renders them out (`deploy.yml`) because the role cannot touch
them, which is the point.

Where the roles are used:

| Workflow | Role | When |
|---|---|---|
| `terraform-plan.yml` | plan | pull requests and pushes to `main` touching `infrastructure/terraform/` or the workflow file itself; also a manual dispatch, refused off `main` |
| `release.yml`, job `images` | ecr-push | every CI run **started by a push** to `main` **in this repository** that succeeded, for the commit that run tested; or a manual dispatch from `main`. The `push` and same-repository conditions are what exclude a fork's pull request, whose CI run also reports a head branch called `main` |
| `release.yml` → `deploy.yml`, staging | staging-deploy | after the images, automatically; the E2E job uses the same role to read the test account's secret |
| `promote.yml` → `deploy.yml`, production | production-deploy | a manual dispatch with a Release run ID, after the production reviewers approve; docs/DEPLOYMENT.md, "Production" |

## Repository variables

Settings → Secrets and variables → Actions → **Variables** (not Secrets:
none of these is one). Until `AWS_ACCOUNT_ID` is set, `release.yml` and
`terraform-plan.yml` skip themselves with a notice; CI (`ci.yml`) never
needs AWS. `promote.yml` has no such guard: dispatched without the
variable it runs its verification and then fails at the role assumption.

| Variable | Value | From |
|---|---|---|
| `AWS_ACCOUNT_ID` | the twelve-digit account ID | `aws sts get-caller-identity` |
| `AWS_REGION` | `eu-central-1` (optional; this is the default) | |
| `TF_STATE_BUCKET` | the state bucket | `terraform output -raw state_bucket_name` in `bootstrap/` |
| `TF_STATE_KMS_KEY_ARN` | the state key | `terraform output -raw state_kms_key_arn` in `bootstrap/` |
| `TF_VARS_STAGING` | the contents of `environments/staging/terraform.tfvars` | the file itself (git-ignored; see its `.example`) |
| `TF_VARS_PRODUCTION` | the contents of `environments/production/terraform.tfvars` | the file itself |

The two `TF_VARS_*` variables hold account IDs, IAM role ARNs, allowed
CIDRs, a domain, a hosted-zone ID and optionally an email address. They
are specific to one account, which is why they are not committed, and they
are not credentials, which is why they are variables and not secrets (a
secret would be masked in plan output, where these values are supposed to
be readable).

With the `gh` CLI, from a checkout with the tfvars files in place:

```sh
gh variable set AWS_ACCOUNT_ID       --body "123456789012"
gh variable set AWS_REGION           --body "eu-central-1"
gh variable set TF_STATE_BUCKET      --body "$(cd infrastructure/terraform/bootstrap && terraform output -raw state_bucket_name)"
gh variable set TF_STATE_KMS_KEY_ARN --body "$(cd infrastructure/terraform/bootstrap && terraform output -raw state_kms_key_arn)"
gh variable set TF_VARS_STAGING      < infrastructure/terraform/environments/staging/terraform.tfvars
gh variable set TF_VARS_PRODUCTION   < infrastructure/terraform/environments/production/terraform.tfvars
gh variable list
```

## Environments

Settings → **Environments**. Two, named exactly as the deploy roles'
trust policies expect. The names are the security boundary: a job that
does not declare `environment: production` cannot obtain a token the
production role accepts.

| | `staging` | `production` |
|---|---|---|
| Required reviewers | none | at least one; **prevent self-review** on |
| Wait timer | 0 | 0 (a reviewer is the gate, not a clock) |
| Deployment branches | `main` only (protected branches) | `main` only (protected branches) |
| Environment secrets / variables | none | none |

Required reviewers on `production` are what "production protection"
means here: the `deploy-production` job of `promote.yml` calls `deploy.yml`
with `environment: production`, and the called `deploy` job declares
`environment: ${{ inputs.environment }}`. That declaration is what pauses
the run until someone other than the person who dispatched it approves it
on the run's page, and the OIDC token for
the production role is not issued until then. Rejecting the review
cancels the run. The run summary the reviewer sees names the commit and
the digests that would be deployed and what staging ran on them. The
branch policy means the environment (and so the role) can be reached only
from `main`, which branch protection below keeps pull-request-only.

```sh
OWNER=n0ah-n0wa; REPO=VitalMesh
REVIEWER_ID="$(gh api users/<github-login> --jq .id)"

gh api --method PUT "repos/$OWNER/$REPO/environments/staging" \
  --input - <<'EOF'
{ "wait_timer": 0, "prevent_self_review": false, "reviewers": [],
  "deployment_branch_policy": { "protected_branches": true, "custom_branch_policies": false } }
EOF

gh api --method PUT "repos/$OWNER/$REPO/environments/production" \
  --input - <<EOF
{ "wait_timer": 0, "prevent_self_review": true,
  "reviewers": [ { "type": "User", "id": $REVIEWER_ID } ],
  "deployment_branch_policy": { "protected_branches": true, "custom_branch_policies": false } }
EOF

gh api "repos/$OWNER/$REPO/environments" --jq '.environments[] | {name, protection_rules, deployment_branch_policy}'
```

Add more reviewers as `{ "type": "User", "id": … }` entries, or a team as
`{ "type": "Team", "id": … }`. With a single-maintainer repository,
prevent-self-review means a second GitHub account (or a team) has to hold
the approval; that is a property of the protection, not a bug in it.

## Branch protection

Settings → Branches → **Add rule** for `main`. `main` is what the push
role and both environments trust, so what can reach `main` is part of the
authentication design.

| Setting | Value |
|---|---|
| Require a pull request before merging | on, 1 approving review, dismiss stale approvals |
| Require status checks to pass | on; required check: **`ci`** (the verdict job in `ci.yml`, which fails unless all eleven jobs passed); require branches to be up to date |
| Require conversation resolution | on |
| Require linear history | on |
| Do not allow bypassing the above settings | on (applies to administrators) |
| Allow force pushes / deletions | off |

```sh
gh api --method PUT "repos/$OWNER/$REPO/branches/main/protection" --input - <<'EOF'
{
  "required_status_checks": { "strict": true, "contexts": ["ci"] },
  "enforce_admins": true,
  "required_pull_request_reviews": { "required_approving_review_count": 1, "dismiss_stale_reviews": true },
  "restrictions": null,
  "required_linear_history": true,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_conversation_resolution": true
}
EOF
```

`terraform-plan.yml`'s jobs are deliberately not required checks: they
need an account, and a plan that cannot run because the account is not yet
configured must not block merging code.

## Actions settings

Settings → Actions → General:

| Setting | Value | Why |
|---|---|---|
| Actions permissions | allow actions from GitHub, verified creators, and the pinned actions in `.github/workflows` | every action is pinned to a commit SHA; a moved tag cannot change what runs |
| Dependabot | on (`.github/dependabot.yml`; enable Dependabot security updates in Settings → Code security) | weekly grouped update PRs for actions, Go modules, crates and base images, each gated by CI |
| Fork pull request workflows | require approval for all outside collaborators | a fork cannot obtain an OIDC token anyway; this keeps CI minutes and secrets-free jobs from running unreviewed code |
| Workflow permissions | read repository contents (the workflows raise `id-token: write` per job themselves) | the default token can do nothing it is not given |
| Allow GitHub Actions to create and approve pull requests | off | |

## Verifying it

1. Set `AWS_ACCOUNT_ID` and the other variables, apply `bootstrap/` if not
   yet applied, then run **Terraform plan** from the Actions tab (from
   `main`: the plan role does not trust other branches). The `Identity`
   step prints the assumed role:
   `arn:aws:sts::<account>:assumed-role/vitalmesh-terraform-plan/gha-plan-<run id>`.
2. In CloudTrail, the same run appears as an `AssumeRoleWithWebIdentity`
   event whose `userIdentity` names `token.actions.githubusercontent.com`
   and whose request parameters carry the `sub` claim above: which
   repository, which trigger. That is the audit trail for every CI action
   in AWS, and the trail itself is created by bootstrap (`logging.tf`).
3. Push to `main` and let CI pass: **Release** then builds, verifies,
   scans and pushes the images for that commit (tag `sha-<commit>`; never
   `latest`), outputs their digests, deploys staging by digest, smoke-tests
   it from the outside and runs the end-to-end demo against it. A commit
   whose CI failed is not released, and the run says so. The smoke test
   needs the staging hostname to resolve: `ingress_domain_name` set in
   staging's tfvars and the DNS record created (the Terraform README's
   first-time order, step 5).
4. Promote: `gh workflow run promote.yml --ref main -f release_run_id=<the
   Release run's ID>`. The `verify` job's summary shows what would be
   deployed; the `production` job waits for a reviewer; approve it as a
   second account. Try it wrong, once: dispatch with the ID of a run that
   failed staging, and watch `verify` refuse it for want of a release
   record.
5. Try it wrong, once: dispatch **Terraform plan** from a branch other
   than `main`. STS refuses with `Not authorized to perform
   sts:AssumeRoleWithWebIdentity`, which is the trust policy working.

## What is deliberately not configured

- **No CI apply.** Applying infrastructure needs broad rights, and a role
  holding them would be the most valuable credential in the account. A
  person applies, from a reviewed plan (`infrastructure/terraform/README.md`).
- **No GitHub secrets.** There is nothing to store: no keys, no
  passwords, no tokens. If a secret ever appears in the repository's
  settings, something has gone wrong with this design.
- **No environment variables in the environments.** Everything a deploy
  needs comes from the SSM parameter Terraform keeps current
  (`/vitalmesh/<env>/deploy`) and from Secrets Manager, read at deploy
  time with the environment's role; copying endpoints into GitHub by hand
  is how a restore under a new name gets missed. The end-to-end account's
  password is a Secrets Manager secret too (`vitalmesh/staging/e2e`,
  generated by Terraform, staging only), never a GitHub secret.
- **No cross-environment trust.** The staging role cannot reach
  production's secrets or cluster and vice versa: separate roles, separate
  KMS keys, separate namespaces, separate subjects.
