# Spec 0014: Bundles for air-gapped sites

- **Status**: accepted; phase 1a implemented (parts 1 bundle mode, 2 the format, keys and plan, 3 the import, 4 the export, 5 the air-gapped page and its CI job); 1b implemented (parts 1 bundle-mode and export feeds, 2 the plan and the format, 3 the export, 4 the import); phase 2 to come
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
  3. the import: the records, the events, `bundle_sources` and `MayPublish`, `bundle reset`, the
     blob store's read-only `imports/` prefix; tested with bundles the tests write;
  4. the export, then the e2e test;
  5. the air-gapped docs page and its CI job.
- **1b. Statements**: bundle-mode feeds. The export carries the statements and documents its feeds
  hold for the bodies the site offers, and the import verifies them as spec 0011's feed client does.
  In four pull requests, in this order:
  1. migration 0021: feed mode `bundle` and its guards (credential, egress, sync, the runner), the
     feeds' last-bundle marks; `export` and what such a feed keeps (the keys file, permanent
     refusals, statements about documents it does not hold); the reaper and the collector; the API,
     CLI and console fields;
  2. the plan's and the manifest's `feeds`, the feed entries in the tar reader and writer,
     `kista bundle inspect` over them, and `kista admin bundle plan` with feeds;
  3. spec 0011's client checks factored out of the run (a statement checked and applied with its
     content from a fetch or from a bundle), then the export of feeds; tested with gateways the
     tests seed;
  4. the import of feeds, its records and events, `bundle reset -feed`, round trips through the
     export, the docs, and the e2e test extended with a feed.

  Each PR updates the docs of the behaviour it changes: PR 1 `attachments.md`, `api.md`,
  `administration/commands.md`, `observability.md` and the console; PR 2 and 3 `upstreams.md` and
  `commands.md`; PR 4 those, `security.md` and `deployment/air-gapped.md`.
- **2. Upload over the API**: chunked, resumable uploads imported by the server, and a console page,
  for administrators who cannot run `kista admin` beside the server and cannot write to its object
  store. In two pull requests: (1) migration 0022, the `uploads/` prefix, the routes, the runner,
  the collector, the docs; (2) the console's upload.

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
  a bundle) refuses an older build for a cell the site already holds newer by the manifest's
  `fetched_at`; that time is the holder's word, so the rule stops a lagging or second exporter, not
  the holder, who could sign an old build with a new time. A site that subscribes
  to Enterest trusts Enterest's key this far; an organisation's own gateway, its own. Keys in KMSs and
  vaults (spec 0004's key sources) are a follow-up.

### The plan

```sh
kista admin bundle plan <tenant> [-upstream <name>,…] [-feed <name>,…] [-o plan.json]
```

The plan lists the tenant's active bundle-mode upstreams (and, from phase 1b, feeds: below) (all of them, or the named ones; a paused
one is left out, and refused when named, since an import fills active upstreams only) as JSON. It
holds no secret:

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
kista bundle sign -key bundle-key.pem -plan plan.json -in acme-2026-10.unsigned -o acme-2026-10.kbundle   # offline
```

`<tenant>` is the exporting kista's tenant whose upstreams and channels serve the plan; it need not
share the site's tenant name. With `-key`, the export signs only if the key's fingerprint is among
the plan's `bundle_keys`, and stops before reading anything otherwise. `kista bundle sign` makes
the same check against the plan it is given, validates that plan, and refuses a bundle that
answers another plan, holds a source or cell the plan did not ask for, is incomplete, or whose
files do not inflate to the manifest's sizes and hashes: a signature vouches for the contents. The plan is validated with spec 0009's grammars first: a plan is input, whoever
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
   - The upstreams come first: an own channel is the source only when no active mirror or
     pull-through upstream matches, and only with an https `serve.public_url`.
   - A source with neither fails as a whole: "not mirrored here".
2. **Keys.** The export checks with the **plan's** keys, which are the site's pins:
   - core and community: DuckDB's keys as the upstream service holds them: compiled in, or
     `upstream.Service.DuckDB` in tests;
   - a repository source: the export fetches `<prefix>/.well-known/duckdb-extension-repo.json` (at
     most 64 KiB) through egress, without a credential (the file is public by design, spec 0001),
     as a mirror's key check does, and keeps it as it came; for an own channel it writes the channel's own file. A source
     whose file lists none of the plan's keys fails as a whole.
3. **The release decides, not the cell record.** A cell record is written only by runs and
   pull-through misses, so a yank, a block or a `release current` made since would not show in it.
   For each plan cell the export looks up the release that serves the cell on the exporting side:
   the channel's servable release for (name, platform, the cell's DuckDB version), the one DuckDB
   would install from that channel now. Two things narrow it, so that the export never skips a file
   the site could take for one it would refuse:
   - when the plan cell lists `versions`, the newest servable release among them;
   - unless `-private` covers the source, the view of a caller without install: public releases
     only.

   Then:

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
   | releases, none of the plan cell's `versions` | `missing`, "no release of the plan's versions on the exporting kista" |
   | a release whose stored body cannot be read (missing, or marked corrupt) | `rejected`, "the body is not available on the exporting kista" |

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
tenant's private builds on a plan. A prefix that names no repository source of the plan refuses the
export; DuckDB's kinds take none, so their private releases never leave.

**`-sync`** first runs the matching mirror-mode upstreams (spec 0009's `sync`) and waits for them;
pull-through upstreams export what they hold, and their never-asked cells are reported as such. A
run that fails, or an upstream paused meanwhile, stops the export before anything is written.

**Per-cell fields.** Each exported cell carries `fetched_at`: the time the exporting side last
confirmed the cell against the source (the cell record's, so a mirror that keeps answering
`unchanged` keeps it fresh), or for an own channel the release's `released_at`. The cell record
speaks for the release only while it holds the same body: otherwise (the source moved on and the
mirror refused the new build) it is the release's own fetch time from its provenance, so a newer
confirmation never dates an older body. It carries `etag`
when the cell record has one. `authenticated` is per cell: true when the release came through a
credentialed upstream or is private on the exporting side (a mirror's private release, or an own
channel's): a release `-private` let leave stays private at the site. The site computes each cell's URL
itself.

**Writing.** Files go straight from the blob domain into the tar: the stored gzip stream of the
body, with the signature appended as a gzip tail (`extfile.GzipTail`), so no temporary copy is
made. The manifest goes first and needs every file's size and SHA-256, so the export reads each
body twice: once to hash, then to write. The tar is written to a temporary name beside `-o`, and
linked into place when complete (never over a file that appeared meanwhile), owner-only (`0600`):
the output is the only disk it needs. `-unsigned` writes the manifest
without the signature and key entries; `kista bundle sign` copies the bundle once, inserting them
after the manifest, and refuses one already signed.

**The `exporter` field** names the exporting kista's version and `serve.public_url`, each at most
256 bytes of printable text, for the record only: the site's trust is the bundle key.

**Audit.** The exporting kista records a **`bundle.export`** event: the plan's SHA-256, the
manifest's `id` and SHA-256, the bundle key's fingerprint (none when unsigned), counts per outcome
(with `failed_sources`), the actor; its fields are `plan`, `manifest`, `manifest_sha256`,
`bundle_key` and `counts`. `kista bundle sign` is a client command and records nothing; the import records the key.

The summary lists, per source: cells per outcome, bytes, the listed fingerprints, the first 20
problems, and the bundle's SHA-256 as the import will compute it (below). The export exits non-zero
when a source failed as a whole; rejected and missing cells alone do not fail it. A failed source is
left out of the manifest, and the bundle of the other sources is still written: the site's targets of
that source find no source in it and are left alone.

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
feeds/<n>/attestation-keys.json        1b: a feed's keys file, at most 64 KiB; after every file
feeds/<n>/batch-<k>.json               1b: statements, at most 1 MiB
feeds/<n>/documents/<sha256 hex>       1b: a document's content, by its content hash, after the
                                       batch that first names it
```

