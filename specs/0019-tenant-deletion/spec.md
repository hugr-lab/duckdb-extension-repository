# Spec 0019: Deleting a tenant

- **Status**: draft
- **Date**: 2026-10-12
- **Author**: vgsml, Claude

## Summary

A server administrator can delete a tenant. Its channels, keys, releases, grants, publishers,
upstreams, attachments, feeds and download statistics are removed, and its bodies become garbage
for the storage collector. Deletion takes two steps. Suspending a tenant (spec 0003) stops serving
and can be undone. Deleting a tenant that has been suspended long enough cannot be undone. The rows
are removed in the background, in small transactions, and the tenant's row stays as a tombstone,
so its name is never given to anyone else.

## Problem

There is no way to remove a tenant today. Spec 0003 says only that "a later deletion keeps a
tombstone". This affects:
- **Enterest.** A customer who leaves expects its extensions, keys, grants and download records to
  go, and the hosted service cannot keep every tenant forever.
- **An organisation's own kista.** A tenant made for a trial, or moved to another server, stays in
  every listing and holds storage.

Removing rows by hand is not an answer:
- About forty tables hold a tenant's state.
- Most foreign keys are `NO ACTION`. Some cascade: publishers' credentials and API keys, upstream
  children, console clients, group members and grants, and feed children. Spec 0003's "nothing
  cascades" is out of date.
- Some tables (download counts, statements) hold millions of rows.
- A foreign key cycle exists: `channels.serving_key_id` points at `channel_keys`.

## Design

### The steps

```text
active ──suspend──▶ suspended ──delete (suspended ≥ tenants.min_suspended)──▶ deleting ──reaper──▶ deleted
   ▲                    │
   └──────resume────────┘
```

1. **Suspend** works as today (spec 0003, 0006, 0007), and now also sets `suspended_at`. Serving
   answers `404` and the tenant's own principals lose the management API. Resume clears
   `suspended_at`.
2. **Delete.** A server administrator deletes a tenant whose `suspended_at` is at least
   `tenants.min_suspended` old. A null `suspended_at` (a tenant suspended by an older binary
   during an upgrade) is refused with a hint to resume and suspend it again. One transaction, in
   this order:
   - sets `deleting_at` (the `UPDATE` first, so it waits for any write in flight, and what that
     write added is read next);
   - records in `tenant_deletions` who started it, the keys to retire (below) and the ids of the
     tenant's channels: no channel can be created from then on, so the list is complete;
   - writes the event `tenant.delete`.

   From that commit the tenant is closed:
   - Every route answers as for an unknown tenant. The only exceptions are a few server
     administrator routes, listed below.
   - Every change to the tenant is refused.
   - The background runs skip it.

   There is no way back. The minimum suspension is the window for changing one's mind.
3. **Reaping.** After a settle period, the reaper removes the tenant's rows in batches. It then
   writes the tombstone: it sets `deleted_at`, clears `display_name` and moves `version` (the
   ETag). The id, name, storage domain and dates stay, and the deletion record says who.

`state` itself keeps its two values, `active` and `suspended`, and a deleting or deleted tenant
stays `suspended`. The API shows a derived `state` (`deleting` or `deleted`). This keeps the CHECK
constraint as it is. Changing it would mean rebuilding `tenants` in SQLite, a table that fourteen
others reference. It also means an older binary sees the tenant as suspended, and never serves it.

### Closing the tenant

- **Writes.** Every transaction that changes a tenant's rows first calls
  `tx.WritableTenant(ctx, id)`. The call reads the tenant row under a shared lock that is held
  until commit (PostgreSQL `FOR SHARE`; SQL Server `WITH (HOLDLOCK, ROWLOCK)`; SQLite already
  serialises writers). It returns `ErrDeleting` when `deleting_at` is set.
  A transaction that then updates the tenant row itself (`BumpAuth`, the shadow backfill, suspend
  and resume) takes the row for update instead (`FOR NO KEY UPDATE`; `WITH (UPDLOCK, HOLDLOCK,
  ROWLOCK)`), so two of them never deadlock upgrading a shared lock.
  - The delete transition's `UPDATE` waits for these transactions, and they wait for it. So no
    write commits on a tenant after the transition commits.
  - Writes that today read the tenant before their transaction move the read inside it.
  - Suspend and resume go through the same call, and refuse a deleting tenant. Today
    `SetTenantState` checks only the target state, so a resume would turn a deleting tenant back
    to `active`. Suspending a suspended tenant, or resuming an active one, stays a no-op, as
    today.
  - The test drives every exported write method of the services from the registry in
    `internal/audit/coverage_test.go`, except the server-wide ones (`CreateTenant`,
    `AddVersion`, `AddVersionCAPI`). Each must refuse a deleting tenant. Writers outside the
    registry are named and tested on their own: the pull-through `upstream.Puller` (its cells and
    events), the re-signer, the statistics writes.
