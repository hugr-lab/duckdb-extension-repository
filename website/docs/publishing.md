---
title: Publishing and promotion
---

# Publishing and promotion

Your own builds reach a channel in one of three ways:

- **over the API**: a person with a tenant token, or a **publisher** (CI of any forge, signing its
  requests with its key), uploads the file into a signed channel;
- **with the CLI** on the server: `kista admin release add`, as the server administrator;
- **by promotion**: a release already in one channel (usually `staging`) is released in another
  (`prod`) with that channel's signature. Nothing is copied.

Every route below is under `/api/v1/tenants/{tenant}`. The design reasoning is in
[spec 0008](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0008-publication/spec.md).

## What is checked

kista checks the uploaded file before anything is committed. A file that fails any check is refused
with `400`, naming the check.

1. **The footer.** The `version` and `platform` you declare must equal what the file's footer says.
   A CI job that picked the wrong artifact fails instead of publishing it.
2. **The metadata prefix.** The 22-byte prefix DuckDB's build writes before the metadata must be
   there. kista never rewrites a body it signs.
3. **The binary format** matches the platform: `linux_*` is an ELF shared object, `osx_*` a thin
   Mach-O dylib or bundle, `windows_*` a PE DLL, on the platform's machine (`amd64`, `arm64`).
   `wasm_*` and unknown platforms are refused.
4. **The entry point.** The binary must export the symbol DuckDB looks up to load the name, and no
   other extension's entry point:

   | ABI in the footer | Symbol |
   | --- | --- |
   | `CPP` (or empty) | `<name>_duckdb_cpp_init` |
   | `C_STRUCT_UNSTABLE`, or `C_STRUCT` with C API `v2.x.y` | `<name>_init_c_api_v2` |
   | any other `C_STRUCT` | `<name>_init_c_api` |

   So a binary built as `y` cannot be published as `x`. This is a naming check, not a code review:
   a publisher granted a name controls the code behind it.
5. The trailing 256 bytes (a signature the upload may carry) are ignored and replaced by the
   channel's signature.

To see what a file's footer says before you publish it, run `kista ext inspect <file>`: it prints
the platform, ABI, DuckDB or C API version, extension version, body hash, and the licence keys
of an extension built with the kista SDK.

The channel must also serve the build's DuckDB version: a `cpp` or `c_struct_unstable` build needs
its exact DuckDB version in the channel's list, a `c_struct` build a channel version whose C API
maximum accepts it (see [Tenants and channels](administration/tenants-and-channels.md)). Otherwise
the answer is `400`.

## Versions, platforms and slots

