# Spec 0021: Licence keys in the build, and licences in bundles

- **Status**: accepted; phase 1 (the key in the build: the SDK, the reader, and kista) implemented
- **Date**: 2026-10-18
- **Author**: vgsml, Claude

## Summary

Spec 0012 keeps two lists of trusted licence keys: the issuer's keys compiled into the SDK, and the
keys in kista's configuration (`licenses.keys`). This spec moves the licence keys into each build:

- **The build carries its keys.** The extension's build gives the SDK its licence keys. The SDK
  writes them into the binary as a **key block** that kista can read.
- **kista records keys from trusted sources only.** It reads the block when a build arrives, and
  records the keys, with their source, only when the build came from:
  - a publisher that the server administrator marked;
  - or a `repository` upstream (by fetch or by bundle).
- **kista checks licences against those keys.** It verifies a licence against the keys of the
  name's live builds from the right source, and serves it in a channel only when a live build in
  that channel declares its key.
- **No key configuration.** `licenses.keys` is removed, and licensing is on unless
  `licenses.enabled: false`.
- **Licences in bundles.** A bundle can carry licences, when the site asks for them and the exporter
  agrees, so an air-gapped site receives them with the extensions.

## Problem

- **One key for every extension.** The SDK compiles the issuer's keys in, so one key signs the
  licences of every extension built with it. A leaked key or a rotation touches all of them. An
  issuer may want a key per extension instead: Enterest does, letting each developer choose the key
  of each extension. Two fixed lists, one in the SDK and one in kista's configuration, cannot
  express that.
- **Configuration on every kista.** Every kista that holds licences has to be configured with the
  issuer's keys. With a key per extension, that list would never be complete.
- **Air-gapped sites.** A site receives extensions in bundles (spec 0014), but receives licences
  only by an administrator's import or by a file placed by hand (spec 0012's follow-up).

## Design

### The key block

The extension's build names its licence keys for its target:

```cmake
add_subdirectory(path/to/sdk/cpp kista_sdk)
kista_sdk_license_keys(myext_loadable_extension ${CMAKE_CURRENT_SOURCE_DIR}/license-key.pem)
kista_sdk_license_keys(myext_extension          ${CMAKE_CURRENT_SOURCE_DIR}/license-key.pem)
```

