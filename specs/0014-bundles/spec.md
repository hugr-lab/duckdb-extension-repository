# Spec 0014: Bundles for air-gapped sites

- **Status**: accepted; phase 1a part 1 (bundle mode) implemented; parts 2-5, 1b and 2 to come
- **Date**: 2026-10-14, revised 2026-10-15 (the export from a connected kista replaces the builder)
- **Author**: vgsml, Claude

## Summary

An air-gapped kista cannot reach its upstreams. A **bundle** carries what those upstreams would
have served, on removable media or through the site's object store: the extension files exactly as
each source served them, the source's published keys, and (phase 1b) signed statements about the
files (spec 0011).

The cycle has three steps:

1. **Plan.** The site writes a **plan** from its own configuration: its bundle-mode upstreams,
   their allowlists and platforms, its channels' DuckDB versions, and what it already holds.
2. **Export.** A connected kista that mirrors the plan's sources, a gateway kista of the
   organisation or Enterest, exports the plan's cells: each file exactly as its source served it,
   from what its mirrors already fetched and verified. `kista admin bundle export` writes one tar
   and signs its manifest with a **bundle key** that the site pinned beforehand.
3. **Import.** The site imports the bundle into its **bundle-mode upstreams**. A file is released
   only if all of these hold:
   - its signature verifies with a key the site pinned for that source;
   - the signed manifest names it for that cell, in a bundle newer than the last one imported;
   - its footer matches the cell;
   - it passes spec 0008's file checks.

   A signed channel re-signs the file with its own key. A passthrough channel serves DuckDB's own
   core files, so nodes on the site autoload without the internet.

The two signatures answer different questions. The source's signature says the file is authentic.
The bundle key's signature says that this file is what the exporting side held for this cell when
the bundle was exported. The second one is what stops whoever carries the bundle from swapping in an old,
authentic, vulnerable build.

**kista and Enterest.** kista holds everything a site and a gateway need: the plan, the export, the
import, the format and the trust model. Air-gapped organisations run kista, and the boundary they
trust is open code. Bundles as a **service** are Enterest's: curated sets, a subscriber's plan
exported on a schedule with Enterest's reports (phase 1b) and its private extensions, delivered to
the subscriber's object store. Enterest does it with the same `kista admin bundle export`.

Delivery, each phase a pull request:

- **1a. Extensions**, in five pull requests, in this order:
  1. migration 0020, bundle mode and its guards (sync, credential, egress, the online key fetch,
     passthrough), the API record's and `upstream show`'s last-bundle fields, the console's upstream
     forms (no "Sync now", "Dry run" or credential for bundle mode), the reaper entry;
  2. `internal/bundle`: the format, validation, canonical order, signing and verifying, the tar
     reader and writer; `kista bundle keygen|keys|sign|inspect` and `kista admin bundle plan`;
  3. the import: the intake split, the records, the events, `bundle keys` per tenant,
     `bundle_sources` and `MayPublish`, `bundle reset`; tested with bundles the tests write;
  4. the export, then the e2e test;
  5. the air-gapped docs page and its CI job.
- **1b. Statements**: bundle-mode feeds. The export carries the statements and documents its feeds
  hold for the bodies the site offers, and the import verifies them as spec 0011's feed client does.
- **2. Upload over the API**: an upload route and a console page, for administrators who cannot run
  `kista admin` beside the server and cannot write to its object store.

## Problem

