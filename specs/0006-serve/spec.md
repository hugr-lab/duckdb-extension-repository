# Spec 0006: Serving, tokens and grants

- **Status**: phase 1 implemented
- **Date**: 2026-10-08
- **Author**: vgsml, Claude

## Summary

`kista serve`: the HTTP server DuckDB installs from. It answers `.well-known` and the extension
paths of every channel from Builds and Releases (introduced here), with the channel's signature and
the stored bodies of spec 0005. It verifies Bearer tokens against the tenant's own issuers, decides
with grants, and keeps spec 0001's existence rule: nothing reveals whether a private name or version
exists.

Delivery is phased:

1. **Serve public releases.** Builds, Releases and their signatures; `kista admin release …`;
   re-signing on key rotation; `kista serve` with listeners, the DuckDB routes, `HEAD`, `Range`,
   ETags, caching headers, health and shutdown. Private releases exist and answer as missing.
2. **Tokens and grants.** The egress client (SSRF guard), issuer records per tenant, JWT
   verification, principals, grants, private releases.

The HTTP management API moves to spec 0007 (with the index API), and serve-path events to spec 0010
(with the audit log). Spec 0001's follow-up table is amended accordingly.

## Problem

Specs 0002-0005 can sign a body, keep keys in a vault and store bodies once. Nothing yet answers a
DuckDB client. A hugr node (duckdb-acl spec 093), a person with `INSTALL tresor FROM hugr`, and an
organisation's CI all need:

- the channel's `.well-known` and its extension files at the paths DuckDB computes;
- the right body for the client's DuckDB version and platform, signed with a key the client trusts;
- public extensions for anyone (httpfs and tresor, the bootstrap of spec 0001), private ones only for
  a granted principal, and nothing that tells an outsider which private extensions exist;
- a way to put our own builds (acl, acl_otel, tresor, hugr_node) into a channel before publication
  (spec 0008) and upstream intake (spec 0009) exist.

## Design

### Builds and releases (phase 1)

A **Build** is a body as a named extension in a tenant; a **Release** is a Build in a channel, with
the channel's signature (spec 0001, "Concepts").

```text
builds              id, tenant_id, name, ext_version, platform,
                    abi (cpp | c_struct | c_struct_unstable),
                    duckdb_version null          -- cpp, c_struct_unstable: the exact version (footer)
                    c_api_major, c_api_minor, c_api_patch null   -- c_struct: the C API it was built for
                    body_hash, origin (admin; later publication, upstream), origin_signature null,
                    created_at, created_by
                    unique (tenant_id, name, body_hash); unique (id, tenant_id)
releases            id, tenant_id, channel_id, build_id, name, ext_version, platform, slot,
                    state (active | deprecated | yanked), visibility (public | private),
                    seq null, created_at, created_by, state_changed_at, state_changed_by, version
                    fk (channel_id, tenant_id) -> channels; fk (build_id, tenant_id) -> builds
                    unique (channel_id, name, ext_version, platform, slot)
                    unique (channel_id, seq) where seq is not null
                    index (channel_id, name, platform, state, seq); unique (id, channel_id)
release_signatures  release_id, channel_id, key_id, signature (exactly 256 bytes), created_at
                    pk (release_id, key_id)
                    fk (release_id, channel_id) -> releases; fk (key_id, channel_id) -> channel_keys
duckdb_version_c_apis  duckdb_version_id, major, max_minor, max_patch      pk (duckdb_version_id, major)
leases              name pk, holder, expires_at, version
channels            + serving_key_id null, fk (serving_key_id, id) -> channel_keys (id, channel_id)
                    + release_version (bumped by every release change and serving-key move)
channel_keys        + unique (id, channel_id)
```

A release's name, extension version and platform are copied from its Build. SQLite cannot add a table constraint to an existing table, so there the serving key's channel
is checked in code. Migration 0003 adds tables and columns only, but sets `-- +min_reader 3`: an older
`kista admin` would retire a serving key or add a DuckDB version without its C API rows.

**Grammars** (checked at release time; the footer is parsed by spec 0002 `extfile`):

