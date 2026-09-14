# The role the deployment pipeline uses for this environment.
#
# It is assumed through GitHub's OIDC provider, so there is no long-lived AWS
# key anywhere (section 59), and only by a job running in the GitHub
# environment of the same name. The production role therefore cannot be
# assumed from a pull request, from a branch, or from a staging job, only
# from a job that has passed the production environment's protection rules.
#
# What it may do is the minimum to deploy (.github/workflows/deploy.yml):
# find the cluster; read one SSM parameter that names this environment's
# endpoints and secrets; read those three secrets to build the Kubernetes
# Secrets; describe the two image repositories to resolve a tag to a
# digest; and manage the one namespace (granted as an EKS access entry in
# the eks module). It cannot push images (that is the ECR push role in
# bootstrap, trusted only from main), cannot read Terraform state, and
# cannot touch the other environment: every resource below is this
# environment's own, and nothing is granted on "*".

# Created once per account by bootstrap. Looked up rather than passed in, so
# this stack has no dependency on bootstrap's state.
data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

data "aws_iam_policy_document" "deploy_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [data.aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Exact match, no wildcard: this repository, this GitHub environment.
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:environment:${var.environment}"]
    }
  }
}

resource "aws_iam_role" "deploy" {
  name                 = "${local.name}-deploy"
  description          = "Deploys VitalMesh ${var.environment}; assumable only from the ${var.environment} GitHub environment"
  assume_role_policy   = data.aws_iam_policy_document.deploy_assume.json
  max_session_duration = 3600

  tags = local.tags
}

data "aws_iam_policy_document" "deploy" {
  statement {
    sid       = "FindTheCluster"
    actions   = ["eks:DescribeCluster"]
    resources = [module.eks.cluster_arn]
  }

  # Where everything is: the parameter below, which Terraform keeps current.
  statement {
    sid       = "ReadThisEnvironmentsDeploymentFacts"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.deploy.arn]
  }

  # A deployment names images by digest (section 32), which it resolves
  # from the tag the release workflow pushed. The repositories live in
  # bootstrap, so they are named by pattern rather than by reference.
  statement {
    sid       = "ResolveImageDigests"
    actions   = ["ecr:DescribeImages"]
    resources = ["arn:${data.aws_partition.current.partition}:ecr:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:repository/vitalmesh/*"]
  }

  statement {
    sid     = "ReadThisEnvironmentsSecrets"
    actions = ["secretsmanager:DescribeSecret", "secretsmanager:GetSecretValue"]
    resources = concat(
      [
        aws_secretsmanager_secret.app.arn,
        module.elasticache.auth_secret_arn,
        module.rds.master_user_secret_arn,
      ],
      var.create_e2e_account ? [aws_secretsmanager_secret.e2e[0].arn] : [],
    )
  }

  # The secrets are encrypted with the data key. Decrypt is allowed only when
  # Secrets Manager is the caller, so the role cannot use the key directly
  # against anything else it protects, such as database snapshots.
  statement {
    sid       = "DecryptThroughSecretsManagerOnly"
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.data.arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.region}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "deploy-${var.environment}"
  role   = aws_iam_role.deploy.id
  policy = data.aws_iam_policy_document.deploy.json
}

# The non-secret facts a deployment needs, published where the deploy role
# can read them: the Terraform outputs are in state, which the role
# deliberately cannot reach, and copying them into GitHub by hand is how
# an environment ends up deployed against a database that was restored
# under a new name last month. Terraform rewrites this whenever any of it
# changes. No value here is a credential; the secrets are named by ARN.
resource "aws_ssm_parameter" "deploy" {
  #checkov:skip=CKV2_AWS_34:Deliberately a plain String. It holds endpoints and the ARNs of secrets, not secret values; those live in Secrets Manager under the data key. Encrypting it would grant the deploy role KMS use for nothing it protects. The security test asserts it carries ARNs, not values.

  name        = "/vitalmesh/${var.environment}/deploy"
  description = "Endpoints and secret ARNs for deploying VitalMesh ${var.environment}; read by the deploy role"
  type        = "String"
  tier        = "Standard"

  value = jsonencode({
    cluster_name               = module.eks.cluster_name
    namespace                  = local.namespace
    database_address           = module.rds.address
    database_port              = module.rds.port
    database_name              = module.rds.database_name
    database_master_secret_arn = module.rds.master_user_secret_arn
    redis_primary_endpoint     = module.elasticache.primary_endpoint_address
    redis_port                 = module.elasticache.port
    redis_auth_secret_arn      = module.elasticache.auth_secret_arn
    app_secret_arn             = aws_secretsmanager_secret.app.arn
    ingress_host               = var.ingress_domain_name
    e2e_email                  = var.create_e2e_account ? local.e2e_email : null
    e2e_secret_arn             = var.create_e2e_account ? aws_secretsmanager_secret.e2e[0].arn : null
  })

  tags = local.tags
}