Feed entries (phase 1b) come after every file, feed by feed in the manifest's order; within a feed,
the keys file first, then each batch followed by the documents it first names. Their names match
`feeds/[1-9][0-9]{0,1}/attestation-keys.json`, `feeds/[1-9][0-9]{0,1}/batch-[1-9][0-9]{0,5}\.json`
and `feeds/[1-9][0-9]{0,1}/documents/[0-9a-f]{64}`: an issuer id has no grammar of its own (spec
0011 bounds only its length), so it never names an entry. Every one is named in the manifest with
its SHA-256 and size. A feed entry that is missing, out of its place, or whose size or bytes differ
from the manifest fails that feed's target; a `sources/` or `files/` entry after a `feeds/` one, and
a feed entry over 16 MiB, stop the import as a malformed tar does (Importing, phase 1b).

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
  - `created_at` at most 10 minutes past the importing host's clock, and no cell's `fetched_at`
    after `created_at`;
  - a gzip entry at most `inflated_size` + 1/64 of it + 64 KiB, so an entry padded with empty
    deflate blocks cannot hold an import's leases for nothing;
  - `authenticated` only on a `repository` source's cells (DuckDB's repositories need no
    credential);
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
   - **Selection.** The import selects the tenant's active bundle-mode upstreams, or the named ones
     (a named one that is paused, not in bundle mode or without a source in the bundle refuses the
     import). A target is matched to a source by kind (core, community) or by normalised prefix
     (repository). An upstream without a source in the bundle is not touched; a bundle that serves
     none of the tenant's upstreams (nor, from 1b, its feeds) is refused, leaving no record.
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
   - **Once per file.** The file is inflated, hashed and parsed once. Each target cell then goes
     through the same intake as a run's fetched file (`release.Ingest`): the signature with that
     target's keys, the footer, spec 0008's file checks. These are cheap beside the inflate and the
     commit, and the body is committed once per channel: a later cell of the same slot finds the
     release. The spool stays open until the file's last cell, so the re-commit spec 0009 makes when
     a Build disappears meanwhile (`ErrBuildGone`) still works.
   - **Per cell.** For each target cell the file fills, the import re-reads the target as a run
     does (spec 0009's freshness):
     - a target paused or removed meanwhile stops, and its run ends `failed`;
     - an entry or platform removed meanwhile, or an unpinned key, applies to the next cell;
     - a tenant now being deleted stops the import.

     **No downgrade per cell.** When the site's cell already holds a different body (a conditional
     outcome with a body hash), the bundle's file is taken only if its `fetched_at` is later than
     the site's cell's; otherwise the cell is counted `stale` and keeps its record and what it
     holds. A second exporter, or a gateway whose mirror lagged, cannot replace a newer build with
     an older authentic one. For the same reason a `failed` cell (a transient intake failure, or a
     file that never came) never overwrites a record of what the cell holds.

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
   - A `fetched` cell whose file never arrived, because the tar ended early or left it out, is
     `failed`.
   - Cells the bundle does not mention keep their records. A bundle may be partial, and the import
     never prunes cells: a run removes cells outside the matrix, and the next plan leaves them out.
6. **Current releases.** A run makes every release it inserts current for its DuckDB version: the
   release gets the next sequence number, and serving picks the newest servable release per DuckDB
   version (spec 0008). The import does the same, at insert. The tar's order cannot decide anything
   a run would not: the reader enforces the canonical file order (by source, name, platform and
   lowest DuckDB version), so releases are inserted per (name, platform) in ascending DuckDB-version
   order, as a run inserts them.
   - A cell whose slot already held its body (`unchanged`, `yanked`) changes nothing. As in a run,
     an administrator's choice of current survives every import that brings nothing new.
   - An import that stopped early leaves its inserted releases current, as an interrupted run
     does; a resume finds them `unchanged` and continues. Nothing waits for a later step, so a crash
     strands nothing.
7. **End.** Each target's run is recorded as a run's is: start, end, counts per outcome and the
   first 20 problems, with `bundle` naming the manifest's `id`. The leases are released. The command
   prints a summary per upstream and exits non-zero if any target failed, was `busy` or `stale`, or
   if the import stopped early (the tar truncated, malformed or stalled; a lost lease; the tenant's
   deletion). The record's `state` is `failed` in the same cases. What completed stays: each
   release is atomic. A stopped import records nothing more on its cells after a lost lease or a
   deletion.

