# Spec 0038: Tenant limits

- **Status**: implemented (PR 1: the limits; PR 2: runs and the console)
- **Date**: 2026-10-29
- **Author**: vgsml, Claude

## Summary

A server administrator sets limits per tenant on what the tenant's own administrators, publishers
and upstreams can make it grow by: the bytes of its catalogue, and the number of its channels,
upstreams and publishers. A tenant's own value replaces a server-wide default from the
configuration, which is only a backstop; a value can be "unlimited". Every intake and create is
checked, exactly, except a bundle import and the bootstrap. A tenant's administrators read their
limits and what they use. Growth past a limit is refused with a `429` of its own type naming the
limit; nothing that exists is removed when a limit is lowered.

This is plan item P4 (needed before production). It is what a hosted service needs for
self-service tenants: Enterest's organisation plans (its plan item E7) set these limits through its
provisioning worker, which is a kista server administrator. The defaults are unlimited, so a
server that sets nothing keeps today's bounds, with two visible changes: a tenant holds at most
1,000 channels (a new ceiling), and the 101st upstream and the 201st publisher are refused with
`429 tenant-limit`, where they used to get `400 invalid` (only the bootstrap and a bundle import
still meet the `400`).

## Problem

A tenant's administrators work on their own, and on a hosted service they are customers. Today
some of what they can make grow is bounded, some is not:

| What grows | Bound today | Who makes it grow |
| --- | --- | --- |
| Catalogue bytes (bodies of builds, icons) | per source, only the builds placed nowhere: `catalogue.max_bytes` (100 GiB) and `catalogue.max_entries` (20,000); an administrator's add is not bounded; placed builds are never counted | the tenant's publishers and administrators, its upstreams' runs and fetches |
| Channels | none | the tenant's administrators |
| Upstreams | 100 (`store.MaxUpstreams`) | the tenant's administrators |
| Publishers of its own | 200 (`store.MaxPublishers`) | the tenant's administrators |
| Distributors' publishers | 10 (`store.MaxDistributorPublishers`, spec 0037) | distribution admins |
| Issuers, grants, groups, feeds, sinks, attachments, blocks, publisher keys | fixed constants (16, 1,000, …) | the tenant's administrators |
| Channel keys | at most a few live per channel; only a server administrator adds a key | server administrators |

So:

- **Bytes are unbounded per tenant.** The per-source cap counts only the builds placed nowhere.
  Builds placed in channels are not counted, and a tenant can make up to 300 sources (200
  publishers and 100 upstreams). A tenant administrator can point an upstream at a server they run
  and mirror any amount of data, or publish placed builds without end. On a hosted service the
  operator pays for that storage.
- **Channels are unbounded.** Each one is a row, an index to render, and a snapshot in each
  replica's cache.
- **Every bound is the same for every tenant.** A hosted service cannot give a free plan less
  than a paid plan, and cannot give one customer more without changing kista's code.

## Design

### The limits

Four limits, each on one tenant:

| Limit | Counts | Ceiling (`limits.ceilings`, the amendment below) |
| --- | --- | --- |
| `bytes` | the tenant's **own** catalogue bytes (below) | none |
| `channels` | the tenant's channels | `channels`, 1,000 by default |
| `upstreams` | the tenant's upstreams | `upstreams`, 100 by default |
| `publishers` | the tenant's own publishers (not distributors') | `publishers`, 200 by default |

- The ceilings are the server's configuration, not constants (amended 2026-10-31, below). As first
  written they were constants that "bound queries and caches". That reason holds for the bounds of
  one request or one answer, which stay in code; a tenant's counts only make queries and caches grow
  linearly, and the operator who raises a ceiling accepts that cost. A limit's value is at most its
  ceiling, and "unlimited" for a count means the ceiling.
- The other bounds in the Problem's table are configured ceilings too (the amendment), not tenant
  limits: a plan has no reason to differ in them. A later limit is added by the same pattern.
- **Distributors are outside the limits.** The builds and icons of distributors' publishers'
  sources, live or removed, are never counted and never refused by a tenant limit. Distributors'
  publishers are counted apart, under their own cap of 10 (spec 0037). A customer's full quota must
  never stop the distributor from delivering what the customer bought or chose. A distributor's
  sources stay bounded per source by `catalogue.max_entries` and `catalogue.max_bytes` (spec 0033).
- **Who is checked.** Every actor is checked, server administrators included: a tenant
  administrator, a publisher, a server administrator's call, an upstream's run and a lazy fetch
  (both act as the system actor `upstream:<tenant>/<upstream>`). A server administrator who needs
  more raises the limit first, and that change is audited. Only two intakes are exempt: a **bundle
  import** (a server administrator's only, for air-gapped sites, spec 0014) and the **bootstrap**
  (`authz.Bootstrap`, spec 0024). They are counted but never refused, and meet only the ceilings
  (`400 invalid`). Both mark their calls' context (`store.WithoutLimits`: the bundle import's
  intake, and the bootstrap's apply, whether `kista serve` applies it as `system:bootstrap` or
  `kista admin bootstrap` as the OS user), and every check reads the mark; no actor is compared.
  `room`'s `admin` flag (`o.Publish == nil`, true for a tenant administrator's add too) keeps
  meaning only the per-source cap's exemption.

### The bytes a tenant uses

