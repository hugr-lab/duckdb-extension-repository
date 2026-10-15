output "url" {
  value = "https://${var.domain}"
}

output "ip" {
  value       = google_compute_global_address.ip.address
  description = "The Ingress's static IP: the domain's A record."
}

output "service_account" {
  value = google_service_account.kista.email
}

output "channel_signer" {
  value       = "gcp:${google_kms_crypto_key.channel.name}/1"
  description = "The channel key's signer reference."
}

output "key_ring" {
  value = google_kms_key_ring.kista.id
}

output "bucket" {
  value = google_storage_bucket.bodies.name
}

output "db_instance" {
  value = google_sql_database_instance.db.name
}

output "db_host" {
  value = local.db_host
}

output "cluster" {
  value = google_container_cluster.gke.name
}

output "get_credentials" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.gke.name} --project ${var.project} --region ${var.region}"
}

output "namespace" {
  value = local.namespace
}
