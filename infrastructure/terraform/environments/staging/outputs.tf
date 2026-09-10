# The interface the deployment pipeline reads. No output is a credential:
# each secret is named by the ARN of the Secrets Manager entry that holds it.

output "cluster_name" {
  description = "EKS cluster name, for aws eks update-kubeconfig."
  value       = module.platform.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = module.platform.cluster_endpoint
}

output "kubernetes_namespace" {
  description = "Namespace the application deploys into; matches infrastructure/kubernetes/overlays/staging."
  value       = module.platform.kubernetes_namespace
}

output "deploy_role_arn" {
  description = "Role the CD job assumes through GitHub OIDC, from the staging GitHub environment only."
  value       = module.platform.deploy_role_arn
}

output "database_address" {
  description = "PostgreSQL hostname, reachable only from inside the cluster."
  value       = module.platform.database_address
}

output "database_port" {
  description = "PostgreSQL port."
  value       = module.platform.database_port
}

output "database_name" {
  description = "Database name."
  value       = module.platform.database_name
}

output "database_master_username" {
  description = "Master user name; its password is in database_master_secret_arn."
  value       = module.platform.database_master_username
}

output "database_instance_identifier" {
  description = "The RDS instance's identifier: the source named in a point-in-time restore."
  value       = module.platform.database_instance_identifier
}

output "database_master_secret_arn" {
  description = "Secret holding the master password, generated and rotated by RDS."
  value       = module.platform.database_master_secret_arn
}

output "redis_primary_endpoint" {
  description = "Redis hostname, reachable only from inside the cluster, over TLS (rediss://)."
  value       = module.platform.redis_primary_endpoint
}

output "redis_port" {
  description = "Redis port."
  value       = module.platform.redis_port
}

output "redis_auth_secret_arn" {
  description = "Secret holding the Redis AUTH token."
  value       = module.platform.redis_auth_secret_arn
}

output "app_secret_arn" {
  description = "Secret holding JWT_SECRET and PROCESSOR_TOKEN as JSON."
  value       = module.platform.app_secret_arn
}

output "alarm_topic_arn" {
  description = "SNS topic every alarm in staging notifies."
  value       = module.platform.alarm_topic_arn
}

output "nat_public_ips" {
  description = "Addresses staging's outbound traffic comes from."
  value       = module.platform.nat_public_ips
}

output "load_balancer_controller_role_arn" {
  description = "IRSA role the AWS Load Balancer Controller runs as; scripts/eks-platform-install.sh reads it."
  value       = module.platform.load_balancer_controller_role_arn
}

output "cluster_autoscaler_role_arn" {
  description = "IRSA role the Cluster Autoscaler runs as; scripts/eks-platform-install.sh reads it."
  value       = module.platform.cluster_autoscaler_role_arn
}

output "vpc_id" {
  description = "The VPC; the load balancer controller is told it at install."
  value       = module.platform.vpc_id
}

output "public_subnet_cidrs" {
  description = "Where the load balancer's nodes sit. The overlay's NetworkPolicy admits exactly these ranges; keep the two in step."
  value       = module.platform.public_subnet_cidrs
}

output "ingress_certificate_arn" {
  description = "ACM certificate the load balancer serves, or null while no domain is set."
  value       = module.platform.ingress_certificate_arn
}