**Stalls.**
- An input that delivers nothing for 5 minutes ends the import, which then completes as at step 7.
  So does an input slower than spec 0009's rate floor (`upstreams.min_rate`) once 30 seconds have
  passed. Standard input takes no read deadline, so a watchdog completes step 7 while the read stays
  blocked, then exits the process.
- An import's leases never outlive these limits. Spec 0019's deletion waits for upstream leases and
  is never blocked indefinitely.
- The time counted is the time the import waits for input; the time it spends committing a file is
  not. The import reads its input through a pump, and the watchdog ends the import by closing the
  pump's pipe, so the import completes step 7 although the pump's read may never return.
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
| `state` | `running`, `done` or `failed`; a `running` row whose lease (`kista/bundle-import/<id>`, held while it runs) has expired is shown as, and at the next import of the tenant rewritten to, `failed`, "interrupted" |
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

A feed (spec 0011) gains a `mode`: `online` (the default, today's feed) or **`bundle`**. A
bundle-mode feed is to statements what a bundle-mode upstream is to files. The mode is set at
creation; a change to it is `400`.

**Bundle-mode feeds.** Like a bundle-mode upstream, it never runs:
- kista never contacts its URL, which is only the issuer's identity: the URL must still be https,
  but its port is not checked against egress; it takes no credential (`400`, at creation and on a
  change); `sync` answers `409`; adding or resuming it requests no run, and the runner never picks
  it;
- its status is `new` until a bundle brings its statements, then that import's (`ok`, or `failing`
  with its error);
- it keeps the feed's other settings: the pinned attestation keys, the kinds accepted (empty: every
  kind, as online), `readers`, and the bodies offered (`upstreams` or named `channels`), which decide
  what it asks for and takes in, as online;
- its record shows the last bundle imported (`bundle_id`, `bundle_created_at`), as an upstream's.

**What a gateway's feed keeps.** An online feed keeps the statements it stored and drops the rest.
To answer a site's plan, an online feed with **`export`** set (a new setting, off by default; a
tenant administrator's, like the feed's other settings) also keeps:
- **the keys file**: the source's `GET …/attestation-keys` answer as the last run fetched it (at
  most 64 KiB), with its fetch time;
- **its permanent refusals**, per (feed, body): the `seq`, the reason and the statement's bytes and
  signature. Every permanent refusal follows verification (a statement that does not verify holds
  the cursor instead), so the bytes are always there. The reasons are those spec 0011's client
  records as permanent, except `unrequested`, which has no body: a kind not accepted, the media
  type, the title, the size, `changed`, `body`, `content`, `document`, a body not held, `limits`;
- **statements about documents it does not hold**: a withdrawal of a document it never stored, and
  any statement about a document it removed (a tombstone, made by an administrator or by the
  collector's `RemoveSuperseded`), which today leaves the tombstone at its old `seq`. Per document,
  the highest `seq` is kept. Storing the document later applies a kept withdrawal with a higher
  `seq` and deletes the row.

Setting `export` resets the feed's cursors, so its next run reads the whole history again and keeps
what it refused before (a statement held already is a no-op). A feed's refusals go when `export` is
cleared or the feed is deleted; the kept statements of an issuer go when no feed of that URL with
`export` remains. Both go with the body's cursors when the collector removes them.

Kept refusals and statements have a cap of their own per tenant (100,000 together), apart from the
quota of feed documents (spec 0011): a source flooding what an export feed keeps never costs another
source a document. Past the cap, a statement an export feed cannot keep holds its body's cursor; the
other bodies go on, and the run ends `failing` with `limits` until the collector removes bodies or
`export` is cleared. Every `limits` refusal of spec 0011 (the documents quota, the heads per kind, an
unresolved `supersedes` past its cap) stays a refusal, kept as such. A kept statement is written
under the attachments lock after reading the document again: one stored meanwhile by another feed
is withdrawn at once instead. A kept withdrawal applies to a document stored later only for the
same body and under a key the storing feed pins. A run writes cursors and what it keeps under the
tenant's feed lock, and only in the export setting it started with (`export_epoch`): a change of
`export` overtakes it, and it stops.

**The plan** gains `feeds`: the tenant's active bundle-mode feeds (or the `-feed` ones; a named one
that is paused, online or unknown is refused, as a named upstream is), merged by
URL as sources are (keys and kinds united, an empty kinds list winning; per body the lowest cursor
among them, so no feed's range starts above its own), each with:
- the feed's URL, its pinned attestation key fingerprints, and the kinds it accepts;
- `bodies`: each body the feed offers now (spec 0011's "bodies offered", which counts bundle-imported
  releases, whose origin is `upstream`) with the last `seq` received, `0` for none;
- `sources`: the plan's sources (indexes, in range, each once) whose files the bundle may bring and
  the feed may then offer. A feed offering `upstreams` names the sources of every bundle-mode
  upstream; one offering `channels` names those of the bundle-mode upstreams into its channels.
  Since sources are merged across upstreams, this is an approximation; it only keeps out of the
  bundle statements the import would refuse.

`kista admin bundle plan -feed <name,...>` names the feeds, as `-upstream` names upstreams; with
only one of the two, the other kind is left out. A plan may have feeds and no sources. It holds at
most 100,000 feed bodies across its feeds, beside its 100,000 cells; past that the plan fails,
naming the feed. The formats stay `kista-bundle-plan/v1` and `kista-bundle/v1`, with `feeds`
optional: no release carries phase 1a alone, so no reader refuses the new field.

**The export.** For each of the plan's feeds, the exporter takes what its tenant holds from the
same issuer (the URL normalised as spec 0011 normalises it). It contacts no feed.
- **Which feed.** The candidates are the tenant's online feeds of that URL with `export` set, whose
  kinds include every kind the plan's feed accepts (an empty list on either side meaning every
  kind), and with a keys file. With none, the feed fails as a whole: "not subscribed here", "not
  kept for export", "accepts fewer kinds" or "never ran"; the bundle carries the others and the
  command exits `1`. With several, each body takes the candidate whose cursor for it is highest,
  and the keys file is the most recently fetched.
