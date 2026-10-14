# Spec 0014: Bundles for air-gapped sites

- **Status**: draft
- **Date**: 2026-10-14
- **Author**: vgsml, Claude

## Summary

An air-gapped kista cannot reach its upstreams. A **bundle** carries what those upstreams would
have served, on removable media or through the site's object store: the extension files exactly as
each source served them, the source's published keys, and (phase 1b) signed statements about the
files (spec 0011).

The cycle has three steps:

1. **Plan.** The site writes a **plan** from its own configuration: its bundle-mode upstreams,
   their allowlists and platforms, its channels' DuckDB versions, and what it already holds.
2. **Build.** On a connected machine, `kista bundle build` fetches the plan's cells as a mirror
   would, checks them, and writes one tar. It is a client tool and needs no kista server. The
   builder signs the bundle's manifest with a **bundle key** that the site pinned beforehand.
3. **Import.** The site imports the bundle into its **bundle-mode upstreams**. A file is released
   only if all of these hold:
   - its signature verifies with a key the site pinned for that source;
   - the signed manifest names it for that cell, in a bundle newer than the last one imported;
   - its footer matches the cell;
   - it passes spec 0008's file checks.

   A signed channel re-signs the file with its own key. A passthrough channel serves DuckDB's own
   core files, so nodes on the site autoload without the internet.

The two signatures answer different questions. The source's signature says the file is authentic.
The bundle key's signature says that this file is what the source served for this cell when the
bundle was built. The second one is what stops whoever carries the bundle from swapping in an old,
authentic, vulnerable build.

Delivery, each phase a pull request:

- **1a. Extensions**:
  - bundle-mode upstreams;
  - bundle keys;
  - the plan, the builder and `inspect`;
  - the import from a file, from standard input or from the site's object store;
  - the import records and the CLI;
  - the console's changes to its upstream forms;
  - the docs, including installing kista itself offline.
- **1b. Statements**: bundle-mode feeds. The builder pulls a source's statements and documents for
  the bodies the site offers, and the import verifies them as spec 0011's feed client does.
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
  `POST …/credential` on a bundle-mode upstream. The builder authenticates instead, and the site's
  configuration plays the credential's part for visibility (Visibility).
- **A passthrough channel takes it** when its kind is `duckdb-core`. The passthrough guard that
  today admits mirrors only admits bundle mode too; pull-through stays refused.

`mode` is fixed at creation. A site that gets a network link later adds a mirror and removes the
bundle-mode upstream: the releases stay with the channel (spec 0009).

`kista admin upstream list` and `show` print the mode. The API's upstream record carries it already.
The console's add form offers `bundle`, and its "Sync now" button is hidden for bundle mode.

### Bundle keys

A bundle key signs a bundle's manifest. It lives with the builder on the connected side. It is an
Ed25519 key in a PEM file:

```sh
kista bundle keygen -o bundle-key.pem      # prints the fingerprint: sha256:<hex of the SPKI DER>
```

The site pins fingerprints in its configuration. This is configuration, not data, because bundle
keys are a server administrator's trust decision, like key sources:

```yaml
upstreams:
  bundle_keys: ["sha256:9a1f…"]            # at least one for any import; at most 10
```

- **Every bundle is signed, and an unsigned bundle is refused.** The bundle carries the public key
  (`kista-bundle.pub`), and the import uses it only when its fingerprint is pinned.
- **Rotation.** Pin the new fingerprint, build with the new key, then remove the old fingerprint.
- **Cost of a stolen key.** Whoever holds a bundle key decides what the site receives, within the
  site's allowlists and the sources' signatures. That is what a carrier can do without one. The docs
  advise keeping the key on the build machine only, or in a hardware token that offers a PEM-capable
  PKCS#11 bridge. Keys in KMSs and vaults (spec 0004's key sources) are a follow-up.

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
  makes the union empty. The builder uses `versions` only to skip files the import would refuse;
  the import applies each upstream's own filter.
- **Dev versions.** A channel's `dev` DuckDB versions (source ids) are left out of core and
  community cells, since DuckDB publishes nothing for them. A repository source keeps them: another
  kista may serve development builds.
