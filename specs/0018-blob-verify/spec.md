# Spec 0018: Verifying stored bodies

- **Status**: implemented (2026-10-11)
- **Date**: 2026-10-11
- **Author**: vgsml, Claude

## Summary

`kista admin blob verify` reads every body record of a storage domain and checks its object in the
store: present and of the recorded length, and, with `-full`, every 1 MiB chunk against its recorded
digest (spec 0005), through the reader serving uses. It reports what is wrong, can mark it as a
reader would (`-mark`) and clear the mark of a body found intact again (`-unmark`), and records each
pass as an event. Spec 0005 and spec 0016 left it as a follow-up.

## Problem

Serving verifies each chunk it sends (spec 0005), so a missing or altered object is found only when
someone downloads it, and then the download fails. A body nobody asked for in months (an old version
a node pins, a release kept for rollback) can be gone without anyone knowing: lost by the store,
deleted by a stale collector request (spec 0016's residual case), removed by hand, or changed by a
writer to the bucket. And a record marked corrupt during an outage stays marked after the store is
restored: serving answers `503` for it until the body is committed again. An operator needs to know
both, before the node that needs the body does: after a restore from backup, a move of the bucket,
a storage incident, or on a schedule.

## Design

### The command

```
kista admin blob verify [-domain <d>] [-full [-mark] [-unmark]] [-concurrency <n>]
```

- Every configured domain, or one. Records are read by body hash, 500 at a time, so a domain of any
  size runs in bounded memory. Each page is checked with `-concurrency` records at once (1 to 32,
  default 4), and finishes before the next is read.
- **Quick** (default): one `Stat` per record. The object must exist and be exactly `stream_len`
  bytes.
- **Full** (`-full`): the stream is read chunk by chunk through serving's reader: one ranged `Get`
  to `stream_len`, each chunk checked against its digest, the bounded calls of spec 0005. A failed
  read is told from a missing or short object as serving tells it (the object's size is asked for
  before calling it either); otherwise it is a transport error.
- **Outcomes** per record:

| Outcome | Meaning | Marked by `-mark` |
| --- | --- | --- |
| `ok` | present, of its length (full: every chunk matches) | |
| `marked` | as `ok`, but the record is marked: served `503` until committed again or unmarked | |
| `missing` | no object | yes (full only) |
| `short` | shorter than recorded | yes (full only) |
| `corrupt` | a chunk differs from its digest (the chunk is named) | yes (full only) |
| `long` | longer than recorded: served all the same (reads stop at `stream_len`, the chunks decide), but not what was uploaded | never |
| `record` | the record itself is damaged (its digests do not cover its stream, a checksum out of range) | never |
| `unreadable` | a transport error (the store answered something else, timed out, refused) | never |

- **A record that changes during the pass is skipped**, not reported: before a problem is reported,
  its record is read again, and if the collector removed it (spec 0016) or a commit gave the body
  another stream since its page was read, it is counted as skipped.
- **Output**: a line per problem (domain, body hash, stream hash, outcome, detail on one line, and
  whether this pass marked or unmarked it, or it was marked before), then a table per domain with
  the counts and the bytes read. The exit status is `0` only when every record is `ok` (or
  skipped) and every domain could be read; a usage error is `2`, a problem `1`.

### Marks

A mark (`corrupt_at`) makes serving answer `503` for the body until a new commit of it uploads the
stream again (spec 0005). So:

- **`-mark`** needs `-full`, and marks only what a reader would mark from the same read: `missing`,
  `short` and `corrupt`. A quick `Stat` alone never marks, nor does a transport error, a `long`
  object or a damaged record. A mark is written once per commit, and never on a record committed
  again since it was read (the same guard as a reader's).
- **The wrong store is not lost bodies.** Marks are written per page, after the page is checked.
  When most records checked so far are missing or short (at least three, and more than half), the
  pass stops with an error and marks nothing more: an unmounted directory, another bucket or prefix
  that answers like an empty store would otherwise take the whole domain offline.
- **`-unmark`** needs `-full`: a `marked` record whose object reads intact has its mark cleared, under
  the same guard. A restored store then serves again without waiting for every body to be committed.
- **Repairing a missing or corrupt body** is a new commit of it, which uploads its stream (spec
  0005). Adding or publishing the same file again where a release already holds it used to return
  that release without storing anything; it now uploads the stream again when the body's record is
  marked, so the flow is: `verify -full -mark`, then the same file added again (`kista admin release
  add`, or the publisher's next run). A promotion reuses the Build and uploads nothing. A mirrored
  body is fetched again only when its upstream's copy changes (runs ask conditionally, spec 0009), so
  it is repaired the same way, with the upstream's file. The pass's output names the bodies;
  `kista admin release list` names their releases.
- Serving replicas cache records for up to 15 minutes (spec 0005), so a mark or an unmark applies
  everywhere within that.

### Events

A pass per domain is the server event `storage.verify` (actor the OS user, subject `domain:<name>`):
`domain`, `full`, `mark`, `unmark`, the counts (`checked`, `ok`, `skipped`, `marked_ok`, `missing`,
`short`, `long`, `corrupt`, `record`, `unreadable`), `marked`, `unmarked`, `bytes`, and `error` when
the pass stopped. Its outcome is `ok` when every record was, `failed` otherwise, so a sink can alert
on it. Body hashes are in the command's output, not in the event: an event is a summary.

### Running it

- It runs beside `kista serve` and the collector: it reads objects and records, and writes only
  marks, as readers do. It takes no lease; two passes at once are only redundant.
- A quick pass costs one `Stat` per record. A full pass reads every stored byte: on a remote store
  that is the egress and request cost of downloading the domain once, and it shares bandwidth with
  serving; its time is about the domain's bytes divided by `-concurrency` times one stream's
  throughput.
- A reasonable schedule: a quick pass daily from cron (its exit status is meant for it), a full one
  monthly and after any storage incident or restore, with `-unmark` after a restore and `-mark` when
  the bodies it finds should stop being offered until repaired.

### Package layout

```text
internal/store     ListBlobs (records by body hash, a damaged one listed with its error);
                   MarkBlobCorrupt answers whether it marked; UnmarkBlob
internal/blob      Service.Verify, its outcomes and result
internal/release  Add uploads a marked body again when its release exists
internal/app       the storage.verify event
cmd/kista          kista admin blob verify
internal/audit     the storage.verify kind
```

## Security

- It is an operator's command (the CLI, with the service's configuration and credentials); there is
  no API route. It reads what the server reads and writes only `corrupt_at`.
- A mark denies service for the body until it is committed again, so marks follow what serving
  itself would conclude from the same full read, never from a size alone or a transport error, and
  never while the store looks like the wrong one. A store writer can make bodies look missing or
  short (within spec 0005's threat model, the same as deleting them), but not make kista serve
  another body: the record's digests decide.
- An unmark only follows a full read that matched every digest.
- Its output and events carry no keys, URLs or credentials: store errors are reduced to a status,
  an error code and a request id (spec 0005).

## Testing

- Store suite on the three dialects: `ListBlobs` paging and a damaged record listed with its error;
  `MarkBlobCorrupt` and `UnmarkBlob` answering whether they changed the record, once, and never on
  another commit or stream.
- `blob`, over the `fs` backend and a wrapping store: quick and full passes over records that are
  ok, marked but intact, missing, short, long, corrupt in a middle chunk and in the last partial one,
  damaged, and unreadable (a failing `Stat`, and a failing read with `Stat` answering); a record
  removed during the pass skipped; `-mark` marking only missing, short and corrupt, once, and only
  from a full pass; `-unmark`; a store that lost most bodies stopping the pass with nothing marked;
  pages smaller than the domain; concurrency 1 and 8; a cancelled pass counting nothing in flight.
- `release`: adding the same file again uploads a marked, lost body and clears its mark.
- CLI: the table, the problem lines, the exit status, `-domain`, the usage errors, the event.

## Alternatives considered

- **A background scrub in `kista serve`**: steady load on the store and the database for something
  needed rarely; an operator runs the command when it matters and sees the answer.
- **Marking from a quick pass**: a size alone, or a store that answers `404` during a failover, would
  take bodies offline that serving would still deliver.
- **Repairing in place** (re-uploading from somewhere): kista keeps no second copy (spec 0005); the
  body comes back only from a new commit of it.
- **Verifying through the API**: a tenant administrator would see storage that is the server's; the
  operator's CLI is the right place.

## Follow-ups

- Objects without a record are the collector's (spec 0016); verify does not list the store.
- A metric of the last pass per domain, if operators want it on a dashboard.
- An upstream run fetching again the bodies it supplied that are marked, so a mirrored body repairs
  itself.
