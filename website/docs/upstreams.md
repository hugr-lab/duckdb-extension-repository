---
title: Upstreams
---

# Upstreams

An **upstream** brings builds into a tenant's [catalogue](catalogue.md) from another DuckDB
repository: DuckDB's core repository, DuckDB's community repository, or any other DuckDB repository
(another kista). kista fetches the binaries the tenant allows, verifies the upstream's signature over
the exact bytes it stores, checks the file as a [publication](publishing.md) is checked, and releases
it into the upstream's channel, its **home channel** (other channels take its builds by their
[rules](#the-catalogue-and-a-channels-rules)):

- into a **signed** channel, re-signed with the channel's key: the signature then means "checked
  and allowed by this tenant";
- into a **passthrough** channel, with DuckDB's own core signature, so DuckDB can autoload it from
  the tenant.

Upstreams are managed by tenant administrators. Runs happen in `kista serve`; the CLI only records
a request. The design reasoning is in
[spec 0009](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0009-upstreams/spec.md).

## Kinds and modes

| Kind | Source | Keys checked at intake |
| --- | --- | --- |
| `duckdb-core` | `https://extensions.duckdb.org` | DuckDB's core keys, carried by kista from the pinned DuckDB |
| `duckdb-community` | `https://community-extensions.duckdb.org` | DuckDB's community keys, from the pin |
| `repository` | a `prefix` you give: `https://host[:port]/<tenant>/<channel>` | the fingerprints you pin (`keys`) |

| Mode | Behaviour |
| --- | --- |
| `mirror` (default) | runs on a schedule and on demand, fetching every cell of its matrix; a [lazy](#lazy-mirrors) mirror fetches only the cells asked for and its eager entries |
| `pull-through` | never runs; a granted caller's miss on a DuckDB route fetches that one binary in the background |
| `bundle` | never contacts its source: only bundle imports fill it, on an air-gapped site ([Bundle mode](#bundle-mode)) |

An upstream has one home channel, the channel it is added with. Its builds land in the catalogue
as its own source, and its runs release them into its home channel; another channel takes them by a
rule naming the upstream, or by following it. Two limits remain: a mirror's matrix follows its home
channel's DuckDB versions, and lazy and pull-through fetches are asked for from its home channel
only (a miss in another channel fetches nothing). An upstream without a home channel, and asks from
any channel whose rule names it, are a follow-up. Bodies are stored once per storage domain. A name
is on the allowlist of at most one upstream of a channel.

## What is fetched

A repository has no listing, so the **allowlist** names what to fetch. The work is a matrix:

```text
channel's DuckDB versions  x  upstream platforms  x  allowlisted names
-> <prefix>/<duckdb version>/<platform>/<name>.duckdb_extension.gz
```

Each element is a **cell**. The path serves the upstream's current build, so a version older than
the upstream's current one cannot be mirrored. An allowlist entry's `versions` filters what is
accepted: empty means any extension version, otherwise only the exact versions listed.

Limits: 20,000 cells an upstream and 100,000 a tenant (adding an entry or a platform that would
exceed them is `400`; a run whose matrix grew past them fails without fetching), 100 upstreams a
tenant, 1,000 entries an upstream, 100 versions an entry, 10 pinned keys.

**Names.** An entry is a canonical name: an alias (`postgres`) is refused with its canonical name
(`postgres_scanner`). A core name is accepted only from a `duckdb-core` upstream and a community name
only from a `duckdb-community` upstream, unless the entry sets `allow_reserved` (a tenant that mirrors
its own build of `httpfs` from another kista says so). `allow_reserved` concerns DuckDB's names only.
An entry added before its name became reserved keeps mirroring it.

A build mirrored from a `duckdb-core` upstream reserves its name server-wide as core, and one from a
`duckdb-community` upstream as community, with the source `upstream:<tenant>/<upstream>` (a bundle
import: `bundle:<id>`), in the release's transaction. Those upstreams' bodies are verified with
DuckDB's own keys, so only DuckDB's names are learned this way ([Reserved
names](publishing.md#reserved-names)).

## Intake verification

A cell's binary is released only if every step passes:

1. **Fetch** through the egress client (below): no redirects, the compressed answer at most
   `blob.max_body` + 1 MiB, at no less than `upstreams.min_rate` after 30 seconds, within
   `upstreams.fetch_timeout`. The answer is spooled to a temporary file first, so a slow upstream
   never holds a storage ingest slot. `If-None-Match` is sent with the cell's last ETag when its last
   outcome was `released`, `unchanged`, `yanked`, `conflict` or `landed`.
2. **Decompress** a gzip answer, under `blob.max_body` + 256 bytes.
3. **Signature**: the last 256 bytes must verify over the body with the upstream's keys. The key that
   verified is recorded.
4. **Footer**: the platform is the cell's; a `cpp` or `C_STRUCT_UNSTABLE` build's DuckDB version is
   the cell's; a `C_STRUCT` build is one the channel serves; the extension version is on the entry's
   `versions` when given.
5. **File**: the format matches the platform and the binary exports the name's entry point, as for a
   publication. A signed binary of one extension is never served under another name.
6. Under the channel lock: a **blocked** body, a name the tenant **shadows** with its own build, and
   the upstream's slot. A slot that holds the same body (in any state) is `unchanged` or `yanked`: an
   administrator's yank, visibility or current choice survives every run. A slot holding another
   body is a `conflict`: versions are immutable within a source, so the version stays as it is.
7. The body is committed and released with origin `upstream`. The upstream's original signature is
   kept with it (a passthrough channel serves that one; a signed channel serves its own), and the
   provenance records the upstream, kind, URL, ETag, verifying key, fetch time and credential.
   The new release becomes **current** for its name and platform: core and community versions are
   often commit hashes, so kista does not order them; the newest accepted build is current.

A refused cell is logged and counted; it never stops the run. A cell that turns `rejected`,
`conflict`, `blocked`, `shadowed` or `other_source` (from another outcome) is recorded as an
`upstream.rejected` event.

## Adding an upstream

```bash
# DuckDB core extensions into a signed channel, private by default
kista admin -config /etc/kista/kista.yaml upstream add acme core \
  -kind duckdb-core -channel prod \
  -platforms linux_amd64,linux_arm64,osx_arm64 \
  -extensions httpfs,spatial,postgres_scanner

# a few community extensions, public
kista admin -config /etc/kista/kista.yaml upstream add acme community \
  -kind duckdb-community -channel prod -platforms linux_amd64 -extensions h3,prql -public

# another kista's channel, its signing key pinned by fingerprint
kista admin -config /etc/kista/kista.yaml upstream add acme hugr \
  -kind repository -prefix https://kista.hugr.example/hugr/prod \
  -keys sha256:3f9a… -channel prod -platforms linux_amd64 -extensions tresor,hugr_node
```

| Flag | Field | Meaning |
| --- | --- | --- |
| `-kind` | `kind` | `duckdb-core`, `duckdb-community` or `repository` (fixed at creation) |
| `-channel` | `channel` | the channel it feeds (fixed) |
| `-platforms` | `platforms` | at least one, in DuckDB's platform grammar |
| `-extensions` | `extensions` | the allowlist; `*` for any name (pull-through only) |
| `-prefix` | `prefix` | `repository` only: an `https://` URL with an optional port and path (fixed) |
| `-keys` | `keys` | `repository` only, at least one: `sha256:<64 lowercase hex>` |
| `-public` | `visibility` | releases it makes from now on are public; default `private` |
| `-mode` | `mode` | `mirror` (default), `pull-through` or `bundle` (fixed) |
| `-fetch` | `fetch` | a mirror's: `eager` (default) or `lazy` ([Lazy mirrors](#lazy-mirrors)); changed later with `upstream fetch` |
| `-credential` | `credential` | `repository` only: a configured credential's name (see [Private upstreams](#private-upstreams)) |

Over the API, the same record is `POST /api/v1/tenants/{t}/upstreams` with a JSON body:

```json
{
  "name": "core",
  "kind": "duckdb-core",
  "channel": "prod",
  "platforms": ["linux_amd64", "osx_arm64"],
  "extensions": [{"name": "httpfs"}, {"name": "spatial", "versions": []}],
  "visibility": "private"
}
```

For a `repository` upstream, kista reads the upstream's `.well-known/duckdb-extension-repo.json` when
the upstream is added and at the start of every run (not in [bundle mode](#bundle-mode), which reads
the file a bundle carries); only keys whose fingerprints are pinned are
used. Adding an upstream whose file lists none of its pinned keys is `400`. A rotation at the
upstream is followed by pinning the new fingerprint, never automatically. On the upstream kista,
`kista admin key list <tenant>/<channel>` shows each key's fingerprint.

Adding an upstream or an entry whose name another source holds in its channel is allowed: its cells
are `other_source` until the channel's administrator switches the name's source to the upstream
([A name's source in a channel](#a-names-source-in-a-channel)).

### From the bootstrap

An install made from a template can make its channels' upstreams on its first start: the
`bootstrap` section's `upstreams` take the fields of `upstream add`, and are checked, made and
recorded once, with the rest of the section
([configuration](configuration.md#bootstrap), spec 0024). A new mirror is due at once. Upstreams
wanted later are added here, as above: the bootstrap is not a reconciler.

## Changing an upstream

| Route (under `/api/v1/tenants/{t}/upstreams`) | CLI |
| --- | --- |
| `GET /`, `GET /{name}` | `upstream list <tenant>`, `upstream show <tenant> <name>` |
| `DELETE /{name}` (`If-Match`) | `upstream remove <tenant> <name>` |
| `POST /{name}/public`, `/private`, `/pause`, `/resume` (`If-Match`) | `upstream public\|private\|pause\|resume <tenant> <name>` |
| `GET, POST /{name}/extensions`; `DELETE /{name}/extensions/{ext}` | `upstream extension add <tenant> <name> <ext> [-versions v1,v2] [-allow-reserved] [-eager]`, `upstream extension remove …` |
| `GET, POST /{name}/platforms`; `DELETE /{name}/platforms/{platform}` | `upstream platform add\|remove <tenant> <name> <platform>` |
| `GET, POST /{name}/keys`; `DELETE /{name}/keys/{fingerprint}` | `upstream key add\|remove <tenant> <name> <fingerprint>` |
| `POST /{name}/sync[?dry_run=true]` | `upstream sync <tenant> <name> [-dry-run]` |
| `GET /{name}/cells[?outcome=…]` | `upstream cells <tenant> <name> [-outcome <outcome>]` |
| `POST /{name}/credential {"credential": name or ""}` (`If-Match`) | `upstream credential <tenant> <name> <credential>\|-` |
| `POST /{name}/fetch {"fetch": "eager" or "lazy"}` (`If-Match`) | `upstream fetch <tenant> <name> eager\|lazy` ([Lazy mirrors](#lazy-mirrors)) |
| `POST /{name}/usage {"usage": "forward" or "off"}` (`If-Match`); `GET /{name}/usage` | `upstream usage <tenant> <name> [forward\|off]` ([forwarding usage](licensing.md#forwarding-usage-upstream)) |

`POST /{name}/extensions` on an existing name replaces its `versions`, `allow_reserved` and `eager` (`200`).
The upstream's `ETag` moves with every change to it or its lists; a sync does not move it.

**Removing** an upstream keeps the releases it made (they belong to the channel; yank them if you
want them gone) and removes its cells, the relayed licences of its entry (each a `license.remove`, reason
`upstream removed`; an import there stays, under the entry's id form; see [Licensing](licensing.md#a-licences-entry))
and the licence keys it brought; the names it provided stop being reserved. An upstream added again
is another entry: its relay supplies nothing until it has fetched builds itself.

**Pausing** an upstream stops runs after the cell in progress, and holds the current release:
narrowing an entry's `versions` holds a version too.

**Visibility** applies to releases the upstream makes from now on. Existing releases keep theirs;
change them per release (see [Publishing](publishing.md#changing-a-release)).

## Runs and schedule

- **Scheduled**: an active `mirror` upstream is due `upstreams.interval` (default 6h, within
  15m..168h) after its last run ends, with up to 10% jitter; a mirror never run is due at once.
  Adding a DuckDB version to the channel, adding a platform, adding or changing an entry, resuming
  the upstream or turning it eager makes it due at once. A run interrupted by an error or a lost lease is due again after
  5 minutes.
- **On demand**: `sync` sets a request; a running `kista serve` takes it within 30 seconds. A request
  that arrives during a run is served by another run right after it. A sync of a paused,
  pull-through or bundle-mode upstream is `409`. A lazy mirror's sync fetches its whole matrix.
- **Dry run** (`?dry_run=true`, `-dry-run`): fetches and checks every cell (a lazy mirror's: the
  cells a scheduled run would fetch, not its whole matrix) without releasing,
  committing or touching the cells and the schedule. Its counts (`would_release` for a cell it would
  release) and first 20 problems go to `last_run`.
- Every replica polls every 30 seconds. One run at a time per upstream (a lease,
  `kista/upstream/<id>`, renewed every 30 seconds). A replica runs at most
  `upstreams.concurrency` upstreams and fetches at most that many cells at once (default 4), and at most 2 at once per upstream host.
- A run follows the record as it is: pausing or removing the upstream stops it after the cell in
  progress; a removed entry, platform or key applies to the next cell.
- A cell the upstream answers `503` or `429` with a `Retry-After` of at most 30 seconds is asked
  again after that delay, at most ten times, before it is `failed`; the host's slot is free while
  it waits. A lazy kista answers a cell it is fetching this way ([The first
  request](#the-first-request)), so a kista mirroring it usually gets the build within its run. Runs,
  pull-through and bundle exports' syncs alike.

### A run at a limit

A tenant's [bytes limit](administration/tenants-and-channels.md#limits) and its source's
[full catalogue](#the-catalogue-and-a-channels-rules) (`catalogue.max_entries`,
`catalogue.max_bytes`) are not the file's fault, and neither clears within a run (spec 0038). A run
reads the tenant's use and the catalogue once when it starts. Without room (the tenant uses its
whole bytes limit or the catalogue is full at the start, or a cell was refused by either since):

- **a cell whose record holds no body** (never fetched, `missing`, or refused: a refusal took
  none) is skipped without a request, its record left as it is, and counted `skipped_at_limit`; a
  lazy mirror's cell never fetched is recorded `failed` instead, so its first request is answered
  `404`, not `503` while nothing comes;
- **a cell that holds a body** runs as ever: conditionally with its ETag (a `304` costs nothing),
  or unconditionally without one; a body the tenant has passes (a yank kept, a changed ETag), and a
  new body is refused after its download;
- **a refused cell** is `failed` (not `rejected`, so no `upstream.rejected` event), its detail naming
  the limit or the cap, among the run's problems; a failed cell keeps the body and the ETag it held.

A run in which a cell was refused ends with `error` "the run failed: …": failed, not interrupted,
so it is not retried after 5 minutes, and its next run comes at its usual time. A run that only
skipped cells did not fail. A dry run skips and decides the same way. The cells come once there is
room:

- **the tenant's bytes limit**: a server administrator raises it, or the tenant's administrators
  purge builds placed nowhere (a purged release's build stops counting at once);
- **a full catalogue**: the tenant's administrators place or purge the source's builds placed
  nowhere, or the operator raises `catalogue.max_entries` or `catalogue.max_bytes`.

A pull-through fetch and a lazy mirror's first request check the same before the download; a cell
skipped or refused so is answered as a failed fetch and not fetched again within
`upstreams.negative_ttl` (a pull-through install keeps failing that long after room is made).

```yaml title="kista.yaml"
upstreams:                   # file-only
  concurrency: 4             # runs, and cells fetched, at once per replica; 1..32
  fetch_timeout: 10m         # one file's download; 1m..2h
  min_rate: 64KiB            # a download's least rate after 30s; at least 1KiB
  interval: 6h               # between a mirror's scheduled runs; 15m..168h
  negative_ttl: 1h           # pull-through and lazy asks: a refused, missing or limited cell is not fetched again within it; 1m..24h
  credentials: []            # private upstreams, below
```

## Statuses

**Upstream** `state`: `active` or `paused`. **Run state** (`run` in the API): `idle`, `requested`,
`dry_run_requested` or `running`. `last_run_at` and `last_run` record the last run:

```json
{
  "started": "2026-10-12T06:00:00Z",
  "ended": "2026-10-12T06:02:41Z",
  "counts": {"released": 3, "unchanged": 41, "missing": 4},
  "problems": [
    {"duckdb_version": "v1.4.1", "platform": "linux_arm64", "name": "spatial", "outcome": "missing"}
  ]
}
```

`error` is set when the run failed before its cells, or when a cell met a [limit](#a-run-at-a-limit). `problems` holds the first 20 cells that were
neither released, unchanged nor landed. A lazy mirror's run also counts `not_asked` (cells of its matrix it
did not fetch and never fetched, as the cells API lists them) and `busy` (cells another fetch held,
skipped). A run without room counts `skipped_at_limit` ([A run at a limit](#a-run-at-a-limit)).

**Cell outcomes** (`upstream cells`, `GET …/cells?outcome=`):

| Outcome | Meaning |
| --- | --- |
| `released` | a new release was made |
| `unchanged` | the slot already holds this body |
| `yanked` | the slot holds this body, yanked; it stays yanked |
| `conflict` | the upstream's slot holds another body; the version is immutable within its source |
| `landed` | the build is in the catalogue, placed nowhere: the channel's rule for the name is `manual` and holds no release of its version |
| `held` | the build is in the catalogue, held (the upstream's [hold](#holds), or the channel's); the placement pass places it in the channel when the hold ends. The cell keeps this outcome until its next fetch |
| `missing` | the upstream answered `404` (also a private upstream without the grant) |
| `rejected` | a check failed: signature, footer, format or entry point |
| `blocked` | the body is blocked in the tenant |
| `shadowed` | a name `"*"` took is reserved since |
| `other_source` | another source holds the name in the channel; decided before the download, asked again at each run |
| `failed` | the fetch failed (network, size, rate, the upstream's `401`/`403`), the catalogue's use could not be read, or a [limit](#a-run-at-a-limit) refused it (the tenant's bytes, the source's full catalogue); retried by the next run |
| `not_asked` | a lazy mirror's cell nobody asked for, of no eager entry, never fetched: listed only with `?outcome=not_asked`, never stored |

Cells that leave the matrix are removed by the next run; removing an entry or a platform removes
its cells at once.

## Reservation and shadowing

A name an upstream of a tenant lists is **reserved in the tenant**, like DuckDB's core and community
names: publishing or promoting it needs a grant that names the extension (see
[Publishing](publishing.md#reserved-names)). Which of them a channel serves is the name's source in
the channel (below). In the index, an upstream release carries `origin: "upstream"` and the
upstream's name; another release of a name the tenant's upstreams provide shows `shadows: upstream`.

## A name's source in a channel

A channel serves each extension name from one **source** (spec 0031):

| Source | Its releases |
| --- | --- |
| `upstream:<name>` | mirrored by that upstream of the channel (a run, a pull-through fetch, a bundle import) |
| `publisher:<name>` | published by a request signed with that publisher's key |
| `tenant` | an administrator's `release add`, or a publication by a tenant principal |

A promotion keeps the source of the release it promotes: a build a distributor published into
`incoming` and an administrator promoted to `prod` is the distributor's publisher's in `prod` too.

- **The first release of a name sets its source** in the channel (an `source.set` event with
  `arrival: true`).
- **Another source's release does not land:** a publication, an `add` or a promotion is refused
  with `409` (problem type `…/source`, naming the source); a mirror's cell is `other_source`, decided
  before the download and asked again at each run; pull-through never fetches the name.
- **Only the selected source's releases are served:** the index, the DuckDB routes, licences and
  usage. The others stay stored, in their state, listed to administrators with `selected: false`.
- **A channel administrator switches it** (admin on the channel, or a tenant administrator):
  `kista admin channel source <tenant>/<channel> <name> -set <source>`, or `PUT
  /api/v1/tenants/{t}/channels/{c}/sources/{name}` with `{"source": "…"}`. Any source of the tenant:
  `tenant`, an upstream (of the channel, or of the channel mirrored releases were promoted from), a
  publisher, whether it has releases there yet or not. The new source's releases are served at once; a mirror fills in at its next
  run. Switching back serves the first source's releases again. `-clear` (`DELETE`) removes the
  source: nothing serves the name until its next release lands and sets it again.
- **A slot is its source's:** two sources may release the same version of a name in one channel,
  even the same body; each release is its source's, and a switch serves the selected one's.
- **A removed upstream's names pass to its successor:** an upstream added again (a new upstream of
  the channel) takes the names whose source is a removed upstream, with the removed one's releases
  of them in the channel: their states, visibility and current choices stay, so a version an
  administrator yanked stays yanked. Nothing else falls back by itself: a yanked or blocked release of the selected source
  leaves the name unserved until it is active again or the source is switched.
- **Listing:** `kista admin channel source <tenant>/<channel>` (or `GET …/sources`) shows each name,
  its source, its rule's mode, the newest version of the source the channel holds and the newest in
  the catalogue, who set it and when, the sources that have releases of it in the channel, and the
  placement pass's last outcome.

Tenant shadows (below) are a passthrough channel's matter and stay as they are.

## The catalogue and a channel's rules

Every build that arrives in a tenant, mirrored or published, is a build of its source in the
tenant's **catalogue** (spec 0033), and channels may take it from there ([The catalogue](catalogue.md)
walks through the common tasks and the console's screens). A version is immutable
within its source in the whole tenant: another body of a version an upstream or a publisher has, or
had once released, is refused in any channel or in the catalogue (`409`; a mirror's cell is
`conflict`). The tenant's own source (`tenant`, everyone who may publish in it) keeps the rule per
channel. A build is kept while a release holds it, or while it waits as landed: a mirror's version
under a manual rule (below) while its cell holds it, or a build published into the catalogue only,
the three newest landed versions of each (source, name, platform, slot); the placement pass's
hourly sweep leaves the others to the collector (spec 0016). A source's builds placed nowhere are
bounded by `catalogue.max_entries` and `catalogue.max_bytes` ([configuration](configuration.md#catalogue));
an administrator's add is not.
A channel takes from the catalogue by its **rule** for each name: the source above, and a **mode**
an administrator gives it.

| Mode | What the channel takes |
| --- | --- |
| `auto` | each new version of the source, per DuckDB version and platform it serves: the newest the cell has not had (a version yanked or purged there is not placed again, nor one older than what the cell had) |
| `manual` | no new version by itself; the versions it holds are completed: a build of one for another DuckDB version or platform the channel serves, made later, is placed too |
| `follow` | a rule a follow wrote (below): as `auto` while the channel follows the source, as `manual` once it no longer does |
| none | a rule an arrival wrote (a mirror's first release in its channel, a publication, a promotion): the channel serves what lands in it, as before; the placement pass leaves it alone, but for a mirror's version held in its own channel (below), which it places there when the hold ends |

- **Choosing**: `kista admin channel source <tenant>/<channel> <name> -set <source> -mode auto`
  (or `-mode` alone to change the mode), or `PUT …/sources/{name}` with `{"source": "…", "mode":
  "auto"}`. The tenant's own source (`tenant`) is never `auto`, and the placement pass never places
  from it: it is everyone who may publish in the tenant. A passthrough channel's rules take no mode.
- **Visibility**: a placement serves a build as it arrived (a mirror's, a publication's), never
  wider; making a release private makes its build private for every later placement, and a version
  private in a channel stays private in the slots a `manual` rule completes.
- **The placement pass** runs every minute on one replica per channel, for the rules whose source
  has builds it has not looked at, and every hour for all of them; adding a DuckDB version to a
  channel makes it look again. A failure (a slot holding another body, a block) is the rule's
  last outcome.
- **A mirror under a `manual` rule** fetches a new version into the catalogue without placing it:
  the cell's outcome is `landed`, and the build is kept until a release takes it. A lazy or
  pull-through fetch under such a rule lands too, and the request misses.
- **Placing by hand**: `kista admin release place <tenant>/<channel> -name <name> -source <source>
  -version <v>`, or `POST …/channels/{c}/extensions/{ext}/releases/place` with `{"source": "…",
  "version": "…"}` (see [Publishing](publishing.md#placing-from-the-catalogue)).
- **Following a whole source**: `kista admin channel follow <tenant>/<channel> <source>`, or `PUT
  …/channels/{c}/follows/{source}` (a channel administrator; an upstream or a publisher, never
  `tenant`; not a passthrough channel). Every name of the source the channel has no rule for is
  taken as an `auto` rule would, its rule written with mode `follow` (a rule an arrival wrote for
  the source, with no mode, becomes the follow's). Skipped, each listed with the reason (`kista
  admin channel follow <tenant>/<channel>`, `GET …/follows[/{source}]`), refreshed at each look (a
  new build of the source, or hourly): a name the tenant uses from another source (another source
  has builds of it, or a channel serves it from another source); a reserved one (DuckDB's names,
  unless the source is DuckDB's own repository of that kind); one
  two of the channel's follows offer; and, for a publisher, a name it may not publish in the channel
  (a follow follows a source, not every grant anyone gives it; a distributor's publisher holds no
  grants: the names it may publish into the catalogue are what a follow takes, but for a name an
  upstream of the tenant lists, which a grant reaches only by naming it, spec 0037). A rule naming
  one takes it. A name
  whose rule is cleared is taken again at the follow's next look: a rule naming another source
  keeps it out. `channel unfollow` (`DELETE`) places nothing more; what it placed stays, its rules
  completing the versions they hold. Removing the source removes its follows.
- **Reading the catalogue**: `kista admin catalogue list <tenant>` and `catalogue show <tenant>
  <source> <name>`, or `GET /api/v1/tenants/{t}/catalogue` and `…/catalogue/{source}/extensions/{ext}`
  (tenant administrators; a distribution admin reads its distributors' publishers' entries only,
  without rules or channels).
- **Publishing into the catalogue only**: `POST /api/v1/tenants/{t}/catalogue/extensions/{ext}/builds`
  (see [Publishing](publishing.md#publishing-into-the-catalogue)), or `kista admin catalogue add`.
- **Purging an entry**: `kista admin catalogue purge <tenant> <build-id>` or `DELETE
  /api/v1/tenants/{t}/catalogue/builds/{id}` (tenant administrators; a distribution admin for a
  distributor's publisher's build) deletes a build that was never released (`409` otherwise); since
  no one could have installed it, its slot is free again, and a mirror's cell that landed it fetches
  it again. A [withdrawn](#withdrawals) build is never purged (`409`, `…/withdrawn`).

### Holds

A **hold** keeps a source's new versions out of every channel for a while (spec 0033): time to look
at a build, or to revoke a stolen key, before an automatic placement serves it.

- **A source's hold**, on a publisher or an eager mirror, `0` (none) by default: an entry arriving
  from it is **held** until its arrival plus the hold. A lazy mirror and a pull-through upstream take
  none (they fetch because someone is installing now), and neither does a bundle-mode upstream nor
  a passthrough channel's upstream (`400`); a mirror with a hold is not made lazy. Raising a hold
  holds the entries held now longer; lowering it shortens only the hold of what arrives from then on.
  A tenant's administrators set it: `kista admin upstream hold <tenant> <name> <duration>` and
  `kista admin publisher hold <tenant> <name> <duration>` (`24h`, `2d`; `0` for none), or `PUT
  …/upstreams/{name}/hold` and `PUT …/publishers/{name}/hold` with `{"hold": "24h"}`. At most 90
  days. A [distributor's publisher](publishing.md#distributors-publishers)'s hold is a distribution
  admin's or a server administrator's only (`404` to a tenant administrator): a tenant that wants
  more waits with a channel hold. An upstream removed and added again (its successor) starts with no hold: set it again.
- **A channel's hold**, set by its administrators (`kista admin channel hold <tenant>/<channel>
  <duration>`, `GET|PUT …/channels/{c}/hold`): an automatic placement into the channel (a rule's,
  a follow's, a mirror's run into its own channel) waits until an entry's arrival plus the channel's
  hold, if that is later than the entry's own. Changing it applies at once, to the entries waiting
  too: lowering it lets them in sooner (the channel's own choice). A channel with a hold asks no lazy
  mirror or pull-through upstream (what they fetch would wait). A passthrough channel takes none.
- **What waits**: the placement pass places nothing held, and looks at a rule again when the first
  hold it waits for ends; then only a version newer than what the channel had, so an older version
  whose hold ends later does not overtake a newer one. A mirror's run lands a held version in the
  catalogue (cell `held`) and writes the name's rule in its channel as a release would; the pass
  places it there when the hold ends, whether or not another channel took it meanwhile and whatever
  the cell holds by then (each version after its own hold, older first), as the run would take it
  then: not while the upstream is paused (once it is resumed), nor once the name or the version has
  left its allowlist, nor once the mirror is made lazy (its askers fetch then, or an administrator
  places it by hand).
  Placing by hand and promoting refuse a held entry (`409`, problem type `…/held`), except a channel
  administrator's placement with `early` (below).
- **A publisher with a hold** publishes into the catalogue only: the channel's route refuses it
  (`409`, `…/held`), and channels take its builds by their rules once the hold has passed (see
  [Publishing](publishing.md#holds-and-early-placement)).
- **Placing early**: `kista admin release place … -early`, or `"early": true` in `…/releases/place`,
  places a held version: a channel administrator's choice to take it sooner, recorded in its
  `release.place` event (`early`). Anyone else is refused.
- **Revoking a key** with `kista admin publisher key remove … -purge-held` (`DELETE
  …/publishers/{name}/keys/{id}?purge_held=true`, a tenant administrator's; a distribution admin's
  for a distributor's publisher) purges the entries
  published with it that are still held and placed nowhere, each with a `catalogue.purge` event;
  removing the publisher purges all of its held entries. A held entry a channel administrator placed
  early is kept, **revoked**: it stays where it is placed, but no rule, follow or promotion takes it
  again, and a placement by hand only with `early`. The answer (`200`, `{"purged": […], "kept":
  […]}`, each kept entry with how many of the tenant's channels hold it), the command's output and
  the removal's event (`purged`, `kept`) say what was purged and what was kept. A revoked entry is
  its bytes' for good: publishing the same file again, even with a new key, finds it revoked (`409`
  on a channel's route); a new version is needed, or a channel administrator places it early. An
  upload in flight when its key or its publisher is removed is refused (`401`), on a channel's route
  up to its release. A routine rotation, without `purge_held`, keeps everything (`204`).
- **Reading**: an upstream's and a publisher's answers carry `hold` (omitted for none; `upstream
  show`, `publisher list`); the catalogue lists each entry's `held` and `revoked` versions and shows
  each build's `state` (`held` or `available`), held, its `available_at`, and revoked, its
  `revoked_at` (`catalogue list`, `catalogue show`'s `HELD UNTIL` and `REVOKED`).

### Withdrawals

A distributor withdraws a version it published into a tenant's catalogue (spec 0033): a distribution
admin or a server administrator, with a reason, for a
[distributor's publisher](publishing.md#distributors-publishers), removed or not. A suspended
tenant is reached too (spec 0037): a distribution admin reads its distributors' entries and
withdraws there, so a tenant resumed later serves nothing withdrawn meanwhile.

```bash
kista admin -config /etc/kista/kista.yaml \
  catalogue withdraw acme publisher:enterest-ci geo_tools 1.4.0 -reason "CVE-2026-0001"
```

```text
POST /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw
{"reason": "CVE-2026-0001"}
```

- **The version, for good.** The version is withdrawn from the source in the tenant, whether the
  source holds builds of it or not (purged, collected, or not arrived yet: the answer then lists no
  builds): no build of it arrives again from that source, for any platform or DuckDB version (`409`,
  problem type `…/withdrawn`), and its builds are never purged nor collected.
- **Immediate.** In one transaction every build of the version becomes `withdrawn`, and every
  channel of the tenant holding a release of one gets its release version bumped: the next request
  serves it nowhere (DuckDB's `INSTALL` of it fails), and the index, bundles and feeds leave it out.
  A placement racing the withdrawal reads the build under a lock the withdrawal waits for: it either
  sees it withdrawn, or commits its release before the withdrawal bumps its channel.
- **Final.** A withdrawn build is never placed again (no rule, follow, promotion, nor by hand, even
  `early`: `409`, `…/withdrawn`); its releases stay listed, `"withdrawn": true`, are never made
  active, current or public again (`409`), and complete nothing (a `manual` rule holds no withdrawn
  version).
- **No automatic downgrade** (the owner's decision): after a withdrawal an `auto` rule or a follow
  does not go back to an older version by itself; the next newer version is placed as usual, or an
  administrator places an older one by hand. A rule's `newest` is the newest available version in
  the catalogue, which the pass takes only when it is newer than what the channel had, a withdrawn
  version included.
- **The tenant is told**: a `catalogue.withdrawn` event (the source, name, version, reason, the
  builds and how many channels it bumped). The answer to the distributor names the tenant, the
  version and the builds withdrawn, never the tenant's channels; withdrawing again answers
  `"already": true`.
- **Per source.** A withdrawal is the distributor's source's: the same body from another of the
  tenant's sources (its own publisher, an administrator's add) is the tenant's to serve, or to
  [block](publishing.md#blocking-a-body) for every source. A tenant's own sources need no
  withdrawal: blocks and yanks do it. A tenant administrator is refused (`404`).
- **Naming the source**: `publisher:<name>`; when two of the tenant's distributors' publishers,
  live or removed, had that name, it is ambiguous (`409`) and `publisher-id:<id>` names one (the
  distribution admins' catalogue lists such entries by `publisher-id:<id>`, with `publisher_id`).
- **Reading**: the catalogue lists each entry's `withdrawn` versions, and a build's `state` is
  `withdrawn` with `withdrawn_at` and `withdrawn_reason` (`catalogue list`'s `WITHDRAWN`, `catalogue
  show`'s `WITHDRAWN`); `release list` shows such a release's state with `(withdrawn)`. A withdrawn
  build, released or not, counts in its source's `catalogue.max_entries` and `catalogue.max_bytes`:
  its body is kept with it, and it is never purged, so a source full of withdrawn builds stays full
  (`429`, which says so); the distributor publishes through a new publisher then.

## Passthrough channels

A passthrough channel serves DuckDB's own core files with DuckDB's own core signature, so DuckDB can
**autoload** from the tenant. Every repository DuckDB is given by URL is core-typed, and autoloading
and a bare `LOAD` trust DuckDB's core keys only; a re-signed file would not autoload.

```bash
kista admin -config /etc/kista/kista.yaml version add v1.4.1 -kind release   # once per server
kista admin -config /etc/kista/kista.yaml channel create acme/core -kind passthrough
kista admin -config /etc/kista/kista.yaml channel versions acme/core -add v1.4.1
kista admin -config /etc/kista/kista.yaml upstream add acme duckdb \
  -kind duckdb-core -channel core -platforms linux_amd64 -extensions httpfs,spatial,iceberg
```

```sql
SET autoinstall_extension_repository = 'http://kista.example.com/acme/core';
```

- It is fed by `duckdb-core` mirror or bundle-mode upstreams only. Community and repository binaries
  would never load from it; pull-through is refused (its callers carry no token).
- Its releases are public and served without tokens, over `http` and `https`, `GET` and `HEAD`. A
  token is never looked at, and a miss is `404`. It has no keys and no `.well-known` file. Setting an
  upstream or a release of it private is refused.
- Nothing is published or promoted into it.
- DuckDB's built-in client speaks plain `http` only and follows no redirects, so an offline autoload
  starts from an `http://` repository; the listener must accept plain http (see
  [Configuration](configuration.md)).

**Tenant shadows.** Releasing into any signed channel of the tenant a build of a core name that is
not DuckDB's own (its original signature does not verify with DuckDB's core keys) records the name
in the tenant's **shadows**. The tenant's passthrough channels then answer `404` for that name, so
autoloading never silently picks DuckDB's build over the tenant's own. Only a tenant administrator
removes a shadow; yanking or blocking the replacement does not bring DuckDB's build back.

```bash
kista admin -config /etc/kista/kista.yaml shadow list acme
kista admin -config /etc/kista/kista.yaml shadow remove acme httpfs
```

API: `GET /api/v1/tenants/{t}/shadows`, `DELETE …/shadows/{name}`.

## Pull-through

A `pull-through` upstream is not run. When a request on a DuckDB route of a signed channel misses,
kista offers the cell to an in-memory queue, after the answer is decided, when:

- the caller holds a valid token with `install` on (channel, name). Anonymous callers and callers
  without the grant never cause a fetch, and every caller gets the answer it would get without
  pull-through;
- the cell is in an upstream's matrix: the request's DuckDB version and platform, and an allowlisted
  name or `*`. The versioned path never triggers a fetch;
- the cell is not in the replica's negative cache: every outcome but `released` keeps it from being
  fetched again for `upstreams.negative_ttl` (default 1h), a failure for a minute. A cell refused by
  a tenant's [limit](#a-run-at-a-limit) or a full catalogue is remembered as a refusal: its installs
  keep failing for up to `upstreams.negative_ttl` after room is made.

The client's request still misses; a later `INSTALL` finds the release. `*` is allowed only on a
pull-through upstream, without versions or `allow_reserved`, and not on `duckdb-core`. It never
matches an alias, a core name or a name another upstream of the channel lists, and it reserves no name in the tenant. On a `duckdb-community` upstream it matches community names (each fetch reserves
its name as community, and the name keeps being pulled through); on any other kind it matches no
reserved name. Limits per replica: 256 queued misses (32 per tenant), 4 fetches at once (and never
all of `upstreams.concurrency`) and
60 a minute per tenant, 8 waiting misses a caller, `upstreams.concurrency` fetches at once, 10,000
cells in the negative cache.

```bash
kista admin -config /etc/kista/kista.yaml upstream add acme community-pt \
  -kind duckdb-community -channel prod -platforms linux_amd64 -extensions '*' -mode pull-through
```

## Lazy mirrors

A mirror's matrix grows with every name, platform and DuckDB version: fifty of DuckDB's extensions on
nine platforms and three DuckDB versions are over a thousand bodies, most of which nobody installs. A
**lazy** mirror (`fetch: lazy`) lists what it may take as any mirror does, but its runs fetch only:

- the cells someone **asked** for ([The first request](#the-first-request)): a cell is (DuckDB
  version, platform, name), so a node asks for the build its own DuckDB version and platform need,
  and nothing else;
- every cell of an **eager** entry (`-eager`, `"eager": true`), fetched before anyone asks, as an
  eager mirror fetches it.

Each run keeps those cells current, conditionally (the ETag kept), as any mirror does. The other
cells are neither fetched nor recorded: `GET …/cells?outcome=not_asked` lists them, and `last_run`
counts them `not_asked`. **Sync** fetches the whole matrix once, the administrator's "fetch now",
without asking for anything, so later runs stay lazy and leave the cells only a sync fetched as they
are. A dry run, `sync -dry-run` included, covers what a scheduled run would: the asked cells and the
eager entries. A bundle export's `-sync` fetches the plan's cells besides them; a plan's cell another
fetch holds at the time fails the export, to be run again.

```bash
# DuckDB core extensions everyone may install, fetched when asked for; httpfs ahead of time
kista admin -config /etc/kista/kista.yaml upstream add acme core -kind duckdb-core -channel prod \
  -platforms linux_amd64,linux_arm64,osx_arm64,windows_amd64 -extensions httpfs,spatial,postgres_scanner -fetch lazy
kista admin -config /etc/kista/kista.yaml upstream extension add acme core httpfs -eager
```

- **Switching.** `upstream fetch <tenant> <name> lazy` keeps the releases there are and records an
  ask for each cell it fetched and found (a run in progress asks for its cells when it ends), so
  they stay current; `eager` makes the upstream due at once (its whole matrix).
- **Asks go** with their entry, platform, or the channel's DuckDB version.
- **One fetch of a cell at a time.** A lazy mirror's run takes each cell's lease
  (`kista/upstream/<id>/<version>/<platform>/<name>`) and skips, counting it `busy`, a cell another
  fetch holds.
- A channel with a [hold](#holds) asks nothing of its lazy mirrors: what they fetch would wait.
  A lazy mirror takes no hold, and a mirror with one is not made lazy.
- Only a mirror takes a fetch mode, and a passthrough channel takes no lazy mirror (its requests
  carry no token, so no one could ask); a lazy mirror lists its names (no `*`, as any mirror). The
  matrix limits count the whole matrix, lazy or not. Everything else is a mirror's: intake, licence
  keys, the name's source in the channel, reservations, visibility, credentials, usage and events.

**Compared with pull-through.** Both fetch on demand. Pull-through answers the first request `404`
and is never run, so a cell it took is never refreshed, and `*` may take any name. A lazy mirror
answers the first request `503` while it fetches, which DuckDB and kista retry; it runs on a
schedule and keeps every cell asked for current; it takes eager entries and **Fetch now**; and it
lists its names.

### The first request

When a caller holding `install` on (channel, name) misses on a DuckDB route of a signed channel, and
an active lazy mirror of the channel lists the name for that platform and DuckDB version, kista
records the ask (inserted when absent; its time renewed only when older than an hour, so retries
cost a read) and reads the cell's record and lease from the database, so every replica answers
alike:

| The cell | Answer on the plain path |
| --- | --- |
| a fetch holds its lease, or it was never fetched | `503` with `Retry-After: 5`: the build is coming |
| fetched, and answered something else (the upstream has no such cell, it was rejected or blocked, another source holds the name, the fetch failed or could not start) | `404`, as without the mirror |

The cell is then offered to the same worker pull-through uses, which fetches it in the background
under the cell's lease. DuckDB 2.0 retries a `503` by itself after its `Retry-After` (eight times at
most, each answer awaited 30 seconds), so the first `INSTALL` usually succeeds within about 40
seconds ([Using kista from DuckDB](duckdb.md#install-and-load)). A failed fetch is tried again by
the next run, or by an ask after a minute; a refused or missing one, or one a
[limit](#a-run-at-a-limit) refused, by an ask after `upstreams.negative_ttl`. A cell never fetched is answered `503` for two minutes after its ask; a
plain request after them opens them again and is offered again, so a dropped offer or a stopped
replica costs a client its retries, never more. A fetch that cannot start (the repository's keys
unreadable, kista's own client refused) records the cell `failed`. The versioned path records the ask but fetches nothing (a
repository serves its current build only) and answers as before. Anonymous callers and callers
without the grant ask nothing and get the answer they would get without the mirror.

Limits: first requests share pull-through's (the queue, 32 waiting misses a tenant, 4 fetches at
once, never every worker, and 60 a minute per tenant) and have, besides, 8 waiting misses a caller, so one downstream
kista's run cannot starve the tenant's other callers, and a host gate of their own on each replica,
one fetch a host per tenant, so a run holding the host's slots does not hold them back (the intake's
slots are shared). An offer dropped by a limit
keeps its ask: the next run fetches the cell.

**Mirroring a lazy channel.** Another kista mirroring a lazy channel, with a credential whose
principal holds `install` there, asks for each cell its run fetches: the first answer is `503`, and
its fetcher asks again after the `Retry-After`, ten times at most ([Runs and
schedule](#runs-and-schedule)), so it gets the build within its run when the fetch ends within about
50 seconds, and otherwise at its next run. Without a credential (a public lazy channel) it asks nothing, and gets only
what others asked for. A downstream kista of a lazy channel is best lazy too, or narrow in its
platforms: an eager mirror of a lazy channel asks for every cell of its matrix at every run.

## Bundle mode

A `bundle` upstream is for an air-gapped site. It is the same record as a mirror's (kind, prefix,
pinned keys, channel, platforms, allowlist, visibility, state), but kista never contacts its source:

- it is never run, and `sync` is `409`;
- a `repository` prefix is only the source's identity: its port is not checked against egress, and
  the source's `.well-known` file is not fetched when it is added (the pinned keys are checked when
  a bundle is imported, against the file the bundle carries);
- it takes no credential (`400`, at creation and on the credential route);
- a passthrough channel takes a `duckdb-core` one.

The site writes a plan (`kista admin bundle plan`): its bundle-mode upstreams' cells and, from spec
0014 phase 1b, its bundle-mode feeds with the bodies they offer and the last statement received
([Feeds for air-gapped sites](attachments.md#feeds-for-air-gapped-sites); exports carry statements and imports take them). A connected kista exports a bundle answering it
([Exporting a bundle](#exporting-a-bundle)), and the site imports it with
`kista admin bundle import`. [Air-gapped sites](deployment/air-gapped.md) walks through the whole
cycle, with bringing kista itself in. The upstream's record shows its mode and the last bundle
imported (`bundle_id`, `bundle_created_at`), over the API, in the console and in
`kista admin upstream show`.

```bash
kista admin -config /etc/kista/kista.yaml upstream add acme core -mode bundle \
  -kind duckdb-core -channel core -platforms linux_amd64 -extensions httpfs,spatial
```

### Exporting a bundle

A bundle is made on the connected side by a kista that mirrors the plan's sources: a gateway kista
of the organisation, or a provider exporting for its subscribers. It exports what its mirrors
already fetched and verified: it fetches no extension of its own (`-sync` runs its mirrors first).

```bash
# at the site: what its bundle-mode upstreams ask for, and what they hold
kista admin -config /etc/kista/kista.yaml bundle plan acme -o plan.json
# on the gateway: unsigned, then signed on a machine that holds the bundle key (recommended)
kista admin -config /etc/kista/kista.yaml bundle export gw -plan plan.json -unsigned -o acme.unsigned -sync
kista bundle sign -key bundle-key.pem -plan plan.json -in acme.unsigned -o acme.kbundle
# or signed as it is written, the key on the gateway
kista admin -config /etc/kista/kista.yaml bundle export gw -plan plan.json -key bundle-key.pem -o acme.kbundle
```

`<tenant>` is the gateway's tenant whose upstreams serve the plan; it need not be named as the
site's. The plan is validated first, and `-key` must be among its bundle keys, before anything is
read. For each of the plan's sources:

- **Where the files come from**: the tenant's active `mirror` or `pull-through` upstreams of the
  same source (the same kind and, for a repository, the same normalised prefix), the first by name
  that holds a servable release for a cell; or, when none matches, for a repository prefix
  `<serve.public_url>/<tenant>/<channel>` (an https `public_url`), the tenant's own signed channel. A source with neither fails as a whole ("not mirrored here"), and so does one
  whose `.well-known` file lists none of the plan's pinned keys (fetched through egress as a
  mirror's key check fetches it; an own channel's is its own). A passthrough channel is never a
  source.
- **The release decides**, not the cell record: each cell takes the release DuckDB would install
  from the channel now, among the plan's versions for the cell.

| On the gateway | In the bundle |
| --- | --- |
| a servable release from this source | its file: the stored body with the signature its source made (an own channel: a signature by a key the plan pins, the serving key first) |
| that release, whose body the site already holds (`body_hash` in the plan) | `unchanged`, no file |
| yanked (a block too) or deprecated | `rejected`, "withdrawn on the exporting kista" |
| a release of another origin (published, promoted, another source) | `rejected`, "not this source's build" |
| private, without `-private <prefix>` | `rejected`, "private on the exporting kista" |
| no release; the mirror refused the cell | `rejected`, with the mirror's reason |
| no release; missing, or never asked by a pull-through upstream | `missing` |
| releases, but none of the plan's versions for the cell | `missing`, "no release of the plan's versions on the exporting kista" |
| a stored body that cannot be read (missing, or marked corrupt) | `rejected`, "the body is not available on the exporting kista" |
| an own channel's release without a signature by a key the plan pins | `rejected`, "no signature by a key the plan pins" |
| outside every exporting upstream's channel versions, platforms or allowlist | `missing`, "outside the exporting matrix" |

Each file is checked as the import will check it (the signature with the plan's keys, the footer,
the versions); a failure is `rejected`. Nothing is re-signed. A cell carries `fetched_at`, when the
source last confirmed that body (the cell record's, kept fresh by unchanged runs; an own channel's
release time), its ETag, and `authenticated` when its release came through a credential or is
private on the gateway, so a release `-private` let out stays private at the site.

`-private <prefix>` (repeatable) names a repository source of the plan whose private releases may
leave: a credentialed or private mirror's, or an own channel's. DuckDB's kinds take no `-private`:
their private releases stay. `-sync` first runs the tenant's matching mirror-mode upstreams (a lazy
one over the cells the plan asks of its source) and waits for them; a run that fails or an upstream paused meanwhile stops the export, with no bundle. The bundle is written to a temporary file beside `-o` and linked into
place when complete, never over an existing file, readable by its owner only (`0600`: it is as
sensitive as the plan; the directory's file system must support hard links), so an interrupted export leaves nothing; it streams the stored files, read once to hash
and once to write, and needs no other disk. The command prints each source's counts, bytes, the
fingerprints its `.well-known` file lists (compare them with the site's pins to notice a rotation),
the first 20 cells not exported, and the bundle's SHA-256 as the import computes it. It exits `1`
when a source or a feed failed as a whole, after writing the bundle of the others. Exporting is a server
administrator's act, recorded as `bundle.export`. A plan discloses the site's configuration to the
gateway: its sources, allowlists, platforms, DuckDB versions and the bodies it holds.

**Statements** (spec 0014 phase 1b). For each of the plan's feeds, the export carries what the
tenant's export feeds of the same URL hold ([Feeds for air-gapped sites](attachments.md#feeds-for-air-gapped-sites)).
It contacts no feed.

On the gateway, subscribe to the source with `export` set
(`kista admin feed add <tenant> <name> -url … -credential … -key … -export`, or
`feed set … -export=true` on an existing feed), and let it run once: setting `export` resets its
cursors, and a feed that never ran has no keys file. The gateway receives statements only for the
bodies it holds itself, under its own grant at the source: a body it does not mirror, or a narrower
grant, gives the site nothing for it. The server administrator who runs the export decides what
leaves: a statement's signed `audience` travels with it, and the site applies its own `readers`.

- **Which feed**: an online feed of the URL with `export` set, whose kinds include every kind the
  site's feed accepts, and which kept a keys file. Without one the feed fails as a whole ("not
  subscribed here", "not kept for export", "accepts fewer kinds", "never ran"), the bundle carries
  the others, and the command exits `1`. With several, each body takes the one that read furthest.
- **Which bodies**: those the plan offers, and from the start those of the files the bundle brings
  for the feed's sources. Each body's range runs from the plan's cursor to what the gateway read.
- **What**: each statement as its issuer signed it. A stored document's content goes with it,
  unless its kind is one the site's feed does not take. A withdrawal goes as signed, without
  content. A refusal, a statement about a document removed here, one about a document never held,
  or one of a kind the site does not take goes without content, with a note saying why. The range
  ends just below a statement that does not verify with the plan's keys or names another issuer,
  or whose stored document cannot be read or does not match it: the site moves no further.
- **Limits**: past 100,000 statements or documents, a 16 MiB part of the manifest, or what is left
  of the manifest's 64 MiB, a feed fails as a whole ("over the bundle's limits for a feed"). A feed
  whose export setting or cursors changed while it was read fails too ("changed during the export"):
  export again. A document removed between reading and writing fails the whole export.
- The command prints per feed the export feeds it came from, the most recently successful one's
  status and last success (so a stale gateway shows), its counts and bytes.

**Licences** (spec 0021). A bundle carries licences only when the site's plan asks
(`bundle plan -licenses`, optionally `-license-holder <sub>`) and the exporter agrees
(`bundle export -licenses`). Without the flag none go, and the command says so; a plan that asks
for none gets none, with the flag or without. For each repository source the bundle carries:

- the licences the tenant holds for the names whose files are in the bundle (`fetched` cells) or
  that the site holds already (`unchanged`): a private release's name only under `-private`, as
  for its file;
- only those in force (valid, or in grace) whose key the server does not refuse
  (`licenses.refuse_keys`);
- only those of the holder the plan names, when it names one;
- at most 1,000, by name (the command says how many it left out).

The token goes as stored, and the manifest's signature covers it, with the plan's request. The
command prints each licence's name, holder and expiry: what leaves is the exporter's to see. When it carries
none although both asked, it says so (none in force for the bundle's names, of the plan's holder). A plan
that asks for licences, and a bundle that carries them, need a kista of the release that brought
licences in bundles, or a later one, on both sides:
an older one refuses them, as plans and manifests are decoded strictly. A bundle without licences
is the same document as before.

A plan's and a manifest's sources have a [package type](concepts.md#packages-and-package-types)
(`package_type`, spec 0034), left out for `duckdb_extension`, the type of every source today: an
extension bundle or plan is the document it was, and an older kista reads it. A source that names
`duckdb_extension`, or a type the importer does not know, is refused.

### Importing a bundle

```bash
kista admin -config /etc/kista/kista.yaml bundle import acme acme.kbundle [-upstream core] [-feed reviews] [-dry-run]
# from standard input, in a pod without a shell:
kubectl exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml bundle import acme - < acme.kbundle
# from the object store, after uploading with the store's own tools (they resume a broken upload):
kista admin -config /etc/kista/kista.yaml bundle import acme blob:default/imports/acme-2026-10-16.kbundle
```

A bundle is read once, front to back, and never extracted. The import:

1. **Checks the head.** The bundle's key must be pinned for the tenant in `upstreams.bundle_keys`,
   the manifest's signature must verify, the manifest must be valid, and its plan must name the
   tenant (`-any-tenant` imports a bundle planned for another). Any failure refuses the bundle
   before anything is taken in, and leaves no record.
2. **Selects the targets**: the tenant's active bundle-mode upstreams, or the `-upstream` ones,
   each matched to a source of the bundle by kind or by prefix. Naming an upstream that is paused,
   not in bundle mode or without a source in the bundle refuses the import, and so does a bundle
   that serves none of the tenant's upstreams or feeds (its bundle-mode feeds are targets too:
   Statements, below). A target another import or run is
   holding is `busy`. A target refuses as `stale` a bundle ordered (by `created_at`, then id)
   before the last one it imported, the same id with another manifest, and a bundle whose plan is
   older than the upstream. The last bundle is recorded on the target before its first release.
3. **Takes in each file** once: inflated into the spool, its size and hash checked against the
   manifest, then, for each target cell it fills, the intake of a fetched file above: the signature
   with the target's keys (DuckDB's for core and community; for a repository, those of the bundle's
   copy of the source's `.well-known` file whose fingerprints the target pinned), the footer, the
   file checks, the slot. A release becomes current as it is inserted, as in a run.
4. **Records the cells without a file**: `missing`, `rejected` (with the exporter's reason after
   `bundle:`), `unchanged` (kept only when the site holds that body; otherwise `missing`), and a
   file that never came, `failed`. Cells the bundle does not mention keep their records.

**No downgrade.** A cell that held a body takes another only if the bundle's copy was fetched at
its source later than the cell's (`fetched_at`); otherwise it is counted `stale` and keeps what it
holds. The cell's record keeps the newest body it held through `missing`, `rejected` and `failed`
outcomes, and a gzip entry or file that does not match the signed manifest (damaged media) is
`failed`, leaving the record as it was. Repeating an import releases nothing new (its cells are
`unchanged`), and importing the same bundle after an interruption resumes it.

**Stalls and limits.** An input that sends nothing for 5 minutes, or less than `upstreams.min_rate`
once 30 seconds of waiting have passed, ends the import; what completed stays (each release is
atomic). One file is spooled at a time: the spool (`blob.spool_dir`) needs about `blob.max_body`
free, beside what `kista serve` uses. The import ends at the tar's end-of-archive blocks.

**`imports/`.** `blob:<domain>/imports/<name>` reads an object under the domain's `imports/`
prefix, `<name>` matching `[a-z0-9][a-z0-9._-]{0,127}`. kista only reads there: it never writes,
lists or deletes `imports/`, and the storage collector never touches it. Remove the object after
the import. A broken read is resumed at its offset up to 5 times.

**Dry run.** `-dry-run` runs every check, spool included, and changes no release, cell or mark. It
records the targets' last run (counted `would_release`) and the import record.

**Records.** Each import, dry runs included, is recorded with its manifest, bundle key, input
SHA-256 (of every byte through the tar's end) and counts per target; a `running` record whose lease
expired shows as `failed (interrupted)`. Each target's run is recorded as a run's, with `bundle`
naming the manifest; the command prints a line per target and exits `1` when a target failed, was
busy or stale, a feed body was a gap, or the import stopped early (the bundle truncated, malformed or stalled; a lost
lease; the tenant's deletion). Importing and `bundle reset` are a server administrator's acts.

```bash
kista admin -config /etc/kista/kista.yaml bundle list acme
kista admin -config /etc/kista/kista.yaml bundle show acme <import id>
kista admin -config /etc/kista/kista.yaml bundle reset acme core   # clears the last-bundle mark
```

`bundle reset` lets a target take bundles older than the last one it imported again: the recovery
when a bundle carried a wrong `created_at`. It is audited (`bundle.reset`).

**Statements** (spec 0014 phase 1b). The import reads a bundle's feeds after its files, so a body
the bundle brings is held when its statements arrive. Its feed targets are the tenant's active
bundle-mode feeds whose URL is a feed of the bundle, or the `-feed` ones (with only one of
`-upstream` and `-feed`, the other kind takes no target). Each is taken under its feed's lease
(another import holding it makes it `busy`), and is `stale` as an upstream target is.

- **Each statement is judged here**, as an online run judges it: the signature with a key the feed
  pins (the bundle's keys file gives the public keys, used only by fingerprint), the issuer, the
  kinds, media type, title and size, the quotas, `supersedes`, withdrawals and the audience rule,
  and the body offered by the feed now. The exporting side's note is only recorded.
- **Content** comes from the bundle, checked against the statement. A statement whose content the
  bundle does not carry is refused, `bundle: no content (<note>)`.
- **A statement that does not verify** holds its body: nothing after it is applied.
- **Cursors** move only once the feed's entries were read whole: to the end of the body's range,
  but no further than a statement verified in this bundle or before. A damaged feed entry fails
  that target only, and its cursors stay.
- **A gap.** A body whose range does not start at the feed's cursor takes nothing (a plan written
  before a reset, or a cursor the collector removed): the target stays `imported`, and the
  command exits `1`. Write a new plan.
- A dry run checks each statement's signature, issuer, kind, title, size, media type and content,
  the bodies the bundle brings counted as held, and stores nothing (`would_store`,
  `would_withdraw`, refusals). Quotas and `supersedes` are judged by a real import only.
- **A statement out of seq order** within its body holds the body too.
- `bundle reset <tenant> -feed <name>` clears a feed's last-bundle mark and resets its cursors.
  Write a new plan after it: the bundle answering it takes the feed's history again (statements
  already stored are no-ops). Use it after a bundle that left statements out or lacked their
  content.
- **Setting up the site's feed.** Add it with the source's tenant URL, exactly as the gateway's
  export feed has it: statements name it as their issuer, and the import matches feeds by it. Pin
  the source's attestation keys (`-key`), not the bundle key, after comparing them out of band: the
  bundle's keys file is unsigned. Its offer must include the bodies the bundle brings: the default
  `upstreams` offer does; with `-channels`, name the bundle-mode upstreams' channels. A body it does
  not offer takes nothing (`not offered`).
  `kista admin feed add acme reviews -mode bundle -url https://<source>/api/v1/tenants/<t> -key sha256:<fp>`

The command prints a line per target, feeds as `feed:<name>`, and each feed's refusals.

**Licences** (spec 0021). After the files, when the bundle was read whole, the import takes each
licence the bundle carries, reading it once (one carried by two sources is one licence, for the
targets of both). A source the import takes no target of (`-upstream`, `-feed`) is passed over. For
each licence:

- **The targets** (spec 0035): every bundle-mode upstream the licence's sources fill in this import
  (those that took the bundle: not `busy`, `stale` or failed) that supplies the name, as a relaying
  upstream must ([Licensing](licensing.md#licences-in-a-tenant)); the licence goes into each one's
  own entry. None: refused, `not_supplied` or `no_target`.
- **The checks**: the licence is in force (`not_in_force`), of the name it is carried for
  (`ext_mismatch`), of the holder the plan named and of the holder this tenant expects, when either
  is set (`holder_mismatch`; [An expected holder](licensing.md#an-expected-holder)), and signed with a key live
  for the target's entry, as the bundle brought its builds (`kid_untrusted`) ([The keys a licence verifies against](licensing.md#the-keys-a-licence-verifies-against)).
  Each target is checked on its own: one may take the licence while another refuses it.
  Licensing off at the site refuses each (`off`).
- **Replacing**: the relay's rules ([Licensing](licensing.md#licences-in-a-tenant)), except that a
  bundle's licence never replaces one an administrator imported here (`superseded`). It is stored
  with source `upstream`, naming the target, and keeps the stored licence's visibility.
- **No revocation.** A bundle may be partial: a licence it leaves out stays, and ends at its `exp`
  plus grace through the expiry pass. A bundle-mode upstream has no relay. Removing the upstream
  removes its licences.
- **A dry run** stores nothing and counts what it would: it verifies against the keys recorded
  before it, so a first import's licences need the real import.

The command prints the licences' counts (`stored`, `replaced`, `unchanged`, `refused`, per target:
a licence two targets take counts twice) and each refusal with its rule and reason, and exits `1` when the licences could not be stored at all. The
import record's counts and the `bundle.import` event carry
them under `licenses`, and the console shows them with the import.

**Provenance and visibility.** A release from a bundle records its source's URL, the manifest's
ETag and fetch time, the verifying key, `bundle` (the manifest's id) and `imported_at`. A cell the
exporting side needed a credential for (`authenticated` in the manifest) is private, whatever the
upstream's visibility, and its provenance records `credential: "bundle:<prefix>"`: it can be made
public, or promoted as public, only where `upstreams.bundle_sources` allows that source for the
tenant ([Configuration](configuration.md)).

### Uploading a bundle

A server administrator who cannot run `kista admin` beside the server, nor write to its object
store, uploads a bundle over the API (or from the console: a tenant's Upstreams page in the server
scope, see [Console](console.md#screens)), and the server imports it. The upload comes in chunks, each
its own request, so an ingress's body limit and a broken connection are no obstacle; a broken
upload resumes with the chunks the server lacks.

```bash
T=https://kista.example.com/api/v1/tenants/acme
F=acme.kbundle
# create: the size, optionally the SHA-256 the export printed, and the import's options
ID=$(curl -sSf -X POST "$T/bundle-uploads" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"size\": $(wc -c < "$F"), \"sha256\": \"<hex>\", \"upstreams\": [\"core\"]}" | jq -r .id)
# cut the chunks by the chunk size the server answered (here the default 16 MiB), each sent with
# its SHA-256 as an RFC 9530 Content-Digest; a chunk sent again after a failure is no harm
CS=16777216
N=$(curl -sSf "$T/bundle-uploads/$ID" -H "Authorization: Bearer $TOKEN" | jq -r .chunks)
for n in $(seq 0 $((N - 1))); do
  dd if="$F" of=chunk bs=$CS skip=$n count=1 2>/dev/null
  curl -sSf --retry 5 -X PUT "$T/bundle-uploads/$ID/chunks/$n" -H "Authorization: Bearer $TOKEN" \
    -H "Content-Digest: sha-256=:$(openssl dgst -sha256 -binary chunk | base64):" --data-binary @chunk
done
# when every chunk is there
curl -sSf -X POST "$T/bundle-uploads/$ID/complete" -H "Authorization: Bearer $TOKEN"
curl -sSf "$T/bundle-uploads/$ID" -H "Authorization: Bearer $TOKEN"     # state, chunks received, import_id
curl -sSf "$T/bundle-imports/<import_id>" -H "Authorization: Bearer $TOKEN"
```

- **Creating** answers the upload's `id`, the chunk size it took (`bundles.chunk_size`, 16 MiB by
  default, or less if the request asks with `chunk_size`, from 1 MiB), the number of chunks and
  when it expires. Chunk `n` (from 0) is the bundle's bytes from `n × chunk_size`. `size` is at most
  `bundles.max_size`. A tenant has at most `bundles.max_uploads` uploads not yet imported, and the
  server `bundles.max_bytes` of uploads whose chunks it keeps (`409` past either).
- **A chunk** is exactly its bytes (`Content-Length` its size), with its `Content-Digest`. It is
  spooled, checked and stored once: the same chunk sent again is a no-op, another one for the same
  number is `409`. A replica takes at most 4 chunks of one upload at once, sharing the replica's
  `blob.max_ingests` (`429` past that, with `Retry-After: 1`: send it again; the problem type says
  which, `chunks-busy` for the upload's own chunks, `ingest-busy` for the replica's ingest slots). `GET` on the upload lists
  the chunks received, so a broken upload resumes with the ones missing. A long upload outlives a
  token: refresh it as the upload goes.
- **Complete** queues the import (`202`) once every chunk is there; a second call changes nothing.
  The administrator who completes it is the import's actor. A replica imports it, as
  `kista admin bundle import` does, with the bundle keys pinned for the tenant; the record is at
  `bundle-imports/<import_id>` (`import_id` shows once the record exists). Given a `sha256`, the
  chunks are checked against it first, and a mismatch imports nothing. An import stopped by a
  shutdown, by a chunk the store could not read for now, or whose targets were all busy, is queued
  again, three times at most (then it fails, `interrupted` or `busy`); `complete` on a `failed`
  upload queues it again.
- **A suspended tenant** takes no new upload, chunk or complete; its queued uploads wait until it is
  resumed. A deleted tenant's uploads go with it, their chunks swept by the collector.
- **Expiry**: an upload sending no chunk for `bundles.upload_ttl` (24 h) expires, at the latest
  `bundles.upload_max_age` (7 days) after its creation, and a failed one `upload_ttl` after its
  failure. `DELETE` aborts one not importing. The collector expires them, deletes the chunks of
  uploads imported, aborted or expired, and the objects under `uploads/` no chunk names.
- Each act is audited: `bundle.upload`, `bundle.upload_complete`, `bundle.upload_abort`, and the
  system's `bundle.upload_fail` and `bundle.upload_expire`; chunks are not.

## Private upstreams

A `repository` upstream whose channel serves private releases (another kista's private channel) needs
a token that the upstream's tenant accepts: a JWT from one of its issuers, with its audience, for a
principal it grants `install` (see [Access](access.md)). kista obtains it without static secrets where
the platform has an identity.

The credential also reads the upstream's licences for this kista, at `<prefix>/-/licenses`, when
licensing is on, and sends it this tenant's daily usage of the versions it supplied, at
`<prefix>/-/usage`, unless the upstream's `usage` is `off`
([Licensing](licensing.md#forwarding-usage-upstream)).

**Credentials are configuration, never data.** The operator names them in `upstreams.credentials`;
a tenant administrator picks one by name. The database and the API hold the name only.

```yaml title="kista.yaml"
upstreams:
  credentials:
    - name: hugr-acme                      # [a-z][a-z0-9-]{0,31}, unique
      tenants: [acme]                      # who may use it, or ["*"]; required
      prefixes: ["https://kista.hugr.example/hugr/"]   # https upstream prefixes it is sent to; required
      allow_public: false                  # releases it brings may be made public
      kind: client_credentials             # client_credentials | token_file
      token_url: https://login.microsoftonline.com/<tenant-id>/oauth2/v2.0/token
      client_id: 00000000-0000-0000-0000-000000000000
      scope: api://kista-hugr/.default     # or audience: …, as the IdP wants
      client_auth: azure                   # azure | file | key_file | secret
    - name: cluster-b
      tenants: [acme]
      prefixes: ["https://kista-b.internal.example/acme/"]
      kind: token_file
      token_file: /var/run/secrets/kista-b/token   # read at each fetch (a projected ServiceAccount token)
```

| Field | Meaning |
| --- | --- |
| `kind: token_file` | `token_file`: an absolute path, read at every fetch. A token file: owned by root or kista's user, any read bits; nobody but the owner may write it ([Files kista reads](security.md#files-kista-reads)) |
| `kind: client_credentials` | `token_url` (https), `client_id`, `scope` or `audience`, and `client_auth` |
| `client_auth: azure` | kista's own managed or workload identity (`azure.identity`) as the client assertion |
| `client_auth: file` | `assertion_file`: a projected token read at each request; a token file, as `token_file` |
| `client_auth: key_file` | `private_key_jwt` with `key_file` (ZITADEL's JSON, or PEM with `key_id`); `assertion_audience` the JWT's `aud` (default `token_url`). The key file is a secret file |
| `client_auth: secret` | `client_secret_file` or `client_secret_env`: the last resort. The file is a secret file |
| `allow` | CIDRs with ports where the token endpoint lives, beside `egress.allow` (`client_credentials` only) |

```bash
kista admin -config /etc/kista/kista.yaml upstream credentials       # names, tenants, prefixes
kista admin -config /etc/kista/kista.yaml upstream add acme hugr -kind repository \
  -prefix https://kista.hugr.example/hugr/prod -keys sha256:3f9a… \
  -channel prod -platforms linux_amd64 -extensions tresor -credential hugr-acme
```

- An upstream may name a credential only when its tenant is listed and its prefix starts with one of
  the credential's prefixes. `duckdb-core` and `duckdb-community` upstreams refuse a credential.
- The token is sent in `Authorization: Bearer` to that prefix only, never on a redirect, never in a
  log line, a cell, an event or an answer.
- A token is cached until a minute before it expires (or half its life when that is shorter), and
  fetched again once on a `401`; a refused token younger than 30 seconds is kept, so an upstream
  refusing every token costs one token request in 30 seconds.
- **Visibility**: a public upstream takes a credential only when the credential sets `allow_public`
  (`400` otherwise). Releases a credentialed upstream brings are private unless the credential, as
  configured at the time, sets `allow_public`. Making such a release public, or promoting it as
  public, is refused unless the credential, as configured now, allows it.
- **Failures**: the upstream's `401` or `403` is a `failed` cell with the detail
  `upstream.auth: the upstream answered <status>`; a removed credential gives
  `upstream.auth: the credential is not configured for this upstream` until an administrator picks another. The IdP refusing kista's own client
  (`invalid_client`) is logged as an error and fails the run.

`GET /api/v1/credentials` lists, to server administrators, each credential's name, kind, tenants,
prefixes and `allow_public`, never its settings. The same credentials serve feed subscriptions (see
[Attachments](attachments.md)).

## Egress

Every outbound request kista makes on a tenant's behalf (upstream fetches, issuer discovery and JWKS,
token endpoints) goes through one client with an SSRF guard:

- Only global unicast addresses are dialed, checked on the connection actually made, so DNS
  rebinding cannot slip past. Private, loopback, CGNAT, link-local, multicast, documentation and other
  special-purpose ranges are refused, including IPv4 embedded in IPv6 forms. Cloud metadata addresses
  (`169.254.0.0/16`, `fe80::/10`, `168.63.129.16`, `100.100.100.200`, `fd00:ec2::/112`) are refused
  even when allowlisted.
- `https` only, port 443 unless an allowlist entry names other ports; no user info in URLs; no
  redirects; the deployment's own `serve.public_url` host is refused.
- No proxy from the environment.

```yaml title="kista.yaml"
egress:                      # file-only
  allow:                     # private networks that may be reached: CIDRs, never host names
    - cidr: 10.20.0.0/16
      ports: [443, 8443]     # empty: 443
  proxy: http://proxy.internal:3128   # optional; kista resolves and checks the target itself
  ca_file: /etc/kista/internal-ca.pem # optional: trust only this CA bundle
  timeout: 10s               # per request for JSON fetches; 1s..60s
  allow_loopback_http: false # plain http to loopback; profile dev only
```

Through `proxy`, kista asks the proxy to `CONNECT` to the address it checked, so the proxy cannot be
used to reach a refused address. Errors are one message per class (refused, cannot connect, status,
too large) with the host only, so adding an upstream or an issuer is not a port scanner. Upstream
file downloads use `upstreams.fetch_timeout` and `upstreams.min_rate` instead of `egress.timeout`.

Upstream changes, runs, releases and rejected cells are recorded as events (`upstream.add`,
`upstream.change`, `upstream.run`, `upstream.release`, `upstream.rejected`, ...; see
[Observability](observability.md)).