- **What the site holds.** `etag` and `body_hash` are given for a cell only when every upstream
  sharing it has a conditional outcome for it (`released`, `unchanged`, `yanked` or `conflict`; spec
  0009) with the same values. Otherwise both are omitted, so a cell one upstream holds and another
  lacks is fetched in full. `body_hash` is spec 0002's composite hash of the body without the
  signature, in hex, as cells store it.
- **Size.** A plan holds at most 100,000 cells.
- **Hand-written plans.** A plan may be written by hand. The builder validates it with spec 0009's
  grammars before making any request.

### Keys for repository sources

Pinning a repository source's key on the site needs its fingerprint from somewhere trustworthy:

- **From the publisher.** A publisher shows its fingerprints out of band; Enterest shows them in a
  subscription's console.
- **From the connected side.** `kista bundle keys <prefix>` fetches the source's `.well-known` file
  and prints each key's fingerprint, for comparison with what the publisher published.
- **From the builder.** The builder prints every source's listed fingerprints in its summary.
  `kista bundle inspect` marks each one as pinned on the site or listed but not pinned, which is how
  an operator notices a key rotation at the source.

The media are never the authority for a fingerprint. Neither is the plan: a plan only repeats the
site's pins.

### Building a bundle

```sh
kista bundle build -plan plan.json -key bundle-key.pem -o acme-2026-10.kbundle \
    [-token <prefix>=<file>]… [-ca-file <pem>] [-tmp-dir <dir>] [-concurrency 4] \
    [-max-body 1GiB] [-allow-private] [-allow-loopback-http]
```

`kista bundle` is a client command beside `kista ext` (spec 0002). It reads no server configuration
and opens no database. The builder signs with its key only if the key's fingerprint is among the
plan's `bundle_keys`; otherwise it stops before fetching anything.

For each source:

1. **Keys.**
   - Core and community use DuckDB's keys compiled into kista (`internal/upstream/duckdbkeys`).
   - A repository source's `<prefix>/.well-known/duckdb-extension-repo.json` is fetched (at most
     64 KiB) and kept as it came. A source whose file lists none of the plan's keys fails as a
     whole.
2. **Cells.** Each cell is fetched from spec 0009's path, `<prefix>/<version>/<platform>/<name>.duckdb_extension.gz`,
   under these rules:
   - **Size.** The compressed answer may be up to `-max-body` plus 1 MiB, and the inflated file up
     to `-max-body` plus 256 bytes.
   - **Transport.** No redirects, and https only; plain http is allowed only to loopback, with
     `-allow-loopback-http`. Private and loopback addresses are refused unless `-allow-private`.
     The builder checks addresses at every dial, as spec 0006's egress does.
   - **Conditional requests.** `If-None-Match` is sent with the plan's `etag` when there is one.
     The answers:
     - `304` records the cell `unchanged`, and the cell carries no file;
     - a `304` to a request that was not conditional is `failed`;
     - `404` is `missing`.
3. **Checks.** The builder checks what the import will check, so useless bytes are not carried.
   - **Signature:** it verifies with the source's keys.
   - **Footer:** the platform is the cell's, and an exact build (`cpp`, `C_STRUCT_UNSTABLE`) names
     the cell's DuckDB version.
   - **Version filter:** the extension version is on `versions`, when a filter is given.

   A failed check records the cell `rejected` with its reason, and the file stays out of the
   bundle. A body whose hash equals the plan's `body_hash` is recorded `unchanged`, also without
   its file. The import runs every check again, with spec 0008's file checks added.
4. **Tokens.**
   - **Matching.** A token goes to the source whose prefix the `-token` prefix covers, compared as
     spec 0009's credentials are (scheme, host, port, path segment boundary). `-token` on a core or
     community source is refused.
   - **Sending.** The token file is read at each request. The token goes in
     `Authorization: Bearer` and is never sent on a redirect, since there are none.
   - **Containment.** The token reaches no file, log line or error.
   - **Refusals.** A `401` or `403` fails the source as a whole and names the status. When an
     authenticated source answers `404` for every cell, the builder warns: kista answers a token
     without the grant as missing (spec 0006).

Files are fetched `-concurrency` at a time (default 4, at most 2 per host). Each file is kept as the
source served it, gzip-compressed, in `-tmp-dir` (default: the system temporary directory). The tar
is then written to a temporary name beside `-o`, deleting each file once it is appended, and
renamed when complete. The builder prints the disk it needs before it starts: the plan's cell count
times `-max-body` is the upper bound.

