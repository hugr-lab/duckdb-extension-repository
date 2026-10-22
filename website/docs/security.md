---
title: Security
---

# Security

DuckDB loads extensions as native code. kista's job is to make sure that what a client loads from a
channel is what the channel's administrators admitted, and that nobody gets a private extension, or
learns it exists, without a grant. This page states what kista guarantees, what it relies on, and
where each guarantee is enforced. The reasoning is in the specs, starting with
[0001](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0001-architecture/spec.md).

## What a signature means

A DuckDB client trusts a repository's keys, pinned at `CREATE EXTENSION REPOSITORY` from the
channel's `.well-known/duckdb-extension-repo.json`, and checks the signature on every `LOAD`.

- **Signed channel.** The signature means "checked and admitted by this tenant into this channel".
  kista re-signs everything it serves there with the channel's key: its own builds, mirrored core
  and community builds, and builds from other repositories. A mirrored binary is served under
  kista's signature only after its original signature was verified (below).
- **Passthrough channel.** The deliberate exception. It serves core extensions byte-identical, with
  DuckDB's own signature, because DuckDB autoloads core extensions only under its built-in keys. It
  adds no signature of its own; its guarantee is the allowlist, and it serves only files that verify
  with DuckDB's core keys. A passthrough channel is public to anyone who can reach it: DuckDB's
  built-in HTTP client sends no token.
- **Released versions are immutable.** A released (channel, name, version, DuckDB version,
  platform) never changes its body. It can be yanked or purged, never replaced; a purged slot stays
  taken. A key rotation changes only the signature.
- **Separate keys per channel.** A key belongs to one channel; one fingerprint exists once per
  server. A `staging` channel that signs whatever a publisher uploads must not share keys with
  `prod`.
- **Revocation.** A yanked binary stays loadable where it is installed, because the client trusts
  the key. Only retiring the key (after rotating to a new one) revokes it everywhere. See
  [Key rotation](administration/key-rotation.md).

## Intake verification

Nothing reaches a signed channel unchecked. kista verifies the exact bytes it stores: a file is
spooled locally, verified, and the same spool is committed, with no second fetch.

- **Mirrored builds** ([Upstreams](upstreams.md)): the original signature must verify with the
  upstream's keys: DuckDB's built-in core or community keys (kista carries its own copy, taken from
  its DuckDB pin and checked in CI), or a repository's keys pinned by fingerprint. A `repository`
  upstream without a pinned key is refused.
- **Published builds** ([Publishing](publishing.md)): the footer must match the declared version
  and platform, and the binary must export the entry point of the declared name and no other
  extension's, so it loads as that name. This is a naming check, not a code review.
- **Both**: the footer's platform, version and ABI must match the path, so an old signed binary
  cannot be served as a newer version; bodies are size-capped and gzip is inflated under a limit.
- **Reserved names.** Core and community names, and names an upstream of the tenant provides, can
  be published or promoted only through grants that name them.
- **Blocks** ban a body hash tenant-wide; every release insert re-checks them.

A build that fails any check is rejected: a publisher gets an error answer, and an upstream run
records the rejection as an `upstream.rejected` event.

## Key custody

- **Private keys never leave the signer.** Production keys live in HashiCorp Vault / OpenBao
  Transit, Azure Key Vault, Azure Managed HSM, AWS KMS or Google Cloud KMS; kista sends a 32-byte
  digest and receives a signature. The database stores public keys and signer references only.
- **Keys are checked, not trusted.** RSA-2048 with `e = 65537`; signing only; not exportable; not
  imported; version pinned; HSM-backed on Azure and Google Cloud by default; tagged or labelled with
  their purpose on Azure, AWS and Google Cloud; properties re-read while in use; every signature
  verified against the registered public key and the key identity the backend reports.
- **References come from the server administrator only.** A reference names a configured source and
  a key inside it, limited by the source's allow list. It never carries a host, so no tenant can make
  kista call another vault with its identity.
