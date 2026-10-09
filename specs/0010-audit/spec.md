# Spec 0010: Audit

- **Status**: draft
- **Date**: 2026-10-09
- **Author**: vgsml, Claude

## Summary

kista records what happens to a tenant as **events**: every management, release and upstream change,
refusals and failed authentications, reads of the log itself, and (phase 2) authenticated installs.
Events are kept in a per-tenant **hash chain** whose head can be **signed** with a key the
database's administrators do not hold, so an edit of the stored log is detectable; they are read by
holders of the `audit` verb, exported with their hashes and verified offline, kept for a tenant's
retention (with legal holds), counted as per-tenant **statistics**, and sent to **sinks**. Personal
data in events (principals, client addresses) is pseudonymised in the chain, so an erasure request
can be honoured without breaking it.

Delivery is phased, each phase a pull request:

- **1a. The log**: the events table with pseudonymised personal data, the canonical form and the
  chain, `Tx.Event`; events of tenants, channels, keys, issuers, grants, audiences, publishers and
  versions; authentication failures and refusals on management routes; `server.start`; the `audit`
  verb; reading, export and verification (API and CLI).
- **1b. The rest**: events of releases, publication, promotion, blocks, upstreams and shadows;
  signed heads; retention, anchors and legal holds; erasure.
- **2. The serve path**: installs, anonymous download counts, token failures on the DuckDB routes,
  pull-through; statistics.
- **3. Sinks**: OpenTelemetry (OTLP/HTTP) and a JSON-lines file.

## Problem

- An administrator's actions are trusted, but an organisation must be able to tell **who did what,
  when, from where**: who granted install on `prod`, who promoted `acl 1.2`, who blocked a body, who
  added an upstream, who removed a tenant shadow. Today only key state changes are recorded
  (`key_events`, spec 0003); the rest is in process logs, which rotate and are not per tenant.
- **Tamper evidence**: anyone with the database's credentials can edit rows. The record must make an
  edit detectable, against evidence the database's administrators cannot rewrite (spec 0001).