The summary lists, per source:
- cells per outcome;
- bytes;
- the listed fingerprints;
- the first 20 problems.

The builder exits non-zero when a source failed as a whole. Rejected cells alone do not fail the
build, as they do not fail a mirror's run.

### The bundle

A bundle is a POSIX tar (ustar or PAX, without sparse entries). Extension files inside it are gzip
streams, as DuckDB's servers serve them: native libraries shrink three to four times, and the
import inflates under a cap as a mirror does. The tar itself is not compressed. The entries come in
this order:

```text
kista-bundle.json                      the manifest; first; at most 64 MiB
kista-bundle.sig                       Ed25519 over "kista-bundle-v1\0" ‖ the manifest's bytes
kista-bundle.pub                       the bundle key's public key (PEM)
sources/<n>/well-known.json            a repository source's .well-known file, as fetched
files/<sha256 hex>.gz                  an extension file as the source served it, gzip-compressed;
                                       named by the SHA-256 of the inflated file (body ‖ signature)
```

The manifest:

```json
{
  "format": "kista-bundle/v1",
  "id": "0192…",                              // a UUIDv7 made by the builder
  "created_at": "2026-10-14T12:30:00Z",
  "builder": "kista v0.3.0",
  "plan": {"tenant": "acme", "created_at": "2026-10-14T12:00:00Z", "sha256": "…"},
  "sources": [
    {"kind": "repository", "prefix": "https://enterest.hugr-lab.com/acme/prod",
     "well_known": {"path": "sources/1/well-known.json", "sha256": "…", "size": 812},
     "authenticated": true,
     "cells": [
       {"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "duckdb_acl",
        "outcome": "fetched", "file": "files/7a3c….gz", "sha256": "7a3c…", "size": 1388416,
        "inflated_size": 4194560, "etag": "\"…\"", "fetched_at": "2026-10-14T12:31:02Z"},
       {"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "tresor",
        "outcome": "unchanged", "etag": "\"…\""},
       {"duckdb_version": "v2.0.0", "platform": "linux_amd64", "name": "acl_otel",
        "outcome": "rejected", "detail": "the signature does not verify with a pinned key"}
     ]}
  ]
}
```

- **Signed.** The manifest binds every file to its cells by hash and size, and the signature covers
  its exact bytes. A carrier can withhold the whole bundle but cannot change, add, move or drop
  anything inside it without the import noticing.
