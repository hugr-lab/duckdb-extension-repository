# Spec 0039: An own kista mirroring a channel, and revocations downstream

- **Status**: accepted (the owner's decisions of 2026-10-30, below)
- **Date**: 2026-10-30
- **Author**: vgsml, Claude

## Summary

An organisation that runs its own kista and mirrors a channel of another kista (its Enterest
tenant's channel, say) can mirror **everything the channel serves to it**: a `repository` mirror
whose allowlist is `*` learns the channel's names from the remote at each run, so an extension chosen
in Enterest reaches the own kista with no change there (plan item G5). And what the remote takes back
can reach the downstream (plan item K8), on an upstream whose tenant administrators switched it on:
a body the remote **withdrew or blocked** is **blocked** in the downstream (served nowhere in the
tenant, never arriving again, liftable by a tenant administrator), and a release the remote **yanked**
is yanked in the mirror's home channel. Online by the mirror's runs, and by bundles for an air-gapped
site. Switched off, the mirror shows the remote's revocations without applying them.

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
withdrawals in a tenant (0033 phase B), blocks (0008: by body hash, tenant-wide; the sweep yanks live
releases; removing a block lets the body be released again and leaves yanked releases yanked),
bundles (0014) with a plan carrying the site's `body_hash` per cell, the licence relay under a
channel's prefix (`<prefix>/-/licenses`, 0012). kista has no un-yank: `Activate` moves only a
deprecated release (`internal/release/release.go`, the `Activate` case).

## Design

### Two routes under a channel's prefix

Both are served beside spec 0012's routes, under `-` (no DuckDB version, extension name or
`.well-known`), so a downstream reaches them with its credential **within the prefix** it already
uses (spec 0009's credential scope holds), and a path-style host shared with something else cannot
answer for kista without serving kista's prefix.

- `GET <prefix>/-/kista`, public: `{"kista": <version>, "api": <n>}` for an existing channel (`404`
  as any missing path otherwise). The **probe**.
- `GET <prefix>/-/revocations[?cursor=]`, a token only (anonymous answers as a missing path, as
  `-/licenses` does): the channel's **revoked rows** in the caller's view (public releases, and those
  of names it holds `install` on), each `{"name", "version", "platform", "body_hash", "state"}`
  where `state` is `withdrawn` (the build was withdrawn, spec 0033 phase B), `blocked` (the body is
  blocked in the tenant) or `yanked`, with `reason` for `withdrawn` (the distributor's text, decision
  4). Rows are every release the channel ever released, of **any source** (not only the name's
  selected one: `SelectedOnly` would drop a previous source's rows), from a list of its own read for
  this route (`snap.Revoked`); `ChannelReleases` and every consumer of it (`Flat`, `Versioned`,
  `Live`, `LiveKey`, the extensions listing, export's servable lookup, `PullThroughFor`) are
  unchanged. Ordered by (name, version, platform, body hash), 1,000 rows a page, an ETag over the
  first page's query (the channel's `release_version`, the tenant's blocks), `304` on a match.
  - A **suspended** tenant answers this route (only this one) to install holders: it only takes
    things away, and Enterest withdraws in suspended tenants (spec 0037). A deleting or deleted
    tenant answers as a missing path.

The index gets the same information for node agents: `extensions/{name}` and `…/versions/{v}` add the
withdrawn releases from `snap.Withdrawn` (a separate list the index's row builders read, nothing
else) as rows with their `state` kept and `withdrawn: true`, `withdrawn_reason`, served nowhere,
`would_serve` as a yanked row has it, as the management listing marks them (`internal/api/channels.go`).
`…/versions/{v}` answers `status: "withdrawn"` where only a withdrawn row would have served the path.

### A kista remote

A `repository` upstream's remote is **a kista** when `<prefix>/-/kista` answers kista's JSON. The
probe runs when the upstream is added and at each run's start (at most 4 KiB read, through egress,
without the credential). A `404`, or a `200` that is not kista's JSON, is a definite **not a kista**
and clears `remote_kista`; a failure (network, `5xx`, `429`) fails the run when the upstream needs it
(`*` or the switch), keeping `remote_kista` as it was. Any other remote (DuckDB's repositories, a
static repository) is not a kista: `*` and the switch are refused on it (`400`), and nothing below
applies.

### Revocations (K8)

#### The switch

`revocations` (boolean, default `false`) on a `repository` upstream whose remote is a kista, and on a
bundle-mode upstream (decision 2). Set at add (`-revocations`, `"revocations": true`) or later
(`POST …/upstreams/{u}/revocations {"revocations": true|false}` with `If-Match`;
`kista admin upstream revocations <tenant> <name> on|off`; the console's upstream form). A change is
an `upstream.revocations` event (`upstream`, `revocations`, `pending`: the stored revocations not
applied yet) and makes the upstream due. Switching on applies the **stored** state at the next run
(below), whatever the remote's ETag says.

#### The pass

Each run of a mirror whose remote is a kista, eager or lazy, with the switch on or off, starts with
the **revocation pass**, before its cells and independent of them: it runs whatever happens next (the
tenant's bytes room, spec 0038; a matrix over its cap; a failed listing of names), and its own
failure records a problem and lets the run go on.

1. **Read.** `GET <prefix>/-/revocations` with the upstream's credential, every page, conditional on
   the stored ETag of the first page (a `304` reads nothing more). A `429` or `503` with
   `Retry-After` is pacing, retried as kista's fetcher retries (spec 0032: at most 30 seconds, ten
   times), not a failure. At most 10,000 rows: more is a problem and the rest is not read (the stored
   rows stay as they were for the rest).
2. **Match**, by build. A row matches a downstream build when the build's source is this upstream
   (`upstream:<id>`, any channel of the tenant) and its name, extension version, platform and body
   hash are the row's. A row matching nothing is ignored: nothing is revoked ahead of time.
3. **Store** each match in `upstream_revocations` (the remote's state and reason as last seen); a
   stored row the remote no longer lists, or whose build is gone, is deleted. The reason from the
   remote is cleaned (printable, cut at a UTF-8 boundary, at most 400 bytes) rather than refused, and
   kept as the remote gave it: a chain does not nest "on the source" prefixes.
4. **Apply**, when the switch is on, each stored row not applied yet (`applied_at` empty) and not
   lifted by an administrator (`lifted_at`, below):

   | The remote's row | The downstream does |
   | --- | --- |
   | `withdrawn` or `blocked` | **blocks** the body (decision A, below) |
   | `yanked` | yanks this upstream's releases of the build in the **home channel** (a yank is that channel's decision); a release promoted elsewhere is the downstream's own choice |

   Each applied row is an `upstream.revocation` event (`upstream`, `name`, `version`, `platform`,
   `body_hash`, `action`: `blocked` | `yanked`, the remote's state and reason), actor
   `system:upstream:<tenant>/<upstream>`. Switched off, nothing is applied; each stored row is a
   problem of the run (`withdrawn on the source: <reason>`, `yanked on the source`) and listed by
   `GET …/upstreams/{u}/revocations` (with `applied_at`, `lifted_at`), so an administrator sees it and
   acts.

A lazy mirror runs the pass at its runs as well, for what it holds. Pull-through upstreams have no
runs: a pull-through upstream of a kista applies none (a follow-up). Only yanks, withdrawals and
blocks are mirrored (decision B): kista has no un-yank, so a remote's later state changes nothing
already applied, and no bundle or row carries `active`.

#### A block from an upstream (decision A)

A withdrawal applied in a mirror is a **block of the body**, made by the upstream, reversible by a
tenant administrator; not 0033's final withdrawal. On top of spec 0008's blocks:

- `blocks` gains `upstream_id` (nullable: an administrator's block has none) and `upstream_reason`
  (the remote's state and reason). The block's `reason` is `withdrawn on the source` or `blocked on
  the source`, its `created_by` the upstream's actor. Applying it is spec 0008's block: the row, then
  the sweep yanking live releases of the body in every channel of the tenant, so it is served nowhere,
  and publishing, promotion, `release add` and every intake refuse the body (a run's cell is
  `blocked`), so it never arrives again. A block is by body: identical bytes held under another
  source are blocked too, as any block blocks them.
- **What the block yanked is recorded** (`block_yanks`: tenant, body hash, release, the release's
  state before), for upstream blocks only.
- **Lifting** (`DELETE …/blocks/{body_hash}`, a tenant administrator, as today): removes the block
  and, for an upstream's block, **restores** the releases its sweep yanked to their recorded state,
  each still yanked by that sweep (`state_changed_by` the upstream's actor) and of a build still
  present; the channels are bumped. `block.remove` records the lift and `restored` (the count). The
  stored revocation is marked `lifted_at`, so the next pass does not block it again; it is applied
  again only if the remote revokes the body anew (its row leaves the remote's list, then returns).
- An administrator's own block, and its removal, behave as today (yanked releases stay yanked).
- A pre-existing administrator's block of the body stays the administrator's: the pass records the
  revocation as applied and blocks nothing.

#### Bundles

- **Export.** A plan declares `accepts_revocations: true` (a site at this version writes it). For such
  a plan the export adds `revocations` to the manifest: every revoked row (`withdrawn`, `blocked`,
  `yanked`), in the exporting channel, of the plan's names, computed in the export's view (`allowed`:
  public rows, private ones only where `-private` covers the source), whatever the cells' current
  bodies; and the exporter's own stored `upstream_revocations` rows of those names, so a chain works
  through a kista whose switch is off. Each row is (name, version, platform, body hash, state,
  reason): no internal source labels. The manifest's format moves only when `revocations` is not
  empty, so an older site (whose plan does not declare it) imports as before.
- **Import.** A bundle-mode upstream stores a manifest's revocations as a pass stores rows (matched by
  build among its own builds, after the bundle's files) and applies them when its switch is on. It
  refuses a manifest whose revocations are over 10,000 rows, carry a malformed body hash, or name a
  name outside the plan; reasons are cleaned as above. A bundle never removes or undoes a stored
  revocation: absence is no signal.
- **Replay.** Bundles are signed and ordered (spec 0014's freshness). After `ResetBundle` an older
  bundle may be imported again, by the operator's choice: it can add only revocations, never undo
  one, and a lifted one stays lifted unless the bundle's rows show it revoked anew after being absent.

### `*` on a mirror (G5)

- A `repository` mirror whose remote is a kista, eager or lazy, takes the entry `*` (`400` otherwise,
  naming why), without `versions` or `allow_reserved`, beside explicit entries or alone. Spec 0032's
  "a lazy upstream takes no `*`" no longer holds for such a mirror.
- **Discovery.** Each run, after the pass, lists the remote's names: the index's
  `GET …/extensions?cursor=&limit=500` is outside the prefix, so the listing is `GET
  <prefix>/-/names[?cursor=]` (the third route under `-`: the channel's names in the caller's view,
  500 a page, a token only, as `-/revocations`). A name is **discovered** when:
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

### Data model (migration 0048, three dialects)

- `upstreams`: `remote_kista`, `revocations` (booleans, default false), `revocations_etag`.
- `upstream_names` (pk `upstream_id, name`): the discovered names, `discovered_at`.
- `upstream_revocations` (pk `upstream_id, build_id`): `body_hash`, `name`, `package_version`,
  `platform`, `state`, `reason`, `seen_at`, `applied_at`, `lifted_at`.
- `blocks`: `upstream_id` (nullable), `upstream_reason`.
- `block_yanks` (pk `tenant_id, body_hash, release_id`): `prior_state`.
- The new tables join tenant deletion's list (`internal/store/tenantdelete.go`), and
  `upstream_names` and `upstream_revocations` are deleted with their upstream (`DeleteUpstream`);
  `block_yanks` goes with its block.
- `min_reader` 48, with PR 1 (an older replica would ignore an upstream's block when lifting it, and
  run a switched-on mirror without the pass). PR 2 needs no new reader level beyond it.

### API, CLI, console, docs

- Routes: `<prefix>/-/kista`, `-/revocations`, `-/names`; `POST|GET …/upstreams/{u}/revocations`;
  `GET …/upstreams/{u}/names`; the upstream's answer gains `remote_kista`, `revocations` and, with
  `*`, `discovered`. Blocks' answers gain `upstream` and `upstream_reason`. Index rows gain
  `withdrawn` and `withdrawn_reason`.
- CLI: `upstream revocations on|off`, `-revocations` at add, `upstream names`.
- Console: the switch on the upstream form and the stored revocations (applied, waiting, lifted) on
  the upstream page, with PR 1; the discovered names, with PR 2. The block page shows an upstream's
  blocks and what lifting restores.
- Docs: `upstreams.md` (a kista remote, the switch, the pass, `*`), `api.md`, `bundles.md` and the
  air-gapped page, `publishing.md` (an upstream's blocks), `duckdb.md` (the `-` routes),
  `observability.md` (the events), `administration/upgrading.md` (schema 48).

## Security

- **What a remote can do**, only where the tenant's administrators switched revocations on: block
  and yank what came from it, matched build by build, and nothing it never sent. A block is lifted by
  an administrator, restoring what it yanked, and is not repeated while lifted; a compromised remote
  cannot remove anything for good. It never touches an administrator's block. It can name extensions
  (within the checks a pull-through `*` passes and the caps), reserving none until it serves them.
  Every body still passes intake verification with the pinned keys.
- **Credential scope.** The probe and the listings are under the prefix the credential is scoped to;
  the probe carries no credential. A host serving something else under the same host but not kista's
  prefix layout cannot pass the probe.
- **Replay.** Online, the pass reads the remote's live state over TLS. Bundles are signed and
  ordered; a replayed bundle can only add revocations, never undo one.
- **What a caller learns.** `-/revocations` and `-/names` answer in the caller's view, with a token;
  the index's withdrawn rows follow the yanked rows' view rules. A suspended tenant shows its
  revocations to its install holders only.

## Testing

- Store suite (three dialects): the migration; `upstream_names`, `upstream_revocations`,
  `block_yanks`; tenant deletion and `DeleteUpstream` removing them.
- Serve: the three `-` routes (views, a token required, paging, ETags, a suspended tenant answering
  revocations only, a deleting one not); rows of a previous source kept.
- Regression, per consumer of the snapshot: withdrawn releases never served (`Flat`, `Versioned`),
  never live for licences (`Live`, `LiveKey`) or usage, absent from the extensions listing, export's
  servable lookup and `PullThroughFor`; the index rows with `withdrawn: true`.
- The pass:
  - before the cells, at a bytes limit, with a matrix over its cap;
  - a `429` paced;
  - a `304` skipping;
  - matching by build only (another version or platform untouched, nothing revoked ahead);
  - switched off: stored and shown, nothing applied;
  - switching on after a `304` applying the stored rows;
  - a promoted release served nowhere once blocked;
  - the yank in the home channel only;
  - reasons cleaned, not nested along a chain.
- Blocks: an upstream's block yanking everywhere and refusing the body; lifting it restoring the
  yanked releases (and an administrator's block lifted as today); a lifted revocation not re-applied
  until revoked anew.
- Bundles: export with and without `accepts_revocations`; an older importer importing a new
  exporter's bundle for an old plan; the importer's validation; a replay after `ResetBundle` adding
  nothing undone; a chain through an exporter whose switch is off.
- Discovery: paging, the cap after filtering, a cut or failed listing deleting nothing, explicit
  entries winning, a name the tenant's own or a distributor's source holds skipped, `TenantCells`,
  reservation only once released, a name that left the remote.
- End to end with DuckDB:
  - **PR 1**: kista A with a distributor's publisher (only its versions can be withdrawn) publishes
    into a channel; kista B mirrors it with explicit entries and the switch on. A withdraws a version:
    B's next run blocks it, so both the flat and the versioned `INSTALL` fail in B, and a release
    promoted in B is served nowhere. B's administrator lifts the block and the release is served
    again. A yank in A is a yank in B. A chain A→B→C (C mirroring B) carries the withdrawal. With the
    switch off, B shows the revocation and still serves.
  - **PR 2**: B mirrors A with `*`; a name published in A appears in B and installs.

## Delivery

1. **PR 1, revocations (K8).** The `-/kista` and `-/revocations` routes, the index's withdrawn rows,
   a kista remote, the switch, the pass, upstreams' blocks and lifting, bundles' revocations, the
   events, the console's switch and revocations, migration 0048 and `min_reader` 48, the PR 1 end to
   end case.
2. **PR 2, `*` on a mirror (G5).** The `-/names` route, discovery, names in the matrix, snapshot,
   placement, licences and reservations, the names route and CLI, the console's names, the PR 2 end to
   end case.

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
  withdrew, made by the upstream, served nowhere in the tenant, the body never arriving again, lifted
  by a tenant administrator (audited), restoring what it yanked. Not 0033's final withdrawal.
- **B.** Only revocations are mirrored (withdrawals and blocks as blocks, yanks as yanks): kista has
  no un-yank, so nothing is re-activated by a mirror.

## Alternatives considered

- **Reading the index API** (`/api/v1/...`): the credential would leave the prefix it is scoped to,
  and a shared path-style host could answer for kista; the `-` routes stay under the prefix.
- **Adding withdrawn releases to the snapshot's release list**: every serving and licence consumer
  would have to skip them; a separate list for the routes that show them changes none of them.
- **Withdrawing in the downstream** (0033's final withdrawal): a compromised remote could remove
  versions for good; a block is lifted by an administrator.
- **Inferring a revocation from the path**: a `404` or an older build may be a removed platform, a
  narrowed view or a transient failure.
- **Per-name index reads**: one request per name held; the revocations list is one request per
  channel, `304` when nothing changed.
- **Reserving discovered names at once**: lets a remote squat a tenant's names.

## Follow-ups

- Revocations for pull-through upstreams of a kista (they have no runs).
- Tenant-level upstream credentials (K7), so an Enterest-managed tenant mirrors without server
  configuration.
- The node agent acting on withdrawn rows (hugr-node).
- `*` in a bundle-mode upstream's plan.
