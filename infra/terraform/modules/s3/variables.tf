variable "name" {
  description = "Name prefix for S3 resources."
  type        = string
}

variable "bucket_name" {
  description = "Name of the uploads bucket. Must be globally unique."
  type        = string
}

variable "force_destroy" {
  description = "Allow Terraform to delete a non-empty bucket."
  type        = bool
  default     = false
}

variable "noncurrent_version_expiration_days" {
  description = "Days after which noncurrent object versions are expired."
  type        = number
  default     = 30
}

variable "abort_incomplete_multipart_days" {
  description = "Days after which incomplete multipart uploads are aborted."
  type        = number
  default     = 7
}

variable "cors_allowed_origins" {
  description = "Origins permitted for browser multipart uploads (presigned PUT)."
  type        = list(string)
  default     = ["*"]
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
