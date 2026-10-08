# Spec 0001: Architecture

- **Status**: accepted
- **Date**: 2026-10-07
- **Author**: vgsml, Claude

## Summary

**kista** (Old Norse / Swedish "chest") is a multi-tenant, open-source (Apache-2.0) server that
DuckDB 2.0 uses as a trusted extension repository. A tenant owns **channels**; each channel is one
DuckDB repository (one `PREFIX`, its own signing key). The server mirrors extensions from
**upstreams** (DuckDB core, community, any other repository, Enterest), verifies their signatures at
intake and serves them re-signed with the channel key, or byte-identical with DuckDB's signature
for core autoloading. It accepts **publications** of new builds, controls who may install, publish
and administer through **grants** on JWT principals, records installs in an **audit** log, and
issues **licences** for paid extensions. Binaries are stored once by content; signatures are stored
per channel.

**Enterest** is our hosted service built on kista (`https://enterest.hugr-lab.com`): a public
repository, tenants for organisations and publishers, and the paid layer (review reports, global
statistics, marketplace).

This spec fixes the model, the URL layout, the components and their interfaces, the open-core
boundary, and the DuckDB behaviour the design depends on. Each component is specified in detail by
a later spec (see Follow-ups).

## Problem

DuckDB loads native code. Today an organisation that runs DuckDB has two choices: trust whatever
`extensions.duckdb.org` and `community-extensions.duckdb.org` serve, or turn extensions off. There
is no place that:

- decides **which** extensions and versions an organisation may use and **who** installed what
  (audit), and works without internet access;
- lets the organisation's own builds, and builds that replace core extensions, be installed under
  the organisation's key;
- keeps some extensions private (installable only by granted users) and others public;
- carries an independent review of an extension (capabilities, CVEs, SBOM) next to the binary, so an
  administrator can decide whether to allow it.

Three audiences need this:

1. **Organisations** that control their extension set: allowlists, versions, audit, air-gapped
   sites. Self-hosted, an Azure Marketplace managed install, later sovereign clouds, and as part of
   the hugr platform next to tresor-server.
2. **Enterest**, our public service: mostly a mirror, plus our own extensions (acl, acl_otel,
   tresor, hugr_node, mssql, mssql_ducklake, ...), external publishers, and review reports by
   subscription.
3. **Anyone who wants a public repository** of their own: a tenant on Enterest, or a self-hosted
   server.

The hugr node agent installs **only** from this repository (duckdb-acl spec 093 names every
extension).

## Design

### Concepts

