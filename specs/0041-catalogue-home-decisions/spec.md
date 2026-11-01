# Spec 0041: The catalogue as the console's home, and the decision queue

- **Status**: draft
- **Date**: 2026-11-01
- **Author**: vgsml, Claude

## Summary

For a tenant's administrators, the console's home becomes the catalogue: a matrix of every
(source, name) against the tenant's channels, showing what each channel serves and what waits.
Beside it, **Needs a decision** gathers what kista leaves to an administrator: new versions under
manual rules, holds ending, names a follow skips, names no channel takes, failed placements. Most of
it is the console reading the API that specs 0033, 0035 and 0011 built. This spec adds the small
server pieces the owner's mockup (2026-10-26) asks for:

- a held version's end in the catalogue's list, and when each rule's next placement waits until;
- what waits under a manual rule, on the rule, and an event when a version starts waiting;
- **decisions** (Skip and Dismiss), kept in kista and the same for every administrator;
- a block that says what its body is.

Plan item K1. The owner's open questions are listed at the end (Q-A to Q-D).

## Problem

The console (spec 0015, spec 0033 phase C, and the gaps plan item K4 closed) has a Catalogue
section and a Sources tab per channel. An administrator sees each piece, but not the tenant as a
whole, and some pieces cost one request per row:

- **Holds.** `GET …/catalogue` lists each entry's `held` versions, not when they stop being held.
  The time is on each build (`available_at`), so "held until Oct 27, 11:00" in a list means one read
  of `…/catalogue/{source}/extensions/{ext}` per held entry. And a source's hold is not the whole
  wait: an automatic placement also waits for the channel's own hold (`AvailableIn(ch.Hold)`,
  `internal/release/place.go`).
- **What waits is not said.** The sources answer gives a rule's `current` and `newest`, but
  `newest` is the source's newest available version, not what the rule could place: a version
  built for DuckDB versions or platforms the channel does not serve, or one the channel had and
  yanked, is not waiting.
- **Nobody is told.** A manual rule means "I choose the versions". When a newer version becomes
  available, the administrator learns of it only by looking (spec 0033's follow-up: "Notifying a
  channel's administrators of a new version under a `manual` rule").
- **Choices are not remembered.** An administrator who decides not to take `acl 0.4.0` into `prod`,
  or who has seen that a follow skips `h3` and is fine with it, has nowhere to record it. A queue kept
  in one browser differs for each administrator and is lost on the next sign-in.
- **A block is a hash.** `GET …/blocks` answers body hashes and reasons. To show "tresor 1.3.0,
  linux_amd64, publisher:ci" the console searches the catalogue per hash.

Two items in the mockup's notes need nothing new:

- **Event kind families** (`release.*` in the Events filter): spec 0040 made `kind` a list of exact
  kinds and families (`kind=release.*`, and exclusions with `!`). It is implemented, so it is UI
  only here.
- **The public-licence warning** (spec 0035 phase 2): implemented, and the console already shows
  its `warnings`.

## Design

### What the mockup's screens take

| Screen (mockup artboard) | From this spec | UI only, on today's API |
| --- | --- | --- |
| **Main** (the catalogue matrix) | `held_until` in `GET …/catalogue`; `waiting` and `waits_until` in each channel's `GET …/sources` | the matrix from `GET …/catalogue` and each signed channel's `GET …/sources` and `GET …/follows`; `+ Rule…` and `Set rule…` (`PUT …/sources/{name}`); the "decisions wait" banner (counted as the queue below) |
| **Catalogue-Entry** (one page per name, its sources side by side) | `waiting` and `waits_until` | each channel's rule set in place (`PUT …/sources/{name}`, `PUT …/follows/{source}`); Place, Promote and Clear; licences per entry (`GET …/licenses?source=`, `…/license-keys?source=`, spec 0035); reports per build (`GET …/extensions/{ext}/attachments?body_hash=`, spec 0011); downloads (`GET …/stats/downloads?extension=&channel=`). The "Take only versions matching" field waits for version constraints (Q-D) and is not shown. |
| **Decisions** (the queue) | decisions (Skip, Dismiss, Undo); `waiting`, `waits_until`, `held_until`; the `source.waiting` event behind "Tell me of these…" (Q-A) | the groups, computed below; Promote (K4's dialog; spec 0043 names the previous stage), Place, Make auto or manual (`PUT …/sources/{name}`), Place early (`…/releases/place` with `early`), Purge (`DELETE …/catalogue/builds/{id}`), Open the block; the Report & licence drawer (specs 0035 and 0011) |
| **Decisions-Rule** (set a rule from the queue) | the notify checkbox only if the owner takes a per-rule setting (Q-A) | the dialog and its `PUT …/sources/{name}`; "What happens" computed from the source's and the channel's holds |
| **Org-Blocks** | `builds` in the block answers | Block…, Unblock (`POST`, `DELETE …/blocks`) |
| **Org-Events** | none | the family chips send spec 0040's `kind` list |

The **Catalogue-Pipeline** screen is spec 0043's (it needs the channels' order); its Retention line
is spec 0042's. The **Retention** screen is spec 0042's. The mockup's other screens (Developers,
Developer-Page, Org-Access, Org-Groups, Org-Identity, Org-Licences, Org-Stats and the Gap-* boards)
are UI only on today's API, or plan item K4's (done), and are not this spec's.