- **Which bodies.** The plan's `bodies`, and, from `0`, each body of a file the bundle exports for
  one of the feed's `sources`.
- **The range** for a body is `(after_seq, seq]`: `after_seq` is the plan's cursor, `seq` the chosen
  feed's cursor (what the gateway read through, spec 0011). A body the gateway never received has
  no range and gets nothing.
- **What it carries**, in `seq` order, for each `seq` in the range it holds a statement for:
  - from the tenant's documents of the issuer and body (whichever feed stored them, since spec 0011
    stores a document once): the statement's bytes and signature, with the document's content; a
    removed (tombstoned) document's statement without content, as a kept one;
  - from the kept statements of the issuer and body: their bytes and signature, without content;
  - from the chosen feed's refusals: their bytes and signature, without content.

  Every statement carried without content, a withdrawal aside, has an unsigned note saying why: the
  refusal's reason, "removed on the exporting kista", or "not held on the exporting kista".

  At one `seq`, a stored statement wins over a kept one, and a kept one over a refusal. A document
  updated by another feed past the chosen cursor arrives with a later range.
- **Its own checks**, as the import will check: each statement verifies with a key the plan pins
  and names the feed's URL as its issuer, and each document's size and `ContentHash` match its
  statement. The first statement of a body that does not verify with the plan's keys, or whose live
  document cannot be read (its object missing, or marked corrupt), ends that body's range just below
  it, as an unverified statement holds a cursor online.
- **The entries**, after every file, feed by feed in plan order:
  - `feeds/<n>/attestation-keys.json`: the keys file;
  - `feeds/<n>/batch-<k>.json`, `k` from `1`: `[{statement, signature, note}]`, base64 as in the
    feed's answer, each batch at most 1 MiB;
  - `feeds/<n>/documents/<content hash hex>`: a document's content, uncompressed, after the batch
    that first names it, once per feed.
- **The manifest** gains `feeds`, each with its URL, its `entries` in the order the tar carries them
  (the keys file, then each batch and the documents it first names: name, hash and size), and per
  body `after_seq` and `seq`. The signed manifest pins every
  entry, so a carrier can drop neither a batch nor a withdrawal.
- **Limits.** At most 50 feeds, 100,000 statements and 100,000 documents per feed, a feed's part of
  the manifest under 16 MiB, and the whole manifest under 64 MiB; past one, the feed fails as a
  whole.
- **A consistent read.** The cursors, refusals, kept statements and documents are not read in one
  snapshot. After reading a feed, the export reads its candidates' export setting and the cursors
  it used again: a feed whose `export` changed or whose cursor went (the collector) fails, "changed
  during the export".
- **Kinds.** A document of a kind the site's feed does not take goes without content, with a note:
  online, the site refuses it before fetching, so its content never leaves the source.
- **The result** prints per feed the most recently successful candidate's status and last success
  (so a stale gateway shows), bodies, statements, withdrawals, statements without content, documents and bytes, and a
  failure; `bundle.export` counts them.

**Disclosure.** As online, the site discloses the hashes it offers to each source; they pass through
the plan, and the gateway and the operator who carries it see them. The gateway's grant at the
source, not the site's, decides what the source answered: a narrower grant gives ranges that are
complete but empty.

**The import** reads the feed entries after the files, so a body the bundle brings is held when its
statements arrive. Its targets are the tenant's active bundle-mode feeds whose URL is one of the
bundle's feeds, or the `-feed` ones; naming a feed that is paused, online, or without a feed in the
bundle refuses the import, and a bundle that serves none of the tenant's upstreams or feeds is
refused. Each target is taken under its lease (`kista/feed/<id>`, shared with online runs: a
target held is `busy`) and is `stale`, as an upstream target, for a bundle ordered before the last
one it imported, the same `id` with another manifest, or a plan older than the feed. For each body:
- a body outside the feed's offer now, or no longer held, takes nothing and gets no cursor;
- a body whose `after_seq` is above the feed's cursor takes nothing and is counted `gap`: the range
  does not start where the site is (a plan written before `bundle reset -feed`, or before the
  collector removed the cursor of a body held again). A `gap` leaves the target `ok` and makes the
  command exit `1`: write a new plan;
- each statement passes the checks of spec 0011's client, the code shared with an online run: the
  issuer is the feed's URL, the signature verifies with a key the feed pins (the bundle's keys file
  gives the public keys, used only by fingerprint), the canonical form, the kinds, the media type,
  title and size, the quotas, `supersedes`, withdrawals and the audience rule. The site judges
  every statement itself; the gateway's note is only recorded;
- a withdrawal is judged as online, whatever the gateway did with it; one about a document the site
  does not hold changes nothing (a bundle-mode feed keeps nothing for export);
- a document's content comes from the bundle, checked against its statement's size and hash. A
  statement that needs content the bundle does not carry is refused, "bundle: no content (<the
  gateway's note>)";
- a statement that does not verify holds the body: nothing after it is applied;
- the cursor moves, never back, to the range's `seq`, but no further than the highest `seq` of a
  statement for the body verified in this bundle or before. Online caps the high-water by the
  highest `seq` verified in the whole run; the import caps it per body, which is stricter. A range
  without a verified statement moves nothing, and the next plan asks for it again.

Entries are checked as they are read: each batch (at most 1 MiB, in memory) against its hash before
any of its statements is applied, each document (spooled, at most `attachments.max_size`) against
its hash. A feed entry that is missing, out of its place, or whose size or bytes differ from the
manifest fails that feed's target, as damaged media: nothing more of it is applied and its cursors
stay; the other targets go on. An unnamed entry is skipped, as in 1a. A `sources/` or `files/`
entry after a `feeds/` entry, or a feed entry over 16 MiB, stops the import as a malformed tar does;
what was applied stays. A target's cursors move only
once its feed's last entry was read: statements stored before an interruption stay (a repeat is a
no-op), and its cursors wait for a whole read. A target's last-bundle mark is recorded as an
upstream's: once its lease is held, before its first statement.

