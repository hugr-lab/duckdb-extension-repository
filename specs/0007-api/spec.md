# Spec 0007: The HTTP API: index and management

- **Status**: phases 1-2 implemented
- **Date**: 2026-10-08
- **Author**: vgsml, Claude

## Summary

`/api/v1` on kista's https listeners:

- the **index**: which extensions and versions a channel serves to the caller, computed by the same
  resolution as the DuckDB routes, for the hugr node agent, the console and people. It obeys the
  existence rule of specs 0001 and 0006;
- **management**: what `kista admin` does, over HTTP, with Bearer tokens. Tenant administrators act
  through grants; **server administrators** come from config.

Delivery is phased:

1. **The index**, on tenant tokens (spec 0006): channels, extensions, versions, an item lookup,
   `whoami`.
2. **Server identity and tenant-level management**: server issuers and administrators from config;
   the authorizer over principals; tenants, DuckDB versions, audiences, issuer records, grants.
3. **Channel-level management**: channels and their DuckDB versions, keys (activate, retire, the
   re-sign status), releases (state, current, visibility).

Uploading builds is publication (spec 0008). Creating keys in a vault on a tenant's request is key
provisioning (a follow-up): until then only a server administrator registers a key, by its signer
reference. The console's login settings and CORS belong to the console (spec 0015).

## Problem

After spec 0006, kista serves DuckDB, but everything else goes through `kista admin` on the server:

- a hugr node (duckdb-acl spec 093) installs `name VERSION v FROM repo`. Its agent needs to ask "is
  `name@v` installable here, and is it deprecated or yanked now", and "what is current", for its own
  DuckDB version and platform, with the same view its DuckDB secret has;
- a tenant administrator on Enterest, or in an organisation that runs kista as a managed service,
  has no shell on the server;
- the console (spec 0015) needs an API to call with the administrator's own token.

## Design

### Common rules

- Routes are under `/api/v1/` on https listeners only (spec 0006: tokens are never used over http);
  on an http listener `/api/` answers `404`. `serve`'s handler hands `/api/` to the API handler
  before its own path rules (which refuse query strings and bodies); the access log and client
  address are shared.
- **Tokens**: only `Authorization: Bearer`; no cookies, no query-string tokens. On the API an
  `Authorization` header with a token that is not valid answers `401` (unlike the DuckDB routes,
  where it falls back to anonymous): a client learns its token is bad instead of silently seeing
  the public view.
- **Existence**: authorisation comes first, from the path, before the body is read, before
  `If-Match` is compared, before anything else is looked up. A caller without a valid token gets
  `401` for anything not public; a caller with a valid token that may not see or do something gets
  `404`; both with constant bytes. Tenant and channel names are public; the list of tenants is not
  (on Enterest it would enumerate customers).
- **Suspended tenants**: `404` for everyone but server administrators.
- **Errors**: `application/problem+json`, `type` `urn:kista:problem:<name>` from a fixed set
  (unauthorized, not-found, method-not-allowed, invalid, conflict, precondition-failed,
  precondition-required, gone, too-large, unsupported-media-type, too-many-requests, unavailable), a
  `title`, and for an allowed caller a `detail` taken from typed public messages, never from
  internal error strings, never naming another resource.
