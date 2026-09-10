# The account's audit trail (section 30: audit logging; section 104:
# auditability), in two parts.
#
#   CloudTrail      every management API call made in the account, in every
#                   region: who created a role, changed a security group,
#                   read a secret, opened the cluster endpoint. Without a
#                   trail, AWS keeps ninety days of this in a console view
#                   and nothing more.
#   access logs     every request made to the state bucket and to the trail
#                   bucket itself, since S3 object reads are data events that
#                   the management trail does not record.
#
# Everything here is append-only in practice: the buckets are versioned so a
# deleted log object stays recoverable, the trail's files carry digests that
# prove nothing was altered after delivery, and prevent_destroy guards all of
# it. A record of what happened is only worth keeping if it cannot be
# quietly edited.

locals {
  trail_name = "vitalmesh"
  trail_arn  = "arn:${local.partition}:cloudtrail:${var.aws_region}:${local.account_id}:trail/${local.trail_name}"

  access_logs_bucket_name = "vitalmesh-access-logs-${local.account_id}-${var.aws_region}"
  cloudtrail_bucket_name  = "vitalmesh-cloudtrail-${local.account_id}-${var.aws_region}"
}

# ------------------------------------------------------------- access logs

# S3 delivers access logs itself, and only into a bucket encrypted with S3's
# own keys: AWS does not support a customer-managed key on an access log
# destination. This is the one bucket here without one.
#trivy:ignore:AVD-AWS-0089
resource "aws_s3_bucket" "access_logs" {
  #checkov:skip=CKV_AWS_145:S3 delivers access logs only into buckets encrypted with S3-managed keys. A customer-managed key is not supported on a log destination.
  #checkov:skip=CKV_AWS_18:This is the access log bucket. Logging it would record its own log deliveries, which AWS advises against.
  #checkov:skip=CKV_AWS_144:No cross-region replica, as for the buckets it records.
  #checkov:skip=CKV2_AWS_62:Nothing consumes bucket events.

  bucket = local.access_logs_bucket_name

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Versioned so that deleting a log object leaves it recoverable for a month:
# an audit trail that can be quietly edited is not one.
resource "aws_s3_bucket_versioning" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  versioning_configuration {
    status = "Enabled"
  }
}

# S3-managed keys, because S3 delivers access logs into no other kind: see
# the comment on the bucket above.
#trivy:ignore:AVD-AWS-0132
resource "aws_s3_bucket_server_side_encryption_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# A year, matching production's log retention.
resource "aws_s3_bucket_lifecycle_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  rule {
    id     = "expire-access-logs"
    status = "Enabled"

    filter {}

    expiration {
      days = 365
    }

    noncurrent_version_expiration {
      noncurrent_days = 30
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.access_logs]
}

