variable "name" {
  description = "Replication group ID, and the prefix for everything created alongside it."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,39}$", var.name))
    error_message = "name must be 3 to 40 lowercase letters, digits or hyphens, starting with a letter."
  }
}

variable "vpc_id" {
  description = "VPC the cache lives in."
  type        = string
}

variable "subnet_ids" {
  description = "Database-tier subnets. Two or more when num_cache_clusters is above 1, so the replica can sit in another zone."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) >= 1
    error_message = "At least one subnet is required."
  }
}

variable "allowed_security_group_ids" {
  description = "Security groups allowed to connect on 6379, keyed by a name for the rule. The keys must be known at plan time; the IDs need not be."
  type        = map(string)
}

variable "engine_version" {
  description = "Redis version."
  type        = string
  default     = "7.1"

  validation {
    condition     = can(regex("^7\\.[0-9]+$", var.engine_version))
    error_message = "engine_version must be a Redis 7 release such as 7.1."
  }
}

variable "node_type" {
  description = "Cache node type, such as cache.t4g.micro."
  type        = string

  validation {
    condition     = can(regex("^cache\\.", var.node_type))
    error_message = "node_type must be an ElastiCache node type, beginning cache."
  }
}

variable "num_cache_clusters" {
  description = "Nodes in the group: 1 for a single node, 2 or more for a primary with replicas and automatic failover."
  type        = number

  validation {
    condition     = var.num_cache_clusters >= 1 && var.num_cache_clusters <= 6
    error_message = "num_cache_clusters must be between 1 and 6."
  }

  validation {
    condition     = var.num_cache_clusters == 1 || length(var.subnet_ids) >= 2
    error_message = "A replicated group needs subnets in at least two zones, or failover has nowhere to go."
  }
}

variable "kms_key_arn" {
  description = "KMS key for data at rest and for the AUTH token secret."
  type        = string
}

variable "log_kms_key_arn" {
  description = "KMS key for the slow-log and engine-log groups."
  type        = string
}

variable "log_retention_days" {
  description = "Days to keep Redis logs."
  type        = number

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a retention period CloudWatch Logs supports."
  }
}

variable "auth_secret_name" {
  description = "Name of the Secrets Manager secret that receives the AUTH token."
  type        = string
}

variable "secret_recovery_window_days" {
  description = "Days a deleted secret can still be recovered. 0 deletes it immediately."
  type        = number

  validation {
    condition     = var.secret_recovery_window_days == 0 || (var.secret_recovery_window_days >= 7 && var.secret_recovery_window_days <= 30)
    error_message = "secret_recovery_window_days must be 0, or between 7 and 30."
  }
}

variable "auth_token_version" {
  description = "Increase to generate and install a new AUTH token. The token never enters Terraform state, so this number is the only way Terraform can tell it should change."
  type        = number
  default     = 1

  validation {
    condition     = var.auth_token_version >= 1 && floor(var.auth_token_version) == var.auth_token_version
    error_message = "auth_token_version must be a whole number, 1 or more."
  }
}

variable "enable_alarms" {
  # Alarms: engine CPU and memory on every member, evictions on every
  # member, and replication lag on every replica.
  description = "Create CloudWatch alarms for engine CPU and memory."
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