**Dry run** checks each statement's signature, issuer, kind, title, size, media type and content,
quotas and `supersedes` aside, and stores nothing: no statement, cursor, mark or status. It treats
the bodies and documents the bundle would bring as held, so its counts (`would_*`) are approximate
where statements depend on each other in the bundle (a withdrawal or a `supersedes` of a document
it carries). It records `feed.run` (with `bundle` and `dry_run`) and the target's counts. **Records**: `bundle_imports`
counts per target, feeds as `feed:<name>`; `bundle list` and `show` print them. The command exits
`1` when a feed target failed, was busy or stale.

**`bundle reset <tenant> -feed <name>`** clears a bundle-mode feed's last-bundle mark and resets
its cursors, so a bundle answering a new plan takes its history again (statements already stored
are no-ops): the recovery
after a bundle whose key holder left statements out or whose gateway lacked their content. It is
audited (`bundle.reset`, naming `feed:<name>`).

**`kista bundle inspect`** checks the feed entries' hashes and order, and each statement's signature
against the bundle's keys file, its issuer, its range, and that a statement with content has its
document in the bundle after the batch that names it, of the size it gives; with `-plan`, it says
whether each key is pinned by the plan's feed or only listed, and checks each body against the plan. **`kista bundle sign`** refuses a bundle with
a feed the plan did not ask for, a body neither in the feed's `bodies` nor of a file the bundle
carries for the feed's `sources`, or an `after_seq` other than the plan's cursor.

### Installing kista offline

The image, the chart and the binaries are signed (spec 0013). Phase 1a adds an "Air-gapped sites"
page to the docs, under Deployment. Its shell blocks run in CI (`deploy/air-gapped/test.sh`,
workflow `air-gapped`), so the page cannot drift: a block's fence carries `ci=connected` or
`ci=site`, and the test runs them in order. Each side works in its own directory; only `transfer/`
(the media) is shared, and the values the page carries out of band come from the connected side's
directory. The connected side's blocks run in a container with a network. The site's run in a container on an internal Docker network whose only other member is the
site's registry (TLS, its own CA), and the test first checks that the site reaches nothing outside.
The artifacts verified are the last release's. Before the first release, they are `:edge`'s, signed
by `image.yml`, and the blocks marked `ci=*-release` (the chart, the binary) are skipped. The cycle
runs with kista built from the commit, so the page is tested against the code beside it. The test
also runs weekly, since the last release and Sigstore's trusted root change without a commit:

- **Image and chart.** Both are OCI artifacts, signed by digest. `oras copy -r` copies them with
  their referrers (the signature and the provenance, Sigstore bundles) into an OCI layout on the
  media, and from it into the site's registry, keeping the digests. Inside the site,
  `cosign verify --trusted-root` checks them offline against Sigstore's trusted root, which
  `cosign initialize` fetched through TUF on the connected side. The root travels on the media, so
  its SHA-256 travels out of band, like the bundle key's fingerprint: a forged root would let the
  carrier sign anything as the release. The page is tested with cosign
  v3.1.3 and oras v1.3.4.
- **Build provenance.** The image's attestation is a referrer too: `cosign verify-attestation
  --type slsaprovenance1` verifies it offline with the same trusted root and identity.
  `gh attestation verify` would need a GitHub login even offline, so the site needs no gh.
- **Binaries.** These are verified with `cosign verify-blob` and their `.sigstore.json` files.
- **Values.** The chart's `image.repository` points at the site's registry.
- **The cycle.**
  - The first bundle key and its pin.
  - The bundle-mode upstreams for the case in Problem, with a passthrough channel for autoload.
  - The channel's DuckDB versions, with C API maximums for C API extensions (spec 0006).
  - The gateway kista on the connected side: its mirror-mode upstreams for the same sources (public
    for DuckDB's kinds, whose private releases never leave), the bundle key, and the export.
  - Plan, export, carry, import, with `-dry-run` first.
  - Sizing (below).
  - A slot holds one body forever, so a wrong body is a permanent `conflict` for that version.

**Sizing.** The import inflates one file at a time into the spool: about `blob.max_body` beside
the server's own ingests.
- In Kubernetes, `kubectl exec` runs it in the serving pod, whose spool emptyDir is sized for the
  server's ingests (spec 0013) and is also the pod's `TMPDIR`; the root filesystem is read-only.
  The chart gains `spool.importHeadroom` (default `0`), which adds to the emptyDir's limit and its
  ephemeral-storage request, also over an explicit `spool.sizeLimit`. Both are Kubernetes
  quantities (bytes, `Ki`, `Mi`, `Gi`, `Ti`) when added; a `sizeLimit` with no headroom is kept as
  written.
- With PostgreSQL, the import can run in a separate pod instead, as a Job with the server's
  configuration and its own emptyDir. A SQLite deployment has a single writer volume, so it imports
  in the serving pod.

### API and CLI

- **Feeds (1b).**
  - `mode: "bundle"` on `POST /api/v1/tenants/{t}/feeds`, and `-mode bundle` on
    `kista admin feed add`; the record and `feed show` give `mode`, `bundle_id` and
    `bundle_created_at`; the console's feed form offers the mode, and no "Sync now" or credential
    for bundle mode.
  - `sync` on a bundle-mode feed answers `409`, and a credential is refused with `400`.
  - `export` on an online feed (`POST …/feeds` and a change), `-export` on `feed add` and
    `feed set`; refused on a bundle-mode feed (`400`).
  - `kista admin bundle plan|import … -feed <name,...>` names feeds, as `-upstream` names
    upstreams; with only one of the two, the other kind is left out. `kista admin bundle reset
    <tenant> -feed <name>`.
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
- **Phase 2.** Uploading a bundle over the API in chunks, imported by the server: below.

### Uploading a bundle (phase 2)

Phase 2 is for administrators who cannot run `kista admin` beside the server and cannot write to
its object store: a bundle uploaded over the API, imported by the server. A bundle is gigabytes,
and the path to the server has limits of its own: an ingress or proxy caps a request body (nginx
1 MiB by default, a cloud load balancer or a serverless front end some tens of MiB), and a long
request breaks. So an upload comes in **chunks**, each its own request, each stored as its own
object; a broken upload resumes by sending the chunks the server lacks. No object store needs
multipart support: the import reads the chunks in order as one stream.

**The session.** A server administrator creates an upload:

```text
POST   /api/v1/tenants/{t}/bundles              {size, sha256, dry_run?, upstreams?, feeds?, any_tenant?}
                                                → 201 {id, chunk_size, chunks, expires_at}
