# Plans production against a mocked AWS provider: no account and no
# credentials, but a real plan. That evaluates what terraform validate
# cannot: the validation rules on every module's inputs (the production
# floors among them), for_each keys and the subnet arithmetic.
#
# The security invariants are in security.tftest.hcl, asserted module by
# module; a test can see only a child module's outputs, so they cannot be
# asserted from here.

mock_provider "aws" {
  source = "../../tests/mocks"
}

variables {
  allowed_account_ids                  = ["123456789012"]
  cluster_admin_principal_arns         = ["arn:aws:iam::123456789012:role/Admin"]
  cluster_endpoint_public_access_cidrs = ["203.0.113.0/24"]
  ingress_domain_name                  = "api.vitalmesh.example"
  route53_zone_id                      = "Z0123456789ABCDEFGHIJ"
}

run "plans" {
  command = plan

  # The overlay deploys into this namespace, and the deploy role may manage
  # this one only: the two have to agree.
  assert {
    condition     = output.kubernetes_namespace == "vitalmesh-production"
    error_message = "The namespace must match infrastructure/kubernetes/overlays/production."
  }
}
