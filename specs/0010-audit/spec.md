# Spec 0010: Audit events and download statistics

- **Status**: draft
- **Date**: 2026-10-09
- **Author**: vgsml, Claude

## Summary

kista records what happens to a tenant as **events** and hands them to the organisation's log
pipeline: every management, release and upstream change, refusals and failed authentications, and
(phase 2) authenticated downloads. Events are written in the transaction of the change they record,
kept for **30 days** for the API and the console's recent-activity views, and delivered at least
once to **sinks**: an OpenTelemetry collector (OTLP/HTTP logs) and JSON lines on a file or stdout.
Long-term storage, search, legal holds, erasure and tamper evidence are the log pipeline's: an event
delivered off the server is out of reach of the server's database.

Downloads are counted as **daily statistics** per extension, version, platform, DuckDB version and
channel, kept for years in the database for the API and the console, and exported as
**OpenTelemetry metrics** with kista's operational metrics.

Delivery is phased, each phase a pull request:

- **1a. Events of changes**: the events table, the catalogue, `Tx.Event` in every change with a
  coverage test, the 30-day buffer, reading (API and CLI), the `audit` verb.
- **1b. Delivery**: the asynchronous writer (refusals, authentication failures, failures), the sink
  registry, the JSON-lines and OTLP sinks, `server.start`.
- **2. Downloads and metrics**: install events, daily download counts and installers, the statistics
  API, OpenTelemetry metrics.

## Problem

- **Who did what, when, from where**: who granted install on `prod`, who promoted `acl 1.2`, who
  blocked a body, who added an upstream. Today only key state changes are recorded (`key_events`,
  spec 0003); the rest is in process logs, which are unstructured and not per tenant.
- **Where it belongs**: kista is not a log store. Organisations, the hugr platform (acl-otel) and
  Enterest run OpenTelemetry collectors and log backends that keep, search and protect logs; kista
  must feed them reliably, not duplicate them.
- **Statistics are product data**: a publisher on Enterest wants to see how often each version of
  its extension is downloaded; a tenant administrator, which versions and DuckDB versions are in use
  before retiring one. That needs durable, cheap counts in kista, independent of any backend.
- **What kista can know**: kista sees downloads, not what runs. DuckDB downloads on `INSTALL` and
  then loads its local copy; which nodes run a version is the nodes' telemetry (acl-otel,
  hugr_node). kista answers who downloaded what: 30 days from its buffer, longer from the pipeline.
- **Privacy**: who downloaded what is personal data; it belongs to the short buffer and the log
  pipeline, not to long-lived tables or metric labels.

## Design

### Events

