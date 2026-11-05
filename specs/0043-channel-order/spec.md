# Spec 0043: The channels' order, a pipeline from nightly to prod

- **Status**: implemented
- **Date**: 2026-11-01
- **Author**: vgsml, Claude

## Summary

A tenant's channels are independent today: `nightly`, `staging` and `prod` are three repositories,
and the order in which a version moves through them lives in the administrators' heads. This spec
lets a tenant record that order: each channel may name its **next** channel. The console draws the
order as a pipeline (the owner's mockup, Catalogue-Pipeline), offers **Promote** from a stage to the
next with the Report & licence panel before it, and the decision queue (spec 0041) names the stage a
waiting version comes from. kista does nothing else with the order: rules, follows, holds and
promotion keep their meaning. Plan item K3, after K1 (spec 0041). The owner's decisions (2026-11-05,
Q-I and Q-J) are at the end.

## Problem

- **No order is recorded.** The mockup's pipeline (`nightly` follows a distributor and takes every
  version, `staging` tries them, `prod` serves them) is a convention. A promotion (spec 0008, `POST
  …/releases/promote`) names its source channel each time; the console's Promote dialog (plan item
  K4) asks for it.
- **The console cannot say "waits for prod, in staging since Oct 24"** without guessing which
  channel comes before `prod`, and cannot lay the channels out as stages.
- Every administrator of the tenant should see the same order, so it belongs in kista, not in a
  browser.

## Design

### The order

A **strict chain** (decision Q-I):

- A signed channel may have one **next** channel: another signed channel of the same tenant and the
  same package type (spec 0034: a promotion stays within a type).
- A channel is the next of at most one channel (at most one **previous**): two channels never
  share a next.
- **No cycle**: following `next` from any channel never comes back to it.
- A **passthrough channel** takes no place in the order (it serves DuckDB's core by its upstream,
  never by promotion).
- A tenant may hold several chains; a chain's first stage is a channel with a next and no previous.

### The API

- `GET /api/v1/tenants/{t}/channel-order[?type=]`: any of the tenant's administrators (admin on the
  tenant, or on any of its channels, as the console's permissions compute it) and its auditors
  (`audit`, spec 0040). The answer lists every edge and an `ETag`:

  ```json
  {"order": [{"channel": "nightly", "next": "staging"}, {"channel": "staging", "next": "prod"}]}
  ```

- `PUT /api/v1/tenants/{t}/channel-order[?type=]` with the whole order of the type (the same body):
  tenant administrators only (it spans channels), `If-Match` optional (`412` when it no longer
  matches). It replaces the type's order in one transaction under the tenant's row lock (channels
  are created under it, spec 0038), checking: every channel named exists, is signed and of the type;
  no channel twice as `channel`, nor twice as `next`; no cycle; a channel is never its own next.
  `400` naming the first problem. An empty list clears the order. Writes `channel.order` (subject
  `tenant:<name>`; data `before` and `after`, the edges, and `package_type` when not the default)
  when something changed, nothing otherwise. `audit.SubjectForm` gains its form; the kind joins the
  events registry and `TestCoverage`.
- Not in the public index: `GET …/channels` and `…/channels/{c}` (spec 0007, public) are unchanged,
  since the order says how an organisation releases, which is not for anonymous callers.
- CLI: `kista admin channel order <t> [-type t]` prints it; `kista admin channel order <t> [-type
  t] nightly staging prod` sets a chain of that package type (the default type without `-type`, as
  the other channel commands take it, spec 0034), and `-clear` clears it.

### Data model

One migration (the next free number), in the three dialects:

```text
channel_next   channel_id (pk, fk channels), next_channel_id (fk channels, unique), set_by, set_at
```

- A table rather than a column on `channels`, so the reaper (spec 0019) deletes it by channel before
  `channels`, and no self-reference on `channels` complicates its deletion. The unique key on
  `next_channel_id` is the chain's "at most one previous".