- **Per target.** `kista_sdk_license_keys(<target> <pem>…)` takes one to four PEM files. It
  generates a source file with the target's key block and adds it to the target. The SDK library
  holds no keys. It refers to the key block's symbol, `kista_sdk_license_keys`, so an extension that
  links the SDK without calling the function fails to link, with an error naming the function.
  - **Hidden.** The symbol, and the SDK, have hidden visibility. A hidden reference is resolved when
    the module links, so even a shared library that may leave symbols undefined (Linux) fails to
    link, and an extension never binds to another loaded module's block. The exception is macOS
    with `-undefined dynamic_lookup` (DuckDB's non-static extension builds), where an undefined
    symbol carries no visibility: such a build fails to load, since no module exports a block.
  - The configure step checks that each file holds exactly one `PUBLIC KEY` PEM block, in the
    canonical form `openssl pkey -pubout` writes (64-column base64; CRLF line ends are normalised),
    and no key given twice.
  - The full check (RSA-2048, `e = 65537`) is the SDK's at run time and kista's at intake.
  - Several extensions can be built in one tree, each with its own keys. Two licensed extensions
    cannot be linked statically into one DuckDB: their blocks share the symbol.
- **No issuer keys in the SDK.** The SDK has no compiled-in issuer keys (`kIssuerKeys` in `keys.cpp`
  goes); each build brings its own.
- **The test key.** `kista_sdk_testkey` is the key block of the licence vectors' test key, as an
  interface library to link instead of calling the function (each target that links it compiles
  its own copy of the block). `kista_demo` links it, so the e2e cases carry a real block.
  `TrustsTestKey()` stays.
- **Layout.** The block is one constant byte array, defined once, with external linkage and marked
  used:

  ```text
  "KISTA-LICENSE-KEYS/1\0"    21 bytes, the magic
  count                       1 byte, 1 to 4
  per key:  length            2 bytes, big-endian, 1 to 2,048
            the key           one SubjectPublicKeyInfo in PEM ("PUBLIC KEY"), `length` bytes
  "\0KISTA-END\0"             11 bytes
  ```

  The SDK reads its keys from the array through a pointer the compiler cannot fold. The magic
  appears nowhere else in the SDK: not as a literal, and not in a self-check.
- **`kista ext inspect <file>`** prints the fingerprints of a binary's licence keys, or why it has
  none.

**Reading the block.** kista reads the body: the file without its 256-byte signature, where the
block lies before DuckDB's metadata.
- It scans for every occurrence of the magic, streaming, during the pass that hashes the body.
  More than 16 occurrences is malformed.
- **Parsing is strict:**
  - the count is 1 to 4, and each length 1 to 2,048;
  - each key is exactly one `PUBLIC KEY` PEM block in the canonical encoding (no headers, 64-column
    base64, LF line ends), with no other bytes;
  - its DER re-encodes identically, and it is RSA-2048 with `e = 65537`;
  - no key appears twice;
  - the end marker follows the last key.
- **The outcome:**
  - one block that parses gives the build's licence keys;
  - several blocks that are byte-identical count as one (a debug build can hold a copy);
  - blocks that differ, or a magic whose block does not parse, give **no keys**, with the reason
    "licence keys: malformed";
  - no magic at all gives no keys: an unlicensed extension, or one built before this spec.

A string in a binary proves nothing by itself, since anyone can write the magic into a file. The
keys count only because of the source the build came from (below).

### Trusted sources

A build is one row per (tenant, name, body), shared by every channel of the tenant. It records its
licence keys, each with its **source**, only when a trusted intake carries its body:

| Source | When | Why it is trusted |
| --- | --- | --- |
| a marked publisher (its id) | its publication carries the body, including a publication of a body already released (kista reads the stored body) | the server administrator decided that this publisher's builds speak for their licences (an issuer's build pipeline, for example) |
| a `repository` upstream (its id) | a fetch downloads the body: a mirror run or a pull-through | the build is the upstream's, verified with the keys the tenant pinned for it |
| a bundle-mode `repository` upstream (its id) | a bundle import brings the body | the same, through a bundle verified with a pinned bundle key |

- **No keys from other paths.** These record nothing:
  - DuckDB's core and community upstreams: anyone can publish a community extension, and their keys
    say nothing about licences;
  - an unmarked publisher, a principal, or `release add` (the CLI or `POST …/releases`);
  - an upstream run that gets a `304`, because it does not read the body. A run that downloads a
    body its slot already holds (`unchanged`) does read it, and records its keys.
- **A source that is gone.** A key counts only while its source exists: an intake that raced its
  source's removal records nothing, or a row nothing counts.
- **Recording later.** When a trusted intake carries a body whose build has no keys from that
  source, the keys are recorded. The release versions of the channels holding it are bumped, so
  served snapshots pick them up.
- **Removing a source.** Removing a publisher or an upstream drops the keys it brought, from every
  build. A purged build's keys go with it.

**Marking a publisher** is a server administrator's act.
- **Routes.** `POST /api/v1/tenants/{t}/publishers/{name}/license-keys` marks it, and `DELETE`
  unmarks it. Each records a new `publisher.change` event. In the CLI: `kista admin publisher
  license-keys <tenant> <name> on|off`. `GET …/publishers` and `publisher list` show the mark.
- **While marked, the server administrator controls it.** Adding or removing its API keys, its
  GitHub bindings and its grants, and changing the members of an extension group its grants name,
  is a server administrator's act, refused to tenant administrators (`404`, as for a route they
  cannot reach). A publisher is marked only while it holds no API key and no GitHub binding (`400`
  otherwise): credentials a tenant administrator made before the mark would outlive it. A tenant administrator can still remove the publisher,
  which drops its keys. So a tenant administrator cannot point a marked publisher at builds of their
  own.
- **A malformed block is refused.** A marked publisher's publication whose body has a malformed
  block is refused (`400`, rule `license_keys`).
- **The publisher itself.** A credential can match several publishers. A publication records keys
  for each matched marked publisher that holds `publish` on the name by its own grants.
- **Unmarking** keeps the keys already recorded. Removing the publisher drops them.

### Verifying licences

A build is **live** in a channel when it has a release there that is `active` or `deprecated`, whose
body is not blocked, and the channel is a signed channel, not a passthrough channel. Licence keys
refused by the server (`licenses.refuse_keys`, below) never count.