- **Separation of duties**: an auditor reads the log without being able to change what it records.
- **Installs**: which nodes and people installed which versions (licence compliance; "who installed
  the version we yanked?").
- **Privacy**: events hold IdP subject ids and client addresses; erasure requests must be possible.
- **Statistics and sinks**: installs per extension, version, channel and principal, per tenant
  (spec 0001; global statistics are Enterest's); central log collection.

## Design

### Events

| Field | Meaning |
| --- | --- |
| `id` | a UUIDv7, made inside the writing transaction (a retried transaction makes a new one) |
| `v` | the catalogue's version, 1 |
| `tenant_id` | the tenant; server-level events (on no tenant: DuckDB versions, `tenant.create`, `server.start`) use the server chain's fixed id `00000000-0000-0000-0000-000000000000`, which is never a tenant's. A server administrator's action on a tenant goes into that tenant's chain |
| `at` | when it happened: the writing process's clock (`Tx.Now`), UTC, truncated to microseconds before it is stored (every engine keeps them); not monotonic across replicas: `seq` is the order |
| `kind` | from the catalogue (below) |
| `outcome` | `ok`; `refused` (authorization or a rule); `failed` (an error after the decision, written asynchronously: a change that fails rolls its event back with it) |
| `actor` | the actor, as the code writes it (`principal:<tenant>/<issuer id>\|sub:<sub>` or `\|client:<id>`, `server:<issuer>\|sub:…`, `os:<uid>:<user>`, `publisher:<tenant>/<names>`, `system:…`, `anonymous`), with its personal part replaced by a ref (below) |
| `subject` | the resource as a path: `channel:prod`, `channel:prod/ext:acl/release:<id>`, `grant:<id>`, `upstream:core`, … |
| `data` | a JSON object of the event's facts, built from the catalogue's fields for the kind (never a request body as a whole): changed fields with old and new values, a release's version and body hash, provenance, … Personal values in it (a grant's principal, an issuer's subject) appear as refs. At most 64 KiB; a larger one keeps its first fields and `"truncated": true` (a change never fails for its event) |
| `client` | a ref of the request's client address (after spec 0006's trusted proxies and the reduction below); none for the CLI and background work |
| `request` | the request's id: `X-Request-Id` when the caller sends one matching `[A-Za-z0-9._-]{1,64}`, else a new UUIDv7 (also logged with the request), for correlation; background work gets its own |
| `seq`, `prev_hash`, `hash`, `chained_at` | the chain (below) |

**Never in events**: tokens, API keys (their prefix only), signer references' credentials, upstream
or sink credentials, key material. The catalogue lists every field each kind may carry; a test fails
when a field outside it, or a field typed as secret, reaches `data`.

**The catalogue** is part of this spec's implementation as a documented list (`internal/audit/kinds.go`
and the API documentation): each kind, its subject and its `data` fields. Phase 1a: `tenant.create`,
`tenant.suspend`, `tenant.resume`, `channel.create`,
`channel.versions`, `version.add`, `version.c_apis`, `key.add`, `key.activate`, `key.retire`,
`key.resign`, `issuer.add`, `issuer.remove`, `audience.add`, `audience.remove`, `grant.add`,
`grant.remove`, `publisher.add`, `publisher.remove`, `publisher.github.add`,
`publisher.github.remove`, `publisher.key.add`, `publisher.key.remove`, `auth.failure`,
`authz.refused`, `audit.read`, `audit.export`, `audit.dropped`, `server.start`.
Phase 1b: `tenant.event_retention`, `tenant.event_hold`, `audit.prune`, `audit.erase`, `release.add`, `release.publish`, `release.promote`, `release.yank`, `release.deprecate`,
`release.activate`, `release.current`, `release.public`, `release.private`, `block.add`,
`block.remove`, `upstream.add`, `upstream.remove`, `upstream.change`, `upstream.release`,
`upstream.rejected`, `upstream.run`, `shadow.add`, `shadow.remove`. Phase 2: `install`,
`auth.failure` on DuckDB routes, `upstream.pull`. Phase 3: `audit.sink`. The catalogue marks each
`data` field as plain, personal (stored as a ref) or forbidden.

### Where events are written

- **Changes**: in the operation's **decisive transaction** (the one whose commit makes the change),
  through `Tx.Event`: the change and its event commit or roll back together. Follow-up work in other
  transactions (a block's sweep of channels, version bumps, `FindOrInsertBuild`) writes none; its
  results are in the decisive event's `data` or in later events of their own kind. Not events:
  leases, `TouchAPIKey`, re-sign batches (one `key.resign` when a re-sign completes), cell rows.
- **Upstreams**: an intake release is `upstream.release` in the ingest transaction; a rejected cell is
  `upstream.rejected` (asynchronous) when its outcome changes from the cell's last; each run ends with
  one `upstream.run` summary (counts and problems, as `last_run`).
- **Refusals and failures**, asynchronously: authentication failures on management routes
  (`auth.failure`: the reason class, never the token; per tenant, or the server chain when the
  tenant is unknown), and `authz.refused` (actor, verb, resource) recorded where a request is finally
  refused: `api.decide`'s refusals before a service is called, and a service's final denial (a
  service that tries several verbs records only when all are denied), never per verb tried; and
  `failed` outcomes.
- **Reads of the log**: `audit.read` (a listing, a head or a verification; at most one per actor,
  tenant and hour on a replica) and `audit.export` (always), in the chain read: a tenant's, or the
  server's; a server administrator reading a tenant's log is recorded in the tenant's chain.
- **`server.start`**: at boot, in the server chain: kista's version, the schema level, and a digest
  of the security-relevant configuration (server issuers and admins, egress, signers' sources, sinks,
  `events:`) with the names of the settings that changed since the previous start.
- The actor and the client reach `Tx.Event` through the request's context (the client key moves from
  `internal/api` to `internal/audit`); the CLI's events carry the OS actor and no client.

