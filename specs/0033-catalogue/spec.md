# Spec 0033: The catalogue

- **Status**: accepted (2026-10-24); phase A1 in progress (parts 1, builds and slots per source,
  and 2, rules' modes and placement, implemented); A2, B and C to come
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
tenant. A licence is bound to the extension's name, its source (the source that owns the key the
licence is signed with) and the organisation it is issued to. Everything else stays as it is: a
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
| **Distributor's publisher** | A publisher a distribution admin or a server administrator marks as a distributor's (`distributor`: the distributor's name, as spec 0030's tenant reservations name their holder): it publishes into the catalogue only, its hold and withdrawals are the distributor's. Not the same mark as spec 0021's licence-key mark, which a distributor's publisher also carries. |
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
  versions at placement. An upstream without a home channel only fills the catalogue.
- **Visibility.** An entry's visibility is what its arrival gives it today: a mirror's public, or
  private when its credential or bundle source says so (specs 0009, 0014); a publication's as the
  request asks (`visibility`, private by default, spec 0008). It is the widest a placement may serve
  it with.
- **A hold** is a duration on a publisher or an eager mirror, `0` by default: an entry arriving from
  it is `held` until `arrived_at + hold`. Lazy and pull-through upstreams take no hold (they fetch
  because someone is installing now). Raising a hold applies to entries already held; lowering it
  applies only to entries arriving from then on. A tenant's administrators set the holds of its
  sources; a distributor's publisher's hold is set by a distribution admin or a server
  administrator only, and a tenant that wants more waits with a channel hold (below). Revoking a
  publisher key (`DELETE …/keys/{k}?purge_held=true`, or removing the publisher) purges the entries
  it published that are still held, with an event: a hold is what lets a stolen key be revoked
  before its builds reach a channel. A routine rotation, without `purge_held`, keeps them.
- **Names a source may bring.** An upstream brings what its allowlist names (spec 0009, with
  `allow_reserved` and the alias rules unchanged). A publication needs `publish` on the name as
  today (specs 0008, 0017, 0030: a reserved name only by a grant naming it). A distributor's
  publisher needs no grant: it publishes into the catalogue only, any name kista does not reserve
  server-wide and that is not reserved in the tenant for another holder (spec 0030); nothing it
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

  The cell outcomes `other_source` and `shadowed` leave the cells (no channel there) and become
  placement outcomes.
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
    distributor;
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
  keys; the next recorder owns it) or by a tenant administrator's act (`PUT …/license-keys/{name}/
  {kid}/owner {"source": …}`). A licence's source is its key's owner for its name.

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
  {"channel": …}`; `PUT …/license-keys/{name}/{kid}/owner`; for a distributor's publisher,
  `PUT …/publishers/{p}/distributor {"distributor": …}` and `POST …/catalogue/{source}/extensions/
  {ext}/versions/{v}/withdraw {"reason": …}` (distribution admins and server administrators).
  Spec 0009's and 0023's routes are otherwise unchanged. CLI: `kista admin catalogue list|show|
  purge|withdraw …`, `kista admin upstream hold|home …`, `kista admin publisher hold|distributor
  …`.
- **Channel**: `…/channels/{c}/sources[/{name}]` gains `mode`, the current and newest available
  versions and the last outcome; `GET|PUT|DELETE …/channels/{c}/follows/{source}` (the skipped
  names on `GET`); `PUT …/channels/{c}/hold`; `…/releases/place`. CLI: `kista admin channel source
  <t>/<c> <name> -set <source> [-mode auto|manual]`, `kista admin channel follow <t>/<c> <source>
  [-remove]`, `kista admin channel hold <t>/<c> <duration>`.
- **Distribution admins** (spec 0030) gain, for distributors' publishers: marking them, setting
  their hold, withdrawing and purging their versions, reading their entries.
- **Events**: `catalogue.arrival`, `catalogue.hold`, `catalogue.withdrawn`, `catalogue.purge`,
  `rule.set`, `rule.clear`, `follow.set`, `follow.clear`, `release.place` (with `early`),
  `placement.failed`, `license.key_conflict`, `license.key_owner`.
- **Existence**: answers about the catalogue follow spec 0007's rules; install and anonymous callers
  get `404` as for any management route.

### Data model