| Where a licence comes from | It is accepted when its key is |
| --- | --- |
| an administrator's import (API, CLI) | a key of a live build of the name, from any trusted source, anywhere in the tenant |
| the relay from upstream U | a key U brought, on a live build of the name that U supplied |
| a bundle through bundle-mode upstream U | the same, for U |

- **Rule order.** A relayed or bundled licence is checked for `not_supplied` first (spec 0012), then
  for its key (`kid_untrusted`). A licence refused for its key that this upstream relayed before
  stays stored (not served while no live build declares its key), rather than being revoked. A name with no keys refuses every licence with `kid_untrusted`,
  saying that no live build of the name declares a licence key. The keys are read inside the
  tenant's transaction, after the supply check.
- **Replacing.** Spec 0012's replacement rules hold, with one more condition. A licence from one
  source replaces a stored licence from another source only when both are signed with the same key,
  or the stored one is past its grace, or its key counts nowhere (refused, or declared by no live
  build of the name). An administrator's import with `If-Match` still replaces any
  licence. So an upstream added in another channel cannot displace a working licence with one signed
  by its own key.
- **Serving.** `GET <prefix>/-/licenses/<name>`, and the list, serve a licence in channel C only when
  a build live in C declares its key.
  - The channel's served snapshot carries each build's key ids, so this costs no query.
  - A licence whose key has no live build in the channel is not served there. It stays stored until
    its grace ends or a newer licence replaces it.
  - A downstream kista's relay treats a licence that is not served as removed, and gets it back when
    it is served again.
- **Refusing a key.** `licenses.refuse_keys` (server configuration, file-only) lists key ids that
  count nowhere: on import, relay, bundle import or serving. It is for a leaked key while builds that
  declare it are still live. Blocking a body (spec 0008) also takes its keys out, because a blocked
  body is not live.
- **Licensing on and off.** `licenses.enabled` (file-only, default `true`):
  - **On.** The licence routes exist. A name takes a licence only once a live build declares a key.
  - **Off.** The licence routes answer as for a missing path, and imports and the relay are refused,
    as when spec 0012 had no keys configured.
- **The old setting.**
  - `licenses.keys` is accepted and ignored, with a start-up warning, in the release that ships this
    phase, and refused in the release after.
  - The routes it fed are removed: `GET /api/v1/licenses/keys` and `kista admin license keys`.
- **Offline checks.** `kista admin license verify` takes the key to check against (`-key`).
- **What a tenant administrator sees.** `GET /api/v1/tenants/{t}/license-keys[/{name}]` answers
  the keys of live builds, without those the server refuses:
  `{"names": [{"name", "keys": [{"kid", "sources": [{"kind": "publisher"|"upstream", "name"}],
  "builds": n}]}]}`. The CLI's form is `kista admin license keys <tenant> [<name>]`, and the
  console's Licences screen shows it.

### Rotation, honestly

- **One licence per name.** kista keeps one licence per (tenant, name). While builds with an old key
  and builds with a new key are both live, the stored licence suits only one of them. An issuer
  rotates with a new version and a switch date, after which it signs with the new key. Binaries that
  already hold a licence keep it until its `exp` plus grace (spec 0012).
- **The old key outlives the switch.** It stays a live key for as long as an old build is live in
  some channel. To end it sooner, an administrator yanks or blocks the old builds, or the server
  administrator refuses the key.

### Licences in bundles (spec 0014)

- **The site asks.** `kista admin bundle plan -licenses` writes `"licenses": true` into the plan,
  optionally with `"license_holder": "<sub>"`, the holder the site expects. A plan without the field
  asks for none.
- **The exporter agrees.**
  - `kista admin bundle export -licenses` carries licences only when the plan asks. Without the flag
    it carries none, and says so.
  - The export prints each licence's name and holder.
  - It carries, per `repository` source, the licences the exporting tenant holds for names whose
    files are in this bundle (`fetched` or `unchanged` cells):
    - only licences in force;
    - only of the expected holder, when the plan names one;
    - at most 1,000 per source;
    - with private releases' names only under `-private`, as for their files.
- **The manifest.** Each source gets `licenses: [{"name", "token"}]`, with the token as stored (at
  most 16 KiB). The manifest's signature covers them.
