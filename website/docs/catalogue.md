---
title: The catalogue
---

# The catalogue

Every build that arrives in a tenant (a publication, a mirror's run, a pull-through or lazy fetch, a
bundle import, an administrator's add) lands in the tenant's **catalogue** first, as a build of its
**source**. Names may coincide: DuckDB's community `h3`, a distributor's `h3` and the tenant's own
`h3` are three entries side by side. A channel serves each name from one source, and **takes** that
source's versions from the catalogue by its administrators' rules. The catalogue itself is never
served to DuckDB.

This page walks through what a tenant's administrators do with it. The reference is in
[The catalogue and a channel's rules](upstreams.md#the-catalogue-and-a-channels-rules),
[Holds](upstreams.md#holds), [Withdrawals](upstreams.md#withdrawals) and
[Publishing into the catalogue](publishing.md#publishing-into-the-catalogue); the design is
[spec 0033](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0033-catalogue/spec.md).

## The parts

| Part | What it is |
| --- | --- |
| **Source** | where a build came from: `upstream:<name>` (a mirror of DuckDB's core or community repository, another repository, a bundle's), `publisher:<name>` (the tenant's CI, or a [distributor's publisher](publishing.md#distributors-publishers)), or `tenant` (its administrators' adds and its principals' publications). To a distribution admin and a server administrator, a distributor's publisher is `publisher-id:<id>` where another distributor's publisher, live or removed, had its name (and that form always names it for them). |
| **Entry** | a build of a source: name, version, platform, DuckDB version or C API, verified once, at arrival, against its source's signature. A version is immutable within its source in the whole tenant (the tenant's own source: within each channel). |
| **Rule** | a channel's choice for a name: the source it serves it from, and a mode (below). |
| **Follow** | a channel taking a whole source automatically: every name of it the channel has no rule for. |
| **Hold** | a time a source's new versions (a publisher's, an eager mirror's), or a channel's automatic placements, wait before any channel takes them automatically. |
| **Withdrawal** | a distributor taking back a version it published: served nowhere in the tenant from then on. |
| **Metadata** | an entry's icon (a small square PNG) and summary (one line of plain text), the same for every version: [below](#give-an-entry-an-icon-and-a-summary). |

A rule's mode says what the channel takes:

| Mode | The channel takes |
| --- | --- |
| `auto` | each new version of the source, once its holds have passed |
| `manual` | the versions an administrator places, and the later builds of those versions (another DuckDB version or platform) |
| `follow` | a rule a follow wrote: `auto` while the channel follows the source, `manual` once it no longer does |
| none | a rule the name's first release wrote: the channel serves what lands in it (a mirror's runs into its own channel, publications into it) |

The tenant's own source, `tenant`, is never `auto` and never followed: it is everyone who may
publish in the tenant, so taking it automatically would let a grant on `dev` reach `prod`.

A version in the catalogue is **available**, **held** (until its hold ends), **revoked** (held when
its publisher key was revoked, and placed early before: it stays where it is, but nothing places it
automatically again) or **withdrawn** (by its distributor, with a reason: served nowhere).

## Common tasks

The commands below are `kista admin -config <file> …`; each has its API route, and all but a
source's hold (`upstream hold`, `publisher hold`) and `catalogue add` have a place in the
[console](console.md) too (the console shows those holds, and sets a channel's).

### Mirror a DuckDB extension into a channel

Add an upstream with the name in its allowlist and the channel as its home channel, as
[Upstreams](upstreams.md) describes. Its runs keep the channel current: the first release writes the
channel's rule for the name, without a mode. Another channel takes the same mirror with a rule of its
own:

```bash
kista admin channel source acme/nightly h3 -set upstream:community -mode auto
```

Two limits remain: a mirror's matrix follows its home channel's DuckDB versions (another channel
gets the builds of the versions both serve), and a lazy or pull-through upstream fetches on a miss in
its home channel only. An upstream without a home channel is a follow-up.

### Take a distributor's builds

A distributor (such as Enterest) publishes into the catalogue only, through its publisher in the
tenant, and reaches no channel by itself. Its builds are held for at least the tenant's minimum hold
for distributors' publishers, one day unless you change it (below). A channel takes its builds by a
rule per name, or by following it:

```bash
kista admin channel source acme/prod geo_tools -set publisher:enterest-ci -mode auto
kista admin channel follow acme/nightly publisher:enterest-ci
kista admin channel follow acme/nightly          # the follows, and the names each skips
```

A follow does not take a name the tenant uses from another source (another source has builds of it,
or a channel serves it from another source), a reserved name, or a name two of the channel's follows
offer: each is listed as skipped, with the reason, after the placement pass's next look at the source
(a new build of it, or hourly). A rule naming the source takes such a name.

What the distributor reads of the tenant's catalogue is its own: a distribution admin reads its
distributors' publishers' entries, and the publisher itself reads its own entries with a signed
request (spec 0037: `GET …/catalogue`, `…/catalogue/publisher:<its name>/extensions/<name>`), so
the distributor's publishing worker checks its versions, holds and withdrawals with its key alone.
Neither reads the tenant's rules, follows or where a build is placed
([Distributors' publishers](publishing.md#distributors-publishers)).

### Choose versions by hand

Give the name a `manual` rule and place the version you want:

```bash
kista admin channel source acme/prod geo_tools -mode manual
kista admin channel source acme/prod              # each name's current and newest version
kista admin release place acme/prod -name geo_tools -source publisher:enterest-ci -version 1.4.0
```

Placing needs `promote` on the name in the channel, which `admin` does not imply. Its builds for
DuckDB versions or platforms that arrive later follow it. A held version is placed only with `-early`,
by a channel administrator; the choice is recorded in the `release.place` event.

### Roll back, or stay on a version

Yank the bad release ([Changing a release](publishing.md#changing-a-release)): the previous one is
current again. Under an `auto`
rule the yanked version is not placed again, and the next version is. To stay on a version, make the
rule `manual`.

### Switch a name's source

```bash
kista admin channel source acme/prod geo_tools -set upstream:org
```

The new source's releases already in the channel are served at once, and with `auto` its newest
available version is placed by the next pass. The other source's releases stay stored and unserved.

### Hold

A source's hold (`kista admin publisher hold`, `kista admin upstream hold`) keeps its new versions
out of every channel for a while: time to look at a build, or to revoke a stolen key before an
automatic placement serves it. A distributor's publisher's hold is the distributor's, but its
builds are held at least for the tenant's **minimum hold for distributors' publishers** (spec 0037),
which the tenant's administrators set and a distribution admin cannot lower: one day by default,
`0` for none.

```bash
kista admin tenant distributor-hold acme       # prints it
kista admin tenant distributor-hold acme 2h    # the console's Publishers screen sets it too
```

Raising it holds the distributors' entries held now longer; lowering it applies to builds arriving
from then on ([The minimum hold](access.md#the-minimum-hold-for-distributors-publishers)). A tenant
that wants longer for one channel holds that channel:

```bash
kista admin channel hold acme/prod 2d
```

A channel's hold applies to its automatic placements only, and changing it applies at once.

### Purge a build placed nowhere

```bash
kista admin catalogue list acme -name maps
kista admin catalogue show acme tenant maps       # each build's id, state and where it is released
kista admin catalogue purge acme <build id>
```

A build that was never released is deleted, and its slot is free again (no one could have installed
it). A withdrawn build is never purged.

### Withdraw a distributor's version

A distribution admin or a server administrator, never a tenant administrator:

```bash
kista admin catalogue withdraw acme publisher:enterest-ci geo_tools 1.4.0 -reason "CVE-2026-0001"
```

Every channel of the tenant stops serving it at once, and no build of the version arrives from that
source again. The tenant's administrators are told by a `catalogue.withdrawn` event. An `auto` rule
does not fall back to an older version by itself: a newer version, or a placement by hand, is needed.

### Give an entry an icon and a summary

An entry (a source's name, of a package type) may carry an **icon** and a **summary**, shown in the
catalogue and on the entry's page. They belong to the entry, not to a version: a new version keeps
them, and they outlive the entry's builds (purged or collected), so a re-publication finds them again.
Only the tenant's deletion removes them.

Who sets them depends on the entry's source:

| Source | Who writes the metadata |
| --- | --- |
| the tenant's own (an upstream, the tenant's own publisher, `tenant`) | a tenant-wide administrator, or a server administrator |
| a [distributor's publisher](publishing.md#distributors-publishers), live or removed | its distribution admin or a server administrator; never the tenant's administrators, who read it |
| any publisher's own entries | the publisher itself, by a [signed request](publishing.md#an-entrys-icon-and-summary) |

```bash
kista admin catalogue meta acme tenant maps                    # the metadata, and who set it
kista admin catalogue meta acme tenant maps -summary "Vector tiles for the maps team" -icon maps.png
kista admin catalogue meta acme tenant maps -clear-icon        # the summary stays
```

A field not named keeps its value. `catalogue show` prints the summary and the icon's hash and size
after its table.

- **The summary** is plain text on one line: at most 160 characters (after Unicode NFC), printable,
  no control, format or line-separator character (bidirectional controls, zero-width characters,
  tag characters), none of the invisible fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, at most
  4 combining marks after a character. It is never markup and never a link: the console renders it as
  text.
- **The icon** is a square PNG, 64 to 512 pixels a side, at most 256 KiB, without text, Exif,
  colour-profile or animation chunks ([the rules](publishing.md#an-entrys-icon-and-summary)). It is
  kept as sent in the tenant's storage domain and counts toward its source's
  `catalogue.max_bytes`: a publisher's or a distribution admin's write beyond it is refused (`429`,
  `catalogue-full`); a tenant's or a server administrator's is not bounded.
- Every write that changes something is a `catalogue.meta` event in the tenant, naming the writer
  and, for a signed write, the key.

## In the console

- **Catalogue** (tenant administrators) lists every source and name with its icon and summary, its
  versions, each marked held, revoked or withdrawn, and the channels whose rules take it, filtered by
  a name prefix and a source. An entry's page shows its icon and summary, with **Edit…** (the
  summary, a PNG file, or removing the icon) on the tenant's own sources, never on a distributor's
  entry, and each version's builds: platform, DuckDB version or C API, state (held
  until when, withdrawn and why, revoked), visibility and the channels it is placed in. **Place…**
  places a version into a signed channel where the caller holds `promote` on the name (privately, not
  current, or early for a held version, a channel administrator's); **Purge…** deletes a build placed
  nowhere; a server administrator withdraws a distributor's version with **Withdraw…** and a reason.
- A channel's **Sources** tab (its administrators) shows each name's source, the rule's mode, the
  version the channel holds and the newest available one, and the placement pass's last outcome;
  **Change…** switches the source or the mode, **Place…** places a version by hand, **Clear…**
  removes the rule. Beside it are the channel's **hold** and its **Follows**, each with the names it
  skips and why, followed and unfollowed there.

See [Administration console](console.md#screens).
