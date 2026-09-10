# PostgreSQL on RDS: private, encrypted, backed up, and reachable only from
# the security groups the caller names.

data "aws_partition" "current" {}

locals {
  parameter_family = "postgres${split(".", var.engine_version)[0]}"
  log_types        = ["postgresql", "upgrade"]

  # The instance's own identifier. Everything else (security group, subnet
  # group, parameter group) keeps var.identifier; only the instance, its
  # log groups (which RDS names after the instance) and its alarms take the
  # suffix, so that a restore can stand up a second instance beside the
  # first without renaming anything that is not the instance.
  instance_identifier = "${var.identifier}${var.instance_suffix}"

  restoring = var.snapshot_identifier != null || var.restore_to_point_in_time != null
}

resource "aws_db_subnet_group" "this" {
  name        = var.identifier
  description = "Database-tier subnets for ${var.identifier}"
  subnet_ids  = var.subnet_ids

  tags = var.tags
}

resource "aws_security_group" "this" {
  name        = "${var.identifier}-postgres"
  description = "PostgreSQL for ${var.identifier}: port 5432 from named security groups only"
  vpc_id      = var.vpc_id

  # No egress rules. Terraform removes AWS's default allow-all egress rule
  # from a security group it creates, and a database never needs to open a
  # connection of its own.

  tags = merge(var.tags, { Name = "${var.identifier}-postgres" })
}

resource "aws_vpc_security_group_ingress_rule" "postgres" {
  for_each = var.allowed_security_group_ids

  security_group_id            = aws_security_group.this.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  description                  = "PostgreSQL from ${each.key}"

  tags = var.tags
}

