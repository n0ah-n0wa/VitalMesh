output "cluster_name" {
  description = "Name of the EKS cluster."
  value       = aws_eks_cluster.this.name
}

output "cluster_arn" {
  description = "ARN of the EKS cluster."
  value       = aws_eks_cluster.this.arn
}

output "cluster_endpoint" {
  description = "HTTPS endpoint of the Kubernetes API."
  value       = aws_eks_cluster.this.endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64 certificate of the cluster's CA, for a kubeconfig."
  value       = aws_eks_cluster.this.certificate_authority[0].data
}

output "cluster_version" {
  description = "Kubernetes version the control plane runs."
  value       = aws_eks_cluster.this.version
}

output "cluster_security_group_id" {
  description = "Security group EKS created for the cluster and attaches to the nodes. The database security groups admit this and nothing else."
  value       = aws_eks_cluster.this.vpc_config[0].cluster_security_group_id
}

output "oidc_provider_arn" {
  description = "ARN of the cluster's IAM OIDC provider, for further IRSA roles."
  value       = aws_iam_openid_connect_provider.cluster.arn
}

output "oidc_issuer" {
  description = "OIDC issuer host and path, without the scheme, as IRSA trust conditions need it."
  value       = local.oidc_issuer
}

output "node_role_arn" {
  description = "IAM role the worker nodes run as."
  value       = aws_iam_role.node.arn
}

output "control_plane_log_group_name" {
  description = "CloudWatch log group receiving API, audit and authenticator logs."
  value       = aws_cloudwatch_log_group.cluster.name
}

output "load_balancer_controller_role_arn" {
  description = "IRSA role for the AWS Load Balancer Controller's service account (kube-system/aws-load-balancer-controller), or null when disabled. The Helm values in infrastructure/kubernetes/platform take it."
  value       = var.enable_load_balancer_controller ? aws_iam_role.irsa["load_balancer_controller"].arn : null
}

output "cluster_autoscaler_role_arn" {
  description = "IRSA role for the Cluster Autoscaler's service account (kube-system/cluster-autoscaler), or null when disabled."
  value       = var.enable_cluster_autoscaler ? aws_iam_role.irsa["cluster_autoscaler"].arn : null
}

output "node_group_name" {
  description = "Name of the managed node group."
  value       = aws_eks_node_group.default.node_group_name
}