PUT    /api/v1/tenants/{t}/bundles/{id}/chunks/{n}   the chunk's bytes            → 204
GET    /api/v1/tenants/{t}/bundles/{id}         the upload: state, chunks received, its import
POST   /api/v1/tenants/{t}/bundles/{id}/complete                                  → 202
DELETE /api/v1/tenants/{t}/bundles/{id}         an upload not yet importing       → 204
GET    /api/v1/tenants/{t}/bundles              the tenant's uploads and imports, newest first
```

- `size` is the bundle's size in bytes, at most `bundles.max_size` (default 20 GiB); `sha256` is
  its SHA-256 (the export prints it), so a damaged or mixed-up upload is refused before it is
  imported. The options are the CLI's import flags.
- The server answers the chunk size (`bundles.chunk_size`, default 16 MiB, 1 to 64 MiB) and the
  number of chunks: chunk `n` (from 0) is the bundle's bytes from `n × chunk_size`, the last one
  shorter.
- **A chunk** is a `PUT` of exactly its bytes (`Content-Length` its size), with the
  `Content-Digest: sha-256=:<base64>:` of RFC 9530. The server reads at most the chunk's size,
  checks the digest, and stores the chunk as `uploads/<id>/<n>` in the tenant's blob domain. A
  chunk sent again replaces itself only with the same digest (a retry); another digest is `409`.
- **Resuming**: `GET` lists the chunks received; the client sends the others, in any order and in
  parallel.
- **Complete** checks every chunk is there, and queues the import (`202`). One replica takes it
  under a lease (`kista/bundle-upload/<id>`), reads the chunks once to check the declared SHA-256
  (a mismatch fails the upload, and nothing is imported), then reads them again in order as one
  stream and imports it as `kista admin bundle import` does, with the bundle keys pinned for the
  tenant. The import's `input_sha256` must equal the declared one too. The upload records the
  import's id; `GET` shows its record and result.
- **The chunks go** once the upload is imported, failed or aborted, and with an upload that
  expires: one not completed within `bundles.upload_ttl` (default 24 h) of its last chunk. The
  storage collector (spec 0016) deletes them, and the objects under `uploads/` whose upload is
  gone.
- **Limits.** At most `bundles.max_uploads` (default 2) uploads per tenant not yet imported, and
  one import of an upload at a time per tenant. A chunk's request has the server's read timeouts;
  a stalled chunk is simply sent again.

**The blob layer** gains the `uploads/` prefix: `uploads/<id>/<n>`, written and deleted by kista
(unlike `imports/`, which kista only reads), listed only by the collector.

**The console** uploads from the tenant's Upstreams page, for a server administrator: a file
picker, its SHA-256 computed in the browser, chunks sent with progress and retried, an upload
resumed after a reload (the session's id kept in the browser's storage), and the import's result.

**Who.** Server administrators only, as the CLI's import: an upload changes what every reader of
the tenant installs. Each act is audited: `bundle.upload` (created, with the size and declared
SHA-256), `bundle.upload_abort`, and the import's own `bundle.import`.

The CLI needs none of this: `kista admin bundle import` reads a file, standard input or
`imports/`.

### Events

Spec 0010's catalogue gains:

- **`bundle.import`**, with:
  - `import` (the record's id), `manifest`, `created_at`, `exporter`, `bundle_key`, `input_sha256`,
    `dry_run`, `state`;
  - `targets`: the upstream names, with counts only up to `MaxData`, since the full counts are on
    the record.

  Its subject is the tenant. The actor is the event's actor.
- **`bundle.export`**, on the exporting kista (with phase 1b's `feed_*` counts and `failed_feeds`): the plan's SHA-256, `manifest`, `bundle_key`, counts
  per outcome. Its subject is the exporting tenant.
- **`bundle.reset`**: the target and the mark it cleared.
- The new kinds and fields join the audit's field allowlist (`internal/audit`) and its coverage
  test; `targets` comes last, since an event's data keeps its first fields when it is truncated.
- **`upstream.run`** gains `bundle` (the manifest's id).
- **`upstream.release`** gains `bundle`, so the audit alone answers "which bundle brought this
  release, and who imported it".
- **`upstream.rejected`** is made when a cell changes into a refusal, as in a run.
- **Phase 2.** `bundle.upload` (an upload created: its id, size and declared SHA-256) and
  `bundle.upload_abort`, subject `tenant:<name>`; the import records `bundle.import` as the CLI's.
- **Phase 1b.** `feed.run` gains `bundle` (the manifest's id) and `dry_run` for an import's target
  feed; `bundle.import`'s targets name feeds too (`feed:<name>`); `bundle.export`'s counts gain the
  feeds'; `bundle.reset` names `feed:<name>`; `feed.add` gains `mode` and `export`, and
  `feed.change` records a change of `export`.

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

**Migration 0021** (phase 1b), on all three dialects, with `-- +min_reader 21`: an older replica
would run a bundle-mode feed online, and would not keep what an `export` feed keeps, leaving silent
gaps.
- `feeds.mode`, `online` | `bundle`, not null, default `online`, with its CHECK (a new column: no
  rebuild); `feeds.export`, a boolean, default false; `feeds.export_epoch`, counting its changes;
- `feeds.bundle_id`, `bundle_created_at` and `bundle_manifest_sha256`, null;
- `feeds.attestation_keys` (the keys file's bytes) and `attestation_keys_at`, null;
- **`feed_refusals`** `(feed_id, body_hash, seq)` primary key, `ON DELETE CASCADE` from `feeds`:
  `reason`, `statement`, `signature` (not null), `refused_at`;
- **`feed_kept`** `(tenant_id, issuer, issuer_id)` primary key: `body_hash`, `seq`, `statement`,
  `signature`, `kept_at`, indexed on `(tenant_id, issuer, body_hash, seq)`;
- both join the reaper's steps (`feed_refusals` before `feeds`) and the collector's step beside
  `DeleteGoneFeedCursors`.

**Migration 0022** (phase 2), on all three dialects, with `-- +min_reader 22` (an older replica
would not import a completed upload, and its collector would not know `uploads/`):
- **`bundle_uploads`**: `id`, `tenant_id`, `size`, `sha256`, `chunk_size`, `options` (JSON: dry
  run, upstreams, feeds, any tenant), `state` (`uploading`, `queued`, `importing`, `imported`,
  `failed`, `aborted`, `expired`), `import_id` (null), `error`, `created_by`, `created_at`,
  `updated_at`; deleted with the tenant;
- **`bundle_upload_chunks`** `(upload_id, n)`: `size`, `sha256`, `received_at`, `ON DELETE CASCADE`.

### Configuration

| Setting | Phase |
| --- | --- |
| `upstreams.bundle_keys` | 1a |
| `upstreams.bundle_sources` | 1a |
| `bundles.max_size` | 2 |
| `bundles.chunk_size` | 2 |
| `bundles.upload_ttl` | 2 |
| `bundles.max_uploads` | 2 |
| The chart's `spool.importHeadroom` | 1a |

Phase 1b adds no setting: `export` and the mode are a feed's.

`spool.importHeadroom` adds to the spool emptyDir's limit and request, also when `spool.sizeLimit`
is set explicitly.

The config check refuses:
- malformed fingerprints, entries without `tenants`, and more than 10 entries;
- `bundle_sources` entries without `prefixes` or `tenants`, and more than 100 entries;
- prefixes that are not https.

### Package layout

```text
internal/bundle          the plan and the manifest (types, validation, canonical order),
                         signing and verifying, the tar reader and writer
