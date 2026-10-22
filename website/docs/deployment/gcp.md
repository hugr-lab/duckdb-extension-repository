---
title: GCP
---

# GCP

kista on Google Cloud runs on GKE Autopilot with the [Helm chart](kubernetes.md), with no static
credential of the service's own. The Terraform recipe in
[`deploy/gcp-gke`](https://github.com/hugr-lab/duckdb-extension-repository/tree/main/deploy/gcp-gke)
makes the cluster, the database, the key, the bucket and the load balancer, and installs the chart,
in one `terraform apply`.

Not Cloud Run: its requests and responses are capped at 32 MiB over HTTP/1 (an extension is often
larger), it redirects http to https (DuckDB's bootstrap follows no redirect), and its disk is
memory (the spool would cost as much). The design is in
[spec 0020](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0020-cloud-recipes/spec.md).

## What the recipe makes

| | |
| --- | --- |
| **The cluster** | GKE Autopilot with private nodes and Workload Identity, on the REGULAR release channel |
| **The identity** | a service account `<prefix>-kista`, no key: kista's Kubernetes service account acts as it through Workload Identity. It holds `cloudkms.signerVerifier` and `cloudkms.viewer` on the key ring, `storage.objectUser` on the bucket, and `cloudsql.instanceUser` with a condition naming the instance |
| **Cloud KMS** | a key ring `<prefix>` and a channel key `<prefix>-channel`: `RSA_SIGN_PKCS1_2048_SHA256`, protection level `HSM`, label `kista-purpose=channel-signing` |
| **GCS** | a bucket `<project>-<prefix>`: uniform access, public access prevented, GCS's default soft delete (7 days) |
| **Cloud SQL** | PostgreSQL 17 on a private IP (private services access), TLS only, IAM authentication, backups with point-in-time recovery; the Google-managed CA (`GOOGLE_MANAGED_CAS_CA`), whose certificates name the instance's DNS name, served by a private Cloud DNS zone on the network |
| **db-init** | a one-time Job (`postgres:17-alpine`, `verify-full`) that grants the IAM user what kista's migrations need, then replaces the built-in `postgres` user's password with one printed nowhere |
| **The chart** | from `oci://ghcr.io/hugr-lab/charts` at `chart_version` (or a local path), with a GKE Ingress, a global static IP, a Google-managed certificate, the `BackendConfig`, an SSL policy of TLS 1.2 and up, and the NetworkPolicy for Google's front ends |
| **The network** | a VPC with a subnetwork, Cloud NAT and private services access, unless `network` names yours |

The database login is the service account itself: kista asks the metadata server for a token
scoped to Cloud SQL login only, for each connection ([State](../state.md#logins)). The DSN names
the instance's name on private services access (`….sql-psa.goog.`, from its `dnsNames`) with its
trailing dot, as the certificate does, and `store.ca_pem` holds the instance's CA. Cloud SQL leaves
that name's record to the customer: the recipe's private zone serves it.

## Deploy

Terraform 1.5 or later (or OpenTofu), credentials for the project (`gcloud auth
application-default login`, or a token in `GOOGLE_OAUTH_ACCESS_TOKEN`), and a project with
billing. A project of kista's own is the clean way out: Cloud KMS never deletes a key ring or a
key, so the recipe names its key ring `<prefix>-<random>` and a re-apply after a destroy makes a
new one. Keep the state in a
remote, encrypted, access-controlled backend: it holds the built-in user's password (live until
db-init succeeds, dead after) and the access token Terraform used (about an hour), and maps the
deployment.

```sh
cd deploy/gcp-gke
cp examples/trial.tfvars my.tfvars   # project, domain, image, chart_version, server_issuers, server_admins
terraform init
terraform apply -var-file my.tfvars
terraform output ip                  # point the domain here
```

- **`domain`** is kista's public URL. With **`dns_zone`** (a Cloud DNS zone in the project) the
  recipe writes its A record; otherwise point the name at `terraform output ip`. Google issues the
  certificate once the name resolves to that address, which can take tens of minutes; until then
  https fails and http works.
- **`image`** is a version or a digest: `ghcr.io/hugr-lab/kista:vX.Y.Z` or `…@sha256:…`. Moving
  tags (`:edge`, `:latest`) are refused unless pinned by digest.
- **`server_issuers`** and **`server_admins`** are `auth.server_issuers` and `auth.server_admins`
  ([Configuration](../configuration.md#auth)), as YAML in a string. A server issuer needs
  `required_claims`.
- **`server_license_admins`** is `auth.server_license_admins`, as YAML in a string: the
  [licence managers](../licensing.md#licences-in-a-tenant), principals of the server issuers who
  manage every tenant's licences and nothing else. Empty (the default), there are none, and the
  configuration leaves the key out.
- **`server_distribution_admins`** is `auth.server_distribution_admins`, as YAML in a string: the
  [distribution admins](../access.md), principals of the server issuers who reserve names in tenants
  and manage the publishers they mark. Empty (the default), there are none, and the configuration
  leaves the key out.
- **`master_authorized_cidrs`** limits who may reach the cluster's API server; Terraform must be
  among them. Empty (the default), the API server takes connections from any address, guarded by
  Google's authentication and IAM.

## The first administration

With a server administrator configured, everything is done through the [API](../api.md) and the
[console](../console.md). The channel's key is the recipe's (`terraform output channel_signer`,
`gcp:<prefix>-channel/1`):

```sh
curl -X POST -H "Authorization: Bearer $SERVER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"signer_ref": "gcp:kista-channel/1", "active": true}' https://kista.example.com/api/v1/tenants/acme/channels/prod/keys
```

Another key in the key ring is usable at once: the service account's roles are on the key ring.

## Run a command

```sh
$(terraform output -raw get_credentials)
kubectl -n kista exec deploy/kista -- /kista admin -config /etc/kista/kista.yaml blob check
```

Scheduled commands are the chart's `adminJobs` ([Kubernetes](kubernetes.md)).

## The network

- The nodes have no public address; the load balancer is the only public endpoint. The
  NetworkPolicy admits Google's front ends (`35.191.0.0/16`, `130.211.0.0/22`) on 8443 and 8081.
- `serve.trusted_proxies` is those ranges and the static IP's `/32`: the external load balancer
  appends its own address to `X-Forwarded-For`, and kista takes the rightmost untrusted hop. The
  audit records the caller's address.
- http and https both serve, without a redirect: DuckDB's bootstrap needs plain http, and kista
  ignores a token sent over plain http.
- `gke.backendConfig.timeoutSec` is 600 seconds: GKE's default of 30 limits a whole response, and
  would cut large publishes and downloads. On a pod a rollout replaces, `serve.shutdown_timeout` bounds the
  transfers still running ([Commands](../administration/commands.md#kista-serve)).
- **Rolling updates.** GKE's NEG readiness gate can pass before the load balancer has checked a new
  pod; a rollout that then removes the old pods leaves no healthy backend for a few seconds
  (measured: `502 failed_to_pick_backend`). The recipe sets the chart's `minReadySeconds: 60`, so a
  new pod serves through the load balancer before an old one goes, `preStopSeconds: 20`, and the
  `BackendConfig`'s connection draining of 30 seconds.
- Cloud NAT is needed: kista pulls its image from ghcr.io and reaches its issuers and upstreams.
- A given `network` needs Cloud NAT (or another way out) and private services access already;
  the recipe makes neither there. GKE then adds the pods' and services' secondary ranges to the
  subnetwork itself, which a Shared VPC's host subnetwork does not allow.

## Limits

- **The spool.** Autopilot gives a pod at most 10 GiB of ephemeral storage: (2 × `max_ingests` +
  1) × `max_body_mib` must fit, or the plan fails. Larger bodies need fewer concurrent ingests.
- **db-init runs once.** It replaces the built-in user's password, so the Job cannot run again as
  it is. To run it again (a new database, say), replace the password and the Job:
  `terraform apply -replace=random_password.root -replace=kubernetes_job_v1.db_init`.

## For production

- Keep `replicas` at 2 or more; a larger `db_tier`, `availability_type = "REGIONAL"` on the
  database (in `main.tf`), `master_authorized_cidrs`.
- `deletion_protection` stays on: the cluster and the database refuse a destroy, and the bucket
  keeps its objects. The key goes only after the database, so it stays too. A destroy still
  removes the rest before it fails: the chart (kista stops serving), the service account and its
  roles, the DNS record. A key's versions, once destroyed, are scheduled for destruction (30
  days), and every signature the channel served depends on them: restore one with
  `gcloud kms keys versions restore` if that was a mistake.
- **Who may act as kista.** Anyone allowed `iam.serviceAccounts.getAccessToken` on the service
  account acts as kista. So does a pod with service account `kista` in namespace `kista` in **any**
  GKE cluster of the project: Workload Identity names the project's pool, not one cluster. Keep
  kista in a project of its own (also the clean way out for Cloud KMS), or keep who may create that
  namespace in the project's other clusters to the platform's operators.
- GCS keeps deleted objects for 7 days by default (soft delete): lengthen it, or turn on
  versioning, to guard against deletion by a compromised identity (garbage collection deletes
  bodies, so it may).
- Pin the image by digest.

## Destroy

`deletion_protection = false`, `apply`, then `terraform destroy`. The key ring stays (Cloud KMS
never deletes one); deleting the project removes everything.
