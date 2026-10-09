# Spec 0010: Audit events and download statistics

- **Status**: implemented (phases 1a, 1b, 2a, 2b)
- **Date**: 2026-10-09
- **Author**: vgsml, Claude

## Summary

kista records what happens to a tenant as **events** and hands them to the organisation's log
pipeline: every management, release and upstream change, refusals and failed authentications, and
(phase 2a) authenticated downloads. Events are written in the transaction of the change they record,
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
- **2. Downloads and metrics**, in two parts: **2a** install events, the DuckDB routes' refusals,
  daily download counts and installers, the statistics API; **2b** OpenTelemetry metrics.

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
| `tenant_id` | the tenant; server-level events (DuckDB versions, `tenant.create` (the tenant does not exist before), `server.start`, a sink's state, drops not attributable to a tenant) use the fixed id `00000000-0000-0000-0000-000000000000`, never a tenant's. A server administrator's other actions on a tenant are the tenant's events; to the tenant's readers their actor shows as `server`, without name or address (as `created_by`, spec 0007) |
| `at` | when it happened (`Tx.Now`, UTC, microseconds) |
| `kind` | from the catalogue (below) |
| `outcome` | `ok`; `refused` (an answer `401`, `403`, or `404` that stands for forbidden); `failed` (an answer `5xx`, or `ErrBusy`). Validation errors and stale `If-Match` (`400`, `409`, `412`, `428`) are not events: nothing happened |
| `actor` | the actor as the code writes it: `principal:<tenant>/<issuer id>\|sub:<sub>` (or `\|client:<id>`), `server:<issuer>\|sub:…`, `os:<uid>:<user>`, `publisher:<tenant>/<names>`, `system:<what>` (`system:upstream:<tenant>/<name>`, `system:resign`, `system:retention`, `system:serve` (`server.start`), `system:sinks`, `system:audit`), `anonymous` |
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
catalogue lists the fields each kind may carry; the services fill them from typed facts (ids, names,
versions, hashes, states), never from request bodies or error texts; `Data` refuses any other field,
and the tests check that no key source reference reaches an event.

**Client addresses**: `events.client_addresses: full | truncated | none` (default `truncated`). An
IPv4-mapped IPv6 address is unmapped first; IPv4 is cut to /24, IPv6 to /48, and an IPv6 address
embedding IPv4 (6to4, NAT64, Teredo; egress's `embedded()`) to the embedded address's /24.

**Request ids**: `X-Request-Id` (`[A-Za-z0-9._-]{1,64}`) is taken from a trusted proxy (spec 0006)
only; otherwise kista makes a UUIDv7. The id is logged with every request (`request_id`).

**The catalogue** (`internal/audit/audit.go`; served publicly at `GET /api/v1/event-kinds` with each
kind's subject form and fields; `v` 1):

- tenants and server: `tenant.create`, `tenant.suspend`, `tenant.resume`, `version.add`,
  `version.c_apis`, `server.start` (kista's version, the schema level, a digest (64 bits of SHA-256 of
  the section's JSON) of each security-relevant configuration section: `profile`, `signers`, `blob`,
  `serve`, `egress`, `auth`, `publish`, `upstreams`, `events`, `statistics`, `telemetry`, and the names of those changed since
  the previous start (a section the previous start did not digest is not one), which every replica
  records);
- channels and keys: `channel.create`, `channel.versions` (versions added and removed), `key.add`,
  `key.activate`, `key.retire`, `key.resign` (actor `system:resign`, when a re-sign completes and the
  serving key moves);
- identity: `issuer.add`, `issuer.remove`, `audience.add`, `audience.remove`, `grant.add`,
  `grant.remove`, `publisher.add`, `publisher.remove`, `publisher.github.add`,
  `publisher.github.remove`, `publisher.key.add`, `publisher.key.remove`;
- releases: `release.add`, `release.publish` (subject the release), `release.promote` (subject
  `channel:<c>/ext:<name>`, `data.releases` the releases made, `from_channel`), all three with
  `shadows` when a reserved or upstream name is released and followed by a `shadow.add` in the same
  transaction when they record a tenant shadow (spec 0009), `release.yank`, `release.deprecate`, `release.activate`,
  `release.current`, `release.public`, `release.private`, `block.add`, `block.remove`;
- upstreams: `upstream.add`, `upstream.remove`, `upstream.change` (`data.change`: `set`,
  `extension.put`, `extension.remove`, `platform.add`, `platform.remove`, `key.add`, `key.remove`,
  with the value), `upstream.release` (an intake
  release), `upstream.rejected` (a cell whose outcome changed to a refusal), `upstream.run` (a run's
  summary, as `last_run`; a pull-through fetch (spec 0009 phase 2) is its `upstream.release` or
  `upstream.rejected`), `shadow.add`, `shadow.remove`;
- access: `auth.failure`, `authz.refused`, `install` (phase 2a; subject
  `channel:<c>/ext:<name>/release:<id>`, `data`: release, name, version, platform, DuckDB version,
  body hash, the `User-Agent`);
- the log itself (server events): `audit.dropped`, `audit.sink`; `request.failed` (1b, a `5xx`).

Tenant and channel removal do not exist; when they do, they are events.

### Where events are written

- **Changes (1a)**: in the operation's **decisive transaction** (the one whose commit makes the
  change), through `Tx.Event(ctx, actor, kind, subject, data)`: the change and its event commit or
  roll back together. The services already take the actor; the client address, the request id and
  `actor_name` come from the request's context (the client key moves from `internal/api` to
  `internal/audit`). Follow-up work in other transactions (a block's sweep of channels, version bumps,
  `FindOrInsertBuild`) writes none (a block's yanks follow its `block.add`). Not events:
  leases, `TouchAPIKey`, re-sign batches, upstream cell rows. A registry of the services' write
  operations, each with its kind, backs a test that fails when an operation emits no event or the
  wrong one.
- **Refusals and failures (1b)**: recorded once, at the API layer, from the request's final answer
  (and spec 0006's DuckDB routes in phase 2a), subject `route:<pattern>`, `data.route` the method and
  the route's pattern: `auth.failure` for a `401` given to a request that sent a credential (actor
  `anonymous`; `data.reason` a class: `malformed`, `invalid`, `api_key_route`, `publisher_route`,
  `stale_token`, `unnamed_writer`; never the token; the tenant's when the path names one, the
  server's on routes without a tenant; a request without a credential is not a failure),
  `authz.refused` for a `403` or a `404` that stands for forbidden (the caller as actor,
  `data.status`), and `request.failed` for a `5xx` (`data.status`). A path naming an unknown or
  suspended tenant records nothing (it names nobody's tenant). Services do not record refusals.
- The CLI writes changes' events in their transactions; it acts as a server administrator, so it has
  no refusals and no asynchronous writer.

**The asynchronous writer (1b)** queues events in memory per tenant (at most 1,000 waiting per tenant,
10,000 in all) and inserts them in batches (every second, or 500). A tenant over its quota drops its
own events only; when all queues together are full, the largest one loses its oldest event, so a
flooded tenant never crowds out another. Refusals are rate-limited per (tenant, actor, kind) to one
a second, each event carrying `count`, the refusals it stands for; a key's suppressed refusals are
emitted as a trailing event once it has been quiet for a second (or when it is evicted, or at
shutdown), so counts add up. Beyond 100 refusals a minute in a tenant they are keyed by the client's
prefix (`/24`, `/48`) instead of the actor; beyond 300 events a minute of a (tenant, kind), by
nothing. The limiter holds at most 10,000 keys (idle ones, then the least recently used, are
evicted). Drops (a full queue, or a batch the database refused) are counted per tenant and kind and
written as `audit.dropped` (actor `system:audit`) once a minute and at shutdown, inserted directly,
never queued. Limits are per replica. The writer outlives `serve`'s listeners (refusals while
draining are written) and its final flush takes at most 10 seconds. These events can be lost in a
crash; changes' events never are.

### The buffer

Events stay in the database for `events.retention` (default **30 days**, at least 2 days (phase 2a counts a day's installers from its events after it ends); at most 365
days, and at most 90 once a sink is configured, for sites without a pipeline). A pass every hour
(lease `kista/events/retention`) deletes older events in batches (`DELETE TOP (n)` on SQL Server;
`WHERE id IN (SELECT … LIMIT n)` on PostgreSQL and SQLite). An event not yet delivered to every sink
that takes it is kept past the retention for at most 7 more days, then deleted. A tenant's events
beyond `events.max_rows_per_tenant` (default 1,000,000) are deleted oldest first, those every sink
has before undelivered ones. Undelivered events deleted either way are counted in one server
`audit.dropped` (actor `system:retention`, `counts`: `overdue`, `overflow`).

### Sinks (1b)

`events.sinks` (config, file-only), each with a name, a kind, and the tenants it takes (`["*"]` for
every tenant, which does not include the server's events; `server: true` adds them; none by default,
so a tenant's events never reach a party by accident).

- **`otlp`**: an OpenTelemetry collector's OTLP/HTTP logs endpoint, written by kista (OTLP/HTTP with
  JSON encoding: the wire format is stable; the Go SDK's log exporter is not), through egress's
  client (no redirects, no proxy from the environment, addresses checked at every dial, egress's
  never-list), with `egress.allow` plus the sink's own allowlist (`allow`, egress's `cidr`/`ports`
  form: internal collectors are the usual target), `https` (plain `http` to loopback in profile
  `dev` with `egress.allow_loopback_http`), `timeout` (default 10s), headers from `headers_file`
  (`Name: value` lines, `#` comments; never in events, logs or errors; `Content-Type`, `Content-Length`
  and `Host` refused). The sink ignores the `OTEL_*` environment (which configures metrics only). Any answer other than `2xx` is a
  failure (a redirect too); the batch is sent again. One `ResourceLogs` per tenant (resource:
  `service.name=kista`, `service.version`, `service.instance.id` (the replica), `kista.tenant.id`, `kista.tenant.name`,
  and `events.resource`'s attributes such as `deployment.environment.name`, except `service.*` and
  `kista.*`, which are kista's), so a collector routes per tenant. Each event is a log record:
  `eventName` `kista.<kind>`, severity INFO for `ok` and WARN otherwise, timestamp `at`, the body a
  map (the event's JSON), attributes `kista.event.id`, `kista.kind`, `kista.outcome`, `kista.actor`,
  `kista.subject`, `kista.request`, `client.address`. No trace context: events do not store it.
  A batch answered `400` or `413` is sent again in halves (a `413` also lowers the largest batch
  until a single event is found to be the one refused); a single event still refused is dropped,
  counted in the tenant's `audit.dropped` (`data.sink`, the event's `count`; never sent to the sink
  that refused it), so that one event never blocks a sink. After 3 drops in 10 minutes (per replica)
  with no batch taken between them the endpoint is failing (backoff, `audit.sink`), and its events
  wait.
- **`jsonl`**: JSON lines (the API's JSON with `tenant_id` and `tenant`) to stdout (`path: "-"`) or a
  file (an absolute path, mode 0600, rotated by size: `max_size`, default 100 MiB; `keep` old files,
  default 5, and `keep: 0` too; a batch never splits across files, and a failed write is truncated
  away before it is sent again). The file is opened without following a symbolic link and made 0600;
  two sinks never share one. On stdout, which `serve`'s JSON log shares, each line is written whole
  and records are told apart by their `kind` and `id`. A file is written by whichever replica holds
  the sink's lease: with several replicas use stdout or a shared volume (a replica reopens the file
  when the path no longer names the one it holds, after another rotated it).
- **Identities**: on a tenant's events a server administrator's or the CLI's actor shows as `server`,
  without name or address, as to the tenant's readers (a sink may route to the tenant's own
  pipeline); `unmasked: true` sends them as they are.

**Bits.** A sink is registered by name in `event_sinks(name, bit, added_at, last_listed_at)`; its
bit (at most 16 sinks, removed ones included until their bits are cleared) is its own until it is
removed. When an event is inserted, its `pending` mask gets the bits of the
registered sinks whose tenant selection takes the event's tenant (a tenant created since the
replica last read the tenants gets the bits of every sink that names tenants; the sender decides).
Every process that writes events registers the configured sinks at start (`serve` and the CLI); a
`serve` replica lists them again every minute (`last_listed_at`) and re-reads the tenants. A sink new
or re-added gets only events written after its registration. A sink removed from the configuration
keeps its bit until no process has listed it for 10 minutes; then a replica's refresh retires it
under the registry's lock, only if it is still unlisted: its row becomes a tombstone that frees the
name (a sink re-added under it gets a new bit) and keeps the bit reserved while the bit is cleared
from every event in batches (each batch only while the tombstone exists: a replica late to it never
clears a bit another sink has taken since); then the tombstone goes and the bit is free. Every
`serve` replica runs this refresh, with no sink configured too. A configured sink that finds no free bit is left out
(logged; the CLI and `serve` still start) until a removed sink's bit is freed.

**Delivery** is at least once and approximately in `at` order: a replica holding the lease
`kista/events/sink/<name>` reads `WHERE (pending & bit) <> 0 ORDER BY at, id` in batches (from the
index of undelivered events), checks again that the sink takes each event's tenant, sends, and clears
the bit (events of tenants it does not take lose the bit unsent). A transaction that commits late
is delivered after events with a later `at`; receivers order by `at` and deduplicate by
`kista.event.id` (a lease lost mid-batch causes duplicates, never a loss). A failing sink is retried
with backoff (up to 5 minutes between attempts; the replica keeps the lease while it waits, so
another does not retry at once), never blocks writing or the other sinks, and is reported in the
process log and as an `audit.sink` server event (actor `system:sinks`, subject `sink:<name>`,
`data`: `state`, `class` (`egress.status`, `egress.refused`, `egress.connect`, `sink`), `status`)
when it starts and stops failing (at most once in 10 minutes; a process starts from the state the
last `audit.sink` reported, so a recovery after a restart or a lease handover is reported too). Running with no sink is valid: the
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
- CLI: `kista admin events list [-server] [-kind …] [-subject …] [-actor …] [-since …] [-limit n]
  [-follow] [-format table|jsonl] [<tenant>]` (`-follow` prints new events, oldest first, polling the
  store and looking back 30 seconds each time, so an event that commits late is printed once;
  `-format jsonl` exports for sites without a pipeline).

### Downloads, statistics and metrics (2a, 2b)

- **What counts**: a `GET` of a binary whose headers were sent with `200`, or `206` for a range
  starting at byte 0, whether or not the body completed. `HEAD`, `304` and a `206` not starting at
  byte 0 do not count; every `200` does (the uncompressed name ignores `Range`), so a client retrying
  a whole download counts again. A download from a passthrough channel counts in that channel's
  tenant.
- **Install events**: such a download by a principal holding `install` on it (a valid tenant token;
  a public release downloaded with a token that holds no `install` counts as authenticated and makes
  no event) is an `install` event
  (actor the principal, subject `channel:<c>/ext:<name>/release:<id>`, `data`: release, name,
  version, platform, the path's DuckDB version, body hash, the `User-Agent`'s first 200 bytes; the
  token's display name as `actor_name`), the first per (principal, release, client network (`/24`,
  `/48`), day) on a replica (a bounded map, 100,000 entries, least recently used dropped), at most
  1,000 a day per principal and 100,000 principals a day on a replica (beyond, downloads count and
  make no event: a token cannot flood its tenant's log; how many principals an issuer admits is the
  issuer's `required_claims`), through the asynchronous writer. A token naming neither a subject
  nor a client is one installer with every other such token of its issuer. A passthrough channel
  looks at no token: its downloads count, anonymous, and make no event. An e2e test checks that one
  `INSTALL` is one download with DuckDB's built-in client and with httpfs and a token, and the latter
  one install event.
- **Refusals on the DuckDB routes**: a token that does not verify is `auth.failure` (`reason`
  `invalid`, subject `route:extension`, `data.route` `<method> extension`; the request is still
  served as anonymous), a valid token without `install` on a path's extension that answers `404` is
  `authz.refused`.
- **Counts**: every such download counts in `download_counts(tenant_id, channel_id, name, ext_version,
  platform, duckdb_version, day, authenticated, count)`: no principal, no address, not deduplicated
  (so counts exceed install events). Counts are kept in memory and upserted every minute, in key
  order and transactions of at most 500 keys (`ON CONFLICT … DO UPDATE SET count = count +
  excluded.count` on PostgreSQL and SQLite; update-then-insert holding `kista/download_counts` on SQL
  Server); counts the store refuses wait for the next minute (beyond 200,000 keys waiting, further
  downloads are not counted, and logged); a crash loses at most a minute. Counts are not evidence:
  anyone may download a public release, and a range from byte 0 counts as a download. `day` is
  text `YYYY-MM-DD` (UTC) on every engine.
- **Installers**: an hourly pass (lease `kista/statistics/daily`) counts, for each of the last 7 days
  not counted yet (`statistics_days`) that ended at least an hour ago (the replicas' writers have
  flushed its events) and whose start is still within `events.retention` (so all its events are in
  the buffer; `events.retention` is at least 2 days; `events.max_rows_per_tenant` may still have
  deleted some, and the day is then undercounted), the distinct principals of the day's `install`
  events per (channel, name, version, platform), tenant by tenant, and stores them in
  `download_installers(tenant_id, channel_id, name, ext_version, platform, day, installers)` with the
  day's mark, in one transaction. A day missed (kista down for longer) has no installers. No
  principal is kept.
- Counts and installers are kept for `statistics.retention` (default 3 years; 30 days to 10 years),
  deleted by the same pass.
- **Statistics API**:

  ```text
  GET /api/v1/tenants/{t}/stats/downloads?from=&to=&channel=&extension=&group=<d>[,<d>]
      d: day | channel | extension | version | platform | duckdb_version
      → {from, to, group, rows: [{dimensions…, count, authenticated, installers}]}
  GET /api/v1/tenants/{t}/stats/releases?channel=&extension=
      → {releases: [{channel, extension, version, platform, last_7_days, last_30_days, last_download_day}]}
  ```

  `from` and `to` are days (default the 30 days to `to`, and `to` today), at most 366 days inclusive;
  `group` defaults to `day`, and `version` brings `extension` with it; a query over more than
  100,000 of the caller's stored rows is refused (`400`: narrow it). `installers` is the sum of the
  daily installers of the rows grouped (a principal installing on two days counts twice) and is
  absent when grouping by DuckDB version (installs are counted per release). `releases` lists the
  releases downloaded within the statistics' retention.

  Readable with `audit` on the tenant (everything), or with `admin`, `publish` or `promote` on an
  extension name, by a token's principal (not a publisher credential): only those names, and only the
  channels the grants cover; a principal with none of these, or asking for a channel or an extension
  none of its grants covers, answers `404` (recorded as refused), so a query never tells what exists
  beyond the caller's scope. The routes take a fresh token, as the other management routes. Server
  administrators read everything; Enterest reads with a server token and applies its own rules.
- **OpenTelemetry metrics (2b)**: the Go SDK's stable metric exporter (OTLP over HTTP with protobuf)
  and its own HTTP client, set up as the hugr platform's other services (tresor-server) from the
  standard environment: `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
  (nothing is exported without one), the `OTEL_EXPORTER_OTLP_(METRICS_)HEADERS`, `TIMEOUT`,
  `CERTIFICATE`, `CLIENT_CERTIFICATE` and `CLIENT_KEY` settings (a private CA, mutual TLS),
  `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` (over kista's defaults `service.name=kista`,
  `service.version`, `service.instance.id`; a malformed part is logged and left out),
  `OTEL_METRIC_EXPORT_INTERVAL` (60 s), `OTEL_SDK_DISABLED`; `OTEL_METRICS_EXPORTER` is `otlp` or
  `none`, and the protocol (`OTEL_EXPORTER_OTLP_METRICS_PROTOCOL`, else `OTEL_EXPORTER_OTLP_PROTOCOL`)
  `http/protobuf`: with an endpoint set, anything else refuses to start. The SDK's errors (an export failing) go to
  kista's log, at most once a minute. The endpoint is the operator's, as the process log's; event
  sinks (tenants' data, routed per tenant) keep their own configuration and egress rules. What
  metrics may name is kista's configuration (`telemetry.metrics`, file-only):
  - `kista.downloads` (`{download}` counter): `kista.authenticated`; and `kista.tenant.id`,
    `kista.channel`, `kista.extension` only for the tenants listed in `telemetry.metrics.tenants`
    (names or `*`; none by default: unlisted tenants' downloads count in series without them);
    `kista.extension.version`, `kista.platform` and `kista.duckdb.version` only when `version`,
    `platform`, `duckdb_version` are in `telemetry.metrics.labels`;
  - `http.server.request.duration` (seconds; `http.request.method` (`_OTHER` beyond the usual
    ones), `http.route`: `api`, `extension`, `extension-versioned`, `well-known`, `healthz`,
    `readyz`, `none`, `other`, never a path; `http.response.status_code`);
  - `kista.downloads.active`, `kista.uploads.active`, `kista.upstream.pull.queue` (gauges of this
    replica), `kista.events.dropped` (a counter of this replica's writer);
  - `kista.events.pending` per sink (`kista.sink`: a sink's name only when metrics may name every
    tenant it takes, or it takes the server's events only; the others together as `_other`, since a
    sink's name may be a tenant's) and `kista.upstream.cells` per outcome (`kista.outcome`): read
    from the store at each export (one pass over at most a million undelivered events, so a longer
    backlog counts as at most a million; at most 5 seconds; a failing read is logged and left out,
    never the whole export), the same on every replica (take the maximum across
    `service.instance.id`, not the sum);
  - at most `telemetry.metrics.max_series` series per instrument (default 10,000; 100 to 1,000,000),
    beyond which the SDK's overflow series (`otel.metric.overflow`) counts. Series are cumulative and
    kept until the process ends: with `*` and the `version` label on a self-service deployment, one
    tenant's many releases can push others' new series into the overflow.

### Configuration

```yaml
events:                       # file-only
  retention: 720h             # 30 days
  max_rows_per_tenant: 1000000
  client_addresses: truncated
  resource: { deployment.environment.name: prod }
  sinks:
    - { name: collector, kind: otlp, url: https://otel.internal:4318/v1/logs, tenants: ["*"], server: true,
        allow: [{ cidr: 10.0.0.0/8, ports: [4318] }], headers_file: /run/secrets/otel-headers }
    - { name: stdout, kind: jsonl, path: "-", tenants: ["*"] }
statistics:
  retention: 26280h           # 3 years
telemetry:                    # phase 2b; the endpoint is OTEL_EXPORTER_OTLP_(METRICS_)ENDPOINT
  metrics: { tenants: ["*"], labels: [platform], max_series: 10000 }
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

Migration 0011 (2a): `download_counts` (primary key: all but `count`; index (tenant_id, day)),
`download_installers` (primary key: all but `installers`; index (tenant_id, day)),
`statistics_days(day, done_at)`. Filtered indexes exist on all three engines (as `ux_releases_seq`);
`&` on integers too.

### Package layout

```text
internal/audit     kinds and their fields, Tx.Event's helpers; writer/ the asynchronous writer,
                   buffer/ the buffer's retention, sinks/ the registry, otlp and jsonl
internal/app       the sinks from config, server.start
internal/egress    + Client.Post
internal/store     + events, sinks, download counts and installers
internal/stats     the download counter, the install deduplication, the daily installers pass (2a)
internal/telemetry OpenTelemetry metrics from the environment, the instruments (2b)
internal/tenants, keys, release, upstream, serve   + events
internal/api, cmd/kista   + reading; internal/api + statistics (2a)
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
- **Egress**: sinks use egress's client with an explicit allowlist; no redirects, no environment
  proxies or `OTEL_*` settings. Metrics go where the operator's standard OTel environment says, as
  the platform's other services, and carry no principal, no address and no unlisted tenant.
- **Privacy**: principals, display names and client addresses live in events only: 30 days in kista
  (addresses reduced by default), then in the organisation's pipeline under its retention and
  erasure. Counts, installers and metrics carry no principal and no address. Events are tenant data,
  read by `audit` holders and server administrators; a server administrator's identity shows to a
  tenant's readers as `server`. Display names are reduced to printable characters.
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
- **Phase 2a**: one download per `INSTALL` and one install event per authenticated one (e2e, the
  built-in client and httpfs); what counts (`200`, a range from 0; not `HEAD`, `304`, later ranges);
  counts on three engines; installers; statistics and who reads them; the DuckDB routes' refusals.
- **Phase 2b**: metrics with no principal and no unlisted tenant, the optional labels, the gauges;
  setup from the environment (no endpoint: nothing; `grpc` refused; an export reaches a fake
  endpoint as `application/x-protobuf`).

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
