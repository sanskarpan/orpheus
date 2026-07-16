variable "cluster_name" {
  description = "Name of the EKS cluster."
  type        = string
}

variable "kubernetes_version" {
  description = "Kubernetes version for the control plane."
  type        = string
  default     = "1.30"
}

variable "vpc_id" {
  description = "VPC to deploy the cluster into."
  type        = string
}

variable "subnet_ids" {
  description = "Private subnet IDs for the control plane ENIs and node group."
  type        = list(string)
}

variable "node_instance_types" {
  description = "Instance types for the managed node group."
  type        = list(string)
  default     = ["m6i.large"]
}

variable "node_min_size" {
  description = "Minimum node count."
  type        = number
  default     = 2
}

variable "node_max_size" {
  description = "Maximum node count."
  type        = number
  default     = 6
}

variable "node_desired_size" {
  description = "Desired node count."
  type        = number
  default     = 3
}

variable "node_disk_size" {
  description = "Root EBS volume size (GiB) per node."
  type        = number
  default     = 50
}

variable "endpoint_public_access" {
  description = "Whether the EKS API server is reachable from the public internet."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
