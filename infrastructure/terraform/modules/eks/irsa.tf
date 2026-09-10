# IAM roles for service accounts. Each is trusted only by one service account
# in one namespace of this cluster, so the permission cannot be borrowed by
# any other pod.
#
# The application itself has none. The gateway and the processor talk to
# PostgreSQL and Redis over the network with credentials from Kubernetes
# Secrets and never call an AWS API, so they are given no IAM role at all.
# The roles here belong to platform components: the CNI, the load balancer
# controller, the autoscaler and, where enabled, the CloudWatch agent. The
# controllers themselves are installed on the cluster from
# infrastructure/kubernetes/platform, which reads the role ARNs from this
# stack's outputs.
#
# IRSA rather than EKS Pod Identity, deliberately. Pod Identity would remove
# the OIDC provider and the per-role trust policy, but the load balancer
# controller and the autoscaler both document IRSA as their primary path,
# the VPC CNI needs its role before the first node is Ready (Pod Identity's
# agent runs as a DaemonSet on those nodes), and one mechanism is simpler
# than two. Nothing here prevents moving to Pod Identity later.

locals {
  irsa_candidates = {
    vpc_cni = {
      enabled         = true
      namespace       = "kube-system"
      service_account = "aws-node"
      policy_arns     = ["${local.policy_prefix}/AmazonEKS_CNI_Policy"]
      policy_json     = null
    }

    # The AWS Load Balancer Controller: turns an Ingress into an Application
    # Load Balancer and keeps its target group pointed at the pods. Its
    # policy is the project's own published one, at the version pinned in
    # infrastructure/kubernetes/platform, kept verbatim so that a diff
    # against upstream is a diff and not a translation. It is broad by
    # nature (the controller creates and deletes load balancers, target
    # groups and security groups) and every write is conditioned on the
    # elbv2.k8s.aws/cluster tag the controller itself sets, so it can only
    # change what it created. The ARNs in it name the aws partition, which
    # is the only one this stack targets.
    load_balancer_controller = {
      enabled         = var.enable_load_balancer_controller
      namespace       = "kube-system"
      service_account = "aws-load-balancer-controller"
      policy_arns     = []
      policy_json     = file("${path.module}/policies/aws-load-balancer-controller.json")
    }

    # The Cluster Autoscaler: grows and shrinks the managed node group
    # within its min and max as pods go Pending or nodes sit empty.
    cluster_autoscaler = {
      enabled         = var.enable_cluster_autoscaler
      namespace       = "kube-system"
      service_account = "cluster-autoscaler"
      policy_arns     = []
      policy_json     = data.aws_iam_policy_document.cluster_autoscaler.json
    }

    cloudwatch = {
      enabled         = var.enable_cloudwatch_observability
      namespace       = "amazon-cloudwatch"
      service_account = "cloudwatch-agent"
      policy_arns     = ["${local.policy_prefix}/CloudWatchAgentServerPolicy"]
      policy_json     = null
    }
  }

  irsa = { for k, v in local.irsa_candidates : k => v if v.enabled }

  irsa_attachments = {
    for pair in flatten([
      for role, cfg in local.irsa : [
        for arn in cfg.policy_arns : { key = "${role}/${basename(arn)}", role = role, policy_arn = arn }
      ]
    ]) : pair.key => pair
  }

  irsa_inline = { for k, v in local.irsa : k => v.policy_json if v.policy_json != null }
}

# AWS's documented policy for the autoscaler, with the write actions
# conditioned on the tag EKS puts on every managed node group's Auto
# Scaling group: k8s.io/cluster-autoscaler/<cluster> = owned. The
# autoscaler can therefore resize this cluster's groups and no other.
data "aws_iam_policy_document" "cluster_autoscaler" {
  #checkov:skip=CKV_AWS_356:The Auto Scaling group is created by EKS with the node group and has no ARN this stack could name; the resource is "*" and the condition on the k8s.io/cluster-autoscaler/<cluster> tag is the scoping, as in AWS's documented policy. The security test asserts the condition is present.

  statement {
    sid = "ResizeThisClustersNodeGroups"
    actions = [
      "autoscaling:SetDesiredCapacity",
      "autoscaling:TerminateInstanceInAutoScalingGroup",
    ]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/k8s.io/cluster-autoscaler/${var.name}"
      values   = ["owned"]
    }
  }

  statement {
    sid = "DiscoverWhatThereIsToScale"
    actions = [
      "autoscaling:DescribeAutoScalingGroups",
      "autoscaling:DescribeAutoScalingInstances",
      "autoscaling:DescribeLaunchConfigurations",
      "autoscaling:DescribeScalingActivities",
      "autoscaling:DescribeTags",
      "ec2:DescribeImages",
      "ec2:DescribeInstanceTypes",
      "ec2:DescribeLaunchTemplateVersions",
      "ec2:GetInstanceTypesFromInstanceRequirements",
      "eks:DescribeNodegroup",
    ]
    resources = ["*"]
  }
}

data "aws_iam_policy_document" "irsa_assume" {
  for_each = local.irsa

  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.cluster.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer}:sub"
      values   = ["system:serviceaccount:${each.value.namespace}:${each.value.service_account}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "irsa" {
  for_each = local.irsa

  name               = "${var.name}-${replace(each.key, "_", "-")}"
  description        = "IRSA role for ${each.value.namespace}/${each.value.service_account} in ${var.name}"
  assume_role_policy = data.aws_iam_policy_document.irsa_assume[each.key].json

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "irsa" {
  for_each = local.irsa_attachments

  role       = aws_iam_role.irsa[each.value.role].name
  policy_arn = each.value.policy_arn
}

resource "aws_iam_role_policy" "irsa" {
  for_each = local.irsa_inline

  name   = replace(each.key, "_", "-")
  role   = aws_iam_role.irsa[each.key].id
  policy = each.value
}