- name: `[a-z0-9_]{1,64}`, declared by the caller (the footer has none). Refused: DuckDB's alias
  sources, which a client can never request because DuckDB rewrites them before building the URL
  (`http`, `https`, `s3`, `md`, `mysql`, `odbc`, `postgres`, `sqlite`, `sqlite3`, `uc_catalog`;
  `src/main/extension/extension_alias.cpp:5-38`), and Windows device names (`con`, `prn`, `aux`,
  `nul`, `com1`-`com9`, `lpt1`-`lpt9`). Phase 1 trusts the server administrator who adds the build;
  spec 0008 binds names to the init symbol for publishers.
- extension version: non-empty, `[A-Za-z0-9._+-]{1,64}`, not `.` or `..`, matched exactly and
  case-sensitively
  (`INSTALL … VERSION 'x'` puts the string into the path as typed; `1.0` and `v1.0` differ).
- platform: `[a-z0-9_]{1,64}`, not `wasm_*` (wasm asks for `.duckdb_extension.wasm` under another
  version scheme; `extension_install.cpp:40-41,221-225`).
- DuckDB version: a release tag `v\d+\.\d+\.\d+` with an optional suffix `-[a-z0-9.]+`
  (`-rc1`, `-dev123`), or a 10-character lowercase hex source id.
- C API version (`c_struct`): `v<major>.<minor>.<patch>`, numeric, as DuckDB's `ParseSemver`
  (`src/main/extension.cpp:149-176`).

**Which DuckDB versions a release serves.** The channel's DuckDB versions (spec 0003) bound
everything:

- `cpp`, `c_struct_unstable`: exactly the build's DuckDB version (`src/main/extension.cpp:56-63`).
  A release is refused if its channel does not serve that version.
- `c_struct`: every channel version that accepts the build. DuckDB supports two C API majors at once
  (on the pin v1 up to `v1.5.6` and v2 up to `v2.0.0`) and accepts major `M` when `(minor, patch)`
  is lexicographically at most its maximum for `M`, so `v1.4.9` passes under `v1.5.6`
  (`src/main/extension.cpp:46-100,118-139`). So a DuckDB version holds
  one maximum per major in `duckdb_version_c_apis`: `kista admin version add … -c-api v1.5.6
  -c-api v2.0.0`, and `kista admin version c-api <name> -c-api …` adds a missing major. Rows are
  facts about the engine and are never changed or removed; adding one bumps `channels.version` of
  every channel serving that version. Existing `duckdb_versions.c_api_version`
  values are read as one maximum when they parse (a version added before migration 0003); a version
  without C API data serves no `c_struct` build.
  This amends spec 0001's "C API at least the minimum".

**Slots and immutability.** One body per (channel, name, extension version, platform, DuckDB
version) (spec 0001). The slot is the DuckDB version for `cpp` and `c_struct_unstable`, and
`capi:<major>` for `c_struct`, so one extension version can ship a v1 and a v2 C API build. Within a
channel, a (name, extension version, platform) holds `c_struct` releases or `cpp`/`c_struct_unstable`
releases, never both (checked under the channel lock). A yanked release keeps its slot: a fix is a
new version.

**Current.** `seq` orders releases for the flat path. A release added normally gets
`MAX(seq) + 1` of its channel (under the channel lock); one added with `-not-current` has no `seq`
and is served only on the versioned path. The flat path for (channel, DuckDB version, platform,
name) serves, among the **active** releases with a `seq` that serve that DuckDB version **and that
the caller may see**, the one with the highest `seq`.

- Deprecating or yanking the current release makes the previous one current again.
- `kista admin release current <id>` gives an active release a new `seq` (a rollback, or making a
  `-not-current` release current). A `c_struct` release becomes current for every DuckDB version
  it serves.
- "The caller may see" (phase 2): a caller without `install` on the path sees only public releases.
  Adding a private release therefore never changes what an anonymous client gets.

**Signatures and the serving key.** One `release_signatures` row per (release, key). Each signed
channel has a **serving key**: every answer of the channel carries the signature by that key, on
every replica. The invariant: **every non-yanked release of the channel has a signature by the
serving key**; a release added after a key became active also has that key's signature, and the
re-signer gives it to the older ones.

