# Spec 0033: The catalogue

- **Status**: implemented (2026-10-25: A1 parts 1, 2, 3a, 4; A2; B; C; part 3b deferred to the
  follow-ups)
- **Changed by** [0037](../0037-distributor-in-a-tenant/spec.md) (PR 2): a distributor's publisher may publish any name kista does not reserve server-wide (tenant reservations are removed, so "nor for another holder" goes, here and in the landing transaction's check); a follow of it takes names as any follow does (no holder check); marking an existing publisher goes for every caller (a distributor's publisher is only created marked; `PUT …/distributor` renames, `404` on an unmarked publisher, and the freshness check goes); distributors' publishers' entries are held for at least the tenant's minimum hold for distributors' publishers (`tenants.distributor_hold_seconds`, a day by default, set by its administrators); the distribution admins' calls are spec 0037's list.
- **Changed by** [0037](../0037-distributor-in-a-tenant/spec.md) (PR 1): a distributor's publisher reads its own catalogue entries with a signed request; a full catalogue's `429` is typed `catalogue-full`; a distribution admin marks no existing publisher (PR 1: a server administrator still marks a fresh one); publisher answers carry the publisher's id.
- **Changed by** [0036](../0036-entry-meta/spec.md) (phase 1): an entry gains metadata (an icon and a summary), kept through withdrawal, purges and source removal; the catalogue answers `distributor`; the event `catalogue.meta`; icons count toward `catalogue.max_bytes` through `icon_bytes`.
- **Date**: 2026-10-24
- **Author**: vgsml, Claude

## Summary

Every tenant gets a **catalogue**: everything that arrives in the tenant, a publication, a mirrored
build or a bundle's, lands there first, as a build of a **source** under a name. Names may coincide
between sources: DuckDB's community `h3`, a distributor's `h3` and the tenant's own `h3` are three
catalogue entries, side by side. A channel **takes** builds from its tenant's catalogue by its
administrator's rules: a name from a chosen source automatically (each new version, once any hold
has passed) or by hand (a version the administrator places), and a whole source automatically.
A source may set a **hold**: its new versions reach no channel automatically before it has passed.
A **distributor** (Enterest) publishes into a tenant's catalogue through its publisher there,
reaches no channel by itself, and can withdraw a version, which is then served nowhere in the
tenant. A licence is bound to the extension's name, its catalogue entry and the organisation it is
issued to (spec 0035). Everything else stays as it is: a
tenant is still the unit (an install has its `default` tenant), with its channels, upstreams,
publishers and grants; the catalogue entry is the one new thing.

## Problem

Today a channel is both where builds arrive and what DuckDB installs from, so every arrival competes
for a name in every channel it reaches:

- **Names collide per channel.** An upstream feeds exactly one channel (spec 0009), a publication
  targets one channel (spec 0008), and spec 0031 decides between them channel by channel: the
  first to land holds the name, the others are refused or recorded as `other_source`. Nothing
  records that DuckDB's `h3` and a distributor's `h3` both exist; a tenant learns of the second only
  from a refusal. A slot (name, version, platform, DuckDB version) is one per channel whatever the
  source, so switching to a source that has the same version under another body serves nothing.
- **The same source is configured once per channel.** Mirroring DuckDB's community repository into
  `prod` and `nightly` is two upstreams, two allowlists, two fetches of each body.
- **A distributor has no place to offer a tenant its builds.** Enterest built its catalogue as a
  second kista (its spec 0003, decisions 26 and 27): an `incoming` channel as the landing place, a
  promotion into `stable` after a holding period, and every tenant's channels mirroring `stable`
  over HTTPS with one credential, with the per-channel name contests above in every channel, and a
  licence waiting until the build has been mirrored into the tenant. Publishing straight into a
  tenant's channels instead decides for the tenant what it serves, and needs its names reserved in
  the tenant (spec 0030) before anything is chosen.
- **A version pending a choice has no home.** What a mirror or a distributor brings either lands in
  a channel, where it is served, or nowhere.

The owner's design (2026-10-24): a catalogue holds what arrives, keyed by where it came from, so
names may coincide; a catalogue per tenant, as everything is; channels take from it, automatically
by the administrator's setting or by hand, also a whole source; a hold on the source, and
optionally on the channel; a licence bound to the name, the source and the organisation.

## Design

### Concepts

| Concept | Meaning |
| --- | --- |
| **Source** | Where a tenant's builds come from, as today: one of its **upstreams** (a mirror of DuckDB's core or community repository, another repository, a bundle's), one of its **publishers** (its CI, or a distributor's publisher), or **`tenant`** (its administrators' adds and its principals' publications). Named `upstream:<name>`, `publisher:<name>`, `tenant`, as spec 0031 names them. |
| **Catalogue entry** | A build of a source in the tenant: (source, name, version, platform, DuckDB version or C API major), verified at arrival against its source's signature, with its licence keys (spec 0021), its visibility, and its state: `held` (until a time), `available`, or `withdrawn`. Key: (tenant, source, name, body hash); a version is immutable within its source. The **catalogue** is a tenant's entries; it is not served to DuckDB. |
| **Rule** | A channel's decision for a name: the source it serves it from (spec 0031's selection) and a mode, `auto` or `manual`. |
| **Follow** | A channel taking a whole source automatically: every name of it the channel has no rule for, within limits (below). |
| **Distributor's publisher** | A publisher a distribution admin or a server administrator marks as a distributor's (`distributor`: the distributor's name, as spec 0030's tenant reservations name their holder): it publishes into the catalogue only, its hold and withdrawals are the distributor's. Not the same mark as spec 0021's licence-key mark, which a distributor's publisher also carries. (Spec 0037 (PR 2): a distributor's publisher is only created marked; tenant reservations are removed.) |
| **Release** | As today, a catalogue entry placed in a channel, signed with the channel's key (the original signature in a passthrough channel). Its slot is now per source: (channel, source, name, version, platform, DuckDB version or C API major). |

A channel is still one DuckDB repository and still serves each name from one source (spec 0031).
What changes is where builds wait, how they reach a channel, and that a slot belongs to a source.
In storage, a catalogue entry is today's build row (spec 0001: tenant-scoped, already apart from
its releases) gaining its source, state and visibility.

### What an administrator does

- **Mirror DuckDB's `h3` into `prod`**: add a community upstream with the entry `h3`, `prod` its
  home channel (spec 0009's routes and CLI as today, `channel` naming it): its runs keep `prod`
  current, and the first release writes `prod`'s rule for `h3`, without a mode (as built in A1 part
  2: the channel serves what lands in it); another channel takes the mirror by a rule with a mode.
- **Take a distributor's builds**: `kista admin channel follow acme/prod publisher:enterest` takes
  every name of it the tenant does not use from another source, each new version once its hold has
  passed; or rule names one by one (`kista admin channel source acme/prod geo_tools -set
  publisher:enterest -mode auto`).
- **Choose versions by hand**: a `manual` rule; `GET …/channels/{c}/sources` shows per name the
  source, the mode, the current placed version, the newest available one and the last outcome;
  `…/releases/place` places a version, and its builds for DuckDB versions or platforms that arrive
  later follow it.
- **Roll back**: yank the bad release (spec 0006: the previous one is current again); with an
  `auto` rule the yanked version is not placed again, and the next version is. To stay on a version,
  switch the rule to `manual`.
- **Switch source**: `PUT …/sources/{name}` with another source: its releases already in the channel
  are served at once, and with `auto` its newest available version is placed by the next pass.

### Sources