- **Validated before use.** The import validates the manifest before anything else:
  - the format and the grammar of every field;
  - sources unique after normalisation;
  - cells unique within a source;
  - one file per cell;
  - every reference to a file giving the same sizes;
  - `etag` at most 256 bytes, and `detail` at most 1,000 bytes of printable UTF-8 (the cell
    columns' sizes);
  - `fetched_at` in RFC 3339 and `id` a UUIDv7;
  - at most 100,000 cells and 50,000 files.
- **Order.** Files appear in canonical order: by source, then name, then platform, then the lowest
  DuckDB version among the cells each file fills, compared with spec 0009's version order
  (`release.CompareVersions`).

### Importing a bundle

```sh
kista admin bundle import <tenant> <file | - | blob:<domain>/imports/<name>> \
    [-upstream <name>,…] [-dry-run] [-tmp-dir <dir>]
```

- `-` reads standard input, for a pod whose image has no shell or `tar`:
  `kubectl exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml bundle import acme - < acme.kbundle`.
- `blob:` reads an object that the operator uploaded to the site's object store with its own tools
  (`azcopy`, `aws s3 cp`, `gsutil`, `mc`; these resume a broken upload) under the domain's new
  `imports/` prefix: `imports/<name>`, where `<name>` matches `[a-z0-9][a-z0-9._-]{0,127}`.
  - The storage collector (spec 0016) does not sweep `imports/`; the operator removes the object
    after the import.
  - This is how a site without `kubectl exec` imports, for example by running an Azure Container
    Apps job (spec 0013 phase 2).
  - It also avoids feeding gigabytes through `exec`.

The import reads the bundle once, front to back, and never extracts it.

1. **Head.**
   - The first three entries must be the manifest, its signature and the public key.
   - The key's fingerprint must be in `upstreams.bundle_keys`, and the signature must verify.
   - The manifest must pass validation.
   - `plan.tenant` must be the tenant being imported into, unless `-any-tenant` is given. Importing
     one bundle into two tenants is two imports, each by a server administrator.

   Any failure refuses the bundle before anything is released.
2. **Targets.**
   - **Selection.** The import selects the tenant's active bundle-mode upstreams, or the named ones.
     A target is matched to a source by kind (core, community) or by normalised prefix (repository).
     An upstream without a source in the bundle is not touched.
   - **Refusals at start.** The tenant must be writable: an import into a tenant being deleted (spec
     0019) is refused. A suspended tenant is imported into, as runs are.
   - **Leases.** The import takes `kista/upstream/<id>` for each target and renews it. A target
     already leased (a run of another import) is reported `busy` and skipped.
   - **Freshness.** A target whose last imported manifest has a later `created_at` is skipped as
     `stale`. Re-importing the same manifest `id` is allowed (it resumes an interrupted import), and
     anything older is refused for that target.
   - **Matrix.** Each target's matrix is computed as a run computes it, with the same caps: a matrix
     over its cap fails the target, as a run fails.
   - **Unplanned cells.** Bundle cells outside every target's matrix are counted `unplanned`.
3. **Keys.** Core and community targets use the compiled-in keys. A repository target uses the keys
   in its source's `.well-known` entry whose fingerprints it pinned. That entry's hash and size must
   match the manifest, and it precedes the source's files. A target with no pinned key in it fills
   nothing; its run is `failed` with `upstream.keys: the bundle's keys list none of the pinned
   fingerprints`.
4. **Each file**, in tar order:
   - **The entry.** It must be a regular file named in the manifest, of the stated size. Its gzip
     stream is inflated to a temporary file under `-tmp-dir`, which defaults to `TMPDIR`. The
     inflated size may not exceed the manifest's `inflated_size`, which itself may not exceed
     `blob.max_body` + 256 bytes, and its SHA-256 must match.
   - **Skipped entries.** Unnamed entries, other entry types and second copies are read through
     without being stored, and counted. Their total size is capped at 64 MiB: past the cap the
     bundle is refused from that point on.
   - **Once per file.** The file is spooled, committed and checked once:
     - the signature with each target's keys;
     - spec 0008's file checks;
     - the footer parsed.
   - **Per cell.** For each target cell the file fills, the import re-reads the target as a run
     does (spec 0009's freshness):
     - a target paused or removed meanwhile stops, and its run ends `failed`;
     - an entry or platform removed meanwhile, or an unpinned key, applies to the next cell;
     - a tenant now being deleted stops the import.

     It then runs the cell's checks (the footer against the cell, the entry's version filter) and the
     locked part of the intake: block, shadow, slot, Build, release. The new release is inserted
     **not current**, and the cell's outcome is recorded as a run records it, with the manifest's
     ETag.
   - **Resources.** The CLI import holds one file at a time: one inflated temporary file and one
     spool, so about twice `blob.max_body` of disk at worst. In a separate process from the server,
     it does not share the server's ingest slots: the docs say so and size the disk.
5. **Cells without a file.**
   - `missing` records `missing`.
   - `unchanged` keeps the target's record when it has one; otherwise the cell is recorded
     `missing` with "not in this bundle and not held".
   - `rejected` records `rejected` with the builder's reason prefixed `bundle:`, control characters
     stripped.
   - A `fetched` cell whose file never arrived, because the tar ended early, is `failed`.
   - Cells the bundle does not mention keep their records. A bundle may be partial, and the import
     never prunes cells: a run removes cells outside the matrix, and the next plan leaves them out.
6. **Current releases.**
   - After the stream, for each target and each (name, platform) that gained releases in this
     import, the import makes current the release a run would have made current: the one from the
     cell that is last in canonical DuckDB-version order among the cells released in this import.
     This is spec 0008's `release current` change, made under the channel lock.
   - A cell whose slot already held its body (`unchanged`, `yanked`) changes nothing. As in a run,
     an administrator's choice of current survives every import that brings nothing new.
   - Releases from an import that stopped early are made current in the same way, for the cells it
     completed.
7. **End.** Each target's run is recorded as a run's is: start, end, counts per outcome and the
   first 20 problems, with `bundle` naming the manifest's `id`. Its last imported `created_at` is
   recorded on the target. The leases are released. The command prints a summary per upstream and
   exits non-zero if any target failed, or if the tar was truncated or malformed. What completed
   stays: each release is atomic.

**Stalls.**
- An input that delivers nothing for 5 minutes ends the import, which then completes as at step 7.
  So does an input slower than spec 0009's rate floor (`upstreams.min_rate`) once 30 seconds have
  passed.
- An import's leases never outlive these limits. Spec 0019's deletion waits for upstream leases and
  is never blocked indefinitely.
- An import ends at the end-of-archive blocks of the tar and does not wait for end of input, which
  some `exec` transports never deliver.

**Dry run.** `-dry-run` runs every check, spool included. It releases nothing and changes no cell
or current release. It records the targets' `last_run` (counts as `would_release`) and the
`upstream.run` event with `dry_run: true`, as a mirror's dry run does.

**Repeating an import.** Importing the same bundle again releases nothing new. Cells that were
released are `unchanged`; refused cells are refused again with the same outcome.

**Actor.** The import runs as the `kista admin` operator, `os:<uid>:<user>`. Importing is a server
administrator's act: `kista admin` already holds the database and every key source.

**Provenance.** A release's provenance is spec 0009's object:
- `url`: the cell's URL at the source;
- `etag` and `fetched_at`: from the manifest;
- `key`: the source key that verified;
- `bundle`: the manifest's `id`;
- `imported_at`: the time of the import;
- `credential`, for an authenticated source only (Visibility).

### Import records

Each import, dry runs included, is a row in **`bundle_imports`**:

| Column | |
| --- | --- |
| `id` | |
| `tenant_id` | |
| `manifest_id` | |
| `manifest_created_at` | |
| `builder` | |
| `bundle_key` | the fingerprint |
| `input_sha256` | the SHA-256 of every byte read from the input, end of archive included; for a `blob:` input the object's bytes |
| `dry_run` | |
| `state` | `running`, `done` or `failed` |
| `counts` | JSON, per target and outcome |
| `started_at`, `finished_at`, `actor` | |

- **CLI.** `kista admin bundle list <tenant>` and `kista admin bundle show <tenant> <id>`.
- **Console.** The console shows a release's `bundle` and `imported_at` in its provenance, and a
  bundle-mode upstream's last import. Phase 1a adds no console page for imports.
- **Freshness mark.** The last imported `created_at` per target is
  `upstreams.bundle_created_at`.

### Visibility

A mirror's releases from a credentialed source are private unless the credential sets
`allow_public` (spec 0009 phase 3): a paid subscription's extensions are not the tenant's to
publish. Bundle mode keeps the rule. The signed manifest's `authenticated` flag says whether the
builder needed a token for a source, and the site's configuration plays the credential's part:

```yaml
upstreams:
  bundle_sources:                          # optional; authenticated sources not listed are private
    - prefixes: ["https://kista-public.example.org/"]
      tenants: [acme]                      # or "*"
      allow_public: true
```

- **Marking.** A release from an authenticated source records in its provenance the pseudo-credential
  `credential: "bundle:<normalised prefix>"`.
- **Checks reused.** Spec 0009's three checks already read the provenance's `credential`:
  - making a release public;
  - promoting it as public;
  - a promotion carrying the credential over.

  For a `bundle:` value they consult `bundle_sources` as configured at that moment: an entry whose
  `tenants` include the release's tenant, whose prefixes cover the source (as credentials' prefixes
  do), and which sets `allow_public`. Promotions keep the value, so a promoted copy stays guarded.
