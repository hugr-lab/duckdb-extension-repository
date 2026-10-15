# kista on ECS Fargate (spec 0020 phase 1): its task role the only identity (no key), signing with
# an AWS KMS key, bodies in S3, the store on RDS PostgreSQL by IAM authentication over
# verify-full, behind an Application Load Balancer that forwards http and https without a redirect.

data "aws_caller_identity" "this" {}
data "aws_partition" "this" {}
data "aws_availability_zones" "up" {
  state = "available"
}

locals {
  name      = var.prefix
  account   = data.aws_caller_identity.this.account_id
  partition = data.aws_partition.this.partition
  own_vpc   = var.vpc_id == null
  azs       = slice(data.aws_availability_zones.up.names, 0, 2)
}

# ---- the network: a VPC of the recipe's own, unless vpc_id names one

resource "aws_vpc" "vpc" {
  count                = local.own_vpc ? 1 : 0
  cidr_block           = "10.40.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = local.name }
}

resource "aws_subnet" "public" {
  count                   = local.own_vpc ? 2 : 0
  vpc_id                  = aws_vpc.vpc[0].id
  availability_zone       = local.azs[count.index]
  cidr_block              = cidrsubnet("10.40.0.0/16", 8, count.index)
  map_public_ip_on_launch = false
  tags                    = { Name = "${local.name}-public-${count.index}" }
}

resource "aws_subnet" "private" {
  count             = local.own_vpc ? 2 : 0
  vpc_id            = aws_vpc.vpc[0].id
  availability_zone = local.azs[count.index]
  cidr_block        = cidrsubnet("10.40.0.0/16", 8, 10 + count.index)
  tags              = { Name = "${local.name}-private-${count.index}" }
}

resource "aws_internet_gateway" "igw" {
  count  = local.own_vpc ? 1 : 0
  vpc_id = aws_vpc.vpc[0].id
}

resource "aws_eip" "nat" {
  count  = local.own_vpc ? 1 : 0
  domain = "vpc"
}

# one NAT gateway, a trial's: production has one per AZ
resource "aws_nat_gateway" "nat" {
  count         = local.own_vpc ? 1 : 0
  allocation_id = aws_eip.nat[0].id
  subnet_id     = aws_subnet.public[0].id
  depends_on    = [aws_internet_gateway.igw]
}

resource "aws_route_table" "public" {
  count  = local.own_vpc ? 1 : 0
  vpc_id = aws_vpc.vpc[0].id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw[0].id
  }
}

resource "aws_route_table" "private" {
  count  = local.own_vpc ? 1 : 0
  vpc_id = aws_vpc.vpc[0].id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.nat[0].id
  }
}

resource "aws_route_table_association" "public" {
  count          = local.own_vpc ? 2 : 0
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public[0].id
}

resource "aws_route_table_association" "private" {
  count          = local.own_vpc ? 2 : 0
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[0].id
}

locals {
  vpc_id             = local.own_vpc ? aws_vpc.vpc[0].id : var.vpc_id
  private_subnet_ids = local.own_vpc ? aws_subnet.private[*].id : var.private_subnet_ids
  public_subnet_ids  = local.own_vpc ? aws_subnet.public[*].id : var.public_subnet_ids
}

data "aws_subnet" "public" {
  count = length(local.public_subnet_ids)
  id    = local.public_subnet_ids[count.index]
}

# ---- security groups: the load balancer is the only way in

