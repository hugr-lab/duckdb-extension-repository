# Spec 0027: The database password from Vault or OpenBao

- **Status**: phase 1 implemented (2026-10-21), but for the kind case (Testing); phase 2 deferred
- **Date**: 2026-10-21
- **Author**: vgsml, Claude

## Summary

A further `store.login` kind, `vault`, makes kista read its database credentials from HashiCorp
Vault or OpenBao itself. It authenticates as the `vault` signer source does, through the same
`vaultapi` client and config shape. Phase 1 reads a password at a KV v2 path or a database secrets
engine static role. Phase 2 (deferred, open questions 1 and 2) would take dynamic credentials
with lease renewal.

**What already works today, with no change to kista:** External Secrets Operator, the Vault
Secrets Operator, the Vault CSI provider or the Vault Agent injector writes the password to a file,
and `store.login.password_file` names it. kista reads that file again for every new connection
([State](../../website/docs/state.md)), so a rotated password takes effect without a restart. The
secret-file rule accepts both shapes these tools produce:

- a Secret volume: root-owned, kista's group, `0440`;
- a file owned by kista's uid at `0600` or narrower, for example the Agent run as the same user.

This spec is for operators who want no such operator or sidecar in the cluster.

## Problem

- **The logins today.** `store.login` takes `password_file`, `password_env`, or a cloud IAM token
  (`entra`, `aws`, `gcp`; spec 0020). On OTC, Hetzner, IONOS or STACKIT there is no IAM login for
  the database. The umbrella chart (`hugr`) brings an OpenBao, and would like kista to take its
  database password from it without one more moving part.
- **The pieces exist.** kista already speaks to Vault for signing (spec 0004, `internal/vaultapi`):
  Kubernetes, JWT or token-file auth, a private CA, no environment variables, errors without
  bodies. The store already asks its login for a secret on every new connection
  (`store.Login.Password`, pgx's `BeforeConnect`; the SQL Server connector likewise).

## Design

### Configuration

```yaml
store:
  kind: postgres
  dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full
  login:
    kind: vault
    vault:                                   # the shape of signers.sources[].vault
      address: https://bao.internal:8200
      namespace: ""
      ca_file: /etc/kista/secrets/ca/bao-ca.pem
      auth: {kind: kubernetes, role: kista-db, token_file: /var/run/secrets/kista/bao/token}
      # one of:
      kv: {mount: secret, path: kista/db, field: password}       # KV v2
      static_role: {mount: database, name: kista}                # database secrets engine, static
      # dynamic_role: {mount: database, name: kista}             # phase 2
      cache: 1m                                                  # kv only; 0s..10m
```

- **The block is file-only**, as `signers` is.
- **The connection settings are reused.** `address`, `namespace`, `ca_file` and `auth` are
  `config.VaultSource`'s connection part, validated by the same rules: https (http on loopback only
  in dev), an absolute CA file, an auth kind of `kubernetes`, `jwt` or `token_file`.
- **Separate clients.** The login and a signer source on the same Vault use separate clients, with
  separate roles. The database role may read one path; the signing role may sign with one key.
- **The user comes from the DSN** for `kv` and `static_role`. For `static_role`, kista checks that
  Vault's `username` equals it, and refuses otherwise.

### Behaviour

- **`kv`**: `GET /v1/<mount>/data/<path>`, the string field `field`.
  - The value is cached for `cache` (default 1 minute, at most 10), so a burst of new connections
    makes one Vault call.
  - An optional `version` pins a KV version. Unpinned, the latest is read: a rotation in Vault takes
    effect within `cache`.
- **`static_role`**: `GET /v1/<mount>/static-creds/<name>`.
  - Vault rotates the password of an existing database user on a schedule. The answer carries
    `username`, `password`, `ttl` and `last_vault_rotation`.
  - kista caches the password until `ttl` minus a margin (30 seconds), then reads it again.
  - Connections opened before a rotation keep working: PostgreSQL and SQL Server check the password
    only at login. The pool recycles them within 30 minutes.
- **Fail closed, and no worse than a database outage.**
  - At start, a Vault that cannot be reached, a login that fails or a field that is missing fails
    the store's open, so kista does not start, as with a database that is down.
  - Later, a failed read ends the cache, and new connections fail with a fixed error that names the
    source (`store: vault login: status 403`), never a value. Existing connections continue.
    `/readyz` follows the database, as today.
- **A rejected password** (SQLSTATE `28P01`, SQL Server error 18456) clears the cache. The next
  connection reads Vault again; the refused attempt is not retried in place, so there is no loop
  (open question 6).
- **Errors and logs** never carry the password, the token or a response body. `vaultapi`
  guarantees this already, and the planted-value scan of spec 0004's tests covers the new paths.
- **Metrics** (OpenTelemetry names, as kista's others): `kista.store.login` (a counter by
  `kista.login.kind` and `kista.outcome`: `vault`, `cached`, `error`, `rejected`; exported to
  Prometheus as `kista_store_login_total`), `kista.store.login.duration` (the Vault calls'
  latency) and `kista.store.login.expiry` (the seconds the cached password has left, for `kv` and
  `static_role`).
- **Admin commands and CronJobs** use the same login: they read the same configuration.

### Phase 2: dynamic credentials (deferred)

`GET /v1/<mount>/creds/<name>` makes a new database user with a lease. It brings three changes:

1. **The login returns the user too.** `store.Login` grows a `Credentials(ctx) (user, password)`
   method, and `BeforeConnect` sets both. Today the user comes from the DSN.
