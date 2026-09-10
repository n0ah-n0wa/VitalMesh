# One managed node group in the private subnets, sized between
# node_min_size and node_max_size by the Cluster Autoscaler once it is
# installed (infrastructure/kubernetes/platform), and repaired by EKS.
#
# A managed node group rather than EKS Auto Mode. Auto Mode would take the
# node group, the autoscaler, the load balancer controller and the storage
# driver off this stack's hands, at a premium on every instance hour and at
# the cost of the launch template below: Auto Mode nodes cannot be given a
# launch template, so the IMDS hop limit, the root volume and the instance
# tags this repository asserts on would be AWS's choices rather than
# reviewed ones. For two small clusters the managed pieces below cover the
# same ground, and every one of them can be read in this file.
#
# One group spanning every zone rather than one per zone. The autoscaler
# cannot then pick which zone a new node lands in, which matters only for
# pods pinned to a zone by a persistent volume, and nothing here has one
# (the state is in RDS and ElastiCache). Karpenter would remove that
# limitation and choose instance types as well; it also needs an SQS
# queue, EventBridge rules and its own node role and CRDs. It is the
# upgrade path, not the starting point.
#
# EKS tags the group's Auto Scaling group with
# k8s.io/cluster-autoscaler/enabled and k8s.io/cluster-autoscaler/<cluster>
# itself, which is how the autoscaler finds it and how its IAM policy in
# irsa.tf is scoped to it.

data "aws_iam_policy_document" "node_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "${var.name}-node"
  description        = "EKS worker nodes for ${var.name}"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json

  tags = var.tags
}

# Two policies, where the usual setup has three. AmazonEKS_CNI_Policy (the
# right to create and delete network interfaces and assign addresses) is not
# on the node: it is on the vpc-cni add-on's own IRSA role in irsa.tf. On the
# node it would be available to every pod that can reach the instance's
# credentials.
resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset(["AmazonEKSWorkerNodePolicy", "AmazonEC2ContainerRegistryReadOnly"])

  role       = aws_iam_role.node.name
  policy_arn = "${local.policy_prefix}/${each.key}"
}

resource "aws_launch_template" "node" {
  name_prefix            = "${var.name}-node-"
  description            = "EKS nodes for ${var.name}: IMDSv2 only, hop limit 1, encrypted root volume"
  update_default_version = true

  # IMDSv2 only, with a hop limit of 1: the node can reach instance metadata,
  # a pod behind the node cannot, so a compromised pod cannot read the node's
  # IAM credentials. Workloads that need AWS use IRSA instead. The
  # application's NetworkPolicies exclude 169.254.0.0/16 as well; this is the
  # same rule enforced one layer lower.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = "/dev/xvda"

    ebs {
      volume_size = var.node_disk_size_gib
      volume_type = "gp3"
      # Encrypted with the account's AWS-managed EBS key rather than the
      # environment's customer-managed one. A customer-managed key here needs
      # a key policy that lets the Auto Scaling service-linked role use it;
      # without that, nodes fail to launch with an error that names neither
      # KMS nor the key.
      encrypted             = true
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(var.tags, { Name = "${var.name}-node" })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(var.tags, { Name = "${var.name}-node" })
  }

  tags = var.tags
}

resource "aws_eks_node_group" "default" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "default"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.subnet_ids
  ami_type        = var.node_ami_type
  capacity_type   = "ON_DEMAND"
  instance_types  = var.node_instance_types

  scaling_config {
    min_size     = var.node_min_size
    desired_size = var.node_desired_size
    max_size     = var.node_max_size
  }

  update_config {
    max_unavailable = 1
  }

  # EKS's own node health monitoring: a node that fails its checks (kubelet
  # gone, network broken, disk full) is drained and replaced without a
  # person noticing first.
  node_repair_config {
    enabled = var.node_auto_repair
  }

  labels = {
    "vitalmesh.io/node-group" = "default"
  }

  launch_template {
    id      = aws_launch_template.node.id
    version = aws_launch_template.node.latest_version
  }

  tags = var.tags

  # Nodes only become Ready once the CNI is running, and the node group
  # waits for Ready. With the CNI created after the group, the group would
  # wait for something that could not start until the group had finished.
  depends_on = [
    aws_iam_role_policy_attachment.node,
    aws_eks_addon.vpc_cni,
    aws_eks_addon.kube_proxy,
  ]
}
