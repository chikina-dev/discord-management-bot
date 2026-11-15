resource "random_bytes" "signing_seed" {
  length = 32
}
resource "random_bytes" "wireguard_server_key" {
  length = 32
}
resource "aws_ssm_parameter" "bot_token" {
  name  = "${local.parameter_prefix}/bot-token"
  type  = "SecureString"
  value = local.receiver_bot_token
  lifecycle {
    precondition {
      condition     = length(trimspace(local.receiver_bot_token)) > 0
      error_message = "DISCORD_BOT_TOKEN must be set in .env."
    }
  }
}
resource "aws_ssm_parameter" "guild_id" {
  name  = "${local.parameter_prefix}/guild-id"
  type  = "String"
  value = local.guild_id
  lifecycle {
    precondition {
      condition     = can(regex("^[0-9]{1,20}$", local.guild_id))
      error_message = "DISCORD_SERVER_ID must be a Discord snowflake ID in .env."
    }
  }
}
resource "aws_ssm_parameter" "signing_seed" {
  name  = "${local.parameter_prefix}/signing-seed"
  type  = "SecureString"
  value = replace(replace(replace(random_bytes.signing_seed.base64, "+", "-"), "/", "_"), "=", "")
}
resource "aws_ssm_parameter" "wireguard_server_key" {
  name  = "${local.parameter_prefix}/wireguard-server-key"
  type  = "SecureString"
  value = random_bytes.wireguard_server_key.base64
}
resource "aws_ssm_parameter" "wireguard_client_public_key" {
  name  = "${local.parameter_prefix}/wireguard-client-public-key"
  type  = "String"
  value = local.wireguard_client_public_key
  lifecycle {
    precondition {
      condition     = can(regex("^[A-Za-z0-9+/]{43}=$", local.wireguard_client_public_key))
      error_message = "PC_WIREGUARD_PUBLIC_KEY must be a Base64-encoded 32-byte public key in .env."
    }
  }
}
resource "aws_ssm_parameter" "discord_webhook_url" {
  name  = "${local.parameter_prefix}/discord-webhook-url"
  type  = "SecureString"
  value = local.discord_webhook_url
  lifecycle {
    precondition {
      condition     = can(regex("^https://(canary\\.|ptb\\.)?discord\\.com/api/webhooks/[0-9]+/[^/?#]+$", local.discord_webhook_url))
      error_message = "DISCORD_WEBHOOK_URL must be a Discord webhook URL in .env."
    }
  }
}
