# The recipe's plan and its refusals, with mock providers: no cloud and no credentials (CI).

mock_provider "google" {
  mock_data "google_client_config" {
    defaults = { access_token = "token" }
  }
  mock_resource "google_container_cluster" {
    defaults = { endpoint = "203.0.113.1", master_auth = [{ cluster_ca_certificate = "Y2E=" }] }
  }
  mock_resource "google_sql_database_instance" {
    defaults = {
      dns_names          = [{ name = "abc123.def456.europe-west1.sql-psa.goog.", connection_type = "PRIVATE_SERVICES_ACCESS", dns_scope = "INSTANCE" }]
      private_ip_address = "10.50.0.3"
    }
  }
  mock_resource "google_compute_global_address" {
    defaults = { address = "203.0.113.10" }
  }
  mock_resource "google_service_account" {
    defaults = {
      email  = "kista-kista@acme-kista.iam.gserviceaccount.com"
      member = "serviceAccount:kista-kista@acme-kista.iam.gserviceaccount.com"
      name   = "projects/acme-kista/serviceAccounts/kista-kista@acme-kista.iam.gserviceaccount.com"
    }
  }
  mock_resource "google_compute_network" {
    defaults = { id = "projects/acme-kista/global/networks/kista" }
  }
  mock_resource "google_compute_subnetwork" {
    defaults = { id = "projects/acme-kista/regions/europe-west1/subnetworks/kista-nodes" }
  }
  mock_resource "google_kms_key_ring" {
    defaults = { id = "projects/acme-kista/locations/europe-west1/keyRings/kista" }
  }
  mock_resource "google_kms_crypto_key" {
    defaults = { id = "projects/acme-kista/locations/europe-west1/keyRings/kista/cryptoKeys/kista-channel" }
  }
  mock_data "google_sql_ca_certs" {
    defaults = { certs = [{ cert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n" }] }
  }
}

mock_provider "helm" {}
mock_provider "kubernetes" {}
mock_provider "random" {}

variables {
  project        = "acme-kista"
  domain         = "kista.example.com"
  image          = "ghcr.io/hugr-lab/kista:v0.1.0"
  chart_version  = "0.1.0"
  server_issuers = "[{name: corp, url: 'https://login.example.com/realms/corp', required_claims: {azp: kista}, roles_claim: realm_access.roles}]"
  server_admins  = "['role:corp|kista-admin']"
}

run "own_vpc" {
  command = apply
  assert {
    condition     = length(google_compute_network.vpc) == 1 && length(google_compute_router_nat.nat) == 1 && length(google_service_networking_connection.psa) == 1
    error_message = "a VPC of its own: Cloud NAT and private services access"
  }
  assert {
    condition     = google_container_cluster.gke.enable_autopilot && google_container_cluster.gke.private_cluster_config[0].enable_private_nodes
    error_message = "the cluster: Autopilot, private nodes"
  }
  assert {
    condition     = google_kms_crypto_key.channel.version_template[0].protection_level == "HSM" && google_kms_crypto_key.channel.purpose == "ASYMMETRIC_SIGN"
    error_message = "the channel key: an HSM signing key"
  }
  assert {
    condition = alltrue([
      !google_sql_database_instance.db.settings[0].ip_configuration[0].ipv4_enabled,
      google_sql_database_instance.db.settings[0].ip_configuration[0].ssl_mode == "ENCRYPTED_ONLY",
      google_sql_database_instance.db.settings[0].ip_configuration[0].server_ca_mode == "GOOGLE_MANAGED_CAS_CA",
    ])
    error_message = "the database: private, TLS only, the CA that names its DNS name"
  }
  assert {
    condition     = local.db_host == "abc123.def456.europe-west1.sql-psa.goog." && google_sql_user.kista.name == "kista-kista@acme-kista.iam"
    error_message = "the DSN: the instance's DNS name with its trailing dot, the service account's database user"
  }
  assert {
    condition     = contains(local.config.serve.trusted_proxies, "203.0.113.10/32") && local.config.store.login.kind == "gcp"
    error_message = "the configuration: the static IP among the trusted proxies, the gcp login"
  }
  assert {
    condition     = local.image_repo == "ghcr.io/hugr-lab/kista" && local.image_tag == "v0.1.0" && local.image_digest == ""
    error_message = "the image by tag"
  }
  assert {
    condition     = google_storage_bucket.bodies.public_access_prevention == "enforced" && google_storage_bucket.bodies.uniform_bucket_level_access
    error_message = "the bucket: private, uniform access"
  }
  assert {
    condition     = length(google_dns_record_set.domain) == 0
    error_message = "no DNS record without dns_zone"
  }
  assert {
    condition     = !contains(keys(local.config.auth), "server_license_admins")
    error_message = "no licence managers: auth.server_license_admins left out of the configuration"
  }
}

run "licence_managers" {
  command = plan
  variables {
    server_license_admins = "['role:corp|kista-licenses']"
  }
  assert {
    condition     = local.auth.server_license_admins == ["role:corp|kista-licenses"] && local.auth.server_admins == ["role:corp|kista-admin"]
    error_message = "auth.server_license_admins beside auth.server_admins"
  }
}

run "a_given_network_and_zone" {
  command = plan
  variables {
    network    = "projects/acme-kista/global/networks/corp"
    subnetwork = "projects/acme-kista/regions/europe-west1/subnetworks/gke"
    dns_zone   = "example-com"
    image      = "ghcr.io/hugr-lab/kista@sha256:0b5019f0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
  assert {
    condition     = length(google_compute_network.vpc) == 0 && length(google_service_networking_connection.psa) == 0 && length(google_dns_record_set.domain) == 1
    error_message = "a given network: none made; the domain's record in the zone"
  }
  assert {
    condition = alltrue([
      google_container_cluster.gke.network == var.network,
      google_container_cluster.gke.subnetwork == var.subnetwork,
      google_sql_database_instance.db.settings[0].ip_configuration[0].private_network == var.network,
      one(one(google_dns_managed_zone.db.private_visibility_config).networks).network_url == var.network,
    ])
    error_message = "a given network: the cluster, the database and the private zone on it"
  }
  assert {
    condition     = local.image_repo == "ghcr.io/hugr-lab/kista" && local.image_tag == "" && local.image_digest == "sha256:0b5019f0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    error_message = "the image by digest"
  }
}

run "a_network_without_subnetwork" {
  command = plan
  variables {
    network = "projects/acme-kista/global/networks/corp"
  }
  expect_failures = [google_container_cluster.gke]
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

run "a_spool_too_large" {
  command = plan
  variables {
    max_body_mib = 2048
  }
  expect_failures = [helm_release.kista]
}

run "a_digest_without_sha256" {
  command = plan
  variables {
    image = "ghcr.io/hugr-lab/kista@"
  }
  expect_failures = [var.image]
}

run "no_ingests" {
  command = plan
  variables {
    max_ingests = 0
  }
  expect_failures = [var.max_ingests]
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
