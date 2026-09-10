# One VitalMesh environment, composed from the building blocks.
#
# Both environment roots call this module and nothing else, so everything
# that must be identical between staging and production is written once,
# here, and each root states only what differs (section 57: "without
# duplicating large amounts of configuration").

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_region" "current" {}

locals {
  name = "vitalmesh-${var.environment}"

  # The Kubernetes namespace the matching overlay deploys into
  # (infrastructure/kubernetes/overlays/<environment>), and so the only one
  # the deploy role is given.
  namespace = "vitalmesh-${var.environment}"

  tags = var.tags
}

module "network" {
  source = "../network"

  name                    = local.name
  cidr_block              = var.vpc_cidr
  az_count                = var.az_count
  single_nat_gateway      = var.single_nat_gateway
  enable_flow_logs        = var.enable_flow_logs
  flow_log_retention_days = var.log_retention_days
  enable_ecr_endpoints    = var.enable_ecr_endpoints
  log_kms_key_arn         = aws_kms_key.logs.arn

  tags = local.tags
}

module "eks" {
  source = "../eks"

  name                         = local.name
  kubernetes_version           = var.kubernetes_version
  subnet_ids                   = module.network.private_subnet_ids
  endpoint_public_access       = var.cluster_endpoint_public_access
  endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs
  secrets_kms_key_arn          = aws_kms_key.data.arn
  log_kms_key_arn              = aws_kms_key.logs.arn
  log_retention_days           = var.log_retention_days

  node_instance_types = var.node_instance_types
  node_min_size       = var.node_min_size
  node_desired_size   = var.node_desired_size
  node_max_size       = var.node_max_size

  admin_principal_arns = var.cluster_admin_principal_arns
  deployer = {
    principal_arn = aws_iam_role.deploy.arn
    namespaces    = [local.namespace]
  }

  enable_cloudwatch_observability = var.enable_container_insights

  enable_load_balancer_controller = true
  enable_cluster_autoscaler       = true
  node_auto_repair                = true
  enable_zonal_shift              = var.enable_zonal_shift
  deletion_protection             = var.cluster_deletion_protection

  tags = local.tags
}

module "rds" {
  source = "../rds"

  identifier = local.name
  vpc_id     = module.network.vpc_id
  subnet_ids = module.network.database_subnet_ids

  # The cluster's security group and nothing else. Every pod sends from a
  # node's network interface, so this admits the application and refuses
  # everything else in the VPC, including the NAT and load balancer tiers.
  allowed_security_group_ids = { eks_cluster = module.eks.cluster_security_group_id }

  instance_class               = var.db_instance_class
  allocated_storage            = var.db_allocated_storage
  max_allocated_storage        = var.db_max_allocated_storage
  multi_az                     = var.db_multi_az
  backup_retention_days        = var.db_backup_retention_days
  deletion_protection          = var.db_deletion_protection
  skip_final_snapshot          = var.db_skip_final_snapshot
  monitoring_interval          = var.db_monitoring_interval
  performance_insights_enabled = var.db_performance_insights_enabled
  connections_alarm_threshold  = var.db_connections_alarm_threshold
  freeable_memory_alarm_bytes  = var.db_freeable_memory_alarm_mib * 1048576

  # A restore, normally all null: docs/DISASTER_RECOVERY.md.
  instance_suffix          = var.db_instance_suffix
  snapshot_identifier      = var.db_restore_snapshot_identifier
  restore_to_point_in_time = var.db_restore_to_point_in_time

  kms_key_arn        = aws_kms_key.data.arn
  log_kms_key_arn    = aws_kms_key.logs.arn
  log_retention_days = var.log_retention_days

  enable_alarms   = true
  alarm_topic_arn = aws_sns_topic.alarms.arn

  tags = local.tags
}

module "elasticache" {
  source = "../elasticache"

  name       = local.name
  vpc_id     = module.network.vpc_id
  subnet_ids = module.network.database_subnet_ids

  allowed_security_group_ids = { eks_cluster = module.eks.cluster_security_group_id }

  node_type          = var.redis_node_type
  num_cache_clusters = var.redis_num_cache_clusters

  kms_key_arn        = aws_kms_key.data.arn
  log_kms_key_arn    = aws_kms_key.logs.arn
  log_retention_days = var.log_retention_days

  auth_secret_name            = "vitalmesh/${var.environment}/redis"
  secret_recovery_window_days = var.secret_recovery_window_days
  auth_token_version          = var.secret_version

  enable_alarms   = true
  alarm_topic_arn = aws_sns_topic.alarms.arn

  tags = local.tags
}
