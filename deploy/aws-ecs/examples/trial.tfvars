# A trial: deletion protection off and a 7-day key window, so that `terraform destroy` removes it.
region              = "eu-central-1"
domain              = "kista.example.com"
route53_zone_id     = "Z0123456789ABCDEFGHIJ"
image               = "ghcr.io/hugr-lab/kista:v0.1.0"
# the server administrators' issuer: a token for audience https://<domain> (the server audience)
server_issuers      = "[{name: corp, url: 'https://login.example.com/realms/corp', required_claims: {azp: kista}, roles_claim: realm_access.roles}]"
server_admins       = "['role:corp|kista-admin']"
deletion_protection = false
key_deletion_days   = 7