internal/upstream        + bundle mode (no sync, no credential, no egress port check, passthrough);
                         the plan; the import: targets, leases, freshness, per-cell intake, current
                         releases; the export: sources from upstreams and own channels, files with
                         their original signatures, the checks, the summary (beside the plan and the
                         import, whose reads of upstreams, cells and keys it shares)
internal/release         + Ingest names the bundle in its event; MayPublish takes the tenant, for
                         "bundle:" pseudo-credentials
internal/blob            + the imports/ prefix, read-only to kista, outside the collector; the
                         uploads/ prefix (phase 2), swept by the collector
internal/attachment      + bundle-mode feeds; `export` and what it keeps; the client's checks
                         shared by a run and a bundle import (1b)
internal/bundle          + the plan's and the manifest's feeds, the feed entries (1b)
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

Phase 2 updates `api.md` (the upload routes), `upstreams.md` (Uploading a bundle),
`configuration.md`, `observability.md`, `storage.md` (the `uploads/` prefix), `console.md`,
`deployment/kubernetes.md` (ingress body limits need no change: chunks fit) and
`deployment/air-gapped.md`.

Phase 1b updates, each with the PR whose behaviour it describes: `attachments.md` (bundle mode,
`export` and what it keeps), `api.md`, `administration/commands.md`, `observability.md`,
`upstreams.md` (statements in a bundle), `security.md` and `deployment/air-gapped.md` (a
statements bullet).

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
- **0011 (1b)**: feeds gain mode `bundle` and `export`; an `export` feed keeps its keys file, its
  permanent refusals and statements about documents it does not hold.
- **0019 (1b)**: the reaper deletes `feed_refusals` and `feed_kept`.
- **0016 (1b)**: the collector deletes them with the body's feed cursors.
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
    gateway, cannot bring an older authentic build back. The record keeps the newest body the cell
    held and its fetch time through `missing`, `rejected` and `failed` outcomes (a high-water
    mark), so marking a cell missing first does not open the door. A cell's `fetched_at` may not be
    later than the manifest's `created_at`, itself at most 10 minutes ahead of the site's clock, so
    a wrong clock blocks a cell for minutes, not for good. Damaged media (a gzip entry or inflated
    file that does not match the signed manifest) fail the cell and leave its record.
  - **Additional defence.** A site that wants it pins exact versions in the entries' `versions`.
- **What the bundle key's holder can do.** They choose what the site receives, within the site's
  allowlists, filters and the sources' signatures: withhold, mark cells `rejected` or `missing`,
  leave out an `authenticated` flag, or offer an authentic build older than what a cell would
  otherwise get. The `fetched_at` the no-downgrade rule compares is the holder's word: the holder,
  unlike a lagging or second exporter, can bring back an older authentic build by signing it with
  a new time. Pinning exact versions (below) is the defence against that. The holder is the exporting side: an
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
    egress and without a credential, as a mirror's key check does (spec 0009): the plan cannot
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
  The service checks it (`Import` and `ResetBundle` require the server-wide admin verb), so phase
  2's route inherits the rule.
  - A tenant administrator configures the bundle-mode upstreams, their allowlists and pinned keys,
    which bound what any import may release into the tenant.
  - The manifest names its tenant, and importing it into another tenant needs `-any-tenant`.
- **Isolation.** An import fills one tenant's upstreams. Bodies are shared within a storage domain
  as spec 0005 shares them.
- **Untrusted text.** The export's reasons are bounded, control characters are stripped, and they are
  shown as data.
