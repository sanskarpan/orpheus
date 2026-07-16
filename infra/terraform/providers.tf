terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }

  # Remote state is expected to be configured per-environment via a backend
  # config file, e.g.:
  #   terraform init -backend-config=envs/dev.s3.tfbackend
  backend "s3" {}
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = "orpheus"
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}
