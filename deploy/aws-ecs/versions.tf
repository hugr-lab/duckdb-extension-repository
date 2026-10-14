terraform {
  required_version = ">= 1.5"
  required_providers {
    aws  = { source = "hashicorp/aws", version = ">= 5.70, < 7" }
    http = { source = "hashicorp/http", version = ">= 3.4, < 4" }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = merge({ "managed-by" = "terraform", "app" = "kista", "prefix" = var.prefix }, var.tags)
  }
}