2. **Lease renewal.** The lease is renewed (`PUT /v1/sys/leases/renew`) while any connection opened
   with it may still live.
   - A new lease is taken when the current one has less than the pool's connection lifetime (30
     minutes) plus a margin left before its `max_ttl`. `max_ttl` must exceed that, or the login
     is refused at start.
   - At shutdown, kista revokes its leases.
3. **Ownership.** Objects a migration creates belong to the dynamic user, whom Vault drops later.
   Either migrations run with `store.migrate: check` and an owner identity (`kista admin migrate`
   with a static login), or kista sets a role after connecting (`SET ROLE kista_owner`, a new
   `store.role`). This is open question 2, deferred with phase 2.

### The chart

Nothing new: `projectedTokens` gives the Vault token file (audience as the Vault role requires). The
egress NetworkPolicy (spec 0025) needs an `extraRules` entry for Vault.

## Security

- **Least privilege.** The documentation gives the policy:
  - `read` on one KV path, or on `database/static-creds/<name>`;
  - for phase 2, `read` on `database/creds/<name>` and `update` on `sys/leases/renew` and
    `sys/leases/revoke`;
  - nothing else.
- **Where requests go** comes only from the configuration: address, namespace, mount, path, role.
  No tenant input reaches it.
- **The password lives in memory** for at most `cache`, or the static role's TTL. It is never
  written to disk, and never in an error, a log or a metric.
- **What it does not change**: the database's TLS (`sslmode=verify-full`, `store.ca_pem`), the
  refusal of a password in the DSN, and the file-only rules.

## Testing

- **Unit tests** with a fake Vault (`httptest`):
  - KV v2 reads with and without a version, a missing field, a non-string field;
  - static-creds with a username that does not match the DSN's;
  - the cache's expiry, and the clear after a rejected password;
  - every failure mapped to a fixed error, and the planted-value scan.
- **Integration** against the dev-mode Vault and OpenBao CI already runs (`KISTA_TEST_VAULT`,
  `KISTA_TEST_OPENBAO`):
  - KV v2 is mounted at `secret/` in dev mode;
  - the database secrets engine runs against the store suite's PostgreSQL (`make test-db`), with a
    static role whose rotation the test forces (`POST database/rotate-role/<name>`). New connections
    take the new password; open ones keep working.
- **The store** opens, migrates and serves queries with `login.kind: vault` on PostgreSQL, from a
  KV v2 secret and from a static role, on Vault and on OpenBao; a rotation takes one rejected
  attempt, then Vault is read again. The rejection's detection is also tested on SQL Server with a
  plain login. SQL Server's static role is tested later (decided 2026-10-21, open question 4). The
  static role's test needs `KISTA_TEST_VAULT_POSTGRES`: the PostgreSQL as Vault reaches it from its
  container.
- **kind**: the bench's OpenBao serves the database password, in place of the `kista-db` Secret
  (to come).

## Alternatives considered

- **External Secrets Operator, the Vault Secrets Operator, the CSI provider or the Agent injector
  writing `password_file`.** This works today and is documented as the first choice. Its costs: one
  more component, and a copy of the password in a Secret or a file.
- **`password_env` from an injector.** It is weaker: an environment variable is visible in the pod
  spec or `/proc`, and does not rotate without a restart.
- **The Vault SDK.** Rejected for the signer already (spec 0004): five endpoints do not justify it.
- **Client certificates from Vault's PKI** (PostgreSQL `cert` auth). This avoids passwords
  altogether, but needs certificate rotation in the TLS config. A possible follow-up.

## Open questions

1. **Phase 2 at all?** Dynamic credentials bring renewal, revocation and object ownership. Static
   roles give rotation without them. The proposal is phase 1 only, until someone needs dynamic
   users. (decided 2026-10-21) Phase 1 only, as proposed: `kv` and `static_role`. `dynamic_role`
   is refused by the configuration; phase 2 needs its own decision when someone asks for it.
2. **Ownership under dynamic users**: `SET ROLE` (a `store.role` setting) or `migrate: check` with a
   separate owner login. (decided 2026-10-21) Deferred with phase 2: the draft gives no
   recommendation, and phase 1 needs none, since `kv` and `static_role` log in as one fixed user
   that owns what it migrates, as today.
3. **The cache default** for KV (1 minute), and whether KV v1 is needed. (decided 2026-10-21) One
   minute, at most 10, as proposed; KV v2 only. A KV v1 mount answers a v2 read with an error, so a
   wrong mount fails at start.
4. **SQL Server**: support `static_role` with Vault's mssql plugin in the same phase, or later?
   (decided 2026-10-21, the draft gave no firm recommendation; the conservative option) The login
   is the same on both dialects (it hands a password to the connector, and checks the user against
   the DSN's), so nothing refuses SQL Server; but phase 1 tests and documents the database secrets
   engine against PostgreSQL only. A tested SQL Server static role is a follow-up.
5. **One Vault client per address**, shared between a signer source and the login when both name
   the same Vault and role. This would save logins, but mixes the roles' duties; the proposal is
   no. (decided 2026-10-21) No, as proposed: the login has a client of its own.
6. **The retry after a rejected password** needs the error from the driver's connect path. Can
   pgx's `BeforeConnect` and the SQL Server connector expose it cleanly, or does the pool's next
   attempt suffice? (decided 2026-10-21, the draft gave no recommendation) Both expose it cleanly:
   the store wraps the connector it already has (pgx's `stdlib.GetConnector`, and kista's own SQL
   Server connector), sees the driver's error (SQLSTATE `28P01`; SQL Server error 18456) and tells
   the login, which drops its cached password. The attempt that was refused fails and is not
   retried in place (the conservative option: no loop by construction); the pool's next attempt
   reads Vault again.
