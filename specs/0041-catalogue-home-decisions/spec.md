# Spec 0041: The catalogue as the console's home, and the decision queue

- **Status**: implemented (phase 1: the API; phase 2: the console)
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

Plan item K1. The owner's decisions (2026-11-05, Q-A to Q-D) are listed at the end.

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
| **Catalogue-Entry** (one page per name, its sources side by side) | `waiting` and `waits_until` | each channel's rule set in place (`PUT …/sources/{name}`, `PUT …/follows/{source}`); Place, Promote and Clear; licences per entry (`GET …/licenses?source=`, `…/license-keys?source=`, spec 0035); reports per build (`GET …/extensions/{ext}/attachments?body_hash=`, spec 0011); downloads (`GET …/stats/downloads?extension=&channel=`). The "Take only versions matching" field waits for version constraints, deferred to a spec of their own (decision Q-D), and is not shown. |
| **Decisions** (the queue) | decisions (Skip, Dismiss, Undo); `waiting`, `waits_until`, `held_until`; the `source.waiting` event, which "Tell me of these…" explains (decision Q-A) | the groups, computed below; Promote (K4's dialog; spec 0043 names the previous stage), Place, Make auto or manual (`PUT …/sources/{name}`), Place early (`…/releases/place` with `early`), Purge (`DELETE …/catalogue/builds/{id}`), Open the block; the Report & licence drawer (specs 0035 and 0011) |
| **Decisions-Rule** (set a rule from the queue) | none: the mockup's notify checkbox is dropped (decision Q-A) | the dialog and its `PUT …/sources/{name}`; "What happens" computed from the source's and the channel's holds |
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
  today; their channels' Sources tabs show `waiting` and `waits_until`, and they read their
  channels' decisions and events. The queue and writing decisions are tenant administrators' only
  (decision Q-B). Their Channels screen lists their own channels only, without the pipeline: the
  pipeline (Channels → Pipeline, spec 0043) is a tenant administrator's view.
- **An auditor** (`audit`, spec 0040) reads the decisions, and writes nothing; the console has no
  auditor's view (spec 0040's follow-up), so this is for the API.

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
  `waits_for` beside it names that candidate's version (review fix: the queue's Place early and
  Purge name it).

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
- **How anyone is told** (decision Q-A): kista keeps no notification settings and delivers
  nothing itself; a self-hosted kista emails no one. The event is read by the tenant's
  administrators and auditors, by a channel's administrators for their channel (below), and by the
  operator's sinks (spec 0010), through which an organisation alerts; Enterest tells its customers
  from the events (a follow-up on its side). In the console, the navigation's "Needs a decision"
  count is the signal, and "Tell me of these…" explains where the event goes.

### A channel's administrators read their channel's events

Spec 0040's readers of `GET …/events` and `…/events/{id}` (the tenant's auditors and
administrators) gain a channel's administrators, for their channel only:

- **The subject.** A caller holding `admin` on a channel, without `audit` on the tenant, must send
  `subject=channel:<c>` or `subject=channel:<c>/…` (a longer prefix), naming a channel it
  administers. `channel:<c>` is read as that exact subject plus the prefix `channel:<c>/`, never as
  a bare prefix, so `channel:prod` does not match `channel:prod2`. That takes in the channel's own
  events (`channel.versions`, the channel's `catalogue.hold`, `follow.set` and `follow.clear`,
  `retention.*` of spec 0042, `key.*`) and those under it (rules, releases, placements,
  `source.waiting`, `decision.*` of the channel).
- **The serving plane's kinds left out**: `install`, `extension.use` and `usage.receive`
  (`audit.ServingKinds`; review fix: `usage.receive` is under `channel:<c>` too), whatever the
  subject. Their actor is the
  installing principal, with its display name, client address and `User-Agent` (spec 0010, spec
  0012), and a channel's administrators do not see installers today: the statistics answer them
  counts per channel and extension (`stats/downloads`, `stats/usage`, through their `admin` grant),
  never who installed. The service filters them out by kind (an exclusion added to the caller's
  `kind` list, so an explicit `kind=install` is `404`). Every event's actor is answered such a
  caller by its id only, without `actor_name` and `client`.
- Any other read by such a caller is `404`, as today; `GET …/events/{id}` answers it only an event
  it could list.