- **`min_reader` is raised to this migration**: the table references `channels`, and an older
  replica's tenant reaper does not know it and would fail deleting `channels` on PostgreSQL and SQL
  Server (as specs 0032 and 0037 found). The table joins `reapSteps` before `channels` and
  `TestReapCompleteness`.

### What uses the order

Nothing on the server's paths: placement, promotion, holds, retention (spec 0042) and serving read
no order. Promotion keeps its rights and its source named in each request (spec 0008: `promote` on
the target, `publish` on the source channel), so the order grants nothing and a promotion between
channels out of order stays allowed: the order is for display only (decision Q-J).

**When a stage waits.** For a stage S with a previous stage P, and a name S's rule serves from a
source X (a rule naming another source, or no rule, is not a wait: the pipeline shows "serves it
from …" or "no rule"):

- **Per cell.** A cell is one of S's DuckDB versions and a platform (current is per line and
  platform, spec 0042's terms). In each cell, P's version is what P's flat path serves there from X
  (its current, in the full view). The cell waits when that version is newer than everything S's
  cell has or had from X, in any state (yanked, retired and purged included): spec 0041's candidate
  function, applied to P's current instead of the catalogue's entries.
- **S with no release of the name** has had nothing in any cell, so every cell where P has a current
  waits.
- **Per name.** S waits for the name when some cell waits; the version to promote is the newest of
  the cells' waiting versions. A promotion takes every build of that version P serves, all or none
  (spec 0008), so cells where P's current is another version are not changed by it.
- A version a skip covers (spec 0041: the same key, (S, X, name), and not newer than the skipped
  version) is left out, of the stage's mark and of "N versions wait". Skip is offered only where S's
  rule for the name is manual (spec 0041's definition); under an `auto` rule the pass places what
  waits.

The console:

| Mockup screen | Uses |
| --- | --- |
| **Catalogue-Pipeline** | the order (`GET …/channel-order`) for the stages and "Edit the order…" (`PUT`, tenant administrators); each stage's card from its channel's reads (its follows, rules counted by mode, DuckDB versions, hold, names) and its retention in force (spec 0042); the table of names per stage from each channel's `GET …/sources`; "3 versions wait for prod": the stages that wait, as above; **Promote** of the previous stage's current (spec 0008's route, the K4 dialog with its source filled in); the **Report & licence** panel before it (UI only: spec 0035's licence of the source's entry, spec 0011's reports of the version's builds); **Skip** in the panel (spec 0041's decision in the target channel) |
| **Decisions** (spec 0041) | "Promote from staging…": the previous stage, when it holds the waiting version as an active or deprecated release; otherwise another channel that does, or Place from the catalogue. On the pipeline screen only, a version the previous stage holds but does not serve as current is not offered. |
| **Catalogue-Entry**, **Main** | the channel columns in the order's sequence, then the others by name |

The pipeline is a tenant administrator's view: "List / Pipeline" on the Channels screen switches
between today's list and the pipeline (Channels → Pipeline); a tenant without an order shows the list
only, with "Set the order…". A channel's administrator who is not a tenant administrator sees the
list of their own channels only, without the pipeline (spec 0041's "Who sees what"); they may still
read the order through the API.

## Security

- **Advisory.** The order changes no placement, promotion, grant or answer to DuckDB; a wrong order
  misleads the console only, and a tenant administrator corrects it.
- **Who.** Only tenant administrators write it (it spans channels); the tenant's administrators,
  channel administrators included, and its auditors read it. The order names channels, whose names
  are public (spec 0007), and their sequence, which a channel administrator may see. Install
  holders, publishers, distribution admins and anonymous callers get `404` (or `401`) as on any
  management route; the public index is unchanged.
- **Tenant isolation**: every channel named is checked to be the tenant's, in the same transaction.

## Testing

- **Store suite** (three dialects): replacing the order; refused cycles (two and three channels), a
  channel naming itself, two channels with the same next, a passthrough channel, another type's
  channel, another tenant's channel; the reaper and `TestReapCompleteness`.
- **API**: the rights (a channel administrator reads and gets `404` on `PUT`; an install holder and a
  distribution admin `404`), `If-Match`, the no-op `PUT` writing no event, `channel.order` in the
  events registry, `SubjectForm` and `TestCoverage`.
- **CLI**: a chain set and printed, `-clear`, `-type`.
- **Console**: unit tests of the pipeline from fixtures (stages; a cell waiting when the previous
  stage's current is newer than everything the stage's cell had, a yanked or purged one included; a
  stage with no release of the name; two cells with different waiting versions, the newest offered;
  a skipped version left out of the count; Promote's source and version filled in from the previous
  stage; a channel administrator seeing the list only); the
  console end to end: set `nightly → staging → prod`, see a version wait for `staging`, open Report &
  licence, promote it, and Skip another.

## Decisions

The owner's decisions of 2026-11-05, which the text above follows:

- **Q-I. The order's shape.** A strict chain: each channel has at most one next and at most one
  previous. Ruled out: several channels sharing a next (fan-in), and named pipelines.
- **Q-J. What kista does with the order.** Display only: the console's pipeline and its defaults
  (the source of the Promote dialog). Ruled out: a promotion defaulting to the previous stage on the
  server, and automatic promotion between stages.

## Alternatives considered

- **A `next_channel_id` column on `channels`**: one join less, but a self-reference on the table
  every tenant deletion and channel read touches.
- **Named pipelines** (`pipelines` with ordered stages, a channel in several of them): more than the
  mockup needs.
- **The order in the console's local storage**: different for each administrator and browser.
- **An order number per channel** (`stage: 1, 2, 3`): cannot say which channel follows which when
  two chains share a tenant.

## Delivery

One PR, `0043-channel-order`, after spec 0041's: the table, the routes, the CLI, the console's
pipeline, the tests. The website: `tenants-and-channels.md` (the order), `console.md` (the
pipeline), `api.md`, `administration/commands.md`, `observability.md` (`channel.order`),
`administration/upgrading.md` (the migration's `min_reader`).

Delivered on one branch, `0043-channel-order-impl`.

### As built

- **The table** is migration `0052_channel_next` (`min_reader` 52), with the unique key
  `ux_channel_next_next` on `next_channel_id`. The reaper deletes it by channel before
  `channel_sources` and `channels`; `TestReapCompleteness` and `TestReapTenant` take it.
- **One check, in the transaction.** `store.SetChannelOrder` reads the tenant's channels, checks the
  edges in the order sent (each name the tenant's, signed, of the type; no channel its own next;
  none twice as a channel nor as a next; then no cycle) and answers `ErrInvalid` naming the first
  problem. It replaces the type's edges by deleting them and inserting the new ones, so a reversed
  chain passes the unique key. An unchanged order (compared by channel) writes nothing and no
  event. The service runs it under the tenant's row lock (`InTenantTxForUpdate`, as channels are
  created).
- **The ETag** is a hash of the edges (`"o<hex>"`): the table keeps no row version, and an order
  set again unchanged keeps its ETag. `If-Match` is compared in the transaction.
- **Who reads.** The route admits any of the tenant's principals that is not a publisher
  (`channelAdmins`); the service admits `audit` on the tenant (its administrators and auditors), or
  `admin` on one of its channels (one authorisation per channel: a grant on the channel, as the
  console's `channelAdmin`, never one on an extension or a group in it). An install holder, an
  extension administrator and a distribution admin get `404`; a caller without a token of the
  tenant `401`.
- **`channel.order`** carries `before` and `after` sorted by channel, `[]` for none.
- **The CLI** prints one chain per line, `nightly -> staging -> prod`; setting takes two channels
  or more (one is a usage error), `-clear` none.
- **The console.** The home's reads read the order once (`GET …/channel-order`) and list the signed
  channels in its sequence (`inOrder`): the matrix's columns, the entry page's rules, the queue's
  channel filter. The queue's **Promote** names the item's previous stage first when it holds the
  version (`promoteFrom`). Channels shows a tenant administrator **Set the order…** while there is
  none, then **List** and **Pipeline** (`?view=pipeline`); the order dialog lists each signed
  channel of the default type with its next, and sends the whole order at the ETag read when it
  opened. The pipeline (`screens/Pipeline.tsx`) draws each chain's stage cards (follows, rules
  counted by mode, DuckDB versions from the channel's public detail, hold, names, "N versions
  wait") and a table per stage (rule, current, "serves it from …", "no rule", what waits).
- **What waits, in the console** (`lib/order.ts`: `cellWaits`, `stageWait`). For a stage's rule of
  source X whose previous stage selects X and has a `current`, the console reads both stages' index
  of the name (`GET …/channels/{c}/extensions/{name}`, a tenant administrator's full view): the
  previous stage's current per cell is a release's `current_for`, and what the stage's cell has or
  had is its releases' `serves` and `would_serve` (active, deprecated, yanked, withdrawn) and its
  `purged` (below). A stage's index holds only its selected source, which is X there. Each (channel,
  name) is read once.
- **Purged releases** (a coordination fix: the Design counts them, no answer carried them). A
  purge (spec 0016 phase 2) deletes the release and keeps its slot in `purged_slots`, which only the
  placement pass's look read. The name's index (`GET …/channels/{c}/extensions/{name}`) now
  answers the name's administrators `purged: [{"version", "duckdb_version", "platform"}]`
  (`store.PurgedSlots`): the channel's purged slots of its selected source and those purged before
  slots had a source (`*`), each on its slot's DuckDB version, of the channel's DuckDB versions,
  filtered by `duckdb_version` and `platform` as the rows are. A C API build's slot names no DuckDB
  version and is left out, as the pass's `newServed` leaves it out. Install holders and anonymous
  callers get no such member; it is read-only and bounded as the rows are, one name of one
  channel. The console counts these versions as had, so a version the stage purged never waits
  again. Retired versions (spec 0042) are to come with that spec.
- **The panel.** **Report & licence** shows the entry's licence (`SourceLicence`) and the reports of
  the bodies of the version's builds in the previous stage, then **Promote…** (K4's dialog, made
  `PromoteDialog`, its source the previous stage, its target the stage, the version filled in; with
  `promote` on the name in the stage and `publish` in the previous one) and **Skip** under a manual
  rule.
- **Not shown: the stage's retention** (spec 0042), which is not implemented yet: the card gains it
  with spec 0042's console.
- **Tests.** Store (three dialects): `TestChannelOrder` (replacement, the refusals, a reversed
  chain, another type's order apart, clearing, the migration's `min_reader`), `TestPurgedSlots`
  (the source's and `*`, never another source's, name's or channel's), the reaper's.
  API: `TestChannelOrderRoutes` (the readers, `404` and `401`, `If-Match` and `412`, `304`, the
  refusals, no event for an unchanged order, `channel.order`'s data, the public list unchanged);
  `TestIndexPurged` (`purged` for the name's administrators, filtered by cell, none for an install
  holder or an anonymous caller).
  `channel.order` in the events registry, `SubjectForm` and `TestCoverage`. CLI: `TestAdminCLI` and
  `TestAdminPackageTypes` (`-type`). Console: `src/screens/pipeline.test.tsx` (the chains and the
  columns' sequence; the cells, a yanked, deprecated and purged version counted as had; a stage
  with no release; the newest of two cells; a skip; Promote
  filled in from the previous stage; Skip; the order dialog with its ETag; a channel administrator's
  list); end to end, `e2e/pipeline.spec.ts` on run.sh's fixture (staging, and the tenant's relay and
  beacon placed into nightly under staging's manual rules): the order set, two versions waiting
  for staging, relay promoted after its panel, beacon skipped, the catalogue's columns in order.

## Follow-ups

- The order in the bootstrap (spec 0024): a tenant section listing its channels' order.
