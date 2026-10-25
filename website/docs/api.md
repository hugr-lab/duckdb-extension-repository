---
title: HTTP API
---

# HTTP API

kista's HTTP API is under `/api/v1/`. It has two parts:

- **the index**: which extensions and versions a channel serves to the caller, computed by the same
  resolution as the DuckDB routes. The hugr node agent, the console and people use it;
- **management**: what `kista admin` does, over HTTP, with Bearer tokens.

The [console](console.md) uses only this API. Each feature page describes its routes in detail; this
page gives the common rules and the route map. The design is in
[spec 0007](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0007-api/spec.md).

## Base path and transport

- Every route is under `/api/v1/` on an **https** listener. On an http listener `/api/` answers
  `404`: tokens are never used over plain http.
- `GET /api/v1/info` (public) answers `{"kista": "<version>", "api": "1"}`.
- Paths are matched exactly as sent: a path with `%` escapes, a backslash, an empty, `.` or `..`
  segment answers `404`.
- Methods are `GET`, `HEAD` (wherever `GET` is), `POST` and `DELETE`. Another method on a known route
  answers `405` with `Allow`.

## Authentication

Tokens are sent only as `Authorization: Bearer <token>`. There are no cookies and no query-string
tokens, so there is no CSRF surface.

| Token | Used on |
| --- | --- |
| A **tenant token**: issued by one of the tenant's issuer records, for one of its audiences | that tenant's routes, `/api/v1/tenants/{t}/…` |
| A **server token**: issued by a server issuer (`auth.server_issuers`), for a server audience | routes without a tenant; on tenant routes a server administrator acts with every right, a licence manager on the licence routes only, a distribution admin on a distributor's calls only ([Access](access.md)) |
| A publisher's **signed request**: `Kista-Statement` and `Kista-Signature`, signed with one of its keys, no `Authorization` | its tenant's `whoami`, and the publication, promotion, release-read and attachment routes only |

