# Spec 0033: The catalogue

- **Status**: draft
- **Date**: 2026-10-24
- **Author**: vgsml, Claude

## Summary

Everything that arrives in kista, a publication, a mirrored build or a bundle's, lands first in a
**catalogue**, as a build of a **source** under a name. Names may coincide between sources: DuckDB's
community `h3`, a distributor's `h3` and a tenant's own `h3` are three entries, side by side. A
channel **takes** builds from the catalogue by its administrator's rules: a name from a chosen
source automatically (each new version, once any hold has passed) or by hand (a version the
administrator places), and a whole source automatically. Sources belong to the **server**, seen by
every tenant (DuckDB's repositories mirrored once, a distributor such as Enterest publishing once),
or to **one tenant** (its publishers, its own upstreams), seen by it alone. A source may set a
**hold**: its new versions reach no channel before it has passed. A server source's version can be
**withdrawn** once, for every tenant. A licence is bound to the extension's name, its source (the
source that owns the key the licence is signed with) and the organisation it is issued to.

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
- **A distributor serving many tenants has no place to publish once.** Enterest built its catalogue
  as a second kista (its spec 0003, decisions 26 and 27): an `incoming` channel as the landing
  place, a promotion into `stable` after a holding period, and every tenant's channels mirroring
  `stable` over HTTPS with one credential. Every body is stored and downloaded twice, a withdrawal
  is carried into each tenant by blocking body hashes there, a licence waits until the build has
  been mirrored into the tenant, and the per-channel name contests apply in every channel.
- **A version pending a tenant's choice has no home.** What a mirror or a distributor brings
  either lands in a channel, where it is served, or nowhere.

The owner's design (2026-10-24): a catalogue holds what arrives, keyed by where it came from, so
names may coincide; one catalogue per kista, its entries the server's (seen by all) or a tenant's
(seen by that tenant); channels take from it, automatically by the administrator's setting or by
hand, also a whole source; a hold on the source, and optionally on the channel; a licence bound to
the name, the source and the organisation.

## Design

### Concepts

| Concept | Meaning |
| --- | --- |
| **Source** | Where builds come from: an **upstream** (a mirror of DuckDB's core or community repository, another repository, a bundle's), a **publisher**, or **`tenant`** (its administrators' adds and its principals' publications). Owned by the server or by one tenant. Named `upstream:<name>`, `publisher:<name>`, `tenant`, and for the server's `server:upstream:<name>`, `server:publisher:<name>`, in answers, events and the CLI. |
| **Catalogue** | Every build of every source. A tenant sees the server's sources and its own; nobody sees another tenant's. Not served to DuckDB. |
| **Catalogue build** | A build of a source: (source, name, version, platform, DuckDB version or C API major), verified at arrival against its source's signature, with its licence keys (spec 0021), its visibility, and its state: `held` (until a time), `available`, or `withdrawn`. Key: (source, name, body hash); a version is immutable within its source. |
| **Rule** | A channel's decision for a name: the source it serves it from (spec 0031's selection) and a mode, `auto` or `manual`. |
| **Follow** | A channel taking a whole source automatically: every name of it the channel has no rule for, within limits (below). |
| **Release** | A catalogue build placed in a channel, signed with the channel's key (the original signature in a passthrough channel), with its state, visibility and slot. A slot is now per source: (channel, source, name, version, platform, DuckDB version or C API major). |

A channel is still one DuckDB repository and still serves each name from one source (spec 0031).
What changes is where builds wait, how they reach a channel, and that a slot belongs to a source.

### What an administrator does

- **Mirror DuckDB's `h3` into `prod`**: add a community upstream with the entry `h3` (spec 0009,
  the CLI and routes as today) with `prod` as its home channel: the entry writes `prod`'s `auto`
  rule for `h3`. Or, where the server mirrors DuckDB's community repository, write the rule only:
  `kista admin channel source acme/prod h3 -set server:upstream:community -mode auto`.
- **Take a distributor's builds**: `kista admin channel follow acme/prod server:publisher:enterest`
  takes every name of it that `acme` does not use already, each new version once its hold has
  passed; or rule names one by one.