data "aws_iam_policy_document" "access_logs" {
  # AWS's documented grant, with both confused-deputy conditions: only S3's
  # log delivery, only on behalf of the two buckets named, only in this
  # account, and each into its own prefix.
  statement {
    sid       = "S3DeliversTheStateBucketsAccessLogs"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.access_logs.arn}/state/*"]

    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = [aws_s3_bucket.state.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account_id]
    }
  }

  statement {
    sid       = "S3DeliversTheTrailBucketsAccessLogs"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.access_logs.arn}/cloudtrail/*"]

    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = [aws_s3_bucket.cloudtrail.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account_id]
    }
  }

  statement {
    sid     = "RefuseUnencryptedTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.access_logs.arn,
      "${aws_s3_bucket.access_logs.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  policy = data.aws_iam_policy_document.access_logs.json

  depends_on = [aws_s3_bucket_public_access_block.access_logs]
}

# -------------------------------------------------------------- CloudTrail

data "aws_iam_policy_document" "cloudtrail_key" {
  #checkov:skip=CKV_AWS_109:A key policy. Its resource wildcard means this key only, and the first statement is AWS's default policy delegating the key to IAM.
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

  # AWS's documented grant for CloudTrail: it may generate data keys for
  # this account's trail and nothing else. Reading the logs back needs
  # kms:Decrypt, which the account grants through IAM.
  statement {
    sid       = "CloudTrailEncryptsThisAccountsTrail"
    actions   = ["kms:GenerateDataKey*"]
    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = ["cloudtrail.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceArn"
      values   = [local.trail_arn]
    }

    condition {
      test     = "StringLike"
      variable = "kms:EncryptionContext:aws:cloudtrail:arn"
      values   = ["arn:${local.partition}:cloudtrail:*:${local.account_id}:trail/*"]
    }
  }

  statement {
    sid       = "CloudTrailDescribesTheKey"
    actions   = ["kms:DescribeKey"]
    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = ["cloudtrail.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceArn"
      values   = [local.trail_arn]
    }
  }
}

resource "aws_kms_key" "cloudtrail" {
  description             = "VitalMesh CloudTrail"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.cloudtrail_key.json

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "cloudtrail" {
  name          = "alias/vitalmesh-cloudtrail"
  target_key_id = aws_kms_key.cloudtrail.key_id
}

resource "aws_s3_bucket" "cloudtrail" {
  #checkov:skip=CKV_AWS_144:No cross-region replica. The trail's digest files prove the record is intact; a second copy in another region is a compliance need this project does not have.
  #checkov:skip=CKV2_AWS_62:Nothing consumes bucket events.

  bucket = local.cloudtrail_bucket_name

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "cloudtrail" {
  bucket = aws_s3_bucket.cloudtrail.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "cloudtrail" {
  bucket = aws_s3_bucket.cloudtrail.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "cloudtrail" {
  bucket = aws_s3_bucket.cloudtrail.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cloudtrail" {
  bucket = aws_s3_bucket.cloudtrail.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.cloudtrail.arn
    }

    bucket_key_enabled = true
  }
}

# A year, matching production's log retention. Raise it here if a retention
# obligation says otherwise; it costs cents.
resource "aws_s3_bucket_lifecycle_configuration" "cloudtrail" {
  bucket = aws_s3_bucket.cloudtrail.id

  rule {
    id     = "expire-trail"
    status = "Enabled"

    filter {}

    expiration {
      days = 365
    }

    noncurrent_version_expiration {
      noncurrent_days = 30
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.cloudtrail]
}

resource "aws_s3_bucket_logging" "cloudtrail" {
  bucket        = aws_s3_bucket.cloudtrail.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "cloudtrail/"

  target_object_key_format {
    partitioned_prefix {
      partition_date_source = "EventTime"
    }
  }

  depends_on = [aws_s3_bucket_policy.access_logs]
}

data "aws_iam_policy_document" "cloudtrail_bucket" {
  # AWS's documented pair of grants, each limited to this account's trail.
  statement {
    sid       = "CloudTrailChecksTheBucketAcl"
    actions   = ["s3:GetBucketAcl"]
    resources = [aws_s3_bucket.cloudtrail.arn]

    principals {
      type        = "Service"
      identifiers = ["cloudtrail.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceArn"
      values   = [local.trail_arn]
    }
  }

  statement {
    sid       = "CloudTrailWritesThisAccountsLogs"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.cloudtrail.arn}/AWSLogs/${local.account_id}/*"]

    principals {
      type        = "Service"
      identifiers = ["cloudtrail.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceArn"
      values   = [local.trail_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "s3:x-amz-acl"
      values   = ["bucket-owner-full-control"]
    }
  }

  statement {
    sid     = "RefuseUnencryptedTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.cloudtrail.arn,
      "${aws_s3_bucket.cloudtrail.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "cloudtrail" {
  bucket = aws_s3_bucket.cloudtrail.id
  policy = data.aws_iam_policy_document.cloudtrail_bucket.json

  depends_on = [aws_s3_bucket_public_access_block.cloudtrail]
}

# Management events, read and write, from every region, with global services
# (IAM, STS) included. The first copy of management events is free; the
# bucket is the cost, and it is cents.
#
# Not mirrored into CloudWatch Logs (trivy AVD-AWS-0162, checkov
# CKV2_AWS_10). That is worth adding once there are alarms to define on it
# (root sign-in, IAM changes); until then it is a second copy at ingestion
# cost with no reader.
#trivy:ignore:AVD-AWS-0162
resource "aws_cloudtrail" "this" {
  #checkov:skip=CKV_AWS_252:No SNS topic per delivered file. Nothing consumes such notifications; the logs are read from the bucket when a question arises.
  #checkov:skip=CKV2_AWS_10:Not mirrored into CloudWatch Logs. That is worth adding once there are alarms to define on it (root sign-in, IAM changes); until then it is a second copy at ingestion cost with no reader.

  name           = local.trail_name
  s3_bucket_name = aws_s3_bucket.cloudtrail.id
  kms_key_id     = aws_kms_key.cloudtrail.arn

  is_multi_region_trail         = true
  include_global_service_events = true
  enable_log_file_validation    = true
  enable_logging                = true

  depends_on = [aws_s3_bucket_policy.cloudtrail]
}
