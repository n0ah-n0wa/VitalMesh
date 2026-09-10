# Who may use the Kubernetes API, as EKS access entries.

locals {
  access_policy_prefix = "arn:${data.aws_partition.current.partition}:eks::aws:cluster-access-policy"
}

resource "aws_eks_access_entry" "admin" {
  for_each = toset(var.admin_principal_arns)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value

  tags = var.tags
}

resource "aws_eks_access_policy_association" "admin" {
  for_each = toset(var.admin_principal_arns)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = aws_eks_access_entry.admin[each.key].principal_arn
  policy_arn    = "${local.access_policy_prefix}/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }
}

# The deploy role manages the application's namespace and nothing outside it.
# That covers Deployments, Services, ConfigMaps, Secrets, Roles and
# RoleBindings, NetworkPolicies and Jobs inside the namespace. It does not
# cover what the Kubernetes base declares at namespace or cluster scope: the
# Namespace itself with its Pod Security labels, the ResourceQuota and the
# LimitRange. A cluster admin applies those once, so a compromised pipeline
# cannot lift its own quota or relax Pod Security.
resource "aws_eks_access_entry" "deployer" {
  cluster_name  = aws_eks_cluster.this.name
  principal_arn = var.deployer.principal_arn

  tags = var.tags
}

resource "aws_eks_access_policy_association" "deployer" {
  cluster_name  = aws_eks_cluster.this.name
  principal_arn = aws_eks_access_entry.deployer.principal_arn
  policy_arn    = "${local.access_policy_prefix}/AmazonEKSAdminPolicy"

  access_scope {
    type       = "namespace"
    namespaces = var.deployer.namespaces
  }
}
