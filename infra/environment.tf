data "dotenv_sensitive" "input" {
  filename = abspath("${path.module}/../.env")
}

locals {
  receiver_bot_token          = try(data.dotenv_sensitive.input.entries.DISCORD_BOT_TOKEN, "")
  guild_id                    = try(data.dotenv_sensitive.input.entries.DISCORD_SERVER_ID, "")
  discord_webhook_url         = try(data.dotenv_sensitive.input.entries.DISCORD_WEBHOOK_URL, "")
  wireguard_client_public_key = try(data.dotenv_sensitive.input.entries.PC_WIREGUARD_PUBLIC_KEY, "")
}