**The asynchronous writer** queues events in memory per tenant (at most 1,000 waiting per tenant,
10,000 in all) and inserts them in batches (every second, or 500 events). A tenant over its quota
drops its own events only; drops are counted per tenant and written directly into that tenant's
chain as `audit.dropped` (with the counts by kind) at least once a minute while dropping. Refusal and
failure events are rate-limited per (tenant, actor, kind) to one a second with a count, in a bounded
map per tenant. Limits are per replica. Events written this way can be lost in a crash; changes'
events never are.

### Pseudonymisation and erasure

Each tenant (and the server chain) has a random 32-byte **event key** (`event_keys`, created with the
tenant). A **personal value** is: the `<sub>` or `<id>` of a principal or server actor's `sub:`/`client:`
part, the OS user name of an `os:` actor, a client address (after its reduction), and every `data`
field the catalogue marks personal (a grant's principal value, an issuer record's subject claims).
Publisher names, issuer ids and tenants are not personal.

A personal value is never written where the chain covers it. In its place goes its **ref**,
`ref:` and the lowercase hex of `HMAC-SHA-256(event key, value)`: in `actor` (for example
`principal:<tenant>/<issuer>|sub:ref:9f…`), in `client`, and in `data`. The clear value is kept in
`event_personal(event_id, field, ref, value)`, outside the chain, and joined back on reading.

- **Online verification** also checks `HMAC(event key, value) == ref` for every clear value that is
  not erased, so a changed clear value is detected; an offline export carries clear values but not
  the event key, so offline verification checks the chain over refs only.
- **Erasure** (phase 1b; `kista admin events erase <tenant> -value <value>` or `-client <address>`,
  server administrator; the address is reduced first, as at write time) computes the ref and replaces
  the matching clear values with `[erased]`; it records `audit.erase` with the ref, never the value.
  The chain still verifies.
- **Reduction**: `events.client_addresses: full | truncated | none` (default `truncated`: /24 and
  /48) applies before the ref is made.
- Refs of low-entropy values (addresses) can be recomputed by whoever holds the event key: refs are
  pseudonymous, not anonymous.

### The chain

A background **chainer** on every replica, holding the lease `kista/events/chainer` (one replica
chains at a time; leases expire, so two may briefly overlap and the unique `seq` index aborts one),
finds tenants with unchained events (`DISTINCT tenant_id` over the index of unchained events). Per
tenant, one transaction under the lock `kista/events/<tenant_id>` reads the head, takes up to 500
unchained events in `(at, id)` order, and compare-and-set (`WHERE seq IS NULL`) gives each the next
`seq`, its `prev_hash` (the previous event's `hash`; 32 zero bytes for the first), `chained_at`, and

```text
hash      = SHA-256(prev_hash ‖ canonical(event))
canonical = "kista-event-v1" ‖ uuid(tenant_id) ‖ u64(seq) ‖ uuid(id) ‖ u16(v) ‖ i64(at)
            ‖ text(kind) ‖ text(outcome) ‖ text(actor) ‖ text(subject) ‖ text(data)
            ‖ opt(client) ‖ text(request)
uuid(x)   = the 16 bytes of the UUID
i64(at)   = microseconds since 1970-01-01 UTC
text(x)   = u32 length ‖ the UTF-8 bytes (data: exactly as kista wrote it)
opt(x)    = 0x00 when absent | 0x01 ‖ text(x)          (client is the only optional field)
integers  = big-endian
```

`actor`, `client` and `data` are hashed with their refs (above). A transaction that commits with an
earlier `at` than events already chained gets a later `seq`: it is never lost or skipped, and `seq`
is the order. Every row is chained, whatever its content. On SQL Server the chainer's read waits for
inserting transactions to commit (they are short); on SQLite a batch holds the write lock briefly.

What the chain proves: once an event is chained, an edit, a deletion, an insertion or a reordering
is detectable from any later head the verifier holds **outside the database** (a signed head counts:
its signature cannot be made without the head signer). It does not prove who wrote a row: a database
writer can insert rows the chainer then chains; and until an event is chained (seconds) it is
unprotected. `verify` and the head report unchained events older than a minute as an alarm.

**Heads** (signed heads from phase 1b). The chainer keeps each tenant's current head (`event_heads`:
`seq`, `hash`, the last event's `at`). When `events.head_signer` is configured (a key source
reference, spec 0004; recommended in production: a KMS or vault key the database's administrators
cannot use, never a channel's key: one whose fingerprint is a channel key's is refused), it signs a
head at most once a minute per tenant: the 32-byte digest
`SHA-256("kista-head-v1" ‖ uuid(tenant_id) ‖ u64(seq) ‖ hash ‖ i64(at))` is given to the signer as
a precomputed digest (as channel signatures, spec 0002), RSA-2048 PKCS#1 v1.5. Signed heads are kept
(`event_signed_heads`) within the tenant's retention, and each is also written to the process log
and to the sinks, so copies exist outside the database. **Rotation**: head signers are listed in
`event_head_keys` (fingerprint, public key, first and last use); the API serves the list; while two
are configured (`events.head_signer` and `events.previous_head_signer`), each signed head carries both
signatures, so a verifier that pins the old key learns and checks the new one.

