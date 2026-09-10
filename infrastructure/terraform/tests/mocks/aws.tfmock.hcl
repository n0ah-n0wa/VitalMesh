# What the mocked AWS provider returns, in every terraform test under
# infrastructure/terraform.
#
# A mocked provider still validates the arguments it is given, so a value
# that ends up in a policy or an ARN has to look like one: left to itself,
# the mock invents a random string, and the plan fails on "invalid JSON" or
# "invalid partition" before reaching anything worth testing. The other
# values are here to keep the plan realistic.
#
# The mock_resource blocks fill computed nested blocks that the modules
# index into ([0]). A mocked apply would otherwise leave them empty and the
# outputs that read them would fail.

mock_data "aws_availability_zones" {
  defaults = {
    names = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
  }
}

mock_data "aws_caller_identity" {
  defaults = {
    account_id = "123456789012"
  }
}

mock_data "aws_partition" {
  defaults = {
    partition  = "aws"
    dns_suffix = "amazonaws.com"
  }
}

mock_data "aws_region" {
  defaults = {
    region = "eu-central-1"
  }
}

mock_data "aws_iam_policy_document" {
  defaults = {
    json          = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sts:AssumeRole\",\"Principal\":{\"Service\":\"ec2.amazonaws.com\"}}]}"
    minified_json = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sts:AssumeRole\",\"Principal\":{\"Service\":\"ec2.amazonaws.com\"}}]}"
  }
}

mock_data "aws_iam_openid_connect_provider" {
  defaults = {
    arn = "arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com"
  }
}

mock_data "aws_eks_addon_version" {
  defaults = {
    version = "v1.0.0-eksbuild.1"
  }
}

mock_resource "aws_eks_cluster" {
  defaults = {
    arn      = "arn:aws:eks:eu-central-1:123456789012:cluster/vitalmesh-mock"
    endpoint = "https://EXAMPLE.gr7.eu-central-1.eks.amazonaws.com"
    identity = [{
      oidc = [{ issuer = "https://oidc.eks.eu-central-1.amazonaws.com/id/EXAMPLE0123456789" }]
    }]
    certificate_authority = [{ data = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t" }]
  }
}

mock_resource "aws_db_instance" {
  defaults = {
    master_user_secret = [{
      kms_key_id    = "arn:aws:kms:eu-central-1:123456789012:key/11111111-1111-1111-1111-111111111111"
      secret_arn    = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:rds!db-mock-AbCdEf"
      secret_status = "active"
    }]
  }
}

mock_resource "aws_kms_key" {
  defaults = {
    arn = "arn:aws:kms:eu-central-1:123456789012:key/22222222-2222-2222-2222-222222222222"
  }
}

# Resources whose ARNs other resources validate as ARNs before accepting
# them (a flow log's destination and role, a cluster's role, an alarm's
# topic). One fixed ARN per type is enough for the mock.
mock_resource "aws_cloudwatch_log_group" {
  defaults = {
    arn = "arn:aws:logs:eu-central-1:123456789012:log-group:/aws/mock/vitalmesh"
  }
}

mock_resource "aws_iam_role" {
  defaults = {
    arn = "arn:aws:iam::123456789012:role/vitalmesh-mock"
  }
}

mock_resource "aws_sns_topic" {
  defaults = {
    arn = "arn:aws:sns:eu-central-1:123456789012:vitalmesh-mock-alarms"
  }
}

mock_resource "aws_secretsmanager_secret" {
  defaults = {
    arn = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:vitalmesh/mock-AbCdEf"
  }
}

# Resources whose IDs other resources validate by prefix before accepting
# them (a node group its launch template, a cluster its subnets).
mock_resource "aws_launch_template" {
  defaults = {
    id             = "lt-0123456789abcdef0"
    latest_version = 1
  }
}

mock_resource "aws_vpc" {
  defaults = {
    id = "vpc-0123456789abcdef0"
  }
}

mock_resource "aws_subnet" {
  defaults = {
    id = "subnet-0123456789abcdef0"
  }
}

mock_resource "aws_security_group" {
  defaults = {
    id = "sg-0123456789abcdef0"
  }
}

mock_resource "aws_internet_gateway" {
  defaults = {
    id = "igw-0123456789abcdef0"
  }
}

mock_resource "aws_nat_gateway" {
  defaults = {
    id = "nat-0123456789abcdef0"
  }
}

mock_resource "aws_route_table" {
  defaults = {
    id = "rtb-0123456789abcdef0"
  }
}

# The certificate's validation challenge, which the real provider knows at
# plan time and the mock only at apply, and its ARN, which the validation
# resource checks the shape of.
mock_resource "aws_acm_certificate" {
  defaults = {
    arn = "arn:aws:acm:eu-central-1:123456789012:certificate/33333333-3333-3333-3333-333333333333"
    domain_validation_options = [{
      domain_name           = "api.vitalmesh.example"
      resource_record_name  = "_abcdef0123456789.api.vitalmesh.example."
      resource_record_type  = "CNAME"
      resource_record_value = "_0123456789abcdef.acm-validations.aws."
    }]
  }
}

mock_resource "aws_route53_record" {
  defaults = {
    fqdn = "_abcdef0123456789.api.vitalmesh.example"
  }
}
