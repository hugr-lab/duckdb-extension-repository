---
title: Observability
---

# Observability

kista reports what happens in four ways:

- **events**: an audit record of every change, refusal and authenticated download, kept 30 days in
  the database and delivered to the organisation's log pipeline through **sinks**;
- **download statistics**: daily counts per extension, version, platform, DuckDB version and
  channel, kept for years;
- **OpenTelemetry metrics**, exported from the standard `OTEL_*` environment;
- **process logs** (JSON on stdout) and **health endpoints**.

kista is not a log store: long-term retention, search and tamper evidence belong to the pipeline
that receives the events. The design is in
[spec 0010](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0010-audit/spec.md).

## Events

Each event has these fields:

| Field | Meaning |
| --- | --- |
| `id` | a UUIDv7 |
| `tenant_id` | the tenant; server-level events use `00000000-0000-0000-0000-000000000000` (in sinks; the API reads one tenant's events and leaves it out) |
| `at` | when it happened (UTC) |
| `kind`, `v` | the kind from the catalogue below, and the catalogue's version (1) |
| `outcome` | `ok`, `refused` (a `401`, or a `404` that stands for forbidden) or `failed` (a `5xx`, a storage pass that failed or found damage, a failing sink) |
| `actor` | who: `principal:<tenant>/<issuer id>\|sub:<sub>`, `server:<issuer>\|sub:…`, `license-manager:<issuer>\|sub:…` (a licence manager), `distribution-admin:<issuer>\|sub:…` (a distribution admin), `os:<uid>:<user>` (the CLI), `publisher:<tenant>/<names>`, `system:<what>`, `anonymous` |
| `actor_name` | a token's `name`, `preferred_username` or `email` claim, for display |
| `subject` | the resource as a path, such as `channel:prod/ext:acl/release:<id>` |
| `data` | the kind's fields only, at most 16 KiB (a larger one is cut and marked `"truncated": true`) |
| `client` | the client address after `serve.trusted_proxies`, reduced by `events.client_addresses` |
| `request` | the request id, logged with the request as `request_id` |

Changes write their event in the same database transaction as the change, so a change and its event
commit or roll back together. Refusals, authentication failures, failures and install events go
through an asynchronous writer and can be lost in a crash.

Events never carry tokens, signed requests, signer references, credentials or private key
material. A failure carries an error class, never an error's text: for `storage.gc` and
`storage.verify`, `error` is `lease`, `implausible`, `domain`, `interrupted` or `error`, and the text
is in the server's log or the command's stderr. Validation errors and stale `If-Match` (`400`, `409`, `412`, `428`) are not events:
nothing happened.

To a tenant's readers, and to sinks unless `unmasked: true`, a server administrator's or the CLI's
actor on a tenant's event shows as `server`, a licence manager's as `license-manager` and a
distribution admin's as `distribution-admin`, without name or address.

### Event kinds

The catalogue is served at `GET /api/v1/event-kinds` with each kind's subject form and fields. These
are all the kinds in `internal/audit`:

| Group | Kind | Subject | Data fields |
| --- | --- | --- | --- |
| Tenants and server | `tenant.create` | `tenant:<name>` | `name`, `display_name`, `storage_domain`, `holder_id` (when set) |
| | `tenant.holder` | `tenant:<name>` | `old`, `new` (the expected holder; empty: none) |
| | `tenant.limits` | `tenant:<name>` | for each limit changed (`bytes`, `channels`, `upstreams`, `publishers`): `old` and `new` (the stored value: a number, `unlimited` or `default`) and `limit` (the value in force; `null` for unlimited bytes); a tenant created with limits has it in the server's events, beside `tenant.create` (spec 0038) |
| | `tenant.suspend`, `tenant.resume` | `tenant:<name>` | none |
| | `tenant.read` | `tenant:<name>` | `state`: a distribution admin read the tenant's state and expected holder (spec 0037), written for a suspended tenant too |
| | `tenant.delete` | `tenant:<name>` | `channels` (a count), `suspended_at`, `keys` (the fingerprints) |
| | `tenant.purge` | `tenant:<name>` | `rows`, `passes`, `late`, `error` |
| | `version.add` | `version:<name>` | `version`, `kind`, `c_apis` |
| | `version.c_apis` | `version:<name>` | `version`, `c_api` |
| | `reserved.add` | `version:<name>` (a version's core extensions), `reserved:<name>` (an administrator's) or `sync:community` (the community sync) | `kind`, `names`, `source`; a version's: `version`; forced over tenants' releases: `forced`, `tenants` |
| | `reserved.remove` | `reserved:<name>` | `name`, `sources` |
| | `reserved.sync` | `sync:community` | `error` (a failed community sync pass: `fetch`, `too_many`, `error`) |
| | `server.start` | `server` | `version`, `schema`, `digests`, `changed` |
| | `signer.source.weak` | `source:<name>` | `source`, `kind`, `protection` |
| Channels and keys | `channel.create` | `channel:<channel>` | `name`, `kind` |
| | `channel.versions` | `channel:<channel>` | `added`, `removed` |
| | `key.add` | `channel:<channel>/key:<id>` | `key`, `fingerprint`, `scheme`, `state`, `protection` |
| | `key.activate`, `key.retire` | `channel:<channel>/key:<id>` | `key`, `fingerprint`, `from`, `forced` |
| | `key.resign` | `channel:<channel>/key:<id>` | `key`, `fingerprint`, `signatures` |
| Identity | `issuer.add` | `issuer:<name>` | `name`, `url`, `algorithms` |
| | `issuer.remove` | `issuer:<name>` | `name`, `url` |
| | `issuer.console.set`, `issuer.console.remove` | `issuer:<name>` | `issuer`, `client_id`, `audience` |
| | `audience.add`, `audience.remove` | `tenant:<name>` | `audience` |
| | `grant.add`, `grant.remove` | `grant:<id>` | `principal`, `verbs`, `channel`, `extension`, `extension_group` |
| | `extension_group.add` | `extension-group:<name>` | `name`, `description` |
| | `extension_group.remove` | `extension-group:<name>` | `name`, `description`, `members` |
| | `extension_group.change` | `extension-group:<name>` | `description` |
| | `extension_group.members` | `extension-group:<name>` | `added`, `removed` |
| | `publisher.add`, `publisher.remove` | `publisher:<name>` | `name`, `id` (the publisher's, spec 0037); added, `distributor` (a distributor's publisher, spec 0033); removed, `purged` and `kept` (the held entries it purged, how many, and those it kept, revoked, each with its `channels`, spec 0033) |
| | `publisher.change` | `publisher:<name>` | `name`, `license_keys` (marked or unmarked as a source of licence keys, spec 0021), or `distributor` and `before` (marked a distributor's publisher, spec 0033) |
| | `publisher.key.add` | `publisher:<name>/key:<id>` | `publisher`, `key`, `fingerprint`, `type`, `title`, `expires_at` (a public SSH key, spec 0023) |
| | `publisher.key.remove` | `publisher:<name>/key:<id>` | `publisher`, `key`, `fingerprint`, `type`, `title`, `purge_held`; with it, `purged` and `kept` (as `publisher.remove`) |
| Licences | `license.refused` | `license:<name>` | `name`, `license`, `issuer`, `rule` (`older`), `source` (the entry named), `count`: an import refused as older than a licence the tenant holds (spec 0037), rate-limited as a refusal |
| | `license.import` | `license:<name>` | `name`, `license`, `issuer`, `holder`, `plan`, `expires_at`, `replaced`, `source` (the licence's catalogue entry, by its source, spec 0035) |
| | `license.change` | `license:<name>` | `name`, `license`, `visibility`, `source` |
| | `license.remove` | `license:<name>` | `name`, `license`, `reason` (`admin`, `revoked`, `expired`, `upstream removed`), `source` |
| | `license.sync` | `upstream:<name>` | `upstream`, `counts`, `error`, `refused` (the system's relay run into its upstream's own entry) |
| | `license.public_warning` | `channel:<channel>/ext:<name>` | `name`, `source` (the entry), `channel`, `act` (the kind of the act's event: `release.public`, `release.place`, `release.promote`, `source.set`, `upstream.release`, `license.import`, `license.change`, `license.sync`, `bundle.import`, …): an entry holding a licence served publicly (its build not withdrawn, its body not blocked), advisory and not complete, once per act, entry, name and channel; written right after the act commits, best effort (a failure is logged, the act stands), by the act's actor; it names the channel for auditors, whatever the caller was answered ([Licensing](licensing.md#public-serving-of-licensed-entries), spec 0035 phase 2) |
| Releases | `release.add` | `channel:<channel>/ext:<name>/release:<id>` | `release`, `name`, `version`, `platform`, `slot`, `body_hash`, `visibility`, `current`, `shadows` |
| | `release.publish` | the release | as `release.add`, and `provenance` |
| | `release.promote` | `channel:<channel>/ext:<name>` | `releases`, `name`, `from_channel`, `version`, `shadows`, `provenance` |
| | `catalogue.arrival` | `ext:<name>` | `build`, `name`, `version`, `platform`, `slot`, `body_hash`, `source`, `visibility`, `available_at` (held until then), `provenance`: a build published into the catalogue only (spec 0033) |
| | `catalogue.purge` | `ext:<name>` | `build`, `name`, `version`, `platform`, `slot`, `body_hash`, `reason` and `key` (a held entry purged with its key, `publisher.key.remove`, or its publisher, `publisher.remove`): an entry purged from the catalogue |
| | `catalogue.hold` | `upstream:<name>`, `publisher:<name>`, `channel:<channel>` or `tenant:<name>` | `source` or `channel`, `hold`, `before`, `held` (the held entries a raise held longer): a hold set (spec 0033); with `tenant:<name>` and `source` `distributors`, the tenant's minimum hold for distributors' publishers (spec 0037), `held` counting every distributor's publisher's entries a raise held longer. A distributor alerts on a publisher's hold a distribution admin set, and on `publisher.key.add` by one |
| | `catalogue.withdrawn` | `ext:<name>` | `source`, `name`, `version`, `reason`, `builds`, `channels` (how many of the tenant's channels it bumped), `package_type`: a distributor's version withdrawn, served nowhere (spec 0033) |
| | `catalogue.meta` | `ext:<name>` | `source`, `name`, `summary` (the new text, `""` when cleared), `icon` and `icon_before` (the icons' hashes, `""` for none), `key` (a signed write's key fingerprint), `package_type`: an entry's [icon and summary](catalogue.md#give-an-entry-an-icon-and-a-summary) written; the same metadata again writes none (spec 0036). A write refused as `stale`, or failed after its icon was uploaded, is the same kind with the outcome `refused` or `failed`, a `reason` and a `count` (the refusals coalesced). A distribution admin's write shows under the generic `distribution-admin` actor in sinks unless unmasked; alert on one by a distribution admin or a newly added key |
| | `follow.set`, `follow.clear` | `channel:<channel>` | `source`: a channel follows a whole source, or no longer does (spec 0033) |
| | `release.place` | `channel:<channel>/ext:<name>` | `releases`, `name`, `version`, `source`, `placed` (`hand`, or the rule's mode `auto` or `manual`, or `home`, a mirror's held version in its own channel: the placement pass, actor `system:placement`), `early` (a held version placed early by hand) (spec 0033) |
| | `release.yank`, `release.deprecate`, `release.activate`, `release.current`, `release.public`, `release.private` | the release | `release`, `name`, `version`, `platform` |
| | `release.purge` | the release | `release`, `name`, `version`, `platform`, `slot`, `body_hash` |
| | `block.add` | `block:<body hash>` | `body_hash`, `reason` |
| | `block.remove` | `block:<body hash>` | `body_hash` |
| Upstreams | `upstream.add` | `upstream:<name>` | `name`, `kind`, `prefix`, `channel`, `mode`, `fetch`, `visibility`, `keys`, `platforms`, `extensions`, `credential` |
| | `upstream.remove` | `upstream:<name>` | `name` |
| | `upstream.change` | `upstream:<name>` | `name`, `change`, `value`, `versions`, `allow_reserved`, `eager`, `visibility`, `state` |
| | `upstream.release` | `channel:<channel>/ext:<name>/release:<id>` | `upstream`, `release`, `name`, `version`, `platform`, `slot`, `body_hash`, `url`, `key`, `bundle` (an import's manifest), `trigger` (`run`; `sync`, a bundle export's `-sync` too; or `ask`: a caller's miss, pull-through or a lazy mirror's first request; none on a bundle import) |
| | `upstream.rejected` | `upstream:<name>` | `upstream`, `duckdb_version`, `platform`, `name`, `outcome` |
| | `upstream.run` | `upstream:<name>` | `upstream`, `dry_run`, `counts`, `error`, `bundle` (an import's manifest) |
| | `bundle.import` | `tenant:<name>` | `import`, `manifest`, `created_at`, `exporter`, `bundle_key`, `input_sha256`, `dry_run`, `state`, `targets` (counts per upstream, and per feed as `feed:<name>`), `licenses` (when the bundle carried licences: `counts` received, stored, replaced, unchanged, refused; `refused`, the first 20 with `name`, `id`, `rule`, `reason`; `error`) |
| | `bundle.upload`, `bundle.upload_complete`, `bundle.upload_abort`, `bundle.upload_fail`, `bundle.upload_expire` | `tenant:<name>` | `upload`; created: `size`, `sha256`, `chunk_size`, `dry_run`, `upstreams`, `feeds`, `any_tenant`; complete: `retry`; fail (the system's): `import`, `error` (a class: `sha256`, `storage`, `interrupted`, `busy`, `another tenant`, `bundle key not pinned`, `refused`, `not found`, `malformed`, `failed`); expire (the collector's): `state` (the one it left) |
| | `bundle.reset` | `upstream:<name>` or `feed:<name>` | `upstream` or `feed`, `manifest`, `created_at` (the mark it cleared; a feed's cursors are reset too) |
| | `bundle.export` | `tenant:<name>` | `plan` (its SHA-256), `manifest`, `manifest_sha256`, `bundle_key` (none when unsigned), `counts` (per outcome, `licenses` carried, and `failed_sources`; per feed `feed_bodies`, `feed_statements`, `feed_withdrawals`, `feed_without_content`, `feed_documents`, and `failed_feeds`) |
| | `bootstrap.apply`, `bootstrap.accept`, `bootstrap.rearm` | `tenant:<name>` | `tenant`, `digest`; apply: `source` (the configuration's file, `env:NAME`, or empty), `version`, `host`, `created` (what was made, by kind; last, so a long list is cut first); accept: `previous`. `serve`'s apply has the actor `system:bootstrap`, and so do the events of the objects it makes |
| | `shadow.add`, `shadow.remove` | `shadow:<name>` | `name` |
| | `source.set` | `channel:<channel>/ext:<name>` | `name`, `source`, `before` (`""`: none), `arrival` (`true`: the first release of the name, or an upstream succeeding a removed one; `false`: an administrator's switch), `mode` (the rule's after an administrator's switch, spec 0033; `follow` when a follow took the name, actor `system:placement`, `arrival` true for a name without a rule); sources as `tenant`, `upstream:<name>`, `publisher:<name>` (spec 0031) |
| | `source.clear` | `channel:<channel>/ext:<name>` | `name`, `before` |
| | `release.source` | `channel:<channel>/ext:<name>/release:<id>` | `release`, `name`, `before`, `source`: written before slots had a source (spec 0033): the name's source brought a body the slot held from another source, and took its release |
| Attachments and feeds | `attachment.add` | `ext:<name>/attachment:<id>` | `body_hash`, `kind`, `origin`, `size` |
| | `attachment.change` | `ext:<name>/attachment:<id>` | `body_hash`, `kind`, `origin`, `change`, `statement` |
| | `attachment.remove` | `ext:<name>/attachment:<id>` | `body_hash`, `kind`, `origin`, `size`, `statement` |
| | `attestation_key.add` | `attestation-key:<id>` | `key`, `fingerprint`, `scheme`, `state` |
| | `attestation_key.activate`, `attestation_key.retire` | `attestation-key:<id>` | `key`, `fingerprint`, `from`, `forced` |
| | `feed.add` | `feed:<name>` | `name`, `url`, `credential`, `kinds`, `readers`, `offer`, `channels`, `keys`, `mode`, `export` |
| | `feed.change` | `feed:<name>` | `name`, `change` (with `export`, `cursors: reset` or `kept: dropped`) |
| | `feed.remove` | `feed:<name>` | `name`, `documents`, `export` |
| | `feed.run` | `feed:<name>` | `feed`, `counts`, `refused`, `bundle` and `dry_run` (an import's), `error` (a class: `unauthorized`, `refused`, `unreachable`, `credential`, `token`, `keys`, `limits`, `failed`) |
| Access | `auth.failure` | `route:<route>` | `reason`, `route`, `count` |
| | `authz.refused` | `route:<route>` | `route`, `verb`, `status`, `count`; for a tenant administrator's write on a distributor's licence entry (spec 0037), `name` (with `name_unverified` when read from an import's token before it was verified), `source` (the entry as named), `entry` (resolved, `upstream-id:<id>` or `publisher-id:<id>`) and `operation` (`import`, `remove`, `public`, `private`; refusals are coalesced per operation); for a distribution admin's `409` on a name or a key one of the tenant's publishers holds (spec 0037), `reason` (`publisher-taken`, `key-taken`; coalesced per reason) |
| | `install` | `channel:<channel>/ext:<name>/release:<id>` | `release`, `source` (the release's, as builds store it: `upstream:<id>`, `publisher:<id>`, `tenant`; spec 0036), `name`, `version`, `platform`, `duckdb_version`, `body_hash`, `user_agent` |
| | `extension.use` | `channel:<channel>/ext:<name>` | `version`, `platform`, `duckdb_version`, `license`, `status`, `events` |
| | `usage.forward` | `upstream:<name>` | `upstream`, `sent`, `skipped`, `failed` |
| | `usage.receive` | `channel:<channel>` | `day`, `part`, `parts`, `sender`, `reports`, `dropped` |
| | `request.failed` | `route:<route>` | `route`, `status`, `count` |
| Storage | `storage.gc` | `domain:<domain>` | `domain`, `builds`, `bodies`, `marked` (streams and icons), `streams`, `icons` (entries' icons deleted, spec 0036), `tmp`, `attachments`, `uploads`, `chunks` (bundle uploads' chunks), `bytes`, `claims`, `resolved`, `dry_run`, `error` |
| | `storage.verify` | `domain:<domain>` | `domain`, `full`, `mark`, `unmark`, `checked`, `ok`, `skipped`, `marked_ok`, `missing`, `short`, `long`, `corrupt`, `record`, `unreadable`, `marked`, `unmarked`, `bytes`, `error` |
| The log itself | `audit.dropped` | `server` | `counts`, `sink` |
| | `audit.sink` | `sink:<name>` | `sink`, `state`, `class`, `status` |

Notes on some kinds:

- `server.start` is recorded by every replica at start: kista's version, the schema level, a digest
  of each security-relevant configuration section (`profile`, `signers`, `blob`, `serve`, `egress`,
  `auth`, `publish`, `upstreams`, `events`, `statistics`, `telemetry`, `tenants`), and the sections changed
  since the previous start.
- `signer.source.weak` is recorded with every `server.start`, once per `kubesecret` key source: a
  source whose keys are only as safe as the cluster's Secrets
  ([Kubernetes Secrets](signing-keys.md#kubernetes-secrets)). `key.add` carries the key's
  `protection` tier (`file`, `kubesecret`, `software` or `kms`).
- `auth.failure` is recorded for a `401` given to a request that sent a credential; `reason` is
  `malformed`, `invalid`, `publisher_route`, `stale_token` or `unnamed_writer`, or for a publisher's
  signed request `signed_stale` (`signed-at` more than five minutes away), `signed_key` (not a key of
  the tenant, or expired) or `signed_signature`.
  `authz.refused` is a known caller refused. These two, `request.failed` and `license.refused`
  are rate-limited per tenant, actor and kind to one a second (an `authz.refused` that names its
  `operation` or its `reason`, spec 0037, per operation or reason too), each carrying `count`, the
  requests it stands for.
- `install` is an authenticated download by a principal holding `install` on it: the first per
  (principal, release, client network, day) on a replica, at most 1,000 a day per principal.
- `extension.use` is a usage report by a principal holding `install` on the extension
  ([Licensing](licensing.md#usage-reports)): the first per (principal, channel, extension, version,
  day) on a replica, with the same caps.
- `audit.dropped` counts events the asynchronous writer or the retention pass had to drop
  (`system:audit`, `system:retention`).
- **`package_type`** (spec 0034): the events that name a package without a channel, and a channel's
  creation, carry the package type when it is not the default (`duckdb_extension`; for a grant
  without a name, `*`): `channel.create`, `grant.add`, `grant.remove`, `extension_group.members`,
  `reserved.add`, `reserved.remove`,
  `catalogue.arrival`, `catalogue.purge`, `license.import`, `license.change`, `license.remove` and
  `license.public_warning`.
  Subjects are unchanged. A group's removal lists its members in one `extension_group.members`
  event per type.
- The background actors are `system:upstream:<tenant>/<name>`, `system:feed:<tenant>/<name>`,
  `system:resign`, `system:retention`, `system:serve`, `system:sinks`, `system:audit` and
  `system:gc` (every garbage collection pass, scheduled or `kista admin blob gc`),
  `system:tenant-delete` (the reaper of a deleted tenant), and `system:community-sync` (the
  community sync: `reserved.add` when it reserves names, `reserved.sync` with outcome `failed` and
  `error` `fetch`, `too_many` or `error` when a pass fails).
- `tenant.delete` and `tenant.purge` are the tenant's events, like `tenant.suspend`. `tenant.delete`
  is the server administrator's delete: its `keys` are fingerprints, never signer references.
  `tenant.purge` is written when the reaper writes the tombstone (`rows` deleted over `passes`);
  with outcome `failed` and `error` a class (`lease`, `interrupted` or `error`) on the first failed
  pass and whenever the class changes, not on every pass; and with `late: true` when rows found
  under a tombstone during the week after it were removed, which should never happen. Server
  administrators still read a deleted tenant's events, with `kista admin events list <tenant>`
  or the API, until `events.retention` removes them.

### Reading events

```bash
kista admin -config kista.yaml events list acme -kind release.yank -since 2026-10-01T00:00:00Z -limit 50
kista admin -config kista.yaml events list acme -follow -format jsonl
kista admin -config kista.yaml events list -server
```

| Flag | Meaning |
| --- | --- |
| `-server` | the server's own events instead of a tenant's |
| `-kind`, `-subject`, `-actor` | filters: `-kind` and `-actor` match exactly, `-subject` matches a prefix |
| `-since` | RFC 3339 |
| `-limit` | how many (default 100) |
| `-follow` | print new events as they arrive, oldest first |
| `-format` | `table` (default) or `jsonl` |

Over the API:

```text
GET /api/v1/tenants/{t}/events[?since=&until=&kind=&actor=&subject=&outcome=&order=asc|desc&cursor=&limit=]
GET /api/v1/tenants/{t}/events/{id}
GET /api/v1/events[…], GET /api/v1/events/{id}     the server's events (server administrators)
```

A tenant's events are read with the `audit` verb on the tenant (`admin` on the tenant implies it), or
by a server administrator. Events are newest first by default. See [Access](access.md) for granting
`audit`, and the console's **Events** section in [Console](console.md).

### The buffer

Events stay in the database for `events.retention`:

- default 30 days, at least 2 days, at most 365 days (90 when a sink is configured);
- an event not yet delivered to every sink that takes it is kept up to 7 more days;
- a tenant holds at most `events.max_rows_per_tenant` events (default 1,000,000); older ones go
  first.

An hourly pass deletes what is past these limits.

## Sinks

Sinks deliver events at least once, roughly in `at` order, to an OTLP/HTTP logs endpoint or to
JSON lines. Receivers should order by `at` and deduplicate by event id. A sink takes the tenants it
lists (`["*"]` for every tenant) and the server's own events only with `server: true`; by default a
sink takes nothing. At most 16 sinks.

```yaml title="kista.yaml"
events:                       # file-only
  retention: 720h             # 30 days
  max_rows_per_tenant: 1000000
  client_addresses: truncated # full | truncated | none
  resource:
    deployment.environment.name: prod
  sinks:
    - name: collector
      kind: otlp
      url: https://otel.internal:4318/v1/logs
      tenants: ["*"]
      server: true
      allow: [{ cidr: 10.0.0.0/8, ports: [4318] }]
      headers_file: /run/secrets/otel-headers
    - name: stdout
      kind: jsonl
      path: "-"
      tenants: ["*"]
```

| Setting | Meaning |
| --- | --- |
| `name` | `[a-z][a-z0-9-]{0,31}`, unique |
| `kind` | `otlp` or `jsonl` |
| `tenants` | tenant names, or `*` |
| `server` | also the server's events |
| `unmasked` | send server administrators', licence managers', distribution admins' and the CLI's identities on tenants' events as they are |
| `url` (otlp) | the collector's OTLP/HTTP logs URL; `https` (plain `http` to loopback only with profile `dev` and `egress.allow_loopback_http`) |
| `allow` (otlp) | addresses the sink may reach besides `egress.allow` (`cidr` and `ports`) |
| `headers_file` (otlp) | an absolute path to `Name: value` lines (`#` comments), for authentication headers. A secret file ([Files kista reads](security.md#files-kista-reads)) |
| `timeout` (otlp) | per request, 1s to 60s (default 10s) |
| `path` (jsonl) | `-` for stdout, or an absolute file path |
| `max_size`, `keep` (jsonl) | rotate at this size (default 100 MiB), keeping this many old files (0 to 100; 0 means 5) |

**otlp.** kista writes OTLP/HTTP with JSON encoding through its egress client: no redirects, no proxy
from the environment, addresses checked at every connection. It ignores the `OTEL_*` environment.
Each tenant's events are one `ResourceLogs` with `service.name=kista`, `service.version`,
`service.instance.id`, `kista.tenant.id`, `kista.tenant.name` and `events.resource`'s attributes.
Each event is a log record named `kista.<kind>`, severity INFO for `ok` and WARN otherwise, with the
event as its body and the attributes `kista.event.id`, `kista.kind`, `kista.outcome`, `kista.actor`,
`kista.subject`, `kista.request` and `client.address`. A batch the collector refuses with `400` or
`413` is split until the one refused event is found and dropped (counted in `audit.dropped`).

**jsonl.** One JSON object per line (the API's event JSON with `tenant_id` and `tenant`). A file is
created with mode 0600 and rotated by size. A file is written by whichever replica holds the sink's
lease: with several replicas use stdout or a shared volume.

A failing sink is retried with backoff (up to 5 minutes), never blocks writing or the other sinks,
and is reported as an `audit.sink` server event when it starts and stops failing. Running without a
sink is valid: the buffer is then the only record.

**Client addresses** in events are reduced by `events.client_addresses`: `truncated` (the default)
keeps IPv4 /24 and IPv6 /48, `full` keeps the address, `none` drops it.

## Download statistics

kista counts downloads of extension binaries:

- a `GET` answered `200`, or `206` for a range starting at byte 0, counts, whether or not the body
  completed;
- `HEAD`, `304` and a range not starting at byte 0 do not;
- a download from a passthrough channel counts, anonymous, in that channel's tenant.

Counts are kept per (channel, source, extension, version, platform, DuckDB version, day,
authenticated) with no principal and no address, and written to the database every minute. The
**source** is whose entry was downloaded: the release's (an upstream, a publisher, or the tenant;
a promotion keeps it), so a download is attributed to a distributor's entry exactly (spec 0036).
Once a day has ended, an hourly pass counts its distinct **installers** from that day's `install`
events, per release and its source (the event's `source`, else the release's; none when the release
is gone). Counts and installers are kept for `statistics.retention` (default 3 years; 30 days to 10
years).

```yaml title="kista.yaml"
statistics:
  retention: 26280h   # 3 years
```

Read them over the API:

```text
GET /api/v1/tenants/{t}/stats/downloads?from=&to=&channel=&extension=&group=<d>[,<d>]
    d: day | channel | source | extension | version | platform | duckdb_version
    → {from, to, group, rows: [{dimensions…, count, authenticated, installers}]}
GET /api/v1/tenants/{t}/stats/releases?channel=&extension=
    → {releases: [{channel, extension, version, platform, last_7_days, last_30_days, last_download_day}]}
```

- `from` and `to` are days (`YYYY-MM-DD`); the default is the 30 days up to today; at most 366 days.
- `group` defaults to `day`; `version` brings `extension` with it. `installers` is absent when
  grouping by `duckdb_version`. `source` is answered as a label (`upstream:<name>`,
  `publisher:<name>`, `tenant`; `publisher:(removed)` for a removed one), `unknown` for what was
  counted before kista recorded the source.
- A query over more than 100,000 stored rows is refused (`400`): narrow it.
- The statistics are of one [package type](concepts.md#packages-and-package-types), `?type=`
  (`duckdb_extension` by default): the rows of that type's channels; a `?channel=` of another type
  is refused (`400`).
- A principal with `audit` on the tenant reads everything; one with `admin`, `publish` or `promote`
  on extensions reads those names in the channels its grants cover; anything else answers `404`.
  Server administrators read everything. The routes need a fresh token, like management.

The console's **Statistics** section shows these. Counts are not evidence: anyone may download a
public release.

### A distributor's statistics

A distributor (such as a storefront publishing developers' extensions into many tenants) reads its
downloads per extension per week, summed over every tenant (spec 0036):

```text
GET /api/v1/distributor-stats/downloads?distributor=<name>&from=&to=[&type=]
    → {distributor, from, to,
       rows: [{week, extension, downloads, public, installer_days, withheld, min_tenants}]}
```

- **Who**: distribution admins and server administrators, with a fresh token. The distributor is a
  query parameter, as its publishers are marked (`publishers.distributor`). `?type=` is the package
  type (the default when not named).
- **Weekly snapshots.** The hourly daily pass computes each complete week once, two days after it
  ends (the counts are flushed and the days' installers counted), and stores it: one row per
  (distributor, package type, week, extension). It is never computed again, so an answered week
  never changes, whatever happens later (a late count, a tenant deleted, the counts pruned by
  `statistics.retention`, a setting changed). The route reads only these snapshots. On its first
  pass after the upgrade it computes every complete week the retention still holds whole, at most
  106 (once: later passes compute only the weeks after the newest one stored); snapshots older than
  106 weeks are deleted. A tenant deleted later keeps its share in the weeks already computed (up to 106 weeks).
- **What counts**: the downloads of the distributor's entries only, in every tenant: its publishers'
  (live, and removed ones remembered), as marked when the week is computed. A tenant's own build of
  the same name, or an upstream's, never counts. A suspended tenant counts; a tenant being deleted
  does not. A used publisher's distributor is never renamed (`409`).
- **Weeks** are complete ISO weeks in UTC, Monday to Sunday, named by their Monday. Only weeks
  computed are answered. `from` and `to` are any days, widened to whole weeks; `to` is clamped to
  the newest week computed, `from` to the oldest kept. The default is the 12 weeks up to the newest;
  at most 53 weeks. The answer's `from`, `to` and each row's `week` are Mondays. Before the first
  week is computed, and for `from` after the newest, the route answers `400` `not-computed`.
- **Grouping is fixed**: one row per (week, extension). No row names a tenant, a channel or a
  per-tenant figure.
- **`downloads`** counts as above; **`public`** is the part from public tenants; **`installer_days`**
  is the daily distinct installers summed over the days and the tenants (a principal installing on
  two days, or in two tenants, counts twice), omitted when left out; **`min_tenants`** is the
  minimum the week was computed with.

**Public tenants and the minimum.** Tenants listed by name in `statistics.public_tenants` (such as
a tenant serving free extensions to anyone) always count, with a token or without, and are outside
the minimum. Every other tenant's downloads count in a row only when at least
`statistics.distributor_min_tenants` such tenants (default 3, at least 2) downloaded there. With
fewer, that part is left out and the row is **`withheld`**: `downloads` equals `public`, and
`installer_days` is omitted. Installer-days take the minimum separately, by the tenants with
installers in the row: with fewer, `installer_days` is omitted even when the downloads are
answered. It is never answered in part. Both settings are applied when a week is computed; an empty
list exempts no one, and a name matching no tenant is logged at start.

```yaml title="kista.yaml"
statistics:
  distributor_min_tenants: 3
  public_tenants: [free]
```

**What the minimum protects, and what it does not.** It keeps a single customer's downloads from
being disclosed by accident in a row of their own. It does not protect against the server's
operator, who lists public tenants and reads the counts anyway, nor against a distributor that
controls several tenants (its own, or one organisation's several tenants): such tenants can make up
the minimum, so a row may reflect one organisation alone. And `withheld` itself says yes or no:
between one tenant and the minimum downloaded the extension that week, not which or how many times.
kista publishes no public statistics of its own.

Usage reports from licensed extensions' SDKs are counted the same way, with a budget of their own,
and read at `GET /api/v1/tenants/{t}/stats/usage`: see [Licensing](licensing.md#usage-reports).
`statistics.usage_accept: false` keeps none of them. Usage counts and users follow
`statistics.retention`.

## Metrics

kista exports OpenTelemetry metrics over OTLP/HTTP with protobuf, configured from the standard
environment. Nothing is exported unless an endpoint is set.

| Variable | Meaning |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | the collector; without either, no metrics |
| `OTEL_EXPORTER_OTLP_(METRICS_)HEADERS`, `…_TIMEOUT`, `…_CERTIFICATE`, `…_CLIENT_CERTIFICATE`, `…_CLIENT_KEY` | headers, timeout, a private CA, mutual TLS |
| `OTEL_EXPORTER_OTLP_(METRICS_)PROTOCOL` | must be `http/protobuf` if set; anything else refuses to start |
| `OTEL_METRICS_EXPORTER` | `otlp` or `none`; anything else refuses to start |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | over kista's defaults `service.name=kista`, `service.version`, `service.instance.id` |
| `OTEL_METRIC_EXPORT_INTERVAL` | export interval in milliseconds (default 60 s) |
| `OTEL_SDK_DISABLED` | `true` turns metrics off |

Export errors go to kista's log, at most once a minute.

| Metric | Type | Attributes |
| --- | --- | --- |
| `kista.downloads` | counter, `{download}` | `kista.authenticated`; for listed tenants `kista.tenant.id`, `kista.channel`, `kista.extension`, and the optional `kista.extension.version`, `kista.platform`, `kista.duckdb.version` |
| `http.server.request.duration` | histogram, seconds | `http.request.method` (`_OTHER` for unusual methods), `http.route`, `http.response.status_code` |
| `kista.downloads.active` | gauge, `{download}` | this replica's downloads in progress |
| `kista.uploads.active` | gauge, `{upload}` | this replica's uploads in progress |
| `kista.upstream.pull.queue` | gauge, `{miss}` | pull-through misses waiting on this replica |
| `kista.events.dropped` | counter, `{event}` | events this replica's writer dropped |
| `kista.usage.dropped` | counter, `{report}` | usage reports this replica did not count: over usage's budgets, or keys lost while the database failed |
| `kista.events.pending` | gauge, `{event}` | `kista.sink`: events waiting per sink |
| `kista.upstream.cells` | gauge, `{cell}` | `kista.outcome`: upstream cells per outcome |
| `kista.upstream.asks` | counter, `{ask}` | `kista.outcome`, lazy mirrors' first requests on this replica: `recorded` (asks inserted or renewed), `answered_503` (requests answered 503, DuckDB's retries included), `fetched` (cells released), `dropped` (offers a queue or a limit dropped; the next run fetches the cell) |
| `kista.tenant.limit.refusals` | counter, `{refusal}` | `kista.limit` (`bytes`, `channels`, `upstreams`, `publishers`): growth this replica refused by a [tenant limit](administration/tenants-and-channels.md#limits) (spec 0038); no tenant label: a tenant reads its own figures from its limits |
| `kista.store.login` | counter, `{request}` | with `store.login.kind: vault`: `kista.login.kind`, `kista.outcome` (`vault`: read from Vault, `cached` (or shared with a read in progress), `error`, `rejected`: the database refused the cached password); from the first read at start; an attempt its caller abandoned is not counted |
| `kista.store.login.duration` | histogram, seconds | with `store.login.kind: vault`: the Vault reads, by `kista.login.kind` and `kista.outcome` |
| `kista.store.login.expiry` | gauge, seconds | with `store.login.kind: vault`: how long the cached password may still be used; `0` when none is cached |

`http.route` is never a path: `api`, `ui`, `cors`, `extension`, `extension-versioned`,
`well-known`, `license`, `licenses`, `usage`, `usage-daily`, `healthz`, `readyz`, `none` (no route matched) or `other`.

`kista.events.pending` and `kista.upstream.cells` are read from the database at each export and are
the same on every replica: take the maximum across `service.instance.id`, not the sum. A sink's name
appears in `kista.sink` only when metrics may name every tenant it takes (or it takes only the
server's events); the others are reported together as `_other`.

What metrics may name is kista's configuration:

```yaml title="kista.yaml"
telemetry:              # file-only; the endpoint is OTEL_EXPORTER_OTLP_(METRICS_)ENDPOINT
  metrics:
    tenants: ["*"]      # tenant names or *: whose ids, channels and extensions appear; none by default
    labels: [platform]  # kista.downloads' optional labels: version, platform, duckdb_version
    max_series: 10000   # per instrument; 100..1,000,000
```

Downloads of unlisted tenants are counted in series without tenant, channel or extension. Beyond
`max_series` series an instrument counts into the SDK's overflow series (`otel.metric.overflow`).
Metrics never carry a principal or a client address.

## Logs

`kista serve` writes JSON logs to stdout at level INFO. Every request is one line:

```json
{"time":"…","level":"INFO","msg":"request","method":"GET","path":"/acme/stable/v2.0.0/linux_amd64/httpfs.duckdb_extension.gz","status":200,"bytes":1234567,"duration":35000000,"client":"203.0.113.7","scheme":"https","listener":":8443","aborted":false,"request_id":"…"}
```

- `duration` is in nanoseconds, rounded to the millisecond.
- The request id is `X-Request-Id` from a trusted proxy (`[A-Za-z0-9._-]{1,64}`), or one kista
  makes. Events caused by the request carry the same id.
- Header values, query strings and bodies are never logged.
- API writes add an `api: write` line, and refusals of an authenticated caller an `api: refused`
  line, with the actor, route template, resource ids, status and client.

The `kista admin` commands write their messages to stderr.

## Health endpoints

| Path | Answer |
| --- | --- |
| `/healthz` | `200 ok` while the process serves requests |
| `/readyz` | `200 ready` when the database is reachable and its schema readable (checked every 5 seconds); `503` otherwise, and from the start of a shutdown, while connections drain |

Both answer `GET` and `HEAD` on every listener, without a token. Use `/healthz` for liveness and
`/readyz` for readiness behind a load balancer.

A `health` listener ([Configuration](configuration.md#serve)) serves only these two paths, on a
port of its own, for the kubelet's probes and a load balancer's health check. It writes no access
log, no event and no metric, so probes do not fill the logs. Do not expose it in a Service; a
load balancer that checks it must reach the pod port directly.

## Configuration summary

| Setting | Default | Range |
| --- | --- | --- |
| `events.retention` | 720h (30 days) | 2 to 365 days; at most 90 with sinks |
| `events.max_rows_per_tenant` | 1,000,000 | 1 to 100,000,000 |
| `events.client_addresses` | `truncated` | `full`, `truncated`, `none` |
| `events.resource` | none | attributes for OTLP exports |
| `events.sinks` | none | at most 16 |
| `statistics.retention` | 26280h (3 years) | 30 days to 10 years |
| `statistics.usage_accept` | `true` | `false` keeps no usage reports |
| `statistics.distributor_min_tenants` | `3` | at least 2: the non-public tenants a row of a [distributor's statistics](#a-distributors-statistics) needs |
| `statistics.public_tenants` | none | the tenants, by name, whose downloads always count in a distributor's statistics |
| `telemetry.metrics.tenants` | none | tenant names or `*` |
| `telemetry.metrics.labels` | none | `version`, `platform`, `duckdb_version` |
| `telemetry.metrics.max_series` | 10,000 | 100 to 1,000,000 |

These blocks are file-only: they have no `KISTA_*` environment variables. See
[Configuration](configuration.md).
