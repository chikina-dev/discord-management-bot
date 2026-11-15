locals {
  region               = "ap-northeast-1"
  profile              = "default"
  account_id           = "435684131663"
  project              = "discord-management-bot"
  hosted_zone_id       = "Z0673595TLUIY67DPP3F"
  hostname             = "discord-gateway.chigayuki.com"
  parameter_prefix     = "/discord-management-bot"
  metrics_namespace    = "DiscordManagementGateway"
  admin_listen_address = "10.203.0.1:8080"

  zone      = "ap-northeast-1a"
  log_group = "/discord-management-bot/gateway"
}
resource "aws_vpc" "gateway" {
  cidr_block           = "10.224.0.0/24"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = local.project }
}
resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.gateway.id
  cidr_block              = "10.224.0.0/26"
  availability_zone       = local.zone
  map_public_ip_on_launch = true
  tags                    = { Name = "${local.project}-public" }
}
resource "aws_internet_gateway" "gateway" { vpc_id = aws_vpc.gateway.id }
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.gateway.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.gateway.id
  }
}
resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}
resource "aws_security_group" "gateway" {
  name        = local.project
  description = "HTTPS callbacks, ACME and authenticated WireGuard only"
  vpc_id      = aws_vpc.gateway.id
}
resource "aws_vpc_security_group_ingress_rule" "public" {
  for_each = {
    https     = { protocol = "tcp", port = 443 }
    acme      = { protocol = "tcp", port = 80 }
    wireguard = { protocol = "udp", port = 51820 }
  }
  security_group_id = aws_security_group.gateway.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = each.value.protocol
  from_port         = each.value.port
  to_port           = each.value.port
}
resource "aws_vpc_security_group_egress_rule" "outbound" {
  security_group_id = aws_security_group.gateway.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
resource "aws_ebs_volume" "data" {
  availability_zone = local.zone
  type              = "gp3"
  size              = 8
  encrypted         = true
  tags              = { Name = "${local.project}-data" }
  lifecycle { prevent_destroy = true }
}
resource "aws_cloudwatch_log_group" "gateway" {
  name              = local.log_group
  retention_in_days = 365
}
resource "aws_s3_bucket" "backup" {
  bucket = "discord-gateway-backup-${local.account_id}-${local.region}"
}
resource "aws_s3_bucket_public_access_block" "backup" {
  bucket                  = aws_s3_bucket.backup.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
resource "aws_s3_bucket_server_side_encryption_configuration" "backup" {
  bucket = aws_s3_bucket.backup.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
  }
}
resource "aws_s3_bucket_lifecycle_configuration" "backup" {
  bucket = aws_s3_bucket.backup.id
  rule {
    id     = "expire-sqlite-backups"
    status = "Enabled"
    filter { prefix = "sqlite/" }
    expiration { days = 30 }
    abort_incomplete_multipart_upload { days_after_initiation = 1 }
  }
}
resource "aws_s3_bucket_policy" "backup" {
  bucket = aws_s3_bucket.backup.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "RequireTLS", Effect = "Deny", Principal = "*", Action = "s3:*"
      Resource  = [aws_s3_bucket.backup.arn, "${aws_s3_bucket.backup.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}
resource "aws_instance" "gateway" {
  ami                    = "ami-0c109265f4db26c9c" # Amazon Linux 2023 ARM64; update explicitly when rebuilding.
  instance_type          = "t4g.nano"
  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.gateway.id]
  iam_instance_profile   = aws_iam_instance_profile.gateway.name
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  credit_specification { cpu_credits = "standard" }
  root_block_device {
    volume_type = "gp3"
    volume_size = 8
    encrypted   = true
  }
  tags       = { Name = local.project }
  depends_on = [aws_route_table_association.public, aws_iam_role_policy.gateway]
}
resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.gateway.id
}
resource "aws_route53_record" "gateway" {
  zone_id = local.hosted_zone_id
  name    = local.hostname
  type    = "A"
  ttl     = 60
  records = [aws_instance.gateway.public_ip]
  lifecycle { ignore_changes = [records] }
}
output "instance_id" { value = aws_instance.gateway.id }
output "public_ip" { value = aws_instance.gateway.public_ip }
output "public_url" { value = "https://${local.hostname}" }
output "admin_url" { value = "http://${local.admin_listen_address}" }
output "data_volume_id" { value = aws_ebs_volume.data.id }
output "backup_bucket" { value = aws_s3_bucket.backup.id }