- When a channel's first release is added, its serving key is the active key.
- When another key becomes active (spec 0003), the re-signer adds its signature to the channel's
  non-yanked releases. The move of the serving key to the active key is one transaction under the
  channel lock: it checks that no non-yanked release lacks the active key's signature, then sets
  `serving_key_id` (compare-and-set) and bumps `release_version`.
- Signing never happens inside a transaction: the signer signs, the signature is verified against
  the key's stored public key (`extfile.Verify`), and a short transaction under the channel lock
  inserts it. A release insert reads the serving and active keys under the lock and commits only if
  its signatures cover both; otherwise it signs with the missing key and retries. So an add that
  races an activation or a move never leaves a release without the serving key's signature.
  Inserting a signature that exists is a no-op.
- `key retire` (with or without `-force`) is refused for the serving key (and, as before, the active
  one). With the invariant, retiring any other key leaves every release servable.
- With `serve.resign` off on every replica and no `kista admin key resign`, the serving key never
  moves; `key activate` says so in its output.
- A move between a client's `HEAD` and its ranged `GET` changes the ETag, so that one install fails
  and is retried; nothing else does.

**The re-signer** runs in `kista serve` when `serve.resign` is on (a replica that should not hold
sign permission leaves it off), and in the foreground as `kista admin key resign <tenant>/<channel>`.
One replica at a time per channel holds a lease (`leases`, compare-and-set with a 2-minute expiry,
renewed per batch, released on shutdown); the work is idempotent, so the lease only saves KMS calls.
It signs sequentially, in batches of 32 inserted per transaction.

**Adding a release** (`kista admin release add <tenant>/<channel> <file|-> -name <name>
[-private] [-not-current]`):

1. read the active and serving keys; the channel must be `signed` with an active key (passthrough
   channels are fed by upstreams, spec 0009);
2. spool the file into the tenant's storage domain (spec 0005); parse and check the footer against
   the grammars; check the slot with a read (to avoid storing a body that would be refused);
3. commit the body; insert or find the Build;
4. sign with the active key and the serving key (if different), verify, then in a short transaction
   under the channel lock: re-read the keys (retry with any missing signature), re-check the slot,
   take `seq`, insert the release and its signatures, set the serving key if unset, bump
   `release_version`.

Adding a build to a slot it already holds, with the same flags, returns the existing release;
different flags are refused (use `release public|private` or `release current`).

**Other commands** take a release id and record who and when: `release list <tenant>/<channel>
[-name x]`, `release yank` (final), `release deprecate`, `release activate`, `release current`,
`release public`, `release private`. Each bumps the channel's `release_version`.

### `kista serve` (phase 1)

```yaml
serve:                                          # file-only (spec 0003 rules), except public_url
  public_url: https://enterest.hugr-lab.com     # https, no path; audiences and docs (phase 2)
  listeners:
    - { addr: ":443", scheme: https, tls: { cert_file: /run/tls/tls.crt, key_file: /run/tls/tls.key } }
    - { addr: ":80", scheme: http }             # optional: public releases only (the bootstrap)
  trusted_proxies: []                           # CIDRs whose X-Forwarded-For gives the client address
  resign: false                                 # run the re-signer on this replica
  max_downloads: 256                            # concurrent binary bodies per replica
  max_downloads_per_client: 8
  min_rate: 16KiB                               # per second, over write_idle_timeout
  write_idle_timeout: 30s
  drain_timeout: 5s                             # /readyz fails this long before shutdown starts
  shutdown_timeout: 30s                         # then downloads still running are aborted
```

**Listeners.** Each declares a scheme:

- `https` with `tls` (a certificate, reloaded when its files change), or with `behind_proxy: true`:
  TLS is terminated in front. A request whose `X-Forwarded-Proto` is anything but exactly one
  `https` is treated as plain http: the header can only downgrade, never upgrade.
  `trusted_proxies` is required with a `behind_proxy` listener (otherwise every client would look
  like the proxy), and requests from other peers are refused and logged.
- `http`: public releases only; an `Authorization` header is ignored.
- `dual` (with `tls`): one port that serves TLS when the first byte is a TLS handshake and plain
  http otherwise.

Addresses are unique; an `https` listener has exactly one of `tls` and `behind_proxy`. kista never
decides a scheme from a client header.

