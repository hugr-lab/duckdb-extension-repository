# Spec 0012: Licensing and usage reports

- **Status**: implemented; phases 1a (part 1 the format, the reference verifier and the vectors; part 2 licences in kista, the relay, the routes, the API and the CLI), 1b (usage reports), 1c (forwarding), 1d (the console) and 2 (the C++ SDK: part 1 the verifier, part 2 the DuckDB side) implemented
- **Changed by** [0037](../0037-distributor-in-a-tenant/spec.md) (PR 1): the relay's replacement rules gain the rule `older`: a licence older (by `iat`) than one the tenant holds from the same issuer for the same holder (in a distributor's entry whatever its key, elsewhere verified by the same key) is refused whatever its expiry (an import and bundles too), so a licence that lasts longer no longer replaces a later one.
- **Date**: 2026-10-17
- **Author**: vgsml, Claude

## Summary

A paid extension needs two things from the repository it came from: a **licence** that says this
organisation may run it (until when, at which plan, with which limits), and a place to **report
usage**. This spec gives kista both, without making kista a seller.

- **Enterest sells and issues.** A developer hands an extension to Enterest and sets its terms
  there: free, a subscription with plans, or a licence. Enterest is the only licence issuer: a
  licence is a small JWS signed by Enterest's licence key. kista holds no licence key.
- **kista relays and serves.** An organisation's kista that mirrors Enterest with the
  organisation's credential (spec 0009 phase 3) receives the organisation's licences for the
  extensions it mirrors, checks them against the licence keys its operator trusts, and serves them
  to its own users from the channel they install from. An Enterest tenant serves the licences
  Enterest imported into it the same way. An air-gapped kista takes a licence file from its
  administrator.
- **The SDK checks and reports.** A licensed extension embeds an open C++ SDK (phase 2). It fetches
  the licence from the repository the extension was installed from, through httpfs with the same
  http secret as `INSTALL`, verifies it offline against Enterest's public keys compiled in, and
  gives the developer one call, `Check()`, for the common case ("refuse without a licence") and the
  plan and limits for the rest. A limit such as "at most 5 attached sources" is the developer's own,
  not kista's. The SDK also delivers the **usage reports** the extension makes (events with counters) to the
  repository; kista keeps
  them for the organisation's audit (phase 1b) and forwards daily aggregates, without identities,
  to the upstream the extension came from (phase 1c).

There are no seats in this version: DuckDB has no uninstall, and counting principals would need a
model this version does not have. The threat model is the honest user (spec 0001): licensing
prevents the accident, not the deliberate act.

Phases:

- **1a. Licences in kista**: the licence format, the Go reference verifier and test vectors, the
  `licenses` table, import and removal (API, CLI), the relay from upstreams, the licence routes,
  events.
- **1b. Usage reports**: the SDK's route, counts, the `extension.use` audit event, the statistics
  API.
- **1c. Forwarding**: daily aggregates to upstreams, and the receiving side a kista offers its
  downstream kistas.
- **1d. The console**: a Licences screen, the upstream's usage switch, usage in Statistics.
- **2. The C++ SDK** (`sdk/cpp`) and end-to-end tests with the pinned DuckDB: part 1 the verifier,
  part 2 the DuckDB side and the e2e cases.

## Problem

- A developer who wants to be paid for an extension has nowhere to say "this organisation may run
  version 2 at the `pro` plan until March". Without that, a paid extension is either public and
  unpaid, or private and unusable outside one organisation.
- An organisation that buys an extension installs it from its own kista (a mirror of Enterest), or
  its users install it from its Enterest tenant directly. Either way the extension has to learn, on
  the user's machine, that it is licensed, without the user configuring anything new, and keep
  working through short outages and on air-gapped sites.
- An extension file is easy to copy. The simplest control a developer wants is "it does not run
  without a licence for it here".
- The organisation wants to see who uses what (audit), and the developer wants to know how the
  extension is used, at a grain the organisation agrees to. kista sees downloads (spec 0010), not
  use.
- Developers want controls of their own ("the `basic` plan attaches at most 2 sources"), which no
  repository can know or enforce.

## Design

### Who does what

| Party | Does |
| --- | --- |
| **Developer** | Builds the extension with the SDK, names its plans and limits in Enterest, and enforces them in code. |
| **Enterest** (closed) | Catalogue, terms, purchase, billing; issues licences signed with its licence key; imports them into its tenants (the API below); receives forwarded usage. |
| **kista** (open, this spec) | Verifies, stores and serves licences; relays them downstream; takes usage reports, keeps them, forwards daily aggregates upstream. Never issues a licence. |
| **SDK** (open, phase 2) | Fetches and verifies the licence, `Check()`, plan and limits, reports usage. |
| **Organisation's administrator** | Decides who may install (grants), sees licences and usage, switches forwarding off. |

Spec 0001 put licence keys and issuance in the open server. The owner decided otherwise
(2026-10-17): Enterest is the only root of licence trust, and kista relays. Spec 0001 is updated
to say so. Spec 0002's and 0003's key-role separation for licence keys applies to Enterest's own
deployment, where its licence key must be distinct from every channel and attestation key.

### The licence

A licence is a JWS in compact serialisation (RFC 7515), RS256 (RSASSA-PKCS1-v1_5, SHA-256), at most
16 KiB. The key is **exactly RSA-2048 with `e = 65537`**: the SDK verifies with DuckDB's bundled
mbedtls wrapper, which takes 256-byte signatures only (`third_party/mbedtls/mbedtls_wrapper.cpp:75`
on the pin).

**Header**, exactly these members:

```json
{"alg": "RS256", "typ": "kista-license+jwt", "kid": "sha256:<64 lowercase hex digits: the SPKI DER's SHA-256>"}
```

`typ` is the domain separator: a verifier refuses any other value, so a JWT signed for another
purpose by the same key is never a licence.

**Payload**, a JSON object:

