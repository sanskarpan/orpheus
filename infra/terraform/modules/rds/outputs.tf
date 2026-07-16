output "endpoint" {
  description = "Connection endpoint (host:port)."
  value       = aws_db_instance.this.endpoint
}

output "address" {
  description = "DNS address of the instance."
  value       = aws_db_instance.this.address
}

output "port" {
  description = "Port the instance listens on."
  value       = aws_db_instance.this.port
}

output "database_name" {
  description = "Initial database name."
  value       = aws_db_instance.this.db_name
}

output "security_group_id" {
  description = "Security group protecting the instance."
  value       = aws_security_group.this.id
}

output "master_secret_arn" {
  description = "Secrets Manager ARN holding the managed master password."
  value       = try(aws_db_instance.this.master_user_secret[0].secret_arn, null)
}
