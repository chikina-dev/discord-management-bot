mock_provider "aws" {}
mock_provider "random" {}

run "read_standard_dotenv_file" {
  command = plan
  module {
    source = "./tests/fixtures/environment"
  }
  assert {
    condition = output.entries == tomap({
      DISCORD_BOT_TOKEN       = "fixture-token"
      DISCORD_SERVER_ID       = "123456789012345678"
      DISCORD_WEBHOOK_URL     = "https://discord.com/api/webhooks/123456789012345678/fixture=token"
      PC_WIREGUARD_PUBLIC_KEY = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
    })
    error_message = "Dotenv loading must preserve quoted values, export syntax, comments, URLs and Base64 padding."
  }
}
