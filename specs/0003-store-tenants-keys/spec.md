# Spec 0003: Store, tenants, channels, signing keys and rotation

- **Status**: implemented
- **Date**: 2026-10-07
- **Author**: vgsml, Claude

## Summary

kista's first persistent state:

1. `internal/store`: metadata on PostgreSQL, SQL Server or SQLite through one dialect layer, with
   embedded per-dialect migrations, a schema-compatibility level for rolling upgrades, and
   compare-and-set writes. A shared test suite runs on all three engines.
2. **Tenants**, **channels** (`signed` or `passthrough`, with the DuckDB versions they serve) and
   **channel signing keys**. Each key belongs to exactly one channel and moves through a rotation
   lifecycle `trusted` → `active` → `trusted` → `retired`, recorded in an append-only event table.
3. A channel's `.well-known/duckdb-extension-repo.json` document, built from its keys.
4. `internal/config`, and `kista admin`: the server administrator's CLI over the same service layer
   the HTTP API will use.

Signer backends (vaults and KMSs behind named key sources) are spec 0004. The HTTP management API needs authentication and comes with
spec 0006. Here, keys use spec 0002's file signer, which is limited to development or to a
configured key directory.

## Problem

Spec 0002 signs and verifies, but nothing yet records which tenants and channels exist, which keys a
channel trusts, or which one signs. Rotation (spec 0001, "Signing keys") is a procedure that runs
over time. DuckDB pins keys at `CREATE` and checks them on every `LOAD` (spec 0002, e2e cases 5 and
6), so that procedure needs state that survives restarts and replicas.

## Design

### Store

**Engines.**
- PostgreSQL (pgx v5 through `database/sql`);
- SQL Server (go-mssqldb ≥ 1.11);
- SQLite (modernc.org/sqlite ≥ 1.46, no cgo).

The implementation is written for kista. It follows the same design as tresor-server's store and
copies no code from it, because tresor-server is under BUSL-1.1.

**Dialect.**

| | PostgreSQL | SQL Server | SQLite |
| --- | --- | --- | --- |
| placeholders (queries use `?`) | `$1…` | `@p1…` | `?` |
| unique violation | 23505 | 2627, 2601 | `SQLITE_CONSTRAINT_UNIQUE`, `_PRIMARYKEY` |
| foreign-key violation | 23503 | 547 | `SQLITE_CONSTRAINT_FOREIGNKEY` |
| retryable | 40001, 40P01, 55P03 (lock timeout) | 1205, 1222, 51000 (lock) | `SQLITE_BUSY`, `SQLITE_LOCKED` (after `busy_timeout`) |
| `Lock(tx, key)` | `pg_advisory_xact_lock(hashtextextended(key, 0))` | `EXEC @r = sp_getapplock …, @LockOwner='Transaction', @LockTimeout=10000; IF @r < 0 THROW 51000…` (the return code is checked, the driver raises nothing) | none: `_txlock=immediate` already holds the write lock |
| session (first statement of every transaction) | `lock_timeout` | `SET XACT_ABORT ON; SET LOCK_TIMEOUT …` (`SessionInitSQL` only runs on a pool reset) | `foreign_keys(1)`, `journal_mode(WAL)`, `busy_timeout(10000)`, `_txlock=immediate` |

**Transaction rules:**
- `Lock` is the first statement of any transaction that needs it.
- Any error rolls the whole transaction back. There is no "insert, catch the unique violation,
  continue" inside a transaction.
- On SQLite every `BeginTx` takes the write lock, so plain reads run outside transactions.
- A retryable engine error (deadlock, serialization failure, lock timeout, `SQLITE_BUSY`) reruns
  the whole transaction function up to 8 times with jitter, then returns `ErrConflict`. Functions
  read what they need inside the transaction. A compare-and-set conflict is returned at once: a
  stale version never succeeds by repeating it.
- Every mutable row has a `version` column. An update is `… WHERE id = ? AND version = ?`, and zero
  rows affected means a conflict.

**Migrations.**
- Each migration is a file `internal/store/migrations/<dialect>/NNNN_name.sql`, embedded in the
  binary, with one parallel file per dialect.
- A file is split on `-- +statement` lines and its statements run one by one in one transaction. SQL
  Server cannot use a column in the batch that adds it, and some drivers do not run multi-statement
  text.