### Retention, anchors and holds (phase 1b)

Each tenant has a retention (`tenants.event_retention`, default 400 days, 30 days to 10 years). A daily
pass (lease `kista/events/retention`) deletes a tenant's chained events older than its retention, in
batches under the tenant's chain lock, oldest first; each batch first writes an `audit.prune` event
(actor `system:retention`) naming the range and the new **anchor** (the last deleted event's `seq` and
`hash`), then records the anchor (`event_anchors`) and deletes. Verification accepts an anchor only if
an `audit.prune` event later in the chain names exactly it, its actor is `system:retention`, its range
is older than the retention in effect then (the chain's `tenant.event_retention` events), and no hold
was in effect (the chain's `tenant.event_hold` events). A retention decrease takes effect 7 days after
it is set (`tenant.event_retention` records both values and the date). A **legal hold**
(`tenants.event_hold`, server administrator) suspends pruning; setting and lifting it are events.
Signed heads older than the retention are pruned with the events, except the newest signed head at or
before the anchor. Server-level events use `events.server_retention` (config, default 400 days).

Tenant removal does not exist yet; when it does, it exports the tenant's log and records the
tenant's final head in the server chain before anything is removed.

### Reading, export and verification

```text
GET  /api/v1/tenants/{t}/events[?since=&until=&kind=&actor=&subject=&cursor=&limit=&pending=true]
GET  /api/v1/tenants/{t}/events/head
GET  /api/v1/tenants/{t}/events/export[?since=<seq>]      JSON lines, streamed (audit.export)
GET  /api/v1/tenants/{t}/events/verify                    the stored chain verified (online checks)
GET  /api/v1/events[...], /head, /export, /verify         the server chain (server administrators)
GET  /api/v1/events/head-keys                             the head signers' public keys (public; 1b)
POST /api/v1/tenants/{t}/event-retention {days}           server administrator; If-Match: the tenant's ETag
POST /api/v1/tenants/{t}/event-hold, /event-release       server administrator; If-Match
```

- **Who reads**: the `audit` verb on the tenant (spec 0001's verb; `admin` implies it; a channel- or
  extension-scoped grant does not reach the tenant's log), or a server administrator. Missing and
  forbidden answer alike (spec 0007).
- **Listing** is keyset-paged by `seq` (`cursor` is the last `seq`), chained events only; `pending=true`
  lists unchained ones. Filters use indexed columns.
- `GET tenants/{t}` shows the retention, its pending decrease and the hold.

CLI:

- `kista admin events list|head|verify|export|erase [-server | <tenant>] …`, `kista admin events
  head-keys`, `kista admin tenant event-retention <t> <days>`, `kista admin tenant event-hold <t>
  on|off`;
- `kista events verify -file events.jsonl [-head-keys keys.pem] [-head <seq>:<hash>]` verifies an
  export **offline**, without configuration or database (a new top-level command, with `kista ext`).
  An export is JSON lines: its anchor (with the `audit.prune` event that names it, when it starts
  after one), the events in `seq` order with their hash fields, and the newest signed head. Without a
  trusted head or head key, the result says "internally consistent only"; with one, the export must
  contain the head's `seq` with that hash and the signature must verify.

### The serve path (phase 2)

- **Installs**: a `GET` of a binary answered `200`, or `206` for a range starting at byte 0, to a
  caller with a valid token. The first per (principal, release, client, day) is an `install` event;
  every one counts in `install_counts(tenant, channel, name, ext_version, platform, principal_ref,
  day, count)`. An e2e test checks that one `INSTALL`, with and without httpfs loaded, makes one event.
- **Anonymous public downloads**: `download_counts(tenant, channel, name, ext_version, platform,
  day, count)`.
- Counts are upserted every minute by the asynchronous writer (`ON CONFLICT` on PostgreSQL and
  SQLite, update-then-insert under a lock on SQL Server) and kept for `events.stats_retention`
  (default 3 years).
- **Token failures** on DuckDB routes (spec 0006's failure log) become `auth.failure` events,
  rate-limited per tenant and reason; requests for a blocked or yanked body by an authenticated caller
  count per (principal, body hash, day).
- **Pull-through** fetches started by a miss are `upstream.pull` events.
- **Statistics**: `GET /api/v1/tenants/{t}/stats/installs?from=&to=&group=extension|version|channel|principal`
  (`audit` verb) from the counts, by day.

### Sinks (phase 3)

`events.sinks` (config, file-only): OTLP/HTTP log endpoints (reached through an explicit egress
allowlist, so an internal collector works) and JSON-lines files (or stdout), each with the tenants it
takes (none by default) and credentials from a key source (never in events or errors). The chainer
hands each chained batch to the sinks with the head; a sink keeps its position per tenant
(`event_sink_positions`), retries with backoff, never blocks chaining, and delivers at least once (the
event `id` deduplicates). Sink lag and failures are logged and, once an hour while they last, an
`audit.sink` server event. SIEMs, syslog and cloud log services are reached through an OpenTelemetry
collector's exporters; per-tenant sinks configured over the API are a follow-up.

### Data model

Migration 0010 (phase 1a; `-- +min_reader 10`: an older replica would change things without events):

```text
events          id, tenant_id (no FK), at, v, kind, outcome, actor, subject, data (text), client null,
                request, seq null, prev_hash null, hash null, chained_at null
                unique (tenant_id, seq) where seq is not null; index (tenant_id, at, id) where seq is null;
                index (tenant_id, kind, at); index (tenant_id, actor, at)
event_personal  event_id, field, ref, value            primary key (event_id, field); index (ref)
event_keys      tenant_id, key, created_at
event_heads     tenant_id, seq, hash, at               primary key (tenant_id)
```

Migration 0011 (phase 1b): `event_signed_heads` (tenant_id, seq, hash, at, fingerprint, signature),
`event_head_keys`, `event_anchors`, `tenants` + `event_retention`, `event_retention_next`,
`event_retention_next_at`, `event_hold`. Migration 0012 (phase 2): `install_counts`,
`download_counts`. Migration 0013 (phase 3): `event_sink_positions`. Filtered indexes exist on all
three engines (as `ux_releases_seq`).

### Package layout

```text
internal/audit      kinds and their fields, Event, canonical form, the asynchronous writer, the
                    chainer, heads, verification, export, pseudonyms, retention
internal/store      + events, keys, heads, anchors, counts; Tx.Event
internal/tenants, keys, release, upstream   + an event in every decisive transaction
internal/api, cmd/kista   + reading, export, verification, retention, holds, erasure
```

A registry of the services' write operations, each with its kind (or "phase 1b" for those 1a does
not cover yet), backs a test that fails when an operation emits no event or the wrong one.

### Changes to earlier specs

- **0006/0007**: grants take the verb `audit` (read a tenant's events and statistics; implied by
  `admin` on the tenant; refused on a channel or an extension, and on an issuer-wide grant, as
  `admin` is); `api.decide`'s
  refusals and failures become events; the request id is logged with every request.
- **0003/0007**: `key_events` and `GET …/keys/events` stay as a key's state history; key changes also
  write events. Tenants gain the retention and hold fields.
- **0008/0009**: every change writes its event in its decisive transaction; upstream runs write
  `upstream.run` summaries.

## Security

- **Tamper evidence**, not tamper proofing: a database writer can still change the log; the chain
  makes it detectable against a signed head (whose key the database's administrators do not hold) or
  a head recorded outside. Anchors are accepted only when the chain names them; a retention decrease
  waits 7 days; holds stop pruning.
- **No secrets** in events: the catalogue's field allowlist, and a test.
- **Privacy**: principals and client addresses are pseudonymised in the chain and erasable in clear;
  client addresses are reduced at write time; events are tenant data read by `audit` holders and
  server administrators, whose reads are themselves recorded.
- **Isolation**: a tenant's events, keys, heads and statistics are its own; one tenant's volume
  cannot drop another's events (per-tenant quotas).
- **Availability**: a change's event costs one insert in its transaction; the serve path never waits
  on the log.

## Testing

- **Store**: migration 0010 on three dialects; `Tx.Event` rolls back with its change; the filtered
  indexes.
- **Canonical form and chain**: the same events give the same hashes on SQLite, PostgreSQL and SQL
  Server; two replicas' chainers chain in one order without gaps; a late commit with an earlier `at`
  is chained, not skipped; verification detects an edited, deleted, inserted and reordered event, a
  rewritten tail against a signed head, a forged anchor, a `NULL`/empty substitution.
- **Coverage**: every registered write operation emits exactly one event of its kind; no secret
  field reaches `data`; no personal value reaches a hashed field.
- **Refusals**: a service trying several verbs records one refusal, only when all are denied.
- **Pseudonyms and erasure**: an edited clear value fails online verification; erased events keep
  verifying; refs match the erased value.
- **Head signers**: a signed head verifies with the served key; a rotation's double signature; a
  channel key refused as head signer; truncation after a signed head copied to the log is detected.
- **Retention**: pruning writes `audit.prune` and keeps the chain verifiable; a decrease waits; a hold
  stops it.
- **API and CLI**: who reads (`audit`, admin, scoped grants, server administrators), reads recorded,
  paging, export and offline verification with and without a head.
- **Phase 2**: one install event per `INSTALL` (e2e, with and without httpfs); counts; failures
  rate-limited; statistics; per-tenant quotas.
- **Phase 3**: a fake OTLP endpoint and a file receive every event at least once, in order, across
  restarts.

## Alternatives considered

- **Events written asynchronously only.** A crash loses the record of a committed change.
- **The hash computed in the change's transaction.** It serializes every change of a tenant on the
  head; the chainer chains after commit.
- **One chain for the server.** Tenants' logs must be exportable separately, and volumes isolated.
- **Clear personal data in the chain.** Erasure would break verification.
- **Unsigned heads only.** The head would come from the database it protects.

## Follow-ups

- RFC 3161 timestamps of signed heads; per-tenant sinks over the API; webhooks.
- Statistics across tenants (Enterest); the console's views (spec 0015).
- Tenant removal (export and final head first).