One migration per phase, each with its `min_reader` (an older replica would place nothing, read
slots by the old key, serve withdrawn entries or the wrong licence). In the three dialects:

```text
A1  builds               + source (as releases.source), state (held, available, withdrawn),
                           available_at, visibility, publisher_key_id?, legacy_duplicate;
                           unique (tenant_id, source, name, body_hash);
                           unique slot (tenant_id, source, name, ext_version, platform, slot)
                           where not legacy_duplicate (below)
    releases             slot unique (channel_id, source, name, ext_version, platform, slot)
    purged_slots         + source ('*' for slots purged before: taken for every source)
    upstreams            + home_channel_id (from channel_id, which goes)
    channel_sources      + mode (auto, manual, follow), last_outcome
    channel_follows      channel_id, source, set_by, set_at; pk (channel_id, source)
    follow_skips         channel_id, source, name, reason; pk (channel_id, source, name)
    upstream_asks        + channel_id
A2  upstreams, publishers, channels   + hold_seconds
B   publishers           + distributor
    builds               + withdrawn_reason
    license_key_owners   tenant_id, name, kid, source, set_by; pk (tenant_id, name, kid)
    licenses             pk (tenant_id, name, source)
```

GC (spec 0016): a build (entry) is dead when it has no release and retention (above) does not keep
it; bodies as today. Tenant deletion (spec 0019) reaps the new tables with the tenant.

### Existing data

The migrations keep what every channel serves, and what arrives next goes where it goes today:

- every upstream's channel becomes its home channel; that channel gets an `auto` rule for each name
  of its allowlist (an existing selection naming the upstream becomes `auto`), and a follow when the
  allowlist has `*` (pull-through, so it works from A1 on);
- every other existing selection (a publisher, `tenant`, an upstream selected in a channel that is
  not its home, by promotion) becomes `manual`: a CI publishing into `dev` does not start reaching
  `prod`, and `nightly`'s mirror does not start flowing into `prod`;
- every build gets the source its releases name; a build whose releases name two sources (spec
  0031's same-body rule) is split into one per source, and every reference to it (releases,
  licence-key records, cells) repointed; all are `available`;
- slots get their source from the release; two bodies of one version under one source in two
  channels (possible today) are both kept, all but the oldest marked `legacy_duplicate` (outside
  the unique slot), and listed in the upgrade notes; a new arrival conflicting with any is refused;
  a slot purged before the migration has no release to read its source from, and stays taken for
  every source;
- (B) a key's owner for a name is the source that recorded it first; licences get their key's
  owner; a licence whose key no record has (rotated out or refused, so not served today either)
  gets the source `unknown`, is kept for its administrators and is not served until re-imported;
- tenants with two upstreams of the same repository (one per channel) keep two sources.

The cells API loses `other_source` and `shadowed` (now placement outcomes, on rules), and the
console's Sources tab shows modes: both are in the upgrade notes.

## Security

- **Tenant isolation is unchanged**: the catalogue is the tenant's, read and placed by it; a
  distributor reaches it only through its publisher and the distribution admins (spec 0030), sees
  its own entries only, and reaches no channel by itself.
- **The hold**: automatic placement, manual placement and promotion refuse a held entry, except a
  channel administrator's audited `early` (the tenant's own choice); the channel route refuses a
  publisher with a hold; lowering a hold does not release entries already held; revoking a key
  purges its held entries; a distributor's hold is not the tenant's to set. A tenant publisher
  without a hold that holds `publish` on a channel reaches that channel at once, as today.
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
  key's owner for a name is the first source to record it and changes only by the owner's removal
  or an administrator's act, so another source's build carrying a copied key gets no licence and
  takes none away.
- **Existence.** Install and anonymous callers learn nothing new: the catalogue is not served, and
  the DuckDB routes and the index are unchanged.

## Testing

