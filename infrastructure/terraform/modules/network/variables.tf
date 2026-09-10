variable "name" {
  description = "Prefix for every resource name, such as vitalmesh-staging."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,40}$", var.name))
    error_message = "name must be 3 to 41 lowercase letters, digits or hyphens, starting with a letter."
  }
}

variable "cidr_block" {
  description = "IPv4 range for the VPC. Must be a /16: the subnet layout in main.tf carves /24s and /19s out of it."
  type        = string

  validation {
    condition     = can(cidrnetmask(var.cidr_block)) && can(regex("/16$", var.cidr_block))
    error_message = "cidr_block must be a valid IPv4 /16."
  }
}

variable "az_count" {
  description = "Availability zones to use. At least two, because an EKS cluster and an RDS subnet group each require two."
  type        = number

  validation {
    condition     = var.az_count >= 2 && var.az_count <= 3
    error_message = "az_count must be 2 or 3."
  }
}

variable "single_nat_gateway" {
  description = "One NAT gateway for the VPC instead of one per zone. Saves roughly USD 32 a month per zone; a failure in that zone then takes all egress with it."
  type        = bool
}

variable "enable_flow_logs" {
  description = "Record VPC flow logs to CloudWatch Logs."
  type        = bool
  default     = true
}

variable "flow_log_retention_days" {
  description = "Days to keep flow logs. One of the values CloudWatch Logs accepts."
  type        = number
  default     = 30

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.flow_log_retention_days)
    error_message = "flow_log_retention_days must be a retention period CloudWatch Logs supports."
  }
}

variable "log_kms_key_arn" {
  description = "KMS key that encrypts the flow log group. Its key policy must let the CloudWatch Logs service use it."
  type        = string
}

variable "tags" {
  description = "Tags for every resource, added to the provider's default tags."
  type        = map(string)
  default     = {}
}

variable "enable_ecr_endpoints" {
  description = "Interface endpoints for ECR (api and dkr) in the private subnets, so image pulls stay inside AWS instead of crossing the NAT gateway. About USD 7 a month per endpoint per zone; worth it where pulls are frequent or NAT data charges add up."
  type        = bool
  default     = false
}
