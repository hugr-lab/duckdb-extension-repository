# Spec 0040: An auditor reads a tenant's structure and follows its events

- **Status**: implemented
- **Date**: 2026-10-31
- **Author**: vgsml, Claude

## Summary

A principal holding `audit` on a tenant reads, read-only, the tenant's structure: its token issuers,
audiences, grants, extension groups and their members, publishers with their keys, and upstreams,
through the administrators' routes and answers. Following the tenant's events gets easier for such
a reader: several kinds in one filter, a resume cursor at the tail, and the oldest retained event's
time, so a gap shows. Enterest's tenant watch (Enterest spec 0012, P7 and P8) needs both: its tenant
reader holds `audit` in every customer tenant, follows the events and, each hour, compares what the
tenant holds with what Enterest made, without a server administrator's token. The owner approved it
on 2026-10-31.

## Problem

`audit` (spec 0010) reads a tenant's events and statistics. The structure behind them is read only by
the tenant's administrators and server administrators (`pathAdmin`; on `…/publishers`, also a
distribution admin, who reads only distributors' publishers). A watcher that should only observe a
tenant needs `admin` there, which lets it change everything, or a server administrator's token.

Enterest's drift check (its spec 0012) reads every customer tenant's issuers, publishers with their
keys and `distributor` mark, and grants, with `created_by`, to report objects Enterest did not make,
and those a member who has left made. The events alone are not enough: an object made before the
watch started, or in a gap, is never seen. Following the events is also clumsy today: `kind` takes
one exact kind, so a reader skipping the noisy ones (`install`, `tenant.read`, `authz.refused`,
`auth.failure`) reads them all; `next` comes only on a full page, so a reader at the tail rebuilds a
cursor by hand; and nothing says what retention has removed.

## Design

### Who reads

A caller holding `audit` on the tenant. `audit` is only ever granted tenant-wide, of every package
type (`store.Grant` validation, spec 0010), and a tenant-wide `admin` grant of every type implies
it, so the tenant's administrators keep these reads. A server administrator reads them as today.
Nobody else gains anything:

- a distribution admin keeps spec 0037's narrowing: the publishers routes as today (distributors'
  publishers only), never issuers, audiences, grants, groups or upstreams. It holds no `audit` (it
  holds `distribute`, which no grant gives).
- an administrator of one package type, of a channel, of an extension or of an extension group holds
  no tenant-wide `audit`, and reads none of the new routes (an extension group's administrator keeps
  reading its own group, as today).
- a licence manager, a publisher, an install or feed holder reads none of them.

### The structure routes

Read-only `GET`s, answers unchanged (the administrators' JSON, `created_by` masked as a tenant
administrator sees it, below):

| Route | Today | With this spec |
| --- | --- | --- |
| `GET tenants/{t}/issuers`, `…/issuers/{name}` | tenant admin | also `audit` |
| `GET tenants/{t}/audiences` | tenant admin | also `audit` |
| `GET tenants/{t}/grants[?…]`, `…/grants/{id}` | tenant admin | also `audit` |
| `GET tenants/{t}/extension-groups[?extension=]`, `…/{g}`, `…/members` | the list: a tenant's principals, the groups they hold grants on (the full list with each group's grants, and `?extension=`, an administrator's only); a group: tenant admin, the group's admin | also `audit`: the administrators' full view (every group, with its grants; `?extension=` allowed) |
| `GET tenants/{t}/publishers`, `…/publishers/{name}`, `…/keys`, `…/keys/{id}` | tenant admin; a distribution admin, its distributors' publishers | also `audit` (every publisher) |
| `GET tenants/{t}/upstreams`, `…/upstreams/{name}` | tenant admin | also `audit` |

