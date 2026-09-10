# Everything that exists once per AWS account rather than once per
# environment, applied once by a person with administrator credentials:
#
#   state.tf   the S3 bucket and KMS key the environment stacks keep state in
#   github.tf  GitHub's OIDC provider and the two CI roles that trust it
#   ecr.tf     the image repositories, shared by staging and production so an
#              image is built once and promoted, not rebuilt per environment
#
# The environment stacks look up what they need from here by name, not by
# reading this stack's state, so the two stay independent.

provider "aws" {
  region              = var.aws_region
  allowed_account_ids = var.allowed_account_ids

  default_tags {
    tags = {
      Project     = "vitalmesh"
      Environment = "shared"
      Stack       = "bootstrap"
      ManagedBy   = "terraform"
      Repository  = "github.com/${var.github_repository}"
    }
  }
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  account_id   = data.aws_caller_identity.current.account_id
  partition    = data.aws_partition.current.partition
  account_root = "arn:${local.partition}:iam::${local.account_id}:root"

  # Bucket names are global. Account and region make it unique and say at a
  # glance which account's state it holds.
  state_bucket_name = "vitalmesh-tfstate-${local.account_id}-${var.aws_region}"

  github_oidc_host = "token.actions.githubusercontent.com"
}
