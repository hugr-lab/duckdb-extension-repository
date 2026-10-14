---
title: Upgrading
---

# Upgrading

Upgrading kista is replacing the binary. The database schema moves forward with embedded
migrations; the rules below decide when a binary may start against a database and how several
replicas run through an upgrade. The design is in
[spec 0003](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0003-store-tenants-keys/spec.md)
("Migrations").

There is no container image or package yet: build the new binary with `make build` (`bin/kista`) or
`go build ./cmd/kista` (see [Development](../development.md)).

## Migrations

- Each migration is an SQL file embedded in the binary, one per database engine (PostgreSQL, SQL
  Server, SQLite), numbered `0001`, `0002`, and so on. They live in
  `internal/store/migrations/<engine>/`.
- Each migration runs in its own transaction. Every run first takes the lock `kista/migrate`, so
  replicas starting together never apply one twice.
- Applied migrations are recorded in the table `schema_migrations` (`version`, `name`,
  `min_reader`, `applied_at`).
- A binary's **schema level** is the number of migrations it carries. This release carries 19.

### Who migrates: `store.migrate`

| Value | At start (`kista serve` and every `kista admin` command) |
| --- | --- |
| `auto` (default) | applies pending migrations, then starts |
| `check` | changes nothing; refuses to start if migrations are pending or the database needs a newer binary |

Use `check` when the runtime database identity has no DDL rights. Then a separate job with DDL
rights runs the migrations before the new binary starts:

```bash
kista admin -config /etc/kista/migrate.yaml migrate     # applies pending migrations; prints "schema ok"
kista admin -config /etc/kista/kista.yaml check         # verifies, changes nothing; prints "schema ok"
```

`kista admin migrate` and `kista admin check` ignore `store.migrate` and do what they say. Every
other `kista admin` command opens the store with the configured mode: with `auto`, running a newer
binary's `kista admin` against a database **migrates it**. Keep old and new binaries apart on
operator hosts during an upgrade.

## Compatibility: `min_reader`

A database is not refused just because it is newer than the binary. Each migration declares
`-- +min_reader N`: the oldest schema level that can still work with the database after it. The
database keeps the highest value seen, and a binary refuses it only when that value is above its
own schema level.

- An **additive** migration (new tables, nullable columns, indexes) keeps `min_reader` where it was:
  an older binary keeps working against the migrated database.
- A migration whose new rules an older binary would break raises `min_reader` to its own number:
  from then on, only binaries that carry it may run.

Readiness follows the same rule. `/readyz` is recomputed every 5 seconds and fails when:

- the database needs a newer binary (`min_reader` above this binary's level); a replica already
  running stays up but drops out of readiness;
- migrations are pending for this binary (with `store.migrate: check`, before the migration job
  ran).

### Migrations that raise `min_reader`

| Migration | `min_reader` | Why an older binary must stop |
| --- | --- | --- |
| `0001_init` | 1 | the first schema |
| `0003_releases` | 3 | an older `kista admin` would retire a serving key or add a DuckDB version without its C API rows |
| `0005_publication` | 5 | an older binary would ignore blocks |
| `0006_publishers` | 6 | an older binary cannot read grants without an issuer (publisher grants) |
| `0008_upstreams` | 8 | an older replica would publish past the reservation of upstream names |
| `0010_events` | 10 | an older replica would change things without recording events |
| `0012_upstream_credential` | 12 | an older replica would make a credentialed upstream's releases public |
| `0013_storage_gc` | 13 | storage GC: commits must claim streams before the collector runs |
| `0014_purged_slots` | 14 | an older replica would refill the slot of a purged release |
| `0018_feeds` | 18 | an older replica would mark documents feedable without issuing their signed statements |
| `0019_tenant_deletion` | 19 | an older replica would let a deleting tenant be resumed and keep running its upstreams and feeds |

Migrations 0002, 0004, 0007, 0009, 0011, 0015, 0016 and 0017 are additive and keep the previous
value.

## Upgrading one server

1. Back up the database ([Backup and restore](backup-restore.md)).
2. Stop `kista serve`.
3. Replace the binary.
4. With `store.migrate: check`, run `kista admin migrate` with the migration identity.
5. Start `kista serve`. With `auto` it migrates first. Watch `/readyz`.

## Rolling upgrades with several replicas

With several replicas on one database, the first new replica that starts (with `auto`), or the
migration job, applies the migrations. What happens to the old replicas depends on the highest
`min_reader` among the new migrations.

**Additive migrations only** (`min_reader` unchanged): old and new replicas run side by side.
Replace replicas one at a time as usual.

**A migration that raises `min_reader` above the old binary's level** (for example `0018_feeds`
when upgrading from a binary of level 17 or lower):