- **Statements in a bundle (1b).**
  - Statements stay signed by their issuer and are checked at the site under its own pins; the
    keys file is unsigned and used only by fingerprint. Neither a carrier nor the bundle key's
    holder can forge, change or reorder a statement.
  - The bundle key's holder (or whoever signs an unsigned export) can leave statements out of a
    range, a withdrawal among them, and withhold documents: the source's numbering has gaps, so a
    range cannot prove it is whole. The site judges every statement itself, applies every
    withdrawal it receives, and moves a cursor no further than a statement it verified, so an empty
    or forged range moves nothing. `bundle reset -feed` takes the history again from another
    bundle. A per-body checkpoint signed by the issuer is a follow-up.
  - A gateway's `export` feed keeps refused statements and statements about documents it does not
    hold, within a cap of its own per tenant, so a source can neither grow them without bound nor
    cost another source a document.
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
- **Export** (`internal/upstream`, beside the import's fakes, and round trips through the import),
  from a store and blob domain the tests seed (mirrored cells with original signatures, an own
  channel, test keys and a fake https repository for `.well-known`):
  - files exported as body ‖ original signature, verifiable with the plan's keys;
  - an own channel's files signed by a key in both the pins and its `.well-known`, after a key
    rotation too; an own passthrough channel refused as a source;
  - `unchanged` by `body_hash` only (an equal ETag alone exports the file);
  - every row of the outcome table: a yank, a block, a hidden deprecation and a `release current`
    made after the run, all decided by the release, not the stale cell record; another source's or a
    published release; the mirror's refusal; pull-through cells never asked; cells outside the
    exporting matrix; none of the plan's versions;
  - private releases, of an own channel and of a credentialed mirror, only with `-private`, and
    only of `<tenant>`; `authenticated` per cell;
  - a release without an original signature `rejected` (a defensive branch: no route makes an
    upstream release without one, so it has no test);
  - `fetched_at` from the cell record (kept fresh by `unchanged` runs) and `released_at` for own
    channels;
  - the files written from the stored gzip with a gzip tail, read twice, no temporary copy;
    `-unsigned` then `kista bundle sign` imports as a bundle exported with `-key` does (the two
    differ only in their ids and times);
  - two upstreams of one source, the first holding the cell wins;
  - a source mirrored nowhere fails as a whole; a `.well-known` listing no planned key fails it;
  - `-sync` runs the mirror-mode upstreams first;
  - an unpinned bundle key stops before reading; a hand-written plan with a bad grammar;
  - a refused export leaving no output (the CLI writes a temporary file, linked into place when complete);
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
  - **Dry run** records `last_run`, its event and the import record, and no cell, mark or release.
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
  1. A source kista serves a signed channel over https (a test CA): a bundle-mode prefix is https
     only.
  2. A gateway kista mirrors it; the export package makes a bundle from the site's plan.
  3. A site kista whose egress refuses every address imports the bundle into a bundle-mode upstream.
  4. DuckDB runs `INSTALL … FROM` and `LOAD` against the site.
  5. A new version at the source, a second bundle; the new version is current.

  The DuckDB pin is a development build, for which DuckDB publishes no core files, and the pinned
  DuckDB refuses core files signed with test keys. Core sources and passthrough are therefore
  covered by unit tests, with the keys from `upstream.Service.DuckDB` (the existing test override).
- **Docs.** The Air-gapped sites page's shell blocks run in CI as described in "Installing kista
  offline": the last release's image (`:edge` before the first), verified at a site that reaches
  only its registry, and a cycle with DuckDB's `json` and `httpfs` from `extensions.duckdb.org`
  through a gateway into a site's passthrough channel.
- **The chart.** `spool.importHeadroom` added to the computed size and to an explicit `sizeLimit`,
  in the volume and the request; malformed quantities refused (`deploy/helm/chartcheck`,
  `deploy/helm/tests/fail`).
- **1b.**
  - **Mode.** A bundle-mode feed is never run, requested or synced (`409`), takes no credential
    (`400`), skips the egress port check but not https; its mode never changes (`400`). Migration
    0021 on all three dialects; the reaper's completeness test with the new tables.
  - **`export`.** Setting it resets the cursors; the feed then keeps its keys file, every permanent
    refusal (bytes and signature), a withdrawal of a document never stored, a statement about a
    tombstone (the highest `seq`), a kept withdrawal applied when the document arrives (same body,
    a pinned key); past its cap the body's cursor held and the run `failing`, the other bodies
    going on, and other feeds unaffected; a run overtaken by a change of `export` writes nothing; clearing it
    deletes the feed's refusals, and the kept statements once no `export` feed of the URL remains;
    the collector removes them with the cursors.
  - **Plan.** Bodies with their cursors, `0` for none; `sources` per offer; `-feed` and
    `-upstream` alone; the cap.
  - **Export.** The candidates by `export`, kinds (empty ones too) and keys file, and each failure
    as a whole; the highest cursor among several; the range `(after_seq, seq]`; stored statements by
    issuer whichever feed stored them, kept ones and refusals, the precedence at one `seq`; a body
    the bundle brings from an allowed source with its whole history, none from another; the range
    ended below a statement that does not verify or a document that cannot be read; batches at
    1 MiB; documents by content hash, once; a tombstone carried without content, a missing live
    object ending the range; the limits; no credential in the bundle.
  - **Import.** Statements checked and applied with the shared client code, arriving for a body
    the same bundle brings; content not carried refused with the exporting side's note; a keys
    file without a pinned key failing the target; the cursor no further than a verified statement,
    never back, held below a statement that does not verify or comes out of order, moved only
    after a whole read; a damaged batch and a truncated bundle failing the feed only; `gap` exiting
    `1`; stale and busy targets; `-feed`; a bundle of feeds only imported with no flag; dry run
    with what the bundle brings treated as held; `bundle reset -feed`. Another issuer and a body
    outside the offer are refused by the shared checks the online client's tests cover.
  - **Round trips** in-process (`internal/upstream`): a gateway's export feed and a site's
    bundle-mode feed through export and import, with the body the same bundle brings; a refusal
    local to the gateway judged again at the site; a withdrawal in a later bundle; a damaged batch;
    a reset and the gap after it. The DuckDB end-to-end test stays as 1a's: DuckDB reads no
    statement, so the round trips cover the path.
- **2.** An upload in chunks, out of order and in parallel; a chunk resent with its digest (a
  no-op) and with another (`409`); a wrong `Content-Length` or digest refused; resuming from the
  chunks received; complete with a chunk missing refused; the declared SHA-256 differing (the dry
  pass fails, nothing changes); the size limit and `max_uploads`; server administrators only; one
  import per upload across replicas (two runners, one lease); the chunks deleted after the import,
  an abort and expiry, and orphans under `uploads/`; the console's upload, resumed after a reload
  (Playwright).

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
- **Issuer-signed checkpoints** (1b): a source signing, per body, the highest `seq` it has issued,
  so a range can prove it is whole and a bundle cannot leave statements out unseen.
- **Yanking by bundle:** `kista admin release yank -bundle <id>`.
- **OCI layouts as a second carrier.**
- **Relaying feeds onward** (spec 0011): a connected kista relaying a source's statements into
  bundles for several sites.
- **Licences** (spec 0012): long-lived licence tokens for air-gapped holders, carried in a bundle.
