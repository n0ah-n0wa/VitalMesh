# The cluster: control plane, its IAM role, and the OIDC provider that lets
# pods assume IAM roles without node credentials.

data "aws_partition" "current" {}

locals {
  policy_prefix = "arn:${data.aws_partition.current.partition}:iam::aws:policy"
  oidc_issuer   = replace(aws_eks_cluster.this.identity[0].oidc[0].issuer, "https://", "")
}

# Created here rather than left to EKS, which would create it on first write
# with no retention (kept forever, billed forever) and no encryption.
resource "aws_cloudwatch_log_group" "cluster" {
  name              = "/aws/eks/${var.name}/cluster"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = var.tags
}

data "aws_iam_policy_document" "cluster_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]

    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "cluster" {
  name               = "${var.name}-cluster"
  description        = "EKS control plane for ${var.name}"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "${local.policy_prefix}/AmazonEKSClusterPolicy"
}

# Envelope encryption of Secrets: the control plane encrypts each data key
# with this KMS key, so what sits in etcd is useless without it.
data "aws_iam_policy_document" "cluster_kms" {
  statement {
    actions   = ["kms:Decrypt", "kms:DescribeKey", "kms:Encrypt", "kms:ListGrants"]
    resources = [var.secrets_kms_key_arn]
  }
}

resource "aws_iam_role_policy" "cluster_kms" {
  name   = "secrets-envelope-encryption"
  role   = aws_iam_role.cluster.id
  policy = data.aws_iam_policy_document.cluster_kms.json
}

# The API endpoint is public, but only to endpoint_public_access_cidrs, and
# every request still needs an IAM identity with an access entry. It is not
# private-only because nothing inside the VPC can run kubectl yet: there is
# no VPN, and GitHub's hosted runners are outside it. Once something is, the
# environment roots set cluster_endpoint_public_access = false and nothing
# else changes, because the private endpoint below is always on.
#trivy:ignore:AVD-AWS-0040
resource "aws_eks_cluster" "this" {
  #checkov:skip=CKV_AWS_39:Deliberate and restricted to listed addresses until something inside the VPC can reach a private endpoint. See the comment above.
  #checkov:skip=CKV_AWS_38:endpoint_public_access_cidrs is validated to refuse 0.0.0.0/0 and to be non-empty. Checkov does not evaluate variable validation.
  #checkov:skip=CKV_AWS_339:Checkov's list of supported versions predates the version set here. EKS itself refuses an unsupported version when the cluster is created.

  name     = var.name
  version  = var.kubernetes_version
  role_arn = aws_iam_role.cluster.arn

  # All five. audit is the record of who did what in the cluster (section
  # 30). The scheduler and controller manager logs explain why a pod is
  # Pending, which is the first question a scale-out that stalls raises.
  enabled_cluster_log_types = ["api", "audit", "authenticator", "controllerManager", "scheduler"]

  # EKS would otherwise install vpc-cni, kube-proxy and CoreDNS itself as
  # unmanaged components that nothing here could configure or upgrade. They
  # are installed as managed add-ons in addons.tf instead, which is what
  # makes it possible to turn on NetworkPolicy enforcement and to give the
  # CNI its own IAM role.
  bootstrap_self_managed_addons = false

  access_config {
    # Access entries only, no aws-auth ConfigMap: who can reach the cluster
    # is then an AWS API object that shows up in plan and in CloudTrail, not
    # YAML edited inside the cluster.
    authentication_mode = "API"

    # Whoever runs terraform apply would otherwise be made cluster-admin
    # silently and permanently. Admins are named in admin_principal_arns
    # instead, where review can see them.
    bootstrap_cluster_creator_admin_permissions = false
  }

  vpc_config {
    subnet_ids              = var.subnet_ids
    endpoint_private_access = true
    endpoint_public_access  = var.endpoint_public_access
    public_access_cidrs     = var.endpoint_public_access ? var.endpoint_public_access_cidrs : null
  }

  encryption_config {
    resources = ["secrets"]

    provider {
      key_arn = var.secrets_kms_key_arn
    }
  }

  # When a version leaves standard support EKS moves the cluster to extended
  # support and bills the control plane at six times the rate, without
  # asking. STANDARD makes it upgrade instead. An upgrade can break things,
  # but it breaks them visibly; the bill does not.
  upgrade_policy {
    support_type = "STANDARD"
  }

  # With this on, EKS refuses DeleteCluster until it is turned off again:
  # a terraform destroy of production stops here, in its own reviewed
  # change, as the database's deletion protection does.
  deletion_protection = var.deletion_protection

  # ARC zonal shift: when one availability zone is impaired, one API call
  # (or ARC's own autoshift) moves the cluster's traffic away from it, and
  # the load balancers the controller creates follow. There is nothing to
  # run and nothing to pay; registering is the whole of it.
  zonal_shift_config {
    enabled = var.enable_zonal_shift
  }

  tags = var.tags

  depends_on = [
    aws_iam_role_policy_attachment.cluster,
    aws_iam_role_policy.cluster_kms,
    aws_cloudwatch_log_group.cluster,
  ]
}

# Lets pods exchange a Kubernetes service account token for an IAM role
# (IRSA). AWS validates this issuer against its own trust store, so no
# certificate thumbprint is pinned here.
resource "aws_iam_openid_connect_provider" "cluster" {
  url            = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list = ["sts.amazonaws.com"]

  tags = var.tags
}