resource "aws_security_group" "lb" {
  name        = "${local.name}-lb"
  description = "kista load balancer"
  vpc_id      = local.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "lb" {
  for_each          = toset(["80", "443"])
  security_group_id = aws_security_group.lb.id
  ip_protocol       = "tcp"
  from_port         = tonumber(each.value)
  to_port           = tonumber(each.value)
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "lb" {
  for_each                     = toset(["8443", "8081"])
  security_group_id            = aws_security_group.lb.id
  ip_protocol                  = "tcp"
  from_port                    = tonumber(each.value)
  to_port                      = tonumber(each.value)
  referenced_security_group_id = aws_security_group.task.id
}

resource "aws_security_group" "task" {
  name        = "${local.name}-task"
  description = "kista tasks: the load balancer on 8443 and 8081"
  vpc_id      = local.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "task" {
  for_each                     = toset(["8443", "8081"])
  security_group_id            = aws_security_group.task.id
  ip_protocol                  = "tcp"
  from_port                    = tonumber(each.value)
  to_port                      = tonumber(each.value)
  referenced_security_group_id = aws_security_group.lb.id
}

# out: https (the image, KMS, S3, STS, logs, issuers, upstreams) and the database
resource "aws_vpc_security_group_egress_rule" "task_https" {
  security_group_id = aws_security_group.task.id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "task_db" {
  security_group_id            = aws_security_group.task.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.db.id
}

resource "aws_security_group" "db" {
  name        = "${local.name}-db"
  description = "kista database: the tasks only"
  vpc_id      = local.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "db" {
  security_group_id            = aws_security_group.db.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.task.id
}

resource "aws_security_group" "endpoints" {
  count       = var.vpc_endpoints ? 1 : 0
  name        = "${local.name}-endpoints"
  description = "VPC endpoints for kista tasks"
  vpc_id      = local.vpc_id
}

# the tasks and db-init (its secret and its log go through the endpoints when private DNS is on)
resource "aws_vpc_security_group_ingress_rule" "endpoints" {
  for_each                     = var.vpc_endpoints ? { task = aws_security_group.task.id, db_init = aws_security_group.db_init.id } : {}
  security_group_id            = aws_security_group.endpoints[0].id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  referenced_security_group_id = each.value
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = var.vpc_endpoints ? toset(["kms", "secretsmanager", "logs"]) : toset([])
  vpc_id              = local.vpc_id
  service_name        = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = local.private_subnet_ids
  security_group_ids  = [aws_security_group.endpoints[0].id]
  private_dns_enabled = true
}

resource "aws_vpc_endpoint" "s3" {
  count             = var.vpc_endpoints && (local.own_vpc || length(var.private_route_table_ids) > 0) ? 1 : 0
  vpc_id            = local.vpc_id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = local.own_vpc ? [aws_route_table.private[0].id] : var.private_route_table_ids
}

# ---- the channel key: RSA_2048 SIGN_VERIFY, its purpose tag what the task role's policy matches

resource "aws_kms_key" "channel" {
  description              = "kista's channel signing key (${local.name})"
  customer_master_key_spec = "RSA_2048"
  key_usage                = "SIGN_VERIFY"
  deletion_window_in_days  = var.key_deletion_days
  # kista-deployment scopes the task role to this deployment's keys: kista-purpose alone would
  # give every kista in the account every channel key
  tags = { "kista-purpose" = "channel-signing", "kista-deployment" = local.name }
}

resource "aws_kms_alias" "channel" {
  name          = "alias/${local.name}-channel"
  target_key_id = aws_kms_key.channel.key_id
}

# ---- the bodies

resource "aws_s3_bucket" "bodies" {
  bucket        = "${local.name}-${local.account}-${var.region}"
  force_destroy = !var.deletion_protection
}

resource "aws_s3_bucket_public_access_block" "bodies" {
  bucket                  = aws_s3_bucket.bodies.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "bodies" {
  bucket = aws_s3_bucket.bodies.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.bodies.arn, "${aws_s3_bucket.bodies.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
  depends_on = [aws_s3_bucket_public_access_block.bodies]
}

resource "aws_s3_bucket_ownership_controls" "bodies" {
  bucket = aws_s3_bucket.bodies.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "bodies" {
  bucket = aws_s3_bucket.bodies.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# kista aborts its failed uploads and its collector the old ones (spec 0016): this is the backstop
resource "aws_s3_bucket_lifecycle_configuration" "bodies" {
  bucket = aws_s3_bucket.bodies.id
  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# ---- the store: PostgreSQL on RDS, IAM authentication, private

resource "aws_db_subnet_group" "db" {
  name       = local.name
  subnet_ids = local.private_subnet_ids
  lifecycle {
    precondition {
      condition     = local.own_vpc || (length(var.private_subnet_ids) >= 2 && length(var.public_subnet_ids) >= 2)
      error_message = "with vpc_id, give private_subnet_ids and public_subnet_ids, two AZs each"
    }
  }
}

# a destroy of a protected deployment keeps a final snapshot, named apart from any earlier one
resource "random_id" "snapshot" {
  byte_length = 4
}

resource "aws_db_instance" "db" {
  identifier                          = local.name
  engine                              = "postgres"
  engine_version                      = var.db_engine_version
  instance_class                      = var.db_instance_class
  allocated_storage                   = 20
  max_allocated_storage               = 100
  storage_encrypted                   = true
  db_name                             = "kista"
  username                            = "kistaadmin"
  manage_master_user_password         = true
  iam_database_authentication_enabled = true
  db_subnet_group_name                = aws_db_subnet_group.db.name
  vpc_security_group_ids              = [aws_security_group.db.id]
  publicly_accessible                 = false
  ca_cert_identifier                  = "rds-ca-rsa2048-g1"
  backup_retention_period             = 7
  deletion_protection                 = var.deletion_protection
  skip_final_snapshot                 = !var.deletion_protection
  final_snapshot_identifier           = var.deletion_protection ? "${local.name}-final-${random_id.snapshot.hex}" : null
  apply_immediately                   = true
}

# the region's RDS CA, which no system trust store holds; it reaches kista inline (store.ca_pem)
data "http" "rds_ca" {
  count = var.rds_ca_pem == null ? 1 : 0
  url   = "https://truststore.pki.rds.amazonaws.com/${var.region}/${var.region}-bundle.pem"
  lifecycle {
    postcondition {
      condition     = self.status_code == 200 && strcontains(self.response_body, "BEGIN CERTIFICATE")
      error_message = "the region's RDS CA bundle could not be fetched; give rds_ca_pem"
    }
  }
}

locals {
  rds_ca = var.rds_ca_pem != null ? var.rds_ca_pem : data.http.rds_ca[0].response_body
}

# ---- identities

data "aws_iam_policy_document" "tasks" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }
  }
}

resource "aws_iam_role" "task" {
  name               = "${local.name}-task"
  assume_role_policy = data.aws_iam_policy_document.tasks.json
}

data "aws_iam_policy_document" "task" {
  # this deployment's keys, by their tags: a key added later with both tags needs no policy change.
  # Whoever may tag keys in this account may make one kista's: deny tagging kista-purpose and
  # kista-deployment in production (an SCP or a permissions boundary).
  statement {
    actions   = ["kms:DescribeKey", "kms:ListResourceTags", "kms:GetPublicKey", "kms:Sign"]
    resources = ["arn:${local.partition}:kms:${var.region}:${local.account}:key/*"]
    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/kista-purpose"
      values   = ["channel-signing", "attestation-signing"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/kista-deployment"
      values   = [local.name]
    }
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${aws_s3_bucket.bodies.arn}/*"]
  }
  statement {
    actions   = ["s3:ListBucket", "s3:ListBucketMultipartUploads"]
    resources = [aws_s3_bucket.bodies.arn]
  }
  statement {
    actions   = ["rds-db:connect"]
    resources = ["arn:${local.partition}:rds-db:${var.region}:${local.account}:dbuser:${aws_db_instance.db.resource_id}/kista"]
  }
}

resource "aws_iam_role_policy" "task" {
  name   = "kista"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json
}

resource "aws_iam_role" "execution" {
  name               = "${local.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.tasks.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# db-init's own execution role: the only one that reads the master password
resource "aws_iam_role" "db_init" {
  name               = "${local.name}-db-init"
  assume_role_policy = data.aws_iam_policy_document.tasks.json
}

resource "aws_iam_role_policy_attachment" "db_init" {
  role       = aws_iam_role.db_init.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "db_init" {
  name = "master-password"
  role = aws_iam_role.db_init.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "secretsmanager:GetSecretValue"
      Resource = aws_db_instance.db.master_user_secret[0].secret_arn
    }]
  })
}

# ---- the configuration: one document, the service's and the commands', read with -config env:

locals {
  config = yamlencode({
    aws = { region = var.region, identity = { kind = "ecs" } }
    store = {
      kind   = "postgres"
      dsn    = "postgres://kista@${aws_db_instance.db.address}:${aws_db_instance.db.port}/kista?sslmode=verify-full"
      login  = { kind = "aws" }
      ca_pem = local.rds_ca
    }
    serve = {
      public_url = "https://${var.domain}"
      # the load balancer's nodes take addresses in its subnets
      trusted_proxies = data.aws_subnet.public[*].cidr_block
      listeners = [
        { addr = ":8443", scheme = "https", behind_proxy = true },
        { addr = ":8081", scheme = "health" },
      ]
    }
    signers = {
      sources = [{
        name     = "aws"
        kind     = "awskms"
        allow    = ["*"]
        awskms   = { region = var.region, account = local.account }
        identity = { kind = "ecs" }
      }]
    }
    blob = {
      spool_dir   = "/var/lib/kista/tmp/spool"
      max_body    = "${var.max_body_mib}MiB"
      max_ingests = var.max_ingests
      domains = [{
        name = "default"
        kind = "s3"
        s3 = {
          endpoint = "https://s3.${var.region}.amazonaws.com"
          bucket   = aws_s3_bucket.bodies.bucket
          region   = var.region
          lookup   = "dns"
          sse      = "s3"
          account  = local.account
          identity = { kind = "ecs" }
        }
      }]
    }
    auth = {
      server_issuers = yamldecode(var.server_issuers)
      server_admins  = yamldecode(var.server_admins)
    }
  })
  container = {
    name      = "kista"
    image     = var.image
    essential = true
    # the spool lives in the image's /var/lib/kista/tmp (owned by kista's uid) on the task's disk:
    # a Fargate bind mount there would be root-owned, and kista could not write it
    readonlyRootFilesystem = false
    linuxParameters        = { capabilities = { drop = ["ALL"] }, initProcessEnabled = false }
    environment            = [{ name = "KISTA_CONFIG", value = local.config }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.logs.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "kista"
      }
    }
  }
}

resource "aws_cloudwatch_log_group" "logs" {
  name              = "/ecs/${local.name}"
  retention_in_days = 30
}

resource "aws_ecs_cluster" "cluster" {
  name = local.name
}

resource "aws_ecs_task_definition" "service" {
  family                   = local.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  task_role_arn            = aws_iam_role.task.arn
  execution_role_arn       = aws_iam_role.execution.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }
  ephemeral_storage {
    size_in_gib = var.ephemeral_storage_gib
  }
  container_definitions = jsonencode([merge(local.container, {
    command      = ["serve", "-config", "env:KISTA_CONFIG"]
    portMappings = [{ containerPort = 8443 }, { containerPort = 8081 }]
    stopTimeout  = 120
  })])
  lifecycle {
    precondition {
      # the spool and the image on the task's disk, with 4 GiB to spare
      condition     = (2 * var.max_ingests + 1) * var.max_body_mib <= (var.ephemeral_storage_gib - 4) * 1024
      error_message = "(2 × max_ingests + 1) × max_body_mib must fit ephemeral_storage_gib less 4 GiB"
    }
  }
}

# the admin commands: aws ecs run-task with this definition and a command override (README)
resource "aws_ecs_task_definition" "command" {
  family                   = "${local.name}-command"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  task_role_arn            = aws_iam_role.task.arn
  execution_role_arn       = aws_iam_role.execution.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }
  ephemeral_storage {
    size_in_gib = var.ephemeral_storage_gib
  }
  container_definitions = jsonencode([merge(local.container, {
    command = ["admin", "-config", "env:KISTA_CONFIG", "check"]
  })])
}

# db-init: the IAM user kista, owner of the database and of schema public, once. The master is a
# member of kista only for the ownership change: a role holding kista inherits rds_iam, and RDS
# would refuse its password from then on.
resource "aws_ecs_task_definition" "db_init" {
  family                   = "${local.name}-db-init"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.db_init.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }
  container_definitions = jsonencode([{
    name        = "psql"
    image       = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
    essential   = true
    secrets     = [{ name = "PGPASSWORD", valueFrom = "${aws_db_instance.db.master_user_secret[0].secret_arn}:password::" }]
    environment = [{ name = "RDS_CA", value = local.rds_ca }, { name = "SQL", value = local.db_init_sql }]
    # the master password goes only to the server the region's CA vouches for
    entryPoint = ["sh", "-c"]
    command    = ["printf '%s' \"$RDS_CA\" >/tmp/rds-ca.pem && psql \"host=${aws_db_instance.db.address} dbname=kista user=kistaadmin sslmode=verify-full sslrootcert=/tmp/rds-ca.pem\" -v ON_ERROR_STOP=1 -c \"$SQL\""]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.logs.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "db-init"
      }
    }
  }])
}

locals {
  db_init_sql = <<-SQL
      DO $$ BEGIN
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kista') THEN CREATE ROLE kista LOGIN; END IF;
      END $$;
      GRANT rds_iam TO kista;
      GRANT kista TO kistaadmin;
      ALTER DATABASE kista OWNER TO kista;
      ALTER SCHEMA public OWNER TO kista;
      REVOKE kista FROM kistaadmin;
    SQL
}

resource "aws_vpc_security_group_ingress_rule" "db_init" {
  security_group_id            = aws_security_group.db.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.db_init.id
}

resource "aws_security_group" "db_init" {
  name        = "${local.name}-db-init"
  description = "kista db-init task"
  vpc_id      = local.vpc_id
}

resource "aws_vpc_security_group_egress_rule" "db_init_https" {
  security_group_id = aws_security_group.db_init.id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "db_init_db" {
  security_group_id            = aws_security_group.db_init.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.db.id
}

# run it once, where Terraform runs (the AWS CLI), and wait for its exit code; IAM and the secret
# take a moment to propagate, so it is tried a few times
resource "terraform_data" "db_init" {
  triggers_replace = [aws_db_instance.db.resource_id]
  depends_on = [
    aws_vpc_security_group_ingress_rule.db_init, aws_vpc_security_group_egress_rule.db_init_https,
    aws_vpc_security_group_egress_rule.db_init_db, aws_iam_role_policy.db_init, aws_iam_role_policy_attachment.db_init,
    aws_route_table_association.private, aws_vpc_endpoint.interface, aws_vpc_security_group_ingress_rule.endpoints,
  ]
  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      for try in 1 2 3 4 5 6; do
        [ "$try" = 1 ] || sleep 20
        arn=$(aws ecs run-task --region ${var.region} --cluster ${aws_ecs_cluster.cluster.name} --launch-type FARGATE \
          --task-definition ${aws_ecs_task_definition.db_init.arn} \
          --network-configuration "awsvpcConfiguration={subnets=[${join(",", local.private_subnet_ids)}],securityGroups=[${aws_security_group.db_init.id}],assignPublicIp=DISABLED}" \
          --query 'tasks[0].taskArn' --output text) || { echo "db-init: run-task failed (try $try)" >&2; continue; }
        case "$arn" in arn:*) ;; *) echo "db-init: no task started (try $try)" >&2; continue ;; esac
        aws ecs wait tasks-stopped --region ${var.region} --cluster ${aws_ecs_cluster.cluster.name} --tasks "$arn" || continue
        read -r code reason < <(aws ecs describe-tasks --region ${var.region} --cluster ${aws_ecs_cluster.cluster.name} --tasks "$arn" \
          --query 'tasks[0].[containers[0].exitCode, stoppedReason]' --output text)
        [ "$code" = 0 ] && exit 0
        echo "db-init: exit $code, $reason (try $try); its log is in ${aws_cloudwatch_log_group.logs.name}, prefix db-init" >&2
      done
      exit 1
    EOT
  }
}

# ---- the load balancer

resource "aws_acm_certificate" "cert" {
  count             = var.certificate_arn == null ? 1 : 0
  domain_name       = var.domain
  validation_method = "DNS"
  lifecycle {
    create_before_destroy = true
    precondition {
      condition     = var.route53_zone_id != null
      error_message = "give certificate_arn, or route53_zone_id to validate a new certificate in"
    }
  }
}

resource "aws_route53_record" "validation" {
  for_each = var.certificate_arn == null ? {
    for o in aws_acm_certificate.cert[0].domain_validation_options : o.domain_name => o
  } : {}
  zone_id         = var.route53_zone_id
  name            = each.value.resource_record_name
  type            = each.value.resource_record_type
  records         = [each.value.resource_record_value]
  ttl             = 300
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "cert" {
  count                   = var.certificate_arn == null ? 1 : 0
  certificate_arn         = aws_acm_certificate.cert[0].arn
  validation_record_fqdns = [for r in aws_route53_record.validation : r.fqdn]
}

locals {
  certificate_arn = var.certificate_arn != null ? var.certificate_arn : aws_acm_certificate_validation.cert[0].certificate_arn
}

resource "aws_lb" "lb" {
  name                       = local.name
  load_balancer_type         = "application"
  subnets                    = local.public_subnet_ids
  security_groups            = [aws_security_group.lb.id]
  drop_invalid_header_fields = true
  # below kista's 120-second keep-alive, so the load balancer never reuses a connection kista closed
  idle_timeout               = 60
  enable_deletion_protection = var.deletion_protection
}

resource "aws_lb_target_group" "kista" {
  name                 = local.name
  port                 = 8443
  protocol             = "HTTP"
  protocol_version     = "HTTP1"
  target_type          = "ip"
  vpc_id               = local.vpc_id
  deregistration_delay = 30
  health_check {
    port                = "8081"
    path                = "/readyz"
    matcher             = "200"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.lb.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = local.certificate_arn
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.kista.arn
  }
}

# plain http is forwarded, not redirected: DuckDB's bootstrap and passthrough autoload need it. The
# API and the console go to https, so a token sent to http:// by mistake is not forwarded (kista
# would ignore it, but it would have crossed the network in the clear).
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.lb.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.kista.arn
  }
}

resource "aws_lb_listener_rule" "http_to_https" {
  listener_arn = aws_lb_listener.http.arn
  priority     = 10
  condition {
    path_pattern {
      values = ["/api/*", "/console", "/console/*"]
    }
  }
  action {
    type = "redirect"
    redirect {
      protocol    = "HTTPS"
      port        = "443"
      status_code = "HTTP_301"
    }
  }
}

resource "aws_route53_record" "name" {
  count   = var.route53_zone_id != null ? 1 : 0
  zone_id = var.route53_zone_id
  name    = var.domain
  type    = "A"
  alias {
    name                   = aws_lb.lb.dns_name
    zone_id                = aws_lb.lb.zone_id
    evaluate_target_health = false
  }
}

# ---- the service

resource "aws_ecs_service" "kista" {
  name                              = "kista"
  cluster                           = aws_ecs_cluster.cluster.id
  task_definition                   = aws_ecs_task_definition.service.arn
  desired_count                     = var.desired_count
  launch_type                       = "FARGATE"
  health_check_grace_period_seconds = 120
  wait_for_steady_state             = true
  propagate_tags                    = "SERVICE"
  network_configuration {
    subnets          = local.private_subnet_ids
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = false
  }
  load_balancer {
    target_group_arn = aws_lb_target_group.kista.arn
    container_name   = "kista"
    container_port   = 8443
  }
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
  depends_on = [aws_lb_listener.https, aws_lb_listener.http, terraform_data.db_init, aws_iam_role_policy.task,
  aws_iam_role_policy_attachment.execution]
}