- **Choose versions by hand**: a `manual` rule; `GET …/channels/{c}/sources` shows per name the
  source, the mode, the current placed version and the newest available one; `…/releases/place`
  places a version.
- **Roll back**: yank the bad release (spec 0006: the previous one is current again); with an
  `auto` rule the yanked version is not placed again, and the next version is. To stay on a version,
  switch the rule to `manual`.
- **Switch source**: `PUT …/sources/{name}` with another source: its releases already in the channel
  are served at once, and with `auto` its newest available version is placed by the next pass.

### Sources

- **A tenant's sources** are what it has today: its upstreams, its publishers, and `tenant`. An
  upstream no longer feeds a channel: it brings builds into the catalogue. The channel an upstream
  was made with (or is given) is its **home channel**: adding an entry writes that channel's `auto`
  rule for the name, unless the channel already has a rule for it, so today's scripts keep
  working. An upstream without a home channel only fills the catalogue.
- **The server's sources** are a server administrator's: upstreams (DuckDB's core and community
  repositories, another repository with a server credential, bundles) and **server publishers**, a
  distributor's CI identity (Enterest's pipeline), with keys as a tenant publisher has (spec 0023).
  They behave as a tenant's sources do (runs, cells, asks, keys, licence-key records), are managed
  through `/api/v1/catalogue/…` below, and are seen by every tenant.
- **An upstream names its DuckDB versions** (its matrix): by default its home channel's versions
  (today's matrix) or, without one, every version of the tenant's channels; for a server upstream,
  every DuckDB release version registered on the server. A publication's DuckDB version must be
  registered on the server (and, on the channel route, served by the channel, as today). Channels
  filter by their own versions at placement.
- **Visibility.** A build's visibility in the catalogue is what its arrival gives it today: a
  mirror's public, or private when its credential or bundle source says so (spec 0009, 0014); a
  publication's as the request asks (`visibility`, private by default, spec 0008). It is the
  widest a placement may serve it with. For a server source it only decides public or private
  serving in tenants, not who sees it in the catalogue (every tenant does).
- **A hold** is a duration on a publisher (tenant's or server's) or an eager mirror, `0` by
  default: a build arriving from it is `held` until `arrived_at + hold`. Lazy and pull-through
  upstreams take no hold (they fetch because someone is installing now). Raising a hold applies to
  builds already held; lowering it applies only to builds arriving from then on, so builds already
  held keep the old time. A server source's hold is set by a server administrator only. Every
  catalogue build records the publisher key it was published with; removing that key, or the
  publisher, purges the key's builds still held (an event says so: for a server publisher, in every
  tenant that follows it), which is what makes a hold useful against a stolen key.
- **Names a source may bring.** An upstream brings what its allowlist names (spec 0009, with
  `allow_reserved` and the alias rules unchanged). A tenant's publisher and `tenant` publish what
  their grants reach (specs 0008, 0017, 0030: a reserved name only by a grant naming it). A server
  publisher publishes any name kista does not reserve server-wide (DuckDB's core and community
  names and their aliases, spec 0030), unless a server administrator (only) allows the name for it.
- **Removing a source** keeps its catalogue builds and their releases (shown as a removed source's);
  rules naming it keep serving what is placed. Spec 0031's successor stays, narrowed: an upstream
  of the same kind and prefix, added again, takes the rules naming the removed one in its home
  channel; elsewhere the rule stays and shows the removed source until an administrator switches
  it.

### The catalogue

- **Arrival.** Every intake path lands a catalogue build: a publication, a mirror's run, a
  pull-through or lazy fetch (spec 0032), a bundle import (spec 0014), an administrator's add. The
  checks are today's: the file (`CheckFile`), the footer against the request or the cell, the
  source's signature, blocks (the tenant's, and withdrawals below), the source's slot (the same
  version with another body is refused), the licence key block (spec 0021, from trusted sources: a
  server publisher counts as a marked one). No channel is involved at arrival, so there is no
  source contest there; the contests of spec 0031 move to placement.
