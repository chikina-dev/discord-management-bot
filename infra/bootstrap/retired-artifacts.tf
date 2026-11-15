# Preserve the existing archive while removing it from this configuration.
moved {
  from = aws_s3_bucket.bucket["artifacts"]
  to   = aws_s3_bucket.retired_artifacts
}
removed {
  from = aws_s3_bucket.retired_artifacts
  lifecycle { destroy = false }
}

moved {
  from = aws_s3_bucket_public_access_block.bucket["artifacts"]
  to   = aws_s3_bucket_public_access_block.retired_artifacts
}
removed {
  from = aws_s3_bucket_public_access_block.retired_artifacts
  lifecycle { destroy = false }
}

moved {
  from = aws_s3_bucket_server_side_encryption_configuration.bucket["artifacts"]
  to   = aws_s3_bucket_server_side_encryption_configuration.retired_artifacts
}
removed {
  from = aws_s3_bucket_server_side_encryption_configuration.retired_artifacts
  lifecycle { destroy = false }
}

moved {
  from = aws_s3_bucket_versioning.bucket["artifacts"]
  to   = aws_s3_bucket_versioning.retired_artifacts
}
removed {
  from = aws_s3_bucket_versioning.retired_artifacts
  lifecycle { destroy = false }
}

moved {
  from = aws_s3_bucket_policy.tls["artifacts"]
  to   = aws_s3_bucket_policy.retired_artifacts
}
removed {
  from = aws_s3_bucket_policy.retired_artifacts
  lifecycle { destroy = false }
}
