# Everything that may differ between environments, and nothing else. The
# two environment roots set these; the module decides everything that must
# be the same in both.
#
# Staging turns several protections off on purpose (section 103: the
# smallest size that still exercises the shape), and the scanners are told
# so beside each resource. The validations marked "production floor" are
# what stop production doing the same: they fail any plan, and
# environments/production/tests/floors.tftest.hcl breaks each one in turn
# under make tf-validate, so a floor that stops refusing is noticed.

variable "environment" {
  description = "Environment name. Names every resource and the Kubernetes namespace the deploy role may manage."
  type        = string

  validation {
    condition     = contains(["staging", "production"], var.environment)
    error_message = "environment must be staging or production."
  }
}

variable "github_repository" {
  description = "owner/name of the repository whose GitHub Actions may assume this environment's deploy role."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must look like owner/name."
  }
}

# ----------------------------------------------------------------- network

variable "vpc_cidr" {
  description = "IPv4 /16 for this environment's VPC. Keep environments on distinct ranges so they can be peered later without renumbering."
  type        = string
}

variable "az_count" {
  description = "Availability zones to spread across: 2 or 3."
  type        = number
}

variable "single_nat_gateway" {
  description = "One NAT gateway for the whole VPC instead of one per zone."
  type        = bool
}

variable "enable_flow_logs" {
  description = "Record VPC flow logs."
  type        = bool
  default     = true

  # Production floor: flow logs are part of the audit trail.
  validation {
    condition     = var.environment != "production" || var.enable_flow_logs
    error_message = "production must record VPC flow logs."
  }
}

# --------------------------------------------------------------------- EKS

variable "kubernetes_version" {
  description = "EKS Kubernetes minor version, such as 1.34."
  type        = string
}

variable "cluster_endpoint_public_access" {
  description = "Serve the Kubernetes API on a public endpoint as well as the private one, reachable only from cluster_endpoint_public_access_cidrs. Needed until kubectl and the deployment runners can reach the VPC."
  type        = bool
}

variable "cluster_endpoint_public_access_cidrs" {
  description = "Addresses allowed to reach the Kubernetes API endpoint."
  type        = list(string)
}

variable "cluster_admin_principal_arns" {
  description = "IAM roles or users given cluster-admin."
  type        = list(string)
}

variable "node_instance_types" {
  description = "Instance types for the node group. x86_64, because the images are amd64."
  type        = list(string)
}

variable "node_min_size" {
  description = "Fewest nodes."
  type        = number
}

variable "node_desired_size" {
  description = "Nodes to run."
  type        = number
}

variable "node_max_size" {
  description = "Most nodes."
  type        = number
}

variable "enable_container_insights" {
  description = "Install the CloudWatch Observability add-on for Container Insights. Charged per metric and per GB of logs."
  type        = bool
  default     = false
}

# -------------------------------------------------------------- PostgreSQL

variable "db_instance_class" {
  description = "RDS instance class."
  type        = string
}

variable "db_allocated_storage" {
  description = "Initial database storage, in GiB."
  type        = number
}

variable "db_max_allocated_storage" {
  description = "Storage autoscaling ceiling, in GiB."
  type        = number
}

variable "db_multi_az" {
  description = "Run a synchronous standby in a second zone."
  type        = bool

  # Production floor.
  validation {
    condition     = var.environment != "production" || var.db_multi_az
    error_message = "production must run the database Multi-AZ."
  }
}

variable "db_backup_retention_days" {
  description = "Days of automated backups and of point-in-time recovery."
  type        = number

  # Production floor.
  validation {
    condition     = var.environment != "production" || var.db_backup_retention_days >= 7
    error_message = "production must keep at least seven days of backups."
  }
}

variable "db_deletion_protection" {
  description = "Refuse to delete the database until this is turned off first. Turning it off is the first step of a deliberate teardown, so it has no production floor; db_skip_final_snapshot has one instead."
  type        = bool
}

variable "db_skip_final_snapshot" {
  description = "Delete the database without a final snapshot."
  type        = bool

  # Production floor: however production's database comes to be deleted, it
  # leaves a snapshot behind.
  validation {
    condition     = var.environment != "production" || !var.db_skip_final_snapshot
    error_message = "production must take a final snapshot when the database is deleted."
  }
}

variable "db_monitoring_interval" {
  description = "Enhanced Monitoring interval in seconds; 0 disables it."
  type        = number
  default     = 0
}

variable "db_performance_insights_enabled" {
  description = "Turn on Performance Insights (free seven-day tier)."
  type        = bool
  default     = false
}

