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
  distributor's publisher only: spec 0037, [Access](access.md)): `kista admin publisher license-keys
  <tenant> <name> on|off`, or `POST`/`DELETE /api/v1/tenants/{t}/publishers/{name}/license-keys`.
  A publisher is marked only while it holds no key; the server administrator registers its keys
  afterwards. While marked, its keys, grants and the members of extension groups its grants name
  are changed by a server administrator only (or a distribution admin, for a distributor's
  publisher); a tenant administrator can still remove it. See
  [Publishing](publishing.md#sources-of-licence-keys).
- **A malformed key block** in a marked publisher's publication is refused (`400`, `license_keys`).
- **Removing a source** (the publisher, or the upstream) drops the keys it brought. Unmarking a
  publisher keeps them.

A key is **live** for a [catalogue entry](upstreams.md#the-catalogue-and-a-channels-rules) (a
source's builds of an extension in the tenant) when that source recorded it on **its own** builds
of the extension, while the source exists, and either:

- a **live build** declares it: a release that is `active` or `deprecated`, in a signed channel
  that serves the extension from that source, its body not blocked, not withdrawn. A held build
  placed early is live; or
- the source's **newest available version** of the extension in the catalogue declares it (not
  held, withdrawn or revoked, its body not blocked): a licence is accepted as soon as the extension
  is available in the catalogue, before any channel takes it.

A record one source made on another source's build counts for neither. A held build (not placed
early), a revoked, withdrawn or blocked one makes no key live, and a removed source has none.
Yanking or blocking every build that declares a key, once newer versions declare another, takes
that key out (a rotation). A key in `licenses.refuse_keys` counts nowhere.

### A licence's entry

A licence belongs to a **catalogue entry**: the tenant, the package type, the source and the name.
Each entry holds at most one. One tenant may hold two `geo_pro` entries: `publisher:enterest-ci`
with a licence Enterest imports, and `upstream:org` with none, or with its own licence relayed from
`org`.

- **Every write names the entry**: an import, a removal, `…/public` and `…/private` take
  `?source=` (`-source` on the CLI), `400` without it. The forms are `upstream:<name>`,
  `publisher:<name>`, `upstream-id:<id>` and `publisher-id:<id>`. The entry must be a live upstream
  or publisher of the tenant whose builds can declare keys: an unknown, removed or malformed source,
  `tenant`, or a DuckDB core or community upstream answers `400` naming why. A removal also takes
  the id form of a removed source whose entry still holds a licence.
- **Only that entry's keys count**: an import or a relay verifies the licence against the keys live
  for its entry, and a channel serves the licence of the entry it serves the name from. No other
  entry matters, whatever keys its builds declare.
- **A copied key** (another source's build declaring the same key) is another entry, with no licence
  until one is put there: by its own upstream's relay of a licence its keys verify, or by an
  administrator's import. The copy takes nothing from the genuine entry. Set an
  [expected holder](#an-expected-holder) when relaying from upstreams you do not fully trust.
- **Removing a source** drops its keys, so its entries have no live key. Removing an upstream removes
  the relayed licences of its entry; an import in a removed source's entry stays, listed under the id
  form, served nowhere, removable, and gone at the end of its grace. Nothing passes to another
  source. An upstream added again (a successor) is another entry: its relay supplies nothing until it
  has fetched builds itself, and its channel serves no licence until then.

`kista admin license keys <tenant> [<name>] [-source <s>]` and
`GET /api/v1/tenants/{t}/license-keys[/{name}][?source=]` list exactly what an import's key check
would accept at that moment: per name and key, the entries it is live for, each with the number of
its builds declaring it (and, for a distributor's publisher, the distributor). A key listed for
several entries is declared by several sources. Keys the server refuses are left out. With
`?source=`, one entry's keys: an empty list when it has none, `400` as for a write (an unknown,
removed or malformed source, `tenant`, a DuckDB core or community upstream). Being listed means the
key check passes; being in force, the holder and `If-Match` are checked separately.

The query's `?source=` names a licence's entry; the licence's own `source` field says how it
arrived (`import` or `upstream`). A write checks, in order: the caller, licensing being on, the
entry (`400` as above; a distributor's entry, below), then the licence (in force, its key live for
the entry, the holder, the [`older` rule](#older-licences-are-refused), `If-Match`).

**A distributor's entry** (spec 0037), a distributor's publisher's, live or removed, is the
distributor's contract with the holder: its licence manager and the server administrators write it,
and a tenant administrator only reads it. A tenant administrator's import, removal, `…/public` or
`…/private` there answers `404` with the problem type `distributors-entry` and the detail "a
distributor's entry: its licence manager or a server administrator writes it", recorded as
`authz.refused` with what it was. A licence's answer carries `distributor` when its entry is a
distributor's publisher's, as the keys listing does. See
[who writes a distributor's entry](access.md#server-administrators-and-tenant-administrators).

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

A tenant holds one licence per [entry](#a-licences-entry), so DuckDB's `h3`, a distributor's `h3` and
an upstream's `h3` can each have a licence of their own. A licence is per package too (its type and
name, spec 0034), all of type `duckdb_extension` today: a channel serves only its own type's
licences, a key is live per (type, name, kid), and the licence routes and commands take `?type=` /
`-type` (`duckdb_extension` by default; an import stores the licence under that type, a relay under
its upstream's).

A licence's entry shows as `entry` in the API (`upstream:<name>`, `publisher:<name>`, or a removed
source's `upstream-id:<id>` or `publisher-id:<id>`); `source` stays how it arrived (`import` or
`upstream`), and `upstream` names the relaying upstream. Reads keep `?source=` optional:
`GET …/licenses/{name}` and `license show|token` act on the name's only licence without it, and
answer `409` naming the entries when there are several; `GET …/licenses?source=` and `license list
-source` list one entry's. A licence comes from one of three places.

**An administrator's import**, for a licence received by other means:

```bash
kista admin license import acme tresor.lic -source publisher:enterest-ci             # a new licence
kista admin license import acme tresor.lic -source publisher:enterest-ci -replace    # replaces the one stored there
```

Over the API the token is the body of `POST /api/v1/tenants/{t}/licenses?source=<entry>`; replacing
the licence stored in the entry needs `If-Match` with its ETag (`409` without it). The licence must
be in force (valid, or in its grace) when it is imported and verify against **that entry's** live
keys (`kid_untrusted`, naming the entry, otherwise); its key must not be refused, and it must name the
tenant's [expected holder](#an-expected-holder) when one is set. The answer's `Location` names the
entry (`…/licenses/<name>?source=…`).

Besides the tenant's administrators, a **licence manager** (`auth.server_license_admins`, a
server-wide role for a job such as a licence reconciler) may call the licence routes of every
tenant, and nothing else. It reads every entry and every key, but writes (imports, removes, makes
private) only the entries of **distributors' publishers** ([Publishing](publishing.md#distributors-publishers)),
named as a distribution admin names them (`publisher:<name>`, `publisher-id:<id>`; a removed one's
id form for a removal). Any other `?source=`, whatever it is, answers as a missing path does (`404`,
recorded as `authz.refused`), before anything about it is checked. It reads every licence and every
key's entries, but its server-wide token writes only into distributors' publishers' entries: it
cannot copy a tenant upstream's licence into another entry, and cannot pin an import into an
upstream's entry where the relay's revocation never removes it. It cannot make a licence public, call the upstreams' relay or any other route. See
[Access](access.md#server-administrators-and-tenant-administrators).

**The relay from an upstream.** A `repository` upstream with a credential
that is not paused ([private upstreams](upstreams.md#private-upstreams)) is asked for the licences its credential holds, every
`licenses.refresh` (6 hours by default), at `<upstream prefix>/-/licenses`. Each licence goes into
the upstream's **own entry** (`upstream:<name>`) when:

- it is in force;
- the upstream supplies that extension: a build of it that the upstream brought since it was
  added, available in the catalogue or live in a channel (not withdrawn, not blocked). An upstream
  does not hand out licences for names it does not supply (`not_supplied`), and a licence it stored
  before for a name it no longer supplies is removed (revoked). An upstream whose builds of the name
  are all still held supplies nothing yet: its relay is refused `not_supplied` and the licence is
  stored at a later run, once the hold ends;
- it verifies against the keys live for the upstream's entry (`kid_untrusted`);
- it names the tenant's [expected holder](#an-expected-holder), when one is set (`holder_mismatch`).

It replaces the licence stored in the entry when it is newer from the same upstream, or lasts
longer, and is not [older](#older-licences-are-refused) than a licence the tenant holds from its
issuer for its holder (refused `older`, the stored licence staying: a licence that lasts longer no
longer replaces a later one of the same issuer and holder). An import in the entry is replaced only by a relayed licence signed with the same key that
lasts longer, or once the import's grace has ended or its key is no longer live (or the server
refuses it); the row is then the relay's, and the relay governs it (a `license.change` event names
the import it replaced and the upstream). A licence refused for its key or its holder stays stored
if this upstream relayed it before, and is tried again at the next run (a key no live build
declares now, a rotation under way): it ends with its grace meanwhile, and it is not served while
no live build of the entry declares its key.

Each entry gets the organisation's licence through its own relay or an import: two upstreams of one
remote (Enterest's `prod` and `nightly` mirrored into a tenant's `prod` and `nightly`) each store
their own copy, and a renewal reaching either first is taken by each at its own run. An upstream's
relay governs only the relayed licences of its own entry: one it stops listing is removed (revoked),
an import is never removed by it, and another entry's copy is that entry's. A `404` from the
upstream means it serves no licences and changes nothing, and so does a failure (the credential
refused with `401` or `403`, a network error, an answer over 1,000 licences or 8 MiB). Removing the
upstream removes the relayed licences of its entry; clearing its credential stops the relay, and its
licences stay until their grace ends. A licence past its grace is removed, whatever its entry.

**A bundle**, for an air-gapped site. The site's plan asks for licences
(`kista admin bundle plan -licenses`, optionally with the holder it expects), the exporting kista
carries the licences it holds for the names whose files the bundle carries
(`bundle export -licenses`: for each name, the licence of the entry the files come from, the
exporting tenant's mirror or the source its own channel serves the name from), and the import reads
each licence once, even when several of the bundle's sources carry it. It goes into **each**
bundle-mode upstream of a carrying source that supplies the name and whose live keys verify it,
checked as a relayed licence is (an [older](#older-licences-are-refused) one refused `older` per
target, before the refusal over an import); it never replaces an administrator's import, and a
bundle never revokes one. The import's counts (stored, replaced, unchanged, refused) are per target, and its
result lists each target's own counts (`targets`, by upstream)
([Exporting a bundle](upstreams.md#exporting-a-bundle),
[Importing a bundle](upstreams.md#importing-a-bundle),
[Air-gapped sites](deployment/air-gapped.md#licences)).

### Older licences are refused

kista refuses to store a licence when the tenant holds another of the same package (type and name),
from the same issuer (`iss`) for the same holder (its id), issued later (`iat`), in **any** entry,
and that licence is in a **distributor's entry** or was **verified by the same key** (spec 0037): the
rule `older`. An import, a replacement with `If-Match`
included, answers `400` "the licence is refused (older): …", naming the entry that holds the later
licence, checked after the key and holder checks and before the stored licence is compared (so not
`409`), and recorded as a `license.refused` event; the relay and a bundle refuse it with the rule
`older` in their results, and the stored licence stays.

- **A distributor's entry counts whatever its key**: only its licence manager and server
  administrators write it, so after the issuer rotates its key, its new licence there still
  supersedes a stale licence of the previous key imported anywhere in the tenant.
- **Elsewhere, the same key**: a stored licence's key is the fingerprint of the key material that
  verified it. A licence in any other entry signed with another key never blocks the issuer's,
  whatever its claims say: a tenant administrator who signs a licence with a key of their own
  (declared by a build of an upstream of theirs) and the issuer's `iss`, holder and a later `iat`
  cannot freeze the issuer's licences.
- **An equal `iat` is not older**: the same file again is taken (the console replaces a licence with
  the same file).
- **Other issuers are not compared**, nor other holders.
- **A stored licence stops counting** once its grace has ended, or while the server refuses its key
  (`licenses.refuse_keys`: a licence signed with a key later compromised cannot block newer ones).
  A key no longer live in its entry still counts: a tenant administrator could make it so (removing
  the distributor's publisher, blocking its bodies) to import an older licence elsewhere.
- **Going back on purpose**: remove the newer licence, then import the older one; both acts are
  audited.
- **Issuers must make `iat` increase for each holder.** Enterest issues `iat = max(now, last + 1)`.
  A third-party issuer that issued two licences in the wrong order sees the earlier-dated one refused.
- **Two delivery paths of one licence** (an issuer's licence for a holder reaching a tenant both by
  an import and by a relay, or by two upstreams): only the path that brings the newest keeps
  renewing; a path that lags behind is refused `older` until it catches up, and its entry keeps the
  licence it holds.

**A copy made public is not prevented.** A tenant administrator reads the distributor's licence,
so they can import a copy of it into another entry where its key is live (an upstream's, their own
publisher's) and make it public there. kista does not refuse that: the distributor's terms and the
[warning](#public-serving-of-licensed-entries) of a licensed entry served publicly cover it.

It protects what kista serves against accidents (a remote restored from a backup, a bundle imported
again, a stale token) and against tenant administrators who are not server administrators on a
kista someone else hosts. It does not stop a writer who may remove the newer licence first (a
server administrator, a licence manager's token), and a licence file placed by hand on a client is
outside it. Short licence lifetimes, renewed by the issuer, remain the real control.

**Its gaps.** kista forgets a licence's `iat` once nothing it serves holds it: after the licence
expires, after a writer removes it, after the relay revokes it. An older licence still in force can
then be stored again: kista keeps no tombstone. The rule refuses new stores only: an older licence
stored in another entry while it was the newest stays there after a newer one arrives elsewhere,
and a channel whose rule is switched to that entry serves it. Short lifetimes bound both.

`kista admin upstream licenses <tenant> <upstream> [-sync]` shows the last relay run (received,
stored, replaced, unchanged, removed, refused with reasons) and with `-sync` asks for one now;
the API has `GET` and `POST …/upstreams/{name}/licenses[/sync]`.

**In the console**, tenant administrators have **Licences**: the tenant's licences with plan,
holder, how each arrived, visibility, expiry, status (one expiring within 30 days, or in its grace,
is flagged) and its **Entry**; a licence's claims, limits and the fingerprint of the key that signed
it; making one public or private, and removing one (none of these on a distributor's licence for a
caller who is not a server administrator: it is read-only to them, spec 0037). **Import a licence** reads the file's extension
and key, and lists the entries whose live keys include it, each with its kind and, for a
distributor's publisher, the distributor (listed but not chosen by a caller who is not a server
administrator: "a distributor's: its licence manager writes it"). It preselects an entry only when
exactly one lists the key; with several, the administrator chooses; another entry may be named (kista's `400
kid_untrusted` is shown when its keys do not verify the licence). The import always names its entry,
and the licence stored there is replaced after a confirmation. A licence is flagged when no live
build of its entry declares its key: it is not served. The **licence keys** table shows each key's
entries with their builds. Last, each relaying upstream's last run with its refusals, and **Sync
now**.

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
| an import (`kista admin license import -source`, `POST …/licenses?source=`) | `400`, `holder_mismatch`, naming the expected and the given holder |
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

- a channel serves the licence of the **entry it serves the extension from** (its rule's source for
  the name), and only while its grace lasts and a live (active or deprecated) release of that entry
  there, that the caller sees, declares the key that signed it. Another entry's licence is never
  served through it, whatever keys its builds declare, and a licence that no live build in the
  channel declares stays stored and is not served there. **Switching the rule's source switches
  kista's answer** at the next request: an entry without a licence answers `404`. The SDK keeps a
  licence it cached before on a `404`, so the switch applies to kista's answers, not to what a
  running client already holds;
- a **private** licence (the default) goes to a token holding `install` on the extension in that
  channel;
- a **public** licence (`kista admin license public <tenant> <name> -source <entry>`) also goes to whoever is served a
  public release of the extension there: for a kista that serves its own users without tokens, on
  an internal network. Anyone who reaches the channel can then copy it;
- everyone else gets exactly the answer of a missing extension path: `401` without a valid token,
  `404` with one, byte for byte. The routes reveal nothing about extensions the caller may not see.

Over plain `http` a token is ignored, as on the extension routes, so only public licences are
served there, and the list route always answers `401`. A passthrough channel has no licences, and
while licensing is off every licence route answers as a missing path.

### Public serving of licensed entries

An organisation may mirror an extension with its licence into its own kista and serve it inside the
organisation as it likes. It must not serve it **publicly** with its licence unless the distributor
allows it; the distributor's public terms say so. kista only **warns** about it: nothing is refused,
and the warning is advisory and not complete.

Visibility belongs to a release; there is no public channel. A release is public when an
administrator makes it public, or when it is placed, promoted or published from an entry whose
visibility is public, unless `private` is asked. A licence's own visibility is separate (above).

An entry is **served publicly** when it has an active or deprecated public release in a signed
channel whose rule selects it, its build not withdrawn and its body not blocked. kista warns when an
entry that holds a licence (any visibility) is served publicly, at the act that changes it:

- a release of the entry written public, made public, taken by an upstream's successor (spec 0031),
  or selected by a switch of the channel's rule;
- a licence stored in such an entry (an import, a relay, a bundle), or made public there.

Declaring the licence's key alone does not warn, nor does an act that changes nothing (a release
made public again, a relay's licence unchanged). Each act warns **once** per entry, name and
channel, however many platforms or releases it touched: a `license.public_warning`
[event](observability.md) (with the act that caused it). The warning is written right after the act
commits, **best effort**: a failure to write it is logged, and never refuses or undoes the act; two
transactions of one act checking at the same moment may rarely write it twice.

Who is told besides the events: an administrator of what the act changed (a server administrator; a
tenant, channel or extension administrator) gets a `warnings` array in the
[API's answer](api.md#warnings) and a notice in the console, and `kista admin` prints it on stderr
(`kista admin: warning: <tenant>: …`, the exit status unchanged). Everyone else's act (a publisher's
publication or promotion, a principal holding only `publish` or `promote`, a licence manager's
import, a distribution admin) and the acts that answer nobody (a mirror's run, the placement pass, a
relay run, an uploaded bundle's import) write only the event.

The warning is not complete: a change that passes none of these acts, such as a broad grant or a
block lifted, warns nothing. Enforcing the rule (for example a distributor's publisher marked "not
public") is a follow-up.

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
another organisation defeat it; the licence names its holder wherever it goes. Serving a licensed
extension publicly is only [warned about](#public-serving-of-licensed-entries), never refused. An
[expected holder](#an-expected-holder) keeps a licence out of the wrong tenant of one kista; it does
not stop a holder copying its own licence elsewhere.

## Events

`tenant.holder` (the expected holder set or cleared), `license.import`, `license.change` (visibility), `license.remove` (with the reason: `admin`,
`revoked`, `expired`, `upstream removed`; each names the licence's entry as `source`),
`license.sync` (each relay run), `license.public_warning` (an entry holding a licence served
publicly, advisory: [above](#public-serving-of-licensed-entries)), `extension.use` (a
usage report by a principal holding `install`), `usage.forward` (each forwarding run), `usage.receive` (each aggregate taken from downstream) and
`upstream.change` with `change: usage`. The licences a bundle carried are in its
`bundle.import` event and import record. See
[Observability](observability.md).
