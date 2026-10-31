---
title: Access
---

# Access

kista has no accounts and stores no passwords. It verifies JWTs from issuers you trust, turns a
token's claims into **principals**, and decides with **grants**: principal × resource × verbs. Each
tenant trusts its own issuers; the server trusts its own for server administrators. The design
reasoning is in
[spec 0006](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0006-serve/spec.md),
[spec 0007](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0007-api/spec.md)
and [spec 0017](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0017-extension-groups/spec.md).

## Public and private

Every release is `public` or `private`.

- A **public** release is served to anyone, over `http` and `https`, with or without a token.
- A **private** release is served only over `https` to a token holding `install` on it. To everyone
  else it does not exist: a private, yanked, unserved and never-existing path answer the same.

| Request on a signed channel's DuckDB route | Public release | Private release | Missing, yanked, not served |
| --- | --- | --- | --- |
| no valid token, or the `http` listener | `200` | `401` | `401` |
| valid token, no `install` | `200` | `404` | `404` |
| valid token with `install` | `200` | `200` | `404` |

An invalid or expired token is treated as **no token**: public releases are still served, so a stale
DuckDB secret never breaks the bootstrap. "Current" is decided per caller: adding a private release
never changes what an anonymous client gets. Tenant and channel names are public, and so is each
signed channel's `/.well-known/duckdb-extension-repo.json`. A passthrough channel looks at no token
and answers `404` for every miss (see [Upstreams](upstreams.md#passthrough-channels)).

## How DuckDB presents a token

DuckDB sends the token of an `http` secret whose `SCOPE` covers the repository's URL:

```sql
CREATE EXTENSION REPOSITORY acme WITH PREFIX 'https://kista.example.com/acme/prod';
CREATE SECRET acme_token (
    TYPE http,
    BEARER_TOKEN 'eyJhbGciOi…',
    SCOPE 'https://kista.example.com/acme/'
);
INSTALL tresor FROM acme;
LOAD tresor;
```

- Tokens are read only on `https` listeners; an `http` listener ignores `Authorization`.
- A token issued by tresor (or any issuer of the tenant) must carry the tenant's audience (below).
- A server administrator's token is no token on the DuckDB routes, and so is a `kista_…` Bearer (a
  publisher's API key before spec 0023), so a stale configuration still installs public releases.
- An issuer record's `max_token_lifetime` (default 24h, at most 7 days by default: `limits.ceilings.issuer_token_lifetime`) bounds how long a token in a
  persistent DuckDB secret can live.

See [Using kista from DuckDB](duckdb.md) for repositories, keys and secrets in full.

## Token issuers per tenant

An **issuer record** names an OIDC/JWT issuer a tenant trusts. A tenant holds at most 16 by default (`limits.ceilings.issuers`).

```bash
# Keycloak: roles from realm_access.roles
kista admin -config /etc/kista/kista.yaml issuer add acme \
  -name kc -url https://sso.acme.example/realms/acme \
  -roles-claim realm_access.roles -groups-claim groups -client-claim azp

# Microsoft Entra ID: pin the directory
kista admin -config /etc/kista/kista.yaml issuer add acme \
  -name entra -url https://login.microsoftonline.com/<tenant-id>/v2.0 \
  -require tid=<tenant-id> -roles-claim roles -client-claim azp -max-lifetime 12h
```

| Field (API) | Flag | Meaning |
| --- | --- | --- |
| `name` | `-name` | `[a-z][a-z0-9-]{0,15}`, unique in the tenant, immutable; principals are namespaced by it |
| `url` | `-url` | the issuer URL, `https`, exact, unique in the tenant, immutable (a new URL is a new record) |
| `jwks_uri` | `-jwks-uri` | skip discovery and use this JWKS |
| `algorithms` | `-alg` (repeat) | from `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `EdDSA`; default all of them. `none` and `HS*` never |
| `required_claims` | `-require claim=value` (repeat) | exact claim values every token must carry, at most 16 |
| `roles_claim`, `groups_claim`, `client_claim` | `-roles-claim`, `-groups-claim`, `-client-claim` | claim paths: keys joined by dots, or a JSON array of keys when a key has dots of its own (`'["https://example.com/roles"]'`) |
| `max_token_lifetime` | `-max-lifetime` | `exp - iat` above it is refused; default 24h, 1m..7d |

When a record is added, discovery (unless `jwks_uri` is given) and the JWKS are fetched through the
[egress client](upstreams.md#egress); the discovered `issuer` must equal the record's URL and the JWKS
must hold a usable signing key. Issuers on a private network need `egress.allow`.

**Required claims matter** for issuers that let anyone choose the audience (GitHub Actions, Google,
a shared Keycloak realm): without them, any account of the issuer could present a token. For Entra,
also set "assignment required" on the app registration.

Over the API (tenant administrators): `GET, POST /api/v1/tenants/{t}/issuers`,
`GET /issuers/{name}`, `DELETE /issuers/{name}` with `If-Match` (an issuer's ETag is its id).
Removing a record removes its grants, so a record added later under the same name starts with none.
A record cannot have a CI token issuer's URL: GitHub Actions' (`https://token.actions.githubusercontent.com`,
or `token.actions.<name>.ghe.com`) or a GitHub Enterprise Server's (`…/_services/token`). Any
workflow there can request a token for any audience. CI publishes as a publisher instead (see
[Publishing](publishing.md#publishers)).

### Audiences

A token must carry, in `aud`, one of its tenant's audiences:

- the canonical `<serve.public_url>/<tenant>`, for example `https://kista.example.com/acme`;
- audiences a **server administrator** assigns, unique across tenants, for IdPs that cannot issue an
  https audience without a verified domain (Entra: `api://kista-acme`):

```bash
kista admin -config /etc/kista/kista.yaml tenant audience add acme api://kista-acme
```

A token for tenant A never works at tenant B. Changing `serve.public_url` changes every canonical
audience.

### Verification

A token is a compact JWS of at most 16 KiB with a `kid`; its key comes from the record's JWKS (RSA
2048-4096 bits, or the curve the `alg` names). `exp` and `iat` are required, `nbf` respected, with
60 seconds of skew. JWKS are cached by `Cache-Control` within 5m..24h; an unknown `kid` triggers at
most one background refresh per URL per minute. Failed tokens are logged with the record and a reason
class, never the token or its claims.

## Principals

A valid token yields principals, namespaced by the issuer record's name:

| Principal | From |
| --- | --- |
| `subject:<issuer>\|<sub>` | the `sub` claim |
| `role:<issuer>\|<role>` | each value at `roles_claim` |
| `group:<issuer>\|<group>` | each value at `groups_claim` (the IdP's groups of people) |
| `client:<issuer>\|<id>` | the value at `client_claim` (`azp`, `appid`, `client_id`) |
| `issuer:<issuer>` | any valid token of that record |
| `publisher:<name>` | a [publisher](publishing.md#publishers)'s credential, not an issuer's token |

Claim values must be strings without control or format characters, at most 256 bytes each and 256
per claim. An Entra groups overage is not resolved.

`GET /api/v1/tenants/{t}/whoami` with a tenant token answers its principals, its grants and
`holds_admin` / `holds_audit`.

## Verbs

| Verb | Gives | Implied by `admin` |
| --- | --- | --- |
| `install` | private releases on the DuckDB routes, and their rows in the index | yes |
| `admin` | management of its resource: the tenant, a channel, an extension or an extension group | - |
| `publish` | upload a build into a channel ([Publishing](publishing.md)) | **no** |
| `promote` | release in a channel a build from another channel of the tenant | **no** |
| `audit` | read the tenant's events and statistics, and its structure read-only ([below](#who-sees-what)); tenant-wide grants of every package type only; `whoami` answers it as `holds_audit` | yes, on a tenant-wide grant of every type |
| `feed` | take the tenant's feed of signed statements ([Attachments](attachments.md)); on the tenant or a channel | yes, on a tenant-wide grant of every type only; on a channel, or on a grant of one type, it is granted explicitly |

`publish` and `promote` are never implied by `admin`, so an administrator's install token cannot
publish native code.

## Grants

A grant is principal × resource × verbs. The resource is one of:

| Resource | Covers |
| --- | --- |
| the tenant | everything in the tenant, every channel, present and future |
| a channel | that channel and every extension in it |
| an extension | that name's releases, in every channel or in one |
| an extension group | each member as a grant on that extension would, in every channel or in one |

```bash
# nodes install everything in prod
kista admin -config /etc/kista/kista.yaml grant add acme \
  -principal 'role:kc|hugr-node' -verb install -channel prod
# a team administers one extension everywhere
kista admin -config /etc/kista/kista.yaml grant add acme \
  -principal 'group:kc|acl-team' -verb admin -extension duckdb_acl
# tenant administrators
kista admin -config /etc/kista/kista.yaml grant add acme -principal 'role:kc|kista-admin' -verb admin
# any token of the record with its required claims may install
kista admin -config /etc/kista/kista.yaml grant add acme -principal issuer:entra -verb install
```

Over the API (tenant administrators):

```bash
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"principal":"role:kc|hugr-node","verbs":["install"],"channel":"prod"}' \
  https://kista.example.com/api/v1/tenants/acme/grants
```

`GET, POST /grants`, `GET, DELETE /grants/{id}`. `GET /grants` takes `?principal=`,
`?extension_group=` and `?extension=`; with `?extension=` each row shows `via` (`tenant`, `channel`,
`extension`, `extension_group`) and `verbs_on_extension`, the verbs that actually apply to that name.
CLI: `grant add|list|remove`, `grant list <tenant> -extension <x>` or `-extension-group <g>`.

Rules:

- An `issuer:` grant carries `install` only, and needs the record to have required claims.
- A publisher grant carries `publish` and `promote` only.
- `audit` is granted on the tenant only, of every package type; `feed` on the tenant or a channel,
  never on an extension or issuer-wide.
- For `publish` and `promote`, a **reserved name** (DuckDB's core and community names, and names an
  upstream of the tenant provides) is reached only by a grant naming that extension, never by a
  tenant-, channel- or group-wide grant.
- An identical grant is not added twice. A tenant holds at most 1,000 grants by default (`limits.ceilings.grants`), both kinds counted.
- A tenant administrator cannot remove the last tenant-wide `admin` grant (or an issuer record whose
  removal would remove it); a server administrator or the CLI can.
- Grants, issuers, audiences and groups are cached per tenant behind a version: a change applies at
  the next request on every replica.

### Package types

A grant has a **package type** (`package_type` in the API, `-type` in the CLI; see
[Concepts](concepts.md#packages-and-package-types)):

- A grant that names an extension names its type: `duckdb_extension` by default; `*` with a name is
  refused (`400`).
- A grant without a name is of every type, `*`, by default (what every such grant meant before
  types existed), or of one type: then it covers that type's channels and packages only, and
  nothing tenant-level, so creating channels, groups and grants, managing issuers and upstreams,
  reading the log and listing or importing licences take a `*` grant. `audit` is never granted on a
  grant of one type (`400`): the log has no type. A tenant-wide `admin` of one type implies no
  `feed`.
- A grant on a channel covers the channel's type only. Without a name, `package_type` set to the
  channel's type is the same grant as `*` there: it is stored, and answered, as `*`, so adding the
  other is a duplicate (`409`).
- Answers (`GET /grants`, `whoami`) and events show `package_type` only when it is not the default
  for the grant's shape; `GET /grants?extension=` takes `?type=` for the name's type. `POST /grants`
  takes the type in its body only: `?type=` there is `400`.
- Today `duckdb_extension` is the only type: grants that name no type mean what they did.

## Extension groups

An **extension group** is a named, explicit set of extension names in a tenant: `hugr-platform` =
`tresor`, `hugr_node`, `duckdb_acl`. A grant on the group covers each member, and a name added to the
group is covered at once. Groups are not patterns, and do not nest.

```bash
kista admin -config /etc/kista/kista.yaml extension-group add acme hugr-platform -description "hugr's own"
kista admin -config /etc/kista/kista.yaml extension-group members acme hugr-platform -add tresor,hugr_node,duckdb_acl
kista admin -config /etc/kista/kista.yaml grant add acme \
  -principal 'role:kc|platform-team' -verb admin -extension-group hugr-platform
```

- A name is `[a-z0-9][a-z0-9-]{0,62}`, unique in the tenant and immutable. A member may be added
  before anything is released under it, and may be in several groups.
- Limits, by default (`limits.ceilings.package_groups`, `group_members`, `tenant_group_members`): 100 groups a tenant, 1,000 members a group, 10,000 members a tenant.
- Only a tenant administrator changes members; every change is an event. `admin` on a group lets its
  holders read the group and administer its members' releases, never change its members.
- A group's grants never reach a reserved name for `publish` or `promote`, and cannot carry `audit`
  or `feed`.
- Removing a group a grant names is `409`: revoke its grants first.

API: `GET, POST /api/v1/tenants/{t}/extension-groups`, `GET, DELETE …/{g}`,
`POST …/{g}/description`, `GET, POST …/{g}/members {"add": [...], "remove": [...]}` (1-100 names a
request; names already present or absent are skipped). See [commands](administration/commands.md).
A member is a (package type, name) pair: a change names its names' type as `"package_type"`
(`duckdb_extension` by default; CLI `-type`), the members list shows a member's `package_type`
when it is not the default, and `GET /extension-groups?extension=` takes `?type=`.

## Server administrators and tenant administrators

| | Server administrator | Tenant administrator |
| --- | --- | --- |
| Who | a principal of a server issuer listed in `auth.server_admins`, or the OS user running `kista admin` | a principal holding `admin` on the tenant |
| Token audience | `auth.server_audiences` (default `serve.public_url`) | the tenant's audiences |
| Scope | every tenant and the server | its tenant |
| Server-only | creating, suspending and resuming tenants; DuckDB versions and their C APIs; assigned audiences; registering signing and attestation keys by signer reference; `-force` on key operations; the configured credentials list | - |

Administrators of a channel, an extension or an extension group administer that resource only.

**Licence managers** (spec 0029) are a server-wide role for a job that writes licences, such as a
licence reconciler: a principal of a server issuer listed in `auth.server_license_admins`. On every
tenant that is active (not suspended, not being deleted) a licence manager may list and read every
licence and the licence keys the live releases declare, and import, replace, remove and make private
the licences of **distributors' publishers'** entries only (spec 0035;
[A licence's entry](licensing.md#a-licences-entry)): it names an entry as a distribution admin
names a distributor's publisher, and any other `?source=` answers as a missing path does (`404`,
recorded as `authz.refused`). It still reads every licence and every key's entries, but its
server-wide token writes only into distributors' publishers' entries: it cannot copy a tenant
upstream's licence into another entry or pin an import into an upstream's entry:

- `GET /api/v1/tenants/{t}/licenses`, `GET …/licenses/{name}`;
- `POST /api/v1/tenants/{t}/licenses?source=`, `DELETE …/licenses/{name}?source=`,
  `POST …/licenses/{name}/private?source=`, for a distributor's publisher's entry;
- `GET /api/v1/tenants/{t}/license-keys`, `GET …/license-keys/{name}`.

Nothing else. It cannot make a licence public (`…/public`: a server administrator's on a
distributor's entry, a tenant administrator's on the tenant's other entries), since a public
licence is served to anyone served a public release. It cannot call the upstreams' licence
relay (`…/upstreams/{u}/licenses[/sync]`, the tenant administrators'), any other tenant route or
any server route. Those answer `404`, as to any caller who may not use them. A licence still has
to verify against a key a live release declares. The role is configuration only: no API call
grants it. Its acts are recorded as `license-manager:<issuer>|<who>`; a tenant's readers see
`license-manager`, without the name. A principal in both lists is a server administrator.

**Who writes a distributor's entry** (spec 0037). A distributor's publisher's licence entry, live
or removed, is the distributor's contract with the tenant's holder:

| Caller | import, replace | `DELETE` | `…/private` | `…/public` | reads |
| --- | --- | --- | --- | --- | --- |
| server administrator (server token, CLI) | yes | yes | yes | yes | yes |
| licence manager | yes | yes | yes | no | yes |
| tenant administrator | refused | refused | refused | refused | yes |

A tenant administrator's write there answers `404` with the problem type `distributors-entry` and
the detail "a distributor's entry: its licence manager or a server administrator writes it" (it
reveals nothing: the caller reads the licence and sees whose entry it is), recorded as
`authz.refused` with the licence's `name`, the entry (`source`) and the `operation` (`import`,
`remove`, `public`, `private`). The tenant still decides what its channels serve, by a name's
[source](upstreams.md#a-names-source-in-a-channel), and may remove the distributor's publisher. The
tenant's other entries are its administrators' and the server administrators' to write, never a
licence manager's.

**Distribution admins** (spec 0030) are a server-wide role for a distributor's provisioning, such as
Enterest's: a principal of a server issuer listed in `auth.server_distribution_admins`. On every
active tenant (and, where said, a suspended one) a distribution admin may:

- read the server's reserved names (`GET /api/v1/reserved-names`: each name and its kind only,
  without `sources`, which name other tenants' upstreams and administrators: spec 0037), the names a
  distributor's publisher may not publish;
- read the tenant's name, state and expected holder (`GET /api/v1/tenants/{t}`, spec 0037), active
  or suspended, so a distributor checks that the tenant is its customer's before it writes there;
  each read is a `tenant.read` event in the tenant;
- create a distributor's publisher, marked from its creation (spec 0033, [Distributors' publishers](publishing.md#distributors-publishers)):
  without `distributor` the creation answers `404`, and for a name a tenant's own publisher holds
  `409` with the problem type `publisher-taken` (the name is taken, and nothing else is said; the
  refusal is an `authz.refused` event). A tenant holds at most 10 distributors' publishers by default (`limits.ceilings`), counted
  apart from its own (at most 200 by default), so a distributor never takes the tenant's room;
- list and read the distributors' publishers (a list leaves the tenant's own publishers out, and
  reading one answers `404`), and for a distributor's publisher only: rename its distributor (while
  unused, spec 0036), set and clear its licence-key mark, manage its keys (with `purge_held`), set
  its hold (which holds its entries only above the tenant's [minimum hold for distributors'
  publishers](#the-minimum-hold-for-distributors-publishers)), purge its builds placed nowhere,
  [withdraw](upstreams.md#withdrawals) its versions, read its entries in the catalogue (without
  channels; reading them and withdrawing reach a suspended tenant too, so a withdrawal never waits
  for the tenant's resumption: spec 0037), and write their [icon and summary](catalogue.md#give-an-entry-an-icon-and-a-summary)
  (spec 0036: the tenant's administrators read a distributor's entry's metadata, and never write
  it);
- never act on the tenant's own publishers (spec 0037), with the licence-key mark or without: their
  mark, hold, keys and grants are the tenant administrators' and the server administrators' (a
  publisher with the licence-key mark alone, a server administrator's, spec 0021). Such a call
  answers the same `404` as a missing publisher;
- never create an ordinary publisher, read or write grants (`…/grants` answers it `404`), or set the
  tenant's minimum hold for distributors' publishers (spec 0037). Grants and publishers a
  distribution admin made before spec 0037 stay, the tenant administrators' to manage;
- never switch a name's source in a channel: that is the channel's administrators'.

Across tenants, a distribution admin reads a distributor's download statistics (`GET
/api/v1/distributor-stats/downloads`, spec 0036): per extension per complete week, summed over every
tenant into a snapshot computed once, naming no tenant or channel, the non-public tenants' part
answered only above a minimum ([A distributor's statistics](observability.md#a-distributors-statistics)).
It never renames a used distributor's publisher's distributor (`409`).

Nothing else: it reads no channel, key or release beyond what those calls answer, and cannot change
a tenant, a channel or a key set; the rest answers `404`. Its acts are recorded as
`distribution-admin:<issuer>|<who>`, shown to a tenant's readers as `distribution-admin`. A
principal is a licence manager or a distribution admin, not both: the configuration is refused, and
so is a token whose principals hold both (`401`; were one accepted, it would act as a licence manager,
and every distribution admin's route would answer it `404`). A server administrator's token acts as
a server administrator, whatever other role its principals hold.

A server has **one distributor** (spec 0037): distribution admins and licence managers are not bound
to a distributor, and reach every distributor's publisher on the server. Other vendors reach a
kista through [upstreams](upstreams.md).

```yaml title="kista.yaml"
auth:                                  # file-only; changes take effect at restart
  server_issuers:
    - name: ops
      url: https://sso.example.com/realms/ops
      required_claims: {azp: kista-ops}  # required for a server issuer
      roles_claim: realm_access.roles
  server_audiences: []                 # default: [ serve.public_url ]
  server_admins:
    - role:ops|kista-admin             # kind:issuer|value; issuer: is refused
  server_license_admins:               # licence managers: every tenant's licences, nothing else
    - role:ops|kista-licenses          # checked as server_admins
  server_distribution_admins:          # distribution admins: a distributor's calls on every tenant
    - client:ops|enterest              # checked as server_admins
  server_egress_allow: []              # CIDRs reached only by the server issuers' fetcher
  admin_token_max_age: 1h              # management needs a token issued this recently; 1m..24h
```

A server issuer's principals never match a tenant's, whatever a tenant names its records. A server
token's identity is at `GET /api/v1/whoami`, with `administrator`, `license_manager` and
`distribution_admin`.

**Fresh tokens for management.** Every management route, reads included, needs a token issued
within `auth.admin_token_max_age` (default 1h), and a write needs a `sub` or client claim to record
as the actor. A node's long-lived install token is not enough; a token that may not use a route at all
gets `404`. See [HTTP API](api.md).

### The minimum hold for distributors' publishers

A tenant's administrators and server administrators set the tenant's **minimum hold for
distributors' publishers** (spec 0037): an entry a distributor's publisher publishes is held for the
larger of the publisher's hold and this minimum, one day unless changed. A distribution admin sets
its publisher's hold to anything, but cannot go below the minimum, nor set it, and reads it only as
its publishers' `effective_hold` (the route answers it `404`): a stolen distribution admin's token
that adds its own key to the distributor's publisher and publishes reaches no channel, even through
the tenant's `auto` rule or follow, before the minimum has passed, time enough to revoke the key with
`purge_held`. A channel administrator's `early` placement still skips it: that is the tenant's
choice. So, by default, a distributor's builds reach a tenant's channels a day after publication.

```bash
kista admin -config /etc/kista/kista.yaml tenant distributor-hold acme        # prints it
kista admin -config /etc/kista/kista.yaml tenant distributor-hold acme 2d     # 0 for none
```

`GET|PUT /api/v1/tenants/{t}/distributor-hold {"hold": "24h"}`, or the console's Publishers screen.
Raising it holds the entries held now of every distributor's publisher of the tenant, live or
removed, longer; lowering it applies to entries arriving from then on. Each change is a
`catalogue.hold` event with the subject `tenant:<name>` and `source` `distributors` (setting the same
value again writes none). A distributor's publisher's answer carries `effective_hold`, the larger of
the two; `kista admin publisher list` shows it as `HELD FOR`, `publisher show` as `held for`, and
`tenant show` prints the minimum as `distributor hold`.

## Who sees what

- Management answers that the caller may not see are `404`, the same as for a resource that does not
  exist.
- A tenant administrator sees `created_by: server` for records a server administrator or the CLI
  made (`license-manager` for a licence manager's acts, `distribution-admin` for a distribution
  admin's), and no infrastructure details (signer
  references, storage domains).
- A holder of `audit` on the tenant (an auditor; a tenant-wide `admin` of every type implies it)
  reads, read-only and with the administrators' answers, the tenant's structure: `GET` on
  `issuers`, `issuers/{name}`, `audiences`, `grants`, `grants/{id}`, `extension-groups` (every
  group with its grants, `?extension=` too), `extension-groups/{g}` and its `members`, `publishers`,
  `publishers/{name}`, their `keys` and `keys/{id}`, `upstreams` and `upstreams/{name}`. It writes
  nothing, and reads neither `grants/principals` nor an upstream's sub-routes. An administrator of
  one package type, a channel, an extension or a group holds no tenant-wide `audit` (a group's
  administrator still reads its own group); a distribution admin still reads only distributors'
  publishers and their keys. These reads write no event.
- `created_by`, and an event's `actor`, take these forms for a tenant's readers:
  `principal:<tenant>/<issuer id>|<sub>` (the token's bare `sub`; `client:<id>` for a token without
  a `sub`; `sub:<sub>` only when the `sub` itself begins with `client:` or `sub:`; the issuer
  record's `id` is in its answer), `publisher:<tenant>/<names>`, `system:<what>`, `anonymous` (an
  event of an unauthenticated request), and, masked, `server`, `license-manager` and
  `distribution-admin`. Only a server administrator sees who those were.
- The index lists, to a caller with `install`, the private extensions it may install; to anyone else,
  public ones only.

See [Security](security.md) for the threat model.
