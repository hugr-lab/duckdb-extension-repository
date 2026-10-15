# kista on GKE Autopilot (spec 0020 phase 2): the chart, its Kubernetes service account acting as a
# Google service account through Workload Identity (no key), signing with a Cloud KMS HSM key,
# bodies in GCS, the store on Cloud SQL for PostgreSQL by IAM authentication over verify-full,
# behind a GKE Ingress that serves http and https without a redirect.

locals {
  name      = var.prefix
  own_vpc   = var.network == null
  namespace = "kista"
  ksa       = "kista"
  # Google's front ends: the external load balancer's proxies and its health checks
  gfe = ["35.191.0.0/16", "130.211.0.0/22"]
}

resource "google_project_service" "apis" {
  for_each = toset([
    "compute.googleapis.com",
    "container.googleapis.com",
    "sqladmin.googleapis.com",
    "servicenetworking.googleapis.com",
    "cloudkms.googleapis.com",
    "dns.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "storage.googleapis.com",
  ])
  service            = each.value
  disable_on_destroy = false
}

# ---- the network: a VPC of the recipe's own, unless network names one

resource "google_compute_network" "vpc" {
  count                   = local.own_vpc ? 1 : 0
  name                    = local.name
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

resource "google_compute_subnetwork" "nodes" {
  count                    = local.own_vpc ? 1 : 0
  name                     = "${local.name}-nodes"
  network                  = google_compute_network.vpc[0].id
  region                   = var.region
  ip_cidr_range            = "10.40.0.0/20"
  private_ip_google_access = true
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.44.0.0/14"
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.48.0.0/20"
  }
}

# the nodes have no public address: kista pulls its image and reaches its issuers and upstreams
resource "google_compute_router" "nat" {
  count   = local.own_vpc ? 1 : 0
  name    = "${local.name}-nat"
  network = google_compute_network.vpc[0].id
  region  = var.region
}

resource "google_compute_router_nat" "nat" {
  count                              = local.own_vpc ? 1 : 0
  name                               = local.name
  router                             = google_compute_router.nat[0].name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}

# private services access: Cloud SQL's private IP
resource "google_compute_global_address" "psa" {
  count         = local.own_vpc ? 1 : 0
  name          = "${local.name}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 20
  network       = google_compute_network.vpc[0].id
}

resource "google_service_networking_connection" "psa" {
  count                   = local.own_vpc ? 1 : 0
  network                 = google_compute_network.vpc[0].id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.psa[0].name]
  # the peering outlives the instance by days on Google's side; the network's deletion removes it
  deletion_policy = "ABANDON"
}

locals {
  network    = local.own_vpc ? google_compute_network.vpc[0].id : var.network
  subnetwork = local.own_vpc ? google_compute_subnetwork.nodes[0].id : var.subnetwork
}

# ---- the cluster

resource "google_container_cluster" "gke" {
  name                = local.name
  location            = var.region
  enable_autopilot    = true
  network             = local.network
  subnetwork          = local.subnetwork
  deletion_protection = var.deletion_protection

  release_channel {
    channel = "REGULAR"
  }

  ip_allocation_policy {
    cluster_secondary_range_name  = local.own_vpc ? "pods" : null
    services_secondary_range_name = local.own_vpc ? "services" : null
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
  }

  dynamic "master_authorized_networks_config" {
    for_each = length(var.master_authorized_cidrs) > 0 ? [1] : []
    content {
      dynamic "cidr_blocks" {
        for_each = var.master_authorized_cidrs
        content {
          cidr_block = cidr_blocks.value
        }
      }
    }
  }

  lifecycle {
    precondition {
      condition     = local.own_vpc || var.subnetwork != null
      error_message = "network: give its subnetwork too"
    }
  }

  depends_on = [google_project_service.apis, google_compute_router_nat.nat]
}

# ---- the identity: no key; kista's Kubernetes service account acts as it

