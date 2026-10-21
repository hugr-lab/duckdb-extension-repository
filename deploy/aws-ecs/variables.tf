variable "region" {
  type    = string
  default = "eu-central-1"
}

variable "prefix" {
  type        = string
  default     = "kista"
  description = "Names every resource."
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,19}$", var.prefix))
    error_message = "prefix: a lowercase letter, then letters, digits and hyphens, 2 to 20 in all."
  }
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "More tags on every resource."
}

variable "vpc_id" {
  type        = string
  default     = null
  description = "An existing VPC; none: the recipe makes one (two AZs, one NAT gateway)."
}

variable "private_subnet_ids" {
  type        = list(string)
  default     = []
  description = "With vpc_id: the tasks' and the database's subnets (two AZs at least)."
}

variable "public_subnet_ids" {
  type        = list(string)
  default     = []
  description = "With vpc_id: the load balancer's subnets. Their CIDRs are kista's trusted_proxies."
}

variable "private_route_table_ids" {
  type        = list(string)
  default     = []
  description = "With vpc_id and vpc_endpoints: the private subnets' route tables, for the S3 gateway endpoint."
}

variable "vpc_endpoints" {
  type        = bool
  default     = false
  description = "Interface endpoints for KMS, Secrets Manager and Logs, and a gateway endpoint for S3."
}

variable "domain" {
  type        = string
  description = "The host name kista answers at; its public URL is https://<domain>."
}

variable "route53_zone_id" {
  type        = string
  default     = null
  description = "A Route 53 zone for the domain: an alias to the load balancer, and the certificate's validation."
}

variable "certificate_arn" {
  type        = string
  default     = null
  description = "An issued ACM certificate for the domain; none: one is requested and DNS-validated in route53_zone_id."
}

variable "image" {
  type        = string
  description = "kista's image, by version or digest (ghcr.io/hugr-lab/kista:vX.Y.Z or …@sha256:…); never a moving tag."
  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.image)) || !can(regex(":(edge|latest)$", var.image))
    error_message = "image: a version tag or a digest; :edge and :latest move (pin edge by its digest instead)."
  }
}

variable "cpu_architecture" {
  type    = string
  default = "ARM64"
  validation {
    condition     = contains(["ARM64", "X86_64"], var.cpu_architecture)
    error_message = "cpu_architecture: ARM64 or X86_64."
  }
}

variable "server_issuers" {
  type        = string
  default     = "[]"
  description = "auth.server_issuers, YAML in a string (the Configuration page)."
}

variable "server_admins" {
  type        = string
  default     = "[]"
  description = "auth.server_admins, YAML in a string: the server administrators' principals."
}

variable "server_license_admins" {
  type        = string
  default     = "[]"
  description = "auth.server_license_admins, YAML in a string: the licence managers' principals (spec 0029); empty, none."
}

variable "desired_count" {
  type    = number
  default = 2
}

variable "cpu" {
  type    = number
  default = 1024
}

variable "memory" {
  type    = number
  default = 2048
}

variable "ephemeral_storage_gib" {
  type        = number
  default     = 30
  description = "The task's disk (21-200 GiB): the image and the spool."
  validation {
    condition     = var.ephemeral_storage_gib >= 21 && var.ephemeral_storage_gib <= 200
    error_message = "ephemeral_storage_gib: 21 to 200."
  }
}

variable "max_body_mib" {
  type        = number
  default     = 1024
  description = "blob.max_body in MiB. The spool needs (2 × max_ingests + 1) × max_body."
}

variable "max_ingests" {
  type    = number
  default = 4
}

variable "db_instance_class" {
  type    = string
  default = "db.t4g.micro"
}

variable "db_engine_version" {
  type    = string
  default = "17"
}

variable "rds_ca_pem" {
  type        = string
  default     = null
  description = "The region's RDS CA bundle (PEM, the regional one: the global bundle overflows the task definition); none: fetched from AWS's trust store over https at plan time."
}

variable "deletion_protection" {
  type        = bool
  default     = true
  description = "On the database and the load balancer; off also skips the database's final snapshot and lets destroy empty the bucket."
}

variable "key_deletion_days" {
  type    = number
  default = 30
  validation {
    condition     = var.key_deletion_days >= 7 && var.key_deletion_days <= 30
    error_message = "key_deletion_days: 7 to 30."
  }
}