# Parameters that differ from RDS's defaults, each with its reason. Static
# ones carry apply_method = pending-reboot: they take effect at creation,
# and a later change waits for the maintenance window (or a deliberate
# reboot) rather than restarting the database on apply.
resource "aws_db_parameter_group" "this" {
  name_prefix = "${var.identifier}-"
  family      = local.parameter_family
  description = "PostgreSQL parameters for ${var.identifier}"

  # TLS for every connection, enforced by the server rather than hoped for
  # from the client. The gateway already refuses a non-TLS DATABASE_URL in
  # staging and production; this makes any client that does not refuse fail
  # as well.
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  # TLS 1.2 at least. PostgreSQL 16 defaults to this; written down so that
  # a future default cannot lower it.
  parameter {
    name  = "ssl_min_protocol_version"
    value = "TLSv1.2"
  }

  # Only members of rds_password may change passwords: the application's
  # future database role cannot reset the master user's, or its own, over
  # the wire.
  parameter {
    name         = "rds.restrict_password_commands"
    value        = "1"
    apply_method = "pending-reboot"
  }

  # Per-statement statistics for the slow-query investigations Performance
  # Insights points at. Static: loaded at start.
  parameter {
    name         = "shared_preload_libraries"
    value        = "pg_stat_statements"
    apply_method = "pending-reboot"
  }

  # Statements slower than a second are logged, so a slow query shows up in
  # CloudWatch rather than only as latency on a dashboard.
  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }

  # ... but never with their bound parameters. The gateway sends every value
  # as a parameter, and PostgreSQL would otherwise log those with the slow
  # statement: a slow login lookup would put an email address in CloudWatch,
  # and a slow credential check the password hash beside it (section 30:
  # passwords, hashes and emails are never logged). The second setting is
  # the same rule for statements that fail.
  parameter {
    name  = "log_parameter_max_length"
    value = "0"
  }

  parameter {
    name  = "log_parameter_max_length_on_error"
    value = "0"
  }

  parameter {
    name  = "log_connections"
    value = "1"
  }

  parameter {
    name  = "log_disconnections"
    value = "1"
  }

  # A lock held longer than deadlock_timeout (one second) is logged with
  # who holds it: the first thing to look at when the ingestion path stalls.
  parameter {
    name  = "log_lock_waits"
    value = "1"
  }

  # A session that opened a transaction and went quiet is holding locks and
  # an xid for nothing. The gateway's pool never does this on purpose; a
  # minute is long enough for anything it does by accident, and short
  # enough that a leaked connection cannot block a migration or vacuum for
  # the rest of the day.
  parameter {
    name  = "idle_in_transaction_session_timeout"
    value = "60000"
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

# Created before the instance so that exported logs land in groups that have
# a retention period and a key. RDS would otherwise create them itself with
# neither. RDS names the groups after the instance, so they follow the
# instance identifier; skip_destroy keeps the old instance's logs when a
# restore replaces it (their retention expires them in due course).
resource "aws_cloudwatch_log_group" "this" {
  for_each = toset(local.log_types)

  name              = "/aws/rds/instance/${local.instance_identifier}/${each.key}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn
  skip_destroy      = true

  tags = var.tags
}

data "aws_iam_policy_document" "monitoring_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["monitoring.rds.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "monitoring" {
  count = var.monitoring_interval > 0 ? 1 : 0

  name               = "${var.identifier}-rds-monitoring"
  description        = "Enhanced Monitoring for ${var.identifier}"
  assume_role_policy = data.aws_iam_policy_document.monitoring_assume.json

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "monitoring" {
  count = var.monitoring_interval > 0 ? 1 : 0

  role       = aws_iam_role.monitoring[0].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

# Both scanners read this module with staging's values, where several of the
# settings below are off on purpose (section 103). The platform module holds
# production to a floor on the ones that protect data.
#trivy:ignore:AVD-AWS-0177
#trivy:ignore:AVD-AWS-0133
resource "aws_db_instance" "this" {
  #checkov:skip=CKV_AWS_293:Set per environment. Production turns deletion protection on, and the platform module requires a final snapshot there regardless.
  #checkov:skip=CKV_AWS_157:Set per environment. The platform module requires Multi-AZ in production.
  #checkov:skip=CKV_AWS_118:Set per environment. Production samples every 60 seconds.
  #checkov:skip=CKV_AWS_353:Set per environment. Production turns Performance Insights on.
  #checkov:skip=CKV_AWS_354:Performance Insights is encrypted with kms_key_arn whenever it is on. Checkov does not evaluate the conditional.

  identifier     = local.instance_identifier
  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.instance_class

  # A restore. Either names a source (a snapshot, or an instance and a
  # moment in its backup window) and creates this instance from it instead
  # of empty. db_name is taken from the source then; RDS rejects it here.
  # docs/DISASTER_RECOVERY.md is the procedure.
  snapshot_identifier = var.snapshot_identifier

  dynamic "restore_to_point_in_time" {
    for_each = var.restore_to_point_in_time == null ? [] : [var.restore_to_point_in_time]

    content {
      source_db_instance_identifier = restore_to_point_in_time.value.source_db_instance_identifier
      restore_time                  = restore_to_point_in_time.value.restore_time
      use_latest_restorable_time    = restore_to_point_in_time.value.use_latest_restorable_time
    }
  }

  db_name  = local.restoring ? null : var.database_name
  username = local.restoring ? null : var.master_username

  # The master password is generated by RDS and kept in Secrets Manager,
  # encrypted with the environment's key. It never appears in this
  # configuration, in a plan or in Terraform state, and RDS can rotate it.
  manage_master_user_password   = true
  master_user_secret_kms_key_id = var.kms_key_arn

  # IAM authentication costs nothing to enable and is the route to an
  # application role with no static password at all. The services use
  # password authentication today; enabling it now avoids changing the
  # instance later.
  iam_database_authentication_enabled = true

  allocated_storage     = var.allocated_storage
  max_allocated_storage = var.max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_arn

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.this.id]
  publicly_accessible    = false
  parameter_group_name   = aws_db_parameter_group.this.name
  ca_cert_identifier     = "rds-ca-rsa2048-g1"

  multi_az = var.multi_az

  # Automated backups, and with them point-in-time recovery to any second
  # inside the window (section 80).
  backup_retention_period  = var.backup_retention_days
  backup_window            = "02:00-03:00"
  maintenance_window       = "sun:03:30-sun:04:30"
  copy_tags_to_snapshot    = true
  delete_automated_backups = false

  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = var.skip_final_snapshot ? null : "${local.instance_identifier}-final"

  auto_minor_version_upgrade = true
  apply_immediately          = false

  enabled_cloudwatch_logs_exports = local.log_types

  performance_insights_enabled          = var.performance_insights_enabled
  performance_insights_kms_key_id       = var.performance_insights_enabled ? var.kms_key_arn : null
  performance_insights_retention_period = var.performance_insights_enabled ? 7 : null

  monitoring_interval = var.monitoring_interval
  monitoring_role_arn = var.monitoring_interval > 0 ? aws_iam_role.monitoring[0].arn : null

  tags = var.tags

  depends_on = [aws_cloudwatch_log_group.this]
}

# ------------------------------------------------------------------ events

# What RDS itself has to say: a failover, a reboot, a maintenance action, a
# backup that failed, storage running out. Alarms watch metrics; this is
# the other half of monitoring, and it goes to the same topic.
resource "aws_db_event_subscription" "this" {
  count = var.enable_alarms ? 1 : 0

  name      = "${local.instance_identifier}-events"
  sns_topic = var.alarm_topic_arn

  source_type = "db-instance"
  source_ids  = [aws_db_instance.this.identifier]

  event_categories = [
    "availability",
    "backup",
    "configuration change",
    "deletion",
    "failover",
    "failure",
    "low storage",
    "maintenance",
    "notification",
    "recovery",
  ]

  tags = var.tags
}

# ------------------------------------------------------------------ alarms

resource "aws_cloudwatch_metric_alarm" "cpu" {
  count = var.enable_alarms ? 1 : 0

  alarm_name          = "${local.instance_identifier}-cpu-high"
  alarm_description   = "PostgreSQL CPU above 80 percent for ten minutes."
  namespace           = "AWS/RDS"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 2
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.this.identifier }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}

# Storage autoscaling grows the volume when free space falls to about ten
# percent. This fires at five, which means autoscaling has not kept up or has
# reached max_allocated_storage. Either way a person is needed.
resource "aws_cloudwatch_metric_alarm" "storage" {
  count = var.enable_alarms ? 1 : 0

  alarm_name          = "${local.instance_identifier}-storage-low"
  alarm_description   = "PostgreSQL free storage below five percent of the initial allocation."
  namespace           = "AWS/RDS"
  metric_name         = "FreeStorageSpace"
  statistic           = "Minimum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.allocated_storage * 1073741824 * 0.05
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.this.identifier }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}