resource "google_service_account" "kista" {
  account_id   = "${local.name}-kista"
  display_name = "kista (${local.name})"
  depends_on   = [google_project_service.apis]
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.kista.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project}.svc.id.goog[${local.namespace}/${local.ksa}]"
  # the workload identity pool exists with the cluster
  depends_on = [google_container_cluster.gke]
}

# ---- Cloud KMS: a key ring of the deployment's own; key rings are never deleted

resource "google_kms_key_ring" "kista" {
  name       = local.name
  location   = var.region
  depends_on = [google_project_service.apis]
}

resource "google_kms_crypto_key" "channel" {
  name     = "${local.name}-channel"
  key_ring = google_kms_key_ring.kista.id
  purpose  = "ASYMMETRIC_SIGN"
  labels   = { "kista-purpose" = "channel-signing" }
  version_template {
    algorithm        = "RSA_SIGN_PKCS1_2048_SHA256"
    protection_level = "HSM"
  }
}

resource "google_kms_key_ring_iam_member" "kista" {
  for_each    = toset(["roles/cloudkms.signerVerifier", "roles/cloudkms.viewer"])
  key_ring_id = google_kms_key_ring.kista.id
  role        = each.value
  member      = google_service_account.kista.member
}

# ---- GCS

resource "google_storage_bucket" "bodies" {
  name                        = "${var.project}-${local.name}"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = !var.deletion_protection
  # spec 0005's backstop for uploads a crash left behind
  lifecycle_rule {
    condition {
      age = 7
    }
    action {
      type = "AbortIncompleteMultipartUpload"
    }
  }
  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "kista" {
  bucket = google_storage_bucket.bodies.name
  role   = "roles/storage.objectUser"
  member = google_service_account.kista.member
}

# ---- Cloud SQL for PostgreSQL: private IP, IAM authentication, the Google-managed CA that names
# the instance's DNS name

# an instance's name is not reusable for a week after its deletion
resource "random_id" "db" {
  byte_length = 3
}

# used once by db-init, then replaced by a value printed nowhere: the state keeps a dead password
resource "random_password" "root" {
  length  = 32
  special = false
}

resource "google_sql_database_instance" "db" {
  name                = "${local.name}-${random_id.db.hex}"
  database_version    = "POSTGRES_17"
  region              = var.region
  root_password       = random_password.root.result
  deletion_protection = var.deletion_protection

  settings {
    tier                        = var.db_tier
    edition                     = var.db_edition
    availability_type           = "ZONAL"
    deletion_protection_enabled = var.deletion_protection
    ip_configuration {
      ipv4_enabled    = false
      private_network = local.network
      ssl_mode        = "ENCRYPTED_ONLY"
      server_ca_mode  = "GOOGLE_MANAGED_CAS_CA"
    }
    database_flags {
      name  = "cloudsql.iam_authentication"
      value = "on"
    }
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }
  }

  depends_on = [google_project_service.apis, google_service_networking_connection.psa]
}

# owned by the IAM user's objects once kista migrates: Terraform cannot drop them
resource "google_sql_database" "kista" {
  name            = "kista"
  instance        = google_sql_database_instance.db.name
  deletion_policy = "ABANDON"
}

resource "google_sql_user" "kista" {
  name            = local.db_user
  instance        = google_sql_database_instance.db.name
  type            = "CLOUD_IAM_SERVICE_ACCOUNT"
  deletion_policy = "ABANDON"
}

resource "google_project_iam_member" "db_login" {
  project = var.project
  role    = "roles/cloudsql.instanceUser"
  member  = google_service_account.kista.member
  condition {
    title      = "${local.name}-db"
    expression = "resource.type == 'sqladmin.googleapis.com/Instance' && resource.name == 'projects/${var.project}/instances/${google_sql_database_instance.db.name}'"
  }
}

data "google_sql_ca_certs" "db" {
  instance = google_sql_database_instance.db.name
}