- **Publishing** stays on today's route, `POST …/channels/{c}/extensions/{ext}/releases`, with its
  grants (`publish` on the channel and name) and answers: it lands the catalogue build and places it
  into that channel, at once when the build is available, or when its hold passes (the answer then
  says `held` and until when; the request is recorded as a **target**: build, channel, actor, key).
  Every check a placement makes, the source contest (`409`) included, runs at the request, and a
  refusal leaves no catalogue build, as today stores nothing; when the hold passes they run again,
  with the key still registered and the actor's grant still held, or the target is dropped (an
  event says why). One build may have several targets (`dev`, then `prod`).
  A server publisher publishes with `POST /api/v1/catalogue/publishers/{p}/extensions/{ext}/builds
  ?version=&platform=[&visibility=]` (spec 0023 signing), into the catalogue only.
- **Mirrors** run their matrix (entries × platforms × the source's DuckDB versions) into the
  catalogue. Lazy and pull-through (specs 0009, 0032) keep their behaviour, for a channel whose
  `auto` rule or follow for the name names the upstream and that has no channel hold:
  - a miss in such a channel records an ask (`upstream_asks` gains the asking channel); when the
    cell's build is already available in the catalogue (another channel or tenant asked first),
    it is placed into the asking channel at once, without a fetch; otherwise the fetch places into
    the asking channel before its lease ends, so the retried request finds the release (spec 0032);
  - a miss in a channel whose rule is `manual` asks nothing and places nothing (the administrator
    pinned the name), as a miss under no rule does;
  - the pass places the fetched build wherever else rules take it.

  A server mirror's catalogue view lists its entries and matrix, never which cells were fetched
  (that would tell one tenant what another installs); a tenant sees a server mirror's builds of the
  cells its own channels hold or asked for. The cell outcomes `other_source` and `shadowed` leave
  the cells (no channel there) and become placement outcomes.
- **Withdrawal** (server sources only; a tenant has blocks and yanks for its own). A server
  administrator, or a distribution admin for a server publisher bound to it, withdraws a version of
  a name from a server source, with a reason. In one transaction its catalogue builds become
  `withdrawn` and every channel holding a release of them (through the tenant's record of the
  server build) gets its release version bumped; the snapshot reads the server build's state, so
  such a release is served nowhere from then on, and is shown as withdrawn. No sweep is needed and
  nothing can be missed: a release of a withdrawn build cannot be made active, and the body cannot
  arrive again in that source. A tenant's own sources' builds of the same body (its own community
  mirror, say) are its own: its administrators are told by an event (`catalogue.withdrawn`), and
  block the body if they agree.
- **Retention.** A catalogue build is kept while it has a release anywhere; a mirror's while it is
  its cell's newest; a publisher's or `tenant`'s until its source's administrator purges it
  (`DELETE …/builds/{id}` for one placed nowhere; never placed, its slot is freed, since no one
  could have installed it). A withdrawn build keeps its record without its body. Otherwise a build
  is dead after spec 0016's grace, as today. Per-source limits bound growth: builds, names and
  bytes per source (configurable; defaults in implementation), refused with `429` when exceeded.
- **Who reads it.** A tenant's catalogue view (its sources and the server's) is listed to its
  administrators in full, and to a holder of `promote` on one of its channels for the names its
  grants reach there (the readers spec 0031 gives a channel's sources): source, name, versions,
  DuckDB versions, platforms, state (held until when, withdrawn with the reason), visibility, and
  whether a build declares licence keys. Install and anonymous callers see nothing of it. What
  other tenants do is not shown: a server mirror's view lists its entries and the versions it has
  made available, never which cells were asked or when a build was last used.

### Rules, follows and placement

- **A rule** (channel, name) → source and mode, written by a channel administrator (spec 0031's
  `PUT …/sources/{name}` with `{"source": …, "mode": "auto"|"manual"}`), by a home channel's entry,
  or by a placement's first arrival (below). `auto` places each new available version of the name
  from the source that the channel serves; `manual` places nothing by itself. `DELETE` clears it.
  `tenant` takes `manual` rules only: it is everyone in the tenant who may publish, so taking it
  automatically would let a grant on `dev` reach `prod`.
- **A follow** (channel, source), written by a channel administrator, is evaluated at each pass,
  never copied into rules: for a name with no rule in the channel it places the source's newest
  available version, as an `auto` rule would, and writes the name's rule with mode `follow` (serve
  this source; automatic while the follow exists, `manual` once it is removed). Removing a follow
  places nothing more; what it placed stays. A follow never takes a name the tenant uses from
  another source: a name kista reserves server-wide (unless the followed source is DuckDB's own
  repository of that kind), a name reserved in the tenant (spec 0030), a name another of the
  tenant's own sources has builds of (its records of server builds aside), or a name whose rule in
  any of the tenant's channels names another source; so `dev` and `prod` following one source both
  take its names. A follow on `tenant` is refused.
