# The recipe's plan and its refusals, with mock providers: no cloud and no credentials (CI).

mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = { names = ["eu-central-1a", "eu-central-1b", "eu-central-1c"] }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{}" }
  }
}

mock_provider "http" {
  mock_data "http" {
    defaults = { status_code = 200, response_body = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n" }
  }
}

mock_provider "random" {}

variables {
  domain          = "kista.example.com"
  image           = "ghcr.io/hugr-lab/kista:v0.1.0"
  certificate_arn = "arn:aws:acm:eu-central-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
}

run "own_vpc" {
  command = plan
  assert {
    condition     = length(aws_subnet.private) == 2 && length(aws_nat_gateway.nat) == 1
    error_message = "a VPC of its own: two private subnets and a NAT gateway"
  }
  assert {
    condition     = aws_lb.lb.idle_timeout == 60 && aws_lb.lb.drop_invalid_header_fields
    error_message = "the load balancer: idle below kista's keep-alive, invalid headers dropped"
  }
  assert {
    condition     = aws_kms_key.channel.customer_master_key_spec == "RSA_2048" && aws_kms_key.channel.key_usage == "SIGN_VERIFY"
    error_message = "the channel key"
  }
  assert {
    condition     = aws_kms_key.channel.tags["kista-deployment"] == "kista"
    error_message = "the channel key names its deployment"
  }
  assert {
    condition     = aws_db_instance.db.iam_database_authentication_enabled && !aws_db_instance.db.publicly_accessible
    error_message = "the database: IAM authentication, private"
  }
  assert {
    condition     = !contains(keys(local.auth), "server_license_admins")
    error_message = "no licence managers: auth.server_license_admins left out of the configuration"
  }
}

run "licence_managers" {
  command = plan
  variables {
    server_admins         = "['role:corp|kista-admin']"
    server_license_admins = "['role:corp|kista-licenses']"
  }
  assert {
    condition     = local.auth.server_license_admins == ["role:corp|kista-licenses"] && local.auth.server_admins == ["role:corp|kista-admin"]
    error_message = "auth.server_license_admins beside auth.server_admins"
  }
}

run "endpoints_in_a_given_vpc" {
  command = plan
  variables {
    vpc_id                  = "vpc-0123456789abcdef0"
    private_subnet_ids      = ["subnet-0000000000000000a", "subnet-0000000000000000b"]
    public_subnet_ids       = ["subnet-0000000000000000c", "subnet-0000000000000000d"]
    private_route_table_ids = ["rtb-0000000000000000a"]
    vpc_endpoints           = true
  }
  assert {
    condition     = length(aws_vpc.vpc) == 0 && length(aws_vpc_endpoint.s3) == 1 && length(aws_vpc_endpoint.interface) == 3
    error_message = "a given VPC: no VPC made; the S3 and interface endpoints"
  }
  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.endpoints) == 2
    error_message = "the endpoints admit the tasks and db-init"
  }
}

run "a_bad_prefix" {
  command = plan
  variables {
    prefix = "Kista_1"
  }
  expect_failures = [var.prefix]
}

run "a_moving_image" {
  command = plan
  variables {
    image = "ghcr.io/hugr-lab/kista:edge"
  }
  expect_failures = [var.image]
}

run "edge_by_digest" {
  command = plan
  variables {
    image = "ghcr.io/hugr-lab/kista@sha256:0b5019f0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
}

run "a_spool_too_large" {
  command = plan
  variables {
    max_body_mib = 4096
    max_ingests  = 4
  }
  expect_failures = [aws_ecs_task_definition.service]
}

run "a_given_vpc_without_subnets" {
  command = plan
  variables {
    vpc_id = "vpc-0123456789abcdef0"
  }
  expect_failures = [aws_db_subnet_group.db]
}

run "a_short_key_window" {
  command = plan
  variables {
    key_deletion_days = 3
  }
  expect_failures = [var.key_deletion_days]
}

run "distribution_admins" {
  command = plan
  variables {
    server_admins              = "['role:corp|kista-admin']"
    server_distribution_admins = "['client:corp|enterest']"
  }
  assert {
    condition     = local.auth.server_distribution_admins == ["client:corp|enterest"] && !contains(keys(local.auth), "server_license_admins")
    error_message = "auth.server_distribution_admins, and no licence managers"
  }
}
