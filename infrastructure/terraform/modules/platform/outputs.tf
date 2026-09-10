# What the deployment pipeline and an operator need, and nothing secret: every
# credential is referenced by the ARN of the secret that holds it.

output "cluster_name" {
  description = "EKS cluster name, for aws eks update-kubeconfig."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = module.eks.cluster_endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64 CA certificate for the Kubernetes API."
  value       = module.eks.cluster_certificate_authority_data
}

output "cluster_oidc_provider_arn" {
  description = "IAM OIDC provider of the cluster, for any further IRSA roles."
  value       = module.eks.oidc_provider_arn
}

output "kubernetes_namespace" {
  description = "Namespace the application deploys into and the deploy role may manage."
  value       = local.namespace
}

output "deploy_role_arn" {
  description = "Role the CD job assumes through GitHub OIDC, from the matching GitHub environment only."
  value       = aws_iam_role.deploy.arn
}

output "vpc_id" {
  description = "VPC of this environment."
  value       = module.network.vpc_id
}

output "private_subnet_ids" {
  description = "Private subnets, where nodes and pods run."
  value       = module.network.private_subnet_ids
}

output "nat_public_ips" {
  description = "Addresses this environment's outbound traffic comes from."
  value       = module.network.nat_public_ips
}

output "database_address" {
  description = "PostgreSQL hostname, reachable only from inside the cluster."
  value       = module.rds.address
}

output "database_port" {
  description = "PostgreSQL port."
  value       = module.rds.port
}

output "database_name" {
  description = "Database name."
  value       = module.rds.database_name
}

output "database_master_username" {
  description = "Master user name. Its password is in database_master_secret_arn."
  value       = module.rds.master_username
}

output "database_instance_identifier" {
  description = "The RDS instance's identifier; the source for a point-in-time restore, and what a restore changes."
  value       = module.rds.instance_identifier
}

output "database_master_secret_arn" {
  description = "Secret holding the master password, generated and rotated by RDS. Combined with database_address to form DATABASE_URL (with sslmode=verify-full)."
  value       = module.rds.master_user_secret_arn
}

output "redis_primary_endpoint" {
  description = "Redis hostname, reachable only from inside the cluster. TLS is required, so REDIS_URL uses rediss://."
  value       = module.elasticache.primary_endpoint_address
}

output "redis_port" {
  description = "Redis port."
  value       = module.elasticache.port
}

output "redis_auth_secret_arn" {
  description = "Secret holding the Redis AUTH token."
  value       = module.elasticache.auth_secret_arn
}

output "app_secret_arn" {
  description = "Secret holding JWT_SECRET and PROCESSOR_TOKEN, as JSON."
  value       = aws_secretsmanager_secret.app.arn
}

output "alarm_topic_arn" {
  description = "SNS topic every alarm in this environment notifies."
  value       = aws_sns_topic.alarms.arn
}

output "data_kms_key_arn" {
  description = "KMS key protecting the database, cache, secrets and Kubernetes Secrets."
  value       = aws_kms_key.data.arn
}

output "logs_kms_key_arn" {
  description = "KMS key protecting log groups and the alarm topic."
  value       = aws_kms_key.logs.arn
}

output "load_balancer_controller_role_arn" {
  description = "IRSA role for the AWS Load Balancer Controller. infrastructure/kubernetes/platform installs the controller with it."
  value       = module.eks.load_balancer_controller_role_arn
}

output "cluster_autoscaler_role_arn" {
  description = "IRSA role for the Cluster Autoscaler. infrastructure/kubernetes/platform installs the autoscaler with it."
  value       = module.eks.cluster_autoscaler_role_arn
}

output "public_subnet_cidrs" {
  description = "Address ranges the load balancer's nodes sit in; the overlay's NetworkPolicy admits exactly these."
  value       = module.network.public_subnet_cidrs
}

output "ingress_certificate_arn" {
  description = "ACM certificate for ingress_domain_name, validated and ready for the load balancer, or null when no domain is set."
  value       = var.ingress_domain_name == null ? null : aws_acm_certificate_validation.ingress[0].certificate_arn
}
