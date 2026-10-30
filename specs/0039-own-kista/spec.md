# Spec 0039: An own kista mirroring a channel, and revocations downstream

- **Status**: accepted (the owner's decisions of 2026-10-30, below)
- **Date**: 2026-10-30
- **Author**: vgsml, Claude

## Summary

An organisation that runs its own kista and mirrors a channel of another kista (its Enterest
tenant's channel, say) can mirror **everything the channel serves to it**: a `repository` mirror
whose allowlist is `*` learns the channel's names from the remote's index at each run, so an
extension chosen in Enterest reaches the own kista with no change there (plan item G5). And what the
remote takes back reaches the downstream: a version **withdrawn** on the remote (spec 0033 phase B)
is withdrawn in the downstream's mirror too, for good, and a release **yanked** there is yanked in
the mirror's channel, by the mirror's runs online and by bundles for an air-gapped site (plan item
K8), on an upstream whose tenant administrators switched it on; switched off, the mirror shows the
remote's revocations without applying them.

## Problem

- **Names.** A mirror fetches cells by path: a DuckDB repository has no listing, so a mirror's
  allowlist names every extension (spec 0009). `*` exists only for pull-through, which needs a
  request first and is never run. A customer mirroring their Enterest channel edits the own kista's
  allowlist for every extension their organisation chooses, and forgets.
- **Withdrawals and yanks stop at the first kista.** A run fetches each cell's flat path. When the
  remote yanks the current release or withdraws its version, the path answers an older build (which
  the mirror holds already: `unchanged`, so its newer release stays current) or `404` (`missing`; the
  release stays, as removing an entry keeps releases). A withdrawn release is left out of the
  remote's index entirely (`ChannelReleases` skips it), so nothing downstream can even learn it. A
  bundle's export answers such a cell `rejected` ("withdrawn on the exporting kista") and the site's
  import records the cell, leaving the site's release served. A version Enterest withdrew for a CVE
  goes on being installed from the customer's own kista and from air-gapped sites.

What exists already: lazy mirrors (0032), pull-through `*` (0009), the index (0007) with yanked rows
(`state: yanked`, `would_serve`), withdrawals in a tenant (0033 phase B: `withdrawn_versions`,
final, every channel's release served nowhere), bundles (0014) with a plan that carries the site's
`body_hash` per cell, licence relay through a mirror (0035).

## Design

### A kista remote

A `repository` upstream's remote is **a kista** when its prefix has the layout
`<root>/<tenant>/<channel>` (spec 0001) and `GET <root>/api/v1/info` answers kista's info
(`{"kista": …, "api": …}`). The upstream records it when it is added and at each run's start (one
small request, at most 4 KiB read; through egress, without the credential, as the `.well-known`
read). The index base is then `<root>/api/v1/tenants/<tenant>/channels/<channel>`, read with the
upstream's credential (spec 0009 phase 3), so it is the view of the principal the downstream
mirrors as: public releases, and those it holds `install` on. Any other remote (DuckDB's
repositories, a static repository) is not a kista: `*` is refused on it and nothing below applies.

### `*` on a mirror (G5)

- A `repository` mirror, eager or lazy, takes the entry `*` when its remote is a kista (adding it
  otherwise answers `400`, naming why), without `versions` or `allow_reserved`, beside explicit
  entries or alone. An explicit entry's settings win for its name. Spec 0032's "a lazy upstream takes
  no `*`" no longer holds for such a mirror: its discovered names are its list.
- **Discovery.** Each run starts by listing the remote channel's extensions
  (`GET …/extensions?cursor=&limit=500`, every page; at most 1,000 names, the entries' cap; more is a
  run problem and the names after the 1,000th by name are left out). A name is **discovered** when it
  passes what a pull-through `*` passes (spec 0009): the name grammar, not reserved server-wide (core,
  community: spec 0030), not an alias, not listed or discovered by another upstream of the channel
  (the earlier keeps it), and no live
  replacement in the home channel (spec 0009's shadowing). Discovered names are kept in
  `upstream_names` (inserted when new, deleted when absent from a listing that completed); a change
  renews the tenant's snapshots, as an entry change does. A listing that fails (network, `401`,
  `5xx`) ends the run `failed` before its cells and keeps the names there are.
- **The matrix** is the discovered names (and the explicit entries) × the upstream's platforms × its
  home channel's DuckDB versions, under spec 0009's matrix limits (20,000 cells an upstream, 100,000
  a tenant): names past the limit, by name, are left out of this run and counted as a problem. Spec
  0038's limits apply to what is fetched, as for any mirror.
- **A discovered name is a listed name** from then on: in the upstream's matrix and snapshot (a lazy
  mirror's asks, spec 0032), placed in the home channel as an eager mirror's entries are (spec 0033),
  supplied for licences (`SuppliedBy`, spec 0035's relay), and reserved in the tenant once a cell of
  it is released (spec 0009's reservation), not before: a remote cannot reserve the tenant's names by
  listing them.
- **A name leaving the remote** (no longer in a completed listing: the remote removed it, or the
  principal lost `install`) leaves the matrix: its cells are removed by the run, as for a removed
  entry, and its releases **stay**, the channel's, as spec 0009 keeps a removed entry's releases
  (decision 1). A withdrawal or yank of its versions still reaches them (below), as long as the
  remote's index shows them to the principal.

### Revocations downstream (K8)

#### What the remote shows

The index shows a release whose build was **withdrawn** as a row of its own, as it shows a yanked
one, to every caller whose view includes the release: `state: "withdrawn"`, `withdrawn_reason` (the
distributor's text, decision 4), served nowhere, `would_serve` listing the DuckDB versions it
would serve. `…/versions/{v}` answers `status: "withdrawn"` where it answers `yanked` for a yanked
one. The snapshot keeps withdrawn releases (`ChannelReleases` no longer leaves them out) with their
`Withdrawn` mark, and the resolution functions skip them as they skip yanked ones, so serving does
not change. A node agent learns a withdrawal the way it learns a yank.

#### The switch

A mirror applies the remote's revocations only when its tenant administrators switched it on
(decision 2):

- `revocations` (boolean, default `false`) on a `repository` upstream whose remote is a kista (on any
  other upstream it is refused, `400`), and on a bundle-mode upstream (its bundles carry them).
  Set when the upstream is added (`-revocations`, `"revocations": true`) or later
  (`POST …/upstreams/{u}/revocations {"revocations": true|false}` with `If-Match`; `kista admin
  upstream revocations <tenant> <name> on|off`; the console's upstream form). A change is an
  `upstream.revocations` event (`upstream`, `revocations`) and makes the upstream due.
- **Off**, the run still reads the remote's rows (the pass below) and **shows** them: each body the
  mirror holds that the remote withdrew or yanked is a problem of the run (`withdrawn on the source:
  <reason>`, `yanked on the source`) and is listed by `GET …/upstreams/{u}/revocations` (name,
  version, platform, body hash, the remote's state and reason, first seen), so an administrator sees
  it and acts (a yank, a block, or switching it on). Nothing is applied, and no
  `upstream.revocation` event is written.
- **Switched on**, the next run applies the remote's state as it is then, whatever arrived before:
  there is no queue of past revocations, only the remote's current rows.
- Bundles carry revocations whatever the switch (the export cannot know the site's); the importing
  upstream's switch decides whether they apply, and off, they are shown as a run's are.

#### A run applies them

After its cells, a run of a mirror whose remote is a kista reads (switch on or off), for each name with a release from
this upstream in its home channel, the remote's `GET …/extensions/{name}` (conditional, the ETag kept
per name in `upstream_names`; a `304` costs nothing) and matches the remote's rows to its own builds
**by body hash** (spec 0002's composite hash, the same on both sides), and, switched on, does:

| The remote's row for a body the mirror holds | The downstream does |
| --- | --- |
| `withdrawn` | withdraws the version from the upstream's source (`upstream:<name>`), as spec 0033 phase B does for a distributor's publisher: `withdrawn_versions` records it (none of it arrives again from this source), its builds are withdrawn, and every channel holding a release of them is bumped, so it is served nowhere in the tenant; the reason is `withdrawn on the source: <reason>`, cut to the 400 bytes a reason holds |
| `yanked` | yanks the upstream's release of that body in the home channel (`state_changed_by` `upstream:<name>`), unless it is yanked already |
| `active` or `deprecated`, a release this upstream's run had yanked | activates it again (only a yank whose `state_changed_by` is this upstream; an administrator's yank or a block stays: decision 3) |
| absent (not in the view) | nothing |

- Withdrawals are final on both sides: nothing activates a withdrawn build again, and a later
  `active` row of the same body changes nothing.
- A yank is the home channel's only (the remote's yank is a channel's decision); a withdrawal is the
  tenant's (the remote's is its source's), so a release promoted from the mirror into another channel
  is served nowhere too.
- Each change is an event in the downstream tenant, `upstream.revocation` (`upstream`, `name`,
  `version`, `body_hash`, `action`: `withdrawn` | `yanked` | `activated`, the remote's reason), besides
  the usual `catalogue.withdrawn` and release events, with actor `system:upstream:<tenant>/<name>`.
- A lazy mirror applies them at its runs as well, for what it holds. Pull-through upstreams are not
  run: a pull-through `*` of a kista applies them nowhere (a follow-up if wanted).
- The pass is bounded as the run is: one conditional request per name held (at most the 1,000
  names), on the run's per-host slots, and a failure of it records a problem and ends the run
  `failed`, leaving what was applied.

#### Bundles

- **Export.** The plan carries, per cell, the site's `body_hash` (spec 0014). For each plan cell
  whose `body_hash` the exporting channel holds yanked or withdrawn (any DuckDB version), the export
  adds a **revocation** to the manifest: (source, name, version, platform, `body_hash`, `withdrawn` |
  `yanked`, reason). For a body that the plan's cell names and that is active again on the exporting
  side, it adds `active`. A withdrawal the exporting kista applied from its own upstream (above) is a
  withdrawal on the exporting side like any other, so revocations travel through any number of
  kistas.
- **Import.** A bundle-mode upstream applies a manifest's revocations as a run applies the remote's
  rows, matched by body hash among its own builds, after the bundle's files, when its switch is on;
  off, it records them as a run shows them. The manifest is signed
  with the bundle key and checked as today (spec 0014), and a bundle older than the target's last
  import is `stale`, as today, so an old bundle cannot replay an `active` over a newer yank;
  withdrawals are final anyway.
- The export's table keeps `rejected` for such a cell's file; the revocation is what the site acts
  on.

### Data model (migration 0048, three dialects)

- `upstreams`: `remote_kista` (boolean, recorded at add and each run's start), `revocations`
  (boolean, default false).
- `upstream_revocations` (`upstream_id`, `body_hash`, `name`, `package_version`, `platform`,
  `state`, `reason`, `seen_at`, `applied_at`): what the remote revoked of what the mirror holds, as
  last seen; a row leaves when the remote's state no longer revokes it or the mirror no longer holds
  the body. It backs the listing and keeps a switched-off mirror's problems readable between runs.
- `upstream_names` (`upstream_id`, `name`, `discovered_at`, `index_etag`): the discovered names, and
  the ETag of each held name's index rows (for explicit entries too).
- No other change: releases' `state_changed_by`, builds' `withdrawn_at`/`withdrawn_reason` and
  `withdrawn_versions` (keyed by source) exist. `min_reader` moves to 48 (an older replica would
  ignore `*` on a mirror and run it with no names, and serve an index without withdrawn rows).

### API, CLI, console, docs

- `-revocations`, `POST …/upstreams/{u}/revocations`, `GET …/upstreams/{u}/revocations`, the CLI's
  `upstream revocations`; the upstream's answer gains `revocations`.
- `upstream add … -entry '*'` and `POST …/entries {"name": "*"}` on a kista remote; the upstream's
  answer gains `remote_kista` and, with `*`, `discovered` (the count) and
  `GET …/upstreams/{u}/names` (the discovered names, paged).
- The index rows' `state` gains `withdrawn` and `withdrawn_reason`; the bundle manifest gains
  `revocations` (its format version moves; an older importer refuses a newer format, as today).
- The console's upstream page shows discovered names, the switch, and the remote's revocations
  (applied, or waiting while the switch is off).
- Docs: `upstreams.md` (a kista remote, `*`, revocations), `api.md` (index rows, names),
  `bundles.md` and the air-gapped page (revocations), `observability.md` (the event),
  `administration/upgrading.md` (schema 48).

## Security

- **What a remote can do.** It already decides what new versions a mirror gets; it can now also
  name the extensions (within the checks a pull-through `*` passes, the caps, and the tenant's
  limits) and, where the tenant administrators switched revocations on, take back what came from it: yank or withdraw only builds whose source is this
  upstream, matched by body hash. It never touches the tenant's own releases, another upstream's, a
  distributor's publisher's, or an administrator's yank or block (it activates only its own yanks).
  Every body still passes intake verification with the pinned keys; discovery adds no trust in a
  body.
- **No name squatting.** Discovered names are reserved in the tenant only once released, so a
  remote listing names it never serves reserves nothing; core and community names are refused as
  for pull-through.
- **Replay and downgrade.** Online, a run reads the remote's state live over TLS with the
  credential. A withdrawal is final on both sides. An `active` row re-activates only a yank this
  upstream made. Bundles are signed with the bundle key, and a stale bundle is refused.
- **What a caller learns.** Withdrawn rows follow the view rules yanked rows follow (public to
  anyone, private to `install` holders), so the index tells no one about a release they could not
  see. `info` is public already.

## Testing

- Store suite (three dialects): `upstream_names`, `remote_kista`, the migration.
- Unit:
  - a kista remote detected and a static one refused for `*`;
  - discovery: paging, the cap, the pull-through checks, a name leaving (cells removed, releases kept), a failed listing keeping the names;
  - a discovered name reserved only once released;
  - licence supply for a discovered name;
  - a lazy `*` mirror asking only for discovered names.
- Revocations:
  - withdrawn rows in the index and `versions/{v}`;
  - the switch: off shows (problems, the listing) and applies nothing; switching on applies the
    remote's current state at the next run; its event; refused on a remote that is not a kista;
  - serving unchanged;
  - a run withdrawing (final; `withdrawn_versions` refusing the version again), yanking in the home
    channel, activating its own yank but not an administrator's or a block;
  - a body another source holds untouched;
  - the event.
- Bundles: an export carrying revocations for the plan's bodies; an import applying them; a stale
  bundle refused; a chain of two kistas.
- End to end: kista B mirrors kista A's channel with `*`; a name published in A appears in B and
  installs with DuckDB; A withdraws its version and B's next run serves it nowhere (`INSTALL` fails),
  and A's yank followed by an activation is mirrored in B.

## Delivery

1. **PR 1, revocations (K8).** A kista remote, the index's withdrawn rows, the switch, the run's
   revocation pass, bundles' revocations, the events, the revocations listing, migration 0048
   (`remote_kista`, `revocations`, `upstream_names` with the ETags, `upstream_revocations`).
2. **PR 2, `*` on a mirror (G5).** Discovery, names in the matrix, snapshot, placement, licences and
   reservations, the names route, the console, and the end-to-end case.

Each PR runs `make test` on the three databases, `make lint`, `make console-test`,
`make console-e2e` and `make e2e` before it opens.

## Decisions

The owner's decisions, 2026-10-30:

1. An extension that leaves the source channel keeps its releases in the mirror (as a removed
   entry's releases stay).
2. Applying the source's withdrawals and yanks is opt-in per upstream, switched on by the tenant's
   administrators (The switch); off, the mirror shows them without applying them.
3. Yanks are mirrored too, and un-yanked only where this upstream made the yank.
4. The index shows a withdrawal's reason to the callers who see the row.

## Alternatives considered

- **Inferring a revocation from the path**: a `404` or an older build on the flat path may be a
  removed platform, a narrowed view or a transient failure; the index says what happened.
- **A feed of revocations** (spec 0011's feed machinery): a second channel of state to keep in step;
  the index already carries yanks and is read with the credential the mirror has.
- **Yanking everywhere a body is held on a remote yank**: a yank is a channel's decision; a
  withdrawal is the one that means "nowhere".
- **Reserving discovered names at once**: lets a remote squat a tenant's names.
- **A `kista` upstream kind**: a `repository` remote that answers `info` is enough, and stays a
  repository for DuckDB.

## Follow-ups

- Revocations for pull-through upstreams of a kista (they have no runs).
- Tenant-level upstream credentials (K7), so an Enterest-managed tenant can mirror without server
  configuration.
- The node agent acting on `withdrawn` rows (hugr-node).
- `*` in a bundle-mode upstream's plan (an air-gapped site taking every name its exporter holds).
