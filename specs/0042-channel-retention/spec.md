# Spec 0042: Version retention in channels

- **Status**: draft
- **Date**: 2026-11-01
- **Author**: vgsml, Claude

## Summary

A channel keeps every release it ever made until an administrator yanks and purges it one by one.
This spec adds **retention**: a rule, the tenant's default and overridden per channel, that keeps
the newest N versions of each name per DuckDB version the channel serves, always its current ones,
and the versions an administrator **pins** (per DuckDB version, with a reason). A pass **retires**
the others: a retired release is not served, and an administrator can **restore** it, until a
**grace** after which the pass purges it as an administrator's purge would (spec 0016). `retired` is
a new release state, because a yank is final. An administrator can also retire a whole DuckDB line
that is no longer built. A purged release's build then goes to the collector after `gc.grace`, unless
something else holds its body. Plan item K2; the owner's mockup's Retention screen. The owner's open
questions are at the end (Q-E to Q-H).

## Problem

- **Channels only grow.** An `auto` rule or a follow places every new version (spec 0033); a mirror's
  runs add each new cell. Old versions stay served for good, and so do their bodies. Over a year a
  nightly channel holds hundreds of versions nobody installs.
- **The only way out is a yank, which is final** (spec 0006: "`release yank` (final)"): a yank means
  "do not run this", nodes learn of it from the index (spec 0007), and a yanked release is never
  active again. Housekeeping is not a security event, and an administrator who cleaned up too much
  has no way back.
- **The purge is by hand** (spec 0016 phase 2, the owner's choice of 2026-10-09: no automatic
  retention), one release at a time, after a yank.
- **DuckDB lines end.** A channel serving `v2.0.0` and the LTS line `v1.5.2` keeps building only for
  `v2.0.0`. The LTS line's old versions must stay while LTS nodes run them, then go together.

## Design

### Terms

| Term | Meaning |
| --- | --- |
| **Line** | a DuckDB version the channel serves (spec 0003's channel versions). A `cpp` or `c_struct_unstable` release serves one line (its slot's DuckDB version); a `c_struct` release every line whose C API maximum accepts it (spec 0006, `duckdb_version_c_apis`). |
| **A version in a line** | the active and deprecated releases of one (source, name, version) that serve the line, on every platform: a version's builds count once, whatever the platforms. |
| **Current** | per line and platform: the release the flat path serves (spec 0006: the active release with the highest `seq`) in the full view and in the public view; and, for each source the channel's rule does not select for the name, that source's active release with the highest `seq` (the serving snapshot holds only the selected sources, `internal/release/resolve.go`, so this is read apart), so a switch back to it finds its current. A line may have several current versions: a channel serves the older version on a platform until the newer one's build for it arrives. |
| **Kept** | in a line: its current versions, its newest N versions, and the versions pinned for it. |

### The rule

- `keep`: N, a whole number at least 1 that fits its column (`400` above it); `grace`: a duration
  of whole seconds, at least 0 (`0` purges at the next pass after the retirement: no time to
  restore), at most the ceiling `limits.ceilings.retention_grace` (default `2160h`, as `max_hold`;
  spec 0038's amendment). "Off" is no rule at all.
- **The tenant's default** (`PUT …/tenants/{t}/retention`), for every signed channel without its
  own; **a channel's own** (`PUT …/channels/{c}/retention`): per field, its value, `"default"` (the
  tenant's), or for `keep`, `"none"` (no retention in that channel, whatever the tenant's default).
  A tenant without a default and a channel without its own keep everything, as today (Q-H).
- **Each field in force on its own**: a channel may set `keep` and take the tenant's `grace`. The
  answer's `in_force` gives each field its value and its `from` (`channel` or `tenant`). **A field
  with no value anywhere means no rule in force**: the pass retires and purges nothing in that
  channel (fail closed), and `in_force` says which field is missing.
