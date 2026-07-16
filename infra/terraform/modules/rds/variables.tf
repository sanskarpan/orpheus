variable "name" {
  description = "Name prefix for RDS resources."
  type        = string
}

variable "vpc_id" {
  description = "VPC to create the security group in."
  type        = string
}

variable "subnet_ids" {
  description = "Subnet IDs for the DB subnet group (database tier)."
  type        = list(string)
}

variable "allowed_cidr_blocks" {
  description = "CIDR blocks permitted to reach Postgres on 5432."
  type        = list(string)
}

variable "instance_class" {
  description = "RDS instance class."
  type        = string
  default     = "db.t4g.medium"
}

variable "engine_version" {
  description = "Postgres engine version."
  type        = string
  default     = "16.4"
}

variable "allocated_storage" {
  description = "Allocated storage in GiB."
  type        = number
  default     = 50
}

variable "max_allocated_storage" {
  description = "Upper bound for storage autoscaling in GiB."
  type        = number
  default     = 200
}

variable "multi_az" {
  description = "Enable Multi-AZ deployment."
  type        = bool
  default     = false
}

variable "database_name" {
  description = "Initial database name."
  type        = string
  default     = "orpheus"
}

variable "username" {
  description = "Master username."
  type        = string
  default     = "orpheus"
}

variable "backup_retention_days" {
  description = "Number of days to retain automated backups."
  type        = number
  default     = 7
}

variable "deletion_protection" {
  description = "Enable deletion protection on the instance."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
