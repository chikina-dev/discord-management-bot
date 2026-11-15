resource "aws_iam_role" "gateway" {
  name = local.project
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}
resource "aws_iam_instance_profile" "gateway" {
  name = local.project
  role = aws_iam_role.gateway.name
}
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.gateway.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
resource "aws_iam_role_policy" "gateway" {
  role = aws_iam_role.gateway.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid = "ReadRuntimeParameters", Effect = "Allow", Action = ["ssm:GetParameter"]
        Resource = [
          aws_ssm_parameter.bot_token.arn,
          aws_ssm_parameter.guild_id.arn,
          aws_ssm_parameter.signing_seed.arn
        ]
      },
      {
        Sid      = "WriteBackups", Effect = "Allow", Action = ["s3:PutObject", "s3:AbortMultipartUpload"]
        Resource = ["${aws_s3_bucket.backup.arn}/sqlite/*"]
      },
      {
        Sid      = "ReadDNSRecord", Effect = "Allow", Action = ["route53:ListResourceRecordSets"]
        Resource = ["arn:aws:route53:::hostedzone/${local.hosted_zone_id}"]
      },
      {
        Sid      = "UpdateOwnIPv4", Effect = "Allow", Action = ["route53:ChangeResourceRecordSets"]
        Resource = ["arn:aws:route53:::hostedzone/${local.hosted_zone_id}"]
        Condition = { "ForAllValues:StringEquals" = {
          "route53:ChangeResourceRecordSetsNormalizedRecordNames" = [local.hostname],
          "route53:ChangeResourceRecordSetsRecordTypes"           = ["A"],
          "route53:ChangeResourceRecordSetsActions"               = ["UPSERT"]
        } }
      },
      {
        Sid      = "PublishLogs", Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
        Resource = ["${aws_cloudwatch_log_group.gateway.arn}:*"]
      },
      {
        Sid       = "PublishHostMetrics", Effect = "Allow", Action = ["cloudwatch:PutMetricData"], Resource = "*"
        Condition = { StringEquals = { "cloudwatch:namespace" = "${local.metrics_namespace}Host" } }
      }
    ]
  })
}
