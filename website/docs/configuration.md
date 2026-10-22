---
title: Configuration
---

# Configuration

kista reads one YAML document (a file, or a variable with `-config env:NAME`) and then applies `KISTA_*` environment overrides. This page lists every
setting: its type, its default and its limits. The pages linked from each section explain how to use
it.

## The file

Both commands take the file with `-config` (or `-config env:NAME`, below); neither looks for a file on its own.

```bash
kista serve -config /etc/kista/kista.yaml
kista admin -config /etc/kista/kista.yaml tenant list
```

- `kista admin` runs as the service's OS user, with the service's file, so both see the same store,
  storage and key sources.
- An empty file keeps the defaults. Note that `store.kind` has no default, so a usable file sets at
  least the store.
- Unknown keys are refused (the YAML decoder runs with known fields only), so a typo fails the start
  instead of being ignored.
- YAML decoding errors and `KISTA_` variable errors never echo values, so a misplaced secret does not
  end up in a log. (Validation errors may quote a non-secret value, such as a CIDR or an origin.)
- The file is read at start. A change takes effect when the process restarts.
- On a platform that mounts no file from configuration (ECS on Fargate, Azure Container Apps),
  `-config env:NAME` reads the same YAML from the environment variable `NAME` instead, at most
  1 MiB. It is decoded exactly as a file, and the `KISTA_` overrides apply after it. The variable
  may start with `KISTA_` (`KISTA_CONFIG`). The configuration holds no secret (they stay in their
  own files and identities), but whoever can describe the task or the app can read it: keep it
  that way.
- With `env:`, whoever can set that variable decides every setting, file-only ones included: the
  file-only boundary becomes "who can edit the task's or the app's environment", the same people
  who could replace a mounted file. A platform that lets less trusted operators set `KISTA_*`
  overrides should not hold the configuration in a `KISTA_` variable. The variable may not be
  named after a setting.

```bash
kista serve -config env:KISTA_CONFIG
```

Value types used in the tables below:

| Type | Written as |
| --- | --- |
| duration | Go duration: `30s`, `10m`, `6h`, `168h` (no `d` unit; 7 days is `168h`) |
| size | bytes as a number, or with a `KiB`, `MiB` or `GiB` suffix: `16KiB`, `100MiB`, `4GiB` |
| path | absolute paths wherever a file or directory is named, unless stated |
| CIDR | `10.0.0.0/8`, `192.168.1.10/32`, `fd00::/8`; host names are never accepted |

For a numeric limit with a default, `0` (or the key left out) means that default. Two exceptions:
the `rotation` minimums and `tenants.min_suspended` take `0` literally (allowed only with
`profile: dev`), and `gc.interval: 0` means no scheduled collection.

## Environment overrides

Every setting that is not file-only can be overridden by a variable named after its path:

- the prefix `KISTA_`;
- each key in upper case;
- `__` (two underscores) between levels.

| Setting | Variable |
| --- | --- |
| `store.kind` | `KISTA_STORE__KIND` |
| `store.dsn` | `KISTA_STORE__DSN` |
| `store.login.kind` | `KISTA_STORE__LOGIN__KIND` |
| `store.login.password_env` | `KISTA_STORE__LOGIN__PASSWORD_ENV` |
| `store.login.password_file` | `KISTA_STORE__LOGIN__PASSWORD_FILE` |
| `store.max_open_conns` | `KISTA_STORE__MAX_OPEN_CONNS` |
| `store.migrate` | `KISTA_STORE__MIGRATE` |
| `store.sqlite.path` | `KISTA_STORE__SQLITE__PATH` |
| `azure.cloud` | `KISTA_AZURE__CLOUD` |
| `azure.identity.client_id` | `KISTA_AZURE__IDENTITY__CLIENT_ID` |
| `azure.identity.tenant_id` | `KISTA_AZURE__IDENTITY__TENANT_ID` |
| `rotation.min_trusted` | `KISTA_ROTATION__MIN_TRUSTED` |
| `rotation.min_demoted` | `KISTA_ROTATION__MIN_DEMOTED` |
| `serve.public_url` | `KISTA_SERVE__PUBLIC_URL` |

That is the complete list. Everything else is **file-only**: `profile`, `azure.identity.kind`, `store.ca_pem`, and
the whole of `aws`, `gcp`, `signers`, `blob`, `egress`, `auth`, `publish`, `upstreams`, `events`, `statistics`,
`telemetry`, `gc`, `ui`, `attachments`, `tenants`, and every `serve` setting but `public_url`. Each of these
switches a guard on or off, or decides what kista may reach, so it cannot be changed by whoever
controls the process environment.

The rules of the loader:

- The environment wins over the file.
- A `KISTA_` variable that names a file-only setting is an error (`... can only be set in the config
  file`), not a silent no-op.
- A `KISTA_` variable that names no setting is an error (`KISTA_STORE__DNS is not a kista setting`).
  The exceptions are the variable that `store.login.password_env` names (you may call it
  `KISTA_DB_PASSWORD`, and it is read as the password, not as a setting) and the one
  `-config env:` names, which holds the configuration.
- Variables starting with `KISTA_TEST_`, `KISTA_E2E_` and `KISTA_LIVE_` are test switches and are
  ignored by the loader.
- Durations, integers and booleans are parsed as in the file; a bad value is reported by variable
  name, never by value.

```bash
KISTA_STORE__DSN='postgres://kista@db.internal:5432/kista?sslmode=verify-full' \
KISTA_STORE__LOGIN__PASSWORD_ENV=KISTA_DB_PASSWORD \
KISTA_DB_PASSWORD='...' \
kista serve -config /etc/kista/kista.yaml
```

## Profile

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `profile` | `prod` or `dev` | `prod` | File-only. `dev` relaxes guards for a developer's machine. |

`profile: dev` logs a warning at every start of `kista serve` and `kista admin`. What it changes:

| Area | `prod` | `dev` |
| --- | --- | --- |
| Store | any reachable PostgreSQL or SQL Server, or SQLite | SQLite, or PostgreSQL / SQL Server on loopback hosts only (every host of a multi-host DSN and the failover partner count) |
| Rotation minimums | at least `24h` each | any value, `0` included |
| `tenants.min_suspended` | at least `24h` | any value, `0` included |
| File signers (`file:` references) | need `signers.allow_file: true` and `signers.file_dir` | need `signers.file_dir` only |
| Vault / OpenBao | `https` address; `software_keys: true` required | `http` on loopback allowed; `software_keys` not required |
| Azure Key Vault | `require_hsm` stays on | `require_hsm: false` allowed |
| Azure identity | `managed`, or `workload` with `client_id` and `tenant_id` | `default` (the developer credential chain) allowed; `workload` without ids allowed |
| AWS identity (KMS sources, S3 domains, the `aws` database login) | a platform identity; steering `AWS_*` variables refused | steering variables not checked; for `awskms`, `identity.kind: static` (key files) and `endpoint_url` on loopback (the moto emulator) allowed |
| `serve.public_url` | `https` | `http` on loopback allowed |
| Egress | no plain http | `egress.allow_loopback_http: true` allowed, which also lets server issuers, OTLP sinks and credential token URLs use http on loopback |
| S3 endpoint | `https` | `http` on loopback allowed |
| Azure Blob | identity required | the Azurite emulator (`endpoint` + `account_key_file`) allowed |
| Default blob root and spool | `/var/lib/kista/blobs`, `/var/lib/kista/spool` | with SQLite at an absolute path: `blobs/` and `spool/` beside the database file |

