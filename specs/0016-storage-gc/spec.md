# Spec 0016: Storage garbage collection

- **Status**: accepted (2026-10-10); phase 1 implemented, 2 next
- **Date**: 2026-10-10
- **Author**: vgsml, Claude

## Summary

kista never deletes an extension body today: a body stays in its storage domain for good once
committed, and so do the objects a crash or a refusal leaves behind. This spec adds a **garbage
collector**: a pass per storage domain, on one replica at a time, that deletes what nothing serves
and nothing can serve again, without racing an intake or a reader into serving a missing body.
Spec 0001 sketched it ("mark-and-sweep over the Builds that reference a body, with a grace period")
and spec 0005 deferred it ("`tmp/` and orphaned streams, the race with a dedup hit").

Delivery, each phase a pull request:

- **1. Orphans**: Builds no release references, bodies no Build references, streams no body
  references, the `fs` store's `tmp/` leftovers; claims and tombstones that make a stream's
  deletion safe against a commit; intake deadlines; `kista admin blob gc` (a dry run by default)
  and the scheduled pass, off until the operator turns it on.
- **2. Purging releases**: a tenant administrator deletes a yanked release explicitly (the owner's
  choice, 2026-10-09: no automatic retention); its slot stays taken; its body becomes garbage.
  Tenant deletion follows the same path later.

## Problem

