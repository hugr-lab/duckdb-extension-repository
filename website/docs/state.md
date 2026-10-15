---
title: State
---

# State

kista keeps its metadata in one database: tenants, channels, keys and their states, DuckDB versions,
releases and their signatures, issuers, grants, publishers, upstreams, attachments, events and
download statistics. Extension bodies are not in the database; they live in blob storage (see
[Storage](storage.md)). Signing keys are not in it either: only their public keys and signer
references are (see [Signing keys](signing-keys.md)).

Three engines are supported through one dialect layer:

| Engine | `store.kind` | Use |
| --- | --- | --- |
| PostgreSQL (tested on 17) | `postgres` | Production, one or more replicas. |
| SQL Server (tested on 2022), Azure SQL | `sqlserver` | Production, one or more replicas. |
| SQLite (pure Go driver, no cgo) | `sqlite` | One process: development, small single-node installs. |

The settings are listed in [Configuration](configuration.md#store).

## SQLite

```yaml title="kista.yaml"
store:
  kind: sqlite
  sqlite:
    path: /var/lib/kista/kista.db
```

- The file is created if it does not exist, with mode `0600`; an existing file with wider
  permissions is changed to `0600`. The `-wal` and `-shm` files get the same mode.
- The path may not contain `?`, `#` or `%`.
- The database runs in WAL mode with `synchronous=FULL` and foreign keys on. WAL does not work on
  network filesystems: put the file on a local disk.
- Every transaction takes the database's write lock at its start, so writes are serialised. Plain
  reads run outside transactions.
- **SQLite means one server process.** Do not run two `kista serve` processes on one file. `kista
  admin` may run beside the server as the same OS user: SQLite's locking covers the two writers.
- `store.max_open_conns` and `store.login` do not apply. `store.login.kind: entra` is refused.
- With `profile: dev` and an absolute `sqlite.path`, the default blob root and spool directory are
  `blobs/` and `spool/` beside the database file.

Back a SQLite store up with:

```bash
kista admin -config /etc/kista/kista.yaml backup /var/backups/kista-2026-10-12.db
```

It writes a consistent copy (`VACUUM INTO`) into a new `0600` file and never over an existing one.
See [Backup and restore](administration/backup-restore.md).

## PostgreSQL

```yaml title="kista.yaml"
store:
  kind: postgres
  dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full
  login: { kind: password, password_file: /run/secrets/kista-db-password }
  max_open_conns: 10
```

- The DSN is anything pgx accepts: a URL or `key=value` pairs, multi-host DSNs included.
- **Off loopback, every host needs `sslmode=verify-full`**: TLS with the certificate verified and
  the server name checked. This is judged on the driver's parsed configuration, so a repeated or
  contradicting parameter cannot slip past it, and every fallback host of a multi-host DSN counts.
  On loopback (or a Unix socket path) any `sslmode` is accepted.
- `PGPASSWORD` and `.pgpass` are never used: the password comes only from `store.login`.
- The server's CA: the DSN's `sslrootcert`, or `store.ca_pem` inline (not both, and not with
  `PGSSLROOTCERT`), or else the system roots. `ca_pem` needs a host reached over TLS.
- The connect timeout is 10 seconds unless the DSN sets `connect_timeout`.

## SQL Server

```yaml title="kista.yaml"
store:
  kind: sqlserver
  dsn: sqlserver://kista@sql.internal:1433?database=kista&encrypt=strict
  login: { kind: password, password_env: KISTA_DB_PASSWORD }
```

- **Off loopback, encryption must be `encrypt=strict`, or `encrypt=true` with certificate
  verification** (no `TrustServerCertificate=true`). The host and the failover partner are both
  checked.
- Every statement is unqualified and resolves in the login's default schema.
- Names use a binary collation (`Latin1_General_100_BIN2`), so `Prod` and `prod` are different
  names, as on the other engines.

## Logins

A DSN never holds a password. The loader refuses a DSN with URL user info containing a password, a
`password` or `pwd` query parameter, or a `password=` / `pwd=` keyword, and the store refuses it
again when it opens. The password (or token) comes from `store.login`, asked for on every new
connection:

| `store.login` | Where the secret comes from |
| --- | --- |
| `kind: password`, `password_env: NAME` | The environment variable `NAME`, read on every new connection. Unset or empty is an error naming the variable. |
| `kind: password`, `password_file: /path` | The file, read on every new connection; trailing newlines are trimmed. It is a secret file ([Files kista reads](security.md#files-kista-reads)): owned by kista's user with mode `0600` or narrower, or root-owned with kista's group and mode `0440` or narrower. |
| `kind: password` with neither | No password (for example trust or peer authentication on a socket). |
| `kind: entra` | A Microsoft Entra token from the service's Azure identity (`azure.identity`), for Azure Database for PostgreSQL Flexible Server or Azure SQL. |
| `kind: aws` | An RDS IAM authentication token, signed locally with the service's AWS identity (`aws`) for the connection's host, port and user. PostgreSQL on RDS or Aurora. |
| `kind: gcp` | A Cloud SQL IAM authentication token of the service's Google principal (`gcp`), scoped to `sqlservice.login` only. PostgreSQL on Cloud SQL. |

`entra`, `aws` and `gcp` take no `password_env` or `password_file`: the token is the password.

Because the secret is read per connection, a rotated password (a re-mounted secret file, or a
changed variable in a process that re-reads it) is picked up without a restart, as soon as the pool
opens a new connection. Connections are recycled after 30 minutes at most.

The variable named by `password_env` may itself start with `KISTA_` (`KISTA_DB_PASSWORD`); the
loader then treats it as the password, not as an unknown setting.

For `entra`, configure the identity and, outside dev, give `workload` its ids:

```yaml title="kista.yaml"
store:
  kind: postgres
  dsn: postgres://kista-identity@kista-pg.postgres.database.azure.com:5432/kista?sslmode=verify-full
  login: { kind: entra }
azure:
  identity: { kind: managed, client_id: 00000000-0000-0000-0000-000000000000 }
```

The token scope is `https://ossrdbms-aad.database.windows.net/.default` for PostgreSQL and
`https://database.windows.net/.default` for SQL Server. `azure.cloud` (`public`, `china`, `usgov`)
picks the Entra authority. The database role or user mapped to the identity is created in the
database as usual for Entra authentication.

For `aws`, the token is made for each new connection and is good for 15 minutes; a connection
outlives it. It is bound to one host, so the DSN may name only one. The database user is granted
`rds_iam`, and the identity `rds-db:connect` on it:

```yaml title="kista.yaml"
store:
  kind: postgres
  dsn: postgres://kista@kista.abcdefghijkl.eu-central-1.rds.amazonaws.com:5432/kista?sslmode=verify-full
  login: { kind: aws }
  ca_pem: |
    -----BEGIN CERTIFICATE-----
    …the region's RDS CA bundle…
    -----END CERTIFICATE-----
aws:
  region: eu-central-1
  identity: { kind: ecs }
```

For `gcp`, the database user is Cloud SQL's name for the IAM user: the principal without
`.gserviceaccount.com`, which the loader checks. The token never carries the `cloud-platform`
scope, so the database server never holds a token good for other APIs. An instance whose
certificates come from `GOOGLE_MANAGED_CAS_CA` is named with a trailing dot in its certificate
(`….sql.goog.`, or `….sql-psa.goog.` on private services access): write the host with that dot,
and `verify-full` matches it. On private services access the name resolves nowhere until a
private Cloud DNS zone serves it ([GCP](deployment/gcp.md)).

```yaml title="kista.yaml"
store:
  kind: postgres
  dsn: host=abc123.def456.europe-west4.sql-psa.goog. user=kista@acme-kista.iam dbname=kista sslmode=verify-full
  login: { kind: gcp }
  ca_pem: |
    …the instance's server CA…
gcp:
  principal: kista@acme-kista.iam.gserviceaccount.com
```

## Connections

| Setting | Effect |
| --- | --- |
| `store.max_open_conns` (default `10`) | Open connections per process; the same number is kept idle. Every replica and every `kista admin` run has its own pool, so size the database's connection limit for replicas × `max_open_conns` plus the CLI. |
| Connection lifetime | 30 minutes, then the connection is replaced. |
| Start-up ping | The database must answer within 30 seconds, or the start fails with `store: cannot reach the database`. |
| Lock wait | 10 seconds (PostgreSQL `lock_timeout`, SQL Server `LOCK_TIMEOUT` and `sp_getapplock`). |

Driver errors are cleaned so they never echo a connection string or a password.

## Transactions and locks

kista needs no external lock service; the database is the coordination point.

- Every mutable row carries a `version` column, and updates are compare-and-set: two administrators
  changing the same thing at once end with one change and one conflict, never a lost update.
- Work that must be serialised takes a named lock as the first statement of its transaction:
  `pg_advisory_xact_lock` on PostgreSQL, `sp_getapplock` (transaction owner) on SQL Server. On
  SQLite every transaction already holds the database write lock.
- A retryable engine error (deadlock, serialisation failure, lock timeout, `SQLITE_BUSY`) reruns
  the whole transaction up to 8 times with jitter, then reports a conflict. A compare-and-set
  conflict is reported at once.

## Leases

Background work that should run on one replica at a time holds a **lease**: a row in the `leases`
table with a holder (the replica's host name plus a random id), an expiry and a version. A replica
takes a lease that is free or expired, renews it while it works, and releases it when done; a
replica that dies simply lets it expire. So does a replica whose runner is still busy when a
shutdown's wait for it ends (`serve.shutdown_timeout`). Leases in use:

| Lease | Work |
| --- | --- |
| `kista/upstream/<id>` | One upstream's run (mirror or sync). |
| `kista/upstream/<id>/<duckdb version>/<platform>/<name>` | One pull-through fetch; deleted when done. |
| `resign/<channel id>` | The re-signer on a channel (replicas with `serve.resign: true`). |
| `kista/feed/<id>` | One feed's run. |
| `kista/events/retention` | Event buffer retention. |
| `kista/events/sink/<name>` | Delivery to one event sink. |
| `kista/statistics/daily` | The daily statistics pass. |
| `kista/blob/gc/<domain>` | Storage garbage collection of a domain. |
| `kista/tenant-delete/<id>` | The tenant reaper on one deleting tenant: its rows in batches, later the sweeps of its tombstone. |

Leases compare times written by different replicas, so keep the replicas' clocks synchronised
(NTP). A released lease is set slightly in the past (5 seconds) so a replica whose clock is a little
behind can take it at once.

## Several replicas

Any number of `kista serve` replicas can run against one PostgreSQL or SQL Server database. What
they must share and what stays per replica:

| Shared by every replica | Per replica |
| --- | --- |
| The database (`store`) | `blob.spool_dir`: local scratch; upstream downloads a crash left there are removed at start |
| The blob storage domains (`blob.domains`): every replica reads and writes the same bodies, so an `fs` domain must be a shared volume | Listeners and TLS files |
| The key sources (`signers`) every replica signs through | `serve.resign`: turn it on only on replicas allowed to sign; leases keep each channel's re-signing on one of them |
| The same configuration otherwise (issuers, egress, sinks, ...) | Download and API limits (`serve.max_downloads` and the others count per replica) |

Scheduled work (upstream runs, feeds, event retention and sinks, statistics, garbage collection) is
polled by every replica and run by the lease holder. Requests made with `kista admin upstream sync`
or `kista admin feed sync` are recorded in the database and taken by a running replica.

SQLite cannot be shared: with SQLite, run exactly one `kista serve`.

## Migrations

The schema is created and upgraded by migrations embedded in the binary, one numbered file per
step and per engine (`internal/store/migrations/<engine>/NNNN_name.sql`). Applied steps are
recorded in the `schema_migrations` table.

| `store.migrate` | At start |
| --- | --- |
| `auto` (default) | Applies pending migrations, then starts. |
| `check` | Changes nothing. Refuses to start if migrations are pending or the database needs a newer binary. |

Use `check` when the runtime identity has no DDL rights, and run the migration as a separate job
with an identity that has them:

```bash
kista admin -config /etc/kista/kista.yaml migrate   # apply pending migrations
kista admin -config /etc/kista/kista.yaml check     # verify only; prints "schema ok"
```

Both commands override `store.migrate` for that run. Every migration run takes the
`kista/migrate` lock first and applies each step in its own transaction, so replicas starting
together never apply a step twice.

**`min_reader`.** Each migration declares the oldest schema level a binary must know to keep
working with the database after it. The database keeps the highest such value. A binary refuses to
start, with `store: the database needs a newer kista binary`, only when that value is above the
newest migration it knows. An additive migration keeps `min_reader` where it was, so an older
replica can keep running and restart during a rolling upgrade; a migration that changes or removes
something older code relies on raises it. Many migrations so far have raised it; the newest,
`0019_tenant_deletion`, raised it to 19, so today a binary that does not know migration 19 does
not start against a database that has it. See [Upgrading](administration/upgrading.md) for the
procedure.

`/readyz` checks the database and the schema level on every replica, so a replica whose binary no
longer fits the database is taken out of rotation.

## Backups

PostgreSQL and SQL Server are backed up with their own tools. SQLite uses `kista admin backup`
(above). Restoring a backup returns key states to that point in time, so a restored database can
lack a key that clients already trust; see [Backup and restore](administration/backup-restore.md).
