# Spec 0042: Version retention in channels

- **Status**: draft
- **Date**: 2026-11-01
- **Author**: vgsml, Claude

## Summary

A channel keeps every release it ever made until an administrator yanks and purges it one by one.
This spec adds **retention**: a rule, the tenant's default and overridden per channel, that keeps
the newest N versions of each name per DuckDB version the channel serves, always its current one,
and the versions an administrator **pins** (per DuckDB version, with a reason). A pass **retires**
the others: a retired release is not served, and an administrator can **restore** it, until a
**grace** after which the pass purges it as an administrator's purge would (spec 0016). `retired` is
a new release state, because a yank is final. An administrator can also retire a whole DuckDB line
that is no longer built. What a purge leaves goes to the catalogue's retention and the collector, as
today. Plan item K2; the owner's mockup's Retention screen.

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
| **Line** | a DuckDB version the channel serves (spec 0003's channel versions). A `cpp` or `c_struct_unstable` release serves one line; a `c_struct` release every line whose C API maximum accepts it (spec 0006). |
| **A version in a line** | the active and deprecated releases of one (source, name, version) that serve the line, on every platform: a version's builds count once, whatever the platforms. |
| **Current** | a version that the flat path serves in the line on some platform (spec 0006: the active release with the highest `seq`), in the full view or in the public view. A channel serves the older version on a platform until the newer one's build for it arrives, so a line may have several current versions. |
| **Kept** | in a line: its current versions, its newest N versions, and the versions pinned for it. |

### The rule

- `keep`: N, a whole number at least 1; `grace`: a duration of whole seconds, at least 0 (`0`
  purges at the next pass after the retirement: no time to restore). Shape only; no ceiling (a larger
  value only keeps more). "Off" is no rule at all.
- **The tenant's default** (`PUT …/tenants/{t}/retention`), for every signed channel without its
  own; **a channel's own** (`PUT …/channels/{c}/retention`): its values, `"default"` (the tenant's),
  or `"none"` (no retention in that channel, whatever the tenant's default). A tenant without a
  default and a channel without its own keep everything, as today (open question 3).
- `keep` and `grace` are set together (`400` for one without the other where no value would be in
  force).
- A **passthrough channel** takes no rule (`400`): it serves DuckDB's core byte-identical, by its
  upstream's cells, and has no current of its own to protect (spec 0009).

### What the pass retires

For each (source, name) with releases in the channel, each line separately: the versions in the
line, ordered newest first (open question 1: by when the channel released them, recommended, or by
spec 0006's version order); the kept ones as above; the rest are **retirable in the line**. A
release is **retired** when it is retirable in every line it serves (open question 2):

- a `c_struct` build serving `v1.5.2` and `v2.0.0` stays while either line keeps its version;
- a release serving no line of the channel (its DuckDB version was removed from the channel) is left
  alone: it serves nothing and keeps nothing; retiring it is the line act below;
- yanked releases (open question 4), withdrawn entries' releases (spec 0033: never purged) and
  releases already retired are not counted and not touched;
- each source is counted on its own: the releases of a source the rule no longer selects stay
  stored and unserved (spec 0031), and keep their newest N, so a switch back serves them at once.

Then the pass **purges** each release retired for longer than the grace in force (the
`state_changed_at` of its retirement plus the channel's grace at the pass), through spec 0016's
purge: the slot stays taken (`purged_slots`), the build loses the reference, the catalogue's
retention and the collector take it from there. The purge's guard applies: while `gc.interval` is 0
(the operator's declaration that not every replica runs the version that knows `purged_slots`), the
pass retires but purges nothing, and the plan says so.

### Pins

A pin keeps (name, version) in the channel's line, or in every line, whatever the rule: `POST
…/channels/{c}/retention/pins {"name", "version", "duckdb_version"?, "reason"}` (no
`duckdb_version`: every line; a reason of 1 to 400 bytes of printable text, a format bound). The
version must have a release in the channel (any state but purged); a pin of a retired version does
not restore it (the console offers both). `GET …/retention/pins` lists them with who and when;
`DELETE …/retention/pins/{id}` unpins. A purge of the version (by hand) removes its pins. A pin
names no source: it keeps the version from any source.

### The retired state

- `releases.state` gains `retired` (spec 0006's `active | deprecated | yanked`), and
  `releases.retired_from` keeps the state it came from (`active` or `deprecated`).
- **Not served**: neither the flat nor the versioned path resolves to it, nor do the index's
  `serves`, the feed's offers (spec 0011), a bundle's export (spec 0014) or pull-through's selection.
  DuckDB at the pin asking for a retired version gets the channel's **missing** answer (spec 0006:
  `401` in a signed channel, identical bytes for a private, yanked, retired or never-existing
  release). DuckDB shows no server text in an install error, so the mockup's "a message saying it
  was retired in prod" is the index's and the console's, not DuckDB's.
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
  complete it (as a yanked version); a mirror's run or a lazy fetch finding its cell's release
  retired leaves it so (the cell's outcome `retired`, as `yanked`); a promotion from it is refused
  (it is not active nor deprecated), and a promotion into a slot it holds is `409` (`…/retired`:
  restore it). A licence key a retired release declares is not live through it (spec 0021).
- **Changes**, each under the channel lock with the snapshot bump, `If-Match` as every release
  change, by the release's extension administrators (as a yank):
  - `POST …/releases/{id}/retire`: an active or deprecated release, by hand;
  - `POST …/releases/{id}/restore`: a retired release, back to `retired_from`, keeping its `seq`
    (so it is current again only if nothing newer is). A withdrawn entry's release is not restored
    (spec 0033 B: never active again);
  - `POST …/releases/{id}/yank` on a retired release yanks it (final, as from any state);
  - `DELETE …/releases/{id}` (purge) takes a retired release too, at any time;
  - a block (spec 0008) yanks a retired release with the body, as any release that is not yanked.
- Events: `release.retire` and `release.restore` (subject the release; data as `release.yank`'s,
  plus `by`: `hand`, `retention` or `line`); a pass's purge is `release.purge` with actor
  `system:retention` and `reason: "retention"`.

### Retiring a line

`POST …/channels/{c}/retention/lines {"duckdb_version": …, "name"?: …}` (a channel administrator):
retires every active or deprecated release of the name (or of every name) that serves that line
and that no other line of the channel keeps, current and pinned versions included (pins for that
line are removed; a pin for every line keeps its version in the other lines). The DuckDB version
need not be served by the channel any more, so the releases a removed line left behind can go too.
The answer lists the releases retired and those kept by another line. Event `retention.line`, and a
`release.retire` (`by: "line"`) per release. Afterwards the line keeps nothing for those names until
a new release arrives for it; removing the version from the channel (spec 0003) stops serving the
line at all, including `c_struct` builds another line keeps.

"No longer built" is the console's label (the line's newest version is older than the name's newest
in another line); kista needs no notion of it: the current version of every line is always kept, so a
line nobody builds for keeps its current until an administrator retires it.

### The pass

- Every `retention.interval` (configuration, default `24h`; `0` turns the scheduled pass off), per
  signed channel with a rule in force, of active tenants (suspended and deleting tenants skipped),
  under a lease per channel (`kista/retention/<channel id>`), in batches with the lease renewed, as
  the placement pass does. A channel's look records `channels.retention_checked_at`.
- Each retirement and purge is its own short transaction under the channel lock, re-checking under
  the lock what the look found (the release's state and version, current, pins, the rule), so a
  release made current, pinned or restored meanwhile is left. Idempotent: a pass interrupted
  anywhere is finished by the next one.
- A failure on one release is logged and counted, and the pass goes on; the channel's summary event
  `retention.run` (subject `channel:<c>`, actor `system:retention`; `retired`, `purged`, `failed`,
  written only when it changed something or failed).
- `kista admin retention run [<t>[/<c>]] [-apply]`: a pass now, a dry run unless `-apply` (the CLI is
  a server administrator).

### The plan

`GET …/channels/{c}/retention/plan` (the channel's administrators): what the next pass would do,
computed as the pass computes it: `retire` (each release with name, version, line, platforms, its
bytes (`builds.body_len`, spec 0038), and why: the kept versions of its lines, or the line that
keeps it), `purge` (the retired releases past their grace, with `purged_at` and their bytes) and
`retired` (in the grace, each with the time it will be purged), with `next_run`
(`retention_checked_at` plus the interval, or none) and the rule in force. For each body, `shared`:
another release of the tenant's channels, or a catalogue build kept by the catalogue's retention,
holds the same body, so the purge frees nothing (only the tenant's own rows are read, never another
tenant's in the storage domain).

### The catalogue's retention

What a purge leaves follows spec 0033's catalogue retention: a publisher's or `tenant`'s builds
placed nowhere are kept among the newest `KeepLanded` (3) versions, a mirror's while its cell holds
the body. That count is a constant in code today; by the owner's rule (no hardcoded limits, spec
0038's amendment) it becomes configuration, `catalogue.keep_landed` (default 3, at least 1), in this
spec's delivery (open question 5).

### API and CLI

| Route | Who |
| --- | --- |
| `GET`, `PUT /api/v1/tenants/{t}/retention` `{"keep": 3, "grace": "720h"}` or `{"keep": "none"}` | tenant administrators (`GET` also `audit`) |
| `GET`, `PUT …/channels/{c}/retention` `{"keep": 5|"default"|"none", "grace": "168h"|"default"}`; the answer adds `in_force` (`keep`, `grace`, `from`: `channel` or `tenant`), `checked_at` and `next_run` | the channel's administrators |
| `GET …/channels/{c}/retention/plan` | the channel's administrators |
| `GET`, `POST …/channels/{c}/retention/pins`, `DELETE …/pins/{id}` | the channel's administrators |
| `POST …/channels/{c}/retention/lines` | the channel's administrators |
| `POST …/extensions/{name}/releases/{id}/retire`, `/restore` | the extension's administrators |

`PUT`s answer the `GET`'s body with an `ETag`, take an optional `If-Match`, write nothing and no
event when nothing changes, and otherwise `retention.set` (subject `tenant` or `channel:<c>`;
`before`, `after`, `in_force`). Pins write `retention.pin` and `retention.unpin` (subject
`channel:<c>/ext:<name>`; `version`, `duckdb_version`, `reason`). CLI: `kista admin tenant retention
<t> [-keep N|none] [-grace d]`, `kista admin channel retention <t>/<c> [-keep N|default|none]
[-grace d|default]`, `kista admin channel retention-plan <t>/<c>`, `kista admin channel pin|unpin
…`, `kista admin channel retire-line <t>/<c> <duckdb-version> [-name n]`, `kista admin release
retire|restore <t>/<c> <id>`, `kista admin retention run`. `release list` shows `retired`;
`…/releases?state=retired` filters.

Configuration:

```yaml
retention:
  interval: 24h   # the scheduled pass; 0: off (kista admin retention run only)
catalogue:
  keep_landed: 3  # versions of a name a publisher's or tenant's source keeps placed nowhere
```

### Data model

One migration (the next free number), in the three dialects, with `min_reader` raised to it (an
older replica would serve no `retired` release correctly: it does not know the state, and would
refuse or misread it):

```text
releases          state CHECK widened to (active, deprecated, yanked, retired)
                  + retired_from  text NULL     -- active | deprecated while retired
tenants           + retention_keep int NULL, retention_grace_seconds bigint NULL   -- NULL: none
channels          + retention_keep int NULL     -- NULL: the tenant's; 0: none here; >= 1
                  + retention_grace_seconds bigint NULL                           -- NULL: the tenant's
                  + retention_checked_at timestamp NULL
retention_pins    id, channel_id (fk), name, package_version, duckdb_version ('' for every line),
                  reason, pinned_by, pinned_at; unique (channel_id, name, package_version, duckdb_version)
```

- The `CHECK` on `releases.state` is unnamed (migration 0003): PostgreSQL drops
  `releases_state_check` by its generated name; SQL Server finds its generated name in
  `sys.check_constraints` and drops it in dynamic SQL; SQLite rebuilds `releases` under
  `-- +foreign_keys_off` (spec 0033 A1 part 1's procedure; `release_signatures` references it), as
  migration 0034 did, keeping its indexes.
- Tenant deletion (spec 0019) reaps `retention_pins` by channel, before `channels`; the columns go
  with their rows.

### The console (the mockup's Retention screen)

A channel's **Retention** tab: the rule (the tenant's default or the channel's own, `keep` and the
grace), what is always kept, the defaults per channel (each channel's `in_force`), the pins (Pin and
Unpin, with the reason), per name and line the current, kept and retired versions (from the
channel's releases and the plan), "Retire the line…", and the plan's three lists (Retire, Purge,
Retired in the grace with Restore). A release's page offers Retire and Restore. The pipeline (spec
0043) shows each channel's `in_force`.

## Security

- **Consent.** Retirement and purges are made only under a rule an administrator set (the tenant's
  default by a tenant administrator, a channel's own by its administrators), or by an administrator's
  act; the pass acts as `system:retention`, audited per release.
- **Never the current.** The current versions of every line are computed in the full view and the
  public view, and re-checked under the channel lock before each retirement: a node installing with
  the flat path always finds what it found before the pass. A pin is honoured the same way.
- **Restorable, then final.** A retirement is undone by a restore during the grace; a purge is final
  (spec 0016) and keeps the slot taken, so the version can never come back with another body.
- **No existence leak.** DuckDB's answer for a retired release is the same missing answer as for any
  release a caller cannot have; the index shows `retired` only where it would show the release.
- **Signatures**: a retired release stays signed by the serving key, so a restore cannot serve an
  unsigned release; a yank still ends it for good.
- **Tenant isolation**: the plan's `shared` and bytes read the tenant's own rows only; bodies shared
  with other tenants in a domain are the collector's matter (spec 0016), never named.
- **Fail closed**: a pass that cannot read the rule, the releases or the pins retires nothing in
  that channel; a release whose lines cannot be computed (an unknown C API maximum) is kept.

## Testing

- **Store suite** (three dialects): the migration on a populated database (the widened `CHECK`, the
  SQLite rebuild keeping signatures and indexes); `retired` excluded from every serving read and
  included in re-signing; `retired_from` and a restore; pins' keys; the reaper.
- **The retention computation** (`internal/release`, table tests): newest N per (source, name, line);
  a version's platforms counted once; a current older than the newest N kept; a private current
  (full view) and a public one both kept; a `c_struct` build across two lines kept by one; a release
  serving no line left alone; pins per line and for every line; yanked, withdrawn and retired
  releases not counted; two sources counted apart.
- **The pass**: lease, idempotence, a retirement re-checked under the lock (a release made current
  or pinned meanwhile is kept), the purge after the grace through spec 0016's purge, no purge while
  `gc.interval` is 0, suspended and deleting tenants skipped, `retention.run` only when something
  changed.
- **Release changes**: retire and restore (to `deprecated` and to `active`, `seq` kept), yank of a
  retired release, purge of a retired release, a block yanking a retired release, a withdrawn
  entry's release not restored, a promotion from and into a retired release refused, a manual rule
  not completing a retired version, an auto rule not placing it again, a mirror's run leaving it.
- **API and CLI**: every route's rights (`404` for a caller without them), the `PUT`s' no-op and
  events, the plan's lists and `shared`, `retire-line`, the index's `retired` status, `release list`.
- **DuckDB end to end** at the pin: `INSTALL x FROM repo VERSION 'old'` fails once the version is
  retired, the flat `INSTALL x FROM repo` still installs the current version, the versioned install
  works again after a restore, and after a purge (the pass with a grace of `0`) it fails for good.

## Open questions

1. **What is "newest"?**
   - *Recommended*: the order in which the channel released the versions (a version's newest
     release's `created_at` in the line). It works for every version scheme, including DuckDB's core
     extensions' commit hashes (`9c1d6a3`), and matches what kista makes current (the last
     released).
   - *Alternative*: spec 0006's version order, as the placement pass uses. Right for semver, wrong for
     hashes (ordered bytewise), and a backport released after a newer version counts as older.
2. **A release serving several lines** (a `c_struct` build serving `v1.5.2` and `v2.0.0`), which the
   mockup shows as "retired in v2.0.0, kept in v1.5.2":
   - *Recommended*: a release is retired only when no line keeps it, so v2.0.0 nodes can still
     install it by version while v1.5.2 keeps it. One state per release, as today.
   - *Alternative*: a retirement per (release, line): the release is not served to the lines that
     retired it. It needs a per-line state, a new table read by every resolution, and the serving
     answer would differ by DuckDB version for one release.
3. **The default**:
   - *Recommended*: no retention unless a tenant (or a channel) sets a rule. Enterest sets its
     tenants' defaults from their plans; a self-hosted organisation opts in.
   - *Alternative*: a server-wide default in `kista.yaml` (`retention.keep`, `retention.grace`),
     under the tenant's.
4. **Yanked releases in the automatic purge**:
   - *Recommended*: no. A yank is a security act, and its purge stays an administrator's (spec
     0016, the owner's choice of 2026-10-09).
   - *Alternative*: the pass also purges yanked releases a grace after their yank, so a channel's
     storage does not keep them for ever.
5. **`catalogue.keep_landed`**: make the catalogue's "newest three placed nowhere" configuration in
   this spec (recommended, as it is the retention a purge hands over to), or leave it to a separate
   change.

## Alternatives considered

- **Retention by yanking and purging**: no new state, but a yank is final and tells nodes "do not
  run this", which housekeeping must not.
- **Retired as a column (`retired_at`) beside the state**, as the catalogue's holds and withdrawals
  are: every serving read checking `state IN (active, deprecated)` would need a second condition,
  where a state value is excluded by the reads as they are; the re-signer's `state <> 'yanked'`
  keeps a retired release signed without a change.
- **Retention by age** (versions older than a duration): an old version that is still current would
  need the same exception, and a channel nobody publishes into would lose everything but its
  current; a count per line keeps a fixed history.
- **Counting per name across lines**: an LTS line with few releases would lose its history to the
  main line's pace; the mockup counts each line on its own.
- **Purging at retirement**: no way back; the grace is the restore window.

## Delivery

Two PRs: (1) the `retired` state (migration, serving, the index, retire and restore, purge,
promotion, the mirrors), with the DuckDB end to end; (2) the rule, pins, the line act, the pass,
the plan, `catalogue.keep_landed`, the CLI and the console's Retention tab. The website:
`tenants-and-channels.md` (retention), `publishing.md` (the release states), `api.md`,
`configuration.md` (`retention.interval`, `catalogue.keep_landed`), `console.md`,
`observability.md` (the events), `administration/commands.md`, `administration/upgrading.md` (the
migration's `min_reader`).

## Follow-ups

- Retention per name (a name keeping more than the channel's N): pins cover single versions; a
  per-name override if a plan needs it.
- Retention in a bundle site's channels (spec 0014): each site's own rule, nothing carried in the
  bundle.
- A remote kista's retirements are not carried downstream (spec 0039 carries yanks and
  withdrawals): a downstream channel keeps by its own rule.
