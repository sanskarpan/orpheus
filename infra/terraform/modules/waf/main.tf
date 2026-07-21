# AWS WAFv2 Web ACL for the Orpheus API (Phase 5).
#
# Rate limiting, AWS-managed rule sets (common + known-bad-inputs + SQLi),
# and optional geo-blocking. Associate `web_acl_arn` with the ALB/API Gateway
# in front of the API. Regional scope (ALB); use scope="CLOUDFRONT" for edge.

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "name" {
  type    = string
  default = "orpheus-api"
}

variable "rate_limit_per_5min" {
  description = "Requests per source IP per 5 minutes before blocking."
  type        = number
  default     = 2000
}

variable "blocked_country_codes" {
  description = "ISO 3166-1 alpha-2 country codes to block (empty = no geo block)."
  type        = list(string)
  default     = []
}

resource "aws_wafv2_web_acl" "this" {
  name  = var.name
  scope = "REGIONAL"

  default_action {
    allow {}
  }

  # Per-IP rate limit.
  rule {
    name     = "rate-limit"
    priority = 1
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit              = var.rate_limit_per_5min
        aggregate_key_type = "IP"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name}-rate-limit"
      sampled_requests_enabled   = true
    }
  }

  # AWS managed common rule set.
  rule {
    name     = "aws-common"
    priority = 2
    override_action {
      none {}
    }
    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesCommonRuleSet"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name}-aws-common"
      sampled_requests_enabled   = true
    }
  }

  # AWS managed known-bad-inputs + SQLi.
  rule {
    name     = "aws-bad-inputs"
    priority = 3
    override_action {
      none {}
    }
    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name}-aws-bad-inputs"
      sampled_requests_enabled   = true
    }
  }

  # Optional geo-block.
  dynamic "rule" {
    for_each = length(var.blocked_country_codes) > 0 ? [1] : []
    content {
      name     = "geo-block"
      priority = 4
      action {
        block {}
      }
      statement {
        geo_match_statement {
          country_codes = var.blocked_country_codes
        }
      }
      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = "${var.name}-geo-block"
        sampled_requests_enabled   = true
      }
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = var.name
    sampled_requests_enabled   = true
  }
}

output "web_acl_arn" {
  value = aws_wafv2_web_acl.this.arn
}