| Field | Meaning |
| --- | --- |
| `id` | a UUIDv7, made inside the writing transaction (a retried transaction makes a new one) |
| `tenant_id` | the tenant; server-level events (DuckDB versions, `tenant.create`, `server.start`, a sink's state, drops not attributable to a tenant) use the fixed id `00000000-0000-0000-0000-000000000000`, never a tenant's. A server administrator's action on a tenant is the tenant's event, and its actor (`server:<issuer>\|sub:…`) is visible to the tenant's readers |
| `at` | when it happened (`Tx.Now`, UTC, microseconds) |
| `kind` | from the catalogue (below) |
| `outcome` | `ok`; `refused` (an answer `401`, `403`, or `404` that stands for forbidden); `failed` (an answer `5xx`, or `ErrBusy`). Validation errors and stale `If-Match` (`400`, `409`, `412`, `428`) are not events: nothing happened |
| `actor` | the actor as the code writes it: `principal:<tenant>/<issuer id>\|sub:<sub>` (or `\|client:<id>`), `server:<issuer>\|sub:…`, `os:<uid>:<user>`, `publisher:<tenant>/<names>`, `system:<what>` (`system:upstream:<tenant>/<name>`, `system:resign`, `system:retention`), `anonymous` |
| `actor_name` | for a token's principal, its `name`, `preferred_username` or `email` claim at the time, for display; otherwise none |
| `subject` | the resource as a path: `channel:prod`, `channel:prod/ext:acl/release:<id>`, `grant:<id>`, `upstream:core`, … (filters match prefixes) |
| `data` | a JSON object of the event's facts, from the catalogue's fields for the kind (never a request body as a whole) |
| `client` | the request's client address after spec 0006's trusted proxies, reduced by `events.client_addresses` (below); none for the CLI and background work |
| `request` | the request's id (below); background work gets its own |

**`data`** holds the catalogue's fields only: changed fields with old and new values, a release's
version and body hash, … Free-form values a caller controls (a publication's provenance) are one
escaped string field, never merged as fields. A failure carries an error **class** from a fixed list
(`egress.refused`, `egress.connect`, `egress.status`, `store.busy`, `signer`, `blob`, …), never an
error's text, which may hold URLs with credentials. At most 16 KiB; a larger one keeps its first
fields and `"truncated": true` (a change never fails for its event).

**Never in events**: tokens, API keys (their prefix only), a key source reference (`key.add` carries
the key's fingerprint and the source's scheme), upstream or sink credentials, key material. The
catalogue marks each field plain, secret-free by construction; a test fails when a field outside the
catalogue, a secret-typed field, or an error text reaches `data`.

**Client addresses**: `events.client_addresses: full | truncated | none` (default `truncated`). An
IPv4-mapped IPv6 address is unmapped first; IPv4 is cut to /24, IPv6 to /48, and an IPv6 address
embedding IPv4 (6to4, NAT64, Teredo; egress's `embedded()`) to the embedded address's /24.

**Request ids**: `X-Request-Id` (`[A-Za-z0-9._-]{1,64}`) is taken from a trusted proxy (spec 0006)
only; otherwise kista makes a UUIDv7 and keeps a caller's value in `data.request_caller`. The id is
logged with every request.

**The catalogue** (`internal/audit/kinds.go`; served at `GET /api/v1/event-kinds` with each kind's
subject form and fields; `v` 1):

- tenants and server: `tenant.create`, `tenant.suspend`, `tenant.resume`, `version.add`,
  `version.c_apis`, `server.start` (kista's version, the schema level, the names of the
  security-relevant settings that changed since the previous start);
- channels and keys: `channel.create`, `channel.versions` (versions added and removed), `key.add`,
  `key.activate`, `key.retire`, `key.resign` (`system:resign` when a re-sign completes, the
  requester in `data`);
- identity: `issuer.add`, `issuer.remove`, `audience.add`, `audience.remove`, `grant.add`,
  `grant.remove`, `publisher.add`, `publisher.remove`, `publisher.github.add`,
  `publisher.github.remove`, `publisher.key.add`, `publisher.key.remove`;
- releases: `release.add`, `release.publish`, `release.promote` (both with `shadows` when a reserved
  or upstream name is released), `release.yank`, `release.deprecate`, `release.activate`,
  `release.current`, `release.public`, `release.private`, `block.add`, `block.remove`;
- upstreams: `upstream.add`, `upstream.remove`, `upstream.change`, `upstream.release` (an intake
  release), `upstream.rejected` (a cell whose outcome changed to a refusal), `upstream.run` (a run's
  summary, as `last_run`), `upstream.pull` (phase 2), `shadow.add`, `shadow.remove`;
- access: `auth.failure`, `authz.refused`, `install` (phase 2; subject
  `channel:<c>/ext:<name>/release:<id>`, `data`: version, platform, DuckDB version, body hash, the
  `User-Agent`);
- the log itself (server events): `audit.dropped`, `audit.sink`.

Tenant and channel removal do not exist; when they do, they are events.

### Where events are written

- **Changes (1a)**: in the operation's **decisive transaction** (the one whose commit makes the
  change), through `Tx.Event(ctx, actor, kind, subject, data)`: the change and its event commit or
  roll back together. The services already take the actor; the client address, the request id and
  `actor_name` come from the request's context (the client key moves from `internal/api` to
  `internal/audit`). Follow-up work in other transactions (a block's sweep of channels, version bumps,
  `FindOrInsertBuild`) writes none; its results are in the decisive event's `data`. Not events:
  leases, `TouchAPIKey`, re-sign batches, upstream cell rows. A registry of the services' write
  operations, each with its kind, backs a test that fails when an operation emits no event or the
  wrong one.
- **Refusals and failures (1b)**: recorded once, at the API layer, from the request's final answer
  (and spec 0006's DuckDB routes in phase 2): `auth.failure` for a `401` (the reason class, never the
  token; per tenant, or the server's when the tenant is unknown), `authz.refused` for a `403` or a
  `404` that stands for forbidden (actor, route, verb), and a `failed` event of the route's kind for a
  `5xx`. Services do not record refusals.
- The CLI writes changes' events in their transactions; it runs the asynchronous writer for its own
  refusals and flushes it before it exits.

**The asynchronous writer (1b)** queues events in memory per tenant (at most 1,000 waiting per tenant,
10,000 in all) and inserts them in batches (every second, or 500). A tenant over its quota drops its
own events only. Refusals are rate-limited per (tenant, actor, kind) to one a second with a count;
beyond 100 a minute per tenant they are coalesced per (tenant, kind, client prefix); the limiter's
map is bounded (10,000 keys, least recently used dropped). Drops are counted and written as
`audit.dropped` (counts by kind) once a minute while dropping, from room the writer reserves for it.
Limits are per replica. These events can be lost in a crash; changes' events never are.

### The buffer

Events stay in the database for `events.retention` (default **30 days**, 1..90 days; up to 365 when no
sink is configured, for sites without a pipeline). A pass every hour (lease `kista/events/retention`)
deletes older events in batches (`DELETE TOP (n)` on SQL Server; `WHERE id IN (SELECT … LIMIT n)`
on PostgreSQL and SQLite). An event not yet delivered to every sink that takes it is kept past the
retention for at most 7 more days, then deleted, counted in `audit.dropped` per sink. A tenant's
events beyond `events.max_rows_per_tenant` (default 1,000,000) are deleted oldest first, the same way.

### Sinks (1b)

`events.sinks` (config, file-only), each with a name, a kind, and the tenants it takes (`["*"]` for
every tenant, which does not include the server's events; `server: true` adds them; none by default,
so a tenant's events never reach a party by accident).

- **`otlp`**: an OpenTelemetry collector's OTLP/HTTP logs endpoint, written by kista (OTLP/HTTP with
  JSON encoding: the wire format is stable; the Go SDK's log exporter is not), through egress's
  client (no redirects, no proxy from the environment, addresses checked at every dial, egress's
  never-list), with the sink's own allowlist (`allow`, egress's `cidr`/`ports` form: internal
  collectors are the usual target), `https` (plain `http` to loopback in profile `dev`), headers from
  `*_file` / `*_env` settings or a key source (never in events, logs or errors). `OTEL_*` environment
  variables are ignored. One `ResourceLogs` per tenant (resource: `service.name=kista`,
  `service.version`, `service.instance.id` (the replica), `kista.tenant.id`, `kista.tenant.name`,
  and `events.resource`'s attributes such as `deployment.environment.name`), so a collector routes
  per tenant. Each event is a log record: `EventName` `kista.<kind>`, severity INFO for `ok` and WARN
  otherwise, timestamp `at`, the body a map (the event's JSON), attributes `kista.event.id`,
  `kista.kind`, `kista.outcome`, `kista.actor`, `kista.subject`, `kista.request`, `client.address`,
  and the trace context of the request's `traceparent` when there was one.
- **`jsonl`**: JSON lines (the API's JSON) to stdout or a file (mode 0600, rotated by size, a number of
  old files kept). A file is written by whichever replica holds the sink's lease: with several
  replicas use stdout or a shared volume.

**Bits.** A sink is registered by name in `event_sinks(name, bit, added_at)`; its bit (at most 16
sinks) is its own for good. When an event is inserted, its `pending` mask gets the bits of the
registered sinks whose tenant selection takes the event's tenant. A sink new or re-added gets only
events written after its registration. A sink removed from the configuration keeps its bit until no
replica has listed it for 10 minutes; then its bits are cleared lazily (on the rows the buffer pass
touches) and its row is removed.

**Delivery** is at least once and approximately in `at` order: a replica holding the lease
`kista/events/sink/<name>` reads `WHERE (pending & bit) <> 0 ORDER BY at, id` in batches (from the
index of undelivered events), checks again that the sink takes each event's tenant, sends, and clears
the bit. A transaction that commits late is delivered after events with a later `at`; receivers
order by `at` and deduplicate by `kista.event.id` (a lease lost mid-batch causes duplicates, never a
loss). A failing sink is retried with backoff (up to 5 minutes between attempts), never blocks
writing or the other sinks, and is reported in the process log and as an `audit.sink` server event
when it starts and stops failing (at most once in 10 minutes). Running with no sink is valid: the
buffer is then the only record.

### Reading (1a)

```text
GET /api/v1/tenants/{t}/events[?since=&until=&kind=&actor=&subject=&outcome=&cursor=&limit=&order=]   audit
GET /api/v1/tenants/{t}/events/{id}                                                                 audit
GET /api/v1/events[...], /api/v1/events/{id}                    the server's events (server administrators)
GET /api/v1/event-kinds                                          the catalogue (any valid token)
```

- **Who reads**: the new grant verb `audit` on the tenant (`admin` implies it; refused on a channel or
  an extension grant and on an issuer-wide grant, as `admin` is, in grant validation and in
  `authz.Covers`), or a server administrator. Missing and forbidden answer alike (spec 0007). `whoami`
  shows whether the caller holds `audit`.
- Newest first by default (`order=asc` oldest first), keyset-paged on `(at, id)` with spec 0007's
  cursors; `since` and `until` are RFC 3339; `subject` matches a prefix.
- CLI: `kista admin events list [-server] [-kind …] [-subject …] [-since …] [-follow] [-format
  table|jsonl] [<tenant>]` (`-follow` prints new events as they are written, oldest first, polling the
  store; `-format jsonl` exports for sites without a pipeline).

### Downloads and statistics (2)

- **What counts**: a `GET` of a binary whose headers were sent with `200`, or `206` for a range
  starting at byte 0, whether or not the body completed. `HEAD`, `304`, later ranges and retries of a
  range do not count. A download from a passthrough channel counts in that channel's tenant.
- **Install events**: such a download by a caller with a valid token is an `install` event, the first
  per (principal, release, client, day) on a replica (a bounded map, 100,000 entries, least recently
  used dropped). An e2e test checks that one `INSTALL`, with and without httpfs loaded, makes one
  event.
- **Counts**: every such download counts in `download_counts(tenant_id, channel_id, name, ext_version,
  platform, duckdb_version, day, authenticated, count)`: no principal, no address, not deduplicated
  (so counts exceed install events). Counts are kept in memory and upserted every minute (`ON CONFLICT
  … DO UPDATE SET count = count + excluded.count` on PostgreSQL and SQLite; update-then-insert in a
  transaction holding `kista/download_counts` on SQL Server); a crash loses at most a minute. `day` is
  a date (text `YYYY-MM-DD` on SQLite).
- **Installers**: a daily pass (lease, after UTC midnight) counts the distinct principals of the
  previous day's `install` events per (channel, release) from the buffer and stores them in
  `download_installers(tenant_id, channel_id, name, ext_version, platform, day, installers)`. No
  principal is kept.
- Counts and installers are kept for `statistics.retention` (default 3 years).
- **Statistics API**:

  ```text
  GET /api/v1/tenants/{t}/stats/downloads?from=&to=&channel=&extension=&group=<d>[,<d>]
      d: day | version | platform | duckdb_version | channel        → rows of {dimensions…, count, installers}
  GET /api/v1/tenants/{t}/stats/releases?channel=&extension=        → per release: 7- and 30-day counts, last download day
  ```

  Readable with `audit` on the tenant (everything), or with `admin`, `publish` or `promote` on an
  extension name, by a token's principal (not a publisher credential): only those names, and only the
  channels the grants cover. Enterest reads with a server token and applies its own rules.
- **OpenTelemetry metrics** (`telemetry.metrics`: an OTLP/HTTP metrics endpoint with the sinks' egress
  rules, every 60 seconds; the Go SDK's stable metric exporter, given egress's client; `OTEL_*`
  ignored; the tenants whose names may appear: none by default):
  - `kista.downloads` (`{download}` counter): `kista.tenant.id`, `kista.channel`, `kista.extension`,
    `kista.authenticated`; `kista.extension.version`, `kista.platform` and `kista.duckdb.version` only
    when listed in `telemetry.metrics.labels`; at most `telemetry.metrics.max_series` series (default
    10,000), beyond which an overflow series counts;
  - `http.server.request.duration` (seconds, `http.route`, `http.response.status_code`);
  - `kista.upstream.cells` by outcome, `kista.upstream.pull.queue`, `kista.events.pending` per sink,
    `kista.events.dropped`, `kista.uploads.active`, `kista.downloads.active`.

### Configuration

```yaml
events:                       # file-only
  retention: 30d
  max_rows_per_tenant: 1000000
  client_addresses: truncated
  resource: { deployment.environment.name: prod }
  sinks:
    - { name: collector, kind: otlp, url: https://otel.internal:4318/v1/logs, tenants: ["*"], server: true,
        allow: [{ cidr: 10.0.0.0/8, ports: [4318] }], headers_file: /run/secrets/otel-headers }
    - { name: stdout, kind: jsonl, path: "-", tenants: ["*"] }
statistics:
  retention: 3y
telemetry:                    # phase 2
  metrics: { url: https://otel.internal:4318/v1/metrics, allow: [...], tenants: ["*"], labels: [], max_series: 10000 }
```

### Data model

Migration 0010 (1a; `-- +min_reader 10`: an older replica would change things without events):

```text
events       id, tenant_id (no FK), at, kind, v, outcome, actor, actor_name null, subject, data (text),
             client null, request, pending (int, bits of sinks still to deliver to)
             primary key (id); index (tenant_id, at, id); index (tenant_id, kind, at);
             index (tenant_id, subject, at); index (at, id) where pending <> 0
event_sinks  name, bit, added_at, last_listed_at        (1b fills it; the table ships in 0010)
```

Migration 0011 (2): `download_counts` (primary key: all but `count`), `download_installers`. Filtered
indexes exist on all three engines (as `ux_releases_seq`); `&` on integers too.

### Package layout

```text
internal/audit     kinds and their fields, Event, Tx.Event's helpers, the asynchronous writer, the
                   buffer's retention, sinks (otlp, jsonl), metrics (2)
internal/store     + events, sinks, download counts and installers
internal/tenants, keys, release, upstream, serve   + events
internal/api, cmd/kista   + reading, statistics
```

### Changes to earlier specs

- **0001**: "Events and statistics": events are delivered to the organisation's log pipeline and kept
  30 days; the hash chain and per-tenant retention become a follow-up; statistics by principal are the
  pipeline's (install events), kista keeps counts and daily installers.
- **0006/0007**: grants take the verb `audit`; refusals and authentication failures become events
  (spec 0006's failure log stays for the process log); the request id is logged; `whoami` shows
  `audit`.
- **0003/0007**: `key_events` and `GET …/keys/events` stay as a key's state history; key changes also
  write events.
- **0008/0009**: every change writes its event in its decisive transaction; upstream runs write
  `upstream.run` summaries.

## Security

- **No secrets** in events: the catalogue's fields, error classes instead of error texts,
  provenance as an escaped string, a test.
- **Tamper evidence** is the log pipeline's: events leave the server within seconds, at least once,
  to a store the server's database administrators do not control. The buffer is a convenience, not
  evidence.
- **Tenant isolation in delivery**: a sink takes only the tenants listed (none by default; the
  server's events only with `server: true`); bits are bound to sink names in the database, and the
  sender checks the tenant again before sending. Metrics name only listed tenants.
- **Egress**: sinks and metrics use egress's client with an explicit allowlist; no redirects, no
  environment proxies or `OTEL_*` settings.
- **Privacy**: principals, display names and client addresses live in events only: 30 days in kista
  (addresses reduced by default), then in the organisation's pipeline under its retention and
  erasure. Counts, installers and metrics carry no principal and no address. Events are tenant data,
  read by `audit` holders and server administrators; server administrators' actions on a tenant are
  visible to its readers.
- **Availability**: a change's event is one insert in its transaction; the serve path never waits on
  events or metrics; sinks never block writing; floods of refusals are coalesced and bounded per
  tenant; the buffer is bounded per tenant.

## Testing

- **Store**: migrations 0010 and 0011 on three dialects; `Tx.Event` rolls back with its change; the
  pending and delete queries.
- **Coverage**: every registered write operation emits exactly one event of its kind; no field outside
  the catalogue, secret-typed field or error text reaches `data`; a refusal is recorded once.
- **Writer**: per-tenant quotas, coalescing, `audit.dropped` never dropped.
- **Buffer**: retention, the 7-day grace for undelivered events, the per-tenant cap.
- **Sinks**: a fake OTLP endpoint and a file receive every event at least once across a restart and a
  failing endpoint; a late commit is delivered, not skipped; a sink renamed, removed or reordered never
  receives another sink's tenants; tenants a sink does not take never reach it; redirects refused;
  credentials never in events or logs.
- **API and CLI**: who reads (`audit`, admin, scoped and issuer-wide grants refused, server
  administrators), filters, paging, `-follow`.
- **Phase 2**: one install event per `INSTALL` (e2e, with and without httpfs); counts and installers;
  statistics and who reads them; metrics with no principal and no unlisted tenant.

## Alternatives considered

- **A self-contained, hash-chained audit log** (an earlier draft of this spec): tamper evidence,
  signed heads, retention with anchors, erasure by pseudonyms. It makes kista a log store, which the
  organisations' pipelines already are; kept as a follow-up for deployments without one.
- **The OpenTelemetry Go SDK's log exporter**: not yet stable; OTLP/HTTP's JSON encoding is.
- **Events only as process logs**: unstructured, lost on a collector's outage, not per tenant.
- **Principals in counts or metrics**: unbounded cardinality, and personal data in long-lived places.
- **Statistics from the log pipeline only**: the console and Enterest's publishers would depend on a
  backend each organisation chooses differently.

## Follow-ups

- A hash-chained, signed audit log for deployments without a log pipeline.
- Per-tenant sinks configured over the API (Enterest tenants' own SIEMs); webhooks.
- Statistics across tenants (Enterest, from its collector); the console's views (spec 0015); air-gapped
  bundles of events (spec 0014).
