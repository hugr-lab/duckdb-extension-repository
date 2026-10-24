# Spec 0034: Package types

- **Status**: accepted (2026-10-24); phase 1 implemented
- **Date**: 2026-10-24
- **Author**: vgsml, Claude

## Summary

kista serves one kind of thing today, DuckDB extensions, and every key that holds a name assumes
it. Later (after the release) kista and Enterest are to distribute ADBC drivers as well: mirrored,
published, placed in channels, licensed, reported on, installed by a client of our own. This spec
makes the model ready for that without building it: a **package** is a named thing of one
**package type** (today only `duckdb_extension`), and every place that holds a name holds the pair
(type, name), directly or through a channel. A channel serves one type. The schema and the Go code
say package where the thing is general (tables, columns, the store's API). Nothing a DuckDB client
or an existing API caller sees changes. The ADBC type itself is a later spec.

## Problem

- **Names collide across types.** `snowflake`, `bigquery`, `mysql` and `postgres` are DuckDB
  extensions and ADBC drivers. A grant on `snowflake`, a reservation of it, a licence for it, a
  catalogue entry of it: each would be ambiguous once a second type exists.
- **The code says extension for everything.** Tables (`extension_groups`), columns (`ext_version`,
  `grants.extension`) and the store's and authz's Go API name the general thing after the one type
  there is; a second type would read wrong in every query.
- **Keys harden.** Spec 0033 phase B re-keys licences per (tenant, name, source) and adds key owners
  per (tenant, name, kid). Re-keying them again for a type later costs another migration on tables
  that carry customers' licences; doing it once, before B, costs a column.

## Design

### Terms

- **Package**: a name of one type, with versions; each version has builds (a platform, and for an
  extension the DuckDB version, ABI and C API). Not a table: as an extension today, a package is its
  name in the catalogue, the rules, the grants. "Extension" stays the word for a package of type
  `duckdb_extension` and for everything DuckDB's protocol names.
- **Package type**: `duckdb_extension`, the only one this spec registers. A type decides how a build
  is checked at intake, signed, laid out and served, how its versions order, which names are
  reserved for it and how a licence key is read from it. A later spec adds `adbc_driver`; the Go
  seam for a type's behaviour is designed there, with the second type in hand.
- The schema's column is `package_type`: `kind` is taken on channels (signed, passthrough), upstreams
  (core, community, repository), grants (the principal) and reserved names (core, community).
  Where "any type" is meant, the value is `*` (one encoding everywhere, never NULL).

### Where the type lives

**Keyed by it directly:**

- **Channel**: `package_type`, set at creation (default `duckdb_extension`), never changed. A
  passthrough channel is `duckdb_extension` only.
- **Catalogue entry (build)**: `package_type`. Identity (tenant, type, source, name, body hash);
  slot (tenant, type, source, name, version, platform, slot).
- **Reserved names**: server-wide per (type, name); DuckDB's core and community names, and DuckDB's
  aliases (`reserved.Canonical`), are `duckdb_extension`'s. A tenant's reservations (spec 0030) per
  (tenant, type, name).
- **Licences**: per (tenant, type, name); spec 0033 phase B makes it (tenant, type, name, source),
  and key owners (tenant, type, name, kid).
- **Grants and package-group members**: below.

**Of a channel's type, through the channel** (no column; the channel is in their key):
releases, purged slots, rules (`channel_sources`), follows and their skips, downloads and
installers, usage counts and users, upstream asks, statistics' per-channel rows.

**Of an upstream's type, through its home channel** (no column): the upstream's entries, versions,
platforms, cells, keys. A `repository` upstream mirrors a remote channel of its home channel's
type; how a remote channel advertises its type is the ADBC spec's (today every remote is
`duckdb_extension`).

**Of a build's type, through the build**: licence keys declared in it (`build_license_keys`).

**No type**: bodies and everything keyed by a body hash (blobs, blocks, attachments, feeds'
statements); publishers (a publisher may publish any type); shadows (spec 0009), which only a
`duckdb_extension` channel records or reads.

### Rules that use the type