Server and tenant audiences are disjoint, so a token is one or the other. See [Access](access.md)
for issuers, principals and grants, and [Publishing](publishing.md#signed-requests) for signed requests.

- A request without a token on a route that is not public answers `401`.
- A token that does not verify answers `401` on the API (the DuckDB routes fall back to anonymous
  instead), so a client learns its token is bad.
- A valid token that may not see or do something answers `404`, with the same bytes as a resource
  that does not exist.
- Authorisation is decided from the path first, before the body is read or `If-Match` compared.
- An unknown or suspended tenant answers `404` to everyone but server administrators.
- A tenant whose deletion started (`deleting` or `deleted`) answers the unknown tenant's `404`, the
  same bytes and headers, to every caller. Server administrators alone still reach its record
  (`GET tenants/{t}`), `DELETE tenants/{t}` and its events; see [Deleting a tenant](#deleting-a-tenant).
- `GET /api/v1/tenants/{t}/whoami` shows a tenant token's principals, the grants that match them,
  `holds_admin` and `holds_audit`. `GET /api/v1/whoami` shows a server token's issuer, principals,
  `administrator`, `license_manager` and `distribution_admin`. A server token on the tenant `whoami` answers `404`; a
  tenant token on the server `whoami` answers `400`.

### Fresh tokens for management

Management routes, reads included, need a token issued (`iat`) within `auth.admin_token_max_age`
(default 1h). A node's long-lived install token is not enough. A write also needs a token that names
its writer (a `sub` or a client claim). Either failure answers `401`.

## Requests and responses

- **JSON bodies** are `application/json` (optionally `charset=utf-8`; otherwise `415`), one object, at
  most 64 KiB (`413`). Unknown and duplicate keys are refused (`400`). A route that takes no body
  refuses one.
- **Raw bodies**: the publication route takes the extension file (`application/octet-stream`); the
  attachment route takes the document with its media type; a licence import takes the licence (at
  most 16 KiB). All need `Content-Length`.
- Every answer carries `X-Content-Type-Options: nosniff` and `Vary: Authorization`.
- **Index answers** have an ETag over their bytes and answer `304` to a matching `If-None-Match`.
  `Cache-Control` is `public, no-cache` for an anonymous answer and `private, no-cache` otherwise.
- **Management answers** carry `Cache-Control: no-store`.
- **Creating** is a `POST` to a collection: `201` with `Location` (when the resource has a `GET`),
  `409` for a duplicate. **Deleting** answers `204`; deleting a tenant answers `202`, since its
  rows are removed in the background.
- **Package types** (spec 0034; see [Concepts](concepts.md#packages-and-package-types)): a body
  names one as `package_type` (a channel, a grant, a group's member change, a placement), and the
  routes keyed or filtered by a name take `?type=`: the licences and licence keys, the server's and
  a tenant's reserved names, attachments, the catalogue (and `catalogue?name=`),
  `grants?extension=`, `extension-groups?extension=` and `stats/*?extension=`. Both default to
  `duckdb_extension`, the only type there is today, and answers leave `package_type` out when it is
  the default, so an existing client reads what it read. A type the server does not know is `400`
  on every route, and so is `?type=` on a route that takes none (a body names the type there); on
  a channel's routes and an upstream's the type is the channel's, and a `?type=` that differs is
  `400` (a channel of a type the server does not know is `404`, as a missing one). The routes
  DuckDB installs from take the type from the channel and ignore `?type=`.

### ETags and If-Match

Resources that writes change carry a version: their `GET` returns `ETag: "v<version>"` and list rows
carry it as `etag`. Tenants, keys, attestation keys, releases, attachments, feeds and other
versioned resources work this way.

- A change to a versioned resource needs `If-Match` with its ETag: `428` without one, `412` when it is
  stale or another resource's. `If-Match: *` matches any version of an existing resource, except
  where a route refuses it (deleting a tenant, a console client).
- `If-Match` takes one strong ETag.
- Issuer records are immutable: their ETag is their id. Deleting a record, and setting or removing
  its console client, takes `If-Match: "<issuer id>"` (`*` is refused for the console client).
- What is only added and removed, never changed, takes no `If-Match`: grants, audiences, DuckDB
  versions, channels and their DuckDB versions, publishers and their credentials, blocks, shadows,
  an upstream's extensions, platforms and keys, and a group's members. Removing an attachment takes
  none either.

A `412` means "it changed: read it again".

### Errors

Errors are `application/problem+json`:

```json
{"type": "urn:kista:problem:invalid", "title": "Invalid", "status": 400, "detail": "limit is 1..500"}
```

| `type` suffix | Status |
| --- | --- |
| `unauthorized` | `401`, with `WWW-Authenticate: Bearer realm="kista"` |
| `not-found` | `404` (also "not yours") |
| `method-not-allowed` | `405` |
| `invalid` | `400`; `411` when `Content-Length` is missing; `408` for an upload slower than the server accepts |
| `conflict` | `409`: a duplicate, a refused state change, the last-admin rule |
| `source` | `409`: another source holds the extension's name in the channel; the detail names it ([spec 0031](upstreams.md#a-names-source-in-a-channel)) |
| `held` | `409`: a held entry placed or promoted without `early`, or a publisher with a hold on a channel's publication route ([holds](upstreams.md#holds)) |
| `withdrawn` | `409`: a [withdrawn](upstreams.md#withdrawals) build placed, its release made active, current or public, its body published again, or the build purged |
| `precondition-failed` | `412` |
| `precondition-required` | `428` |
| `too-large` | `413` |
| `unsupported-media-type` | `415` |
| `too-many-requests` | `429`, with `Retry-After` |
| `unavailable` | `503`; with `Retry-After: 1` when the store or the storage collector is busy |

`401` and `404` have constant bytes and no `detail`, except a publisher's signed request's `401`,
which names the key's fingerprint kista found and kista's clock ([spec 0023](publishing.md#signed-requests)). Other answers to an allowed caller carry a
`detail`: a fixed message, or for an invalid value the service's own wording of the value and the
rule (at most 300 bytes). It is never an internal error.

### Pagination

Lists that can be long are paged with an opaque cursor:

- `limit` is 1 to 500 (default 100);
- the answer carries `next` when there is more; pass it back as `cursor`;
- a cursor is only valid for the list that returned it (`400` otherwise).

The index's `extensions` list is ordered by name and its cursor is the last name, so it never
expires. Some lists take a name `prefix` (`…/channels/{c}/extensions`, `…/channels/{c}/releases`,
`…/grants/principals`, `…/extension-groups`, `…/extension-groups/{g}/members`,
`…/upstreams/{name}/extensions`).

### Rate limits

A token bucket per client address (IPv6 by /64, after `serve.trusted_proxies`) is checked before
anything else: `serve.api_rate` requests a second, burst `serve.api_burst` (default 20 and 40; set
both or neither). Over the limit the answer is `429` with `Retry-After`. Adding an issuer (which
fetches its discovery document) runs one at a time per tenant: a second one meanwhile answers `429`.
Uploads (publications and attachments) take a slot per caller, per tenant and per server; without
one the answer is `429`. See
[Configuration](configuration.md).

### CORS

CORS is off unless `ui.allowed_origins` lists exact origins (for the console mounted in a shell on
another origin). Then `/api/v1/*` allows those origins, no credentials, the methods
`GET, HEAD, POST, PUT, DELETE`, the headers
`Authorization, Content-Type, Content-Digest, If-Match, If-None-Match, traceparent`, and exposes
`ETag, Location, Retry-After`.
See [Console](console.md).

## Who may call

The tables below use these names:

| Name | Meaning |
| --- | --- |
| public | anyone; a tenant token, if sent, selects the caller's view |
| server | a server administrator |
| tenant admin | `admin` on the tenant (or a server administrator) |
| channel admin, extension admin | `admin` on that channel or extension (a tenant admin is both) |
| auditor | `audit` on the tenant (`admin` on the tenant implies it) |
| principal | any principal of a tenant token; the route decides what it shows |

## Routes

All paths are relative to `/api/v1`. `{t}` is a tenant, `{c}` a channel.

### Server and tenants

| Route | Who |
| --- | --- |
| `GET info` | public |
| `GET whoami` | a server token |
| `GET console`, `GET tenants/{t}/console` | public: the console's sign-in configuration |
| `GET duckdb-versions`, `GET duckdb-versions/{v}` | public |
| `POST duckdb-versions`, `POST duckdb-versions/{v}/c-apis`, `POST duckdb-versions/{v}/core-extensions` | server: [DuckDB versions](#duckdb-versions-and-reserved-names) |
| `GET reserved-names[?name=]` | server, a distribution admin: [reserved names](#duckdb-versions-and-reserved-names) |
| `POST reserved-names`, `DELETE reserved-names/{name}` | server |
| `GET tenants[?state=]`, `POST tenants`; `POST tenants/{t}/suspend`, `/resume` | server |
| `GET tenants/{t}` | tenant admin; a deleting or deleted tenant: server |
| `DELETE tenants/{t}` | server: [deleting a tenant](#deleting-a-tenant) |
| `PUT tenants/{t}/holder {"holder_id"}`, `DELETE tenants/{t}/holder` (`If-Match`) | server: [the expected holder](#the-expected-holder) |
| `GET tenants/{t}/audiences` | tenant admin |
| `POST tenants/{t}/audiences`, `POST tenants/{t}/audiences/remove` | server |

#### The tenant record

```json
{"name": "acme", "display_name": "Acme Corp", "state": "suspended", "storage_domain": "default",
 "created_at": "2026-10-01T09:00:00Z", "suspended_at": "2026-10-02T09:00:00Z",
 "deletable_at": "2026-10-09T09:00:00Z", "holder_id": "org-7f3a", "etag": "\"v7\""}
```

- `state` is `active`, `suspended`, `deleting` or `deleted`.
- `suspended_at` is set while the tenant is suspended (it stays set once its deletion started).
- Server administrators only: `storage_domain`; `deletable_at`, when a suspended tenant may be
  deleted (`suspended_at` plus `tenants.min_suspended`; absent for a tenant suspended by an older
  kista); and, on `GET tenants/{t}` and `DELETE tenants/{t}` of a deleting or deleted tenant,
  `deletion`: `started_at`, `started_by`, `passes`, `table` (the table the reaper reached),
  `deleted_rows`, `last_error` (a class: `lease`, `interrupted` or `error`), `next_attempt_at`
  (until it finishes), `finished_at`, and `keys`, the keys to retire (`kind` `channel` or
  `attestation`, `id`, `fingerprint`, `signer_ref`).
- `holder_id` is the holder the tenant's licences must name, when one is set (its administrators
  see it too).
- `GET tenants?state=deleting,deleted` lists the tenants in those states (`400` for another value).
  Without `state`, every state but `deleted` is listed.
- `POST tenants` takes `name`, and optionally `display_name`, `storage_domain` and `holder_id`.

#### The expected holder

A server administrator may set the holder a tenant's licences must name: their `sub`, 1 to 128
bytes. kista then refuses, on every path that stores a licence, one of another holder (see
[Licensing](licensing.md#an-expected-holder)). A tenant administrator reads it on the tenant record,
and cannot change or clear it.

- `PUT tenants/{t}/holder` with `{"holder_id": "…"}` sets it, `DELETE tenants/{t}/holder` clears it.
  Both answer the tenant record, and need `If-Match` with the tenant's ETag (or `*`): `428` without
  one, `412` when it is stale. Each change is a `tenant.holder` event with the `old` and the `new`
  holder.
- Setting it while the tenant holds a licence of another holder answers `409`, naming those
  licences (the first 20, and how many more); remove them first. Nothing is removed for you.
- Setting the holder the tenant already has, or clearing none, changes nothing: `200` with the
  same ETag and no event. A provisioning service may `PUT` it on every run.
- A deleting or deleted tenant answers `404`.

#### Deleting a tenant

`DELETE tenants/{t}` deletes a tenant that has been suspended for at least
`tenants.min_suspended`. It cannot be undone; see
[Deleting a tenant](administration/tenants-and-channels.md#deleting-a-tenant) for what goes and what
is kept.

- It needs `If-Match` with the tenant's ETag: `428` without one or with `*`, `412` when it is stale.
- It answers `202` with the tenant record, `deletion` included: the reaper removes the rows later.
- It answers `409 conflict`, with the reason in `detail`, when the tenant is active ("only a
  suspended tenant can be deleted"), when it has been suspended for less than the minimum (the
  `detail` says from when it may be deleted), when an older kista suspended it ("resume it and
  suspend it again"), and when its deletion already started ("the tenant is being deleted", "the
  tenant was deleted").
- From then on the tenant answers as unknown on every route except its `GET`, this `DELETE` and its
  events, for server administrators. Suspend and resume answer `404`.

```bash
ETAG=$(curl -sS -D - -o /dev/null -H "Authorization: Bearer $SERVER_TOKEN" \
  https://extensions.example.com/api/v1/tenants/acme | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
curl -sS -X DELETE -H "Authorization: Bearer $SERVER_TOKEN" -H "If-Match: $ETAG" \
  https://extensions.example.com/api/v1/tenants/acme
```

#### DuckDB versions and reserved names

A version is registered with its core extensions, which become reserved core names ([Reserved
names](publishing.md#reserved-names); `kista duckdb core-extensions <tag>` prints them).

- `POST duckdb-versions` takes `name`, `kind` (`release` or `dev`), `c_api_maxima`, and
  `core_extensions` (required: a list of names) with `core_extensions_commit` (optional: the commit
  of DuckDB's sources they were read at) and `force`. It answers `201` with the version, and
  `core_extensions_added`, the names that were not reserved before. Without `core_extensions`:
  `400`. A name tenants have releases under answers `409`, naming them, unless `force` is true (as
  for `POST reserved-names`).
- `POST duckdb-versions/{v}/core-extensions` with `core_extensions` (and `core_extensions_commit`,
  `force`) reserves more of a registered version's core extensions: `200` with
  `core_extensions_added`, `404` for an unknown version.
- `GET reserved-names` lists `names`, each with `name`, `kind` (`core` or `community`), `added_at`
  and `sources` (`builtin`, `version:<v>@<commit>`, `upstream:<tenant>/<upstream>`, `bundle:<id>`,
  `admin:<actor>`, `community-sync@<commit>`); `?name=` reads one. Names are reserved per package
  type: these routes take `?type=` (`duckdb_extension`, DuckDB's names, by default).
- `POST reserved-names` with `names`, `kind` and `force` reserves names as an administrator's. A
  name that becomes reserved (or goes from community to core) with releases that are not yanked or
  purged in some tenant answers `409`, naming the tenants, unless
  `force` is true: the releases stay, and only grants naming the extension reach the name from then
  on. It answers `200` with `added` (the names that were not reserved as that kind before) and
  `tenants` (the tenants that keep releases, by name). Each name added is a `reserved.add` event.
- `DELETE reserved-names/{name}` removes a name only administrators reserved (`204`, a
  `reserved.remove` event); a name a version, an upstream or the seed brought answers `409`.

### The index

| Route | Who |
| --- | --- |
| `GET tenants/{t}/whoami` | public (a tenant token or a publisher credential) |
| `GET tenants/{t}/channels` | public |
| `GET tenants/{t}/channels/{c}` | public: kind, DuckDB versions and C API maxima, signing keys |
| `GET tenants/{t}/channels/{c}/extensions[?duckdb_version=&platform=&prefix=&cursor=&limit=]` | public, in the caller's view |
| `GET tenants/{t}/channels/{c}/extensions/{name}[?duckdb_version=&platform=]` | public, in the caller's view |
| `GET tenants/{t}/channels/{c}/extensions/{name}/versions/{v}?duckdb_version=&platform=` | public: `available`, `deprecated`, `yanked` or `missing` |
| `GET tenants/{t}/channels/{c}/extensions/{name}/attachments[/{id}]` | public, in the caller's view |
| `GET tenants/{t}/attestation-keys` | public |

The caller's view is the public releases plus those of the extensions it holds `install` on in that
channel. See [Using kista from DuckDB](duckdb.md).

### Channels and keys

| Route | Who |
| --- | --- |
| `POST tenants/{t}/channels` | tenant admin |
| `GET, POST tenants/{t}/channels/{c}/duckdb-versions`; `DELETE …/duckdb-versions/{v}` | channel admin |
| `GET tenants/{t}/channels/{c}/sources`; `GET, PUT, DELETE …/sources/{name}` | channel admin: [a name's source in the channel](upstreams.md#a-names-source-in-a-channel) and its [rule](upstreams.md#the-catalogue-and-a-channels-rules) (`PUT` takes `{"source": "tenant" \| "upstream:<name>" \| "publisher:<name>", "mode": "auto" \| "manual"}`, `mode` optional; a follow's rules have mode `follow`; each answer is `{"name", "source", "mode", "current", "newest" (the newest available in the catalogue; a version with a held build is not), "outcome", "outcome_at", "set_by", "set_at", "available": {"<source>": <releases>}}`, `source` `""` when none; a list is `{"sources": [...]}`). A release's management answer carries `source` and `selected` (whether it is the channel's source of the name: only those are served) |
| `GET tenants/{t}/channels/{c}/follows`; `GET, PUT, DELETE …/follows/{source}` | channel admin: the channel's [follows](upstreams.md#the-catalogue-and-a-channels-rules) (`PUT` takes no body; each answer is `{"source", "set_by", "set_at", "skips": {"<name>": "<reason>"}}`; a list is `{"follows": [...]}`) |
| `GET tenants/{t}/catalogue[?source=&name=]` | tenant admin: the tenant's [catalogue](upstreams.md#the-catalogue-and-a-channels-rules), `{"entries": [{"source", "name", "versions": [newest first], "rules": {"<channel>": "<mode>"}, "held": [the versions with a held build, omitted for none], "revoked": [...], "withdrawn": [...]}]}`; a distribution admin: its distributors' publishers' entries only (removed ones too), `rules` empty, each with its `publisher_id` (a name two of them had is `publisher-id:<id>` as a source) |
| `GET tenants/{t}/catalogue/{source}/extensions/{ext}` | tenant admin: an entry's versions (newest first), each with its builds: `{"versions": [{"version", "builds": [{"id", "platform", "slot", "abi", "duckdb_version", "c_api", "body_hash", "visibility", "origin", "checked", "created_at", "state": "held" \| "available" \| "withdrawn", "available_at" (held), "revoked_at", "withdrawn_at", "withdrawn_reason", "placed": [{"channel", "state"}]}]}]}`; a distribution admin: a distributor's publisher's entry (`publisher:<name>` or `publisher-id:<id>`, removed or not), without `placed` |
| `GET, PUT tenants/{t}/channels/{c}/hold` | channel admin: the channel's [hold](upstreams.md#holds), `{"channel", "hold": "2h0m0s"}`; `PUT` takes `{"hold": "2h"}` (`0s` for none, at most 90 days) |
| `POST tenants/{t}/catalogue/extensions/{ext}/builds?version=&platform=[&visibility=]` | `publish` on the extension tenant-wide, or a [distributor's publisher](publishing.md#distributors-publishers) without grants: [publishing into the catalogue](publishing.md#publishing-into-the-catalogue) only |
| `DELETE tenants/{t}/catalogue/builds/{id}` | tenant admin; a distribution admin for a distributor's publisher's build: purge a build no release holds (`409` when one does, or it was withdrawn) |
| `POST tenants/{t}/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw {"reason": "…"}` | server, a distribution admin: [withdraw](upstreams.md#withdrawals) a version of a distributor's publisher (`publisher:<name>`, or `publisher-id:<id>` when the name is ambiguous, `409` otherwise; removed or not), whether it holds builds of it or not; answers `{"tenant", "source", "name", "version", "reason", "withdrawn": [{"build", "platform", "slot"}], "already"}`, never the channels |
| `GET tenants/{t}/channels/{c}/keys` | channel admin: keys, serving and active key, re-sign progress; a server administrator also sees each key's `signer_ref` and `protection` tier |
| `POST tenants/{t}/channels/{c}/keys` | server: a key by signer reference |
| `GET …/keys/events`, `GET …/keys/{id}` | channel admin |
| `POST …/keys/{id}/activate`, `/retire` (`?force=true` for server) | channel admin |

See [Tenants and channels](administration/tenants-and-channels.md) and
[Key rotation](administration/key-rotation.md).

### Releases, publication and blocks

| Route | Who |
| --- | --- |
| `GET tenants/{t}/channels/{c}/releases[?state=&prefix=&cursor=&limit=]` | channel admin |
| `GET tenants/{t}/channels/{c}/extensions/{ext}/releases[/{id}]` | extension admin, its publishers |
| `POST …/extensions/{ext}/releases?version=&platform=[&visibility=&current=]` | `publish` on the extension (raw file body) |
| `POST …/extensions/{ext}/releases/promote` | `promote` on the extension |
| `POST …/extensions/{ext}/releases/place` | `promote` on the extension; a name without a rule in the channel, or `"early": true` (a held version), channel admin ([placing from the catalogue](publishing.md#placing-from-the-catalogue)) |
| `POST …/extensions/{ext}/releases/{id}/yank`, `/deprecate`, `/activate`, `/current`, `/public`, `/private` | extension admin |
| `DELETE …/extensions/{ext}/releases/{id}` | extension admin: purge a yanked release |
| `GET, POST tenants/{t}/blocks`; `GET, DELETE tenants/{t}/blocks/{hash}` | tenant admin |
| `GET, POST tenants/{t}/publishers`; `GET, DELETE …/publishers/{name}` | tenant admin; a distribution admin all but `DELETE`; `POST {"name", "distributor"}` creates a [distributor's publisher](publishing.md#distributors-publishers): server, a distribution admin (`404` to a tenant admin) |
| `GET, POST …/publishers/{name}/github`; `GET, DELETE …/github/{id}` | tenant admin |
| `GET, POST …/publishers/{name}/keys`; `GET, DELETE …/keys/{id}` | tenant admin; a distribution admin for a marked publisher; `DELETE …?purge_held=true` (a tenant admin; a distribution admin for a distributor's publisher) purges the builds the key published that are still held, and answers what it purged and kept, revoked ([holds](publishing.md#holds-and-early-placement)) |
| `PUT …/publishers/{name}/hold {"hold": "24h"}` | tenant admin: the publisher's [hold](publishing.md#holds-and-early-placement); a publisher answers `hold` (omitted for none); a distributor's publisher's: server, a distribution admin only |
| `PUT …/publishers/{name}/distributor {"distributor": "enterest"}` | server, a distribution admin: rename a [distributor's publisher](publishing.md#distributors-publishers)'s distributor, or mark a fresh publisher a distribution admin created (`404` for a tenant administrator's or an operator's publisher, `400` for one that is not fresh); a publisher answers `distributor` (omitted for the tenant's own) |
| `POST, DELETE …/publishers/{name}/license-keys` | server, a distribution admin: mark a publisher a source of licence keys, or unmark it |

A publisher answers with its `keys` and `license_keys` (true when marked; to a distribution admin,
an unmarked publisher's `keys` are empty). While it is marked, adding or removing its keys and
grants is a server administrator's or a distribution admin's (`404` to a tenant administrator). A
distribution admin marks a publisher only before it holds keys or grants (`400`). A distributor's
publisher is marked so too, and holds no grants (`400` to a server administrator or a distribution
admin, `404` to a tenant administrator). A marked publisher's publication of a body whose key block is malformed answers
`400` (`license_keys`).

See [Publishing](publishing.md).

### Upstreams

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/upstreams`; `GET, DELETE …/upstreams/{name}` | tenant admin |
| `POST …/upstreams/{name}/public`, `/private`, `/pause`, `/resume`, `/sync`, `/credential` | tenant admin |
| `POST …/upstreams/{name}/fetch {"fetch": "eager"\|"lazy"}` (`If-Match`) | tenant admin: a mirror's fetch mode ([Lazy mirrors](upstreams.md#lazy-mirrors)); an upstream answers `fetch`, an entry `eager` |
| `PUT …/upstreams/{name}/hold {"hold": "24h"}` (`If-Match` optional) | tenant admin: an eager mirror's [hold](upstreams.md#holds); an upstream answers `hold` (omitted for none) |
| `GET …/upstreams/{name}/cells[?outcome=]` | tenant admin; `outcome=not_asked`: a lazy mirror's cells offered, not fetched |
| `GET, POST …/upstreams/{name}/extensions`; `DELETE …/extensions/{entry}` | tenant admin |
| `GET, POST …/upstreams/{name}/platforms`; `DELETE …/platforms/{item}` | tenant admin |
| `GET, POST …/upstreams/{name}/keys`; `DELETE …/keys/{item}` | tenant admin |
| `GET tenants/{t}/shadows`; `DELETE tenants/{t}/shadows/{name}` | tenant admin |
| `GET credentials` | server: the configured upstream credentials (names, tenants, prefixes, and the upstreams that forward usage with each) |

See [Upstreams](upstreams.md).

### Licences

| Route | Who |
| --- | --- |
| `GET tenants/{t}/licenses[?source=]`; `GET, DELETE …/licenses/{name}[?source=]` | a tenant admin or a licence manager |
| `POST tenants/{t}/licenses` (the licence as the body; `If-Match` to replace one) | a tenant admin or a licence manager |
| `POST …/licenses/{name}/private` (`If-Match`) | a tenant admin or a licence manager |
| `POST …/licenses/{name}/public` (`If-Match`) | tenant admin |
| `GET …/upstreams/{name}/licenses`; `POST …/upstreams/{name}/licenses/sync` | tenant admin |
| `GET …/upstreams/{name}/usage`; `POST …/upstreams/{name}/usage {"usage": "forward"\|"off"}` (`If-Match`) | tenant admin: the usage switch and the last forwarding run |
| `GET tenants/{t}/license-keys`, `GET …/license-keys/{name}` | a tenant admin or a licence manager: the tenant's live licence keys, with their owners |
| `PUT …/license-keys/{name}/{kid}/owner {"source": "upstream:<name>"\|"publisher:<name>"}` | tenant admin: give a key to a source that declares it (spec 0033) |

A licence is answered as its claims and state (`name`, `owner`, `id`, `issuer`, `holder_id`,
`holder`, `plan`, `versions`, `limits`, `issued_at`, `expires_at`, `grace_until`, `key`, `source`,
`upstream`, `visibility`, `status`, `etag`), never with the token. A tenant holds a licence per
extension and source (spec 0033 phase B): `owner` is the licence's source, the source that owns its
key for the extension (`upstream:<name>`, `publisher:<name>`, a removed one's `upstream-id:<id>` or
`publisher-id:<id>`, or `unknown`), and `source` how it arrived (`import` or `upstream`). The routes
of one licence (`GET`, `DELETE`, `…/public`, `…/private`) take `?source=` (the owner as answered, or
its id form): without it they act on the extension's only licence, and answer `409` naming the
sources where it has several. A removed source is `upstream-id:<id>` or `publisher-id:<id>` in the
licence routes, even where the catalogue routes still show it by name (a removed distributor's
publisher). An import stores the licence for its key's owner; a new one answers
`201` with `Location` (`…/licenses/<name>?source=<owner>`); over a stored one of that source (or,
where it holds none, one of source `unknown` signed with the same key) without `If-Match`, `409`. `GET …/upstreams/{name}/licenses`
answers the last relay run, `{"last_run_at", "result": {"counts", "refused", "error"}}`, and the sync
request (`202`) answers the same; `409` for an upstream that relays nothing or is paused, and while
licensing is off (an import too). An import or a relayed licence verifies against a live key of its
key's owner (spec 0021, 0033: a live build declaring it, or the owner's newest available version in
the catalogue); without one it is refused (`400`, `kid_untrusted`), and a relayed licence whose key
another source owns is refused with `key_owner`. In a tenant with
an [expected holder](#the-expected-holder), an import of another holder's licence answers `400`
with `holder_mismatch` in its detail, naming the expected and the given holder.
`GET …/license-keys` answers `{"names": [{"name", "keys": [{"kid", "sources": [{"kind":
"publisher"|"upstream", "name"}], "builds": n, "owner"}]}]}` (`owner` omitted while the key has
none: no source's available build declares it, another source's record is held still, or its owner
was removed and no source recorded it since). `PUT …/owner` answers `{"name", "kid", "owner",
"before", "moved": [licence ids]}`: the source must have recorded the key for the extension (`400`
otherwise), and the licences signed with it move from the owner before (or, where it has none, one
of source `unknown`), `409` while the source holds a licence of that extension already. A licence is a package's (spec 0034): these
routes take `?type=` (`duckdb_extension` by default), an import stores the licence under that type,
and a channel serves only its own type's licences. See [Licensing](licensing.md).

### Identity, grants and groups

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/issuers`; `GET, DELETE …/issuers/{name}` | tenant admin |
| `POST, DELETE …/issuers/{name}/console` | tenant admin: the console's client at that issuer |
| `GET tenants/{t}/grants[?principal=&extension_group=&extension=]`, `POST tenants/{t}/grants` | tenant admin; a distribution admin (`POST`: `publish`, `promote`, `admin` on an extension) |
| `GET tenants/{t}/grants/principals[?prefix=]` | tenant admin |
| `GET, DELETE tenants/{t}/grants/{id}` | tenant admin; a distribution admin (`DELETE`: the grants it may add) |
| `GET tenants/{t}/reserved-names`, `GET …/reserved-names/{name}` | tenant admin, a distribution admin: [names reserved in the tenant](#names-reserved-in-a-tenant) |
| `PUT, DELETE tenants/{t}/reserved-names/{name}` | server, a distribution admin |
| `GET tenants/{t}/extension-groups` | principal (tenant admins see all; group administrators theirs) |
| `POST tenants/{t}/extension-groups` | tenant admin |
| `GET tenants/{t}/extension-groups/{g}`, `GET …/{g}/members` | tenant admin, the group's administrators |
| `DELETE …/extension-groups/{g}`; `POST …/{g}/description`, `POST …/{g}/members` | tenant admin |

See [Access](access.md).

#### Names reserved in a tenant

A server administrator or a distribution admin reserves a name in a tenant for a holder (spec 0030):
in that tenant, `publish` and `promote` reach it only through grants naming the extension
([Reserved names](publishing.md#reserved-names)).

- `PUT tenants/{t}/reserved-names/{name}` with `{"holder": "…"}` (1 to 128 bytes of text) reserves it,
  or changes its holder: `200` with `name`, `holder`, `added_at`, `added_by`, `changed`, and
  `previous_holder` when another held it. `releases` lists the name's releases in the tenant that are
  not yanked or purged (`channel`, `version`, `platform`, `state`, `origin`), so the holder knows who
  was there before it; they stay as they are. An alias or an invalid name is `400`. A new name or
  another holder is a `reserved.tenant.add` event; the same holder again changes nothing.
- `DELETE tenants/{t}/reserved-names/{name}` releases it (`204`; `404` when not reserved); no release
  changes. A `reserved.tenant.remove` event.
- `GET tenants/{t}/reserved-names` lists them (`reserved_names`), `GET …/{name}` reads one: the
  tenant's administrators read them too, and change none.

### Attachments and feeds

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/extensions/{ext}/attachments` | extension admin, auditor (`GET`); `POST` also publishers |
| `GET, DELETE …/attachments/{id}`; `GET …/attachments/{id}/content` | extension admin, auditor (`GET`) |
| `POST …/attachments/{id}/readers`, `/administrators`, `/feedable`, `/unfeedable` | tenant admin |
| `GET tenants/{t}/blocks/{hash}/attachments`; `DELETE …/attachments/{id}` | tenant admin |
| `POST tenants/{t}/attestation-keys` | server |
| `GET tenants/{t}/attestation-keys/{id}`; `POST …/{id}/activate`, `/retire` | tenant admin |
| `POST tenants/{t}/feed`; `GET tenants/{t}/feed/documents/{issuer_id}` | principal holding `feed` (not a management route) |
| `GET, POST tenants/{t}/feeds`; `GET, POST, DELETE …/feeds/{name}`; `POST …/feeds/{name}/sync` (`409` for a bundle-mode feed) | tenant admin |

See [Attachments and the feed](attachments.md).

### Bundles

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/bundle-uploads`; `GET, DELETE …/bundle-uploads/{id}`; `POST …/{id}/complete` | server |
| `PUT tenants/{t}/bundle-uploads/{id}/chunks/{n}` (the chunk's bytes, with `Content-Digest: sha-256=:…:`) | server |
| `GET tenants/{t}/bundle-imports`, `GET …/bundle-imports/{id}` | server |

An upload's state, a suspended tenant, a limit or another chunk for the same number answer `409`; a
chunk whose bytes do not match its `Content-Digest` or its size, `400`; a chunk number past the
last, `404`; a chunk without `Content-Length`, `411`; too many chunks at once, `429` with
`Retry-After`. See
[Uploading a bundle](upstreams.md#uploading-a-bundle).

### Events and statistics

| Route | Who |
| --- | --- |
| `GET event-kinds` | public: the event catalogue |
| `GET tenants/{t}/events[?since=&until=&kind=&actor=&subject=&outcome=&order=&cursor=&limit=]`, `GET …/events/{id}` | auditor; a deleting or deleted tenant: server |
| `GET events[...]`, `GET events/{id}` | server: the server's own events |
| `GET tenants/{t}/stats/downloads?from=&to=&channel=&extension=&group=` | principal: everything with `audit`, otherwise what its `admin`, `publish` or `promote` grants cover |
| `GET tenants/{t}/stats/releases?channel=&extension=` | the same |
| `GET tenants/{t}/stats/usage?from=&to=&channel=&extension=&group=` | the same; usage reports, users received from downstream marked `received` when grouped by `status` ([Licensing](licensing.md#usage-reports)) |

Statistics are of one package type, `?type=` (`duckdb_extension` by default): the rows of that
type's channels. A `?channel=` of another type is `400`.

See [Observability](observability.md).

## Logs and events

Each request is one line of kista's JSON log, without header values, query strings or bodies. Each
management write and each refusal of an authenticated caller adds a structured line (actor, route
template, resource ids, status, client). A refused caller (`404` standing for forbidden), a token
that is not accepted, and a `5xx` are also recorded as events (`authz.refused`, `auth.failure`,
`request.failed`).

## Example

```bash
# $TOKEN: a tenant token from one of the tenant's issuers, for the tenant's audience
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://repo.example.com/api/v1/tenants/acme/channels/stable/extensions/httpfs/versions/1.2.0?duckdb_version=v2.0.0&platform=linux_amd64"

# yank a release: read its ETag, then send it back
curl -si -H "Authorization: Bearer $TOKEN" \
  https://repo.example.com/api/v1/tenants/acme/channels/stable/extensions/httpfs/releases/<id> | grep -i etag
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'If-Match: "v3"' \
  https://repo.example.com/api/v1/tenants/acme/channels/stable/extensions/httpfs/releases/<id>/yank
```