- **What leaks today** (no code deletes a body, a Build or a release):
  - an **object without a row**: a crash or a database failure between a stream's upload and its
    `blobs` row (spec 0005's commit uploads, then writes the row);
  - **`tmp/` files** of the `fs` store: a crash inside `Put` before the rename;
  - a **stream no row references**: a later commit of the same body with another compressor's
    output replaces the row's `stream_hash` (spec 0005), the old object stays;
  - a **body no Build references**: committed, then the Build insert failed;
  - a **Build no release references**: committed and inserted, then the release refused (a slot
    taken meanwhile, a block, a shadow, a signing failure). An upstream failing the same cell on
    every run leaves one per new upstream build.
- **What grows by design**: yanked releases are kept forever, and with them their bodies (phase 2).
- **Why it is not simple**: a commit uploads before it writes the row and runs outside any lock,
  and re-uploads an existing object (spec 0005's "always upload", which repairs and hides dedup);
  intake finds and reuses an existing Build; serving caches blob records without expiry and reads a
  stream lazily, in ranges, maybe minutes after resolving it. Deleting at the wrong moment turns
  into a corrupt mark and `503`s, or a release insert failing on its Build.

## Design

### What is live

In a storage domain (its tenants by `storage_domain`):

- a **Build is live** when a release references it, or it was used within the grace period
  (`builds.used_at`, set at insert and refreshed by every intake that finds it; migration 0013 sets
  it to `created_at` on existing Builds, and the pass does the same for the few an older replica
  still running inserts without it): an intake inserts or finds its Build before its
  release, outside the channel's lock;
- a **body row is live** when a live Build references its body, or it was committed within the
  grace (`blobs.committed_at`, refreshed by every commit after its upload);
- a **stream is live** when a body row of the domain references it, or a commit claims it (below).

**Intake deadlines.** Every intake (an API publication, `kista admin release add`, an upstream
cell, a promotion) runs from its commit to its release insert under one deadline,
`intake_deadline` (one hour): the commit's claim, the upload, the Build, signing and the channel's
lock. An intake past it fails and leaves garbage. `gc.grace` (default 24 hours) is refused below
four times the deadline: what an intake touched stays live for its whole run, with room for clock
skew between replicas and the object store.

Blocks, upstream cells and events name body hashes without keeping bodies: a block bans a hash
whether or not it is stored, a cell's hash is its last fetch's, an event is history.

### Streams: claims and tombstones

A stream's object is deleted only while no row references it and no commit is writing it, and only
a grace after it became unreferenced (readers). Two tables: `blob_claims(domain, stream_hash,
claim_id, expires_at)` (a commit's claim, one row per commit) and `blob_tombstones(domain, stream_hash,
since, state, deleting_at)` with `state` `marked` or `deleting`. Every change to them for a stream
holds the lock `kista/blob/<domain>/<stream_hash>` (a store lock: an advisory lock on PostgreSQL
and SQL Server; on SQLite every transaction is exclusive); a transaction needing two streams' locks
takes them in order, before any write. No network call runs inside these transactions. Times are
the replicas' clocks (`Tx.Now`); the grace absorbs skew between them and the object store.

- **A commit** (spec 0005's precompression, then):
  1. in a transaction holding its stream's lock: if the stream's tombstone is `deleting`, it waits
     and tries again (a collector is deleting the object), within its intake's deadline, then fails
     as busy; otherwise it removes the tombstone and inserts its claim (`expires_at` its
     intake's deadline); a commit giving up removes its claim under the lock;
  2. uploads the stream (spec 0005's "always upload");
  3. in a transaction holding its stream's lock and, when the row holds another stream (another
     compressor's output, spec 0005's replace), that stream's lock too (the row read first, both
     locks taken in order, the row read again under them): checks its claim is still there and
     unexpired, otherwise goes back to step 1 (claim, upload again); inserts or refreshes the body
     row (an update that finds no row inserts: the collector may have removed it), clears
     `corrupt_at` and sets `committed_at` as today; removes its claim and any tombstone of its
     stream; marks a replaced stream with `since` now (a reader may hold it from now on).
- **Marking** by the collector writes a tombstone only where there is none, under the stream's
  lock, after checking there that no row references the stream and no claim is live: for a stream
  found in the store's listing without a row, and for the stream of a body row it deletes. Only a
  commit's replace moves an existing tombstone's `since` forward.
- **Deleting**, by the collector holding the domain's lease: for a `marked` tombstone older than
  the grace, in a transaction holding the lock, the checks are repeated (no row, no live claim) and
  the tombstone set to `deleting` with `deleting_at` now, the delete's **stamp**. Then, outside any
  transaction: the lease is renewed (a renewal extends a lease its holder still holds, never one
  that expired, even if no other replica took it), the tombstone is read again and must still carry
  the stamp (no other collector took it over), and the object is deleted (a 30-second timeout);
  then, under the lock, the tombstone carrying the stamp is removed.
  - A delete **the store refuses** (no permission, an object lock): the tombstone carrying the
    stamp goes back to `marked` (its `since` kept), so commits of the stream go on, and the pass
    stops with the error; the next pass tries again.
  - A delete **whose outcome is unknown** (a timeout, a cancelled pass) leaves the tombstone
    `deleting`: commits of the stream wait until a collector finishes it.
  - A `deleting` tombstone is taken over (re-checked under the lock, re-stamped, deleted again: a
    delete is idempotent) only when its stamp is older than thirty minutes, three leases and far
    beyond a delete's timeout: a collector paused past that finds its stamp gone and stops. Only a
    collector holding the lease ever resolves a `deleting` tombstone; a commit never takes one
    over.
- **A resolver** runs every five minutes per domain on the replica holding the domain's lease
  (taking it if free), whatever `gc.interval`, after the marker check below: it finishes stale
  `deleting` tombstones, so a commit waits at most about half an hour after a collector's crash,
  never a day.
- A due tombstone whose stream has a row or a live claim is removed instead of deleted. Expired
  claims are removed without the stream's lock: every check of a claim is of a live one, so an
  expired claim decides nothing.

While a row references a stream, its object is present: a commit's claim precedes its upload and is
checked when its row is written; the collector deletes only while no claim is live and no row
exists, under the lock; a commit meeting a `deleting` tombstone waits until the delete is done and
the tombstone gone, then uploads. The one residual case is a delete request that reaches the store
more than thirty minutes after its collector checked its stamp (a process suspended between the
check and the request, or a request held in the network): the object store offers no conditional
delete to fence it. A reader then finds the object missing and marks the row corrupt
(spec 0005); the next commit of the body repairs it, and `kista admin blob verify` (a follow-up)
finds such rows.

### Builds and bodies

- **Builds**: a Build without a release and unused for the grace is deleted, one at a time, the
  check repeated in the deleting statement (`used_at` older than the cutoff and no release); a
  delete failing on the foreign key (a release inserted meanwhile), or on a lock it could not get,
  skips that Build.
- **`FindOrInsertBuild`** refreshes `used_at`; a refresh that finds no row (collected meanwhile)
  inserts the Build again.
- **A release insert whose Build was deleted meanwhile** fails on the `build_id` foreign key with a
  distinct error (not `ErrInvalid`: an upstream cell records it as `failed`, never `rejected`); the
  intake starts again from its commit (its spooled file is still open: the body row and the stream
  are written again), finds or inserts its Build, and inserts the release; once. A promotion
  re-reads its source release, and fails if it is gone.
- **Body rows**: a row is deleted only when no Build row at all references its body in the domain
  (step 1 removed the dead ones), committed before the cutoff, under its stream's lock, the checks
  repeated in the deleting statement (`committed_at`, the stream, no Build, no live claim); its
  stream gets its tombstone in the same transaction. A promotion inserting a release on a Build
  keeps the Build, so its body is never deleted under it.

### Readers

A release's Build is live, so its body and stream are: nothing a release serves is collected. A
stream readers may still hold (a replaced stream, a purged release's in phase 2) is deleted only a
grace after it was last marked. Each replica's blob record cache, which has no expiry today, gets
one: 15 minutes, well under the grace, so a replica never resolves a body to a stream deleted since.

### The pass

Every `gc.interval` (default `0`: no scheduled pass until the operator sets one, after every replica
runs this version), per domain, under the lease `kista/blob/gc/<domain>` (one replica collects a
domain; each domain on its own schedule, in parallel), in batches with the lease renewed (a lease
lost mid-pass stops it). A replica whose pass found the lease held tries again five minutes later:

1. **Builds** without a release, unused for the grace: deleted, one at a time.
2. **Body rows** no live Build references, committed before the cutoff: deleted, their streams
   marked.
3. **Listing**: the store's `streams/` walked (the `List` callback consumed in batches of 1,000);
   a stream no row references and no claim holds is marked (where unmarked), under its lock. Ages
   come from `since`, never the store's modification time (the one exception is `tmp/`).
4. **Tombstones**: `marked` ones older than the grace deleted, stale `deleting` ones deleted again,
   due ones whose stream has a row or a live claim removed; then expired claims removed.
5. **Leftovers of uploads**: `tmp/` objects of the `fs` store older than the grace (by their
   modification time); incomplete S3 multipart uploads under the prefix older than the grace,
   aborted (Azure discards uncommitted blocks itself).

The pass first reads the domain's marker (spec 0005) and stops if it is not this deployment's. A
store that cannot list, or a database error, stops the pass for the domain; nothing is deleted on a
guess. A pass that ran logs its counts and writes a server event `storage.gc` (`domain`,
`builds`, `bodies`, `marked`, `streams`, `tmp`, `uploads`, `bytes`, `claims`, `resolved`,
`dry_run`; outcome `failed` with `error` when it stopped on one): nothing of a tenant. A dry run
counts this pass alone: the streams of the bodies it would delete are not counted as marked, so it
underestimates what two passes delete.

Versioned S3 buckets and buckets with object lock keep deleted objects (or refuse the delete,
which stops the pass): their storage is the bucket's lifecycle rules' to manage. A bucket lifecycle
rule `AbortIncompleteMultipartUpload` is worth setting as well on AWS; MinIO and SeaweedFS list
incomplete uploads as the pass needs.

### Phase 2: purging releases

A tenant administrator (admin on the release's channel or extension, as for a yank) deletes a
**yanked** release explicitly: `DELETE /api/v1/tenants/{t}/channels/{c}/extensions/{name}/releases/{id}`
with `If-Match`, `kista admin release purge <tenant> <channel> <release-id>`. There is no automatic
retention.

- Only a yanked release: an active or deprecated one is refused (`409`): yank it first.
- A re-sign batch skips a release purged since it read it.
- **The slot stays taken**: in the channel's lock, the release's signatures and the release are
  deleted and its slot written to `purged_slots(channel_id, name, ext_version, platform, slot, abi,
  body_hash, seq, release_id, purged_at)`; the slot candidates include purged slots (a union beside
  the releases joined to their Builds), so a purged slot is never refilled, with its body or another
  ("a released (channel, name, version, platform) never changes its body", spec 0001): a purged
  candidate has the state `purged`, which every slot check refuses (an add, a publication, a
  promotion and an upstream cell alike, never "existing"); a channel's next sequence number counts
  purged slots too. The purge moves the channel's `release_version` (snapshots and the index follow). Its
  id answers `404`.
- The Build loses that reference; the collector deletes it a grace after its last use, then its body
  row, then the stream a grace after that.
- An event `release.purge` (subject the release; `data`: name, version, platform, body hash).
- A promotion that read the Build before the purge and inserts after its collection fails on the
  foreign key and retries once, as an intake does.

### Configuration, CLI

```yaml
gc:                        # file-only
  interval: 0              # e.g. 24h once every replica runs this version; 0: kista admin blob gc only
  grace: 24h               # at least four times the intake deadline (1h)
```

`kista admin blob gc [-domain d] [-apply]`: a pass now, a dry run unless `-apply` (counts and bytes
only). With `gc.interval` 0 (replicas maybe not all on this version), `-apply` also needs `-force`.
A domain whose pass stopped on an error is reported on stderr and the command exits non-zero.
Server administrators only (the CLI is).

### Data model

Migration 0013 (phase 1, `-- +min_reader 13`, so a replica started after it is this version):
`builds.used_at` (set to `created_at` on existing Builds), indexes on `builds (body_hash)` and `builds (used_at)`,
`blob_claims` (primary key `domain, stream_hash, claim_id`) and `blob_tombstones` (primary key
`domain, stream_hash`). Replicas already running an
older version keep running until restarted: the scheduled pass is off by default, and the operator
turns it on after the rollout. Phase 2: `purged_slots` (primary key `channel_id, name, ext_version,
platform, slot`), in its own migration.

### Package layout

```text
internal/blob      + tombstones in the commit, the record cache's expiry, the collector (gc.go)
internal/store     + used_at, unreferenced Builds and rows, tombstones, purged slots (2)
internal/release   + the retry on a collected Build; purge (2)
internal/app, cmd/kista, internal/api   + the scheduled pass, kista admin blob gc; purge (2)
```

### Changes to earlier specs

- **0005**: a commit claims its stream before the upload and writes the row after it, both under
  the stream's lock; an update that finds no row inserts; a replaced stream is marked; blob records
  are cached for 15 minutes; GC is this spec's.
- **0001**: Blob GC as designed here; a yanked release may be purged explicitly (2), its slot kept
  in `purged_slots` (yanked releases were "kept for audit": the event `release.purge` keeps what it
  was).
- **0008/0009**: every intake runs from its commit to its release under a one-hour deadline;
  one whose Build was collected meanwhile starts again from its commit, once.

## Security

- **Never a served body**: a release keeps its Build, body and stream live; deletions re-check
  what the pass found, in the deleting statement or under the stream's lock; the grace covers
  intakes in flight, readers mid-download and caches.
- **Fail closed**: a store that cannot list, a database error, a marker that is not this
  deployment's: the domain is not collected. Two deployments never share a prefix (spec 0005).
- **Old replicas**: migration 0013's `min_reader` keeps an older replica from starting; one already
  running does not know claims or `used_at`, so the scheduled pass is off until the operator turns
  it on after the rollout (`kista admin blob gc` is a dry run unless asked).
- **Tenant isolation**: bodies are shared within a domain by design (spec 0005); the collector's
  counts are the domain's, never a tenant's. A purge is the tenant's own release, under its grants.
- **Bounded work**: batches, a lease per domain, a dry run by default from the CLI.

## Testing

- **Store suite** (three dialects): unreferenced Builds by `used_at` (and one an older replica
  inserted); a release on a collected Build; unreferenced rows, kept while a Build references them
  or a commit claims their stream; claims, a lost claim, a replace's mark; tombstones: a claimed
  stream not deleted, a commit refused while deleting, a stale delete taken over (the first
  collector's stamp then no longer its own, its end a no-op), a refused delete back to `marked`;
  strict lease renewal.
- **Blob service** (a fake store over `fs`): a released body survives; a listed orphan deleted by
  two passes a grace apart; a commit waiting on a delete then uploading; a refused delete stopping
  the pass (recorded as failed) and the next pass deleting; a lease lost mid-pass stopping before
  the next delete; the resolver finishing a stale delete, and refusing a foreign marker; the record
  cache's expiry; `tmp/` on `fs`; S3 incomplete uploads (SeaweedFS); the marker check.
- **CLI**: a dry run by default, `-apply` refused without `-force` while `gc.interval` is 0, a
  domain error exiting non-zero.
- **Intake**: a Build collected between `FindOrInsertBuild` and the release insert: started again
  from the commit; collected again: the intake fails after the second try.
- **Backends**: `List` and `Delete` on fs, S3 (SeaweedFS) and Azurite in the existing suites.
- **e2e** (phase 2, with purging; phase 1's grace keeps a collection out of reach end to end
  without an injected clock): a purged release's slot cannot be refilled, DuckDB at the pin cannot
  install it, and other releases are served throughout passes.

## Alternatives considered

- **Writing the row before the upload, under a domain lock**: closes the delete race without
  tombstones, but a dedup hit then trusts a row whose stream may never be uploaded, and a committer
  cannot upload another compressor's stream; a domain-wide lock serializes commits behind deletes.
- **Reference counts on rows**: a counter per body on every Build insert and delete; one more
  write on the intake's path and a counter to repair after any bug.
- **Age alone** (an object older than the grace and unreferenced at listing time): the listing and
  the delete are minutes apart; a commit re-uploading in between loses its object.
- **Object store lifecycle rules**: they know nothing of rows; usable only for `tmp/`.

## Follow-ups

- Tenant deletion; an automatic retention for yanked releases, if wanted later.
- `kista admin blob verify` (spec 0005's follow-up): every row's object present and intact.
- Moving a tenant between domains (spec 0005's follow-up), which leaves the old domain's bodies to
  this collector.
