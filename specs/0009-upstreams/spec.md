# Spec 0009: Upstreams

- **Status**: accepted; phases 1a, 1b and 2 implemented; 3 by amendment
- **Date**: 2026-10-08
- **Author**: vgsml, Claude

## Summary

A tenant fills its channels from **upstreams**: DuckDB's core repository, DuckDB's community
repository, or any other DuckDB repository (another kista, Enterest). kista fetches the binaries a
tenant allows, verifies the upstream's signature over the exact bytes it stores, checks the file as
spec 0008 checks a publication, and releases it:

- into a **signed** channel, re-signed with the channel's key (the signature means "checked and
  allowed");
- into a **passthrough** channel, with DuckDB's own core signature, so DuckDB can autoload it from
  the tenant.

Delivery is phased, each phase a pull request:

- **1a. Mirror into signed channels**: upstream records and their allowlists; DuckDB's keys; a
  streaming egress fetch; intake; runs on demand; reservation of upstream names; API and CLI.
- **1b. Schedule and passthrough**: scheduled runs and the run on a new DuckDB version; passthrough
  channels; tenant shadows; blocks reaching passthrough channels.
- **2. Pull-through**: an authorized miss on a DuckDB route fetches the binary in the background.
- **3. Private upstreams**: a Bearer credential for an upstream channel that is not public (an
  Enterest subscription). Designed in an amendment to this spec before its code.

## Problem

- **Organisations** want one place their nodes and people install from, including the core and
  community extensions they allow (`httpfs`, `spatial`, `postgres_scanner`, a few community ones),
  for exactly the DuckDB versions and platforms they run, with nothing else reachable. Today an
  operator downloads each file and runs `kista admin release add`, which also skips DuckDB's
  signature: kista cannot tell a mirrored binary from an arbitrary one.
- **Offline and restricted networks**: nodes cannot reach `extensions.duckdb.org`. Autoloading core
  extensions (DuckDB loads `json`, `parquet`, `httpfs` on first use) needs a core-typed repository
  the nodes can reach: `autoinstall_extension_repository` pointing at the tenant.
- **Enterest** is mostly a mirror; a self-hosted kista mirrors from Enterest's public channels.
- The architecture (spec 0001) fixes the rules: intake verification, passthrough, shadowing,
  pull-through only for authorized callers. Nothing implements them yet: a passthrough channel can be
  created but serves nothing (spec 0006 answers `404`).

## Design

### DuckDB behaviour relied on

On the pin (`e2e/.build/src`, `v2.0.0-dev1`, eb0d9df; paths under `src/`), confirmed in source and,
for DuckDB's servers, against `extensions.duckdb.org` and `community-extensions.duckdb.org`:

- **Paths.** DuckDB installs from `<prefix>/<version>/<platform>/<name>.duckdb_extension.gz`, or
  `<prefix>/<name>/<ext version>/<version>/<platform>/…` for `INSTALL x VERSION`
  (`main/extension/extension_install.cpp:218-232`). `<version>` is the release tag for a release
  build and the source id for a development build (`extension_install.cpp:39-48`), so a channel's
  DuckDB version may be a source id (`eb0d9df48e` on the pin), for which DuckDB publishes nothing.
  The name is the canonical one: aliases (`postgres` → `postgres_scanner`, `http`/`https`/`s3` →
  `httpfs`, …, `main/extension/extension_alias.cpp`) are applied before the URL is built.
- **Files.** DuckDB's servers answer `.gz` files (gzip of the whole file, signature included; the
  `Content-Type` is not always set), opaque S3 ETags (not content hashes), `304` to `If-None-Match`,
  `404` for a missing path, no redirects, over `http` and `https`. Community C API extensions are
  stored per DuckDB version, like the others.
- **Keys and signatures.** Core keys are `public_keys` and community keys `community_public_keys` in
  `main/extension/extension_helper.cpp` (lines 414 and 636). Every trusted key is tried
  (`extension_load.cpp:407-421`) over the composite hash (spec 0001). A repository's keys are in
  `<prefix>/.well-known/duckdb-extension-repo.json` (`extension_repository_manager.hpp:35`), read up
  to 64 KiB; a fingerprint is `sha256:` and the hex SHA-256 of the SPKI DER
  (`extension_repository_manager.cpp:116-125`). DuckDB's core site has no such file.