**DuckDB rewrites `http://` to `https://` after httpfs is loaded, keeping the port**
(`extension_install_dynamic.cpp:312-314`; `http_util.cpp:518-521`). The bootstrap repository
`http://host/<tenant>/<channel>` therefore keeps working only if `https://host/…` reaches the same
kista: either the default ports (http on 80, https on 443 of the same host), or a `dual` listener on
one port. After the bootstrap a client uses an `https://` repository. On Azure Container Apps the
ingress must accept plain http (`allowInsecure: true`; otherwise it answers `301`, which DuckDB's
built-in client does not follow); it then forwards it with `X-Forwarded-Proto: http`, which the
downgrade rule turns into an http request.

**Routes.** A custom top-level handler (not `http.ServeMux`, which cleans paths and redirects)
matches `r.URL.EscapedPath()`:

```text
/<tenant>/<channel>/.well-known/duckdb-extension-repo.json
/<tenant>/<channel>/<duckdb_version>/<platform>/<name>.duckdb_extension[.gz]
/<tenant>/<channel>/<name>/<ext_version>/<duckdb_version>/<platform>/<name>.duckdb_extension[.gz]
/healthz  /readyz
```

- Every segment must match its grammar. Any `%`, an empty segment, `.` or `..`, a trailing slash,
  a query string, the two names of a versioned path differing, an absolute-form or `*` request
  target (`OPTIONS *` included): all answer `404`, like an unknown path. No such path can exist, so
  this tells nothing. There is no normalisation and no redirect.
- Methods are decided from the path's shape alone, before anything is looked up: `GET` and `HEAD`;
  others `405`. A request with a body (`Content-Length` above 0, or chunked) is refused with `400`.
- Tenant names already exclude `api`, `admin`, `healthz`, `readyz`, `metrics`, `static`, `ui`
  (spec 0003).

