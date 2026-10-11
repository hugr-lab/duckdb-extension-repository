# Spec 0018: Verifying stored bodies

- **Status**: draft
- **Date**: 2026-10-11
- **Author**: vgsml, Claude

## Summary

`kista admin blob verify` reads every body record of a storage domain and checks that its object is
in the store and intact: present, of the recorded length, and, with `-full`, every 1 MiB chunk
matching its recorded digest (spec 0005). It reports what is wrong, can mark it corrupt as a reader
would, and records the pass as an event. Spec 0005 and spec 0016 left it as a follow-up.

## Problem

Serving verifies each chunk it sends (spec 0005), so a missing or altered object is found only when
someone downloads it, and then the download fails. A body nobody asked for in months (an old
version a node pins, a release kept for rollback) can be gone without anyone knowing: lost by the
store, deleted by a stale collector request (spec 0016's residual case), removed by hand, or planted
by a writer to the bucket. An operator needs to know before the node that needs it does, and needs
it after a restore from backup, a move of the bucket, or a storage incident.

## Design

### The command

```
kista admin blob verify [-domain <name>] [-full] [-mark] [-concurrency 4]
```

- Every configured domain, or one. Records are read page by page by body hash (500 at a time), so
  a domain of any size runs in bounded memory.
- **Quick** (default): `Stat` per record. The object must exist and be exactly `stream_len` long.
- **Full** (`-full`): the whole stream is read, chunk by chunk, each checked against the record's
  digests, through the same reader serving uses (one ranged `Get` from the start, the bounded calls
  of spec 0005). A transport error is told from corruption as serving tells it (the object's size is
  asked for before calling a short read corruption), and is reported as `unreadable`, never marked.
- A record whose chunk digests do not cover its stream (a damaged row) is reported `record`.
- **Outcomes** per record: `ok`, `missing`, `short`, `long` (the object is longer than recorded:
  never served beyond `stream_len`, but not what was uploaded), `corrupt` (a chunk differs, with
  its index), `record`, `unreadable`.
- **Output**: one line per problem (domain, body hash, stream hash, outcome, the chunk or sizes,
  whether the record was already marked), then per domain the counts and the bytes read. The exit
  status is non-zero when any record is not `ok` or a domain could not be read.
- **`-mark`**: a `missing`, `short`, `long` or `corrupt` record gets `corrupt_at`, exactly as a
  reader marks it (once per commit; a record committed again since it was read is left alone). The
  next commit of the body (a publication, a promotion, an upstream run) uploads the stream again and
  clears it (spec 0005). Without `-mark` nothing is written but the event.
- **Concurrency**: `-concurrency` records at once (1 to 32, default 4), so a full pass over a remote
  store is not one request at a time, nor a flood.
- It runs beside `kista serve`: it only reads objects and records, and marks as a reader does. It
  takes no lease (two passes at once are only redundant).

### Events

A pass per domain is the server event `storage.verify` (actor the OS user, subject
`domain:<name>`): `domain`, `full`, `mark`, `checked`, `ok`, `missing`, `short`, `long`, `corrupt`,
`record`, `unreadable`, `marked`, `bytes`, and `error` when the pass stopped. Its outcome is `ok`
when every record is ok, `failed` otherwise, so the event log and a sink alert on it. Body hashes are not in the event (they are in the command's output
and the log): an event is a summary.

### Package layout

```text
internal/store     ListBlobs (a domain's records by body hash, paged)
internal/blob      Service.Verify (a domain: records, checks, marks) and its result
internal/app       the storage.verify event
cmd/kista          kista admin blob verify
internal/audit     the storage.verify kind
```

## Security

- It is an operator's command (the CLI, the service's own configuration and credentials); there is
  no API route. It reads what the server reads and writes only `corrupt_at`, which a reader writes
  too.
- A mark never repairs anything or deletes anything; it cannot be used to deny service beyond what
  the store's state already denies (a marked record is still served while its object is good: the
  reader verifies chunks, not the mark).
- Nothing in the store is trusted: the record's digests decide, as when serving.

## Testing

- Store suite on the three dialects: `ListBlobs` paging.
- `blob` with the `fs` backend and the in-memory test store: quick and full passes over records that
  are ok, missing, short, long, corrupt in a middle chunk and in the last partial one, a damaged
  record, and a store whose reads fail (unreadable, never marked); `-mark` marking each kind once and
  a record committed again since it was read left unmarked; concurrency.
- CLI: the output, the exit status, `-domain`, the event written.
- The events coverage test lists the kind.

## Alternatives considered

- **A background scrub in `kista serve`**: steady load on the store and the database for something
  needed rarely; an operator runs the command when it matters (after an incident, on a schedule of
  their own) and sees the answer.
- **Repairing in place** (re-uploading from somewhere): kista keeps no second copy (spec 0005); the
  body comes back only from a new commit of it, which the mark prompts.
- **Verifying through the API**: a tenant administrator would see storage that is the server's; the
  operator's CLI is the right place.

## Follow-ups

- Objects without a record are the collector's (spec 0016); verify does not list the store.
- A metric of the last pass per domain, if operators want it on a dashboard.
