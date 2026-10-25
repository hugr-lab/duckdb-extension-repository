---
title: Licensing
---

# Licensing

A paid extension checks, on the user's machine, that its organisation holds a **licence** for it.
kista keeps the organisation's licences and serves them to its users from the channel they install
from. kista issues nothing: a licence is signed by the issuer that sells the extension (Enterest),
and kista stores only licences that verify against a key the extension's own builds declare.

The design reasoning is in
[spec 0012](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0012-licensing/spec.md)
and, for the keys, [spec 0021](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0021-licence-keys/spec.md).
This page covers what kista does today: licences in a tenant, the relay from upstreams, the routes
an extension's SDK reads, the usage reports it sends, the daily usage forwarded upstream, and the C++
SDK an extension embeds.

## The licence

A licence is a small signed token (a JWS, `typ` `kista-license+jwt`, RS256 with an RSA-2048 key)
naming:

- the **extension** it is for, and optionally a **version range**;
- the **holder**: its id at the issuer (`sub`; for Enterest, an organisation) and its display name;
- the **plan** (`pro`, `enterprise`, …) and **limits** the extension's developer defines
  (`{"max_attached_sources": 5}`): kista carries them, the extension enforces them;
- when it was issued, when it **expires**, and a **grace** period after that (7 days unless the
  licence says otherwise, at most 90).

`kista admin license verify <file> -key <pem>` prints a licence's claims and status offline, checked
against the given public key.

## The keys a licence verifies against

