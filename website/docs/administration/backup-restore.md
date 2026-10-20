---
title: Backup and restore
---

# Backup and restore

kista keeps its state in three places, each backed up with its own tools. kista itself has two
commands for this: `kista admin backup` (SQLite only) and `kista admin blob verify` (checking bodies
after a restore). Everything else is the database's, the object store's and the key source's own
backup.

## What state exists

| State | Where | What it holds | Backed up with |
| --- | --- | --- | --- |
| The database | PostgreSQL, SQL Server or SQLite (`store`) | tenants, channels, DuckDB versions, keys (public keys and signer references), key history, releases and their signatures, blob records (body length and chunk digests), issuers, grants, publishers, upstreams, attachments' records, feeds, events, statistics, the deployment id and the storage domain pins | the engine's own backups; `kista admin backup` for SQLite |
| Blob storage | each storage domain's store (`blob.domains`: `fs`, `s3`, `azureblob`) | extension bodies (`streams/`), attachment contents (`attachments/`), the domain marker `kista-domain.json` | the object store's own backup, snapshots or versioning |
| Private keys | each key source (`signers.sources`; key files in `signers.file_dir`) | the channel and attestation signing keys | the key source's own backup; kista never sees private keys |

Also part of a deployment, and backed up with your configuration management:

- the configuration file and the files it names (TLS certificates, the database password file,
  storage access key files, upstream credential files, CA bundles); a restored file must keep the
  owner and mode [Files kista reads](../security.md#files-kista-reads) asks for;
- event sink files, if a `jsonl` sink writes to disk (they are copies of the events; the database
  keeps its own for `events.retention`).

Not state: the spool directory (`blob.spool_dir`) holds files only while they are taken in, and
`tmp/` in an `fs` domain holds unfinished uploads. Neither needs a backup.

## How the parts depend on each other

- **The database is the trust root.** `.well-known` is built from the keys in the database. A
  release is served only with a signature row from the database, and every byte served is checked
  against chunk digests in the database. A body in the store without its record is never served.
- **Blob records point at objects.** Each body record names a stream object in its domain. A record
  whose object is gone is served `503` once a reader finds it.
- **Storage domains are pinned.** The database records each domain's store identity (its kind plus
  the `fs` root, or the endpoint, bucket and prefix, or the account, container and prefix). Each
  store holds `kista-domain.json` naming the domain, the store and the deployment id from the
  database. At startup kista refuses a domain whose store does not match its pin or whose marker
  names another deployment or domain.
- **Keys live outside.** The database holds only public keys and signer references. A key whose
  private part is lost cannot sign again: that channel needs a new key and a
  [rotation](key-rotation.md).

## What to back up, and in what order

1. **The database first.**
2. **Then the blob storage**, finishing within `gc.grace` (default 24h) of the database backup.

Why this order: a body upload writes the object first and its record afterwards, and the garbage
collector deletes an object only a grace period after nothing references it. So objects referenced
by the database backup are still in the store for at least `gc.grace` after it, and a blob backup
taken in that window covers every record of the database backup. Extra objects in the blob backup
(uploaded after the database backup) are harmless: the collector removes them as orphans after a
restore.

Key sources are backed up on their own schedule. What matters is that every key the database names
(`kista admin key list`, `kista admin attestation-key list`) can still be opened from its source.

### SQLite

```bash
kista admin -config /etc/kista/kista.yaml backup /var/backups/kista/kista-2026-10-12.db
```

It writes a consistent copy (`VACUUM INTO`) while kista runs, to a **new** file with mode `0600`.
It refuses an existing file. Copy the file elsewhere afterwards. Do not copy the live database file
with `cp`: in WAL mode, recent changes live in the `-wal` file.

### PostgreSQL and SQL Server

Use the engine's own backups (`pg_dump`, point-in-time recovery, SQL Server backups, or the managed
service's backups). `kista admin backup` refuses them.

### Blob storage

Copy the domain's objects with the store's tools, keeping their keys: `streams/`,
`attachments/` and `kista-domain.json`. For an `fs` domain, copy the root directory. For `s3` and
`azureblob`, bucket or container versioning and replication work as well.

## Restoring

1. **Stop every `kista serve` replica.** Running replicas cache by version counters that a restored
   database repeats, so they must start fresh.
2. **Restore the database.**
   - SQLite: put the backup file at `store.sqlite.path`, owned by the service user with mode
     `0600`, and remove any `-wal` and `-shm` files left by the previous database.
   - PostgreSQL and SQL Server: restore with the engine's tools.
3. **Restore the blob storage to the same location**: the same `fs` root, or the same endpoint,
   bucket or container, and prefix, including `kista-domain.json`. The pins in the database refuse
   another location, and there is no command to re-pin a domain.
