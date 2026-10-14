---
title: Configuration
---

# Configuration

kista reads one YAML file and then applies `KISTA_*` environment overrides. This page lists every
setting: its type, its default and its limits. The pages linked from each section explain how to use
it.

## The file

Both commands take the file with `-config`; neither looks for a file on its own.

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

That is the complete list. Everything else is **file-only**: `profile`, `azure.identity.kind`, and
the whole of `signers`, `blob`, `egress`, `auth`, `publish`, `upstreams`, `events`, `statistics`,
`telemetry`, `gc`, `ui`, `attachments`, `tenants`, and every `serve` setting but `public_url`. Each of these
switches a guard on or off, or decides what kista may reach, so it cannot be changed by whoever
controls the process environment.

The rules of the loader:

- The environment wins over the file.
- A `KISTA_` variable that names a file-only setting is an error (`... can only be set in the config
  file`), not a silent no-op.
- A `KISTA_` variable that names no setting is an error (`KISTA_STORE__DNS is not a kista setting`).
  The one exception is the variable that `store.login.password_env` names: you may call it
  `KISTA_DB_PASSWORD`, and it is read as the password, not as a setting.
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
| AWS identity (KMS sources, S3 domains) | a platform identity; steering `AWS_*` variables refused | steering variables not checked; for `awskms`, `identity.kind: static` (key files) and `endpoint_url` on loopback (the moto emulator) allowed |
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
| `store.login.kind` | `password`, `entra` | `password` | How the password or token is obtained. `entra` needs `postgres` or `sqlserver`. |
| `store.login.password_env` | variable name | none | The variable holding the password, re-read on every new connection. |
| `store.login.password_file` | path | none | A file holding the password (trailing newlines are trimmed), re-read on every new connection. Not together with `password_env`. |
| `store.max_open_conns` | integer | `10` | The connection pool size (PostgreSQL and SQL Server). |
| `store.migrate` | `auto`, `check` | `auto` | `auto` applies pending migrations at start; `check` only verifies the schema. |
| `store.sqlite.path` | path | none | The SQLite file. Required for `sqlite`. May not contain `?`, `#` or `%`. |

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
| `signers.sources` | list | none | Named vault or KMS sources. |

Each entry of `signers.sources`:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | `[a-z][a-z0-9-]{0,15}`, unique. Reserved: `file`, `vault`, `azurekv`, `awskms`, `gcpkms`, `pkcs11`, `http`, `https`, `arn`, `projects`. A signer reference is `<name>:<key>`. |
| `kind` | `vault`, `azurekv`, `awskms` | required | `gcpkms` is recognised but not available yet (a later phase of spec 0004); the loader refuses it. |
| `allow` | list of key-name prefixes | required | Which keys of the source may be used; `"*"` for any. For `azurekv`, prefixes are written in lower case. For `awskms`, whole key ids (`mrk-` ids only with `allow_multi_region`) or `"*"`. |
| `max_concurrency` | integer | `4` | Signing calls in flight to this source. |
| `recheck` | duration | `10m` | How often key properties are re-read; `0..10m`. |
| `timeout` | duration | `10s` | Per call; `1s..60s`. |
| `health_key` | string | none | Accepted by the loader; not used by this version. |
| `identity` | block | none | Required for `azurekv` and `awskms`; refused for `vault`. Its fields are below. |
| `vault` | block | | Exactly one block matching `kind`. |
| `azurekv` | block | | |
| `awskms` | block | | |

`identity` block of a source:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `kind` | `managed`, `workload`, `default` (Azure); `pod_identity`, `irsa`, `imds`, `ecs`, `static` (AWS) | required | `default` and `static` only with `profile: dev`. |
| `client_id` | string | none | Azure only, as in `azure.identity`. Required for `workload` outside dev. |
| `tenant_id` | string | none | Azure only. Required for `workload` outside dev. |
| `role_arn` | IAM role ARN | none | AWS `irsa` only, and required there: the role the ServiceAccount is bound to, in the region's partition. `AWS_ROLE_ARN` must equal it. |
| `access_key_file` | absolute path | none | AWS `static` only, and required there. Mode `0600` or narrower. |
| `secret_key_file` | absolute path | none | AWS `static` only, and required there. Mode `0600` or narrower. |

