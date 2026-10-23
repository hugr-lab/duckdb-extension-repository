# Spec 0032: Lazy mirrors

- **Status**: draft
- **Date**: 2026-10-23
- **Author**: vgsml, Claude

## Summary

A mirror can be **lazy**: it lists what it may take as any mirror does, but fetches an extension for
a platform only once someone has asked for it there, and from then on keeps it current at every
run, for every DuckDB version of the channel. Entries an administrator marks **eager** are fetched
at once, as today, and **sync** fetches the whole matrix on demand. The first request for a cell not
fetched yet is answered `503` with `Retry-After` while the fetch runs, which DuckDB 2.0 retries by
itself, and kista's own fetcher now retries such an answer within its run, so another kista
mirroring a lazy channel gets the build on its first run. A distributor's catalogue (Enterest's)
can then offer a chosen set of DuckDB's core and community extensions on every platform and DuckDB
version without storing the ones nobody installs.

## Problem

A `mirror` fetches its whole matrix: every DuckDB version of the channel × every platform × every
allowlisted name, at every run. A catalogue that offers fifty of DuckDB's extensions on nine
platforms and three DuckDB versions stores over a thousand bodies, most of which no one installs.

A `pull-through` upstream fetches only what is asked for, but:

- the request that asks misses (`404`): a person runs `INSTALL` again, and another kista's mirror
  records `missing` and waits for its next run, six hours later by default;
- it is never run: a cell once taken is never refreshed, so a new version upstream never arrives;
- nothing can be fetched ahead of time, for an entry everyone will want.

The owner's design (2026-10-23): names a channel takes are fetched for everyone who asks, the ones
the administrator marks at once and the rest on first request; eager fetching is a setting of the
mirror.

## Design

### The setting

