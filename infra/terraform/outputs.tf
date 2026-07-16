output "vpc_id" {
  description = "ID of the VPC."
  value       = module.vpc.vpc_id
}

output "private_subnet_ids" {
  description = "Private subnet IDs (application/EKS nodes)."
  value       = module.vpc.private_subnet_ids
}

output "eks_cluster_name" {
  description = "Name of the EKS cluster."
  value       = module.eks.cluster_name
}

output "eks_cluster_endpoint" {
  description = "EKS API server endpoint."
  value       = module.eks.cluster_endpoint
}

output "eks_oidc_provider_arn" {
  description = "IAM OIDC provider ARN for IRSA."
  value       = module.eks.oidc_provider_arn
}

output "rds_endpoint" {
  description = "RDS Postgres connection endpoint (host:port)."
  value       = module.rds.endpoint
}

output "rds_secret_arn" {
  description = "Secrets Manager ARN holding the Postgres master credentials."
  value       = module.rds.master_secret_arn
}

output "redis_primary_endpoint" {
  description = "ElastiCache Redis primary endpoint."
  value       = module.elasticache.primary_endpoint_address
}

output "s3_bucket_name" {
  description = "Name of the uploads bucket."
  value       = module.s3.bucket_name
}

output "s3_bucket_arn" {
  description = "ARN of the uploads bucket."
  value       = module.s3.bucket_arn
}