`vault` block (HashiCorp Vault or OpenBao Transit):

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `address` | URL | required | `https://host[:port]`, no path. `http` only on loopback with `profile: dev`. |
| `namespace` | string | none | Letters, digits, `_`, `-` and `/`. |
| `mount` | string | required | The Transit mount, a relative path. |
| `ca_file` | path | none | The CA bundle to trust for the address. |
| `auth.kind` | `kubernetes`, `jwt`, `token_file` | required | No static tokens in the config, no AppRole. |
| `auth.mount` | string | `kubernetes` or `jwt` | The auth mount path. |
| `auth.role` | string | none | Required for `kubernetes` and `jwt`. |
| `auth.token_file` | absolute path | required | The projected service-account token, the JWT, or Vault Agent's token. |
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
| `kind` | `fs`, `s3`, `azureblob` | required | Exactly one block matching `kind`. |
| `timeout` | duration | `30s` | Remote stores only: connect, headers and idle reads/writes; `1s..5m`. |
| `fs.root` | absolute path | required | A clean absolute directory (a local disk or a mounted volume). |
| `s3.endpoint` | URL | required | `https://host[:port]`; `http` only on loopback with dev. |
| `s3.bucket` | string | required | A valid bucket name. |
| `s3.prefix` | string | empty | Empty, or lower-case segments each ending with `/`. |
| `s3.region` | string | required | Cloudflare R2: `auto`. |
| `s3.lookup` | `path`, `dns` | required | Addressing style. A bucket with dots over https needs `path`. |
| `s3.sse` | `none`, `s3`, `kms` | required | Server-side encryption. |
| `s3.kms_key_id` | string | none | Required with `sse: kms`, refused otherwise. |
| `s3.access_key_file` | absolute path | required without `identity` | Refused with `identity`. |
| `s3.secret_key_file` | absolute path | required without `identity` | Refused with `identity`. |
| `s3.ca_file` | absolute path | system roots | Trust only this CA bundle. Refused with `identity`. |
| `s3.identity` | block | none | An AWS platform identity instead of the key files: `kind` `pod_identity`, `irsa` (with `role_arn`), `imds` or `ecs`; not `static`. The endpoint must then be AWS S3 in `s3.region`: `https://s3.<region>.amazonaws.com` or its dualstack, FIPS or `.amazonaws.com.cn` form, no port. |
| `s3.role_arn` | IAM role ARN | none | With `identity` only: a role to assume after it, in the region's partition. |
| `s3.account` | string | none | With `identity` only, and required there: the bucket owner's 12-digit account, sent as `x-amz-expected-bucket-owner`. |
| `azureblob.account` | string | required | 3-24 lower-case letters and digits. |
| `azureblob.container` | string | required | 3-63 lower-case letters, digits and single dashes. |
| `azureblob.prefix` | string | empty | As for S3. |
| `azureblob.cloud` | `public`, `china`, `usgov` | `public` | |
| `azureblob.identity` | block | required | `managed` or `workload` (`default` in dev). |
| `azureblob.endpoint` | URL | none | Azurite only, dev only: `http://<loopback host:port>/<account>`. |
| `azureblob.account_key_file` | absolute path | none | Azurite only, dev only, with `endpoint` and without `identity`. |

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
| `serve.min_rate` | size | `16KiB` | Least average rate (per second) of a download, and of a publication upload through the API. |
| `serve.write_idle_timeout` | duration | `30s` | The window `min_rate` is measured over; at least `1s`. |
| `serve.drain_timeout` | duration | `5s` | `/readyz` fails this long before shutdown starts. |
| `serve.shutdown_timeout` | duration | `30s` | Then downloads still running are aborted. |
| `serve.api_rate` | number | `20` | Requests per second per client on `/api/`. Set together with `api_burst`. |
| `serve.api_burst` | integer | `40` | Burst for `api_rate`. |

Each listener:

| Setting | Type | Meaning |
| --- | --- | --- |
| `addr` | `host:port` | Unique. `":443"` listens on every address. |
| `scheme` | `https`, `http`, `dual` | `https` has exactly one of `tls` and `behind_proxy`. `http` has neither and serves public releases only. `dual` needs `tls`: one port serving TLS or plain http by the first byte. |
| `tls.cert_file`, `tls.key_file` | absolute paths | Reloaded when the files change. |
| `behind_proxy` | boolean | TLS is terminated in front; `X-Forwarded-Proto` can only downgrade a request to http. |

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
| `egress.ca_file` | absolute path | system roots | Trust only this CA bundle (a private IdP). |
| `egress.timeout` | duration | `10s` | Per request; `1s..60s`. |

The deployment's own `public_url` host is always refused.

## auth

