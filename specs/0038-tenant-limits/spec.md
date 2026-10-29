# Spec 0038: Tenant limits

- **Status**: accepted
- **Date**: 2026-10-29
- **Author**: vgsml, Claude

## Summary

A server administrator sets limits per tenant on what the tenant's own administrators, publishers
and upstreams can make it grow by: the bytes of its catalogue, and the number of its channels,
upstreams and publishers. Each limit has a server-wide default in the configuration, which a
tenant's own value replaces; a value can be "unlimited". A tenant's administrators read their
limits and what they use. Growth past a limit is refused with a `429` of its own type naming the
limit; nothing that exists is removed when a limit is lowered.

This is plan item P4 (needed before production). It is what a hosted service needs for
self-service tenants: Enterest's organisation plans (its plan item E7) set these limits through its
provisioning worker, which is a kista server administrator. The defaults keep today's behaviour,
so a server that sets nothing sees no change.

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

| Limit | Counts | Ceiling (code) | Default (config) |
| --- | --- | --- | --- |
| `bytes` | the tenant's **own** catalogue bytes (below) | none | unlimited |
| `channels` | the tenant's channels | 1,000 (new `store.MaxChannels`) | the ceiling |
| `upstreams` | the tenant's upstreams | 100 (`store.MaxUpstreams`, unchanged) | the ceiling |
| `publishers` | the tenant's own publishers (not distributors') | 200 (`store.MaxPublishers`, unchanged) | the ceiling |

- The ceilings stay constants in the code. They bound queries and caches; they are not policy. A
  limit's value is at most its ceiling, and "unlimited" for a count means the ceiling.
- The other fixed bounds in the table above stay as they are. They are small and cheap, and a plan
  has no reason to differ in them. A later limit is added by the same pattern.
- **Distributors are outside the limits.** The builds and icons of distributors' publishers'
  sources are never counted and never refused by a tenant limit. Distributors' publishers are
  counted apart, under their own cap of 10 (spec 0037). A customer's full quota must never stop the
  distributor from delivering what the customer bought or chose. A distributor's sources stay
  bounded per source by `catalogue.max_entries` and `catalogue.max_bytes` (spec 0033).
- **A server administrator is outside the limits.** What a server administrator does directly is
  counted but never refused by a limit: a bundle import, an add to the catalogue, a sync of an
  upstream they start, a create of a channel, upstream or publisher. The server's operator decides
  those. The ceilings still apply to them (`400 invalid`, below).

### The bytes a tenant uses

A tenant's own bytes are the sum of:

- `blobs.body_len` of each build of the tenant whose source is not a distributor's publisher: every
  build row, placed or not, from any origin (publication, an administrator's add,
  an upstream, a bundle). A body in two of the tenant's sources counts twice; a body shared with
  another tenant counts in each.
- `entry_meta.icon_bytes` of each entry of those sources (spec 0036).

It is a logical figure, not the storage the server spends: blobs are shared within a storage domain
(spec 0005). It is predictable for the customer, and it is what a plan can promise. Attachments are
not counted: they are bounded by count and size already (`MaxTenantAttachments`).

A tenant frees bytes by removing builds: purging a yanked release (spec 0016), and removing builds
placed nowhere from the catalogue (spec 0033). Withdrawn builds, which are never purged, belong to
distributors' sources only (spec 0033 phase B), so they never count here.

The figure is read live: one `SUM` over the tenant's builds (through the index that leads with
`tenant_id`), joined to `blobs` on the tenant's storage domain, with the distributors' sources (at
most 10, read first) left out by their source names, plus one `SUM` over `entry_meta`. It is the
per-source `SourceUsage` query (spec 0033), widened to the tenant. No counter is kept (see
Alternatives).

### Where each limit is enforced

- **`bytes`, at admission, before the body is stored.** `release.Service.room` already runs at every
  intake: a publication, an administrator's add, an upstream's run, an upstream's fetch for a lazy
  mirror, a bundle import. It gains the tenant check after the per-source check. A body the source
  already has is no new bytes, as today. The check refuses when `used + size > limit` for an actor
  that is not a server administrator, on a source that is not a distributor's.
  - **An icon write** (spec 0036) checks its growth (the new icon's bytes less the stored icon's)
    the same way, beside the per-source check. Unlike that per-source check, this one covers
    tenant administrators too.
  - **A publication is refused before its body is read** when the tenant is already at or over
    the limit (`used >= limit`), as spec 0036's pre-checks are: a client repeating a large upload
    against a full tenant costs one query, not an upload. The check after the spool, with the
    size, still decides.
  - **The check is read outside any lock**, as the per-source check is. Intakes in flight at once
    can pass it together. The overshoot is bounded by the tenant's intakes in flight: its upload
    slots (`publish.max_per_tenant`, 8 by default), its lazy fetches (4) and the upstream runs the
    server's workers take for it at once, each body at most `blob.max_body`. A limit is a bound
    against abuse, not an exact meter; the figure the tenant reads is exact.
