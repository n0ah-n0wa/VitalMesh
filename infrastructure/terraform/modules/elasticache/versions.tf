terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    # Only for an ephemeral password, which exists for the length of one run
    # and is never recorded in state.
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }
}
