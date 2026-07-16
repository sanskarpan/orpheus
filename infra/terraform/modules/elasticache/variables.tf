variable "name" {
  description = "Name prefix for ElastiCache resources."
  type        = string
}

variable "vpc_id" {
  description = "VPC to create the security group in."
  type        = string
}

variable "subnet_ids" {
  description = "Subnet IDs for the cache subnet group (database tier)."
  type        = list(string)
}

variable "allowed_cidr_blocks" {
  description = "CIDR blocks permitted to reach Redis on 6379."
  type        = list(string)
}

variable "node_type" {
  description = "ElastiCache node type."
  type        = string
  default     = "cache.t4g.small"
}

variable "engine_version" {
  description = "Redis engine version."
  type        = string
  default     = "7.1"
}

variable "num_cache_nodes" {
  description = "Number of nodes in the replication group (1 primary + N-1 replicas)."
  type        = number
  default     = 2
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