- as soon as it is applied, old replicas fail `/readyz` within about 5 seconds and cannot restart
  on the old binary;
- so the upgrade requires **every** replica to run the new binary. Load balancers that follow
  `/readyz` send traffic only to new replicas; capacity is reduced until all old replicas are
  replaced;
- plan for it: with `store.migrate: check`, a new replica refuses to start until the migration job
  has run, so run the job and then replace the replicas as quickly as the platform allows; or
  upgrade all replicas in one maintenance window;
- an old replica that is still running is not stopped by kista. Replace it promptly: it still runs
  background work (re-signing, upstream and feed runs) with the old rules.

Several upgrades at once (for example from level 12 to 18) apply every pending migration in order;
the highest `min_reader` among them decides.

### Features that wait for every replica

Some features change what every replica must do. kista cannot see which binary each replica runs,
so the operator declares it:

- **`gc.interval`** stays `0` (the default) until every replica runs a binary with migrations 0013
  and 0014. While it is `0`, there is no scheduled garbage collection, `kista admin blob gc -apply`
  needs `-force`, and purging a yanked release is refused (`release purge` needs `-force` from the
  CLI). Set `gc.interval` (for example `24h`) once the rollout is complete. See
  [Storage](../storage.md).
- **Deleting a tenant** (migration 0019) needs no setting. Only a binary with migration 0019
  records when a tenant was suspended, and the migration gives tenants that are already suspended
  the migration time. So no tenant can be deleted until `tenants.min_suspended` (at least 24 hours
  outside `profile: dev`) has passed since the upgrade. An older replica that is still running
  sees a deleting tenant as suspended and does not serve it; one still running a day after the
  migration is outside what kista supports. A tenant suspended by an older binary while the
  rollout ran has no suspension time: resume it and suspend it again before deleting it. The
  reaper runs only on a replica whose binary matches the database's schema level exactly. See
  [Deleting a tenant](tenants-and-channels.md#deleting-a-tenant).

## Upgrading to this release

### Files kista reads

Every file the configuration names now follows one rule
([Files kista reads](../security.md#files-kista-reads)). Check the files before the upgrade: a
file that breaks its rule stops `kista serve` at start, or fails the read that needs it, with an
error naming the file and the rule.

**Files that had no check before** and now have one:

- secrets: `store.login.password_file`, the listener `tls.key_file`, the OTLP `headers_file`, and
  the upstream credential `client_secret_file` and `key_file`;
- tokens: the upstream credential `token_file` and `assertion_file`;
- public files: `egress.ca_file`, the Vault `ca_file`, the S3 `ca_file` and the listener
  `tls.cert_file`.

**What may now be refused:**

- **Owners of public files and tokens.** A public file or a token must be owned by root or by
  the user kista runs as. A CA bundle on the host owned by another user is refused; so is a token
  file written by another user.
- **Readable directories.** A secret or a token is opened inside its directory, which kista must
  be able to read (the `r` bit, not only `x`). `/etc/ssl/private` at `0710 root:ssl-cert` is not
  readable, for example.
- **Links.** A secret's or a token's link must be relative and stay inside the file's directory;
  an absolute link is refused even when it points inside. certbot's `live/privkey.pem` leaves its
  directory: point `tls.key_file` at the real file or a copy.
- **Hard links.** A secret or a token must have exactly one.
- **Sizes.** An S3 or Azure Blob key file over 4 KiB used to be cut at 4 KiB; it is now refused.
  A Vault token file over 64 KiB likewise.

**What is now accepted:**

- A Vault `kubernetes` or `jwt` token file no longer needs mode `0600` or narrower: like every
  token, any read bits on a file owned by root or kista's user.
- The AWS static key files (`identity.access_key_file`, `identity.secret_key_file`) may be
  relative links inside their directory; any link used to be refused.
- Secrets may be root-owned with kista's group and mode `0440` or narrower: a Kubernetes Secret
  volume under `fsGroup`.

## Downgrading

A binary runs against a newer database only while the database's `min_reader` is at most the
binary's schema level. Migrations have no down steps. To go back past a migration that raised
`min_reader`, restore the database backup taken before the upgrade
([Backup and restore](backup-restore.md)), with the consequences described there.

## Checklist

- Read the release's migrations: which ones raise `min_reader`.
- Back up the database.
- Check the files the configuration names against
  [Files kista reads](../security.md#files-kista-reads) (what changed: [above](#files-kista-reads)).
- Decide who migrates (`auto` on the first replica, or a `kista admin migrate` job).
- Run `kista admin check` with the new binary afterwards: `schema ok`.
- Watch `/readyz` on every replica, and the server events
  (`kista admin events list -server`).
- After the rollout, set or keep `gc.interval` as described above.