A tenant's own bytes are the sum of:

- `body_len` of each **distinct body** among the tenant's own builds that the collector keeps
  (spec 0016): a build that is landed, or withdrawn, or has a release. A build the collector will
  delete (no release, not landed, not withdrawn: a purged release's, or an intake's that never
  landed) is not counted; its room returns at once, and its blob goes at the next collection. A
  kept build is the tenant's own unless its source is a distributor's publisher, live or removed:
  `NOT EXISTS` against `publishers` (`distributor IS NOT NULL`) and against
  `removed_distributor_publishers`, matched on the source's publisher id. This differs from
  `SourceUsage` (spec 0033) on purpose: the per-source cap bounds a source's backlog, the builds
  placed nowhere, kept or not; the tenant limit bounds what the tenant holds, placed or not.
- `icon_bytes` of each entry of the tenant's own sources (spec 0036), while the entry still has a
  build. On SQL Server the column is summed as `SUM(CAST(icon_bytes AS bigint))`: an `int` sum
  overflows at 2 GiB. The same latent overflow in `SourceUsage` (`internal/store/rules.go`) is
  fixed with it.

**Distinct bodies.** A body in two of the tenant's sources counts once: a successor's copies of
releases (`TakeReleases`, spec 0033), the same file published by two publishers, and a mirror of
what the tenant also publishes add no bytes. This matches the storage, which keeps one blob per
body in a domain (spec 0005), and an idempotent re-publish is never growth. A body shared with
another tenant counts in each tenant: the figure is the tenant's, not the server's, and says nothing
about what other tenants hold. Attachments are not counted: they are bounded by count and size
already (`MaxTenantAttachments`).

A tenant frees bytes by removing builds: purging a yanked release (spec 0016) and removing builds
placed nowhere from the catalogue (spec 0033). Withdrawn builds, which are never purged, belong to
distributors' sources only (spec 0033 phase B), so they never count here.

**Reading it.** Migration 0047 stores `body_len` on `builds` (filled from `blobs` for existing rows;
every insert sets it, a copy copies it). A new index on `builds (tenant_id, body_hash)` carries the
columns the filters read: `body_len`, `source`, `landed`, `withdrawn_at` and `id` (`INCLUDE` on
PostgreSQL and SQL Server; key columns on SQLite). The figure is one `SUM` over the distinct bodies,
and its real cost is:

- one pass over the tenant's entries in that index (no table reads);
- for each build neither landed nor withdrawn, one probe of `ix_releases_build` for the `EXISTS`
  on releases;
- for each build of a publisher's source, two primary-key probes for the distributor `NOT EXISTS`
  (`publishers`, `removed_distributor_publishers`);
- one `SUM` over the tenant's `entry_meta` rows, with an `EXISTS` probe per entry into the new
  index for "the entry still has a build".

It is linear in the tenant's builds. When a tenant's `bytes` is unlimited the figure is never
read, neither by the checks nor by a run. Each intake still reads the tenant's row (a shared lock,
in the build's insert and when a build is kept) to learn that its limit is unlimited.

### Where each limit is enforced