- **Compatibility.** A bundle without licences is the same `kista-bundle/v1` as today. A plan that
  asks for licences, and a bundle that carries them, need a kista of this version on both sides. An
  older one refuses them: plans and manifests are decoded strictly.
- **The import.** It applies each licence after the files, once per licence. Of the bundle-mode
  upstreams the source fills, the licence goes to the first one that supplies the name.
  - The licence is verified against that upstream's keys, and must be of the expected holder if the
    plan named one.
  - Spec 0012's relay rules apply, with the replacement condition above.
  - A bundle's licence never replaces an administrator's import.
  - It is stored with `source` `upstream` and that upstream's id. A dry run stores nothing and counts.
- **No revocation.** A bundle may be partial, so a licence it leaves out is not removed. A bundle
  licence ends at its `exp` plus grace, through the expiry pass. A bundle-mode upstream has no relay.
- **Records.** The import record's counts and the `bundle.import` event gain licences stored,
  unchanged and refused, each refusal with its reason.

### Changes to spec 0012

- **Trust.** `licenses.keys`, the SDK's compiled-in keys, and "Turning licensing on" are replaced by
  the above.
- **Follow-ups.** "Enterest's licence keys in `keys.cpp`" is dropped. "Licences carried in a bundle"
  is this spec.

### Data model

Migration 0027, on all three dialects. Its schema change is additive, but it raises `min_reader` to
27: an older replica would let a tenant administrator change a marked publisher's credentials and
would verify licences against `licenses.keys`.

```text
build_license_keys   tenant_id, build_id (cascade), kid, pem, source_kind ('publisher' | 'upstream'),
                     source_id, recorded_at; primary key (build_id, kid, source_kind, source_id)
publishers           license_keys boolean not null default false
```

`bundle_imports.counts` is already JSON; the licence counts go there. `publisher.change` is added
to the audit's event kinds.

### Upgrading

- **Existing builds carry no keys.** Builds published or mirrored before this spec have no recorded
  keys, and builds made with an earlier SDK have no key block. So once `licenses.keys` is gone, a
  stored licence is accepted and served only after its extension is rebuilt with this SDK and
  published or mirrored again.
- **Who is affected.** No issuer publishes keys yet, so this concerns test deployments only.
- **Licensing on by default.** A deployment that ran with no keys, and so with licensing off, now has
  the licence routes and relays licences from its credentialed upstreams. `licenses.enabled: false`
  keeps it off.
- **The notes.** `administration/upgrading.md` lists all of this, and the migration.

## Security

- **Who decides a name's keys.** Only trusted sources decide:
  - a publisher the server administrator marked, and whose credentials and grants only the server
    administrator can change;
  - a `repository` upstream, for the names it supplies, verified with its pinned keys;
  - a bundle verified with a pinned bundle key.

  A tenant administrator, a channel's publisher or a principal can publish a build under a name, but
  cannot add a key through it.
- **Keys stay with their source.** A licence from an upstream is checked only against the keys that
  upstream brought. It cannot replace a working licence from another source unless it is signed with
  the same key. A licence is served only in the channels whose live builds declare its key. So an
  upstream a tenant administrator adds in one channel cannot displace or break another channel's
  licence.
- **The SDK is the check that matters.** A genuine build trusts only its own key block, so a licence
  signed with any other key fails in that binary, whatever kista stored. kista's checks keep wrong
  licences out of its store and its relays.
- **Forged blocks.** A block in a binary from an untrusted path is ignored. An issuer can compare the
  keys read back from its builds with the keys it meant to put in; Enterest does (its spec 0003).
- **Parsing.** The binary is untrusted input. The scan is bounded by the body's size, the parse by
  the block's limits, and every key is checked before use.
- **Refusing a key.** `licenses.refuse_keys` and body blocks take a leaked key out at once, while old
  builds that declare it stay installed elsewhere.
- **Bundles.**
  - **Consent.** Licences leave a tenant only when the site asks and the exporter agrees with a
    flag. The exporter sees each holder.
  - **Scope.** Licences go only for names whose files are in the same bundle.
  - **The holder.** The site can pin the holder it expects.
  - **Precedence.** A bundle's licence never replaces an administrator's import.
  - **Tampering.** The manifest's signature covers the licences, so a carrier cannot change them.
