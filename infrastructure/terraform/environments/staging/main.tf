# Staging: the same shape as production at the smallest size that still
# exercises it (section 103). Everything below is a value; the shape itself is
# in modules/platform, shared with production.

provider "aws" {
  region              = "eu-central-1"
  allowed_account_ids = var.allowed_account_ids

  default_tags {
    tags = {
      Project     = "vitalmesh"
      Environment = "staging"
      ManagedBy   = "terraform"
      Repository  = "github.com/n0ah-n0wa/VitalMesh"
    }
  }
}

module "platform" {
  source = "../../modules/platform"

  environment       = "staging"
  github_repository = "n0ah-n0wa/VitalMesh"

  # Two zones and one NAT gateway: the cheapest layout EKS and RDS accept.
  # A zone failure takes staging's egress with it, which staging can bear.
  vpc_cidr           = "10.20.0.0/16"
  az_count           = 2
  single_nat_gateway = true
  enable_flow_logs   = true

  # Public, but only to the listed addresses: nothing inside the VPC can run
  # kubectl yet. See the README before changing either.
  kubernetes_version                   = "1.34"
  cluster_endpoint_public_access       = true
  cluster_endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs
  cluster_admin_principal_arns         = var.cluster_admin_principal_arns

  # The staging overlay's HPA ceilings are three gateways at 100m and three
  # processors at 100m: well inside two t3.medium beside the system pods.
  node_instance_types       = ["t3.medium"]
  node_min_size             = 1
  node_desired_size         = 2
  node_max_size             = 3
  enable_container_insights = false

  # No ECR endpoints (about USD 14 a month for two zones, against a few
  # small pulls a day), no zonal shift (one NAT gateway means a zone failure
  # takes staging's egress anyway), no deletion protection: staging is
  # meant to be destroyable in one command.
  enable_ecr_endpoints        = false
  enable_zonal_shift          = false
  cluster_deletion_protection = false

  # The hostname the overlay's Ingress serves. Both come from tfvars because
  # the domain is not decided yet (OQ-18); the platform module refuses a
  # production without one.
  ingress_domain_name = var.ingress_domain_name
  route53_zone_id     = var.route53_zone_id

  # Single-AZ, no deletion protection and no final snapshot: staging holds
  # synthetic data only (section 103), so being able to destroy and rebuild
  # it cleanly matters more than keeping it. Point-in-time recovery still
  # runs, for three days, so a restore can be rehearsed here (OQ-32).
  db_instance_class        = "db.t4g.micro"
  db_allocated_storage     = 20
  db_max_allocated_storage = 50
  db_multi_az              = false
  db_backup_retention_days = 3
  db_deletion_protection   = false
  db_skip_final_snapshot   = true
  db_monitoring_interval   = 0
  # db.t4g.micro has 1 GiB: RDS allows about 110 connections; alarm at 90,
  # and below 100 MiB of freeable memory.
  db_connections_alarm_threshold = 90
  db_freeable_memory_alarm_mib   = 100

  # A restore, normally all null: docs/DISASTER_RECOVERY.md.
  db_instance_suffix             = var.db_instance_suffix
  db_restore_snapshot_identifier = var.db_restore_snapshot_identifier
  db_restore_to_point_in_time    = var.db_restore_to_point_in_time

  db_performance_insights_enabled = false

  redis_node_type          = "cache.t4g.micro"
  redis_num_cache_clusters = 1

  log_retention_days = 14
  alarm_email        = var.alarm_email

  # Deleted secrets go immediately, so a destroyed staging can be recreated
  # under the same secret names the next day.
  secret_recovery_window_days = 0
}