locals {
  # the database user of a service account: its email without .gserviceaccount.com
  db_user = trimsuffix(google_service_account.kista.email, ".gserviceaccount.com")
  # the instance's name on private services access (…sql-psa.goog.), which its certificate names
  # with the trailing dot; so does the DSN. Cloud SQL leaves its record to the customer.
  db_host = "${trimsuffix(one([for d in google_sql_database_instance.db.dns_names : d.name if d.connection_type == "PRIVATE_SERVICES_ACCESS"]), ".")}."
  db_ca   = join("", [for c in data.google_sql_ca_certs.db.certs : c.cert])
}

# the instance's DNS name resolves nowhere public: a private zone on the network serves it
resource "google_dns_managed_zone" "db" {
  name       = "${local.name}-db"
  dns_name   = local.db_host
  visibility = "private"
  private_visibility_config {
    networks {
      network_url = local.network
    }
  }
  depends_on = [google_project_service.apis]
}

resource "google_dns_record_set" "db" {
  managed_zone = google_dns_managed_zone.db.name
  name         = local.db_host
  type         = "A"
  ttl          = 300
  rrdatas      = [google_sql_database_instance.db.private_ip_address]
}

# ---- db-init: a one-time Job that lets the IAM user create kista's tables

resource "kubernetes_namespace_v1" "kista" {
  metadata {
    name = local.namespace
  }
}

resource "kubernetes_secret_v1" "db_init" {
  metadata {
    name      = "db-init"
    namespace = kubernetes_namespace_v1.kista.metadata[0].name
  }
  data = {
    password   = random_password.root.result
    "ca.pem"   = local.db_ca
    "init.sql" = local.db_init_sql
  }
}

locals {
  # The database is the API's, owned by cloudsqlsuperuser, of which postgres is a member: it grants
  # the IAM user what kista's migrations need, then replaces its own password with one printed
  # nowhere. One transaction.
  db_init_sql = <<-SQL
    BEGIN;
    GRANT CONNECT, TEMPORARY, CREATE ON DATABASE kista TO "${local.db_user}";
    GRANT USAGE, CREATE ON SCHEMA public TO "${local.db_user}";
    SELECT format('ALTER ROLE postgres PASSWORD %L', replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', '')) \gexec
    COMMIT;
  SQL
}

resource "kubernetes_job_v1" "db_init" {
  metadata {
    name      = "db-init"
    namespace = kubernetes_namespace_v1.kista.metadata[0].name
  }
  spec {
    backoff_limit = 2
    template {
      metadata {}
      spec {
        restart_policy = "Never"
        security_context {
          run_as_non_root = true
          run_as_user     = 70
          run_as_group    = 70
        }
        container {
          name  = "psql"
          image = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
          # the private zone and the peering take a while to answer: try for five minutes
          command = ["sh", "-c", <<-SH
            for i in $(seq 1 30); do
              psql "host=$DB_HOST port=5432 dbname=kista user=postgres sslmode=verify-full sslrootcert=/db-init/ca.pem" \
                -v ON_ERROR_STOP=1 -f /db-init/init.sql && exit 0
              sleep 10
            done
            exit 1
          SH
          ]
          env {
            name  = "DB_HOST"
            value = local.db_host
          }
          env {
            name = "PGPASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.db_init.metadata[0].name
                key  = "password"
              }
            }
          }
          volume_mount {
            name       = "db-init"
            mount_path = "/db-init"
            read_only  = true
          }
          security_context {
            allow_privilege_escalation = false
            capabilities {
              drop = ["ALL"]
            }
          }
          resources {
            requests = { cpu = "250m", memory = "256Mi" }
          }
        }
        volume {
          name = "db-init"
          secret {
            secret_name = kubernetes_secret_v1.db_init.metadata[0].name
          }
        }
      }
    }
  }
  wait_for_completion = true
  timeouts {
    create = "15m"
  }
  lifecycle {
    # Autopilot fills in the pod template and annotates the Job: without this, every
    # plan would replace the Job and run it again with the dead password. Run it again on purpose
    # with -replace (the docs say how).
    ignore_changes = [spec, metadata[0].annotations]
  }
  depends_on = [google_sql_database.kista, google_sql_user.kista, google_dns_record_set.db]
}

# ---- the address: a global static IP, the Ingress's, and its DNS name

