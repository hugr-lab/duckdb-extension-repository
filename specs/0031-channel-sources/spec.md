# Spec 0031: A name's source in a channel

- **Status**: accepted
- **Date**: 2026-10-22
- **Author**: vgsml, Claude

## Summary

A channel serves each extension name from one **source**: a mirror (an upstream of the channel), a
publisher, or the tenant itself. The source of a name in a channel is set when the name first lands
there, by whoever brings it, and a channel administrator can change it. Releases from other sources
are kept out: a publication is refused, a mirror's cell is not taken. Switching the source serves the
other source's releases at once, and switching back restores the first. This replaces spec 0009's
"a replacement wins" with "the selected source wins", so a tenant that mirrors DuckDB's or
another repository's extensions and also takes builds from a distributor (Enterest) under the same
name sees one of them in a channel, the one it chose, and never a silent mix.

## Problem

A tenant can get an extension name from several places: a mirror of DuckDB's core or community
repository, a mirror of another kista, its own CI publishing, a distributor publishing into it
(Enterest's pipeline, its spec 0003). Today kista decides between them by kind (spec 0009):

- a **replacement** (anything not mirrored) wins over a mirror whatever the order: a publication
  under a name a mirror provides needs only a grant naming it, and the mirror's cells become
  `shadowed`; a mirror cannot take a name a replacement holds, and adding an upstream entry for it is
  refused (`409`);
- two publishers of the same name in one channel simply both publish: versions mix in one index.

So a distributor's build silently replaces what a tenant mirrors, or the tenant's CI and a
distributor interleave versions of one name. What the owner wants (2026-10-22): no names reserved up
front; the channel keeps what it serves until its administrator chooses otherwise; the first to land
a name automatically becomes its source; conflicts never land; the choice is per channel.

## Design

### Sources

Every release records its **source** (a new column, set at insert, never changed):

| Source | Releases |
| --- | --- |
| `upstream:<upstream>` | mirrored by that upstream of the channel (a run, a pull-through fetch, a bundle import) |
| `publisher:<publisher>` | published or promoted by a request signed with that publisher's key (spec 0023) |
| `tenant` | everything else: an administrator's `release add`, a publication or promotion by a tenant principal holding `publish` or `promote` |

A **promotion** keeps the source of the release it promotes, whoever promotes it: a build Enterest
published into `incoming` and an administrator promoted to `prod` is Enterest's publisher's in `prod`
too. The `upstream:` form names the upstream by its id internally and by its name in answers; a
removed upstream's releases keep it (shown as removed).

### The selected source of a name in a channel

```text
channel_sources   channel_id fk, name, source, set_by, set_at;   pk (channel_id, name)
```

- **Arrival sets it.** A release inserted for a name that has no selected source in the channel sets
  it, in the release's transaction, under the channel lock (`set_by` the release's actor). That is
  the "automatic" choice: the first to land.
- **A release from another source is kept out**, checked again under the channel lock before the
  insert:
  - a publication or an administrator's add answers `409` (`type …/source`), naming the channel's
    source for the name; nothing is stored;
  - a promotion likewise, into the target channel;
  - a mirror's cell is `other_source` (a new cell outcome), decided before the download, and asked
    again at each run (so a switch takes effect at the next run);
  - pull-through never offers a name whose source in the channel is another than the upstream.
- **Serving reads only the selected source's releases**: the index, the resolver, `.well-known`'s
  keys are unchanged; the snapshot (spec 0003) keeps the selection, and a change bumps the channel's
  `release_version`. Releases of other sources (there before a switch) stay stored and keep their
  state; they are listed to administrators with `selected: false`, and never served.
- **Switching.** A channel administrator (admin on the channel; a tenant administrator) sets the
  source: `PUT /api/v1/tenants/{t}/channels/{c}/sources/{name}` with `{"source": "…"}`, any source
  the tenant has (an upstream of the channel, a publisher of the tenant, `tenant`), whether it has
  releases there yet or not. The new source's stored releases are served at once; a mirror fills in
  at its next run, a publisher publishes. `DELETE …/sources/{name}` clears it: the next arrival sets
  it again. `GET …/sources[/{name}]` lists the selections, each with the sources that have releases
  of the name in the channel (what a switch could serve now). CLI: `kista admin channel source
  <tenant>/<channel> <name> [-set <source> | -clear]`. Events: `source.set`, `source.clear` (the
  name, the source before and after, whether it was an arrival).