- The store suite on three dialects: entries per source (two sources, one name, one body, one
  version); slots per source; rules, modes, follows, their limits and skips (contested,
  tenant-reserved for another holder); holds (raised, lowered, the channel's, a distributor's);
  withdrawal; licences per (name, source), key owners (a copied key changes nothing; the owner's
  removal; an administrator's act); each migration's backfill from a spec 0031 database (home
  channels, `auto` and `manual`, pull-through follows, splits, duplicate and purged slots, `unknown`
  licences).
- The placement pass: idempotent, leased, queued, newest-only ordering (a held older version does
  not overtake), yanked and purged versions not re-placed, failures recorded, suspended tenants
  skipped; a placement racing a withdrawal; a revoked key purging held entries.
- Intake: every path lands an entry; the channel route places in the same request and refuses a
  held publisher; the catalogue-only route places nothing; an upstream with a home channel serves
  new entries; lazy and pull-through through a rule or a follow fetch and place within one
  request's retry window.
- Licences: imported before any placement; one served per channel by its source.
- The existing DuckDB end-to-end suite passes unchanged after each phase.
- New e2e with the pinned DuckDB: a tenant's `repository` upstream (another kista) and a
  distributor's publisher both bring `geo_tools`; `prod` serves the upstream's by its home rule,
  `nightly` has an `auto` rule for the distributor's and gets its builds after the hold (`INSTALL
  geo_tools FROM …`, the right body each); a follow of the distributor takes its other names and
  lists `geo_tools` as skipped; a withdrawal stops `nightly`'s install at once; a held entry placed
  `early`; a licensed extension's licence imported before placement, served once placed, and
  verified by the SDK.

## Delivery

One spec, four phases, each a PR with its own reviews and its own migration:

- **A1. The catalogue**: entries per source, upstreams with home channels, slots per source, rules
  with `auto` and `manual`, follows (with their limits and skips), the placement pass and manual
  placement, the channel route through the catalogue, the catalogue-only route for tenant
  publishers, the read routes, retention and caps, the migration.
- **A2. Holds**: source and channel holds, `early`, the channel route refusing held publishers,
  revoked keys purging held entries.
- **B. Distributors and licences**: distributors' publishers, distribution admins' holds,
  withdrawals and purges, licences per (name, source) with key owners, relay and usage.
- **C. The console and the end to end**: a Catalogue screen (sources, names, versions, held and
  withdrawn, place, purge), the channel's Sources tab with modes, follows, skips and the hold, the
  new DuckDB end to end, the website's pages.

A1 comes in parts, each a PR: part 1, builds and slots per source (migration 0034); part 2, the
rules' modes and the placement (the pass, `auto` and a `manual` rule's completed versions, manual
placement, the catalogue's read routes, a mirror under a `manual` rule landing its builds without
placing them); part 3, upstreams without a home channel, the catalogue-only route, retention and
caps, the catalogue's own slot uniqueness; part 4, follows.

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
- **Left to later parts**: the entries' state, `available_at`, visibility and publisher key, the
  catalogue's own slot uniqueness and `legacy_duplicate` (part 2); a source may still release
  another body of a version in another channel until then. The data model's "one migration per
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

Enterest moves onto it after B (its tenants' administrators use kista's API and CLI until C): one
kista, its pipeline a distributor's publisher in every tenant, publishing each approved version into
every tenant's catalogue with a day's hold and withdrawing there itself; each tenant mirrors
DuckDB's repositories it wants. Its spec 0003's decisions change accordingly.

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
  name; `other_source` and `shadowed` become placement outcomes; passthrough channels take core
  names from eager `duckdb-core` upstreams only.
- **0012, 0021**: licences per (tenant, name, source); key owners per (name, key); live keys from
  the catalogue's newest available version; usage forwarded by the selected source.
- **0014**: a bundle imports into its upstream's source in the catalogue.
- **0015**: the console's catalogue, rules, follows and holds (phase C).
- **0016**: catalogue retention and caps, purging unplaced entries.
- **0023**: revoking a key may purge its held entries.
- **0030**: distributors' publishers (a mark of their own) publish into the catalogue without grants;
  distribution admins mark them, set their holds, withdraw and purge their versions; a
  distributor's tenant reservations are no longer needed (kept, unchanged; a follow honours them).
- **0031**: selections gain a mode; follows; contests happen at placement; the same-body rule goes
  (slots are per source).
- **0032**: asks come from channels whose rule or follow names the upstream; the fetch places into
  the asking channel, or an available entry is placed without a fetch.

## Follow-ups

- Merging two upstreams of one repository.
- Version constraints in a rule (`auto` within `1.x`).
- Notifying a channel's administrators of a new version under a `manual` rule.
- Publishing one build into many tenants' catalogues in one request, for a distributor.
- Catalogue reads for holders of `promote`, scoped to their names.
