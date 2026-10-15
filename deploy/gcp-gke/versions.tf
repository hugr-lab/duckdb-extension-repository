terraform {
  required_version = ">= 1.5"
  required_providers {
    google     = { source = "hashicorp/google", version = ">= 6.14, < 8" }
    helm       = { source = "hashicorp/helm", version = ">= 3.0, < 4" }
    kubernetes = { source = "hashicorp/kubernetes", version = ">= 2.36, < 3" }
    random     = { source = "hashicorp/random", version = ">= 3.6, < 4" }
  }
}

provider "google" {
  project        = var.project
  region         = var.region
  default_labels = merge({ "managed-by" = "terraform", "app" = "kista", "prefix" = var.prefix }, var.labels)
}

data "google_client_config" "this" {}

# the cluster the recipe makes: its endpoint is known only after it exists
provider "kubernetes" {
  host                   = "https://${google_container_cluster.gke.endpoint}"
  token                  = data.google_client_config.this.access_token
  cluster_ca_certificate = base64decode(google_container_cluster.gke.master_auth[0].cluster_ca_certificate)
}

provider "helm" {
  kubernetes = {
    host                   = "https://${google_container_cluster.gke.endpoint}"
    token                  = data.google_client_config.this.access_token
    cluster_ca_certificate = base64decode(google_container_cluster.gke.master_auth[0].cluster_ca_certificate)
  }
}
