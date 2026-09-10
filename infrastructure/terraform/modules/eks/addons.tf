# Managed add-ons. Each version is EKS's default for the cluster's Kubernetes
# version rather than the newest, so a plan does not change just because AWS
# published a release overnight.

locals {
  addon_names = concat(
    ["vpc-cni", "kube-proxy", "coredns", "metrics-server"],
    var.enable_cloudwatch_observability ? ["amazon-cloudwatch-observability"] : [],
  )

  container_insights_log_groups = var.enable_cloudwatch_observability ? toset(["application", "dataplane", "host", "performance"]) : toset([])
}

data "aws_eks_addon_version" "this" {
  for_each = toset(local.addon_names)

  addon_name         = each.key
  kubernetes_version = aws_eks_cluster.this.version
  most_recent        = false
}

resource "aws_eks_addon" "vpc_cni" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "vpc-cni"
  addon_version               = data.aws_eks_addon_version.this["vpc-cni"].version
  service_account_role_arn    = aws_iam_role.irsa["vpc_cni"].arn
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  # NetworkPolicy enforcement. The VPC CNI ships the policy agent but leaves
  # it off, and without it every NetworkPolicy in the manifests is accepted
  # and ignored: default-deny denying nothing. This is what the manifests'
  # README means by "the EKS VPC CNI with policy enforcement enabled".
  configuration_values = jsonencode({
    enableNetworkPolicy = "true"
  })

  tags = var.tags

  depends_on = [aws_iam_role_policy_attachment.irsa]
}

resource "aws_eks_addon" "kube_proxy" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "kube-proxy"
  addon_version               = data.aws_eks_addon_version.this["kube-proxy"].version
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  tags = var.tags
}

# CoreDNS and metrics-server run as Deployments, so they need nodes to be
# scheduled on and are created after the node group.
resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "coredns"
  addon_version               = data.aws_eks_addon_version.this["coredns"].version
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  tags = var.tags

  depends_on = [aws_eks_node_group.default]
}

# The HorizontalPodAutoscalers scale on CPU, which they read from the
# metrics API. Without metrics-server that API does not exist, and every HPA
# reports <unknown> and never scales.
resource "aws_eks_addon" "metrics_server" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "metrics-server"
  addon_version               = data.aws_eks_addon_version.this["metrics-server"].version
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  tags = var.tags

  depends_on = [aws_eks_node_group.default]
}

# The CloudWatch Observability add-on writes to these groups. Left to it, it
# creates them on first write with no retention and no encryption, and a
# later terraform apply then fails because they already exist. Creating them
# first, with both, avoids the race as well as the defaults.
resource "aws_cloudwatch_log_group" "container_insights" {
  for_each = local.container_insights_log_groups

  name              = "/aws/containerinsights/${var.name}/${each.key}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = var.tags
}

resource "aws_eks_addon" "cloudwatch_observability" {
  count = var.enable_cloudwatch_observability ? 1 : 0

  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "amazon-cloudwatch-observability"
  addon_version               = data.aws_eks_addon_version.this["amazon-cloudwatch-observability"].version
  service_account_role_arn    = aws_iam_role.irsa["cloudwatch"].arn
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  tags = var.tags

  depends_on = [
    aws_eks_node_group.default,
    aws_iam_role_policy_attachment.irsa,
    aws_cloudwatch_log_group.container_insights,
  ]
}
