# A trial: deletion protection off, so that `terraform destroy` removes it (the KMS key ring stays:
# Cloud KMS never deletes one; a project of kista's own is the clean way out).
project = "acme-kista"
region  = "europe-west1"
domain  = "kista.example.com"
# a version, or until the first release :edge pinned by its digest (ghcr.io/hugr-lab/kista@sha256:…)
image = "ghcr.io/hugr-lab/kista:v0.1.0"
# the published chart's version; before the first release, a local path: chart = "../helm/kista"
chart_version = "0.1.0"
# the server administrators' issuer: a token for audience https://<domain> (the server audience)
server_issuers      = "[{name: corp, url: 'https://login.example.com/realms/corp', required_claims: {azp: kista}, roles_claim: realm_access.roles}]"
server_admins       = "['role:corp|kista-admin']"
deletion_protection = false
# optional: licence managers, who manage every tenant's licences and nothing else (spec 0029)
# server_license_admins = "['role:corp|kista-licenses']"