**`.well-known`** of a signed channel with trusted keys: spec 0003's document, `Cache-Control:
public, no-cache` (httpfs caches by `max-age` in-process; `no-cache` makes a `CREATE OR REPLACE`
after a key change see it), and an ETag of its bytes. It is read through a path that needs no
authorisation (it is public by design). A passthrough channel, or a signed one without keys, has
none: `404`, since `.well-known` is public whatever the caller.

**Binaries.** Resolution: the tenant and channel (unknown or suspended: `404`); the caller's view
(phase 1: public only); the release (flat: current; versioned: the active or deprecated release of
that extension version that serves the DuckDB version, and for `c_struct` the one with the highest
C API major the DuckDB version supports); its Build; its blob record (spec 0005 `Record`, in the
tenant's domain); its signature by the serving key. Resolutions are cached per (path, the caller's
view, `channels.version`, `release_version`); every request reads both counters with the channel
row, so a yank, a visibility change, a serving-key move or a change of the channel's DuckDB versions
takes effect on every replica at the next request. Blob records follow spec 0005's cache: the `503`
for a record marked corrupt is best-effort, the per-chunk verification is what guarantees bytes.

- `.gz`: `blob.OpenGzip`, served with `http.ServeContent`: `HEAD`, **one** range (a request with
  several gets the whole file with `200`, which RFC 9110 allows), `If-None-Match`, `If-Range`; no
  `Last-Modified`.
- plain name: `blob.OpenPlain`, sent whole with `200` and `Content-Length`, no `Accept-Ranges`
  (spec 0005: httpfs accepts it on the pin); `If-None-Match` gives `304`.
- a record with `corrupt_at` set answers `503` before any header; a stream that fails verification
  after headers were sent aborts the connection, on the `.gz` and the plain name alike: the reader
  keeps `ErrCorrupt`, and after `ServeContent` (or the plain copy) returns, the handler panics with
  `http.ErrAbortHandler` in the handler goroutine itself, never in a spawned one.
- `Content-Type` is fixed (`application/gzip`, `application/octet-stream`), with
  `X-Content-Type-Options: nosniff`, never a `Content-Encoding`.

**Answers in phase 1.** A public release is served. In an existing tenant and channel, everything
else (a private release, a yanked one, a version or platform the channel does not serve, a name
that never existed) answers **missing**: `401` in a signed channel, identical bytes in every case.
An unknown tenant or channel, or a suspended tenant, answers `404` (names are public). A passthrough
channel answers `404` for every binary until spec 0009 feeds it.

**Headers.**

| Answer | `Cache-Control` | Other |
| --- | --- | --- |
| `200`, `206`, `304` of a public release | `public, no-cache, no-transform` | ETag (spec 0005); on https listeners `Vary: Authorization` |
| `200`, `206`, `304` of a private release (phase 2) | `private, no-store, no-transform` | ETag; `Vary: Authorization` |
| `400`, `401`, `404`, `405`, `503` | `private, no-store` | `Vary: Authorization`; no ETag; `401` adds `WWW-Authenticate: Bearer realm="kista"` |

The `401` answers are byte-identical to each other, and so are the `404` answers of a channel's
paths. `Range`, `If-Range` and `If-None-Match` are looked at only after the authorisation decision.

**Resources.**

- `ReadHeaderTimeout` 10s, `IdleTimeout` 120s, `MaxHeaderBytes` 64 KiB (Entra tokens are large),
  at most 16 concurrent HTTP/2 streams per connection.
- A download slot is taken only for a `GET` that resolved and was authorised (never for `HEAD` or
  `304`): at most `max_downloads` per replica and `max_downloads_per_client` per client address,
  otherwise `503` with `Retry-After`. Since it is taken after the decision, a `503` tells nothing a
  `200` would not.
- No total write timeout (bodies can be large), but the write deadline moves only after every
  `min_rate × write_idle_timeout` bytes, so a client must read at least `min_rate` on average.
- A tenant whose storage domain is not configured answers `503` after the decision, and is logged
  (DuckDB retries `503`, so this costs the client a few retries).

**Client address.** The connection's address, or, when it is in `trusted_proxies`, the rightmost
`X-Forwarded-For` entry that is not. Used for logs and the per-client limit only.

**Background work, health and shutdown.**

- The blob service's public check (spec 0005) every 10 minutes; the re-signer if enabled.
- `/healthz` answers while the process runs. `/readyz` answers from a state computed every 5
  seconds, process-wide only: the store reachable and its schema readable by this binary, and not
  shutting down. A blob domain found public is not a readiness failure (it would take every tenant
  down): that domain's tenants are served `503` after the decision, and it is logged as an error.
- On `SIGTERM`: `/readyz` fails, a drain period passes, `Shutdown` runs with a bound, then remaining
  downloads are aborted; the re-signer stops and releases its lease.
- Logs: one access line per request (method, path, status, bytes, duration, client address,
  listener). Never a header value, a token, or a query string.

### Tokens, issuers and grants (phase 2)

**Egress** (`internal/egress`): every outbound request made on a tenant's behalf (issuer discovery,
JWKS; upstreams in spec 0009) goes through one client.

- Addresses are checked on the connection actually dialed (`net.Dialer.Control`), so DNS rebinding
  and Happy Eyeballs cannot slip past a check made on a different resolution. TLS verification and
  SNI stay on the host name.
- Only global unicast addresses pass. Refused: everything in the IANA special-purpose registries
  (loopback, `0.0.0.0/8`, private, CGNAT, link-local, benchmarking, `192.0.0.0/24`, `240.0.0.0/4`,
  multicast, broadcast, ULA, site-local, documentation), and IPv6 forms that embed an IPv4 address
  are unwrapped and re-checked (mapped, compatible, NAT64 `64:ff9b::/96` and `64:ff9b:1::/48`,
  6to4, Teredo). Cloud metadata and platform endpoints (`169.254.169.254`, `168.63.129.16`,
  `100.100.100.200`, `fd00:ec2::254`) are refused even when allowlisted.
- `egress.allow` (file-only) lists CIDRs (with optional ports) that may be reached despite the rules
  above, for an IdP on a private network. It never names hosts. Without it only port 443 is used.
- `https` only; `http` to loopback only with `egress.allow_loopback_http`, file-only and only with
  `profile: dev`. No userinfo in URLs. No redirects for discovery and JWKS. Response size capped
  (1 MiB) after decompression (transport compression is off), total time capped (10s). The
  deployment's own `public_url` host is refused.
- No proxy from the environment; an explicit, file-only `egress.proxy` for deployments that must
  use one. Through it, kista resolves and checks the target itself and asks the proxy to `CONNECT`
  to the checked address (TLS verification still on the host name), so the proxy cannot be used to
  reach a refused address.
- Errors are one message per class (refused address, cannot connect, TLS, status, too large),
  with the host but nothing else, so `issuer add` is not a port scanner.

**Issuer records** (`issuers` table, per tenant; at most 16):

- a `name` (`[a-z][a-z0-9-]{0,15}`) and the issuer URL (`https`, exact), both immutable (a new URL
  is a new record), each unique within the tenant;
- the accepted algorithms (`RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`,
  `EdDSA`), never `none` or `HS*`;
- `required_claims`: exact claim values every token must carry (`tid=…` for Entra,
  `repository_owner=acme` for GitHub Actions). Issuers that let anyone choose the audience (GitHub
  Actions, Google, a shared Keycloak realm) are only safe with them;
- claim paths for roles, groups and the client id (`azp`, `appid`, `client_id`); a path is a list of
  keys, so a key may contain dots (Auth0's `https://…/roles`);
