# Spec 0017: Extension groups

- **Status**: draft
- **Date**: 2026-10-10
- **Author**: vgsml, Claude

## Summary

An **extension group** is a named, explicit set of extension names in a tenant: `hugr-platform` =
`tresor`, `hugr_node`, `duckdb_acl`. A grant may name an extension group instead of one extension,
in every channel or in one, so one grant (`role:kc|platform-team`: `admin` on `hugr-platform`)
covers a family of extensions, and a name added to the group is covered at once. Only a tenant
administrator changes a group's members, and every change is an event. Groups are not patterns: a
name is in a group because someone put it there. One phase, one pull request; the console's screens
for groups are spec 0015's phase 2.

"Extension group" is always written in full (routes, fields, events, CLI): spec 0006's principals
already have a `group:` kind, the IdP's groups of people.

## Problem

Spec 0006 grants on the tenant, a channel, or one extension name. A tenant mirroring the community
repository (360 names) and publishing its own builds has one coarse choice and one fine one:

- a tenant- or channel-wide grant gives a team every extension, private ones included;
- one grant per name and principal: ten teams times thirty extensions is three hundred grants
  (against spec 0007's limit of 1000 per tenant), and a new extension in the family needs a grant
  per team, which is forgotten.

The console's Access screens (spec 0015 phase 2) need a unit between "one extension" and "a
channel". A channel is already a set of extensions, but it is a DuckDB repository with its own key
and URL: making it the unit of access mixes what is served with who may use it.

## Design

### The model

- An **extension group** belongs to a tenant: a `name` (`[a-z0-9][a-z0-9-]{0,62}`, a path segment,
  unique in the tenant, immutable: renaming is creating another and granting again), a
  `description` (at most 256 bytes of printable text, as a tenant's display name), `created_at`,
  `created_by`, and a `version` that only a description change moves.
- Its **members** are extension names (spec 0008's grammar, `release.ValidName`), each with
  `added_at` and `added_by`. A name may be in several groups, and may be added before anything is
  released under it. Members are names, not releases: a grant on the group covers the name in
  every channel the grant reaches, and nothing in a channel it does not reach.
- **Limits**: 100 groups per tenant, 1000 members per group, 10000 members per tenant. Grants on
  groups count toward spec 0007's `MaxGrants` (1000 per tenant) with the others. Each limit is
  counted inside the writing transaction, under the tenant's auth lock (`kista/tenant-auth/<id>`,
  which grant writes already take), so concurrent writes cannot pass one together; removing members
  or grants never hits a limit.
- No patterns, no nesting, no labels.

### Grants on an extension group

A grant's resource gains a fourth kind (amending spec 0006): the tenant; a channel; an extension
(every channel or one); **an extension group (every channel or one)**. Channels are compared as
before (by id in the row; channels are never renamed or removed).

**Stored apart.** Grants on groups live in their own table, `extension_group_grants`, not in
`grants`. Every evaluator today reads a `grants` row without an extension as "every extension"; a
group grant written there would be read so by any code that does not know about groups, including
a replica of an older version still running while a newer one migrates. In a table of its own, code
that does not know about groups never sees it: it covers nothing there, which is the safe failure.
So migration 0016 is additive and keeps `min_reader` where it was (spec 0003): an older binary
ignores group grants and grants less, never more.

**What a caller holds.** `GetTenantAuth` also reads the groups, their members (one set per group,
built once and cached with the tenant's auth) and the group grants: `TenantAuth` gains `Groups` and
`GroupGrants`. The evaluators stop taking `[]store.Grant`: they take `auth.Held`, what
`TenantAuth.For(principals)` returns once per request, the caller's own grants of both kinds, each
group grant pointing at its member set. A group grant covers a name by a set lookup, so the cost
stays linear in the caller's grants, as spec 0007's `MaxGrants` intends; nothing is expanded per
member. Changing the signatures is deliberate: every call site (`auth.Allows` in the DuckDB routes,
`auth.InstallScope` and the `holds_*` flags in the index, `authz.Covers` in management, publication
and the index's provenance, the statistics scope) fails to compile until it is moved, so none is
left reading only the plain grants. The statistics scope keeps its own rules (spec 0010) on `Held`:
a group grant reads its members' statistics, never "any extension"; when its names would exceed
1000, the store reads unfiltered and the scope filters in memory (a `name IN (…)` list stays under
SQL Server's parameter limit). Lists of grants (`GET …/grants`, `whoami`, `grant list`) show group
grants as stored, never per member. The lock-out count (spec 0007) reads the `grants` table only, so
group grants never count.

**Per verb**, a group grant covers each member as a grant on that extension in the same channel
scope would, with two exceptions:

| Verb | Through a group |
| --- | --- |
| `install`, `admin` | as on the extension, reserved names included |
| `publish`, `promote` | as on the extension, except a **reserved name**: the route's `Resource.Reserved`, the same value that decides it for tenant- and channel-wide grants today (spec 0008; the publish path also rechecks the tenant's upstream-provided names under the insert locks, spec 0009). Only a grant on the extension itself reaches one; a group never does. Promotion's two halves are decided separately, each by the same rule |
| `audit` | refused: the tenant's log is granted tenant-wide only (spec 0010) |

An `issuer:` grant on a group carries `install` only, as on an extension. A publisher grant (spec
0008) may name a group: a CI identity publishing a family.

**Reading a group.** `Resource` gains `ExtensionGroup`, and `Covers` a case for it, decided on the
stored group grants (so an empty group is readable too): a tenant-wide `admin` grant, or an `admin`
grant on that group (in any channel), lets the caller read the group and its members. Neither
changes the members, which would let a group's administrator widen their own rights, nor the
group's grants (grants are tenant administration, spec 0007). `whoami`'s `holds_admin` is true for
an `admin` grant on a group too, so spec 0015's console gate lets a group's administrator in.

**Consistent reads.** `GetTenantAuth` runs separate queries outside a transaction. With members
read apart from grants, a read racing a revoke and an add could combine an old grant with a new
member, a state that never existed. Every write to a table it reads bumps the tenant's
`auth_version` in the same transaction (issuers, audiences, grants, publishers and their
credentials already do; groups, members and group grants will). It reads the version before and
after, and reads again when it moved; after three tries it reads inside a transaction under the
tenant's auth lock, which writers hold, so churn slows a request but never fails it. It returns the
version it read. The cache keeps one entry per tenant, served when its version is at least the one
the caller's tenant row carries, and replaced only by a higher one, so member writes do not pile up
copies and interleaved requests do not thrash it.

**Publishing in flight.** As with revoking a grant today, a publish authorised before a name left
its group can still complete; the rights are decided when the request starts (and the reserved
recheck runs under the insert locks).

### Data model

Migration `0016_extension_groups` on all three dialects (SQL Server columns `nvarchar` with
`Latin1_General_100_BIN2`, named constraints, as in 0004 and 0006):

```
extension_groups         id, tenant_id, name, description, version, created_at, created_by
                         PK (id); UNIQUE (tenant_id, name); UNIQUE (id, tenant_id); FK tenant
extension_group_members  group_id, tenant_id, extension, added_at, added_by
                         PK (group_id, extension); FK (group_id, tenant_id) → groups ON DELETE CASCADE;
                         index (tenant_id, extension)
extension_group_grants   id, tenant_id, group_id, issuer_id, publisher_id, kind, value, channel_id,
                         verbs, created_at, created_by
                         the checks of grants (0006: one of issuer and publisher, publisher kind);
                         FK (group_id, tenant_id) → groups (NO ACTION); FK issuer and FK
                         publisher ON DELETE CASCADE; FK channel (NO ACTION);
                         indexes (tenant_id), (group_id), (issuer_id), (publisher_id)
```

Grant ids are UUIDs in both tables. `GET` and `DELETE …/grants/{id}` try `grants`, then
`extension_group_grants`; `404` means neither. A list merges both before sorting by `(created_at,
id)` and paging, so its cursor is as stable as today's. Removing an issuer record or a publisher
removes their grants on groups (the cascade, so an older replica's removal, which knows only
`grants`, still succeeds). `MaxGrants` counts both tables in both insert paths; during a rolling
upgrade an older replica counts only `grants` and may exceed it, an accepted limit of the rollout.
An identical grant is not added twice (principal, group, channel, verbs).

Every write (group, member, group grant) runs under the tenant's auth lock and bumps the tenant's
`auth_version`, so a change applies at the next request on every replica (spec 0006).

**Removing a group** that a grant names is refused (`409`, "revoke its grants first"), checked by
the service under the lock: removing it would silently take rights from people, and the grants are
where those rights are visible. Its members go with it, and its removal event lists them.

### API

Shapes: an extension group is `name, description, members (count), created_at, created_by, etag`,
plus `grants (count)` for tenant administrators; a member is `name, added_at, added_by`.
`created_by` and `added_by` follow spec 0007 (a tenant administrator sees `server` for a server
administrator). Lists are paged by name with spec 0007's cursor and `limit`, and take spec 0015's
`?prefix=`.

| Route | Who |
| --- | --- |
| `GET /api/v1/tenants/{t}/extension-groups[?prefix=&extension=&cursor=&limit=]` | the groups the caller may read: all for a tenant admin, those it holds `admin` on otherwise; `?extension=` (tenant admin): the groups holding that name |
| `POST …/extension-groups {name, description}` | tenant admin; `201`, `Location` |
| `GET …/extension-groups/{g}` | tenant admin, or `admin` on the group |
| `POST …/extension-groups/{g}/description {description}` (`If-Match`) | tenant admin |
| `DELETE …/extension-groups/{g}` (`If-Match`) | tenant admin; `409` while a grant names it |
| `GET …/extension-groups/{g}/members[?prefix=&cursor=&limit=]` | tenant admin, or `admin` on the group |
| `POST …/extension-groups/{g}/members {add, remove}` | tenant admin |

- **Members** are changed by one route, like a channel's DuckDB versions: `add` and `remove` hold
  together 1-100 extension names, validated first (an invalid name refuses the whole request,
  `400`; so does a limit). Names already present, or not members, are skipped rather than refused:
  a batch from a list of 360 names should not fail on one that is already there. The answer is
  `{added, removed}`, what actually changed. Member changes do not move the group's `version`, so
  its `ETag` guards the description and the removal, not the members.
- **Grants**: `POST …/grants` takes `extension_group` (with an optional `channel`); with
  `extension` too it is `400`, and so is a group that does not exist (a reference in the body, spec
  0007). A grant's shape gains `extension_group`. `GET …/grants` takes `?extension_group=` (the
  grants on that group) and `?extension=` (every grant that reaches the name: tenant-wide, on its
  channel, on the extension, on a group holding it). With `?extension=` each row carries `via`
  (`tenant | channel | extension | extension_group`) and `verbs_on_extension`: the verbs that
  actually apply to that name in the row's channel scope, from one per-grant helper that `Covers`
  uses too (an issuer-wide `admin`, or a group's or channel's `publish` on a reserved name, is left
  out). Filters combine with `?principal=`; `?extension=`
  takes `release.ValidName`, `?extension_group=` the group grammar (`400` otherwise).
- `whoami` lists a matching group grant with its `extension_group`, like any grant, and so does a
  publisher's `whoami` (spec 0008).
- A group the caller may not read, or that does not exist, is `404` (from the path, spec 0007).
- Routing: the authorizer's resource takes the path's `{g}` as `ExtensionGroup`, and request logs
  carry it like `{c}` and `{ext}`.

### CLI

```
kista admin extension-group add <tenant> <group> [-description …]
kista admin extension-group list <tenant>
kista admin extension-group show <tenant> <group>          # the group, its members, its grants
kista admin extension-group description <tenant> <group> <text>
kista admin extension-group remove <tenant> <group>
kista admin extension-group members <tenant> <group> [-add a,b,…] [-remove c,…]
kista admin grant add <tenant> -principal … -verb … -extension-group <group> [-channel …]
kista admin grant list <tenant> [-extension-group …] [-extension …]
```

The CLI and the API call the same services (`internal/tenants`), so the rules are the same; the
CLI sends members in batches of 100.

### Events

Spec 0010's identity events gain:

- `extension_group.add`, `extension_group.remove` (data `name`, `description`; the removal also
  `members`, the count: the names are in `extension_group.members` events written before it, by
  100, since a thousand names exceed spec 0010's 16 KiB of data);
- `extension_group.change` (`description` with the old and new value, spec 0010's rule for changes);
- `extension_group.members` (`added`, `removed`: the names actually changed; nothing changed, no
  event), as `channel.versions` does for a channel's DuckDB versions.

The subject is `extension-group:<name>` (added to the audit catalogue's subject forms). `grant.add`
and `grant.remove` gain `extension_group` (the name), in their data and in the catalogue. Who held what at a member change is read from
the `grant.*` events, which carry the group.

### Console

Spec 0015 phase 2 draws the extension groups list, the group page (members paged with a prefix
search, "Who may use it", recent changes) and the group as a target in the Access forms. When
members are added to a group whose grants include `publish` or `promote`, or more than one
principal, the console says who gains what. This spec adds no screens.

## Security

- **Membership is authority.** Adding a name to a group grants at once whatever the group's grants
  hold, `publish` to a publisher included. So only a tenant administrator (or a server
  administrator, or the CLI) changes members, every change is an event naming who and which names,
  and `admin` on a group never includes it. The deployment guidance: one publisher per group.
- **Fails closed across versions.** Group grants are stored apart from `grants`, so code that does
  not know them (an older replica) grants nothing through them, and the evaluators' new signature
  leaves no call site on the old path.
- **No patterns.** A pattern (`hugr_*`) would let a newly published or mirrored name fall under a
  grant nobody chose, and with `publish` it would let a publisher claim names. Explicit names only.
- **Reserved names** (core, community, the tenant's upstreams') are reached for `publish` and
  `promote` only by a grant naming the extension, so a group cannot become a way to shadow them.
- **The decision still comes from the path** (spec 0006): tenant, channel and name are known before
  anything is resolved; the member sets are the tenant's cached auth, the same for every request;
  the index applies the view in memory, with `?prefix=` after it (spec 0015). The cached auth is a
  consistent read (the version checked around it).
- **No existence leaks.** Group routes are management routes (fresh tokens, spec 0007): a caller
  without the right gets `404`, the same as for a group that does not exist. Members may be names
  never released; only those who may read the group see them, and nothing says whether a member is
  released anywhere. A group's administrator sees no grant counts.
- **Statistics** follow the members: a group grant reads its members' statistics, never "any
  extension" (spec 0010: a query never tells what exists beyond the caller's scope).
- **Tenant isolation**: groups, members and group grants carry the tenant, with composite foreign
  keys, as issuers and channels do.
- **Bounded cost**: the limits above, member sets built once per cached auth, evaluation linear in
  the caller's own grants, one cached copy per tenant, and the index's view as a set.

## Testing

- Store suite on PostgreSQL, SQL Server and SQLite: groups, members, group grants; the limits,
  including concurrent writes; the cascade of members; issuer and publisher removal taking group
  grants; the duplicate check; `MaxGrants` over both tables; the auth version bump; the `409` on
  removing a granted group; two "replicas" migrating 0016 at once; `Reset` clearing the new tables.
- `GetTenantAuth`: groups and member sets; a version moved during the read is read again, and
  under churn the locked read; the cache served at an equal or higher version, never replaced by a
  lower one; every write to a table it reads bumps the version.
- Evaluators with group grants in every channel and in one: `auth.Allows`, `auth.InstallScope`,
  `authz.Covers` for every verb, the reserved rule (a core name and an upstream-provided name),
  promotion with one half through a group, `audit` refused, `issuer:` install only, a removed
  member covering nothing; reading an empty group; the statistics scope, including more than 1000
  names; the lock-out count ignoring group admin grants; `holds_admin` for a group administrator;
  an older binary's view (only the `grants` table) granting nothing through a group.
- API: the routes and who may call them (`404` for others), the filtered group list for a group
  administrator, `?prefix=` and cursors, the members batch answers, `If-Match`, the grant filters
  with `via` and `verbs_on_extension`, events written with their data.
- CLI: the commands against the shared services.
- End-to-end with the pinned DuckDB: a private extension installs with a token whose only grant is
  on a group; a group grant in channel A installs from A and not from B; a name added to the group
  installs at the next request and, removed, gets `404`; the index lists a group-covered private
  name.

## Alternatives considered

- **Group grants in the `grants` table** (a nullable `group_id`): simpler joins, but every evaluator
  and an older replica read such a row as tenant- or channel-wide. Rejected (stored apart).
- **Name patterns** (`hugr_*`, prefixes): no list to maintain, but a published or mirrored name
  falls under grants nobody chose.
- **Labels on extensions** selected by grants: the same problem, and labels would sit in release
  metadata a publisher controls.
- **Channels as the unit of access**: a channel is a DuckDB repository with its own key and URL;
  splitting channels to split access multiplies keys and `CREATE EXTENSION REPOSITORY` lines.
- **Groups scoped to a channel**: a grant already carries the channel.
- **Nesting groups**: little gain over adding names to two groups, and it makes who-may-what hard
  to read.
- **Groups of principals, or named bundles of verbs (roles)**: the IdP's roles and groups are
  already principals (spec 0006); what was missing is a set of extensions.
- **Let a group's `admin` manage its members**: it would add names they hold no right on. A later
  spec may add a dedicated verb that only a tenant administrator grants.
- **Removing a group removes its grants** (as removing an issuer record does): rights would vanish
  without a grant being revoked. Refused instead.

## Amends

- Spec 0006: a grant's resource may be an extension group (stored in its own table).
- Spec 0007: `MaxGrants` counts both tables; the grant routes take and show `extension_group`;
  `whoami`'s `holds_admin` includes a group `admin` grant.
- Spec 0010: the `extension_group.*` events and subject; `grant.*` data gains `extension_group`.
- Spec 0015: the console gate admits an administrator of a group.

## Follow-ups

- The console's group and Access screens (spec 0015 phase 2), the index showing an extension's
  groups to tenant administrators, and the summary of principals with grant counts for the Access
  screen, which phase 2 owns.
- A verb letting a group's owners manage its members, if teams ask for it.