- **Statistics.** These writes have no foreign key:
  - `AddDownloadCounts` and `PutInstallers` insert only where `EXISTS` a tenant with `deleting_at`
    null, and report the skipped keys as done, so the serve counter does not keep them.
    `AddDownloadCounts`' update of an existing row may still land before the reaper reaches the
    table, which is harmless.
  - `Daily` skips deleting and deleted tenants.
- **Background runs.**
  - `DueUpstreams`, `DueFeeds` and `ChannelsToResign` skip tenants with `deleting_at` set. The
    pull-through `Puller` takes misses only from served requests, which the gate already stops,
    and its writes go through `WritableTenant`.
  - A run already going fails on its next write through `WritableTenant`, and releases its lease.
  - The re-signer's insert goes through `WritableTenant`.
  - The storage collector does not skip the tenant. It only deletes, by reference, and its
    attachment sweep is what removes the tenant's content objects.
- **API.** The route gate (`api.go`, today `t.State != active && !admin`) changes. For a deleting or
  deleted tenant, every tenant route answers the unknown tenant's `404`, the same in status, body
  and headers as for a name that never existed, to every caller. The only exceptions are these
  server administrator routes:
  - `GET /api/v1/tenants/{t}`;
  - `GET /api/v1/tenants/{t}/events[/{id}]`;
  - `DELETE /api/v1/tenants/{t}` (`409 conflict`, "the tenant is being deleted" or "the tenant
    was deleted").
- **CLI.** The services return `ErrDeleting` for reads and writes of a deleting or deleted tenant.
  The exceptions are `tenant show`, `tenant reap` and `events list`. The CLI prints "the tenant is
  being deleted" or "the tenant was deleted". The API maps `ErrDeleting` to `409 conflict`, which
  it can only meet in a race.
- **Listings.**
  - `ListTenants` takes a state filter. Listings default to every state but `deleted`.
  - The event sinks' name resolver keeps resolving tombstones, because the tenant's remaining
    events still name it.
  - `blob.MissingDomains` ignores tombstones. A deleted tenant's storage domain must stay
    configured until the collector has removed its bodies: the collector runs only over configured
    domains. A domain left out earlier keeps them (Follow-ups).
- **Names.** `tenant create` with a tombstone's name answers `409 conflict`: "the name belonged to
  a deleted tenant and is never reused". This needs no new problem type.

### The reaper

- **Where it runs.** `kista serve` runs it. Every minute a replica selects tenants whose
  `deleting_at` is older than the settle period and whose next attempt is due. It takes the lease
  `kista/tenant-delete/<id>` (TTL 5 minutes, renewed after every batch).
- **Version gate.** A replica reaps only when the database's schema level equals the binary's own.
  An older binary does not know a newer table, and must not write a tombstone over it.
- **Settle period.** Two hours after `deleting_at`, by the reaper's clock: twice the intake
  deadline of spec 0016, which also leaves room for clock skew between replicas. Uploads spooled
  before the delete, and upstream cells mid-flight, are long done by then.
  Their writes are refused anyway. The settle period only keeps the reaper from deleting under
  them.
- **Runs in progress.** While an upstream's or a feed's lease (`kista/upstream/<id>`,
  `kista/feed/<id>`) is still held, the reaper leaves that upstream or feed for the next pass. It
  deletes an expired lease row together with its upstream or feed: `kista/feed/<id>`,
  `kista/upstream/<id>`, and the pull-through cell leases under `kista/upstream/<id>/`. With the
  channels it deletes `resign/<channel id>`.
- **Order**, children before parents. Cascading children are deleted explicitly first, so that a
  batch stays bounded.
  1. feeds: `feed_cursors`, `feed_channels`, `feed_keys`, `feeds`, and their lease rows;
  2. the source side of the feed: `statements`, `statement_counters`;
  3. `attachments` (own and feed documents);
  4. `attestation_keys`;
  5. `extension_group_grants`, `extension_group_members`, `extension_groups`;
  6. `grants`, `issuer_console_clients`, `issuers`;
  7. `api_keys`, `publisher_github`, `publishers`;
  8. `upstream_cells`, `upstream_versions`, `upstream_entries`, `upstream_platforms`,
     `upstream_keys`, `upstreams`, and their lease rows;
  9. `shadows`, `blocks`;
  10. `UPDATE channels SET serving_key_id = NULL` for the tenant's channels;
  11. `release_signatures`, `releases`, `purged_slots` (by the channel ids in the deletion
      record), `builds`;
  12. `key_events`, `channel_keys`;
  13. `channel_duckdb_versions`, `channels`;
  14. `download_counts`, `download_installers`;
  15. the tombstone.
- **Batches.** A batch deletes at most 1,000 rows in its own transaction, through a bounded
  subquery rather than a list of keys (composite keys would exceed SQL Server's 2,100 parameters):
  `DELETE TOP (1000) … WHERE …` on SQL Server, `WHERE ctid IN (SELECT ctid … LIMIT 1000)` on
  PostgreSQL, `WHERE rowid IN (SELECT rowid … LIMIT 1000)` on SQLite. A child table without
  `tenant_id` is selected through its parent (`WHERE upstream_id IN (SELECT id FROM upstreams
  WHERE tenant_id = ?)`). A table is done when a batch deletes none.
- **The tombstone transaction** does four things:
  - checks that no table in the list still has a row for the tenant (`purged_slots` by the channel
    ids in the deletion record);
  - writes the tombstone with `UPDATE … WHERE id = ? AND deleted_at IS NULL`;
  - writes `tenant.purge`, only when that `UPDATE` changed a row;
  - deletes the reaper's own lease.

  If a row is found, the pass starts again from its table.
- **Resumable.** A pass interrupted by a lost lease, a restart or an error leaves the tenant
  deleting, and the next pass continues from the rows that are left.
- **Progress.** `tenant_deletions` records the reaper's state: passes, the current table, rows
  deleted so far, the last error's class, and the time of the next attempt.
- **Failures.**
  - After a failed pass the next attempt waits 1 minute, doubling up to 1 hour.
  - `tenant.purge` with outcome `failed` is written on the first failure and when the error class
    changes, not on every pass.
- **A late sweep.** For seven days after a tombstone, the reaper checks it once an hour. Rows found
  under it are reaped again, with a `tenant.purge` that has `late: true`. This should never happen.
  The sweep makes sure it would show.
- **Completeness test.** The list is in one place in the code. A store test reads each dialect's
  schema and classifies every table:
  - reaped: the list;
  - kept: `tenants`, `tenant_deletions`, `tenant_audiences`, `key_fingerprints`, `events`;
  - global: `blobs`, `blob_claims`, `blob_tombstones`, `storage_domains`, `duckdb_versions`,
    `duckdb_version_c_apis`, `deployment`, `leases`, `event_sinks`, `statistics_days`,
    `schema_migrations` (created by the migrator), and the dialect's own (`sqlite_*`).

  A table in none of the three fails the test. A new feature's table cannot be forgotten.

### What is kept, and for how long

An operator answering a deletion request needs this table.

| What | Where | How long |
| --- | --- | --- |
| The tombstone: id, name, storage domain, created, suspended, deleting and deleted times | `tenants` | forever: the name is never reused |
| The deletion record: who started it, the former channel ids, the keys to retire (kind, id, fingerprint, signer reference) | `tenant_deletions` | forever; server administrators only |
| Assigned audiences | `tenant_audiences` | forever: another tenant cannot take them and accept tokens minted for this one |
| Key fingerprints | `key_fingerprints` | forever: a key that signed for this tenant never signs for another |
| The tenant's events (actor names, reduced client addresses) | `events`, and every configured sink | `events.retention` in the database (sinks keep them as long as they keep anything) |
| Bodies | blob storage | Builds go at the reap. Each body is then collected on a collector pass at least `gc.grace` after it was last used, and its stream a grace later (spec 0016). With `gc.interval: 0`, nothing goes until `kista admin blob gc -apply` runs. |
| Attachment contents | blob storage, `attachments/` | collected as orphans by the same passes |
| Objects in versioned or object-locked buckets | the bucket | as the bucket's own policy says (spec 0016) |
| Key material | the key sources (Vault, Key Vault, a key file) | until the operator retires it; kista only signs with it |
| Statements given to subscribers | other kista servers | their choice: their feed shows `unauthorized`, and their documents stay |
| Backups | the operator's database and blob backups | their own retention |
| Logs and traces naming `kista.tenant.name` | the telemetry backend | its own retention |

The tenant's events stay until retention, and were sent to the sinks. They are the record of what
happened before the deletion, and an erasure of them is a later option (Follow-ups).

Withdrawals are not issued for offered documents. They would need the attestation key at deletion
time, and a subscriber that no longer reaches its source decides about its documents anyway.

### Rolling upgrades

Migration 0019 is `-- +min_reader 19`. That stops an older binary from starting once the migration
ran. It does not stop one that is already running, so the design does not lean on it:
- `suspended_at` is set only by this version. The migration fills it with the migration time for
  tenants that are already suspended. So no tenant can be deleted until `tenants.min_suspended` has
  passed since the upgrade: at least 24 hours outside `profile: dev`.
- An older replica sees a deleting tenant as `suspended`, and does not serve it.
- An older replica still running a day after the migration is outside what kista supports, as in
  spec 0016. The upgrade notes say so.

### Backups

Restoring a database backup from before a deletion brings the tenant back in its old state. If it
was active, it serves again, and its bodies may already be collected: those releases then fail
to download, and `blob verify` (spec 0018) reports them. After a restore, the operator:
- compares `kista admin tenant list -state deleting,deleted`, and `tenant.delete` events kept in a
  sink, with what the restored database says;
- deletes again whatever came back;
- runs `kista admin blob verify` (spec 0018).

### Interfaces

- **CLI**
  - `kista admin tenant delete <name> -confirm <name>`. The name is given twice, so a pasted command
    with the wrong tenant fails. It prints the keys to retire in their sources. It also warns when
    `gc.interval` is `0`.
  - `kista admin tenant reap <name> [-force]`. It runs the reaper in the command, under the same
    lease, and returns when the tombstone is written. Before the settle period ends it refuses and
    says how long remains. `-force` skips the wait: the operator vouches that no `kista serve` and
    no other command is running, which is the case for a SQLite replica stopped for maintenance.
  - `kista admin tenant show <name>` (new). The tenant's record. For a suspended tenant it also
    shows when it becomes deletable. For a deleting or deleted one it shows the deletion record and
    the keys to retire.
  - `kista admin tenant list [-state <s>,…]`. The default is every state but `deleted`.
- **HTTP API**
  - `DELETE /api/v1/tenants/{t}` is for server administrators, like every management route (a
    fresh token, spec 0007). `If-Match` must be the tenant's ETag; `*` is refused with `428`.
  - It answers `202` because the work continues in the background, as an upstream's or a feed's
    sync does. The body is the tenant record with `deletion`.
  - It answers `409 conflict` when the tenant is not suspended, when it is suspended for less than
    the minimum (`detail` names when it may be deleted), or when it is already deleting.
  - The tenant record gains:
    - `state`: `active`, `suspended`, `deleting` or `deleted`;
    - `suspended_at`;
    - `deletable_at`, for a suspended tenant, server administrators only;
    - `deletion`, server administrators only: `started_at`, `started_by`, `passes`, `table`,
      `deleted_rows`, `last_error`, `next_attempt_at`, `finished_at`, and `keys` (`kind`, `id`,
      `fingerprint`, `signer_ref`).
  - `GET /api/v1/tenants?state=` filters the list.
- **Console**
  - On the server's view of a suspended tenant: **Delete…**, enabled from `deletable_at`. Its
    dialog lists the keys to retire and asks for the tenant's name typed in full, as the release
    purge dialog does.
  - A deleting or deleted tenant shows a deletion view instead of the tenant's screens, which
    would all answer `404`. The view shows the state, when it started and by whom, the progress,
    the last error and the keys.
- **Configuration**
  - `tenants.min_suspended` is a duration, file-only. Its default, `168h`, is set in
    `config.Default()` as the rotation minimums are. Outside `profile: dev` it is at least `24h`;
    in dev it may be `0`. A negative value is refused.
  - The process that takes the delete applies its own value, so `kista admin` and `kista serve`
    should share the configuration.

### Events

Both are the tenant's events, like `tenant.suspend` and `tenant.resume` (spec 0010: a server
administrator's actions on a tenant are the tenant's events). A server administrator reads them
with `kista admin events list <tenant>` or `GET /api/v1/tenants/{t}/events`, both of which accept a
tombstone. Spec 0010's catalogue and the coverage test gain:

- `tenant.delete`: actor, the administrator; subject `tenant:<name>`. Data: `channels` (a
  count), `suspended_at`, and `keys`, the fingerprints (never signer references, spec 0010).
- `tenant.purge`: actor `system:tenant-delete`; subject `tenant:<name>`. Data: `rows`, `passes`,
  `late`, and `error`, a class (`lease`, `interrupted`, `error`), when the outcome is `failed`.

### Data model (migration 0019, three dialects)

- `tenants`: `suspended_at`, `deleting_at` and `deleted_at`, timestamps in each dialect's
  existing form, null. They are nullable, so an older binary's insert still works.
  - Already suspended tenants get the migration time, in the store's own UTC form:
    `now()` on PostgreSQL, `SYSUTCDATETIME()` on SQL Server, and
    `strftime('%Y-%m-%dT%H:%M:%S.000000Z', 'now')` on SQLite (the text form the store reads;
    `CURRENT_TIMESTAMP` is not).
- `tenant_deletions`, one row per deletion:
  - `tenant_id` (primary key, references `tenants`), `started_at`, `started_by` (`varchar(400)`),
    `keys` and `channel_ids` (JSON text);
  - the progress: `passes`, `current_table`, `deleted_rows`, `last_error`, `next_attempt_at`,
    `finished_at`, `last_checked_at`.
- An index on `tenants (deleting_at)` for the reaper's selection.
- `-- +min_reader 19`.

### Package layout

- `internal/store/tenantdelete.go`: the list, batches, the tombstone transaction,
  `WritableTenant`, and the deletion record.
- `internal/tenants`: `Delete` and `Show`, and the state rules for suspend and resume.
- `internal/tenants/reaper.go`: the runner, which `kista serve` starts and `tenant reap` calls.
- `internal/api`: the gate, `DELETE`, the record fields and the state filter.
- `cmd/kista/admin.go`: `tenant delete|reap|show`, and `list -state`.
- `web/console`: the delete dialog and the deletion view.

### Changes to earlier specs

- **0001.** The "events exported first" line on tenant removal is replaced by this spec's table.
- **0003.**
  - The tombstone is defined here.
  - Suspend and resume set `suspended_at` and check the current state.
  - The "nothing cascades" line is corrected.
- **0006, 0007.** The route gate; the `DELETE` route; the tenant record's fields; `?state=`.
- **0010.** `tenant.delete` and `tenant.purge` in the catalogue.
- **0011.** "Tenant deletion removes them all" now points here.
- **0015.** The delete dialog and the deletion view.
- **0016.** The tenant-deletion follow-up is done.
- **Docs.** `administration/tenants-and-channels.md` (it says there is no deletion yet),
  `commands.md`, `configuration.md`, `api.md`, `console.md`, `observability.md`,
  `backup-restore.md`, `upgrading.md`.

## Security

- **Fail closed.**
  - From the commit of the delete, the tenant answers as unknown on every serving route and every
    tenant route.
  - No write commits for it, because the shared lock serialises writes against the transition.
  - The runners skip it, and the re-signer no longer signs for it.
  - The reaper only deletes. Nothing serves or signs from a half-deleted tenant.
- **No existence leak.** A deleting or deleted tenant's `404` is byte for byte the answer for a
  name that never existed, on serving routes, API routes and the console's CORS lookup. Only
  server administrators see the difference.
- **No reuse.** The tombstone keeps the name, so a prefix that DuckDB clients trusted through
  `.well-known` never serves another tenant's keys. The fingerprints keep a key from signing for
  another tenant. The audiences keep another tenant from accepting tokens minted for this one.
- **Who can delete.** Only a server administrator, with a fresh token or from the server's shell.
  These guard against a deletion by mistake:
  - the two steps;
  - the minimum suspension;
  - an exact `If-Match`;
  - the name typed again, in the CLI and the console.

  A tenant administrator cannot delete its own tenant. In Enterest a customer's request goes
  through Enterest's own flow.
- **Isolation.**
  - Every delete is keyed by the tenant id, or by the ids of the tenant's own parent rows.
  - The store test checks that another tenant's rows survive, and that none of them references
    the deleted tenant's channels.
  - Bodies are shared within a storage domain, and only the collector removes them, by reference.
- **Signer references** are never in events. They are kept in the deletion record, which only
  server administrators read, as `…/keys` shows them today.

## Testing

- **Store suite, three dialects.**
  - The fixture is a tenant with rows in every table of the list, including a channel with a
    serving key, a release with signatures, a purged slot and both kinds of feed rows, beside a
    second tenant. Reaping the first leaves only its tombstone, deletion record, audiences,
    fingerprints and events. The second is unchanged, row for row.
  - Interrupt after N batches and run again: the pass completes.
  - A row inserted behind the reaper's back makes the tombstone transaction start again.
  - The late sweep finds a row inserted after the tombstone.
  - The completeness test fails for an unclassified test table.
  - The migration fills `suspended_at` for a tenant that was suspended before it, and
    `GetTenant` reads it back in each dialect.
  - A batch over `download_counts` (eight key columns) deletes 1,000 rows on SQL Server.
  - `WritableTenant` blocks the delete transition until the writer commits. The writer's next
    transaction is refused.
- **Services.**
  - Every write method in the coverage registry refuses a deleting tenant.
  - Resume of a deleting tenant is refused, as is suspend.
  - Delete is refused for an active tenant, one under the minimum, one with a null
    `suspended_at`, or one already deleting.
  - Two transactions that update the tenant row, under different advisory keys, do not
    deadlock.
  - A tombstone's name and fingerprints cannot be reused, and its audiences cannot be assigned
    elsewhere.
  - The upstream, feed and re-sign runners skip the tenant, and the statistics writes skip it.
  - The settle period, the backoff and the late sweep run under an injected clock.
- **API and CLI.**
  - The `DELETE` route: `If-Match` exact and `*`, `202`, the three `409`s.
  - The identical `404`s.
  - The administrator's `GET` and events on a tombstone.
  - `?state=`; `-confirm` mismatch; `tenant reap` before and after the settle period, and with
    `-force`; `tenant show`.
  - Both events, and `failed` written once per class.
- **Console.**
  - The dialog appears for a suspended tenant and is enabled from `deletable_at`.
  - It needs the typed name.
  - A deleting tenant shows the deletion view.
- **e2e.** DuckDB installs from a tenant's channel. The tenant is suspended and deleted (dev,
  `min_suspended: 0`). Then `.well-known` and the binary answer the same `404` as an unknown
  tenant. A collection end to end needs an injected clock (spec 0016), so it stays in the Go
  suites.