- `max_token_lifetime` (default 24h, at most 7 days): `exp - iat` above it is refused (DuckDB
  secrets can be persistent);
- discovery runs at `issuer add` (through egress) and in the background; the discovered `issuer`
  must equal the record's URL; the JWKS URI comes from discovery or an explicit `jwks_uri`.

`kista admin issuer add|list|remove`. Grants reference the record by id, and removing a record
removes its grants, so a record added later under the same name starts with none.

**Audiences.** A token must carry, in `aud` (a string or an array, compared exactly), one of its
tenant's audiences:

- the canonical `<public_url>/<tenant>`;
- audiences a **server administrator** assigns (`kista admin tenant audience add <tenant> <aud>`),
  unique across all tenants and never equal to another tenant's canonical audience. Entra cannot
  issue an https audience without a verified domain, hence `api://kista-acme`. For Entra, the
  documentation requires "assignment required" on the app, or any app in the directory can get a
  token for it.

A token for tenant A never works at tenant B. `<public_url>` itself is reserved for the server
level (spec 0007). Changing `public_url` changes every canonical audience; the documentation says
so.

**Verification**, on https requests with `Authorization: Bearer <token>`:

- compact JWS only (three parts), at most 16 KiB; a JWE, the JSON serialisation, duplicate JSON keys,
  an unknown `crit`, `typ` other than absent, `JWT` or `at+jwt`, a missing `kid` mean no valid token.
  `jku`, `x5u`, `jwk` and `x5c` in the header are ignored;
- the tenant comes from the path; the token's `iss` (from the single parse) selects the tenant's
  record with that exact URL;
- the key is the record's JWKS key with that `kid`; its `kty`/`crv` must match the `alg` (`ES256`
  with P-256, `EdDSA` with Ed25519), its `use` is `sig` or absent, its `alg` absent or equal;
  RSA keys are 2048-4096 bits;
- `exp`, `iat` required; `nbf` respected; 60s of skew; the lifetime cap; the audience; the
  required claims;
- JWKS are fetched once per URL (shared by records with the same URL, verifiers stay per record),
  cached by `Cache-Control` within 5m-24h, at most 64 keys. An unknown `kid` triggers at most one
  refresh per URL per minute, single-flight; a request never waits for a refresh that is running or
  rate-limited: its token is not valid now;
- an invalid token is treated as **no token**: public releases are still served (a stale DuckDB
  secret must not break the bootstrap); everything else answers `401`.

**Principals**, namespaced by the issuer record's name (spec 0001): `subject:<issuer>|<sub>`,
`role:<issuer>|<role>`, `group:<issuer>|<group>`, `client:<issuer>|<id>`, and `issuer:<issuer>`
for any valid token of that record. Claim values must be strings (non-string array elements are
dropped), without control characters, at most 256 bytes each and 256 per claim. An Entra groups
overage is not resolved (no Graph lookup). The `server:` prefix is reserved for spec 0007's server
administrators: no tenant principal ever matches a server one.

**Grants** (`grants` table, per tenant): `principal × resource × verbs`.

- Resource: the tenant; a channel; an extension name (in every channel of the tenant, including
  future ones, or in one channel).
- Verbs here: `install` and `admin` (`admin` on the tenant implies every verb in the tenant).
  `publish` and `promote` come with spec 0008, `audit` with spec 0010.
