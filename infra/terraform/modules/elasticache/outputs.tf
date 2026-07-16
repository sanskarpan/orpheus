output "primary_endpoint_address" {
  description = "Primary endpoint address for read/write."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "reader_endpoint_address" {
  description = "Reader endpoint address for replicas."
  value       = aws_elasticache_replication_group.this.reader_endpoint_address
}

output "port" {
  description = "Port Redis listens on."
  value       = aws_elasticache_replication_group.this.port
}

output "security_group_id" {
  description = "Security group protecting the cluster."
  value       = aws_security_group.this.id
}
