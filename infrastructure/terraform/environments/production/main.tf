# Production (section 104): private managed database and cache, HTTPS,
# restricted network access, separate secrets. Everything below is a value;
# the shape itself is in modules/platform, shared with staging.

provider "aws" {
  region              = "eu-central-1"
  allowed_account_ids = var.allowed_account_ids

  default_tags {
    tags = {
      Project     = "vitalmesh"
      Environment = "production"
      ManagedBy   = "terraform"
      Repository  = "github.com/n0ah-n0wa/VitalMesh"
    }
  }
}

module "platform" {
  source = "../../modules/platform"

  environment       = "production"
  github_repository = "n0ah-n0wa/VitalMesh"

  # Three zones, a NAT gateway in each: losing one zone loses a third of the
  # capacity and none of the egress. 10.30/16 keeps it clear of staging's
  # 10.20/16, so the two could be peered without renumbering either.
  vpc_cidr           = "10.30.0.0/16"
  az_count           = 3
  single_nat_gateway = false
  enable_flow_logs   = true

  # Public, but only to the listed addresses: nothing inside the VPC can run
  # kubectl yet. See the README before changing either.
  kubernetes_version                   = "1.34"
  cluster_endpoint_public_access       = true
  cluster_endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs
  cluster_admin_principal_arns         = var.cluster_admin_principal_arns

  # Sized against the production overlay's HPA ceilings: twenty gateways at
  # 500m and twelve processors at a full core is 22 vCPU and about 16 GiB at
  # full scale, roughly six m6i.xlarge once system pods are counted, which
  # max_size 8 allows. The Cluster Autoscaler (installed from
  # infrastructure/kubernetes/platform) grows the group from the three
  # desired nodes, one per zone, towards that ceiling as pods go Pending.
  node_instance_types       = ["m6i.xlarge"]
  node_min_size             = 3
  node_desired_size         = 3
  node_max_size             = 8
  enable_container_insights = true

  # ECR endpoints keep every image pull inside AWS; zonal shift lets an
  # impaired zone be evacuated with one call; and the cluster, like the
  # database, refuses to be deleted until that is turned off in its own
  # reviewed change.
  enable_ecr_endpoints        = true
  enable_zonal_shift          = true
  cluster_deletion_protection = true

  # The hostname the overlay's Ingress serves. Both come from tfvars because
  # the domain is not decided yet (OQ-18); the platform module refuses a
  # production without one.
  ingress_domain_name = var.ingress_domain_name
  route53_zone_id     = var.route53_zone_id

  # Multi-AZ with automatic failover, fourteen days of point-in-time
  # recovery, a final snapshot on delete, and deletion protection that has to
  # be switched off in its own reviewed change before a destroy can succeed.
  db_instance_class        = "db.m6g.large"
  db_allocated_storage     = 100
  db_max_allocated_storage = 500
  db_multi_az              = true
  db_backup_retention_days = 14
  db_deletion_protection   = true
  db_skip_final_snapshot   = false
  db_monitoring_interval   = 60
  # db.m6g.large has 8 GiB: RDS allows about 900 connections; alarm at
  # 700, and below 800 MiB (a tenth) of freeable memory.
  db_connections_alarm_threshold = 700
  db_freeable_memory_alarm_mib   = 800

  # A restore, normally all null: docs/DISASTER_RECOVERY.md.
  db_instance_suffix             = var.db_instance_suffix
  db_restore_snapshot_identifier = var.db_restore_snapshot_identifier
  db_restore_to_point_in_time    = var.db_restore_to_point_in_time

  db_performance_insights_enabled = true

  # A primary and a replica in another zone, with automatic failover.
  redis_node_type          = "cache.m6g.large"
  redis_num_cache_clusters = 2

  # A year. The EKS audit log, the database connection log and the flow
  # logs are the audit trail (section 30), and the platform module accepts
  # no less in production. Retention is billed as storage, which is cheap;
  # ingestion is what costs, and it does not depend on retention.
  log_retention_days = 365
  alarm_email        = var.alarm_email

  # A deleted secret stays recoverable for thirty days.
  secret_recovery_window_days = 30
}