- An `issuer:` grant is refused unless the record has `required_claims`.
- `kista admin grant add|list|remove <tenant> -principal … -verb … [-channel …] [-extension …]`.
  Grant management over HTTP, and who besides server administrators may do it, is spec 0007.
- Grants, issuer records and audiences are read through caches invalidated by a per-tenant
  version, so a change applies at the next request on every replica.

**The decision comes from the path.** A grant names a tenant, channel or extension, all known from
the path before anything is resolved. So: verify the token, compute the caller's principals, decide
whether they hold `install` on (tenant, channel, name), then resolve, with a caller who does not
see only public releases. Private rows are then never read for such a caller, and the work does not
depend on what exists.

| Request | Public release | Private release | Missing, yanked, not served |
| --- | --- | --- | --- |
| no valid token, or the http listener | `200` | `401` | `401` |
| valid token, no `install` | `200` | `404` | `404` |
| valid token, `install` | `200` | `200` | `404` |

**Logs**: a failed token is logged with the issuer record (if one matched) and the reason class,
rate-limited per tenant; never the token or its claims.

### Package layout

```text
internal/release   builds, releases, signatures, the serving key, the re-signer
internal/serve     the server, listeners, routes, health, shutdown
internal/egress    the outbound client with the SSRF guard            (phase 2)
internal/auth      issuer records, JWKS, JWT verification, principals  (phase 2)
internal/authz     + grants; the Authorizer takes principals and an extension resource (phase 2)
internal/store     + migration 0003 (phase 1, min_reader 3), 0004 (phase 2), both additive
```

`authz.Actor` gains a principal set; `Authorizer.Allow` gains the extension resource. Public reads
(`.well-known`) use a store path that needs no authorisation. Code comments that still number grants
as spec 0005 or audit as spec 0009 are corrected.

## Security

- **Fail closed.** No release, no serving-key signature, no configured domain, a corrupt record or
  an unverifiable stream means no body. A footer outside the grammars is refused at release time.
- **No existence leaks.** The decision comes from the path, and a caller who may not see private
  releases never has them resolved: private, yanked, unserved and never-existing paths answer
  identically, after the same work. "Current" is per caller, so a new private version does not
  change an anonymous answer. `503` and download limits apply only after the decision. Tenant and
  channel names are public. Nothing is reachable by body hash.
- **Tokens.** Only over https, only from the tenant's own issuers, only with the tenant's audience,
  with lifetime caps and required claims; never logged or forwarded. Invalid tokens fall back to
  anonymous, which sees only public releases.
- **Tenant isolation.** Principals carry the issuer record's name; removing a record removes its
  grants; audiences are unique across tenants; server principals are a separate namespace.
- **SSRF.** Discovery and JWKS go through egress, checked on the dialed address, without redirects;
  metadata endpoints are unreachable whatever the allowlist.
- **Scheme.** A listener's scheme is configuration; a proxy header can only downgrade.
- **Keys** never leave the signer; signing happens at release and re-sign time, outside
  transactions, and each signature is verified before it is stored. Every served release always has
  a serving-key signature; only an install that straddles the instant of a serving-key move sees
  two, and fails cleanly.
- **DoS.** Download slots per replica and per client, a minimum read rate, header and idle
  timeouts, HTTP/2 stream caps, one range per request, no request bodies, JWKS refresh rate-limited
  and never awaited.

## Testing

