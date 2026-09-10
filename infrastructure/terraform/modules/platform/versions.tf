terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    # Only for ephemeral passwords, which exist for the length of one run and
    # are never recorded in state.
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }
}