Server identity: the issuers whose tokens can make someone a server administrator. The whole block
is file-only and is read at start. See [Access](access.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `auth.server_issuers` | list | none | OpenID issuers at the server level. |
| `auth.server_audiences` | list | `[serve.public_url]` | Audiences a server token must carry. Required (or `public_url`) with server issuers. Values under `public_url/` are refused: those are tenants' audiences. |
| `auth.server_egress_allow` | list | none | Like `egress.allow` (`cidr`, `ports`), reached only by the server issuers' fetcher. |
| `auth.server_admins` | list | none | Principals of server issuers who are server administrators. |
| `auth.admin_token_max_age` | duration | `1h` | How recently a management token must have been issued; `1m..24h`. |

Each server issuer:

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | `[a-z][a-z0-9-]{0,15}`, unique. |
| `url` | URL | required | The issuer URL, https (http on loopback only with dev and `egress.allow_loopback_http`). Unique, and not a trusted-publishing provider's URL. |
| `jwks_uri` | URL | discovered | |
| `algorithms` | list | all supported | Of `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `EdDSA`. |
| `required_claims` | map | required | Claims every token must carry, at most 16. Mandatory for a server issuer. |
| `roles_claim`, `groups_claim`, `client_claim` | claim path | none | Keys joined by dots (`realm_access.roles`), or a JSON array of keys when a key has dots. |
| `max_token_lifetime` | duration | `24h` | `1m..168h`. |

A server administrator is written `kind:issuer|value`, where `kind` is `subject`, `role`, `group`
or `client`, and `issuer` is a server issuer's name: `role:ops|kista-admin`. `issuer:<name>` is
refused, since every account of the issuer would be an administrator.

## publish

Publication limits and trusted-publishing providers. The whole block is file-only. See
[Publishing](publishing.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `publish.max_per_principal` | integer | `2` | Uploads at once per principal; `1..64`, not above `max_per_tenant` (each at its default when left out: `max_per_principal: 16` alone is refused, as is `max_per_tenant: 1` alone). |
| `publish.max_per_tenant` | integer | `8` | Uploads at once per tenant; `1..256`. |
| `publish.providers` | list | GitHub Actions | Trusted-publishing issuers, each `name` (`[a-z][a-z0-9-]{0,31}`) and `url` (https, no trailing slash). Absent: GitHub Actions (`https://token.actions.githubusercontent.com`) alone. An empty list `[]` turns trusted publishing off. |

## upstreams

Upstream runs and the credentials of private upstreams. The whole block is file-only. See
[Upstreams](upstreams.md).

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `upstreams.concurrency` | integer | `4` | Upstream runs at once per replica, and cells fetched at once by them; `1..32`. |
| `upstreams.fetch_timeout` | duration | `10m` | One file's download; `1m..2h`. |
| `upstreams.min_rate` | size | `64KiB` | A download's least rate; at least `1KiB`. |
| `upstreams.interval` | duration | `6h` | Between a mirror's scheduled runs; `15m..168h`. |
| `upstreams.negative_ttl` | duration | `1h` | A pull-through miss is not retried within it; `1m..24h`. |
| `upstreams.credentials` | list | none | Named credentials, below. |

Each credential:

| Setting | Type | Meaning |
| --- | --- | --- |
| `name` | string | `[a-z][a-z0-9-]{0,31}`, unique. |
| `tenants` | list | Tenant names that may use it, or `"*"`. Required. |
| `prefixes` | list of URLs | https upstream prefixes it is sent to. Required. |
| `allow_public` | boolean | Releases it brings may be made public. Default `false`. |
| `kind` | `token_file`, `client_credentials` | |
| `token_file` | absolute path | `token_file`: the bearer token, read from the file. |
| `token_url` | URL | `client_credentials`: the token endpoint, https. |
| `client_id` | string | `client_credentials`: required. |
| `scope`, `audience` | string | Optional token request parameters. |
| `client_auth` | `azure`, `file`, `key_file`, `secret` | How the client authenticates. `azure` uses `azure.identity`. |
| `assertion_file` | absolute path | `file`: a client assertion read from the file. |
| `key_file` | absolute path | `key_file`: ZITADEL's JSON key or a PEM key. |
| `key_id` | string | `key_file` with a PEM key. |
| `assertion_audience` | string | `key_file`: the JWT's `aud`; default `token_url`. |
| `client_secret_file`, `client_secret_env` | path / variable | `secret`: exactly one. |
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
| `unmasked` | boolean | `false` | Send server administrators' and the CLI's identities on tenants' events as they are (by default they show as `server`). |
| `url` | URL | | `otlp`: an OTLP/HTTP logs endpoint, https. |
| `allow` | list | none | `otlp`: private addresses of the endpoint (`cidr`, `ports`). |
| `headers_file` | absolute path | none | `otlp`: `Name: value` lines. |
| `timeout` | duration | `10s` | `otlp`: `1s..60s`. |
| `path` | absolute path or `-` | | `jsonl`: the file, or `-` for stdout. Unique per sink. |
| `max_size` | size | `100MiB` | `jsonl`: a file's size before it rotates. |
| `keep` | integer | `5` | `jsonl`: rotated files kept, `0..100` (`0` means 5). |

## statistics

| Setting | Type | Default | Meaning |
| --- | --- | --- | --- |
| `statistics.retention` | duration | `26280h` (3 years) | How long download counts and installers are kept; 30 days to 10 years. |

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
