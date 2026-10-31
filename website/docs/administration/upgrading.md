---
title: Upgrading
---

# Upgrading

Upgrading kista is replacing the binary. The database schema moves forward with embedded
migrations; the rules below decide when a binary may start against a database and how several
replicas run through an upgrade. The design is in
[spec 0003](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0003-store-tenants-keys/spec.md)
("Migrations").

A new version is a new image `ghcr.io/hugr-lab/kista:vX.Y.Z` (deploy it by digest) or a new
release binary, both verified as [Deployment](../deployment/index.md) says; or build it with
`make build` (see [Development](../development.md)).

## Migrations

- Each migration is an SQL file embedded in the binary, one per database engine (PostgreSQL, SQL
  Server, SQLite), numbered `0001`, `0002`, and so on. They live in
  `internal/store/migrations/<engine>/`.
- Each migration runs in its own transaction. Every run first takes the lock `kista/migrate`, so
  replicas starting together never apply one twice.
- Applied migrations are recorded in the table `schema_migrations` (`version`, `name`,
  `min_reader`, `applied_at`).
- A binary's **schema level** is the number of migrations it carries. The next release carries 29; v0.1.0 to v0.1.3 carry 28.

### Who migrates: `store.migrate`

| Value | At start (`kista serve` and every `kista admin` command) |
| --- | --- |
| `auto` (default) | applies pending migrations, then starts |
| `check` | changes nothing; refuses to start if migrations are pending or the database needs a newer binary |

Use `check` when the runtime database identity has no DDL rights. Then a separate job with DDL
rights runs the migrations before the new binary starts:

```bash
kista admin -config /etc/kista/migrate.yaml migrate     # applies pending migrations; prints "schema ok"
kista admin -config /etc/kista/kista.yaml check         # verifies, changes nothing; prints "schema ok"
```

`kista admin migrate` and `kista admin check` ignore `store.migrate` and do what they say. Every
other `kista admin` command opens the store with the configured mode: with `auto`, running a newer
binary's `kista admin` against a database **migrates it**. Keep old and new binaries apart on
operator hosts during an upgrade.

## Compatibility: `min_reader`

A database is not refused just because it is newer than the binary. Each migration declares
`-- +min_reader N`: the oldest schema level that can still work with the database after it. The
database keeps the highest value seen, and a binary refuses it only when that value is above its
own schema level.

- An **additive** migration (new tables, nullable columns, indexes) keeps `min_reader` where it was:
  an older binary keeps working against the migrated database.
- A migration whose new rules an older binary would break raises `min_reader` to its own number:
  from then on, only binaries that carry it may run.

Readiness follows the same rule. `/readyz` is recomputed every 5 seconds and fails when:

- the database needs a newer binary (`min_reader` above this binary's level); a replica already
  running stays up but drops out of readiness;
- migrations are pending for this binary (with `store.migrate: check`, before the migration job
  ran).

### Migrations that raise `min_reader`

| Migration | `min_reader` | Why an older binary must stop |
| --- | --- | --- |
| `0001_init` | 1 | the first schema |
| `0003_releases` | 3 | an older `kista admin` would retire a serving key or add a DuckDB version without its C API rows |
| `0005_publication` | 5 | an older binary would ignore blocks |
| `0006_publishers` | 6 | an older binary cannot read grants without an issuer (publisher grants) |
| `0008_upstreams` | 8 | an older replica would publish past the reservation of upstream names |
| `0010_events` | 10 | an older replica would change things without recording events |
| `0012_upstream_credential` | 12 | an older replica would make a credentialed upstream's releases public |
| `0013_storage_gc` | 13 | storage GC: commits must claim streams before the collector runs |
| `0014_purged_slots` | 14 | an older replica would refill the slot of a purged release |
| `0018_feeds` | 18 | an older replica would mark documents feedable without issuing their signed statements |
| `0019_tenant_deletion` | 19 | an older replica would let a deleting tenant be resumed and keep running its upstreams and feeds |
| `0020_bundles` | 20 | an older replica would fetch a bundle-mode upstream online, or give it a credential |
| `0021_bundle_feeds` | 21 | an older replica would run a bundle-mode feed online, or keep no export feed's statements |
| `0022_bundle_uploads` | 22 | an older replica would not import a completed upload, and its collector would not know `uploads/` |
| `0027_license_keys` | 27 | an older replica would let a tenant administrator change a marked publisher's credentials, and would verify licences against `licenses.keys` |
| `0028_publisher_keys` | 28 | GitHub trusted publishing and API keys are removed: an older replica would still read their tables |
| `0029_tenant_holder` | 29 | an older replica would store a licence of another holder than the tenant expects |
| `0030_reserved_names` | 30 | DuckDB's names are the server's data: an older replica would read only the lists compiled into it |
| `0031_tenant_reserved_names` | 31 | an older replica would let a wide publish grant reach a name reserved in a tenant |
| `0032_channel_sources` | 32 | an older replica would serve another source's releases of a name |
| `0033_lazy_mirrors` | 33 | an older replica would run a lazy mirror as an eager one, fetching its whole matrix |
| `0034_build_sources` | 34 | an older replica would refuse a second source's release of a version, or read another source's release as the slot's |
| `0035_rule_modes` | 35 | an older replica's mirror runs would place a manual rule's new versions, and its collector remove landed builds |
| `0036_catalogue_slots` | 36 | an older replica would take a second body of a version from a source in another channel |
| `0037_follows` | 37 | an older replica would treat a follow's rules as manual ones |
| `0038_package_names` | 38 | the schema's tables and columns are renamed (no change in behaviour): an older replica would query the old names |
| `0039_package_types` | 39 | every key that holds a name holds its package type: an older replica's upserts would fail against the new keys, and it must not read rows of a type it does not know |
| `0040_holds` | 40 | an older replica would place held entries, and take a held publisher's publications on a channel's route |
| `0041_distributors` | 41 | an older replica would serve withdrawn entries, and take a distributor's publisher's hold and keys as the tenant's |
| `0042_license_owners` | 42 | licences are per (name, source): an older replica would serve a licence through any source's build, and store and read licences by name |
| `0043_license_entries` | 43 | licences are per catalogue entry and key owners go: an older replica reads and writes `license_key_owners` and relies on the relay's exceptions |
| `0045_download_sources` | 45 | download counts and installers are kept per source: an older replica would add counts by the old key, which the new primary key no longer matches |
| `0046_distributor_holds` | 46 | tenant reservations are dropped and distributors' publishers take the tenant's minimum hold: an older replica reads the dropped table on every channel snapshot, and would ignore the minimum |
| `0047_tenant_limits` | 47 | an older replica would insert builds without their body's length, and check no tenant limit |
| `0048_upstream_revocations` | 48 | an older replica would take a body an upstream blocked from that upstream again, and run a switched-on mirror without its revocations |
| `0049_upstream_names` | 49 | an older replica would run a `*` mirror of a kista with no names, and count none of them in a tenant's cells |

Migrations 0002, 0004, 0007, 0009, 0011, 0015, 0016, 0017, 0023 (the bootstrap's record), 0024
(licences), 0025 (usage reports) and 0026 (usage forwarding) are additive and keep the previous value. During a rolling
upgrade to the release with 0024, an upstream removed through an older replica leaves the licences
it relayed: they are served until their grace ends, or until an administrator removes them
(`kista admin license remove`). During one to the release with 0025, older replicas answer usage
reports `404`, a day the statistics pass counts on an older replica gets no usage users, and a
tenant deleted by an older replica's reaper leaves its usage rows until `statistics.retention`.
Usage forwarding starts with the release that has 0026: the first replica on it forwards to every
credentialed upstream, all of which start at `forward`. An upstream receives only usage it
supplied, and only if it takes usage (an upstream that answers `404` gets nothing). To keep an
upstream from receiving it, run `kista admin migrate` and then
`kista admin upstream usage <tenant> <name> off` before the new `kista serve` starts.
`/stats/usage` grouped by `status` now answers users received from downstream kistas in their own
row (`status: received`) beside this kista's; a client that sums `users` sees the same totals.

## Upgrading one server

1. Back up the database ([Backup and restore](backup-restore.md)).
2. Stop `kista serve`.
3. Replace the binary.
4. With `store.migrate: check`, run `kista admin migrate` with the migration identity.
5. Start `kista serve`. With `auto` it migrates first. Watch `/readyz`.

## Rolling upgrades with several replicas

With several replicas on one database, the first new replica that starts (with `auto`), or the
migration job, applies the migrations. What happens to the old replicas depends on the highest
`min_reader` among the new migrations.

**Additive migrations only** (`min_reader` unchanged): old and new replicas run side by side.
Replace replicas one at a time as usual.

**A migration that raises `min_reader` above the old binary's level** (for example `0018_feeds`
when upgrading from a binary of level 17 or lower):

- as soon as it is applied, old replicas fail `/readyz` within about 5 seconds and cannot restart
  on the old binary;
- so the upgrade requires **every** replica to run the new binary. Load balancers that follow
  `/readyz` send traffic only to new replicas; capacity is reduced until all old replicas are
  replaced;
- plan for it: with `store.migrate: check`, a new replica refuses to start until the migration job
  has run, so run the job and then replace the replicas as quickly as the platform allows; or
  upgrade all replicas in one maintenance window;
- an old replica that is still running is not stopped by kista. Replace it promptly: it still runs
  background work (re-signing, upstream and feed runs) with the old rules.

Several upgrades at once (for example from level 12 to 18) apply every pending migration in order;
the highest `min_reader` among them decides.

### Features that wait for every replica

Some features change what every replica must do. kista cannot see which binary each replica runs,
so the operator declares it:

- **`gc.interval`** stays `0` (the default) until every replica runs a binary with migrations 0013
  and 0014. While it is `0`, there is no scheduled garbage collection, `kista admin blob gc -apply`
  needs `-force`, and purging a yanked release is refused (`release purge` needs `-force` from the
  CLI). Set `gc.interval` (for example `24h`) once the rollout is complete. See
  [Storage](../storage.md).
- **Deleting a tenant** (migration 0019) needs no setting. Only a binary with migration 0019
  records when a tenant was suspended, and the migration gives tenants that are already suspended
  the migration time. So no tenant can be deleted until `tenants.min_suspended` (at least
  `tenants.min_suspended_floor`, 24 hours by default, outside `profile: dev`) has passed since the
  upgrade. An older replica that is still running sees a deleting tenant as suspended and does
  not serve it; one still running a day after the migration is outside what kista supports. A tenant suspended by an older binary while the
  rollout ran has no suspension time: resume it and suspend it again before deleting it. The
  reaper runs only on a replica whose binary matches the database's schema level exactly. See
  [Deleting a tenant](tenants-and-channels.md#deleting-a-tenant).

## Upgrading to the next release (schema 49)

### Configured ceilings (spec 0038, amended)

- No tenant ceiling is fixed in code any more (the bounds of one request or document still are): the ceilings (channels, upstreams, publishers, keys, issuers,
  grants, groups, feeds, blocks, attachments, an upstream's matrix, the longest hold, a publisher
  key's lifetime) are [`limits.ceilings`](../configuration.md#limitsceilings). A server that sets
  none keeps today's values. A ceiling lowered below a tenant's stored limit caps it. No migration.
- The upper bounds of the server's own settings are
  [`limits.ceilings`](../configuration.md#limitsceilings) too: `events_retention` (365 days),
  `events_retention_with_sinks` (90 days), `statistics_retention` (10 years) and
  `admin_token_max_age` (24h). The floors outside `profile: dev` are settings beside what they
  bound: [`tenants.min_suspended_floor`](../configuration.md#tenants) and
  [`rotation.min_floor`](../configuration.md#rotation) (24 hours each). Leaving them out keeps
  today's behaviour.

### `*` on a mirror of a kista (spec 0039, PR 2)

- **Migration `0049_upstream_names`** adds the table `upstream_names`: the names a mirror's `*`
  took from its kista remote. It raises `min_reader` to 49: stop every older replica first (one
  would run a `*` mirror with no names).
- **What changes.** Nothing until an upstream takes `*`. New: the route `<prefix>/-/names`, `*` on a
  repository mirror whose remote is a kista (its runs read the remote's names after the revocation
  pass; a `names` problem when that fails), the upstream's `discovered`, `…/upstreams/{name}/names`
  and the CLI's `upstream names` ([`*` on a mirror of a kista](../upstreams.md#on-a-mirror-of-a-kista)).

### Revocations from a kista (spec 0039, PR 1)

- **Migration `0048_upstream_revocations`** adds `upstreams.remote_kista`, `revocations` (both
  false) and `revocations_etag`, and the tables `upstream_revocations` (a remote's revocations
  matched to an upstream's builds, `unlisted_at` once the remote stops listing one something was
  applied of), `upstream_blocks` (an upstream's blocks of bodies, indexed by tenant) and
  `upstream_yanks` (what an upstream yanked, with each release's prior state). It raises
  `min_reader` to 48: stop every older replica first.
- **What changes.** Nothing is applied until a tenant administrator switches an upstream on. New:
  the route `<prefix>/-/revocations`, every repository mirror's runs reading it first (a
  `revocations` problem when it fails; an older remote is simply not a kista), the upstream's
  `remote_kista` and `revocations`, `…/upstreams/{name}/revocations` and the CLI's
  `upstream revocations` and `upstream revocation lift`, the events `upstream.revocations`,
  `upstream.revocation` and `upstream.revocation.lift`, and the index's `withdrawn` rows. A plan
  accepts revocations (`accepts_revocations`), and a bundle carrying them is `kista-bundle/v2`; a
  plan written by an older site gets a `v1` bundle without them, so the gateway and the site upgrade
  in either order
  ([Revocations from a kista](../upstreams.md#revocations-from-a-kista)).

### Tenant limits (spec 0038)

- **Migration `0047_tenant_limits`** adds `tenants.limit_bytes`, `limit_channels`,
  `limit_upstreams` and `limit_publishers` (all `NULL`: the configuration's defaults), and
  `builds.body_len`, filled from `blobs`, with an index `ix_builds_tenant_body`. It raises
  `min_reader` to 47: stop every older replica first. A build an older replica still inserts during
  the rollout keeps no length until the collector (`gc`) or the next `kista serve` start fills it;
  it counts as `0` until then.
- **What changes.** A server that sets nothing keeps today's bounds, with two visible changes: a
  tenant holds at most 1,000 channels (a new ceiling), and the 101st upstream and the 201st publisher
  of a tenant are refused with `429 tenant-limit` where they got `400`. New: the configuration's
  [`limits:`](../configuration.md#limits), `GET|PUT /api/v1/tenants/{t}/limits`, `limits` in
  `POST /api/v1/tenants` and the bootstrap's tenant, `kista admin tenant limits`, the event
  `tenant.limits`, the problem type `tenant-limit` and the metric `kista.tenant.limit.refusals`
  ([Limits](tenants-and-channels.md#limits)). On SQL Server, a source's icons are summed as `bigint`.

### The distributor in a tenant (spec 0037)

- **Migration `0046_distributor_holds`** drops `tenant_reserved_names` (spec 0030's names reserved
  in a tenant: they are lost) and adds `tenants.distributor_hold_seconds`, `NOT NULL DEFAULT 86400`.
  It raises `min_reader` to 46: stop every older replica first.
- **Tenant reservations are removed.** `GET|PUT|DELETE /api/v1/tenants/{t}/reserved-names[/{name}]`,
  `kista admin tenant-reserved`, the `reserved.tenant.add` and `reserved.tenant.remove` events and the
  console's "Names reserved for a distributor" card go. A tenant's own grants reach every name kista
  does not reserve server-wide and no upstream of the tenant lists; an upstream's `allow_reserved`
  concerns DuckDB's names only.
- **A narrower distribution admin.** It no longer creates ordinary publishers, writes or reads
  grants, or reads the tenant's own publishers (`404`); a name one of them holds answers
  `publisher-taken` (`409`). Grants and publishers a distribution admin made before stay, the
  tenant administrators' to manage. An existing publisher is never marked a distributor's any more,
  by anyone.
- **The minimum hold for distributors' publishers** is one day in every tenant from the upgrade on:
  a distributor's builds reach the tenant's channels a day after publication unless its
  administrators lower it (`kista admin tenant distributor-hold <tenant> 0`,
  [the minimum hold](../access.md#the-minimum-hold-for-distributors-publishers)). Entries held
  already keep their hold; lowering the minimum releases none of them.

### A distributor's download statistics (spec 0036, phase 2)

- **Migration `0045_download_sources`** adds `source` to the keys of `download_counts` and
  `download_installers`, with an index `(source, day)`: in place on PostgreSQL and SQL Server, by
  rebuilding both tables on SQLite. Rows already there get the source `''`: they count for no
  distributor, and the tenant's statistics answer them as `unknown`. It adds `distributor_weeks` and
  `distributor_weeks_done`, the weekly snapshots. It raises `min_reader` to 45: stop every older
  replica first.
- **What changes.** A download counts under its release's source, and `install` events carry
  `source`; `GET …/stats/downloads` groups by `source`; `GET /api/v1/distributor-stats/downloads`
  answers a [distributor's statistics](../observability.md#a-distributors-statistics) to distribution
  admins and server administrators, from the weekly snapshots the daily pass computes (its first
  pass computes the complete weeks the retention holds, at most 106). New settings:
  `statistics.distributor_min_tenants` (default 3, at least 2) and `statistics.public_tenants` (none
  by default), applied when a week is computed. A used distributor's publisher's distributor is no
  longer renamed (`409`). The route answers from the first computed week on (`400` before), and
  the week of the upgrade holds only the downloads counted after it (those before have no source).
- **Statistics' row limit.** Counts are now kept per source too, so a tenant's statistics query
  over a long range reaches the 100,000 stored rows it reads (`400`, narrow the range) sooner when a
  name has several sources.

### An entry's icon and summary (spec 0036, phase 1)

- **Migration `0044_entry_meta`** adds `entry_meta` (an entry's summary and icon, who set them, and
  the last signed write's mark) and `icon_claims` and `icon_tombstones`. It does not raise
  `min_reader`: an older replica ignores the tables, and the metadata routes answer `404` there.
- **What changes.** `PUT` and `GET …/catalogue/{source}/extensions/{ext}/meta` and `GET …/icon`
  ([An entry's icon and summary](../publishing.md#an-entrys-icon-and-summary)); the catalogue's reads
  answer `summary`, `icon` and `distributor`; the event `catalogue.meta`; the problem type `stale`;
  `kista admin catalogue meta`. A storage domain holds `icons/<sha256>` objects, which the collector
  marks and deletes as it does streams (`storage.gc` counts `icons`; `blob gc` prints `ICONS`). An
  icon counts toward its source's `catalogue.max_bytes`. The console's content security policy takes
  `blob:` images.

### Licences per catalogue entry (spec 0035, phase 1)

- **Before upgrading.** Licences of source `unknown` (imports whose key had no owner after migration
  0042, served nowhere already) are deleted by the migration: export them first, with the older
  binary, `kista admin license token <tenant> <name> -source unknown`, and import them again after
  the upgrade into the entry they belong to. A licence manager's client (Enterest's reconciler) must
  send `?source=` on every licence write before kista is upgraded: the older kista ignores it on an
  import, so the order is safe.
- **Migration `0043_license_entries`** raises `min_reader` to 43: stop every older replica first. In
  the three databases it:
  - keeps a relayed licence that is in its relaying upstream's entry (`upstream:<id>`) as it is;
  - deletes a relayed licence that is in **another relaying upstream's** own entry (a `repository`
    upstream with a credential, not in bundle mode, a paused one included): that upstream's relay,
    requested below, stores its own copy there and governs it, where an import would never be
    revoked by it;
  - turns a relayed licence that is in any other entry (a publisher's, a bundle-mode or
    credential-less upstream's: migration 0042 gave it its key's owner, or an administrator's
    `license owner` moved it there), or whose relaying upstream is gone, into an **import** in that
    same entry (`source` `import`, no upstream), its visibility kept: the channels
    serving it keep serving it, and it shows as imported from then on (its relay no longer governs
    it; remove it when the relay's own copy is back, or leave it to its grace). The relaying
    upstream's own entry gets its copy at its next relay run, requested below: until then that
    entry's channels serve no licence for the name;
  - moves a relayed licence of source `unknown` into its relaying upstream's entry, when that upstream
    exists and its entry holds no licence of the name (a source holds at most one licence of a name,
    so no two rows meet there); otherwise, and for every import of source `unknown`, it is deleted;
    other imports keep their source as their entry;
  - requests a relay run for every relaying upstream (a `repository` upstream with a credential, not
    in bundle mode; a paused one's runs once it is resumed);
  - renames `licenses.owner_source` to `entry_source`, still in the primary key;
  - drops `license_key_owners` and the index `ix_build_license_keys_kid`.
- **What changes.** A licence belongs to a catalogue entry, and every write names it: an import,
  `DELETE`, `…/public` and `…/private` require `?source=` (`-source`), `400` without it. An import
  verifies against that entry's live keys only; a relay stores into its own upstream's entry; a
  channel serves the licence of the entry it serves the name from. Key owners go: no first recorder,
  no `license owner`, no `PUT …/license-keys/{name}/{kid}/owner`, no `key_owner` refusal, no
  `license.key_owner` or `license.key_conflict` events. A licence answers `entry` instead of `owner`;
  `GET …/license-keys` answers per key the entries it is live for, each with its `builds` (no
  `owner`, no top-level `builds`), and takes `?source=`. A licence manager names only the entries of
  distributors' publishers (any other `?source=` answers `404`, recorded as `authz.refused`). A bundle's licence goes into every supplying, verifying
  bundle-mode upstream, counted per target. See [Licensing](../licensing.md#a-licences-entry).

### The console's catalogue and a channel's rules (spec 0033, phase C)

- No migration. The console gains a **Catalogue** section for tenant administrators, and a channel's
  **Sources** tab shows each rule's mode, the current and newest versions and the last outcome, with
  the channel's hold and follows beside it. Its **Switch…** is now **Change…**, which sets the mode
  too ([Administration console](../console.md#screens)).
- A mirror's cells keep the outcomes `other_source` and `shadowed`: a mirror still releases into
  its home channel, where they are decided.
- Small changes: a rule's `newest` skips revoked versions, as the placement pass does;
  `kista admin catalogue show` ends with an `ID` column (the build's, for `catalogue purge`); placing
  by hand takes `publisher-id:<id>` from a server administrator, as its catalogue names a
  distributor's publisher whose name another one had.

### Licences per source, and a key's owner (spec 0033, phase B part 2)

Key owners are removed by spec 0035 ([Licences per catalogue entry](#licences-per-catalogue-entry-spec-0035-phase-1),
above): an upgrade from a release before 0042 runs both migrations, and only 0043's result counts.
As 0042 recorded it:

- **Migration `0042_license_owners`** adds `license_key_owners` (each key's owner for a name in a
  tenant) and gives licences their source (`owner_source`, part of their primary key; SQLite
  rebuilds the table), and raises `min_reader` to 42: stop every older replica first.
- **What it records.** Each key's owner, as kista computed it then, by the database's clock; a key another source
  recorded first on a build still held gets none yet. Each stored licence gets its key's owner, or
  the source `unknown` where the key has none: kept, listed and removable (`?source=unknown`,
  `-source unknown`), served nowhere until imported again or relayed again by its upstream, or
  until its grace ends.
- **What changes.** A channel serves the licence of the source it serves the extension from. The
  relay stores a licence when its upstream owns the key, or when it is the very licence the key's
  owner holds (another upstream of the product, in another channel, stores it as its own at its next
  run); otherwise it refuses it with `key_owner`. Usage goes to the source a channel serves the
  extension from. An import is accepted as soon as the key's owner has the extension available in
  the catalogue, before any placement.
- **API and CLI.** A licence answers `owner` (its source); `source` stays how it arrived. The routes
  of one licence take `?source=` and answer `409` without it where the extension has licences of
  several sources; an import's `Location` names the source. `GET …/license-keys` answers each key's
  `owner`, and lists the keys of a source's newest available version too. New: `PUT
  …/license-keys/{name}/{kid}/owner` and `kista admin license owner`; `-source` on `license list|show|
  token|remove|public|private`; `license list` gains `OWNER`, `license keys` `OWNER`. Events:
  `license.key_owner`, `license.key_conflict`; `license.import`, `license.change` and
  `license.remove` gain `source`. The relay's refusals gain `key_owner`.

### Distributors' publishers and withdrawals (spec 0033, phase B part 1)

- **Migration `0041_distributors`** gives publishers a `distributor` (none for every existing one),
  builds a withdrawn marker (`withdrawn_at`, `withdrawn_reason`; none is withdrawn), a table of
  withdrawn versions (`withdrawn_versions`, empty) and a table remembering removed distributors'
  publishers; it raises `min_reader` to 41: stop every older replica first.
- Nothing changes until a distribution admin or a server administrator marks a publisher a
  [distributor's](../publishing.md#distributors-publishers): such a publisher publishes into the
  catalogue only, without grants, and its keys and hold are the distributor's, no longer a tenant
  administrator's (`404`). A server administrator creates one marked (`POST …/publishers` with
  `distributor`, `publisher add … -distributor`); an existing publisher is marked only when a
  distribution admin created it and it is fresh (spec 0037: never; a distributor's publisher is
  created marked only).
- New routes: `PUT …/publishers/{name}/distributor` (and `distributor` in `POST …/publishers`), `POST …/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw`
  ([withdrawals](../upstreams.md#withdrawals)); the publisher hold route, the catalogue's read
  routes and its purge admit distribution admins (for distributors' publishers). New in answers: a
  publisher's `distributor`, a catalogue entry's `withdrawn` versions, a catalogue build's
  `withdrawn` state with `withdrawn_at` and `withdrawn_reason`, a release's `withdrawn`; the problem
  type `…/withdrawn` (`409`). Events: `catalogue.withdrawn`, `publisher.change` gains `distributor`
  and `before`, `publisher.add` gains `distributor` (the event catalogue's version stays 1). A source may be named `publisher-id:<id>`
  on the withdrawal and the distribution admins' reads. A problem's detail no longer starts with `store: `.
- `purge_held=true` on a key's removal is a distribution admin's too, for a distributor's
  publisher; purging a catalogue build now waits for the releases in flight into the tenant's
  channels (the tenant's upstream lock).

### Holds (spec 0033, phase A2)

- **Migration `0040_holds`** gives upstreams, publishers and channels a hold (`hold_seconds`, `0`
  for none) and builds when a held one becomes available, the publisher key that published it and
  when a purge kept it revoked, and raises `min_reader` to 40: stop every older replica first.
- Nothing is held until an administrator sets a hold ([Holds](../upstreams.md#holds)): every
  existing build is available, and every channel, publisher and upstream has none.
- A publisher with a hold is refused on a channel's publication route (`409`, problem type
  `…/held`) and publishes into the catalogue; a CI that publishes into a channel keeps working until
  its publisher is given a hold.
- New in answers: an upstream's and a publisher's `hold`, a catalogue entry's `held` and `revoked`
  versions, and a catalogue build's `state`, `available_at` and `revoked_at`; a mirror's cell may
  be `held`. `DELETE …/publishers/{name}/keys/{id}` takes `?purge_held=true` (answered `200` with
  what it purged and kept). Events: `catalogue.hold`, and new fields on `catalogue.arrival`,
  `catalogue.purge`, `release.place`, `publisher.key.remove` and `publisher.remove` (the event
  catalogue's version stays 1).
- A publication whose publisher or signing key is removed while its upload is in flight is refused
  (`401`), and a mirror's arrival from an upstream removed meanwhile fails its cell.

### Package types (spec 0034, phase 2)

- **Migration `0039_package_types`** gives every key that holds a name its
  [package type](../concepts.md#packages-and-package-types): channels, builds (their identity and
  slot), grants, extension groups' members, the server's and the tenants' reserved names, and
  licences get a `package_type` column, and their keys and indexes take it. Every existing row is
  `duckdb_extension`, the one type there is; a grant without an extension is `*` (every type), so it
  keeps meaning what it says. It raises `min_reader` to 39: stop every older replica first. On
  SQLite the tables whose keys change are rebuilt, with foreign keys off and checked before the
  commit.
- Nothing a DuckDB client or an existing API caller reads changes: `package_type` and `?type=`
  default to `duckdb_extension` and are left out of answers for it. A request that names a type the
  server does not know is `400`; the event catalogue (`GET /api/v1/event-kinds`) lists
  `package_type` among the fields of the events that may carry it.

### Package names in the schema (spec 0034, phase 1)

- **Migration `0038_package_names`** renames the schema to say package where the thing is general:
  `extension_groups`, `extension_group_members` and `extension_group_grants` become
  `package_groups`, `package_group_members` and `package_group_grants`; the name column of a
  group member and of a grant, `extension`, becomes `package`; `ext_version` becomes
  `package_version` in every table that has it. It raises `min_reader` to 38: stop every older
  replica first.
- Nothing else changes: every row keeps what it held, and the API, the CLI, events, bundles and
  configuration keep their names. Only a query that reads kista's database directly (a report, a
  dashboard) needs the new names.

### A channel follows a whole source (spec 0033, phase A1 part 4)

- **Migration `0037_follows`** adds the channels' follows and the names they skip, and raises
  `min_reader` to 37: stop every older replica first. Nothing is followed until an administrator
  says so.

### A version is immutable within its source in the whole tenant (spec 0033, phase A1 part 3)

- **Migration `0036_catalogue_slots`** gives builds their slot and makes a source's version unique
  in the tenant, and raises `min_reader` to 36: stop every older replica first.
- Two bodies of one version a source released in two channels before are both kept, the released
  one (else the oldest) holding the unique key and the others legacy duplicates; a new arrival of
  another body is refused. The tenant's own source keeps the rule per channel.
- `catalogue.max_entries` (20,000) and `catalogue.max_bytes` (100 GiB) bound each source's builds
  placed nowhere; released builds do not count.
- Builds landed by A1 part 2 (a mirror under a manual rule) are kept while their cell holds them;
  the first hourly sweep leaves the others to the collector.

### A channel's rules take from the catalogue (spec 0033, phase A1 part 2)

Every build stays in the tenant's catalogue, and a channel takes from it by its rule for each name,
`auto` or `manual` ([The catalogue and a channel's rules](../upstreams.md#the-catalogue-and-a-channels-rules)).

- **Migration `0035_rule_modes`** gives rules an optional mode and builds the visibility and
  provenance of their first release and a `landed` flag, and raises `min_reader` to 35: stop every
  older replica first.
- Every existing rule has no mode: channels serve what lands in them, as before, and nothing changes
  until an administrator gives a rule a mode (`channel source … -mode auto|manual`).
- A mirror whose name's rule is set `manual` fetches new versions into the catalogue only (cell
  outcome `landed`); place them with `release place`.

### A slot is its source's (spec 0033, phase A1 part 1)

Two sources may release the same version of a name in one channel; the channel serves the one whose
source it selected ([A name's source in a channel](../upstreams.md#a-names-source-in-a-channel)).

- **Migration `0034_build_sources`** gives each build its source and makes releases' slots and
  purged slots per source, and raises `min_reader` to 34: stop every older replica first. On SQLite
  it rebuilds the `builds`, `releases` and `purged_slots` tables, with foreign keys off on its
  connection and checked before it commits.
- A build whose releases named two sources (the same body released by one source and then by
  another) becomes one build per source, each with the licence-key records its source made.
- A licence key counts as live, and an upstream's licences and usage are relayed and forwarded,
  only through releases a channel serves: of the source the channel selected for the name.
- A slot purged before the upgrade stays taken for every source; one purged after it, for its own.
- Publishing a body another source released in the channel now makes a release of the publisher's
  own (`201`) where it took the existing one (`200`).

### A name's source in a channel (spec 0031)

A channel serves each extension name from one source, which its administrator selects
([A name's source in a channel](../upstreams.md#a-names-source-in-a-channel)).

- **Migration `0032_channel_sources`** records each release's source and each channel's selections,
  and raises `min_reader` to 32: stop every older replica first.
- Each release gets the source it came from: the upstream whose cell holds its body (else one of
  its channel's upstreams listing the name; a removed upstream's otherwise), the publisher whose
  key published it, the source of the release a promotion copied, or the tenant.
- Each channel selects, for each name with a release that is not yanked, the source of the release
  it served: a replacement before a mirror (the old rule), the newest current one first. A channel
  that held releases of one name from several non-mirror sources (an administrator's `release add`
  and a publisher's CI, say) serves only the newest's from now on: check `kista admin channel
  source <tenant>/<channel>` after the upgrade, and switch where needed.
- From now on, a publication under a name a mirror holds is refused (`409`) until the channel's
  administrator switches the source, where before it replaced the mirror.

### Reserved names as data, and a distributor's names in a tenant (spec 0030)

- **Migration `0030_reserved_names`** makes DuckDB's names the server's data (`kista admin
  reserved`), seeded with the lists kista compiled in until now; a new DuckDB version is added with
  its core names (`version add -core-extensions`). It raises `min_reader` to 30.
- **Migration `0031_tenant_reserved_names`** adds names reserved in a tenant and raises `min_reader`
  to 31 (migration `0046` drops them again, spec 0037).

### An expected holder per tenant (spec 0028)

A server administrator may set the holder a tenant's licences must name
([An expected holder](../licensing.md#an-expected-holder)).

- **Migration `0029_tenant_holder`** adds the nullable column `tenants.holder_id` and raises
  `min_reader` to 29: stop every older replica first, since one would store licences without the
  check.
- Nothing changes until a holder is set: every tenant starts with none.

## Upgrading to v0.1.0 (schema 28)

### Publishers sign their requests (spec 0023)

A publisher authenticates by requests signed with one of its developers' SSH keys. GitHub trusted
publishing and API keys are removed. See [Publishers](../publishing.md#publishers).

- **Migration `0028_publisher_keys`** drops the tables `publisher_github` and `api_keys`, adds
  `publisher_keys` and two release columns, and raises `min_reader` to 28: stop every older replica
  first. Publishers and their grants stay.
- **Each publisher needs a key** (`kista admin publisher key add`) before it publishes again. A
  `kista_…` API key is an invalid token on the API (`401`); on the DuckDB routes it is taken as no
  token, so public installs keep working.
- **`publish.providers` is refused** at start: remove it from the configuration.

### Licence keys in the builds (spec 0021)

Licences no longer verify against `licenses.keys`. They verify against the keys an extension's live
builds declare in their key blocks, recorded when a trusted source brought the build: a publisher a
server administrator marked, or a `repository` upstream. See
[Licensing](../licensing.md#the-keys-a-licence-verifies-against).

- **Migration `0027_license_keys`** adds the table `build_license_keys` and the column
  `publishers.license_keys`, and raises `min_reader` to 27: stop every older replica first.
- **`licenses.keys` is ignored**, with a warning at start. Remove it: the next release refuses it.
  The routes it fed are gone: `GET /api/v1/licenses/keys` and `kista admin license keys` without
  a tenant. `kista admin license verify` now needs `-key`.
- **Existing builds carry no keys.** Builds published or mirrored before this release have none
  recorded, and builds made with an earlier SDK have no key block. A stored licence is accepted and
  served again only once its extension is rebuilt with this SDK and published by a marked publisher
  or mirrored again. No issuer publishes keys yet, so this concerns test deployments only.
- **Licensing is on by default.** A deployment that ran without keys, so with licensing off, now has
  the licence routes and relays licences from its credentialed upstreams. `licenses.enabled: false`
  keeps it off.
- **A leaked key** is taken out with `licenses.refuse_keys`.

### Files kista reads

Every file the configuration names now follows one rule
([Files kista reads](../security.md#files-kista-reads)). Check the files before the upgrade: a
file that breaks its rule stops `kista serve` at start, or fails the read that needs it, with an
error naming the file and the rule.

**Files that had no check before** and now have one:

- secrets: `store.login.password_file`, the listener `tls.key_file`, the OTLP `headers_file`, and
  the upstream credential `client_secret_file` and `key_file`;
- tokens: the upstream credential `token_file` and `assertion_file`;
- public files: `egress.ca_file`, the Vault `ca_file`, the S3 `ca_file` and the listener
  `tls.cert_file`.

**What may now be refused:**

- **Owners of public files and tokens.** A public file or a token must be owned by root or by
  the user kista runs as. A CA bundle on the host owned by another user is refused; so is a token
  file written by another user.
- **Readable directories.** A secret or a token is opened inside its directory, which kista must
  be able to read (the `r` bit, not only `x`). `/etc/ssl/private` at `0710 root:ssl-cert` is not
  readable, for example.
- **Links.** A secret's or a token's link must be relative and stay inside the file's directory;
  an absolute link is refused even when it points inside. certbot's `live/privkey.pem` leaves its
  directory: point `tls.key_file` at the real file or a copy.
- **Hard links.** A secret or a token must have exactly one.
- **Sizes.** An S3 or Azure Blob key file over 4 KiB used to be cut at 4 KiB; it is now refused.
  A Vault token file over 64 KiB likewise.

**What is now accepted:**

- A Vault `kubernetes` or `jwt` token file no longer needs mode `0600` or narrower: like every
  token, any read bits on a file owned by root or kista's user.
- The AWS static key files (`identity.access_key_file`, `identity.secret_key_file`) may be
  relative links inside their directory; any link used to be refused.
- Secrets may be root-owned with kista's group and mode `0440` or narrower: a Kubernetes Secret
  volume under `fsGroup`.

## Downgrading

A binary runs against a newer database only while the database's `min_reader` is at most the
binary's schema level. Migrations have no down steps. To go back past a migration that raised
`min_reader`, restore the database backup taken before the upgrade
([Backup and restore](backup-restore.md)), with the consequences described there.

## Checklist

- Read the release's migrations: which ones raise `min_reader`.
- Back up the database.
- Check the files the configuration names against
  [Files kista reads](../security.md#files-kista-reads) (what changed: [above](#files-kista-reads)).
- Decide who migrates (`auto` on the first replica, or a `kista admin migrate` job).
- Run `kista admin check` with the new binary afterwards: `schema ok`.
- Watch `/readyz` on every replica, and the server events
  (`kista admin events list -server`).
- After the rollout, set or keep `gc.interval` as described above.