- Every run starts with `Lock("kista/migrate")`. Under that lock it creates `schema_migrations` if
  missing (SQL Server: `IF OBJECT_ID(N'schema_migrations', N'U') IS NULL CREATE TABLE …`, unqualified
  like every other statement, so it resolves in the login's default schema), reads the applied
  versions, and applies the rest.
- **Compatibility, not equality.** Each migration file declares `-- +min_reader N`: the oldest binary
  schema level that can still work with the database after it. The database keeps the highest
  `min_reader` seen. A binary refuses to start only when that value exceeds its own schema level.
  - Additive migrations (tables, nullable columns, indexes) keep `min_reader` where it was.
  - A contracting migration raises it, and ships at least one release after the code stopped using
    the old shape.

  So an old replica can restart during a rolling upgrade.
- **Who migrates.** `store.migrate: auto` (default) migrates at start. `check` only verifies, for
  deployments whose runtime identity has no DDL rights; there, `kista admin migrate` runs as a
  separate job.
- Statements that cannot run in a transaction (`CREATE INDEX CONCURRENTLY`, `ALTER DATABASE`) need a
  non-transactional migration kind, added when the first one is needed.

**Connections.**
- A DSN must not contain a password. A `Login` supplies one per new connection:
  - `password_env` / `password_file` is re-read on every connection;
  - `entra` takes a token for PostgreSQL Flexible Server or Azure SQL from managed or workload
    identity.
- Off loopback, PostgreSQL requires `sslmode=verify-full`, and SQL Server `encrypt=strict` (or
  `true` with certificate verification).
  - This is judged on the driver's parsed configuration, never on the DSN text: the last of
    repeated parameters wins, as in the driver.
  - Every host the driver may use counts: pgx's multi-host fallbacks and SQL Server's failover
    partner.
  - The dev profile's "loopback only" rule uses the same parsed check (`store.DSNIsLocal`).
  - Lock waits are bounded by `store.LockTimeout` (default 10 s; PostgreSQL `lock_timeout`, SQL
    Server `LOCK_TIMEOUT` and `sp_getapplock`). A transaction that panics is rolled back, so its
    locks are released.
- SQLite:
  - the file is pre-created with mode `0600`, so its `-wal`/`-shm` files get the same mode. The path
    may not contain `?`, `#` or `%`, which would change the driver's `file:` URI;
  - WAL mode does not work on network filesystems;
  - SQLite means one server process. `kista admin` runs as the same OS user, and SQLite's locking
    covers the two writers.
  - `kista admin backup <file>` (`VACUUM INTO`) makes a consistent copy into a new `0600` file, never
    over an existing one. PostgreSQL and SQL Server use their own backups.
- Restoring a backup reverts key states to that point, so a restored database can lack a key that
  clients already trust. That is a documented operational risk.

**Readiness.** `store.Ready(ctx)` pings the database and checks the schema level. Spec 0006 wires it
to `/readyz`.

**Errors.** `ErrNotFound`, `ErrExists`, `ErrConflict`, `ErrInvalid`. No raw SQL exists outside the
package.

### Column types

| Kind | PostgreSQL | SQL Server | SQLite |
| --- | --- | --- | --- |
| id: UUIDv7, generated in Go, canonical lowercase | `varchar(36)` | `nvarchar(36) COLLATE Latin1_General_100_BIN2` | `TEXT` |
| name, enum, fingerprint, version string | `varchar(n)` | `nvarchar(n) COLLATE Latin1_General_100_BIN2` | `TEXT` |
| timestamp: generated in Go, UTC, microseconds | `timestamptz` | `datetime2(6)` | `TEXT`, RFC 3339 with 6 fraction digits and `Z` |
| public key (SPKI DER, 294 bytes) | `bytea` | `varbinary(1024)` | `BLOB` |
| counters, versions | `bigint` | `bigint` | `INTEGER` |

The binary collation on SQL Server makes comparisons byte-exact, as on the other two engines. The
default collation is case-insensitive and ignores trailing spaces. Strings are `nvarchar`, because
the driver sends `nvarchar` parameters and a `varchar` column would turn indexed lookups into scans.

Timestamps come from the process clock, never from the database (`GETDATE()` is local time). Hosts
are assumed to be NTP-synchronized. Enum columns carry `CHECK` constraints in all three dialects.

### Data model (migration 0001)

```text
tenants           id pk, name unique, display_name, state ('active'|'suspended'), created_at, version
duckdb_versions   id pk, name unique,        -- a release tag (v2.0.0) or a 10-char source id
                  kind ('release'|'dev'), c_api_version null, created_at
channels          id pk, tenant_id fk, name, kind ('signed'|'passthrough'), created_at, version
                  unique (tenant_id, name); unique (id, tenant_id)
channel_duckdb_versions   channel_id fk, duckdb_version_id fk;   pk (channel_id, duckdb_version_id)
key_fingerprints  fingerprint pk, key_id     -- one fingerprint per server, across every key table
channel_keys      id pk, tenant_id, channel_id, fingerprint unique, signer_ref, public_key,
                  state ('trusted'|'active'|'retired'), trusted_since, state_changed_at,
                  created_at, created_by, state_changed_by, version
                  fk (channel_id, tenant_id) → channels (id, tenant_id)
                  unique (channel_id) where state = 'active'
                  index (tenant_id)
key_events        id pk, key_id fk, from_state null, to_state, actor, forced, at   -- append-only
```

**Names and ids.**
- Tenant and channel names are URL path segments (`/<tenant>/<channel>`): `[a-z0-9][a-z0-9-]{0,62}`.
- Reserved names: `api`, `admin`, `healthz`, `readyz`, `metrics`, `static`, `ui`.
- Names never change and are never reused. There is no deletion in this spec, and a later deletion
  keeps a tombstone.
- A channel's `kind` never changes.

**Tenant state.** `suspended` is reserved for subscriptions. Spec 0006 refuses to serve a suspended
tenant.

**DuckDB versions** are a global table. Spec 0001's rule "a `C_STRUCT` build is served under every
DuckDB version of the channel whose C API is at least the build's minimum" needs each version's C
API level, and a release tag and a source id can name the same engine build.

**Key fingerprints.**
- `key_fingerprints` is inserted in the same transaction as the key, so a fingerprint exists once on
  the server.
- Licence keys (spec 0012) will insert into the same registry. That is spec 0002's key-role
  separation, and it holds before licence keys exist.
- A key belongs to one channel, so the same key never serves two channels or two tenants.

**Constraints.**
- **One active key per signed channel**, by the partial unique index: PostgreSQL partial index, SQL
  Server filtered index, SQLite partial index. `channel_id` is never null, so SQL Server's NULL
  semantics do not arise.
- **Foreign keys** are `NO ACTION`; nothing cascades. SQL Server refuses multiple cascade paths, and
  deletions will be explicit application steps.
- **Passthrough channels** hold no keys.

**Who changed what.** `created_by` / `state_changed_by` and `key_events.actor` hold an actor string:
- `os:<uid>:<name>` from the CLI (`os/user.Current()`);
- `principal:<issuer-id>|<sub>` from the HTTP API (spec 0006);
- `system`.

Spec 0010's audit log supersedes `key_events` as the record, and `key_events` stays as the key
history.

**Caching.** Every key-state change and every change to a channel's DuckDB versions increments
`channels.version` in the same transaction. A server process caches a channel's `.well-known` and
signer under that version and revalidates it with a short TTL (spec 0006). This is how changes made
by `kista admin` reach running servers on all three engines; PostgreSQL's LISTEN/NOTIFY is not
available everywhere.

### Key lifecycle

```text
          add                 activate              activate another         retire
(none) ─────────▶ trusted ──────────────▶ active ──────────────────▶ trusted ─────────▶ retired
```

| State | In `.well-known` | Signs new releases |
| --- | --- | --- |
| `trusted` | yes | no |
| `active` | yes | yes, exactly one per signed channel |
| `retired` | no | no |

Every transition takes `Lock("kista/channel/<id>")` first, writes a `key_events` row, and increments
`channels.version`.

- **add.**
  1. The signer reference is opened (see "Signer references") and its public key checked: RSA-2048,
     `e = 65537`.
  2. A **probe signature** over a fixed body hash, `SHA-256("kista key probe")`, must verify. This
     proves the key can sign and that kista can use it.
  3. The fingerprint must be new on the server. The channel may hold at most 16 non-retired keys, and
     the resulting `.well-known` must fit in 64 KiB.
  4. The key starts `trusted`, with `trusted_since = now`.
  5. Only a channel that has never had a key may get its first key directly as `active`: no client
    trusts the channel yet.
- **activate.** In two statements, by primary key:
  1. the current active key becomes `trusted`;
  2. the new one becomes `active`.

  PostgreSQL and SQLite check the partial unique index row by row, so the demotion must come first,
  and a single `UPDATE … CASE` swap is not used.

  Activation requires the key to have been trusted for `rotation.min_trusted`, measured from
  `trusted_since`, which a demotion never resets. `--force` skips the wait and is recorded as forced.
  A key demoted by a rollback can be activated again at once, because clients already trust it.
- **retire.**
  - It is refused for the active key.
  - It is refused for a key demoted less than `rotation.min_demoted` ago (default 7 days), unless
    `--force`. Clients that installed with it need time to reinstall.
  - From spec 0008 on, retire is also refused while any release of the channel is signed only by
    this key.
  - A retired key never comes back. A new key is added instead.
  - A key that was never active waits `min_demoted` from when it was added. It has been published in
    `.well-known`, so clients may already trust it.
- **Re-signing** with the new active key happens per release, from spec 0008 on. That spec also
  re-reads the key state in the same transaction that records a signature, so a concurrent
  activation cannot leave signatures from the old key recorded after rotation.

`rotation.min_trusted` (default 7 days) and `rotation.min_demoted` (default 7 days) cannot be set
below 24 h outside `profile: dev`. The trusted period only counts once spec 0006 serves
`.well-known`. Keys added before that have not really been seen by clients.

### Signer references

A key's `signer_ref` says where its private key lives. **Only the server administrator sets it**,
through `kista admin` or config. The HTTP API never accepts one from a tenant: spec 0006 provisions
tenant keys itself. A reference chosen by a caller would let the server read arbitrary files, or
call arbitrary vaults with its own identity.

- `file:<name>` resolves to `<signers.file_dir>/<name>`. `<name>` is a single path element, with no
  separators or `..`. It is allowed only with `profile: dev` or `signers.allow_file: true`. The file
  signer's own checks apply too: `O_NOFOLLOW`, mode `0600`, the owner.
- Vault and KMS references (`<source>:<key>`, named key sources) come with spec 0004.
- **Every open checks the key against the stored row.** The signer's public key (SPKI DER) must
  equal the row's `public_key`, which is what `.well-known` publishes, and its fingerprint must equal
  the row's `fingerprint`. Otherwise the signer fails closed. A changed file or an edited reference
  never signs with a key nobody registered.
- **The database is the trust root for `.well-known`.** Whoever can write the database can add a key
  that clients will pin at `CREATE`. Database credentials are protected accordingly.

### `.well-known`

`WellKnown(channel)` returns `{"signature_keys": [...]}` with the SPKI PEM of every `active` and
`trusted` key:
- the active key comes first, then trusted keys by `trusted_since`, then by fingerprint. The output
  is byte-for-byte deterministic;
- the 64 KiB limit applies to the exact serialized bytes and is also checked at `add`;
- a signed channel with no key returns an error, because DuckDB requires a non-empty array;
- a passthrough channel has none.

Spec 0006 serves it.

### Service layer and authorization

`internal/keys` and `internal/tenants` are the services. Every method takes
`(ctx, actor Actor, …)` and calls an `Authorizer` before acting:

```go
type Actor struct{ Kind ActorKind; ID string } // os | principal | system
type Authorizer interface {
    Allow(ctx context.Context, a Actor, verb Verb, tenant, channel string) error
}
```

The CLI uses an authorizer that allows the server administrator everything. Spec 0006 adds the
grant-based one. Keys are addressed as `<tenant>/<channel>` plus a key id or fingerprint, and the
service checks that the key belongs to that channel, so a tenant API can never reach another
tenant's key by id.

### Config

YAML plus environment overrides `KISTA_<PATH>`, with `__` between levels. The loader is written for
kista: `yaml.v3` with `KnownFields`, and a walk over the struct tags. viper does not refuse unknown
variables and matches keys case-insensitively.

- Unknown keys in the file and unknown `KISTA_*` names are refused.
- Errors never echo values or DSNs.
- **File-only settings** (no `KISTA_*` override), because each one switches off a guard: `profile`,
  `signers.allow_file`, `signers.file_dir`. `profile` defaults to `prod`.
- `profile: dev` logs a warning at every start. It refuses a store that is not SQLite or a loopback
  host, so a dev profile never runs against a production database.

```yaml
profile: prod                 # prod | dev
store:
  kind: postgres              # postgres | sqlserver | sqlite
  dsn: postgres://kista@db.internal/kista?sslmode=verify-full
  login: { kind: entra }      # entra | password (password_env / password_file)
  max_open_conns: 10
  migrate: auto               # auto | check
  # sqlite: { path: /var/lib/kista/kista.db }
azure:
  identity: { kind: managed, client_id: "…" }   # for the entra database login; key sources have their own (spec 0004)
rotation:
  min_trusted: 168h
  min_demoted: 168h
signers:
  allow_file: false
  file_dir: /etc/kista/keys
```

The two names spec 0002 used for the file opt-in are unified as `signers.allow_file`.

### `kista admin`

```text
kista admin migrate | check
kista admin backup <file>                                (SQLite)
kista admin tenant create <name> [--display-name …]  |  tenant list  |  tenant suspend|resume <name>
kista admin version add <name> --kind release|dev [--c-api v1.2.0]  |  version list
kista admin channel create <tenant>/<channel> --kind signed|passthrough
kista admin channel versions <tenant>/<channel> --add <version> --remove <version>
kista admin channel list <tenant>
kista admin key add <tenant>/<channel> --signer <ref> [--active]
kista admin key list <tenant>/<channel>                  id, fingerprint, state, signer ref, dates
kista admin key activate <tenant>/<channel> <key-id|fingerprint> [--force]
kista admin key retire   <tenant>/<channel> <key-id|fingerprint> [--force]
kista admin key events   <tenant>/<channel>
kista admin wellknown <tenant>/<channel>
```

The CLI runs as the service's OS user, with the service's config. It holds no logic of its own.

### Package layout

```text
internal/config     types, loading, validation
internal/store      dialects, migrations, entities, Login, Ready
internal/tenants    tenants, channels, DuckDB versions (services)
internal/keys       key lifecycle, signer resolution, .well-known (service)
internal/authz      Actor, Authorizer (allow-all for the CLI in this spec)
internal/signer     + open.go (reference resolution)
cmd/kista           + admin subcommands
```

## Security

- **Keys never leave the signer.** Only public keys and references are stored.
- **Signer references come from the server administrator only.** Files come only from
  `signers.file_dir`, and only in dev or with explicit opt-in. Every open is checked against the
  stored fingerprint, and every `add` proves possession with a probe signature.
- **Role separation**: one fingerprint per server (a registry across key tables), one channel per
  key.
- **Rotation cannot strand clients by accident.**
  - A key is trusted for a minimum time before activation.
  - The active key cannot be retired.
  - A key is demoted for a minimum time before retirement.
  - Releases are checked from spec 0008 on.
  - The minimums have a 24 h floor outside dev.
  - Every forced step is recorded.
- **Guards cannot be switched off from the environment**: `profile` and the signer settings are
  file-only, and dev refuses non-local databases.
- **Database**:
  - credentials never appear in DSNs, config, logs or errors;
  - transport security is required off loopback;
  - SQLite files are `0600`;
  - a database that needs a newer binary refuses to start.
- **Tenant integrity**: the composite foreign key ties a key's tenant to its channel's tenant, and
  service methods check that a key belongs to the addressed channel.
- **Concurrency.** Locks are taken first. Activation demotes before promoting, and the partial unique
  index is the backstop. Two administrators activating at once end with exactly one active key and
  no lost update.

## Testing

- **Store suite** (`internal/store/storetest`), run on every dialect:
  - SQLite always runs;
  - PostgreSQL 17 and SQL Server 2022 run as GitHub Actions service containers on the Linux job
    (`KISTA_TEST_POSTGRES` / `KISTA_TEST_SQLSERVER`). One database per package run, with tables
    emptied between tests. CI sets `KISTA_TEST_REQUIRE_DBS=1`, so a missing database fails the job
    instead of skipping;
  - macOS runs SQLite only;
  - `make test-db` starts both databases locally with Docker Compose.
- **Store cases:**
  - CRUD;
  - name rules, reserved names and the binary collation (`Prod` ≠ `prod` on SQL Server);
  - uniqueness: tenant name, channel per tenant, fingerprint registry;
  - the composite foreign key refusing a key whose tenant differs from its channel's;
  - `CHECK` constraints;
  - compare-and-set conflicts;
  - the partial unique index refusing a second active key, directly in SQL;
  - concurrent activations under `-race`: N goroutines, exactly one active key, no lost update;
  - `sp_getapplock` failure codes;
  - migrations: idempotent, run concurrently by two "replicas", the `min_reader` rule (an older
    binary starts; a database needing a newer one is refused), and statement splitting.
- **Lifecycle cases:**
  - every transition and its event row;
  - `min_trusted` from `trusted_since` across a demotion;
  - `min_demoted`, `--force`, and the 24 h floor outside dev;
  - first-key `--active` only on a channel that never had a key;
  - passthrough refuses keys;
  - the 16-key cap;
  - a probe signature that fails;
  - a signer whose key differs from the row;
  - addressing a key through the wrong channel;
  - `.well-known`: exact bytes, order, the 64 KiB limit, no keys means an error.
- **Config:**
  - unknown keys and unknown `KISTA_*` names are refused;
  - `KISTA_PROFILE` and `KISTA_SIGNERS__*` are refused;
  - a password in a DSN is refused;
  - dev against a non-local store is refused;
  - errors carry no values.
- **CLI.** End to end on SQLite in a temporary directory.
- **e2e** (spec 0002's harness, with `internal/store`, `internal/keys` and `internal/config` added to
  the e2e workflow's path filter). A channel created in a store, with keys from the store, and its
  `WellKnown` output served:
  - DuckDB's `CREATE` reads it, and `duckdb_extension_repositories()` reports exactly the store's
    fingerprints, before and after a rotation;
  - a local-directory prefix reads `.well-known` without httpfs (confirmed), so the case runs in
    tier A. It also checks the rotation end to end: after retirement, an install signed with the old
    key fails `LOAD` until `FORCE INSTALL` of a build signed with the new key.

## Alternatives considered

- **Import or copy tresor-server's store.** It is a different module under BUSL-1.1, and kista is
  Apache-2.0. A shared Apache-licensed module is possible later if the owner wants one.
- **Native `uuid` / `uniqueidentifier` columns.** go-mssqldb's `uniqueidentifier` byte order needs
  special handling, and text ids are uniform across the three engines at a negligible cost.
- **Database time (`now()`).** SQL Server's `GETDATE()` is local time, and the three engines differ.
  Process time in UTC, from NTP-synchronized hosts, is uniform.
- **Refuse any database newer than the binary**, as tresor-server does. It breaks rolling upgrades
  when an old replica restarts. A compatibility level does not.
- **Keys per tenant, shared by its channels.** Spec 0001 chose per-channel keys, so staging cannot
  sign for prod.
- **viper for config.** It cannot refuse unknown environment variables and is case-insensitive.
- **The Key Vault signer and the HTTP API in this spec.** Each doubles the review surface, and
  neither is needed to prove the store and the lifecycle. They are specs 0004 and 0006.

## Follow-ups

- **Spec 0004.** Signer backends behind named key sources: Vault/OpenBao, Azure Key Vault and
  Managed HSM, AWS KMS, Google Cloud KMS (versioned keys, refusal of exportable keys and keys with
  wrap/encrypt ops, the probe, platform credentials).
- **Spec 0006.** Serving `.well-known` (with the version-keyed cache), issuer records, grants, the
  HTTP management API over these services, tenant key provisioning, `/readyz`.
- **Spec 0008.** Releases and signatures, re-signing, the retirement check.
- **Spec 0010.** Admin actions in the tamper-evident audit log.
- **Later:**
  - `store.schema`, for sharing a database with other hugr services;
  - a config-driven bootstrap of the first tenant, for managed installs (spec 0013);
  - tenant and channel deletion (with tombstones).