- The API rule admits the caller by the path's tenant; the service checks the subject against the
  caller's channels and applies the kind exclusion (defence in depth, as spec 0040's `readTenant`).
  Nothing else of spec 0040 changes (the `kind` list, the settled tail, `pruned_through`, the actor
  forms).

### Decisions

A **decision** is an administrator's answer to a queue item, kept in kista so every administrator
sees the same queue. A dismissal is keyed to the situation (decision Q-C):

| Kind | Scope | Keyed to | The item comes back when |
| --- | --- | --- | --- |
| `skip` | a channel, a source, a name | the waiting version skipped | a version newer than it waits |
| `dismiss` (a follow's skip) | a channel, a source, a name | the follow's skip reason, a JSON string | the follow skips the name for another reason |
| `dismiss` (no channel, or some channels only) | the tenant, a source, a name | the channels whose rules take the name, a JSON array of channel names sorted bytewise (`[]`: none) | that set changes |

- **One decision per (scope, kind, source, name)**: a later decision replaces it (a skip of `0.4.1`
  replaces the skip of `0.4.0`). Only the newest waiting version is ever shown, so remembering one
  version per item is enough.
- **`situation`** is the dismissal's key: a string for a follow's skip, a sorted array for the
  channels taking the name. It is stored canonically, as the JSON text of that value (no spaces, the
  array sorted and without duplicates), and compared as stored: the item comes back when the
  canonical text of what the console now sees differs.
- **A decision changes nothing else.** A skip does not stop a placement by hand or a promotion, and
  a manual rule places nothing by itself anyway. Skip is offered only under a manual rule (or a
  follow's rule acting as manual), where a version waits for a person: in the queue's first group
  and on the pipeline (spec 0043); under an `auto` rule there is nothing to skip. Wherever a waiting
  count is shown (the navigation, the pipeline's "N versions wait"), a version a skip covers is left
  out by the same key: (channel, source, name), and the waiting version not newer than the skipped
  one. A dismissal does not change a follow. The decision only shapes the queue.
- **Routes** (answers: `id`, `kind`, `channel` when scoped to one, `source` as answers name it,
  `name`, `version` (a skip's) or `situation` (a dismissal's), `decided_by` masked as `created_by` is
  (spec 0040), `decided_at`, `package_type` when not the default, and `etag`, its row version as a
  strong ETag, in every answer, lists included, so any client can Undo; a single decision's answer
  also sends it as the `ETag` header):
  - `GET /api/v1/tenants/{t}/decisions[?type=&channel=&cursor=&limit=]`: the tenant's
    administrators and its auditors; every decision of the tenant, paged by id.
  - `GET /api/v1/tenants/{t}/channels/{c}/decisions[?cursor=&limit=]`: also the channel's
    administrators, for their channel (spec 0040's pattern: a channel's administrators read their
    own channel; tenant administrators and auditors read all).
  - `PUT /api/v1/tenants/{t}/decisions` with `{"kind": "skip"|"dismiss", "channel"?: …, "source":
    …, "name": …, "version"?: …, "situation"?: …, "type"?: …}`: the tenant's administrators only
    (decision Q-B).
    It writes or replaces the decision of (scope, kind, source, name) and answers it (`200`; `201`
    when new).
  - `DELETE /api/v1/tenants/{t}/decisions/{id}` with `If-Match` (the `etag`): the tenant's
    administrators (Undo). A stale ETag is `412`: another administrator changed the decision since.
- **Ids are checked against their scope**: an id of another tenant answers `404`, the same bytes
  as an unknown id; a channel's read answers only that channel's decisions.
- **Checks** (shape and existence, `400` or `404` as the catalogue's routes answer): the source is
  one of the tenant's live sources, and the name has entries of it in the catalogue, of the
  channel's package type; a skip's version is one of them, in a signed channel; a dismissal's
  `situation` is of its kind's JSON type (a string of at most 400 bytes for a follow's skip; an array
  of the tenant's channel names for the tenant's), stored canonically. A decision on a removed source
  cannot be written.
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
                  index  (source, tenant_id, package_type, name)       -- a source's and the collector's deletes
                  index  (tenant_id, id)                               -- the list, the reaper, the tenant's key
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
- **A channel's administrators' events** are their channel's subject and those under it only,
  checked by the API rule and again by the service, an exact subject or a `/` prefix (no
  `prod` → `prod2`); the tenant's other events (identity, grants, other channels, refusals with
  `route:` subjects) stay its auditors' and administrators'. The serving plane's kinds (`install`,
  `extension.use`, and `usage.receive`, whose subject is `channel:<c>`) are left out: they name
  installers, whom a channel's administrators do not see today (their statistics are counts). They
  are one list, `audit.ServingKinds`, which a test checks against every event `internal/serve`
  writes under a channel's subject. An event's actor is answered a channel's administrators by its
  id only: `actor_name` and `client` are cleared. The rest under the channel is what they already read through its management routes
  (its rules, follows, hold, releases, keys and their events, `…/keys/events`).
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
  reads, and writes nothing; a distribution admin and an install holder `404`, a publisher's signed
  request `401`, not a token on a management route); ids out of
  scope `404`; `etag` in list answers, `If-Match` on `DELETE` (`412` when stale); `situation`
  stored canonically (an array sent unsorted matches the sorted one); the checks (`400`, `404`);
  paging;
  `decision.set`, `decision.clear` and `source.waiting` in the events registry, `SubjectForm` and
  `TestCoverage`; `held_until`, `waiting`, `waits_until`; `builds` in the block answers.
