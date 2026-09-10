output "vpc_id" {
  description = "ID of the VPC."
  value       = aws_vpc.this.id
}

output "vpc_cidr_block" {
  description = "IPv4 range of the VPC."
  value       = aws_vpc.this.cidr_block
}

output "availability_zones" {
  description = "Zones the subnets were placed in, in subnet order."
  value       = local.azs
}

output "public_subnet_ids" {
  description = "Public subnets, one per zone. For internet-facing load balancers and NAT only."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnets, one per zone. Where EKS nodes and pods run."
  value       = aws_subnet.private[*].id
}

output "database_subnet_ids" {
  description = "Database subnets, one per zone, with no route out of the VPC. For RDS and ElastiCache."
  value       = aws_subnet.database[*].id
}

output "nat_public_ips" {
  description = "Public addresses that outbound traffic from the private subnets appears to come from. Useful when a third party must allow-list this environment."
  value       = aws_eip.nat[*].public_ip
}

output "flow_log_group_name" {
  description = "CloudWatch log group holding VPC flow logs, or null when flow logs are off."
  value       = var.enable_flow_logs ? aws_cloudwatch_log_group.flow_logs[0].name : null
}

output "public_subnet_cidrs" {
  description = "Address ranges of the public subnets, in zone order. Load balancers created by the controller sit here, so a NetworkPolicy that admits them names these ranges."
  value       = aws_subnet.public[*].cidr_block
}

output "vpc_endpoint_security_group_id" {
  description = "Security group on the interface endpoints, or null when there are none."
  value       = var.enable_ecr_endpoints ? aws_security_group.endpoints[0].id : null
}
