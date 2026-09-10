# Plans staging against a mocked AWS provider: no account and no
# credentials, but a real plan. That evaluates what terraform validate
# cannot: the validation rules on every module's inputs, for_each keys and
# the subnet arithmetic.
#
# The security invariants are module properties and staging uses the same
# modules as production; they are asserted once, in
# environments/production/tests/security.tftest.hcl.

mock_provider "aws" {
  source = "../../tests/mocks"
}

variables {
  allowed_account_ids                  = ["123456789012"]
  cluster_admin_principal_arns         = ["arn:aws:iam::123456789012:role/Admin"]
  cluster_endpoint_public_access_cidrs = ["203.0.113.0/24"]
}

run "plans" {
  command = plan

  # The overlay deploys into this namespace, and the deploy role may manage
  # this one only: the two have to agree.
  assert {
    condition     = output.kubernetes_namespace == "vitalmesh-staging"
    error_message = "The namespace must match infrastructure/kubernetes/overlays/staging."
  }
}