# PostgreSQL's connection ceiling follows the instance class (RDS sets
# max_connections from memory), and the gateway's pool times out rather
# than queues when it is reached. The threshold is the caller's, since only
# it knows the class; zero turns the alarm off.
resource "aws_cloudwatch_metric_alarm" "connections" {
  count = var.enable_alarms && var.connections_alarm_threshold > 0 ? 1 : 0

  alarm_name          = "${local.instance_identifier}-connections-high"
  alarm_description   = "PostgreSQL connections above ${var.connections_alarm_threshold} for five minutes: near the instance class's ceiling."
  namespace           = "AWS/RDS"
  metric_name         = "DatabaseConnections"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.connections_alarm_threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.this.identifier }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}

# Memory the engine has left for its buffers. Below this, PostgreSQL is
# reading from disk what it used to have in cache, and swapping is next.
resource "aws_cloudwatch_metric_alarm" "memory" {
  count = var.enable_alarms && var.freeable_memory_alarm_bytes > 0 ? 1 : 0

  alarm_name          = "${local.instance_identifier}-memory-low"
  alarm_description   = "PostgreSQL freeable memory below ${floor(var.freeable_memory_alarm_bytes / 1048576)} MiB for ten minutes."
  namespace           = "AWS/RDS"
  metric_name         = "FreeableMemory"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 2
  threshold           = var.freeable_memory_alarm_bytes
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.this.identifier }
  alarm_actions       = [var.alarm_topic_arn]
  ok_actions          = [var.alarm_topic_arn]

  tags = var.tags
}
