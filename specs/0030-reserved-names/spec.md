# Spec 0030: Reserved names as data, and a distributor's names in a tenant

- **Status**: implemented (parts 1, 2 and 3)
- **Changed by** [0036](../0036-entry-meta/spec.md) (phase 1): distribution admins write a distributor's entries' icon and summary.
- **Changed by** [0036](../0036-entry-meta/spec.md) (phase 2): distribution admins read a distributor's download statistics across tenants (`GET /api/v1/distributor-stats/downloads`).
- **Date**: 2026-10-22
- **Author**: vgsml, Claude

## Summary

The names DuckDB owns (core and community) become the server's data, not lists compiled into kista.
A DuckDB version brings its core names when it is registered, an upstream brings the names it
mirrors from DuckDB's repositories, and a server administrator adds one by hand. The list still only
grows. A tenant can also hold names for a **distributor** (Enterest): a name reserved in a tenant is
reached there only by a grant that names it, as a core name is, so the tenant's wide grants never
reach the catalogue a distributor publishes into it. A narrow server role, the **distribution
admins**, manages those reservations and the distributor's marked publishers, and nothing else.

## Problem

- **The core list follows kista's pin, not the DuckDB versions it serves.** Spec 0008 generates it
  from the DuckDB commit in `e2e/DUCKDB_PIN` and compiles it in. A DuckDB release with a new core
  extension (v2.0.0's `legacy_parser`) is unprotected on every server until kista's pin moves, CI
  regenerates the list and a release ships it, though the server serves that DuckDB version from the
  day it is registered.
- **Regenerating on every pin is busywork that hides the reason.** The check that the committed lists
  are current fails CI on an unrelated change (a pin move), and the fix is mechanical.
- **A distributor has no way to keep its names in a tenant.** Enterest publishes the extensions of
  its catalogue into its customers' tenants (Enterest's spec 0003). A tenant administrator may give
  their people `publish` on the whole tenant or a channel; such a grant reaches any name that is not
  reserved, so one of them could publish a build under the name of an extension Enterest
  distributes, shadowing it for every client of the channel.
- **A distributor needs server power for little.** Marking a publisher to declare licence keys
  (spec 0021) and managing a marked publisher's keys and grants (spec 0023) are a server
  administrator's; Enterest would hold that role to do a few things.

## Design

### The server's reserved names

Two tables replace the compiled lists:

```text
reserved_names          name pk, kind (core|community), added_at
reserved_name_sources   name fk, source, added_at;   pk (name, source)
reserved_version        a single row: the list's version, a counter
```

- A `source` says why a name is reserved: `builtin` (the lists kista shipped until this spec,
  loaded by the migration), `version:<duckdb version>@<commit>`, `upstream:<tenant>/<upstream>`,
  `bundle:<id>`, `community-sync@<commit>` or `admin:<principal>`. A name keeps every source it
  was given, so the versions of DuckDB that have it as core are known.
- **One list for every DuckDB version.** Core names differ between DuckDB versions (a release adds
  some, as v2.0.0 added `legacy_parser`). A name core in any registered version is reserved for
  every version: a channel serves several versions and its clients upgrade, and DuckDB's core names
  in practice only grow, so a list per version would complicate every check for little. (Serving
  is per version anyway: a build is served only for the DuckDB version it was built for.)
- A name is never removed and never changes kind, except from community to core (a community
  extension DuckDB takes in); a name reserved both ways is core. Every insert is insert-if-absent, so
  core always wins a race. The counter is bumped only when a row is inserted or a kind changes.
- Removal is offered for one case: a name whose **only** sources are `admin:`, by a server
  administrator, audited. A name DuckDB brought stays.
- The **aliases** (`http`, `sqlite`, ...) and DuckDB's signing keys stay compiled in: they are
  DuckDB's grammar and trust roots, refused or used everywhere (specs 0006, 0009), and change with
  DuckDB's loader. `make reserved` keeps generating them (aliases, `internal/upstream/duckdbkeys`)
  from the pin, and its CI check stays for those; it no longer writes the name lists.