- **Repository typing.** Every repository given by URL, `custom_extension_repository` and
  `autoinstall_extension_repository` included, is core-typed (`ExtensionRepository("", url)` keeps
  the default type, `main/extension_install_info.hpp:135`): its files are checked against the core
  keys at install (`extension_install_dynamic.cpp:36`); autoloading and a bare `LOAD` of a non-
  community extension trust the core keys only (`extension_helper.cpp:900-916`). So a passthrough
  channel serving DuckDB's own core files works for autoload, `INSTALL x` and `LOAD x`.
- **Transport.** DuckDB's built-in client speaks plain `http` only, one `GET`, no fallback, no
  redirects (`http_util.cpp:515`); `https` goes through httpfs, which probes with `HEAD` and falls
  back to the path without `.gz` (`extension_install_dynamic.cpp:144-148`), and once httpfs is loaded
  an `http` repository is upgraded to `https` (`:311-313`). An offline autoload therefore starts from
  `autoinstall_extension_repository = 'http://…'`, and a passthrough channel must answer the same
  bytes at the same paths over both schemes, `HEAD` included, without redirects.
- **ABI.** `cpp` and `C_STRUCT_UNSTABLE` builds require their exact DuckDB version; `C_STRUCT`
  builds are checked by C API version (`main/extension.cpp:55-63`).

### Upstreams

An **upstream** belongs to a tenant and feeds **one channel** of that tenant. Two channels fed from
the same source are two upstreams; bodies are stored once per storage domain whatever feeds them. A
name is on the allowlist of at most one upstream of a channel (`409` otherwise), so one source
decides a name's current release.

**Replacements.** A release is an **upstream release** when its origin is `upstream`. A release of
another origin is a **replacement** unless its Build's origin is `upstream` (a mirrored build
promoted to another channel is still DuckDB's build, not a replacement). The rules below use these
two terms only.

| Field | Meaning |
| --- | --- |
| `name` | unique in the tenant, the grammar of channel names |
| `kind` | `duckdb-core`, `duckdb-community` or `repository` |
| `prefix` | `repository` only: an `https://` URL, normalised, with an optional port and path, and no user info, query, fragment, `.` or `..` segments (plain `http` only to loopback with `egress.allow_loopback_http`, profile `dev`); a port other than 443 must be one egress allows. Core and community use `https://extensions.duckdb.org` and `https://community-extensions.duckdb.org` |
| `keys` | `repository` only, at least one: fingerprints of the upstream's signing keys |
| `channel` | the channel it feeds; a passthrough channel takes `duckdb-core` upstreams only |
| `mode` | `mirror` or `pull-through` (phase 2) |
| `platforms` | at least one, from DuckDB's platform grammar (spec 0006) |
| `extensions` | the allowlist: entries `{name, versions, allow_reserved}`; `versions` empty means any, otherwise exact extension versions |
| `visibility` | `public` or `private` (default): the visibility of the releases it makes from now on in a signed channel (existing releases keep theirs). A passthrough channel's releases are public by nature (spec 0001) |
| `state` | `active` or `paused` |

**What is fetched** is the matrix of the channel's DuckDB versions × `platforms` × the allowlist's
names: for each cell, `<prefix>/<version>/<platform>/<name>.duckdb_extension.gz`. A repository has no
listing, so the allowlist names what to fetch; the path serves the upstream's current build, and
`versions` filters what is accepted. A version older than the upstream's current one cannot be
mirrored (one build per path). The matrix of an upstream is at most 20,000 cells and a tenant's
upstreams at most 100,000 cells: adding an entry or a platform that would exceed them is refused
(`400`); a run whose matrix exceeds them (the channel gained DuckDB versions) fails without fetching,
with that reason.

**Names.**