- **Credentials come from the platform**: managed or workload identity, EKS Pod Identity, IRSA,
  ECS task roles, EC2 instance profiles, GKE Workload Identity, Google workload identity federation
  from a projected token, Kubernetes or JWT login, Vault Agent token files. Steering environment
  variables are refused outside `profile: dev`. An S3 domain with an AWS identity follows the same
  rules; it is accepted only for AWS S3 hosts, and every request names the bucket's expected owner.
  On Google Cloud, kista acts only as the configured service account (`principal`), and refuses
  credential sources that answer with the node's identity.
- **File keys** are for development, or need an explicit `signers.allow_file: true`; the file must
  be mode `0600` or narrower, owned by kista's user, and not a symlink. This is stricter than the
  rule for other files (below), on purpose: a Kubernetes Secret mounted into `signers.file_dir` is
  refused.
- **Keys in a Kubernetes Secret** (`kubesecret`) are an explicit opt-in for clouds without a KMS:
  protected only by the cluster's etcd encryption and its RBAC on Secrets. The configuration must
  acknowledge it, kista checks the files come from a read-only Secret volume, and every start logs
  a warning and records `signer.source.weak`
  ([Kubernetes Secrets](signing-keys.md#kubernetes-secrets)).
- Signing happens when a release is made, never on a download, so a KMS outage stops publishing,
  not serving.

Details: [Signing keys](signing-keys.md).

## Files kista reads

kista reads passwords, keys, tokens and certificates from files the configuration names. They all
follow one rule ([spec 0013](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0013-deployment/spec.md)).
The file is checked on the opened descriptor, at every read.

- **A secret's or a token's links stay in its directory.** The file is opened inside its own
  directory (Go's `os.Root`). A relative symlink may lead anywhere under that directory; a link
  that leaves it, or an absolute link (even one pointing inside it), is refused. This is the
  layout Kubernetes uses for Secret and projected volumes (`x -> ..data/x`,
  `..data -> ..2026_10_14…`). The directory must be readable by kista (the `r` bit, not only
  `x`): `/etc/ssl/private` at `0710 root:ssl-cert` fails, and the error names the directory.
- **A public file's links are followed anywhere**: its content is not secret. certbot's `live/`
  links and the system CA bundle's links work for `tls.cert_file` and the `ca_file`s. certbot's
  `privkey.pem` is a secret, though, and its `live/` link leaves the directory: point
  `tls.key_file` at the real file under `archive/` (if its owner, mode and directory fit the
  secret rule), or at a copy, for example one a certbot deploy hook makes.
- A missing file is retried once, for a `..data` swap in progress.
- **A regular file**, opened without blocking, so a FIFO in its place is refused, not waited on.
- **Nobody but the owner may write it** (no group or other write bit).
- **A secret or a token has exactly one hard link.**
- Then the rule of the file's class:

| Class | Owner and mode |
| --- | --- |
| **secret** | owned by the user kista runs as, with no group or other bits (`0600` or narrower); or owned by root, with group equal to kista's group (not `0`) and mode `0440` or narrower |
| **token** | owned by root or by the user kista runs as, any read bits; nobody but the owner may write it (a projected service account token: short-lived and bound to an audience) |
| **public** | owned by root or by the user kista runs as, any read bits; nobody but the owner may write it |

"The user kista runs as" is the effective uid, and "kista's group" the effective gid. The second
shape of a secret is a Kubernetes Secret volume under `fsGroup`, with `defaultMode` `0440` or
`0400`: the kubelet makes the files root-owned, with the pod's `fsGroup` and mode `0440`. It
trusts root and kista's primary group only: not a supplementary group, and never gid `0`. On a
plain host, do not share kista's group with other users. On a system without Unix ownership, only
the directory, regular-file and size rules apply.

| File | Class | Size limit |
| --- | --- | --- |
| `store.login.password_file` | secret | 64 KiB |
| Vault `auth.token_file` with `kind: token_file` (a Vault token) | secret | 64 KiB |
| Vault `auth.token_file` with `kind: kubernetes` or `jwt` | token | 64 KiB |
| Vault `ca_file` | public | 1 MiB |
| S3 `access_key_file` and `secret_key_file` (blob domains) | secret | 4 KiB |
| S3 `ca_file` | public | 1 MiB |
| Azure Blob `account_key_file` | secret | 4 KiB |
| AWS `identity.access_key_file` and `identity.secret_key_file` (`static`) | secret | 64 KiB |
| AWS web identity and Pod Identity token files (`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`) | token | 64 KiB |
| GCP `credentials_file` (with `profile: dev` it may be a `service_account` key, which holds a private key, yet is checked as public: a gap accepted in dev only) | public | 64 KiB |
| the token file an `external_account` file names (`credential_source.file`) | token | 64 KiB |
| listener `tls.cert_file` | public | 1 MiB |
| listener `tls.key_file` | secret | 1 MiB |
| `egress.ca_file` | public | 1 MiB |
| OTLP sink `headers_file` | secret | 64 KiB |
| upstream credential `token_file` and `assertion_file` | token | 64 KiB |
| upstream credential `client_secret_file` and `key_file` | secret | 64 KiB |

A file that breaks its rule stops kista at start, or fails the read that needs it. The error
names the file and the rule it breaks, with the owner, group or mode that breaks it; never the
file's content. What this rule changes for existing files is in
[Upgrading](administration/upgrading.md#files-kista-reads).

Some files keep rules of their own:

- **Key files** in `signers.file_dir`: no symlink, owned by kista's user, mode `0600` or narrower
  ([Signing keys](signing-keys.md#file-keys)). A Kubernetes Secret mounted there is
  refused.
- **The SQLite database**: created or changed to `0600` at open ([State](state.md)).
- **The jsonl event sink and the blob spool**: files kista writes itself, with mode `0600` (the
  spool directory `0700`).

The Azure workload identity token (`AZURE_FEDERATED_TOKEN_FILE`) is read by the Azure SDK, not by
this rule.

**Docker and host files.** Docker Swarm's default secret mount, `/run/secrets/<name>` owned by
root with mode `0444`, is refused for a secret: others can read it. Compose's file-based secrets
are bind mounts, which keep the host file's owner and mode. A secret file on a host or in a bind
mount must be owned by the uid kista runs as (for example `65532`, the distroless `nonroot`
user) with mode `0400` or `0600`, or owned by root with kista's group and mode `0440`.

## Tokens and issuers

- **Issuers are per tenant.** A tenant administrator registers the tenant's identity providers.
  Algorithms are limited to `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`
  and `EdDSA`; never `none` or `HS*`.
- **The audience is bound to the tenant**: its canonical URL, or an audience the server
  administrator assigns. A token minted for one tenant does not work at another, even with the same
  identity provider. Server audiences and tenant audiences are disjoint.
- **Principals are namespaced by the issuer record**, so controlling claims at one issuer never
  satisfies a grant made for another. Removing an issuer removes its grants.
- **Tokens are read only over https**, never logged, never put in events, never forwarded upstream.
  An invalid token is treated as no token. Management calls require a recently issued token.
- **Server administrators** come from the configuration file (`auth.server_admins`, principals of
  `auth.server_issuers`, which must have required claims). The `kista admin` CLI acts as the server
  administrator: it runs as the service's OS user with the service's configuration.
- **Licence managers** (`auth.server_license_admins`, from the configuration file only) manage
  every tenant's licences and nothing else, so a job that writes licences needs no server
  administrator's token. A leaked licence manager's token can import, replace, remove licences and
  make them private. It can never make one public (which would serve its token to anyone served a
  public release), publish, block, or add upstreams or keys. A licence still has to verify against
  a key a live release declares.
- **Distribution admins** (`auth.server_distribution_admins`, from the configuration file only)
  provision a distributor's part of every tenant. A leaked distribution admin's token can reserve
  names in a tenant (narrowing its wide grants; a server administrator releases them), create and
  mark fresh publishers, give its marked publishers keys, and grant `publish`, `promote` and `admin`
  on the names reserved in the tenant: so it can publish under those names there, as the distributor
  does. It cannot take a tenant's publisher with grants, grant on a name the tenant does not
  reserve, remove a grant the tenant made, read channels or keys, or change a tenant, a channel or a
  key set. A reservation's answer names the releases under the name in the tenant.
- **Publishers** sign each request with their developer's SSH key (spec 0023). kista stores only
  public keys, so a copy of its database cannot publish. A signature binds the method, the path and
  query, the body's SHA-256 and a time within five minutes, so a captured request cannot be sent
  elsewhere or with another body; the body is checked before anything is written. Signed requests
  are accepted only on the publishers' routes.
- **DuckDB clients** send the token from an http secret. Its `SCOPE` must end with a slash
  (`https://host/acme/prod/`): DuckDB matches scopes by string prefix, so without the slash the
  token would also be sent to `https://host/acme/prod2/`.

Details: [Access](access.md).

## Grants and visibility

- **Default deny.** Without a grant, a principal gets nothing beyond public releases. `publish` and
  `promote` are never implied by `admin`, and an issuer-wide grant never carries `admin`'s rights.
- **Public and private.** A public release is served to anyone, also over plain `http` (the
  signature, not the transport, protects it; this is how a client bootstraps httpfs). A private
  release needs `https` and a token with `install`.
- **No existence leaks.** Inside a channel, a request without a valid token for anything not public
  answers `401`; with a valid token but no grant, `404`, the same as a missing path, with identical
  bodies and headers and the same database work. Non-public answers carry
  `Cache-Control: private, no-store` and `Vary: Authorization`.
- **What is always public**: tenant and channel names, a signed channel's
  `/.well-known/duckdb-extension-repo.json` (the public keys DuckDB needs before it can send a
  token), a tenant's attestation public keys, and passthrough channels.
- **Blobs are never reachable by hash.** A body is resolved only through a release the caller may
  see, and whether another tenant stores the same body is not observable: every commit uploads.

## Outbound requests

Every request kista makes on a tenant's behalf (issuer discovery, JWKS, upstream fetches, private
upstream token endpoints, feeds, event sinks) goes through one egress client:

- `https` only; plain `http` only to loopback with `egress.allow_loopback_http`, which needs
  `profile: dev`;
- the host is resolved by kista and **every address is checked on the connection actually dialed**,
  so DNS rebinding does not help;
- only global unicast addresses on port 443. Private, loopback, shared, documentation, multicast
  and other special-purpose ranges are refused, including IPv4 addresses embedded in IPv6 (mapped,
  NAT64, 6to4, Teredo) and zoned addresses;
- link-local ranges and cloud metadata endpoints (`169.254.0.0/16`, `fe80::/10`, `168.63.129.16`,
  `100.100.100.200`, `fd00:ec2::/112`) are refused **even when allowlisted**: a tenant can never
  reach IMDS and the managed identity behind every key;
- the deployment's own host (`serve.public_url`) is refused;
- redirects are never followed; responses are size-capped (1 MiB by default for metadata) and
  time-bounded; proxy environment variables are ignored.

```yaml title="kista.yaml"
egress:                        # file-only
  allow:                       # private ranges kista may reach; CIDRs, never host names
    - cidr: 10.20.0.0/16
      ports: [443, 8443]       # empty: 443
  proxy: http://proxy.internal:3128   # explicit; CONNECT to the checked address
  ca_file: /etc/kista/private-ca.pem  # trust only this bundle
  timeout: 10s                 # 1s to 60s
auth:
  server_egress_allow:         # reached only by the server issuers, never by tenants' issuers
    - cidr: 10.30.0.0/24
```

Key sources and object stores are configured by the server administrator and use their own
clients, with the same rules: TLS 1.2 or later, no proxy from the environment, no redirects. See
[Configuration](configuration.md).

## Storage integrity

The object store is not trusted. Every chunk of a body is checked against a digest held in the
database before it is sent; a mismatch aborts the response and marks the body. Attachment contents
are checked against their hash on every read. Storage domains are pinned to their stores and marked
with the deployment; kista refuses to start on a store that is readable anonymously. A bucket
writer can deny service, not substitute a body. See [Storage](storage.md).

## Attachments and signed statements

[Attachments](attachments.md) are documents about a body (SBOM, vulnerability scan, review,
provenance). They are shown only through a release the reader may see, are served as data with
`nosniff` and a sandboxing content security policy, and record who stands behind them: a
publisher's self-assessment is labelled as such.

Between repositories, documents travel as **signed statements**: a JSON object canonicalised by
RFC 8785, signed RSA PKCS#1 v1.5 over SHA-256 of `kista-statement-v1\0` followed by it, with a
tenant's attestation key. The domain separator keeps a statement signature apart from an extension
signature, and the fingerprint registry (and on Azure, AWS and Google Cloud the
`attestation-signing` tag or label) keeps an attestation key apart from a channel key. A subscriber
accepts statements only under keys it pinned, only for bodies it holds, and verifies every field it
shows; content is fetched only from the fixed path under the configured feed URL, through egress.

## Bundles

An air-gapped site takes extensions from **bundles** a connected kista exports
([Exporting a bundle](upstreams.md#exporting-a-bundle), [Importing a bundle](upstreams.md#importing-a-bundle)).
The trust comes from the site:

- **Bundle keys.** Every bundle is signed with an Ed25519 key, and is imported into a tenant only
  when that key's fingerprint is pinned for the tenant in the configuration (`upstreams.bundle_keys`):
  one tenant's key holder cannot sign for another tenant. Sign offline (`kista bundle sign`) rather
  than on an internet-facing gateway.
- **Source keys.** Every file must still verify with the source's keys as the site pinned them:
  DuckDB's compiled-in keys, or a repository's pinned fingerprints, looked up in the bundle's copy
  of its `.well-known` file. The bundle key vouches for what the bundle contains, never for a file.
- **Freshness.** A bundle-mode upstream refuses bundles older than the last one it imported, and a
  cell refuses a build fetched at its source earlier than the newest one it held (also after a
  bundle marked it missing). This stops a lagging or second exporter from rolling a cell back. The
  fetch time is the bundle key holder's word, though: the holder can withhold builds, and could
  bring back an older authentic build by signing it with a new time. Pin exact versions in the
  allowlist where that matters.
- **Visibility.** A cell the exporting side needed a credential for stays private unless
  `upstreams.bundle_sources` allows that source for the tenant. The flag is the exporter's word: an
  honest-user control, not DRM.
- **Input.** The manifest, the tar and its order are checked, entries outside the manifest are read
  through and capped, sizes are bounded by `blob.max_body`, and one file at a time is spooled.
- **The gateway.** The export ships each mirrored file with the signature its source made, never a
  key of the gateway's: a gateway without the bundle key can only withhold. An own channel exported
  as a source is the exception, signed by the channel's key the site pinned. Apart from `-sync`,
  which runs the mirrors, the export contacts nothing but the sources' `.well-known` files, through
  egress, and no credential reaches a bundle.
  A plan discloses the site's sources, allowlists, platforms, DuckDB versions and held bodies to
  whoever exports it.
- **Statements** (spec 0014 phase 1b). They stay signed by their issuer and are judged at the site
  under its own pins; the bundle's keys file is unsigned and used only by fingerprint. Neither a
  carrier nor the bundle key's holder can forge, change or reorder a statement. The holder can leave
  statements out of a range, a withdrawal among them, and withhold documents: a source's numbering
  has gaps, so a range cannot prove it is whole. The site applies every withdrawal it receives and
  moves a cursor no further than a statement it verified; `bundle reset -feed` takes the history
  again from another bundle. A gateway's export feeds keep what they refused and statements about
  documents they do not hold within a cap of their own, apart from the documents quota.

## Tenant deletion

- **Two steps, server administrators only.** A tenant is suspended first and can be deleted only
  after `tenants.min_suspended`; the delete needs the tenant's exact ETag over the API and the name
  typed again in the CLI and the console. A tenant administrator cannot delete its own tenant.
- **Fail closed.** From the delete, the tenant answers `404` on every DuckDB and API route exactly
  as a name that never existed, no write for it commits, its upstreams, feeds and re-signing stop,
  and nothing is served or signed from a half-deleted tenant.
- **No reuse.** The tenant's row stays as a tombstone, so its name, and the `.well-known` keys
  DuckDB clients pinned under its prefix, never pass to another tenant. Its key fingerprints never
  sign for another tenant, and its audiences cannot be assigned to another one.
- **Signer references** stay out of events; only server administrators read them, in the
  deletion's record, to retire the keys in their sources.

Details: [Deleting a tenant](administration/tenants-and-channels.md#deleting-a-tenant).

## Audit

Every management, release, key, upstream and storage change is recorded as an event in the same
transaction as the change, along with refusals and failed authentications; authenticated downloads
are recorded too (once a day per principal, release and client). Events carry no tokens or secrets.
kista keeps them for `events.retention` (30 days by default) and delivers them at least once to
sinks (OpenTelemetry logs, JSON lines). **Tamper evidence is the log pipeline's**:
send events to a store the database administrators do not control. See
[Observability](observability.md).

## What kista trusts

| Trusted | Why |
| --- | --- |
| The configuration file | it decides key sources, stores, egress, server administrators. Guards (`profile`, `signers`, `blob`, `egress`, `auth`, ...) can only be set in the file, never from `KISTA_*` variables |
| The database | it is the trust root for `.well-known`: whoever can write it can add a key clients will pin. Protect its credentials accordingly |
| Server and tenant administrators | their actions are trusted and recorded as events |
| The key backends | to keep keys non-exportable and to sign only with the pinned key, which kista checks as far as each backend reports |

| Not trusted | How it is checked |
| --- | --- |
| Upstream repositories | signatures against pinned keys, footers, entry points, size limits |
| Publishers' uploads | footers, entry points, reserved names, grants |
| The object store | chunk digests and content hashes in the database |
| Tenants' issuer records and feed URLs | the egress guard |
| Feed sources | pinned keys, signed fields, content hashes |
| Bundles and their media | pinned bundle keys, then every file against the pinned source keys, freshness per upstream and cell |
| Clients | grants, constant `401` / `404`, download and rate limits |

## Hardening checklist

- Run with `profile: prod` (the default). It refuses development shortcuts: file keys without
  `allow_file`, plain `http` to vaults and stores, Azure `default` credentials, Google
  `service_account` key files, `require_hsm: false`, rotation minimums below 24 hours.
- Keep channel keys in a vault or KMS; give kista read and sign on its keys only, and alert on key
  changes in the backend's audit log.
- Block public access on buckets and containers.
- Keep `staging` and `prod` keys apart.
- Send events to an external log pipeline.
- Protect the database credentials as you protect the keys; on AWS and GCP, log in with the
  workload's identity (`store.login.kind: aws` or `gcp`) instead of a password.
- On a cloud, scope signing to one deployment's keys and deny tagging them to anyone else (the
  [AWS](deployment/aws.md#for-production) recipe's `kista-purpose` and `kista-deployment` tags).
- Deploy the image by digest and verify its signature ([Deployment](deployment/index.md#verifying)).
- On Kubernetes, follow the chart's hardening list ([Kubernetes](deployment/kubernetes.md#hardening)):
  the NetworkPolicy on, egress to `169.254.169.254` blocked where no metadata identity is used,
  and the namespace's RBAC treated as the administration boundary.

## Reporting a vulnerability

Report vulnerabilities privately through a
[GitHub security advisory](https://github.com/hugr-lab/duckdb-extension-repository/security/advisories/new)
on the repository. Do not open a public issue.
