# Spec 0041: The catalogue as the console's home, and the decision queue

- **Status**: draft
- **Date**: 2026-11-01
- **Author**: vgsml, Claude

## Summary

The console's tenant home becomes the catalogue: a matrix of every (source, name) against the
tenant's channels, showing what each channel serves and what waits. Beside it, **Needs a decision**
gathers what kista leaves to an administrator: new versions under manual rules, holds ending, names
a follow skips, names no channel takes, failed placements. Most of it is the console reading the
API that specs 0033, 0035 and 0011 built. This spec adds the four small server pieces the owner's
mockup (2026-10-26) asks for: a held version's end in the catalogue's list, an event when a new
version waits under a manual rule, **decisions** (Skip and Dismiss, remembered in kista and the same
for every administrator), and a block that says what its body is. Plan item K1.

## Problem

The console (specs 0015 and 0033 phase C, the gaps closed by plan item K4) has a Catalogue section
and a Sources tab per channel. An administrator can see each piece, but not the tenant as a whole,
and some pieces cost one request per row:

- **Holds.** `GET …/catalogue` lists each entry's `held` versions, not when they stop being held.
  The time is on each build (`available_at`), so "held until Oct 27, 11:00" in a list means one read
  of `…/catalogue/{source}/extensions/{ext}` per held entry.