A release fills a **slot**: (channel, source, name, extension version, platform, and the DuckDB
version for `cpp` and `c_struct_unstable` builds, or `capi:<major>` for `c_struct` builds). One
extension version therefore ships one file per platform and DuckDB version, and may ship both a C
API v1 and a v2 build. A slot is its source's ([A name's source in a
channel](upstreams.md#a-names-source-in-a-channel)): another source may release the same version of
the name in the channel, and the channel serves the one whose source it selected.

- **A released version is immutable** within its source, in every channel of the tenant and in its
  catalogue (the tenant's own source, per channel). The same body in the same slot with the same choices is the existing release (`200`);
  another body, or other choices, is `409`. A fix is a new version. A body another source released
  in the channel is a release of the publisher's own (`201`).
- A yanked release keeps its slot.
- The extension version is matched exactly and case-sensitively: `1.0` and `v1.0` differ, and DuckDB
  asks for the string as typed in `INSTALL x FROM r VERSION '…'`.
- Platforms follow DuckDB's names: `linux_amd64`, `linux_arm64`, `osx_arm64`, `windows_amd64`, ...

**Current.** A release published with `current=true` (the default) becomes the one DuckDB gets on
the plain path (`INSTALL x FROM r`). With `current=false` it is served only on the versioned path
(`INSTALL x FROM r VERSION 'v1.2.0'`) until someone makes it current. "Current" is decided per
caller: a caller who may not see private releases gets the newest public one.

## Visibility

A release is `public` (served to anyone) or `private` (served only to a token holding `install`; see
[Access](access.md)). Over the API a publication is **private unless you pass
`visibility=public`**, so a forgotten parameter never publishes to everyone. The CLI's
`release add` is public unless you pass `-private`.

## Reserved names

Names DuckDB owns are **reserved**. The server keeps the list as data, and it only grows:

- the core extensions of every registered DuckDB version, given when the version is registered
  ([DuckDB versions](administration/tenants-and-channels.md#duckdb-versions));
- the names a `duckdb-core` or `duckdb-community` upstream mirrors, learned as it mirrors them
  ([Upstreams](upstreams.md));
- duckdb/community-extensions' names, read every `interval` (a day by default) when
  `reserved.community_sync` is on
  ([Configuration](configuration.md#reserved));
- the names a server administrator adds (`kista admin reserved add`, below);
- the core and community names kista knew before the list was data (its seed).

A name core in any version is core; a community name may become core, never the other way. Names
an upstream of the tenant provides are reserved in that tenant too (see [Upstreams](upstreams.md)).

For `publish` and `promote`, a reserved name is reached only by a grant that **names the
extension**. A tenant-wide or channel-wide grant, or a grant on an extension group, never reaches
`httpfs`. Other verbs are not affected: a tenant administrator still manages `httpfs`'s releases.
A release under a reserved name shows `shadows: core`, `community` or `upstream` in the index.

DuckDB's alias names (`http`, `https`, `s3`, `postgres`, `sqlite`, ...) and Windows device names are
refused by the name grammar: DuckDB rewrites an alias before it builds the URL, so nothing could ever
install them.

Reservations are per [package type](concepts.md#packages-and-package-types) (spec 0034): DuckDB's
names, its aliases, and every reservation made so far are `duckdb_extension`'s, and a name reserved
for one type neither reserves nor frees the same name of another. The reserved-name commands and
routes take `-type` / `?type=` (`duckdb_extension` by default).

## Who may publish

| Action | Needs |
| --- | --- |
| Publish name `n` into channel `c` | `publish` covering (`c`, `n`) |
| Promote `n` from channel `s` to channel `t` | `promote` covering (`t`, `n`) **and** `publish` covering (`s`, `n`) |
| Read the extension's releases in `c` | `admin`, `publish` or `promote` covering (`c`, `n`) |
| Block a body, manage publishers | `admin` on the tenant |

`publish` and `promote` are **never implied by `admin`**, and an issuer-wide grant
(`issuer:<record>`) cannot carry them: publishing native code is granted explicitly. Both channels
must be `signed` channels of the tenant; nothing is published or promoted into a passthrough
channel.

A person's token used to publish, promote or block must be fresh: issued within
`auth.admin_token_max_age` (default 1h), with a `sub` or client claim naming the writer. A DuckDB
secret holding a long-lived install token is not enough.

## Publishing over the API

```text
POST /api/v1/tenants/{t}/channels/{c}/extensions/{ext}/releases
     ?version=<ext version>&platform=<platform>[&visibility=public|private][&current=true|false]
     Content-Type: application/octet-stream
     Content-Length: <required>
     body: the extension file
```

```bash
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @build/release/extension/acl/acl.duckdb_extension \
  "https://kista.example.com/api/v1/tenants/acme/channels/staging/extensions/acl/releases?version=v0.3.0&platform=linux_amd64&visibility=private"
```

The answer is `201` with the release and `Location: …/extensions/acl/releases/<id>` (`200` when the
same body was already released in that slot with the same choices):

```json
{
  "id": "4f0c…",
  "name": "acl",
  "version": "v0.3.0",
  "platform": "linux_amd64",
  "slot": "capi:1",
  "abi": "c_struct",
  "build_c_api": "v1.2.0",
  "state": "active",
  "visibility": "private",
  "seq": 7,
  "body_hash": "9a1b…",
  "created_at": "2026-10-12T09:30:00Z",
  "origin": "publication",
  "etag": "\"v1\""
}
```

| Status | Meaning |
| --- | --- |
| `400` | a footer, prefix, format or entry-point mismatch; an unknown query parameter; a DuckDB version the channel does not serve; an upload that ended before its length |
| `401` | no valid token, or a stale one; for a publisher's signed request, see [Signed requests](#signed-requests) |
| `404` | the token may not publish this name in this channel (also for a reserved name without a grant naming it) |
| `408` | the upload was slower than `serve.min_rate` |
| `409` | the slot holds another body or other choices; the body is blocked; the channel has no active key; another source holds the name in the channel (problem type `…/source`, which names it: [A name's source in a channel](upstreams.md#a-names-source-in-a-channel)) |
| `411` | no `Content-Length` (chunked uploads are refused) |
| `413` | longer than `blob.max_body` plus the 256-byte signature |
| `415` | a `Content-Type` other than `application/octet-stream` |
| `429` | busy or full, by its problem type (below) |

A client that sends `Expect: 100-continue` gets its `401` or `404` before sending the file.

**A `429` says why in its problem type** (spec 0037), so a CI script acts on the type:

| Type | Why | `Retry-After` | A script |
| --- | --- | --- | --- |
| `…/upload-slots` | too many uploads at once, of the caller, the tenant or the server | `5` | retries |
| `…/ingest-busy` | every ingest slot is taken | `5` | retries |
| `…/rate-limited` | the per-client rate limit | the limiter's | retries |
| `…/catalogue-full` | the source's catalogue holds as many entries or bytes as it may (`catalogue.max_entries`, `max_bytes`) | none | stops: it clears only when the source's builds are placed, purged or trimmed |
| `…/tenant-limit` | the tenant's [limit](administration/tenants-and-channels.md#limits) on its own bytes is reached (spec 0038; the problem's `limit` names it, without figures) | none | stops: it clears only when the tenant frees room or a server administrator raises the limit |

A publication's body is read before the tenant's bytes are checked (it carries no digest before
its body), so a full tenant costs one upload per refused publication; the same body again is no
growth and always passes. A distributor's publisher is outside the tenant's limits.

**Limits.** The upload must arrive at no less than `serve.min_rate` (default 16 KiB/s) and within
`Content-Length / min_rate` plus a minute. At most `publish.max_per_principal` uploads run at once
per caller and `publish.max_per_tenant` per tenant, and uploads leave one of `blob.max_ingests` free
for the CLI and upstream intake. The counters are per replica.

```yaml title="kista.yaml"
publish:                     # file-only
  max_per_principal: 2       # 1..64, default 2
  max_per_tenant: 8          # 1..256, default 8
```

See [Configuration](configuration.md) for every setting.

## Publishing with the CLI

On a host with the server's configuration, the server administrator adds a file directly:

```bash
kista admin -config /etc/kista/kista.yaml \
  release add acme/staging build/acl.duckdb_extension -name acl -private
```

| Flag | Meaning |
| --- | --- |
| `-name <name>` | the extension name (required; the footer has none) |
| `-private` | a private release (the CLI's default is public) |
| `-not-current` | served on the versioned path only |
| `-unchecked` | skip the prefix, format and entry-point checks, for a body the operator vouches for (a test fixture, a platform kista cannot parse). An unchecked release cannot be promoted (`409` over the API); adding or publishing the same body with the checks marks it checked |

`-` reads the file from standard input. The command prints the release id. The CLI is not limited by
grants or reserved names. See [commands](administration/commands.md) for the rest of `release`.

## Promotion

Promotion releases, in the target channel, Builds that are already in another channel of the tenant,
signed with the target channel's key.

```text
POST /api/v1/tenants/{t}/channels/{target}/extensions/{ext}/releases/promote
{"from_channel": "staging", "release": "<id>"}        one release, or
{"from_channel": "staging", "version": "v0.3.0"}      every active release of that version
optional: "visibility": "private", "current": false
```

```bash
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"from_channel":"staging","version":"v0.3.0"}' \
  https://kista.example.com/api/v1/tenants/acme/channels/prod/extensions/acl/releases/promote
```

A publisher promotes with a [signed request](#signed-requests) whose `sha256` is that of the exact
JSON bytes sent (`--data-binary`).

- Promoting a **version** takes every active release of (`ext`, `version`) in the source: every
  platform and every slot. All of them are inserted, or none (a slot conflict on any is `409` for
  all). The answer lists the releases: `{"releases": [...]}`.
- Promoting one **release** by id answers that release with its `Location`.
- The answer is `201`, or `200` when the target already holds them all with the same choices.
- Source releases must be `active` (not deprecated or yanked), checked, and not blocked.
- **Visibility** is the source's, unless you ask for `private`. Promotion never makes a private
  build public; that is `POST …/releases/{id}/public` by the extension's administrator.
- The target must serve each build's DuckDB version (`400` names the platform and version).
- A source channel you may not publish from answers `404`, like a missing release.
- Only the releases the source channel serves are promoted: its [source](upstreams.md#a-names-source-in-a-channel)
  of the name's. A promotion keeps their source: in the target, it lands only if the name has no
  source there yet (it sets it) or the same one; otherwise `409` with problem type `…/source`.

The CLI does the same:

```bash
kista admin -config /etc/kista/kista.yaml \
  release promote acme/prod -name acl -from staging -version v0.3.0
```

`-release <id>` promotes one release; `-private` and `-not-current` narrow the result.

## Publishing into the catalogue

A build can be published into the tenant's [catalogue](upstreams.md#the-catalogue-and-a-channels-rules)
only, placed in no channel, for the channels' rules or an administrator to take later:

```text
POST /api/v1/tenants/{t}/catalogue/extensions/{ext}/builds?version=&platform=[&visibility=]
```

The request is a channel's publication's, without `current`: the body is the file, a publisher
signs it as any request (the signed statement is checked, not recorded on the build: a later
placement carries the build's provenance only). It takes `publish` on the extension tenant-wide (a
grant on a channel does not reach it; a reserved name only through a grant naming it), or a
[distributor's publisher](#distributors-publishers), which needs no grant. The build's
DuckDB version must be registered on the server (a C API build's is not checked). The same body
again answers the build as it is, whatever `visibility` asks. The answer is the build (`201`, or
`200` when the source has the body already): `{"id", "platform", "slot", "abi", "duckdb_version", "c_api", "body_hash", "visibility",
"origin", "checked", "created_at", "state", "available_at", "placed", "publisher_id", "distributor"}` (`state` is `held` until `available_at` when the
publisher has a [hold](#holds-and-early-placement), else `available`; `publisher_id` and, for a
distributor's publisher, `distributor` name the publisher that published, spec 0037, and are left
out for a tenant principal's publication). The CLI's form is
`kista admin catalogue add <tenant> <file|-> -name <name> [-private]` (a tenant administrator's,
source `tenant`).

The catalogue is per [package type](concepts.md#packages-and-package-types): a publication into it
names its type in the query (`?type=`, `duckdb_extension` by default), which a publisher's signed
statement covers like the rest of the request; the catalogue's routes and `kista admin catalogue`
take `?type=` / `-type`. A build's identity and slot are within its type, and this server reads
DuckDB extensions only: another type is refused (`409`).

## Placing from the catalogue

Every build that arrives in a tenant is a build of its source in the tenant's
[catalogue](upstreams.md#the-catalogue-and-a-channels-rules). Placing releases a version of a name from a source of the
tenant in a channel: every build of the version the channel serves, all or none, signed with the
channel's key.

```text
POST /api/v1/tenants/{t}/channels/{c}/extensions/{ext}/releases/place
{"source": "upstream:core", "version": "1.2.0"}
optional: "visibility": "private", "current": false
```

```bash
kista admin -config /etc/kista/kista.yaml \
  release place acme/prod -name h3 -source upstream:community -version v4.1.2
```

- It needs `promote` on the name in the channel (a publisher may, with a signed request). A channel
  administrator places any checked build of the catalogue, and where the name has no rule yet the
  placement writes one, mode `manual`. Anyone else places only what a promotion would take: builds
  of the rule's source that are active or deprecated in a channel where it may publish the name;
  for anything else (no rule, no such source or build) it gets `404`. A placement under a name whose
  rule names another source is `409` (problem type `…/source`). A build whose body a purge took is
  never placed again.
- The version's builds that arrive later for another DuckDB version or platform the channel serves
  are placed by the placement pass: the channel holds the version, not its builds.
- **Visibility** is the build's as it arrived (a mirror's, or a publication's; private once any of
  its releases was made private), unless you ask for `private`; a private upstream's credential keeps
  it private as promotion does. A mirror's build keeps origin `upstream` and its original signature.
- The answer is `201` with `{"releases": [...]}`, or `200` when the channel already holds them all
  with the same choices; `404` when the catalogue has no build of the version the channel serves.
- A placement takes builds of the channel's [package type](concepts.md#packages-and-package-types)
  only; a body naming another `package_type` is `409`, and so is a promotion between channels of
  two types.
- A held version is placed only with `"early": true` (below).

### Holds and early placement

A publisher may have a [hold](upstreams.md#holds) (`kista admin publisher hold <tenant> <name>
<duration>`, or `PUT …/publishers/{name}/hold {"hold": "24h"}`, a tenant administrator's): every
build it publishes is held until its arrival plus the hold.

- **It publishes into the catalogue only.** The channel's publication route refuses it with `409`
  and the problem type `…/held`, whose detail names the catalogue's route; a refusal stores nothing.
  It publishes with `POST …/catalogue/extensions/{ext}/builds` (above), which needs `publish` on the
  extension tenant-wide, and channels take its builds by their rules (`auto`, or a follow) once the
  hold has passed. A publisher without a hold that holds `publish` on a channel reaches it at once,
  as before.
- **Placing early.** A held version is placed by hand only with `"early": true` (`release place
  … -early`), by a channel administrator holding `promote` on the name: the tenant's own choice to
  take a version sooner, recorded in `release.place` (`"early": true`). Anyone else is refused; a
  placement or a promotion of a held entry without it is `409` (`…/held`).
- **Revoking a key.** Removing a publisher's key with `?purge_held=true` (`publisher key remove …
  -purge-held`, a tenant administrator's; a distribution admin's for a distributor's publisher)
  purges the builds published with it that are still held and placed nowhere: a hold is what lets a stolen key be revoked before its builds reach a channel.
  Removing the publisher purges all of its held builds. A held build a channel administrator placed
  early is kept, revoked: it stays where it was placed, but no rule, follow or promotion takes it
  again, only another placement with `early`; the answer (`200`), the command and the removal's event
  list what was purged and what was kept, with how many channels hold it. A revoked build stays
  revoked for its bytes: publishing the same file again, even signed with a new key, is refused on a
  channel's route (`409`) and placed nowhere from the catalogue's; publish a new version. An upload
  in flight when its key or its publisher is removed is refused (`401`). A rotation without
  `purge_held` keeps them (`204`).

### Distributors' publishers

A distributor, such as Enterest, publishes into a tenant's catalogue through a publisher a
distribution admin or a server administrator creates as the distributor's (spec 0033):

```bash
kista admin -config /etc/kista/kista.yaml publisher add acme enterest-ci -distributor enterest
```

or `POST /api/v1/tenants/{t}/publishers {"name": "enterest-ci", "distributor": "enterest"}` (the
distributor's name, 1 to 128 bytes of text; a tenant administrator passing `distributor` is
refused, `404`). A distributor's publisher is only ever created marked: an existing publisher is
never marked one (spec 0037), and `PUT …/publishers/{name}/distributor` on a publisher without the
mark answers `404` to everyone. On a distributor's publisher it renames the distributor (`PUT
…/publishers/{name}/distributor {"distributor": "enterest-eu"}`, `publisher distributor <tenant>
<name> <distributor>`, a distribution admin's or a server administrator's), unless the publisher has
been used (builds, releases, rules, a hold or downloads: `409`, spec 0036). The mark is never
removed: the publisher is removed instead.

A distribution admin creates only distributors' publishers: `POST …/publishers` without
`distributor` answers it `404`. For a name a tenant's own publisher holds it answers `409` with the
problem type `publisher-taken`: the name is taken, and nothing else is said about that publisher
(the refusal is recorded as `authz.refused`, `reason` `publisher-taken`). A tenant holds at most 10
distributors' publishers, counted apart from its own publishers (at most 200): a distributor never
uses up the tenant's room.

Every publisher answer carries the publisher's `id` (spec 0037): a distributor recognises its own
publisher by it after a publisher of the same name was removed and created again. The ids are
opaque and grant nothing.

- **The catalogue only, without grants.** It publishes with `POST
  …/catalogue/extensions/{ext}/builds` (above) any name kista does not reserve server-wide (DuckDB's
  core and community names; `404` otherwise, as a missing grant), a name an upstream of the tenant
  lists included (each is its
  own source in the catalogue). The channel's publication route refuses it (`404`), and it holds no
  grant (`400` to a server administrator; `404` to a distribution admin, whom the grant routes do not
  admit, and to a tenant administrator, for
  whom its grants are not theirs): what it publishes reaches no channel by itself. Channels take its builds by their rules: an `auto` rule naming it, or
  a follow (`channel follow <tenant>/<channel> publisher:enterest-ci`), which takes its names the
  tenant does not use from another source, as any follow does, but for a name an upstream of the
  tenant lists: a grant reaches such a name only by naming it, and a distributor's publisher holds
  none, so a rule naming it takes it (spec 0037).
- **Held for the tenant's minimum** (spec 0037). What it publishes is held for the larger of its own
  hold and the tenant's [minimum hold for distributors'
  publishers](access.md#the-minimum-hold-for-distributors-publishers), one day unless the tenant's
  administrators change it. A distribution admin reads the minimum only as its publishers'
  `effective_hold`, and cannot lower it.
- **The distributor's, not the tenant's**: its keys (as a [marked publisher](#sources-of-licence-keys)'s),
  its [hold](#holds-and-early-placement) (`publisher hold`, a distribution admin's or a server
  administrator's: `404` to a tenant administrator; a tenant that wants more raises its minimum
  hold, above),
  revoking its key with `purge_held`, purging its builds placed nowhere (`DELETE
  …/catalogue/builds/{id}`), and [withdrawing](upstreams.md#withdrawals) a version. A tenant's
  administrators may still remove it, purge its builds placed nowhere, and choose what their
  channels take.
- **Its licences** (spec 0035): marked for licence keys, its publications record their keys, and
  its builds of a name are a catalogue entry of their own. Its licence is imported into that entry,
  named (`?source=publisher:<name>`, `-source`), as soon as a build is in the catalogue, by a server
  administrator or a licence manager (Enterest's reconciler), and a channel serves it where its rule
  names the publisher. A tenant administrator reads it and writes it no more (spec 0037:
  [who writes a distributor's entry](access.md#server-administrators-and-tenant-administrators)). Another source of the same name, a pull-through `*`
  upstream included, is another entry with its own licence or none: nothing to give or move
  ([A licence's entry](licensing.md#a-licences-entry)).
- **What a distribution admin reads**: its distributors' publishers' entries in the catalogue
  (`GET …/catalogue`, `…/catalogue/{source}/extensions/{ext}`), removed publishers' included, without
  the channels' rules or where a build is placed; the catalogue-only route's answer carries no
  `placed` for a distributor's publisher either.
- **What the publisher reads** (spec 0037): its own entries, with a signed request (the statement
  naming an empty body, as a signed `whoami` does), so a publishing worker checks its versions,
  holds and withdrawals with its key alone. `GET …/catalogue` and
  `GET …/catalogue/{source}/extensions/{ext}` answer its source only, as a distribution admin reads
  it: no rules, no placements, `publisher_id` on each entry; `?type=` is taken. `?source=` (on the
  list, where it may be left out) or `{source}` is `publisher:<its name>` or `publisher-id:<its
  id>`, both of which always mean the signer, even where a removed publisher had its name; any
  other source answers `404`. A tenant's own publisher's signed read answers `404`.

## An entry's icon and summary

A publisher sets the icon and the summary of its own catalogue entries (spec 0036; what they are and
who else sets them: [The catalogue](catalogue.md#give-an-entry-an-icon-and-a-summary)), with a signed
request, any publisher of the tenant, a distributor's or the tenant's own:

```text
PUT /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
    {"summary": "Geometry tools", "icon": {"content_type": "image/png", "data": "<standard base64>"}}
GET /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
GET /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/icon[?type=]
```

`{source}` is `publisher:<its name>` or `publisher-id:<its id>`; any other source, an entry that does
not exist (no build of the name and type from the source), or a name a distributor's publisher may
not publish answers `404`. A signed `GET` names an empty body, as `whoami` does.

- **The whole metadata.** A `PUT` replaces both fields: a field left out or `null` clears it, and
  `"summary": ""` clears the summary. The icon is new (`{"content_type": "image/png", "data": …}`),
  the stored one kept by its hash (`{"sha256": "<the icon's sha256>"}`), or `null`. To change the
  summary alone, read the metadata and send the icon back by its hash: a hash that is no longer the
  stored one is `409`, type `conflict` (someone wrote meanwhile: read again). The same metadata again
  changes nothing.
- **The answer**: `{"source", "name", "summary": string | null, "icon": {"sha256", "width",
  "height"} | null, "set_at", "signed_at", "distributor"}`; `set_by` too, to the tenant's
  administrators, server administrators and distribution admins only.
- **Replay.** A signed write whose `signed-at` is older than the entry's `signed_at`, or the same
  second with another body, is `409`, type `stale`: within the five minutes a signed request lives, a
  captured one never undoes a newer write, and the same request again changes nothing. Every
  accepted signed write moves `signed_at` to its `signed-at`; a token's write (an administrator's, a
  distribution admin's) that changes something moves it to the later of kista's clock and
  `signed_at` (it never goes back), and a token's write that changes nothing writes nothing. A client
  answered `stale` reads again, and waits until its clock passes `signed_at` when that is ahead of
  it. A request signed ahead of kista's clock (within the five minutes) and never delivered can still
  land within its window, over a token's write made before its `signed-at`.
- **Checked first.** A write is checked against the entry (stale, a kept icon changed), the key, and
  the byte cap before anything is uploaded; an icon the entry has already is not uploaded again
  while its object is there and matches (a missing one is uploaded again: the write repairs it). A
  write that fails after its icon was uploaded leaves the object to the collector, and is recorded.
- **The body** is at most 384 KiB (`413`, "the body is at most 384 KiB"); every other management
  route keeps 64 KiB. The caller is authorized before the body is read.
- **The rate**: a caller's metadata writes in a tenant are limited, on each replica, to one a
  second in bursts of ten (`429`, type `rate-limited`, with `Retry-After`), beside the API's
  per-client rate. A write spends it only once its body is the signed one and it passed its checks:
  a captured request replayed (`stale`) spends none.
- **The byte cap**: the icon counts toward the source's `catalogue.max_bytes` with its builds placed
  nowhere; a write that grows it beyond the cap is `429`, type `catalogue-full`, with no
  `Retry-After` (a smaller icon, or the same one kept, passes). An icon of the tenant's own source
  also counts toward the tenant's [bytes limit](administration/tenants-and-channels.md#limits)
  (spec 0038), for every writer: beyond it, `429` typed `tenant-limit`.
- **Busy**: a write of an icon the collector is deleting waits up to ten seconds, then answers `503`
  with `Retry-After`.
- A key revoked, or a publisher removed, while the write is in flight writes nothing (`401`).
- **Recorded**: a stale write, and one that failed after its icon was uploaded, are `catalogue.meta`
  events with the outcome `refused` or `failed` and a `reason`.

**The icon's rules** (`400`, type `invalid`, naming the rule):

- `content_type` `image/png`, at most 256 KiB, square, 64 to 512 pixels a side;
- PNG chunks: the image's own (`IHDR`, `PLTE`, `tRNS`, `IDAT`, `IEND`) and `sRGB`, `gAMA`, `cHRM`,
  `pHYs`, `bKGD` with their exact sizes and in the PNG specification's order, each once but `IDAT`,
  the `IDAT` chunks consecutive and their image data ending with the last of them, nothing after
  `IEND`;
- refused, never stripped: colour profiles (`iCCP`), text (`tEXt`, `zTXt`, `iTXt`), Exif (`eXIf`),
  animation (`acTL`, `fcTL`, `fdAT`), `sPLT`, and any other chunk. Most exporters write some of
  these; strip them first:

```bash
oxipng --strip all icon.png            # or
pngcrush -rem allb icon.png icon-stripped.png
```

kista keeps the bytes as sent, so the same icon has one hash wherever it goes, and serves the icon
with its own type and headers (`image/png`, `nosniff`, `Content-Security-Policy: default-src 'none';
sandbox`, `inline`, `same-origin`, `no-store`), after the caller is authorized and the entry found,
checked against its hash.

With the [CI script below](#publishing-from-ci):

```bash
M="/api/v1/tenants/acme/catalogue/publisher:enterest-ci/extensions/geo_tools/meta"
printf '{"summary":"Geometry tools","icon":{"content_type":"image/png","data":"%s"}}' \
  "$(base64 < icon.png | tr -d '\r\n')" > meta.json
{ read -r ST; read -r SIG; } < <(signed PUT "$M" meta.json)
curl --fail-with-body -sS -X PUT "$KISTA$M" -H "Kista-Statement: $ST" -H "Kista-Signature: $SIG" \
  -H "Content-Type: application/json" --data-binary @meta.json
```

The publication's body stays the file alone: the metadata travels in its own request, and a new
version keeps the entry's metadata. `kista_publish.py` in the developer kit gains `--summary` and
`--icon` in a later release of `hugr-lab/extension-ci-tools`.

## Changing a release

An administrator of the extension (or of its channel or tenant) changes a release with
`POST …/channels/{c}/extensions/{ext}/releases/{id}/{change}` and `If-Match` with the release's ETag
(`*` for any), or `kista admin release <change> <tenant>/<channel> <release-id>`:

| Change | Effect |
| --- | --- |
| `yank` | final: never served again; the previous release becomes current again |
| `deprecate` | still served on the versioned path, no longer current |
| `activate` | a deprecated release is active again |
| `current` | makes an active release current (a rollback, or a release published with `current=false`) |
| `public`, `private` | changes visibility |

`DELETE …/releases/{id}` (and `kista admin release purge`) deletes a yanked release for good; its
slot stays taken (see [Storage](storage.md)).

Every change bumps the channel's release version, so every replica serves the change at the next
request.

## Blocking a body

A tenant administrator can **block** a body hash: every release of that body in every channel of the
tenant, passthrough channels included, is yanked, and the body is never released again in the
tenant (publication, promotion, `release add` and upstream intake all check it).

```bash
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"body_hash":"9a1b…","reason":"CVE-2026-1234"}' \
  https://kista.example.com/api/v1/tenants/acme/blocks
```

| Route | |
| --- | --- |
| `GET /blocks`, `GET /blocks/{hash}` | list, read |
| `POST /blocks {body_hash, reason}` | `201`; blocking a blocked hash sweeps again and answers `200` |
| `DELETE /blocks/{hash}` | `204`; the body may be released again, yanked releases stay yanked |

CLI: `kista admin block add <tenant> <hash> -reason …`, `block list`, `block remove`. A hash the
tenant does not hold may be blocked ahead of time. A tenant holds at most 10,000 blocks. The index
shows `blocked: true` on a yanked row whose body is blocked. A block stops one artifact; to stop a
publisher, remove its grants or the publisher.

## Publishers

A **publisher** is a named identity in a tenant that may only publish and promote. Grants name it
as `publisher:<name>`. It authenticates by **signing its requests** with its developer's SSH key,
whose public half is registered on it, like an SSH key on GitHub: CI on GitHub, GitLab, Gitea or
anywhere else publishes the same way, and kista holds no secret that publishes. Its keys can change
without touching its grants.

What a publisher may do, whatever its grants say: publish, promote, read the releases its grants
cover, add attachments to what it publishes (see [Attachments](attachments.md)), and
`GET …/whoami`, whose `publishers` give each publisher's `id` and, for a distributor's publisher, its
`distributor` (spec 0037); a distributor's publisher also reads its own catalogue entries
([above](#distributors-publishers)). Nothing else: no install, no management. A grant to a publisher may carry only
`publish` and `promote`.

```bash
# the developer: a key for CI (the private half becomes a CI secret)
ssh-keygen -t ed25519 -N '' -C acl-ci -f publisher_key

# a tenant administrator, or the server administrator with the CLI
kista admin -config /etc/kista/kista.yaml publisher add acme ci
kista admin -config /etc/kista/kista.yaml publisher key add acme ci publisher_key.pub -expires 365d
kista admin -config /etc/kista/kista.yaml grant add acme -principal publisher:ci -verb publish -channel staging
kista admin -config /etc/kista/kista.yaml grant add acme -principal publisher:ci -verb publish -channel staging -extension httpfs
```

The last grant is needed only for a reserved name. One publisher per family of extensions keeps
grants readable; a grant may also name an extension group (see [Access](access.md)).

Over the API (tenant administrators):

| Route (under `/api/v1/tenants/{t}`) | |
| --- | --- |
| `GET /publishers`, `POST /publishers {"name": …}` | list, add; a publisher's answer has its `id` (spec 0037) and lists its keys |
| `GET /publishers/{name}`, `DELETE /publishers/{name}` | read, remove |
| `GET /publishers/{name}/keys`, `POST /publishers/{name}/keys {"public_key": …, "expires_at": …}` | list, register a key |
| `GET /publishers/{name}/keys/{id}`, `DELETE /publishers/{name}/keys/{id}[?purge_held=true]` | read, remove one; `purge_held` purges what it published that is still held, and answers what it purged and kept (revoked) |
| `PUT /publishers/{name}/hold {"hold": "24h"}` | its [hold](#holds-and-early-placement) (`0s` for none); answers the publisher, with `hold` |

Removing a publisher removes its keys and grants; caches drop it at the next request.

### Keys

- **Types**: `ssh-ed25519` (recommended) and `ecdsa-sha2-nistp256`, for a key held in a KMS. RSA,
  DSA, certificates and hardware (`sk-`) keys are refused.
- **The input** is the line of the `.pub` file `ssh-keygen` writes: `ssh-ed25519 AAAA… comment`. The
  comment becomes the key's title (printable ASCII, at most 100 characters).
- **A key is known by its fingerprint**, `SHA256:…`, as `ssh-keygen -lf publisher_key.pub` prints
  it. A fingerprint is unique in a tenant (`409`); the same key may serve publishers of other
  tenants.
- **Expiry** is optional: `-expires` takes `<days>d` or an RFC 3339 time, within a year; over the
  API, `expires_at` in RFC 3339. A year is recommended. An expired key verifies nothing.
- **A publisher holds at most 10 keys**, expired ones included.
- **Removing a key** stops it at once: keys are looked up on every request, never cached.

**Rotation**: register the new key, replace the CI secret, watch the old key's `last_used_at` stop
(`publisher key list`), then remove the old key.

### Signed requests

A publisher's request carries two headers and no `Authorization`:

```text
Kista-Statement: <the statement, standard base64>
Kista-Signature: <the SSH signature, standard base64 on one line, without its armor lines>
```

The **statement** names the request exactly: five lines, each ending in LF.

```text
kista-request v1
method POST
path /api/v1/tenants/acme/channels/staging/extensions/acl/releases?version=1.4.0&platform=linux_amd64
sha256 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
signed-at 2026-10-19T12:00:00Z
```

- `path` is the request's path and query, exactly as sent: the same parameters in the same order,
  encoded the same way (a `+` in a version is sent, and stated, as `%2B`). A reverse proxy in front
  of kista must pass the request URI unchanged.
- `sha256` is the plain SHA-256 of the request body: the file for a publication, the JSON for a
  promotion, and the SHA-256 of nothing for a `GET`.
- `signed-at` is UTC, to the second.

The **signature** is OpenSSH's `ssh-keygen -Y sign` over the statement in the namespace
`kista-request@v1`, so a signature made for git commits or anything else never verifies here.
OpenSSH 8.1 or later makes it; any CI host has it.

```bash
ssh-keygen -Y sign -f publisher_key -n kista-request@v1 < statement > statement.sig
```

**What kista checks**:

| Answer | When |
| --- | --- |
| `400` | the headers or the statement are malformed; the signature is malformed, of a key type kista does not take, or not in the namespace `kista-request@v1` or with a hash other than `sha256` and `sha512`; `method` or `path` is not this request's; a request carries both a signature and `Authorization`; the body's SHA-256 is not the statement's (checked as it is read, before anything is written) |
| `401` | `signed-at` is more than five minutes from kista's clock; the key is not one of the tenant's, or it expired; the signature does not verify |
| `404` | the publisher's grants do not cover the request, as for any caller |

A `401` names the key's fingerprint that kista found and kista's clock, so a wrong key and a skewed
clock can be told apart; it does not say which check failed. A client sending `Expect:
100-continue` gets its `401` or `404` before the file.

A captured request can be replayed only within its five minutes, to the same path, with the same
body: a publication then answers the existing release (`200`) and changes nothing; a promotion
promotes the same; an attachment is added again; an entry's metadata write older than the entry's
last one, or of its second with another body, is refused (`409`, type `stale`: [An entry's icon
and summary](#an-entrys-icon-and-summary)), and the same one again changes nothing. The statement
and the signature a release keeps are shown to the extension's administrators only, so another
publisher of the channel cannot replay them.

### Publishing from CI

Any CI publishes with a shell, OpenSSH 8.1 or later (`openssh-client` on Debian and Ubuntu,
`openssh-keygen` on Alpine), `curl`, and the private key as a secret:

```bash
# the key, from a secret; ssh-keygen wants a file of mode 0600 ending in a newline
install -m 600 "$KISTA_PUBLISHER_KEY_FILE" publisher_key   # a file variable (GitLab), or
# printf '%s' "$KISTA_PUBLISHER_KEY_B64" | base64 -d > publisher_key && chmod 600 publisher_key
# (the key in base64: GitLab's masked variables hold no newlines)

KISTA=https://kista.example.com
sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
signed() {   # signed <method> <path and query> [file]: the two headers' values, one per line
  { printf 'kista-request v1\nmethod %s\npath %s\n' "$1" "$2"
    printf 'sha256 %s\n' "$( if [ -n "${3:-}" ]; then sha256 < "$3"; else printf '' | sha256; fi )"
    printf 'signed-at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"; } > statement
  ssh-keygen -Y sign -q -f publisher_key -n kista-request@v1 < statement > statement.sig
  base64 < statement | tr -d '\r\n'; echo
  grep -v -- ----- statement.sig | tr -d '\r\n'; echo
}

# first, the key and its grants: whoami
W=/api/v1/tenants/acme/whoami
{ read -r ST; read -r SIG; } < <(signed GET "$W")
curl --fail-with-body -sS "$KISTA$W" -H "Kista-Statement: $ST" -H "Kista-Signature: $SIG"

# then each build
FILE=build/release/extension/acl/acl.duckdb_extension
P="/api/v1/tenants/acme/channels/staging/extensions/acl/releases?version=1.4.0&platform=linux_amd64"
{ read -r ST; read -r SIG; } < <(signed POST "$P" "$FILE")
curl --fail-with-body -sS -X POST "$KISTA$P" -H "Kista-Statement: $ST" -H "Kista-Signature: $SIG" \
  -H "Content-Type: application/octet-stream" --data-binary @"$FILE"
```

A retry signs again, so it never outlives `signed-at`. A client written in Go can sign with
`golang.org/x/crypto/ssh` instead, for example over a KMS's signer (`ssh.NewSignerFromSigner`).

```yaml title=".gitlab-ci.yml"
publish:
  stage: deploy
  image: alpine:3
  rules: [{if: $CI_COMMIT_TAG =~ /^v/}]
  script:
    - apk add --no-cache bash curl openssh-keygen
    - bash scripts/publish.sh   # the commands above; KISTA_PUBLISHER_KEY_FILE is a protected File variable
```

**The developer kit.** The [extension template](development.md#starting-an-extension-the-template)
publishes from GitHub Actions with the fork's `kista_publish.py`, which does the above for every
build of a distribution: it reads each build's version and platform from its footer, skips wasm
builds (refused, above), signs each request, and retries a `429`, a `408`, a `5xx` or a broken
connection, signing again (it should stop on a `429` typed `catalogue-full` or `tenant-limit`, [above](#publishing-over-the-api)). Its `--check` signs a `whoami` first. It needs only Python 3 and OpenSSH,
so it also runs in another CI: see the fork's
[KISTA.md](https://github.com/hugr-lab/extension-ci-tools/blob/main/KISTA.md).

The release keeps the statement and the signature as they came (`publisher_statement`,
`publisher_signature` in its management answer, for the extension's administrators), and its
provenance records the credential, `key:<fingerprint>`. The signature carries the public key, so who
asked for this body at this path can be checked after the key is removed.

GitHub Actions trusted publishing and API keys (before spec 0023) are removed: a configuration with
`publish.providers` is refused at startup. An issuer record cannot have a CI token issuer's URL
(GitHub Actions', GitHub Enterprise's), since any workflow there can request a token for any
audience; records that had one stay unused.

### Sources of licence keys

A licensed extension's build declares its licence keys in a key block (see
[Licensing](licensing.md#the-keys-a-licence-verifies-against)). kista records them only when a
trusted source brings the build. A publisher is one when a **server administrator marks** it, for
example the build pipeline of the issuer that signs the extension's licences:

```bash
kista admin -config /etc/kista/kista.yaml publisher add acme tresor-ci
kista admin -config /etc/kista/kista.yaml publisher license-keys acme tresor-ci on   # off unmarks it
kista admin -config /etc/kista/kista.yaml grant add acme -principal publisher:tresor-ci -verb publish -channel prod -extension tresor
kista admin -config /etc/kista/kista.yaml publisher key add acme tresor-ci tresor-ci.pub -expires 365d
```

Over the API: `POST` (mark) and `DELETE` (unmark) `/api/v1/tenants/{t}/publishers/{name}/license-keys`,
server administrators only (and distribution admins, for a distributor's publisher). Each records a
`publisher.change` event.

- **Its publications record keys.** Each publication by the marked publisher records the keys the
  body declares, including a publication of a body that is already released (from an unmarked path,
  say). The publisher must hold `publish` on the name itself.
- **A malformed key block is refused**: `400`, with `license_keys` in the detail. `kista ext
  inspect <file>` shows what kista reads.
- **Its keys and grants are the server administrator's.** A distribution admin changes a
  distributor's publisher's keys and licence-key mark only, never its grants (it holds none) nor the
  tenant's own publishers', marked or not, which answer it `404` (spec 0037). A publisher holding a key is not marked
  (`400`): keys a tenant administrator registered would outlive the mark. While it is marked,
  adding or removing its keys and grants, and changing the members of an extension group its grants
  name, is refused to a tenant administrator (`404`). A tenant administrator can still remove the
  publisher, which drops the licence keys it brought.
- **Unmarking** keeps the keys already recorded. Its credentials are then a tenant administrator's
  again, so marking it again needs it to hold none.

Publications by any other publisher, by a principal, and `release add` record no keys.

## Provenance in the index

Every release has an `origin`: `admin` (the CLI), `publication`, `promotion`, `placement` (from the
catalogue, spec 0033) or `upstream` (a mirror's, also when placed from the catalogue). Its
`provenance` (the actor, a publisher's key, the source channel and release for a promotion) is shown to the extension's administrators and to holders of `publish` or `promote`
covering it. Who made the release is shown to administrators only, not to publishers.

## Signatures and key rotation

A publication or promotion is signed with the channel's active key (and its serving key, when they
differ) outside any transaction, and each signature is verified before it is stored. When a channel's
active key changes, the re-signer (`serve.resign: true` on a replica, or
`kista admin key resign <tenant>/<channel>`) signs every non-yanked release with the new key, and
then moves the serving key. Nothing has to be published again. See
[Key rotation](administration/key-rotation.md).

Publications, promotions and blocks are recorded as events (see [Observability](observability.md)).