- A `mirror` upstream gains `fetch_mode`: `eager` (the default: today's behaviour) or `lazy`, set
  when it is added (`-fetch lazy`, `"fetch": "lazy"`) and changed later (`POST /{name}/fetch
  {"fetch": …}` with `If-Match`, `upstream fetch <tenant> <name> eager|lazy`; a change bumps the
  tenant's snapshots as any upstream change does). A passthrough channel refuses `lazy` (its
  requests carry no token, so no one could ask), as it refuses pull-through.
- An allowlist entry gains `eager` (`-eager`, `"eager": true`): on a lazy upstream its cells are
  fetched by every run. On an eager upstream it means nothing.
- A lazy upstream takes no `*` entry: it lists the names it may fetch.
- The matrix limits (cells per upstream and per tenant) count the whole matrix, lazy or not.

### Asks

An **ask** is (upstream, name, platform), recorded with when it was last asked (`upstream_asks`).
It is recorded when a caller holding `install` on (channel, name) misses on a DuckDB route of the
channel, plain or versioned path, for a name and platform in a lazy upstream's matrix, whether or
not the fetch is admitted at once: inserted when absent, its time refreshed only when older than an
hour, so retries and polls cost a read. An ask covers every DuckDB version of the channel: once the
channel serves a new DuckDB version, the next run fetches the extension for it.

### Runs

A run of a lazy upstream fetches, of its matrix, the cells of its asks and of its eager entries,
conditionally (the ETag kept), as an eager run fetches every cell; cells outside them are neither
fetched nor recorded. The schedule, outcomes and limits are a mirror's; `last_run` adds the count
of cells `not_asked`. A dry run covers the same cells.

**Sync** (`upstream sync`, `POST /{name}/sync`) on a lazy upstream fetches the whole matrix once:
the administrator's "fetch now". It records no ask, so later runs stay lazy. A `bundle export
-sync` of a lazy upstream syncs the cells its plan exports.

Switching to `eager` makes the upstream due at once (the whole matrix); switching to `lazy` keeps the
releases there are and records an ask for each (name, platform) a release covers.

A run of a lazy upstream and a first-request fetch never fetch one cell at once: the run takes the
cell's lease (the pull-through worker's, `kista/upstream/<id>/<version>/<platform>/<name>`) and
skips, and counts, a cell another replica holds. Eager upstreams take no cell leases, as today.

### The first request

When a caller holding `install` on (channel, name) misses on the plain path of a DuckDB route, and a
lazy upstream of the channel has the cell in its matrix, the ask is recorded and the cell offered
to the pull-through worker (spec 0009, which now takes lazy upstreams too, and a miss's principal
for its share). In the ask's transaction the cell's row and lease are read, so every replica
answers alike:

- `503` with `Retry-After: 5` while the cell was never fetched, or its lease is held (a fetch is
  running): the build is coming;
- otherwise today's answer (`404`): the cell was fetched and answered something else (the upstream
  has no such cell, it was rejected, another source holds the name, the body is blocked, the fetch
  failed); a failed cell is fetched again by the next run, or by an ask after the negative cache's
  minute;
- a caller without the grant, or anonymous, gets today's answer and asks nothing.

The versioned path records the ask but fetches nothing (a repository serves its current build
only); it answers as today.

**DuckDB relied on** (on the pin): a client retries a `503` as a throttled answer after its
`Retry-After` (src/main/http/http_util.cpp:506-509), three retries and five more for throttling
(src/include/duckdb/main/http/http_util.hpp:38, http_retry_budget.hpp:124, the delay capped at 20
seconds, hpp:122), within a retry quota shared per host (hpp:70), and waits 30 seconds for each
answer (http_util.hpp:37). At `Retry-After: 5` that is about 40 seconds, so DuckDB 2.0's first
`INSTALL` usually succeeds, also through two lazy kistas. DuckDB 1.x
retries without honouring `Retry-After`, briefly, and may fail the first `INSTALL`; the person runs
it again (the docs say so).

### kista's own fetcher

An upstream answering `503` or `429` with `Retry-After` of at most 30 seconds is asked again after
that delay, at most three times for a cell, before the cell is `failed`: for runs, pull-through and
lazy fetches, of every kind. The retry is the cell's (around the download in the run's cell
fetch), the partial file truncated and the per-host slot released while it waits. So a kista
mirroring a lazy channel, with a credential whose principal holds `install` there, gets each build
on its first run, and a storm of `503`s costs a run at most about a minute and a half a cell. Without a credential (a public lazy channel) it asks nothing, and gets only what
others asked for.

### Limits

First-request fetches share pull-through's limits per tenant (fetches at once and per minute, the
queue of 32 a tenant), now with a share per caller too (8 queued asks a principal), so one
downstream kista's run cannot starve the other callers of a tenant. They have a per-host gate of
their own on each replica (one fetch a host), so a refresh run holding the run's slots for a host
does not hold a first request back. A dropped offer keeps its ask, so the next run fetches it.

### What does not change

A lazy upstream's releases are a mirror's in every other way: intake verification, licence keys
(spec 0021), the name's source in the channel (spec 0031: `upstream:<id>`), its allowlisted names
provided in the tenant at once (the server-wide reservation of a core or community name, spec 0030,
comes with its first fetch, as for any mirror), visibility, credentials, usage, statuses and events.

### Observability

- Metrics: lazy asks by outcome (`fetched`, `answered_503`, `dropped`), and asks recorded per
  upstream; the extension route's duration is not held by a wait (there is none).
- Events: `upstream.release` carries `trigger` (`run`, `sync`, `ask`); an ask's other outcomes are
  the cell's, as a run's are.
- The cells API answers `not_asked` cells of a lazy upstream with `?outcome=not_asked`, so an
  administrator, or a distributor's console, sees what is offered but not fetched yet.

### Data model

- `upstreams`: `fetch_mode` (`eager` | `lazy`, default `eager`);
- `upstream_entries`: `eager` (boolean, default false);
- `upstream_asks`: (upstream, name, platform) with `asked_at`; removed with its entry or platform;
- all three dialects, one migration each; the minimum reader moves (an older replica would run a
  lazy upstream as an eager one, and its pull-through worker would ignore it).

### Docs and the console

upstreams.md (the modes, the setting, runs, asks, the first request, a comparison with
pull-through), configuration.md, api.md (`/fetch`, `eager`, `not_asked`), duckdb.md (the first
`INSTALL` from a lazy channel: DuckDB 2.0 and 1.x), observability.md, security.md, the commands.
The console's upstream form takes the setting, an entry its `eager` mark; a lazy upstream's page
shows asked and not-asked cells and **Fetch now**. A downstream kista of a lazy channel is best lazy
too, or narrow in its platforms: an eager mirror of a lazy channel asks for every cell at every run.

## Security

- **No anonymous fetches**: only a caller holding `install` on the name causes one, as for
  pull-through; the allowlist bounds what can be fetched at all.
- **Bounded**: fetches per tenant and per caller are limited; no request is held open.
- **Nothing learned**: a caller without the grant gets the answer it gets today, fetched or not.
- Intake verifies a lazily fetched body as any mirrored one: DuckDB's keys or the pinned ones.

## Testing

- The store suite on all three dialects: the columns and `upstream_asks`.
- Unit: a lazy run fetching asked cells and eager entries only, every DuckDB version of an ask,
  refreshing an asked cell to a new version, `not_asked` counted; sync fetching the matrix without
  asks; a first request answered `503`, then served; a refusal answered `404`; a caller without the
  grant asking nothing; two replicas answering a miss alike; a versioned miss recording an ask; a
  run skipping a cell a fetch holds; the fetcher retrying a `503` with `Retry-After` within a cell,
  three times at most, its host slot free meanwhile; the per-caller share; switching modes both
  ways; a passthrough channel refusing `lazy`.
- End to end with DuckDB: a lazy `duckdb-core` upstream in a signed channel; `INSTALL` of an
  allowlisted extension succeeds on the first try (DuckDB's own retry), and the next run refreshes
  it; another kista mirroring that channel with a credential takes the build on its first run; an
  eager entry is fetched before anyone asks.

## Alternatives considered

- **Holding the first request until the fetch ends**: needs a per-cell signal across replicas and
  every drop path, ties connections up behind proxies, and is cut short by a downstream kista's
  10-second header timeout anyway; DuckDB 2.0 retries a `503` by itself.
- **Changing pull-through instead**: its users expect no runs; a lazy mirror is a mirror's setting,
  as the owner asked.
- **Asks per cell**: every DuckDB upgrade would wait again for every extension; an ask per (name,
  platform) fetches all of the channel's versions.
- **Listing an upstream's versions without bodies**: a DuckDB repository has no listing, and the
  version is in the body's footer.

## Follow-ups

- Forgetting asks nobody renewed in a long time (their releases stay, runs stop refreshing them).
- Anonymous asks on a public lazy channel, if a public catalogue needs them.
- `sync` of one entry.