## Alternatives considered

- **`ON DELETE CASCADE` and one `DELETE FROM tenants`.** SQL Server refuses multiple cascade paths,
  and one transaction over millions of rows holds locks for minutes and can exhaust the log.
- **Delete inside the API request.** A large tenant takes longer than any request timeout, and a
  failure halfway leaves no record of what is left.
- **New values in `tenants.state`.** That changes the CHECK constraint, which in SQLite means
  rebuilding a table fourteen others reference, in a migration that cannot turn foreign keys off.
  An older replica would also see an unknown state.
- **Remove the row too, and allow the name again.** A client that trusted the prefix through
  `.well-known` would trust whoever takes the name next.
- **Keep everything and only hide the tenant.** The data never goes, which neither a departing
  customer nor the storage bill accepts.
- **Delete the tenant's events at once.** They are the record of what happened before the
  deletion, and they already reached the sinks. Retention bounds them.
- **Issue withdrawals for every offered document first.** That needs the attestation key at
  deletion time, and a subscriber that loses its source decides about its documents anyway.
- **Cancel during the settle period.** The minimum suspension is the window. A second undo window
  would make "deleted" mean "deleted unless …".

## Follow-ups

- Deleting a channel, with the same reaper over a channel's rows.
- Erasing a deleted tenant's events before their retention ends, for an erasure request.
- Exporting a tenant (its releases, the public halves of its keys, its grants) before suspension.
  Its principals lose the API at suspension.
- Dropping a dedicated storage domain as a whole when its only tenant is deleted.
- Enterest's self-service deletion request on top of these routes.
