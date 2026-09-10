# The role the deployment pipeline uses for this environment.
#
# It is assumed through GitHub's OIDC provider, so there is no long-lived AWS
# key anywhere (section 59), and only by a job running in the GitHub
# environment of the same name. The production role therefore cannot be
# assumed from a pull request, from a branch, or from a staging job, only
# from a job that has passed the production environment's protection rules.
#
# What it may do is the minimum to deploy: find the cluster, read this
# environment's three secrets to build the Kubernetes Secrets, and manage the
# one namespace (granted as an EKS access entry in the eks module). It cannot
# push images; that is the ECR push role in bootstrap, trusted only from main.

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

  statement {
    sid     = "ReadThisEnvironmentsSecrets"
    actions = ["secretsmanager:DescribeSecret", "secretsmanager:GetSecretValue"]
    resources = [
      aws_secretsmanager_secret.app.arn,
      module.elasticache.auth_secret_arn,
      module.rds.master_user_secret_arn,
    ]
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
