# The application's own secrets, generated here and never stored in state.
#
# Each value comes from an ephemeral resource, which exists only for the
# length of one plan or apply, and reaches Secrets Manager through a
# write-only argument, which is sent to AWS and not recorded. So there is no
# point at which the values are in Terraform state, in a plan file, or in
# this repository: Secrets Manager is the only place they exist.
#
# Only an increase in secret_version makes Terraform write new values; a
# plain apply leaves the existing secret alone. Rotating is therefore a
# reviewed one-line change in the environment root, and it rotates the Redis
# AUTH token (in the elasticache module) at the same time.
#
# What is not here: DATABASE_URL and REDIS_URL. Both are assembled by the
# deployment pipeline from pieces that already exist elsewhere: the RDS
# master secret (created and rotated by RDS), the Redis AUTH secret, and the
# endpoints in this module's outputs. Storing the assembled URLs as well
# would be a second copy of each credential to keep in step.

ephemeral "random_password" "jwt_secret" {
  # The gateway refuses a JWT_SECRET under 32 bytes; this is twice that.
  length  = 64
  special = false
}

ephemeral "random_password" "processor_token" {
  # Shared by the gateway (PROCESSOR_TOKEN) and the processor
  # (INTERNAL_TOKEN). The processor requires at least 16 characters.
  length  = 48
  special = false
}

resource "aws_secretsmanager_secret" "app" {
  #checkov:skip=CKV2_AWS_57:Rotated through Terraform by increasing secret_version. Pods read each value once at start and accept only one, so rotating needs a coordinated rollout, which is a deployment and not a timer.

  name                    = "vitalmesh/${var.environment}/app"
  description             = "VitalMesh ${var.environment}: JWT signing key and the gateway-to-processor token"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "app" {
  secret_id = aws_secretsmanager_secret.app.id

  # One JSON document with both values. PROCESSOR_TOKEN is written once and
  # the deployment pipeline gives it to the gateway as PROCESSOR_TOKEN and to
  # the processor as INTERNAL_TOKEN, which is what keeps the two equal.
  secret_string_wo = jsonencode({
    JWT_SECRET      = ephemeral.random_password.jwt_secret.result
    PROCESSOR_TOKEN = ephemeral.random_password.processor_token.result
  })
  secret_string_wo_version = var.secret_version
}