- **Yanks and blocks are unchanged**: a yanked release of the selected source is not served; the
  name is then served by nothing until a release of the source is current again or the source is
  switched. Nothing falls back to another source by itself.

### What changes in spec 0009

- "A replacement published into the channel later wins" and the `shadowed` cell outcome for it go:
  a mirror whose name another source holds is `other_source`; a publication under a name a mirror
  holds is refused until the channel's administrator switches the source.
- Adding an upstream or an entry whose name has releases of another source in the channel is no
  longer refused: the entry is added, its cells are `other_source` until a switch.
- Two upstreams of one channel still never list the same name (spec 0009's collision rule stays).
- **Tenant shadows** (passthrough channels, spec 0009 phase 1b) are about autoloading DuckDB's core
  builds and stay as they are.
- Reserved names (specs 0008, 0030) stay: they decide who may publish or promote a name; the source
  decides which arrival a channel takes. A distributor's tenant reservations (0030) remain available
  and are independent of sources.

### Existing data

Migration 0032 (`min_reader 32`: an older replica would serve another source's releases):
- `releases.source` filled from what each release recorded: `upstream:` for upstream releases (the
  upstream from the release's actor, `system:upstream:<tenant>/<name>`, or its cell), `publisher:`
  for releases whose actor is a publisher, `tenant` otherwise; a promotion's (origin `promotion`)
  from the earliest other release of the same build in the tenant, the one it was promoted from,
  else `tenant`. From now on a promotion copies the source of the release it promotes.
- `channel_sources` filled for every (channel, name) with a release that is not yanked: the source
  of the replacement spec 0009 serves today if there is one (so nothing a tenant serves changes),
  else the mirror's.

### Index and API answers

- The index (spec 0007) shows the selected source's releases only, as today's view: an anonymous or
  install caller sees no other source. A release row gains `source` for administrators.
- `GET …/extensions/{ext}/releases` (administrators, publishers) lists every source's releases with
  `source` and `selected`.

## Security

- **Fail closed and explicit**: a second source never lands silently, and nothing falls back to
  another source by itself (a yank leaves the name unserved rather than serving a mirror the tenant
  stopped trusting). A switch is a channel administrator's act, audited.
- **Who may switch** is who may administer the channel: a publisher (even with `publish` on the
  name), an install principal or a distributor cannot. A distribution admin (spec 0030) holds no
  `admin` on channels, so it cannot take a name from a tenant's mirror; it can only publish into a
  name that is unselected or already its publisher's.
- **Existence**: an answer that names a channel's source goes to callers who may publish or
  administer there; others get spec 0007's `404`.
- **Signing** is unchanged: only stored, signed releases of the selected source are served.

## Testing

- The store suite on three dialects: the column and the table, arrival under concurrent inserts
  (one source wins, the other refused), switching, the migration's backfill.
- Intake: a mirror's cell `other_source` before download, taken after a switch at the next run;
  pull-through skips; publications and promotions refused with `409` naming the source; a promotion
  keeps its source.
- Serving: the index and the resolver show only the selected source; a switch is seen at once (the
  snapshot), switching back restores.
- e2e: a tenant mirrors an extension, a publisher's publication under its name is refused, the
  administrator switches, DuckDB installs the publisher's build, switches back, installs the mirror's.

## Alternatives considered

- **Reserving names up front** (the catalogue in every tenant, spec 0030's tenant reservations):
  needs a distributor to know every tenant's names in advance and decides globally what a channel
  should decide. Kept in kista for whoever needs it, not used by Enterest.
- **The first to land always wins, no switching**: a tenant could never move from a mirror to a
  distributor's build without yanking and re-adding.
- **Per tenant instead of per channel**: a tenant could not try a distributor's build in a `nightly`
  channel while `prod` keeps the mirror.
- **Keeping "a replacement wins"**: the silent replacement is the problem.

## Changes to earlier specs

- **0003**: snapshots carry the channel's selections.
- **0007**: index rows of the selected source only; the `source` field for administrators.
- **0008**: publications and promotions refused (`409`) under another source.
- **0009**: replacements no longer win by kind; `other_source` replaces `shadowed` for that case;
  entries for names another source holds are allowed.