Kept closed: `GET …/grants/principals` (the console's picker); an upstream's sub-routes
(`…/upstreams/{name}/cells`, `/usage`, `/revocations`, `/names`, `/licenses`, `/extensions`,
`/platforms`, `/keys`: an upstream's answer already carries its entries, platforms and keys); every
write (the console client's `POST`/`DELETE` included) and every server-wide route. `?type=` (spec
0034) narrows grants, groups and publishers for an auditor as for an administrator.

The issuer answer gains two fields, for administrators and auditors alike:

- `id`, its record id, as 0037 gave publishers theirs: a principal's `created_by` names the issuer
  by that id, so a reader can tell which issuer made something;
- `console`, its console client when one is set (spec 0015): `client_id`, `scopes`,
  `audience_parameter`, `audience`, `created_at`, `created_by`. The first four are already public
  (`GET tenants/{t}/console` answers them to anyone, for the console's sign-in); only `created_at`
  and `created_by` are new. A public PKCE client holds no secret.

### How

- **API rules.** Issuers, audiences, grants, groups and upstreams: `pathVerbs` with `auditors`
  (`audit`; a tenant-wide `admin` of every type implies it), the events routes' rule; a group's
  routes keep admitting its own administrator. Publishers and keys: a new access kind,
  `auditOrDistributor` (`pathVerbs` with `auditors`, or a distribution admin as `distributionAdmin`
  admits one), used by those four routes only.
- **Services, two checks** (defence in depth: the API rule and the service both decide):
  - `readTenant`: `audit` on the tenant (`Authz.Allow(…, VerbAudit, Resource{Tenant})`), never a
    distribution admin. `ListIssuers`, `ListAudiences`, `ListGrants`, `GetPackageGroup`,
    `PackageGroupMembers` (beside the group administrator's path), `upstream.Service.List` and
    `Get` call it, and so does `IssuerConsoleClients`, the issuers' console clients for their
    answers. `ListPackageGroups`' full view (today `tenantAdmin`: every group with its grants,
    `?extension=`) becomes `admin` or `audit`; anyone else keeps today's view.
  - `readPublishers`: `readTenant`, or `tenantOrDistributor` as today. `ListPublishers`,
    `TenantPublisherKeys` and `ListPublisherKeys` call it; the narrowing to distributors' publishers
    stays on `distributor(a)`. `ListPublisherKeys` reads its publisher after `readPublishers`
    (`publisherOf`); the `publisher` lookup it used had no other caller, and is gone.
  - Every write keeps its check (`admin`; a distribution admin on a distributor's publisher).
- **No audit event** is written for these reads, as for the events' reads.

### Following the events

`GET tenants/{t}/events` (spec 0010) already answers each event's `request` (the request id), which
ties the events one request wrote (a key's removal with `purge_held` and the `catalogue.purge` it
causes). It gains, for any caller that reads it:

- **`kind` takes a list**, comma-separated: each entry a kind from the event catalogue
  (`GET /api/v1/event-kinds`) or a family prefix ending in `.*` (`grant.*`) of one that exists, no
  entry twice; that bounds the list, with no fixed cap. A list starting with `!` excludes instead
  (`kind=!install,tenant.read,authz.refused,auth.failure`), so a reader does not miss kinds added
  later; the two forms do not mix. A family is expanded to the catalogue's kinds in it, and the list
  read as exact matches on `ix_events_tenant_kind` (`kind IN (…)`, `kind NOT IN (…)`), never a
  `LIKE`. (Not a range `kind >= 'grant.' AND kind < 'grant/'`: on PostgreSQL the column takes the
  database's collation, often a linguistic one such as `en_US.utf8`, where `.` and `/` are ignored at
  the first level and `'grant.add' < 'grant/'` is false.)
- **The tail settles.** The writer commits each replica's events in batches about every second,
  so an event can commit after a later one is already readable. With `order=asc`, a read answers
  only events older than now minus a settle window (`events.settle`, default 30s, within 1s..1h; the
  window `kista admin events list -follow` used, which now reads the setting too), and covers the
  time up to that point.
- **Paging with `order=asc`** (documented in `api.md`): `next` is always present. A full page's
  `next` is its last event's cursor; a shorter page is the tail, and its `next` is the time the read
  covered (now minus `events.settle`, or `until` when sooner), whatever the `kind` filter matched,
  so a filtered reader advances too and `oldest_at` never shows it a false gap. With no cursor the
  read starts at the oldest retained event, with `since` at `since`; with `until` it covers up to
  `until`. A reader keeps `next` and asks again later. The cursor stays opaque (never built by hand).
  An event that commits later than the settle window is missed by a tail reader (the writer stalled
  longer than `events.settle`); an operator raises the window if that happens. `order=desc` is
  unchanged (`next` on a full page only).
- **What retention removed**: the answer gains `oldest_at`, the time of the tenant's oldest retained
  event (omitted when it holds none). A reader whose cursor is older than `oldest_at` has missed
  events and reads the structure afresh. Events the writer or retention dropped are still counted by
  `audit.dropped` (spec 0010).

### For consumers

What a reader gets: `200` with the answers above; `404` with no right on the tenant, for a
suspended, deleting or unknown tenant, and for a missing object; `401` for a token whose issuer was
removed (spec 0007), or that is stale: older than `auth.admin_token_max_age` (default 1h; these are
manage routes), after which it gets a fresh token; `429` (`rate-limited`) past the API rate.

`created_by` and an event's `actor` take these forms for a tenant's readers:
`principal:<tenant>/<issuer id>|<sub>` (the token's bare `sub`; `client:<id>` for a token with no
`sub`; `sub:<sub>` only when the sub itself begins `client:` or `sub:`), `publisher:<tenant>/<names>`,
`system:<what>`, `anonymous` (an event of an unauthenticated request), and, masked, `server`,
`license-manager` and `distribution-admin`. Only a server administrator sees who those were.

### Rate

The API's rate is per client address (`serve.api_rate`, default 20 a second, burst 40). A watcher's
load is small (an inventory of six lists an hour per tenant, an events read every few minutes:
about 3 requests a second for 1,000 tenants), and Enterest runs its own kista: its deployment raises
`api_rate` and `api_burst` if it needs to. No per-principal budget: a configured setting covers it,
with no code.

### Docs

`access.md` (what `audit` reads; `holds_audit` in the verbs table; the `created_by` forms),
`api.md` (the rows above, the events parameters; remove the stale `…/publishers/{name}/github` rows),
`observability.md` (the actor forms corrected to the bare `sub`; the events parameters, `request`,
`next` and `oldest_at`), `console.md` (the issuer answer's `console`), `publishing.md` and `tenants-and-channels.md` (who reads).

## Security

- **What an auditor learns** is the tenant's current structure: issuers (URL, JWKS URI, algorithms,
  `required_claims` with their values, the roles, groups and client claim paths, `max_token_lifetime`,
  `id`, the console client's id, scopes and audience settings), audiences, grants, extension groups and members, publishers with their keys (type,
  fingerprint, title, public key, expiry, `last_used_at`), upstreams (prefix, channel, mode, keys,
  platforms, entries, the configured credential's name, the last run's summary, whose errors may
  name the remote's answers). An upstream's prefix never carries userinfo or a query (refused when
  set); an issuer's JWKS URI never carries userinfo (refused), and a query in it is the identity
  provider's own public endpoint. No credential hides in either. The events an
  auditor already reads tell most of it one change at a time (`issuer.add` with its URL,
  `grant.add` with the principal and scope, `publisher.key.add` with the fingerprint, spec 0010);
  `last_used_at`, the issuer's claim settings and its console client are new to it. Nothing secret is there: no token,
  no client secret, no private key, no upstream credential (only its name). Of these resources only
  the issuers and upstreams carry an ETag; it lets no one write.
- **A distribution admin gains nothing**: it holds no `audit`, the issuers', grants' and others'
  services refuse it whatever the API rule says, and its publishers' narrowing is unchanged.
- **No existence leak**: without a right the answer is the same `404` as for an unknown tenant.
- **Writes** keep their check in the API rule and in the service.
- **The events' `kind` list** is bounded by the catalogue: each entry a known kind or family, none
  twice, read as exact matches.

## Testing

- `internal/api`, for each structure route: a tenant-wide auditor gets `200` and the
  administrators' JSON (`created_by` masked alike; a distributor's publisher with its keys, its
  `created_by` shown as `distribution-admin`; the group list's full view with grants and
  `?extension=`); every write answers it `404`, and so do `…/grants/principals` and each
  upstream sub-route named above; `?type=` narrows as for an administrator. Refused (`404`): an administrator
  of one package type; an extension group's administrator (except its own group; the group list
  keeps its narrow view for anyone but an administrator or auditor); an auditor of
  tenant B on tenant A (with an A token, no grant there: `404`; its B token is not one of A's:
  `401`); a server token with no role; an install holder; a licence manager. A stale
  token, one older than `admin_token_max_age`, gets `401`. A distribution admin still reads only distributors' publishers and keys, `404` on
  the rest, and `404` on publisher reads in a suspended tenant.
- `internal/tenants` and `internal/upstream`: `readTenant` admits `audit`, a tenant-wide `admin` and a
  server administrator; `ListIssuers`, `ListAudiences`, `ListGrants`, the group reads and the upstream
  reads refuse an `ActorDistributionAdmin`; the writes refuse an auditor.
- Events: a `kind` list with an exact kind and a prefix (the store's `IN` and `NOT IN` on every
  engine);
  an exclusion list; an unknown kind, a duplicate or a mixed list refused (`400`). With `order=asc`:
  a full page's `next` is its last event; a short page's `next` is the covered time (now minus
  `events.settle`), with and without a `kind` filter; an event stored late (committed with an `at`
  10s in the past, after a read at the tail) is answered by the next read with that read's `next`,
  never skipped; events newer than the window are not answered yet; `until` bounds the covered time; with no
  cursor the read starts at the oldest retained event. `oldest_at` follows retention.
- The issuer answer carries `id`, equal to the id in a principal's `created_by`, and `console` once a
  console client is set (absent before, and after its removal).

## Alternatives considered

- **One new endpoint listing everything** (`GET tenants/{t}/inventory`): one request, but a second
  format for the same objects to keep in step; the reads are hourly and few.
- **A narrower answer for auditors** (no public keys, no claim values): nothing in them is secret,
  and two shapes of one object cost more than they protect.
- **A separate `GET …/issuers/{name}/console`**: one more route for four fields the issuer's answer
  can carry.
- **A per-principal rate budget**: code for a load a setting already covers.
- **Enterest reads as a server administrator**: a token that can change every tenant, held by a
  watcher that only reads; refused by the owner (Enterest spec 0012, decision 2).

## Delivery

One PR, `0040-audit-reads`: the API rules and access kind, `readTenant` / `readPublishers` and their
callers, the group list's full view, the issuer's `id` and `console`, the events' `kind` list and
exclusions, the settled tail and `next`, `oldest_at`, the `events.settle` setting
(`configuration.md`), the tests and the docs.
No migration.

## Follow-ups

- The console shows these screens to an auditor only if it gains an auditor's view; not needed by
  Enterest.