- **Nobody is told.** A manual rule means "I choose the versions". When a newer version becomes
  available, the administrator learns of it only by looking (spec 0033's follow-up: "Notifying a
  channel's administrators of a new version under a `manual` rule").
- **Choices are not remembered.** An administrator who decides not to take `acl 0.4.0` into `prod`,
  or who has seen that a follow skips `h3` and is fine with it, has nowhere to record it. A queue kept
  in one browser differs for each administrator and is lost on the next sign-in.
- **A block is a hash.** `GET …/blocks` answers body hashes and reasons. To show "tresor 1.3.0,
  linux_amd64, publisher:ci" the console searches the catalogue per hash.

Two more items in the mockup's notes need nothing new:

- **Event kind families** (`release.*` in the Events filter): spec 0040 made `kind` a list of exact
  kinds and families (`kind=release.*`, and exclusions with `!`). It is implemented, so it is UI
  only here.
- **The public-licence warning** (spec 0035 phase 2): implemented, and the console already shows
  its `warnings`.

## Design

### What the mockup's screens take

| Screen (mockup artboard) | From this spec | UI only, on today's API |
| --- | --- | --- |
| **Main** (the catalogue matrix, the tenant's home) | `held_until` in `GET …/catalogue` | the matrix from `GET …/catalogue` and each signed channel's `GET …/sources` and `GET …/follows`; `+ Rule…` and `Set rule…` (`PUT …/sources/{name}`); the "decisions wait" banner (counted as the queue below) |
| **Catalogue-Entry** (one page per name, its sources side by side) | none | each channel's rule set in place (`PUT …/sources/{name}`, `PUT …/follows/{source}`); Place, Promote and Clear; licences per entry (`GET …/licenses?source=`, `…/license-keys?source=`, spec 0035); reports per build (`GET …/extensions/{ext}/attachments?body_hash=`, spec 0011); downloads (`GET …/stats/downloads?extension=&channel=`). The "Take only versions matching" field waits for version constraints (open question 2) and is not shown. |
| **Decisions** (the queue) | decisions (Skip, Dismiss, Undo); `held_until`; the `source.waiting` event behind "Tell me of these…" (open question 1) | the groups, computed below; Promote (K4's dialog; K3 names the previous stage), Make auto or manual (`PUT …/sources/{name}`), Place early (`…/releases/place` with `early`), Purge (`DELETE …/catalogue/builds/{id}`), Open the block; the Report & licence drawer (specs 0035 and 0011) |
| **Decisions-Rule** (set a rule from the queue) | the notify checkbox only if the owner chooses per-rule settings (open question 1) | the dialog and its `PUT …/sources/{name}`; "What happens" computed from the holds (`hold` of the source and the channel) |
| **Org-Blocks** | `builds` in the block answers | Block…, Unblock (`POST`, `DELETE …/blocks`) |
| **Org-Events** | none | the family chips send spec 0040's `kind` list |

The **Catalogue-Pipeline** screen is spec 0043's (it needs the channels' order); its Retention line
is spec 0042's. The **Retention** screen is spec 0042's. The mockup's other screens (Developers,
Developer-Page, Org-Access, Org-Groups, Org-Identity, Org-Licences, Org-Stats and the Gap-* boards)
are UI only on today's API, or plan item K4's (done), and are not this spec's.

### The queue, as the console computes it

All from reads an administrator already has, per signed channel of the default package type (other
types through the API, as the Catalogue section does today):

| Group | Found from | Actions |
| --- | --- | --- |
| New versions under manual rules | a rule with mode `manual` (or `follow` acting as manual) whose `newest` is newer than its `current` (spec 0033's sources answer), less those a **skip** covers | Promote from the channel that holds it (K3: the previous stage), Skip, Make auto |
| Holds ending within a day | the catalogue's `held_until`, for a (source, name) an `auto` rule or a follow takes | Place early, Purge |
| Names a follow skips | `GET …/follows/{source}`'s skipped names, less those a channel **dismissal** covers | Set rule, Dismiss |
| In no channel | catalogue entries with no rule in any channel (the catalogue's `rules`), or taken by some channels only, less those a tenant **dismissal** covers | Place, Purge, Set rule, Dismiss |
| Placements that failed | a rule's last `outcome` | Open the block, Make manual |

"Within a day" is the console's display choice, not a server setting. The console reads the
decisions once (`GET …/decisions`; a channel's administrator who is not a tenant administrator,
each of its channels' `GET …/channels/{c}/decisions`) and filters in the page.

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

### Telling of a version that waits under a manual rule

- **The event `source.waiting`** (subject `channel:<c>/ext:<name>`, actor `system:placement`; data
  `source`, `version`, `current`, `mode`, and `package_type` when not the default): written by the
  placement pass when its look at a rule of mode `manual`, or of mode `follow` while the channel no
  longer follows the source, finds the rule's `newest` (spec 0033: available, not held, withdrawn
  nor revoked) newer than its `current` (spec 0006's version order, as the sources answer compares
  them) and newer than the version the rule last told of.
- **Once per version.** `channel_sources.told_version` keeps the version the last event named; a
  source switch clears it. The first look after a rule becomes manual tells of the newest version if
  it waits already.
- **When.** The pass already looks at manual rules when their source has new builds (to complete a
  chosen version). A newer version whose hold ends later wakes the rule then: the look records
  `due_at` for the end of the hold of the newest held version newer than `current`, as it does for
  the holds an `auto` rule waits for. The hourly sweep catches the rest.
- **Not for the `tenant` source.** The pass never looks at its rules (spec 0033: `tenant` is never
  automatic, and the pass skips it). Its waiting versions are in the queue, from the reads, but no
  event tells of them.
- **Who reads it**: whoever reads the tenant's events (`audit`, spec 0040's reader, and the tenant's
  administrators) and the operator's sinks (spec 0010). A channel's administrator who is not a
  tenant administrator does not read events today. Delivering the event to a person (mail, chat) is
  outside kista unless the owner decides otherwise (open question 1).

### Decisions

A **decision** is an administrator's answer to a queue item, kept in kista so every administrator
sees the same queue:

| Kind | Scope | Means | The item comes back when |
| --- | --- | --- | --- |
| `skip` | a channel, a source, a name, a version | "the channel does not take this version" | a version newer than the skipped one waits |
| `dismiss` | a channel, a source, a name, the newest version at the time | "this follow skip is fine" | a newer version of the name arrives from the source |
| `dismiss` | the tenant (no channel), a source, a name, the newest version at the time | "this name stays out of the channels" (In no channel) | a newer version arrives |

- **One decision per (channel or tenant, kind, source, name)**: a later decision replaces it (a skip
  of `0.4.1` replaces the skip of `0.4.0`). Only the newest waiting version is ever shown, so
  remembering one version per item is enough, and the rows are bounded by the tenant's channels
  times its catalogue entries: no ceiling and no cleanup pass are needed.
- **A decision changes nothing else.** A skip does not stop a placement by hand or a promotion, a
  manual rule places nothing by itself anyway, and an `auto` rule has nothing to skip. A dismissal
  does not change a follow. The decision only shapes the queue.
- **Routes** (answers: `id`, `kind`, `channel` when scoped to one, `source` as answers name it,
  `name`, `version`, `note` when given, `decided_by` masked as `created_by` is (spec 0040),
  `decided_at`, `package_type` when not the default):
  - `GET /api/v1/tenants/{t}/channels/{c}/decisions`: the channel's administrators.
  - `PUT /api/v1/tenants/{t}/channels/{c}/decisions` with `{"kind": "skip"|"dismiss", "source": …,
    "name": …, "version": …, "note"?: …}`: the channel's administrators. It writes or replaces
    the decision of (channel, kind, source, name), and answers it (`200`; `201` when new).
  - `DELETE /api/v1/tenants/{t}/channels/{c}/decisions/{id}`: the channel's administrators (Undo).
  - `GET /api/v1/tenants/{t}/decisions[?type=]`: the tenant's administrators and its auditors
    (`audit`, spec 0040): every decision of the tenant, the channels' included.
  - `PUT /api/v1/tenants/{t}/decisions` with `{"kind": "dismiss", "source", "name", "version",
    "note"?, "type"?}` and `DELETE /api/v1/tenants/{t}/decisions/{id}`: the tenant's
    administrators (a tenant-wide dismissal; a tenant administrator also removes a channel's).
- **Checks** (shape and existence, `400` or `404` as the catalogue's routes answer): the source is
  one of the tenant's live sources, and the version is one of its versions of the name in the
  catalogue (any state), of the channel's package type; a `skip` only in a signed channel; `note`
  at most 400 bytes of printable text (a format bound, as a withdrawal's reason). A decision on a
  removed source cannot be written.
- **Gone with what they name**: removing a source deletes its decisions (as its follows and skips,
  spec 0033 part 4); tenant deletion (spec 0019) reaps them.
- **Events** `decision.set` (with `before` when it replaced one) and `decision.clear`, subject
  `channel:<c>/ext:<name>` or `ext:<name>`, data `kind`, `source`, `version`, `package_type`.
- **CLI**: `kista admin decision list <t>[/<c>]`, `kista admin decision skip <t>/<c> <name>
  <version> -source <s> [-note …]`, `kista admin decision dismiss <t>[/<c>] <name> <version> -source
  <s>`, `kista admin decision remove <t> <id>`.

### What a blocked body is

`GET …/blocks` and `GET …/blocks/{hash}` answer each block with `builds`: the tenant's catalogue
builds with that body, any state (`source` as answers name it, `name`, `version`, `platform`,
`slot`, `package_type` when not the default), by source and name. A body nothing in the tenant holds
(blocked ahead of time, or whose builds the collector removed) answers `"builds": []`. One query
joining `blocks` to `builds` by (tenant, body hash) for the list. `kista admin block list` shows
`WHAT` (the first build, and `+n`).

### Data model

One migration (the next free number), in the three dialects, additive only:

```text
channel_sources   + told_version   text NULL      -- the version the last source.waiting named
decisions         id, tenant_id, package_type, channel_id NULL (fk channels), kind ('skip'|'dismiss'),
                  source, name, package_version, note NULL, decided_by, decided_at
                  unique (channel_id, kind, source, name)              where channel_id IS NOT NULL
                  unique (tenant_id, package_type, kind, source, name) where channel_id IS NULL
```

The two unique keys are partial (filtered on SQL Server), as the catalogue's slot key is (spec
0033 A1 part 3a); text columns BIN2 on SQL Server. `min_reader`: not raised; an older replica writes
no `told_version` (an event told twice at most during a rollout) and knows no decisions (its console
shows a longer queue). Tenant deletion reaps `decisions` before `channels`.

### The console

- **The tenant's home** is the catalogue matrix (Main): rows (source, name) with the entry's icon
  and summary (spec 0036); a column per signed channel of the default package type with its follow
  and hold in the header; each cell the rule's mode and current version, `→ waits` for a newer one
  (less a skip), a held version with `held_until`, a failed outcome, "serves it from …" when the
  channel's rule names another source, the follow's skip with its reason, and `+ Rule…`. Filters by
  source. "Needs a decision" sits under Catalogue in the navigation, with its count.
- **The entry's page** is per name, every source side by side (today it is per (source, name)): each
  channel's rule edited in place (unsaved changes listed, one `PUT …/sources/{name}` per changed
  channel on Save), each source's licence and its versions' reports.
- **Needs a decision**: the groups above, filtered by channel; each action as listed; Skip and
  Dismiss write a decision, Undo removes the decisions this page wrote; the Report & licence drawer.
- **Blocks** shows "What it is" from `builds`. **Events** offers the families as chips.
- The console never offers an action its caller may not take (`mayPromote`, a channel
  administrator's `early`, as today) and shows the API's refusal otherwise.

## Security

- **Decisions are advisory.** They change no placement, rule, follow, grant or serving answer; the
  worst a wrong decision does is hide a queue item from administrators, who can remove it. Writing
  one takes admin on the channel (or the tenant), as setting the rule it is about does.
- **No existence leak.** Every new route is a management route: install holders and anonymous
  callers get `404` (or `401` without a valid token) as on any other; a channel's administrator
  reads only its channel's decisions; a distribution admin reads none (it reads no channel, spec
  0033 B), and its view of the catalogue gains `held_until` only for the entries it already sees.
- **Blocks' `builds`** are the tenant's own catalogue rows, read by its administrators, who already
  read them through the catalogue; nothing of another tenant sharing the body in a storage domain.
- **The event** carries versions and a source label, the facts the sources answer shows; it is the
  tenant's, read by its auditors and the operator's sinks.
- **Fail closed**: a failure writing `told_version` or the event is the look's error (the rule is
  looked at again); a decision that fails to write leaves the item in the queue.

## Testing

- **Store suite** (three dialects): `held_until` per version (two builds of one version, one held
  longer; a hold ended; a raised hold); decisions: a replacement by (scope, kind, source, name), the
  two partial keys (a channel's and the tenant's decision of one name both kept), removal with the
  source, the reaper; `told_version` cleared by a source switch; blocks' `builds` (two sources of
  one body, none).
- **The placement pass**: `source.waiting` once per version for a manual rule (a second look writes
  none; a newer version writes one; a version arriving held is told when its hold ends, woken by
  `due_at`); none for `auto`, for a follow acting as `auto`, for a rule without a mode, for
  `tenant`; one for a follow's rule after the follow is removed.
- **API**: the decision routes (a channel administrator on its channel and `404` on another; a
  tenant administrator everywhere; an auditor reads `GET …/decisions` and writes nothing; a
  distribution admin, an install holder and a publisher `404`); the checks (`400`, `404`); events
  `decision.set` and `decision.clear` in the events registry and `TestCoverage`; `held_until` in the
  catalogue answer; `builds` in the block answers.
- **CLI**: `decision list|skip|dismiss|remove`, `catalogue list`'s `HELD`, `block list`'s `WHAT`.
- **Console**: unit tests of the matrix and the queue's groups from fixtures (a skip hides a version
  until a newer one, a dismissal until a newer version); the console end to end
  (`web/console/e2e`): the home matrix of the existing fixture (`nightly` following a distributor's
  publisher with a day's hold), a Skip remembered across a reload and by a second administrator,
  Undo, a dismissal of the follow's skip, a block showing what it is.
- No DuckDB end to end changes: nothing served changes.

## Open questions

1. **How is an administrator told, and where do notification settings live?** The mockup has "Tell
   me of these…" on the queue and "Tell staging's administrators when a new version of it waits"
   in the rule dialog.
   - *Recommended*: kista writes the event `source.waiting` for every manual rule, always, and
     keeps no notification settings. Delivery is the reader's: a self-hosted organisation sends
     events to its log pipeline (spec 0010's sinks) and alerts there; Enterest (closed) reads the
     events with spec 0040's cursor and mails or messages its customers' administrators, keeping
     their settings. The console's "Tell me of these…" explains this (and, in Enterest's shell,
     opens Enterest's settings). The rule dialog's checkbox is dropped.
   - *Alternative A*: the same event, plus a per-rule `notify` flag (`channel_sources.notify`),
     so the event is written only for rules an administrator asked about.
   - *Alternative B*: kista delivers: per-tenant webhooks (`PUT …/tenants/{t}/notifications` with
     a URL, the event kinds, a signing secret; egress-checked, retried), a spec of its own.
   - *Alternative C*: kista keeps per-administrator subscriptions (principal, kinds, channels) for
     a deliverer to read. kista knows no mail addresses, so a deliverer is still needed.
2. **Version constraints in a rule** (`auto` within `1.x`; the entry page's "Take only versions
   matching"): in this spec, or deferred?
   - *Recommended*: deferred to a spec of its own, after K1. They change the placement pass (what
     `auto` and a follow's rules take, `newest`, the due work), need a grammar for versions that are
     not semver (DuckDB's core extensions use commit hashes), and no K1 screen needs them except
     that one field, which stays hidden until then.
   - *Alternative*: a minimal form here: a version prefix per rule (`"take": "1."`), with `auto`
     taking only versions starting with it and the sources' `newest` filtered by it.
3. **Who reads the decisions?**
   - *Recommended*: the channels' administrators read their channels' decisions, and the tenant's
     administrators and auditors read all of them.
   - *Alternative*: administrators only, no auditors.

## Alternatives considered

- **The queue computed in kista** (`GET …/decisions/queue`): one read for the console, but a
  second place encoding what the console shows, and a server answer to keep in step with the
  screens. The reads already exist; only the decisions need storing.
- **Decisions per (channel, name, version)**, one row per skipped version, as the mockup's note
  says: the queue only shows the newest waiting version, so older rows would need a cleanup pass
  or a ceiling. One row per item, holding the version decided on, behaves the same.
- **Decisions in the browser's storage**: different for each administrator and each browser, which
  is what the owner's note rules out.
- **A skip that stops placements**: it would make a skip a second kind of rule; a manual rule
  already places nothing by itself, and a block or a yank stops a version for good.
- **Blocks recording what the body was at block time**: survives the collector, but the answer is
  then a copy that can go stale (a body arriving later under another name); the join answers what
  the tenant holds now, and an attachment on the block keeps the evidence (spec 0011).

## Delivery

One PR, `0041-catalogue-home` (the API pieces, the migration, the CLI, the console screens, the
tests); or two if the console part is large: first the API (held_until, `source.waiting`,
decisions, blocks' `builds`), then the console. The website: `catalogue.md` (`held_until`, the
waiting event), `console.md` (the home, the queue, decisions), `api.md` (the routes),
`observability.md` (`source.waiting`, `decision.*`), `administration/commands.md`.

## Follow-ups

- Version constraints in a rule (open question 2).
- Delivery of notifications by kista, if the owner chooses it (open question 1, alternative B).
- A tenant-wide read of every channel's rules with `current`, `newest` and the outcome, if the
  matrix's one read per channel becomes slow for tenants with many channels.
- The queue for other package types (spec 0034) in the console.
