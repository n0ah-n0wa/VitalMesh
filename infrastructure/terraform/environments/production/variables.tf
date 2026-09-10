# Only what is specific to one AWS account and one team. Everything else about
# this environment is a literal in main.tf, where review can see it.

variable "allowed_account_ids" {
  description = "The AWS account this environment lives in. The provider refuses to run against any other, so an apply with the wrong profile fails before it creates anything."
  type        = list(string)

  validation {
    condition     = length(var.allowed_account_ids) > 0 && alltrue([for a in var.allowed_account_ids : can(regex("^[0-9]{12}$", a))])
    error_message = "allowed_account_ids must list one or more twelve-digit AWS account IDs."
  }
}

variable "cluster_admin_principal_arns" {
  description = "IAM roles or users given cluster-admin. Whoever runs terraform apply is not made admin implicitly, so include yourself."
  type        = list(string)
}

variable "cluster_endpoint_public_access_cidrs" {
  description = "Addresses that may reach the Kubernetes API endpoint: your office or VPN, and the deployment runners."
  type        = list(string)
}

variable "alarm_email" {
  description = "Address to receive alarms, or empty for none."
  type        = string
  default     = ""
}

variable "ingress_domain_name" {
  description = "Hostname the Ingress serves; an ACM certificate is issued for it. Must equal the host in infrastructure/kubernetes/overlays/production/ingress.yaml (currently api.vitalmesh.example, a placeholder until a domain exists)."
  type        = string
  default     = null
}

variable "route53_zone_id" {
  description = "Hosted zone ingress_domain_name lives in, for the certificate's DNS validation. Set together with ingress_domain_name."
  type        = string
  default     = null
}

# A database restore is a reviewed change to these three, applied, verified
# and then reverted: docs/DISASTER_RECOVERY.md has the procedure.

variable "db_instance_suffix" {
  description = "Suffix for the database instance identifier, set only during a restore."
  type        = string
  default     = ""
}

variable "db_restore_snapshot_identifier" {
  description = "Restore the database from this snapshot into a new instance. Null normally."
  type        = string
  default     = null
}

variable "db_restore_to_point_in_time" {
  description = "Restore the database to a moment inside its backup window, into a new instance. Null normally."
  type = object({
    source_db_instance_identifier = string
    restore_time                  = optional(string)
    use_latest_restorable_time    = optional(bool)
  })
  default = null
}