- An allowlist name is a canonical name in the extension-name grammar; an alias is refused with its
  canonical name (the pin's alias list is generated with the reserved names).
- A **core** name (spec 0008's reserved list) is accepted only from a `duckdb-core` upstream, a
  **community** name only from a `duckdb-community` upstream, unless the entry sets
  `allow_reserved` (a tenant that mirrors its own build of `httpfs` from another kista says so).
- `"*"` (any name, pull-through only) never matches a reserved name.

**Keys.** kista carries DuckDB's core and community key lists, generated from the pin like spec
0008's reserved names (the alias list is generated into `internal/reserved` beside them), with a CI job that fails when they change. A `repository` upstream's keys are
read at the start of each run, and when the upstream is added, from its `.well-known` file (at most
64 KiB, through egress); only keys whose fingerprints are pinned are used. Adding an upstream whose
file lists none of its pinned fingerprints is refused (`400`); a run without one fails. A rotation at
the upstream is followed by pinning the new fingerprint, never automatically.

### Intake

A cell is fetched and checked; a build is released only if all of these hold (spec 0001, "Intake
verification"):

1. **Fetch** through egress (spec 0006: public addresses unless the server allows a range, checked
   at every dial; no redirects; the deployment's own host refused) with a new streaming method: a
   compressed size cap of `blob.max_body` + 1 MiB, a rate floor of `upstreams.min_rate` (default
   64 KiB/s, applied after 30 seconds) and `upstreams.fetch_timeout` (default 10m), with timeouts of its own (the JSON fetches'
   10s does not apply). The answer goes to a temporary file first, outside the blob service's ingest
   slots, so a slow upstream never holds a slot; spooling and committing then use at most half of
   the replica's ingest slots (at least one), so runs never starve uploads.
   `If-None-Match` is sent with the cell's last ETag (at most 256 bytes) when the last outcome was
   `released`, `unchanged`, `yanked` or `conflict` (the same answer would change nothing); after
   any other outcome the cell is fetched again, so a fixed cause (a key, a filter, a block lifted, a
   shadow removed) takes effect. `404` records the cell as `missing`; `304` keeps its outcome (a
   `304` to a request that was not conditional is `failed`).
2. **Decompress**: a gzip answer (by its magic bytes) is inflated under `blob.max_body` + 256 bytes;
   a plain answer is taken as is. The result goes into a spool (spec 0005: Spool, verify, Commit).
3. **Signature**: the last 256 bytes verify over the spool's composite hash with the upstream's
   keys: core keys for `duckdb-core`, community keys for `duckdb-community`, the pinned keys for
   `repository`. The key that verified is recorded.
4. **Footer**: the platform is the cell's; a `cpp` or `C_STRUCT_UNSTABLE` build's DuckDB version is
   the cell's; a `C_STRUCT` build is one the channel serves (spec 0006). The extension version is on
   the entry's `versions` when they are given.
5. **File**: spec 0008's checks: the format matches the platform and the binary exports the name's
   entry point, so a signed binary of one extension is never served under another name. The Build
   is recorded as checked, so it can be promoted.
6. **Under the channel lock**, with the release insert:
   - a blocked body (spec 0008) is `blocked`;
   - a name with a live replacement in the channel is `shadowed` (in a passthrough channel a name the
     tenant shadows is still taken in, and hidden when served: removing the shadow serves it at once);
   - the **slot** (spec 0006: name, extension version, platform, and the DuckDB version for exact
     builds): when it holds a release of the same body in any state with any choices, the cell is
     `unchanged` (or `yanked`), and nothing changes: a yank, a visibility or a current change by an
     administrator survives every run. When it holds another body, the cell is a `conflict`:
     versions are immutable (spec 0001), so the version stays as it is in this channel; the
     upstream's next version is released normally.
7. Then the spool is committed, the Build is found or inserted with origin `upstream`, and a release
   is inserted with origin `upstream`, the cell's **original signature** on the release
   (`releases.origin_signature`: a Build's `origin_signature` is the first intake's, which a body
   added otherwise lacks), and provenance `{"upstream", "kind", "url", "etag", "key", "fetched_at"}`
   (`key`: the verifying fingerprint). The new release becomes **current** for its name and
   platform: versions of core and community extensions are often commit hashes, so kista does not
   order them; the newest accepted build is current, and an older version returns only if the
   upstream serves one this channel never held (a slot it held stays as it is). A run takes the
   cells of one (name, platform) in ascending DuckDB-version order, one at a time (different names
   and platforms run in parallel), so a `C_STRUCT` build from a newer DuckDB version's cell is
   inserted last and is the current one for every version it serves. An administrator who wants to
   hold a version pauses the upstream or narrows the entry's `versions`: the next accepted version
   is current again.

In a signed channel the release is signed with the active key, as every release; in a passthrough
channel it gets no signature rows (phase 1b). A rejected cell is logged with the upstream, the cell
and the reason, and counted in the run; it never stops the run. Spec 0010 makes these events.

### Runs

- **On demand** (1a): a tenant administrator asks for a run (`POST …/sync`, no `If-Match`, the
  upstream's `version` unchanged), which sets the upstream's `requested_at`. A **dry run**
  (`?dry_run=true`) fetches and checks every cell without releasing or committing anything, and
  without sending `If-None-Match` or touching the cells or the schedule: its counts (`would_release`
  for a cell it would release) and first 20 problems go to `last_run`. A real request and a dry one
  pending together make one real run.
- **Scheduled** (1b): every active `mirror` upstream is due at `next_run_at`, set to the end of its
  last run plus `upstreams.interval` (default 6h, within 15m..7d) with up to 10% jitter; a mirror
  never run is due at once. Adding a DuckDB version to a channel, adding a platform or adding or
  changing an entry of an upstream, or resuming it, makes the upstream due, also during its run (the
  run's end does not move a later due mark). A scheduled run interrupted by an error or a lost lease is
  due again after 5 minutes.
- Every replica polls for due or requested upstreams every 30 seconds, requested ones first, and runs
  up to `upstreams.concurrency` of them at once, each only while it holds the lease
  `kista/upstream/<id>` (renewed during the run). A request that arrives during a run is served by
  another run right after it.
- A run follows the upstream as it is: it reads the record at most every two seconds and before each
  cell, so pausing or removing the upstream stops the run after the cell in progress, and a removed
  entry or platform, a narrowed version list or an unpinned key applies to the next cell. A run starts
  only for an upstream that is active and requested or due when it starts (another replica may have
  run it meanwhile). A run interrupted by a shutdown or a lost lease leaves its request for another
  run; the lease is renewed during the run, every 30 seconds.
- A run fetches at most `upstreams.concurrency` cells at a time per replica (default 4) and at most
  2 at a time per upstream host per replica (fixed).
- A run records on the upstream its start, end, whether it was a dry run, and counts per outcome
  with the first 20 errors. The last outcome of each cell (ETag, body hash, outcome, detail, time)
  is kept in `upstream_cells`; cells that left the matrix (a DuckDB version, platform or entry
  removed) are removed by the next run; removing an entry or a platform removes its cells at once.

Removing an upstream keeps the releases it made (they are the channel's; an administrator yanks
them) and removes its cells; the names it provided stop being reserved (below).

### Reservation and shadowing in signed channels

- A name an upstream of a tenant provides is **reserved in the tenant** (spec 0001), like spec 0008's
  core and community names: publishing or promoting it needs a grant that names the extension. A
  release insert into a signed channel takes the tenant's lock `kista/tenant-upstreams/<id>` and then
  the channel lock (always in this order; the store's transactions take several lock keys), and reads
  the reservation again under them, so an upstream added between the authorization and the insert is
  seen (and a tenant shadow is recorded under them, phase 1b). Changes to allowlists take the
  tenant's lock only; inserts into a passthrough channel the channel lock only. Inserts into a
  tenant's signed channels are therefore serialized (each insert is short).
- Adding an upstream or an entry whose name has a live replacement in its channel is refused (`409`,
  naming the names) until an administrator drops the entry or yanks the releases. A replacement
  published into the channel later wins: the upstream's cells are `shadowed`.
- The index's `shadows` (spec 0008) is empty for builds that came from an upstream (mirrored, or
  promoted from a mirror) and, for replacements of a name an upstream of the tenant provides,
  `upstream` (a core or community name keeps `core` or `community`). An upstream release's index row
  carries `upstream`, the upstream's name. Adding or removing an entry bumps `release_version` on the tenant's channels, as a
  block does, so snapshots and index ETags follow.

### Passthrough channels (phase 1b)

A passthrough channel serves `body ‖ DuckDB's core signature` (`releases.origin_signature`): the
file is identical to DuckDB's; the `.gz` answer is kista's assembly of it (spec 0001), not DuckDB's
compressed bytes.

- It is fed by `duckdb-core` upstreams only; a community or repository binary would never load from
  it.
- Its releases are public and served without tokens, over `http` and `https`, `GET` and `HEAD`
  (spec 0006's rules for public releases): a token is never looked at, and a miss is `404`. It has
  no keys and no `.well-known` file; its ETag is computed over the original signature. Its upstreams'
  visibility is `public`, and setting it `private` is refused.
- Nothing is published or promoted into it (spec 0008 already refuses), and its inserts take the
  block, slot and version-bump steps without signatures or a serving key; resolving its releases
  needs no serving key either.
- **Tenant shadows.** Releasing into any signed channel of the tenant a body that is not DuckDB's own
  core build (its original signature does not verify with DuckDB's core keys), under a core name or a
  name an upstream of one of the tenant's passthrough channels lists, records the name in the
  tenant's **shadows**: the tenant's own `httpfs`, whether added, published, promoted or mirrored from
  another repository. Passthrough channels of the tenant answer `404` for a shadowed name, so autoloading
  does not silently pick DuckDB's build over the replacement. Only a tenant administrator removes a
  shadow (`DELETE …/shadows/{name}`): yanking or blocking the replacement does not bring DuckDB's
  build back. Adding and removing a shadow bumps the tenant's passthrough channels. Replacements
  released before phase 1b get their shadows when the tenant's first passthrough upstream is added,
  once per tenant (`tenants.shadows_backfilled`, migration 0009): a shadow an administrator removed
  does not come back when a passthrough upstream is re-added.
- Its releases cannot be made private (`409`). Its index rows of a shadowed name stay `active`, with
  no versions served and not current.
- **Blocks** reach passthrough channels: spec 0008's sweep and the version bump on block and unblock
  cover every channel of the tenant.

### Pull-through (phase 2)

An upstream in `pull-through` mode is not run on a schedule. A request on a DuckDB route of a
signed channel that misses (spec 0006: the binary path, no release found) offers the cell to a
bounded in-memory queue, after the answer is decided and without touching the database, when:

- the caller is authorized: a valid token holding `install` on (channel, name). Anonymous callers
  and callers without the grant never cause a fetch, and every caller gets the answer it gets
  without pull-through;
- the cell is in an upstream's matrix (the request's DuckDB version and platform, an allowlisted
  name or `"*"`); the versioned path never triggers a fetch;
- the cell is not in the replica's negative cache: every outcome but `released` keeps a cell from
  being fetched again for `upstreams.negative_ttl` (default 1h; a yanked or unchanged release still
  misses, and fetching again would change nothing), a failure (network, store) for a minute
  (bounded, in memory; a full cache evicts, it is never cleared at once).

Whether a miss may be offered is decided from the channel's snapshot (which carries the channel's
pull-through upstreams, their platforms and names, and every name an upstream of the channel lists;
every change to an upstream renews the tenant's snapshots). A worker reads the upstream as it is
now (active, the name and platform still listed, the DuckDB version still the channel's), takes the
lease `kista/upstream/<id>/<cell>` (renewed while the fetch runs, deleted after), runs the intake,
and records the cell (a `"*"` upstream records released cells only, so names callers probe do not
pile up; a `304` keeps the cell's record; a stopping fetch records nothing). Limits, all per replica:
the queue holds 256 misses, a tenant at most 32 of them; a tenant runs at most 4 fetches at once and
starts at most 60 a minute; `upstreams.concurrency` fetches run at once; a miss over a limit is
dropped (a later request retries); a cell queued or fetching is not queued again; the negative cache
holds at most 10,000 cells; a repository's keys are read at most every 5 minutes (a failing read at
most every minute).

- `"*"` is a pull-through upstream's entry only, without versions or `allow_reserved`, and not on a
  `duckdb-core` upstream (every core name is reserved: it would take none; on `duckdb-community` it
  takes the names newer than the pin). It never matches a reserved name, an alias, nor a name another
  upstream of the channel lists (an explicit entry wins; the worker checks again), and it reserves
  no name in the tenant: only listed names are reserved. A `"*"` upstream may take a name the tenant
  publishes in another channel: list names where that matters.
- A pull-through upstream is not run: `sync` answers `409`; its cells exist only for misses.
- A passthrough channel never pulls through: its callers carry no token (adding a pull-through
  upstream to one is refused).

### Private upstreams (phase 3)

A `repository` upstream whose channel serves private releases needs a Bearer token, kept in a secret
source (spec 0004's key sources show the pattern: a vault, never the database) and sent only to the
upstream's prefix. An Enterest subscription is such an upstream. The credential's model, its
rotation and egress rules are designed in an amendment before the code. Until then a `repository`
upstream fetches public releases only.

**Enterest** is not a separate kind (spec 0001 listed one): an Enterest channel is a `repository`
upstream with `https://enterest.hugr-lab.com/<tenant>/<channel>` and its pinned keys. Enterest's feed
of attachments is spec 0011's.

### API and CLI

Tenant administrators, with spec 0007's conventions (`ETag` and `If-Match` on the upstream's
`version`; add/remove items without; cursors on lists):

```text
GET, POST /api/v1/tenants/{t}/upstreams                  POST: 201, Location
GET, DELETE …/upstreams/{name}                            the record, its last run and run state
                                                          (idle, requested, dry_run_requested, running);
                                                          DELETE: If-Match
POST   …/upstreams/{name}/public, /private, /pause, /resume   If-Match
GET, POST …/upstreams/{name}/extensions; DELETE …/extensions/{ext}   POST on an existing name
                                                          replaces its versions and allow_reserved (200)
GET, POST …/upstreams/{name}/platforms;  DELETE …/platforms/{platform}
GET, POST …/upstreams/{name}/keys;       DELETE …/keys/{fingerprint}
POST   …/upstreams/{name}/sync[?dry_run=true]             202 with the upstream (its run state)
GET    …/upstreams/{name}/cells[?outcome=&cursor=]
GET    /api/v1/tenants/{t}/shadows; DELETE …/shadows/{name}   (phase 1b)
```

`kind`, `prefix`, `channel` and `mode` are fixed at creation. Every change to the record or its
allowlist, platforms or keys moves its `version` (a sync does not). Errors: `400` for an invalid
field, a passthrough channel with a non-core upstream, a reserved or alias name not allowed, a
`repository` whose keys are not listed, `"*"` outside a pull-through upstream or with versions or
`allow_reserved` or on `duckdb-core`, a pull-through upstream on a passthrough channel; `409` for a
duplicate name, a name another upstream of the channel lists, a collision with releases, or a sync
of a paused or pull-through upstream; `412`/`428` per
spec 0007. The index shows `origin: "upstream"` and the upstream's name on its releases to callers
who see the row; `provenance` to tenant administrators.

CLI: `kista admin upstream add|list|show|remove|sync|pause|resume|public|private <tenant> …`,
`upstream extension|platform|key add|remove`, `upstream cells`, `shadow list|remove`.

Config (`upstreams:`): `concurrency`, `fetch_timeout`, `min_rate`, `interval` (1b), `negative_ttl`
(2; default 1h, within 1m..24h).

Limits: 100 upstreams a tenant, 1,000 entries an upstream, 100 versions an entry, 10 pinned keys.

### Data model

Migration 0008 (phase 1a, every table below; `-- +min_reader 8`: an older replica would publish past
the new reservation), on all three dialects, with child tables instead of JSON columns:

```text
upstreams          id, tenant_id, name, kind, prefix null, channel_id, mode, visibility, state,
                   version, requested_at null, request_dry_run, next_run_at null, last_run_at null,
                   last_run (text) null, created_at, created_by        unique (tenant_id, name),
                                                                       index (channel_id)
upstream_keys      upstream_id, fingerprint                            primary key (both)
upstream_platforms upstream_id, platform                               primary key (both)
upstream_entries   upstream_id, name, allow_reserved                   primary key (upstream_id, name)
upstream_versions  upstream_id, name, ext_version                      primary key (all)
upstream_cells     upstream_id, duckdb_version, platform, name, etag null, body_hash null,
                   outcome, detail null, fetched_at                    primary key (upstream_id,
                                                                       duckdb_version, platform, name)
shadows            tenant_id, name, created_at, created_by             primary key (both)   (used from 1b)
releases           + origin_signature (bytes) null
```

`last_run` is a small JSON document in a text column (`nvarchar(max)` on SQL Server), never queried.

Migration 0009 (phase 1b) adds `tenants.shadows_backfilled` (the one-time shadow backfill).

### Package layout

```text
internal/upstream             upstream records, intake orchestration, runs, scheduler, pull-through
internal/upstream/duckdbkeys  DuckDB's core and community keys, generated from the pin
internal/reserved             + the pin's extension aliases
internal/egress               + a streaming, conditional fetch with a rate floor
internal/release              + Ingest: an upstream build into a channel (signed, passthrough)
internal/serve                + passthrough channels (1b); pull-through on a miss (2)
internal/api, cmd/kista       + upstreams, shadows
```

### Changes to earlier specs

- **0001**: no `enterest` kind (a `repository` upstream); a pull-through allowlist may be `"*"` (2); a passthrough `.gz` is identical once
  inflated, not byte for byte; DuckDB's original signature is kept per release
  (`releases.origin_signature`), the Build's being the first intake's; "an old signed binary cannot
  be served as a newer version" holds by the footer check per cell and by slots never refilled (the
  flat path names no extension version).
- **0006**: passthrough channels serve releases, with the original signature (1b); a miss can offer a
  pull-through fetch (2).
- **0007**: the index shows `origin: "upstream"`; `shadows` gains `upstream`; passthrough channels
  list releases (1b).
- **0008**: names an upstream of the tenant provides are reserved, re-checked under the lock; blocks
  sweep passthrough channels (1b); a release of a core name that is not DuckDB's build records a
  tenant shadow (1b).

## Security

- **Fail closed.** A cell is released only after its signature verifies with the upstream's keys
  over the stored bytes, its footer matches the path, and its entry point is the name's. A
  `repository` without a listed pinned key is refused, and a run without one fails.
- **Trust.** Admin input (the allowlist, pinned fingerprints) is trusted; the upstream's answer is
  not. kista's copy of DuckDB's keys and aliases comes from the pin and is checked in CI. Core names
  come from DuckDB's core repository (community names from community) unless an entry says
  otherwise, so a foreign binary is never re-signed under `httpfs` by accident; provenance names the
  kind, URL and verifying key. A passthrough channel serves only files that verify with DuckDB's core
  keys: nothing DuckDB would not have served itself.
- **Immutability.** A slot an upstream once filled is never refilled with another body, and an
  administrator's yank or visibility choice survives every run (a new upstream version becomes
  current; pausing the upstream holds the current one). Blocks cover every channel.
- **Shadowing.** A tenant's replacement of a core name hides DuckDB's build from its passthrough
  channels until an administrator removes the shadow; yanking the replacement does not silently bring
  DuckDB's build back.
- **Egress.** Fetches use spec 0006's client: addresses checked at every dial (no DNS rebinding),
  public addresses only unless the server allows a range, no redirects, the deployment's own host
  refused, size caps before and after inflation, a timeout and a rate floor.
- **DoS.** Matrices are capped per upstream and tenant; runs are one at a time per upstream with
  per-replica and per-host concurrency; downloads stay outside the ingest slots; a sync during a run
  is merged into one follow-up run. Pull-through is triggered only by authorized callers, through an
  in-memory queue and a negative cache, and capped per tenant and replica.
- **No existence leaks.** Pull-through changes no answer and adds no database work to the request.
  Runs and cells are management data (tenant admin).
- **Isolation.** Upstreams, cells and shadows are per tenant; Builds are per tenant. Bodies are
  shared within a storage domain (spec 0005), and whether another tenant holds one is not observable.
- **Passthrough** is public by nature (spec 0001): DuckDB's built-in client sends no token. Its
  guarantee is the allowlist; nothing private enters it.

## Testing

- **Intake** against a fake upstream (an `httptest` server, a test repository key): verified; a
  wrong key; a signature over other bytes; a footer for another platform or DuckDB version; a
  `C_STRUCT_UNSTABLE` build for another version; a binary of another name; a version filter; a gzip
  bomb; a compressed answer over the cap; a slow answer; `304`, `404`; a redirect and a private
  address refused; blocked and shadowed names; a slot conflict; a yanked, re-visibilitied and
  not-current release surviving a run whose ETag changed; a rejected cell refetched without
  `If-None-Match`; a dry run releasing nothing; reserved and alias names; the matrix caps.
- **Keys**: the generated lists parse and match the pin (CI regenerates them). An opt-in network test
  (`KISTA_TEST_NETWORK=1`, run in CI) takes a core and a community file of a released DuckDB version
  (several MB each, too large for `testdata`) through the intake.
- **Store**: migration 0008 and the new tables on all three dialects.
- **Races**: a publication and an upstream add on the same name; two replicas and one upstream (one
  run); a sync during a run.
- **API and CLI**: the routes × callers, `If-Match`, refusals.
- **Phase 1b**: a passthrough channel serves the upstream's file (identical once inflated, over
  `http` and `https`, `HEAD`), with the original signature; a tenant shadow hides a name and survives
  a yank of the replacement; a block yanks passthrough releases; scheduling and the DuckDB-version
  trigger.
- **e2e** (pinned DuckDB): a `repository` upstream (a second kista server in the harness, under
  another host name, since egress refuses the deployment's own) is mirrored into a signed channel;
  `INSTALL … FROM` and `LOAD` it; a new upstream version is mirrored by the next run. Passthrough
  cannot be checked by DuckDB on the pin: it is a development build for which DuckDB has published no
  binaries, and we never enable `allow_unsigned`. It is checked in Go, and in e2e once kista pins a
  released DuckDB version; the opt-in network test takes DuckDB's own `inet` for v1.4.1 through a
  passthrough channel and compares the served file with DuckDB's.
- **Phase 2**: an authorized miss fetches once (two concurrent misses, one fetch); anonymous and
  ungranted misses never fetch, with identical answers; the negative cache; the caps.

## Alternatives considered

- **Fetch the upstream's whole catalog.** DuckDB's repositories have no listing; an allowlist is
  also the policy the organisation wants.
- **Trust upstream keys from `.well-known` without pinning.** Then whoever controls the upstream (or
  its DNS) chooses what kista signs. Pinned fingerprints make a rotation a deliberate act.
- **One upstream feeding several channels.** Saves fetches, but mixes visibility, shadowing and
  passthrough rules across channels; two upstreams fetch twice and store once.
- **Order versions to choose the current one.** Core and community versions are often commit hashes;
  the newest accepted build, with slots never refilled, needs no order.
- **Shadowing while a replacement is live.** A yank would silently bring DuckDB's build back to
  autoloading clients; an explicit shadow does not.
- **Allow community binaries in passthrough.** DuckDB checks a core-typed repository against core
  keys only; they would never load.

## Follow-ups

- Phase 3 (private upstreams) by amendment.
- Spec 0010: events for intake rejections, runs and pull-through; a run history.
- Spec 0011: attachments and Enterest's feed.
- Mirroring versions older than the upstream's current one (the versioned layout, where an upstream
  serves it).
- The DuckDB e2e check of passthrough once the pin is a released DuckDB version.
