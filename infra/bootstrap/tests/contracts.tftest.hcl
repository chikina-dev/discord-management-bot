mock_provider "aws" {
  override_during = plan
}
run "only_current_bootstrap_resources" {
  command = plan
  assert {
    condition     = toset(keys(aws_s3_bucket.bucket)) == toset(["state", "transport"])
    error_message = "Bootstrap must not provision the unused release archive."
  }
  assert {
    condition     = toset(keys(aws_s3_bucket_versioning.bucket)) == toset(["state"])
    error_message = "Only state uses versioning; transfer payloads must not persist as versions."
  }
  assert {
    condition     = alltrue([for access in aws_s3_bucket_public_access_block.bucket : access.block_public_acls && access.block_public_policy && access.ignore_public_acls && access.restrict_public_buckets])
    error_message = "The remaining bootstrap buckets must be private."
  }
}
