output "replication_group_id" {
  description = "ID of the replication group."
  value       = aws_elasticache_replication_group.this.id
}

output "primary_endpoint_address" {
  description = "Hostname for writes. Resolves only inside the VPC; connect with rediss:// (TLS is required)."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "reader_endpoint_address" {
  description = "Hostname spreading reads across replicas. Same as the primary on a single-node group."
  value       = aws_elasticache_replication_group.this.reader_endpoint_address
}

output "port" {
  description = "Port Redis listens on."
  value       = aws_elasticache_replication_group.this.port
}

output "auth_secret_arn" {
  description = "Secrets Manager secret holding the AUTH token. Its value is the token and nothing else."
  value       = aws_secretsmanager_secret.auth.arn
}

output "security_group_id" {
  description = "Security group guarding the cache."
  value       = aws_security_group.this.id
}