Each build of a licensed extension carries its licence keys in a **key block**, written by the SDK
at build time (`kista_sdk_license_keys`; see the SDK's README). The extension trusts those keys
alone. `kista ext inspect <file>` prints them.

kista reads the key block from every body it takes in, but a string in a binary proves nothing by
itself. It **records** a build's keys only when a trusted source brought the body:

| Source | When |
| --- | --- |
| a publisher a server administrator **marked** as a source of licence keys | its publication carries the body, including a body already released |
| a `repository` upstream | a mirror run or a pull-through downloads the body, or a bundle import brings it |

Nothing else records keys: DuckDB's core and community upstreams, an unmarked publisher, a
principal, `release add`, and an upstream run that gets a `304` (it does not read the body). A
key whose source is gone counts for nothing.

- **Marking a publisher** is a server administrator's act (or a distribution admin's, for a
  publisher without keys or grants: [Access](access.md)): `kista admin publisher license-keys
  <tenant> <name> on|off`, or `POST`/`DELETE /api/v1/tenants/{t}/publishers/{name}/license-keys`.
  A publisher is marked only while it holds no key; the server administrator registers its keys
  afterwards. While marked, its keys, grants and the members of extension groups its grants name
  are changed by a server administrator (or a distribution admin) only; a tenant administrator can
  still remove it. See [Publishing](publishing.md#sources-of-licence-keys).
- **A malformed key block** in a marked publisher's publication is refused (`400`, `license_keys`).
- **Removing a source** (the publisher, or the upstream) drops the keys it brought. Unmarking a
  publisher keeps them.

A source's key is **live** when a live build of the extension declares it (a release that is
`active` or `deprecated`, in a signed channel that serves the extension from that source, whose
body is not blocked), or when the source's **newest available version** of the extension in the
[catalogue](upstreams.md#the-catalogue-and-a-channels-rules) declares it (not held, withdrawn or revoked, its body not blocked): a
licence is accepted as soon as the extension is available in the catalogue, before any channel takes
it. Yanking or blocking every build that declares a key, once newer versions declare another, takes
that key out (a rotation).

### A key's owner

The same key may reach a tenant through several sources: a distributor's publisher and an upstream
mirroring another kista both bring an extension built with it, or a build carries a copy of another
issuer's key. In a tenant, each key of an extension has one **owner**, and a licence signed with it
is that source's:

- **The first recorder owns it**: the first source whose own build of the extension recorded the
  key while the build counted (available, or placed; a held entry counts once its hold has ended or
  it is placed early, a revoked entry or a blocked body never, a withdrawn entry only if it counted
  before). Ownership is recorded then (a `license.key_owner` event) and stays: another source
  recording the same key later changes nothing (a `license.key_conflict` event, once per source and
  extension), so a build carrying a copied key gets no licence and takes none away. While a
  source's record of the key is on an entry still held, the key has no owner: a source recording it
  meanwhile gets no licence through it. Once that hold resolves (it ends, or the entry is placed or
  withdrawn), the first record to have counted owns the key.
- **A distributor's publisher** does not become the owner of a key of an extension that a
  `repository` upstream of the tenant lists (by name, or `*`): the tenant's own upstream keeps the
  extension's keys, and an administrator gives one to the distributor when that is what they want.
- **It changes only** when the owner goes or by a tenant administrator's act. Removing the publisher
  or the upstream drops its keys and its ownership, and the key then has **no owner** until a source
  records it afresh (a build arriving after the removal) or an administrator gives it: a source that
  recorded it while the other owned it, or whose records did not count then, does not take it by
  itself. The act:

  ```bash
  kista admin license owner acme geo_tools sha256:… publisher:enterest-ci
  ```

  or `PUT /api/v1/tenants/{t}/license-keys/{name}/{kid}/owner` with `{"source": "publisher:enterest-ci"}`.
  The source must have recorded the key for the extension. The licences signed with the key move to
  it (a `license.key_owner` event naming them): the owner's before, or, where it has none, one of
  source `unknown` signed with the key; refused (`409`) while the source holds a licence of that
  extension already: remove it first. A relayed licence moved to an upstream is that upstream's to
  revoke from then on; an imported one stays imported.

`kista admin license keys <tenant> [<name>]` and `GET /api/v1/tenants/{t}/license-keys[/{name}]`
list each name's live keys, the sources that brought each, how many builds declare it, and its
owner (`owner`, when it has one).

## On and off

Licensing is on by default:

```yaml
licenses:
  enabled: true        # false: <prefix>/-/licenses answers as a missing path; imports and relays are refused
  refuse_keys: []      # key ids (sha256:…) that count nowhere: a leaked key, while builds declaring it are live
  refresh: 6h          # the relay's interval
```

These settings are file-only. A key in `refuse_keys` is never accepted on import or relay and never
served. `licenses.keys`, the trusted keys of earlier releases, is accepted and ignored with a warning
at start; the next release refuses it.

## Licences in a tenant

A tenant holds one licence per extension name **and source**: the source that owns the key the
licence is signed with ([A key's owner](#a-keys-owner)), so DuckDB's `h3`, a distributor's `h3` and
an upstream's `h3` can each have a licence of their own. A licence is per package too (its type and
name, spec 0034), all of type `duckdb_extension` today: a channel serves only its own type's
licences, a key is live per (type, name, kid), and the licence routes and commands take `?type=` /
`-type` (`duckdb_extension` by default; an import stores the licence under that type, a relay under
its upstream's).

A licence's source shows as `owner` in the API (`upstream:<name>`, `publisher:<name>`, a removed
source's `upstream-id:<id>` or `publisher-id:<id>`, or `unknown`); `source` stays how it arrived
(`import` or `upstream`). The routes for one licence (`GET`, `DELETE`, `…/public`, `…/private`)
and `license show|token|remove|public|private` take `?source=` / `-source`, which a name with
licences of several sources needs (`409` without it); `GET …/licenses?source=` and `license list
-source` list one source's. A licence comes from one of three places.

**An administrator's import**, for a licence received by other means:

```bash
kista admin license import acme tresor.lic             # a new licence
kista admin license import acme tresor.lic -replace    # replaces the one stored for that extension and source
```

Over the API the token is the body of `POST /api/v1/tenants/{t}/licenses`; replacing a stored
licence needs `If-Match` with its ETag. The licence is stored for the source that owns its key for
the extension, and must verify and be in force (valid, or in its grace) when it is imported, against
that source's live keys. A key no source owns takes no licence (`kid_untrusted`). Where the source
holds no licence for the extension, the import replaces one of source `unknown` signed with the
same key (with `If-Match`, as any replacement). The answer's `Location` names the source
(`…/licenses/<name>?source=…`).

Besides the tenant's administrators, a **licence manager** (`auth.server_license_admins`, a
server-wide role for a job such as a licence reconciler) may call the licence routes of every
tenant, and nothing else. It cannot make a licence public, call the upstreams' relay or any other
route. See [Access](access.md#server-administrators-and-tenant-administrators).

**The relay from an upstream.** A `repository` upstream with a credential
that is not paused ([private upstreams](upstreams.md#private-upstreams)) is asked for the licences its credential holds, every
`licenses.refresh` (6 hours by default), at `<upstream prefix>/-/licenses`. For each licence:

- it must be in force;
- the upstream must supply that extension: a build of it that the upstream brought since it was
  added, available in the catalogue or live in a channel (not withdrawn, not blocked). An upstream
  does not hand out licences for names it does not supply (`not_supplied`);
- the upstream must own the licence's key for the extension (`key_owner` otherwise: another
  source's), and the licence must verify against a key **this upstream** brought, live
  (`kid_untrusted`);
- it must name the tenant's [expected holder](#an-expected-holder), when one is set (`holder_mismatch`);
- it is stored as this upstream's licence of the extension, and replaces the one stored for it when
  it is newer from the same upstream, or lasts longer. One imported for the same source is replaced
  only when both are signed with the same key, or the stored one's grace has ended, or its key is
  no longer live (or the server refuses it);
- a licence refused for its key stays stored if this upstream relayed it before: it is not served
  while no live build declares its key, and it ends with its grace.

An upstream's relay governs the relayed licences of its source, whichever upstream relayed them
(one the upgrade to spec 0033 gave it, one an administrator moved to it), and those of source
`unknown` it relayed itself: one it relays again unchanged becomes its own, one of source `unknown`
signed with the same key is replaced by its licence, and one it stops listing is removed
(revoked). A `404` from the upstream means it serves no licences and changes nothing, and so does a
failure (the credential refused with `401` or `403`, a network error, an answer over 1,000
licences or 8 MiB). Removing the upstream removes the licences its relay governs; clearing its
credential stops the relay, and its licences stay until their grace ends. A licence an
administrator moved to another source with its key is that source's, and an import an
administrator's: neither the relay nor the upstream's removal takes it. A licence past its grace
is removed, whatever its source.

**A bundle**, for an air-gapped site. The site's plan asks for licences
(`kista admin bundle plan -licenses`, optionally with the holder it expects), the exporting kista
carries the licences it holds for the names whose files the bundle carries
(`bundle export -licenses`: for each name, the licence of the source the files come from, the
exporting tenant's mirror or the source its own channel serves the name from), and the import takes
each into the first bundle-mode upstream of its source that supplies the name and owns its key (else
the first that supplies it, refused for the key). It is checked as a relayed licence is, against the
keys that upstream's builds declare; it never replaces an administrator's import, and a bundle never
revokes one ([Exporting a bundle](upstreams.md#exporting-a-bundle),
[Importing a bundle](upstreams.md#importing-a-bundle),
[Air-gapped sites](deployment/air-gapped.md#licences)).

`kista admin upstream licenses <tenant> <upstream> [-sync]` shows the last relay run (received,
stored, replaced, unchanged, removed, refused with reasons) and with `-sync` asks for one now;
the API has `GET` and `POST …/upstreams/{name}/licenses[/sync]`.

**In the console**, tenant administrators have **Licences**: the tenant's licences with plan,
holder, how each arrived, its source (**Of**), visibility, expiry and status (one expiring within 30 days, or in its grace, is
flagged); a licence's claims, limits and the fingerprint of the key that signed it; importing a file
(a licence already stored for the extension is replaced after a confirmation), making one public or
private, and removing one; the **licence keys** of the tenant's live builds, with their sources; and
each relaying upstream's last run with its refusals, and **Sync now**. A licence whose key no live
build declares is flagged: it is not served.

### An expected holder

A tenant belongs to one holder, but kista does not know which unless it is told. A server
administrator may set the holder a tenant's licences must name, their `sub`:

```bash
kista admin tenant create acme -holder-id org-7f3a   # at creation
kista admin tenant holder acme org-7f3a              # later
kista admin tenant holder acme -clear
```

Over the API it is `holder_id` in `POST /api/v1/tenants`, and `PUT`/`DELETE
/api/v1/tenants/{t}/holder` ([The expected holder](api.md#the-expected-holder)). In the
[console](console.md#screens)'s server scope, the new tenant form takes it, the tenants list shows
it, and a tenant's page sets and clears it. With a holder set,
every path that stores a licence in the tenant refuses one of another holder, after its signature
is checked and before anything is written:

| Path | The refusal |
| --- | --- |
| an import (`kista admin license import`, `POST …/licenses`) | `400`, `holder_mismatch`, naming the expected and the given holder |
| the relay from an upstream | refused with the reason `holder_mismatch`, counted in the run and its `license.sync` event; the others are stored |
| a bundle's import | refused with the reason `holder_mismatch`, counted in the import's record and its `bundle.import` event |

A licence stored before for that name stays, so a relay naming another holder revokes nothing. Only
a server administrator sets or clears the holder: a tenant administrator sees it (`tenant show`, the
tenant record) and cannot change it. Setting it while the tenant holds a licence of another holder
is refused, naming those licences: remove them first. Each change is a `tenant.holder` event.

A bundle plan's holder (`-license-holder`) is a separate check, made by the exporting kista; the
tenant's is made by the importing one.

### A changed plan

An upgrade, a downgrade or a renewal is a new licence from the issuer. Nothing is reinstalled: a
relayed licence is replaced at the next relay run (or at once with `-sync`), and the extension
fetches it again at its next `LOAD`. The binary is untouched, so `FORCE INSTALL` is neither needed
nor enough.

## What extensions read

An extension's SDK reads its licence from the repository it was installed from, with the same http
secret as `INSTALL`:

```text
GET <prefix>/-/licenses/<name>     the licence (application/jwt), with an ETag; If-None-Match → 304
GET <prefix>/-/licenses            {"licenses": [...]}: the licences the caller may use here
```

The second route is what a downstream kista's relay reads, so an organisation's kista passes its
licences on to its regional or air-gapped kistas.

Who gets a licence:

- a channel serves the licence of the **source it serves the extension from** (its rule for the
  name), and only while a live (active or deprecated) release there declares the key that signed
  it. Another source's licence, and a licence that no live build in the channel declares, stay
  stored and are not served there: a build carrying a copy of another source's key gets nothing;
- a **private** licence (the default) goes to a token holding `install` on the extension in that
  channel;
- a **public** licence (`kista admin license public <tenant> <name>`) also goes to whoever is served a
  public release of the extension there: for a kista that serves its own users without tokens, on
  an internal network. Anyone who reaches the channel can then copy it;
- everyone else gets exactly the answer of a missing extension path: `401` without a valid token,
  `404` with one, byte for byte. The routes reveal nothing about extensions the caller may not see.

Over plain `http` a token is ignored, as on the extension routes, so only public licences are
served there, and the list route always answers `401`. A passthrough channel has no licences, and
while licensing is off every licence route answers as a missing path.

## Usage reports

An extension reports its use, through the SDK, to the repository it was installed from, with the
same http secret. The C++ SDK sends one event with a count of 1 per call; the format allows more, so
a client may aggregate first:

```text
POST <prefix>/-/usage/<name>      Content-Type: application/json, at most 64 KiB → 204
```

```json
{"version": "1.4.0", "platform": "linux_amd64", "duckdb_version": "v2.0.0",
 "license": "lic-1", "status": "valid",
 "events": [{"name": "query", "count": 1},
            {"name": "attach", "count": 12, "counters": {"sources": 30}}]}
```

- **Who may report** is decided from the path, as for a licence: a token holding `install` on the
  extension in that channel, or anyone served a public release of it. Everyone else gets the answer
  of a missing path, and the body is not read.
- **The rules**: `status` is the SDK's licence status (`valid`, `grace`, `expired`, `not_yet_valid`,
  `not_covered`, `invalid`, or `missing` without a licence); `license` is the licence's id; event and
  counter names are `[a-z][a-z0-9_.]{0,63}`; at most 32 events and 16 counters per event; counts are
  integers from 0 to 2^53-1; `version`, `platform`, `duckdb_version` and `status` are required. A
  report breaking them answers `400` with the rule, as `<rule>: <message>`: `json`, `version`,
  `status`, `license`, `events`, `name` or `value`; `415` (`content`) without `application/json`,
  `413` (`size`) over 64 KiB. The rules are stable: an SDK's tests may rely on them.
- A report for a version the caller is not served in that channel, or with a malformed `platform` or
  `duckdb_version`, is dropped and answered `204` all the same.
- **Counts** are kept per (channel, extension, version, event, counter, licence status, day,
  authenticated), with no principal and no address, and written every minute. A count stops at
  2^53-1. Usage has a budget of its own (100,000 keys waiting): a burst of reports beyond it is
  dropped (`kista.usage.dropped`) and never takes download counts with it. A replica also keeps at
  most 256 distinct (event, counter, status) keys per channel, extension and version a day; a report
  adding more is dropped the same way, so an extension should use a fixed set of event names.
- **Audit**: a report by a principal holding `install` on the extension is an `extension.use`
  event, the first per (principal, channel, extension, version, day) on a replica. Once a day has
  ended, the hourly statistics pass counts that day's distinct **users** per (channel, extension,
  version) from these events. Anonymous reports, and reports by a token without `install`, count and
  make no event. A principal counts as a user only on a day its extension sent a report: the SDK
  reports nothing by itself, so an extension that reports nothing has no users here (installs are
  under Downloads).
- `statistics.usage_accept: false` answers `204` to every report it would take, and keeps nothing;
  who may report is still decided first.

Counts are claims, not evidence: anyone who may install a public release can report. They are for
audit and trends, never for billing.

Read them with the download statistics' scopes ([Observability](observability.md#download-statistics)):

```text
GET /api/v1/tenants/{t}/stats/usage?from=&to=&channel=&extension=&group=<d>[,<d>]
    d: day | channel | extension | version | status
    → {from, to, group,
       rows:  [{dimensions…, event, counter, value, authenticated}],
       users: [{dimensions…, users}]}     grouped by status, received users carry status "received"
```

Rows are always per event and counter (`counter` absent for the event's own count). `status` sets use
without a licence apart, and aggregates received from downstream kistas apart as `received`; `users` are summed over the stored rows (per channel, extension, version and
day), so a principal on two days, versions or channels counts twice when those are not grouped by.
Grouped by `status`, users are this kista's (no status: a principal is counted whatever the status
it reported) or received (`received`).

The console shows the same under **Statistics → Usage**: users per day, then users and events by
day, extension or version, with use without a valid licence (any status but `valid` or `grace`) and
received counts in their own columns.

## Forwarding usage upstream

An organisation that buys an extension tells its seller how the extension is used: kista forwards
daily aggregates, never reports or identities, to the upstream that supplied it. An organisation's
kista forwards to Enterest; a regional kista forwards to the organisation's.

Each `repository` upstream with a credential has a usage switch, `forward` (the default) or `off`:

```bash
kista admin upstream usage acme enterest off       # stop forwarding
kista admin upstream usage acme enterest           # the switch and the last run
```

Over the API: `POST …/upstreams/{name}/usage {"usage": "off"}` with `If-Match`, and
`GET …/upstreams/{name}/usage` for the last run. `GET /api/v1/credentials` lists, for each
credential, the upstreams that forward usage with it (`forwards_usage`).

Once a day, one replica sends each upstream set to `forward` (active, with a credential) the last 3
days that ended at least an hour ago:

```text
POST <upstream prefix>/-/usage     Authorization: Bearer <the credential's token>
{"day": "2026-10-17", "part": 1, "parts": 1,
 "reports": [{"extension": "myext", "version": "1.4.0", "users": 42,
              "events": [{"name": "attach", "count": 380, "counters": {"sources": 912}}]}]}
```

- **What is sent**: the versions of the upstream's builds (brought since it was added), each with
  its counts summed over the channels that serve the extension from this upstream and hold a release
  of that version from it (the same version from another source, or in a channel that serves the
  extension from another source, is not counted), all callers and licence statuses, local and
  received from downstream.
- **What is not**: no principal, address, channel name or licence id; `sender` is the upstream
  record's random id. `users` is left out below 5, so a small organisation's single user is not
  singled out, and a version with nothing else to say is left out. A version's report holds at most
  1,024 counts. This request is all a subscriber discloses.
- **Each day is sent whole and replaces** what the upstream holds for it from this kista, so sending
  it again is harmless: a late report reaches the upstream the next day, and a failed day is sent
  again. A large day goes in parts (at most 1,000 reports, 20,000 counts and 1 MiB each); the first
  part clears the day, and each part replaces the versions it carries. A part that fails does not
  stop the others, so the upstream may hold a partial day until the next run.
- An upstream answering `404` takes no usage: the day is skipped. Each run is a `usage.forward`
  event with the days sent, skipped and failed (with why).

**Receiving.** `POST <prefix>/-/usage` is kista's own route too:

- it needs a valid token (anonymous: `401`) and `application/json` (`415`); the body is at most
  1 MiB (`413`), 1,000 reports, 20,000 counts and 1,024 counts per report, `part` is within 1..`parts` (at most 1,000), and
  `day` is one of the last 7 finished days (`400` with the rule otherwise);
- a report is kept only for an extension the caller holds `install` on, at a version live in that
  channel: counts and users are the sender's claims. The others are dropped, and the answer is
  `204` all the same;
- the rest replace what that sender sent for the day in that channel. The sender is kept only as a
  digest of its principal and `sender` (pseudonymous). Each request kept is a `usage.receive`
  event;
- kistas forward up a tree: two kistas forwarding the same versions to each other would count each
  other's totals again;
- received counts and users appear in `/stats/usage` with the status `received` (users are told
  apart when grouped by `status`), and are forwarded on with this kista's own usage.

`statistics.usage_accept: false` keeps no received aggregate either.

## The C++ SDK

A C++ DuckDB extension embeds the SDK (`kista/sdk/cpp` in
[hugr-lab/extension-ci-tools](https://github.com/hugr-lab/extension-ci-tools), the developer kit,
Apache-2.0; see [Development](development.md#the-developer-kit-for-extensions)) to check its
licence and report its usage:

```cpp
// in the extension's Load
kista::License::Register(loader, "myext");

// where a licence matters
auto lic = kista::License::Get(context, "myext");
lic.Check();                                     // throws unless valid or grace
lic.Limit<int64_t>("max_attached_sources", 1);   // the developer's controls, with a default
kista::Usage::Send(context, "myext", "attach", {{"sources", 3}});       // now; true when taken
kista::Usage::SendAsync(context, "myext", "attach", {{"sources", 3}});  // from a thread of its own
```

- **Where the licence comes from.** When the extension loads, the SDK reads the file
  `myext.kista-license` beside its module. Then it asks the repository the extension was installed
  from, with the http secret `INSTALL` used. A new licence is kept beside the module, and an
  unchanged one costs a `304`. A user can therefore place the file by hand. An air-gapped site's
  kista receives its licences in bundles, and serves them as any other.
- **What a user sees.** `SELECT * FROM myext_license()` shows the status, plan, holder, expiry,
  source, limits, and why. `myext_license_refresh()` fetches the licence again now: after an
  upgrade, a running process sees the new plan without reinstalling.
- **Usage.** The extension reports what it wants, when it wants, to the same repository: one call
  is one report of one event with its counters, under the licence status of the moment. `Send`
  waits for the answer (a 5-second connect and stall timeout, no retries) and answers `true` when the repository
  answered 2xx: delivered, not necessarily counted. `SendAsync` sends from a thread of its own, with
  a connection of its own that does not see the caller's session settings. The SDK keeps nothing,
  retries nothing and reports nothing on its own: installs and downloads are counted by kista
  already.
- **Trust.** Each build names its licence keys (`kista_sdk_license_keys`), and trusts only those:
  the SDK has none of its own. A build without them does not link (or, on macOS with
  `-undefined dynamic_lookup`, does not load). A test build can trust the licence vectors' test
  key, through a separate test-only library.

The SDK's README covers building, the licence keys, the session caveat at load and the test key.
The SDK for C API extensions, and the SDK on Windows, are follow-ups.

## What licensing does not do

Licensing protects honest users from accidents, not against deliberate acts. A patched binary, an
extension loaded with `allow_unsigned_extensions`, a rolled-back clock or a licence copied to
another organisation defeat it; the licence names its holder wherever it goes. An
[expected holder](#an-expected-holder) keeps a licence out of the wrong tenant of one kista; it does
not stop a holder copying its own licence elsewhere.

## Events

`tenant.holder` (the expected holder set or cleared), `license.import`, `license.change` (visibility), `license.remove` (with the reason: `admin`,
`revoked`, `expired`, `upstream removed`; each names the licence's `source`), `license.key_owner`
(a key's owner recorded, by its first recorder with `first: true` or by an administrator with the
licences that `moved`), `license.key_conflict` (another source recording a key it does not own),
`license.sync` (each relay run), `extension.use` (a
usage report by a principal holding `install`), `usage.forward` (each forwarding run), `usage.receive` (each aggregate taken from downstream) and
`upstream.change` with `change: usage`. The licences a bundle carried are in its
`bundle.import` event and import record. See
[Observability](observability.md).