The dev profile never runs against a production database: the loopback check is made on the
driver's own parsing of the DSN, not on its text.

## store

The metadata database. See [State](state.md) for the engines, DSNs, logins and replicas.

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `store.kind` | `postgres`, `sqlserver`, `sqlite` | none (required) | The engine. |
| `store.dsn` | string | none | PostgreSQL or SQL Server connection string. Required for those two. Must not contain a password. |
| `store.login.kind` | `password`, `entra`, `aws`, `gcp`, `vault` | `password` | How the password or token is obtained. `entra` and `vault` need `postgres` or `sqlserver`; `aws` (RDS IAM) and `gcp` (Cloud SQL IAM) need `postgres` and a DSN with one host. |
| `store.login.password_env` | variable name | none | The variable holding the password, re-read on every new connection. Only with `password`. |
| `store.login.password_file` | path | none | A file holding the password (trailing newlines are trimmed), re-read on every new connection. Not together with `password_env`; only with `password`. A secret file: see [Files kista reads](security.md#files-kista-reads). |
| `store.login.vault` | block | none | The password from HashiCorp Vault or OpenBao; only with `vault`, and file-only. Its settings are below. |
| `store.ca_pem` | PEM certificates | none | The PostgreSQL server's CA, inline, instead of a file named by the DSN's `sslrootcert` (not both). The only CA the server's certificate is checked against; `verify-full` still applies. |
| `store.max_open_conns` | integer | `10` | The connection pool size (PostgreSQL and SQL Server). |
| `store.migrate` | `auto`, `check` | `auto` | `auto` applies pending migrations at start; `check` only verifies the schema. |
| `store.sqlite.path` | path | none | The SQLite file. Required for `sqlite`. May not contain `?`, `#` or `%`. |

`store.login.vault` (kind `vault`; see [State](state.md#the-password-from-vault-or-openbao)). The
connection settings are a `vault` key source's, checked by the same rules; the login has a Vault
client of its own.

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `address` | URL | required | `https://host[:port]`, no path. `http` only on loopback with `profile: dev`. |
| `namespace` | string | none | A Vault Enterprise or OpenBao namespace: letters, digits, `_`, `-` and `/`. |
| `ca_file` | absolute path | none | The CA bundle to trust for the address. Public material. |
| `auth.kind` | `kubernetes`, `jwt`, `token_file` | required | As for a key source: no static token in the config, no AppRole. |
| `auth.mount` | string | `kubernetes` or `jwt` | The auth mount path, relative. |
| `auth.role` | string | none | Required for `kubernetes` and `jwt`: a role for the database login only. |
| `auth.token_file` | absolute path | required | The projected service-account token, the JWT, or Vault Agent's token. |
| `kv.mount`, `kv.path`, `kv.field` | strings | required for `kv` | A KV v2 secret: `GET /v1/<mount>/data/<path>`, the string field `field`. Relative paths, no `..`. |
| `kv.version` | integer | `0` | A pinned KV version; `0` reads the latest. |
| `static_role.mount`, `static_role.name` | strings | required for `static_role` | A database secrets engine static role: `GET /v1/<mount>/static-creds/<name>`. |
| `cache` | duration | `1m` | `kv` only: how long the password is kept, `0s` to `10m`. A static role's password is kept until its TTL less 30 seconds. |

Exactly one of `kv` and `static_role`. `dynamic_role` (dynamic credentials) is refused: it is not in
this version.

## azure

The service's own Azure identity. It is used by the `entra` database login and by upstream
credentials with `client_auth: azure`. Key sources, S3 domains and Azure Blob domains have their
own `identity` blocks.

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `azure.cloud` | `public`, `china`, `usgov` | `public` | The Azure cloud of the Entra database login. |
| `azure.identity.kind` | `managed`, `workload`, `default` | none | File-only. `default` (the developer credential chain) only with `profile: dev`. |
| `azure.identity.client_id` | string | none | The user-assigned identity's or the app's client id. Required for `workload` outside dev. |
| `azure.identity.tenant_id` | string | none | The Entra tenant. Required for `workload` outside dev. |

## aws and gcp

The service's own AWS and Google Cloud identities, for what is not a key source or a blob domain:
today the `aws` and `gcp` database logins ([State](state.md#logins)). Both sections are file-only.

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `aws.region` | string | none | A region such as `eu-central-1`. Required with `aws.identity`. |
| `aws.identity` | block | none | As a key source's `identity`: `pod_identity`, `irsa` (with `role_arn`), `imds`, `ecs`; `static` only with `profile: dev`. |
| `gcp.principal` | string | none | The service account kista acts as (`<name>@<project>.iam.gserviceaccount.com`); required outside dev. |
| `gcp.credentials_file` | path | none | An `external_account` file (a `service_account` key only with `profile: dev`); absent, the metadata server. As a `gcpkms` source's. |
| `gcp.endpoint_url` | URL | none | The tests' fake: only with `profile: dev`, on loopback, without `credentials_file`. |

## rotation

Key-lifecycle minimums. See [Key rotation](administration/key-rotation.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `rotation.min_trusted` | duration | `168h` | How long a key is published as trusted before it may be activated. |
| `rotation.min_demoted` | duration | `168h` | How long a demoted key stays published before it may be retired. |

Outside `profile: dev`, neither may be below `24h`. Neither may be negative.

## signers

Where signing keys may come from. The whole block is file-only. See
[Signing keys](signing-keys.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `signers.allow_file` | boolean | `false` | Allow `file:` signer references outside `profile: dev`. |
| `signers.file_dir` | absolute path | none | The only directory `file:` references resolve in. Without it, `file:` references are refused in every profile. |
| `signers.sources` | list | none | Named vault, KMS or Kubernetes Secret sources. |

Each entry of `signers.sources`:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | `[a-z][a-z0-9-]{0,15}`, unique. Reserved: `file`, `vault`, `azurekv`, `awskms`, `gcpkms`, `kubesecret`, `pkcs11`, `http`, `https`, `arn`, `projects`. A signer reference is `<name>:<key>`. |
| `kind` | `vault`, `azurekv`, `awskms`, `gcpkms`, `kubesecret` | required | |
| `allow` | list of key-name prefixes | required | Which keys of the source may be used; `"*"` for any. For `azurekv`, prefixes are written in lower case. For `awskms`, whole key ids (`mrk-` ids only with `allow_multi_region`) or `"*"`. For `gcpkms`, CryptoKey id prefixes. |
| `max_concurrency` | integer | `4` | Signing calls in flight to this source. |
| `recheck` | duration | `10m` | How often key properties are re-read; `0..10m`. |
| `timeout` | duration | `10s` | Per call; `1s..60s`. |
| `health_key` | string | none | Accepted by the loader; not used by this version. |
| `identity` | block | none | Required for `azurekv` and `awskms`; refused for `vault`, `gcpkms` and `kubesecret`. Its fields are below. |
| `vault` | block | | Exactly one block matching `kind`. |
| `azurekv` | block | | |
| `awskms` | block | | |
| `gcpkms` | block | | |
| `kubesecret` | block | | |

`identity` block of a source:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `kind` | `managed`, `workload`, `default` (Azure); `pod_identity`, `irsa`, `imds`, `ecs`, `static` (AWS) | required | `default` and `static` only with `profile: dev`. |
| `client_id` | string | none | Azure only, as in `azure.identity`. Required for `workload` outside dev. |
| `tenant_id` | string | none | Azure only. Required for `workload` outside dev. |
| `role_arn` | IAM role ARN | none | AWS `irsa` only, and required there: the role the ServiceAccount is bound to, in the region's partition. `AWS_ROLE_ARN` must equal it. |
| `access_key_file` | absolute path | none | AWS `static` only, and required there. A secret file ([Files kista reads](security.md#files-kista-reads)). |
| `secret_key_file` | absolute path | none | AWS `static` only, and required there. A secret file ([Files kista reads](security.md#files-kista-reads)). |

`vault` block (HashiCorp Vault or OpenBao Transit):

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `address` | URL | required | `https://host[:port]`, no path. `http` only on loopback with `profile: dev`. |
| `namespace` | string | none | Letters, digits, `_`, `-` and `/`. |
| `mount` | string | required | The Transit mount, a relative path. |
| `ca_file` | path | none | The CA bundle to trust for the address. Owned by root or kista's user. |
| `auth.kind` | `kubernetes`, `jwt`, `token_file` | required | No static tokens in the config, no AppRole. |
| `auth.mount` | string | `kubernetes` or `jwt` | The auth mount path. |
| `auth.role` | string | none | Required for `kubernetes` and `jwt`. |
| `auth.token_file` | absolute path | required | The projected service-account token, the JWT, or Vault Agent's token. A Vault token (`kind: token_file`) is a secret file; a projected token or JWT is a token file: owned by root or kista's user, any read bits; nobody but the owner may write it ([Files kista reads](security.md#files-kista-reads)). |
| `software_keys` | boolean | `false` | Must be `true` outside dev: an acknowledgement that Transit keys are software keys. |

`azurekv` block (Azure Key Vault or Managed HSM):

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `vault` | string | none | The vault name (`https://<vault>.vault.<cloud suffix>`). |
| `managed_hsm` | string | none | Or a Managed HSM name. Exactly one of `vault` and `managed_hsm`. 3-24 letters, digits and dashes. |
| `cloud` | `public`, `china`, `usgov` | `public` | |
| `require_hsm` | boolean | `true` | Only RSA-HSM keys. `false` only with `profile: dev`. |

`awskms` block (AWS KMS):

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `region` | string | required | A region such as `eu-central-1`. The partition follows from it. |
| `account` | string | required | The 12-digit account that holds the keys. Quote it in YAML. |
| `role_arn` | IAM role ARN | none | A role to assume after the identity, in the region's partition. The only way to reach a key in another account. |
| `allow_multi_region` | boolean | `false` | Accept multi-Region keys (`mrk-` ids). |
| `fips` | boolean | `false` | Use the FIPS endpoints of KMS, and of STS in the `us-` regions. Only in the `aws` and `aws-us-gov` partitions; not with `endpoint_url`. |
| `endpoint_url` | URL | none | The moto emulator only: `profile: dev`, `http` or `https` on a loopback host, with `identity.kind: static`. Replaces the KMS and STS endpoints. |

`gcpkms` block (Google Cloud KMS):

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `key_ring` | string | required | `projects/<project id>/locations/<location>/keyRings/<ring>`; the project by its id, not its number. |
| `principal` | service account email | none | The service account kista acts as, `<name>@<project>.iam.gserviceaccount.com`. Required outside dev. |
| `require_hsm` | boolean | `true` | Only `HSM` and `HSM_SINGLE_TENANT` keys. `false` (also `SOFTWARE`) only with `profile: dev`. |
| `credentials_file` | absolute path | none | Absent: the metadata server. Otherwise an `external_account` file; a `service_account` key file only with `profile: dev`. Public material: owned by root or kista's user ([Files kista reads](security.md#files-kista-reads)). A `service_account` key holds a private key, yet is checked as public: a gap accepted in dev only. |
| `endpoint_url` | URL | none | The tests' fake only: `profile: dev`, `http` or `https` on a loopback host, without `credentials_file`. Requests carry a fixed test token. |

`kubesecret` block (a Kubernetes Secret volume; the weakest source, see
[Kubernetes Secrets](signing-keys.md#kubernetes-secrets)):

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `dir` | absolute path | required | The Secret volume's `mountPath`, clean. Must not overlap `signers.file_dir` or another `kubesecret` source's `dir`, nor hold another secret file the configuration names. Checked at every open: a read-only tmpfs mount (Linux only), the kubelet's layout, root-owned files with kista's group and mode `0440` or narrower. |
| `etcd_encryption_at_rest` | boolean | `false` | Must be `true` outside dev: your statement that the cluster encrypts Secrets at rest. kista cannot check it, and logs it at every start. |

## blob

Where extension bodies are stored. The whole block is file-only. See [Storage](storage.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `blob.domains` | list | one `fs` domain named `default` at `/var/lib/kista/blobs` | Named stores. When set, it must include `default`. |
| `blob.spool_dir` | absolute path | `/var/lib/kista/spool` | Local scratch for uploads and downloads; one per replica. |
| `blob.max_body` | size | `1GiB` | The largest body accepted; at most `4GiB`. |
| `blob.max_ingests` | integer | `4` | Bodies taken in at once; `1..256`. |

Each domain:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | `[a-z][a-z0-9-]{0,15}`, unique. Tenants choose a domain at creation; `default` otherwise. |
| `kind` | `fs`, `s3`, `azureblob`, `gcs` | required | Exactly one block matching `kind`. |
| `timeout` | duration | `30s` | Remote stores only: connect, headers and idle reads/writes; `1s..5m`. |
| `fs.root` | absolute path | required | A clean absolute directory (a local disk or a mounted volume). |
| `s3.endpoint` | URL | required | `https://host[:port]`; `http` only on loopback with dev. |
| `s3.bucket` | string | required | A valid bucket name. |
| `s3.prefix` | string | empty | Empty, or lower-case segments each ending with `/`. |
| `s3.region` | string | required | Cloudflare R2: `auto`. |
| `s3.lookup` | `path`, `dns` | required | Addressing style. A bucket with dots over https needs `path`. |
| `s3.sse` | `none`, `s3`, `kms` | required | Server-side encryption. |
| `s3.kms_key_id` | string | none | Required with `sse: kms`, refused otherwise. |
| `s3.access_key_file` | absolute path | required without `identity` | Refused with `identity`. A secret file ([Files kista reads](security.md#files-kista-reads)). |
| `s3.secret_key_file` | absolute path | required without `identity` | Refused with `identity`. A secret file. |
| `s3.ca_file` | absolute path | system roots | Trust only this CA bundle. Refused with `identity`. Owned by root or kista's user. |
| `s3.identity` | block | none | An AWS platform identity instead of the key files: `kind` `pod_identity`, `irsa` (with `role_arn`), `imds` or `ecs`; not `static`. The endpoint must then be AWS S3 in `s3.region`: `https://s3.<region>.amazonaws.com` or its dualstack, FIPS or `.amazonaws.com.cn` form, no port. |
| `s3.role_arn` | IAM role ARN | none | With `identity` only: a role to assume after it, in the region's partition. |
| `s3.account` | string | none | With `identity` only, and required there: the bucket owner's 12-digit account, sent as `x-amz-expected-bucket-owner`. |
| `azureblob.account` | string | required | 3-24 lower-case letters and digits. |
| `azureblob.container` | string | required | 3-63 lower-case letters, digits and single dashes. |
| `azureblob.prefix` | string | empty | As for S3. |
| `azureblob.cloud` | `public`, `china`, `usgov` | `public` | |
| `azureblob.identity` | block | required | `managed` or `workload` (`default` in dev). |
| `azureblob.endpoint` | URL | none | Azurite only, dev only: `http://<loopback host:port>/<account>`. |
| `azureblob.account_key_file` | absolute path | none | Azurite only, dev only, with `endpoint` and without `identity`. A secret file. |
| `gcs.bucket` | string | required | 3-63 lower-case letters, digits, `-` and `_`; no dots, no `goog`. |
| `gcs.prefix` | string | empty | As for S3. |
| `gcs.principal` | service account email | none | As in `gcpkms`. Required outside dev. |
| `gcs.credentials_file` | absolute path | none | As in `gcpkms`. Absent: the metadata server. |
| `gcs.endpoint_url` | URL | none | fake-gcs-server only: dev only, on loopback, without `credentials_file`. |

Two domains may not share a store (an overlapping `fs.root`, or the same bucket or container with
overlapping prefixes): one's garbage collection would delete the other's objects.

## serve

`kista serve`: listeners, proxies and limits. Every setting but `public_url` is file-only. See
[Security](security.md) for TLS and proxies, and [Using kista from DuckDB](duckdb.md) for the http
bootstrap.

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `serve.public_url` | URL | none | `https://host[:port]`, no path, no trailing slash (`http` only on loopback with dev). The base of tenants' canonical token audiences and the default server audience. |
| `serve.listeners` | list | none | At least one is required by `kista serve`. |
| `serve.trusted_proxies` | list of CIDRs | none | Peers whose `X-Forwarded-For` gives the client address. Required with a `behind_proxy` listener. |
| `serve.resign` | boolean | `false` | Run the re-signer on this replica (signs releases with a channel's active key). |
| `serve.max_downloads` | integer | `256` | Concurrent body downloads per replica; more get `503` with `Retry-After`. |
| `serve.max_downloads_per_client` | integer | `8` | Concurrent body downloads per client address. |
| `serve.min_rate` | size | `16KiB` | Least average rate (per second) of a download, and of a publication upload or a bundle chunk through the API. |
| `serve.write_idle_timeout` | duration | `30s` | The window `min_rate` is measured over; at least `1s`. |
| `serve.drain_timeout` | duration | `5s` | `/readyz` fails this long before shutdown starts. |
| `serve.shutdown_timeout` | duration | `30s` | Then downloads still running are aborted. It also bounds the wait for the background runners after it; the event writer and the download counter then get 11 seconds of their own ([kista serve](administration/commands.md#kista-serve)). |
| `serve.api_rate` | number | `20` | Requests per second per client on `/api/`. Set together with `api_burst`. |
| `serve.api_burst` | integer | `40` | Burst for `api_rate`. |

Each listener:

| Setting | Type | Meaning |
| --- | --- | --- |
| `addr` | `host:port` | Unique. `":443"` listens on every address. |
| `scheme` | `https`, `http`, `dual`, `health` | `https` has exactly one of `tls` and `behind_proxy`. `http` has neither and serves public releases only. `dual` needs `tls`: one port serving TLS or plain http by the first byte. `health` has neither: probes only (below). |
| `tls.cert_file`, `tls.key_file` | absolute paths | Reloaded when the files change (checked at most once a minute); a failed reload is logged as a warning and the current certificate stays. The certificate is public material, the key a secret file ([Files kista reads](security.md#files-kista-reads)). |
| `behind_proxy` | boolean | TLS is terminated in front; `X-Forwarded-Proto` can only downgrade a request to http. |

A `health` listener is a probe port, for the kubelet's liveness and readiness probes and a load
balancer's health check. Never expose it in a Service or to clients: a load balancer that checks
it must reach the pod port directly (see the chart notes in
[spec 0013](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0013-deployment/spec.md)).

- It is plain http. It answers only `GET` and `HEAD` on `/healthz` and `/readyz`, with no query
  string (not even an empty `?`), the same answers as on the other listeners. Paths are matched
  as on the other listeners, without unescaping. Everything else gets `404` with no body,
  `OPTIONS *` included; a `GET` with a body gets `400`, as on the other listeners.
- It writes no access log, no event and no metric.
- Its timeouts are short: 5 seconds to read a request or write an answer, 30 seconds idle, and
  `MaxHeaderBytes` 1 KiB (Go's server reads up to 4 KiB more).
- It takes neither `tls` nor `behind_proxy`. There is at most one, and a configuration with only a
  `health` listener is refused.
- `/healthz` and `/readyz` keep answering on every other listener.

```yaml title="kista.yaml"
serve:
  trusted_proxies: [10.0.0.0/8]
  listeners:
    - { addr: ":8443", scheme: https, behind_proxy: true }
    - { addr: ":8081", scheme: health }
```

## egress

The outbound client for requests kista makes on a tenant's behalf: issuers' JWKS, upstreams,
feeds, OTLP sinks. The whole block is file-only. See [Upstreams](upstreams.md) and
[Security](security.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `egress.allow` | list | none | Private networks that may be reached. Public addresses need no entry. |
| `egress.allow[].cidr` | CIDR | required | Never a host name. |
| `egress.allow[].ports` | list of ports | `[443]` | |
| `egress.allow_loopback_http` | boolean | `false` | Plain http to loopback; `profile: dev` only. |
| `egress.proxy` | URL | none | An explicit `http://host:port` proxy. `HTTP_PROXY` and friends are never read. |
| `egress.ca_file` | absolute path | system roots | Trust only this CA bundle (a private IdP). Owned by root or kista's user. |
| `egress.timeout` | duration | `10s` | Per request; `1s..60s`. |

The deployment's own `public_url` host is always refused.

## auth

Server identity: the issuers whose tokens can make someone a server administrator or a licence
manager. The whole block is file-only and is read at start, so no API caller can widen a server
role. See [Access](access.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `auth.server_issuers` | list | none | OpenID issuers at the server level. |
| `auth.server_audiences` | list | `[serve.public_url]` | Audiences a server token must carry. Required (or `public_url`) with server issuers. Values under `public_url/` are refused: those are tenants' audiences. |
| `auth.server_egress_allow` | list | none | Like `egress.allow` (`cidr`, `ports`), reached only by the server issuers' fetcher. |
| `auth.server_admins` | list | none | Principals of server issuers who are server administrators. |
| `auth.server_license_admins` | list | none | Principals of server issuers who are licence managers: they manage every tenant's licences and nothing else (spec 0029). |
| `auth.server_distribution_admins` | list | none | Principals of server issuers who are distribution admins: on every tenant they reserve names, mark publishers and manage the marked ones, and grant `publish`, `promote` and `admin` on extensions; nothing else (spec 0030). Not also a licence manager. |
| `auth.admin_token_max_age` | duration | `1h` | How recently a management token must have been issued; `1m..24h`. |

Each server issuer:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | `[a-z][a-z0-9-]{0,15}`, unique. |
| `url` | URL | required | The issuer URL, https (http on loopback only with dev and `egress.allow_loopback_http`). Unique, and not a CI token issuer's URL (GitHub Actions', GitHub Enterprise's). |
| `jwks_uri` | URL | discovered | |
| `algorithms` | list | all supported | Of `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `EdDSA`. |
| `required_claims` | map | required | Claims every token must carry, at most 16. Mandatory for a server issuer. |
| `roles_claim`, `groups_claim`, `client_claim` | claim path | none | Keys joined by dots (`realm_access.roles`), or a JSON array of keys when a key has dots. |
| `max_token_lifetime` | duration | `24h` | `1m..168h`. |

A server administrator is written `kind:issuer|value`, where `kind` is `subject`, `role`, `group`
or `client`, and `issuer` is a server issuer's name: `role:ops|kista-admin`. `issuer:<name>` is
refused, since every account of the issuer would be an administrator, and so is `publisher:`. A
`role:`, `group:` or `client:` principal needs its issuer's `roles_claim`, `groups_claim` or
`client_claim`; without it the principal could never match and is refused. A licence manager and a
distribution admin are written and checked the same way: `role:ops|kista-licenses`; a token whose
principals make it both is refused.

## publish

Publication limits. The whole block is file-only. See
[Publishing](publishing.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `publish.max_per_principal` | integer | `2` | Uploads at once per principal; `1..64`, not above `max_per_tenant` (each at its default when left out: `max_per_principal: 16` alone is refused, as is `max_per_tenant: 1` alone). |
| `publish.max_per_tenant` | integer | `8` | Uploads at once per tenant; `1..256`. |
| `publish.providers` | | | Removed with trusted publishing (spec 0023): a configuration that sets it, even to `[]`, is refused. Publishers sign their requests with keys. |

## upstreams

Upstream runs and the credentials of private upstreams. The whole block is file-only. See
[Upstreams](upstreams.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `upstreams.concurrency` | integer | `4` | Upstream runs at once per replica, and cells fetched at once by them; `1..32`. |
| `upstreams.fetch_timeout` | duration | `10m` | One file's download; `1m..2h`. |
| `upstreams.min_rate` | size | `64KiB` | A download's least rate, and a bundle import's input's; at least `1KiB`. |
| `upstreams.interval` | duration | `6h` | Between a mirror's scheduled runs; `15m..168h`. |
| `upstreams.negative_ttl` | duration | `1h` | A pull-through miss is not retried within it; `1m..24h`. |
| `upstreams.credentials` | list | none | Named credentials, below. |
| `upstreams.bundle_keys` | list | none | Bundle keys a site accepts ([Bundle mode](upstreams.md#bundle-mode)), at most 10: each `{fingerprint: "sha256:<hex>", tenants: [<name>…] or ["*"]}`. A bundle is imported into a tenant only when its key is pinned for that tenant. Read at start. |
| `upstreams.bundle_sources` | list | none | Which authenticated bundle sources' releases may be public ([Importing a bundle](upstreams.md#importing-a-bundle)), at most 100: each `{prefixes: [<https URL>…], tenants: [<name>…] or ["*"], allow_public: true}`. A prefix covers a source at or below its path, on the same host and port. Without an entry, a release from an authenticated cell stays private. Read at start (by the CLI, when it runs). |

Each credential:

| Setting | Type | Meaning |
| --- | --- | --- |
| `name` | string | `[a-z][a-z0-9-]{0,31}`, unique. |
| `tenants` | list | Tenant names that may use it, or `"*"`. Required. |
| `prefixes` | list of URLs | https upstream prefixes it is sent to. Required. |
| `allow_public` | boolean | Releases it brings may be made public. Default `false`. |
| `kind` | `token_file`, `client_credentials` | |
| `token_file` | absolute path | `token_file`: the bearer token, read from the file. A token file: owned by root or kista's user, any read bits; nobody but the owner may write it ([Files kista reads](security.md#files-kista-reads)). |
| `token_url` | URL | `client_credentials`: the token endpoint, https. |
| `client_id` | string | `client_credentials`: required. |
| `scope`, `audience` | string | Optional token request parameters. |
| `client_auth` | `azure`, `file`, `key_file`, `secret` | How the client authenticates. `azure` uses `azure.identity`. |
| `assertion_file` | absolute path | `file`: a client assertion read from the file. A token file, as `token_file`. |
| `key_file` | absolute path | `key_file`: ZITADEL's JSON key or a PEM key. A secret file ([Files kista reads](security.md#files-kista-reads)). |
| `key_id` | string | `key_file` with a PEM key. |
| `assertion_audience` | string | `key_file`: the JWT's `aud`; default `token_url`. |
| `client_secret_file`, `client_secret_env` | path / variable | `secret`: exactly one. The file is a secret file. |
| `allow` | list | The token endpoint's private addresses (`cidr`, `ports`), beside `egress.allow`. `client_credentials` only. |

## events

The event buffer and its sinks. The whole block is file-only. See
[Observability](observability.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `events.retention` | duration | `720h` (30 days) | `48h..8760h`; at most `2160h` (90 days) when sinks are configured. |
| `events.client_addresses` | `full`, `truncated`, `none` | `truncated` | How client addresses are kept in events. |
| `events.max_rows_per_tenant` | integer | `1000000` | `1..100000000`. |
| `events.resource` | map | none | Attributes every OTLP export carries (`deployment.environment.name`, ...). |
| `events.sinks` | list | none | At most 16. |

Each sink:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | `[a-z][a-z0-9-]{0,31}`, unique. |
| `kind` | `otlp`, `jsonl` | required | |
| `tenants` | list | none | Tenant names, or `"*"`. |
| `server` | boolean | `false` | The server's own events too. |
| `unmasked` | boolean | `false` | Send server administrators', licence managers', distribution admins' and the CLI's identities on tenants' events as they are (by default they show as `server`, `license-manager` or `distribution-admin`). |
| `url` | URL | | `otlp`: an OTLP/HTTP logs endpoint, https. |
| `allow` | list | none | `otlp`: private addresses of the endpoint (`cidr`, `ports`). |
| `headers_file` | absolute path | none | `otlp`: `Name: value` lines. A secret file ([Files kista reads](security.md#files-kista-reads)). |
| `timeout` | duration | `10s` | `otlp`: `1s..60s`. |
| `path` | absolute path or `-` | | `jsonl`: the file, or `-` for stdout. Unique per sink. |
| `max_size` | size | `100MiB` | `jsonl`: a file's size before it rotates. |
| `keep` | integer | `5` | `jsonl`: rotated files kept, `0..100` (`0` means 5). |

## statistics

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `statistics.retention` | duration | `26280h` (3 years) | How long download counts and installers, and usage counts, users and received aggregates, are kept; 30 days to 10 years. |
| `statistics.usage_accept` | bool | `true` | `false` answers licensed extensions' usage reports and downstream kistas' daily usage, and keeps nothing ([Licensing](licensing.md#usage-reports)). |

File-only. See [Observability](observability.md).

## telemetry

What OpenTelemetry metrics may name. Where they go is set by the standard `OTEL_*` environment
variables. File-only. See [Observability](observability.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `telemetry.metrics.tenants` | list | none | Tenant names, or `"*"`, whose ids, channels and extensions may appear in metrics. |
| `telemetry.metrics.labels` | list | none | Optional labels of `kista.downloads`: `version`, `platform`, `duckdb_version`. |
| `telemetry.metrics.max_series` | integer | `10000` | Series per instrument before the overflow series counts; `100..1000000`. |

## gc

Storage garbage collection. File-only. See [Storage](storage.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `gc.interval` | duration | `0` | Between scheduled passes; `0` means none (only `kista admin blob gc`), otherwise `1h..168h`. |
| `gc.grace` | duration | `24h` | How long what became unreferenced is kept; `4h..720h`. |

While `gc.interval` is `0`, `kista admin blob gc -apply` and `kista admin release purge` need
`-force`, because kista cannot tell that every replica runs a binary that knows about collection.

## ui

The administration console. File-only. See [Console](console.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `ui.enabled` | boolean | `true` | Serve the console and its routes. |
| `ui.environment` | string | none | A badge in the console's header; at most 32 characters. |
| `ui.allowed_origins` | list of origins | none | Shells on other origins that may host the console. |
| `ui.frame_ancestors` | list of origins | none | Who may frame the standalone console. |
| `ui.connect_src` | list of origins | none | Extra `connect-src` origins. |
| `ui.server_clients` | list | none | The console's public (PKCE) client at each server issuer. |

An origin is `https://host[:port]` with no path and no `*` (`http` only on loopback).

Each server client:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `issuer` | string | required | A server issuer's name; one client per issuer. |
| `client_id` | string | required | |
| `scopes` | list | `openid`, `profile`, `offline_access` | |
| `audience_parameter` | string | none | The name of the authorization request parameter that asks for an audience, for IdPs that need one. |

## attachments

Documents about bodies. File-only. See [Attachments](attachments.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `attachments.max_size` | integer (bytes) | `4194304` (4 MiB) | The largest document; 64 KiB to 16 MiB. A plain number of bytes: suffixes are not accepted here. |
| `attachments.max_reads` | integer | `8` | Documents held in memory at once; `0..256`. |
| `attachments.feed_interval` | duration | `6h` | Between a feed's scheduled runs; at least `5m`. |

## licenses

Licensing (spec 0012, spec 0021). File-only. Licences verify against the keys the extensions' builds
declare, never a key configured here. See [Licensing](licensing.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `licenses.enabled` | boolean | `true` | Off: the routes extensions read (`<prefix>/-/licenses`) answer as a missing path; imports and relay requests answer `409`, and no relay runs. |
| `licenses.refuse_keys` | list of key ids | none | Key ids (`sha256:<64 lowercase hex>`, as `kista ext inspect` prints) that count nowhere: on import, relay or serving. For a leaked key while builds that declare it are live. |
| `licenses.keys` | list of strings | none | Spec 0012's trusted keys. Accepted and ignored, with a warning at start; the next release refuses it. |
| `licenses.refresh` | duration | `6h` | Between an upstream's licence relay runs; `1h..24h`. |

## bundles

Bundles uploaded over the API (spec 0014 phase 2). File-only. See
[Uploading a bundle](upstreams.md#uploading-a-bundle).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `bundles.max_size` | size | `20GiB` | An upload's largest size; 1 MiB to 1024 GiB. |
| `bundles.chunk_size` | size | `16MiB` | The largest chunk; 1 to 64 MiB. A client may ask for smaller chunks. |
| `bundles.upload_ttl` | duration | `24h` | An upload sending no chunk this long expires, and a failed one kept for retry; `1h..168h`. |
| `bundles.upload_max_age` | duration | `168h` | An upload not completed this long after its creation expires; at least `upload_ttl`, at most `720h`. |
| `bundles.max_uploads` | integer | `2` | A tenant's uploads not yet imported (failed ones included); `1..20`. |
| `bundles.max_bytes` | size | `100GiB` | The declared sizes of the server's uploads not yet imported; at least `max_size`. |

## tenants

The tenant lifecycle. File-only. See
[Deleting a tenant](administration/tenants-and-channels.md#deleting-a-tenant).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `tenants.min_suspended` | duration | `168h` | How long a tenant stays suspended before it may be deleted. |

Outside `profile: dev` it may not be below `24h`; in dev it may be `0`. It may not be negative. The
process that takes the delete applies its own value (`kista admin tenant delete`, or the replica
that answers `DELETE /api/v1/tenants/{t}`), so `kista admin` and `kista serve` should share the
configuration.

## reserved

The server's reserved extension names (spec 0030). File-only. See
[Reserved names](publishing.md#reserved-names).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `reserved.community_sync.enabled` | boolean | `false` | Follow duckdb/community-extensions: reserve its new extension names as community. |
| `reserved.community_sync.interval` | duration | `24h` | Between reads; `1h..168h`. |
| `reserved.community_sync.url` | URL | `https://codeload.github.com/duckdb/community-extensions/tar.gz/HEAD` | A gzip tarball of the repository's default branch, as `git archive` writes it (its commit in the pax header): GitHub's, or a mirror's. `https`. |

When enabled, one replica per interval reads the tarball through egress (a public host needs no
`egress.allow` entry; a mirror on a private network does), reads each
`extensions/<dir>/description.yml`'s name, and reserves the names the server does not hold yet as
community, with the source `community-sync@<commit>`: one `reserved.add` event (`sync:community`,
actor `system:community-sync`) with the names. A failed read is logged and tried again at the next
interval; it never removes a name, and a name already reserved (core, or community from another
source) is left as it is. A server that does not sync can add the names by hand: `kista duckdb
community-extensions | ...` then `kista admin reserved add <name>... -kind community`.

```yaml title="kista.yaml"
reserved:
  community_sync:
    enabled: true        # off by default
    interval: 24h
```

## bootstrap

What a first start creates, so an install made from a template (the Azure recipe, the Marketplace
application) starts with its tenant. File-only. It is applied **once**: afterwards the API and the
console own what it made, and it never undoes an administrator's change.

```yaml
bootstrap:
  versions:                              # server-wide, as version add
    - version: v1.4.1
      kind: release
      c_apis: [v1.2.0]
      core_extensions: [autocomplete, core_functions, icu, json, parquet]   # abridged: list all that kista duckdb core-extensions v1.4.1 prints
      core_extensions_commit: b390a7c3760bd95926fe8aefde20d04b349b472e       # optional: the commit it read
  tenant:
    name: acme
    display_name: Acme
    domain: default                      # a blob domain; default: default
    audiences: [api://kista-acme]        # as tenant audience add
  channels:
    - name: prod
      kind: signed                       # signed (needs key) or passthrough
      versions: [v1.4.1]                 # added to the channel, never removed
      key: azure:kista-channel/<version> # a signer reference: the channel's first, active key
  attestation_key: azure:kista-att/<v>   # optional, the tenant's active attestation key
  issuer:                                # optional, as issuer add
    name: entra
    url: https://login.microsoftonline.com/<tid>/v2.0
    required_claims: { tid: <tid> }
    roles_claim: roles
    client_claim: azp
    max_lifetime: 12h                    # also jwks_uri, algorithms, groups_claim
    console: { client_id: <id>, audience: api://kista-acme }   # optional, as issuer console; also scopes, audience_parameter
  grants:                                # principals of that issuer, as grant add
    - { principal: "role:entra|kista-admin", verbs: [admin] }  # optional channel and extension
  upstreams:                             # optional, as upstream add (spec 0024)
    - name: core
      kind: duckdb-core                  # duckdb-core, duckdb-community or repository
      channel: prod                      # one of the section's channels
      platforms: [linux_amd64, linux_arm64]
      extensions: [ { name: httpfs } ]   # also versions, allow_reserved
      visibility: public                 # default: private (public on a passthrough channel)
    - name: vendor
      kind: repository
      prefix: https://repo.example.com/vendor/prod
      keys: ["sha256:<fingerprint>"]     # pinned: one of them signs the source
      credential: vendor-acme            # optional: one of upstreams.credentials
      mode: mirror                       # or pull-through
      channel: prod
      platforms: [linux_amd64]
      extensions: [ { name: acl } ]
```

- **Where it runs.** `kista serve` applies it as `system:bootstrap`, after migrations and before its
  listeners bind, so a replica serves only once the bootstrap is done. Replicas that start together
  apply it once: one holds the lease `kista/bootstrap`, and the others wait for its record, up to
  four minutes. `kista admin bootstrap` applies it as the OS user.
- **Check before any write.** Every object is compared with what exists. It is created when absent,
  left alone when the same, and refused when different; one refusal refuses the whole bootstrap.
  Nothing is written, and `serve` exits and lists the differences. Different means, for example:
  - another channel kind;
  - a channel whose keys do not include the section's key active;
  - a key that another channel or tenant holds (compared by fingerprint);
  - an issuer with any field different;
  - another console client;
  - another C API for a major a version already has;
  - a new version without `core_extensions` (spec 0030);
  - a suspended or deleted tenant.

  A version's C APIs and core extensions the server does not have yet are added (`add`), and the
  core extensions reserved.

  The check opens and probes the signers (an attestation key for its own purpose) and fetches a new
  issuer's discovery document and JWKS. For an upstream, it makes the checks `upstream add` makes:
  - a source's `.well-known` is read, and one of the pinned keys must be in it;
  - in a channel that exists, a name another upstream lists, or a replacement holds, is refused;
  - so are the limits.

  An upstream that exists is the same when its kind, prefix, channel, mode, visibility and
  credential are, its keys and platforms include the section's, and each of the section's names is
  on its allowlist with the same versions and `allow_reserved`. More than the section lists is the
  administrator's.
- **Existing tenants.** The bootstrap never creates anything in a tenant it did not make. Only
  `kista admin bootstrap -adopt` does. Server-wide DuckDB versions and C APIs are not a tenant's.
  A tenant whose objects are all the same is just recorded.
- **Failures that pass.** A failure that may pass is tried again for up to four minutes, after which
  `serve` exits without binding:
  - a signer or an issuer that cannot be reached yet (on a first Azure install, a role assignment
    can take minutes to propagate);
  - an upstream's `.well-known` that cannot be reached: no connection, a timeout, a 5xx or 429.
    Any other answer (a 404 included) is a refusal, and so is a host name that does not resolve:
    egress answers it as it answers an address it refuses, so no tenant learns which internal names
    exist. DNS must work when kista starts;
  - the network;
  - a busy database.

  The record is `pending` from before its first write. The next start goes on with the tenant it
  made, and creates only what is still absent.
- **Once.** The record (table `bootstrap`) keeps the applied section and its digest. The digest is
  taken with defaults filled in and Azure and Cloud KMS key versions left out, so a redeployment
  that renders a new key version is no change. When the section changes later:
  - `serve` logs one warning and starts;
  - `kista admin bootstrap` exits `1`, naming the changed fields, until `-accept`.

  `-rearm` forgets the record. The tenant then exists, so the next apply needs `-adopt`, and it
  makes again whatever is absent, a grant revoked since included.
- **Validation at load:**
  - a tenant is required;
  - names, kinds, verbs, versions, C APIs and signer references are checked;
  - nothing is listed twice: a version, an audience, a channel or its version, a grant, a signer
    reference, an upstream, a name within an upstream, or a name on two upstreams of one channel;
  - the domain must be a configured blob domain;
  - an audience may be neither under `serve.public_url` nor a server audience;
  - the issuer passes the checks of `issuer add` (https, `max_lifetime` within 1m to 7d);
  - grants name principals of the bootstrap's issuer only, and never `issuer:` (every account),
    `admin` to a client, or an extension group (the bootstrap makes none);
  - a grant's channel must be one of the section's channels;
  - the console's audience must be one of the tenant's;
  - an upstream passes `upstream add`'s checks of its fields:
    - name, kind, mode (mirror or pull-through), visibility, platforms, a prefix that is https on a
      port egress reaches, and pinned fingerprints;
    - the allowlist: at least one name for a mirror, canonical names, reserved names only from
      their own repository unless `allow_reserved`, and `"*"` only on a pull-through upstream;
    - its channel is one of the section's. A passthrough channel takes `duckdb-core` mirrors only,
      with public releases;
    - a credential names one of `upstreams.credentials` that is for this tenant, covers the prefix,
      and allows public releases when the upstream makes them;
    - the limits: 100 upstreams, 1,000 names, 100 versions per name, 10 keys, and 20,000 cells per
      upstream (and 100,000 per tenant) counted from the section's channels;
  - a console client at a server issuer's URL is refused, since a tenant's console does not sign in
    there (spec 0015). Such a tenant has no console client: its administrators use the API, or a
    server administrator uses the console.

**Upstreams.** A new mirror is due at once, so its first run starts when the upstream workers do,
on whichever replica takes it. A credential's token is first fetched by that run, and a failure
there shows in the run's status and events (spec 0009), not in the bootstrap. The bootstrap reads
each source's `.well-known` at startup, so a source must be reachable then: one at a private
address needs its CIDR in `egress.allow`, and a cluster that denies egress by default needs a policy
of its own that lets kista's pods reach the sources on 443, and DNS (the chart's NetworkPolicy
covers ingress only). A source that cannot be reached is tried again for four minutes; then `serve`
exits.
An upstream added to the section after the bootstrap was applied gets only the warning above: make
it with `kista admin upstream add` or the API. `-rearm` then `-adopt` would make it too, but it also
restores grants revoked since, so run `-dry-run -adopt` first.

It holds no secret: keys are signer references and issuers are public URLs. It does not record who
changed the configuration; the platform's control-plane log does (the Activity Log, the Kubernetes
audit log).

## A complete example

A production replica on PostgreSQL with a password from a file, keys in OpenBao, bodies in S3, TLS
terminated by a proxy, one server issuer and a JSON-lines event sink.

```yaml title="/etc/kista/kista.yaml"
profile: prod

store:
  kind: postgres
  # no password in the DSN; off loopback sslmode=verify-full is required
  dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full
  login: { kind: password, password_file: /run/secrets/kista-db-password }
  max_open_conns: 10
  migrate: auto                    # check, when a separate job runs kista admin migrate

rotation:
  min_trusted: 168h                # at least 24h outside dev
  min_demoted: 168h

tenants:
  min_suspended: 168h              # before a suspended tenant may be deleted; at least 24h outside dev

signers:
  allow_file: false
  sources:
    - name: bao
      kind: vault
      allow: ["kista-"]            # key-name prefixes this source may use
      vault:
        address: https://bao.internal:8200
        mount: transit
        ca_file: /etc/kista/bao-ca.pem
        auth: { kind: kubernetes, role: kista, token_file: /var/run/secrets/kista/vault-token }
        software_keys: true        # required outside dev

blob:
  domains:
    - name: default                # required whenever domains are listed
      kind: s3
      s3:
        endpoint: https://s3.eu-central-1.amazonaws.com
        bucket: acme-kista
        prefix: bodies/
        region: eu-central-1
        lookup: dns
        sse: s3
        access_key_file: /run/secrets/s3-access-key
        secret_key_file: /run/secrets/s3-secret-key
  spool_dir: /var/lib/kista/spool  # local to this replica
  max_body: 1GiB

serve:
  public_url: https://extensions.example.com
  listeners:
    - { addr: ":8443", scheme: https, behind_proxy: true }
  trusted_proxies: ["10.0.0.0/8"]
  resign: true                     # run the re-signer on this replica

egress:
  allow:
    - { cidr: "10.20.0.0/16", ports: [443] }   # a private IdP

auth:
  server_issuers:
    - name: ops
      url: https://idp.example.com/realms/ops
      required_claims: { azp: kista }
      roles_claim: realm_access.roles
  server_admins: ["role:ops|kista-admin"]

ui:
  environment: production
  server_clients:
    - { issuer: ops, client_id: kista-console }

events:
  retention: 720h
  sinks:
    - { name: audit, kind: jsonl, path: /var/log/kista/events.jsonl, tenants: ["*"], server: true }

gc:
  interval: 24h
```

## Validation

`kista serve` and `kista admin` validate the whole configuration before doing anything and report
every problem at once, joined by `;` (problems with `KISTA_` variables are reported first, on their
own):

```text
kista serve: config: store.dsn must not contain a password; use store.login; serve.trusted_proxies is required with a behind_proxy listener: without it every client looks like the proxy
```

`kista admin -config <file> check` is a quick way to validate a changed file before a restart: it
loads and validates the file and checks the schema without changing anything. (Other `kista admin`
commands open the store with `store.migrate`, so with `auto` a newer binary applies its migrations
first.) The rules worth knowing beyond the tables above:

- paths named in the file are absolute;
- every URL that kista calls is https, with http only on loopback under `profile: dev`;
- name fields follow the patterns shown, so they can appear in URLs and lease names unescaped;
- a list of blocks with a `kind` takes exactly the one block of that kind.