| Member | Required | Meaning |
| --- | --- | --- |
| `v` | yes | The format version, `1`. A verifier refuses any other. |
| `iss` | yes | The issuer's URL: `https://` and at least one more byte, all printable ASCII (0x21-0x7E), at most 256 bytes. |
| `jti` | yes | The licence id, `[A-Za-z0-9_-]{1,64}`, unique per issuer. |
| `sub` | yes | The holder's id at the issuer (for Enterest, an organisation), 1 to 128 bytes. |
| `holder` | no | The holder's display name, at most 200 bytes. |
| `ext` | yes | The extension name (`[a-z0-9_]{1,64}`, spec 0008). |
| `versions` | no | `{"min": "1.2", "below": "2.0"}`, either member or both. Absent: every version. |
| `plan` | yes | `[a-z0-9][a-z0-9_-]{0,31}`. |
| `limits` | no | An object of at most 64 members, names `[a-z][a-z0-9_]{0,63}`, values `true`/`false`, integers, or strings of at most 256 bytes. Meaning is the developer's. |
| `iat` | yes | Issued at, seconds since the epoch, not negative. |
| `exp` | yes | Expiry, after `iat`. |
| `grace` | no | Seconds the licence still works after `exp`, 0 to 7,776,000 (90 days); absent means 604,800 (7 days). |

**Parsing rules**, so the Go and the C++ verifier never read one token differently:

- base64url without padding (RFC 7515 §2), refusing padding, whitespace, other alphabets and
  non-zero trailing bits;
- strict UTF-8 JSON (RFC 8259), whitespace only space, tab, line feed and carriage return (no
  byte order mark); invalid UTF-8 (overlong forms, encoded surrogates, beyond U+10FFFF) and
  unpaired surrogate escapes are refused, never replaced; U+0000 is refused in every string and
  member name, escaped or not (a C string would end there);
- the top-level value is level 1, and no array or object is deeper than level 32;
- lengths are counted in bytes of the decoded UTF-8 value, not of its escaped form;
- no duplicate member names at any depth, compared after unescaping (DuckDB's bundled yyjson
  accepts duplicates, so the SDK walks objects itself);
- every number is a JSON integer: no fraction, no exponent, within ±(2^53-1); `exp + grace` must be
  within that too;
- unknown members are ignored: they are signed, and a change of meaning bumps `v`.

**Versions** are a lowercase `v` optionally, then 1 to 4 dot-separated components of 1 to 9 digits
without leading zeros (`0` itself allowed), compared component by component, missing ones as zero
(`1.2` = `1.2.0`). A `min` or `below` not of that form makes the licence `invalid`; a `min` not
below `below` is allowed and covers nothing. An extension
version not of that form (a commit hash) is outside every range, so a licence with `versions` does
not cover it.

**Status**, the first row that applies, with a skew of 300 seconds:

| Status | When |
| --- | --- |
| `invalid` | Not a licence: format, parsing rule, `typ`, `alg`, a `kid` not among the trusted keys, signature, a payload rule, or `ext` not the extension asking. |
| `not_covered` | The extension's version is outside `versions`. |
| `not_yet_valid` | `now + skew < iat`. |
| `valid` | `now < exp`. |
| `grace` | `now < exp + grace`. |
| `expired` | Otherwise. |

The holder is not checked by a verifier: the extension cannot know which organisation runs it.
A licence copied to another organisation works, and names its holder wherever it goes (in the
file, in the console, in usage reports). That is the honest-user line.

