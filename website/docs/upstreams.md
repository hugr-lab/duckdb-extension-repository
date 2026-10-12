---
title: Upstreams
---

# Upstreams

An **upstream** fills one channel of a tenant from another DuckDB repository: DuckDB's core
repository, DuckDB's community repository, or any other DuckDB repository (another kista). kista
fetches the binaries the tenant allows, verifies the upstream's signature over the exact bytes it
stores, checks the file as a [publication](publishing.md) is checked, and releases it:

- into a **signed** channel, re-signed with the channel's key: the signature then means "checked
  and allowed by this tenant";
- into a **passthrough** channel, with DuckDB's own core signature, so DuckDB can autoload it from
  the tenant.

Upstreams are managed by tenant administrators. Runs happen in `kista serve`; the CLI only records
a request. The design reasoning is in
[spec 0009](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0009-upstreams/spec.md).

## Kinds and modes

| Kind | Source | Keys checked at intake |
| --- | --- | --- |
| `duckdb-core` | `https://extensions.duckdb.org` | DuckDB's core keys, carried by kista from the pinned DuckDB |
| `duckdb-community` | `https://community-extensions.duckdb.org` | DuckDB's community keys, from the pin |
| `repository` | a `prefix` you give: `https://host[:port]/<tenant>/<channel>` | the fingerprints you pin (`keys`) |

| Mode | Behaviour |
| --- | --- |
| `mirror` (default) | runs on a schedule and on demand, fetching every cell of its matrix |
| `pull-through` | never runs; a granted caller's miss on a DuckDB route fetches that one binary in the background |

An upstream feeds exactly one channel. Two channels fed from the same source are two upstreams;
bodies are stored once per storage domain either way. A name is on the allowlist of at most one
upstream of a channel.

## What is fetched

A repository has no listing, so the **allowlist** names what to fetch. The work is a matrix:

```text
channel's DuckDB versions  x  upstream platforms  x  allowlisted names
-> <prefix>/<duckdb version>/<platform>/<name>.duckdb_extension.gz
```

Each element is a **cell**. The path serves the upstream's current build, so a version older than
the upstream's current one cannot be mirrored. An allowlist entry's `versions` filters what is
accepted: empty means any extension version, otherwise only the exact versions listed.

Limits: 20,000 cells an upstream and 100,000 a tenant (adding an entry or a platform that would
exceed them is `400`; a run whose matrix grew past them fails without fetching), 100 upstreams a
tenant, 1,000 entries an upstream, 100 versions an entry, 10 pinned keys.

**Names.** An entry is a canonical name: an alias (`postgres`) is refused with its canonical name
(`postgres_scanner`). A core name is accepted only from a `duckdb-core` upstream and a community name
only from a `duckdb-community` upstream, unless the entry sets `allow_reserved` (a tenant that mirrors
its own build of `httpfs` from another kista says so).

## Intake verification

A cell's binary is released only if every step passes:

1. **Fetch** through the egress client (below): no redirects, the compressed answer at most
   `blob.max_body` + 1 MiB, at no less than `upstreams.min_rate` after 30 seconds, within
   `upstreams.fetch_timeout`. The answer is spooled to a temporary file first, so a slow upstream
   never holds a storage ingest slot. `If-None-Match` is sent with the cell's last ETag when its last
   outcome was `released`, `unchanged`, `yanked` or `conflict`.
2. **Decompress** a gzip answer, under `blob.max_body` + 256 bytes.
3. **Signature**: the last 256 bytes must verify over the body with the upstream's keys. The key that
   verified is recorded.
4. **Footer**: the platform is the cell's; a `cpp` or `C_STRUCT_UNSTABLE` build's DuckDB version is
   the cell's; a `C_STRUCT` build is one the channel serves; the extension version is on the entry's
   `versions` when given.
5. **File**: the format matches the platform and the binary exports the name's entry point, as for a
   publication. A signed binary of one extension is never served under another name.
6. Under the channel lock: a **blocked** body, a name the tenant **shadows** with its own build, and
   the slot. A slot that holds the same body (in any state) is `unchanged` or `yanked`: an
   administrator's yank, visibility or current choice survives every run. A slot holding another
   body is a `conflict`: versions are immutable, so the version stays as it is in this channel.
7. The body is committed and released with origin `upstream`. The upstream's original signature is
   kept with it (a passthrough channel serves that one; a signed channel serves its own), and the
   provenance records the upstream, kind, URL, ETag, verifying key, fetch time and credential.
   The new release becomes **current** for its name and platform: core and community versions are
   often commit hashes, so kista does not order them; the newest accepted build is current.

