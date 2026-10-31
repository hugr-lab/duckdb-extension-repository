---
title: Tenants and channels
---

# Tenants and channels

A **tenant** is an organisation's space: its channels, its issuers and grants, its storage domain.
A **channel** is one DuckDB repository inside a tenant, served at
`https://<host>/<tenant>/<channel>`. This page sets them up with `kista admin`; the same steps exist
in the [HTTP API](../api.md) and the [console](../console.md) where noted. The model is in
[Concepts](../concepts.md); the design is in
[spec 0003](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0003-store-tenants-keys/spec.md)
and [spec 0006](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0006-serve/spec.md).

In the examples, `ka` stands for `kista admin -config /etc/kista/kista.yaml`.

## Names

Tenant and channel names are URL path segments: `[a-z0-9][a-z0-9-]{0,62}`. These names are
reserved: `api`, `admin`, `healthz`, `readyz`, `metrics`, `static`, `ui`. A name never changes and
is never reused, not even after its tenant is [deleted](#deleting-a-tenant); there is no deletion of
channels yet. Names are public: anyone can learn that a
tenant or channel exists; the list of tenants is not public.

## DuckDB versions

A channel serves an explicit list of DuckDB versions, chosen from the server-wide list. Register
each version once, with the highest C API version it accepts for each C API major, and its core
extensions:

```bash
kista duckdb core-extensions v2.0.0 >core-v2.0.0.txt
ka version add v2.0.0 -kind release -c-api v1.5.6 -c-api v2.0.0 -core-extensions core-v2.0.0.txt
kista duckdb core-extensions eb0d9df48e -trust-commit | ka version add eb0d9df48e -kind dev -core-extensions -
ka version c-api eb0d9df48e -c-api v2.0.0      # add a major later; an existing one never changes
ka version core-extensions v2.0.0 -core-extensions more.txt   # reserve names the list missed
ka version list
```

- A version name is a release tag, `v<major>.<minor>.<patch>` with an optional lowercase suffix
  such as `-rc1`, or a 10-character lowercase hex source id. Use `-kind release` for tags and
  `-kind dev` for source ids (kista checks the form of the name, not that it matches the kind).
- C API maxima are facts about the engine. They decide which `c_struct` builds a version serves.
  A version without them serves no `c_struct` build.
- The core extensions are required. Each becomes a reserved core name (with the source
  `version:<name>@<commit>`), so no grant but one naming the extension publishes under it
  ([Reserved names](../publishing.md#reserved-names)). `kista duckdb core-extensions <tag or source
  id>` prints them, read from DuckDB's sources at that ref: the in-tree extensions, the ones
  DuckDB builds in or autoloads, and the out-of-tree ones its `.github/config` loads. It fails when
  one of those sources reads empty, or the list lacks `icu`, `json` or `parquet`: DuckDB's sources
  changed layout, and this kista cannot read them. A source id needs `-trust-commit`: GitHub serves
  any fork's commit under DuckDB's name, so only you can say the commit is DuckDB's. Versions
  registered before kista took core extensions keep the names kista knew.
- A name tenants have releases under (not yanked) is refused unless you pass `-force` (API:
  `"force": true`), as for an administrator's reservation: the releases stay, and only grants naming
  the extension reach the name from then on.

### A new DuckDB version

1. `kista duckdb core-extensions <tag>`, then `version add` with it (and its C API maxima).
2. `channel versions <tenant>/<channel> -add <tag>` for each channel that serves it.
3. Mirror or publish the version's builds; the core extensions it brought are reserved from step 1.

Only server administrators add versions (CLI, `POST /api/v1/duckdb-versions`, or the console's
server view).

## Creating a tenant

```bash
ka tenant create acme -display-name "Acme Corp"
ka tenant create acme-cn -display-name "Acme China" -domain cn
ka tenant list
```

`tenant create` prints the new tenant's id. Only server administrators create tenants.

`-holder-id <id>` sets the holder the tenant's licences must name (their `sub`); `ka tenant holder
<name> <id>|-clear` sets or clears it later. Only server administrators change it; the tenant's
administrators see it. See [An expected holder](../licensing.md#an-expected-holder).

### Storage domains

A tenant's extension bodies live in one **storage domain**, a named store configured under
`blob.domains` (see [Storage](../storage.md)). Without `-domain` the tenant lives in `default`,
which every configuration has. The domain:

- must be configured when the tenant is created (otherwise the command fails);
- never changes afterwards; there is no command to move a tenant between domains;
- keeps tenants apart for data residency: deduplication of identical bodies happens only inside a
  domain.

At startup, and with `ka blob check`, kista lists tenants whose domain is no longer configured
(a deleted tenant's is not listed).
Such a tenant is served nothing; it never falls back to another domain.

Domain names follow `[a-z][a-z0-9-]{0,15}`. The domain is shown to server administrators only.

### Limits

A server administrator limits what a tenant's own administrators, publishers and upstreams can
make it grow by (spec 0038): four limits, each a number, `0` (none at all) or `unlimited`.

| Limit | Counts | Ceiling |
| --- | --- | --- |
| `bytes` | the tenant's own catalogue bytes (below) | none |
| `channels` | its channels | 1000 |
| `upstreams` | its upstreams | 100 |
| `publishers` | its own publishers (not distributors') | 200 |

```bash
ka tenant limits acme                                   # what is set, in force and used
ka tenant limits acme -bytes 50GiB -channels 20         # set those named; the others unchanged
ka tenant limits acme -upstreams default                # the configuration's limits: again
```

Over the API, `GET /api/v1/tenants/{t}/limits` (the tenant's administrators and server
administrators) answers each limit's `set` (a number, `"unlimited"` or `"default"`), `limit` (the
value in force; `null` for unlimited bytes) and `used`; `PUT` (server administrators only) takes the
limits it names, and `POST /api/v1/tenants` takes them as `limits` at creation. Each change is a
`tenant.limits` event in the tenant's audit, with each limit's value before and after; the limits
given at creation are one in the server's events, beside `tenant.create`.

- **What bytes count.** Each distinct body of the tenant's builds that the collector keeps (placed in
  a channel, landed in the catalogue, or withdrawn) once, whatever source holds it, mirrors
  included; and the icons of its entries that still have such a build. Builds of distributors'
  publishers, live or removed, are never counted nor refused: a customer at its limit still gets
  what it bought. A purged release's build no longer counts at once, before the collector removes
  it.
- **Who is checked.** Everyone who makes the tenant grow: its administrators, its publishers, a
  server administrator's call, its upstreams' runs and lazy fetches. Only a bundle import and the
  bootstrap are counted but never refused. A server administrator who needs more raises the limit.
- **A refusal** is `429` with the problem type `tenant-limit` and a `limit` member naming the limit,
  without `Retry-After`: it clears as the tenant frees room or its limit is raised. kista's clients
  never retry it. An upstream run without room skips the cells that hold no body, and a run in which
  a cell is refused ends failed, its next run at the usual time
  ([A run at a limit](../upstreams.md#a-run-at-a-limit)).
- **The console's Limits page** shows the tenant's administrators what is set, what applies and
  what is used, read only.
- **Lowering a limit removes nothing.** A tenant over a limit still reads, serves, promotes and
  removes; only growth of that kind is refused until it is back under the limit.
- **The configuration's [`limits:`](../configuration.md#limits)** are the default of a tenant
  without values of its own: a backstop, not a plan. A hosted service sets every tenant's values.

### Suspending a tenant

```bash
ka tenant suspend acme
ka tenant resume acme
```

A suspended tenant is served nothing: DuckDB routes answer `404`, and its API answers `404` to
everyone but server administrators. Nothing is deleted.

Suspending records when it happened (`suspended_at`), and resuming clears it. Suspending is also
the first step of deleting a tenant.

### Deleting a tenant

Deleting a tenant removes its channels, keys, releases, grants, issuers, publishers, upstreams,
documents, feeds and download statistics, and cannot be undone. A distributor's weekly snapshots
(spec 0036) are sums over tenants and are kept: the deleted tenant's share stays in the weeks
already computed, for up to 106 weeks. It takes two steps:

1. **Suspend** the tenant (above). This can be undone.
2. **Delete** it, once it has been suspended for at least `tenants.min_suspended` (default `168h`,
   at least `24h` outside `profile: dev`; see [Configuration](../configuration.md#tenants)). The
   minimum suspension is the window for changing one's mind.

```bash
ka tenant suspend acme
ka tenant show acme                    # "deletable from" says when the delete is accepted
ka tenant delete acme -confirm acme    # the name twice, so a pasted command cannot hit another tenant
```

- Only a server administrator deletes a tenant: with `kista admin`, with
  `DELETE /api/v1/tenants/{t}` ([HTTP API](../api.md#deleting-a-tenant)), or with **Delete…** in the
  console's server view. A tenant administrator cannot delete its own tenant.
- The delete is refused for an active tenant, for one suspended for less than the minimum (the
  answer says when it may be deleted), for one already being deleted, and for one suspended by a
  kista older than this feature: resume it and suspend it again.
- `tenant delete` prints the **keys to retire**: the tenant's channel keys and attestation keys,
  with their fingerprints and signer references. kista only signed with them; disable or destroy
  them in their key sources (Vault, Key Vault, a key file). It also warns when `gc.interval` is `0`,
  since the tenant's bodies then stay in storage until `ka blob gc -apply` runs.

From the moment of the delete the tenant is **closed**: DuckDB routes and every API route answer
`404` exactly as for a tenant that never existed, every change is refused, and its upstreams, feeds
and re-signing stop. Only server administrators still read its record
(`GET /api/v1/tenants/{t}`, `ka tenant show`) and its events (`ka events list acme`). Its state
shows as `deleting`.

**The reaper.** Two hours after the delete (the settle period, so uploads and upstream fetches that
started before it are done), `kista serve` removes the tenant's rows in small batches in the
background, one replica at a time. A pass that fails or is interrupted is resumed later (after 1
minute, doubling up to 1 hour). When nothing is left it writes the **tombstone**: the tenant's row
stays with its id, name, storage domain and dates, its display name is cleared, and its state shows
as `deleted`. A replica reaps only while the database's schema is exactly its own version.

```bash
ka tenant show acme                    # the deletion's progress, last error and keys to retire
ka tenant list -state deleting,deleted # by default, list shows every state but deleted
ka tenant reap acme                    # reap here and now, after the settle period
ka tenant reap acme -force             # skip the settle period: only when nothing else runs
```

`tenant reap` runs the reaper in the command and returns when the tombstone is written. `-force` is
for a server where no `kista serve` and no other command is running, such as a SQLite store stopped
for maintenance.

A tombstone's storage domain must stay configured until the [storage collector](../storage.md) has
removed its bodies: the collector only runs over configured domains.

#### What is kept, and for how long

| What | Where | How long |
| --- | --- | --- |
| The tombstone: id, name, storage domain, created, suspended, deleting and deleted times | `tenants` | forever: the name is never reused |
| The deletion record: who started it, the former channel ids, the keys to retire (kind, id, fingerprint, signer reference) | `tenant_deletions` | forever; server administrators only |
| Assigned audiences | `tenant_audiences` | forever: another tenant cannot take them and accept tokens minted for this one |
| Key fingerprints | `key_fingerprints` | forever: a key that signed for this tenant never signs for another |
| The tenant's events (actor names, reduced client addresses) | `events`, and every configured sink | `events.retention` in the database (sinks keep them as long as they keep anything) |
| Bodies | blob storage | Builds go at the reap. Each body is then collected on a collector pass at least `gc.grace` after it was last used, and its stream a grace later. With `gc.interval: 0`, nothing goes until `kista admin blob gc -apply` runs. |
| Attachment contents | blob storage, `attachments/` | collected as orphans by the same passes |
| Objects in versioned or object-locked buckets | the bucket | as the bucket's own policy says |
| Key material | the key sources (Vault, Key Vault, a key file) | until the operator retires it; kista only signs with it |
| Statements given to subscribers | other kista servers | their choice: their feed shows `unauthorized`, and their documents stay |
| Backups | the operator's database and blob backups | their own retention |
| Logs and traces naming `kista.tenant.name` | the telemetry backend | its own retention |

No withdrawals are issued for documents the tenant offered to subscribers. Restoring a database
backup taken before a deletion brings the tenant back: see
[Backup and restore](backup-restore.md#a-backup-from-before-a-tenant-deletion). The design is in
[spec 0019](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0019-tenant-deletion/spec.md).

## Creating a channel

```bash
ka channel create acme/prod -kind signed
ka channel versions acme/prod -add v2.0.0 -add eb0d9df48e
ka channel list acme
```

`channel versions` without flags prints the channel's versions. Removing a version
(`-remove v2.0.0`) stops serving every build under it in that channel.

A tenant administrator may create channels and change their DuckDB versions over the API
(`POST /api/v1/tenants/{t}/channels`) or in the console. Keys are registered by a server
administrator only (see below).

A channel serves one **package type**, set when it is created and never changed:
`duckdb_extension` by default, the only type there is today (`-type` in the CLI, `"package_type"`
in the API; a passthrough channel's is always `duckdb_extension`). Builds are placed, promoted and
followed into a channel of their own type only (another is `409`), and on a channel's routes a
`?type=` other than the channel's is `400`. The channel lists show a channel's type only when it is
not the default. See [Concepts](../concepts.md#packages-and-package-types).

A signed channel may have a **hold** (`ka channel hold acme/prod 1d`, its administrators'; `0` for
none): an automatic placement into it waits until an entry's arrival plus the hold, and it asks no
lazy mirror. See [Holds](../upstreams.md#holds).

### Channel kinds

| | `signed` | `passthrough` |
| --- | --- | --- |
| Serves | `body ‖ channel signature` | `body ‖ DuckDB's core signature`, byte-identical to `extensions.duckdb.org` |
| Keys and `.well-known` | yes; DuckDB pins the keys at `CREATE EXTENSION REPOSITORY` | none; `.well-known` answers `404` |
| Fed by | `release add`, publication, promotion, upstreams of any kind | `duckdb-core` upstreams only |
| Private releases and grants | yes | no: every release is public, tokens are never looked at |
| DuckDB clients | `INSTALL x FROM <repo>`, `LOAD x FROM <repo>` | `custom_extension_repository` and `autoinstall_extension_repository`, so DuckDB can autoload core extensions |

A channel's kind never changes. Use separate channels, with separate keys, for builds of different
trust: a `staging` channel signs whatever a publisher uploads, so its key must not be trusted
where `prod` is.

**Passthrough channels** exist for autoloading. DuckDB treats those URLs as core-typed and checks
them against its built-in core keys only, so only core extensions work there. If the tenant
releases its own build under a core name into a signed channel (its own `httpfs`, for example), the
name becomes a **shadow**: the tenant's passthrough channels stop serving DuckDB's build of it, so
autoloading never silently picks DuckDB's build over the replacement. List and remove shadows with
`ka shadow list acme` and `ka shadow remove acme httpfs`. See [Upstreams](../upstreams.md).

## Binding keys to a signed channel

A signed channel needs an active key before anything can be released into it. A key belongs to
exactly one channel, and a fingerprint exists once on the server.

```bash
ka key check -signer prod:ext-acme-prod/0f1e...      # optional: open, check and probe the key
ka key add acme/prod -signer prod:ext-acme-prod/0f1e... -active
ka wellknown acme/prod
```

- `-signer` is a signer reference into a configured key source (`<source>:<key>`), or
  `file:<name>` for a key file in `signers.file_dir` (only with `profile: dev` or
  `signers.allow_file: true`). See [Signing keys](../signing-keys.md).
- `key add` opens the key, checks it is RSA-2048 with `e = 65537`, and verifies a probe signature.
  It prints the key id and fingerprint.
- `-active` works only for a channel that has **never** had a key: no client trusts the channel yet.
  Every later key starts `trusted` and is activated through rotation (see
  [Key rotation](key-rotation.md)).
- A channel holds at most 16 keys that are not retired by default (`limits.ceilings.signing_keys`), and its `.well-known` must fit in 64 KiB.
- A passthrough channel takes no keys.

Only a server administrator registers a key, through the CLI, `POST …/channels/{c}/keys` or the
console's key view. A tenant cannot name a signer reference.

## Issuers per tenant

Private releases and management by tenant administrators need tokens, and each tenant trusts its own
token issuers. An issuer record names the issuer URL, the accepted algorithms, the claims every
token must carry, and where roles, groups and the client id are found:

```bash
ka issuer add acme -name corp -url https://login.microsoftonline.com/<tenant id>/v2.0 \
  -require tid=<tenant id> -roles-claim roles -max-lifetime 24h
ka issuer list acme
```

- At `issuer add` kista fetches discovery and the JWKS through its egress rules; the command fails
  if they cannot be fetched.
- A tenant holds at most 16 issuer records by default (`limits.ceilings.issuers`). A record's name and URL never change; a new URL is a new
  record.
- Removing a record removes its grants.
- A token must carry the tenant's audience: the canonical `<public_url>/<tenant>`, or an audience a
  server administrator assigns with `ka tenant audience add acme api://kista-acme` (unique across
  tenants; nothing under `serve.public_url` can be assigned).
- For the administration console, register its public client at the issuer:
  `ka issuer console acme corp -client-id <id>`.

Tenant administrators manage issuer records themselves over the API and in the console; audiences
are server-only. Issuers, claims and principals are covered in [Access](../access.md).

## Tenant administrators

A tenant administrator is a principal of one of the tenant's issuers that holds the `admin` verb on
the whole tenant:

```bash
ka grant add acme -principal 'role:corp|kista.admin' -verb admin
ka grant add acme -principal 'subject:corp|<object id>' -verb admin
ka grant list acme
```

| Grant | Lets the principal manage |
| --- | --- |
| `admin` on the tenant | issuers, grants, channels, and everything inside the tenant |
| `admin` with `-channel prod` | that channel's DuckDB versions, keys (activate, retire) and releases |
| `admin` with `-extension x` (optionally `-channel`) | that extension's releases |
| `audit` on the tenant | nothing: it reads the tenant's events and statistics, and its issuers, audiences, grants, extension groups, publishers with their keys, and upstreams, with the administrators' answers (spec 0040) |

Rules:

- A tenant administrator cannot create tenants, add DuckDB versions, assign audiences, register a
  key, or use `force` on key changes: those stay with server administrators.
- `admin` cannot be granted to a whole issuer (`issuer:` principals).
- A tenant administrator cannot remove the last tenant-wide `admin` grant, or an issuer record whose
  removal would remove it. A server administrator or the CLI can always recover a tenant.
- Management over the API needs a fresh token: issued at most `auth.admin_token_max_age` ago
  (default 1 hour).

## A complete example

```bash
kista duckdb core-extensions v2.0.0 | ka version add v2.0.0 -kind release -c-api v1.5.6 -c-api v2.0.0 -core-extensions -
ka tenant create acme -display-name "Acme Corp"
ka channel create acme/prod -kind signed
ka channel versions acme/prod -add v2.0.0
ka key add acme/prod -signer prod:ext-acme-prod/0f1e... -active
ka channel create acme/core -kind passthrough
ka channel versions acme/core -add v2.0.0
ka issuer add acme -name corp -url https://login.example.com/realms/acme -require azp=duckdb
ka grant add acme -principal 'group:corp|kista-admins' -verb admin
```

Then DuckDB clients use `https://<host>/acme/prod` as described in
[Using kista from DuckDB](../duckdb.md), and the passthrough channel is fed by a `duckdb-core`
upstream ([Upstreams](../upstreams.md)).