**Reference verifier, vectors, a test signer.** `internal/license` has `Parse` (header and
payload, all rules above, no keys; also how a stored licence's claims are read back),
`Token.Verify(keys)` (the key and the signature), `Claims.Status(ext, version, now)` and
`Verify(token, keys, ext, version, now)`, all three in one, which the vectors describe; with an
empty `version` the `not_covered` row is skipped (kista stores licences for a name, not a
version). A refusal is a `license.Error` with its rule; `ErrUntrustedKey`, `ErrSignature` and
`ErrWrongExtension` are the ones a caller tells apart. A generator (`go run ./internal/license/vectorgen`) writes
`internal/license/testdata/vectors.json` from a test key committed beside it (named as a test key,
never trusted by a release build): each case is a token, the extension and version asking, `now`,
the expected status and claims, and for an invalid token the stable code of the rule it breaks
(`license.Rule*`: `json_duplicate`, `iss`, `kid_untrusted`, `signature`, …), so the C++ verifier is
checked to refuse each token for the same reason. They cover every row and parsing rule above
with both sides of each boundary, a 3072-bit and a 1024-bit key (named by their own and by the
trusted `kid`), a PSS signature, and the order of the status rows. The generator signs with plain
modular arithmetic, so no library's key-size policy or random salt changes the file. RSA PKCS#1 v1.5 is deterministic, so the file is reproducible; CI regenerates it and fails
on a difference. The C++ SDK's tests (phase 2) read the same file. `vectorgen sign -claims <file>`
signs with the test key, so a developer can test the SDK locally without Enterest.

### Licences in a tenant (1a)

```text
licenses  tenant_id, name, license_id, issuer, holder_id, holder, plan, token, key,
          issued_at, expires_at, grace_until, source ('import' | 'upstream'),
          upstream_id null, visibility ('private' | 'public'), actor, updated_at, version
          primary key (tenant_id, name)
```

One licence per extension name in a tenant, served from every channel of the tenant (the relay
brings it through one upstream's channel; a promotion carries the extension to others). `token` is
the JWS as received; the other columns are its verified claims, for listing and for the console.
Migration 0024, additive, on all three dialects (the plan's column is `plan_name`: `PLAN` is a
reserved word on SQL Server). It also adds `license_syncs` (`upstream_id` primary key, cascading
with the upstream; `tenant_id`, `requested_at`, `last_run_at`, `last_result`): the relay's state per
upstream. Both join spec 0019's reap steps; `licenses.upstream_id` has no foreign-key cascade:
removing an upstream removes its licences explicitly (below).

**Trusted keys** are server configuration, never data (a file-only section, as server
administrators and credentials are):

```yaml
licenses:
  keys:                     # the issuers' licence public keys, SPKI PEM; none: licensing is off
    - |
      -----BEGIN PUBLIC KEY-----
      …
      -----END PUBLIC KEY-----
  refresh: 6h               # the relay's interval, 1h..24h
```

kista stores only licences whose status is `valid` or `grace` against these keys, under the name
they name. Without keys, imports and the relay are refused (the API: `409`, "licensing is off"),
and the routes below answer as for a missing path, the list route included: an empty list would
make a downstream relay revoke everything it holds. `GET /api/v1/licenses/keys`
(server administrators) and the start-up log show the fingerprints. The docs say where Enterest
publishes its keys; nothing defaults to them. An operator may trust other keys; the SDK will still
refuse their licences, which the console says.

**Import** (an air-gapped site, or Enterest writing into its own tenants): a tenant administrator
posts the token; the name is the licence's `ext`. A new licence answers `201` with `Location`. A
licence already stored for the name is replaced only with `If-Match` on it, whatever its source;
without one the answer is `409`.

**Relay** from upstreams: every `refresh`, one replica asks each `repository` upstream of an active
tenant that has a credential and is not paused (bundle-mode upstreams take no credential and are
not asked). A run holds the upstream's lease (`kista/licenses/upstream/<id>`, 15 minutes, above a
run's worst case), checks the upstream is still due once it holds it, and a replica runs at most two
at once:

```text
GET <upstream prefix>/-/licenses        Authorization: Bearer <the credential's token>
→ 200 {"licenses": ["<jws>", …]}        at most 1,000, at most 8 MiB, through egress
```

- An answer over either cap, or one that does not parse, is a failure: nothing changes.
- On `200`, each licence is checked: it verifies, and the upstream supplies its `ext` (an upstream
  does not hand out licences for names it does not supply): the upstream brought a build of it into
  its channel since it was added (a release whose provenance names it, created after it), and a
  live release of that build is in the tenant, in any channel (a promotion releases the same
  build, so yanking the mirror's own copy keeps the promoted ones supplied; an upstream removed and
  added again under the same name supplies only what it brings itself). A licence that fails is
  refused with its reason.
- A licence is stored when there is none for its name, when the stored one came from this upstream
  and the new one has a later `iat`, when the new one's `exp` is later than the stored one's
  (whatever the stored one's source), or when the stored one is past its grace.
- The answer is applied in one transaction that reads the upstream again: one paused, removed or
  given another prefix or credential meanwhile takes nothing from it.
- `200` is authoritative for this upstream: a stored licence whose `source` is this upstream and
  that the answer leaves out is removed (revocation). Licences from other sources are untouched.
- `404` means the upstream serves no licences, and `401`, `403`, `5xx` or a network failure is a
  failure: nothing is removed, and licences from it stay until `grace_until`.
- Licences past `grace_until` are removed, whatever their source, by an expiry pass every replica
  tries each 30 seconds under one lease (`kista/licenses/expiry`); a tenant being deleted is
  skipped.
- **Removing an upstream** removes its licences in the same transaction, each with a
  `license.remove` event. **Clearing its credential**, or removing the credential from the
  configuration, stops the relay; its licences stay until `grace_until`.
- Each run records `license.sync`. `POST …/upstreams/{u}/licenses/sync` asks for a run now (`409`
  for an upstream that relays nothing, or a paused one).
  Spec 0009's credential rules hold: the token goes only to the credential's prefixes, never on a
  redirect, never logged.

**A changed plan** (an upgrade, a downgrade, a renewal) is a new licence from Enterest with a later
`iat`; nothing in kista or on the user's machine is reinstalled:

- a relayed licence is replaced at the next relay run (`refresh`, 6 h by default); an administrator
  who wants it now runs `kista admin upstream licenses <tenant> <upstream> -sync` or presses Sync in the
  console. An Enterest tenant has it as soon as Enterest imports it;
- the SDK asks the repository at every `LOAD` (a conditional request, below), so a user has the new
  plan at the next `LOAD` in a new process, or at once with `myext_license_refresh()`. The binary is
  untouched: the licence is not part of it, so `FORCE INSTALL` is neither needed nor enough.

**Visibility.** A licence is `private` by default: served only to a token holding `install` on the
name in the channel. An organisation whose kista serves its own users without tokens (an internal
network, public releases) sets the licence `public` (`POST …/licenses/{name}/public`, `If-Match`,
a `license.change` event): it is then served to whoever is served a public release of the name.
The console warns that a public licence works for anyone who can reach the channel. A relayed
licence keeps the visibility its stored predecessor had.

### Routes on a channel prefix

DuckDB never asks for these paths: `-` is not a DuckDB version (`store.ValidDuckDBVersion`), an
extension name (`[a-z0-9_]{1,64}`), a tenant or channel name, or `.well-known`, so they cannot
collide with spec 0006's layouts. They are matched before the binary grammar, each with a route
label of its own for metrics and for `auth.failure` and `authz.refused` (`data.route`).

```text
GET  <prefix>/-/licenses/<name>     the licence for one extension          1a  label license
GET  <prefix>/-/licenses            the licences the caller may use here   1a  label licenses
POST <prefix>/-/usage/<name>        an SDK's usage report                  1b  label usage
POST <prefix>/-/usage               a downstream kista's daily aggregate   1c  label usage-daily
```

The `GET` routes take `GET` and `HEAD` and no body, as spec 0006's routes do; the `POST` routes take
`POST` only, with a JSON body (`405` with `Allow` otherwise). A passthrough channel has none of them
(`404`).

**Authorisation**, from the path only, before any body is read:

- the caller may use `<name>` here if it holds `install` on `<name>` in this channel and an active
  or deprecated release of `<name>` exists in it (pull-through names not fetched yet do not count);
  for a `public` licence, and for usage reports, also anyone who is served a public release of
  `<name>` here;
- otherwise the answer is the binary route's answer for a missing path **for this caller** (spec
  0006: `401` without a valid token, `404` with one), byte-identical, so the routes reveal nothing
  about names the caller may not see; a missing licence answers the same;
- on plain `http` a token is ignored, as on the binary routes, so only public licences and public
  names' reports work there;
- answers carry `Cache-Control: private, no-store` and `Vary: Authorization`.

`GET …/-/licenses/<name>` answers `200`, `Content-Type: application/jwt`, the token as stored (one
past `grace_until` is gone), with `ETag` `"<jti>.<iat>"`; `If-None-Match` with it answers `304`.
`GET …/-/licenses` needs a valid token (anonymous: `401`) and answers
`{"licenses": [...]}` with the private and public licences of the names the caller may use here.
It is what a downstream kista's relay reads, so an organisation's kista serves Enterest's licences
onward to its own regional or air-gapped kistas without any of them issuing anything.

### Usage reports (1b)

**A usage report**, `POST <prefix>/-/usage/<name>`, `application/json`, at most 64 KiB. The C++
SDK sends one event with a count of 1 per call; the format allows up to 32 events with any counts,
so an extension or another client may aggregate first:

```json
{"version": "1.4.0", "platform": "linux_amd64", "duckdb_version": "v2.0.0",
 "license": "<jti or omitted>", "status": "valid",
 "events": [{"name": "query", "count": 1},
            {"name": "attach", "count": 12, "counters": {"sources": 30}}]}
```

- `version` must be a version of a release of the name the caller may use in this channel, and
  `platform` and `duckdb_version` follow spec 0006's grammars; otherwise the report is dropped
  silently (`204`). `status` is the SDK's licence status (`missing` when there is none). Event and
  counter names are `[a-z][a-z0-9_.]{0,63}`; at most 32 events and 16 counters per event; counts and
  counter values are integers from 0 to 2^53-1; `version`, `platform`, `duckdb_version` and
  `status` are required, and `license` is a licence id. A report that breaks these rules answers
  `400` with the rule, as `<rule>: <message>` (`json`, `version`, `status`, `license`, `events`,
  `name`, `value`; `415` and `content` without `application/json`, `413` and `size` over 64 KiB);
  this happens only after authorisation. Unknown members are ignored, so a later SDK may send more.
- The answer is `204`. A report is the client's claim, not evidence: anyone who may install a public
  release may send one.
- **Counts** are summed in memory per `(tenant, channel, name, version, event, counter, status, day,
  authenticated)`, where `counter` is `''` for the event's own count and `status` is the report's,
  so use without a licence can be shown apart (1d). Every minute they are upserted
  into `usage_counts`, as spec 0010 counts downloads: key order, 500 keys a transaction, the same
  SQL Server lock pattern (`kista/usage_counts`). Sums saturate at 2^53-1, in memory and in the
  database, so the sum of claims never overflows. Event names are the client's, so a replica keeps
  at most 256 distinct (event, counter, status) keys per (channel, name, version) a day; a report
  adding more is dropped whole and counted in the metric. The body is read within 10 seconds. The
  budget is usage's own: 100,000 keys waiting,
  beyond which reports are dropped and counted in a metric. A burst of reports never takes download
  counts with it.
- **Audit**: a report by a token holding `install` on the name is an `extension.use` event:
  - actor: the principal;
  - subject: `channel:<c>/ext:<name>`;
  - `data`: version, platform, DuckDB version, licence id, status, event names.

  An event is written only for the first such report per (principal, channel, name, version, day)
  on a replica. Dedup runs through a bounded map of its own (100,000 entries, least recently used
  dropped), capped at 1,000 a day per principal and 100,000 principals a day, through spec 0010's
  asynchronous writer. A report with a valid token that does not hold `install` (on a public release)
  counts as authenticated and makes no event, as downloads do. Anonymous reports count and make no
  event.
- **Users**: spec 0010's hourly statistics pass also counts, for each finished day, the distinct
  principals of that day's `extension.use` events. It counts them per (channel, name, version) and
  stores them in `usage_users`, written only by that pass. No principal is kept. A principal is a
  user only on a day its extension sent a report: the SDK reports nothing by itself, so an extension
  that reports nothing has no users here (installs are under downloads).
- `statistics.usage_accept: false` (default `true`) makes the route answer `204` and keep nothing,
  for a site that wants no usage data at all.

**Statistics API**:

```text
GET /api/v1/tenants/{t}/stats/usage?from=&to=&channel=&extension=&group=<d>[,<d>]
    d: day | channel | extension | version | status
    → {from, to, group, rows: [{dimensions…, event, counter, value, authenticated}],
       users: [{dimensions…, users}]}     grouped by status, received users carry "received" (1d)
```

Readers, scopes, `404` outside a scope, and the limits on `from` and `to` are spec 0010's for
`/stats/downloads`. Values are local counts plus received aggregates (1c).

### Forwarding (1c)

Each `repository` upstream with a credential has `usage`: `forward` (the default) or `off`.

A daily pass runs on one replica (lease `kista/usage/forward`). It skips paused upstreams. For
each upstream set to `forward`, it sends each of the last 3 days that ended at least an hour ago.
Each day carries the `(name, version)` pairs the upstream brought into its channel since it was
added, counted only in the channels that hold a release of such a build (the build-based rule of
the relay: a promotion releases the same build elsewhere; the same version from another source is
not this upstream's):

```text
POST <upstream prefix>/-/usage     Authorization: Bearer <the credential's token>
{"day": "2026-10-17", "sender": "<the upstream's id>", "part": 1, "parts": 1,
 "reports": [{"extension": "myext", "version": "1.4.0", "users": 42,
              "events": [{"name": "attach", "count": 380, "counters": {"sources": 912}}]}]}
```

- The day's totals for those rows are summed over the tenant's channels, authenticated and
  anonymous, local and received.
- The request carries no principal, address, channel name or licence id. `sender` is the upstream
  record's random id: it keeps two upstreams of one kista that share a credential apart at the
  receiver.
- `users` is left out below 5, so a small organisation's single user is not singled out.
- At most 1,000 reports, 20,000 counts and 1 MiB per request; a day that needs more goes in several
  requests, each with `"part": n` and `"parts": total`. A day with nothing is sent as one empty
  part. A version's report holds at most 1,024 counts (counters beyond are cut, by name), and a
  version with neither events nor users is left out.
- **A day is sent whole and replaces what the upstream holds for it from this caller**: the first
  part clears the caller's day, and each part replaces the `(name, version)` pairs it carries (a
  pair is never split). A part that fails does not stop the next ones, so the receiver may hold a
  partial day until the next run; a day failing on its last run stays partial. Resending, whole or
  in part, is therefore harmless: the pass sends the last 3 days every day, so late reports reach the upstream,
  and a failure is retried the next day. `404` means the upstream takes no usage.
- Each run records `usage.forward`: days sent, skipped (`404`), failed, each with its reason.
- What a subscriber discloses is exactly that request. The docs say so, `GET /api/v1/credentials`
  shows which credentials forward usage (`forwards_usage`), and `POST …/upstreams/{u}/usage
  {"usage": "off"}` stops it. `GET …/upstreams/{u}/usage` answers the switch and the last run,
  kept in `usage_forwards`.

**The receiving side**, `POST <prefix>/-/usage`, is kista's too. So a regional kista forwards to
the organisation's kista, and that one forwards to Enterest:

- **Access:** a valid token is required (anonymous: `401`).
- **Limits:** `application/json` (`415` otherwise), at most 1 MiB (`413`), read within 30 seconds;
  at most 1,000 reports, 20,000 keys and 1,024 keys per report, `part` within 1..`parts` and `parts` at most 1,000, and
  `day` one of the last 7 finished days (`400` with the rule otherwise).
- **Scope:** a report is kept only for a name the caller holds `install` on, at a live version: its
  counts and users are the sender's claims, and a public release's could be made up by anyone.
  Reports for other names are dropped. The answer is `204` whatever was dropped, so it reveals
  nothing. Each request kept is a `usage.receive` event (actor the principal, subject the channel,
  with the day, part, sender digest and the reports kept and dropped).
- **Storage:** the rest replaces the caller's rows in `usage_received` for that day and channel, as
  the sender's rule above says, each part in one transaction, so a resend never doubles a count. The
  sender is kept as the SHA-256 of the principal and the `sender` field (pseudonymous: whoever can
  guess a principal can recompute it; the key stays within SQL Server's index limit). A version's
  users are a row with an empty event; rows are inserted 100 to a statement.
- Received rows are added to the statistics (status `received`, since statuses are not forwarded)
  and to this kista's own forwarding. Two kistas forwarding the same versions to each other would
  count each other's totals again; a chain is a tree, never a cycle.

### Data model

```text
licenses        (above)                                                            1a, 0024
usage_counts    tenant_id, channel_id, name, ext_version, event, counter, status,
                day, authenticated, value; primary key all but value               1b, 0025
usage_users     tenant_id, channel_id, name, ext_version, day, users;
                primary key all but users; replaced by the hourly pass             1b, 0025
usage_received  tenant_id, channel_id, sender, day, name, ext_version, event,
                counter, users, value; primary key all but users and value         1c, 0026
upstreams.usage 'forward' | 'off', not null default 'forward'                      1c, 0026
usage_forwards  upstream_id (pk, cascade), tenant_id, last_run_at, last_result      1c, 0026
```

All of it is additive. The usage tables follow `statistics.retention`, and all of them join spec
0019's reap steps (its table-list test covers them).

### API and CLI

```text
GET    /api/v1/tenants/{t}/licenses                        tenant administrators, licence managers (1a, 0029)
GET    /api/v1/tenants/{t}/licenses/{name}                 tenant administrators, licence managers, ETag (1a, 0029)
POST   /api/v1/tenants/{t}/licenses                        import, the JWS as the body (1a)
POST   /api/v1/tenants/{t}/licenses/{name}/public          tenant administrators, If-Match (1a)
POST   /api/v1/tenants/{t}/licenses/{name}/private         licence managers too, If-Match (1a, 0029)
DELETE /api/v1/tenants/{t}/licenses/{name}                 If-Match (1a)
GET    /api/v1/tenants/{t}/upstreams/{u}/licenses          the last relay run (1a)
POST   /api/v1/tenants/{t}/upstreams/{u}/licenses/sync     tenant administrators, 202 (1a)
GET    /api/v1/tenants/{t}/license-keys[/{name}]           the live builds' licence keys (0021, 0029)
GET    /api/v1/tenants/{t}/stats/usage                     (1b)
POST   /api/v1/tenants/{t}/upstreams/{u}/usage             {"usage": "forward"|"off"}, If-Match (1c)
GET    /api/v1/tenants/{t}/upstreams/{u}/usage             the switch and the last forwarding run (1c)
```

- **Who.** The licence routes and the licence keys' routes are the tenant administrators'. Since
  spec 0029 the licence managers (`auth.server_license_admins`) may call them too, except
  `…/public`. Only the tenant administrators make a licence public, and only they call the
  upstreams' relay routes.
- **A licence** is shown as its claims and its state: `name`, `id`, `issuer`, `holder_id`, `holder`,
  `plan`, `versions`, `limits`, `issued_at`, `expires_at`, `grace_until`, `key`, `source`,
  `upstream`, `visibility`, `status` (`valid`, `grace`; `expired` between the end of its grace and
  the expiry pass), with its `etag`. The API never answers the token; the CLI's `token` command
  reads it.
- **CLI** (`kista admin`, as every administration command):
  - `license keys`, `license list <tenant>`, `license show|token|remove|public|private <tenant> <name>`;
  - `license import <tenant> <file|-> [-replace]`;
  - `license verify <file|-> [-key <pem>]`: offline, prints the claims and the status against the
    configured keys or the given one;
  - `upstream licenses <tenant> <upstream> [-sync]`: the last relay run, and a run now;
  - `upstream usage <tenant> <upstream> [forward|off]` (1c): the switch and the last run, and a
    change.

### Events

New catalogue entries, with their subject forms in `audit.SubjectForm` and its coverage test:

| Event | Actor | Subject | Data |
| --- | --- | --- | --- |
| `license.import` | the administrator, or a licence manager (0029) | `license:<name>` | licence id, issuer, holder, plan, expiry, the replaced licence's id |
| `license.change` | the administrator, or a licence manager (0029) | `license:<name>` | `visibility` |
| `license.remove` | the administrator, a licence manager (0029), or `system:licenses` | `license:<name>` | licence id, why (`admin`, `revoked`, `expired`, `upstream removed`) |
| `license.sync` | `system:licenses` | `upstream:<u>` | counts (received, stored, replaced, unchanged, removed, refused), the refusals with their rules (a licence's own rule, `not_in_force`, `not_supplied`, `superseded`), or the failure |
| `extension.use` | the principal | `channel:<c>/ext:<name>` | (above) |
| `usage.forward` | `system:usage` | `upstream:<u>` | days sent, skipped, failed |
| `usage.receive` | the downstream's principal | `channel:<c>` | day, part, parts, sender digest, reports kept and dropped |
| `upstream.change` | the administrator | `upstream:<u>` | `change` `usage`, `value` |

### Console (1d)

- **Licences** (tenant administrators):
  - the list, with plan, holder, source, visibility, expiry and status; licences expiring within
    30 days are flagged;
  - a licence's claims and limits, and the fingerprint of the key that signed it. kista cannot know
    which keys an extension's SDK compiles in, so instead of a warning per licence the page says
    that a licence signed by a key other than the extension's issuer's is stored and refused by the
    extension (amended in 1d);
  - import from a file, making a licence public or private, and removing one;
  - the last sync per upstream, with its refusals.
- **Upstreams**: the usage switch, and one sentence on what it forwards.
- **Statistics**: a Usage tab with events and counters and users, by day, extension and version.
  Use without a licence (the reports' `status`) is shown apart, and so are aggregates received from
  downstream kistas: `/stats/usage` marks received users `received` when grouped by `status`.

### The SDK (phase 2)

A C++ library under `sdk/cpp` (Apache-2.0), for C++ extensions on the pinned DuckDB. A version for C
API extensions is a follow-up. The interface the developer sees:

```cpp
// in the extension's Load: registers the load hook and myext_license()
kista::License::Register(loader, "myext");

auto lic = kista::License::Get(context, "myext");   // cached; never throws for status
lic.Check();                                         // throws unless valid or grace: the one-line gate
lic.Status();                                        // valid | grace | expired | missing | invalid | …
lic.Plan();                                          // "pro"
lic.Limit<int64_t>("max_attached_sources", 1);       // the developer's controls, with a default
kista::Usage::Send(context, "myext", "attach", {{"sources", 3}});       // now; true when taken
kista::Usage::SendAsync(context, "myext", "attach", {{"sources", 3}});  // from a thread of its own
```

- **`Check()` is the simple case.** Most developers want only "is there a licence for this
  extension here". `Check()` throws a DuckDB `PermissionException` unless the status is `valid` or
  `grace`. The message names the extension, the status and what to do: `no licence for myext: ask
  your administrator, or place myext.kista-license next to the extension`. In `grace` it also logs a
  warning, once. A developer calls it from a table function's bind or a scalar function's first
  call: one line per entry point, and after the first call a cached lookup.
- **`myext_license()`** is a table function `Register` adds. It returns the status, plan, holder,
  holder id, licence id, issuer, issue and expiry times, the end of grace, the source (file or
  repository), the limits (as JSON) and a message, so a user or an administrator can see why a
  refusal happened. **`myext_license_refresh()`** fetches the licence again now, in a running
  process (after an upgrade, say), and returns the same row.
- **Where the licence comes from.** `Register` adds an `ExtensionCallback`. Its `OnExtensionLoaded`
  fires inside `FinishLoad`, after `ExtensionInstallInfo` is set (`extension_manager.cpp:12-21`,
  `extension_load.cpp:824-826` on the pin). There the SDK reads the licence in this order:
  1. A file `<name>.kista-license` beside the extension's own module, found with `dladdr` (or
     `GetModuleHandleEx` on Windows). DuckDB records no install directory. This file is also how an
     air-gapped user without a kista places one.
  2. The repository: `install_info->repository_url`, the prefix as typed in `CREATE EXTENSION
     REPOSITORY`, with a trailing `/` trimmed (`extension_install_dynamic.cpp:192,287`). It is empty
     for a static or a `LOAD '<path>'` extension, and a local (non-http) repository counts as none:
     then only the file is used.
  - **The fetch** autoloads httpfs, as `INSTALL` does, and goes through
    `HTTPTransportManager::CreateSession(context, url)`. httpfs adds the Bearer token of the http
    secret matching the URL, so a user who could `INSTALL` needs nothing else. The secret must still
    exist in a later process (persistent, or created again), or only the file works.
  - **When:** at every load, conditionally (`If-None-Match` with the file's licence), so a changed
    plan arrives at the next `LOAD` and an unchanged one costs a `304`; again when the licence in use
    is past half its lifetime, on the next `Get`, at most hourly; and on `myext_license_refresh()`.
    A 5-second timeout and no retries; a failed fetch keeps the file, and a `404` from the repository
    keeps it too (a revoked licence simply stops being renewed).
  - **The load hook's connection** (confirmed on the pin): `OnExtensionLoaded` has no
    `ClientContext`, so the SDK opens a connection of its own, in a transaction. It sees the http
    secret and global settings, but not the user's session settings (a session-only
    `ca_cert_file`, say). A fetch there that gets no answer settling it (anything but `200`, `304` or
    `404`) is made again once, on the first `Get`, with the user's context. After that, `Get` asks
    at most hourly while there is no answer, no usable licence, or the licence is past half its
    lifetime. HTTP never runs under the SDK's own lock.
  - **No downgrade:** a licence from the repository replaces a usable one (`valid` or `grace`) only
    if it is usable too and not older (`iat`). A stale or compromised repository cannot swap a
    working licence for an expired or older one. A licence that does not verify is never kept. The
    body is read up to the licence's maximum size.
  - **The file** is written through a temporary file of its own, created exclusively and never
    through a link, flushed, then renamed. Only a regular file is read.
  - **Where the result goes:** the fetched licence is written beside the module, through a temporary
    file and a rename, best effort, because the directory may be read-only.
- **Trust**: Enterest's licence public keys are compiled in. A test build may add the vectors' test
  key, only through a separate test-only library (amended in phase 2: no build option can put it
  into the library an extension links). No runtime setting can add a key. Superseded by spec 0021:
  each build names its licence keys (`kista_sdk_license_keys`); the SDK holds none.
- **Usage** (amended after phase 2 part 2: the SDK no longer counts or decides when to send):
  - **What the SDK does.** It delivers a report when the extension asks, and nothing else. One
    call is one report of one event (a count of 1) with the extension's counters, under the licence
    status of the moment, to the repository the extension was installed from, with the http secret
    matching the report's URL as the sending context sees it (normally the one `INSTALL` used). Nothing is sent at load or when a connection closes, nothing is kept in memory
    or on disk, and a failed report is not retried. The status is what the SDK knows at the time:
    after a load whose fetch was deferred, it settles at the first `License::Get`, so an extension
    calls `Get` first when the status matters.
  - **What the extension decides.** Which events and counters, when, how often, whether to
    aggregate first, and what to do when a report fails. Installs and downloads are counted by kista
    already (spec 0010), so the SDK reports no `load` event of its own. Users (above) come only
    from reports, so they count the principals whose extension reported something that day.
  - **`Send`** posts with the caller's context (in its transaction, or one of its own outside a
    query) with a 5-second connect and stall timeout and no retries, and waits for the answer. It answers `true` when
    the repository answered 2xx. kista answers `204` to reports it drops silently too (an unserved
    version, usage turned off), so `true` means delivered, not counted. It is called from the thread
    running the context's query, or with no query running; never from a `ClientContextState`
    callback (`QueryEnd`), where DuckDB holds the context's lock and `Send` would wait on it.
  - **`SendAsync`** posts from a thread of its own, with a connection of its own: it sees the http
    secret and global settings, not the caller's session settings, nor a connection- or
    transaction-scoped secret (a report then goes without a token and a private channel refuses
    it). It returns at once; with 16 of
    the extension's reports on their way in the process, it drops the new one, silently. The thread
    holds the database until the report is sent, so a short program that closes it at once still
    reports (the database closes after the report); at exit the process waits up to 6 seconds for
    such threads, not necessarily until they finish.
  - **Rules.** A report whose names, number of counters or values break the report's rules is not
    sent (`Send` answers `false`). Nothing is sent when `Register` was not called, the extension has no
    version, or it was not installed from an http repository.
  - **Why the SDK does no more.** An earlier design summed counts and sent them when a connection closed. A
    connection is not the extension's lifecycle: a server with a pool closes connections all the
    time, and that send ran under DuckDB's connection lock; a killed process lost its counts anyway.
    The extension knows its own lifecycle; the SDK does not.
- **Where DuckDB 2.0 keeps the module**: an extension installed from a named repository is under
  `<extension directory>/<version>/<platform>/repositories/<name>/`, and the licence file is kept
  there, beside it.
- **What the developer decides**: what each status allows (refuse, degrade, warn). The SDK never
  stops DuckDB by itself.
- **Delivery** (both parts implemented): part 1 is the verifier (`include/kista/license.hpp`: the strict JSON, the parsing
  rules, the key, the signature with DuckDB's bundled mbedtls, the status), checked against every
  vector in CI (`make sdk-test`, on the DuckDB build at the pin). The test key is only in a
  separate test-only library (`kista_sdk_testkey`), never in `kista_sdk`, which an extension links; part 2 is the DuckDB side above
  (`Register`, the fetch and the file, `Check`, the table functions, usage) and the e2e cases. Usage
  was then reduced to `Send` and `SendAsync` (above).
  Enterest's keys were to be added to `src/keys.cpp` when published; spec 0021 replaced that with
  keys each build names.
- **Confirmed in phase 2** (the e2e, in CI on Linux and run locally on macOS):
  - DuckDB links each loadable extension statically with DuckDB itself
    (`EXTENSION_STATIC_BUILD`), mbedtls included. `MbedTlsWrapper` and `HTTPTransportManager` are
    therefore the extension's own copies, and the hidden symbols do not matter.
  - The callback and the session work outside a user's query, with the caveat above.
  - Windows (`GetModuleHandleEx` for the module) is written but not tested yet: a follow-up.

## Security

- **kista issues nothing.** It holds no licence key; it stores only licences that verify against the
  operator's configured keys. A tenant administrator cannot add a trusted key. A compromised kista
  could serve a licence to the wrong user of the same organisation, never mint one.
- **Root of trust in the SDK**: Enterest's keys are compiled into the extension (since spec 0021,
  the keys its build names); nothing at run time adds one. A forged licence fails in the SDK even
  if a kista accepted it.
- **A licence is private by default.** Only a token holding `install` gets it. A public licence is
  the administrator's explicit act, with a warning, since anyone who reaches the channel can then
  copy it. The holder is not checked, unless the tenant expects one (spec 0028).
- **No existence leaks**:
  - the licence and usage routes decide from the path, before any body is read, and answer like the
    binary routes for a missing path, for each caller;
  - `GET …/-/licenses` lists only names the caller may use;
  - reports for versions or names outside the caller's view are dropped with `204`.
- **Relay binding**: an upstream's licences are accepted only for names it supplies. An answer over
  a cap changes nothing, so a truncated list never revokes.
- **Tenant isolation**: licences, counts and received aggregates are per tenant; a licence is served
  only from the channels of the tenant that holds it.
- **Credentials**: the relay and forwarding use spec 0009's credentials under its rules (prefixes,
  no redirects, never logged). A downstream caller's `Authorization` is never sent upstream.
- **Disclosure**: forwarded usage carries no principal, address, channel or licence id, and `users`
  below 5 is left out. Forwarding is on by default because it is part of buying a licensed
  extension; one switch stops it, and `statistics.usage_accept` keeps nothing at all.
- **Counts are claims**: any caller who may install a public release can report. Counts are for
  audit and trends, never for billing evidence; the docs say so.
- **DoS**:
  - body caps: 64 KiB per report, 1 MiB per aggregate, 8 MiB per relay answer;
  - key caps per request, and versions restricted to the ones served;
  - usage's own pending budget, apart from downloads';
  - spec 0010's per-principal caps on events;
  - received aggregates replace rather than add.

  A report never blocks a download.
- **The SDK's requests**: the licence fetch and the usage reports go only to the repository the
  extension was installed from, never through a redirect, with the http secret that matches the URL.
  - **Session settings.** `Send` and a fetch on `Get` use the caller's session, its TLS and proxy
    settings included (that is how a session's `ca_cert_file` works). A site where untrusted users
    run SQL against a secret they cannot read keeps them from changing HTTP settings (duckdb-acl).
  - **Trusted repository.** The timeout bounds a connect and a stall, not a slow answer, and a
    report's answer is read whole: the repository is the one the user installed code from, so it is
    trusted with the extension's requests as with its binary.
  - **Plain http.** An extension installed from an `http://` repository fetches and reports over
    plain http, as `INSTALL` downloaded it.
- **Parsing parity**: the format's parsing rules and the shared vectors make the Go and C++
  verifiers agree on every token, including hostile ones.
- **The honest-user line** (spec 0001): a patched binary, a rolled-back clock, a copied licence file
  or `allow_unsigned_extensions` defeat it. Each is a deliberate act; the licence's holder travels
  with it.

## Testing

- **`internal/license`**: the vectors (every status, every parsing rule, the boundaries), the
  generator's reproducibility in CI, and fuzzing of `Parse` against the duplicate-member and UTF-8
  rules.
- **Store suite** on all three dialects:
  - `licenses`: replace rules, `If-Match`, removal by source, upstream removal with events;
  - `usage_counts` upserts;
  - `usage_users` written by the pass;
  - `usage_received` replace per (sender, day, channel), several parts in one transaction;
  - tenant deletion reaps every table.
- **Relay**, with two kistas in one test:
  - answers `200`, `404` and `401`, an answer over a cap, a bad signature, an untrusted key;
  - a licence for a name the upstream does not supply;
  - replacement by `iat` and by `exp`, revocation by omission, expiry removal, paused and
    bundle-mode upstreams;
  - the credential is sent only to its prefix.
- **Routes**:
  - the authorisation matrix: anonymous; a token without a grant; a token with `install`; public and
    private releases; private and public licences; passthrough; pull-through names not fetched yet;
  - `401` and `404` byte-identical to the binary routes' for the same caller;
  - `http` against `https`, `no-store`, `405` and the body rules.
- **Usage**:
  - report validation; silent drops for unknown versions;
  - counts per key and usage's own budget;
  - `extension.use` once per day, with its caps;
  - `usage_users`;
  - the statistics API's scopes.
- **Forwarding**:
  - the rows chosen by provenance;
  - no principal in the body; `users` below 5 left out;
  - resending a day replaces it; parts; `404`;
  - the sender's bodies pass the receiver's parser, and received rows are forwarded on (the chain,
    link by link, in place of a three-kista test);
  - the receiving side's day window, caps and silent drops.
- **e2e** (phase 2): the pinned DuckDB runs a test extension built with the SDK and the test key.
  - `INSTALL` and `LOAD` from a private channel with an http secret; the licence is fetched and
    written beside the module; status `valid`; a limit is read; `myext_license()` shows the licence.
  - An upgrade: a new licence with another plan imported into kista; `myext_license_refresh()` and
    a new process's `LOAD` see it without reinstalling; an unchanged licence answers `304`.
  - The file is used offline.
  - An expired or a missing licence is refused by `Check()` with its message; a licence in grace is
    allowed.
  - Usage reported by `Send` (its answer) and by `SendAsync` reaches kista and is visible in
    `/stats/usage`; nothing is reported at load or when a connection closes.

## Alternatives considered

- **kista issues licences with a publisher's key (spec 0001's outline).** Every kista would become
  a potential root, and every tenant administrator a potential issuer. Trust would also be split
  across keys the SDK cannot know. One issuer, compiled into the SDK, is simpler, and it is where
  the selling happens.
- **Seats.** DuckDB has no uninstall and no stable client identity, so a seat model needs check-ins
  and release. Left out until there is demand. The forwarded `users` count is the data a later model
  would start from.
- **Short-lived per-user tokens signed by kista.** kista would need a key the SDK trusts (a
  delegation chain), which makes it a root again. A per-organisation licence relayed as it is needs
  nothing.
- **A licence in a DuckDB secret or a setting.** Every licensed extension would register the same
  secret type or setting, and they would collide. A file beside the extension does not.
- **Spec 0011's canonical JSON with a raw signature.** JWS needs no canonicalisation in C++, because
  the signature covers the bytes sent, and it is widely understood.
- **Forwarding as increments.** Sending a day once, as a sum to add, loses late reports and
  double-counts on retries. A whole day that replaces is idempotent.

## Follow-ups

- Licences carried in a bundle (spec 0014), for air-gapped sites without an administrator's import.
- The SDK for C API extensions.
- The SDK on Windows, tested (the module lookup with `GetModuleHandleEx` is written, not run).
- ~~Enterest's licence keys in `sdk/cpp/src/keys.cpp` once they are published.~~ Superseded by
  spec 0021: each build names its licence keys.
- Seats, or concurrent-use limits enforced with check-ins, if Enterest's plans need them.
- ~~An expected holder per tenant, set by a server administrator, so a licence for another holder is
  refused at import (useful to Enterest).~~ Done in spec 0028, on every path that stores a licence.
