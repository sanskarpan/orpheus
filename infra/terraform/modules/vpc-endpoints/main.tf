# VPC endpoints (Phase 5): keep AWS API traffic on the private network.
#
# A gateway endpoint for S3 (no NAT cost, no internet path) and interface
# endpoints for ECR (pull images), Secrets Manager (External Secrets), and
# CloudWatch Logs. This removes the dependency on a NAT gateway / internet
# egress for the control-plane's AWS calls.

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "vpc_id" {
  type = string
}

variable "region" {
  type = string
}

variable "private_route_table_ids" {
  description = "Route tables to attach the S3 gateway endpoint to."
  type        = list(string)
  default     = []
}

variable "private_subnet_ids" {
  description = "Subnets for the interface endpoints."
  type        = list(string)
  default     = []
}

variable "endpoint_security_group_ids" {
  type    = list(string)
  default = []
}

# S3 — gateway endpoint (free; routes via route tables).
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = var.vpc_id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = var.private_route_table_ids
}

# Interface endpoints (ENIs in the private subnets).
locals {
  interface_services = [
    "ecr.api",
    "ecr.dkr",
    "secretsmanager",
    "logs",
  ]
}

resource "aws_vpc_endpoint" "interface" {
  for_each = toset(local.interface_services)

  vpc_id              = var.vpc_id
  service_name        = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.private_subnet_ids
  security_group_ids  = var.endpoint_security_group_ids
  private_dns_enabled = true
}

output "s3_endpoint_id" {
  value = aws_vpc_endpoint.s3.id
}

output "interface_endpoint_ids" {
  value = { for k, v in aws_vpc_endpoint.interface : k => v.id }
}
