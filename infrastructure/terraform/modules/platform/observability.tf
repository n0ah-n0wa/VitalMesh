# Where alarms go. Every alarm in this environment (RDS CPU and storage,
# Redis engine CPU and memory) notifies this one topic.
#
# Log groups are created next to the resources that write to them, in each
# module, so each gets a retention period and the logs key before anything
# is written: VPC flow logs, the EKS control plane, Container Insights when
# it is enabled, PostgreSQL, and Redis.

resource "aws_sns_topic" "alarms" {
  name              = "${local.name}-alarms"
  display_name      = "VitalMesh ${var.environment} alarms"
  kms_master_key_id = aws_kms_key.logs.arn

  tags = local.tags
}

# An email subscription stays pending until its link is clicked, and AWS
# delivers nothing to it until then. That is AWS's own safeguard against
# subscribing someone else's address, and it means an apply alone does not
# prove alarms reach anyone.
resource "aws_sns_topic_subscription" "email" {
  count = var.alarm_email == "" ? 0 : 1

  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = var.alarm_email
}
