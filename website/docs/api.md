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
| A publisher's **signed request**: `Kista-Statement` and `Kista-Signature`, signed with one of its keys, no `Authorization` | its tenant's `whoami`, and the publication, promotion, release-read and attachment routes only; a distributor's publisher also its own catalogue entries (`GET …/catalogue`, `GET …/catalogue/{source}/extensions/{ext}`, spec 0037); any publisher its own entries' metadata and icon (`PUT`, `GET …/meta`, `GET …/icon`, spec 0036) |

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
  routes keyed or filtered by a name take `?type=`: the licences and licence keys, the server's
  reserved names, attachments, the catalogue (and `catalogue?name=`),
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

### Warnings

An administrator's change may answer, besides its usual body, a `warnings` array: advisory notes
about what it did, which went through. Today they are licence warnings (spec 0035 phase 2): an entry
that holds a licence served publicly ([Licensing](licensing.md#public-serving-of-licensed-entries)),
written as events right after the change commits, best effort.

```json
{"name": "tresor", "visibility": "public", "…": "…",
 "warnings": [{"name": "tresor", "source": "publisher:enterest-ci", "channel": "prod",
               "message": "publisher:enterest-ci holds a licence for tresor and has a public release in channel prod: …"}]}
```

Each warning names the extension, the entry (`source`, as answers name a source), the channel and
a message; one per entry, name and channel, however many platforms or releases the change touched.
The member is left out when there is none, and the answer's status and ETag are as without it. It
is answered only to a caller who administers the route's path (a server administrator; a tenant,
channel or extension administrator): a publisher's signed request, a principal without `admin`
there (one holding `publish` or `promote`), a licence manager and a distribution admin get none,
and their change writes only the event. A read's answer and an error answer never carry it.

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
| `stale` | `409`: a signed metadata write older than the entry's last one, or of its second with another body ([spec 0036](publishing.md#an-entrys-icon-and-summary)); read the metadata again |
| `publisher-taken` | `409`: a distribution admin's new [distributor's publisher](publishing.md#distributors-publishers) whose name a tenant's own publisher holds (spec 0037); it says nothing else about that publisher |
| `not-computed` | `400`: a [distributor's statistics](observability.md#a-distributors-statistics) query before any week is computed, or with `from` after the newest week computed (spec 0036); the same query is answered once its weeks are |
| `precondition-failed` | `412` |
| `precondition-required` | `428` |
| `too-large` | `413`: a management body over 64 KiB (384 KiB for an entry's metadata), the detail saying which |
| `unsupported-media-type` | `415` |
| `distributors-entry` | `404`: a tenant administrator's write on a distributor's publisher's licence entry ([who writes it](access.md#server-administrators-and-tenant-administrators), spec 0037), the one `404` with a type and a detail of its own |
| `rate-limited` | `429`: the per-client [rate limit](#rate-limits), or a caller's entry metadata writes (per replica); `Retry-After` the limiter's |
| `upload-slots` | `429`: the upload slots per caller, tenant or server, for publications and attachments; `Retry-After: 5` |
| `ingest-busy` | `429`: every ingest slot taken, for a publication (`Retry-After: 5`) or a bundle's chunk (`Retry-After: 1`) |
| `chunks-busy` | `429`: too many chunks of one bundle upload in flight; `Retry-After: 1` |
| `catalogue-full` | `429`: the source's catalogue holds as many entries or bytes as it may (`catalogue.max_entries`, `max_bytes`; its entries' icons count in bytes, spec 0036); no `Retry-After`: it clears when the source's builds are placed, purged or trimmed, so a client stops |
| `issuer-busy` | `429`: an issuer is being added to the tenant; `Retry-After: 5` |
| `tenant-limit` | `429`: a [tenant limit](administration/tenants-and-channels.md#limits) reached (spec 0038), named by the problem's `limit` member (`bytes`, `channels`, `upstreams`, `publishers`), without figures; no `Retry-After`: it clears when the tenant frees room or a server administrator raises the limit, so a client stops |
| `unavailable` | `503`; with `Retry-After: 1` when the store or the storage collector is busy |

`401` and `404` have constant bytes and no `detail`, except a publisher's signed request's `401`,
which names the key's fingerprint kista found and kista's clock ([spec 0023](publishing.md#signed-requests)),
and the `distributors-entry` `404`, which says why. Each `429` has its own type (spec 0037): a client
acts on the type, retrying all but `catalogue-full` and `tenant-limit`. Other answers to an allowed caller carry a
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
both or neither). Over the limit the answer is `429` (`rate-limited`) with `Retry-After`. Adding
an issuer (which fetches its discovery document) runs one at a time per tenant: a second one
meanwhile answers `429` (`issuer-busy`). Uploads (publications and attachments) take a slot per
caller, per tenant and per server; without one the answer is `429` (`upload-slots`). An entry's
metadata writes (spec 0036) are limited per caller and tenant, on each replica, to one a second in
bursts of ten, counted once a write passed its checks (`429`, `rate-limited`). See
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
| `GET tenants/{t}` | tenant admin; a deleting or deleted tenant: server; a distribution admin: `{"name", "state", "holder_id"}` of an active or suspended tenant (spec 0037; `holder_id` left out when none is set; no ETag; a `tenant.read` event each time) |
| `DELETE tenants/{t}` | server: [deleting a tenant](#deleting-a-tenant) |
| `PUT tenants/{t}/holder {"holder_id"}`, `DELETE tenants/{t}/holder` (`If-Match`) | server: [the expected holder](#the-expected-holder) |
| `GET tenants/{t}/limits`; `PUT tenants/{t}/limits` (`If-Match` optional) | tenant admin reads, server sets: [the tenant's limits](#the-tenants-limits) |
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
- `POST tenants` takes `name`, and optionally `display_name`, `storage_domain`, `holder_id` and
  `limits` (the limits' `PUT` body: [the tenant's limits](#the-tenants-limits)), applied in the
  creating transaction and recorded as `tenant.limits` beside `tenant.create`.

#### The tenant's limits

A server administrator limits what a tenant can make its catalogue grow by and how many channels,
upstreams and publishers of its own it holds (spec 0038, [Limits](administration/tenants-and-channels.md#limits)).

- `GET tenants/{t}/limits` (the tenant's administrators and server administrators, a suspended
  tenant's too for the latter) answers `{"limits": {"bytes": {"set", "limit", "used"}, "channels": …,
  "upstreams": …, "publishers": …}}`: `set` is the tenant's own value (a number, `"unlimited"`, or
  `"default"` when the configuration's `limits:` applies), `limit` the value in force (a number, or
  `null` for unlimited bytes; a count's unlimited is its ceiling), `used` what it counts. Its `ETag`
  is over the stored values.
- `PUT tenants/{t}/limits` (server administrators) takes the limits it names, e.g.
  `{"bytes": "50GiB", "channels": 20, "upstreams": "default", "publishers": "unlimited"}`: a number
  (for `bytes` also a size: `KiB`, `MiB`, `GiB`, `TiB`), `0` for none, `"unlimited"`, or
  `"default"`. A limit left out is unchanged. A count above its ceiling, a negative number or an
  unknown name is `400`. `If-Match`, when sent, must be the `GET`'s ETag (`412` otherwise). It
  answers as the `GET`. A change writes `tenant.limits` with each changed limit's `old` and `new`
  stored value and the `limit` in force; one that changes nothing writes nothing. The tenant's
  version is not bumped.

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
  `admin:<actor>`, `community-sync@<commit>`); `?name=` reads one. A distribution admin gets each
  name and its kind only (spec 0037): `sources` names other tenants' upstreams and administrators. Names are reserved per package
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
| `GET tenants/{t}/whoami` | public (a tenant token or a publisher credential; a publisher's `publishers` give each one's `id`, and a distributor's publisher's `distributor`, spec 0037) |
| `GET tenants/{t}/channels` | public |
| `GET tenants/{t}/channels/{c}` | public: kind, DuckDB versions and C API maxima, signing keys |
| `GET tenants/{t}/channels/{c}/extensions[?duckdb_version=&platform=&prefix=&cursor=&limit=]` | public, in the caller's view |
| `GET tenants/{t}/channels/{c}/extensions/{name}[?duckdb_version=&platform=]` | public, in the caller's view |
| `GET tenants/{t}/channels/{c}/extensions/{name}/versions/{v}?duckdb_version=&platform=` | public: `available`, `deprecated`, `yanked`, `withdrawn` (only a [withdrawn](upstreams.md#withdrawals) release would serve it: a `withdrawn` list of its rows) or `missing`. An index row of a withdrawn build carries `"withdrawn": true` and `withdrawn_reason`, its DuckDB versions in `would_serve` (spec 0039) |
| `GET tenants/{t}/channels/{c}/extensions/{name}/attachments[/{id}]` | public, in the caller's view |
| `GET tenants/{t}/attestation-keys` | public |

The caller's view is the public releases plus those of the extensions it holds `install` on in that
channel. See [Using kista from DuckDB](duckdb.md).

### Channels and keys

| Route | Who |
| --- | --- |
| `POST tenants/{t}/channels` | tenant admin |
| `GET, POST tenants/{t}/channels/{c}/duckdb-versions`; `DELETE …/duckdb-versions/{v}` | channel admin |
| `GET tenants/{t}/channels/{c}/sources`; `GET, PUT, DELETE …/sources/{name}` | channel admin: [a name's source in the channel](upstreams.md#a-names-source-in-a-channel) and its [rule](upstreams.md#the-catalogue-and-a-channels-rules) (`PUT` takes `{"source": "tenant" \| "upstream:<name>" \| "publisher:<name>", "mode": "auto" \| "manual"}`, `mode` optional; a follow's rules have mode `follow`; each answer is `{"name", "source", "mode", "current", "newest" (the newest available in the catalogue: a version with a held, withdrawn or revoked build is not), "outcome", "outcome_at", "set_by", "set_at", "available": {"<source>": <releases>}}`, `source` `""` when none; a list is `{"sources": [...]}`). A release's management answer carries `source` and `selected` (whether it is the channel's source of the name: only those are served) |
| `GET tenants/{t}/channels/{c}/follows`; `GET, PUT, DELETE …/follows/{source}` | channel admin: the channel's [follows](upstreams.md#the-catalogue-and-a-channels-rules) (`PUT` takes no body; each answer is `{"source", "set_by", "set_at", "skips": {"<name>": "<reason>"}}`; a list is `{"follows": [...]}`) |
| `GET tenants/{t}/catalogue[?source=&name=]` | tenant admin: the tenant's [catalogue](upstreams.md#the-catalogue-and-a-channels-rules), `{"entries": [{"source", "name", "versions": [newest first], "rules": {"<channel>": "<mode>"}, "held": [the versions with a held build, omitted for none], "revoked": [...], "withdrawn": [...], "summary", "icon": {"sha256", "width", "height"}, "distributor" (a distributor's publisher's entry; spec 0036, the metadata omitted for none)}]}`; a distribution admin: its distributors' publishers' entries only (removed ones too), `rules` empty, each with its `publisher_id` (a name two of them had is `publisher-id:<id>` as a source), in a suspended tenant too (spec 0037); a distributor's publisher's signed request (spec 0037): its own entries so, `?source=` left out or `publisher:<its name>` or `publisher-id:<its id>` (any other `404`) |
| `GET tenants/{t}/catalogue/{source}/extensions/{ext}` | tenant admin: an entry's versions (newest first), each with its builds: `{"versions": [{"version", "builds": [{"id", "platform", "slot", "abi", "duckdb_version", "c_api", "body_hash", "visibility", "origin", "checked", "created_at", "state": "held" \| "available" \| "withdrawn", "available_at" (held), "revoked_at", "withdrawn_at", "withdrawn_reason", "placed": [{"channel", "state"}]}]}], "summary", "icon", "distributor"}` (spec 0036); a distribution admin: a distributor's publisher's entry (`publisher:<name>` or `publisher-id:<id>`, removed or not), without `placed`, in a suspended tenant too (spec 0037); a distributor's publisher's signed request: its own entry so (`publisher:<its name>` or `publisher-id:<its id>`, either meaning the signer; any other `404`) |
| `GET, PUT tenants/{t}/channels/{c}/hold` | channel admin: the channel's [hold](upstreams.md#holds), `{"channel", "hold": "2h0m0s"}`; `PUT` takes `{"hold": "2h"}` (`0s` for none, at most `limits.ceilings.max_hold`, 90 days by default) |
| `POST tenants/{t}/catalogue/extensions/{ext}/builds?version=&platform=[&visibility=]` | `publish` on the extension tenant-wide, or a [distributor's publisher](publishing.md#distributors-publishers) without grants: [publishing into the catalogue](publishing.md#publishing-into-the-catalogue) only; the answer names the publisher that published (`publisher_id`, and `distributor` for a distributor's), neither for a tenant principal's publication |
| `GET, PUT tenants/{t}/catalogue/{source}/extensions/{ext}/meta`; `GET …/icon` | spec 0036, [an entry's icon and summary](publishing.md#an-entrys-icon-and-summary), a rule of their own: any verified caller of the tenant (a signed request from any of its publishers too), the service deciding by the entry's source, every refusal `404`. The tenant's own sources: tenant admin; a distributor's publisher's: a distribution admin or a server administrator (a tenant admin reads it); a publisher's signed request: its own source only. `PUT {"summary": string \| null, "icon": {"content_type": "image/png", "data": "<base64>"} \| {"sha256"} \| null}` replaces both (the body at most 384 KiB); answers `{"package_type" (omitted for the default), "source", "name", "summary", "icon": {"sha256", "width", "height"} \| null, "set_at", "signed_at", "set_by" (not to a publisher), "distributor"}`; `409` `stale`, or `conflict` when a kept icon's hash changed; `429` `catalogue-full`, or `rate-limited` beyond a caller's metadata writes (one a second, bursts of ten); `503` when the icon is being collected. The caller is authorized before the body is read. `GET …/icon` answers the PNG with kista's own headers |
| `DELETE tenants/{t}/catalogue/builds/{id}` | tenant admin; a distribution admin for a distributor's publisher's build: purge a build no release holds (`409` when one does, or it was withdrawn) |
| `POST tenants/{t}/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw {"reason": "…"}` | server, a distribution admin: [withdraw](upstreams.md#withdrawals) a version of a distributor's publisher (`publisher:<name>`, or `publisher-id:<id>` when the name is ambiguous, `409` otherwise; removed or not), whether it holds builds of it or not, a suspended tenant's too (spec 0037); answers `{"tenant", "source", "name", "version", "reason", "withdrawn": [{"build", "platform", "slot"}], "already"}`, never the channels |
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
| `POST …/extensions/{ext}/releases/place` | `promote` on the extension; a name without a rule in the channel, or `"early": true` (a held version), channel admin ([placing from the catalogue](publishing.md#placing-from-the-catalogue)); `source` as answers name it, and for a server administrator also `publisher-id:<id>` of a live distributor's publisher, as its catalogue names one |
| `POST …/extensions/{ext}/releases/{id}/yank`, `/deprecate`, `/activate`, `/current`, `/public`, `/private` | extension admin |
| `DELETE …/extensions/{ext}/releases/{id}` | extension admin: purge a yanked release |
| `GET, POST tenants/{t}/blocks`; `GET, DELETE tenants/{t}/blocks/{hash}` | tenant admin |
| `GET, POST tenants/{t}/publishers`; `GET, DELETE …/publishers/{name}` | tenant admin; `POST {"name", "distributor"}` creates a [distributor's publisher](publishing.md#distributors-publishers): server, a distribution admin (`404` to a tenant admin). A distribution admin (spec 0037) lists and reads distributors' publishers only (a tenant's own one is left out of a list, and reading it is `404`), creates only with `distributor` (`404` without), and gets `409` `publisher-taken` for a name a tenant's own publisher holds; never `DELETE`. A tenant holds at most 200 publishers of its own and, counted apart, 10 distributors' publishers by default (`limits.ceilings`). Every publisher answer carries its `id` (spec 0037) |
| `GET, POST …/publishers/{name}/github`; `GET, DELETE …/github/{id}` | tenant admin |
| `GET, POST …/publishers/{name}/keys`; `GET, DELETE …/keys/{id}` | tenant admin; a distribution admin for a distributor's publisher only (spec 0037: the tenant's own publishers, marked or not, answer it the `404` of a missing publisher); a key is unique in the tenant: one another publisher holds is `409`, recorded for a distribution admin as an `authz.refused` (`reason` `key-taken`) when a tenant's own publisher holds it; `DELETE …?purge_held=true` purges the builds the key published that are still held, and answers what it purged and kept, revoked ([holds](publishing.md#holds-and-early-placement)) |
| `PUT …/publishers/{name}/hold {"hold": "24h"}` | tenant admin: the publisher's [hold](publishing.md#holds-and-early-placement); a publisher answers `hold` (omitted for none); a distributor's publisher's: server, a distribution admin only, and its answer carries `effective_hold`, the larger of its hold and the tenant's minimum (spec 0037; omitted for none) |
| `GET, PUT tenants/{t}/distributor-hold` | tenant admin: the tenant's [minimum hold for distributors' publishers](access.md#the-minimum-hold-for-distributors-publishers) (spec 0037), `{"hold": "24h0m0s"}`, one day by default; `PUT` takes `{"hold": "24h"}` (`0s` for none, at most `limits.ceilings.max_hold`, 90 days by default). A distribution admin gets `404`: it reads the minimum only as its publishers' `effective_hold` |
| `PUT …/publishers/{name}/distributor {"distributor": "enterest"}` | server, a distribution admin: rename a [distributor's publisher](publishing.md#distributors-publishers)'s distributor (`409` once it is used: builds, releases, rules, a hold or downloads, spec 0036); an existing publisher is never marked one (spec 0037: a publisher without the mark answers `404` to everyone); a publisher answers `distributor` (omitted for the tenant's own) |
| `POST, DELETE …/publishers/{name}/license-keys` | server; a distribution admin for a distributor's publisher only: mark a publisher a source of licence keys, or unmark it |

A publisher answers with its `id`, its `keys` and `license_keys` (true when marked). While it is marked, adding or
removing its keys and grants is a server administrator's (`404` to a tenant administrator), and a
distribution admin's for a distributor's publisher only: to a distribution admin, the tenant's own
publishers, marked or not, answer `404` as missing ones (spec 0037). A distributor's publisher is
marked so too, and holds no grants (`400` to a server administrator, `404` to a tenant
administrator and to a distribution admin, whom the grant routes do not admit). A marked publisher's publication of a body whose key block is malformed answers
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
| `GET tenants/{t}/licenses[?source=]`; `GET …/licenses/{name}[?source=]` | a tenant admin or a licence manager |
| `POST tenants/{t}/licenses?source=` (the licence as the body; `If-Match` to replace one) | a tenant admin, except into a distributor's publisher's entry (spec 0037: `404 distributors-entry`); a licence manager into a distributor's publisher's entry only |
| `DELETE …/licenses/{name}?source=`; `POST …/licenses/{name}/private?source=` (`If-Match`) | a tenant admin, except in a distributor's publisher's entry; a licence manager in a distributor's publisher's entry only |
| `POST …/licenses/{name}/public?source=` (`If-Match`) | tenant admin, except in a distributor's publisher's entry (a server administrator's) |
| `GET …/upstreams/{name}/licenses`; `POST …/upstreams/{name}/licenses/sync` | tenant admin |
| `GET …/upstreams/{name}/usage`; `POST …/upstreams/{name}/usage {"usage": "forward"\|"off"}` (`If-Match`) | tenant admin: the usage switch and the last forwarding run |
| `GET …/upstreams/{name}/revocations`; `POST …/upstreams/{name}/revocations {"revocations": true\|false}` (`If-Match`) | tenant admin: [revocations from a kista](upstreams.md#revocations-from-a-kista), `{"remote_kista", "revocations", "stored": [{"build", "name", "version", "platform", "body_hash", "state", "reason", "seen_at", "applied_state", "applied_at", "lifted_state", "lifted_at", "unlisted_at" (the source no longer lists it: kept for what was applied or lifted), "status": "applied" (something applied is left to lift) \| "lifted" \| "waiting" (nothing applied)}]}`, its `ETag` the answer's content (a run moves it); the switch answers the upstream (`400` on one that is neither a repository mirror of a kista nor in bundle mode) |
| `DELETE …/upstreams/{name}/revocations/{build}` (`If-Match`) | tenant admin: lift the upstream's revocation of a build; answers `{"build", "restored"}`; `409` when nothing applied is left to lift (never applied, or lifted already) |
| `GET …/upstreams/{name}/names[?prefix=&limit=&after=]` | tenant admin: the names a mirror's `*` took from its kista remote ([`*` on a mirror of a kista](upstreams.md#on-a-mirror-of-a-kista)), `{"names": [...], "next"}`; none without `*`. The upstream's answer carries their number, `discovered`, on a mirror with `*` |
| `GET tenants/{t}/license-keys[?source=]`, `GET …/license-keys/{name}[?source=]` | a tenant admin or a licence manager: the tenant's live licence keys, each with the entries it is live for |

A licence is answered as its claims and state (`name`, `entry`, `id`, `issuer`, `holder_id`,
`holder`, `plan`, `versions`, `limits`, `issued_at`, `expires_at`, `grace_until`, `key`, `source`,
`upstream`, `visibility`, `status`, `etag`, and `distributor` when its entry is a distributor's
publisher's, spec 0037), never with the token. A tenant holds a licence per
catalogue entry (spec 0035): `entry` is the licence's entry, by its source (`upstream:<name>`,
`publisher:<name>`, or a removed one's `upstream-id:<id>` or `publisher-id:<id>`), and `source` how
it arrived (`import` or `upstream`): the query's `?source=` names the entry, the answer's `source`
is how the licence arrived. Every write (the import, `DELETE`, `…/public`, `…/private`)
**requires** `?source=` naming the entry (`upstream:<name>`, `publisher:<name>`, `upstream-id:<id>`,
`publisher-id:<id>`), given once: `400` without it or given twice, and `400` naming why for
`tenant`, a malformed form, a source that is no upstream or publisher of the tenant, a removed
source, or a DuckDB core or community upstream; `DELETE` also takes the id form of a removed source
whose entry still holds a licence (`400` when it holds none, `404` for a live entry without one).
The checks run in that order: the caller, licensing (`409` while it is off, on an import), the
entry, then the licence itself. A licence manager names only the entries of distributors'
publishers (`publisher:<name>` of a live one, `publisher-id:<id>`, and a removed one's id form on
`DELETE`): any other `?source=`, whatever it is, answers as a missing path does (`404`, the refusal
recorded as `authz.refused`), before anything about it is checked. A read (`GET
…/licenses/{name}`) takes `?source=` optionally: without it, the extension's only licence, and
`409` naming the entries where it has several. A removed source is `upstream-id:<id>` or
`publisher-id:<id>` in the licence routes, even where the catalogue routes still show it by name (a
removed distributor's publisher). An import stores the licence in the entry it names; a new one
answers `201` with `Location` (`…/licenses/<name>?source=<entry>`); over the licence stored in that
entry without `If-Match`, `409`. `GET …/upstreams/{name}/licenses`
answers the last relay run, `{"last_run_at", "result": {"counts", "refused", "error"}}`, and the sync
request (`202`) answers the same; `409` for an upstream that relays nothing or is paused, and while
licensing is off (an import too). An import verifies against the live keys of the entry it names,
and a relayed licence against those of its upstream's own entry (spec 0021, 0035: that source's own
live builds declaring the key, or its newest available version in the catalogue); without one it is
refused (`400`, `kid_untrusted`, naming the entry). In a tenant with
an [expected holder](#the-expected-holder), an import of another holder's licence answers `400`
with `holder_mismatch` in its detail, naming the expected and the given holder. A licence older (by
`iat`) than one the tenant holds of the same name from the same issuer for the same holder, in a
distributor's entry whatever its key or in any other entry when verified by the same key, is refused (`400`, "the licence is refused (older): …"), checked after the key and the
holder and before the stored licence is compared, so not `409` (spec 0037,
[Licensing](licensing.md#older-licences-are-refused)), recorded as a `license.refused` event; the
relay and bundles refuse it with the rule `older` in their results.
`GET …/license-keys` answers `{"names": [{"name", "keys": [{"kid", "sources": [{"kind":
"publisher"|"upstream", "name", "distributor", "builds": n}]}]}]}`: exactly what an import's key
check would accept now, per entry (`sources`: the entries the key is live for, `distributor` for a
distributor's publisher, `builds` the entry's builds declaring the key); keys the server refuses are
left out. With `?source=`, one entry's keys: `200` with an empty list when it has none, `400` as for
a write (an unknown, removed or malformed source, `tenant`, a DuckDB core or community upstream);
from a licence manager, a source that is not a distributor's publisher answers `404`. A licence is a
package's (spec 0034): these
routes take `?type=` (`duckdb_extension` by default), an import stores the licence under that type,
and a channel serves only its own type's licences. An import and a `…/public` may answer licence
[warnings](#warnings) to a tenant administrator, as may a release made public, a placement, a
promotion and a source's switch to an administrator of the path; a relay run (the sync request is
`202`) and an uploaded bundle's import write only the events. See [Licensing](licensing.md).

### Identity, grants and groups

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/issuers`; `GET, DELETE …/issuers/{name}` | tenant admin |
| `POST, DELETE …/issuers/{name}/console` | tenant admin: the console's client at that issuer |
| `GET tenants/{t}/grants[?principal=&extension_group=&extension=]`, `POST tenants/{t}/grants` | tenant admin (spec 0037: not a distribution admin) |
| `GET tenants/{t}/grants/principals[?prefix=]` | tenant admin |
| `GET, DELETE tenants/{t}/grants/{id}` | tenant admin |
| `GET tenants/{t}/extension-groups` | principal (tenant admins see all; group administrators theirs) |
| `POST tenants/{t}/extension-groups` | tenant admin |
| `GET tenants/{t}/extension-groups/{g}`, `GET …/{g}/members` | tenant admin, the group's administrators |
| `DELETE …/extension-groups/{g}`; `POST …/{g}/description`, `POST …/{g}/members` | tenant admin |

See [Access](access.md).

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
| `GET tenants/{t}/stats/downloads?from=&to=&channel=&extension=&group=` | principal: everything with `audit`, otherwise what its `admin`, `publish` or `promote` grants cover; `group` takes `source` too, a label (`unknown` for counts before spec 0036) |
| `GET tenants/{t}/stats/releases?channel=&extension=` | the same |
| `GET tenants/{t}/stats/usage?from=&to=&channel=&extension=&group=` | the same; usage reports, users received from downstream marked `received` when grouped by `status` ([Licensing](licensing.md#usage-reports)) |
| `GET distributor-stats/downloads?distributor=&from=&to=[&type=]` | server, a distribution admin: a [distributor's statistics](observability.md#a-distributors-statistics) (spec 0036), `{"distributor", "from", "to", "rows": [{"week", "extension", "downloads", "public", "installer_days", "withheld", "min_tenants"}]}`, one row per week and extension, read from the weekly snapshots summed over every tenant, of the weeks computed only. `from` and `to` take any day, widened to whole weeks (`to` clamped to the newest week computed, `from` to the oldest kept); the answer's `from`, `to` and `week` are Mondays. `withheld`: the downloads' non-public part left out (`downloads` equals `public`, `installer_days` omitted); `installer_days` is omitted whenever its non-public part is left out; `min_tenants` is the minimum the week was computed with. `400` `invalid` for a missing, repeated or overlong (over 128 bytes) `distributor`, an unknown `type`, a day that is not one, `from` after `to`, a range before the oldest kept and more than 53 weeks; `400` `not-computed` before any week is computed and for `from` after the newest week computed |

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
