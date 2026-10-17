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
| A **server token**: issued by a server issuer (`auth.server_issuers`), for a server audience | routes without a tenant; on tenant routes a server administrator acts with every right |
| A **publisher credential**: an API key or a trusted-publishing token | its tenant's `whoami`, and the publication, promotion, release-read and attachment routes only |

Server and tenant audiences are disjoint, so a token is one or the other. See [Access](access.md)
for issuers, principals and grants, and [Publishing](publishing.md) for publisher credentials.

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
  `holds_admin` and `holds_audit`. `GET /api/v1/whoami` shows a server token's issuer, principals and
  `administrator`. A server token on the tenant `whoami` answers `404`; a tenant token on the
  server `whoami` answers `400`.

### Fresh tokens for management

Management routes, reads included, need a token issued (`iat`) within `auth.admin_token_max_age`
(default 1h). A node's long-lived install token is not enough. A write also needs a token that names
its writer (a `sub` or a client claim). Either failure answers `401`.

## Requests and responses

- **JSON bodies** are `application/json` (optionally `charset=utf-8`; otherwise `415`), one object, at
  most 64 KiB (`413`). Unknown and duplicate keys are refused (`400`). A route that takes no body
  refuses one.
- **Raw bodies**: the publication route takes the extension file (`application/octet-stream`); the
  attachment route takes the document with its media type. Both need `Content-Length`.
- Every answer carries `X-Content-Type-Options: nosniff` and `Vary: Authorization`.
- **Index answers** have an ETag over their bytes and answer `304` to a matching `If-None-Match`.
  `Cache-Control` is `public, no-cache` for an anonymous answer and `private, no-cache` otherwise.
- **Management answers** carry `Cache-Control: no-store`.
- **Creating** is a `POST` to a collection: `201` with `Location` (when the resource has a `GET`),
  `409` for a duplicate. **Deleting** answers `204`; deleting a tenant answers `202`, since its
  rows are removed in the background.

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
| `precondition-failed` | `412` |
| `precondition-required` | `428` |
| `too-large` | `413` |
| `unsupported-media-type` | `415` |
| `too-many-requests` | `429`, with `Retry-After` |
| `unavailable` | `503`; with `Retry-After: 1` when the store or the storage collector is busy |

`401` and `404` have constant bytes and no `detail`. Other answers to an allowed caller carry a
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
another origin). Then `/api/v1/*` allows those origins, no credentials, the headers
`Authorization, Content-Type, If-Match, If-None-Match, traceparent`, and exposes `ETag, Location`.
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
| `POST duckdb-versions`, `POST duckdb-versions/{v}/c-apis` | server |
| `GET tenants[?state=]`, `POST tenants`; `POST tenants/{t}/suspend`, `/resume` | server |
| `GET tenants/{t}` | tenant admin; a deleting or deleted tenant: server |
| `DELETE tenants/{t}` | server: [deleting a tenant](#deleting-a-tenant) |
| `GET tenants/{t}/audiences` | tenant admin |
| `POST tenants/{t}/audiences`, `POST tenants/{t}/audiences/remove` | server |

#### The tenant record

```json
{"name": "acme", "display_name": "Acme Corp", "state": "suspended", "storage_domain": "default",
 "created_at": "2026-10-01T09:00:00Z", "suspended_at": "2026-10-02T09:00:00Z",
 "deletable_at": "2026-10-09T09:00:00Z", "etag": "\"v7\""}
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
- `GET tenants?state=deleting,deleted` lists the tenants in those states (`400` for another value).
  Without `state`, every state but `deleted` is listed.

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
| `GET tenants/{t}/channels/{c}/keys` | channel admin: keys, serving and active key, re-sign progress |
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
| `POST …/extensions/{ext}/releases/{id}/yank`, `/deprecate`, `/activate`, `/current`, `/public`, `/private` | extension admin |
| `DELETE …/extensions/{ext}/releases/{id}` | extension admin: purge a yanked release |
| `GET, POST tenants/{t}/blocks`; `GET, DELETE tenants/{t}/blocks/{hash}` | tenant admin |
| `GET, POST tenants/{t}/publishers`; `GET, DELETE …/publishers/{name}` | tenant admin |
| `GET, POST …/publishers/{name}/github`; `GET, DELETE …/github/{id}` | tenant admin |
| `GET, POST …/publishers/{name}/keys`; `DELETE …/keys/{id}` | tenant admin |

See [Publishing](publishing.md).

### Upstreams

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/upstreams`; `GET, DELETE …/upstreams/{name}` | tenant admin |
| `POST …/upstreams/{name}/public`, `/private`, `/pause`, `/resume`, `/sync`, `/credential` | tenant admin |
| `GET …/upstreams/{name}/cells[?outcome=]` | tenant admin |
| `GET, POST …/upstreams/{name}/extensions`; `DELETE …/extensions/{entry}` | tenant admin |
| `GET, POST …/upstreams/{name}/platforms`; `DELETE …/platforms/{item}` | tenant admin |
| `GET, POST …/upstreams/{name}/keys`; `DELETE …/keys/{item}` | tenant admin |
| `GET tenants/{t}/shadows`; `DELETE tenants/{t}/shadows/{name}` | tenant admin |
| `GET credentials` | server: the configured upstream credentials (names, tenants, prefixes) |

See [Upstreams](upstreams.md).

### Identity, grants and groups

| Route | Who |
| --- | --- |
| `GET, POST tenants/{t}/issuers`; `GET, DELETE …/issuers/{name}` | tenant admin |
| `POST, DELETE …/issuers/{name}/console` | tenant admin: the console's client at that issuer |
| `GET tenants/{t}/grants[?principal=&extension_group=&extension=]`, `POST tenants/{t}/grants` | tenant admin |
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

### Events and statistics

| Route | Who |
| --- | --- |
| `GET event-kinds` | public: the event catalogue |
| `GET tenants/{t}/events[?since=&until=&kind=&actor=&subject=&outcome=&order=&cursor=&limit=]`, `GET …/events/{id}` | auditor; a deleting or deleted tenant: server |
| `GET events[...]`, `GET events/{id}` | server: the server's own events |
| `GET tenants/{t}/stats/downloads?from=&to=&channel=&extension=&group=` | principal: everything with `audit`, otherwise what its `admin`, `publish` or `promote` grants cover |
| `GET tenants/{t}/stats/releases?channel=&extension=` | the same |

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
