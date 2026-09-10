terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }

  # Partial configuration: the bucket and the key are specific to one account
  # and come from bootstrap's outputs at init time.
  #
  #   terraform init \
  #     -backend-config="bucket=<bootstrap output state_bucket_name>" \
  #     -backend-config="kms_key_id=<bootstrap output state_kms_key_arn>"
  #
  # The key below is what isolates this environment's state: a separate
  # object with its own lock, which nothing run against staging opens.
  backend "s3" {
    key          = "environments/production/terraform.tfstate"
    region       = "eu-central-1"
    encrypt      = true
    use_lockfile = true
  }
}
