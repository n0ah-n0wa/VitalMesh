# Bootstrap against a mocked AWS provider: no account and no credentials,
# but a real plan and a mocked apply (which creates nothing) so that the
# references between resources resolve. That evaluates what terraform
# validate cannot: variable validation, for_each keys, conditional
# expressions, and the security review's invariants below.

mock_provider "aws" {
  source = "../tests/mocks"
}

variables {
  allowed_account_ids = ["123456789012"]
}

run "applies" {
  command = apply

  assert {
    condition     = length(aws_ecr_repository.this) == 2
    error_message = "Bootstrap must create one ECR repository per service image."
  }

  # A tag that can be pushed twice can be made to mean a different image
  # after staging has tested it.
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_tag_mutability == "IMMUTABLE" && r.image_scanning_configuration[0].scan_on_push && r.encryption_configuration[0].encryption_type == "KMS" && r.encryption_configuration[0].kms_key == aws_kms_key.ecr.arn])
    error_message = "Every ECR repository must have immutable tags, scan on push, and encryption with the ECR key."
  }

  # Every bucket: ACLs off, all four public-access blocks, versioned, and
  # a policy that refuses plaintext transport.
  assert {
    condition = alltrue(flatten([
      [for o in [aws_s3_bucket_ownership_controls.state, aws_s3_bucket_ownership_controls.access_logs, aws_s3_bucket_ownership_controls.cloudtrail] : o.rule[0].object_ownership == "BucketOwnerEnforced"],
      [for b in [aws_s3_bucket_public_access_block.state, aws_s3_bucket_public_access_block.access_logs, aws_s3_bucket_public_access_block.cloudtrail] : b.block_public_acls && b.block_public_policy && b.ignore_public_acls && b.restrict_public_buckets],
      [for v in [aws_s3_bucket_versioning.state, aws_s3_bucket_versioning.access_logs, aws_s3_bucket_versioning.cloudtrail] : v.versioning_configuration[0].status == "Enabled"],
      [for d in [data.aws_iam_policy_document.state_bucket, data.aws_iam_policy_document.access_logs, data.aws_iam_policy_document.cloudtrail_bucket] : anytrue([for s in d.statement : s.effect == "Deny" && contains([for c in s.condition : c.variable], "aws:SecureTransport")])],
    ]))
    error_message = "Every bucket must have ACLs disabled, public access blocked, versioning on, and a deny on plaintext transport."
  }

  assert {
    condition = alltrue(flatten([
      [for r in aws_s3_bucket_server_side_encryption_configuration.state.rule : [for d in r.apply_server_side_encryption_by_default : d.sse_algorithm == "aws:kms" && d.kms_master_key_id == aws_kms_key.state.arn]],
      [for r in aws_s3_bucket_server_side_encryption_configuration.cloudtrail.rule : [for d in r.apply_server_side_encryption_by_default : d.sse_algorithm == "aws:kms" && d.kms_master_key_id == aws_kms_key.cloudtrail.arn]],
    ]))
    error_message = "State and the trail must each be encrypted with their own customer-managed key."
  }

  assert {
    condition     = aws_kms_key.state.enable_key_rotation && aws_kms_key.ecr.enable_key_rotation && aws_kms_key.cloudtrail.enable_key_rotation
    error_message = "Every key must rotate."
  }

  assert {
    condition     = aws_cloudtrail.this.is_multi_region_trail && aws_cloudtrail.this.enable_log_file_validation && aws_cloudtrail.this.include_global_service_events && aws_cloudtrail.this.enable_logging && aws_cloudtrail.this.kms_key_id == aws_kms_key.cloudtrail.arn
    error_message = "The trail must cover every region and global services, carry digests, and be encrypted with its key."
  }

  assert {
    condition     = aws_s3_bucket_logging.state.target_bucket == aws_s3_bucket.access_logs.id && aws_s3_bucket_logging.cloudtrail.target_bucket == aws_s3_bucket.access_logs.id
    error_message = "The state and trail buckets must both deliver access logs."
  }

  # The CI roles trust exact subjects: no wildcard that a branch name or a
  # fork could satisfy.
  assert {
    condition = alltrue(flatten([
      for d in [data.aws_iam_policy_document.plan_assume, data.aws_iam_policy_document.ecr_push_assume] : [
        for s in d.statement : alltrue([for c in s.condition : c.test == "StringEquals" && !anytrue([for v in c.values : strcontains(v, "*")])])
      ]
    ]))
    error_message = "CI role trust policies must match exact subjects, with StringEquals and no wildcard."
  }

  # The push role is trusted from main only; plan from main and pull
  # requests.
  assert {
    condition = alltrue(flatten([
      for s in data.aws_iam_policy_document.ecr_push_assume.statement : [
        for c in s.condition : endswith(c.variable, ":sub") ? toset(c.values) == toset(["repo:n0ah-n0wa/VitalMesh:ref:refs/heads/main"]) : true
      ]
    ]))
    error_message = "The image-push role must be assumable from main only."
  }

  # Plan may read configuration and state, never data.
  assert {
    condition = alltrue([
      anytrue([for s in data.aws_iam_policy_document.plan_state.statement : s.effect == "Deny" && contains(s.actions, "logs:GetLogEvents") && contains(s.actions, "secretsmanager:GetSecretValue")]),
      anytrue([for s in data.aws_iam_policy_document.plan_state.statement : s.effect == "Deny" && contains(s.actions, "s3:GetObject") && try(toset(s.not_resources), null) == toset(["${aws_s3_bucket.state.arn}/*"])]),
      anytrue([for s in data.aws_iam_policy_document.plan_state.statement : s.effect == "Deny" && contains(s.actions, "kms:Decrypt") && try(toset(s.not_resources), null) == toset([aws_kms_key.state.arn])]),
    ])
    error_message = "The plan role must be denied data-plane reads: log contents, secret values, objects outside the state bucket, decryption with any other key."
  }

  assert {
    condition = alltrue([
      for s in data.aws_iam_policy_document.plan_state.statement : s.effect == "Deny" || !contains(s.actions, "s3:PutObject") || try(toset(s.resources), null) == toset(["${aws_s3_bucket.state.arn}/*.tflock"])
    ])
    error_message = "The plan role may write nothing but lock files."
  }

  # The push role can put images and nothing else: no delete, no policy
  # change, no repository creation, and only on the named repositories.
  assert {
    condition = alltrue([
      for s in data.aws_iam_policy_document.ecr_push.statement :
      !anytrue([for a in s.actions : strcontains(a, "Delete") || strcontains(a, "Policy") || strcontains(a, "CreateRepository")]) &&
      (contains(s.resources, "*") ? toset(s.actions) == toset(["ecr:GetAuthorizationToken"]) : true)
    ])
    error_message = "The image-push role must not delete images or change repositories, and only GetAuthorizationToken may be unscoped."
  }
}
