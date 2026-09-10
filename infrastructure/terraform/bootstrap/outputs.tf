output "aws_region" {
  description = "Region of the state bucket and the registries."
  value       = var.aws_region
}

output "state_bucket_name" {
  description = "Bucket the environment stacks keep state in. Pass to terraform init as -backend-config=\"bucket=...\"."
  value       = aws_s3_bucket.state.id
}

output "state_kms_key_arn" {
  description = "KMS key encrypting the state bucket. Pass to terraform init as -backend-config=\"kms_key_id=...\"."
  value       = aws_kms_key.state.arn
}

output "access_log_bucket_name" {
  description = "Bucket holding the state and trail buckets' server access logs: who read, wrote or locked each environment's state, under state/."
  value       = aws_s3_bucket.access_logs.id
}

output "cloudtrail_bucket_name" {
  description = "Bucket holding the account's CloudTrail: every management API call, from every region."
  value       = aws_s3_bucket.cloudtrail.id
}

output "cloudtrail_arn" {
  description = "The trail."
  value       = aws_cloudtrail.this.arn
}

output "github_oidc_provider_arn" {
  description = "IAM OIDC provider for GitHub Actions. The environment stacks find it by URL; this is for reference."
  value       = aws_iam_openid_connect_provider.github.arn
}

output "terraform_plan_role_arn" {
  description = "Read-only role for terraform plan, assumable from pull requests and main."
  value       = aws_iam_role.plan.arn
}

output "ecr_push_role_arn" {
  description = "Role that pushes images, assumable from main only."
  value       = aws_iam_role.ecr_push.arn
}

output "ecr_repository_urls" {
  description = "Registry URL of each repository, keyed by repository name."
  value       = { for name, repo in aws_ecr_repository.this : name => repo.repository_url }
}
