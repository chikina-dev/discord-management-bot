terraform {
  required_version = ">= 1.14.0"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 6.0" }
    random = { source = "hashicorp/random", version = "~> 3.7" }
    dotenv = { source = "germanbrew/dotenv", version = "~> 1.0" }
  }
  backend "s3" {}
}
provider "aws" {
  region              = local.region
  profile             = local.profile
  allowed_account_ids = [local.account_id]
  default_tags { tags = { Project = local.project, ManagedBy = "Terraform" } }
}
