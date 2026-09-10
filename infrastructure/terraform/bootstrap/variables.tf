variable "aws_region" {
  description = "Region for the state bucket and the ECR repositories. The environment roots name the same region in their backend and provider blocks."
  type        = string
  default     = "eu-central-1"

  validation {
    condition     = can(regex("^[a-z]{2}(-[a-z]+)+-[0-9]$", var.aws_region))
    error_message = "aws_region must look like eu-central-1."
  }
}

variable "allowed_account_ids" {
  description = "The AWS account this is applied to. The provider refuses to run against any other, so a wrong profile fails before it creates anything."
  type        = list(string)

  validation {
    condition     = length(var.allowed_account_ids) > 0 && alltrue([for a in var.allowed_account_ids : can(regex("^[0-9]{12}$", a))])
    error_message = "allowed_account_ids must list one or more twelve-digit AWS account IDs."
  }
}

variable "github_repository" {
  description = "owner/name of the repository whose GitHub Actions may assume the CI roles."
  type        = string
  default     = "n0ah-n0wa/VitalMesh"

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must look like owner/name."
  }
}

variable "ecr_repositories" {
  description = "ECR repositories to create, one per service image."
  type        = set(string)
  default     = ["vitalmesh/api-gateway", "vitalmesh/processor"]
}

variable "ecr_keep_images" {
  description = "Images kept per repository before the oldest are expired. An expired image that production still runs cannot be pulled again, so this is generous."
  type        = number
  default     = 200

  validation {
    condition     = var.ecr_keep_images >= 20
    error_message = "ecr_keep_images must be at least 20, so a rollback target is never expired early."
  }
}
