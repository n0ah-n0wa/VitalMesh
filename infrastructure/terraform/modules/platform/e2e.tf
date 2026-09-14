# The account the end-to-end tests sign in with after a deployment
# (.github/workflows/release.yml, job e2e-staging). Created only where
# var.create_e2e_account is set: staging, whose data is synthetic (section
# 103). Production has no such account.
#
# The password follows the pattern of every other generated secret here:
# an ephemeral value written through a write-only argument, so it is in
# Secrets Manager and nowhere else, not in state and not in a plan. The
# deploy job reads it once to create the account inside the cluster (the
# gateway's own `users create`, which takes the password on standard
# input), and the E2E job reads it once to sign in. Rotating it is the same
# secret_version bump as the others; the account then needs recreating,
# which docs/DEPLOYMENT.md describes.

locals {
  e2e_email = "e2e-${var.environment}@vitalmesh.example"
}

ephemeral "random_password" "e2e" {
  # Well over the gateway's minimum of twelve; alphanumeric so that it
  # survives every shell and JSON boundary it crosses unchanged.
  length  = 40
  special = false
}

resource "aws_secretsmanager_secret" "e2e" {
  #checkov:skip=CKV2_AWS_57:Rotated through Terraform by increasing secret_version, like the other generated secrets; a rotation function would change the secret without changing the account.

  count = var.create_e2e_account ? 1 : 0

  name                    = "vitalmesh/${var.environment}/e2e"
  description             = "VitalMesh ${var.environment}: password of the end-to-end test account ${local.e2e_email}"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "e2e" {
  count = var.create_e2e_account ? 1 : 0

  secret_id                = aws_secretsmanager_secret.e2e[0].id
  secret_string_wo         = ephemeral.random_password.e2e.result
  secret_string_wo_version = var.secret_version
}
