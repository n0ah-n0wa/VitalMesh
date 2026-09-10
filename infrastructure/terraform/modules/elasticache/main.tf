# Redis on ElastiCache: private, encrypted at rest and in transit, with an
# AUTH token that never passes through Terraform state.
#
# Nothing here is a record of truth. Redis holds rate-limit counters, a
# short-lived cache and idempotency locks (SPECIFICATIONS.md section 23), all
# of which the gateway survives losing. That is why there are no backups
# below, and why the service keeps working when this is unreachable.

locals {
  replicated       = var.num_cache_clusters > 1
  parameter_family = "redis${split(".", var.engine_version)[0]}"
  log_types        = ["slow-log", "engine-log"]

  # ElastiCache names a replication group's members <id>-001, <id>-002 and
  # so on. Building the names here, rather than reading member_clusters,
  # keeps them known at plan time so the alarms can be keyed on them.
  member_ids = [for i in range(var.num_cache_clusters) : format("%s-%03d", var.name, i + 1)]
}

resource "aws_elasticache_subnet_group" "this" {
  name        = var.name
  description = "Database-tier subnets for ${var.name}"
  subnet_ids  = var.subnet_ids

  tags = var.tags
}

resource "aws_security_group" "this" {
  name        = "${var.name}-redis"
  description = "Redis for ${var.name}: port 6379 from named security groups only"
  vpc_id      = var.vpc_id

  # No egress rules, as with the database: a cache opens no connections.

  tags = merge(var.tags, { Name = "${var.name}-redis" })
}

resource "aws_vpc_security_group_ingress_rule" "redis" {
  for_each = var.allowed_security_group_ids

  security_group_id            = aws_security_group.this.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
  description                  = "Redis from ${each.key}"

  tags = var.tags
}

resource "aws_cloudwatch_log_group" "this" {
  for_each = toset(local.log_types)

  name              = "/aws/elasticache/${var.name}/${each.key}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = var.tags
}

# The AUTH token. An ephemeral resource exists only for the length of one
# plan or apply and is never written to state; it reaches ElastiCache and
# Secrets Manager through write-only arguments, which are sent to AWS and
# not recorded either. So the token is in exactly two places, both
# encrypted: the cache itself and the secret the deploy role reads.
#
# Alphanumeric only. The gateway reads it as part of a rediss:// URL, where
# several of the symbols ElastiCache would accept need escaping and are an
# easy way to produce a URL that looks right and fails to authenticate.
ephemeral "random_password" "auth_token" {
  length  = 64
  special = false
}

resource "aws_elasticache_replication_group" "this" {
  #checkov:skip=CKV_AWS_31:The AUTH token is set, through auth_token_wo, which checkov does not read. Transit encryption is required below.
  #checkov:skip=CKV2_AWS_50:Failover and Multi-AZ follow num_cache_clusters. Staging runs one node by choice and the platform module requires a replica in production.

  replication_group_id = var.name
  description          = "Redis for ${var.name}: rate limits, cache and idempotency locks"
  engine               = "redis"
  engine_version       = var.engine_version
  node_type            = var.node_type
  num_cache_clusters   = var.num_cache_clusters
  port                 = 6379
  parameter_group_name = "default.${local.parameter_family}"

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = [aws_security_group.this.id]

  automatic_failover_enabled = local.replicated
  multi_az_enabled           = local.replicated

  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn

  # TLS required, not merely offered. The gateway already refuses a
  # plaintext redis:// URL in staging and production; "required" makes the
  # server refuse one as well.
  transit_encryption_enabled = true
  transit_encryption_mode    = "required"

  auth_token_wo         = ephemeral.random_password.auth_token.result
  auth_token_wo_version = var.auth_token_version
  # ROTATE keeps the old token valid alongside the new one after a change,
  # so pods holding the old one keep working until they are rolled.
  auth_token_update_strategy = "ROTATE"

  snapshot_retention_limit   = 0
  auto_minor_version_upgrade = true
  apply_immediately          = false
  maintenance_window         = "sun:04:30-sun:05:30"

  dynamic "log_delivery_configuration" {
    for_each = local.log_types

    content {
      destination      = aws_cloudwatch_log_group.this[log_delivery_configuration.value].name
      destination_type = "cloudwatch-logs"
      log_format       = "json"
      log_type         = log_delivery_configuration.value
    }
  }

  tags = var.tags
}

resource "aws_secretsmanager_secret" "auth" {
  #checkov:skip=CKV2_AWS_57:Rotated through Terraform by increasing secret_version, which changes the cache's token and this secret in the same apply. A rotation function would change only this secret.

  name                    = var.auth_secret_name
  description             = "Redis AUTH token for ${var.name}"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = var.tags
}

# The same ephemeral value as the replication group's, in the same run, and
# tied to the same version number: they are only ever written together.
resource "aws_secretsmanager_secret_version" "auth" {
  secret_id                = aws_secretsmanager_secret.auth.id
  secret_string_wo         = ephemeral.random_password.auth_token.result
  secret_string_wo_version = var.auth_token_version
}

# ------------------------------------------------------------------ alarms

# Redis runs commands on a single thread, so engine CPU rather than host CPU
# is the ceiling that matters.
resource "aws_cloudwatch_metric_alarm" "engine_cpu" {
  for_each = var.enable_alarms ? toset(local.member_ids) : toset([])

  alarm_name          = "${each.key}-engine-cpu-high"
  alarm_description   = "Redis engine CPU above 80 percent for ten minutes."
  namespace           = "AWS/ElastiCache"
  metric_name         = "EngineCPUUtilization"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 2
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { CacheClusterId = each.key }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}

# Above this, eviction starts removing keys, and the keys with the shortest
# remaining life go first: the idempotency locks among them.
resource "aws_cloudwatch_metric_alarm" "memory" {
  for_each = var.enable_alarms ? toset(local.member_ids) : toset([])

  alarm_name          = "${each.key}-memory-high"
  alarm_description   = "Redis memory above 80 percent of what is available to it."
  namespace           = "AWS/ElastiCache"
  metric_name         = "DatabaseMemoryUsagePercentage"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 2
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { CacheClusterId = each.key }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}

# Eviction is the memory alarm's failure mode: with no room, Redis removes
# keys, and the keys with the shortest remaining life go first, which are
# the idempotency locks. A single eviction is a correctness question, not a
# capacity one.
resource "aws_cloudwatch_metric_alarm" "evictions" {
  for_each = var.enable_alarms ? toset(local.member_ids) : toset([])

  alarm_name          = "${each.key}-evictions"
  alarm_description   = "Redis evicted keys in the last five minutes: it is out of memory and idempotency locks may be gone."
  namespace           = "AWS/ElastiCache"
  metric_name         = "Evictions"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { CacheClusterId = each.key }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}

# A replica that falls far behind is a failover that would lose recent
# writes: rate-limit counters at worst, an idempotency lock at best.
resource "aws_cloudwatch_metric_alarm" "replication_lag" {
  for_each = var.enable_alarms && local.replicated ? toset(slice(local.member_ids, 1, length(local.member_ids))) : toset([])

  alarm_name          = "${each.key}-replication-lag"
  alarm_description   = "Redis replica more than thirty seconds behind the primary."
  namespace           = "AWS/ElastiCache"
  metric_name         = "ReplicationLag"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 30
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { CacheClusterId = each.key }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}