variable "db_connections_alarm_threshold" {
  description = "Alarm when connections exceed this; a little under the instance class's max_connections. Zero for no alarm."
  type        = number
  default     = 0
}

variable "db_freeable_memory_alarm_mib" {
  description = "Alarm when the database's freeable memory falls below this many MiB; about a tenth of the instance class's memory. Zero for no alarm."
  type        = number
  default     = 0
}

variable "db_instance_suffix" {
  description = "Suffix for the database instance identifier, used only during a restore (docs/DISASTER_RECOVERY.md)."
  type        = string
  default     = ""
}

variable "db_restore_snapshot_identifier" {
  description = "Restore the database from this snapshot, into a new instance named with db_instance_suffix. Null normally."
  type        = string
  default     = null
}

variable "db_restore_to_point_in_time" {
  description = "Restore the database from another instance's backups at a moment in its window, into a new instance named with db_instance_suffix. Null normally."
  type = object({
    source_db_instance_identifier = string
    restore_time                  = optional(string)
    use_latest_restorable_time    = optional(bool)
  })
  default = null
}

# ------------------------------------------------------------------- Redis

variable "redis_node_type" {
  description = "ElastiCache node type."
  type        = string
}

variable "redis_num_cache_clusters" {
  description = "Redis nodes: 1, or 2 and more for a replica with automatic failover."
  type        = number

  # Production floor.
  validation {
    condition     = var.environment != "production" || var.redis_num_cache_clusters >= 2
    error_message = "production must run a Redis replica, for automatic failover."
  }
}

# ---------------------------------------------------- logging and secrets

variable "log_retention_days" {
  description = "Days to keep every log group this environment creates."
  type        = number

  # Production floor: the EKS audit log, the database connection log and
  # the flow logs are the audit trail (section 30).
  validation {
    condition     = var.environment != "production" || var.log_retention_days >= 365
    error_message = "production must keep logs for at least 365 days."
  }
}

variable "alarm_email" {
  description = "Address subscribed to the alarm topic. Empty for no subscription; AWS emails a confirmation link before any alarm is delivered."
  type        = string
  default     = ""

  validation {
    condition     = var.alarm_email == "" || can(regex("^[^@[:space:]]+@[^@[:space:]]+\\.[^@[:space:]]+$", var.alarm_email))
    error_message = "alarm_email must be empty or an email address."
  }
}

variable "secret_recovery_window_days" {
  description = "Days a deleted secret stays recoverable. 0 deletes at once, which lets a destroyed environment be recreated under the same secret names."
  type        = number
}

variable "secret_version" {
  description = "Increase to generate and install new application secrets and a new Redis AUTH token. They never enter Terraform state, so this number is how Terraform knows to write new ones."
  type        = number
  default     = 1
}

variable "tags" {
  description = "Extra tags for every resource."
  type        = map(string)
  default     = {}
}

# --------------------------------------------------------- EKS platform

variable "enable_ecr_endpoints" {
  description = "Interface endpoints for ECR in the private subnets, so image pulls never cross the NAT gateway. About USD 7 a month per endpoint per zone."
  type        = bool
  default     = false
}

variable "enable_zonal_shift" {
  description = "Register the cluster with ARC zonal shift. Free."
  type        = bool
  default     = false
}

variable "cluster_deletion_protection" {
  description = "Refuse to delete the cluster until this is turned off first. As with the database, turning it off is the first step of a deliberate teardown, so it has no production floor."
  type        = bool
  default     = false
}

variable "ingress_domain_name" {
  description = "Hostname the application's Ingress serves, for which an ACM certificate is issued and validated through Route 53. Must equal the host in infrastructure/kubernetes/overlays/<environment>/ingress.yaml: the load balancer controller finds the certificate by that host. Null issues no certificate, and the load balancer then serves HTTP only."
  type        = string
  default     = null

  validation {
    condition     = var.ingress_domain_name == null || can(regex("^([a-z0-9-]+\\.)+[a-z]{2,}$", var.ingress_domain_name))
    error_message = "ingress_domain_name must be a lowercase hostname such as api.example.com, or null."
  }

  # Production floor: section 104 requires HTTPS.
  validation {
    condition     = var.environment != "production" || var.ingress_domain_name != null
    error_message = "production must serve HTTPS: set ingress_domain_name (and route53_zone_id) so a certificate is issued."
  }
}

variable "route53_zone_id" {
  description = "Hosted zone that ingress_domain_name lives in, used to answer ACM's validation challenge. Required with ingress_domain_name."
  type        = string
  default     = null

  validation {
    condition     = (var.route53_zone_id == null) == (var.ingress_domain_name == null)
    error_message = "route53_zone_id and ingress_domain_name go together: set both or neither."
  }
}
