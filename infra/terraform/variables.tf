variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Deployment environment (dev, staging, prod)."
  type        = string
  default     = "dev"

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "name_prefix" {
  description = "Prefix applied to resource names."
  type        = string
  default     = "orpheus"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.0.0.0/16"
}

variable "az_count" {
  description = "Number of Availability Zones to spread subnets across."
  type        = number
  default     = 3
}

variable "single_nat_gateway" {
  description = "Use a single shared NAT gateway (cheaper, non-HA) instead of one per AZ."
  type        = bool
  default     = true
}

variable "kubernetes_version" {
  description = "EKS control plane Kubernetes version."
  type        = string
  default     = "1.30"
}

variable "eks_node_instance_types" {
  description = "Instance types for the default EKS managed node group."
  type        = list(string)
  default     = ["m6i.large"]
}

variable "eks_node_min_size" {
  description = "Minimum size of the EKS managed node group."
  type        = number
  default     = 2
}

variable "eks_node_max_size" {
  description = "Maximum size of the EKS managed node group."
  type        = number
  default     = 6
}

variable "eks_node_desired_size" {
  description = "Desired size of the EKS managed node group."
  type        = number
  default     = 3
}

variable "rds_instance_class" {
  description = "Instance class for the RDS Postgres primary."
  type        = string
  default     = "db.t4g.medium"
}

variable "rds_allocated_storage" {
  description = "Allocated storage (GiB) for RDS."
  type        = number
  default     = 50
}

variable "rds_max_allocated_storage" {
  description = "Upper bound (GiB) for RDS storage autoscaling."
  type        = number
  default     = 200
}

variable "rds_multi_az" {
  description = "Enable Multi-AZ for the RDS instance."
  type        = bool
  default     = false
}

variable "database_name" {
  description = "Initial Postgres database name."
  type        = string
  default     = "orpheus"
}

variable "database_username" {
  description = "Master username for Postgres."
  type        = string
  default     = "orpheus"
}

variable "redis_node_type" {
  description = "ElastiCache node type for Redis."
  type        = string
  default     = "cache.t4g.small"
}

variable "redis_num_cache_nodes" {
  description = "Number of Redis nodes (replicas) in the replication group."
  type        = number
  default     = 2
}

variable "tags" {
  description = "Additional tags merged onto all resources."
  type        = map(string)
  default     = {}
}
