output "address" {
  description = "Hostname of the instance. Resolves only inside the VPC."
  value       = aws_db_instance.this.address
}

output "port" {
  description = "Port PostgreSQL listens on."
  value       = aws_db_instance.this.port
}

output "instance_identifier" {
  description = "The instance's own identifier: identifier plus instance_suffix. A restore changes it."
  value       = aws_db_instance.this.identifier
}

output "identifier" {
  description = "RDS instance identifier."
  value       = aws_db_instance.this.identifier
}

output "arn" {
  description = "ARN of the RDS instance."
  value       = aws_db_instance.this.arn
}

output "database_name" {
  description = "Name of the database created on the instance."
  value       = aws_db_instance.this.db_name
}

output "master_username" {
  description = "Master user name. The password is in the secret named by master_user_secret_arn."
  value       = aws_db_instance.this.username
}

output "master_user_secret_arn" {
  description = "Secrets Manager secret holding the master password, created and rotated by RDS."
  value       = aws_db_instance.this.master_user_secret[0].secret_arn
}

output "security_group_id" {
  description = "Security group guarding the instance."
  value       = aws_security_group.this.id
}
