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
promotion keep their meaning. Plan item K3, after K1 (spec 0041).

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

- A signed channel may have one **next** channel: another signed channel of the same tenant and the
  same package type (spec 0034: a promotion stays within a type). Several channels may share a next
  (`dev-a` and `dev-b` both before `staging`); a channel names at most one (open question 1).
- **No cycle**: following `next` from any channel never comes back to it.
- A **passthrough channel** takes no place in the order (it serves DuckDB's core by its upstream,
  never by promotion).
- A **stage's previous** channels are those naming it as next; the pipeline's first stages are the
  channels with a next and no previous.

### The API

- `GET /api/v1/tenants/{t}/channel-order[?type=]`: the tenant's administrators and its auditors
  (`audit`, spec 0040). The answer lists every edge and an `ETag`:

  ```json
  {"order": [{"channel": "nightly", "next": "staging"}, {"channel": "staging", "next": "prod"}]}
  ```

- `PUT /api/v1/tenants/{t}/channel-order[?type=]` with the whole order of the type (the same body):
  tenant administrators (open question 2), `If-Match` optional (`412` when it no longer matches).
  It replaces the type's order in one transaction under the tenant's row lock (channels are created
  under it, spec 0038), checking: every channel named exists, is signed and of the type; no channel
  twice as `channel`; no cycle; a channel is never its own next. `400` naming the first problem. An
  empty list clears the order. Writes `channel.order` (subject `tenant`; `before` and `after`, the
  edges) when something changed, nothing otherwise.
- Not in the public index: `GET …/channels` and `…/channels/{c}` (spec 0007, public) are unchanged,
  since the order says how an organisation releases, which is not for anonymous callers.
- CLI: `kista admin channel order <t>` prints it; `kista admin channel order <t> nightly staging
  prod` sets a chain (the common case), and `-clear` clears it; a fan-in order goes through the
  API.

### Data model

One migration (the next free number), in the three dialects, additive:

```text
channel_next   channel_id (pk, fk channels), next_channel_id (fk channels), set_by, set_at
```

A table rather than a column on `channels`, so the reaper (spec 0019) deletes it by channel before
`channels`, and no self-reference on `channels` complicates its deletion. `min_reader` is not
raised: an older replica ignores the table (the console of an older replica shows no pipeline).

### What uses the order

Nothing on the server's paths: placement, promotion, holds, retention (spec 0042) and serving read
no order. Promotion keeps its rights and its source named in each request (spec 0008: `promote` on
the target, `publish` on the source channel), so the order grants nothing and a promotion between
channels out of order stays allowed (open question 3).

The console:

| Mockup screen | Uses |
| --- | --- |
| **Catalogue-Pipeline** | the order (`GET …/channel-order`) for the stages and "Edit the order…" (`PUT`); each stage's card from its channel's reads (its follows, rules counted by mode, DuckDB versions, hold, names) and its retention in force (spec 0042); the table of names per stage from each channel's `GET …/sources`; "3 versions wait for prod": a stage's manual rule whose `newest` is what the previous stage holds; **Promote** from the previous stage (spec 0008's route, the K4 dialog with its source filled in); the **Report & licence** panel before it (UI only: spec 0035's licence of the source's entry, spec 0011's reports of the version's builds); **Skip** in the panel (spec 0041's decision in the target channel) |
| **Decisions** (spec 0041) | "Promote from staging…": the previous stage that holds the version; without an order, the channel that holds it |
| **Catalogue-Entry**, **Main** | the channel columns in the pipeline's order, then the others by name |

"List / Pipeline" on the Channels screen switches between today's list and the pipeline; a tenant
without an order sees the list only, with "Set the order…".

## Security

- **Advisory.** The order changes no placement, promotion, grant or answer to DuckDB; a wrong order
  misleads the console only, and a tenant administrator corrects it.
- **Who.** Only tenant administrators write it (it spans channels); auditors and administrators read
  it; a channel's administrator who is not a tenant administrator neither reads nor writes it (open
  question 2), and the console then shows no pipeline. Install holders, publishers, distribution
  admins and anonymous callers get `404` (or `401`) as on any management route; the public index is
  unchanged.
- **Tenant isolation**: every channel named is checked to be the tenant's, in the same transaction.

## Testing

- **Store suite** (three dialects): replacing the order; refused cycles (two and three channels), a
  channel naming itself, a passthrough channel, another type's channel, another tenant's channel;
  fan-in kept; the reaper.
- **API**: the rights (`404` for a channel administrator, an install holder, a distribution admin),
  `If-Match`, the no-op `PUT` writing no event, `channel.order` in the events registry and
  `TestCoverage`.
- **CLI**: a chain set and printed, `-clear`.
- **Console**: unit tests of the pipeline from fixtures (stages, waits, Promote's source filled in
  from the previous stage); the console end to end: set `nightly → staging → prod`, see a version
  wait for `staging`, open Report & licence, promote it, and Skip another.

## Open questions

1. **The order's shape.**
   - *Recommended*: one next per channel, several channels may share a next (fan-in), no cycles.
     The mockup's single chain is the common case, and two development channels feeding one staging
     channel need no second concept.
   - *Alternative A*: a strict chain (one next and at most one previous per channel).
   - *Alternative B*: named pipelines (`pipelines` with ordered stages), a channel in several of
     them: more than the mockup needs.
2. **Who sets the order, and who reads it?**
   - *Recommended*: tenant administrators set it; tenant administrators and auditors read it.
   - *Alternative*: a channel's administrators set their channel's next (and read the order), the
     cycle check spanning channels they do not administer.
3. **Should kista use the order?**
   - *Recommended*: not now: display and the console's defaults only.
   - *Alternative*: a promotion without a source (`from` left out) promotes from the previous stage,
     and later rules such as "promote to the next stage once the reports are in". Either is a
     follow-up spec once the order exists.

## Alternatives considered

- **A `next_channel_id` column on `channels`**: one join less, but a self-reference on the table
  every tenant deletion and channel read touches.
- **The order in the console's local storage**: different for each administrator and browser.
- **An order number per channel** (`stage: 1, 2, 3`): cannot say which channel follows which when
  two chains share a tenant.

## Delivery

One PR, `0043-channel-order`, after spec 0041's: the table, the routes, the CLI, the console's
pipeline, the tests. The website: `tenants-and-channels.md` (the order), `console.md` (the
pipeline), `api.md`, `administration/commands.md`, `observability.md` (`channel.order`).

## Follow-ups

- The order in the bootstrap (spec 0024): a tenant section listing its channels' order.
- Promotion defaulting to the previous stage, and automatic promotion between stages (open question
  3).