A refused cell is logged and counted; it never stops the run. A cell that turns `rejected`,
`conflict`, `blocked` or `shadowed` (from another outcome) is recorded as an `upstream.rejected`
event.

## Adding an upstream

```bash
# DuckDB core extensions into a signed channel, private by default
kista admin -config /etc/kista/kista.yaml upstream add acme core \
  -kind duckdb-core -channel prod \
  -platforms linux_amd64,linux_arm64,osx_arm64 \
  -extensions httpfs,spatial,postgres_scanner

# a few community extensions, public
kista admin -config /etc/kista/kista.yaml upstream add acme community \
  -kind duckdb-community -channel prod -platforms linux_amd64 -extensions h3,prql -public

# another kista's channel, its signing key pinned by fingerprint
kista admin -config /etc/kista/kista.yaml upstream add acme hugr \
  -kind repository -prefix https://kista.hugr.example/hugr/prod \
  -keys sha256:3f9a… -channel prod -platforms linux_amd64 -extensions tresor,hugr_node
```

| Flag | Field | Meaning |
| --- | --- | --- |
| `-kind` | `kind` | `duckdb-core`, `duckdb-community` or `repository` (fixed at creation) |
| `-channel` | `channel` | the channel it feeds (fixed) |
| `-platforms` | `platforms` | at least one, in DuckDB's platform grammar |
| `-extensions` | `extensions` | the allowlist; `*` for any name (pull-through only) |
| `-prefix` | `prefix` | `repository` only: an `https://` URL with an optional port and path (fixed) |
| `-keys` | `keys` | `repository` only, at least one: `sha256:<64 lowercase hex>` |
| `-public` | `visibility` | releases it makes from now on are public; default `private` |
| `-mode` | `mode` | `mirror` (default) or `pull-through` (fixed) |
| `-credential` | `credential` | `repository` only: a configured credential's name (see [Private upstreams](#private-upstreams)) |

Over the API, the same record is `POST /api/v1/tenants/{t}/upstreams` with a JSON body:

```json
{
  "name": "core",
  "kind": "duckdb-core",
  "channel": "prod",
  "platforms": ["linux_amd64", "osx_arm64"],
  "extensions": [{"name": "httpfs"}, {"name": "spatial", "versions": []}],
  "visibility": "private"
}
```

For a `repository` upstream, kista reads the upstream's `.well-known/duckdb-extension-repo.json` when
the upstream is added and at the start of every run; only keys whose fingerprints are pinned are
used. Adding an upstream whose file lists none of its pinned keys is `400`. A rotation at the
upstream is followed by pinning the new fingerprint, never automatically. On the upstream kista,
`kista admin key list <tenant>/<channel>` shows each key's fingerprint.

Adding an upstream or an entry whose name already has a live replacement in its channel (a release
of another origin) is `409` until an administrator drops the entry or yanks those releases.

## Changing an upstream

| Route (under `/api/v1/tenants/{t}/upstreams`) | CLI |
| --- | --- |
| `GET /`, `GET /{name}` | `upstream list <tenant>`, `upstream show <tenant> <name>` |
| `DELETE /{name}` (`If-Match`) | `upstream remove <tenant> <name>` |
| `POST /{name}/public`, `/private`, `/pause`, `/resume` (`If-Match`) | `upstream public\|private\|pause\|resume <tenant> <name>` |
| `GET, POST /{name}/extensions`; `DELETE /{name}/extensions/{ext}` | `upstream extension add <tenant> <name> <ext> [-versions v1,v2] [-allow-reserved]`, `upstream extension remove …` |
| `GET, POST /{name}/platforms`; `DELETE /{name}/platforms/{platform}` | `upstream platform add\|remove <tenant> <name> <platform>` |
| `GET, POST /{name}/keys`; `DELETE /{name}/keys/{fingerprint}` | `upstream key add\|remove <tenant> <name> <fingerprint>` |
| `POST /{name}/sync[?dry_run=true]` | `upstream sync <tenant> <name> [-dry-run]` |
| `GET /{name}/cells[?outcome=…]` | `upstream cells <tenant> <name> [-outcome <outcome>]` |
| `POST /{name}/credential {"credential": name or ""}` (`If-Match`) | `upstream credential <tenant> <name> <credential>\|-` |