### Who sees what

The catalogue's reads are the tenant's administrators' (spec 0033; a distribution admin reads its
distributors' publishers' entries only). So:

- **A tenant administrator** (and a server administrator in an opened tenant): the console's home is
  **Catalogue** (the matrix), with **Needs a decision** under it in the navigation and its count.
- **A channel's administrator who is not a tenant administrator**: the home stays **Channels**, as
  today; their channels' Sources tabs show `waiting` and `waits_until`. Whether they also get a
  queue, and which part of it, is Q-B; as recommended there, the queue and decisions are tenant
  administrators' only.
- **An auditor** (`audit`, spec 0040) reads the decisions; the console has no auditor's view (spec
  0040's follow-up), so this is for the API.

### The queue, as the console computes it

All from reads a tenant administrator already has, per signed channel of the default package type
(other types through the API, as the Catalogue section does today):

| Group | Found from | Actions |
| --- | --- | --- |
| New versions under manual rules | a rule with mode `manual` (or `follow` acting as manual) with a `waiting` version (below), less those a **skip** covers | **Promote** from a channel that holds the waiting version as an active or deprecated release (spec 0043: the previous stage first); else **Place** from the catalogue; Skip; Make auto |
| Holds ending within a day | a rule's `waits_until` (the source's and the channel's holds), and the catalogue's `held_until` for names no automatic rule takes yet | Place early, Purge |
| Names a follow skips | `GET …/follows/{source}`'s skipped names, less those a dismissal covers | Set rule, Dismiss |
| In no channel | catalogue entries with no rule in any channel (the catalogue's `rules`), or taken by some channels only, less those a dismissal covers | Place, Purge, Set rule, Dismiss |
| Placements that failed | a rule's last `outcome` | Open the block, Make manual |

"Within a day" is the console's display choice, not a server setting. The console reads the
decisions once (`GET …/decisions`) and filters in the page.

### `held_until` in the catalogue's list

`GET /api/v1/tenants/{t}/catalogue` answers, per entry, beside `held` (the versions with a held
build, newest first), `held_until`: an object from each of those versions to the latest
`available_at` of its builds that are held now, the moment the version leaves `held`:

```json
{"source": "publisher:enterest", "name": "geo_tiles", "versions": ["0.2.0", "0.1.4"],
 "held": ["0.2.0"], "held_until": {"0.2.0": "2026-10-27T11:00:00Z"}}
```

One grouped read (`MAX(available_at)` per (source, name, version) where `available_at` is later than
now): `HeldVersions` answers the times with the versions, so the list costs no more reads than
today. A distribution admin's and a distributor's publisher's views of their own entries carry it
too (they already see `held`). `kista admin catalogue list` shows the time in its `HELD` column.
This is the source's hold only; a channel's own hold is per rule, below.

### `waiting` and `waits_until` on a rule

`GET …/channels/{c}/sources[/{name}]` answers, per rule with a mode:

- **`waiting`**: the newest version the rule could take that the channel has not had, defined as
  the placement pass defines its candidates (spec 0033 A1 part 2): a version of the rule's source
  with an available entry (not held, withdrawn nor revoked, its body not blocked) for some cell the
  channel serves (one of its DuckDB versions, a platform), newer (spec 0006's order) than everything
  that cell has or had from the source (`served.newest`: releases in any state, yanked, retired
  (spec 0042) and purged slots included). Omitted when nothing waits. For an `auto` rule it is what
  the next look places; for a `manual` rule, what an administrator may place.
- **`waits_until`**: the moment the rule's next automatic placement can happen, when it waits for a
  hold: the earliest `AvailableIn(ch.Hold)` (the later of the source's hold and the channel's) among
  the candidates newer than the cells had, as the look records it in `due_at`. Omitted when nothing
  is held, and for a `manual` rule (a channel's hold applies to automatic placements only).

The pass and the answer share one function (the candidates per cell), so the console and the pass
never disagree. The sources answer reads the channel's served cells, as the pass's look does; its
cost per channel is the look's.

### Telling of a version that waits under a manual rule

- **The event `source.waiting`** (subject `channel:<c>/ext:<name>`, actor `system:placement`; data
  `source`, `version`, `current`, `mode`, and `package_type` when not the default): written by the
  placement pass when its look at a rule of mode `manual`, or of mode `follow` while the channel no
  longer follows the source, finds a `waiting` version (above) newer than the version the rule last
  told of.
- **Once per version.** `channel_sources.told_version` keeps the version the last event named.
  `SetChannelRule` clears it on any change of the rule's source or mode (`internal/store/sources.go`),
  so the first look after a rule becomes manual tells of the waiting version if there is one.
- **In one transaction, conditionally.** The event and `told_version` are written together, by an
  update that requires what the look read: the source and the mode unchanged, `set_at` not after
  the look's start, and `told_version` still the value read (as `SetRuleChecked` guards its update,
  `internal/store/rules.go`). The event is written only when the update changed a row.
- **A lapsed lease.** The pass renews its lease only before inserts (`place.go`), so a look that
  outlives its lease may run beside another replica's look of the same rule. The conditional update
  is what keeps the tell single: of two looks that read the same `told_version`, only the first to
  commit changes the row and writes the event; the second changes nothing and writes none.
- **When.** The pass already looks at manual rules when their source has new builds (to complete a
  chosen version). A newer version whose hold ends later wakes the rule then: the look records
  `due_at` for the end of the source's hold of the newest held candidate, as it does for an `auto`
  rule. The hourly sweep catches the rest.
- **A failed tell is not a failed placement.** An error writing the tell is logged and the next look
  tries again; it is never the rule's `outcome` (`SetRuleOutcome`), which the queue shows as a failed
  placement.
- **Not for the `tenant` source.** The pass never looks at its rules (spec 0033: `tenant` is never
  automatic). Its waiting versions are in the sources answer and the queue, but no event tells of
  them.
- **Who reads it** and how anyone is told: Q-A. A self-hosted kista emails no one.

### Decisions

A **decision** is an administrator's answer to a queue item, kept in kista so every administrator
sees the same queue. As recommended in Q-C:

| Kind | Scope | Keyed to | The item comes back when |
| --- | --- | --- | --- |
| `skip` | a channel, a source, a name | the waiting version skipped | a version newer than it waits |
| `dismiss` (a follow's skip) | a channel, a source, a name | the follow's skip reason | the follow skips the name for another reason |
| `dismiss` (no channel, or some channels only) | the tenant, a source, a name | the set of channels whose rules take the name (empty: none) | that set changes |

- **One decision per (scope, kind, source, name)**: a later decision replaces it (a skip of `0.4.1`
  replaces the skip of `0.4.0`). Only the newest waiting version is ever shown, so remembering one
  version per item is enough.
- **A decision changes nothing else.** A skip does not stop a placement by hand or a promotion, a
  manual rule places nothing by itself anyway, and an `auto` rule has nothing to skip. A dismissal
  does not change a follow. The decision only shapes the queue.
- **Routes** (answers: `id`, `kind`, `channel` when scoped to one, `source` as answers name it,
  `name`, `version` (a skip's) or `situation` (a dismissal's), `decided_by` masked as `created_by` is
  (spec 0040), `decided_at`, `package_type` when not the default, and an `ETag` of its row version):
  - `GET /api/v1/tenants/{t}/decisions[?type=&channel=&cursor=&limit=]`: the tenant's
    administrators and its auditors; every decision of the tenant, paged by id.
  - `GET /api/v1/tenants/{t}/channels/{c}/decisions[?cursor=&limit=]`: also the channel's
    administrators, for their channel (spec 0040's pattern: a channel's administrators read their
    own channel; tenant administrators and auditors read all).
  - `PUT /api/v1/tenants/{t}/decisions` with `{"kind": "skip"|"dismiss", "channel"?: …, "source":
    …, "name": …, "version"?: …, "situation"?: …, "type"?: …}`: the tenant's administrators (Q-B).
    It writes or replaces the decision of (scope, kind, source, name) and answers it (`200`; `201`
    when new).
  - `DELETE /api/v1/tenants/{t}/decisions/{id}` with `If-Match` (the ETag): the tenant's
    administrators (Undo). A stale ETag is `412`: another administrator changed the decision since.
  - Should the owner give channel administrators a queue (Q-B's alternative), `PUT` and `DELETE`
    also come under `…/channels/{c}/decisions` for the channel's administrators.
- **Ids are checked against their scope**: an id of another tenant, and on a channel's route an id
  of another channel or of the tenant's scope, answers `404`, the same bytes as an unknown id.
- **Checks** (shape and existence, `400` or `404` as the catalogue's routes answer): the source is
  one of the tenant's live sources, and the name has entries of it in the catalogue, of the
  channel's package type; a skip's version is one of them, in a signed channel; a dismissal's
  `situation` is what the console was shown (a skip reason from the follow's list, or a list of the
  tenant's channel names). A decision on a removed source cannot be written.
- **Gone with what they name**: removing a source deletes its decisions (as its follows and skips,
  spec 0033 part 4); when the collector deletes the last build of a (source, name), it deletes that
  (source, name)'s decisions in the same transaction, so decisions never outlive the catalogue
  entries they are about; tenant deletion (spec 0019) reaps them.
- **Events** `decision.set` (with `before` when it replaced one) and `decision.clear`; subject
  `channel:<c>/ext:<name>` for a channel's decision, `ext:<name>` for the tenant's; data `id`,
  `kind`, `source`, `version` or `situation`, `package_type`. Both subject forms are added to
  `audit.SubjectForm`, and both kinds to the events registry.

### What a blocked body is

`GET …/blocks` and `GET …/blocks/{hash}` answer each block with `builds`: the tenant's catalogue
builds with that body, any state (`source` as answers name it, `name`, `version`, `platform`,
`slot`, `package_type` when not the default), by source and name. A body nothing in the tenant holds
(blocked ahead of time, or whose builds the collector removed) answers `"builds": []`. One query
joining `blocks` to `builds` by (tenant, body hash) for the list. `kista admin block list` shows
`WHAT` (the first build, and `+n`).

### Data model

One migration (the next free number), in the three dialects:

```text
channel_sources   + told_version   text NULL      -- the version the last source.waiting named
decisions         id, tenant_id, package_type, channel_id NULL (fk channels), kind ('skip'|'dismiss'),
                  source, name, package_version NULL, situation NULL, version (the row's, for the ETag),
                  decided_by, decided_at
                  unique (channel_id, kind, source, name)              where channel_id IS NOT NULL
                  unique (tenant_id, package_type, kind, source, name) where channel_id IS NULL
                  index  (tenant_id, package_type, source, name)       -- the collector's and a source's deletes
```

- The two unique keys are partial (filtered on SQL Server), as the catalogue's slot key is (spec
  0033 A1 part 3a); text columns BIN2 on SQL Server.
- **`min_reader` is raised to this migration.** `decisions` references `channels`; an older
  replica's tenant reaper (spec 0019, `reapSteps`) does not know the table and would fail deleting
  `channels` on PostgreSQL and SQL Server, as specs 0032 and 0037 found for their tables. The new
  table joins `reapSteps` before `channels` and `TestReapCompleteness`.

### The console

- **The tenant's home** (tenant administrators) is the catalogue matrix (Main): rows (source, name)
  with the entry's icon and summary (spec 0036); a column per signed channel of the default package
  type with its follow and hold in the header; each cell the rule's mode and current version, `→
  waits` with the rule's `waiting` (less a skip), a held candidate with `waits_until`, a failed
  outcome, "serves it from …" when the channel's rule names another source, the follow's skip with
  its reason, and `+ Rule…`. Filters by source. "Needs a decision" sits under Catalogue in the
  navigation, with its count: the console's own signal that something waits.
- **The entry's page** is per name, every source side by side (today it is per (source, name)): each
  channel's rule edited in place (unsaved changes listed, one `PUT …/sources/{name}` per changed
  channel on Save), each source's licence and its versions' reports.
- **Needs a decision**: the groups above, filtered by channel; each action as listed; Skip and
  Dismiss write a decision, Undo deletes the decisions this page wrote (with their ETags); the
  Report & licence drawer.
- **Channels' Sources tabs** (every channel administrator) show `waiting` and `waits_until`.
- **Blocks** shows "What it is" from `builds`. **Events** offers the families as chips.
- The console never offers an action its caller may not take (`mayPromote`, a channel
  administrator's `early`, as today) and shows the API's refusal otherwise.

## Security

- **Decisions are advisory.** They change no placement, rule, follow, grant or serving answer; the
  worst a wrong decision does is hide a queue item from administrators, who can remove it.
- **No existence leak.** Every new route is a management route: install holders and anonymous
  callers get `404` (or `401` without a valid token) as on any other; a channel's administrator
  reads only its channel's decisions, and an id outside the route's scope is `404`; a distribution
  admin reads none (it reads no channel, spec 0033 B), and its view of the catalogue gains
  `held_until` only for the entries it already sees.
- **`waiting` and `waits_until`** are computed from what the sources answer already shows its
  readers (the channel's releases, the source's catalogue entries and holds).
- **Blocks' `builds`** are the tenant's own catalogue rows, read by its administrators, who already
  read them through the catalogue; nothing of another tenant sharing the body in a storage domain.
- **The event** carries versions and a source label, facts the sources answer shows; it is the
  tenant's.
- **Fail closed**: a tell that fails is tried again and never shown as a failed placement; a
  decision that fails to write leaves the item in the queue.

## Testing

- **Store suite** (three dialects): `held_until` per version (two builds of one version, one held
  longer; a hold ended; a raised hold); decisions: a replacement by (scope, kind, source, name), the
  two partial keys (a channel's and the tenant's decision of one name both kept), the row version,
  deletion with the source and with the collector's last build of a (source, name), the reaper and
  `TestReapCompleteness`; `told_version` cleared by `SetChannelRule` on a source change and on a mode
  change; the conditional tell (a second look reading the same `told_version` writes nothing); blocks'
  `builds` (two sources of one body, none).
- **`waiting` and `waits_until`**: a version for a DuckDB version or platform the channel does not
  serve is not waiting; one the channel had (yanked, retired, purged) is not; a channel's hold later
  than the source's sets `waits_until`; the answer and the pass's candidates agree (one function).
- **The placement pass**: `source.waiting` once per version for a manual rule (a second look writes
  none; a newer version writes one; a version arriving held is told when its hold ends, woken by
  `due_at`; a mode change and back tells again); none for `auto`, for a follow acting as `auto`, for
  a rule without a mode, for `tenant`; one for a follow's rule after the follow is removed; two looks
  racing past a lapsed lease tell once; a failed tell is logged, not the rule's outcome.
- **API**: the decision routes (a tenant administrator writes; a channel administrator reads its
  channel's and gets `404` on another channel's, on the tenant route and on any write; an auditor
  reads and writes nothing; a distribution admin, an install holder and a publisher `404`); ids out of
  scope `404`; `If-Match` on `DELETE` (`412` when stale); the checks (`400`, `404`); paging;
  `decision.set`, `decision.clear` and `source.waiting` in the events registry, `SubjectForm` and
  `TestCoverage`; `held_until`, `waiting`, `waits_until`; `builds` in the block answers.
- **CLI**: `catalogue list`'s `HELD`, `block list`'s `WHAT`.
- **Console**: unit tests of the matrix and the queue's groups from fixtures (a skip hides a version
  until a newer one, a dismissal until its situation changes, Promote offered only where a channel
  holds the version, Place otherwise; a channel administrator's home is Channels); the console end
  to end (`web/console/e2e`): the home matrix of the existing fixture (`nightly` following a
  distributor's publisher with a day's hold), a Skip remembered across a reload and seen by a second
  administrator, Undo, a dismissal of the follow's skip, a block showing what it is.
- No DuckDB end to end changes: nothing served changes.

## Open questions

- **Q-A. How is an administrator told of a waiting version?** The mockup has "Tell me of these…" on
  the queue and "Tell staging's administrators when a new version of it waits" in the rule dialog.
  - *Recommended*: kista writes `source.waiting` for every manual rule and keeps no notification
    settings. A channel's administrators may read their own channel's events (`GET …/events` with
    `subject=channel:<c>/`, a widening of spec 0040's readers to that subject prefix); the
    navigation's count is the in-console signal; delivery to people is the operator's sinks (spec
    0010) or Enterest's. A self-hosted kista emails no one. The rule dialog's checkbox is dropped.
  - *Alternative 1*: a per-rule `notify` flag (`channel_sources.notify`), the event written only for
    rules an administrator asked about.
  - *Alternative 2*: kista delivers, by per-tenant webhooks (a URL, the kinds, a signing secret;
    egress-checked, retried): a spec of its own.
  - *Alternative 3*: per-administrator subscriptions (principal, kinds, channels) kept in kista for a
    deliverer to read; kista knows no mail addresses, so a deliverer is still needed.
  - Enterest-side follow-ups, whichever is chosen: Enterest's tenant watch (its spec 0012) ignores
    `system:` events, so it must take `source.waiting` explicitly to tell its customers; and its
    provisioning, if it sets retention (spec 0042), needs `retention.set` in its `kista_acts`.
- **Q-B. A channel administrator's queue.**
  - *Recommended*: the queue and decisions are tenant administrators' only; no decision is written
    on a channel's route. A channel administrator works from the channel's Sources tab (`waiting`,
    `waits_until`, outcomes), and reads its channel's decisions.
  - *Alternative*: channel administrators get a partial queue for their channels (new versions,
    follow skips, failed placements; not "Holds ending" for names no rule takes, nor "In no channel",
    which need the catalogue's reads), and write their channels' decisions on `…/channels/{c}/
    decisions`.
- **Q-C. What a dismissal is keyed to.**
  - *Recommended*: a follow skip's dismissal is keyed to the skip's reason, and an "In no channel" or
    "taken by some channels only" dismissal to the set of channels taking the name; the item comes
    back when the situation changes. A Skip of a new version stays keyed to the version.
  - *Alternative*: every decision keyed to the newest version at the time, so a dismissed item comes
    back with every new release of the name.
- **Q-D. Version constraints in a rule** (`auto` within `1.x`; the entry page's "Take only versions
  matching").
  - *Recommended*: deferred to a spec of its own after K1. They change the placement pass (what
    `auto` and a follow's rules take, `waiting`, the due work) and need a grammar for versions that are
    not semver (DuckDB's core extensions use commit hashes); the field stays hidden until then.
  - *Alternative*: a minimal form here: a version prefix per rule (`"take": "1."`), `auto` and
    `waiting` taking only versions starting with it.

## Alternatives considered

- **The queue computed in kista** (`GET …/decisions/queue`): one read for the console, but a
  second place encoding what the console shows, and a server answer to keep in step with the
  screens. The reads already exist; only the decisions, and what a rule waits for, need the server.
- **The source's `newest` as "waiting"**: it counts versions for DuckDB versions or platforms the
  channel does not serve, and versions the channel yanked or purged; the pass would never place them.
- **Decisions per (channel, name, version)**, one row per skipped version, as the mockup's note
  says: the queue only shows the newest waiting version, so older rows would only accumulate.
- **Decisions in the browser's storage**: different for each administrator and each browser, which
  is what the owner's note rules out.
- **A ceiling on decisions** under `limits.ceilings`: deleting them with the catalogue entries they
  name bounds them by the catalogue, which its own caps bound.
- **A skip that stops placements**: it would make a skip a second kind of rule; a manual rule
  already places nothing by itself, and a block or a yank stops a version for good.
- **Blocks recording what the body was at block time**: survives the collector, but the answer is
  then a copy that can go stale; the join answers what the tenant holds now, and an attachment on
  the block keeps the evidence (spec 0011).

## Delivery

One PR, `0041-catalogue-home`, or two if the console part is large: first the API (`held_until`,
`waiting` and `waits_until`, `source.waiting`, decisions, blocks' `builds`, the migration), then the
console. The website: `catalogue.md` (`held_until`, `waiting`, the waiting event), `console.md` (the
home, the queue, decisions), `api.md` (the routes), `observability.md` (`source.waiting`,
`decision.*`), `administration/upgrading.md` (the migration's `min_reader`).

## Follow-ups

- Version constraints in a rule (Q-D).
- Delivery of notifications by kista, if the owner chooses it (Q-A).
- A tenant-wide read of every channel's rules with `current`, `waiting` and the outcome, if the
  matrix's one read per channel becomes slow for tenants with many channels.
- The queue for other package types (spec 0034) in the console.