- **Channel administrators' events**: a channel administrator reads `subject=channel:<c>` (the
  channel's own events, `channel.versions`, `catalogue.hold`, `follow.*`, `retention.*`, and those
  under `channel:<c>/`), `subject=channel:<c>/ext:<name>`, and `…/events/{id}` of such an event;
  `channel:prod` does not match `channel:prod2`'s events; `install` and `extension.use` are never
  answered (`kind=install` is `404`, a list without `kind` leaves them out, `…/events/{id}` of one is
  `404`); `404` without the subject, with another channel's, with a prefix that is not a channel's
  (`channel:`), and for an event of another channel; an auditor and a tenant administrator
  unchanged.
- **CLI**: `catalogue list`'s `HELD`, `block list`'s `WHAT`.
- **Console**: unit tests of the matrix and the queue's groups from fixtures (a skip hides a version
  until a newer one, a dismissal until its situation changes, Promote offered only where a channel
  holds the version, Place otherwise; a channel administrator's home is Channels); the console end
  to end (`web/console/e2e`): the home matrix of the existing fixture (`nightly` following a
  distributor's publisher with a day's hold), a Skip remembered across a reload and seen by a second
  administrator, Undo, a dismissal of the follow's skip, a block showing what it is.
- No DuckDB end to end changes: nothing served changes.

## Decisions

The owner's decisions of 2026-11-05, which the text above follows:

- **Q-A. How an administrator is told of a waiting version.** kista writes `source.waiting` for every
  manual rule and keeps no notification settings; a channel's administrators read their own
  channel's events (`subject=channel:<c>/`, a widening of spec 0040's readers); the navigation's
  count is the in-console signal; delivery to people is the operator's sinks or Enterest's. A
  self-hosted kista emails no one; the rule dialog's checkbox is dropped. Ruled out: a per-rule
  `notify` flag, webhooks delivered by kista, and per-administrator subscriptions kept in kista.
- **Q-B. A channel administrator's queue.** The queue and decisions are tenant administrators'
  only: no decision is written on a channel's route. A channel administrator works from its
  channels' Sources tabs and reads its channels' decisions and events. Ruled out: a partial queue
  for channel administrators, and their writing decisions.
- **Q-C. What a dismissal is keyed to.** The situation: a follow skip's dismissal to the skip's
  reason, an "In no channel" or "taken by some channels only" dismissal to the set of channels
  taking the name; a Skip of a new version stays keyed to the version. Ruled out: every decision
  keyed to the newest version, so a dismissed item came back with every release.
- **Q-D. Version constraints in a rule.** Deferred to a spec of their own after K1; the entry page's
  "Take only versions matching" field stays hidden until then. Ruled out: a version-prefix filter in
  this spec.

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
`decision.*`, a channel's administrators reading their channel's events), `access.md` (the same),
`administration/upgrading.md` (the migration's `min_reader`).

Delivered as two phases on one branch, `0041-catalogue-home-impl`: phase 1 the API, phase 2 the
console.

### As built (phase 1: the API)

- **One look, two readers.** `lookAt` (`internal/release/place.go`) reads a rule's candidates as the
  pass always did (checked, servable, not purged for good, a slot that never had the version, neither
  revoked nor withdrawn), each with its `AvailableIn(ch.Hold)` and whether it is newer than every
  cell it serves has or had (`served.newest`, the pass's `newer`: every cell of the build, not some
  cell). The pass places from it; the sources answer's `waiting` (`waitingOf`) and `waits_until`
  (`ruleLook.waitsUntil`) read the same look (`lookOf`). The pass reads each rule it looks at; the
  sources answer reads a channel's rules at once (`store.ChannelRuleReads`: their builds, the builds
  purged for good, the channel's releases and purged slots, of its rules with a mode only: a rule an
  arrival wrote waits for nothing), the channel's DuckDB versions and the
  tenant's blocks once and an upstream's blocks once, so its statements do not grow with the rules (a
  test counts them); a name's answer reads that name only.