- **The honest-user line** is unchanged. A patched binary with a key of its own defeats its own
  check, as before.

## Testing

- **The key block:**
  - the generator and the reader across every limit;
  - no block;
  - one block, and two identical blocks;
  - two differing blocks;
  - a truncated block, a block with a non-RSA-2048 key, a key with trailing bytes, and a duplicate
    key;
  - a block split across the scan's chunk boundary;
  - `kista ext inspect`.
- **The SDK:**
  - linking without `kista_sdk_license_keys` fails with the named error;
  - two extensions in one tree with different keys;
  - `kista_sdk_testkey` carries the test key's block;
  - the vectors pass.
- **Sources** (store and service tests, on all three dialects):
  - a marked publisher's publication records keys, and an unmarked one's does not;
  - a body published untrusted, then by the marked publisher, records keys;
  - a `repository` mirror run and a pull-through record keys, and core, community and `unchanged`
    runs do not;
  - promotion keeps the keys;
  - removing a source drops its keys;
  - a tenant administrator's change to a marked publisher's credentials is refused;
  - a malformed block from a marked publisher is refused.
- **Verification:**
  - an import checks against every source's keys;
  - the relay checks only against its upstream's keys;
  - the replacement condition: an upstream in another channel with its own key cannot displace a
    licence;
  - serving per channel, with a yanked or blocked build, a passthrough channel, and
    `licenses.refuse_keys`;
  - `licenses.enabled: false`;
  - `licenses.keys` warned about.
- **Bundles:**
  - the plan asks or not, and the exporter agrees or not;
  - names without files carry no licences;
  - the expected holder;
  - one target per licence;
  - no revocation;
  - an import's licence not replaced;
  - a dry run;
  - an older reader refusing a bundle with licences.
- **e2e:**
  - `kista_demo`, built with the test key's block and published by a marked publisher, takes a
    licence with no key configured;
  - published by an unmarked principal, it takes none;
  - mirrored by a second kista, its licence is relayed;
  - carried in a bundle to a site, the pinned DuckDB loads it there with the bundle's licence.

## Alternatives considered

- **A key field in the publication.** Only the publication path would carry it. Mirrors and bundles
  would need a transport of their own, and the field could disagree with the binary.
- **A certificate chain from one issuer root key.** It needs one root key everywhere again, compiled
  into the SDK and configured in kista.
- **Keeping `licenses.keys` beside the key block.** Two sources of trust that can disagree, and
  configuration that never ends. Its one use, refusing a key, is `licenses.refuse_keys`.
- **One licence per (name, key)**, so that old and new builds both get a licence during a rotation.
  A rotation is a new version and a switch date; this can come later if issuers need it.
- **The key in DuckDB's metadata footer.** Its fields are 32 bytes, too small for a key, and DuckDB
  writes them.

## Delivery

- **Phase 1, the key in the build**, in two pull requests:
  1. **The SDK and the reader:**
     - `kista_sdk_license_keys` and the key block;
     - `kista_sdk_testkey` as a block;
     - the reader and `kista ext inspect`;
     - the SDK's README and `development.md`.
  2. **kista:**
     - migration 0027, sources and marked publishers;
     - verification, replacement and serving per channel;
     - `licenses.enabled`, `licenses.refuse_keys`, and removing `licenses.keys`;
     - the console's Licences screen (the keys, and the import note);
     - the docs: `licensing.md` (its "Turning licensing on" and trust sections rewritten),
       `publishing.md`, `configuration.md`, `api.md`, `console.md`, `observability.md`
       (`publisher.change`), `administration/commands.md` and `administration/upgrading.md`;
     - the e2e cases, the Go ones and the console's `e2e/run.sh`.
- **Phase 2, licences in bundles:**
  - the plan, the export and the import;
  - the records and the `bundle.import` event;
  - the docs: `deployment/air-gapped.md`, `upstreams.md`, `administration/commands.md` and
    `observability.md`;
  - the e2e case through a bundle.

The Helm chart and the cloud recipes do not set `licenses`, so nothing there changes.

## Follow-ups

- An expected licence holder per tenant (spec 0012's follow-up), as its own spec.
- The SDK for C API extensions carries the same key block.
- One licence per (name, key), if rotations need it.
