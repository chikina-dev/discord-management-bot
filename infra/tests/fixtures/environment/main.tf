terraform {
  required_providers {
    dotenv = { source = "germanbrew/dotenv", version = "~> 1.0" }
  }
}
data "dotenv_sensitive" "input" {
  filename = "${path.module}/values.env"
}
output "entries" {
  value     = data.dotenv_sensitive.input.entries
  sensitive = true
}