- **A held version's builds never wait.** A candidate of a version the channel holds (an active
  release) is the pass's to complete, so it is never `waiting`: a second platform, or a DuckDB
  version the channel starts serving, tells nothing and queues nothing. A channel has no list of
  platforms (every platform is served), so "a platform the channel does not serve" is no case; the
  test takes a second platform of a held version instead.
- **Blocked bodies.** `waiting` leaves out a candidate whose body the tenant blocked, or its upstream
  blocked for an upstream source (spec 0039). The pass still tries such a build: its insert refuses
  it and the refusal is the rule's `outcome`, which the queue's "Placements that failed" group (Open
  the block) relies on. Under a rule placing automatically, `waiting` is what the next look places:
  per platform and slot the newest candidate, less one a newer one of another slot shadows (it
  serves a cell in common, as the pass's check finds), so a blocked newest one leaves its slot, and
  the older slots it shadows, nothing waiting (the pass places nothing older there); under a manual rule an older unblocked version
  still waits, since an administrator may place it.
- **`waits_until`** is the earliest `AvailableIn(ch.Hold)` after now among the candidates newer than
  their cells had, for an `auto` rule and a follow's rule while the channel follows the source. The
  pass's `due_at` is unchanged for an automatic rule (it also wakes for a completion's hold); under a
  manual rule it is also the earliest end of the source's hold among the newer held candidates (not
  only the newest's), so a version is told when it starts waiting. `waits_for` names the candidate's
  version, the newest of those whose hold ends then.
- **The tell** is written after the look's `SetRuleChecked`, by `store.TellWaiting`: one transaction
  updating `told_version` where the source, the mode, `set_at <= start` and the `told_version` read
  by `DueRules` still hold, and writing the event only when a row changed. Its error is logged; the
  rule's outcome is never touched. `current` is the newest active or deprecated (not withdrawn)
  version of the source in the channel.
- **Decisions.** `before` in `decision.set` is the replaced decision's version or situation.
  `package_type` is in the answers and the events when not the default. A channel's decision
  (dismissals too) is on a signed channel; a follow skip's `situation` is 1 to 400 bytes. On the
  tenant's list `?type=` absent lists every type, and an unknown `?channel=` is `404`. A catalogue
  purge (`DELETE …/catalogue/builds/{id}`) of a (source, name)'s last build deletes its decisions as
  the collector's does, so neither path leaves decisions behind. `SetDecision` reads the entry (and
  a skip's version) in its own transaction, and a replacement updates at the row version it read (a
  decision deleted meanwhile is `412`, never an answer and an event without a row). Its reads lock
  (shared, until commit) the entry's builds and the source's upstream or publisher row
  (`EntryVersions`, `LiveSource`; PostgreSQL `FOR SHARE`, SQL Server `HOLDLOCK, ROWLOCK`), and a
  removed source's decisions are deleted after its row, so the collector's, a purge's and a
  removal's deletes wait for a decision being written and delete it with them. The write takes the
  tenant's row (shared) first, which a source's removal takes for update first, and then the source
  row and the builds, which no deleter takes in the other order: no lock cycle. A store test starts
  the delete between the check and the insert on each dialect. The index is
  `(source, tenant_id, package_type, name)`, which a source's delete (`WHERE source = ?`) uses, and
  `ix_decisions_tenant (tenant_id, id)` serves the tenant's list, the reaper's batched deletes and the
  tenant's foreign key.
- **A publisher's signed request** on the decision routes is `401`, as on every management route
  that does not admit publishers (it is not a token there), rather than the `404` the Testing list
  names; an install holder, a distribution admin and the other refusals are `404`.