- **Counts, in the insert's transaction.** `CreateChannel`, `InsertUpstream` and `InsertPublisher`
  (own publishers) already count in their transaction (channels will). Each reads the tenant's
  limit in the same transaction and compares. The transaction holds a lock that orders the
  tenant's inserts of that kind: `TenantUpstreamLock` for upstreams (held today), and a new
  `TenantLimitLock(tenant, kind)` for channels and publishers. Two inserts at once never both pass
  the last place. The limit is read from the tenant's row with a shared lock
  (`lockedTenantQuery`, as `WritableTenant` reads it); a change of the limits updates that row, so
  a change and an insert are ordered too.
- **Which comes first.** The limit is checked before the ceiling. A limit equal to its ceiling
  (the default) refuses with `tenant-limit`; only a server administrator, outside the limits, meets
  a ceiling's `400`.

A lowered limit removes nothing. A tenant over a limit can still read, serve, promote and remove;
only growth of that kind is refused until it is back under the limit.

### Refusals

One new problem type among the `429`s (spec 0037: one type per cause):

- `tenant-limit`, status `429`, no `Retry-After`: it clears as the tenant frees room or its limit is
  raised. The problem carries `"limit": "bytes" | "channels" | "upstreams" | "publishers"`. The
  detail names the limit and says who can change it ("a server administrator"). It carries no
  figures. A publisher with a signed request learns only which limit refused it; the tenant's
  administrators read the figures (below).
- The ceilings keep their `400 invalid` refusal ("a tenant holds at most …") for a server
  administrator, the one caller who can reach them. Every other caller meets the limit first, so a
  client sees one type for "the plan is full".