- **At intake.** A release from an authenticated source is private, whatever the upstream's
  `visibility` says, unless such an entry allows it. A bundle-mode upstream may itself be public:
  that is its visibility for unauthenticated sources, as a mirror's is.
- **Passthrough.** A passthrough channel is public by nature and takes `duckdb-core` only, which is
  never authenticated: the rule does not reach it.
- **Not DRM.** The flag is the builder's word, and the bundle key's holder could leave it out.
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
- **The builder.**
  - It fetches the source's `/attestation-keys` and keeps the file as it came, like a
    `.well-known` file.
  - It posts the bodies to `/feed` with their cursors, adds the bodies of files it bundles for an
    allowed source with no cursor, and follows `more`.
  - It fetches each statement's document from the fixed path, `/feed/documents/<issuer id>`.
  - It checks size, content hash (spec 0011's `ContentHash`) and signature against a pinned key.
  - It writes, after the files:
    - `feeds/<n>/attestation-keys.json`;
    - the answers as `feeds/<n>/batch-<k>.json` (each at most 1 MiB);
    - each document as `feeds/<n>/documents/<issuer id>` (each within spec 0011's limit), after its
      batch.

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
page to the docs, under Deployment. Its commands are run in CI against a local registry with no
route out, so the page cannot drift:

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
  - Plan, build, carry, import, with `-dry-run` first.
  - Sizing (below).
  - A slot holds one body forever, so a wrong body is a permanent `conflict` for that version.

**Sizing.** The CLI import needs about twice `blob.max_body` of temporary disk.
- In Kubernetes, `kubectl exec` runs it in the serving pod, whose spool emptyDir is sized for the
  server's own ingests (spec 0013). The import gets its own space through `-tmp-dir`, on a volume
  with room. The chart gains `spool.importHeadroom` (default `0`), which adds to the emptyDir's
  limit and its ephemeral-storage request.
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
  - Client side: `kista bundle keygen|keys|build|inspect`.
  - Site side: `kista admin bundle plan|import|list|show`.
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
  - `import` (the record's id), `manifest`, `created_at`, `builder`, `bundle_key`, `input_sha256`,
    `dry_run`, `state`;
  - `targets`: the upstream names, with counts only up to `MaxData`, since the full counts are on
    the record.

  Its subject is the tenant. The actor is the event's actor.
- **`upstream.run`** gains `bundle` (the manifest's id).
- **`upstream.release`** gains `bundle`, so the audit alone answers "which bundle brought this
  release, and who imported it".
- **`upstream.rejected`** is made per rejected cell, as in a run.

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
- **`upstreams.bundle_created_at`**, null.
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

The config check refuses:
- malformed fingerprints, and more than 10 of them;
- `bundle_sources` entries without `prefixes` or `tenants`;
- prefixes that are not https.

### Package layout

```text
internal/bundle          the plan and the manifest (types, validation, canonical order),
                         signing and verifying, the tar reader and writer
internal/bundle/build    the builder: keys, fetches, the client-side checks, tokens, the summary
internal/upstream        + bundle mode (no sync, no credential, no egress port check, passthrough);
                         the import: targets, leases, freshness, per-cell intake, current releases
internal/release         + Ingest split: the file's checks once, the cell's checks and insert per
                         cell; MayPublish for "bundle:" pseudo-credentials
internal/blob            + the imports/ prefix, read-only to kista, outside the collector
internal/attachment      + bundle-mode feeds (1b)
cmd/kista                + bundle keygen|keys|build|inspect, admin bundle plan|import|list|show
web/console              + bundle mode on the upstream forms; provenance fields
```

### Docs

These pages are updated in the same PR:
- `website/docs/upstreams.md`: bundle mode;
- `configuration.md`: the settings above;
- `administration/commands.md`;
- `api.md`: the mode, and `409` on `sync`;
- `observability.md`: the events;
- `security.md`: the trust model above;
- `deployment/kubernetes.md`: the import's disk, `importHeadroom`, and import as a Job;
- the new `deployment/air-gapped.md`, with its sidebar entry.

### Changes to earlier specs

- **0009**:
  - upstreams gain mode `bundle`;
  - the passthrough guard admits it;
  - `checkPrefix` skips the egress port check for it;
  - credentials are refused on it;
  - provenance gains `bundle` and `imported_at`;
  - `bundle:` pseudo-credentials.
- **0005 / 0016**: the `imports/` prefix, outside the collector.
- **0010**: `bundle.import`, and `bundle` on `upstream.run` and `upstream.release`.
- **0011 (1b)**: feeds gain mode `bundle`.
- **0013**: the chart's `spool.importHeadroom`.
- **0001**: the follow-up for bundles points here.

## Security

- **Trust comes from the site.** The site pins the bundle keys and the source keys; DuckDB's keys
  are compiled in. The manifest, the `.well-known` file, the tar and its order are input to be
  checked.
- **Authentic and fresh.**
  - The source's signature proves a file authentic.
  - The signed manifest proves it is what the source served for that cell when the bundle was
    built.
  - Without the manifest's signature, a carrier could put any build the source ever signed into a
    cell that never held its version. Spec 0009 releases such a version and makes it current: a
    downgrade to a known-vulnerable build.
  - With it, the carrier can only withhold the bundle.
  - **Replay.** Per target, an import of a bundle older than the last one imported is refused, so
    replaying an old bundle is refused too.
  - **Additional defence.** A site that wants it pins exact versions in the entries' `versions`.
- **What the bundle key's holder can do.** They choose what the site receives, within the site's
  allowlists, filters and the sources' signatures: a stale but authentic build, a cell marked
  `rejected` or `missing`, an `authenticated` flag left out. The holder is the operator's own build
  machine, and the key is a separate secret from anything on the site.
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
- **Tokens.**
  - The builder sends a token only to a prefix it covers, over https, never on a redirect, never to
    DuckDB's repositories.
  - The builder refuses private and loopback addresses unless told otherwise, so a hand-written plan
    cannot make it probe an internal host with a token.
  - No token reaches the bundle, a log or an error.
  - The plan and the bundle hold no secret. Bundle-mode upstreams and feeds hold no credential.
- **Paid extensions.** Files from an authenticated source are private unless the site's
  `bundle_sources` allow otherwise for that tenant. The marker survives promotion. It is an
  honest-user control, like licensing.
- **Who imports.** Only a server administrator, through `kista admin` (1a) or the upload route (2).
  - A tenant administrator configures the bundle-mode upstreams, their allowlists and pinned keys,
    which bound what any import may release into the tenant.
  - The manifest names its tenant, and importing it into another tenant needs `-any-tenant`.
- **Isolation.** An import fills one tenant's upstreams. Bodies are shared within a storage domain
  as spec 0005 shares them.
- **Untrusted text.** Builder reasons are bounded, control characters are stripped, and they are
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
- **Builder**, against a fake repository and a fake core source (a URL override and test keys):
  - verified files;
  - a wrong key, another platform, another DuckDB version, a version filter;
  - `304` (conditional and not), `404`;
  - a body equal to `body_hash`;
  - redirects and private addresses refused;
  - the token sent only to the prefix it covers, never to core, and absent from the bundle and the
    output;
  - `401`;
  - a source whose keys file lists no pinned key;
  - the caps and a gzip bomb;
  - a hand-written plan with a bad grammar;
  - an unpinned bundle key;
  - an interrupted build leaving no output.
- **Import**, from bundles the tests write:
  - **Valid imports and repeats.** A valid import. A repeat releases nothing. An older bundle is
    `stale`. The same id resumes.
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
  - **Visibility.** Authenticated sources and `bundle_sources` across tenants; promotion keeps the
    marker; the refusals.
  - **Changes during an import.** A target paused or removed meanwhile; a tenant deleted meanwhile;
    a matrix over its cap.
  - **Dry run** records only `last_run` and its event.
  - **Concurrency.** Two imports at once: one is `busy`.
  - **Input.** Standard input, a `blob:` input, and idle and slow input.
  - **Passthrough.** A passthrough channel fed by a core bundle.
- **Upstreams.**
  - Bundle mode is never scheduled.
  - `sync` answers `409`.
  - A credential is refused at creation and on the route.
  - No egress port check.
- **Store.**
  - Migration 0020 on all three dialects.
  - SQLite's rebuild keeps every child table's rows.
  - `bundle_imports`.
- **e2e** (pinned DuckDB):
  1. A second kista in the harness serves a signed channel.
  2. `kista bundle build` makes a bundle from it.
  3. A first kista whose egress refuses the second imports the bundle into a bundle-mode upstream.
  4. DuckDB runs `INSTALL … FROM` and `LOAD` against the first kista.
  5. A new version at the source, a second bundle; the new version is current.
- **Docs.** The offline install's commands run in CI against a registry with no route out.
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
- **A server-side export (`kista admin bundle export <channel>`).**
  - It needs a kista on the connected side and administrator access to it, which an Enterest
    subscriber does not have.
  - It would ship files re-signed with the exporting channel's key, so the site would trust one key
    more.
  - The builder fetches from any DuckDB repository with what a DuckDB client has. A connected kista
    remains a valid source through its channels.
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
