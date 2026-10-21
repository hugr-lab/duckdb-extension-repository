variable "project" {
  type        = string
  description = "The project to deploy in. A project of kista's own is the clean way out: Cloud KMS key rings are never deleted."
}

variable "region" {
  type    = string
  default = "europe-west1"
}

variable "prefix" {
  type        = string
  default     = "kista"
  description = "Names every resource; the service account is <prefix>-kista."
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,17}$", var.prefix))
    error_message = "prefix: lower-case letters, digits and hyphens, 2 to 18 characters, starting with a letter"
  }
}

variable "labels" {
  type    = map(string)
  default = {}
}

# ---- the network

variable "network" {
  type        = string
  default     = null
  description = "A VPC network's self link to use; null makes one, with a Cloud NAT and private services access. A given network needs both already."
}

variable "subnetwork" {
  type        = string
  default     = null
  description = "With network: the subnetwork for the cluster, in the region."
}

# ---- the address

variable "domain" {
  type        = string
  description = "kista's public host name, e.g. kista.example.com. Its DNS must point at the static IP (output ip) before Google issues the certificate."
}

variable "dns_zone" {
  type        = string
  default     = null
  description = "A Cloud DNS managed zone (in this project) for the domain: the recipe writes its A record. Null: point the name at output ip yourself."
}

# ---- kista

variable "image" {
  type        = string
  description = "kista's image: ghcr.io/hugr-lab/kista:vX.Y.Z or …@sha256:…"
  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.image)) || (!can(regex("@", var.image)) && !can(regex(":(edge|latest)$", var.image)))
    error_message = "image: a version or a digest; a moving tag (:edge, :latest) only pinned by digest"
  }
}

variable "chart" {
  type        = string
  default     = "oci://ghcr.io/hugr-lab/charts/kista"
  description = "The Helm chart: the published one, or a local path."
}

variable "chart_version" {
  type        = string
  default     = null
  description = "The chart's version (a release's X.Y.Z); null with a local path."
}

variable "server_issuers" {
  type        = string
  description = "auth.server_issuers as YAML: the identity provider of the people who administer kista."
}

variable "server_admins" {
  type        = string
  description = "auth.server_admins as YAML."
}

variable "server_license_admins" {
  type        = string
  default     = "[]"
  description = "auth.server_license_admins as YAML: the licence managers' principals (spec 0029); empty, none."
}

variable "replicas" {
  type    = number
  default = 2
}

variable "max_body_mib" {
  type        = number
  default     = 1024
  description = "blob.max_body in MiB."
  validation {
    condition     = var.max_body_mib >= 1 && floor(var.max_body_mib) == var.max_body_mib
    error_message = "max_body_mib: a whole number of MiB"
  }
}

variable "max_ingests" {
  type    = number
  default = 4
  validation {
    condition     = var.max_ingests >= 1 && floor(var.max_ingests) == var.max_ingests
    error_message = "max_ingests: a whole number, at least 1"
  }
}

# ---- the database

variable "db_tier" {
  type    = string
  default = "db-custom-1-3840"
}

variable "db_edition" {
  type    = string
  default = "ENTERPRISE"
  validation {
    condition     = contains(["ENTERPRISE", "ENTERPRISE_PLUS"], var.db_edition)
    error_message = "db_edition: ENTERPRISE or ENTERPRISE_PLUS"
  }
}

variable "master_authorized_cidrs" {
  type        = list(string)
  default     = []
  description = "Who may reach the cluster's API server (where Terraform runs among them); empty: any address, with Google's authentication."
}

variable "deletion_protection" {
  type        = bool
  default     = true
  description = "Protects the cluster and the database from a destroy, and keeps the bucket's objects."
}