- **Upstream runs and fetches.** A run that meets `tenant-limit` records the cell as `failed`
  (retried by the next run: never the file's fault, so no `upstream.rejected` event) with the
  detail naming the limit, records it among the run's problems, and **stops**: the limit does not
  clear within a run, and going on would download every remaining body only to refuse it. The cells
  not reached keep their state. A lazy mirror's fetch (spec 0032) that is refused is recorded as a
  failed fetch and answers the client as a failed fetch does today.
- **`catalogue-full` in an upstream run, changed the same way.** Today a run records a cell refused
  by the per-source cap (spec 0033) as `rejected`: the file's fault, an `upstream.rejected` event,
  and no ETag on the next run, so every run downloads the whole body again only to refuse it. The
  source is the upstream, so its catalogue does not empty within a run either. It becomes what
  `tenant-limit` is: the cell `failed` with the detail naming the cap, the run's problem, and the
  run stopped. A bundle import keeps its handling (it is a server administrator's, run once).
- **Events.** A refusal writes no event of its own: the refused request is not an action. Each
  change of limits writes one (below).

### The API

- `GET /api/v1/tenants/{t}/limits`: the tenant's administrators and server administrators
  (`pathAdmin`), suspended tenants included. It answers each limit with its value, where it comes
  from and what the tenant uses:

  ```json
  {"limits": {
    "bytes":      {"limit": 53687091200, "from": "tenant", "used": 1234567},
    "channels":   {"limit": 1000, "from": "server", "used": 3},
    "upstreams":  {"limit": 0,    "from": "tenant", "used": 0},
    "publishers": {"limit": null, "from": "server", "used": 12}
  }}
  ```

  `limit: null` is unlimited (only `bytes` can be: a count's unlimited is its ceiling, shown as
  the number). `from` is `tenant` when the tenant has its own value, `server` for the default.
- `PUT /api/v1/tenants/{t}/limits`: server administrators only (`serverAdmin`, a fresh token, a
  named writer, as `…/holder`), suspended tenants included. The body names the limits it sets; a
  limit it leaves out is not changed:

  ```json
  {"bytes": "50GiB", "channels": 20, "upstreams": "default", "publishers": "unlimited"}
  ```

  - a number (for `bytes`, also a size such as `"50GiB"`, the configuration's `ByteSize` syntax):
    the tenant's own value, `0` included (no growth of that kind at all);
  - `"unlimited"`: no limit for `bytes`, the ceiling for a count;
  - `"default"`: the tenant's own value is cleared, the server's default applies;
  - a count above its ceiling, a negative number or an unknown limit name is `400 invalid`.

  The answer is the `GET`'s. The change writes the event `tenant.limits` in the tenant's audit,
  with each changed limit's `from`/`to`, the values as the body gave them.
- A distribution admin neither reads nor sets limits. Enterest sets them from its provisioning
  worker, which is a server administrator for its task (Enterest spec 0008).
- `used` counts what the limit counts: the tenant's own bytes, its channels, its upstreams, its own
  publishers (not distributors').

### The configuration

```yaml
limits:            # the defaults per tenant (spec 0038); a tenant's own value replaces each
  bytes: 0         # its own catalogue bytes; 0: unlimited (the default)
  channels: 0      # 0: the ceiling, 1000
  upstreams: 0     # 0: the ceiling, 100
  publishers: 0    # 0: the ceiling, 200
```

- `0` in the configuration means "unlimited" (the ceiling for a count), as `0` means "the
  default" everywhere in kista's configuration. A tenant's own `0` means none: the `PUT` body is
  explicit. A server whose new tenants should start with none of something sets it per tenant at
  creation; Enterest's provisioning does.
- A count above its ceiling refuses to start. `bytes` takes `ByteSize`.
- Environment variables follow the configuration's usual mapping (`KISTA_LIMITS_BYTES`, …).

### The data model

Migration 0047, additive, in the three dialects, with no `min_reader` bump:

```
tenants.limit_bytes       bigint  NULL   -- NULL: the server's default; -1: unlimited; >= 0: the limit
tenants.limit_channels    int     NULL   -- NULL: the default; -1: the ceiling; 0..ceiling
tenants.limit_upstreams   int     NULL
tenants.limit_publishers  int     NULL
```

- Columns on `tenants`, not a table: they go with the tenant when it is deleted (spec 0019), and the
  insert transactions already hold a shared lock on that row.
- An older replica during an upgrade ignores the columns and does not enforce the limits. kista is
  not released; the window is a rolling upgrade's.
- The store suite's `Reset` needs no change (the columns are the tenants').

### The CLI and the console

- `kista tenant limits <tenant>`: prints the limits and their use, as the `GET`.
  `kista tenant limits <tenant> -bytes 50GiB -channels 20 -upstreams default -publishers unlimited`
  sets them, as the `PUT`, through the store with the server administrator's audit actor, as
  `kista tenant holder` does.
- The console's tenant overview shows each limit, its use and where it comes from, read only, for
  the tenant's administrators and server administrators. A server administrator's editor belongs
  to plan item K4's server screens; until then the CLI and the API set limits.

### Interaction

- **Enterest (E7).** Launch: Enterest sets nothing, every tenant has the server's defaults, which
  keep today's behaviour. After the free period, a plan maps to the four values and the provisioning
  worker puts them, idempotently, on each plan change and on each provisioning. An operator's
  audited change of a customer's quota (G1) is a task that does the same. A distributor's
  publications are never refused by a customer's limit, so a customer at its quota still gets what
  it bought. The builds Enterest makes for an organisation's own developers are published by its
  distributor's publishers too, so they are bounded by Enterest's own quotas, not by this limit.
  Between creating a tenant and putting its limits, a tenant has the server's defaults: a hosted
  server sets its defaults to its smallest plan once the free period ends.
- **Tenant deletion (spec 0019).** Nothing new: the columns go with the row.
- **Follows (spec 0033), promotion, re-signing.** They place builds already in the catalogue: no
  new build, no new bytes.
- **Catalogue cap (spec 0033).** Both apply; a refusal names its own type (`catalogue-full` for
  the source, `tenant-limit` for the tenant). The per-source cap still exempts an administrator's
  add; the tenant limit does not exempt a tenant administrator.
- **The download path** is never refused by a limit: a limit stops growth, not serving.

## Security

- **Who can change a limit.** Only a server administrator, with a fresh token. A tenant
  administrator cannot raise their own limits. Each change is audited in the tenant's log with the
  actor.
- **What a tenant learns.** Its administrators read its own limits and its own use, nothing about
  other tenants. Shared blobs are counted in full in each tenant, so the figure says nothing about
  what another tenant stores. A publisher learns only the name of the limit that refused it.
- **Fail closed.** A failure to read the use or the limit refuses the intake (an error, as a
  failed per-source check does today), never admits it.
- **Bypasses considered.**
  - *Many sources:* the limit is per tenant, not per source.
  - *Placed builds:* every build row counts, placed or not.
  - *A distributor's publisher as a way around:* only distribution admins and server
    administrators create those (spec 0037), and a tenant administrator cannot publish through one.
  - *A tenant administrator's add:* checked; only a server administrator's add is exempt.
  - *Many requests at once:* the overshoot is bounded (above); counts are exact.
  - *Removing and adding back:* removals free room, which is the intent.
- **Denial of service by the check itself.** One `SUM` per intake, over the tenant's builds through
  the tenant index; an intake already reads the per-source figure the same way. A tenant with a
  very large catalogue pays the cost on its own intakes.

## Testing

- **Store suite (three dialects):** the tenant's use against a hand count (placed, unplaced,
  withdrawn, icons, a distributor's source left out, a body in two sources, a body shared with
  another tenant); the limit columns' round trip (`NULL`, `-1`, `0`, a value); two concurrent
  inserts of a channel and of a publisher at the last place, one refused; a limit lowered between
  two inserts.
- **Release:** a publication, an administrator's add, an upstream ingest and a bundle import at
  `bytes` (the import passes, the others are refused with `tenant-limit`); a body the source has
  already passes; a distributor's publisher's publication passes past the limit; an icon's growth
  refused, an icon replaced by a smaller one passes.
- **API:** `GET` for a tenant administrator and a server administrator, `404` for others; `PUT` by
  a server administrator only, the event, `default`/`unlimited`/`0`/sizes, `400`s; each count's
  refusal as `429 tenant-limit` with its `limit`; a publisher's refusal carries no figures; a
  suspended tenant's limits read and set.
- **Upstream:** a run past `bytes` records the cell `failed` with `tenant-limit`, no
  `upstream.rejected` event, and stops (later cells untouched); the same for `catalogue-full`; a
  lazy fetch past the limit answers as a failed fetch.
- **Config:** defaults, ceilings, `0`, a count above its ceiling refused at start.
- **CLI:** `kista tenant limits` read and set.
- **Console:** the overview shows the limits (unit test and the console e2e's tenant page).

## Alternatives considered

- **A counter kept per tenant.** Exact under concurrency, no `SUM` per intake. But builds are added
  in one place and removed in several (the purge, the trim of landed builds, the catalogue's purge,
  tenant deletion), icons change in others, and every one of them would have to keep the counter
  right forever. The live `SUM` is already what the per-source cap pays.
- **A periodic recount.** No drift and a cheap check, but the lag lets a tenant overshoot by
  whatever it lands in the period.
- **Physical bytes.** Shared blobs make them unpredictable for a customer (a mirror of the core
  extensions costs nothing when another tenant has them) and would reveal what other tenants hold.
- **Limits set by a distribution admin.** Enterest's provisioning already acts as a server
  administrator; a distribution admin acts for a distributor in many customers' tenants and should
  not change their plans (decision 3).
- **Every fixed bound as a limit.** Issuers, grants, groups, feeds and the rest are cheap and
  small; making them per tenant adds API and tests for no plan that needs them.
- **`403` or `507` for a refusal.** kista's capacity refusals are `429` with a type of their own
  (spec 0037); a client handles them in one place.

## Changes to earlier specs

- 0033: the catalogue's admission (`room`) also checks the tenant's limit.
- 0036: an icon write also checks the tenant's limit, for tenant administrators too.
- 0037: `tenant-limit` joins the `429` types.
- 0009, 0032: an upstream run stops at a `tenant-limit` or `catalogue-full` refusal, recording the
  cell `failed` (no longer `rejected` for `catalogue-full`); a refused lazy fetch is a failed fetch.

## Website

- `configuration.md`: the `limits` section.
- `administration/tenants-and-channels.md`: limits, the `GET`/`PUT`, what counts as bytes, the
  overshoot bound, what a lowered limit does.
- `administration/commands.md`: `kista tenant limits`.
- `api.md`: the routes and the `tenant-limit` problem type.
- `upstreams.md`: a run stopped by `tenant-limit`.
- `administration/upgrading.md`: migration 0047.

## Follow-ups

- More limits by the same pattern when a plan needs them (catalogue entries per tenant, issuers,
  grants).
- A server administrator's editor in the console (K4).
- Download or egress limits per tenant: out of scope; serving is never refused by a limit.
- The developer kit's `kista_publish.py` (plan item K5) stops on `tenant-limit` as on
  `catalogue-full`.

## Decisions

The owner's decisions of 2026-10-29, which the text above follows:

1. **A distributor's builds and a customer's quota:** never counted, never refused. A customer at
   its quota still gets what it bought or chose.
2. **Which bytes count:** every build of the tenant's own sources, upstream mirrors included, and
   their icons.
3. **Who sets limits:** server administrators only (Enterest's provisioning worker is one).
4. **The set of limits:** bytes, channels, upstreams, own publishers.
5. **An upstream run at a limit** stops at its first `tenant-limit` refusal (and, below, at a
   `catalogue-full` one).
