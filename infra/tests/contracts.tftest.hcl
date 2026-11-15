mock_provider "aws" {
  override_during = plan
}

override_resource {
  target          = aws_ssm_parameter.bot_token
  override_during = plan
  values          = { arn = "arn:aws:ssm:ap-northeast-1:435684131663:parameter/discord-management-bot/bot-token" }
}
override_resource {
  target          = aws_ssm_parameter.guild_id
  override_during = plan
  values          = { arn = "arn:aws:ssm:ap-northeast-1:435684131663:parameter/discord-management-bot/guild-id" }
}
override_resource {
  target          = aws_ssm_parameter.signing_seed
  override_during = plan
  values          = { arn = "arn:aws:ssm:ap-northeast-1:435684131663:parameter/discord-management-bot/signing-seed" }
}

override_resource {
  target          = aws_s3_bucket.backup
  override_during = plan
  values          = { arn = "arn:aws:s3:::fixture-backups" }
}
override_resource {
  target          = aws_cloudwatch_log_group.gateway
  override_during = plan
  values          = { arn = "arn:aws:logs:ap-northeast-1:435684131663:log-group:/discord-management-bot/gateway" }
}

mock_provider "random" {
  override_during = plan
  mock_resource "random_bytes" {
    override_during = plan
    defaults = {
      base64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
      hex    = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
    }
  }
}
mock_provider "dotenv" {
  override_during = plan
  mock_data "dotenv_sensitive" {
    defaults = {
      entries = {
        DISCORD_BOT_TOKEN       = "fixture-bot-token"
        DISCORD_SERVER_ID       = "123456789012345678"
        DISCORD_WEBHOOK_URL     = "https://discord.com/api/webhooks/123456789012345678/fixture-token"
        PC_WIREGUARD_PUBLIC_KEY = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
      }
    }
  }
}

run "infrastructure_contract" {
  command = plan

  assert {
    condition     = aws_instance.gateway.instance_type == "t4g.nano" && aws_instance.gateway.credit_specification[0].cpu_credits == "standard"
    error_message = "Use nano with bounded CPU-credit charges."
  }
  assert {
    condition     = aws_subnet.public.map_public_ip_on_launch == true
    error_message = "The public subnet owns automatic IPv4 assignment at launch."
  }
  assert {
    condition     = toset([for rule in aws_vpc_security_group_ingress_rule.public : rule.from_port]) == toset([80, 443, 51820])
    error_message = "Only HTTPS callbacks, ACME and WireGuard may be reachable publicly."
  }
  assert {
    condition     = aws_ebs_volume.data.encrypted == true && aws_cloudwatch_log_group.gateway.retention_in_days == 365
    error_message = "Persistent data must be encrypted and logs kept for one year."
  }
  assert {
    condition     = alltrue([for rule in aws_cloudwatch_event_rule.notify : rule.state == "ENABLED"])
    error_message = "Configured notification routes must be enabled."
  }
  assert {
    condition     = aws_cloudwatch_metric_alarm.app["delivery_backlog"].metric_name == "OldestPendingSeconds" && !contains(keys(local.metric_alarms), "WorkerFailure")
    error_message = "Worker business outcomes must not create infrastructure alarms."
  }
  assert {
    condition     = one([for statement in jsondecode(aws_iam_role_policy.gateway.policy).Statement : statement if statement.Sid == "UpdateOwnIPv4"]).Condition["ForAllValues:StringEquals"]["route53:ChangeResourceRecordSetsNormalizedRecordNames"] == [local.hostname]
    error_message = "DNS changes must be restricted to the gateway hostname."
  }
}


run "managed_parameters" {
  command = plan
  assert {
    condition     = aws_ssm_parameter.bot_token.type == "SecureString" && aws_ssm_parameter.signing_seed.type == "SecureString" && aws_ssm_parameter.wireguard_server_key.type == "SecureString" && aws_ssm_parameter.discord_webhook_url.type == "SecureString"
    error_message = "Terraform must own each runtime secret as a SecureString."
  }
  assert {
    condition     = aws_ssm_parameter.bot_token.value == "fixture-bot-token" && aws_ssm_parameter.guild_id.value == "123456789012345678" && aws_ssm_parameter.wireguard_client_public_key.value == "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" && aws_ssm_parameter.discord_webhook_url.value == "https://discord.com/api/webhooks/123456789012345678/fixture-token"
    error_message = "SSM resources must use the existing Bot and PC identities supplied to Terraform."
  }
  assert {
    condition     = random_bytes.signing_seed.length == 32 && random_bytes.wireguard_server_key.length == 32 && aws_ssm_parameter.signing_seed.value == "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" && aws_ssm_parameter.wireguard_server_key.value == random_bytes.wireguard_server_key.base64
    error_message = "Generate stable 32-byte keys and preserve the signing seed's Base64URL encoding."
  }
  assert {
    condition     = aws_cloudwatch_event_api_destination.discord.invocation_endpoint == aws_ssm_parameter.discord_webhook_url.value && length(aws_cloudwatch_event_rule.notify) == 2
    error_message = "Alert and recovery routes must use the Terraform-managed webhook."
  }
  assert {
    condition     = one([for statement in jsondecode(aws_iam_role_policy.gateway.policy).Statement : statement if statement.Sid == "ReadRuntimeParameters"]).Resource == [aws_ssm_parameter.bot_token.arn, aws_ssm_parameter.guild_id.arn, aws_ssm_parameter.signing_seed.arn]
    error_message = "The Go runtime must not receive access to WireGuard or notification secrets."
  }
}

run "missing_environment_values" {
  command = plan
  override_data {
    target = data.dotenv_sensitive.input
    values = { entries = {} }
  }
  expect_failures = [
    aws_ssm_parameter.bot_token,
    aws_ssm_parameter.guild_id,
    aws_ssm_parameter.discord_webhook_url,
    aws_ssm_parameter.wireguard_client_public_key
  ]
}

run "canary_webhook_input" {
  command = plan
  override_data {
    target = data.dotenv_sensitive.input
    values = {
      entries = {
        DISCORD_BOT_TOKEN       = "fixture-bot-token"
        DISCORD_SERVER_ID       = "123456789012345678"
        DISCORD_WEBHOOK_URL     = "https://canary.discord.com/api/webhooks/123456789012345678/fixture-token"
        PC_WIREGUARD_PUBLIC_KEY = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
      }
    }
  }
  assert {
    condition     = aws_cloudwatch_event_api_destination.discord.invocation_endpoint == "https://canary.discord.com/api/webhooks/123456789012345678/fixture-token"
    error_message = "The supplied Canary webhook must be preserved without rewriting its hostname."
  }
}