- **A channel hold**, an optional duration on the channel: an automatic placement into it waits
  until the build's arrival plus the channel's hold, if that is later than the source's.
- **Placement** inserts a release of a catalogue build into a channel, through today's insert
  (spec 0008's release service): signing with the channel's keys before the transaction, then under
  the channel lock the checks: the catalogue build still not held (unless `early`) nor withdrawn
  (read with a row lock that the withdrawal takes too), the block, the slot (per source), the
  selected source, the channel's DuckDB versions, reserved names; then the release, its signatures,
  the snapshot bump. A server build placed into a tenant first gets the tenant's own build record
  (referring to the server's), and its body is copied into the tenant's storage domain when that is
  not the server's (`default`), checked against the body hash, so serving, blocks, attachments,
  GC and tenant deletion (spec 0019) keep working per tenant and residency holds.
- **Automatic placements** are made by the placement pass, under a lease (one replica at a time),
  from a queue of due work: an arrival, a hold ending, a rule, follow or channel version changed,
  a target due; and an hourly sweep that catches anything the queue missed. It skips suspended and
  deleting tenants. For each `auto` rule and follow concerned it places, per cell, the newest
  available build of the source that is newer (by spec 0006's version order) than the channel's newest release of that source in
  the cell, and never a build that has or had a release or a purged slot there, so a yanked or
  purged version is not placed again and an older version whose hold ends later does not overtake a
  newer one. A failure (a block, a slot) is the rule's last outcome, shown with it, and is not
  retried until something changes.
- **By hand**: `POST …/channels/{c}/extensions/{ext}/releases/place` with `{"source": …,
  "version": …, "visibility": "private"?, "current": bool?, "early": bool?}`: every build of the
  version the channel serves, all or none. It takes `promote` on the channel for the name (reserved
  names only through a grant naming them, as today). A held build is placed only with `early`, by a
  channel administrator, audited; so is a version every release of which in the tenant is yanked
  (someone took it out on purpose). CLI: `kista admin release place <t>/<c> <name> <version> -source
  <source> [-early]`.
- **Promotion** (spec 0008) is a placement whose source is what another channel serves: today's
  route and grants (`promote` on the target, `publish` on the source channel), now also refusing a
  held build without `early`.
- **First arrival.** A placement under a name with no rule (a publication, a promotion, a manual
  placement) writes the rule, mode `manual`, when the caller administers the channel or publishes
  into it (as spec 0031's arrival); a `promote` holder who is neither gets `409` and a channel
  administrator writes the rule first. A placement under a name whose rule names another source is
  `409` (spec 0031's `…/source`).
- **Visibility** only narrows: a placement is public only when the catalogue build is public and the
  placer allows it.
- **Passthrough channels** (spec 0009) take rules only for core names from a `duckdb-core`
  upstream source (the tenant's or the server's) in eager mode (a passthrough request carries no
  token to ask with), without a hold, public, and nothing else is placed into them. A tenant
  shadow is recorded at placement, never at arrival: a body that is not DuckDB's core build placed in a signed channel under a core name or a name a passthrough channel's
  rule names.

### Licences

A licence (spec 0012) names its holder (`sub`, an organisation) and the extension (`ext`), and is
signed with a key. It is bound to:

- **the organisation**: the tenant's expected holder (spec 0028) is checked on every path, as
  today; a licence for another holder is never stored;
- **the name**: `ext`;
- **the source**, by its key. In a tenant, each licence key belongs to one source, its **owner**,
  derived from spec 0021's key records in the tenant's view: a server source that declares it, if
  any (a tenant cannot take a distributor's key over by mirroring a binary that carries it), else
  the tenant's source that recorded it first; when the owner goes (spec 0021 drops a removed
  source's keys), the next declarer owns it. Another source declaring an owned key gets no
  licence through it, and an event (`license.key_conflict`) tells the tenant's administrators. A
  licence's source is its key's owner. Ownership is per tenant: one tenant's sources never affect
  another's.

What changes:

- **Stored per (tenant, name, source)** instead of (tenant, name): a tenant may hold licences for
  two extensions of one name from different sources. Importing a licence replaces the one of the
  same (name, source), with spec 0021's same-key condition. The relay (spec 0012) applies a
  licence only when its key's owner in the tenant is the relaying upstream, and replaces by (name,
  that upstream).
- **Served by the channel's selected source**: `-/licenses/<name>` in a channel serves the licence
  whose source is the channel's source for the name, when that source has a live release there
  declaring the key (spec 0021's rule). There is never more than one.
- **Live keys.** A key is live for a tenant when a release in one of its channels declares it
  (live, as spec 0021 says), or when the newest available, not withdrawn, version of the name in
  its source declares it, so a licence is accepted as soon as the extension is available in the
  catalogue, without waiting for a placement, and a rotated-out key stops counting once the newer
  versions declare another and old releases are yanked (spec 0021's rotation). Held builds' keys do
  not count.
- **Relay** stays for a tenant's own `repository` upstreams with a credential. A server upstream
  relays none (its credential is the server's, not an organisation's); licences for server sources'
  extensions come by import (a licence manager, spec 0029, or the tenant's administrator).
- **Usage** (spec 0012) is counted per channel as today; forwarding goes to a name's selected source
  in that channel when it is a tenant's own `repository` upstream with forwarding on. A server
  source's usage is read by server administrators in the statistics, per source.

The source is not a claim in the licence: a product reaches tenants through different sources along
a relay chain (a distributor's publisher in one kista, an upstream in an organisation's own kista),
and in each kista the key's owner is the source there.

### The API and the CLI

- **Server** (server administrators; distribution admins for the server publishers they are
  bound to): `GET|POST /api/v1/catalogue/upstreams[/{u}]` and `…/publishers[/{p}]` with spec 0009's
  and 0023's sub-routes (minus the channel), `PUT …/{source}/hold`,
  `GET /api/v1/catalogue/{source}/extensions[/{ext}[/versions/{v}]]`,
  `POST …/versions/{v}/withdraw {"reason": …}`, the publication route above. CLI: `kista admin
  catalogue upstream|publisher|hold|withdraw …`. `auth.server_distribution_admins` entries gain the
  server publishers each may administer (keys and withdrawals; never holds nor reserved names).
- **Tenant**: `GET /api/v1/tenants/{t}/catalogue[?source=&name=]`,
  `GET …/catalogue/{source}/extensions/{ext}` (the readers above); `PUT …/upstreams/{u}/hold`,
  `…/publishers/{p}/hold`, `PUT …/upstreams/{u}/home {"channel": …}`, `DELETE …/builds/{id}`
  (purge, its sources' administrators). Spec 0009's and 0023's routes otherwise unchanged.
- **Channel**: `…/channels/{c}/sources[/{name}]` gains `mode`, the current and newest available
  versions and the last outcome; `PUT|DELETE …/channels/{c}/follows/{source}`;
  `PUT …/channels/{c}/hold`; `…/releases/place`. CLI: `kista admin channel source <t>/<c> <name>
  -set <source> [-mode auto|manual]`, `kista admin channel follow <t>/<c> <source> [-remove]`,
  `kista admin channel hold <t>/<c> <duration>`.
- **Events**: `catalogue.arrival`, `catalogue.hold`, `catalogue.withdraw` (server), its tenant
  echo `catalogue.withdrawn`, `catalogue.purge`, `rule.set`, `rule.clear`, `follow.set`,
  `follow.clear`, `release.place` (with `early`), `placement.failed`, `license.key_conflict`.
- **Existence**: another tenant's sources are spec 0007's `404`; a server source is not secret
  from a tenant's readers.

### Data model

**Where the server's sources are stored.** Everything about a source is stored per tenant today:
upstreams, their cells and asks, publishers and their keys, builds, licence-key records, a body's
storage domain, GC. So the server's sources are stored as the sources of an internal owner row in
`tenants`, `-server` (a name no tenant can have: spec 0003's rule starts with `[a-z0-9]`), and all
of that works for them unchanged. It is storage only, not a tenant anyone sees: it has no channels,
issuers, grants or members, lives in the `default` domain, and is never listed, served, suspended
or deleted. Every loop over tenants that is not about sources leaves it out (the tenant list,
statistics and usage, feeds, the reserved-names sync, deletion and its reaper, the bootstrap,
suspension); a test walks each of them. The alternative, a nullable owner on every table that has
a tenant, needs a second form of every path and unique keys that behave differently with nulls in
the three dialects.

Migration 0034 (`min_reader 34`: an older replica would place nothing, serve withdrawn builds and
read slots by the old key) in the three dialects:

```text
tenants              the internal '-server' owner row
builds               + source (as releases.source), state (held, available, withdrawn),
                     available_at, visibility, publisher_key_id?, catalogue_build_id? (a tenant's
                     record of a server build); unique (tenant_id, source, name, body_hash);
                     unique slot (tenant_id, source, name, ext_version, platform, slot) where
                     not legacy_duplicate (below)
releases             slot unique (channel_id, source, name, ext_version, platform, slot)
purged_slots         + source ('*' for slots purged before: taken for every source)
upstreams            + home_channel_id (from channel_id, which goes), duckdb_versions, hold_seconds
publishers           + hold_seconds, allowed_reserved (server publishers)
channels             + hold_seconds
channel_sources      + mode (auto, manual, follow), last_outcome
channel_follows      channel_id, source, set_by, set_at; pk (channel_id, source)
build_targets        build_id, channel_id, actor, publisher_key_id?, at; pk (build_id, channel_id)
withdrawals          source, body_hash, name, ext_version, reason, actor, at; pk (source, body_hash)
upstream_asks        + channel_id
licenses             pk (tenant_id, name, source)
```

GC (spec 0016): a build is dead when it has no release, no tenant build refers to it, and
retention (above) does not keep it; a body is dead when no build of any tenant, `-server`
included, holds it in the domain, as today. Tenant deletion reaps the tenant's builds, never
`-server`'s.

### Existing data

The migration keeps what every channel serves, and what arrives next goes where it goes today:

- every upstream's channel becomes its home channel; that channel gets an `auto` rule for each name
  of its allowlist (an existing selection naming the upstream becomes `auto`), and a follow when the
  allowlist has `*` (pull-through); its DuckDB versions are its home channel's;
- every other existing selection (a publisher, `tenant`, an upstream selected in a channel that is
  not its home, by promotion) becomes `manual`: a CI publishing into `dev` does not start reaching
  `prod`, and `nightly`'s mirror does not start flowing into `prod`;
- every build gets the source its releases name; a build whose releases name two sources (spec
  0031's same-body rule) is split into one per source, its releases, signatures and licence-key
  records repointed; all are `available`;
- slots get their source from the release; two bodies of one version under one source in two
  channels (possible today) are both kept, all but the oldest marked `legacy_duplicate` (outside
  the unique slot), and listed in the upgrade notes; a new arrival conflicting with any is refused;
  a slot purged before the migration has no release to read its source from, and stays taken for
  every source;
- licences get the source that owns their key in the tenant (spec 0021's records); a licence whose
  key no record has (rotated out or refused, so not served today either) gets the source
  `unknown`, is kept for its administrators and is not served until re-imported;
- tenants with two upstreams of the same repository (one per channel) keep two sources.

The cells API loses `other_source` and `shadowed` (now placement outcomes, on rules), and the
console's Sources tab shows modes: both are in the upgrade notes.

## Security

- **Tenant isolation.** A tenant's sources and builds are seen and placed by it alone; a server
  source is shared by the server administrator's decision only. A tenant cannot publish into a
  server source, set its hold or withdraw from it; a server publisher cannot place into any
  channel, and a distribution admin administers only the server publishers bound to it. Bodies are
  read only through the tenant's own release, from its own domain. A tenant does not learn what
  other tenants ask for or install.
- **The hold holds on every path**: automatic placement, the channel route (the build waits for its
  target), manual placement and promotion all refuse a held build except a channel
  administrator's audited `early`; lowering a hold does not release builds already held; a server
  source's hold is a server administrator's. It protects tenants from a server source's stolen key
  or compromised upstream; a tenant publisher's key that holds `publish` on a channel reaches that
  channel after the publisher's own hold, as today.
- **Withdrawal is immediate and final**: the withdrawal's transaction bumps every channel holding a
  release of the build, and serving reads the build's state; placement re-checks it under the
  channel lock; a withdrawn build's release is never re-activated, nor does the body re-arrive in
  its source. It reaches only releases of the server's build, never a tenant's own builds of the
  same body.
- **Fail closed.** Nothing falls back to another source by itself (spec 0031); a follow never takes
  a name the tenant uses or a reserved name from a foreign source; `tenant` is never automatic.
- **Grants keep their meaning**: placing needs `promote` for the name on the target; only a channel
  administrator or a publisher into the channel writes a rule by arrival; a publish grant on `dev`
  reaches `prod` only through a rule `prod`'s administrator wrote for that publisher.
- **Verification happens once**, at arrival, against the source's signature; placement re-signs
  with the channel's key and re-checks blocks, withdrawals, holds, slots and the source under the
  channel lock; a copy into another domain is checked against the body hash.
- **Licences**: the expected holder on every path; keys from trusted sources only (spec 0021); a key
  belongs to one source, so another source's build declaring a copied key gets no licence; a server
  credential never relays an organisation's licences.
- **Limits**: per-source quotas, one leased placement pass, and spec 0009's fetch limits per
  upstream (the server's mirrors fetch once for every tenant).
- **Existence.** Install and anonymous callers learn nothing new: the catalogue is not served, and
  the DuckDB routes and the index are unchanged.

## Testing

- The store suite on three dialects: builds per source (two sources, one name, one body, one
  version); slots per source; holds (raised, lowered, the channel's); rules, modes, follows and
  their limits; withdrawal bumping the channels that hold the build; licences per (name, source) and
  key owners per tenant (a server source's key not taken over by a tenant's mirror);
  the migration's backfill from a spec 0031 database (home channels, `auto` and `manual`, splits,
  duplicate slots).
- The placement pass: idempotent, leased, newest-only ordering (a held older version does not
  overtake), yanked and purged versions not re-placed, failures recorded, suspended tenants skipped,
  a cross-domain placement copying and checking the body; a placement racing a withdrawal.
- Intake: every path lands a catalogue build; the channel route places at once or at the end of the
  hold; an upstream with a home channel serves new entries; lazy and pull-through through a follow
  fetch and place in one request's retry window.
- Licences: imported before any placement; one served per channel by its source; a copied key
  declared by another source verifies nothing; a server upstream relays none.
- The existing DuckDB end-to-end suite passes unchanged after phase A1.
- New e2e with the pinned DuckDB: a server mirror of DuckDB's community repository and a tenant's
  publisher both bring a name (`quack`, not a reserved one); tenant A's `prod` serves the
  community's by a rule, tenant B follows a server publisher and gets its builds after the hold;
  a withdrawal stops B's install at once; a held build placed `early`; a licensed extension's
  licence imported before placement, served once placed, and verified by the SDK.

## Delivery

One spec, four phases, each a PR with its own reviews:

- **A1. The catalogue in a tenant**: sources with home channels, builds per source, slots per
  source, rules with `auto` and `manual`, the placement pass and manual placement, the channel
  route through the catalogue, the migration, GC; the existing e2e suite unchanged.
- **A2. Follows and holds**: follows with their limits, source and channel holds, `early`, lazy and
  pull-through through follows, quotas.
- **B. The server's catalogue and licences**: the server's sources and routes, server
  publications, placement across tenants and domains, withdrawal,
  distribution admins bound to publishers, licences per (name, source) with key owners, relay and
  usage.
- **C. The console and the end to end**: a Catalogue screen (sources, names, versions, held and
  withdrawn, place), the channel's Sources tab with modes, follows and the hold, server sources in
  the Server screen, the new DuckDB end to end, the website's pages.

Enterest moves onto it after B (its tenants' administrators use kista's API and CLI until C): one
kista, its pipeline a server publisher with a day's hold, DuckDB's repositories server mirrors. Its
spec 0003's decisions change accordingly.

## Alternatives considered

- **A catalogue per tenant only**: every tenant mirrors DuckDB's repositories itself and a
  distributor publishes into each tenant: the duplication this spec removes.
- **One catalogue with no owners**: a tenant's private publications would be visible to others, or
  need per-entry grants that amount to owners.
- **Catalogue entries keyed by name**: the collision moves up a level; a tenant could not have
  DuckDB's `h3` in one channel and a distributor's in another.
- **A second kista for a distributor's catalogue** (Enterest's decision 27): no kista changes, at
  the cost of double storage and downloads, withdrawals carried by hand, licences waiting for the
  mirror, and the per-channel contests.
- **Server sources with a nullable owner** instead of the internal owner row: every per-tenant
  table and path (upstreams, publishers, keys, cells, GC) would need a second form.
- **Copying a follow into per-name rules**: it could not be removed or changed afterwards.
- **The source as a licence claim**: the source differs at each kista of a relay chain; the key's
  owner identifies it in each.
- **Licences per issuer**: one issuer (Enterest) signs licences for many developers' extensions,
  so it does not separate two extensions of one name.
- **Holds per channel only**: the purpose is to keep a compromised source from reaching anyone
  automatically; that is the source's property. A channel may add its own.
- **Withdrawal for a tenant's own sources**: blocks and yanks already do it there.
- **Publishing a tenant's build into the catalogue only**: the channel route covers CI; a
  catalogue-only route for tenant publishers is a follow-up if wanted.

## Changes to earlier specs

- **0001**: the concepts gain Source, Catalogue, Rule and Follow; an Upstream feeds the catalogue;
  a slot is per source.
- **0006**: a release of a withdrawn body is never served nor re-activated.
- **0007**: a snapshot leaves out releases of withdrawn builds; the sources answers gain modes and
  versions.
- **0008**: publications land in the catalogue and are placed into their channel when available;
  promotions are placements and respect holds; slots per source.
- **0009**: an upstream feeds the catalogue and may have a home channel; its own DuckDB versions;
  two upstreams may list one name; `other_source` and `shadowed` become placement outcomes;
  passthrough channels take core names from `duckdb-core` sources only.
- **0012, 0021**: licences per (tenant, name, source); key owners; live keys from the catalogue's
  newest available version; relay from a tenant's upstreams only; usage forwarded by the selected
  source.
- **0014**: a bundle imports into its upstream's source in the catalogue.
- **0015**: the console's catalogue, rules, follows and holds (phase C).
- **0016**: catalogue retention, purging unplaced builds, the server's sources' builds.
- **0030**: server publishers take no reserved name unless allowed; distribution admins bound to
  server publishers; a distributor's tenant reservations are no longer needed for a server
  catalogue (kept, unchanged).
- **0031**: selections gain a mode; follows; contests happen at placement; the same-body rule goes
  (slots are per source).
- **0032**: asks come from channels whose rule or follow names the upstream; the fetch places into
  the asking channel.

## Follow-ups

- Merging two sources of one repository.
- Version constraints in a rule (`auto` within `1.x`).
- Forwarding a server source's usage to its distributor; licence managers scoped to sources.
- Showing a server source to some tenants only.
- Notifying a channel's administrators of a new version under a `manual` rule.
- A server-wide block by body hash, beyond a server source's withdrawal.