- A follow takes only its source's names of the channel's type; a rule's name is of its channel's
  type; a placement places, and a promotion (spec 0008) promotes, only into a channel of the
  build's type: another type is refused (`409`).
- A publication into a channel takes the channel's type. One into the catalogue names it in the
  query (`?type=`, default `duckdb_extension`), which a publisher's signed statement (spec 0023)
  covers like the rest of the request; never a header. The intake checks the body by that type's
  rules (spec 0008's for an extension).
- The resolver's snapshot (`Provided`, `TenantReserved`, `Listed`) is keyed by (type, name): an
  upstream's allowlist of one type never reserves or blocks a name of another.
- A licence is served only by a channel of its type: `-/licenses/<name>` and the list filter by the
  channel's type, and a live key is (type, name, kid). The token gains no claim here; the ADBC spec
  adds an optional type claim (absent meaning `duckdb_extension`), and from then an import whose
  named type contradicts the token is refused. An import names its type (`?type=`, default
  `duckdb_extension`); a relay takes its upstream's.
- Attachments (spec 0011) on `…/extensions/{name}/attachments`: the body must be a build of that
  (type, name) in the tenant, the type by `?type=` (default `duckdb_extension`).
- Statistics: the `extension` dimension is (type, name); a tenant's totals group by type.

### Grants

A grant's resource gains a type, beside its channel and name:

- **A grant that names a package** (`extension`) names a concrete type; `*` with a name is refused
  (`400`, and `CHECK (extension IS NULL OR package_type <> '*')`). It covers that (type, name)
  only. A package group's **members** are (type, name) pairs; a group grant (`package_group_grants`)
  has no type of its own: it covers its members.
- **A grant without a name** has `*` (any type, the default) or a type. A typed one covers a
  resource only when the resource has that type and a channel or a name (`r.PackageType == type` and
  `r.Channel != "" || r.Extension != ""`): nothing tenant-level, so creating channels, groups and
  grants, managing issuers and upstreams, reading the log, and listing or importing licences need a
  `*` grant (or a tenant administrator's). Fail closed. With `*` it covers what it covers today.
- **A channel-scoped grant** is of the channel's type in effect; its column holds `*` or the
  channel's type, nothing else.
- `authz.Resource` gains `PackageType`, filled from the channel or from the route's `?type=` (access
  is decided from the path before a body is read, so never from a body). `GrantCovers`, `Reaching`,
  `Allows` and `InstallScope` compare it (`InstallScope` returns `all` only for a grant that covers
  the channel's type); group members are keyed by (type, name); reserved names are checked per
  (type, name).
- **Existing rows**: grants with a name and every group member get `duckdb_extension`; grants
  without a name get `*`. So no existing grant reaches a driver of the same name when drivers come;
  a tenant-wide grant without a name (an administrator's, an install everywhere) keeps meaning the
  whole tenant, as it says.
- New grants default as the existing ones were set: a name means `duckdb_extension`, no name means
  `*`. A grant's `package_type` is omitted in responses (the grants list, `whoami`) and events when
  it is that default for its shape. Bootstrap config (spec 0024) takes `package_type` as an optional
  field, never filled in by its defaulting and left out of a grant's key when default, so the digest
  of every applied bootstrap stays the same.

### Vocabulary

The word "package" is used where the thing is general: the schema and the Go code (below), the
website's concepts, the console's general screens, this and later specs. The API's and the CLI's
names (`extension`, `…/extension-
groups`, `-extension`, `…/extensions/{name}`) do not change in this spec: while there is one type
they are right, and renaming now doubles the surface until 1.0 for nothing a caller gains. The API
and CLI rename, with the old names as aliases, is the ADBC spec's, when a second type makes
"extension" wrong.

The type appears as `package_type` in bodies and responses, omitted in responses when it is the
default, so what an existing caller reads does not change; requests are decoded strictly (an older
server refuses a field it does not know), so a client sends it only when it is not the default. It
appears as `?type=` (default `duckdb_extension`) on the tenant-level routes keyed or filtered by a
name: `licenses[/{name}…]`, `license-keys/{name}…`, the server's and a tenant's reserved names,
attachments, the catalogue's routes (and `catalogue?name=`), `grants?extension=`,
`extension-groups?extension=`, `stats/*?extension=`. On a channel's routes and an upstream's
(`upstreams/{u}/extensions/{entry}`) the type is the channel's: a `?type=` that differs is refused
(`400`). The CLI takes `-type`.

### The rename (phase 1)

The schema and the Go code move to the package vocabulary, with no change in behaviour, in one
migration (0038) with a `min_reader` bump (an older binary would query the old names):

```text
extension_groups            -> package_groups
extension_group_members     -> package_group_members;  extension -> package
extension_group_grants      -> package_group_grants
grants.extension            -> grants.package
ext_version                 -> package_version          (builds, releases, purged_slots, the
                                                         upstreams' versions, downloads, statistics,
                                                         usage and its forwarding: every table that
                                                         has it)
constraints and indexes     *_extension_group*  -> *_package_group* where the dialect can
                                                         rename them (As built, below)
```

- **PostgreSQL**: `ALTER TABLE … RENAME [COLUMN] …`, `ALTER INDEX … RENAME`, `ALTER TABLE … RENAME
  CONSTRAINT`.
- **SQL Server**: `sp_rename` for tables, columns, indexes and named constraints; system-named
  ones keep their generated names.
- **SQLite**: `ALTER TABLE … RENAME TO` and `RENAME COLUMN` (indexes, partial ones included, follow
  the column); an index whose name changes is dropped and created.
- **Go**: the store's types and methods (`Grant.Extension` -> `Package`, `ExtVersion` ->
  `PackageVersion`, `ExtensionGroup*` -> `PackageGroup*` and the like), `authz.Resource`'s
  `Extension` and `ExtensionGroup` -> `Package` and `PackageGroup`, and the general code that uses
  them (the catalogue, placement, rules, grants, groups, licences, usage, statistics). Code that is
  about DuckDB extensions keeps its words: the extension file and its checks (`internal/extfile`),
  DuckDB's sources and aliases, the serving layout, upstreams of DuckDB's kinds.
- **Not renamed**: the API's JSON fields, routes and query parameters, the CLI's flags and commands,
  events and their subjects, bundle manifests, config keys (the rename there is the ADBC spec's,
  with aliases); `name` columns (they already are the package's name).

### Data model (phase 2)

One migration (0039), after the rename, in the three dialects:

```text
channels                + package_type not null default 'duckdb_extension'
builds                  + package_type not null default 'duckdb_extension'
                          ux_builds_source  (tenant_id, package_type, source, name, body_hash)
                          ux_builds_slot    (tenant_id, package_type, source, name,
                                             package_version, platform, slot)
                                             where not legacy_duplicate and source <> 'tenant'
                          ix_builds_source_name    (tenant_id, package_type, source, name,
                                                    created_at)
                          ix_builds_source_created (tenant_id, package_type, source, created_at)
grants                  + package_type not null ('duckdb_extension' with a package, else '*');
                          check (package is null or package_type <> '*')
package_group_members   + package_type not null default 'duckdb_extension';
                          pk (group_id, package_type, package);
                          ix_package_group_members_tenant (tenant_id, package_type, package)
reserved_names          + package_type; pk (package_type, name)
reserved_name_sources   + package_type; pk (package_type, name, source);
                          fk (package_type, name) -> reserved_names (the old fk on name dropped
                          first: fk_reserved_name_sources_name on SQL Server, the system-named
                          reserved_name_sources_name_fkey on PostgreSQL)
tenant_reserved_names   + package_type; pk (tenant_id, package_type, name)
licenses                + package_type; pk (tenant_id, package_type, name)
```

- **`min_reader` bumps.** An older replica's `ON CONFLICT (name)` on reserved names, and its
  licence and member upserts, fail against the new keys on PostgreSQL and SQLite; and an older
  binary must not read rows of a type it does not know. Spec 0033's phases bump it too.
- **SQL Server**: the inline primary keys have system names; they are dropped by the dynamic lookup
  0034_build_sources uses. New columns take the schema's `COLLATE Latin1_General_100_BIN2`.
- **SQLite**: a primary key change is a table rebuild, under `-- +foreign_keys_off` (0034).
- Every existing row gets the values above, so every channel serves what it served and every key
  holds what it held.
- **Unknown types fail closed.** A binary reads only the types it registers: a row of another type
  (written by a newer binary during a downgrade window that `min_reader` did not stop) matches no
  grant, serves nothing, places nothing.
- The Go side: `internal/pkgtype` lists the registered types and validates a value; the store's
  types gain `PackageType`; every lookup by name takes the pair.

### Bundles

A bundle (spec 0014) carries sources (`Manifest.Sources`, `Plan.Sources`), each an upstream's, so
the type belongs to a source, not to each entry: `Source.package_type` and
`PlanSource.package_type`, omitted (required omitted) when `duckdb_extension`. Manifests and plans
are decoded strictly, so a v0.1.4 importer or exporter reads every extension bundle and plan a newer
one writes; a source of a type an importer does not know is refused.

### Events

Events that name a package without a channel (`catalogue.*`, `license.*`, `reserved.*`, grants and
groups) carry `package_type` when it is not the default. Subjects (`ext:<name>`, `license:<name>`,
`reserved:<name>`) are unchanged for `duckdb_extension`; another type's subjects are its spec's.

### Delivery

Two phases, each a PR, both before spec 0033's phase A2 (so B's licence keys and key owners are
born with the type): **phase 1**, the rename (0038, no change in behaviour); **phase 2**, the types
(0039).

### As built (phase 1)

- **Migration 0038** (`0038_package_names`, `min_reader` 38) in the three dialects renames the three
  group tables, `extension` to `package` in `package_group_members` and `grants`, and `ext_version`
  to `package_version` in the nine tables that have it: `builds`, `releases`, `purged_slots`,
  `upstream_versions`, `download_counts`, `download_installers`, `usage_counts`, `usage_users`,
  `usage_received` (the slot keys `ux_builds_slot`, `ux_releases_slot` and `pk_purged_slots`
  follow the column; the system-named releases unique that spelled `ext_version` was already
  replaced by `ux_releases_slot` in 0034). The named constraints and indexes of the group tables
  follow.
- **PostgreSQL**: besides the named ones, a `DO` block renames every constraint whose name still
  spells the old words: the system-named primary keys of the groups and group grants and the
  groups' foreign key to tenants (with their indexes), and PostgreSQL 18's not-null constraints.
- **SQL Server**: `sp_rename` throughout; renaming a column that is a key of the filtered
  `ux_builds_slot` works (its filter names other columns). The system-named primary keys and
  foreign key of the group tables (`PK__extensio__…`, `FK__extension__…`) keep their generated
  names, which do not spell `extension_group`.
- **SQLite**: constraint names inside a table's definition (`uq_extension_groups_name`,
  `pk_extension_group_members`, …) keep their old spelling: SQLite cannot rename them without
  rebuilding the tables, and nothing addresses them. The five group indexes are dropped and created.
- **Go**: the store (`PackageGroup`, `InsertPackageGroup`/`GetPackageGroup`/`ListPackageGroups`/
  `DeletePackageGroup`, `MaxPackageGroups`, `Grant.Package`/`PackageGroupID`/`PackageGroupName`,
  `PackageVersion` on builds, releases, statistics and usage rows and keys), `authz.Resource`'s
  `Package` and `PackageGroup`, `tenants.GrantOn` and the tenants service's group methods
  (`CreatePackageGroup` and the like), and their callers. Kept: the API's and the configuration's
  wire structs (their fields mirror their JSON and YAML names, `extension`, `extension_group`), event
  kinds and payload keys, the CLI's commands, and `release.ValidExtVersion`, the grammar of a
  version as a segment of DuckDB's paths (a type's version rules are the ADBC spec's).
- **Tests**: `TestMigrationPackageNames` migrates a populated database at level 37 on the three
  dialects and reads every row back through the store (grants with and without a name, a
  publisher's, a group with members and a grant on it, a build, its release, a purged slot, an
  upstream's versions, downloads, installers, usage counts, users and received usage), and checks
  that no table, column, index or constraint keeps an old name (SQLite's in-table constraint names
  aside). The migration tests of earlier levels keep the old names they were written for.

## Security

- **No widening.** Every existing grant with a name, and every group member, is
  `duckdb_extension`: none reaches a package of a type added later. A grant that names a package
  must name its type. A typed grant without a name covers only that type's packages and channels,
  and nothing tenant-level.
- **Reservations, licences and keys** are per type: a name reserved for DuckDB protects the
  extension and does not unreserve or reserve a driver; a licence is served only by a channel of its
  type and a key is live per (type, name, kid). A second type brings its reserved names in its spec,
  before any of its packages can arrive.
- **Fail closed** on an unknown type: refused at every route (`400`) and never matched when read.
- **Existence.** The type adds no answer to an anonymous or install caller beyond what a channel
  already answers.

## Testing

- Phase 1: the store suite on the three dialects, unchanged in what it asserts, runs on the renamed
  schema; the migration on a populated database keeps every row (groups, members, grants, builds,
  releases, slots, statistics, usage); the e2e cases unchanged.
- Phase 2: the store suite on the three dialects: migration 0039 on populated databases (grants with and without
  a name, members, reservations, licences, builds); two entries of one name in two types do not
  collide in the catalogue, the slots, the reservations, the licences (a test-only second type);
  rows of an unregistered type are ignored.
- Authz: a named grant covers its type only; `*` with a name refused; a typed grant without a name
  covers that type's channels and packages and no tenant resource; members by (type, name);
  reserved names per type; `InstallScope` by the channel's type.
- Bootstrap: an applied bootstrap's digest is unchanged after the upgrade.
- API and CLI: `?type=` and `package_type` defaulting to `duckdb_extension`; responses unchanged for
  `duckdb_extension`; an unknown type refused; a promotion or placement across types refused;
  bundles round-trip with v0.1.4's manifest decoder.
- e2e: the DuckDB cases unchanged.

## Alternatives considered

- **Add the type with the ADBC spec.** Re-keys licences, key owners, reservations and the catalogue
  a second time, after phase B has filled them. Rejected for the cost asymmetry.
- **One table of packages** (name, type, owner) referenced by id. A package has no state of its own
  in kista today; every name-keyed table would move to an id for nothing. Kept open for when a
  package gains state.
- **Type in the name** (`adbc:snowflake`). Breaks name validation, DuckDB paths and every
  comparison by name.
- **Mixed channels.** Two layouts, two signature formats and rules that ask the type: the owner
  chose one type per channel (2026-10-24).
- **Untyped grants meaning every type.** Keeps rows smaller, but every existing grant on
  `postgres` would reach the `postgres` driver the day it arrives.
- **Renaming the API and CLI now**, with aliases. A second set of names until 1.0 while one type
  exists; deferred to the ADBC spec (the owner agreed, 2026-10-24).
- **Keeping the old table and Go names.** Cheaper now, but every query and type would name the
  general thing after one type; the owner chose the rename, as its own step (2026-10-24).

## Follow-ups

- **ADBC drivers** (a later spec, after the release): the `adbc_driver` type (intake checks, a
  detached signature by the channel key, a dbc-compatible index and packages, semver ordering, its
  reserved names); a `dbc` upstream (Columnar's PGP signature verified at intake, re-signed, a
  private registry by API key) and how a remote channel advertises its type; licence formats: ours
  (Enterest's JWS with a type claim, an ADBC SDK on the extension SDK's model) and a vendor's opaque
  licence per source (Columnar's `columnar.lic`); our client (install from a channel, verify, write
  the ADBC manifest, fetch licences, the tenant's tokens); the API and CLI rename with aliases.
- **Enterest**: its Extension becomes a Package with a type (its own spec; Enterest is not
  released, so the rename is free there); its build pipeline per type.