- **Requests**: `GET`, `HEAD`, `POST`, `DELETE` (and `405` with `Allow` from the route's shape);
  bodies `application/json` exactly (`charset=utf-8` allowed; `415` otherwise), one object, at most
  64 KiB (`413`), read only after the decision, with a read deadline; unknown and duplicate JSON
  keys refused; a request that takes no body (`GET`, `DELETE`, suspend and resume) refuses one. A
  required `If-Match` is one strong ETag or `*` (any version of the existing resource).
- **Responses**: `X-Content-Type-Options: nosniff`; management answers `Cache-Control: no-store`.
- **Limits**: a token bucket per client address (IPv6 by /64; spec 0006's trusted proxies),
  checked before token verification, the table bounded; file-only `serve.api_rate` and
  `serve.api_burst` (default 20 a second, burst 40); when the table is full, buckets that have
  refilled are dropped (at most one sweep a second) and a new client waits rather than everyone
  being reset; `429` with `Retry-After`. Calls that make egress requests (issuer add) also run
  one at a time per tenant: a second one meanwhile is `429`. A tenant holds at most 1000 grants.
  A store that stays busy (lock timeouts, deadlocks) is `503` with `Retry-After`, never `412`.
- **Logs**: one line per request as in spec 0006, never a header value, query string or body; one
  structured line per write and per authenticated request refused by authorisation: actor, route
  template, resource ids, result, client address. Spec 0010 turns them into audit events.

### The index (phase 1)

```text
GET /api/v1/info
GET /api/v1/tenants/{t}/whoami
GET /api/v1/tenants/{t}/channels
GET /api/v1/tenants/{t}/channels/{c}
GET /api/v1/tenants/{t}/channels/{c}/extensions[?duckdb_version=&platform=&cursor=&limit=]
GET /api/v1/tenants/{t}/channels/{c}/extensions/{name}[?duckdb_version=&platform=]
GET /api/v1/tenants/{t}/channels/{c}/extensions/{name}/versions/{v}?duckdb_version=&platform=
```

**The caller's view** of a channel: public releases, plus those of the extensions it holds `install`
on **in that channel** (a tenant- or channel-wide grant covers every extension). A helper next to
spec 0006's `auth.Allows` returns `(all, names)` for a principal set and a channel, computed per
request from the tenant's cached grants (linear in the grants, no cache of its own). A server
administrator (phase 2) sees everything. Everything below is computed in that view.

**One computation.** A channel's releases with their builds (active, deprecated and yanked), its C
API maxima and keys are read once per (channel, `channels.version`, `release_version`),
independently of any caller, and kept in memory, indexed by (name, platform): only the newest
snapshot of a channel is kept, concurrent requests wait for one build, and a request holding older
counters uses a newer snapshot; the view is applied in memory, so the work does not depend on who
asks or what is private (spec 0006's rule). Then the resolution functions decide, for each of the
channel's DuckDB versions and each platform, which release the **flat** path serves (current) and
which the **versioned** path serves for each extension version. Those functions take a raw candidate
list and apply every rule themselves (state, `seq` order, visibility, which DuckDB versions a build
serves, the highest C API major for `c_struct`, nothing without a serving key; a passthrough channel
has no releases); they move from `internal/serve` and `internal/store`'s query filters to
`internal/release`, and the DuckDB routes use them too (their SQL keeps at most a pre-filter), so
the index cannot disagree with what DuckDB gets.

**Routes:**

- `info` (public): kista's version and API version.
- `whoami` (a valid tenant token): its principals in the tenant (as strings), the grants that match
  them, and whether any of them is `admin` (an install identity should hold none).
- `channels` (public): name and kind of each channel of the tenant.
- `channels/{c}` (public): kind; the DuckDB versions served with their C API maxima; for a signed
  channel the trusted and active keys' fingerprints and states (`.well-known`, structured).
- `extensions` (one row per extension name in the view): name, visibility (public, private, or
  mixed); with `duckdb_version` and `platform`, the current version there (or none). Ordered by
  name; `limit` up to 500 (default 100); `cursor` is the last name returned, so it never expires and
  says nothing about changes the caller cannot see.
- `extensions/{name}`: the releases of that name in the view, one row per release (an extension
  version on a platform has one row per build: a `cpp` build per DuckDB version, a `c_struct` build
  per C API major), newest first:
  - extension version, platform, ABI, the build's DuckDB version (`cpp`, `c_struct_unstable`) or C
    API version (`c_struct`), visibility, state (`active`, `deprecated`, `yanked`), created at;
  - the body hash (spec 0002's composite hash of the file without its last 256 bytes, as
    `kista ext inspect` prints it) and the serving key's fingerprint;
  - `serves`: for each DuckDB version whose versioned path resolves to this row, the version, its
    relative versioned path, and the flat path when this row is also current there; `current_for`
    lists those versions; a yanked row serves nothing and lists instead, in `would_serve`, the DuckDB
    versions whose versioned path it would serve if nothing were yanked;
  With `duckdb_version` and `platform`, only rows those paths reach (`serves` or `would_serve`). A name the caller cannot see, or
  that does not exist, answers `200` with no rows.
- `…/versions/{v}` (the agent's question): for that DuckDB version and platform, `status` is the state
  of the row the versioned path serves (`available` or `deprecated`); otherwise `yanked` if a yanked
  row in view would have served it; otherwise `missing`, which covers "does not exist" and "not in
  your view" alike. The row and whether it is current come with it, and `yanked` lists the yanked
  rows in view that would have served the path: with two C API majors of one version, yanking the
  higher one leaves the path `available` with the lower build, and a node that installed the higher
  one learns it by its body hash.
- Yanked releases appear to every caller whose view would include the release (public to anyone,
  private to `install` holders): a node with a yanked version installed learns it.
- Every index answer has an ETag over its bytes (`304` on `If-None-Match`), `Vary: Authorization`,
  and `Cache-Control: public, no-cache` for an anonymous public answer, `private, no-cache`
  otherwise.
- `current_for` is computed for the caller's view, like the DuckDB routes (spec 0006): the agent
  should ask with the token its DuckDB secret uses.
- Passthrough channels list no extensions until spec 0009.

### Server administrators and the authorizer (phase 2)

```yaml
auth:                                   # file-only
  server_issuers:                       # an issuer record's fields; required_claims is mandatory
    - name: ops
      url: https://login.microsoftonline.com/<tid>/v2.0
      required_claims: { tid: "<tid>" }
      roles_claim: roles
  server_audiences: [ "<application client id>" ]  # Entra v2 tokens carry the app's client id; default: [ serve.public_url ]
  server_egress_allow: []               # CIDRs only the server issuers' fetcher may reach (never tenants)
  server_admins:                        # principals of the server issuers; issuer: is refused
    - role:ops|kista.admin
    - subject:ops|<object id>
  admin_token_max_age: 1h               # management needs a token issued at most this long ago (default 1h)
```

- **Which identity.** A token whose `aud` contains a server audience is verified against the server
  issuers only; any other token against the tenant's records only. Server audiences are disjoint
  from every tenant audience: none may lie under `public_url` (except `public_url` itself), so no
  tenant created later can have one as its canonical audience; assigned audiences are checked at
  startup (a conflict refuses to start) and at `audience add`. So a token is a server token or a
  tenant token, never both, even with an `aud` array. The DuckDB routes treat a token carrying a
  server audience as no token.
- A server token on `/api/v1/tenants/{t}/whoami` is `404`: its identity is at `/api/v1/whoami`.
- **Routes without a tenant** (`info`, `/api/v1/whoami`, `/api/v1/duckdb-versions`, tenants): a
  token without a server audience is ignored on public ones; on the others it answers `404`, and
  `/api/v1/whoami` answers `400` "not a server token". On tenant routes, a server administrator
  acts with every right and sees everything in the index.
- **Namespaces.** A server issuer's principals carry the issuer id `server:<name>`, which no tenant
  record can have (tenant record ids are UUIDs), so `server_admins` never matches a tenant
  principal, whatever a tenant names its records. Actors are recorded as `server:<issuer>|<sub>` and
  `principal:<tenant>/<issuer record id>|<sub>` (or `client:` and the client id when there is no
  `sub`).
- **Server issuers** are checked at load (name grammar, https URL, algorithms, required claims,
  `server_admins` parsed against them, server audiences set); discovery and JWKS are fetched lazily
  through egress, with a verifier and cache of their own, so tenant issuer churn cannot evict them.
  A server issuer on a private network is reached through `auth.server_egress_allow`, used only by
  the server issuers' fetcher: tenants' issuer records (which tenant administrators add over the API)
  never reach it.
- **Fresh tokens for management.** Every management route, reads included (they show issuers,
  grants, keys and who changed what), needs `iat` within `admin_token_max_age`. A DuckDB secret
  holding a long-lived install token (up to 7 days, spec 0006) is not enough. An install principal
  (a node's identity) should not hold `admin`; `whoami` flags a token whose principals do.
- **Writers are named**: a write needs a `sub` or a client claim to record as the actor.
- A management request with a token that is stale, or (for a write) names nobody, is `401` like
  any other token that is not valid for it; a token that may not use the route at all is `404`
  first.
- Configuration changes (server issuers, administrators) take effect at restart.

**The authorizer** (`internal/authz`):

```go
type Actor struct {
    Kind       ActorKind       // os | principal | server | system
    ID         string          // the recorded actor string
    Tenant     string          // a principal actor's tenant (its token's)
    Principals auth.Principals // a principal actor's
}
type Resource struct{ Tenant, Channel, Extension string }
type Authorizer interface {
    Allow(ctx context.Context, a Actor, verb Verb, r Resource) error
}
```

- `ServerAdmin` (the CLI) allows OS actors everything, as today.
- `Grants` (the API) allows OS actors and server administrators everything (the API makes a server
  actor of an administrator only). Tenant principals, by their grants:
  - `admin` on the tenant: tenant-wide operations (issuers, grants, channels) and everything inside;
  - `admin` on a channel: that channel's DuckDB versions, keys and releases;
  - `admin` on an extension (in one channel or every channel): that extension's releases;
  - `install`: the index rows of its extensions;
  - reading management data (issuers, grants, keys, releases) needs `admin` on that resource; the
    old `read` verb goes.
- **Server-only**, inside the services (so the CLI and the API agree): creating tenants, DuckDB
  versions and their C APIs, audiences, registering a key by signer reference, and `force` on key
  operations.
- **No lock-out by accident** (best effort): a tenant administrator cannot remove the last
  tenant-wide `admin` grant, or an issuer record whose removal would remove it; the check runs inside
  the transaction under the tenant's lock, so two concurrent removals cannot both pass. It counts
  grants, not people who hold them; a server administrator or the CLI recovers.
- `issuer:` grants with `admin` are refused when added (CLI and API), and ignored when evaluated
  (rows added before this spec grant no `admin`).
- **Infrastructure details** (a key's signer reference, a tenant's storage domain) are shown to
  server administrators only, and so is who among them made a record: a tenant administrator sees
  `created_by: server`.
- An actor's `<sub>` is recorded as `sub:<sub>` when it starts with `client:` or `sub:`, so it
  never reads as a client id. Server issuers' names and URLs are unique in config.

### Management (phases 2 and 3)

**ETags and `If-Match`.** Resources with a `version` column (tenants, channels, keys, releases) have
`ETag: "v<version>"`, returned by their `GET` and carried in list rows as `etag`; every write to them
needs `If-Match` (`428` without, `412` when stale).
Issuer records are immutable: their ETag is their id, and deleting one needs it (a record re-added
under the same name is not removed by mistake). Grants, audiences and DuckDB versions are added and
removed, never changed, and take no `If-Match`. Creating is a `POST` to the collection: `201` with
`Location` where the created resource has a `GET` (tenants, issuers, grants, keys, DuckDB versions;
audiences and a channel's DuckDB versions are add/remove items without one), `409` for a duplicate.
Services take an expected version (0 from the CLI) compared inside the transaction.

**Lists** are ordered and paged with an opaque `(sort key, id)` cursor (management lists are
administrator-only, so a cursor may expire without telling anything): releases by
`(created_at, id)`, key events by `(at, id)` through the channel's keys, grants by `(created_at, id)`,
issuers and tenants by name. `limit` is 1..500 (default 100).

**Shapes** (phase 2): a tenant is `name, display_name, state, created_at, etag` (and
`storage_domain` for server administrators); a DuckDB version `name, kind, c_api_maxima`; an issuer
record the fields of spec 0006 with claim paths as arrays of keys and `max_token_lifetime` as a
duration (`24h`), plus `created_at, created_by, etag`; a grant `id, principal, verbs, channel,
extension, created_at, created_by`. Audience changes answer the tenant's audiences. Deletions answer
`204`. Management answers carry `Cache-Control: no-store`.

Phase 2:

| Route | Who |
| --- | --- |
| `GET /api/v1/whoami` | a server token |
| `GET, POST /api/v1/tenants`; `GET /api/v1/tenants/{t}`; `POST …/{t}/suspend`, `/resume` | server (a tenant administrator may `GET` its own tenant) |
| `GET /api/v1/duckdb-versions`, `…/{v}` | public |
| `POST /api/v1/duckdb-versions`; `POST …/{v}/c-apis` | server |
| `GET /api/v1/tenants/{t}/audiences` | tenant admin |
| `POST …/audiences {audience}`; `POST …/audiences/remove {audience}` | server |
| `GET, POST /api/v1/tenants/{t}/issuers`; `GET, DELETE …/issuers/{name}` | tenant admin |
| `GET, POST /api/v1/tenants/{t}/grants[?principal=]`; `GET, DELETE …/grants/{id}` | tenant admin |

Phase 3 (every id is looked up with the tenant and channel from the path; release routes carry the
extension name, so an extension administrator's right is decided from the path too):

| Route | Who |
| --- | --- |
| `POST /api/v1/tenants/{t}/channels` | tenant admin |
| `GET, POST …/channels/{c}/duckdb-versions`; `DELETE …/duckdb-versions/{v}` | channel admin |
| `GET …/channels/{c}/keys` | channel admin: keys, the serving key, the active key, releases still unsigned by the active key, the last time a re-signer held the channel's lease |
| `GET …/keys/events[?cursor=]` | channel admin |
| `GET …/keys/{id}` | channel admin |
| `POST …/keys {signer_ref, active}` | server |
| `POST …/keys/{id}/activate`, `/retire` (`force` for server administrators only) | channel admin |
| `GET …/channels/{c}/releases[?state=&cursor=]` | channel admin |
| `GET …/extensions/{name}/releases[/{id}]` | extension admin |
| `POST …/extensions/{name}/releases/{id}/yank`, `/deprecate`, `/activate`, `/current`, `/public`, `/private` | extension admin |

There is no re-sign endpoint: replicas with `serve.resign` re-sign on their own (spec 0006); the key
view shows whether one is working and how much is left.

Errors map from typed service errors: a stale expected version is `412`; a refused state change
(spec 0003/0006 rules), a duplicate, or the lock-out rule is `409`; an invalid value, a reference in
the body to something that does not exist (a grant's issuer or channel, an audience to remove), or
an issuer whose discovery document or JWKS cannot be fetched at add is `400`; a path resource the
caller may not see, or that does not exist, is `404`.

### Package layout

```text
internal/api      /api/v1: routing, problems, limits, the index (phase 1), management (phases 2-3)
internal/release  + the resolution functions the DuckDB routes and the index share
internal/auth     + server issuers and principals, the install scope helper, a shared tenant-auth cache
internal/authz    Actor with principals, Resource, the Grants authorizer (phase 2)
internal/serve    hands /api/ to the API handler on https listeners; server tokens are no token
internal/config   + auth.server_issuers, server_audiences, server_egress_allow, server_admins,
                  admin_token_max_age, serve.api_rate and serve.api_burst
internal/app      + server identity (its own verifier and egress), the startup audience check,
                  the services with the API's authorizer
internal/tenants  + server-only checks, lock-out, expected versions (keys: signer references and
internal/keys       force are server-only)
```

## Security

- **Identity**: tokens only over https; server and tenant tokens separated by audience, with
  disjoint audiences enforced; server principals in a namespace tenant records cannot reach; server
  issuers must have required claims; writes need fresh tokens.
- **Escalation**: a tenant administrator's grants stay inside its tenant; tenants, DuckDB versions,
  audiences, signer references and `force` are server-only and checked in the services; extension
  administrators are scoped by the path; ids are looked up within the path's tenant and channel;
  the server issuers' private network is not reachable by tenants' issuer records.
- **No existence leaks**: authorisation first; constant `401` and `404`; the index computes only the
  caller's view, its cursors are names in that view and never expire, its ETags are over the bytes
  it answers; `missing` is the same for "absent" and "not yours".
- **No CSRF surface**: Bearer tokens only, no cookies (CORS comes with the console, spec 0015).
- **DoS**: rate limit before verification, body caps and read deadlines, pagination limits,
  per-tenant serialisation of egress-backed calls.

## Testing

- **Index**: rows per view (anonymous, `install` on one extension in one channel and not another,
  channel-wide, tenant-wide, server administrator), with `serves` and `current_for` checked against
  real `HEAD` answers of the DuckDB routes for every (DuckDB version, platform, name, version) of the
  channel, including two C API majors of one version in different states and a newer private
  release;
  yanked rows per view; the item lookup's four statuses; pagination by name across changes (no
  expiry); `304`; anonymous and invalid-token answers.
- **Authorizer** (phase 2-3): every route × caller (anonymous, tenant token without grants, with
  `install`, extension admin, channel admin, tenant admin, server admin, another tenant's admin, a
  stale token for writes), with `401` and `404` bytes and headers compared; a tenant record named
  like a server issuer; an `aud` array mixing a server and a tenant audience; ids from another
  tenant or channel; an extension administrator probing another extension's release ids.
- **Management**: each write's rules surfacing as `409`/`400`; `If-Match` (`428`, `412`); `force`
  refused for tenant administrators; the lock-out rules; server audiences disjoint at startup and at
  `audience add`.
- **HTTP**: problem bodies, `405`, `413`, `415`, unknown and duplicate keys, `429` before
  verification.
- **e2e**: with the token its DuckDB secret uses, the agent asks `…/versions/{v}` for the pinned
  DuckDB and platform, gets `available`, and `INSTALL x FROM r VERSION '{v}'` succeeds; after a
  yank it gets `yanked`; a second extension version and a C API build exercise `current_for`.

## Alternatives considered

- **Server administrators as a special tenant.** Tenant records could then reach server rights by
  naming.
- **Trying tenant records, then server issuers.** Two verifications per request, misleading logs,
  and an `aud` array could satisfy both.
- **Cursors bound to change counters.** They expire on changes the caller cannot see, which tells it
  they happened.
- **One row per release as "extensions"**: it duplicates the release listing and answers the agent's
  question poorly; rows per name and per version do both.
- **Accept signer references from tenant administrators.** It lets a tenant point kista at any key
  kista's identity can use (spec 0003).
- **A re-sign endpoint.** The re-signer already runs where sign permission is; an endpoint would
  run signing on replicas without it.

## Follow-ups

- Key provisioning on a tenant's request (spec 0004's "tenant key provisioning").
- Spec 0008: publication over this API. Its `publish` and `promote` verbs are not implied by
  `admin` on an extension unless that spec says so.
- Spec 0010: the `audit` verb, events and their routes.
- Spec 0015: the console, its login settings (client ids, scopes per issuer) and CORS.