`POST /{name}/extensions` on an existing name replaces its `versions` and `allow_reserved` (`200`).
The upstream's `ETag` moves with every change to it or its lists; a sync does not move it.

**Removing** an upstream keeps the releases it made (they belong to the channel; yank them if you
want them gone) and removes its cells; the names it provided stop being reserved.

**Pausing** an upstream stops runs after the cell in progress, and holds the current release:
narrowing an entry's `versions` holds a version too.

**Visibility** applies to releases the upstream makes from now on. Existing releases keep theirs;
change them per release (see [Publishing](publishing.md#changing-a-release)).

## Runs and schedule

- **Scheduled**: an active `mirror` upstream is due `upstreams.interval` (default 6h, within
  15m..168h) after its last run ends, with up to 10% jitter; a mirror never run is due at once.
  Adding a DuckDB version to the channel, adding a platform, adding or changing an entry, or resuming
  the upstream makes it due at once. A run interrupted by an error or a lost lease is due again after
  5 minutes.
- **On demand**: `sync` sets a request; a running `kista serve` takes it within 30 seconds. A request
  that arrives during a run is served by another run right after it. A sync of a paused or
  pull-through upstream is `409`.
- **Dry run** (`?dry_run=true`, `-dry-run`): fetches and checks every cell without releasing,
  committing or touching the cells and the schedule. Its counts (`would_release` for a cell it would
  release) and first 20 problems go to `last_run`.
- Every replica polls every 30 seconds. One run at a time per upstream (a lease,
  `kista/upstream/<id>`, renewed every 30 seconds). A replica fetches at most
  `upstreams.concurrency` cells at once (default 4), and at most 2 at once per upstream host.
- A run follows the record as it is: pausing or removing the upstream stops it after the cell in
  progress; a removed entry, platform or key applies to the next cell.

```yaml title="kista.yaml"
upstreams:                   # file-only
  concurrency: 4             # cells fetched at once per replica; 1..32
  fetch_timeout: 10m         # one file's download; 1m..2h
  min_rate: 64KiB            # a download's least rate after 30s; at least 1KiB
  interval: 6h               # between a mirror's scheduled runs; 15m..168h
  negative_ttl: 1h           # pull-through: a miss is not retried within it; 1m..24h
  credentials: []            # private upstreams, below
```

## Statuses

**Upstream** `state`: `active` or `paused`. **Run state** (`run` in the API): `idle`, `requested`,
`dry_run_requested` or `running`. `last_run_at` and `last_run` record the last run:

```json
{
  "started": "2026-10-12T06:00:00Z",
  "ended": "2026-10-12T06:02:41Z",
  "counts": {"released": 3, "unchanged": 41, "missing": 4},
  "problems": [
    {"duckdb_version": "v1.4.1", "platform": "linux_arm64", "name": "spatial", "outcome": "missing"}
  ]
}
```

`error` is set when the run failed before its cells. `problems` holds the first 20 cells that were
neither released nor unchanged.

**Cell outcomes** (`upstream cells`, `GET …/cells?outcome=`):

| Outcome | Meaning |
| --- | --- |
| `released` | a new release was made |
| `unchanged` | the slot already holds this body |
| `yanked` | the slot holds this body, yanked; it stays yanked |
| `conflict` | the slot holds another body; the version is immutable in this channel |
| `missing` | the upstream answered `404` (also a private upstream without the grant) |
| `rejected` | a check failed: signature, footer, format or entry point |
| `blocked` | the body is blocked in the tenant |
| `shadowed` | the tenant serves its own build of this name in the channel |
| `failed` | the fetch failed (network, size, rate, the upstream's `401`/`403`); retried by the next run |

Cells that leave the matrix are removed by the next run; removing an entry or a platform removes
its cells at once.

## Reservation and shadowing

A name an upstream of a tenant lists is **reserved in the tenant**, like DuckDB's core and community
names: publishing or promoting it needs a grant that names the extension (see
[Publishing](publishing.md#reserved-names)). A replacement published into the channel later wins: the
upstream's cells for it become `shadowed`. In the index, an upstream release carries
`origin: "upstream"` and the upstream's name; a replacement of an upstream-provided name shows
`shadows: upstream`.

## Passthrough channels

A passthrough channel serves DuckDB's own core files with DuckDB's own core signature, so DuckDB can
**autoload** from the tenant. Every repository DuckDB is given by URL is core-typed, and autoloading
and a bare `LOAD` trust DuckDB's core keys only; a re-signed file would not autoload.

```bash
kista admin -config /etc/kista/kista.yaml version add v1.4.1 -kind release   # once per server
kista admin -config /etc/kista/kista.yaml channel create acme/core -kind passthrough
kista admin -config /etc/kista/kista.yaml channel versions acme/core -add v1.4.1
kista admin -config /etc/kista/kista.yaml upstream add acme duckdb \
  -kind duckdb-core -channel core -platforms linux_amd64 -extensions httpfs,spatial,iceberg
```

```sql
SET autoinstall_extension_repository = 'http://kista.example.com/acme/core';
```

- It is fed by `duckdb-core` mirror upstreams only. Community and repository binaries would never
  load from it; pull-through is refused (its callers carry no token).
- Its releases are public and served without tokens, over `http` and `https`, `GET` and `HEAD`. A
  token is never looked at, and a miss is `404`. It has no keys and no `.well-known` file. Setting an
  upstream or a release of it private is refused.
- Nothing is published or promoted into it.
- DuckDB's built-in client speaks plain `http` only and follows no redirects, so an offline autoload
  starts from an `http://` repository; the listener must accept plain http (see
  [Configuration](configuration.md)).

**Tenant shadows.** Releasing into any signed channel of the tenant a build of a core name that is
not DuckDB's own (its original signature does not verify with DuckDB's core keys) records the name
in the tenant's **shadows**. The tenant's passthrough channels then answer `404` for that name, so
autoloading never silently picks DuckDB's build over the tenant's own. Only a tenant administrator
removes a shadow; yanking or blocking the replacement does not bring DuckDB's build back.

```bash
kista admin -config /etc/kista/kista.yaml shadow list acme
kista admin -config /etc/kista/kista.yaml shadow remove acme httpfs
```

API: `GET /api/v1/tenants/{t}/shadows`, `DELETE …/shadows/{name}`.

## Pull-through

A `pull-through` upstream is not run. When a request on a DuckDB route of a signed channel misses,
kista offers the cell to an in-memory queue, after the answer is decided, when:

- the caller holds a valid token with `install` on (channel, name). Anonymous callers and callers
  without the grant never cause a fetch, and every caller gets the answer it would get without
  pull-through;
- the cell is in an upstream's matrix: the request's DuckDB version and platform, and an allowlisted
  name or `*`. The versioned path never triggers a fetch;
- the cell is not in the replica's negative cache: every outcome but `released` keeps it from being
  fetched again for `upstreams.negative_ttl` (default 1h), a failure for a minute.

The client's request still misses; a later `INSTALL` finds the release. `*` is allowed only on a
pull-through upstream, without versions or `allow_reserved`, and not on `duckdb-core`. It never
matches a reserved name, an alias, or a name another upstream of the channel lists, and it reserves
no name in the tenant. Limits per replica: 256 queued misses (32 per tenant), 4 fetches at once and
60 a minute per tenant, `upstreams.concurrency` fetches at once, 10,000 cells in the negative cache.

```bash
kista admin -config /etc/kista/kista.yaml upstream add acme community-pt \
  -kind duckdb-community -channel prod -platforms linux_amd64 -extensions '*' -mode pull-through
```

## Private upstreams

A `repository` upstream whose channel serves private releases (another kista's private channel) needs
a token that the upstream's tenant accepts: a JWT from one of its issuers, with its audience, for a
principal it grants `install` (see [Access](access.md)). kista obtains it without static secrets where
the platform has an identity.

**Credentials are configuration, never data.** The operator names them in `upstreams.credentials`;
a tenant administrator picks one by name. The database and the API hold the name only.

```yaml title="kista.yaml"
upstreams:
  credentials:
    - name: hugr-acme                      # [a-z][a-z0-9-]{0,31}, unique
      tenants: [acme]                      # who may use it, or ["*"]; required
      prefixes: ["https://kista.hugr.example/hugr/"]   # https upstream prefixes it is sent to; required
      allow_public: false                  # releases it brings may be made public
      kind: client_credentials             # client_credentials | token_file
      token_url: https://login.microsoftonline.com/<tenant-id>/oauth2/v2.0/token
      client_id: 00000000-0000-0000-0000-000000000000
      scope: api://kista-hugr/.default     # or audience: …, as the IdP wants
      client_auth: azure                   # azure | file | key_file | secret
    - name: cluster-b
      tenants: [acme]
      prefixes: ["https://kista-b.internal.example/acme/"]
      kind: token_file
      token_file: /var/run/secrets/kista-b/token   # read at each fetch (a projected ServiceAccount token)
```

| Field | Meaning |
| --- | --- |
| `kind: token_file` | `token_file`: an absolute path, read at every fetch |
| `kind: client_credentials` | `token_url` (https), `client_id`, `scope` or `audience`, and `client_auth` |
| `client_auth: azure` | kista's own managed or workload identity (`azure.identity`) as the client assertion |
| `client_auth: file` | `assertion_file`: a projected token read at each request |
| `client_auth: key_file` | `private_key_jwt` with `key_file` (ZITADEL's JSON, or PEM with `key_id`); `assertion_audience` the JWT's `aud` (default `token_url`) |
| `client_auth: secret` | `client_secret_file` or `client_secret_env`: the last resort |
| `allow` | CIDRs with ports where the token endpoint lives, beside `egress.allow` (`client_credentials` only) |

```bash
kista admin -config /etc/kista/kista.yaml upstream credentials       # names, tenants, prefixes
kista admin -config /etc/kista/kista.yaml upstream add acme hugr -kind repository \
  -prefix https://kista.hugr.example/hugr/prod -keys sha256:3f9a… \
  -channel prod -platforms linux_amd64 -extensions tresor -credential hugr-acme
```

- An upstream may name a credential only when its tenant is listed and its prefix starts with one of
  the credential's prefixes. `duckdb-core` and `duckdb-community` upstreams refuse a credential.
- The token is sent in `Authorization: Bearer` to that prefix only, never on a redirect, never in a
  log line, a cell, an event or an answer.
- A token is cached until a minute before it expires (or half its life when that is shorter), and
  fetched again once on a `401`; a refused token younger than 30 seconds is kept, so an upstream
  refusing every token costs one token request in 30 seconds.
- **Visibility**: a public upstream takes a credential only when the credential sets `allow_public`
  (`400` otherwise). Releases a credentialed upstream brings are private unless the credential, as
  configured at the time, sets `allow_public`. Making such a release public, or promoting it as
  public, is refused unless the credential, as configured now, allows it.
- **Failures**: the upstream's `401` or `403` is a `failed` cell with the detail
  `upstream.auth: the upstream answered <status>`; a removed credential gives
  `upstream.auth: the credential is not configured for this upstream` until an administrator picks another. The IdP refusing kista's own client
  (`invalid_client`) is logged as an error and fails the run.

`GET /api/v1/credentials` lists, to server administrators, each credential's name, kind, tenants,
prefixes and `allow_public`, never its settings. The same credentials serve feed subscriptions (see
[Attachments](attachments.md)).

## Egress

Every outbound request kista makes on a tenant's behalf (upstream fetches, issuer discovery and JWKS,
token endpoints) goes through one client with an SSRF guard:

- Only global unicast addresses are dialed, checked on the connection actually made, so DNS
  rebinding cannot slip past. Private, loopback, CGNAT, link-local, multicast, documentation and other
  special-purpose ranges are refused, including IPv4 embedded in IPv6 forms. Cloud metadata addresses
  (`169.254.0.0/16`, `fe80::/10`, `168.63.129.16`, `100.100.100.200`, `fd00:ec2::/112`) are refused
  even when allowlisted.
- `https` only, port 443 unless an allowlist entry names other ports; no user info in URLs; no
  redirects; the deployment's own `serve.public_url` host is refused.
- No proxy from the environment.

```yaml title="kista.yaml"
egress:                      # file-only
  allow:                     # private networks that may be reached: CIDRs, never host names
    - cidr: 10.20.0.0/16
      ports: [443, 8443]     # empty: 443
  proxy: http://proxy.internal:3128   # optional; kista resolves and checks the target itself
  ca_file: /etc/kista/internal-ca.pem # optional: trust only this CA bundle
  timeout: 10s               # per request for JSON fetches; 1s..60s
  allow_loopback_http: false # plain http to loopback; profile dev only
```

Through `proxy`, kista asks the proxy to `CONNECT` to the address it checked, so the proxy cannot be
used to reach a refused address. Errors are one message per class (refused, cannot connect, status,
too large) with the host only, so adding an upstream or an issuer is not a port scanner. Upstream
file downloads use `upstreams.fetch_timeout` and `upstreams.min_rate` instead of `egress.timeout`.

Upstream changes, runs, releases and rejected cells are recorded as events (`upstream.add`,
`upstream.change`, `upstream.run`, `upstream.release`, `upstream.rejected`, ...; see
[Observability](observability.md)).