### Where names come from

1. **A DuckDB version's core names.** Registering a version requires them for a version registered
   after this spec:
   - API: `POST /api/v1/duckdb-versions` takes `core_extensions` (a list of names, required); each
     is reserved as core with `source version:<name>@<commit>`. `POST
     /api/v1/duckdb-versions/{name}/core-extensions` adds names to a registered version (as
     `{name}/c-apis` adds C API versions). The answer lists the names that were new.
   - The bootstrap file (spec 0024) takes `core_extensions` per version, and the chart passes it.
   - The list comes from `kista duckdb core-extensions <ref>`, a command that needs no store (like
     `kista bundle inspect`): it streams DuckDB's source tarball at `<ref>` (a release tag, or a dev
     build's commit; `codeload.github.com` resolves a 10-character source id) and runs spec 0008's
     generator over it, never executing what it fetched. It prints the names and the commit it read.
     A generator that misparsed a new layout fails loudly (as built: below). An operator without
     github.com writes the file elsewhere and passes it.
   - A version registered before this spec keeps the builtin names.
2. **An upstream's names.** A build mirrored from a `duckdb-core` upstream reserves its name as core,
   from a `duckdb-community` upstream as community, in the same transaction as its release; a bundle
   imported in bundle mode (spec 0014) does the same for the builds it brings from those kinds. So a
   server that mirrors DuckDB's repositories learns their names as it serves them. The upstream's
   prefixes are fixed and its bodies verified with DuckDB's keys (spec 0009), so only DuckDB's names
   are learned this way.
