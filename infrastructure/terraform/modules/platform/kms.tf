# Two customer-managed keys per environment, split by who needs to use them.
#
#   data  RDS storage, the RDS master password, ElastiCache at rest, the
#         application and Redis secrets, and Kubernetes Secrets in etcd
#   logs  every CloudWatch log group, and the alarm topic
#
# The split is the point. CloudWatch Logs can only encrypt with a key whose
# policy names the Logs service as a principal, and granting that on the key
# that also protects the database would let a service with no business near
# the data use it. Keeping the grant on a key that protects only logs keeps
# it harmless.
#
# One pair per environment rather than one shared pair, so staging and
# production share no key: disabling or scheduling deletion of one cannot
# touch the other.

locals {
  account_root = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"
}

# The account administers the key through IAM, which is AWS's default key
# policy written out so it is reviewable. Services that use the key (RDS,
# ElastiCache, Secrets Manager) do so through grants they create on behalf of
# a caller who has IAM permission, and the EKS cluster role is given
# permission explicitly in the eks module.
data "aws_iam_policy_document" "data_key" {
  #checkov:skip=CKV_AWS_109:A key policy. Its resource wildcard means this key only, and this statement is AWS's default policy delegating the key to IAM.
  #checkov:skip=CKV_AWS_111:As CKV_AWS_109.
  #checkov:skip=CKV_AWS_356:As CKV_AWS_109.

  statement {
    sid       = "AccountAdministersThroughIAM"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = [local.account_root]
    }
  }
}

resource "aws_kms_key" "data" {
  description             = "VitalMesh ${var.environment}: database, cache, secrets and Kubernetes Secrets"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.data_key.json

  tags = local.tags
}

resource "aws_kms_alias" "data" {
  name          = "alias/${local.name}-data"
  target_key_id = aws_kms_key.data.key_id
}

data "aws_iam_policy_document" "logs_key" {
  #checkov:skip=CKV_AWS_109:A key policy. Its resource wildcard means this key only, and this statement is AWS's default policy delegating the key to IAM.
  #checkov:skip=CKV_AWS_111:As CKV_AWS_109.
  #checkov:skip=CKV_AWS_356:As CKV_AWS_109.

  statement {
    sid       = "AccountAdministersThroughIAM"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = [local.account_root]
    }
  }

  # CloudWatch Logs encrypts log groups itself and so has to be named here.
  # The condition limits it to this environment's log groups: every module
  # names its groups /aws/<service>/<environment name>/<log>, and ArnLike's
  # wildcard spans path segments, so one pattern covers them all. A log
  # group anywhere else in the account cannot be attached to this key.
  statement {
    sid = "CloudWatchLogsEncryptsThisAccountsLogGroups"
    actions = [
      "kms:Decrypt*",
      "kms:Describe*",
      "kms:Encrypt*",
      "kms:GenerateDataKey*",
      "kms:ReEncrypt*",
    ]
    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = ["logs.${data.aws_region.current.region}.amazonaws.com"]
    }

    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:${data.aws_partition.current.partition}:logs:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:log-group:/aws/*/${local.name}/*"]
    }
  }

  # CloudWatch alarms and RDS event notifications publish to the alarm
  # topic, which is encrypted with this key, and each publisher needs the
  # key to do so. Both statements are AWS's documented ones.
  statement {
    sid       = "CloudWatchAlarmsPublishToTheEncryptedTopic"
    actions   = ["kms:Decrypt", "kms:GenerateDataKey*"]
    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = ["cloudwatch.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid       = "RDSEventsPublishToTheEncryptedTopic"
    actions   = ["kms:Decrypt", "kms:GenerateDataKey*"]
    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = ["events.rds.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_kms_key" "logs" {
  description             = "VitalMesh ${var.environment}: log groups and the alarm topic"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.logs_key.json

  tags = local.tags
}

resource "aws_kms_alias" "logs" {
  name          = "alias/${local.name}-logs"
  target_key_id = aws_kms_key.logs.key_id
}
