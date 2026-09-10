variable "identifier" {
  description = "RDS instance identifier, and the prefix for everything created alongside it."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,50}$", var.identifier))
    error_message = "identifier must be 3 to 51 lowercase letters, digits or hyphens, starting with a letter."
  }
}

variable "vpc_id" {
  description = "VPC the database lives in."
  type        = string
}

variable "subnet_ids" {
  description = "Database-tier subnets, in at least two zones. They should have no route out of the VPC."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) >= 2
    error_message = "An RDS subnet group needs subnets in at least two availability zones."
  }
}

variable "allowed_security_group_ids" {
  description = "Security groups allowed to connect on 5432, keyed by a name for the rule. The keys must be known at plan time; the IDs need not be."
  type        = map(string)
}

variable "engine_version" {
  description = "PostgreSQL version. A major version such as 16 lets RDS choose and apply minor versions."
  type        = string
  default     = "16"

  validation {
    condition     = can(regex("^1[6-9](\\.[0-9]+)?$", var.engine_version))
    error_message = "engine_version must be PostgreSQL 16 or later, like 16 or 16.4, to match what the service is tested against."
  }
}

variable "instance_class" {
  description = "RDS instance class, such as db.t4g.micro."
  type        = string

  validation {
    condition     = can(regex("^db\\.", var.instance_class))
    error_message = "instance_class must be an RDS class, beginning db."
  }
}

variable "allocated_storage" {
  description = "Initial storage, in GiB."
  type        = number

  validation {
    condition     = var.allocated_storage >= 20
    error_message = "gp3 storage for PostgreSQL starts at 20 GiB."
  }
}

variable "max_allocated_storage" {
  description = "Ceiling for storage autoscaling, in GiB. RDS grows the volume up to this when free space runs low."
  type        = number

  validation {
    condition     = var.max_allocated_storage >= var.allocated_storage
    error_message = "max_allocated_storage must be at least allocated_storage."
  }
}

variable "multi_az" {
  description = "Keep a synchronous standby in a second zone and fail over to it. Roughly doubles the instance cost."
  type        = bool
}

variable "backup_retention_days" {
  description = "Days of automated backups, which is also the point-in-time recovery window."
  type        = number

  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 1 and 35. Zero would disable point-in-time recovery, which section 80 requires."
  }
}

variable "deletion_protection" {
  description = "Refuse to delete the instance until this is turned off in a separate change."
  type        = bool
}

variable "skip_final_snapshot" {
  description = "Delete without a final snapshot. Only sensible where the data is synthetic."
  type        = bool
}

variable "kms_key_arn" {
  description = "KMS key for storage, the RDS-managed master password secret and Performance Insights."
  type        = string
}

variable "log_kms_key_arn" {
  description = "KMS key for the exported PostgreSQL log groups."
  type        = string
}

variable "log_retention_days" {
  description = "Days to keep exported PostgreSQL logs."
  type        = number

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a retention period CloudWatch Logs supports."
  }
}

variable "monitoring_interval" {
  description = "Seconds between Enhanced Monitoring samples; 0 turns it off. Billed as CloudWatch Logs ingestion."
  type        = number
  default     = 0

  validation {
    condition     = contains([0, 1, 5, 10, 15, 30, 60], var.monitoring_interval)
    error_message = "monitoring_interval must be one of 0, 1, 5, 10, 15, 30 or 60."
  }
}

variable "performance_insights_enabled" {
  description = "Turn on Performance Insights. The seven-day tier used here is free, but the smallest instance classes do not support it."
  type        = bool
  default     = true
}

variable "database_name" {
  description = "Database created on the instance."
  type        = string
  default     = "vitalmesh"
}

variable "master_username" {
  description = "Master user name. Its password is generated and held by RDS in Secrets Manager; it never passes through Terraform."
  type        = string
  default     = "vitalmesh_admin"
}

variable "connections_alarm_threshold" {
  description = "Alarm when DatabaseConnections exceeds this. RDS derives max_connections from the instance class's memory (about 110 on 1 GiB, 900 on 8 GiB); set this a little under it. Zero turns the alarm off."
  type        = number
  default     = 0

  validation {
    condition     = var.connections_alarm_threshold >= 0
    error_message = "connections_alarm_threshold must be zero or positive."
  }
}

variable "freeable_memory_alarm_bytes" {
  description = "Alarm when FreeableMemory falls below this many bytes; about a tenth of the instance class's memory is a good line. Zero turns the alarm off."
  type        = number
  default     = 0

  validation {
    condition     = var.freeable_memory_alarm_bytes >= 0
    error_message = "freeable_memory_alarm_bytes must be zero or positive."
  }
}

# ----------------------------------------------------------------- restore

variable "instance_suffix" {
  description = "Appended to the instance identifier (and to its log groups, alarms and final snapshot), so that a restore can create a new instance beside the old one. Empty normally; something like -r20260910 during a restore. See docs/DISASTER_RECOVERY.md."
  type        = string
  default     = ""

  validation {
    condition     = can(regex("^(-[a-z0-9]+)?$", var.instance_suffix))
    error_message = "instance_suffix must be empty or a hyphen followed by lowercase letters and digits."
  }
}

variable "snapshot_identifier" {
  description = "Create the instance from this snapshot (a final snapshot, an automated one, or a manual copy) instead of empty. Requires a new instance_suffix. Mutually exclusive with restore_to_point_in_time."
  type        = string
  default     = null
}

variable "restore_to_point_in_time" {
  description = "Create the instance from another instance's backups at a moment inside its retention window: restore_time as RFC 3339 UTC, or use_latest_restorable_time. Requires a new instance_suffix. Mutually exclusive with snapshot_identifier."
  type = object({
    source_db_instance_identifier = string
    restore_time                  = optional(string)
    use_latest_restorable_time    = optional(bool)
  })
  default = null

  validation {
    condition     = var.restore_to_point_in_time == null || var.snapshot_identifier == null
    error_message = "Set snapshot_identifier or restore_to_point_in_time, not both."
  }

  validation {
    condition     = var.restore_to_point_in_time == null || (var.restore_to_point_in_time.restore_time != null) != (try(var.restore_to_point_in_time.use_latest_restorable_time, false) == true)
    error_message = "restore_to_point_in_time needs exactly one of restore_time or use_latest_restorable_time = true."
  }

  validation {
    condition     = (var.restore_to_point_in_time == null && var.snapshot_identifier == null) || var.instance_suffix != ""
    error_message = "A restore creates a new instance: give it an instance_suffix so it does not replace the one being restored from."
  }
}

variable "enable_alarms" {
  description = "Create CloudWatch alarms for CPU, free storage and (when thresholds are set) connections and memory, and the RDS event subscription."
  type        = bool
  default     = true
}

variable "alarm_topic_arn" {
  description = "SNS topic the alarms notify. Required when enable_alarms is true."
  type        = string
  default     = null
}

variable "tags" {
  description = "Tags for every resource, added to the provider's default tags."
  type        = map(string)
  default     = {}
}
