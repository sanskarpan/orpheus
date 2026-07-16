locals {
  name = "${var.name_prefix}-${var.environment}"

  common_tags = merge(
    {
      Project     = "orpheus"
      Environment = var.environment
    },
    var.tags,
  )
}

module "vpc" {
  source = "./modules/vpc"

  name               = local.name
  cidr               = var.vpc_cidr
  az_count           = var.az_count
  single_nat_gateway = var.single_nat_gateway
  cluster_name       = "${local.name}-eks"
  tags               = local.common_tags
}

module "eks" {
  source = "./modules/eks"

  cluster_name       = "${local.name}-eks"
  kubernetes_version = var.kubernetes_version
  vpc_id             = module.vpc.vpc_id
  subnet_ids         = module.vpc.private_subnet_ids

  node_instance_types = var.eks_node_instance_types
  node_min_size       = var.eks_node_min_size
  node_max_size       = var.eks_node_max_size
  node_desired_size   = var.eks_node_desired_size

  tags = local.common_tags
}

module "rds" {
  source = "./modules/rds"

  name                  = local.name
  vpc_id                = module.vpc.vpc_id
  subnet_ids            = module.vpc.database_subnet_ids
  allowed_cidr_blocks   = [var.vpc_cidr]
  instance_class        = var.rds_instance_class
  allocated_storage     = var.rds_allocated_storage
  max_allocated_storage = var.rds_max_allocated_storage
  multi_az              = var.rds_multi_az
  database_name         = var.database_name
  username              = var.database_username

  tags = local.common_tags
}

module "elasticache" {
  source = "./modules/elasticache"

  name                = local.name
  vpc_id              = module.vpc.vpc_id
  subnet_ids          = module.vpc.database_subnet_ids
  allowed_cidr_blocks = [var.vpc_cidr]
  node_type           = var.redis_node_type
  num_cache_nodes     = var.redis_num_cache_nodes

  tags = local.common_tags
}

module "s3" {
  source = "./modules/s3"

  name        = local.name
  bucket_name = "${local.name}-uploads"

  tags = local.common_tags
}