- **Store suite** on all three dialects: builds, releases, slots (including `capi:<major>` and the
  `c_struct`/`cpp` exclusion), `seq` and its partial unique index, signatures (256 bytes, the
  composite keys refuse another channel's key), C API rows and the legacy fallback, leases, issuers,
  audiences (unique, not a canonical one), grants.
- **Release service:** add (idempotent, slot conflicts, a version the channel lacks, a passthrough
  channel, no active key, the alias and device names, every grammar), current and rollback,
  `-not-current`, yank, deprecate, visibility; signing outside the transaction (a key retired
  meanwhile), the serving key during a rotation, the re-signer (two replicas, one lease), `key
  retire` refused while needed, with and without `-force`.
- **Serve:** every route and refusal (escapes, dots, trailing slash, query, mismatched names,
  methods, bodies, absolute-form); `.gz` bytes equal `extfile.WriteGzip`; `HEAD`, one range, several
  ranges (whole file), `If-None-Match`, `If-Range`; the plain name and `304`; the answer table with
  bytes compared directly; headers per answer; download limits and the minimum rate; a corrupt
  record (`503`) and a stream corrupted mid-response (aborted); `/readyz` during shutdown; listeners
  (`behind_proxy` with `X-Forwarded-Proto: http`, `dual`), certificate reload, trusted proxies.
- **Phase 2:** egress (every refused range and embedded form, metadata addresses with an allowlist,
  rebinding between resolutions, redirects, size after decompression, ports, the own host); JWT
  (`none`, `HS256` with a public key, wrong `kty`/`crv`, unknown and missing `kid`, rotated JWKS,
  an unknown-`kid` flood, expired, `nbf`, lifetime, audience of another tenant, a GitHub-style token
  carrying the victim's audience without the required claim, JWE, duplicate keys, oversize);
  principals (dotted claim paths, arrays, control characters); grants (tenant, channel, extension,
  `issuer:` refused without required claims, removal applies at once, a removed and re-added issuer
  record has no grants); the answer table, and "current" per caller (a private release newer than a
  public one). A local OIDC fake serves discovery and JWKS with `Cache-Control`, rotates `kid`, and
  signs RS256 and ES256, on an `httptest` TLS server with `egress.allow` for 127.0.0.1.
- **e2e** with the pinned DuckDB against `internal/serve` on `httptest` listeners (spec 0002's
  file-server cases stay: they pin DuckDB's own behaviour):
  - the bootstrap over `http://` with `USING PUBLIC KEY`, `INSTALL httpfs FROM r`, `LOAD`, then
    `INSTALL tresor FROM r` after the rewrite to https (a `dual` listener);
  - `.well-known` over http with httpfs loaded (`.well-known` is read without `ca_cert_file`, so not
    over the test CA); https installs with `USING PUBLIC KEY` and `ca_cert_file`;
  - a `cpp` and a `c_struct` build (`demo_capi`) for one channel version; the versioned path
    (`INSTALL x FROM r VERSION '…'`);
  - a key rotation: activate, re-sign, the serving key moves, a fresh install verifies, an old
    install still loads;
  - phase 2: a private release with an http secret holding a token from the fake IdP (`200`),
    without it (`401`), with another tenant's token (`401`), with a token and no grant (`404`).

## Alternatives considered

- **A static tree written at release time.** It cannot apply grants, and every rotation rewrites it.
- **Serve the active key's signature as soon as it exists.** During a rotation two replicas would
  give one client two signatures between its `HEAD` and its `GET`.
- **Hold a store lock while signing.** A KMS call can outlast the lock timeout and block every
  writer (on SQLite, all of them).
- **A pointer table for "current".** `seq` gives the same choice with one ordering, works per
  caller, and a rollback is one update.
- **Trust `X-Forwarded-Proto` to upgrade.** A client could claim https on an http path.
- **`401` for an invalid token even on public releases.** A DuckDB secret with an expired token would
  break the public bootstrap.
- **`403` for "valid token, no grant".** It confirms that the path exists.
- **The issuer URL as the principal namespace.** Two records with one URL and different claim
  settings would share principals.
- **Allowlisting egress by host name.** A host can resolve to a metadata address.
- **The management API in this spec.** It depends on server-level identity and overlaps the index
  API; it gets its own spec (0007).

## Follow-ups

- Spec 0007: the HTTP API: the index for the node agent and the console, and management (tenants,
  versions, channels, keys with provisioning, issuers, audiences, grants, releases), with server
  administrators (`server:` principals) and grant management by tenant administrators.
- Spec 0008: publication (upload, publishers, API keys, trusted publishing, the init-symbol binding,
  reserved names, blocks; blocks join resolution under the same "same work" rule), and an extension's
  default visibility.
- Spec 0009: upstreams, passthrough serving, and shadowing of core names in passthrough channels.
- Spec 0010: audit, including serve-path events (installs by principal, anonymous counters,
  rate-limited `401` events).
- Later: custom domains; a CDN in front of public releases (never redirecting a serve request off
  host, since httpfs would carry the Bearer token there).
