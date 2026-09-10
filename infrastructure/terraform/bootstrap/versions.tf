terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }

  # No backend block, deliberately. Bootstrap creates the bucket every other
  # stack keeps its state in, so on its first run there is nowhere remote to
  # keep its own. Its state holds no secrets (a bucket, keys, roles and
  # repositories) and the README shows how to move it into the bucket
  # afterwards with terraform init -migrate-state.
}