resource "google_compute_global_address" "ip" {
  name       = "${local.name}-ip"
  depends_on = [google_project_service.apis]
}

resource "google_dns_record_set" "domain" {
  count        = var.dns_zone == null ? 0 : 1
  managed_zone = var.dns_zone
  name         = "${var.domain}."
  type         = "A"
  ttl          = 300
  rrdatas      = [google_compute_global_address.ip.address]
}

# ---- kista: the chart

locals {
  image_digest = try(regex("@(sha256:[0-9a-f]{64})$", var.image)[0], "")
  image_repo   = local.image_digest != "" ? split("@", var.image)[0] : replace(var.image, "/:[^/:]+$/", "")
  image_tag    = local.image_digest != "" ? "" : try(regex(":([^/:]+)$", var.image)[0], "")

  config = {
    gcp = { principal = google_service_account.kista.email }
    store = {
      kind   = "postgres"
      dsn    = "host=${local.db_host} user=${local.db_user} dbname=kista sslmode=verify-full"
      login  = { kind = "gcp" }
      ca_pem = local.db_ca
    }
    serve = {
      public_url = "https://${var.domain}"
      # the load balancer appends its own address to X-Forwarded-For: without its /32 every client
      # would appear as the load balancer
      trusted_proxies = concat(local.gfe, ["${google_compute_global_address.ip.address}/32"])
    }
    signers = {
      sources = [{
        name   = "gcp"
        kind   = "gcpkms"
        allow  = ["*"]
        gcpkms = { key_ring = google_kms_key_ring.kista.id, principal = google_service_account.kista.email }
      }]
    }
    blob = {
      max_body    = "${var.max_body_mib}MiB"
      max_ingests = var.max_ingests
      domains = [{
        name = "default"
        kind = "gcs"
        gcs  = { bucket = google_storage_bucket.bodies.name, principal = google_service_account.kista.email }
      }]
    }
    auth = {
      server_issuers = yamldecode(var.server_issuers)
      server_admins  = yamldecode(var.server_admins)
    }
  }

  values = {
    image          = { repository = local.image_repo, tag = local.image_tag, digest = local.image_digest }
    replicas       = var.replicas
    serviceAccount = { name = local.ksa }
    gcp            = { serviceAccount = google_service_account.kista.email }
    config         = local.config
    gke = {
      backendConfig      = { enabled = true, timeoutSec = 600 }
      managedCertificate = { enabled = true }
    }
    ingress = {
      enabled = true
      # GKE's controller takes an Ingress by this annotation (Autopilot has no IngressClass object)
      annotations = {
        "kubernetes.io/ingress.class"                 = "gce"
        "kubernetes.io/ingress.global-static-ip-name" = google_compute_global_address.ip.name
      }
      hosts = [{ host = var.domain }]
    }
    networkPolicy = { loadBalancerRanges = local.gfe }
    # the load balancer drops a pod from its NEG before the pod stops serving
    preStopSeconds = 20
    # GKE's NEG readiness gate can pass before the load balancer has checked a new pod: a rollout
    # waits this long before it removes an old one
    minReadySeconds = 60
  }
}

resource "helm_release" "kista" {
  name             = "kista"
  namespace        = kubernetes_namespace_v1.kista.metadata[0].name
  chart            = var.chart
  version          = var.chart_version
  values           = [yamlencode(local.values)]
  wait             = true
  timeout          = 900
  create_namespace = false

  lifecycle {
    precondition {
      # Autopilot gives a pod at most 10 GiB of ephemeral storage
      condition     = (2 * var.max_ingests + 1) * var.max_body_mib <= 10240
      error_message = "the spool, (2 × max_ingests + 1) × max_body_mib, exceeds Autopilot's 10 GiB of ephemeral storage"
    }
  }

  depends_on = [
    kubernetes_job_v1.db_init,
    google_service_account_iam_member.workload_identity,
    google_kms_key_ring_iam_member.kista,
    google_storage_bucket_iam_member.kista,
    google_project_iam_member.db_login,
  ]
}
