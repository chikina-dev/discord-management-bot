terraform {
  required_version = ">= 1.14.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}
provider "aws" {
  region              = local.region
  profile             = local.profile
  allowed_account_ids = [local.account_id]
  default_tags { tags = { Project = "discord-management-bot", ManagedBy = "Terraform" } }
}
locals {
  region     = "ap-northeast-1"
  profile    = "default"
  account_id = "435684131663"

  buckets = {
    state     = "discord-gateway-state-${local.account_id}-${local.region}"
    transport = "discord-gateway-ansible-${local.account_id}-${local.region}"
  }
}
resource "aws_s3_bucket" "bucket" {
  for_each = local.buckets
  bucket   = each.value
}
resource "aws_s3_bucket_public_access_block" "bucket" {
  for_each                = aws_s3_bucket.bucket
  bucket                  = each.value.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
resource "aws_s3_bucket_server_side_encryption_configuration" "bucket" {
  for_each = aws_s3_bucket.bucket
  bucket   = each.value.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
  }
}
resource "aws_s3_bucket_versioning" "bucket" {
  for_each = { for name, bucket in aws_s3_bucket.bucket : name => bucket if name != "transport" }
  bucket   = each.value.id
  versioning_configuration { status = "Enabled" }
}
resource "aws_s3_bucket_policy" "tls" {
  for_each = aws_s3_bucket.bucket
  bucket   = each.value.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "RequireTLS", Effect = "Deny", Principal = "*", Action = "s3:*"
      Resource  = [each.value.arn, "${each.value.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}
output "state_bucket" { value = aws_s3_bucket.bucket["state"].id }


resource "aws_s3_bucket_lifecycle_configuration" "transport" {
  bucket = aws_s3_bucket.bucket["transport"].id
  rule {
    id     = "expire-abandoned-transfers"
    status = "Enabled"
    filter {}
    expiration { days = 1 }
    abort_incomplete_multipart_upload { days_after_initiation = 1 }
  }
}
output "ansible_bucket" { value = aws_s3_bucket.bucket["transport"].id }