- **An upstream** no longer feeds a channel: it brings builds into the catalogue. The channel it
  was made with (or is given: `PUT …/upstreams/{u}/home`) is its **home channel**: adding an entry
  writes that channel's `auto` rule for the name, unless the channel already has a rule for it, so
  today's scripts keep working. Its matrix's DuckDB versions are its home channel's (today's
  matrix) or, without one, every version the tenant's channels serve; channels filter by their own
  versions at placement. An upstream without a home channel only fills the catalogue. (As built: the
  first release writes the home channel's rule without a mode, see As built (A1 part 2); every
  upstream has its home channel, the one it was made with, and the `home` route and command, an
  upstream without one and its matrix without one are A1 part 3b's, deferred to the follow-ups.)
- **Visibility.** An entry's visibility is what its arrival gives it today: a mirror's public, or
  private when its credential or bundle source says so (specs 0009, 0014); a publication's as the
  request asks (`visibility`, private by default, spec 0008). It is the widest a placement may serve
  it with.
- **A hold** is a duration on a publisher or an eager mirror, `0` by default: an entry arriving from
  it is `held` until `arrived_at + hold`. Lazy and pull-through upstreams take no hold (they fetch
  because someone is installing now). Raising a hold applies to entries already held; lowering it
  applies only to entries arriving from then on. A tenant's administrators set the holds of its
  sources; a distributor's publisher's hold is set by a distribution admin or a server
  administrator only, and a tenant that wants more waits with a channel hold (below; spec 0037 (PR 2):
  or raises its minimum hold for distributors' publishers, which their entries always take). Revoking a
  publisher key (`DELETE …/keys/{k}?purge_held=true`, or removing the publisher) purges the entries
  it published that are still held, with an event: a hold is what lets a stolen key be revoked
  before its builds reach a channel. A routine rotation, without `purge_held`, keeps them.
- **Names a source may bring.** An upstream brings what its allowlist names (spec 0009, with
  `allow_reserved` and the alias rules unchanged). A publication needs `publish` on the name as
  today (specs 0008, 0017, 0030: a reserved name only by a grant naming it). A distributor's
  publisher needs no grant: it publishes into the catalogue only, any name kista does not reserve
  server-wide and that is not reserved in the tenant for another holder (spec 0030; spec 0037 (PR 2):
  tenant reservations are removed, any name kista does not reserve server-wide); nothing it
  publishes is served until the tenant's rules take it.
- **Removing a source** keeps its entries and their releases (shown as a removed source's); rules
  naming it keep serving what is placed. Spec 0031's successor stays, narrowed: an upstream of the
  same kind and prefix, added again, takes the rules naming the removed one in its home channel;
  elsewhere the rule stays and shows the removed source until an administrator switches it.

### The catalogue

- **Arrival.** Every intake path lands an entry: a publication, a mirror's run, a pull-through or
  lazy fetch (spec 0032), a bundle import (spec 0014), an administrator's add. The checks are
  today's: the file (`CheckFile`), the footer against the request or the cell, the source's
  signature, blocks, the source's slot (the same version with another body is refused), the
  licence key block (spec 0021, from trusted sources). No channel is involved at arrival, so there
  is no source contest there; the contests of spec 0031 move to placement.
- **Publishing into a channel** stays on today's route, `POST …/channels/{c}/extensions/{ext}/
  releases`, with its grants and answers: it lands the entry and places it into that channel in the
  same request. Every check a placement makes, the source contest (`409`) included, runs first, and
  a refusal leaves no entry, as today stores nothing. A publisher with a hold is refused there
  (`409`, `type …/held`): it publishes into the catalogue (below), and channels take its builds by
  rules once the hold has passed.
- **Publishing into the catalogue only**: `POST /api/v1/tenants/{t}/catalogue/extensions/{ext}/
  builds?version=&platform=[&visibility=]`, signed as spec 0023 says, by a distributor's publisher
  (names above) or by a principal or publisher holding `publish` on the name tenant-wide. The
  answer is the entry (`201`, or `200` for the same body again).
- **Mirrors** run their matrix into the catalogue. Lazy and pull-through (specs 0009, 0032) keep
  their behaviour, for a channel whose `auto` rule or follow for the name names the upstream and
  that has no channel hold:
  - a miss in such a channel records an ask (`upstream_asks` gains the asking channel); when the
    cell's entry is already available (another channel asked first), it is placed into the asking
    channel at once, without a fetch; otherwise the fetch places into the asking channel before its
    lease ends, so the retried request finds the release (spec 0032);
  - a miss in a channel whose rule is `manual` asks nothing and places nothing (the administrator
    pinned the name), as a miss under no rule does;
  - the pass places the fetched entry wherever else rules take it.

  (As built: asks come from the upstream's home channel only, `upstream_asks` gains no channel and
  a fetch places into the home channel; a miss under a `manual` rule lands the entry and misses, see
  As built (A1 part 2 and part 4). The asks of other channels, and a `manual` rule asking nothing,
  are in the follow-ups.) The cell outcomes `other_source` and `shadowed` leave the cells (no
  channel there) and become placement outcomes (as built: they stay, see As built (C)).
- **Withdrawal**, for a distributor's publisher (removed or not): a distribution admin or a server
  administrator withdraws a version of a name from that source in a tenant, with a reason. In one
  transaction its entries become `withdrawn` and every channel of the tenant holding a release of
  them gets its release version bumped; the snapshot reads the entry's state, so such a release is
  served nowhere from then on, and is shown as withdrawn. A release of a withdrawn entry cannot be
  made active, and the body cannot arrive again in that source. The answer names the tenant and the
  version, not its channels. The tenant's administrators are told (`catalogue.withdrawn`); the same
  body from another of its sources is theirs to block. A tenant's own sources need no withdrawal:
  blocks and yanks do it.
- **Retention.** An entry is kept while it has a release; a mirror's while it is its cell's newest;
  a publisher's or `tenant`'s newest three versions per name placed nowhere, the older ones dead
  after spec 0016's grace. A tenant's administrators, and for a distributor's publisher a
  distribution admin, purge an entry placed nowhere (`DELETE …/catalogue/builds/{id}`; never
  placed, its slot is freed, since no one could have installed it). A withdrawn entry keeps its
  record and drops its body reference. Per-source caps (entries and bytes; `catalogue.max_entries`,
  `catalogue.max_bytes` per source, defaults in implementation) refuse arrivals beyond them with
  `429`. Bodies stay deduplicated within a storage domain (spec 0005), so tenants sharing a domain
  store a distributor's build once.
- **Who reads it**: `GET /api/v1/tenants/{t}/catalogue[?source=&name=]` and
  `GET …/catalogue/{source}/extensions/{ext}`, for the tenant's administrators: source, name,
  versions, DuckDB versions, platforms, state (held until when, withdrawn with the reason),
  visibility, whether an entry declares licence keys, and in which of the tenant's channels it is
  placed. A distribution admin reads a distributor's publisher's own entries, without channels or
  placements (spec 0030: distribution admins read no channel). Others see nothing of it.

### Rules, follows and placement

- **A rule** (channel, name) → source and mode, written by a channel administrator (spec 0031's
  `PUT …/sources/{name}` with `{"source": …, "mode": "auto"|"manual"}`), or by a first arrival
  (below), which gives it no mode (as built: the channel serves what lands in it, a mirror's runs and
  publications into it, and the placement pass leaves the rule alone until an administrator gives it
  a mode). `auto` places each new available version of the name
  from the source that the channel serves; `manual` places no new version by itself, but a version
  the channel holds is completed: a build of it that arrives later for another DuckDB version or
  platform the channel serves is placed too, after any hold (the owner's choice, 2026-10-24: an
  administrator chooses a version, not its builds). `DELETE` clears it.
  The administrator who writes an `auto` rule consents for the placements it makes, a reserved
  name included. `tenant` takes `manual` rules only: it is everyone in the tenant who may publish,
  so taking it automatically would let a grant on `dev` reach `prod`.
- **A follow** (channel, source), written by a channel administrator, is evaluated at each pass,
  never copied into rules: for a name with no rule in the channel it places the source's newest
  available version, as an `auto` rule would, and writes the name's rule with mode `follow` (serve
  this source; automatic while the follow exists, `manual` once it is removed). Removing a follow
  places nothing more; what it placed stays. A follow does not take a name the tenant uses from
  another source:
  - a name kista reserves server-wide, unless the followed source is DuckDB's own repository of
    that kind;
  - a name reserved in the tenant (spec 0030) for another holder than the followed publisher's
    distributor (removed by spec 0037 (PR 2), with tenant reservations);
  - a name another of the tenant's sources has entries of, or whose rule in any of the tenant's
    channels names another source;
  - a name two of the channel's follows both offer (`contested`: neither places).

  Each name a follow skips is listed with the reason (`GET …/channels/{c}/follows/{source}`), so
  an administrator sees why a name is missing and writes a rule for it. `dev` and `prod` following
  one source both take its names. A follow on `tenant` is refused.
- **A channel hold**, an optional duration on the channel: an automatic placement into it waits
  until the entry's arrival plus the channel's hold, if that is later than the source's.
- **Placement** inserts a release of an entry into a channel, through today's insert (spec 0008's
  release service): signing with the channel's keys before the transaction, then under the channel
  lock the checks: the entry still not held (unless `early`) nor withdrawn (read with a row lock
  the withdrawal takes too), the block, the slot (per source), the selected source, the channel's
  DuckDB versions; then the release, its signatures, the snapshot bump. A placement by a person or
  a publisher also checks reserved names against its grants, as today.
- **Automatic placements** are made by the placement pass, under a lease (one replica at a time),
  from a queue of due work (an arrival, a hold ending, a rule, follow or channel version changed)
  and an hourly sweep that catches anything the queue missed; it skips suspended and deleting
  tenants. For each `auto` rule and follow concerned it places, per cell, the newest available
  entry of the source that is newer (by spec 0006's version order) than the channel's newest
  release of that source in the cell, and never an entry that has or had a release or a purged slot
  there, so a yanked or purged version is not placed again and an older version whose hold ends
  later does not overtake a newer one. A failure (a block, a slot) is the rule's last outcome, shown
  with it, and is not retried until something changes.
- **By hand**: `POST …/channels/{c}/extensions/{ext}/releases/place` with `{"source": …,
  "version": …, "visibility": "private"?, "current": bool?, "early": bool?}`: every entry of the
  version the channel serves, all or none. It takes `promote` on the channel for the name (reserved
  names only through a grant naming them, as today). A held entry is placed only with `early`, by a
  channel administrator (the tenant's own choice to take a version sooner), audited. CLI: `kista
  admin release place <t>/<c> <name> <version> -source <source> [-early]`.
- **Promotion** (spec 0008) is a placement whose entries are what another channel serves: today's
  route and grants (`promote` on the target, `publish` on the source channel), now also refusing a
  held entry without `early`.
- **First arrival.** A placement under a name with no rule (a publication, a promotion) writes the
  rule without a mode (as spec 0031's arrival); a manual placement where no rule is takes a channel
  administrator, and writes a `manual` rule (as built: anyone else learns nothing, `404`). A placement under a name whose rule names another source is
  `409` (spec 0031's `…/source`).
- **Visibility** only narrows: a placement is public only when the entry is public and the placer
  allows it.
- **Passthrough channels** (spec 0009) take rules only for core names from a `duckdb-core`
  upstream in eager mode (a passthrough request carries no token to ask with), without a hold,
  public, and nothing else is placed into them. A tenant shadow is recorded at placement, never at
  arrival: a body that is not DuckDB's core build placed in a signed channel under a core name or a
  name a passthrough channel's rule names.

### Licences

**Superseded by spec 0035** where it concerns key owners: a licence belongs to a catalogue entry,
every licence write names it (`?source=`), it verifies against that entry's own live keys, the relay
stores into its own upstream's entry, and key owners, their route and events are gone. The text
below is kept as history.

A licence (spec 0012) names its holder (`sub`, an organisation) and the extension (`ext`), and is
signed with a key. It is bound to:

- **the organisation**: the tenant's expected holder (spec 0028) is checked on every path, as
  today; a licence for another holder is never stored;
- **the name**: `ext`;
- **the source**, by its key. In a tenant, for each (name, key), one source is the key's
  **owner**: the first source whose available entry or release of the name recorded the key (spec
  0021's records; held entries do not count). Another source declaring the same key for the name
  gets no licence through it, and an event (`license.key_conflict`) tells the tenant's
  administrators. Ownership changes only when the owner goes (spec 0021 drops a removed source's
  keys; the next recorder owns it; as built: the next source to record it afresh) or by a tenant
  administrator's act (`PUT …/license-keys/{name}/{kid}/owner {"source": …}`). A licence's source is
  its key's owner for its name.

What changes:

- **Stored per (tenant, name, source)** instead of (tenant, name): a tenant may hold licences for
  two extensions of one name from different sources. Importing a licence replaces the one of the
  same (name, source), with spec 0021's same-key condition. The relay (spec 0012) applies a licence
  only when its key's owner for the name is the relaying upstream, and replaces by (name, that
  upstream).
- **Served by the channel's selected source**: `-/licenses/<name>` in a channel serves the licence
  whose source is the channel's source for the name, when that source has a live release there
  declaring the key (spec 0021's rule). There is never more than one.
- **Live keys.** A key is live when a release in one of the tenant's channels declares it (live, as
  spec 0021 says), or when the newest available, not withdrawn, version of the name in its owner
  source declares it: a licence is accepted as soon as the extension is available in the catalogue,
  without waiting for a placement, and a rotated-out key stops counting once newer versions declare
  another and old releases are yanked (spec 0021's rotation).
- **Usage** (spec 0012) is counted per channel as today; forwarding goes to a name's selected
  source in that channel when it is a `repository` upstream with forwarding on (instead of the
  provenance query).

The source is not a claim in the licence: a product reaches tenants through different sources along
a relay chain (a distributor's publisher in one kista, an upstream in an organisation's own kista),
and in each tenant the key's owner is the source there.

### The API and the CLI

- **Catalogue**: the read and publish routes above; `DELETE …/catalogue/builds/{id}` (purge);
  `PUT …/upstreams/{u}/hold`, `…/publishers/{p}/hold`, `PUT …/upstreams/{u}/home
  {"channel": …}` (as built: not made, with A1 part 3b in the follow-ups); `PUT
  …/license-keys/{name}/{kid}/owner` (removed by spec 0035); for a distributor's publisher,
  `PUT …/publishers/{p}/distributor {"distributor": …}` and `POST …/catalogue/{source}/extensions/
  {ext}/versions/{v}/withdraw {"reason": …}` (distribution admins and server administrators).
  Spec 0009's and 0023's routes are otherwise unchanged. CLI: `kista admin catalogue list|show|
  purge|withdraw …`, `kista admin upstream hold|home …` (as built: `hold` only), `kista admin
  publisher hold|distributor …`.
- **Channel**: `…/channels/{c}/sources[/{name}]` gains `mode`, the current and newest available
  versions and the last outcome; `GET|PUT|DELETE …/channels/{c}/follows/{source}` (the skipped
  names on `GET`); `PUT …/channels/{c}/hold`; `…/releases/place`. CLI: `kista admin channel source
  <t>/<c> <name> -set <source> [-mode auto|manual]`, `kista admin channel follow <t>/<c> <source>`
  (as built: `channel unfollow` removes it), `kista admin channel hold <t>/<c> <duration>`.
- **Distribution admins** (spec 0030) gain, for distributors' publishers: marking them, setting
  their hold, withdrawing and purging their versions, reading their entries (spec 0037 (PR 2): the
  role's calls are that spec's list; an existing publisher is never marked, a distributor's is
  created marked).
- **Events**: `catalogue.arrival`, `catalogue.hold`, `catalogue.withdrawn`, `catalogue.purge`,
  `source.set` (with `mode`), `source.clear`, `follow.set`, `follow.clear`, `release.place` (with
  `early`), `license.key_conflict`, `license.key_owner` (both removed by spec 0035); a placement's
  failure is its rule's last outcome, not an event (as built, see As built (A1 part 2): spec 0031's
  `source.set` and `source.clear` kept, no `rule.set`, `rule.clear` nor `placement.failed`).
- **Existence**: answers about the catalogue follow spec 0007's rules; install and anonymous callers
  get `404` as for any management route.

### Data model

One migration per phase, each with its `min_reader` (an older replica would place nothing, read
slots by the old key, serve withdrawn entries or the wrong licence). In the three dialects:

```text
A1  builds               + source (as releases.source), visibility, legacy_duplicate (as built:
                           held is A2's available_at, withdrawn B's own marker);
                           unique (tenant_id, source, name, body_hash);
                           unique slot (tenant_id, source, name, ext_version, platform, slot)
                           where not legacy_duplicate (below)
    releases             slot unique (channel_id, source, name, ext_version, platform, slot)
    purged_slots         + source ('*' for slots purged before: taken for every source)
    upstreams            + home_channel_id (from channel_id, which goes)
    channel_sources      + mode (auto, manual, follow), last_outcome
    channel_follows      channel_id, source, set_by, set_at; pk (channel_id, source)
    follow_skips         channel_id, source, name, reason; pk (channel_id, source, name)
    upstream_asks        + channel_id (as built: not added; asks stay the home channel's)
A2  upstreams, publishers, channels   + hold_seconds
    builds               + available_at (held until; set at arrival), publisher_key_id,
                           revoked_at (kept by a purge of held entries)
    channel_sources      + due_at (when the hold a rule waits for ends)
B   publishers           + distributor (part 1: the distributor's name, NULL for the tenant's own)
    builds               + withdrawn_at, withdrawn_reason (part 1)
    withdrawn_versions   tenant_id, package_type, source, name, package_version, reason,
                           withdrawn_by, withdrawn_at; pk (tenant_id, package_type, source, name,
                           package_version) (part 1, as built: a version withdrawn for good, below)
    removed_distributor_publishers   tenant_id, publisher_id, name, distributor, removed_at,
                           removed_by; pk (tenant_id, publisher_id) (part 1, as built: a removed
                           distributor's publisher remembered, below)
    license_key_owners   tenant_id, name, kid, source, set_by; pk (tenant_id, name, kid) (part 2;
                           dropped by spec 0035's migration 0043)
    licenses             pk (tenant_id, name, source) (part 2; spec 0035: the source is the
                           licence's entry, entry_source)
```

GC (spec 0016): a build (entry) is dead when it has no release and retention (above) does not keep
it; bodies as today. Tenant deletion (spec 0019) reaps the new tables with the tenant.

### Existing data

The migrations keep what every channel serves, and what arrives next goes where it goes today:

- every upstream's channel becomes its home channel; that channel gets an `auto` rule for each name
  of its allowlist (an existing selection naming the upstream becomes `auto`), and a follow when the
  allowlist has `*` (pull-through, so it works from A1 on) (as built: no rule gets a mode and nothing
  is followed by the migrations; a pull-through upstream keeps serving its home channel by its own
  fetches);
- every other existing selection (a publisher, `tenant`, an upstream selected in a channel that is
  not its home, by promotion) becomes `manual`: a CI publishing into `dev` does not start reaching
  `prod`, and `nightly`'s mirror does not start flowing into `prod` (as built: it keeps no mode,
  and the placement pass leaves such a rule alone, so nothing new reaches `prod` by it either, see
  As built (A1 part 2));
- every build gets the source its releases name; a build whose releases name two sources (spec
  0031's same-body rule) is split into one per source, and every reference to it (releases,
  licence-key records, cells) repointed; all are `available`;
- slots get their source from the release; two bodies of one version under one source in two
  channels (possible today) are both kept, all but the oldest marked `legacy_duplicate` (outside
  the unique slot), and listed in the upgrade notes; a new arrival conflicting with any is refused;
  a slot purged before the migration has no release to read its source from, and stays taken for
  every source;
- (B) key owners, and licences of source `unknown`: removed by spec 0035, whose migration 0043
  moves relayed licences into their relaying upstream's entry and deletes the `unknown` imports;
- tenants with two upstreams of the same repository (one per channel) keep two sources; each
  stores its own copy of the licence through its relay (spec 0035).

The cells API loses `other_source` and `shadowed` (now placement outcomes, on rules), and the
console's Sources tab shows modes: both are in the upgrade notes. (As built: the cells keep them,
see As built (C); the upgrade notes say so.)

## Security

- **Tenant isolation is unchanged**: the catalogue is the tenant's, read and placed by it; a
  distributor reaches it only through its publisher and the distribution admins (spec 0030), sees
  its own entries only, and reaches no channel by itself.
- **The hold**: automatic placement, manual placement and promotion refuse a held entry, except a
  channel administrator's audited `early` (the tenant's own choice); the channel route refuses a
  publisher with a hold; lowering a source's hold does not release entries already held, while a
  channel's hold, the tenant's own choice for its own channel, applies at once to the automatic
  placements waiting for it, lowered or raised; revoking a key purges its held entries, and those a
  channel administrator placed early stay where they are, revoked: no rule, follow or promotion
  takes them again, only another `early` placement; a distributor's hold is not the tenant's to set.
  A tenant publisher without a hold that holds `publish` on a channel reaches that channel at once,
  as today.
- **Intake against revocation**: a publication's build is written under the lock that removing its
  key or its publisher takes, and only while both exist, and its release on a channel's route checks
  them again under row locks the removal waits for, so an upload in flight when they are removed
  lands nothing and is released nowhere (it fails as a revoked key's request does); a removed
  upstream brings
  nothing either. A source's hold is read with a lock its raise waits for, so a build arriving while
  a hold is raised is held the raised time.
- **Withdrawal is immediate and final**: the withdrawal's transaction bumps every channel holding a
  release of the entry, and serving reads the entry's state; placement re-checks it under the
  channel lock; a withdrawn entry's release is never re-activated, nor does the body re-arrive in
  its source. The distributor learns nothing about the tenant's channels from it.
- **Fail closed.** Nothing falls back to another source by itself (spec 0031); a follow never takes
  a name the tenant uses from another source, a name two follows offer, nor a reserved name from a
  foreign source; `tenant` is never automatic.
- **Grants keep their meaning**: placing needs `promote` for the name on the target; only a channel
  administrator or a publisher into the channel writes a rule by arrival; a publish grant on `dev`
  reaches `prod` only through a rule `prod`'s administrator wrote for that publisher. A
  distributor's catalogue-only publication needs no grant because it reaches no channel by itself;
  per-source caps bound what it can store.
- **Verification happens once**, at arrival, against the source's signature; placement re-signs
  with the channel's key and re-checks blocks, withdrawals, holds, slots and the source under the
  channel lock.
- **Licences**: the expected holder on every path; keys from trusted sources only (spec 0021); a
  licence belongs to a catalogue entry and verifies against that entry's own keys (spec 0035), so
  another source's build carrying a copied key is another entry, with no licence until one is put
  there, and takes nothing from the genuine entry.
- **Existence.** Install and anonymous callers learn nothing new: the catalogue is not served, and
  the DuckDB routes and the index are unchanged.

## Testing

- The store suite on three dialects: entries per source (two sources, one name, one body, one
  version); slots per source; rules, modes, follows, their limits and skips (contested,
  tenant-reserved for another holder, removed by spec 0037 (PR 2)); holds (raised, lowered, the channel's, a distributor's);
  withdrawal; licences per (name, source) (spec 0035: per entry; key owners removed); each
  migration's backfill from a spec 0031 database (home
  channels, `auto` and `manual`, pull-through follows, splits, duplicate and purged slots, `unknown`
  licences; as built, the migrations give no modes and no follows, so their tests check that none
  is given, see Existing data).
- The placement pass: idempotent, leased, queued, newest-only ordering (a held older version does
  not overtake), yanked and purged versions not re-placed, failures recorded, suspended tenants
  skipped; a placement racing a withdrawal; a revoked key purging held entries.
- Intake: every path lands an entry; the channel route places in the same request and refuses a
  held publisher; the catalogue-only route places nothing; an upstream with a home channel serves
  new entries; lazy and pull-through through a rule or a follow fetch and place within one
  request's retry window (as built: through the home channel only, see the Mirrors item above).
- Licences: imported before any placement; one served per channel by its source.
- The existing DuckDB end-to-end suite passes unchanged after each phase.
- New e2e with the pinned DuckDB: a tenant's `repository` upstream (another kista) and a
  distributor's publisher both bring `geo_tools`; `prod` serves the upstream's by its home rule,
  `nightly` has an `auto` rule for the distributor's and gets its builds after the hold (`INSTALL
  geo_tools FROM …`, the right body each); a follow of the distributor takes its other names and
  lists `geo_tools` as skipped; a withdrawal stops `nightly`'s install at once; a held entry placed
  `early`; a licensed extension's licence imported before placement, served once placed, and
  verified by the SDK (as built: `TestCatalogueSources` and `TestLicenseFromDistributor`, see As
  built (C) and (B part 2)).

## Delivery

One spec, four phases, each a PR with its own reviews and its own migration:

- **A1. The catalogue**: entries per source, upstreams with home channels, slots per source, rules
  with `auto` and `manual`, follows (with their limits and skips), the placement pass and manual
  placement, the channel route through the catalogue, the catalogue-only route for tenant
  publishers, the read routes, retention and caps, the migration.
- **A2. Holds**: source and channel holds, `early`, the channel route refusing held publishers,
  revoked keys purging held entries.
- **B. Distributors and licences**: distributors' publishers, distribution admins' holds,
  withdrawals and purges, licences per (name, source) with key owners, relay and usage (the licence
  part superseded by spec 0035: licences per catalogue entry).
- **C. The console and the end to end**: a Catalogue screen (sources, names, versions, held and
  withdrawn, place, purge), the channel's Sources tab with modes, follows, skips and the hold, the
  new DuckDB end to end, the website's pages.

B comes in two parts, each a PR: part 1, distributors' publishers, distribution admins' holds,
withdrawals and purges (migration 0041); part 2, licences per (name, source) with key owners, relay
and usage.

A1 comes in parts, each a PR: part 1, builds and slots per source (migration 0034); part 2, the
rules' modes and the placement (the pass, `auto` and a `manual` rule's completed versions, manual
placement, the catalogue's read routes, a mirror under a `manual` rule landing its builds without
placing them); part 3, upstreams without a home channel, the catalogue-only route, retention and
caps, the catalogue's own slot uniqueness; part 4, follows. Part 3 came in two, 3a (the catalogue's
own entries) and 3b (upstreams without a home channel); 3b is deferred to the follow-ups (the
owner's choice, 2026-10-24): a mirror already feeds other channels through their rules, and
Enterest does not need it.

### As built (A1 part 1)

- **Migration 0034** gives `builds` a `source` (its first release's; a build without releases, by
  who made it, as migration 0032 read releases' actors) and makes it unique per (tenant, source,
  name, body); a build whose releases named two sources (spec 0031's same-body rule) is split, each
  copy taking the licence-key records its source made, and its releases repointed (no other table
  holds build ids). `releases`' slot and `purged_slots`' key gain the source; a slot purged before
  has the source `*` and stays taken for every source. The columns keep no default.
- **SQLite** cannot change a unique key in place, nor drop a table other tables reference while
  foreign keys are on, even deferred. A migration marked `-- +foreign_keys_off` runs on a connection
  of its own with them off (SQLite's documented procedure: a pragma a transaction cannot change),
  checks `PRAGMA foreign_key_check` before it commits (naming the first broken row), and the
  connection is discarded afterwards, never pooled.
- **Intake** finds and inserts builds and checks slots within the release's source, and refuses a
  release whose build is another source's; spec 0031's same-body rule and its `SetReleaseSource` go.
  A source bringing a body another source released in the channel makes a release of its own
  (`201`, not `200`). An upstream succeeding a removed one takes the removed one's releases of the
  name in the channel, and its purged slots, so a yank, a purge or a choice survives it.
- **Only the selected source counts**: a licence key is live (spec 0021), an upstream relays
  licences and receives usage (spec 0012), through releases of the source the channel selected for
  the name; another source's releases stay stored and unserved.
- **Left to later parts**: the entries' state, `available_at` and publisher key (A2); the
  catalogue's own slot uniqueness and `legacy_duplicate` (part 3a, done). The data model's "one migration per
  phase" is one per part in A1.

### As built (A1 part 2)

- **Modes are an administrator's.** A rule a first arrival writes (a mirror's release in its
  channel, a publication, a promotion) has no mode: the channel serves what lands in it, as before
  this spec, and the placement pass leaves it alone; so the upgrade changes nothing that arrives.
  `PUT …/sources/{name}` with `mode`, or `channel source … -mode`, sets `auto` or `manual`; a placement
  by hand where no rule is writes `manual`. `tenant` is never `auto`, and the pass never places from
  it (a manual rule of it would let a grant on `dev` complete `prod`'s versions); a passthrough
  channel's rules take no mode. Migration 0035 adds `mode` (`''`), `checked_at`, the last `outcome`;
  builds gain `visibility` and `provenance` (as their first release that is not a promotion came)
  and `landed`.
- **The placement pass** (`release.Placer`, every minute, a lease per channel renewed before each
  build): rules with a mode, of signed channels of active tenants, whose source has builds made since
  its last look (taken 5 minutes back, for transactions that commit late), and all of them every hour;
  a DuckDB version added to a channel clears its rules' looks. Cells are (platform, DuckDB version),
  so a C API build counts for every DuckDB version it serves. An `auto` rule places, per slot, the
  newest checked build that is newer than every version its cells have or had and than any other
  build placed with it; both modes complete an active version's slots, not current where a newer
  version serves a cell and private where the version is private in the channel. A build whose body
  a purge took (released nowhere, a purged slot) is never placed. A mirror's build placed keeps
  origin `upstream` and its original signature (licences, usage and bundles go on), with `placed` in
  its provenance; any other is origin `placement`. Event `release.place`; the last failure is the
  rule's outcome; the look's bookkeeping is skipped when the rule changed since it began.
- **Placing by hand** (`POST …/releases/place`, `release place`, `promote` on the name): a channel
  administrator places any checked build of the catalogue; anyone else only builds of the rule's
  source active or deprecated where it may publish the name (what a promotion takes), and gets `404`
  for anything else (no rule, no such source, no such build).
- **Visibility**: making a release private makes its build private, so no placement serves it
  publicly again.
- **A mirror under a manual rule** (its own, or a removed one's a successor answers to) lands a
  version the channel does not hold: the body committed, the build recorded with its keys and
  marked `landed` (kept from the collector until a release takes it), no release; the cell's outcome
  is `landed`. A version it holds is completed as the pass would. Lazy and pull-through fetches go
  through the same intake: under a manual rule they land, and the request misses (the Design's "asks
  nothing" is left to part 4). The land path skips the release's reservation and `*` checks, which a
  placement makes.
- **Events** are `source.set` (with `mode`) and `release.place`, not `rule.set` and
  `placement.failed`; a failure is the rule's outcome. The read routes do not show entries' state
  (holds come in A2) nor whether a build declares licence keys.
- **Left**: a recheck racing a running look waits for the hourly sweep; purged slots from before
  migration 0034 (`*`) keep older versions of every source out of their cell.

### As built (A1 part 3a)

A1 part 3 comes in two: 3a, the catalogue's own entries (below); 3b, upstreams without a home
channel, deferred.

- **A version is immutable within its source in the whole tenant** (migration 0036: builds gain
  `slot` and `legacy_duplicate`; a unique key (tenant, source, name, version, platform, slot) where
  not a legacy duplicate and the source is not `tenant`, a partial or filtered index in each
  dialect). Before a body is stored, and again at the insert, another body of the source's slot is
  refused: among its builds, legacy duplicates included, and its purged slots in the tenant's
  channels (a version once released stays the source's after its build is collected; a slot purged
  before migration 0034, `*`, stays its channel's matter). `409` as a slot; a mirror's cell
  `conflict`. **The tenant's own source keeps the rule per channel** (found in review: it is
  everyone who may publish in the tenant, so tenant-wide it would let one principal squat another's
  versions). Of two bodies a source released in two channels before, the released one (else the
  oldest) keeps the key. A successor upstream copying a removed one's build whose slot it holds with
  another body inserts the copy as a legacy duplicate.
- **Publishing into the catalogue only**: `POST …/catalogue/extensions/{ext}/builds` (`publish` on
  the extension tenant-wide, publishers signed as on a channel's route: the statement is checked,
  not recorded on the build), and `kista admin catalogue add` (a tenant administrator's, source
  `tenant`; the file checks always run, an unchecked build could never be placed). The build's
  DuckDB version must be registered on the server (not checked for a C API build). A name reserved
  during the upload is checked again before the build lands. The build lands, its event
  `catalogue.arrival` in the same transaction; the same body again answers the build as it is.
- **Retention**: a landed build is kept until a release takes it, a publisher's or `tenant`'s among
  the `KeepLanded` (3) newest landed versions of its (source, name, platform, slot), a mirror's while
  one of its upstream's cells holds its body; the placement pass's hourly sweep leaves the others to
  the collector (their grace starts then).
- **Purging an entry**: `DELETE …/catalogue/builds/{id}`, `kista admin catalogue purge` (tenant
  administrators): a build never released (no release, no purged slot of its body) is deleted with
  its key records (`catalogue.purge`); its slot is free again, and a mirror's cell that landed it
  loses its ETag, so the next run fetches it again.
- **Caps**: `catalogue.max_entries` (20,000) and `catalogue.max_bytes` (100 GiB, a size) bound a
  source's builds placed nowhere (its catalogue backlog: released builds are the channels'),
  checked before a new build is committed, dry runs included, outside any lock (concurrent intakes
  may pass together, bounded by the upload slots); an administrator's add is not bounded. `429` on
  the API, a mirror's cell `rejected`.

### As built (A1 part 4)

- **Migration 0037**: `channel_follows` (channel, source, who, when, `checked_at`) and
  `follow_skips` (channel, source, name, reason); a rule's mode may be `follow`.
- **A follow** (`PUT|DELETE …/channels/{c}/follows/{source}`, `kista admin channel follow|unfollow`;
  a channel administrator; an upstream or a publisher, never `tenant`, never into a passthrough
  channel; events `follow.set`, `follow.clear`). The placement pass looks at the follows whose source
  has builds made since its last look (all of them hourly), with the channel's rules under the same
  lease: for each name of the source with no rule in the channel it writes the rule (mode `follow`,
  actor `system:placement`, a `source.set` event with `arrival: true`) and places as an `auto` rule;
  a `follow` rule acts as `auto` while the channel follows the source and as `manual` after.
- **Limits**, each a skip listed with its reason: the channel's rule naming another source; a name
  another follow of the channel offers (`contested`); a core or community name unless the source is
  DuckDB's own repository of that kind; a name reserved in the tenant (any holder: a distributor's
  exception comes with its publishers, B; spec 0037 (PR 2): removed with tenant reservations, and a
  follow of a distributor's publisher skips a name an upstream of the tenant lists instead); another live source of the tenant with builds of the
  name, or a rule of any of its channels naming another source ("the tenant uses it from another
  source"); and, for a publisher, **a name it may not publish in the channel** (found in review: a
  follow is consent to a source, not to every grant anyone gives it anywhere in the tenant; a
  distributor's publisher, which has no grants, comes in B). The tenant's names and rules are read
  once per look. A name taken earlier keeps its rule if it becomes contested or used elsewhere
  later.
- **A rule an arrival wrote** (no mode) for the followed source, a publication into the channel or a
  promotion, becomes the follow's (mode `follow`, a `source.set` with `arrival: false`); a rule an
  administrator set keeps its mode. A rule cleared by an administrator is taken again by the
  follow's next look: to keep the name out, a rule naming another source does.
- **A source removed** (an upstream, a publisher) removes its follows and their skips; the rules they
  wrote stay, acting as manual. An administrator switching a follow's rule to another source without
  a mode makes it manual. At intake, a follow's rule of a channel no longer following its source is
  manual too (a mirror lands its new versions).
- **Bookkeeping**: the lease is renewed before each follow and every 50 names; a failure on one name
  is that name's (the rule's outcome) and the look goes on; each look replaces the skips, so a skip
  whose cause went away is taken at the next look of the source (a new build, or the hourly sweep).
- **Left to later parts**: lazy and pull-through fetches through a follow (an ask comes from the
  upstream's home channel only; with A1 part 3b); a distributor's tenant reservations (B).

### As built (A2)

- **Migration 0040** (`0040_holds`, `min_reader` 40: an older replica would place held and revoked
  entries and take a held publisher's publications on a channel's route) in the three dialects:
  `hold_seconds` (`0` by default) on upstreams, publishers and channels; builds gain `available_at`
  and `publisher_key_id` (left to A2 by A1 part 1) and `revoked_at` (below), and rules `due_at`
  (below). Additive columns only: no SQLite rebuild, SQL Server's text column BIN2.
- **An entry's state is not a column**: an entry is `held` while its `available_at` is later than
  now (`NULL`: available on arrival, every existing build), so a hold ends without a write; `withdrawn`
  comes with its own marker and its reason in B. The hold is read when the build is inserted (in
  its transaction, from its source's row, with a shared row lock held until commit: `FOR SHARE` on
  PostgreSQL, `HOLDLOCK, ROWLOCK` on SQL Server, the existing tenant-row helper made generic, so a
  raise's update waits for the insert and its `RaiseHeld` then finds the build); a removed upstream
  or publisher there is `store.ErrSourceGone` (fail closed: it brings nothing; a mirror's cell
  `failed`). A build found again keeps its own; a successor's copy of a released build (spec 0031's
  `TakeReleases`) keeps the original's arrival (`created_at`, which a channel's hold counts from),
  `available_at` and `revoked_at` (`Build.Copied`, replacing the misnamed `NoHold`).
- **A hold** is a duration of whole seconds, `0` to 90 days: `PUT …/upstreams/{u}/hold`,
  `…/publishers/{p}/hold` and `…/channels/{c}/hold` take `{"hold": "24h"}` (the upstream's route takes
  an optional `If-Match`, as its other changes require one), and answers show `hold` as a Go duration
  (omitted on an upstream and a publisher without one; `GET …/channels/{c}/hold` reads a channel's).
  CLI: `upstream hold`, `publisher hold`, `channel hold` (`24h`, `2d`, `0`). Event `catalogue.hold`
  (`source` or `channel`, `hold`, `before`, and `held`, how many held entries a raise held longer).
  Raising a source's hold sets each entry held now to its arrival plus the new hold, if later.
- **Who**: a tenant's administrators set an upstream's and a publisher's hold, a channel's
  administrators the channel's. A distributor's publisher's hold, and the distributor mark, are B: the
  publisher's route is a tenant administrator's, and a distribution admin is refused there.
- **An eager mirror only** (the most conservative reading of "a publisher or an eager mirror"): a
  lazy mirror, a pull-through upstream, a bundle-mode upstream and an upstream of a passthrough
  channel are refused a hold (`400`), and a mirror with a hold is not made lazy (`400`: set it to `0`
  first). A channel's hold is refused on a passthrough channel. An upstream removed and added again
  (spec 0031's successor) is a new upstream with no hold: its administrator sets it again (the
  docs say so); the successor's copies of the removed one's released builds keep theirs.
- **A mirror's own channel.** As built in A1 part 2, a mirror's runs release into its channel under a
  rule without a mode; such a release is an automatic placement, so it waits for the source's hold and
  the channel's: the run lands the entry (cell outcome `held`, kept with its ETag) and writes the
  name's rule as its release would (the first arrival; a successor taking a removed upstream's rule
  and releases). The placement pass places it there when the hold ends. Which entries are the pass's
  does not rest on `landed` (another channel's release, by an auto rule, a follow or an early
  placement, clears it, and the trim unlands a build whose cell moved on): the entries **held at
  their arrival** are, those with `available_at` set. A source's hold sets it; an entry held by the
  home channel's hold only gets `available_at` = its arrival (`store.HeldAtArrival`: not held by
  itself, so other channels take it as their own holds allow, but marked as left to the pass), the
  one marker without a column of its own. `store.DueRules` takes a mirror's rule without a mode in its
  home channel while the mirror has such entries of the name, woken like any rule (a new build,
  `due_at`), and the pass places them as an `auto` rule would (`placed: "home"`): those not released,
  yanked or purged in the home channel (its own releases, whatever happened elsewhere) and newer
  than the channel's newest, each when its hold ends, so an upstream that moves on faster than its
  hold has each version placed in turn, older first. It makes the run's checks as the run would make
  them then (`homeOf`): nothing while the upstream is paused or gone or homed elsewhere (the hourly
  sweep looks again after a resume), nothing of a name or a version out of its allowlist or a
  platform it no longer takes; a name `*` took is re-checked under the insert's locks (`anyKind`:
  a name reserved since, in the tenant or as core, or listed by another upstream, is refused, the
  rule's outcome), and DuckDB's names are reserved as the run reserves them. Only an eager mirror's
  (or a bundle-mode upstream's) entries are placed so: once a mirror is made lazy, the entries it
  left held are its askers' to fetch, or a channel administrator's to place by hand. The trim
  (`StaleMirrorLanded`) keeps a mirror's landed build while it is held (its own hold or its home
  channel's), whatever its cell holds by then, and while its upstream is paused if it was held at
  its arrival, so the sweep after the resume places it (it trims it then, as the pass's placement
  in the same sweep keeps it). When a run finds an entry it already has and leaves it to the pass,
  the home rule is made due at once (`RecheckRule`: `checked_at` cleared, `due_at` now), since the
  entry's old arrival would not wake the pass. A bundle import into a channel with a hold does the
  same.
- **The channel's hold** applies to automatic placements: rules, follows, a mirror's runs into its
  own channel. Publications on the channel's route, an administrator's `release add`, placing by hand
  and promotion do not wait for it. A channel with a hold offers no cell to a lazy mirror or a
  pull-through upstream (its snapshot holds none; setting the hold renews it). Unlike a source's,
  a channel's hold is not recorded on the entries: changing it applies at once to every automatic
  placement waiting for it, lowered as well as raised (the tenant's own choice for its own channel,
  the Security section says so).
- **Due work**: a rule's look records `due_at`, when the first hold it waited for ends (a source's or
  the channel's); `DueRules` takes a rule whose `due_at` has passed. Setting a channel's hold makes its
  rules due (a shorter hold ends sooner); a hold raised since a look wakes the pass early, which
  looks again. A placement refused under the lock because a hold was raised meanwhile is looked at
  again a minute later. The hourly sweep is unchanged. Placing only versions newer than what the
  channel had keeps an older version whose hold ends later from overtaking a newer one. Making a
  channel's rules due (`RecheckRules`: a DuckDB version added, the hold changed) sets `due_at` to now
  besides clearing `checked_at`, and a look begun before it does not record itself over it (its
  update requires `checked_at` set or `due_at` before the look's start: times are of microseconds in
  the three dialects, and a recheck in the look's own microsecond counts as after it, so the rule is
  looked at once more); a rule changed by an administrator clears `due_at`. A look that fails with an error (not a placement's refusal) records
  only its outcome (`SetRuleOutcome`): `checked_at` and `due_at` stay, so the next pass looks again.
  `DueRules` reads the home channel of the upstreams of the rules without a mode it lists, not every
  upstream.
- **`early`** on manual placement only (`"early": true`, `release place -early`): a channel
  administrator (admin on the channel, with `promote` on the name as any placement) places held
  entries; anyone else asking for it is refused as unauthorized (`404`, nothing said). Without it a
  held entry is `409` with the problem type `…/held`. `release.place` carries `early: true` when a
  held entry was placed so. A promotion takes no `early` (the conservative reading): it always
  refuses a held entry, placed early in its source channel or not, and a channel administrator places
  the version early in the target instead. The insert checks the entry's `available_at` again under
  the channel lock (a hold raised meanwhile counts); the withdrawal's row lock is B's.
- **The channel route** refuses a publisher with a hold after authorization and before the upload
  is read (`409`, `…/held`, the detail naming the catalogue's route); a publication whose build
  turns out held under the lock (the hold set meanwhile) is refused the same way. A publisher
  without a hold publishing a body it published into the catalogue while it had one finds that
  entry held (`409`: lowering a hold does not release entries already held).
- **Purging held entries**: `DELETE …/keys/{k}?purge_held=true` (`true` or `false`, `400`
  otherwise; `publisher key remove -purge-held`) purges, in the key's removal transaction, the
  entries published with the key (`publisher_key_id`, recorded from the signed request) that are held
  now and were never released; removing the publisher does so for all its held entries. Each purge is
  a `catalogue.purge` event with `reason` (`publisher.key.remove` or `publisher.remove`) and the key;
  `publisher.key.remove` gains `purge_held`. `purge_held=true` is a tenant administrator's: a
  distribution admin asking for it is refused (`404`, as its other refusals) until B designs a
  distributor's purges; it still removes a marked publisher's key without it.
- **Revoked entries**: a held entry the purge keeps (placed early, or released once and purged
  since) is marked revoked (`builds.revoked_at`, the conservative choice over deleting a release a
  channel administrator chose): it stays served where it is placed, and is never placed
  automatically again (rules, follows, a mirror's home rule) nor promoted; placing it by hand takes
  `early`, a channel administrator's (`409 …/held` otherwise, as a held entry, the detail saying it
  was revoked), checked again under the channel lock with the hold. The removal transaction takes
  the tenant's upstream lock after the auth lock, the lock every release into the tenant's signed
  channels takes, and the purge checks each entry's releases and purged slots under it before
  deleting it (no longer a foreign-key error caught mid-transaction, which aborts it on PostgreSQL
  and on SQL Server with `XACT_ABORT`). The key removal with `purge_held=true` answers `200` with
  `{"purged": [{build, name, version, platform, slot}], "kept": [… , "channels": n]}` (the tenant's
  channels holding a release of it; the tenant's own, so its count is no leak), `204` without it
  as before; `publisher key remove -purge-held` prints the count and the kept entries; the removal's
  event (`publisher.key.remove`, `publisher.remove`) carries `purged` (how many) and `kept` (each
  with `channels`). Publisher removal's answer stays `204`.
- **Intake against revocation**: a publication's build transaction (catalogue route and channel
  route alike) takes the tenant's auth lock (`store.PublisherAuthLock`, the lock removing a key or a
  publisher takes) and requires its publisher and the key that signed it to exist
  (`store.PublicationLive`), so an upload in flight when its key is revoked, or its publisher
  removed, is refused (`store.ErrSourceGone`, answered `401` as a revoked key's request) and never
  lands after the removal's purge. The chosen lock over a row lock on `publisher_keys`: one order
  of locks with the removals, no deadlock between a removal's tenant-row update and an intake's
  shared tenant-row lock on SQL Server. On a channel's route the release is written later, in
  `insert`'s transaction (the signatures made between, outside any transaction), so `insert` checks
  again for a publication item, reading the publisher's and the key's rows with shared row locks
  (`PublicationLive` reads them so everywhere) that a removal's `DELETE` waits for: a key revoked
  (with `purge_held` or not) or a publisher removed in between releases nothing (`401`), and a
  publication that finds its release already made is refused as well once its key is gone. Not the
  auth advisory lock there: `insert`'s grant re-check may read the tenant's auth on a second
  connection under that lock (`GetTenantAuth`'s locked read), and would wait on its own
  transaction; for the same reason the check comes after the re-check.
- **Lock order of the purging removals**: a removal that purges (key removal with `purge_held`,
  publisher removal) takes the tenant's upstream lock first, then its auth lock (`purgeLocks`). An
  insert holds the upstream lock and may wait, on another connection, for the auth lock (the grant
  re-check's locked read, taken only when the tenant's auth changes during its read); with the
  upstream lock first, a removal never holds the auth lock while waiting for the upstream lock, so
  the purge adds no cycle. What remains is spec 0030's: an insert holding a shared lock on its
  tenant's row while that locked read waits for an auth change that waits to update the tenant's
  row; the lock timeout breaks it and the change is retried (rare: the auth must change during the
  read).
- **A revoked entry is its body's for good** in its source: a body arriving again is the same
  catalogue entry (source, name, body), revoked, so publishing the same bytes again, even with a
  new, legitimate key, is refused on a channel's route (`409 …/held`) and answered with the revoked
  entry on the catalogue's route, never placed automatically. A new version is needed; a channel
  administrator may still place the revoked one early (the conservative choice: a revoked key's
  bytes are not trusted again by being sent again).
- **Reads**: the catalogue's entries list their `held` and `revoked` versions; its builds show
  `state` (`held` or `available`), a held one's `available_at` and a revoked one's `revoked_at`, also
  in the catalogue-only route's answer (`catalogue list`'s `HELD` and `REVOKED`, `catalogue show`'s
  `HELD UNTIL` and `REVOKED`); `catalogue.arrival` carries
  `available_at` for a held entry; a rule's `newest` is the newest version without a held build;
  `publisher list` shows `HOLD`, `upstream show` `hold:`. The console's cells filter lists `held`;
  no screen changes (C).

### As built (B part 1)

- **Migration 0041** (`0041_distributors`, `min_reader` 41: an older replica would serve withdrawn
  entries and take a distributor's publisher's hold and keys as the tenant's) in the three dialects:
  `publishers.distributor` (NULL for the tenant's own), `builds.withdrawn_at` and
  `builds.withdrawn_reason` (no state column, as A2's held: an entry is withdrawn while
  `withdrawn_at` is set, which keeps serving's read a column of the build it joins already),
  `withdrawn_versions` and `removed_distributor_publishers` (below). Additive only: no SQLite
  rebuild; the existing rows are the tenant's own publishers and entries not withdrawn, so every
  channel serves what it served.
- **The mark**: a distribution admin's or a server administrator's (`404` to a tenant
  administrator); the distributor's name is 1 to 128 bytes of printable text, as a tenant
  reservation's holder. Found in review: marking any existing publisher would let a distribution
  admin take over a tenant's own publisher (its keys, its hold, its builds), so a server
  administrator, or a distribution admin, creates a distributor's publisher marked (`POST
  …/publishers {"name", "distributor"}`, `kista admin publisher add <t> <p> -distributor <d>`,
  event `publisher.add` with `distributor`), and an existing publisher is marked (`PUT
  …/publishers/{p}/distributor {"distributor": …}`, `publisher distributor <t> <p> <d>`, event
  `publisher.change` with `distributor` and `before`) only when a distribution admin created it
  (its `created_by`; one a tenant administrator or an operator created, a server token or the CLI,
  even a licence-key publisher with keys a server administrator added, is refused `404`, nothing
  said) and it is fresh: no grants, no keys unless it carries the licence-key mark already, no hold,
  and never a source of anything (no build, live, purged or withdrawn, no release, purged slot,
  withdrawn version, rule or follow naming it; `400`, the detail without counts), checked under the
  tenant's auth lock. Rules and follows change under a channel's lock, not that one: a rule or a
  follow naming a distribution admin's publisher written while it is marked is a window left open
  (it takes nothing the distributor did not publish, and the publisher is the distribution admin's
  own making). A distributor's publisher takes no grant
  afterwards, from anyone (`400` to a server administrator or a distribution admin, `404` to a tenant
  administrator, for whom it is marked: it reaches no channel by itself, and a `promote` grant would
  let it place); marking it again renames its distributor; the mark is never removed (the publisher
  is removed instead). It counts as spec 0021's mark for its keys and grants (a tenant
  administrator's change is `404`), so a distribution admin manages its keys, and may give it the
  licence-key mark while it holds keys. *Superseded in part by spec 0037 (PR 2): an existing
  publisher is never marked, by anyone (`PUT …/distributor` only renames a distributor's
  publisher's distributor, `404` on an unmarked one), and the freshness check goes; a grant to a
  distributor's publisher is `400` to a server administrator and `404` to a distribution admin,
  whom the grant routes no longer admit.*
- **The catalogue only.** The catalogue-only route admits a distributor's publisher's signed request
  without grants (a route flag, `distributors`); the service publishes any name kista does not
  reserve server-wide and the tenant does not reserve for another holder than its distributor (spec
  0037 (PR 2): server-wide only, tenant reservations are removed), and refuses the rest as a missing
  grant does (`404`, nothing said). The name is checked again in the
  build's landing transaction (`tx.ReservedKind`, `tx.TenantReservedHolder`), with the tenant still
  active (a suspension or a reservation during the upload refuses it); the reservation is read
  without a lock a reservation waits for, so one committed in between lands the build in the
  catalogue only: a follow re-checks the reservation under the channel lock before it writes a rule,
  and a placement by an `auto` rule needs the tenant's own rule, so nothing reaches a channel by it. A name an upstream of the
  tenant lists is allowed (spec 0030's lookup counts it as reserved, but the spec's e2e has a
  tenant's upstream and a distributor bring one name; each is its own source). Marked for licence
  keys, its publication records the build's keys without the grant a marked publisher otherwise
  needs (part 2 decides who owns a key for a name an upstream lists too, below). The channel's
  route refuses it (the route's grant check, `404`, and the service again). Its hold (`PUT
  …/publishers/{p}/hold`, the route now admitting distribution admins) is a distribution admin's or
  a server administrator's only: a tenant administrator is refused (`404`), a distribution admin is
  refused on any other publisher.
- **Purges**: `purge_held=true` on a key's removal is a distribution admin's for a distributor's
  publisher (still refused for a publisher with the licence-key mark alone); `DELETE
  …/catalogue/builds/{id}` admits a distribution admin for a distributor's publisher's build (removed
  or not; any other build is `404`), and now takes the tenant's upstream lock, which every release
  into its signed channels takes, so no placement releases the build between the check and the
  deletion. A tenant's administrators still purge any build placed nowhere, a distributor's included.
- **Withdrawal is the version's** (`POST …/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw
  {"reason": …}`, `kista admin catalogue withdraw <t> <source> <name> <version> -reason <text> [-type
  t]`): a distribution admin's or a server administrator's (a tenant administrator is `404`), for a
  distributor's publisher, removed or not; the version is a build's version (spec 0008's grammar,
  `400` otherwise) and the reason 1 to 400 bytes of printable text. Found in
  review: marking the builds alone let a new build of the version (another platform, DuckDB version
  or C API slot) arrive and be placed, so the version itself is recorded in `withdrawn_versions`,
  whether the source holds builds of it or not (purged, collected, or not arrived yet: the answer
  lists no builds; `404` only for a source that is not a distributor's publisher of the tenant), and
  stays when a build is collected meanwhile. In one transaction, holding the tenant's upstream lock
  and then its auth lock (a publication's build transaction takes the auth lock, so a build of the
  version lands either before, and is marked, or after, and is refused), the version is recorded,
  its entries not withdrawn yet are marked, and every channel of the tenant holding a release of
  any of them gets its release version bumped; the event `catalogue.withdrawn` (source, name,
  version, reason, builds, how many channels) is the tenant's. The answer names the tenant, the
  version and the entries withdrawn (the distributor's own), never a channel; a version withdrawn
  already answers `"already": true` and records nothing. `FindOrInsertBuild` refuses any build of a
  withdrawn version from its source (`409 …/withdrawn`), as the catalogue's route does before the
  body is stored.
- **A removed distributor's publisher is remembered**: removing one writes its id, name and
  distributor to `removed_distributor_publishers` (reaped with the tenant), so its versions can still
  be withdrawn, purged and read. Every distributor's publisher, live or removed, is a source a
  distribution admin addresses: `publisher:<name>` when one of them had the name, and
  `publisher-id:<id>` always (found in review: a later publisher of the same name hid an older
  removed one); an ambiguous `publisher:<name>` is `409`, naming the id form, and the distribution
  admins' catalogue lists such an entry as `publisher-id:<id>`, every entry with its `publisher_id`.
  A server administrator (a server token, the CLI) reads and names them so too (found in review: it
  could not follow the `409`'s advice), any other source as before; a tenant administrator's view is
  unchanged.
  Not in the Data model as drafted: the spec's "removed or not" needs it.
- **Serving reads the entry's state**: a channel's snapshot leaves out releases of withdrawn entries
  (`store.ChannelReleases`), so the DuckDB routes, the index, bundle exports and pull-through's
  selection never serve them; a channel feed's offer and its `ReleasedIn` leave their bodies out, and
  a licence key a withdrawn build declares is not live through it. Management listings keep the
  release with `"withdrawn": true` (`release list` shows `(withdrawn)`), and the catalogue shows the
  build's `state` `withdrawn` with `withdrawn_at` and `withdrawn_reason`, and each entry's
  `withdrawn` versions; a rule's `current` and `newest` skip withdrawn versions.
- **Never again**: every release insert (a placement by a rule, a follow, by hand even `early`, a
  promotion, a mirror's run) reads the entry with a shared row lock that the withdrawal's update
  waits for (`store.BuildWithdrawn`), under the channel lock and the tenant's upstream lock, and
  refuses a withdrawn one (`409`, problem type `…/withdrawn`), before it would answer a release
  already in the slot as made (found in review: a promotion of a withdrawn release into a channel
  holding it answered success); the placement pass skips withdrawn entries before it, and a withdrawn
  release holds its version for no one, so a `manual` rule completes nothing of it. A release of a withdrawn entry is never made active, current or public again
  (`409`; the conservative reading of "made active": current and public too; yank, deprecate and
  private stay allowed). A withdrawn entry is never purged (`409`), never collected (the collector's
  dead-build condition skips it), skipped by a purge of held entries and by the landed trim.
- **No automatic downgrade** (the owner's decision, 2026-10-24): a withdrawn release still counts as
  what its cells had and as what they serve, so after a withdrawal an `auto` rule or a follow never
  places an older version by itself, and a late build completing an older version the channel holds
  is not made current where the withdrawn version was (found in review); a newer version, or a
  placement by hand, is needed. A rule's `newest` (the Sources view)
  stays the newest available version in the catalogue, not withdrawn nor held, which the pass takes
  only when it is newer than what the channel had (as for a yanked version); the docs say so.
- **Per source**: a withdrawal is the distributor's source's; the same body from the tenant's own
  publisher or an administrator's add is the tenant's to serve, or to block for every source.
- **Caps** (found in review, reverting a first deviation): a withdrawn entry counts in its source's
  `catalogue.max_entries` and `catalogue.max_bytes`, released or not, since its body is kept: a
  distributor that publishes and withdraws does not grow a tenant's storage without bound. Since a
  withdrawn entry is never purged, a source full of them stays full: the `429`'s detail says
  withdrawn builds count and are never purged, and a new publisher is the way out.
- **Deviation: the body stays.** The Retention section's "a withdrawn entry keeps its record and drops
  its body reference" is left to a follow-up: the body is kept with the record (the releases of the
  entry still reference the build, and readers of a release's body other than serving would need
  care); the record is what keeps the version from arriving again.
- **Follows** (A1 part 4's seams): a follow of a distributor's publisher takes a name reserved in the
  tenant for its distributor (checked again under the channel lock when it writes the rule), and
  skips one reserved for another holder (spec 0037 (PR 2): superseded: tenant reservations are
  removed, and it skips a name an upstream of the tenant lists, checked again under the lock); the "name it may not publish in the channel" limit uses the
  distributor rules instead of grants (it has none): the names it may publish into the catalogue,
  which the server-wide and tenant-reservation limits already check.
- **A distribution admin's reads**: `GET …/catalogue` and `…/catalogue/{source}/extensions/{ext}`
  admit distribution admins and answer the distributors' publishers' entries only (distribution
  admins are server-wide, so every distributor's in the tenant), without rules and without where a
  build is placed; the catalogue-only route's answer to a distributor's publisher carries no
  `placed` either. What a distribution admin still learns about placements is inherent to its other
  answers: a build's `revoked_at` and the catalogue's `revoked` versions say a held build was placed
  early once, a purge refused (`412`) or a purge of held entries' `kept` list (with `channels`) says a
  build was released. The mark's refusals tell no counts.
- **Errors**: `store.ErrWithdrawn` is `409` with the problem type `withdrawn`; an ambiguous source
  name is `409` (`conflict`); a problem's detail no longer starts with `store: ` (the prefix is
  trimmed as the services' others are).
- **Left to part 2**: licences per (name, source), key owners, the relay and usage; the live keys of
  the catalogue's newest available, not withdrawn, version. A note for its design: a distributor's
  publisher carrying the licence-key mark records a build's keys for any name it may publish,
  including a name an upstream of the tenant lists; which source owns such a key for the name is
  part 2's to decide (the first recorder, or the upstream; decided in part 2, below).

### As built (B part 2)

**Superseded by spec 0035** where it concerns licences: the key owners (the first recorder, the
tombstones, distributors and upstreams, ownership changes and the owner route), the relay's and
bundles' exceptions (including the bundle target "the first bundle-mode upstream … that owns the
key"), the licences of source `unknown`, the `owner` field and migration 0042's owners. Kept below
as history.

- **Migration 0042** (`0042_license_owners`, `min_reader` 42: an older replica would serve a licence
  through any source's build and store and read licences by name) in the three dialects:
  `license_key_owners` (tenant, package type, name, kid → source, `set_by`, `set_at`; pk (tenant,
  type, name, kid), spec 0034's type added to the Data model's row, `set_at` as `channel_sources`
  has it), reaped with the tenant; `licenses` gains its source (`owner_source`: the column `source`
  already says how a licence arrived, spec 0012's `import` or `upstream`, so the Data model's
  `source` is named apart) and its primary key becomes (tenant, type, name, source): PostgreSQL and
  SQL Server drop `pk_licenses` by the name migration 0039 gave it, SQLite rebuilds the table under
  `-- +foreign_keys_off`; BIN2 for the new text columns on SQL Server. An index on
  `build_license_keys (tenant_id, kid)` makes the first recorder's read and the conflict's count
  selective (`ix_build_license_keys_kid`).
- **A key's owner** is the first source whose **own** entry of the name (the build's source is the
  recording source: a record on another source's build counts for nothing) recorded the key while
  the entry counted: from its recording, or from the end of its hold (or its first release, if it
  was placed early before), never for a revoked entry (a revoked key's bytes are not trusted again,
  A2) nor for a body blocked now (the tenant's distrust of the bytes, as a revocation; a lifted
  block, whose row is gone, counts again), nor once it was withdrawn before it counted; a removed
  source's records count for nothing, and ties go to the smaller source string, so every replica
  computes the same owner. **A hold is not a copier's chance**: while another source's record made
  no later than the first recorder's start is on an entry still held (counting later, not
  withdrawn), the key has **no owner** (computed "", nothing claimed, so a copier recording inside
  an upstream's hold gets no licence through the key); once the hold resolves the rule above
  decides. The conservative reading of the review's finding, recorded: by that rule a copier whose
  available record came before the held entry's hold ended is still the first to have counted when
  the hold ends, so the window only keeps the key ownerless during the hold (the tenant's
  administrators see no owner and can give the key, `license owner`); an earlier recorder winning
  over a held one by its recording time would contradict the migration's case of a held entry placed
  early after another source's record. The owner is **recorded** (claimed) by the first act that
  needs it, so it stays whatever happens to the records afterwards (a purge, the collector, a
  withdrawal): the record of a key on an entry that counts (`RecordLicenseKeys`, in the intake's
  transaction), a licence stored for it (import, relay, bundle; a bundle's dry run claims nothing),
  or an administrator's act. Until then it is computed when read (a held entry's record starts
  counting when the hold ends, without a write). A claim is an insert-if-absent (`ON CONFLICT DO
  NOTHING`, SQL Server's `MERGE … WITH (HOLDLOCK)`), so two intakes racing keep the one that
  committed first; each claim is a `license.key_owner` event (`first: true`, actor
  `system:licenses`). A source recording a key another source owns is a `license.key_conflict` event
  (source, owner), once per source and name.
- **Distributors and upstreams** (B part 1's question): a distributor's publisher's record does not
  count toward a key's ownership for a name a `repository` upstream of the tenant lists (by name, or
  `*`), live: the tenant's own upstream keeps the name's keys (the spec's Alternatives: a
  distributor's copy must not take a tenant's licences away from its own upstream), and a tenant
  administrator gives a key to the distributor's publisher by the act below when that is what they
  want. A key already recorded as the distributor's stays its own when an upstream listing the name
  is added later (first recorder). For any other name the distributor's publisher is a recorder as
  any marked publisher. DuckDB's core and community upstreams record no keys (spec 0021), so they
  are not counted.
- **Ownership changes** only when the owner goes or by a tenant administrator's act. Removing a
  publisher or an upstream drops its keys (spec 0021) and leaves the keys it owned, recorded or as
  the first recorder computed, **without an owner**: the owner row stays with source `''` (a removed
  owner's), and only records made after its `set_at` count toward the next owner, so a key never
  passes by itself to a source that recorded it while another owned it, or whose records did not
  count then (a distributor's publisher's for a name the removed upstream listed: `listed` is read
  live). The conservative choice over the spec's "the next recorder owns it": the key waits for a
  source recording it afresh (a build arriving after the removal; a claim replaces the removed
  owner's row: compare-and-set on its `set_at`, so a claim computed before another removal refreshed
  it records nothing), or for the act below. The removal writes the removed owner's row only where
  there is none (insert-if-absent) or over the removed owner's row it read (the same
  compare-and-set), never over an administrator's act racing it. `DeleteLicenseKeysOf` runs before
  the source's row and entries go (the first recorder reads them). The act (`PUT
  …/license-keys/{name}/{kid}/owner {"source": …}`, `kista admin license owner <t> <name> <kid>
  <source>`; tenant administrators and server administrators, not licence managers: the spec names a
  tenant administrator's act). The source is a live upstream or publisher of the tenant
  (`upstream:<name>`, `publisher:<name>`, or the id forms below) that recorded the key on its own
  build of the name (`400` otherwise, the conservative reading: a source that never declared the key
  would serve nothing with it). The act moves the licences of the name signed with the key from the
  owner before to the new one (where it holds none, a licence of source `unknown` signed with the
  key moves instead), and is refused (`409`) while the new owner holds a licence of the name already
  (nothing removed silently, as spec 0028's holder change); giving a key to its recorded owner
  changes nothing and writes no event. A relayed licence (source `upstream`) moved to an upstream
  gets that upstream as its relayer (its `upstream_id`), so its relay governs it; an imported one
  moved to an upstream stays `import` (conservative: no relay ever deletes an administrator's
  import). Event `license.key_owner` with `before` and the `moved` licence ids. Unmarking a
  publisher keeps its keys and its ownership (spec 0021's unmarking keeps the keys).
- **Live keys** of a source (spec 0021's rule per source): those it recorded on its own live builds
  (an active or deprecated release in a signed channel that serves the build's source for the name,
  not blocked, not withdrawn), or on its newest available version of the name in the catalogue (not
  held, withdrawn nor revoked, not blocked; spec 0006's version order, moved to the store as
  `store.CompareVersions`), so a licence is taken before any placement. `GET …/license-keys` lists
  those of every source, each key with its `owner` (omitted while it has none), the recorded owners
  read in one query.
- **Licences per (name, source).** A licence's source is its key's owner for its name: an import
  stores it for the owner (claimed), verified against the owner's live keys, and replaces the one of
  that (name, source) only with `If-Match`, as before; where the owner holds none, a licence of
  source `unknown` signed with the same key is the one it replaces (`If-Match` on that one: an
  existing client sending none gets `409` and reads the list, rather than two licences of the name
  and `409` on every sourceless route after); the relay applies a licence when its upstream owns the
  key, or **when it is byte for byte the licence the key's owner holds for the name** (relayed or
  imported; the owner's decision: a licence is the organisation's purchase of the product, and every
  channel serving a genuine build of it serves it, as for a tenant mirroring two channels of one
  remote into two of its own; the key's owner only stops a source carrying a copied key from getting
  a licence of its own), stored as (name, that upstream); any other licence signed with a key
  another source owns is refused `key_owner`; verified against the keys it brought, and replaces by
  (name, that upstream) with spec 0012's and 0021's rules (where the upstream holds none for the
  name, a licence of source `unknown` it relayed itself with the same key is replaced, keeping its
  visibility; one it relays again unchanged that another upstream relayed before becomes its own,
  `upstream_id`, an import staying an import; a bundle's apply does the same); a bundle's licence
  goes to the first bundle-mode upstream of its source that supplies the name and owns the key (else
  the first that supplies it, refused for the key unless it is the owner's very licence). A refusal
  is never a revocation: a row stored from the upstream stays and the licence is tried again at the
  next run (a renewal reaching the upstream before the owner's relay has it). Imports go to the
  key's owner only; an administrator giving the key to an upstream that holds such a copy is refused
  `409` as over any licence of it (remove the copy first). The `license.key_conflict` event stays as
  it is (recorded when a key is recorded, before any licence is relayed: not trivially suppressible
  for such an upstream). `not_supplied` now reads the catalogue: a build of the name the upstream
  brought since it was added (a successor's copies are not its own), not withdrawn, revoked nor
  blocked, available or live. The expected holder (spec 0028) is checked on every path as before.
- **Served by the channel's source**: `-/licenses/<name>` reads the licence of the source the
  channel's snapshot selects for the name, and the list keeps only those; another source's licence,
  even when the channel's builds declare its key (a copy), is not served there. A bundle's export
  carries, per name, the licence of the source its files come from (its mirrors', in order, or the
  source its own channel serves the name from).
- **Addressing a licence**: the API answers a licence's source as `owner` (`upstream:<name>`,
  `publisher:<name>`, a removed source's `upstream-id:<id>` or `publisher-id:<id>`, or `unknown`);
  `source` keeps its meaning, how the licence arrived (a deviation in naming, so existing clients
  read what they read). `GET`, `DELETE`, `…/public`, `…/private` on `…/licenses/{name}` take
  `?source=` (the owner as answered, or its id form): without it they act on the name's only licence
  and answer `409` naming the sources where there are several, so existing clients keep working for
  a name of one source; `GET …/licenses?source=` filters; an import's `Location` names the source.
  CLI: `-source` on `license list|show|token|remove|public|private`, `OWNER` in `license list` and
  `license keys`, `license owner`. The import takes no source: its key decides. Events
  `license.import`, `license.change`, `license.remove` gain `source`.
- **Licences of a removed source**: an upstream's relay governs its own rows only: the relayed
  licences of its source (source `upstream`, owner the upstream, whichever upstream relayed them:
  one an administrator moved to it) and those of source `unknown` it relayed itself (another
  upstream's copy of the same licence is that upstream's); its revocation and its removal
  (`license.remove` naming the `source` too) take only those. One an administrator moved with its
  key is the other source's, and an import is an administrator's. A removed publisher's licences
  stay (served nowhere: its keys are gone), as the conservative choice over deleting a licence,
  until removed or past their grace; importing it again stores it for the key's owner once it has
  one.
- **Usage** is forwarded by a name's selected source: an upstream receives the counts of the (name,
  version) pairs of channels whose rule for the name names it and that hold a release of the version
  from it, of a build it brought since it was added (instead of the provenance query).
- **Existing data** (the rule implemented, the spec's: "a key's owner for a name is the source that
  recorded it first; licences get their key's owner"): the migration claims, per (tenant, type,
  name, kid), the first recorder as kista computes it, read in SQL by the database's clock
  (PostgreSQL `now()`, SQL Server `SYSUTCDATETIME()`, SQLite `strftime` in the store's layout): the
  source's own build, its source existing, not revoked, its body not blocked, and not a
  distributor's publisher's for a name a repository upstream lists; a record counts from its
  recording, the end of its hold or its first release if sooner (ordered by that, then by source),
  and not when withdrawn before it; none is claimed for a key another source's earlier record is
  still held for (the first claim after the hold decides). Each licence gets its key's owner for its
  name; one whose key has none gets `unknown` (kept, listed, removable, served nowhere until
  imported again). A licence relayed by an upstream whose key another source recorded first is that
  source's (the spec's rule; giving it to the upstream that relayed it would make a licence's source
  depend on how it arrived, which the Alternatives rule out): the licence stays the owner's, and the
  upstream's next relay stores the very same licence as its own beside it (another one signed with
  the key is refused).
- **The console** (C's) only addresses a licence by its source (`?source=`), keys its rows by name
  and source, and shows the source (**Of**); the import reads the licence's key (the JWS header's
  `kid`) and replaces the licence of the source that owns it (the keys' `owner`), or one of source
  `unknown` signed with it; a licence is flagged not served unless its key is a live key of its own
  source.
- **Tests**: the store suite on three dialects (the migration on a populated level-41 database:
  first recorders, own builds only, a distributor's for a listed name, held and revoked records,
  relayed and imported licences, `unknown`; key owners: the claim and its event, a copied key's
  conflict told once, a held entry counted after its hold, the act and the licences moved, the
  owner's removal; live keys of the newest available version; the migration's held cases, a hold
  ended before another source's record and a held entry placed early after it, agreeing with kista,
  and a blocked body; a copier inside another source's hold getting no owner until the hold
  resolves; the owner's removal leaving the key without one until a fresh record, a distributor's
  earlier record on a listed name included); the licence service on one engine
  (`TestLicenseSources`: a mirror's relayed licence, a marked publisher's copy getting nothing and
  taking nothing, its own key's licence beside it, `?source=`, the act refused over a licence and
  then moving one, the relay refused `key_owner` for another licence of a key it does not own, then
  storing the owner's very licence as its own and revoking only that; a second upstream of the
  repository in another channel relaying the owner's licence, both channels serving it, a refusal
  keeping its row, each relay revoking its own row only; a licence relayed by another upstream given
  back to its owner, kept unchanged and revoked by the owner's relay; a licence of source `unknown`
  replaced by an import and moved by the act); the relay and bundle tests (the key's owner taking
  the bundle's licence); serving (another source's channel serves nothing, its builds carrying the
  key or not); the API (the `409`s of several sources and of a source holding a licence, `400` for a
  source that never declared the key, `?source=unknown` and the id form), the CLI and the events
  registry; usage forwarding by the selected source; the e2e (`TestLicenseFromDistributor`: a
  distributor's publisher's `kista_demo` in the catalogue, its licence imported before placement,
  `404` until prod's auto rule places it, then served and valid in DuckDB at the pin; a tenant
  upstream bringing `kista_demo` with the same key into nightly: the key stays the distributor's,
  nightly serves no licence and DuckDB reads `missing` there, prod keeps its own).

Enterest moves onto it after B (its tenants' administrators use kista's API and CLI until C): one
kista, its pipeline a distributor's publisher in every tenant, publishing each approved version into
every tenant's catalogue with a day's hold and withdrawing there itself; each tenant mirrors
DuckDB's repositories it wants. Its spec 0003's decisions change accordingly.

### As built (C)

- **No migration and no new route.** The console uses the routes A1 to B built. Three small server
  changes, found in review: placing by hand resolves `publisher-id:<id>` of a live distributor's
  publisher for a server administrator (the form its catalogue answers one in when another
  distributor's publisher, live or removed, had its name; a removed one, and the form from anyone
  else, place nothing, as before); a rule's `newest` skips revoked versions too, which the pass never
  places (`Newest` and the pass's candidates agree: not held, withdrawn nor revoked); `kista admin
  catalogue show` gains an `ID` column, the build's, which `catalogue purge` takes.
- **The Catalogue section** (`…/catalogue`, tenant administrators: the catalogue's reads are
  theirs; a server administrator's opened tenant too): every (source, name) with its versions
  newest first, each marked held, revoked or withdrawn, and the channels whose rules take it with
  the mode. The API answers the catalogue whole, so the name prefix and the source are filtered in
  the page, as the Sources tab already did. An entry's page (`…/catalogue/<source>/<name>`) reads
  the entry (`?source=&name=`) and its versions with their builds: state (held until when,
  withdrawn with its reason and when, revoked), visibility, where each is placed; a version's state
  is read from its builds (the versions read), so nothing is offered for a withdrawn version before
  the entry is read, and a failed read of the entry or the channels is shown. A removed source's
  entries (`upstream:(removed)`, `publisher:(removed)`, not read by name) are listed without a page,
  rows keyed by their position, since two removed sources of one name answer one label. Its
  actions:
  - **Place…** a version into a signed channel of the default package type (the console shows that
    type's catalogue only; other types through the API and the CLI), offered where the caller holds
    `promote` on the name: found in the e2e, `admin` does not imply `promote` (spec 0008), so the
    console's permissions gained `mayPromote`, which hides the action from a tenant administrator
    without it (a grant on a group counts, its members unknown to the console: the API decides).
    The dialog takes private, not current, and early for a version with a held or revoked build,
    enabled for a channel administrator only. Not offered for a withdrawn version.
  - **Purge…** a build placed nowhere and not withdrawn (a tenant administrator's; the API also
    refuses one released once and purged since, and the page shows its answer).
  - **Withdraw…** with a reason (at most 400 bytes of UTF-8, counted as the server does), shown to a
    server administrator only, for an entry the catalogue
    answers with a `publisher_id` (a distributor's publisher: the server actor reads it so,
    phase B); a tenant administrator is never offered it, and distribution admins have no console.
- **The Sources tab** keeps its table (the source stays the second column) and gains the mode (in a
  signed channel; a rule without one reads "as it lands"), the version the channel holds and the
  newest available one when it is newer (spec 0006's order, the store's `CompareVersions` in the
  console: a held version placed early may be newer than the newest available), and the last outcome
  with its time. **Switch…** became **Change…**: the source and the mode (keep, `auto`, `manual`;
  `auto` disabled for `tenant`, and reset when the source becomes `tenant`), one `PUT`; the "keep"
  choice says what the server leaves (none for a new rule; `manual` for an `auto` rule moved to the
  tenant and for a follow's rule moved to another source), and the dialog never starts from a
  removed source's label. **Place…** places a version of the row's source by hand (the newest suggested), where the
  caller holds `promote`; early is always offered there (the tab does not read holds), the API
  answering `409 …/held` without it. Above the table, the channel's **hold** (`GET|PUT …/hold`;
  days typed as the CLI takes them, `2d`, are sent as hours, and the Go duration answered is shown
  shortened, `48h`); below it, **Follows**: each followed source, since when and by whom, the names
  it skips with the reason (as of the pass's last look), **Unfollow…**, and a field to follow a
  source (the sources the channel's rules and releases name, offered). A passthrough channel shows
  neither, nor the mode. Every source in a path keeps its colon (`sourceSeg`), and a version its `+`
  (`versionSeg`): the API refuses an escaped path. A rule's **Change…** without a mode keeps the
  rule's, and a new rule has none (the API's and the service's comments said manual; corrected).
- **Smaller**: a withdrawn release is badged in a channel's release lists; the Publishers list shows
  each publisher's hold and a distributor's publisher's distributor, and an upstream's page its home
  channel and a mirror's hold (read only: set with the CLI or the API).
- **Deviation: the cells keep `other_source` and `shadowed`.** The Existing data section's "the cells
  API loses `other_source` and `shadowed`" did not happen: as built in A1, a mirror still releases
  into its home channel, where its runs decide them, so the upstream page's cells filter keeps them;
  the upgrade notes say so.
- **The DuckDB end to end** (`e2e/catalogue_test.go`, `TestCatalogueSources`, about 10 seconds):
  another kista's `loadable_extension_demo` stands for `geo_tools` (the pin builds no `geo_tools`,
  and kista's file checks need the name's entry point), mirrored by a `repository` upstream with
  `prod` its home channel; a distributor's publisher with a hold of one second (the server's unit)
  publishes a copy of it with another version in its footer (`withVersion`) and `kista_demo` into
  the catalogue. `nightly`'s `auto` rule places the distributor's build only once the hold has
  passed, and DuckDB at the pin installs the upstream's body from `prod` and the distributor's from
  `nightly` (`extension_version`, the upstream's the built file's own footer version); `prod`'s follow of the distributor places `kista_demo` and lists
  `geo_tools` as skipped ("the channel serves it from another source"); the withdrawal makes
  `nightly`'s next install fail while `prod` still installs the upstream's; a version held for an hour is refused without `early` and installed
  from `nightly` once placed early. The licence part is B part 2's `TestLicenseFromDistributor`,
  unchanged. `publishTo` gained `publishFile` for a file of the test's own.
- **The console's end to end** (`web/console/e2e/catalogue.spec.ts`): `run.sh` adds a channel
  `nightly` following a distributor's publisher with a day's hold, which publishes `geo_tools` 0.9
  and 1.0 and a `hello_acme` of its own into the catalogue (signed requests, a `publish` function
  shared with the existing publication), and the tenant's `maps` 1.0 and 1.1 (`catalogue add`); kista
  is started again after the publications, since the placement pass looks at every follow at its
  start (the skip) and then every minute (`serve.drain_timeout: 1ms` there, so the restart does not
  wait the default 5 seconds). A server administrator withdraws 0.9 with its reason,
  places the held 1.0 early into `nightly`, and purges `maps` 1.1; a tenant administrator without
  `promote` is offered neither placing nor withdrawing (checked once the entry and the channels are
  read), changes `nightly`'s rule for `geo_tools` from
  the follow's to `manual`, sees the follow skip `hello_acme` ("the tenant uses it from another
  source"), unfollows and follows again, and sets the channel's hold to `2d` and back to none. The
  0031-era test of the Sources tab uses **Change…**. The fixture takes a name without a licence key.
  The API test of distributors places the live one of two distributors' publishers of one name by
  `publisher-id:<id>` (a server administrator), refused for the removed one and to a tenant
  administrator; the revoked entry's test checks the rule's `newest` skips it.

## Alternatives considered

- **One catalogue per kista, with the server's sources shared by every tenant** (an earlier draft of
  this spec): DuckDB's repositories mirrored and a distributor's builds published once, but it
  needed sources without a tenant (an internal owner), placement across tenants and storage
  domains, and withdrawal across tenants. The owner chose a catalogue per tenant (2026-10-24):
  bodies are deduplicated within a storage domain anyway, so the cost is a distributor's upload per
  tenant and each tenant's own mirror fetches.
- **Catalogue entries keyed by name**: the collision moves up a level; a tenant could not have
  DuckDB's `h3` in one channel and a distributor's in another.
- **A second kista for a distributor's catalogue** (Enterest's decision 27): no kista changes, at
  the cost of double storage and downloads, withdrawals carried by hand, licences waiting for the
  mirror, and the per-channel contests.
- **Copying a follow into per-name rules**: it could not be removed or changed afterwards.
- **The source as a licence claim**: the source differs at each kista of a relay chain; the key's
  owner identifies it in each tenant.
- **Licences per issuer**: one issuer (Enterest) signs licences for many developers' extensions,
  so it does not separate two extensions of one name.
- **A key's owner by precedence** (a distributor's first): a distributor's build carrying a copy of
  a key could take a tenant's licences away from its own upstream; the first recorder is stable.
- **Holding a channel-route publication until its hold passes** (a recorded target): the grants,
  key and rule would have to be checked again later; a held publisher publishes into the catalogue
  instead.
- **Holds per channel only**: the purpose is to keep a compromised source from reaching a channel
  automatically; that is the source's property. A channel may add its own.
- **Withdrawal for a tenant's own sources**: blocks and yanks already do it there.

## Changes to earlier specs

- **0001**: the concepts gain Catalogue entry, Rule and Follow; an Upstream feeds the catalogue; a
  slot is per source.
- **0006**: a release of a withdrawn entry is never served nor re-activated.
- **0007**: a snapshot leaves out releases of withdrawn entries; the sources answers gain modes,
  versions and outcomes.
- **0008**: publications land in the catalogue; the channel route places in the same request and
  refuses a held publisher; a catalogue-only route; promotions respect holds; slots per source.
- **0009**: an upstream feeds the catalogue and may have a home channel; two upstreams may list one
  name (as built: of two channels; within one channel a name stays on one upstream's allowlist);
  `other_source` and `shadowed` become placement outcomes (as built: they stay cell outcomes);
  passthrough channels take core names from eager `duckdb-core` upstreams only.
- **0012, 0021**: licences per catalogue entry; live keys per entry, from the catalogue's newest
  available version; key owners removed by spec 0035; usage forwarded by the selected source.
- **0014**: a bundle imports into its upstream's source in the catalogue.
- **0015**: the console's catalogue, rules, follows and holds (phase C).
- **0016**: catalogue retention and caps, purging unplaced entries.
- **0023**: revoking a key may purge its held entries.
- **0030**: distributors' publishers (a mark of their own) publish into the catalogue without grants;
  distribution admins mark them, set their holds, withdraw and purge their versions; a
  distributor's tenant reservations are no longer needed (kept, unchanged; a follow honours them;
  spec 0037 (PR 2) removes them).
- **0031**: selections gain a mode; follows; contests happen at placement; the same-body rule goes
  (slots are per source).
- **0032**: asks come from channels whose rule or follow names the upstream; the fetch places into
  the asking channel, or an available entry is placed without a fetch (as built: not yet, in the
  follow-ups; asks stay the home channel's).

## Follow-ups

- Upstreams without a home channel (A1 part 3b): a mirror that only fills the catalogue, its
  matrix's DuckDB versions its own, its lazy asks from any channel whose rule or follow names it
  (`upstream_asks` gaining the asking channel, the fetch placing into it, an available entry placed
  without a fetch); `PUT …/upstreams/{u}/home` and `kista admin upstream home`.
- A miss under a `manual` rule asking nothing (as built it lands the entry and misses).
- A withdrawn entry dropping its body reference (B part 1 keeps the body with the record).
- The catalogue's reads saying whether an entry declares licence keys.
- Merging two upstreams of one repository.
- Version constraints in a rule (`auto` within `1.x`).
- Notifying a channel's administrators of a new version under a `manual` rule.
- Publishing one build into many tenants' catalogues in one request, for a distributor.
- Catalogue reads for holders of `promote`, scoped to their names.
