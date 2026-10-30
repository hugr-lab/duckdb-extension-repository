# Spec 0039: An own kista mirroring a channel, and revocations downstream

- **Status**: accepted (the owner's decisions of 2026-10-30, below); PR 1 (revocations) implemented
- **Date**: 2026-10-30
- **Author**: vgsml, Claude

## Summary

An organisation that runs its own kista and mirrors a channel of another kista (its Enterest
tenant's channel, say) can mirror **everything the channel serves to it**: a `repository` mirror
whose allowlist is `*` learns the channel's names from the remote at each run, so an extension chosen
in Enterest reaches the own kista with no change there (plan item G5). And what the remote takes back
can reach the downstream (plan item K8), on an upstream whose tenant administrators switched it on:
a body the remote **withdrew or blocked** is **blocked for that upstream** in the downstream (its
releases of the upstream's builds, and their copies, yanked; the body never taken from that upstream
again), and a release the remote **yanked** is yanked in the mirror's home channel. Both are the
upstream's acts, reversible by a tenant administrator, who restores what they yanked. Online by the
mirror's runs, and by bundles for an air-gapped site. Switched off, the mirror shows the remote's
revocations without applying them.

## Problem

- **Names.** A mirror fetches cells by path: a DuckDB repository has no listing, so a mirror's
  allowlist names every extension (spec 0009). `*` exists only for pull-through, which needs a
  request first and is never run. A customer mirroring their Enterest channel edits the own kista's
  allowlist for every extension their organisation chooses, and forgets.
- **Revocations stop at the first kista.** A run fetches each cell's flat path. When the remote
  yanks the current release or withdraws its version, the path answers an older build the mirror
  already holds (`unchanged`: its newer release stays current) or `404` (`missing`: the release
  stays, as removing an entry keeps releases). A withdrawn release is left out of the remote's
  snapshot entirely (`ChannelReleases`), so nothing downstream can even learn it. A bundle's export
  answers such a cell `rejected` ("withdrawn on the exporting kista") and the site's import records
  the cell, leaving the site's release served. A version Enterest withdrew for a CVE goes on being
  installed from the customer's own kista and from air-gapped sites.

What exists already: lazy mirrors (0032), pull-through `*` (0009), the index (0007) with yanked rows,
withdrawals in a tenant (0033 phase B), blocks (0008: an administrator's, by body hash, tenant-wide;
the sweep yanks live releases; removing a block lets the body arrive again and leaves the yanked
releases yanked), bundles (0014) with a plan carrying the site's `body_hash` per cell, the licence
relay under a channel's prefix (`<prefix>/-/licenses`, 0012). kista has no un-yank: `Activate` moves
only a deprecated release (`internal/release/release.go`, the `Activate` case).

## Design

### Routes under a channel's prefix

Served beside spec 0012's routes, under `-` (no DuckDB version, extension name or `.well-known`), so
a downstream reaches them with its credential **within the prefix** it already uses (spec 0009's
credential scope holds), and a path-style host shared with something else cannot answer for kista
without serving kista's prefix layout. `parseRoute` refuses any query string, so these routes page by
path: the cursor is a path segment (an opaque base64url token of the last row's key).

- `GET <prefix>/-/revocations` and `GET <prefix>/-/revocations/<cursor>`: the channel's **revoked
  rows** in the caller's view (anonymous: public releases; a token: also those of names it holds
  `install` on). Answer: `{"api": 1, "rows": [...], "next": "<cursor>"}` (`next` absent on the last
  page), each row `{"name", "version", "platform", "body_hash", "state", "reason"}`, 1,000 rows a
  page, ordered by (name, version, platform, body hash). A row is a release the channel released, of
  **any source** (not only the name's selected one: `SelectedOnly` would drop a previous source's
  rows), whose state is, strongest first:
  - `withdrawn`: its build was withdrawn (spec 0033 phase B), `reason` the distributor's text
    (decision 4);
  - `blocked`: its body is blocked by an administrator (spec 0008), `reason` empty (an
    administrator's reason stays in the tenant);
  - `yanked`: the release is yanked, `reason` empty;
  - and for a release of an upstream's build whose body **that upstream blocked** applying its
    remote's withdrawal or block (below), that block's state and reason, whatever yanked the release
    first (the upstream's yank, then the block, or an administrator's yank: looked up by the
    release's source and body), so a chain carries the strongest state on. A snapshot reads only the
    upstream blocks of the bodies its channel holds, and keeps its rows with it (computed once). A release an
    upstream only yanked shows `yanked`, `reason` empty.

  The rows come from a list of their own (`snap.Revoked`); `ChannelReleases` and every consumer of it
  (`Flat`, `Versioned`, `Live`, `LiveKey`, the extensions listing, export's servable lookup,
  `PullThroughFor`) are unchanged. The rows of a snapshot are computed once (every row, and the
  public view's); a caller's rows are those of the names it holds `install` on and the public rows of
  the others, so the ETag is a digest of the snapshot's rows, the tenant's state, the cursor and the
  **names the caller holds** `install` on: a grant change is never answered `304` with stale rows, and
  a page is found by binary search. An upstream block made, changed or removed bumps every channel
  holding a release of the body from that upstream, so the snapshot moves with it.

  Served by **signed** channels only: a passthrough channel answers as a missing path (`404`). A
  **suspended** tenant answers this route (only this one) to tokens holding `install` on a name the
  channel has a release of, with those names' rows: it takes things away, and Enterest withdraws in
  suspended tenants (spec 0037); anonymous callers and other tokens get `404`. A deleting or deleted
  tenant, and a missing channel, answer as a missing path (`404`).
- `GET <prefix>/-/names` and `…/-/names/<cursor>` (PR 2): the channel's extension names in the
  caller's view, 500 a page, the same answer shape and rules (not in a suspended tenant).

The index gets the information for node agents: `extensions/{name}` and `…/versions/{v}` add the
withdrawn releases from `snap.Withdrawn` (a separate list only the index's row builders read) as rows
with their `state` kept and `withdrawn: true`, `withdrawn_reason`, served nowhere, `would_serve` as a
yanked row has it, as the management listing marks them (`internal/api/channels.go`).
`…/versions/{v}` answers `status: "withdrawn"` where only a withdrawn row would have served the path.

### A kista remote

`remote_kista` is a property of a `repository` mirror only (a bundle-mode upstream never reads a
remote and keeps it false). There is no separate probe (no kista version shown anonymously): a
`repository` upstream's remote is
**a kista** when `GET <prefix>/-/revocations`, read with the upstream's credential if it has one,
answers kista's shape (`"api"` and `"rows"`). It is read when the upstream is added and by each run's
pass (below).

- A `200` of that shape sets `remote_kista`.
- A `404`, or a `200` of another shape, is **not a kista**: on an upstream with neither the switch
  nor `*` it clears `remote_kista`; on one with either, it is a run problem ("the source answers no
  revocations: not a kista, suspended or gone") and the stored state stays (a suspended Enterest
  tenant answers a credential without `install` as a missing path).
- A `401` or `403` to an upstream switched off whose remote is not known as a kista is **not a
  kista** too, silently (a static repository behind a credential); otherwise it is a problem.
- A failure (network, `5xx`, `429` after pacing) is a problem and changes nothing.
- A read is bounded: at most 11 pages (10,000 rows at 1,000 a page, and one more), no page on from
  an empty page, no cursor twice; each is a problem, and a cut read forgets nothing.

`*` and the switch are refused (`400`) on an upstream whose remote is not a kista; DuckDB's
repositories and static repositories stay as they are.

### Revocations (K8)

#### The switch

`revocations` (boolean, default `false`) on a `repository` upstream whose remote is a kista, and on a
bundle-mode upstream (decision 2). Set at add (`-revocations`, `"revocations": true`) or later
(`POST …/upstreams/{u}/revocations {"revocations": true|false}` with `If-Match`;
`kista admin upstream revocations <tenant> <name> on|off`; the console's upstream form). A change is
an `upstream.revocations` event (`upstream`, `upstream_id`, `revocations`, `pending`: the stored revocations not
applied yet) and makes the upstream due. Switching on applies the **stored** state at the next run
(below), whatever the remote's ETag says.

#### The pass

Each run of a `repository` mirror, eager or lazy, switch on or off, starts with the
**revocation pass** (a remote not known as a kista is read too, so one that becomes a kista is
noticed; its `404` only keeps `remote_kista` clear), before its cells and independent of them: it runs whatever happens next (the
tenant's bytes room, spec 0038; a matrix over its cap; a failed listing of names), and its own
failure records a problem and lets the run go on.

1. **Read.** `-/revocations`, every page, conditional on the stored ETag of the first page (a `304`
   reads nothing more). A `429` or `503` with `Retry-After` is pacing, retried as kista's fetcher
   retries (spec 0032: at most 30 seconds, ten times), not a failure. At most 10,000 rows: more is a
   problem and the rest is not read (stored rows past it stay as they were).
2. **Match**, by build. A row matches a downstream build when the build's source is this upstream
   (`upstream:<id>`, any channel of the tenant) and its name, extension version, platform and body
   hash are the row's. A row matching nothing is ignored: nothing is revoked ahead of time.
3. **Store** each match in `upstream_revocations`: the remote's state and reason as last seen. A
   stored row the remote no longer lists (after a complete read), or whose build is gone, is deleted
   only when nothing was applied or lifted of it; otherwise it is kept and marked **unlisted**
   (`unlisted_at`): what it applied stays applied and can still be lifted, it is no longer applied,
   and a remote dropping it for a run and listing it again undoes no lift (listed again, it is as it
   was). The reason is cleaned (printable, cut at a UTF-8 boundary, at most 400 bytes) rather than
   refused, and kept as the remote gave it: a chain nests no prefixes.
4. **Apply**, when the switch is on, each stored row whose state is **stronger** than what was applied
   for it (precedence `withdrawn` > `blocked` > `yanked` > nothing), unless an administrator lifted it
   at that state or a stronger one (below):

   | The remote's state | The downstream does |
   | --- | --- |
   | `withdrawn` or `blocked` | an **upstream block** of the body (below) |
   | `yanked` | yanks this upstream's releases of the build in the **home channel** (a yank is that channel's decision; a copy promoted or placed elsewhere is the downstream's own choice) |

   So a yank followed by the remote's withdrawal is applied again, as a block. A weaker later state
   changes nothing applied (decision B: nothing is re-activated by a mirror). Each application is an
   `upstream.revocation` event (`upstream`, `upstream_id`, `name`, `version`, `platform`,
   `body_hash`, `action`: `blocked` | `yanked`, the remote's state and reason, `yanked`). Each is
   applied in one transaction under the tenant's upstream lock (and, for a block, every channel's
   lock: the block, its sweep, the mark and the event; a run's due withdrawals and blocks go up to
   100 to a transaction), the row read again there and applied only
   while still due, so an apply and a lift never interleave. Switched off, nothing is applied; the
   stored rows due are counted (`revoked_on_source`) and summed up in **one** problem of the run, so
   the cells' problems keep their room, and listed by `GET …/upstreams/{u}/revocations` (with what
   was applied and lifted), so an administrator sees them and acts. The pass runs under the run's
   lease renewal and pause check.

**Actor**: every act of the pass is `system:upstream:<tenant>/<upstream>`; the upstream's `id` is in
each record and event, so a renamed upstream's acts stay attributed.

A lazy mirror runs the pass at its runs as well, for what it holds. Pull-through upstreams have no
runs: a pull-through upstream of a kista applies none (a follow-up).

#### An upstream block (decisions A and D)

A withdrawal or block applied in a mirror is the **upstream's block of the body**, narrow and
reversible; not 0033's final withdrawal and not an administrator's tenant-wide block. Kept apart from
spec 0008's `blocks` (whose rows, routes and sweep stay as they are), in `upstream_blocks`:

- **Intake.** The body is never taken from **this upstream** again: a run's cell, a lazy or
  pull-through fetch and a bundle import of it from this upstream are `blocked` (checked under the
  channel lock with the release insert, as spec 0009 checks `blocks`). The same bytes from any other
  source (the tenant's own publishers, DuckDB's mirrors, another upstream, a passthrough channel)
  stay as they are.
- **Sweep.** In the block's transaction, under every channel's lock of the tenant (a promotion or
  placement into any channel is ordered against it, as spec 0008's sweep orders them; creating a
  channel takes the tenant's upstream lock, so none appears meanwhile, and channels that keep
  changing make it `ErrBusy` after three attempts), with one query across the tenant, every live
  release of a build of **this upstream** with that body is yanked: in the home channel and its
  copies elsewhere (a promotion or placement releases the same build), so it is served nowhere in the
  tenant. Nothing else is touched.

#### What an upstream yanked is recorded (decision C)

Both an upstream block's sweep and an upstream's yank record each release they yank in
`upstream_yanks` (the release, the upstream, the build, the body hash, the release's state before,
and the cause: `block` or `yank`). Only yanks made by an upstream are reversible; an administrator's
yank stays final, as today.

#### Lifting (decisions A and C)

A tenant administrator lifts an upstream's revocation of a build:
`DELETE …/upstreams/{u}/revocations/{build_id}` (`If-Match`), `kista admin upstream revocation lift
<tenant> <upstream> <build>`, the console's button.

- The upstream block of that body, if any, is removed, and the releases recorded for it are
  **restored**: under each channel's lock, each recorded release that is **still yanked**, whose build
  is **not withdrawn** (spec 0033), and whose body is **not blocked** by an administrator, goes back
  to its recorded state; the channel is bumped. Its records are deleted either way.
- Only a row something applied is left to lift: one never applied (the switch off) or lifted
  already is refused (`409`), with no change and no event.
- The stored row gets `lifted_at` and the state lifted, the stronger of its stored and applied
  states; the pass does not apply it again unless the remote's state becomes **stronger** than the
  state lifted (a yank lifted, then withdrawn).
- An `upstream.revocation.lift` event (`upstream`, `upstream_id`, `build`, `body_hash`, `state`,
  `restored`).
- The mark, the restore, the upstream's version and the event are one transaction, under the
  tenant's upstream lock and every channel's lock.

**An administrator's block takes over**: an administrator blocking a body (spec 0008) deletes, in the
same transaction, the `upstream_yanks` records of releases with that body, so no later lift restores
what the administrator blocked. Two upstreams never share a build, so each upstream's revocations,
blocks and lifts are its own; the same body from two upstreams is two builds, revoked and lifted
separately.

#### Bundles

- **Export.** A plan declares `accepts_revocations: true` (a site at this version writes it). For such
  a plan the export adds `revocations` to the manifest: the exporting channel's `-/revocations` rows
  (the same rule as online: what the exporter applied itself, never a revocation it stored but did
  not apply, never a lifted one) of the plan's names, computed in the export's view (`allowed`:
  public rows, private ones only where `-private` covers the source), whatever the cells' current
  bodies. Each row is (name, version, platform, body hash, state, reason): no internal source labels.
  The manifest's format moves only when `revocations` is not empty, so an older site, whose plan does
  not declare it, imports as before. At most 10,000 rows a source; rows over it are counted
  (`revocations_left_out`) and are a problem of the export, never left out silently.
- **Import.** A bundle-mode upstream stores a manifest's revocations as the pass stores rows (matched
  by build among its own builds, after the bundle's files), and at every import, with or without
  revocations in the bundle, applies the stored ones due when its switch is on (a switch turned on
  since the last bundle applies what is stored). It
  refuses a manifest whose revocations are over 10,000 rows, carry a malformed body hash, or name a
  name outside the plan; reasons are cleaned as above.
- **Absence is no signal** in a bundle: a bundle never removes a stored revocation, so a lift is final
  over bundles in practice (only a later bundle carrying a stronger state applies again).
- **Replay.** Bundles are signed and ordered (spec 0014's freshness). After `ResetBundle` an older
  bundle may be imported again, by the operator's choice: it can only add revocations or strengthen
  them, never undo one, and a lifted one stays lifted unless the bundle's state is stronger.

### `*` on a mirror (G5, PR 2)

- A `repository` mirror whose remote is a kista, eager or lazy, takes the entry `*` (`400` otherwise,
  naming why), without `versions` or `allow_reserved`, beside explicit entries or alone. Spec 0032's
  "a lazy upstream takes no `*`" no longer holds for such a mirror.
- **Discovery.** Each run, after the pass, reads `-/names` (every page). A name is **discovered** when:
  - it passes what a pull-through `*` passes (spec 0009): the name grammar, not reserved server-wide
    (spec 0030), not an alias, no live replacement in the home channel;
  - no explicit entry of any upstream of the channel lists it, and no other upstream discovered it
    first (`ChannelEntryOwner` and the entry collision checks read `upstream_names` as well);
  - no source of the tenant other than an upstream (its own, a distributor's publisher) holds it in
    any channel.

  At most 1,000 names are kept, counted after these checks, by name; more is a problem. Only a
  complete listing (every page read, under the cap) deletes names absent from it; a listing that
  failed or was cut keeps the names there are. A change renews the tenant's snapshots, as an entry
  change does.
- **The matrix** is the discovered names and the explicit entries × the platforms × the home
  channel's DuckDB versions. `TenantCells` and spec 0009's caps (20,000 cells an upstream, 100,000 a
  tenant) count the discovered names; names past a cap, by name, are left out of the run and counted
  as a problem. Spec 0038's limits apply to what is fetched.
- **A discovered name behaves as a listed name** in the snapshot, a lazy mirror's asks, placement in
  the home channel (spec 0033) and licence supply (`SuppliedBy`, spec 0035), with one difference:
  `UpstreamProvides` (the tenant's reservation, and spec 0037's follow skips) counts a discovered name
  only once this upstream holds a release of it, so a remote cannot reserve the tenant's names by
  listing them. A name that leaves the remote (decision 1) leaves `upstream_names`: its cells leave
  the matrix and its releases stay, as for a removed entry, and it stops being provided, as a removed
  entry's names do (spec 0009).

### Data model

**Migration 0048** (PR 1, three dialects; `min_reader` 48: an older replica would take a body an
upstream blocked from that upstream again, and run a switched-on mirror without the pass):

- `upstreams`: `remote_kista`, `revocations` (booleans, default false), `revocations_etag`.
- `upstream_revocations` (pk `upstream_id, build_id`): `body_hash`, `name`, `package_version`,
  `platform`, `state`, `reason`, `seen_at`, `applied_state`, `applied_at`, `lifted_state`,
  `lifted_at`, `unlisted_at`.
- `upstream_blocks` (pk `upstream_id, body_hash`): `tenant_id`, `state` (`withdrawn` | `blocked`),
  `reason`, `created_at`; an index on `tenant_id` (a snapshot reads the tenant's).
- `upstream_yanks` (pk `release_id`): `tenant_id`, `upstream_id`, `build_id`, `body_hash`,
  `prior_state`, `cause`.
- Deletion: all three join tenant deletion's list (`internal/store/tenantdelete.go`).
  `DeleteUpstream` deletes the upstream's `upstream_revocations`, `upstream_blocks` and
  `upstream_yanks` (its yanked releases stay yanked, as removing an upstream keeps its releases'
  states). Deleting a release or a channel deletes its `upstream_yanks` rows.

**Migration 0049** (PR 2, `min_reader` 49: an older replica would run a `*` mirror with no names):
`upstream_names` (pk `upstream_id, name`): `discovered_at`; in tenant deletion's list and deleted
with its upstream.

### API, CLI, console, docs

- Routes: `<prefix>/-/revocations[/<cursor>]` (PR 1), `<prefix>/-/names[/<cursor>]` (PR 2);
  `POST|GET …/upstreams/{u}/revocations`, `DELETE …/upstreams/{u}/revocations/{build_id}`;
  `GET …/upstreams/{u}/names` (PR 2); the upstream's answer gains `remote_kista`, `revocations` and,
  with `*`, `discovered`. Index rows gain `withdrawn` and `withdrawn_reason`.
- CLI: `upstream revocations on|off`, `-revocations` at add, `upstream revocation lift`,
  `upstream names` (PR 2).
- Console: with PR 1, the switch on the upstream form and the stored revocations (applied, waiting,
  lifted, with **Lift**) on the upstream page; with PR 2, the discovered names.
- Docs: `upstreams.md` (a kista remote, the switch, the pass, upstream blocks, lifting, `*`), `api.md`,
  `bundles.md` and the air-gapped page, `publishing.md` (an upstream block beside an administrator's),
  `duckdb.md` (the `-` routes), `observability.md` (the events), `administration/upgrading.md`
  (schemas 48 and 49).

## Security

- **What a remote can do**, only where the tenant's administrators switched revocations on: yank and
  block what came from it, matched build by build, and nothing it never sent, nor the same bytes from
  any other source. Every act is lifted by an administrator, restoring what it yanked, and is not
  repeated at the same strength; a compromised remote cannot remove anything for good. It never
  undoes an administrator's block or yank. It can name extensions (within the checks a pull-through
  `*` passes and the caps), reserving none until it serves them. Every body still passes intake
  verification with the pinned keys.
- **Credential scope.** The routes are under the prefix the credential is scoped to. Nothing
  anonymous shows kista's version.
- **Replay.** Online, the pass reads the remote's live state over TLS. Bundles are signed and
  ordered; a replayed bundle can only add or strengthen revocations, never undo one.
- **What a caller learns.** `-/revocations` and `-/names` answer in the caller's view, their ETag
  bound to it; the index's withdrawn rows follow the yanked rows' view rules. A suspended tenant shows
  its revocations to its install holders only.

## Testing

- Store suite (three dialects): migrations 0048 and 0049; the new tables; tenant deletion,
  `DeleteUpstream`, release and channel deletion removing them.
- Serve: `-/revocations` (views, anonymous public rows, paging by path, a query string refused, the
  ETag changing with a grant, a suspended tenant answering install holders only, a deleting one not,
  rows of a previous source kept, an applied upstream revocation shown with its state); `-/names`.
- Regression, per consumer of the snapshot: withdrawn releases never served (`Flat`, `Versioned`),
  never live for licences (`Live`, `LiveKey`) or usage, absent from the extensions listing, export's
  servable lookup and `PullThroughFor`; the index rows with `withdrawn: true`.
- The pass: before the cells, at a bytes limit, with a matrix over its cap; a `429` paced; a `304`
  skipping; matching by build only (another version or platform untouched, nothing revoked ahead);
  switched off: stored and shown, nothing applied; switching on after a `304` applying the stored
  rows; precedence (a yank, then a withdrawal, applied again as a block; a weaker state changing
  nothing); reasons cleaned, not nested along a chain; a suspended remote kept as a problem.
- Upstream blocks: the sweep covering copies and nothing else (the tenant's own build of the same
  bytes served); the body refused from this upstream only; lifting restoring (not a release yanked
  since by an administrator, not a withdrawn build, not a body an administrator blocked); a lifted
  revocation not re-applied unless stronger; an administrator's block taking over; an upstream's yank
  lifted.
- Bundles: export with and without `accepts_revocations`; an older importer importing a new
  exporter's bundle for an old plan; the importer's validation; a replay after `ResetBundle` adding
  nothing undone; a lift staying lifted over later bundles of the same state.
- Discovery (PR 2): paging, the cap after filtering, a cut or failed listing deleting nothing,
  explicit entries winning, a name the tenant's own or a distributor's source holds skipped,
  `TenantCells`, reservation only once released, a name that left the remote.
- End to end with DuckDB:
  - **PR 1**: kista A with a distributor's publisher (only its versions can be withdrawn) publishes
    into a channel; kista B mirrors it with explicit entries and the switch on. A withdraws a version:
    B's next run blocks it for the upstream, so both the flat and the versioned `INSTALL` fail in B,
    and a release B promoted to another channel is served nowhere. B's administrator lifts it and the
    releases are served again. A yank in A is a yank in B's home channel, and lifting restores it. A
    chain A→B→C (C mirroring B) carries the withdrawal. With the switch off, B shows the revocation and
    still serves.
  - **PR 2**: B mirrors A with `*`; a name published in A appears in B and installs.

## Delivery

1. **PR 1, revocations (K8).** `-/revocations`, the index's withdrawn rows, a kista remote, the
   switch, the pass, upstream blocks and yanks with lifting, bundles' revocations, the events, the
   console's switch and revocations, migration 0048 with `min_reader` 48, the PR 1 end to end case.
2. **PR 2, `*` on a mirror (G5).** `-/names`, discovery, names in the matrix, snapshot, placement,
   licences and reservations, the names route and CLI, the console's names, migration 0049 with
   `min_reader` 49, the PR 2 end to end case.

Each PR runs `make test` on the three databases, `make lint`, `make console-test`,
`make console-e2e` and `make e2e` before it opens.

## Decisions

The owner's decisions, 2026-10-30:

1. An extension that leaves the source channel keeps its releases in the mirror (as a removed
   entry's releases stay).
2. Applying the source's revocations is opt-in per upstream, switched on by the tenant's
   administrators; off, the mirror shows them without applying them. Bundles carry them whatever the
   switch.
3. Superseded by B.
4. The remote shows a withdrawal's reason to the callers who see the row.

- **A.** A withdrawal applied in a mirror is reversible: a block of exactly the builds the source
  withdrew, made by the upstream, lifted by a tenant administrator (audited), restoring what it
  yanked. Not 0033's final withdrawal.
- **B.** Only revocations are mirrored (withdrawals and blocks as blocks, yanks as yanks): kista has
  no un-yank, so nothing is re-activated by a mirror's run.
- **C.** A yank from an upstream is reversible as its block is: what it yanked is recorded and an
  administrator restores it, audited. An administrator's yank stays final.
- **D.** An upstream's block is narrow: the body is refused from that upstream only, and only the
  releases of that upstream's builds and their copies are yanked. Kept in `upstream_blocks`, apart
  from an administrator's tenant-wide `blocks`.

## Alternatives considered

- **Reading the index API** (`/api/v1/...`): the credential would leave the prefix it is scoped to,
  and a shared path-style host could answer for kista; the `-` routes stay under the prefix.
- **A separate public probe** (`-/kista`): it shows kista's version anonymously; `-/revocations`'s
  shape is the probe.
- **Adding withdrawn releases to the snapshot's release list**: every serving and licence consumer
  would have to skip them; a separate list for the routes that show them changes none of them.
- **Withdrawing in the downstream** (0033's final withdrawal), or a tenant-wide block: a compromised
  remote could remove versions for good, or the tenant's own copy of the same bytes.
- **An upstream scope column on `blocks`**: every reader of `blocks` (publication, promotion,
  intake, the index's `blocked`, the routes) would branch on it; a table of its own changes none.
- **Inferring a revocation from the path**: a `404` or an older build may be a removed platform, a
  narrowed view or a transient failure.
- **Exporting stored but unapplied revocations along a chain**: a kista whose administrators chose
  not to apply them would still impose them downstream; online and bundles both carry what was
  applied.
- **Reserving discovered names at once**: lets a remote squat a tenant's names.

## Follow-ups

- Revocations for pull-through upstreams of a kista (they have no runs).
- Tenant-level upstream credentials (K7), so an Enterest-managed tenant mirrors without server
  configuration.
- The node agent acting on withdrawn rows (hugr-node).
- `*` in a bundle-mode upstream's plan.