3. **An administrator's.** `kista admin reserved add <name> -kind core|community`, `kista admin
   reserved list [-name n]` (with each name's sources), `POST|GET /api/v1/reserved-names` (server
   administrators and distribution admins read; server administrators write). Before an addition
   the answer names the tenants with live releases under the name, and adding is refused without
   `-force` when there are some (the tenants keep them: below).
4. **The community list.** duckdb/community-extensions grows every week, so a server may follow it:
   - `reserved.community_sync` (off by default): every `interval` (a day by default) the server
     reads the names of `extensions/*/description.yml` in duckdb/community-extensions' default
     branch (`url`, GitHub's codeload by default; a mirror of the repository works) and reserves the new
     ones as community, `source community-sync@<commit>`. It needs egress to that host only, and
     is a server's choice: a registry that mirrors community extensions, or a distributor's
     (Enterest's), turns it on. A failed read is logged and retried at the next interval; it never
     removes a name.
   - `kista duckdb community-extensions [ref]` prints the same list (at a commit, default the
     default branch), for a server that does not sync, to pass to `reserved add`.

   A community name matters less than a core one: DuckDB autoloads only core extensions (its
   `AUTOLOADABLE_EXTENSIONS`), so a community name is installed only by a client that asks for it
   from a repository, but a tenant should not be able to pass a build off as a community one.

### What a reservation changes, and what it does not

- Every check that spec 0008 and spec 0009 make against the compiled lists reads the table instead,
  through one lookup, `reserved.Lookup(ctx, tenant, name)`, which answers core, community, tenant
  (below) or upstream (spec 0009's names an upstream of the tenant provides), in that precedence:
  authorization of `publish` and `promote`, attachments (whose `publish` check is the same), the
  intake's entry rules, pull-through's `"*"`, the index's `shadows`, and the grant explanations of
  the CLI and the API (which today ignore upstream names: they read all four now).
- **The hot paths** (serving the index, pull-through) read a set held on the channel's snapshot,
  keyed by `reserved_version` (read with the channel row, as the channel's own counters are, spec
  0003); a new row bumps the counter, and every replica's next request rebuilds. Authorization and
  the other paths query the tables directly.
- **Under the lock.** Spec 0009 reads a tenant's upstream reservation again under the tenant's lock
  and the channel lock before a release insert; that re-check now reads the server-wide names and the
  tenant's reserved names too, so a name reserved while an upload spools is seen.
- **Releases that exist stay.** A release under a name that becomes reserved stays as it is, as in
  spec 0008. An upstream entry added before its name became reserved keeps mirroring it
  (grandfathered); one added after is refused without `allow_reserved`.
- **Community becoming core.** The intake starts recording tenant shadows (spec 0009) for
  community-built mirrors of the name in signed channels, so a passthrough channel answers `404`
  for it there; the change is audited and the tenants with such mirrors are listed in the answer.
- **Pull-through.** `"*"` on a `duckdb-community` upstream matches community names (whatever their
  source), never a core name, a tenant's reserved name or an upstream name of the tenant; `"*"` on
  any other kind matches no reserved name (spec 0009, otherwise unchanged). So a community name the
  first fetch reserves keeps being pulled through.

### Names reserved in a tenant

```text
tenant_reserved_names   tenant_id fk, name, holder, added_at;   pk (tenant_id, name)
```

- In its tenant, a reserved name is reached for `publish` and `promote` only by a grant that names
  the extension (in the channel or in every channel); a tenant- or channel-wide grant never reaches
  it, nor does an extension group's. An upstream entry for it needs `allow_reserved`, and `"*"`
  never matches it.
- What it does not stop: a tenant administrator may still grant `publish` that names the extension,
  and may add or manage its releases with `admin`. It narrows the tenant's **wide** grants; the
  distributor's own checks (Enterest's spec 0003) watch for named grants it did not make.
- `holder` is free text naming who holds it (`enterest`). It grants nothing; who publishes is still
  decided by grants. A release under the name is an ordinary release: `shadows` is not set by a
  tenant reservation.
- Reserving takes the tenant's lock (as an upstream change does), bumps the tenant's channels (so
  their snapshots see it), and answers with the name's live releases in the tenant, so the holder
  knows who was there before it. Releasing (`DELETE`) changes no release.
- **Who**: server administrators and distribution admins write
  (`PUT|DELETE /api/v1/tenants/{t}/reserved-names/{name}`, `kista admin tenant-reserved add|remove
  <tenant> <name> -holder <text>`); the tenant's administrators also read their own tenant's
  (`GET`), as the console shows them.
- A tenant's deletion (spec 0019) removes its rows: the reaper's steps gain the table.

### The distribution admins

`auth.server_distribution_admins` (principals, as `auth.server_license_admins` of spec 0029) may,
on every tenant, and nothing else:
- reserve and release names in the tenant, and read the server's reserved names;
- mark a publisher to declare licence keys, and manage a marked publisher's keys and grants (spec
  0021, 0023): the steps a server administrator alone takes today;
- create publishers, and write `publish` and `promote` grants and `admin` grants on extensions (not
  on a tenant or a channel), so it can set up a distributor's principals in a tenant.

It is never a tenant administrator: it reads no channel, key or release beyond what those calls
answer, and cannot change a tenant, a channel or a key set.

### Data model

Migration 0030, in every dialect (PostgreSQL, SQL Server, SQLite):

- `reserved_names` and `reserved_name_sources` created and filled with the builtin lists (`source
  builtin`); `reserved_version` created at 1;
- `tenant_reserved_names` created (as built: in migration 0031, with `min_reader 31`);
- `min_reader 30`: a replica older than this would read only its compiled lists (as built; the
  tenant-reserved names' reason is migration 0031's `min_reader 31`).

The `.txt` lists stay in the repository as the documented seed of the migration, and the store
suite checks the migration holds them.

## Security

- **Growth fails closed, with care.** A name added makes publishing it narrower, never wider; but it
  changes the intake for an existing entry (grandfathered, above) and the shadows of a community name
  that becomes core, so additions are previewed and audited, and an administrator's mistake can be
  undone.
- **Little new trust in the network.** The server never fetches DuckDB's source: an operator's
  command does, once per version, from github.com, and writes a list a person passes on; the
  air-gapped path is a file. A name an upstream brings is reserved because its upstream is a DuckDB
  repository the server already verifies. The community sync, off by default, is the exception: it
  trusts what its URL serves (TLS, GitHub, DuckDB's review of what is merged into
  community-extensions). What it changes only narrows who may publish a name, and it leaves a name a
  tenant has releases under, or a jump of more than 100 names, to an administrator (as built, part 3).
- **Tenant isolation.** A tenant's reserved names reach only that tenant. Only server and
  distribution admins reserve and release them: a tenant administrator cannot lift a distributor's
  reservation, nor make one that would block another tenant.
- **The distribution admins** hold a fixed list of calls; a stolen principal of that role can reserve
  names (an annoyance, undone by a server administrator), manage the publishers a distributor
  marked, and grant `publish`, `promote` and `admin` on the names reserved in a tenant: it can
  publish under those names there, as the distributor does. It cannot take a tenant's publisher with
  grants, grant on other names, remove the tenant's grants, or change a tenant's keys; it reads a
  tenant's channels only as a reservation's answer names releases under the name (as built, part 2).
- **Existence.** The server's reserved names are DuckDB's, public anyway; a tenant's are read only
  by its administrators and the admins above.

## Testing

- The store suite on the three dialects: the migration's builtin rows, each source, insert-if-absent,
  community to core, `reserved_version` bumps, the tenant table and the reaper.
- Authorization: a reserved name (from a version, an upstream, a bundle, an administrator, a tenant)
  is not reached by a tenant- or channel-wide or group `publish` or `promote`, nor an attachment's
  `publish`, and is by one naming it; a reservation made while an upload spools is seen under the
  lock.
- The intake: names it mirrors from core and community upstreams are reserved; a grandfathered entry
  keeps mirroring; `"*"` on community keeps pulling through a community name after its first fetch,
  and never takes a tenant's reserved name.
- The CLI's generator against DuckDB's source at v2.0.0 (`legacy_parser` is core) and at a dev
  commit; a list missing a known core name is refused.
- The community sync against a fake repository: new names reserved, a failed read changes nothing,
  off by default.
- The distribution admins: each call allowed, everything else refused.
- The e2e: a version registered with its core names; a wide `publish` grant refused on one of them
  and on a tenant-reserved name, and allowed with a grant naming it.

## Delivery

Three parts, each a pull request:

1. **The server's names** (implemented): the tables and their seed, the process's view and its
   version, a version's core extensions (API, CLI, bootstrap, console, `kista duckdb`), the
   administrator's names, the intake's learning, `"*"` on community, the re-check under the lock,
   the grant explanations, `make reserved` narrowed.
2. **A tenant's names and the distribution admins** (implemented): `tenant_reserved_names` (its own
   migration, with its `min_reader`), the lookup in that precedence, the role and its calls, the
   reaper.
3. **The community sync** (implemented): `reserved.community_sync`.

### As built (part 1)

- Migration 0030 creates the server's tables only; `tenant_reserved_names` comes with part 2.
- The process's view is an atomic snapshot of the list at a `reserved_version`, seeded from the
  compiled lists. A channel read (`GetServeChannel`, which reads the version with the channel row)
  reloads it when the version moved, and the paths that decide on a name (publish, promote, an
  attachment's `publish`, an upstream's entries, the grant explanations, the release list and the
  statistics a name's publishers read) reload it before they check. The channel's snapshot and the
  resolver's cache hold nothing the list decides (`shadows` and `"*"` read the view when asked), so
  a new name invalidates neither.
- A transaction reserves its names first and bumps `reserved_version` once, last
  (`BumpReservedVersion`), so names are always locked before the version and two writers of several
  names cannot deadlock. A name already reserved as asked, with the source, is only read: an intake
  of a known name takes no lock on it (SQL Server's `MERGE ... HOLDLOCK` would hold one to commit).
  A removal deletes the `admin:` sources and then the name only if no source is left, so a source
  another transaction commits meanwhile keeps the name.
- A version's core extensions are refused over tenants' releases unless forced, as an
  administrator's names are; either refusal looks only at names whose kind would change.
- `kista duckdb core-extensions` guards against a misparse by requiring each source it reads (the
  entry tables, `AUTOLOADABLE_EXTENSIONS`, the `.github/config` loads) to give names, and the list to
  hold `icu`, `json` and `parquet`, which every DuckDB version has. It does not compare with the
  names already core: a version may lawfully lack another's (v1.4.1 has no `legacy_parser`). It
  reads the pre-2.0 layout too (`{nullptr, nullptr, false}` ends `internal_extensions[]`; checked
  back to v0.9.2), and a checkout with `-dir` (`-commit` names its commit), which the e2e and the
  deploy scripts use. A commit ref needs `-trust-commit`: codeload serves a fork's commit under
  `duckdb/duckdb`'s name. A tarball is bounded in entries kept (each counting at least 512 bytes),
  bytes kept, and bytes inflated; redirects stay on the host. `version add` refuses the community
  list as a version's core extensions.
- `POST duckdb-versions/{v}/core-extensions` and `version core-extensions` answer the names new to
  the server's list; the bootstrap adds the names of a version it lists that the server does not
  hold as core.
- An attachment's `publish` reads the synced list, without the re-check under a lock a release
  insert makes: it documents a body its publisher released.
- A bundle's source is `bundle:<id>`, the bundle's manifest id.
- An administrator's addition records one `reserved.add` event per name; a version's, one for its
  new names. The intake's learning is not a separate event: the release's records it, and the
  name's source names the upstream or the bundle.
- A community name that becomes core: the intake records shadows for community-built mirrors of it
  from then on (the shadow decision reads the list in the transaction); the backfill of mirrors
  that exist, and listing their tenants, are a follow-up (not in parts 1 and 2).
- The Marketplace package carries v2.0.0's core extensions in its DuckDB choice, read at the tag's
  commit.

### As built (part 2)

- Migration 0031 creates `tenant_reserved_names` (with `added_by`) and sets `min_reader 31`.
- **The lookup** is `store.ReservedFor(tenant, name)`: core or community from the synced view, then
  the tenant's table, then the tenant's upstream entries. Publish, promote, an attachment's
  `publish`, the publishers' release reads and the grant explanations call it; the index, the
  statistics scope and pull-through read the tenant's names and its upstreams' from the channel's
  snapshot (`TenantReserved` beside `Provided`). A reservation bumps every channel of the tenant
  (their release version), so the snapshots follow. The release insert re-checks the tenant's names
  under its locks, beside the server's and the upstreams'.
- A reservation takes the tenant's upstream lock (as an upstream change does) in a transaction that
  refuses a deleting tenant; an upstream entry for a tenant-reserved name needs `allow_reserved`,
  checked under the same lock.
- A reservation needs an active tenant; the holder is 1 to 128 bytes of text without CR, LF or NUL.
  The answer says `changed` (and `previous_holder`); the same holder again is no change and no
  event. The reaper removes a deleted tenant's rows. Editing an upstream entry for a tenant-reserved
  name needs `allow_reserved` too.
- **The distribution admins** are an actor of their own (`distribution-admin:<issuer>|<who>`) with a
  verb no grant gives (`distribute`; `admin` does not imply it). The API admits them on the routes of
  their calls (publishers, a publisher's keys and licence-key mark, grants, the tenant's reserved
  names, `GET /api/v1/reserved-names`); each service call narrows them, tighter than the design
  above after review:
  - publishers: create, list, read (an unmarked one without its keys); mark one only while it holds
    no keys and no grants (otherwise a tenant's publisher with wide grants would become the
    distributor's), unmark; keys only of marked publishers; no removal of a publisher;
  - grants: list and read all; add only `publish`, `promote` and `admin` on a name **reserved in the
    tenant** (any channel scope), never on the tenant, a channel or a group, never `issuer:`; a
    publisher principal must be marked; remove only grants a distribution admin made, or a marked
    publisher's (the tenant's own grants stay its administrators').
  So a distributor's reach in a tenant is the names reserved there. It may reserve any name, and the
  answer names the releases under it (channels included): the Security section's "reads no channel"
  holds for its other calls. A principal may not be both a licence manager and a distribution admin
  (the configuration is refused, and a token whose principals hold both is refused at
  verification); one listed with the server administrators acts as one. Tenant events and sinks
  show `distribution-admin` unless unmasked. The AWS, GCP and Container Apps stacks take
  `server_distribution_admins` beside `server_license_admins`; the Marketplace package does not.
- A name `"*"` took is checked again under the release insert's locks (the server's list, the
  tenant's names, its upstreams' entries): a reservation made while a pull-through miss was queued
  or fetched wins, and the cell is `shadowed`. `"*"` also leaves out names any upstream of the
  tenant lists, not only the channel's.
- A channel created while a reservation commits may keep a snapshot without the name until its next
  change; only the index's provenance view reads that set (pull-through on it needs an upstream,
  whose addition bumps it).
- The console shows a tenant's administrators its reserved names, read only, on the publishers
  page.

### As built (part 3)

- `reserved.community_sync` takes `enabled` (off by default), `interval` (24h; 1h..168h) and `url`: a
  gzip tarball of the repository's default branch as `git archive` writes it, by default GitHub's
  `codeload.github.com/duckdb/community-extensions/tar.gz/HEAD`. The commit in its pax header names
  the source; a tarball without one is refused (a mirror serving `git archive` output has it).
- One replica per interval: the pass takes the lease `kista/reserved/community-sync` for 90% of the
  interval and does not release it. It streams the tarball through the tenants' egress client
  (bounded in size, time and pace; `internal/duckdbsrc` keeps only the `description.yml` files),
  and reserves only the names the server does not hold yet, so a name already core or community
  gains no source per pass. One `reserved.add` event (`sync:community`, actor
  `system:community-sync`) per pass that added names; the version is bumped once.
- A name a tenant has releases under is not reserved by the sync (logged, and `skipped` in the
  event): reserving it would narrow that tenant's wide grants on a third party's say, so an
  administrator decides (`reserved add -force`). More than 100 new names in one pass is refused as a
  changed or tampered source. A name reserved between the read and the transaction is left without
  the sync's source. A failed pass is a `reserved.sync` event with outcome `failed` and an error
  class, as other background jobs record theirs.
- After a restart, or an interval made shorter, the lease taken before runs out first. Names the
  sync reserved stay, as every name DuckDB brought does: a wrong `url` (a fork) adds names nobody
  publishes under, at worst, since a tenant's names are skipped.
- The CI's `network` job reads the real repository.

## Alternatives considered

- **Keep the compiled lists, regenerated per pin.** A server is protected only once kista's pin and
  release catch up with the DuckDB versions it serves.
- **Refuse reserved names outright.** Core builds for a dev pin and a tenant's own build of a core
  extension are legitimate (spec 0008); grants that name the extension already say who may.
- **A distributor's reservation that also says who may publish.** Grants already say that.
- **The server fetching DuckDB's source at registration.** It would give a registry egress to
  github.com it otherwise never needs.
- **Server tokens for a distributor's provisioning.** It works, but holds every power of the server
  for a handful of calls.

## Changes to earlier specs

- **0003**: `version add` takes the version's core extensions.
- **0007**: the endpoints above; `shadows` unchanged (a tenant reservation sets none).
- **0008**: the reserved names are the server's; the generated lists seed them; `make reserved`
  generates only the aliases and DuckDB's keys.
- **0009**: the intake reserves the names it mirrors from core and community upstreams; `"*"` on
  community matches community names; entries are grandfathered; the re-check under the lock reads
  the server's and the tenant's reserved names.
- **0014**: a bundle's intake reserves as an upstream's does.
- **0019**: the reaper removes a tenant's reserved names.
- **0021, 0023**: the distribution admins may mark publishers and manage marked publishers.
- **0024**: the bootstrap's versions take `core_extensions`.