Air-gapped sites and sovereign clouds are one of the audiences spec 0001 names. The architecture
gives them bundles (spec 0001's open-core table). Specs 0010 and 0011 left air-gapped events and
attachments to this spec.

Today an operator of such a site downloads each file on a connected machine, carries it in, and
runs `kista admin release add`. That path loses everything spec 0009 gives a mirror:

- **No signature check.** `release add` verifies no upstream signature. kista cannot tell a
  mirrored binary from an arbitrary one, and neither can its audit.
- **No passthrough.** A passthrough channel takes only upstream releases. Without one, nodes cannot
  autoload `httpfs` or `spatial`.
- **No policy.** The allowlist, the version filters, the reserved names and the tenant shadows all
  live on upstreams. A hand-added release bypasses them.
- **No bookkeeping.** Nothing records which DuckDB versions and platforms were brought in, or what
  changed upstream since the last visit.

A concrete case: an organisation runs hugr nodes in a classified network on DuckDB 2.0,
`linux_amd64` and `linux_arm64`. It wants:

- `httpfs`, `spatial` and `postgres_scanner` from DuckDB's core repository, autoloadable;
- two community extensions;
- its own `duckdb_acl` and `tresor` from Enterest, a private subscription.

Signed channels need DuckDB 2.0's trusted repositories (spec 0001). A DuckDB 1.x node can use the
passthrough channel only.

Once a month someone carries a disk in. What lands on the site must be what the organisation's
connected mirror would have accepted at the time of the build, and nothing else.

## Design

### Bundle-mode upstreams

An upstream's `mode` (spec 0009: `mirror` or `pull-through`) gains **`bundle`**. A bundle-mode
upstream is the same record as any other:

- `kind`, `prefix`, `keys`, `channel`, `platforms`, `extensions`, `visibility` and `state` mean what
  they mean for a mirror;
- the matrix caps apply: 20,000 cells per upstream and 100,000 per tenant.

It differs in these ways:

- **It never fetches.**
  - It is never due; spec 0009's scheduling already selects mirrors only.
  - `sync` answers `409`, as for pull-through (a new `ErrBundle` beside `ErrPullThrough`).
  - It is never a pull-through target.
  - kista never contacts its `prefix`: for a `repository` source, the prefix is the source's
    identity. Adding one therefore skips the egress port check that `checkPrefix` makes for an
    online source. Core and community upstreams keep their `NULL` prefix and match a bundle by kind.
- **Its keys are not read online.** Adding a bundle-mode `repository` upstream does not fetch the
  source's `.well-known` file. Whether the source lists a pinned key is checked at import: the
  bundle carries the file. The docs give operators the way to learn fingerprints out of band (Keys
  below).
- **It takes no credential.** Spec 0009 phase 3 credentials are how kista authenticates to a source,
  and this upstream contacts none. Creation with a credential is `400`, and so is
  `POST …/credential` on a bundle-mode upstream. The exporting kista's upstream authenticates instead, and the site's
  configuration plays the credential's part for visibility (Visibility).
- **A passthrough channel takes it** when its kind is `duckdb-core`. The passthrough guard that
  today admits mirrors only admits bundle mode too; pull-through stays refused.

`mode` is fixed at creation. A site that gets a network link later adds a mirror and removes the
bundle-mode upstream: the releases stay with the channel (spec 0009).

`kista admin upstream list` prints the mode, and `show` the mode and the last bundle. The API's upstream record carries it already.
The console's add form offers `bundle`, and its "Sync now" button is hidden for bundle mode.

### Bundle keys

A bundle key signs a bundle's manifest. It is an Ed25519 key in a PEM file, made where it will be
kept:

```sh
kista bundle keygen -o bundle-key.pem      # prints the fingerprint: sha256:<hex of the SPKI DER>
```

The fingerprint is computed as kista computes every key's (`internal/extfile/keys.go`): SHA-256 of
the DER of the parsed public key, re-marshalled, never of the PEM's bytes. Only Ed25519 is accepted.

The site pins fingerprints in its configuration, per tenant, as credentials are scoped. This is
configuration, not data, because bundle keys are a server administrator's trust decision, like key
sources; the whole `upstreams` block is file-only, and a change applies at the next start (the CLI
import reads the file when it runs):

```yaml
upstreams:
  bundle_keys:                             # at most 10 entries
    - fingerprint: "sha256:9a1f…"
      tenants: [acme]                      # or "*"
```

- **Every bundle is signed, and an unsigned bundle is refused.** The bundle carries the public key
  (`kista-bundle.pub`), and the import uses it only when its fingerprint is pinned **for the tenant
  being imported into**: one tenant's key holder cannot sign for another tenant on a shared site.
- **Where the key lives.** The export can sign as it writes (`-key`), which puts the key on the
  exporting kista, an internet-facing server. A compromised gateway then holds the key. For that
  reason the export can also write an unsigned bundle (`-unsigned`), which **`kista bundle sign`**
  signs on another machine that holds the key, offline. Enterest signs subscribers' bundles that
  way, and the docs recommend it.
- **Rotation.** Pin the new fingerprint, sign with the new key, then remove the old fingerprint.
- **What the key's holder can do.** Choose what the site receives, within the site's allowlists and
  filters and the sources' signatures: withhold, mark cells `missing` or `rejected`, leave out the
  `authenticated` flag, or offer an older authentic build. The import's per-cell freshness (Importing
  a bundle) refuses an older build for a cell the site already holds newer. A site that subscribes
  to Enterest trusts Enterest's key this far; an organisation's own gateway, its own. Keys in KMSs and
  vaults (spec 0004's key sources) are a follow-up.

### The plan

```sh
kista admin bundle plan <tenant> [-upstream <name>,…] [-o plan.json]
```

The plan lists the tenant's bundle-mode upstreams (all of them, or the named ones) as JSON. It holds
no secret:

```json
{
  "format": "kista-bundle-plan/v1",
  "created_at": "2026-10-14T12:00:00Z",
  "tenant": "acme",
  "bundle_keys": ["sha256:9a1f…"],
  "sources": [
    {"kind": "duckdb-core",
     "cells": [{"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "httpfs",
                "versions": [], "etag": "\"9f1c…\"", "body_hash": "3b0c…"}]},
    {"kind": "repository", "prefix": "https://enterest.hugr-lab.com/acme/prod",
     "keys": ["sha256:4be1…"], "cells": [ … ]}
  ]
}
```

- **Sources.** There is one source per kind for core and community, and one per normalised prefix
  for a repository. Two upstreams of the same source, such as core feeding a passthrough channel
  and a signed channel, merge into one source. Its keys are the union of the upstreams' pinned
  keys.
- **Cells.** The cells are the union of the upstreams' matrices: the channel's DuckDB versions,
  the platforms and the allowlisted names. Each cell has `versions` from the entry's filter. For a
  merged cell `versions` is the union of the filters, and an empty filter (meaning any version)
  makes the union empty. The export uses `versions` only to skip files the import would refuse;
  the import applies each upstream's own filter.
- **Dev versions.** A channel's `dev` DuckDB versions (source ids) are left out of core and
  community cells, since DuckDB publishes nothing for them. A repository source keeps them: another
  kista may serve development builds.
- **What the site holds.** `etag` and `body_hash` are given for a cell only when every upstream
  sharing it has a conditional outcome for it (`released`, `unchanged`, `yanked` or `conflict`; spec
  0009) with the same values. Otherwise both are omitted, so a cell one upstream holds and another
  lacks is fetched in full. `body_hash` is spec 0002's composite hash of the body without the
  signature, in hex, as cells store it.
- **Bundle keys.** `bundle_keys` repeats the fingerprints pinned for the tenant.
- **Its date bounds old bundles.** A target takes no bundle whose plan is older than the target
  itself (`plan.created_at` before the upstream's `created_at`), so a new or re-created upstream
  cannot be filled from a bundle made for an earlier configuration.
- **Size.** A plan holds at most 100,000 cells.
- **Hand-written plans.** A plan may be written by hand. The export validates it with spec 0009's
  grammars before reading anything.

### Keys for repository sources

Pinning a repository source's key on the site needs its fingerprint from somewhere trustworthy:

- **From the publisher.** A publisher shows its fingerprints out of band; Enterest shows them in a
  subscription's console.
- **From the connected side.** `kista bundle keys <prefix>` fetches the source's `.well-known` file
  and prints each key's fingerprint, for comparison with what the publisher published.
- **From the export.** The export prints every source's listed fingerprints in its summary.
  `kista bundle inspect` marks each one as pinned on the site or listed but not pinned, which is how
  an operator notices a key rotation at the source.

The media are never the authority for a fingerprint. Neither is the plan: a plan only repeats the
site's pins.

### Exporting a bundle

A bundle is made by a **connected kista** that mirrors the plan's sources: a gateway kista the
organisation runs on the connected side, or Enterest for its subscribers. It exports what its
mirrors already fetched, verified and recorded (spec 0009), so the bundle path adds no second
fetcher.

```sh
kista admin bundle export <tenant> -plan plan.json (-key bundle-key.pem | -unsigned) -o acme-2026-10.kbundle \
    [-sync] [-private <prefix>]…
kista bundle sign -key bundle-key.pem -in acme-2026-10.unsigned -o acme-2026-10.kbundle   # offline
```

`<tenant>` is the exporting kista's tenant whose upstreams and channels serve the plan; it need not
share the site's tenant name. With `-key`, the export signs only if the key's fingerprint is among
the plan's `bundle_keys`, and stops before reading anything otherwise; `kista bundle sign` makes
the same check. The plan is validated with spec 0009's grammars first: a plan is input, whoever
wrote it.

For each source in the plan, taken in canonical order (kind, then normalised prefix):

1. **Where the files come from.**
   - **An upstream of `<tenant>`** of the same source: the same kind for core and community, the
     same normalised prefix for a repository, active, in `mirror` or `pull-through` mode. When
     several match, a cell is taken from the first that holds a servable release for it, in the
     order of their names.
   - **Or one of `<tenant>`'s own signed channels**, when a repository source's prefix is
     `<serve.public_url>/<tenant>/<channel>`: Enterest exporting its own extensions to a subscriber,
     or a gateway exporting what it publishes. A passthrough channel has no key of its own and is
     never a source. Here the site trusts the exporting kista's channel key, which it pinned when it
     planned that prefix.
   - A source with neither fails as a whole: "not mirrored here".
2. **Keys.** The export checks with the **plan's** keys, which are the site's pins:
   - core and community: DuckDB's keys as the upstream service holds them: compiled in, or
     `upstream.Service.DuckDB` in tests;
   - a repository source: the export fetches `<prefix>/.well-known/duckdb-extension-repo.json` (at
     most 64 KiB) through the matching upstream's egress and credential, as a mirror's key check
     does, and keeps it as it came; for an own channel it writes the channel's own file. A source
     whose file lists none of the plan's keys fails as a whole.
3. **The release decides, not the cell record.** A cell record is written only by runs and
   pull-through misses, so a yank, a block or a `release current` made since would not show in it.
   For each plan cell the export looks up the release that serves the cell on the exporting side:
   the channel's servable release for (name, platform, the cell's DuckDB version), the one DuckDB
   would install from that channel now. Then:

   | On the exporting side | Exported as |
   | --- | --- |
   | a servable release from this source, with an original signature | the file, unless `unchanged` (below) |
   | a release that is yanked, blocked or deprecated-and-hidden | `rejected`, "withdrawn on the exporting kista" |
   | a release from another source, or published there | `rejected`, "not this source's build" |
   | no release, cell `missing` or no record | `missing` |
   | no release, cell `rejected`, `blocked`, `shadowed` or `conflict` | `rejected`, with the mirror's reason |
   | no release, a pull-through upstream never asked for the cell | `missing`, "never fetched by pull-through" |
   | outside the exporting upstream's matrix (allowlist, platforms, DuckDB versions) | `missing`, "outside the exporting matrix" |
   | a private release (private channel, or `private` visibility) not covered by `-private` | `rejected`, "private on the exporting kista" |

   **`unchanged`**: the release's body hash equals the plan cell's `body_hash`. The cell then carries
   that `body_hash` and no file. The plan's ETag alone never decides.
4. **The file.**
   - From an upstream: the release's body followed by its **original signature**
     (`releases.origin_signature`, spec 0009), a file the source signed, verifiable with its keys.
     For a `C_STRUCT` release that fills several cells, the signature is the first intake's; the
     file verifies with the source's keys but may not be the exact bytes the source serves on
     another path. A release without an original signature is `rejected`.
   - From an own channel: the body followed by a signature of the channel by a key that is both
     among the plan's pins and listed in the channel's `.well-known` file, the serving key first: a
     site that still pins a key the channel rotated away from receives files it can verify.
   - Nothing is re-signed by the export.
5. **Checks.** Each file is checked as the import will check it: the signature with the plan's
   keys, the footer against the cell, the version filter. A failure records `rejected` and leaves
   the file out. The import runs every check once more, with spec 0008's file checks.

**Private releases.** `-private <prefix>` names the plan sources whose private releases may leave:
an own channel's private builds, or a credentialed mirror's (spec 0009 phase 3) private releases.
Without it, they are `rejected`. It covers `<tenant>` only, so an operator cannot export another
tenant's private builds on a plan.

**`-sync`** first runs the matching mirror-mode upstreams (spec 0009's `sync`) and waits for them;
pull-through upstreams export what they hold, and their never-asked cells are reported as such.

**Per-cell fields.** Each exported cell carries `fetched_at`: the time the exporting side last
confirmed the cell against the source (the cell record's, so a mirror that keeps answering
`unchanged` keeps it fresh), or for an own channel the release's `released_at`. It carries `etag`
when the cell record has one. `authenticated` is per cell: true when the release came through a
credentialed upstream, or is an own channel's private release. The site computes each cell's URL
itself.

**Writing.** Files go straight from the blob domain into the tar: the stored gzip stream of the
body, with the signature appended as a gzip tail (`extfile.GzipTail`), so no temporary copy is
made. The manifest goes first and needs every file's size and SHA-256, so the export reads each
body twice: once to hash, then to write. The tar is written to a temporary name beside `-o`, and
renamed when complete; the output is the only disk it needs. `-unsigned` writes the manifest
without the signature and key entries; `kista bundle sign` copies the bundle once, inserting them
after the manifest, and refuses one already signed.

**The `exporter` field** names the exporting kista's version and `serve.public_url`, each at most
256 bytes of printable text, for the record only: the site's trust is the bundle key.

**Audit.** The exporting kista records a **`bundle.export`** event: the plan's SHA-256, the
manifest's `id` and SHA-256, the bundle key's fingerprint (none when unsigned), counts per outcome,
the actor. `kista bundle sign` is a client command and records nothing; the import records the key.

The summary lists, per source: cells per outcome, bytes, the listed fingerprints, the first 20
problems, and the bundle's SHA-256 as the import will compute it (below). The export exits non-zero
when a source failed as a whole; rejected and missing cells alone do not fail it.

**Who exports.** A server administrator of the exporting kista, through `kista admin`. A plan
discloses the site's configuration to the exporting side: to Enterest, for its subscribers, that is
part of the service, and the docs say so. An API route is phase 2.

### The bundle

A bundle is a POSIX tar of regular files, ustar or PAX. Extension files inside it are gzip streams,
as DuckDB's servers serve them: native libraries shrink three to four times, and the import
inflates under a cap as a mirror does. The tar itself is not compressed. The entries come in this
order:

```text
kista-bundle.json                      the manifest; first; at most 64 MiB
kista-bundle.sig                       64 raw bytes: Ed25519 over "kista-bundle-v1\0" ‖ the manifest's bytes
kista-bundle.pub                       the bundle key's public key (PEM, Ed25519)
sources/<n>/well-known.json            a repository source's .well-known file; all before any file
files/<sha256 hex>.gz                  an extension file, gzip-compressed (one member, nothing after
                                       it); named by the SHA-256 of the inflated file (body ‖ signature)
```

An unsigned bundle (`-unsigned`) lacks the second and third entries until `kista bundle sign` adds
them; the import refuses it.

**Entries.** Names match exactly `kista-bundle.json`, `kista-bundle.sig`, `kista-bundle.pub`,
`sources/[1-9][0-9]{0,4}/well-known.json` and `files/[0-9a-f]{64}\.gz`. The reader checks the raw
headers itself rather than relying on Go's `archive/tar`, which merges PAX and GNU names silently:
a PAX global header (`g`), GNU long names or links (`L`, `K`), sparse entries (`S`, or
`GNU.sparse.*` records), links, devices and directories are counted as skipped, and among the
first three entries they refuse the bundle. A `sources/` entry after a `files/` entry refuses the
bundle.

The manifest:

```json
{
  "format": "kista-bundle/v1",
  "id": "0192…",                              // a UUIDv7 made by the export
  "created_at": "2026-10-14T12:30:00Z",
  "exporter": {"version": "kista v0.3.0", "public_url": "https://gateway.example.org"},
  "plan": {"tenant": "acme", "created_at": "2026-10-14T12:00:00Z", "sha256": "…"},
  "sources": [
    {"kind": "repository", "prefix": "https://enterest.hugr-lab.com/acme/prod",
     "well_known": {"path": "sources/1/well-known.json", "sha256": "…", "size": 812},
     "cells": [
       {"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "duckdb_acl",
        "outcome": "fetched", "file": "files/7a3c….gz", "sha256": "7a3c…", "size": 1388416,
        "inflated_size": 4194560, "body_hash": "3b0c…", "etag": "\"…\"",
        "fetched_at": "2026-10-14T12:31:02Z", "authenticated": true},
       {"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "tresor",
        "outcome": "unchanged", "body_hash": "9e21…", "fetched_at": "2026-10-14T12:31:05Z"},
       {"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "acl_otel",
        "outcome": "rejected", "detail": "withdrawn on the exporting kista"}
     ]}
  ]
}
```

- **Signed.** The manifest binds every file to its cells by hash and size, and the signature covers
  its exact bytes. A carrier can withhold the whole bundle but cannot change, add, move or drop
  anything inside it without the import noticing.
- **Validated before use.** The import decodes the manifest strictly (unknown fields and duplicate
  keys refused) and validates it before anything else:
  - the format and the grammar of every field;
  - sources unique after normalisation;
  - cells unique within a source;
  - one file per cell;
  - every reference to a file giving the same sizes;
  - `etag` at most 256 bytes, `detail` at most 1,000 bytes and `exporter`'s fields at most 256
    bytes (the stored record at most 1,000), of printable UTF-8 (control characters stripped before they are stored or shown);
  - `fetched_at` and `created_at` in RFC 3339, and `id` a UUIDv7;
  - `created_at` at most 10 minutes past the importing host's clock;
  - at most 100,000 cells and 50,000 files.
- **Order.** Sources come in canonical order (kind, then normalised prefix). Files appear in
  canonical order: by source, then name, then platform, then the lowest DuckDB version among the
  cells each file fills, compared with spec 0009's version order (`release.CompareVersions`).

### Importing a bundle

```sh
kista admin bundle import <tenant> <file | - | blob:<domain>/imports/<name>> \
    [-upstream <name>,…] [-dry-run] [-any-tenant]
```

- `-` reads standard input, for a pod whose image has no shell or `tar`:
  `kubectl exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml bundle import acme - < acme.kbundle`.
- `blob:` reads an object that the operator uploaded to the site's object store with its own tools
  (`azcopy`, `aws s3 cp`, `gsutil`, `mc`; these resume a broken upload) under the domain's new
  `imports/` prefix, inside the domain's configured store prefix: `imports/<name>`, where `<name>`
  matches `[a-z0-9][a-z0-9._-]{0,127}`.
  - The blob layer admits `imports/` keys for reading (`Get`, `Stat`) and refuses them for writing
    and deleting: kista never changes what the operator uploaded.
  - The storage collector (spec 0016) lists only `streams/` and `tmp/`, so it never touches
    `imports/`; the operator removes the object after the import.
  - A transient read error re-opens the object at the current offset (`Get` with an offset), up to
    5 times, instead of failing a read of gigabytes.
  - This is how a site without `kubectl exec` imports, for example by running an Azure Container
    Apps job, and it avoids feeding gigabytes through `exec`.

The import reads the bundle once, front to back, and never extracts it. **`input_sha256`** is the
SHA-256 of every byte through the tar's end-of-archive blocks, whatever the input; the export
prints the same value.

1. **Head.**
   - The first three entries must be the manifest, its signature and the public key.
   - The key's fingerprint must be pinned in `upstreams.bundle_keys` for the tenant, and the
     signature must verify.
   - The manifest must pass validation.
   - `plan.tenant` must be the tenant being imported into, unless `-any-tenant` is given. Importing
     one bundle into two tenants is two imports, each by a server administrator.

   Any failure refuses the bundle before anything is released.
2. **Targets.**
   - **Selection.** The import selects the tenant's active bundle-mode upstreams, or the named ones.
     A target is matched to a source by kind (core, community) or by normalised prefix (repository).
     An upstream without a source in the bundle is not touched.
   - **Leases first.** The import takes `kista/upstream/<id>` for each target and renews it, then
     checks that the tenant is writable, as a run does, so spec 0019's reaper never sees a gap. A
     target already leased (a run or another import) is reported `busy` and skipped. An import into
     a tenant being deleted is refused; a suspended tenant is imported into, as runs are.
   - **Freshness.** Bundles are ordered by (`created_at`, `id`). A target refuses, as `stale`:
     - a bundle ordered before the last one it imported;
     - the same `id` with a different manifest (its SHA-256 differs from the one recorded);
     - a bundle whose `plan.created_at` is before the target's `created_at`.

     The same `id` and manifest resumes an interrupted import. The mark (`bundle_id`,
     `bundle_created_at` and the manifest's SHA-256 on the target) is recorded once the leases are
     held and before the first release, so an interrupted import still closes the door on older
     bundles. A dry run never records it.
   - **Matrix.** Each target's matrix is computed as a run computes it, with the same caps: a matrix
     over its cap fails the target, as a run fails.
   - **Unplanned cells.** Bundle cells outside every target's matrix are counted `unplanned`.
3. **Keys.** Core and community targets use DuckDB's keys as the upstream service holds them. A
   repository target uses the keys in its source's `.well-known` entry whose fingerprints it pinned.
   That entry's hash and size must match the manifest, and it precedes every file. A target with no
   pinned key in it fills nothing; its run is `failed` with `upstream.keys: the bundle's keys list
   none of the pinned fingerprints`.
4. **Each file**, in tar order:
   - **The entry.** It must be a regular file named in the manifest, of the stated size. Its gzip
     stream (one member, nothing after it) is inflated **straight into the spool** (`blob.spool_dir`)
     through a SHA-256 tee: no other temporary copy. The inflated size may not exceed the manifest's
     `inflated_size`, which itself may not exceed `blob.max_body` + 256 bytes, and its SHA-256 must
     match; otherwise the spool is dropped and the file fills nothing.
   - **Skipped entries.** Unnamed entries, other entry types and second copies are read through
     without being stored, and counted. Their total size is capped at 64 MiB: past the cap the
     bundle is refused from that point on.
   - **Once per file.** The file is checked once: the signature with each target's keys, spec
     0008's file checks, the footer parsed. The spool stays open until the file's last cell, so the
     re-commit spec 0009 makes when a Build disappears meanwhile (`ErrBuildGone`) still works.
   - **Per cell.** For each target cell the file fills, the import re-reads the target as a run
     does (spec 0009's freshness):
     - a target paused or removed meanwhile stops, and its run ends `failed`;
     - an entry or platform removed meanwhile, or an unpinned key, applies to the next cell;
     - a tenant now being deleted stops the import.

     **No downgrade per cell.** When the site's cell already holds a different body, the bundle's
     file is taken only if its `fetched_at` is later than the site's cell's; otherwise the cell is
     `stale` and keeps what it holds. A second exporter, or a gateway whose mirror lagged, cannot
     replace a newer build with an older authentic one.

     It then runs the cell's checks (the footer against the cell, the entry's version filter) and the
     locked part of the intake: block, shadow, slot, Build, release. The cell's outcome is recorded
     as a run records it, with the manifest's `etag` and `fetched_at`.
   - **Resources.** One file at a time, in the spool: about `blob.max_body` of disk at worst. A CLI
     import is a separate process from the server and does not share its ingest slots: the docs
     size the spool for both.
5. **Cells without a file.**
   - `missing` records `missing`.
   - `unchanged` keeps the target's record only when it holds the cell's `body_hash`; otherwise the
     cell is recorded `missing` with "not in this bundle and not held".
   - `rejected` records `rejected` with the export's reason prefixed `bundle:`, control characters
     stripped.
   - A `fetched` cell whose file never arrived, because the tar ended early, is `failed`.
   - Cells the bundle does not mention keep their records. A bundle may be partial, and the import
     never prunes cells: a run removes cells outside the matrix, and the next plan leaves them out.
6. **Current releases.** A run makes every release it inserts current for its DuckDB version: the
   release gets the next sequence number, and serving picks the newest servable release per DuckDB
   version (spec 0008). The import does the same, deferred to the end so the tar's order never
   decides: after the stream, every release it inserted gets its sequence number per (name,
   platform), in ascending canonical DuckDB-version order, under the channel lock.
   - A cell whose slot already held its body (`unchanged`, `yanked`) changes nothing. As in a run,
     an administrator's choice of current survives every import that brings nothing new.
   - Releases from an import that stopped early are made current in the same way, for the cells it
     completed. **On a resume**, a release still not current whose provenance names this manifest's
     `id` is made current as if inserted now, so a crash between insert and step 6 strands nothing.
7. **End.** Each target's run is recorded as a run's is: start, end, counts per outcome and the
   first 20 problems, with `bundle` naming the manifest's `id`. The leases are released. The command
   prints a summary per upstream and exits non-zero if any target failed, or if the tar was
   truncated or malformed. What completed stays: each release is atomic.

**Stalls.**
- An input that delivers nothing for 5 minutes ends the import, which then completes as at step 7.
  So does an input slower than spec 0009's rate floor (`upstreams.min_rate`) once 30 seconds have
  passed. Standard input takes no read deadline, so a watchdog completes step 7 while the read stays
  blocked, then exits the process.
- An import's leases never outlive these limits. Spec 0019's deletion waits for upstream leases and
  is never blocked indefinitely.
- An import ends at the end-of-archive blocks of the tar and does not wait for end of input, which
  some `exec` transports never deliver.

**Recovering from a bad mark.** A bundle whose `created_at` was wrong but within the 10-minute
allowance, or an exporter's clock set ahead, can leave a target refusing honest bundles.
`kista admin bundle reset <tenant> <upstream>` clears the target's mark; it is audited
(`bundle.reset`) and is a server administrator's act.

**Dry run.** `-dry-run` runs every check, spool included. It releases nothing and changes no cell,
mark or current release. It records the targets' `last_run` (counts as `would_release`) and the
`upstream.run` event with `dry_run: true`, as a mirror's dry run does. A `C_STRUCT` file is counted
`would_release` once per cell it fills, where a real import counts the later cells `unchanged`.

**Repeating an import.** Importing the same bundle again releases nothing new. Cells that were
released are `unchanged`; refused cells are refused again with the same outcome.

**Actor.** The import runs as the `kista admin` operator, `os:<uid>:<user>`. Importing is a server
administrator's act: `kista admin` already holds the database and every key source.

**Provenance.** A release's provenance is spec 0009's object:
- `url`: the cell's URL at the source, computed by the site;
- `etag` and `fetched_at`: from the manifest's cell;
- `key`: the source key that verified;
- `bundle`: the manifest's `id`;
- `imported_at`: the time of the import;
- `credential`, for an authenticated cell only (Visibility).

### Import records

Each import, dry runs included, is a row in **`bundle_imports`**:

| Column | |
| --- | --- |
| `id` | |
| `tenant_id` | |
| `manifest_id` | |
| `manifest_created_at` | |
| `manifest_sha256` | the SHA-256 of the manifest's bytes: a resume needs the same |
| `exporter` | the manifest's, as data |
| `bundle_key` | the fingerprint |
| `input_sha256` | the SHA-256 of every byte through the end-of-archive blocks, whatever the input |
| `dry_run` | |
| `state` | `running`, `done` or `failed`; a `running` row whose leases have expired is shown as, and at the next import of the tenant rewritten to, `failed`, "interrupted" |
| `counts` | JSON, per target and outcome |
| `started_at`, `finished_at`, `actor` | |

- **CLI.** `kista admin bundle list <tenant>` and `kista admin bundle show <tenant> <id>`.
- **Console.** The console shows a release's `bundle` and `imported_at` in its provenance, and a
  bundle-mode upstream's last import. Phase 1a adds no console page for imports.
- **Freshness mark.** Per target: `upstreams.bundle_id`, `bundle_created_at` and
  `bundle_manifest_sha256`. The API's upstream record exposes them for "last import".
- **Tenant deletion.** Spec 0019's reaper gains `bundle_imports` (by tenant).

### Visibility

A mirror's releases from a credentialed source are private unless the credential sets
`allow_public` (spec 0009 phase 3): a paid subscription's extensions are not the tenant's to
publish. Bundle mode keeps the rule. The signed manifest's per-cell `authenticated` flag says
whether the exporting side needed a credential for the cell, or exported a private release of its
own, and the site's configuration plays the credential's part:

```yaml
upstreams:
  bundle_sources:                          # optional; authenticated sources not listed are private
    - prefixes: ["https://kista-public.example.org/"]
      tenants: [acme]                      # or "*"
      allow_public: true
```

- **Marking.** A release from an authenticated cell records in its provenance the pseudo-credential
  `credential: "bundle:<normalised prefix>"`. Credential names may not contain `:`, so the two never
  collide.
- **Checks reused.** Spec 0009's three checks already read the provenance's `credential`:
  - making a release public;
  - promoting it as public;
  - a promotion carrying the credential over.

  For a `bundle:` value they consult `bundle_sources`: an entry whose `tenants` include the
  release's tenant, whose prefixes cover the source (as credentials' prefixes do), and which sets
  `allow_public`. The check therefore needs the tenant: `release.MayPublish` becomes
  `func(tenant, credential string) bool` at its call sites (`internal/release/release.go`,
  `publish.go`). The server reads `bundle_sources` at start, so a change applies at the next start;
  the CLI reads it when it runs. Promotions keep the value, so a promoted copy stays guarded.
- **At intake.** A release from an authenticated cell is private, whatever the upstream's
  `visibility` says, unless such an entry allows it. A bundle-mode upstream may itself be public:
  that is its visibility for unauthenticated sources, as a mirror's is.
- **Passthrough.** A passthrough channel is public by nature and takes `duckdb-core` only, which is
  never authenticated: the rule does not reach it.
- **Not DRM.** The flag is the exporter's word, and the bundle key's holder could leave it out.
  This is the honest-user control spec 0001 sets for licensing. It prevents the accident, not the
  deliberate act; an administrator could add the bodies by hand just as well.

### Statements in a bundle (phase 1b)

A feed's `mode` (new; default `online`) gains **`bundle`**:

- **Like a bundle-mode upstream, it never runs.**
  - No contact with its URL: the URL is the issuer's identity.
  - No credential, and no `sync`.
  - Its status stays `new` until an import and then reflects the last import (`ok` or `failing`).
- **The plan** gains `feeds`, each with:
  - the feed's URL;
  - its pinned attestation keys;
  - the kinds it accepts;
  - per body the feed would offer (spec 0011's selection, as it is now), the last `seq` received;
  - which of the plan's sources' new bodies it may also be offered. By default these are the
    sources of upstreams in the feed's offer selection, so a body from source A is never disclosed
    to feed B outside B's offer.
- **The export** takes what the exporting kista's own feed of the same URL holds (spec 0011 keeps
  statements as their issuer signed them, with their documents); it does not contact the feed.
  - It writes the feed's attestation keys file as the feed client last fetched it.
  - For each body the plan offers, and each body it exports for an allowed source, it writes the
    statements past the plan's cursor as `feeds/<n>/batch-<k>.json` (each at most 1 MiB), after the
    files.
  - It writes each statement's document as `feeds/<n>/documents/<issuer id>` (each within spec
    0011's limit), after its batch.
  - It checks size, content hash (spec 0011's `ContentHash`) and signature against the plan's
    pinned keys, as the import will.

  The covered range is what the exporting feed has read through for each body, clamped below any
  statement the gateway's feed removed (a tombstone, spec 0011), so a range declared complete never
  hides a statement.
  The manifest's `feeds` section names every entry with its hash and size. Per body, it gives the
  `after_seq` sent and the `seq` the source reported as read through: the range the batches cover
  completely.
- **The import** checks what spec 0011's feed client checks:
  - the issuer is the feed's URL;
  - the signature verifies with a pinned key;
  - the canonical form;
  - the kinds;
  - the quotas;
  - `supersedes` and withdrawals;
  - the audience rule;
  - the body is in the feed's offer selection now, and the tenant still holds it.

  A cursor advances only within the range the signed manifest says the batches cover. A carrier
  cannot drop a withdrawal: the manifest pins every batch's hash. Statements are imported after the
  files, so a body the bundle brings is held when its statements arrive.
- **Disclosure.** As online, the site discloses the hashes it offers to each source. They pass
  through the plan, and the operator who carries the plan sees them.

### Installing kista offline

The image, the chart and the binaries are signed (spec 0013). Phase 1a adds an "Air-gapped sites"
page to the docs, under Deployment. Its commands run in CI so the page cannot drift. Signatures and
attestations exist for released versions only, so the test is pinned to the last release: an online
stage pulls it (`cosign save`, the chart, `gh attestation trusted-root`), and an offline stage, in a
container without a network (`--network none`) with a local registry, loads and verifies them:

- **Image and chart.** Both are OCI artifacts, signed by digest. They are copied with their
  signatures and attestations into the site's registry (`cosign save`/`cosign load`, or
  `oras copy` with referrers), keeping the digests. Inside the site, `cosign verify` checks them
  offline against a Sigstore trusted root saved beside them. Which cosign v3 flags carry the new
  bundle format and referrers is settled by the CI test, and the page pins the cosign version.
- **Build provenance.** GitHub's attestation is verified offline with
  `gh attestation verify --bundle … --custom-trusted-root`.
- **Binaries.** These are verified as on the Deployment page, with their `.sigstore.json` files.
- **Values.** The chart's `image.repository` points at the site's registry.
- **The cycle.**
  - The first bundle key and its pin.
  - The bundle-mode upstreams for the case in Problem, with a passthrough channel for autoload.
  - The channel's DuckDB versions, with C API maximums for C API extensions (spec 0006).
  - The gateway kista on the connected side: its mirror-mode upstreams for the same sources, the
    bundle key beside it, and the export.
  - Plan, export, carry, import, with `-dry-run` first.
  - Sizing (below).
  - A slot holds one body forever, so a wrong body is a permanent `conflict` for that version.

**Sizing.** The import inflates one file at a time into the spool: about `blob.max_body` beside
the server's own ingests.
- In Kubernetes, `kubectl exec` runs it in the serving pod, whose spool emptyDir is sized for the
  server's ingests (spec 0013) and is also the pod's `TMPDIR`; the root filesystem is read-only.
  The chart gains `spool.importHeadroom` (default `0`), which adds to the emptyDir's limit and its
  ephemeral-storage request, also over an explicit `spool.sizeLimit`.
- With PostgreSQL, the import can run in a separate pod instead, as a Job with the server's
  configuration and its own emptyDir. A SQLite deployment has a single writer volume, so it imports
  in the serving pod.

### API and CLI

- **Upstreams.**
  - `mode: "bundle"` on `POST /api/v1/tenants/{t}/upstreams`, and `-mode bundle` on
    `kista admin upstream add`.
  - `sync` on a bundle-mode upstream answers `409`.
  - A credential on a bundle-mode upstream is refused with `400`, both at creation and through
    `POST …/credential`.
- **CLI.**
  - Anywhere, offline: `kista bundle keygen|sign|inspect`, and `kista bundle keys <prefix>` online.
  - The connected side: `kista admin bundle export`.
  - The site: `kista admin bundle plan|import|list|show|reset`.
- **`kista bundle inspect <bundle> [-plan <plan>]`** works offline:
  - it checks the manifest's signature with the bundle's key;
  - it checks every file's hash and size;
  - it checks every file's source signature with the bundle's `.well-known` keys.

  With `-plan` it adds checks against the plan's pins: is the bundle key pinned, and is each listed
  source key pinned, listed only, or missing. It prints totals, inflated sizes and the import's disk
  need. Without `-plan` it reports the bundle as consistent, not as trusted.
- **Phase 2.** `POST /api/v1/tenants/{t}/bundles` lets a server administrator upload a bundle as a
  streamed body, up to `bundles.max_size` (default 20 GiB).
  - The body goes to the domain's `imports/` prefix, so the collector leaves it alone.
  - The route answers `202` with the import's id. One replica then imports it under a lease, as the
    CLI does, and `GET …/bundles/{id}` gives the import record.
  - The console uploads from the tenant's Upstreams page.
  - Phase 2 settles resumable uploads and ingress body limits.

### Events

Spec 0010's catalogue gains:

- **`bundle.import`**, with:
  - `import` (the record's id), `manifest`, `created_at`, `exporter`, `bundle_key`, `input_sha256`,
    `dry_run`, `state`;
  - `targets`: the upstream names, with counts only up to `MaxData`, since the full counts are on
    the record.

  Its subject is the tenant. The actor is the event's actor.
- **`bundle.export`**, on the exporting kista: the plan's SHA-256, `manifest`, `bundle_key`, counts
  per outcome. Its subject is the exporting tenant.
- **`bundle.reset`**: the target and the mark it cleared.
- The new kinds and fields join the audit's field allowlist (`internal/audit`) and its coverage
  test; `targets` comes last, since an event's data keeps its first fields when it is truncated.
- **`upstream.run`** gains `bundle` (the manifest's id).
- **`upstream.release`** gains `bundle`, so the audit alone answers "which bundle brought this
  release, and who imported it".
- **`upstream.rejected`** is made when a cell changes into a refusal, as in a run.

`website/docs/observability.md` lists the new kind and fields.

### Data model

**Migration 0020**, on all three dialects, with `-- +min_reader 20`:

- **Why `min_reader`.** An older replica's `Sync` refuses only pull-through, and its credential
  route accepts any upstream. So it would fetch a bundle-mode upstream online, or give it a
  credential.
- **`upstreams.mode`'s CHECK** gains `bundle`.
  - PostgreSQL drops `upstreams_mode_check` and adds the new constraint.
  - SQL Server looks up the constraint's system name, as `0006_publishers.sql` does.
  - SQLite rebuilds `upstreams`. Five tables reference it with `ON DELETE CASCADE`, and the store
    runs migrations in a transaction with foreign keys on, so dropping the table would delete their
    rows. The migration copies `upstream_keys`, `upstream_platforms`, `upstream_entries`,
    `upstream_versions` and `upstream_cells` aside, rebuilds `upstreams`, and restores them. A test
    seeds every child table and checks it survives.
- **`upstreams.bundle_id`, `bundle_created_at` and `bundle_manifest_sha256`**, null.
- **`bundle_imports`**, above.

Phase 1b adds `feeds.mode` (`online` | `bundle`) and the feeds' import marks, with their own
`min_reader`. Phase 2 adds the upload state to `bundle_imports`.

### Configuration

| Setting | Phase |
| --- | --- |
| `upstreams.bundle_keys` | 1a |
| `upstreams.bundle_sources` | 1a |
| `bundles.max_size` | 2 |
| The chart's `spool.importHeadroom` | 1a |

`spool.importHeadroom` adds to the spool emptyDir's limit and request, also when `spool.sizeLimit`
is set explicitly.

The config check refuses:
- malformed fingerprints, entries without `tenants`, and more than 10 entries;
- `bundle_sources` entries without `prefixes` or `tenants`;
- prefixes that are not https.

### Package layout

```text
internal/bundle          the plan and the manifest (types, validation, canonical order),
                         signing and verifying, the tar reader and writer
internal/bundle/export   the export: sources from upstreams and own channels, files with their
                         original signatures, the checks, the summary
internal/upstream        + bundle mode (no sync, no credential, no egress port check, passthrough);
                         the import: targets, leases, freshness, per-cell intake, current releases
internal/release         + Ingest split: the file's checks once, the cell's checks and insert per
                         cell; MayPublish for "bundle:" pseudo-credentials
internal/blob            + the imports/ prefix, read-only to kista, outside the collector
internal/attachment      + bundle-mode feeds (1b)
cmd/kista                + bundle keygen|keys|sign|inspect, admin bundle export|plan|import|list|show|reset
web/console              + bundle mode on the upstream forms; provenance fields
```

### Docs

These pages are updated in the same PR:
- `website/docs/upstreams.md`: bundle mode;
- `configuration.md`: the settings above;
- `administration/commands.md`;
- `api.md`: nothing beyond the record's fields (the mode's rules are on the Upstreams page);
- `observability.md`: the events;
- `security.md`: the trust model above;
- `deployment/kubernetes.md`: the import's disk, `importHeadroom`, and import as a Job;
- the new `deployment/air-gapped.md`, with its sidebar entry.

### Changes to earlier specs

- **0009**:
  - upstreams gain mode `bundle`;
  - the passthrough guard already refuses only pull-through, so it admits bundle mode; the mode
    check at creation accepts `bundle`;
  - `checkPrefix` takes the mode and skips the egress port check for it, and creation skips the
    online key fetch (`repoKeys`);
  - credentials are refused on it, and credential names may not contain `:`;
  - provenance gains `bundle` and `imported_at`;
  - `bundle:` pseudo-credentials.
- **0005 / 0016**: the `imports/` prefix, outside the collector.
- **0010**: `bundle.import`, `bundle.export`, `bundle.reset`, and `bundle` on `upstream.run` and
  `upstream.release`.
- **0008**: `release.MayPublish` takes the tenant.
- **0019**: the reaper deletes `bundle_imports`.
- **0011 (1b)**: feeds gain mode `bundle`.
- **0013**: the chart's `spool.importHeadroom`.
- **0001**: the follow-up for bundles points here.

## Security

- **Trust comes from the site.** The site pins the bundle keys and the source keys; DuckDB's keys
  are compiled in. The manifest, the `.well-known` file, the tar and its order are input to be
  checked.
- **Authentic and fresh.**
  - The source's signature proves a file authentic.
  - The signed manifest proves it is what the exporting side held for that cell when the bundle
    was exported.
  - Without the manifest's signature, a carrier could put any build the source ever signed into a
    cell that never held its version. Spec 0009 releases such a version and makes it current: a
    downgrade to a known-vulnerable build.
  - With it, the carrier can only withhold the bundle.
  - **Replay.** Per target, a bundle ordered before the last one imported, a reused `id` with
    another manifest, or a bundle made from a plan older than the target is refused. A future
    `created_at` is refused beyond 10 minutes, and `bundle reset` recovers a target a bad mark
    blocks.
  - **No downgrade per cell.** A cell the site holds with another body is replaced only by a file
    confirmed at the source later than the site's own record: a second exporter, or a lagging
    gateway, cannot bring an older authentic build back.
  - **Additional defence.** A site that wants it pins exact versions in the entries' `versions`.
- **What the bundle key's holder can do.** They choose what the site receives, within the site's
  allowlists, filters and the sources' signatures: withhold, mark cells `rejected` or `missing`,
  leave out an `authenticated` flag, or offer an authentic build older than what a cell would
  otherwise get, though never older than what the site holds. The holder is the exporting side: an
  organisation's gateway, or Enterest for its subscribers. Signing offline (`-unsigned`, then
  `kista bundle sign`) keeps the key off the internet-facing gateway; the docs recommend it. Keys
  are pinned per tenant, so one tenant's key holder cannot sign for another.
- **The gateway.** Mirrored files carry their source's own signature, never a key of the
  gateway's: a compromised gateway without the bundle key can only withhold. Own-channel sources
  are the exception: there the site trusts the gateway's channel key, because it planned that
  prefix as a source.
- **Order.** Which release becomes current is decided by canonical cell order after the stream,
  never by tar order.
- **Fail closed.**
  - A bad head refuses the bundle: the manifest, its signature, the key's pin, the tenant.
  - A repository target without a pinned key fills nothing.
  - A file whose hash or size differs from the manifest fills nothing.
  - A truncated tar keeps what completed and fails the rest.
- **Resources.**
  - The manifest is at most 64 MiB, 100,000 cells and 50,000 files.
  - Every inflated file is bounded by the manifest's size, itself at most `blob.max_body` + 256
    bytes.
  - Skipped bytes are capped at 64 MiB.
  - A file is verified once, however many cells it fills: a 1 GiB file mapped to many cells costs
    one spool, not one per cell. The cells it can fill are bounded by its footer: its name and
    platform, and the targets' DuckDB versions.
  - Idle and slow input end the import, and leases never outlive it.
- **Tokens and the network.**
  - The export makes no request of its own but the repository sources' `.well-known` files, through
    the matching upstream's egress and credential, as a mirror does (spec 0009): the plan cannot
    point it anywhere its upstreams do not already go.
  - No credential reaches the bundle, a log or an error.
  - The plan and the bundle hold no secret. Bundle-mode upstreams and feeds hold no credential.
- **Who exports.** A server administrator of the exporting kista. A plan discloses the site's
  configuration to the exporting side: to Enterest, for its subscribers, that is part of the
  service, and the docs say so.
- **Paid extensions.** Files from an authenticated source are private unless the site's
  `bundle_sources` allow otherwise for that tenant. The marker survives promotion. It is an
  honest-user control, like licensing.
- **Who imports.** Only a server administrator, through `kista admin` (1a) or the upload route (2).
  - A tenant administrator configures the bundle-mode upstreams, their allowlists and pinned keys,
    which bound what any import may release into the tenant.
  - The manifest names its tenant, and importing it into another tenant needs `-any-tenant`.
- **Isolation.** An import fills one tenant's upstreams. Bodies are shared within a storage domain
  as spec 0005 shares them.
- **Untrusted text.** The export's reasons are bounded, control characters are stripped, and they are
  shown as data.
- **Disclosure.** A plan names the tenant's sources, allowlists, platforms, DuckDB versions and the
  hashes it holds; a bundle adds what was fetched. Both are as sensitive as the site's configuration.
  The docs say so.

## Testing

- **Plan.**
  - Sources merged by kind and prefix.
  - The union of keys and filters, an empty filter included.
  - `etag` and `body_hash` given only when every sharing upstream agrees.
  - Dev versions left out for DuckDB's kinds.
  - The cell cap.
  - `plan.created_at` against a target created later.
- **Format** (`internal/bundle`): sign and verify with the `\0`-separated context and 64 raw
  bytes; the fingerprint from the parsed key, not the PEM bytes; a non-Ed25519 `.pub`; strict JSON
  (unknown fields, duplicate keys); every name grammar; `../`, absolute names, a PAX global header,
  a GNU long name, sparse records, links; `sources/` after `files/`; a `.well-known` over 64 KiB;
  trailing gzip data and a second gzip member; an inflated size over `inflated_size`; control
  characters in `detail` and `exporter`; canonical order of sources and files.
- **`kista bundle inspect` and `sign`**: a consistent bundle, each kind of inconsistency the import
  refuses, `-plan` marking keys pinned, listed or missing; `sign` on an unsigned bundle, on a signed
  one (refused), with a key the plan does not list (refused).
- **Export**, from a store and blob domain the tests seed (mirrored cells with original
  signatures, an own channel, test keys and a fake repository for `.well-known`):
  - files exported as body ‖ original signature, verifiable with the plan's keys;
  - an own channel's files signed by a key in both the pins and its `.well-known`, after a key
    rotation too; an own passthrough channel refused as a source;
  - `unchanged` by `body_hash` only (an equal ETag alone exports the file);
  - every row of the outcome table: a yank, a block, a hidden deprecation and a `release current`
    made after the run, all decided by the release, not the stale cell record; another source's or a
    published release; `conflict`, `shadowed`; pull-through cells never asked; cells outside the
    exporting matrix;
  - private releases, of an own channel and of a credentialed mirror, only with `-private`, and
    only of `<tenant>`; `authenticated` per cell;
  - a release without an original signature `rejected`;
  - `fetched_at` from the cell record (kept fresh by `unchanged` runs) and `released_at` for own
    channels;
  - the files written from the stored gzip with a gzip tail, read twice, no temporary copy;
    `-unsigned` then `kista bundle sign` gives the same bundle as `-key`;
  - two upstreams of one source, the first holding the cell wins;
  - a source mirrored nowhere fails as a whole; a `.well-known` listing no planned key fails it;
  - `-sync` runs the mirror-mode upstreams first;
  - an unpinned bundle key stops before reading; a hand-written plan with a bad grammar;
  - an interrupted export leaving no output;
  - the `bundle.export` event; no credential in the bundle or the output.
- **Import**, from bundles the tests write:
  - **Valid imports and repeats.** A valid import. A repeat releases nothing. An older bundle is
    `stale`; so is an equal `created_at` with a lower `id`, the same `id` with another manifest, and
    a plan older than the target. A `created_at` more than 10 minutes ahead is refused. The same id
    and manifest resumes. `bundle reset` reopens a target, and is audited.
  - **Keys per tenant.** A key pinned for tenant A is refused for tenant B.
  - **No downgrade.** A file for a cell the site holds with another body, with an earlier
    `fetched_at`, is `stale`; with a later one it is released. Two exporters' bundles in either
    order.
  - **`unchanged`** against a record holding another body becomes `missing`.
  - **Crash and resume.** An import killed after inserting releases and before step 6; the resume
    makes them current. A `running` record with expired leases is shown `failed`, "interrupted".
  - **Current releases.** Exact builds for two DuckDB versions in one import: both are served.
  - **Bad heads.** An unsigned bundle, an unpinned key, a bad signature, a manifest edited after
    signing, another tenant (and `-any-tenant`).
  - **Bad files.** A file whose hash or size differs, a file signed by an unpinned source key, a
    file named for another cell's platform or DuckDB version, a binary of another name, a tampered
    `.well-known`.
  - **Tar structure.** A manifest not first or over its cap, duplicate sources and cells, unknown
    entries, links, sparse entries, duplicate files, the skipped-bytes cap, a truncated tar, the
    end-of-archive with no end of input.
  - **Cell mapping.** Unplanned cells, a file shared by two targets, and a `C_STRUCT` file serving
    several versions, all verified once.
  - **Current release.** Current decided by canonical order with the tar in another order. An
    administrator's current kept.
  - **Intake outcomes.** Blocked, shadowed and conflicting slots.
  - **Visibility.** Authenticated cells and `bundle_sources` across tenants, through
    `MayPublish(tenant, credential)`; promotion keeps the marker; a credential name with `:`
    refused; the refusals.
  - **Changes during an import.** A target paused or removed meanwhile; a tenant deleted meanwhile;
    a matrix over its cap.
  - **Dry run** records only `last_run` and its event.
  - **Concurrency.** Two imports at once: one is `busy`.
  - **Input.** Standard input (the watchdog ends a blocked read), a `blob:` input under the store's
    prefix with a transient error re-opened at its offset, `imports/` refused for writing, and idle
    and slow input. `input_sha256` equal for the same bundle from a file, stdin and `blob:`.
  - **Spool.** The file inflated straight into the spool; a hash mismatch drops it; one file held
    across all its cells with a Build removed meanwhile (`ErrBuildGone`).
  - **Leases.** The lease is taken before the writable-tenant check; a tenant deletion waits for it.
  - **Passthrough.** A passthrough channel fed by a core bundle.
- **Upstreams.**
  - Bundle mode is never scheduled.
  - `sync` answers `409`.
  - A credential is refused at creation and on the route.
  - No egress port check.
- **Store.**
  - Migration 0020 on all three dialects.
  - SQLite's rebuild keeps every child table's rows.
  - `bundle_imports`, and the reaper's completeness test with it.
- **e2e** (pinned DuckDB), in-process as the harness runs kista today (`e2e/upstream_test.go`):
  1. A source kista serves a signed channel.
  2. A gateway kista mirrors it; the export package makes a bundle from the site's plan.
  3. A site kista whose egress refuses every address imports the bundle into a bundle-mode upstream.
  4. DuckDB runs `INSTALL … FROM` and `LOAD` against the site.
  5. A new version at the source, a second bundle; the new version is current.

  The DuckDB pin is a development build, for which DuckDB publishes no core files, and the pinned
  DuckDB refuses core files signed with test keys. Core sources and passthrough are therefore
  covered by unit tests, with the keys from `upstream.Service.DuckDB` (the existing test override).
- **Docs.** The offline install's commands run in CI as described there, pinned to the last
  release, the offline stage without a network.
- **1b.**
  - Statements in a bundle are verified, stored and cursored as spec 0011's client does.
  - Statements arrive for a body the same bundle brings.
  - These are refused:
    - a statement by an unpinned key;
    - one from another issuer;
    - one for a body outside the offer;
    - a batch whose hash does not match the manifest.
  - A cursor never passes the covered range.
- **2.** An upload, the size limit, server administrators only, and one import per upload across
  replicas.

## Alternatives considered

- **No bundle signature, per-file signatures only.** This was the first draft. It is wrong: source
  signatures prove authenticity, not freshness. A carrier could release any historical build into a
  version the channel never held, and that version would become current.
- **Signing with a source key, or with an exporting kista's channel key.** Either lets only the
  source make bundles, and DuckDB's own repositories never will. A bundle key belongs to whoever
  builds for the site, which is the party the site trusts to visit the internet on its behalf.
- **A client-side builder (`kista bundle build`)**, fetching the plan's cells itself with what a
  DuckDB client has, no kista needed on the connected side. This was the first design. It repeats
  spec 0009's fetching, key handling, credentials, egress rules and checks in a second
  implementation, and Enterest would hand subscribers tokens to run it rather than export for them.
  The export reuses the mirror, ships the same bytes (`origin_signature` keeps each source's
  signature), and needs a gateway kista, which an organisation already running kista can afford. A
  builder can still be added later for sites with no connected kista.
- **An export re-signed by the gateway's channel keys.** The site would then trust the gateway's
  keys besides the sources'. Exporting the original signatures keeps the gateway out of the trust
  chain.
- **Importing as `release add` in a loop.** This loses verification, passthrough, reservation,
  shadows and the cells: the problem itself.
- **A `bundle` kind instead of a mode.** The kind says what the source is, and decides its keys and
  reserved names. Whether kista reaches the source is a separate question.
- **Decompressed files, or a compressed tar.** Decompressed files triple the media. Compressing the
  whole tar costs a full decompression at import and gains nothing over per-file gzip.
- **A site backup as the bundle.** Two sites with their own keys, grants and audit are not
  replicas. A backup carries what the connected site decided, not what the air-gapped site decides.
- **OCI artifacts as the carrier.** Many air-gapped sites have a registry, but a tar needs nothing on
  either side. The `blob:` input already covers transfers that need to resume. An OCI layout of the
  same manifest is a follow-up.

## Follow-ups

- **Events and statistics leaving the site** (spec 0010): a bundle in the other direction, signed by
  the site, for an organisation's central audit.
- **Bundle keys in key sources** (spec 0004's vaults and KMSs) instead of a PEM file.
- **Yanking by bundle:** `kista admin release yank -bundle <id>`.
- **OCI layouts as a second carrier.**
- **Relaying feeds onward** (spec 0011): a connected kista relaying a source's statements into
  bundles for several sites.
- **Licences** (spec 0012): long-lived licence tokens for air-gapped holders, carried in a bundle.