4. **Make sure the key sources still hold every key** the restored database names.
5. **Check the schema and the storage** with the binary you will run:

   ```bash
   kista admin -config /etc/kista/kista.yaml check
   kista admin -config /etc/kista/kista.yaml blob check
   ```

   `check` must print `schema ok`. A database restored from before an upgrade may need migrations
   (`kista admin migrate`, or `store.migrate: auto`); a binary older than the backup's
   `min_reader` is refused ([Upgrading](upgrading.md)). `blob check` prints `domain <d> ok` per
   domain and fails if a tenant's domain is not configured.
6. **Verify the bodies** (below), then start `kista serve`.

## Verifying bodies after a restore

`kista admin blob verify` reads every body record of a storage domain and checks its object
([spec 0018](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0018-blob-verify/spec.md)).

```bash
kista admin -config /etc/kista/kista.yaml blob verify                  # quick: every object present, of its length
kista admin -config /etc/kista/kista.yaml blob verify -full -unmark    # read every chunk; clear marks of intact bodies
```

- **Quick** (default): one `Stat` per record.
- **Full** (`-full`): reads every stream chunk by chunk and checks each 1 MiB chunk against its
  digest, as serving does. On a remote store this costs about one download of the domain.
- **`-unmark`** (with `-full`): clears the "corrupt" mark of a body found intact. Use it after a
  restore: a body marked while the store was broken is otherwise served `503` until it is uploaded
  again.
- **`-mark`** (with `-full`): marks `missing`, `short` and `corrupt` bodies as a reader would, so
  they are served `503` instead of failing mid-download. If most bodies checked so far look missing
  or short, the pass stops and marks nothing more: that usually means the wrong store.
- `-domain <d>` checks one domain; `-concurrency <n>` (1 to 32, default 4) sets how many records
  are checked at once.
- It checks extension bodies only. Attachment contents (`attachments/`) are not covered; check one
  with `kista admin attachment show <tenant> <extension> <id> -content`.

Outcomes:

| Outcome | Meaning |
| --- | --- |
| `ok` | present and of its length (full: every chunk matches) |
| `marked` | as `ok`, but marked: served `503` until uploaded again or unmarked |
| `missing` | no object |
| `short` | shorter than recorded |
| `long` | longer than recorded; still served correctly, but not what was uploaded |
| `corrupt` | a chunk differs from its digest |
| `record` | the record itself is damaged |
| `unreadable` | the store answered with an error or timed out |

The command exits `0` only when every record is `ok` (or skipped because it changed during the
pass), so it fits cron: a quick pass daily, a full pass monthly and after any restore or storage
incident. Each pass is recorded as the server event `storage.verify`, with outcome `failed` when
something was wrong.

### Repairing a missing or corrupt body

kista keeps no second copy of a body. A body comes back only by uploading the same file again:

1. find the bodies in `blob verify`'s output and their releases with
   `kista admin release list <tenant>/<channel>`;
2. mark them with `blob verify -full -mark`, so they are served `503` rather than failing;
3. add the same file again: `kista admin release add <tenant>/<channel> <file> -name <name>` (with
   the same flags as the release), or let the publisher run again. When the body's record is marked,
   adding the file uploads the stream again and clears the mark.

A mirrored body is fetched again only when its upstream's copy changes, so repair it the same way
with the upstream's file. A promotion reuses the stored body and uploads nothing.

## Restoring an older database: consequences

A restored database is the state at the time of its backup:

- **Keys.** Key states go back to that point. A key added or activated after the backup is missing
  or in its old state, and clients may already trust it. Compare `kista admin key list` and
  `kista admin wellknown` with what clients use. A missing key can be registered again with
  `kista admin key add` and the same signer reference; it starts as `trusted` with a new
  `trusted_since`.
- **Releases.** Releases made after the backup are gone, and their slots are free again. Bodies
  uploaded after the backup are orphans in the store and are collected after `gc.grace` once
  `gc.interval` is set, or by `kista admin blob gc -apply`.
- **Grants, issuers, publishers and upstreams** go back too; publisher keys registered after the
  backup no longer work, and keys removed since are back: remove them again.
- **Objects deleted since the backup** (by the collector, or by hand) show up as `missing` in
  `blob verify`.
- **Events and statistics** after the backup are lost from the database; copies sent to event sinks
  remain there.
- **Deleted tenants** come back: see below.

### A backup from before a tenant deletion

Restoring a database backup taken before a tenant was
[deleted](tenants-and-channels.md#deleting-a-tenant) brings the tenant back in the state it had
then. If it was active, it serves again; if its deletion had started but not finished, the reaper
carries on. Its bodies may already be collected: those releases then fail to download, and
`blob verify` reports them as `missing`. After such a restore:

1. Compare what the restored database says with what was deleted:
   `kista admin tenant list -state deleting,deleted`, and the `tenant.delete` events kept in an
   event sink (the database's own copies may be gone with the restore).
2. Delete again whatever came back: suspend it, and delete it once `tenants.min_suspended` has
   passed. The restored `suspended_at`, if the tenant was already suspended, counts.
3. Run `kista admin blob verify` (above), and repair or yank what it reports for the tenants that
   stay.