| Concept | Meaning |
| --- | --- |
| **Tenant** | A namespace: its own issuers, administrators, channels, grants, upstreams, publishers, and storage domain. The unit of isolation and of subscription. A managed install is one tenant; Enterest hosts many. Tenant and channel names are public. |
| **Channel** | One DuckDB repository inside a tenant (`prod`, `staging`, `nightly`, ...; created at run time). Kind `signed` or `passthrough` (below). A signed channel has a key set and a list of DuckDB versions it serves. |
| **Blob** | A body (the file minus its last 256 bytes), addressed by its **body hash** and stored as a precompressed stream keyed by the stream's own hash (spec 0005); the footer belongs to the Build. Global within a storage domain and never visible through the API: it is reached only through a Build the caller may see. |
| **Extension** | A name inside a tenant; grants that name it say who may publish it (spec 0008). Default visibility `public` or `private`; licence policy `free` or `licensed`. |
| **Build** | Tenant-scoped: name, extension version, platform, ABI type, DuckDB compatibility (an exact DuckDB version for `CPP`; a minimum C API version for `C_STRUCT`), body hash, origin (an upstream with DuckDB's original signature, or a publication with publisher and provenance). Key: (tenant, name, body hash). The same body under two names is two Builds. |
| **Release** | A Build in a channel. State `active`, `deprecated` (versioned path only), or `yanked` (served nowhere; kept for audit). Optional visibility override. Carries the channel's signature. For each (channel, DuckDB version, platform, name), one release is **current**: the one served on the flat path. |
| **Block** | A tenant-wide ban on a body hash (a CVE kill switch): no release with that body is served in any channel of the tenant. |
| **Upstream** | An external source a tenant mirrors from: `duckdb-core`, `duckdb-community`, any DuckDB repository (prefix plus keys pinned by fingerprint), or `enterest`. Has an allowlist of names, versions and platforms, and a mode: scheduled mirror or pull-through. |
| **Publisher** | A named identity in a tenant that only publishes and promotes, authenticated by its credentials: trusted-publishing bindings (CI OIDC) and API keys (spec 0008). People publish with their tenant tokens. On Enterest a publisher has an identity across tenants. |
| **Grant** | `principal × resource × verbs`. Resource: tenant, channel, or extension (explicit release versions later). Verbs: `install`, `publish`, `promote`, `audit` (read events), `admin`. |
| **Attachment** | Data about a body: a review report, an SBOM, a CVE scan, a provenance statement. Owned by a source (tenant, or Enterest) with its own visibility (and a subscription, for reports). Read only through a Build the caller may see. |
| **Licence** | A signed token that lets a licensed extension run for a holder (tenant, subject or client) until an expiry. Issued with the publisher's licence key. |
| **Event** | Audit: who, what, when, from where, outcome. Covers install, publish, promote, yank, grant, mirror and licence events. |

### URL layout

The tenant and channel are path segments. `https://<host>/<tenant>/<channel>` is the `PREFIX` a
DuckDB client uses:

```text
/<tenant>/<channel>/.well-known/duckdb-extension-repo.json          signature_keys of a signed channel
/<tenant>/<channel>/<duckdb_version>/<platform>/<name>.duckdb_extension.gz
/<tenant>/<channel>/<name>/<ext_version>/<duckdb_version>/<platform>/<name>.duckdb_extension.gz
/api/v1/...                                                          management and index API
```

```sql
CREATE EXTENSION REPOSITORY hugr WITH PREFIX 'https://enterest.hugr-lab.com/acme/prod';
CREATE SECRET hugr_repo (TYPE http, BEARER_TOKEN '…', SCOPE 'https://enterest.hugr-lab.com/acme/prod/');
INSTALL tresor FROM hugr; LOAD tresor FROM hugr;
```

- `<duckdb_version>` is the DuckDB release tag (`v2.0.0`) or, on a dev build, its source id
  (`eb0d9df48e`). The versioned layout uses the same segment; there is no other "revision".
- A `C_STRUCT` build is served under every DuckDB version of the channel that accepts its C API
  version: the same major, and `(minor, patch)` lexicographically at most that version's maximum for
  the major (a DuckDB version supports two majors at once; spec 0006). A `CPP` or `C_STRUCT_UNSTABLE` build is
  served only under its exact version.
- The server answers the `.gz` name and the plain name, `GET` and `HEAD`. The `.gz` answer is always
  gzip.
- A channel's prefix is permanent. DuckDB records it in the `.info` file and `UPDATE EXTENSIONS`
  goes back to it, so a renamed channel would break installed clients.
- A custom domain per tenant (`ext.acme.com` → tenant `acme`) is a follow-up.

### Body, signatures and serving

A DuckDB extension file is `body ‖ signature`.

- The body ends with a 256-byte metadata footer: platform, DuckDB version or C API version,
  extension version, and ABI type. The footer has **no extension name**.
- The signature is the last 256 bytes: RSA-2048 PKCS#1 v1.5, SHA-256 DigestInfo, over the
  **composite hash** of the body. The composite hash is the SHA-256 of each 1 MiB chunk, then the
  SHA-256 of the concatenated digests.

How kista stores and serves it:

- **Body hash** = the composite hash. It addresses the blob, and Builds, attachments and blocks refer
  to it. A body is stored once per storage domain, however many channels, tenants or
  upstreams serve it.
- **Signatures** are 256-byte rows: DuckDB's original one and one per release in a signed channel.
- **Serving without a per-request cost.** The blob store keeps the body once, as a precompressed
  deflate stream (no raw copy: spec 0005) that ends with a sync flush (not a final block). The gzip
  answer for a channel is then streamed as three pieces:
  1. the gzip header and the stored deflate stream;
  2. a final *stored* deflate block with the 256 signature bytes;
  3. the gzip trailer: the CRC-32 of the whole file (`crc32_combine` of the body's stored CRC with
     the signature's CRC) and its size.

  So there is no cache and no compression per request, and dedup holds for the compressed form.
  The stored stream is checked as it is sent: each 1 MiB chunk against a digest recorded when it
  was made (spec 0002), so a changed stream never reaches a client.
- **No redirects.** DuckDB's built-in HTTP client does not follow them (`follow_location=false`).
  Binaries are streamed by the server; a CDN can sit in front only as a caching proxy for public
  releases (a follow-up).
- **ETag** = `H("gz" ‖ body hash ‖ signature)` for the `.gz` answer and `H("plain" ‖ body hash ‖ signature)`
  for the plain one (spec 0005). `If-None-Match` is
  evaluated only after authorization.

### Versions are immutable

A node's cluster profile (duckdb-acl spec 093) names an extension, a version and a repository; the
signature, checked by DuckDB against the channel's keys from discovery, is the trust. kista makes
that enough:

- a released (channel, name, extension version, DuckDB version, platform) never changes its body.
  It can be yanked, not replaced; a fix is a new version;
- so the same version in the same channel is always the same code, and a key rotation changes only
  the signature, which DuckDB checks.

Spec 093's optional `sha256` (of the installed file, signature included) adds nothing to this and
breaks on every key rotation, so duckdb-acl removes it (its spec 103, PR hugr-lab/duckdb-acl#183).
kista does not pin by hash.

### Channel kinds

- **`signed`**: serves `body ‖ channel signature`. DuckDB trusts the channel's keys from its
  `.well-known` file (pinned at `CREATE EXTENSION REPOSITORY`) or `USING PUBLIC KEY`. Clients use
  `INSTALL x FROM <repo>` and `LOAD x FROM <repo>`. Our builds, external publications and
  replacements of core or community extensions are served here. Grants are enforced.
- **`passthrough`**: serves `body ‖ DuckDB's core signature`, byte-identical to
  `extensions.duckdb.org`, and no `.well-known` file. It exists for `custom_extension_repository` /
  `autoinstall_extension_repository`: DuckDB treats those URLs as **core-typed** and checks them
  against its built-in core keys only, so only core extensions work (community binaries fail).
  It gives an organisation:
  - an allowlist;
  - offline operation;
  - autoloading of core extensions.

  It gives **no** per-user control. Autoinstall is usually an `http://` URL, served by DuckDB's
  built-in client, which sends no `Authorization` header at all. A passthrough channel is therefore
  public to whoever can reach it, and its audit records the client address only. Publication into a
  passthrough channel is impossible.

  If a tenant shadows a core name (publishes its own `httpfs` into a signed channel), the
  passthrough channels of that tenant stop serving that name by default. This prevents autoloading
  from silently picking DuckDB's build over the replacement; an administrator can override it.

  Passthrough is a deliberate exception to "every binary is re-signed": DuckDB autoloads core
  extensions only under its own key.

**Bootstrapping a client.**

- Private channels need `https` and httpfs, because only httpfs adds the Bearer token from an http
  secret. DuckDB's built-in client never sends one, and without httpfs DuckDB cannot read
  `.well-known` either.
- httpfs itself comes from one of two places:
  - core autoinstall, from DuckDB or a tenant's passthrough channel, when DuckDB has published
    binaries for the client's version;
  - a public signed channel over plain `http://` (the built-in client), with
    `CREATE … USING PUBLIC KEY` and `INSTALL httpfs FROM r` / `LOAD httpfs FROM r`. This is the
    route for a version DuckDB has not published.
- kista therefore also serves **public** releases over plain `http://`. The signature, not the
  transport, protects them. Private releases are served only over `https`.
- So the public minimum is **httpfs** plus **tresor**, both public. With tresor the client logs in
  and gets the token for the rest.
- The client also needs `allow_extension_repositories = 'allowed'`, which can only be set at
  startup.

### Signing keys

A `Signer` signs a 32-byte digest: RSA-2048, PKCS#1 v1.5, SHA-256 DigestInfo.

- The composite hash is computed by kista and passed as the **digest**. It must not be hashed again
  (AWS KMS `MessageType=DIGEST`, Key Vault `sign RS256` over the digest). The key never leaves the
  signer.
- Implementations, as named key sources (spec 0004): a local key file (development), Azure Key
  Vault / Managed HSM, AWS KMS, Google Cloud KMS, HashiCorp Vault / OpenBao Transit; PKCS#11 later.
- Only **RSA-2048** works: DuckDB requires a 256-byte signature.

Every signed channel has a key set: one active key plus trusted keys. `.well-known` lists all
trusted keys, as SPKI PEM or base64 DER, one key per string.

DuckDB pins the keys at `CREATE` and checks them on **every `LOAD`**, not only at `INSTALL`.
Rotation is therefore:

1. add the new key as trusted;
2. clients recreate the repository (or pass both keys);
3. make the new key active and re-sign: new signature rows only, bodies untouched;
4. drop the old key only after clients have reinstalled. A dropped key makes already-installed
   extensions fail to load.

Signing happens at release time (publish or promote), never per request. A `staging` channel signs
whatever a publisher uploads, so its key must not be trusted wherever `prod` is.

### Intake verification

A build from an upstream is accepted only if all of these hold:

- the original signature verifies over the **exact bytes that are committed**: the file is spooled
  locally, its composite hash is computed and the signature verified, then the same spool is
  committed (spec 0005: Spool, verify, Commit), with no second fetch;
- the key matches the upstream:
  - `duckdb-core`: DuckDB's built-in core keys;
  - `duckdb-community`: DuckDB's built-in community keys;
  - another repository: its keys, pinned by fingerprint when the upstream is configured;
  - `enterest`: Enterest's channel keys, pinned the same way.

  kista carries its own copy of DuckDB's built-in key lists, taken from the duckdb pin;
- the footer's platform, version and ABI match the requested path, so an old signed binary cannot
  be served as a newer version;
- the body is within the size limit, and gzip is decompressed under an output limit.

A failure rejects the build and writes an event.

**Pull-through** fetches on a miss only when all of these hold:

- the request is already authorized;
- the (name, version, platform) is on the upstream's allowlist;
- no other replica is fetching it: one fetch at a time through a database lease.

The fetch runs in the background through the same intake, then releases and signs the build. Until
then the request answers `404`. Misses are cached negatively. Anonymous requests never cause an
upstream fetch.

### Publication and promotion

A publisher uploads to a channel it holds `publish` on, usually `staging` (spec 0008: a channel
that takes direct publications must not share trusted keys with `prod`). Authentication is one of:

- a JWT;
- an API key;
- a trusted-publishing CI token, bound to the CI's immutable repository and owner ids, not to
  names.

The server checks the footer and the declared name, version and platform. The name is not in the
footer, so it is bound by the entry point the ELF / Mach-O / PE file exports for DuckDB to load
(`<name>_duckdb_cpp_init`, `<name>_init_c_api` or `<name>_init_c_api_v2` on the pin; spec 0008). Uploads answer the same whether the body was
already stored or not, and are never short-circuited on a client-declared hash.

**Promotion** releases the same Build in another channel with that channel's signature. It needs
`promote` on the target channel **and** `publish` on the name.

**Shadowing**: every DuckDB core and community name is reserved from the start, from the static
lists in the duckdb pin. Publishing or promoting under a reserved name, or under a name an upstream
of the tenant provides, needs grants that name it (spec 0008: a tenant- or channel-wide grant does
not reach it), and the release shows that it shadows the upstream. Adding an upstream that collides with an existing publication is refused until an
administrator resolves it.

**Yank** removes a release from serving. **Block** bans a body hash tenant-wide. A yanked binary
stays loadable on clients that already installed it, because they trust the key. Only a key
rotation revokes it everywhere.

### Identity and grants

- **Issuers are per tenant**, stored in the database as issuer records: the issuer URL, the
  algorithms, and the role and group claim paths. Verifiers are cached per (tenant, issuer record),
  never per issuer string, so two tenants with the same issuer and different settings do not
  interfere.
- **Audience is bound to the tenant.** The `aud` must be the tenant's canonical URL, or an
  audience kista assigns to the tenant that the tenant administrator cannot change. A token minted
  for tenant A does not work at tenant B, even with the same IdP.
- **Every principal is namespaced by the issuer record**: `subject:<issuer>|<sub>`,
  `role:<issuer>|<name>`, `group:<issuer>|<name>`, `client:<issuer>|<id>`. Controlling claims at
  one issuer never satisfies a grant made for another. This differs from tresor-server, where only
  `subject:` carries the issuer.
- **Bootstrap**: server administrators come from the config file and create tenants. A tenant
  administrator holds `admin` on the tenant.
- **Tokens from DuckDB**: an http secret with `SCOPE` on the prefix **with a trailing slash**
  (`'https://…/acme/prod/'`). DuckDB matches the scope by plain string prefix, so without the slash
  the token also goes to `…/acme/prod2/` and other tenants. kista emits scopes this way everywhere.
  tresor keeps the secret fresh.

### Visibility and existence

- A release is **public** (served to anyone) or **private** (needs a token with `install`).
- `.well-known` of a signed channel is always public, so tenant and channel names are public. A
  passthrough channel is public by nature.
- Within a channel, nothing reveals whether a private name or version exists:
  - **without a valid token**, everything that is not public answers `401`;
  - **with a valid token but no grant**, the answer is `404`, the same as for a missing path;
  - both answers have byte-identical bodies and headers, and the "missing" and "forbidden" paths do
    the same database work.
- Every non-public answer, including `401` and `404`, carries `Cache-Control: private, no-store`
  and `Vary: Authorization`.
- The index, attachments and the management API follow the same rule.
- Blobs are never reachable by hash. Whether another tenant holds a body is not observable.

### Licensing

Licensing is part of the open server:

- A licensed extension has a publisher **licence key** (a `Signer`, separate from channel keys,
  and with a domain-separated signing input).
- A grantee with an entitlement obtains a **licence token**: the extension, the holder, the version
  range, the expiry, and optionally the body hashes it applies to.
- An open SDK, compiled into the extension, verifies the token offline against the publisher's
  public key and enforces the expiry with a grace period. Air-gapped sites get long-lived tokens.

**Threat model.** Licensing is a compliance and honest-user control, not DRM. A tenant controls
its own signing keys, so a tenant administrator could publish a patched build that skips the check,
and a self-hoster can allow unsigned extensions. Binding the token to body hashes makes this a
deliberate act, not an accident. Enterest adds a marketplace and billing on top.

### Upstream `enterest` and reports

Attachments flow by **feed**, not by mirror origin. A tenant with a subscription receives
Enterest's attachments for **every body hash it holds**, whether the build was mirrored from
Enterest, from DuckDB core, or from community. The attachments are stored with Enterest as their
source and shown next to the build in the tenant's UI. A different body hash is a different build,
so a report never applies to another binary.

### Events and statistics

Events are written asynchronously in batches, off the serving path. Anonymous public downloads are
counted as aggregates, not as rows. Each tenant sets a retention. Optionally, events go to an
OpenTelemetry log sink as well.

The event log is append-only with a hash chain, and can be exported outside the server: an
administrator's actions are trusted but must be tamper-evident.

`401` events are rate-limited. Tokens never appear in events or logs.

Per-tenant statistics (installs by extension, version, channel, principal) are computed from
events. Global statistics across tenants belong to Enterest.

### Storage, replicas and lifecycle

- **Metadata**: PostgreSQL, SQL Server or SQLite. SQLite means **one replica**; HA needs PostgreSQL
  or SQL Server. Writes are compare-and-set on a version column. Migrations are rolling-safe:
  expand first, contract in a later release.
- **Blobs** (spec 0005): filesystem, S3-compatible (AWS S3, Cloudflare R2, MinIO), Azure Blob or
  Google Cloud Storage, behind one interface. A tenant belongs to a **storage domain**: dedup happens
  only inside a domain. Data residency or a sovereign cloud gets its own domain.
- **Blob GC**: mark-and-sweep over the Builds that reference a body, with a grace period, so it
  never races a concurrent intake.
- **Tenant deletion** cascades to its Builds, Releases, grants, keys and attachments. Its events
  are exported first. Blobs are left to GC.
- **A new DuckDB release** is added to a channel's version list. That triggers mirroring for the
  allowlisted names and, for `C_STRUCT` builds, serving under the new version with no new body.
- **Outbound requests on a tenant's behalf** (issuer discovery, JWKS, upstreams) go through one
  egress client. It refuses loopback, link-local and private addresses after DNS resolution and on
  redirects, caps size and time, and supports a per-deployment allowlist. On Azure this keeps a
  tenant from reaching IMDS and the managed identity behind every key.

### Components

One Go binary, `kista`. The module is `github.com/hugr-lab/duckdb-extension-repository`; renaming
the repository to `hugr-lab/kista` before the first code is proposed. The packages mirror
tresor-server:

| Package | Role |
| --- | --- |
| `internal/config` | YAML + env (`KISTA_*`), a strict loader (spec 0003): server administrators, store, blob, signers, egress, telemetry |
| `internal/store` | Metadata on `database/sql`: PostgreSQL (pgx), SQL Server (go-mssqldb), SQLite (modernc). A `Dialect` struct (rebind, error classes, locks), hand-rolled migrations per dialect, and a shared test suite for all three. |
| `internal/blob` | Content-addressed bodies and their precompressed deflate streams behind one interface: filesystem, S3-compatible, Azure Blob, Google Cloud Storage (spec 0005). GC. |
| `internal/extfile` | The extension file format: footer, composite hash, signature verification, init-symbol check, the gzip assembly. |
| `internal/signer` | `Signer` and named key sources: file, Azure Key Vault / Managed HSM, AWS KMS, Google Cloud KMS, Vault / OpenBao (spec 0004). |
| `internal/egress` | The outbound HTTP client with the SSRF guard. |
| `internal/auth` | Issuer records per tenant, JWT verification, principals. |
| `internal/authz` | Grants and the 401/404 rule. |
| `internal/serve` | The DuckDB-facing routes. |
| `internal/api` | The management and index API. |
| `internal/upstream` | Mirror and pull-through. |
| `internal/audit` | Events, the hash chain, sinks. |
| `internal/license` | Licence keys, entitlements, tokens. |
| `internal/telemetry` | OpenTelemetry. |
| `web/` | The administration console (a micro-frontend), embedded in the binary. |

It is deployed as a distroless image, a Helm chart, and Bicep for Azure Container Apps (PostgreSQL
Flexible or Azure SQL with Entra auth, Key Vault, managed identity). An Azure Marketplace managed
application follows. This is the same shape as tresor-server.

### Open-core boundary

| kista (this repository, Apache-2.0) | Enterest (separate repository, closed) |
| --- | --- |
| tenants, channels, keys, upstreams, mirror, pull-through | developer cabinet, publisher sign-up, cross-tenant publisher identity |
| publication, promotion, trusted publishing, API keys, yank, block | Extension Intelligence: builds from source, analysis, reports |
| grants, audit, per-tenant statistics | global download and usage statistics |
| attachments, the attachment feed client | the report feed and subscriptions |
| licence keys, entitlements, tokens; the licensing SDK | marketplace and billing for paid extensions |
| administration console, built as a micro-frontend | the cabinet and report views, also micro-frontends |
| bundles for air-gapped sites, deployment artifacts | managed Azure / sovereign-cloud operations |

The console is a micro-frontend served by kista: it runs standalone, and the hugr platform (and
Enterest) mount it into their own shell, so a platform user sees one interface. The framework and
the mounting contract are decided in the console's spec.

The open server has no paid-feature switches. Enterest uses only kista's public extension points:
attachments and their feed, events, and the management API.

### DuckDB behaviour relied on (duckdb pin `eb0d9df`, `v2.0-cyanoptera`)

Paths are relative to the duckdb source tree.

| Behaviour | Source | Confirmed |
| --- | --- | --- |
| Keys come from `<prefix>/.well-known/duckdb-extension-repo.json`: `signature_keys` is a non-empty array of strings, the file is at most 64 KiB, and a trailing `/` on the prefix is trimmed | `src/main/extension/extension_repository_manager.cpp:24,131-200` | e2e (0002) |
| Keys are SPKI PEM or base64 DER, RSA-2048 only, one key per string; they are pinned at `CREATE` and checked on every `LOAD` | `extension_repository_manager.cpp:48-115,394-418`; `extension_helper.cpp:873-888` | e2e (0002) |
| The signature covers everything but the last 256 bytes, footer included. It is `mbedtls_pk_verify(SHA256)` over the composite hash (1 MiB chunks) | `src/include/duckdb/main/extension.hpp:45-46`; `extension_load.cpp:273-297,424-439`; `mbedtls_wrapper.cpp:73-97` | e2e (0002) |
| The footer fields are platform, DuckDB or C API version, extension version and ABI type. There is no name | `extension_load.cpp:346-385` | e2e (0002) |
| `custom_extension_repository`, `autoinstall_extension_repository` and `INSTALL x FROM '<url>'` are core-typed: DuckDB's core keys apply. Autoload is core-only | `src/main/extension_install_info.cpp:62-97`; `extension_helper.cpp:201-211,900-916`; `physical_load.cpp:16-25` | e2e (0002) |
| A user-repository install lives under `…/repositories/<repo>/` and is loaded with `LOAD x FROM <repo>` (a bare `LOAD` checks core keys). Installs from two repositories coexist; `LOAD x FROM r2` refuses one from r1. It needs `allow_extension_repositories='allowed'` | `extension_install_dynamic.cpp:370-379`; `extension_load.cpp:614-677` | e2e (0002) |
| Paths are flat `<prefix>/<duckdb_version>/<platform>/<name>.duckdb_extension.gz` and versioned `<prefix>/<name>/<version>/<duckdb_version>/<platform>/…`. The DuckDB version is the tag, or the source id on a dev build | `extension_install.cpp:39-47,218-240` | source + experiment |
| Three transport paths. Local directory: file reads. `http://` with httpfs never loaded: the built-in client sends one `GET` of `.gz`, with no fallback, no redirects and never `Authorization`; `If-None-Match` comes only from `UPDATE EXTENSIONS`. `https://`, or `http://` once httpfs is loaded (bumped to https): httpfs checks `.gz`, then the plain name, then reads, and sends no ETag. A body is gunzipped only if it is gzip, and the gzip trailer is not checked | `extension_install_dynamic.cpp:132-167,225-315`; `http_util.cpp:29-31,515`; `gzip_file_system.cpp:477-533` | e2e (0002) |
| An http secret's `SCOPE` matches by string prefix, and the longest match wins. httpfs adds the Bearer token | `src/main/secret/secret.cpp:14-32`; the httpfs side is not in the pin | e2e (0002) |
| `.well-known` is read through DuckDB's file system, so an `http(s)` prefix needs httpfs (a local-directory prefix does not); it is read without the statement's secrets or `ca_cert_file`, so a private-CA https server cannot serve it. Without httpfs, `CREATE` on `http://` needs `USING PUBLIC KEY` | `extension_repository_manager.cpp:131-160`; `fs.OpenFile` without an opener | e2e (0002) |
| `.info` records the repository URL, and `UPDATE EXTENSIONS` goes back to it, sending `If-None-Match`, but only for flat (core-typed) installs: it does not see installs from a user-provided repository. On the flat layout, installing from another URL needs `FORCE` | `extension_helper.cpp:294-305`; `extension_install_dynamic.cpp:330-352` | e2e (0002) |
| Community keys are trusted only with `allow_community_extensions` | `extension_helper.cpp:636,866` | source |

Consequences:

- a replacement of a core extension is served from a signed channel and must be loaded explicitly
  (`LOAD httpfs FROM hugr`); it is never autoloaded;
- moving a node from one channel to another means a new repository name, or `CREATE OR REPLACE`
  of the same name followed by `FORCE INSTALL`;
- `UPDATE EXTENSIONS` does not cover extensions from a kista channel: a client updates with
  `FORCE INSTALL x FROM r [VERSION …]`;
- kista's serve answers `HEAD` and `Range` (httpfs uses both) and serves public releases over plain
  `http` for the bootstrap;
- every client bootstraps the same way: `CREATE … USING PUBLIC KEY` on `http://`, then
  `INSTALL httpfs FROM r` and `LOAD httpfs FROM r`, then `https://` with `.well-known` and secrets;
- core extensions for a pin DuckDB has not published are built by us and served from signed
  channels; kista never relies on a patched DuckDB.

## Security

- **Fail closed.** No grant means no binary. An unverifiable upstream signature, a footer that does
  not match the path, or a missing init symbol rejects the build.
- **No existence leaks** inside a channel: the rules under "Visibility and existence". Tenant and
  channel names are public.
- **Keys never leave the signer.** Channel keys and licence keys are separate. Staging keys are not
  trusted where prod is.
- **Tenant isolation**:
  - principals are namespaced by the issuer record;
  - the audience is bound to the tenant;
  - verifiers are cached per tenant;
  - blobs and attachments are reachable only through the tenant's own Builds;
  - dedup is invisible to callers.
- **SSRF.** Every tenant-supplied URL is fetched through the egress guard.
- **Token handling.** Scopes carry a trailing slash. kista never forwards a client's
  `Authorization` header upstream. Tokens never appear in logs or events.
- **Caching.** Non-public answers are `private, no-store` with `Vary: Authorization`. A CDN may
  cache only releases that are and stay public; changing visibility purges it.
- **What a signature means.** A signed channel's signature means "admitted by this tenant into this
  channel". A passthrough channel adds none: its guarantee is the allowlist, and the signature is
  still DuckDB's.
- **Trusted input.** Server-administrator config and tenant-administrator actions are trusted and
  recorded tamper-evidently. Publisher uploads and upstream data are verified.
- **DoS.** Size caps on uploads and upstream fetches, and a decompression limit. Pull-through only
  for authorized, allowlisted requests, single-flight, with negative caching. Rate-limited `401`
  events.

## Testing

This spec has no code. Each component spec brings:

- Go unit tests;
- the store suite on all three dialects (SQL Server and PostgreSQL in containers in CI);
- end-to-end tests that run the pinned duckdb against a test kista.

**0002's exit criterion** is an e2e confirmation of every "source" row in the table above, in
particular:

- the core-typing of `custom_extension_repository` against a passthrough channel;
- the versioned layout;
- an http secret with and without httpfs, and the trailing-slash scope;
- the `.gz` / plain / `HEAD` / `If-None-Match` behaviour;
- the assembled gzip stream (DuckDB decompresses it, the signature verifies);
- key rotation across `LOAD`.

## Alternatives considered

- **One key per tenant instead of per channel.** A staging build would then be installable wherever
  prod is trusted.
- **Re-sign everything, no passthrough.** Core extensions could then not be autoloaded from our
  repository: DuckDB checks core-typed URLs against its own keys.
- **Hash pins in the cluster profile.** A sha256 of the served file changes on every key rotation
  without any code change. Immutable versions plus DuckDB's signature check give the same guarantee
  without touching profiles.
- **Store whole signed files per channel, or gzip on the fly with a local cache.** The first copies
  binaries on every promotion. The second needs CPU and disk per replica, and its caches start
  cold. Precompressed bodies with a per-channel tail block avoid both.
- **Redirects to presigned blob URLs.** DuckDB's built-in client does not follow redirects.
- **Static tree on a CDN, no server.** It cannot enforce grants, audit installs or issue licences.
- **Tenant as a subdomain.** That needs wildcard certificates and DNS per tenant. A path works
  everywhere; custom domains come later.
- **A key-value store like tresor-server's.** Releases, grants and events need relational queries.
  `database/sql` with a dialect layer is kept, and the data model is relational.

## Follow-ups

| Spec | Scope |
| --- | --- |
| 0002 | `extfile`, the file signer, gzip assembly; e2e confirmation of the DuckDB behaviour table (implemented) |
| 0003 | `store` on three dialects, migrations; tenants, channels, keys, rotation; config; `kista admin` |
| 0004 | Signer backends: named key sources; Azure Key Vault / Managed HSM, AWS KMS, Google Cloud KMS, Vault / OpenBao Transit (PKCS#11 later) |
| 0005 | Blob storage for extension bodies: filesystem, S3-compatible (AWS S3, R2, MinIO), Azure Blob, Google Cloud Storage |
| 0006 | Builds and releases, `serve`, `egress`, `auth`, `authz`: issuer records, grants, 401/404, caching headers |
| 0007 | The HTTP API: the index (releases, body hashes, visibility; the node agent needs it first) and management, with server administrators |
| 0008 | Publication and promotion: API keys, trusted publishing, init-symbol check, reserved names, block (yank is in 0006) |
| 0009 | Upstreams: core / community / repository / `enterest`, intake, mirror, pull-through, passthrough channels |
| 0010 | Audit: hash chain, export, sinks, per-tenant statistics, serve-path events |
| 0011 | Attachments and the feed from Enterest |
| 0012 | Licensing: licence keys, entitlements, tokens, the SDK (C++, for DuckDB extensions) |
| 0013 | Deployment: image, Helm, Bicep, Azure Marketplace managed application |
| 0014 | Bundles for air-gapped sites |
| 0015 | Administration console: micro-frontend, mounting contract with the hugr platform and Enterest |
| later | custom domains, a CDN for public releases, wasm signatures |

In duckdb-acl (its spec 103, PR hugr-lab/duckdb-acl#183): the optional `sha256` of
cluster-profile extensions is removed.

kista depends on DuckDB only. duckdb-acl, hugr_node and tresor are clients of it, and every feature
works for any DuckDB client.
