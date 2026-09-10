# GitHub Actions authenticates to AWS through OIDC: each job receives a
# short-lived token naming its repository, branch or environment, and trades
# it for temporary credentials. No AWS access key is stored anywhere
# (section 59).
#
# Two roles here, each trusted from an exact subject and nothing broader. The
# environment deploy roles live in each environment stack (modules/platform/
# deploy.tf), trusted only from the GitHub environment of the same name.
#
# There is deliberately no apply role. Applying infrastructure needs broad
# rights, and a CI role holding them is the most valuable credential in the
# account; until an apply pipeline is designed around a permissions boundary,
# applies are made by a person (section 111).

resource "aws_iam_openid_connect_provider" "github" {
  url            = "https://${local.github_oidc_host}"
  client_id_list = ["sts.amazonaws.com"]
}

# ------------------------------------------------------------------ plan

data "aws_iam_policy_document" "plan_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.github_oidc_host}:aud"
      values   = ["sts.amazonaws.com"]
    }

    # Pull requests and main, by exact subject. GitHub does not issue OIDC
    # tokens to workflows triggered from forks, so a fork's pull request
    # cannot assume this either.
    condition {
      test     = "StringEquals"
      variable = "${local.github_oidc_host}:sub"
      values = [
        "repo:${var.github_repository}:pull_request",
        "repo:${var.github_repository}:ref:refs/heads/main",
      ]
    }
  }
}

resource "aws_iam_role" "plan" {
  name                 = "vitalmesh-terraform-plan"
  description          = "Read-only terraform plan for VitalMesh, from pull requests and main"
  assume_role_policy   = data.aws_iam_policy_document.plan_assume.json
  max_session_duration = 3600
}

# AWS's ReadOnlyAccess lets plan read every resource it manages. It does not
# include secretsmanager:GetSecretValue, so the role can see that a secret
# exists but not what it holds. Plan does not need it either: the secret
# versions are written through write-only arguments, and the provider
# refreshes those with ListSecretVersionIds rather than by reading the value.
#
# ReadOnlyAccess is still broader than "read the configuration": it also
# reads data. The deny statements in plan_state below take that back.
resource "aws_iam_role_policy_attachment" "plan_read_only" {
  role       = aws_iam_role.plan.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/ReadOnlyAccess"
}

# Plan also reads state and holds the lock while it runs. The lock is an
# object beside the state, so the only writes allowed are to *.tflock.
data "aws_iam_policy_document" "plan_state" {
  # Plan reads how things are configured, never what they contain. The
  # managed policy above does not draw that line: it would let a job
  # triggered by any pull request read the EKS audit log, the database
  # connection log, every object in every bucket, every image layer, and
  # any SSM parameter. None of that is needed to compute a plan, so it is
  # denied here, with the two exceptions plan does need: the state bucket's
  # objects, and decrypting with the state key to read them. Public SSM
  # parameters (AMI IDs and the like, under /aws/service/) stay readable.
  statement {
    sid    = "NeverReadData"
    effect = "Deny"
    actions = [
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "logs:FilterLogEvents",
      "logs:GetLogEvents",
      "logs:GetLogRecord",
      "logs:GetQueryResults",
      "logs:StartLiveTail",
      "logs:StartQuery",
      "rds:DownloadCompleteDBLogFile",
      "rds:DownloadDBLogFilePortion",
      "secretsmanager:GetSecretValue",
    ]
    resources = ["*"]
  }

  statement {
    sid           = "NeverReadObjectsOutsideState"
    effect        = "Deny"
    actions       = ["s3:GetObject", "s3:GetObjectVersion"]
    not_resources = ["${aws_s3_bucket.state.arn}/*"]
  }

  statement {
    sid           = "NeverDecryptWithAnyOtherKey"
    effect        = "Deny"
    actions       = ["kms:Decrypt"]
    not_resources = [aws_kms_key.state.arn]
  }

  statement {
    sid           = "NeverReadPrivateParameters"
    effect        = "Deny"
    actions       = ["ssm:GetParameter", "ssm:GetParameterHistory", "ssm:GetParameters", "ssm:GetParametersByPath"]
    not_resources = ["arn:${local.partition}:ssm:*::parameter/aws/service/*"]
  }

  statement {
    sid       = "ListState"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.state.arn]
  }

  statement {
    sid       = "ReadState"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.state.arn}/*"]
  }

  statement {
    sid       = "HoldTheLockAndNothingElse"
    actions   = ["s3:DeleteObject", "s3:PutObject"]
    resources = ["${aws_s3_bucket.state.arn}/*.tflock"]
  }

  statement {
    sid       = "UseTheStateKey"
    actions   = ["kms:Decrypt", "kms:DescribeKey", "kms:GenerateDataKey"]
    resources = [aws_kms_key.state.arn]
  }
}

resource "aws_iam_role_policy" "plan_state" {
  name   = "terraform-state"
  role   = aws_iam_role.plan.id
  policy = data.aws_iam_policy_document.plan_state.json
}

# ------------------------------------------------------------ image push

data "aws_iam_policy_document" "ecr_push_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.github_oidc_host}:aud"
      values   = ["sts.amazonaws.com"]
    }

    # main only. An image is pushed after it has merged, so a pull request
    # cannot place an image in the registry production pulls from.
    condition {
      test     = "StringEquals"
      variable = "${local.github_oidc_host}:sub"
      values   = ["repo:${var.github_repository}:ref:refs/heads/main"]
    }
  }
}

resource "aws_iam_role" "ecr_push" {
  name                 = "vitalmesh-ecr-push"
  description          = "Pushes VitalMesh images from main to its own ECR repositories"
  assume_role_policy   = data.aws_iam_policy_document.ecr_push_assume.json
  max_session_duration = 3600
}

data "aws_iam_policy_document" "ecr_push" {
  # GetAuthorizationToken is account-wide by AWS's design and cannot be
  # scoped to a repository; the token it returns grants nothing the
  # statement below does not.
  statement {
    sid       = "LogInToTheRegistry"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid = "PushToTheseRepositoriesOnly"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:CompleteLayerUpload",
      "ecr:DescribeImages",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
    ]
    resources = [for r in aws_ecr_repository.this : r.arn]
  }
}

resource "aws_iam_role_policy" "ecr_push" {
  name   = "push-images"
  role   = aws_iam_role.ecr_push.id
  policy = data.aws_iam_policy_document.ecr_push.json
}
