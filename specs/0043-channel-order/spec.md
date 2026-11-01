# Spec 0043: The channels' order, a pipeline from nightly to prod

- **Status**: draft
- **Date**: 2026-11-01
- **Author**: vgsml, Claude

## Summary

A tenant's channels are independent today: `nightly`, `staging` and `prod` are three repositories,
and the order in which a version moves through them lives in the administrators' heads. This spec
lets a tenant record that order: each channel may name its **next** channel. The console draws the
order as a pipeline (the owner's mockup, Catalogue-Pipeline), offers **Promote** from a stage to the
next with the Report & licence panel before it, and the decision queue (spec 0041) names the stage a
waiting version comes from. kista does nothing else with the order: rules, follows, holds and
promotion keep their meaning. Plan item K3, after K1 (spec 0041). The owner's open questions are at
the end (Q-I, Q-J).

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

As recommended in Q-I, a **strict chain**:

- A signed channel may have one **next** channel: another signed channel of the same tenant and the
  same package type (spec 0034: a promotion stays within a type).
- A channel is the next of at most one channel (at most one **previous**). Fan-in (`dev-a` and
  `dev-b` both before `staging`) can be allowed later without a migration: it only drops this check.
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
- CLI: `kista admin channel order <t>` prints it; `kista admin channel order <t> nightly staging
  prod` sets a chain, and `-clear` clears it.

### Data model

One migration (the next free number), in the three dialects:

```text
channel_next   channel_id (pk, fk channels), next_channel_id (fk channels, unique), set_by, set_at
```

- A table rather than a column on `channels`, so the reaper (spec 0019) deletes it by channel before
  `channels`, and no self-reference on `channels` complicates its deletion. The unique key on
  `next_channel_id` is the chain's "at most one previous" (dropped if fan-in is allowed).
- **`min_reader` is raised to this migration**: the table references `channels`, and an older
  replica's tenant reaper does not know it and would fail deleting `channels` on PostgreSQL and SQL
  Server (as specs 0032 and 0037 found). The table joins `reapSteps` before `channels` and
  `TestReapCompleteness`.

### What uses the order

Nothing on the server's paths: placement, promotion, holds, retention (spec 0042) and serving read
no order. Promotion keeps its rights and its source named in each request (spec 0008: `promote` on
the target, `publish` on the source channel), so the order grants nothing and a promotion between
channels out of order stays allowed (Q-J).

**A stage waits** when, for a name both stages serve from the same source, the previous stage's
current version (spec 0006: what its flat path serves) is newer than the stage's own; the version to
promote is that current version of the previous stage. (A version the previous stage holds but does
not serve as current is not offered.)

The console:

| Mockup screen | Uses |
| --- | --- |
| **Catalogue-Pipeline** | the order (`GET …/channel-order`) for the stages and "Edit the order…" (`PUT`, tenant administrators); each stage's card from its channel's reads (its follows, rules counted by mode, DuckDB versions, hold, names) and its retention in force (spec 0042); the table of names per stage from each channel's `GET …/sources`; "3 versions wait for prod": the stages that wait, as above; **Promote** of the previous stage's current (spec 0008's route, the K4 dialog with its source filled in); the **Report & licence** panel before it (UI only: spec 0035's licence of the source's entry, spec 0011's reports of the version's builds); **Skip** in the panel (spec 0041's decision in the target channel) |
| **Decisions** (spec 0041) | "Promote from staging…": the previous stage, when it holds the waiting version as an active or deprecated release; otherwise another channel that does, or Place from the catalogue |
| **Catalogue-Entry**, **Main** | the channel columns in the order's sequence, then the others by name |

"List / Pipeline" on the Channels screen switches between today's list and the pipeline; a tenant
without an order sees the list only, with "Set the order…" for a tenant administrator.

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
- **CLI**: a chain set and printed, `-clear`.
- **Console**: unit tests of the pipeline from fixtures (stages, a stage waiting when the previous
  stage's current is newer, Promote's source and version filled in from the previous stage); the
  console end to end: set `nightly → staging → prod`, see a version wait for `staging`, open Report &
  licence, promote it, and Skip another.

## Open questions

- **Q-I. The order's shape.**
  - *Recommended*: a strict chain: each channel has at most one next and at most one previous.
    Fan-in can be added later without a migration (the unique key on `next_channel_id` is dropped).
  - *Alternative*: one next per channel, several channels allowed to share a next (`dev-a` and
    `dev-b` before `staging`).
- **Q-J. Should kista use the order?**
  - *Recommended*: display only: the console's pipeline and its defaults.
  - *Alternative 1*: a promotion without a source (`from` left out) promotes from the previous stage.
  - *Alternative 2*: automatic promotion between stages (for example once the reports are in), a
    follow-up spec.

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

## Follow-ups

- The order in the bootstrap (spec 0024): a tenant section listing its channels' order.
- Promotion defaulting to the previous stage, and automatic promotion between stages (Q-J).
- Fan-in, if the owner chooses it (Q-I).