- **`bytes`, exactly, where a build becomes kept.** Only kept builds count, and a build becomes
  kept in two places only: when it lands in the catalogue (`SetLanded`) and when it gets a release
  (`InsertRelease`). Both check, under the lock `TenantBytesLock(tenant)`
  (`kista/tenant-bytes/<tenant id>`, the shape of the tenant's other locks), the tenant's limit
  read through `lockedTenantQuery` (a shared lock on the tenant's row) against the figure, and
  refuse when `used + growth > limit`. The growth is the build's `body_len` when the tenant does
  not count its body yet, plus its entry's icon when the entry has no kept build yet; a build kept
  already, or a body counted already, adds nothing and always passes. An intake's build is in
  flight between its insert and its release (two transactions): it is not counted then, and is
  checked when its release keeps it, so two intakes in flight at once never both take the last
  room. A build the tenant no longer counts (unlanded, its release purged) found again by an
  intake is checked the same way when it is kept again.
- **`bytes`, too, in the transaction that inserts a build.** The transaction that inserts a new
  build of an own source (a publication, an administrator's add, an upstream's ingest, a lazy
  fetch, a succession's copy) makes the same check before the insert, under the same lock, so a
  build row past the limit is not even inserted; the check where it is kept decides.
  - **A limit set after the caller read the tenant.** The caller adds the lock when the tenant has a
    bytes limit as it reads it (`store.BytesLocks`, read before the transaction). A limit set
    between that read and the insert finds the transaction without the lock: the insert then takes
    it in the transaction, after the row locks it holds. A deadlock this may make is the engine's to
    detect, and the transaction is retried, as any other. A release's transaction always takes the
    lock so (below), after row locks of its own (the tenant's row, a succession's copies): such a
    cycle with a landing is detected and retried the same way.
  - **The lock is always the last lock a transaction takes.** Keys are taken in the order given,
    and these transactions take different ones first (the publisher's auth lock for a build's
    insert or a landing, the auth and icon locks for an icon's commit: these take the bytes lock as
    their last key when they open). A release's transaction (`TenantUpstreamLock` and the
    channel's lock for a signed channel, the channel's lock alone for a passthrough one, the
    channel's lock for a placement or a promotion) takes it in the transaction, at the first
    release that keeps a build, after every item's recheck: a recheck may read the tenant's auth
    under the auth lock on another connection, which a landing holds while it waits for the bytes
    lock, a cycle the database could not see. No transaction takes an advisory lock after the
    bytes lock, so it adds no cycle among advisory locks; a cycle with row locks is the engine's to
    detect, as above.
  - **Two intakes of one tenant never both take the last room**, on one replica or many, whatever
    path each comes by: there is no overshoot. (An older replica still running during the rollout
    of migration 0047 does not check; the rollout is the only window.)
  - **A lowering `PUT` and an intake are ordered.** The `PUT` updates the tenant's row; the
    intake's shared lock on that row waits for it, or the `PUT` waits for the intake to commit.
- **Fast paths before the body moves.** They refuse early and cheaply; the transaction still
  decides.
  - `room` (spec 0033), after the spool, with the body's hash and size: refused when the body is new
    to the tenant and `used + size > limit`. This saves storing a body that would be refused.
  - **A publication's body is not pre-checked before it is read.** The upload carries no digest
    before its body (`Content-Length` only), so a pre-check could not tell an idempotent re-publish
    of a body the tenant has (which must pass) from new bytes. A full tenant costs one upload per
    refused publication, which the upload slots and the per-actor rate already bound.
  - **An upstream cell** (`cell` in a run, and the lazy puller's fetch): before the download, a
    cell whose record holds no body (a refusal's took none) is skipped when `used >= limit`, or
    when its source's catalogue is full ([Upstream runs at a limit](#upstream-runs-at-a-limit)). A
    cell that has a body already runs as ever (conditionally with its ETag): a `304` costs
    nothing. A `200` whose body is new to the tenant is refused after the spool by `room`. A run reads `used` (and its source's catalogue) once when it starts, not per cell; it
    adds nothing for the bodies it admits, since it cannot tell a body new to the tenant from one
    another source holds, and a figure that overcounts would refuse what the transaction takes: the
    first refusal after a download (by `room` or the transaction) stops its new downloads instead.
  - **An icon write** (spec 0036) checks its growth (the new icon's bytes less the stored icon's)
    before its upload, beside the per-source check, and again in its transaction. Unlike that
    per-source check, this one covers tenant administrators too.
- **Counts, exactly, in the insert's transaction.** Each insert reads the tenant's limit and count
  in its transaction, under a lock that orders the tenant's inserts of that kind, so two inserts
  never both take the last place:
  - upstreams: `TenantUpstreamLock`, held today;
  - publishers: the tenant's auth lock under `InTenantTxForUpdate`, which `AddPublisher` holds
    today (no new lock);
  - channels: `CreateChannel` moves from `InTx` to `InTenantTxForUpdate`
    (`internal/tenants/tenants.go`), as `AddPublisher` uses: its update lock on the tenant's row
    orders the tenant's channel creates, with no new lock. A name the tenant holds already
    answers `409` before any count (added after PR 2), so a distributor's provisioning that
    makes a channel or finds it learns it is there at the limit too, rather than `429`.

  The limit is read from the tenant's row in the same transaction (under the update lock, or the
  upstream insert's shared lock through `lockedTenantQuery`); a change of the limits updates that
  row, so a change and an insert are ordered too.
- **Which comes first.** The limit is checked before the ceiling. Every checked actor meets the
  limit, so only the bootstrap and a bundle import meet a ceiling's `400`.

A lowered limit removes nothing. A tenant over a limit can still read, serve, promote and remove;
only growth of that kind is refused until it is back under the limit.

### Refusals

- **`tenant-limit`**, a new problem type among the `429`s (spec 0037: one type per cause), status
  `429`, no `Retry-After`, like `catalogue-full`: it clears as the tenant frees room or its limit
  is raised. The problem carries `"limit": "bytes" | "channels" | "upstreams" | "publishers"`. The
  detail names the limit and says a server administrator can raise it; when the limit equals its
  ceiling, it says instead that the tenant holds the most kista allows. It carries no figures: a
  publisher with a signed request learns only which limit refused it, and the tenant's
  administrators read the figures (below).
- **Never retried by kista's clients.** kista's CLI, the console, and the developer kit's
  `kista_publish.py` (plan item K5) treat `tenant-limit` as final, as they treat `catalogue-full`.
- **An error reading the figure or the limit** fails closed: the intake is refused as an error (a
  `500` on the API; a run's cell `failed`, never `rejected`, since it is not the file's fault).
- **A metric** counts refusals by limit: `kista.tenant.limit.refusals` with the attribute `kista.limit`
  (in Prometheus, `kista_tenant_limit_refusals_total{kista_limit}`). Operators see
  tenants pressing on their plans, and a hosted service can offer a larger plan. It has no tenant
  label (cardinality): a tenant's own figures are its limits' `used`.
- **Events.** A refusal writes no event of its own: the refused request is not an action. Each
  change of limits writes one (below).

### Upstream runs at a limit

A run reads the tenant's use and its source's catalogue once when it starts. It has **no room**
when the tenant uses its whole bytes limit or the catalogue is full then, or once a cell was
refused by either during the run. Without room:

1. **a cell whose record holds no body** (never fetched, `missing`, or refused: a refusal's
   record names a body it did not take; a dry run decides by the same record) is skipped without
   a request: its record is left as it is (no
   write, so an ask, a refusal's event and an export read it unchanged), and the run counts it
   `skipped_at_limit` in its summary and its `upstream.run` event;
2. **a cell that holds a body** runs as a run would: conditionally with its ETag, or
   unconditionally when it keeps none; `room` decides its body, and passes one the tenant has (a
   yank kept, a changed ETag). Everything that brings no new body goes on: `304`s, yanks and
   removals the upstream reports, signatures, metadata;
3. **a refused cell** (`tenant-limit` or `catalogue-full`) is recorded `failed` (retried by the
   next run: never the file's fault, so no `upstream.rejected` event), with the detail naming the
   limit or the cap, among the run's problems. A `failed` record keeps the body hash and the ETag
   the cell held (as an import's does: a record of a body taken, or a failed one that kept it), so
   a landed build stays its cell's and the collector leaves it; after a refusal or a miss it holds
   none. A failed record confirms nothing: a bundle export takes a cell record's `fetched_at` and
   ETag only from a record of a body taken;
4. the run ends with a cause wrapped in `errFailed` **only when a cell was refused during it**: the
   request is recorded as failed and is not re-queued as interrupted (`internal/upstream/run.go`);
   the next run comes at its usual time. A run that only skipped cells did not fail.

A read of the use that fails at the run's start ends the run as interrupted, as any other store
error: it is due again after 5 minutes. A read that fails in a cell's `room` is
`release.ErrUsage`, the cell `failed`.

**What remains.** The check before a download fires only when `used >= limit`. While
`used < limit < used + size`, a cell's new body is downloaded and refused by `room` on every run
(its record keeps no ETag that the upstream would answer `304`): the cost of reading the figure
once, not per body, until room is made or the limit is raised. (As built: the first version of
this PR failed every cell without an ETag at a limit, which turned missing and refused cells into
failures and lost a landed build's cell body; the rule above replaces it.)

**`catalogue-full` changes the same way.** Today a run records a cell refused by the per-source cap
(spec 0033) as `rejected`: the file's fault, an `upstream.rejected` event, and no ETag on the next
run, so every run downloads the whole body again only to refuse it. The source is the upstream, so
its catalogue does not empty within a run either. A bundle import keeps its handling (it is a
server administrator's, run once, and exempt from the tenant limit).

**The lazy puller** (spec 0032) checks before the download as above: a cell holding no body is
skipped, not requested, its record left as it is (a lazy mirror's cell never fetched is recorded
failed, so its first request is answered `404` now, not `503` while nothing comes); a cell holding
one runs, and one refused by
`tenant-limit` or `catalogue-full` is recorded as a failed fetch. Either answers the client as a
failed fetch does today, but is remembered for `NegativeTTL` (an hour by default), not the minute
of a network failure: asking again within the hour would be refused again, and a pull-through
install keeps failing that long after room is made. Only a refusal by a limit is remembered so: a
network or authorization failure of the same fetch keeps its minute.

### The API

- `GET /api/v1/tenants/{t}/limits`: the tenant's administrators and server administrators
  (`pathAdmin`), a suspended tenant's too for server administrators (to its administrators a
  suspended tenant is unknown, as on every route); a distribution admin is refused like any caller
  without admin (`404`). It answers each limit with what is set, what applies and what the tenant
  uses, and an `ETag` over the stored values:

  ```json
  {"limits": {
    "bytes":      {"set": 53687091200, "limit": 53687091200, "used": 1234567},
    "channels":   {"set": "default",   "limit": 1000,        "used": 3},
    "upstreams":  {"set": 0,           "limit": 0,           "used": 0},
    "publishers": {"set": "unlimited", "limit": 200,         "used": 12}
  }}
  ```

  - `set` is the tenant's stored value: a number, `"unlimited"`, or `"default"` (none stored, the
    server's default applies). It is what a `PUT` takes, so a client compares it with what it
    would put.
  - `limit` is the value in force: a number, or `null` for unlimited `bytes` (a count's unlimited
    is its ceiling, shown as the number).
  - `used` counts what the limit counts: the tenant's own bytes, its channels, its upstreams, its
    own publishers (not distributors'). It is read even when `bytes` is unlimited: the `GET` is the
    one place that pays for it then.
- `PUT /api/v1/tenants/{t}/limits`: server administrators only (`serverAdmin`, a fresh token, a
  named writer, as `…/holder`), suspended tenants included. `If-Match` is optional; when sent, it
  must match the `GET`'s `ETag` (`412` otherwise). The body names the limits it sets; a limit left
  out is not changed, and only the named columns are written:

  ```json
  {"bytes": "50GiB", "channels": 20, "upstreams": "default", "publishers": "unlimited"}
  ```

  - a number (for `bytes`, also a size such as `"50GiB"`, the configuration's `ByteSize` syntax):
    the tenant's own value; `0` means none, no growth of that kind at all;
  - `"unlimited"`: no limit for `bytes`, the ceiling for a count;
  - `"default"`: the tenant's own value is cleared, the server's default applies;
  - a count above its ceiling, a negative number or an unknown limit name is `400 invalid`.

  The answer is the `GET`'s. The tenant's `version` is not bumped: limits change no snapshot. A
  `PUT` that changes nothing writes nothing and no event. A change writes the event
  `tenant.limits` in the tenant's audit with, for each changed limit, the stored value before and
  after, normalised (a number, `"unlimited"` or `"default"`; a size as its bytes), and the value in
  force after it.
- `POST /api/v1/tenants` takes an optional `limits` object, the `PUT`'s body, applied in the
  creating transaction and recorded as `tenant.limits` beside `tenant.create`. A tenant Enterest
  creates never runs on the server's defaults.
- A distribution admin neither reads nor sets limits. Enterest sets them from its provisioning
  worker, which is a server administrator for its task (Enterest spec 0008).

### The configuration

```yaml
limits:                # the defaults per tenant (spec 0038), a backstop; a tenant's own value replaces each
  bytes: unlimited     # its own catalogue bytes: a size (50GiB), 0 (none) or unlimited
  channels: unlimited  # a number up to 1000, 0 (none) or unlimited (the ceiling)
  upstreams: unlimited # up to 100
  publishers: unlimited # up to 200
```

- A key left out means `unlimited`. `0` means none, as in the API: one meaning in both places. It
  differs from kista's other settings, where `0` means "the default", and the website warns about
  it.
- A count above its ceiling refuses to start.
- **The ceilings are configured too** (amended 2026-10-31, below): `limits.ceilings`.
- **The defaults are a backstop, not a plan.** A change of the configuration changes every tenant
  without a value of its own, across the whole fleet as replicas restart, with no audit event. A
  hosted service puts explicit values on each tenant instead.

### The data model

Migration 0047, in the three dialects, `min_reader` 47:

```
tenants.limit_bytes       bigint  NULL       -- NULL: the server's default; -1: unlimited; >= 0: the limit
tenants.limit_channels    int     NULL       -- NULL: the default; -1: the ceiling; 0..ceiling
tenants.limit_upstreams   int     NULL
tenants.limit_publishers  int     NULL
builds.body_len           bigint  NULL       -- the body's length; NULL until filled
ix_builds_tenant_body     ON builds (tenant_id, body_hash), carrying body_len, source, landed, withdrawn_at, id
```

- Columns on `tenants`, not a table: they go with the tenant when it is deleted (spec 0019), and the
  limit checks read that row.
- `body_len` is filled from `blobs` by the migration for existing rows, and set by every insert
  (a copy copies it). It is nullable, not `0` by default, so a row an older replica inserts is
  recognisable: `FillBodyLen`, beside `FillUsedAt` (`internal/store/gc.go`) and run by the
  collector the same way, sets the `NULL`s of a domain's builds from `blobs`; `kista serve` also
  runs it once at its start, for every domain, so a server whose collector does not run
  (`gc.interval` 0) fixes them on its next restart. Until then such a row counts as `0`. A row whose blob is gone stays `NULL` and counts as `0`: it holds no bytes.
- `min_reader` 47: an older binary would insert builds without `body_len` and would not check
  limits, so it does not start on the new schema. One still running during the rollout does both
  until it is replaced; the collector's fill repairs the first, and the rollout is the only window
  for the second.

### Bootstrap, the CLI and the console

- **Bootstrap** (spec 0024): the tenant section takes `limits` (the configuration's syntax). The
  plan shows it as a change like the tenant's other fields, naming only the limits that differ;
  applying it writes `tenant.limits` as the bootstrap actor: for a tenant it creates, in the
  creating transaction (`CreateTenantWithLimits`, the event in the server's events beside
  `tenant.create`, as `POST /tenants`), for an existing one the changed limits only. The plan is
  checked as applied, past the limits (a dry run too). As a partial `PUT`, it sets only the limits it names: a tenant section
  without `limits`, or without one of them, leaves the stored values as they are. The bootstrap
  itself is exempt from the limits.
- **CLI:** `kista admin tenant limits <tenant>` prints the limits as the `GET`.
  `kista admin tenant limits <tenant> -bytes 50GiB -channels 20 -upstreams default -publishers
  unlimited` sets them as the `PUT`, through the store with the server administrator's audit actor,
  as `kista admin tenant holder` does. A flag left out changes nothing: the command tells an
  omitted flag from an empty one with `flag.Visit`, and an empty value is refused.
- **Console:** the tenant's **Limits** page (the console has no tenant overview; a section of its
  own, beside Licences) shows each limit, what is set, what applies and what is used, read only,
  for the tenant's administrators and server administrators. A server administrator's editor
  belongs to plan item K4's server screens; until then the CLI and the API set limits. The console
  shows a `tenant-limit` refusal with its detail, which names its limit, and never retries it (nor
  `catalogue-full`): its API client keeps the problem's type.

### Interaction

- **Enterest (E7).** The provisioning worker always puts explicit values: at creation (in the
  `POST`), on each plan change, and on each provisioning sweep, `"unlimited"` included during the
  free period. It never relies on the server's defaults. An operator's audited change of a
  customer's quota (G1) is a task that does the same. A distributor's publications are never
  refused by a customer's limit, so a customer at its quota still gets what it bought. The builds
  Enterest makes for an organisation's own developers are published by its distributor's
  publishers too, so they are bounded by Enterest's own quotas, not by this limit.
- **Tenant deletion (spec 0019).** Nothing new: the columns go with the row.
- **Follows (spec 0033), promotion, re-signing.** They place builds already in the catalogue: no
  new build, no new bytes. A succession's copy is a new build of a body the tenant already counts.
- **Catalogue cap (spec 0033).** Both apply; a refusal names its own type (`catalogue-full` for
  the source, `tenant-limit` for the tenant). The per-source cap still exempts an administrator's
  add; the tenant limit does not.
- **The download path** is never refused by a limit: a limit stops growth, not serving.

## Security

- **Who can change a limit.** Only a server administrator, with a fresh token, or the bootstrap
  from the server's configuration. A tenant administrator cannot raise their own limits. Each
  change is audited in the tenant's log with the actor. A change of the configuration's defaults is
  not, which is why a hosted service sets every tenant explicitly.
- **What a tenant learns.** Its administrators read its own limits and its own use, nothing about
  other tenants. Bodies shared with other tenants count in full in each tenant, so the figure says
  nothing about what another tenant stores. A publisher learns only the name of the limit that
  refused it.
- **Fail closed.** A failure to read the use or the limit refuses the intake; it never admits it.
- **Bypasses considered.**
  - *Many sources:* the limit is per tenant, not per source.
  - *Placed builds:* every kept build counts, placed or not.
  - *A distributor's publisher as a way around:* only distribution admins and server
    administrators create those (spec 0037), and a tenant administrator cannot publish through one.
    A removed distributor's publisher's builds stay excluded, and nothing publishes through it.
  - *A tenant administrator's add:* checked, though the per-source cap exempts it.
  - *A server administrator's call:* checked; raising the limit is the audited way past it.
  - *Many requests at once, many replicas:* bytes and counts are checked under a lock in the
    inserting transaction: no overshoot.
  - *Removing and adding back:* removals free room, which is the intent.
  - *Builds waiting for the collector:* not counted, but no longer served or placed, so they hold
    no bytes a tenant can use.
- **Denial of service by the check itself.** Only a tenant with a `bytes` limit pays for it: one
  `SUM` per new build, linear in its builds (one pass over the new index plus the probes listed in
  "Reading it"), under a lock that orders that tenant's new builds, so a tenant's intakes wait on
  one another for that read. A run reads the figure once. A tenant with a very large catalogue pays
  the cost on its own intakes only.

## Testing

- **Store suite (three dialects):**
  - the tenant's use against a hand count: placed, landed, unlanded and waiting for the collector,
    a distinct body in two sources, a succession's copy, a body shared with another tenant, icons,
    an icon of an entry with no build left, a distributor's source and its icons left out, a
    **removed** distributor's publisher's withdrawn build left out;
  - icons summed past 2 GiB on SQL Server (`SourceUsage` too);
  - `body_len` filled by the migration and set by every insert and copy; a `NULL` row filled by
    `FillBodyLen`, counted as `0` before;
  - the limit columns' round trip (`NULL`, `-1`, `0`, a value);
  - two concurrent inserts of a channel and of a publisher at the last place, one refused; two
    concurrent intakes of new bodies at the last room, one refused;
  - a lowering `PUT` racing an insert, for a count and for `bytes` (an intake of a new body): the
    insert is refused by the new limit unless it committed before the `PUT`;
  - two intakes by different paths at the last room (a publication and an upstream ingest; a
    signed and a passthrough channel's), one refused, no deadlock.
- **Release:**
  - a publication, a tenant administrator's add (refused by `bytes` while the per-source cap
    exempts it), a server administrator's add, an upstream ingest: all refused at `bytes`; a bundle
    import and the bootstrap pass;
  - an idempotent re-publish of a body the tenant has, at a full tenant, passes;
  - a distributor's publisher's publication passes past the limit;
  - a purge, then a retry: passes once the release is purged, before the collector runs;
  - an icon's growth refused, a smaller icon passes, a distributor's icon not counted.
- **API:** `GET` for a tenant administrator and a server administrator, `404` for a distribution
  admin and others; `PUT` by a server administrator only; a partial `PUT` leaves the other limits;
  a no-op `PUT` writes no event; `If-Match` honoured; the event's before, after and value in force;
  `set` round-trips through `PUT`; `default`, `unlimited`, `0`, sizes, the `400`s; `POST /tenants`
  with `limits`; each count's refusal as `429 tenant-limit` with its `limit`, and the ceiling's
  wording; a publisher's refusal carries no figures; a distributor's publisher's create is not
  counted against `publishers`; a suspended tenant's limits read and set; the metric.
- **Upstream:** at the limit from the start, a `304` and a yank are applied, a missing cell is left
  untouched and counted `skipped_at_limit`, and the run does not fail (a dry run alike); a refusal
  in mid-run records the cell `failed` with `tenant-limit`, no `upstream.rejected` event, a later
  cell holding no body is skipped without a request, a conditional `200` with a new body is refused
  keeping its record's body and ETag, and the run ends failed without an interrupted retry; a cell
  holding a body without an ETag runs at the limit and a failed fetch keeps its body (its landed
  build not stale); the same for `catalogue-full`; a lazy fetch past the limit is not requested and
  remembered for `NegativeTTL`; a usage-read error records
  `failed` (as built: a room check's read error is `release.ErrUsage`, tested as a mapping; the
  upstream's switch takes it as `failed`, since a store error cannot be made on demand).
- **Config:** a key left out, `unlimited`, `0`, `50GiB`, a count above its ceiling refused at start.
- **Bootstrap:** limits in the plan and applied, the event as the bootstrap actor.
- **CLI:** `kista admin tenant limits` read and set; an omitted flag unchanged, an empty one refused.
- **Console:** the Limits page shows the limits (unit test and the console e2e's tenant page); a
  `tenant-limit` and a `catalogue-full` are final, never retried (unit test).

## Alternatives considered

- **A counter kept per tenant.** No `SUM` per new build. But builds are added in several places and
  removed in several (the purge, the trim of landed builds, the collector, tenant deletion), icons
  change in others, and every one would have to keep the counter right forever. With `body_len` on
  the build, the `SUM` is one pass over one index plus probes, and only for tenants with a `bytes`
  limit.
- **A check outside any lock**, as the per-source cap's. A small free plan could then overshoot by
  many intakes times `blob.max_body` (gibibytes); the lock costs only tenants with a limit.
- **Counting every build row.** Simpler, but a succession's copies and a re-publish into a second
  source would count bytes the storage does not hold, and a copy could be refused.
- **A periodic recount.** No drift and a cheap check, but the lag lets a tenant overshoot by
  whatever it lands in the period.
- **Physical bytes.** Shared blobs make them unpredictable for a customer (a mirror of the core
  extensions costs nothing when another tenant has them) and would reveal what other tenants hold.
- **Exempting server administrators.** `room`'s `admin` flag covers a tenant administrator's add
  too, and a second notion of "administrator" would be needed; raising the limit, audited, is the
  way past it.
- **Limits set by a distribution admin.** Enterest's provisioning already acts as a server
  administrator; a distribution admin acts for a distributor in many customers' tenants and should
  not change their plans (decision 3).
- **Every fixed bound as a limit.** Issuers, grants, groups, feeds and the rest are cheap and
  small; making them per tenant adds API and tests for no plan that needs them.
- **`403` or `507` for a refusal.** kista's capacity refusals are `429` with a type of their own
  (spec 0037); a client handles them in one place.

## Changes to earlier specs

- 0033: the catalogue's admission (`room`) and a build's insert also check the tenant's limit;
  `SourceUsage` sums icons as `bigint` on SQL Server.
- 0036: an icon write also checks the tenant's limit, for tenant administrators too.
- 0037: `tenant-limit` joins the `429` types.
- 0009, 0032: a run stops new downloads at a `tenant-limit` or `catalogue-full` refusal, records the
  cell `failed` (no longer `rejected` for `catalogue-full`) and ends failed; a lazy fetch is checked
  before its download, and a refused one is remembered for `NegativeTTL`.
- 0024: the bootstrap's tenant takes `limits`.
- 0003: a tenant holds at most 1,000 channels (a new ceiling), and its channels are counted
  against its `channels` limit.
- 0008 (publishers, as 0023 left them) and 0009 (upstreams): the 101st upstream and the 201st own
  publisher are refused with `429 tenant-limit` (the limit, by default the ceiling), where they
  used to get `400 invalid` (`internal/store/upstreams.go`, `internal/store/publishers.go`); only
  the bootstrap and a bundle import still meet the `400`.

## Delivery

Two PRs on branches `0038-tenant-limits-p1` and `0038-tenant-limits-p2`. Each updates the website
pages of the behaviour it changes, and each runs, before it opens, `make test` on the three engines,
`make lint`, the console's tests, `make console-e2e` and `make e2e`.

1. **The limits.** Migration 0047 (the tenant columns, `body_len`, the index, `min_reader` 47) and
   `FillBodyLen`; the store's figure and the `SourceUsage` cast; the configuration; the `GET`, the
   `PUT` and `POST /tenants`' `limits`; the bootstrap's `limits`; `kista admin tenant limits`; the
   count checks (`CreateChannel` under `InTenantTxForUpdate`, the ceiling of channels) and the
   bytes checks (`room`, the build's insert, and the deciding check where a build is kept,
   `SetLanded` and `InsertRelease`, under `TenantBytesLock`; icons); `tenant-limit` and the
   metric. Pages: `configuration.md`, `administration/tenants-and-channels.md`,
   `administration/commands.md`, `api.md`, `catalogue.md`, `publishing.md`, `observability.md`,
   `administration/upgrading.md`. Between the PRs, an upstream run that meets `tenant-limit` records
   the cell as today's default does (`rejected`); the limit holds, only the run's handling waits.
2. **Runs and the console.** The run at a limit (a cell holding no body skipped, a refused cell
   `failed` keeping its body and ETag, the `errFailed` end when a cell was refused; `catalogue-full`
   the same) and the check before a cell's download; the lazy
   puller's check before the download and its `NegativeTTL`; a use that could not be read
   (`release.ErrUsage`) a failed cell; the console's Limits page and its refusal display. Pages:
   `upstreams.md`, `console.md`, `administration/tenants-and-channels.md` (links to both). As built:
   `TestRunAtTenantLimit`, `TestRunMeetsTenantLimit`, `TestRunAtLimitKeepsBodies`, `TestRunAtFullCatalogue`,
   `TestPullAtTenantLimit`, `TestUsageErr`, the console's `limits.test.tsx` and its e2e's Limits
   page.

## Website

- `configuration.md`: the `limits` section, and the warning that `0` there means none; (PR 2)
  `upstreams.negative_ttl` remembers a limit's refusal.
- `administration/tenants-and-channels.md`: limits, the `GET`/`PUT`, what counts as bytes, what a
  lowered limit does, the defaults as a backstop.
- `administration/commands.md`: `kista admin tenant limits`.
- `api.md`: the routes, `POST /tenants`' `limits`, and the `tenant-limit` problem type.
- `catalogue.md`: the tenant limit beside the per-source cap.
- `publishing.md`: `tenant-limit` refusals, never retried.
- `upstreams.md`: a run at a limit.
- `console.md`: the tenant's Limits page, and a limit's refusal never retried.
- `observability.md`: the refusals metric.
- `administration/upgrading.md`: migration 0047, its `min_reader` and the `body_len` fill.

## Follow-ups

- More limits by the same pattern when a plan needs them (catalogue entries per tenant, issuers,
  grants).
- A server administrator's editor in the console (K4).
- Download or egress limits per tenant: out of scope; serving is never refused by a limit.
- The developer kit's `kista_publish.py` (plan item K5) stops on `tenant-limit` as on
  `catalogue-full`.

## Amendment: configured ceilings (2026-10-31)

The owner: "Я бы не хотел хардкодить ограничения, нужно управляемые лимиты" — no limit is fixed in
code. Every ceiling below was a constant; each is now a server setting under `limits.ceilings`,
today's value its default, checked only for shape (a count at least 1, a duration at least 1s). They
are the server's, never a tenant's: a tenant's own limits (above) and the configuration's defaults
stay at or below them, and the bootstrap tenant's `limits` refuse a `ceilings` key. `store.SetCeilings`
puts them in force as the store opens (`kista serve` and `kista admin` alike); the checks, refusals
and their types are unchanged.

A ceiling lowered below what a tenant stored caps that value: the value in force is the smaller of
the two, so growth past the ceiling is refused as `429 tenant-limit` at its ceiling, never `400`.
What a tenant already holds stays; the stored value is kept and counts again if the ceiling is raised.

| Was | Kind | `limits.ceilings.` key | Default |
| --- | --- | --- | --- |
| `MaxChannels` | product | `channels` | 1000 |
| `MaxUpstreams` | product | `upstreams` | 100 |
| `MaxPublishers` | product | `publishers` | 200 |
| `MaxDistributorPublishers` | product | `distributor_publishers` | 10 |
| `MaxPublisherKeys` | product | `publisher_keys` | 10 |
| `keys.MaxKeys` (channel and attestation keys) | product | `signing_keys` | 16 |
| `MaxIssuers` | product | `issuers` | 16 |
| `MaxGrants` | product | `grants` | 1000 |
| `MaxPackageGroups`, `MaxGroupMembers`, `MaxTenantMembers` | product | `package_groups`, `group_members`, `tenant_group_members` | 100, 1000, 10000 |
| `MaxFeeds`, `MaxFeedKeys`, `MaxFeedKept` | product | `feeds`, `feed_keys`, `feed_kept` | 50, 10, 100000 |
| `MaxBlocks` | product | `blocks` | 10000 |
| `MaxTenantAttachments`, `MaxAttachmentHeads` | product | `attachments`, `attachment_heads` | 100000, 20 |
| `MaxUpstreamEntries`, `MaxEntryVersions`, `MaxUpstreamKeys` | product | `upstream_entries`, `entry_versions`, `upstream_keys` | 1000, 100, 10 |
| `MaxUpstreamCells`, `MaxTenantCells` | product | `upstream_cells`, `tenant_cells` | 20000, 100000 |
| `MaxDiscoveredNames` (spec 0039) | product | `discovered_names` | 1000 |
| `MaxHold` | product | `max_hold` | 2160h (90 days) |
| `tenants.MaxKeyLifetime` | product | `publisher_key_lifetime` | 8784h (366 days) |

What stays in code, and why (bounds on one request, one document or one remote answer, which protect
the server and are part of a format, not a tenant's room):

- `MaxSinks` (16): a bit each in `events.pending`, a column's width.
- Sizes and counts of one request or answer: request bodies, `MaxStatement`, `MaxData`, usage
  reports, feed and stats pages, `maxRevocationsPage`/`maxNamesPage` and their row bounds, the bundle
  manifest's bounds (`MaxCells`, `MaxFiles`, `MaxRevocations`, …), `MaxMemberChange` per request.
- Content rules of a format: `MaxSummary` (160), the icon's size and sides, reasons' and
  descriptions' lengths, licence and token sizes, DuckDB's 64 KiB `.well-known`.

## Decisions

The owner's decisions of 2026-10-29, which the text above follows:

1. **A distributor's builds and a customer's quota:** never counted, never refused. A customer at
   its quota still gets what it bought or chose.
2. **Which bytes count:** every build of the tenant's own sources, upstream mirrors included, and
   their icons (each body once, see "Distinct bodies").
3. **Who sets limits:** server administrators only (Enterest's provisioning worker is one), and the
   bootstrap from the server's configuration.
4. **The set of limits:** bytes, channels, upstreams, own publishers.
5. **An upstream run at a limit** stops its new downloads at its first `tenant-limit` refusal, and
   at a `catalogue-full` one.