- **A channel's administrators' events.** The route rule admits a tenant principal without `audit`
  (`channelAdmins`), and the handler checks the subject's channel against `admin` on it. In a `kind`
  list that names other kinds too, the serving plane's kinds (`audit.ServingKinds`: `install`,
  `extension.use`, `usage.receive`) are dropped; a list naming only them is `404`. The answers clear
  `actor_name` and `client`. The store's subject filter gained `SubjectTree` (the exact subject, or the prefix
  `subject/`).
- **Docs.** `administration/upgrading.md` lists `0050_event_pruned` too, which spec 0040's change
  had left out of the `min_reader` table.

### As built (phase 2: the console)

- **The home's reads** (`web/console/src/lib/home.tsx`) are read once for a tenant administrator's
  pages: the catalogue, the signed channels of the default type with each one's sources, follows and
  hold, and every decision (paged). The matrix, the queue and the navigation's count share them, and
  every action reads them again. The queue's groups are pure functions (`lib/queue.ts`) with the unit
  tests.
- **The home.** The console, standalone or embedded, opens a tenant administrator's (and a server
  administrator's opened tenant) on **Catalogue**, with **Needs a decision** under it and its count
  (visually hidden text for screen readers); anyone else's on **Channels**. The micro-frontend's
  default route is the tenant's index, which goes on to the caller's home; the host is told that path,
  never the index.
- **Promote** in the queue names the first channel, by name, holding the waiting version as an
  active or deprecated release that the caller may publish from, until spec 0043 gives the channels
  an order; it is offered only with `promote` on the name in the target, and **Place** only with
  `promote`.
- **Holds** name their version (the catalogue's `held_until`, a rule's `waits_for`): **Place early**
  and **Purge** are offered for that version only, the builds placed nowhere listed before the
  confirmation. A name in no channel links to its page to purge, build by build; nothing purges
  every version from the queue. **Make auto** and **Make manual** are one `PUT …/sources/{name}`. "In
  no channel, or some channels only" counts the signed channels of the default type.
- **Undo** removes each decision as its `DELETE` succeeds; one another administrator changed (`412`)
  or removed (`404`) since is dropped, and the page says so.
- **The rule dialog** says what happens next: nothing by itself under `manual`; under `auto` the
  newest version the channel lacks, at the pass's look once the later of its source's hold
  (`held_until`) and the channel's (from each build's arrival) has passed, or after `waits_until`
  when the rule waits already. Matrix cells with a rule offer **Set rule…**, empty ones **+ Rule…**,
  each labelled with the name and the channel. The "decisions wait" banner counts each group.
- **The entry's page** is `catalogue/{name}` (the old `catalogue/{source}/{name}` shows the same
  page): the rules editor (with each channel's downloads of the name over 30 days, **Promote…**
  from that channel, K4's dialog, and **Clear…**), then each source's section (its licence, the
  licence keys its builds declare from `…/license-keys/{name}?source=`, and its versions), then the
  name's **Reports** (every body's: a report belongs to a body, and the page shows them once). The
  **Report & licence** drawer in the queue shows the licence and reports, its toggle with
  `aria-expanded`.
- **A channel's administrator** who is not an auditor gets an **Events** tab on the channel's page,
  reading `subject=channel:<c>`, which offers neither the serving plane's kinds nor their families.
- **Tests.** Unit tests of the matrix's cells, the queue's groups, Skip and Dismiss with Undo, the
  rule editor's changes and the home by grants (`src/screens/home.test.tsx`); the console end to end
  (`e2e/decisions.spec.ts`) on run.sh's fixture, a manual rule for the tenant's `atlas` in prod and
  enterest-ci's `tiles`, which nightly's follow places once the publisher's day's hold ends: the
  home, a Skip and its Undo, a dismissal of a name in no channel and its Undo, a follow skip's
  dismissal, a block saying what its body is; a stale-ETag Undo is a unit test. `console.spec.ts`
  waits for the channel's page after leaving the home; `mfe.spec.ts` lands on the catalogue.

## Follow-ups

- Version constraints in a rule: a spec of their own (decision Q-D).
- Enterest's side (decision Q-A): its tenant watch (its spec 0012) ignores `system:` events, so it
  takes `source.waiting` explicitly to tell its customers.
- A tenant-wide read of every channel's rules with `current`, `waiting` and the outcome, if the
  matrix's one read per channel becomes slow for tenants with many channels.
- The queue for other package types (spec 0034) in the console.