- **Passthrough channels** take no rule, no pin, no restore and no line act (`400`): they serve
  DuckDB's core byte-identical, by their upstream's cells (spec 0009).

### What the pass retires

For each (source, name) with releases in the channel, each line separately: the versions in the
line, ordered newest first by the version's **first release in the line** (`MIN(created_at)` of its
releases serving the line; Q-H); the kept ones as above; the rest are **retirable in the line**. A
release is **retired** when it is retirable in every line it serves (Q-G):

- a `c_struct` build serving `v1.5.2` and `v2.0.0` stays while either line keeps its version, so
  `v2.0.0` nodes can still install it by version while `v1.5.2` keeps it;
- a release serving no line of the channel (its DuckDB version was removed from the channel) is left
  alone: it serves nothing and keeps nothing; retiring it is the line act below;
- yanked releases (Q-H), withdrawn entries' releases (spec 0033: never purged) and releases already
  retired are not counted and not touched;
- each source is counted on its own: the releases of a source the rule no longer selects stay
  stored and unserved (spec 0031), and keep their current and newest N, so a switch back serves them
  at once.

All the releases of one (name, version) the pass retires in a channel are retired together (one
event, below). Then the pass **purges** each release retired for longer than the grace in force
(`retired_at` plus the channel's grace at the pass), unless its version is pinned in a line it
serves, through spec 0016's purge: the slot stays taken (`purged_slots`), the build loses the
reference and its `used_at` is set to the purge. The purge's guard applies: while `gc.interval` is 0
(the operator's declaration that not every replica runs the version that knows `purged_slots`), the
pass retires but purges nothing, and the plan says so.

**What happens to the body.** A purged release's build is not handed to the catalogue's retention:
it stays as it was (not `landed`), and `KeepLanded` acts on landed builds only. The collector (spec
0016) deletes the build after `gc.grace` unless another release references it, then the body row
unless another build (another release's, or a landed catalogue build) references the body, then the
stream a grace later.

### Pins

A pin keeps (name, version) in one line, or in every line, whatever the rule: `POST
…/channels/{c}/retention/pins {"name", "version", "duckdb_version"?, "reason"}` (no
`duckdb_version`: every line; a reason of 1 to 400 bytes of printable text, a format bound).

- The version must have a release in the channel (any state but purged), and `duckdb_version`, when
  given, must be a line one of its releases serves (`400` otherwise).
- A pin keeps an active or deprecated version from retirement, and a **retired** version from the
  purge; it does not restore a retired version (the console offers both).
- A purge of one of the version's releases (by hand) drops the pin only when no release of the
  version is left in the pinned line (or, for every line, in the channel).
- A pin names no source: it keeps the version from any source.
- **Bounded without a ceiling**: a pin needs a release of its version, and its line must be one that
  release serves, so a channel's pins are at most its releases' (name, version, line) triples plus
  one per (name, version); they are bounded by what the channel holds.
- `GET …/retention/pins[?name=&cursor=&limit=]` lists them with who and when; `DELETE
  …/retention/pins/{id}` unpins (an id of another channel is `404`).

### The retired state

- `releases.state` gains `retired` (spec 0006's `active | deprecated | yanked`);
  `releases.retired_from` keeps the state it came from (`active` or `deprecated`) and
  `releases.retired_at` when (the grace's clock: `state_changed_at` is also written by `SetReleaseSeq`
  and `SetReleaseVisibility`).
- **Not served**: neither the flat nor the versioned path resolves to it, nor do the index's
  `serves`, the feed's offers (spec 0011), a bundle's export (spec 0014) or pull-through's selection.
  DuckDB at the pin asking for a retired version gets the channel's **missing** answer, the same
  bytes as for a private, yanked or never-existing release: kista's own rule that a missing answer
  says nothing (spec 0001's "Visibility and existence", spec 0006). DuckDB would not show a reason
  anyway over https (httpfs gives a generic error, without the status or the body;
  `extension_install_dynamic.cpp:145-163` at `d54cbbe`), though over plain http it shows the status
  and keeps the body in `HTTPException.extra_info` (`:312-317`). So the mockup's "a message saying
  it was retired in prod" is the index's, below, and the console's.
- **The index** (spec 0007) shows it as it shows a yanked release, to a caller whose view includes
  it: `state: "retired"` in `extensions/{name}`, with `would_serve`; `…/versions/{v}` answers
  `status: "retired"` when a retired release in view would have served the path (and no active or
  deprecated one does), so a node's agent knows the version is gone from this channel, not that it
  must not run it.
- **Kept signed.** A retired release keeps its signatures and the re-signer keeps adding the serving
  key's (spec 0006's invariant covers every release that is not yanked), so a restore is a state
  change only.
- **Nothing revives it by itself.** An `auto` rule, a follow or a mirror's home rule never places
  its version again in its cells (it is a release the cells had, spec 0033); a manual rule does not
  complete it (as a yanked version). **Every insert into its slot is refused** with `409`, problem
  type `retired` ("restore it"): a promotion into it, a publication (spec 0008), a placement by
  hand, a first arrival, a bundle import. `checkSlot` (`internal/release/release.go`) answers a
  same-body release as the existing one today; for a retired release it refuses instead. A mirror's
  run, a lazy fetch and a bundle import's cell record the outcome `retired` (as `yanked`) and leave
  it. A promotion from a retired release is refused (it is not active nor deprecated). A licence key
  a retired release declares is not live through it (spec 0021).
- **Changes**, each under the channel lock with the snapshot bump, `If-Match` as every release
  change, by the release's extension administrators:
  - `POST …/releases/{id}/restore`: a retired release, back to `retired_from`, keeping its `seq` (so
    it is current again only if nothing newer is); `retired_from` and `retired_at` are cleared. A
    withdrawn entry's release is not restored (spec 0033 B: never active again);
  - `POST …/releases/{id}/yank` on a retired release yanks it (final, as from any state);
  - `DELETE …/releases/{id}` (purge) takes a retired release too, at any time;
  - a block (spec 0008) yanks a retired release with the body, as any release that is not yanked.
  - Retiring one release by hand is not offered (the mockup has no such action; a follow-up).
- **An upstream's revocation** (spec 0039) yanks a retired release like any other, recording
  `prior_state = retired` in `upstream_yanks`; `retired_from` and `retired_at` are kept through the
  yank. Lifting the revocation returns the release to `retired` with `retired_at` set to the lift: a
  new grace, so the release is not purged at the next pass right after a long revocation.
- `GET …/channels/{c}/releases?state=retired` is accepted (`internal/api/channels.go`).

### Retiring a line

`POST …/channels/{c}/retention/lines {"duckdb_version": …, "name"?: …}` (a channel administrator):
retires every active or deprecated release of the name (or of every name) that serves that line and
that no other line of the channel keeps, current and pinned versions included (pins for that line
are removed; a pin for every line keeps its version in the other lines).

- The DuckDB version need not be served by the channel any more, so the releases a removed line
  left behind can go too: which releases serve it is read from the version's own C API maxima
  (`duckdb_version_c_apis`) for `c_struct` releases, and from the slot (the DuckDB version) for
  `cpp` and `c_struct_unstable` ones.
- The answer lists the releases retired and those kept by another line. Event `retention.line`
  (subject `channel:<c>`; data `duckdb_version`, `name` when given, `retired` (how many releases),
  `kept` (how many another line keeps), `pins_removed`), and a `release.retire` per (name, version)
  with `by: "line"`.
- Afterwards the line keeps nothing for those names until a new release arrives for it; removing the
  version from the channel (spec 0003) stops serving the line at all, including `c_struct` builds
  another line keeps.

"No longer built" is the console's label (the line's newest version is older than the name's newest
in another line); kista needs no notion of it: the current version of every line is always kept, so a
line nobody builds for keeps its current until an administrator retires it.

### The pass

- Every `retention.interval` (configuration, default `24h`; `0` turns the scheduled pass off), per
  signed channel with a rule in force, of active tenants (suspended and deleting tenants skipped),
  under a lease per channel (`kista/retention/<channel id>`), in batches with the lease renewed before
  each write, as the placement pass does. A channel's look records `channels.retention_checked_at`.
- Each retirement and purge is its own short transaction under the channel lock, re-checking under
  the lock what the look found (the releases' state and version, current, pins, the rule), so a
  release made current, pinned or restored meanwhile is left. Idempotent: a pass interrupted
  anywhere is finished by the next one.
- A failure on one version is logged and counted, and the pass goes on; the channel's summary event
  `retention.run` (subject `channel:<c>`; `retired`, `purged`, `failed`), written only when it changed
  something or failed.
- The actor is `system:release-retention` (`system:retention` is the events pruner's, spec 0010).
- `kista admin retention run [<t>[/<c>]] [-apply]`: a pass now, a dry run unless `-apply` (the CLI is
  a server administrator).

### The plan

`GET …/channels/{c}/retention/plan[?cursor=&limit=]` (the channel's administrators and the tenant's
auditors): what the next pass would do, computed as the pass computes it, paged by (name, version):

- `retire`: each (name, version) with its lines, platforms, release ids, bytes (`builds.body_len`,
  spec 0038), and why (the kept versions of its lines, or the line that keeps it);
- `purge`: the retired releases past their grace, with their bytes, and those a pin keeps;
- `retired`: in the grace, each with the time it will be purged;
- `next_run` (`retention_checked_at` plus the interval, or none) and the rule `in_force`.

Per body, **`shared`**: whether the purge would free it in the tenant, that is whether another
release of the tenant's channels, or a landed catalogue build, references the same body. `shared_by`
names the holders only among the channels the caller administers, and `catalogue` for a landed
build; holders in other channels are counted, not named. Only the tenant's own rows are read, never
another tenant's in the storage domain, and bodies the collector would keep for another tenant are
not this answer's.

### API and CLI

| Route | Who |
| --- | --- |
| `GET`, `PUT /api/v1/tenants/{t}/retention` `{"keep": 3, "grace": "720h"}` or `{"keep": "none"}` | tenant administrators (`GET` also auditors) |
| `GET`, `PUT …/channels/{c}/retention` `{"keep": 5|"default"|"none", "grace": "168h"|"default"}`; the answer adds `in_force` (each field's value and `from`), `checked_at` and `next_run` | the channel's administrators (`GET` also auditors) |
| `GET …/channels/{c}/retention/plan` | the channel's administrators, auditors |
| `GET`, `POST …/channels/{c}/retention/pins`, `DELETE …/pins/{id}` | the channel's administrators (`GET` also auditors) |
| `POST …/channels/{c}/retention/lines` | the channel's administrators |
| `POST …/extensions/{name}/releases/{id}/restore` | the extension's administrators |

`PUT`s answer the `GET`'s body with an `ETag`, take an optional `If-Match`, write nothing and no
event when nothing changes, and otherwise `retention.set` (subject `tenant:<name>` or
`channel:<c>`; `before`, `after`, `in_force`). Pins write `retention.pin` and `retention.unpin`
(subject `channel:<c>/ext:<name>`; `id`, `version`, `duckdb_version`, `reason`).

Release events: `release.retire`, one per (name, version) per channel (subject
`channel:<c>/ext:<name>`; data `version`, `releases` (the ids), `lines`, `by`: `retention` or
`line`); `release.restore` (subject the release, as `release.yank`); a pass's purge is
`release.purge`, which gains a `reason` field (`retention`; an administrator's purge has none).
`audit.SubjectForm` gains the forms of `retention.*` and `release.retire`; every new kind joins the
events registry and `TestCoverage`.

CLI: `kista admin tenant retention <t> [-keep N|none] [-grace d]`, `kista admin channel retention
<t>/<c> [-keep N|default|none] [-grace d|default]`, `kista admin channel retention-plan <t>/<c>`,
`kista admin channel pin|unpin …`, `kista admin channel retire-line <t>/<c> <duckdb-version> [-name
n]`, `kista admin release restore <t>/<c> <id>`, `kista admin retention run`. `release list` shows
`retired`.

Configuration:

```yaml
retention:
  interval: 24h   # the scheduled pass; 0: off (kista admin retention run only)
limits:
  ceilings:
    retention_grace: 2160h   # the longest grace a tenant or a channel may set
```

### Data model

One migration (the next free number), in the three dialects, with `min_reader` raised to it: an
older replica does not know the `retired` state (and would refuse or misread it), and its reaper does
not know `retention_pins`, which references `channels` (as specs 0032 and 0037 found):

```text
releases          state CHECK widened to (active, deprecated, yanked, retired)
                  + retired_from  text NULL       -- active | deprecated, while retired (kept through an upstream's yank)
                  + retired_at    timestamp NULL  -- the grace's clock
tenants           + retention_keep int NULL, retention_grace_seconds bigint NULL   -- NULL: not set
channels          + retention_keep int NULL     -- NULL: the tenant's; 0: none here; >= 1
                  + retention_grace_seconds bigint NULL                           -- NULL: the tenant's
                  + retention_checked_at timestamp NULL
retention_pins    id, channel_id (fk channels), name, package_version, duckdb_version ('' for every line),
                  reason, pinned_by, pinned_at; unique (channel_id, name, package_version, duckdb_version)
```

- The `CHECK` on `releases.state` is unnamed (migration 0003): PostgreSQL drops
  `releases_state_check` by its generated name; SQL Server finds its generated name in
  `sys.check_constraints` and drops it in dynamic SQL; SQLite rebuilds `releases` under
  `-- +foreign_keys_off` (spec 0033 A1 part 1's procedure; `release_signatures` references it), as
  migration 0034 did, keeping its indexes.
- `upstream_yanks.prior_state` takes `retired` too.
- Tenant deletion (spec 0019) reaps `retention_pins` by channel, before `channels`, and
  `TestReapCompleteness` covers it; the columns go with their rows.

### The console (the mockup's Retention screen)

A channel's **Retention** tab: the rule (the tenant's default or the channel's own, `keep` and the
grace, each with where it comes from), what is always kept, the defaults per channel (each channel's
`in_force`), the pins (Pin and Unpin, with the reason), per name and line the current, kept and
retired versions (from the channel's releases and the plan), "Retire the line…", and the plan's
three lists (Retire, Purge, Retired in the grace with Restore). A release's page offers Restore. The
pipeline (spec 0043) shows each channel's `in_force`.

## Security

- **Consent.** Retirement and purges are made only under a rule an administrator set (the tenant's
  default by a tenant administrator, a channel's own by its administrators), or by an administrator's
  act; the pass acts as `system:release-retention`, audited per version.
- **Pushing versions out.** A principal holding only `publish` on a name in a channel with an `auto`
  rule, or publishing into it directly, can make N new versions and so push older ones out of the
  newest N, retiring them at the next pass and purging them after the grace. Mitigations: the current
  versions are never retired; pins keep what nodes need; the grace leaves time to restore; each pass
  that changes something writes `retention.run`, and each retirement `release.retire`, which the
  tenant's administrators and sinks see; and, if the owner takes Q-E, recently downloaded versions
  are kept.
- **Never the current.** The current versions of every line are computed in the full view and the
  public view, and re-checked under the channel lock before each retirement: a node installing with
  the flat path finds what it found before the pass. A pin is honoured the same way. One exception
  is an administrator's own act: with `keep: 1`, yanking the current version leaves the flat path
  empty in that line until a restore or a new release, since the version before it was retired (Q-H).
- **Restorable, then final.** A retirement is undone by a restore during the grace; a purge is final
  (spec 0016) and keeps the slot taken, so the version can never come back with another body.
- **No existence leak.** DuckDB's answer for a retired release is the same missing answer as for any
  release a caller cannot have; the index shows `retired` only where it would show the release; the
  plan names other channels only to their administrators.
- **Signatures**: a retired release stays signed by the serving key, so a restore cannot serve an
  unsigned release; a yank still ends it for good.
- **Tenant isolation**: the plan's `shared` and bytes read the tenant's own rows only.
- **Fail closed**: a pass that cannot read the rule, the releases or the pins retires nothing in
  that channel; a field with no value in force means no rule; a release whose lines cannot be
  computed (an unknown C API maximum) is kept.

## Testing

- **Store suite** (three dialects): the migration on a populated database (the widened `CHECK`, the
  SQLite rebuild keeping signatures and indexes); `retired` excluded from every serving read and
  included in re-signing; `retired_from`, `retired_at` and a restore; an upstream's yank of a retired
  release and its lift (back to `retired`, a new `retired_at`); pins' keys and their validation; the
  reaper and `TestReapCompleteness`.
- **The retention computation** (`internal/release`, table tests): newest N per (source, name, line)
  by first release; a version's platforms counted once; a current older than the newest N kept; a
  private current (full view) and a public one both kept; a non-selected source's current kept; a
  `c_struct` build across two lines kept by one; a release serving no line left alone; pins per line
  and for every line, a pinned retired version not purged, a pin kept while a release of its version
  is left in its line; yanked, withdrawn and retired releases not counted; two sources counted apart;
  `keep` from the channel and `grace` from the tenant; a missing field: nothing retired or purged.
- **The pass**: lease, idempotence, a retirement re-checked under the lock (a release made current
  or pinned meanwhile is kept), the purge after the grace through spec 0016's purge, no purge while
  `gc.interval` is 0, suspended and deleting tenants skipped, one `release.retire` per (name,
  version), `retention.run` only when something changed, the actor.
- **Release changes and intake**: restore (to `deprecated` and to `active`, `seq` kept), yank of a
  retired release, purge of a retired release, a block yanking a retired release, a withdrawn entry's
  release not restored; `409 …/retired` for a promotion into a retired slot, a publication of the same
  body and of another, a placement by hand, a first arrival and a bundle import; a mirror's run, a
  lazy fetch and a bundle cell recording `retired`; a manual rule not completing a retired version,
  an auto rule not placing it again.
- **The line act**: a line the channel serves and one it no longer serves (`c_struct` matched by the
  version's maxima, `cpp` by its slot); a passthrough channel refused.
- **API and CLI**: every route's rights (`404` for a caller without them; auditors read and write
  nothing), the `PUT`s' no-op and events, `in_force` per field, `keep` above its column and `grace`
  above its ceiling refused, the plan's lists, paging and `shared_by` limited to the caller's
  channels, the index's `retired` status, `?state=retired`, `release.purge`'s `reason`,
  `SubjectForm` and `TestCoverage`.
- **DuckDB end to end** at the pin: `INSTALL x FROM repo VERSION 'old'` fails once the version is
  retired, the flat `INSTALL x FROM repo` still installs the current version, the versioned install
  works again after a restore, and after a purge (the pass with a grace of `0`) it fails for good.

## Open questions

- **Q-E. Versions still in use.** duckdb-acl's spec 093 installs extensions by version, so a node may
  run a version the rule would retire.
  - *Recommended*: keep versions downloaded within `retention.keep_downloaded_within` (configuration,
    off by default), and show each version's last download in the plan (the statistics already
    answer `last_download_day` per release).
  - *Alternative 1*: only show the last download in the plan; the rule ignores it.
  - *Alternative 2*: retire as the rule says, but never purge a version downloaded recently.
- **Q-F. A retired-then-purged release looks like a yank to nodes.** Spec 0016: a purged release
  leaves the index, and a node reads its absence as a yank.
  - *Recommended*: record why a slot was purged (`purged_slots.cause`: `yanked` or `retired`), and
    keep a retired-then-purged slot in the index with a status of its own ("gone from this channel,
    not revoked"); amend specs 0007 and 0016.
  - *Alternative 1*: no automatic purge: the pass retires only, and purging stays an administrator's
    act.
  - *Alternative 2*: accept it: nodes treat such a version as yanked.
- **Q-G. A `c_struct` release serving several lines**, which the mockup shows as "retired in v2.0.0,
  kept in v1.5.2".
  - *Recommended*: one state per release, retired only when no line keeps it. As a consequence,
    `v2.0.0` nodes keep installing it by version while `v1.5.2` keeps it.
  - *Alternative*: a retirement per (release, line), as the mockup shows: feasible but costlier (a
    per-line state read by every resolution, and one release answering differently by DuckDB version).
- **Q-H. To confirm, as the spec now stands:**
  - no retention unless a tenant or a channel sets a rule (alternative: a server-wide default in
    `kista.yaml`, under the tenant's);
  - the pass does not purge yanked releases, whose purge stays an administrator's (spec 0016, the
    owner's choice of 2026-10-09) (alternative: purge them a grace after their yank);
  - "newest" is the order of the versions' first release in the line, which works for commit-hash
    versions (alternative: spec 0006's version order, as the placement pass uses);
  - `keep: N` counts the current version among the N, so with `keep: 1` a yank of the current leaves
    the line's flat path empty until a restore or a new release (alternative: N versions besides the
    current, so `keep: 1` keeps one fallback).

## Alternatives considered

- **Retention by yanking and purging**: no new state, but a yank is final and tells nodes "do not
  run this", which housekeeping must not.
- **Retired as a column (`retired_at` alone) beside the state**, as the catalogue's holds and
  withdrawals are: every serving read checking `state IN (active, deprecated)` would need a second
  condition, where a state value is excluded by the reads as they are; the re-signer's
  `state <> 'yanked'` keeps a retired release signed without a change.
- **Retention by age** (versions older than a duration): an old version that is still current would
  need the same exception, and a channel nobody publishes into would lose everything but its
  current; a count per line keeps a fixed history.
- **Counting per name across lines**: an LTS line with few releases would lose its history to the
  main line's pace; the mockup counts each line on its own.
- **Purging at retirement**: no way back; the grace is the restore window.

## Delivery

Two PRs: (1) the `retired` state (migration, serving, the index, restore, purge, every intake path,
the mirrors and bundles, spec 0039's revocations), with the DuckDB end to end; (2) the rule, pins,
the line act, the pass, the plan, the ceiling, the CLI and the console's Retention tab. The website:
`tenants-and-channels.md` (retention), `publishing.md` (the release states), `api.md`,
`configuration.md` (`retention.interval`, `limits.ceilings.retention_grace`), `console.md`,
`observability.md` (the events), `administration/commands.md`, `administration/upgrading.md` (the
migration's `min_reader`).

## Follow-ups

- Retiring one release by hand (`POST …/releases/{id}/retire`); the mockup does not show it.
- `KeepLanded` (the catalogue's three newest landed versions, spec 0033) is a constant in code; by
  the owner's rule it becomes configuration, as a separate cleanup.
- Retention per name (a name keeping more than the channel's N): pins cover single versions; a
  per-name override if a plan needs it.
- Retention in a bundle site's channels (spec 0014): each site's own rule, nothing carried in the
  bundle.
- A remote kista's retirements are not carried downstream (spec 0039 carries yanks and
  withdrawals): a downstream channel keeps by its own rule.
