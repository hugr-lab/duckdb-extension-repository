---
title: Licensing
---

# Licensing

A paid extension checks, on the user's machine, that its organisation holds a **licence** for it.
kista keeps the organisation's licences and serves them to its users from the channel they install
from. kista issues nothing: a licence is signed by the issuer that sells the extension (Enterest),
and kista stores only licences that verify against the issuer keys its operator trusts.

The design reasoning is in
[spec 0012](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0012-licensing/spec.md).
This page covers what kista does today: licences in a tenant, the relay from upstreams, the routes
an extension's SDK reads, the usage reports it sends, and the daily usage forwarded upstream. The C++
SDK comes in a later phase.

## The licence

A licence is a small signed token (a JWS, `typ` `kista-license+jwt`, RS256 with an RSA-2048 key)
naming:

- the **extension** it is for, and optionally a **version range**;
- the **holder** (the organisation at the issuer) and its display name;
- the **plan** (`pro`, `enterprise`, …) and **limits** the extension's developer defines
  (`{"max_attached_sources": 5}`): kista carries them, the extension enforces them;
- when it was issued, when it **expires**, and a **grace** period after that (7 days unless the
  licence says otherwise, at most 90).

`kista admin license verify <file> [-key <pem>]` prints a licence's claims and status offline.

## Turning licensing on

Licensing is off until the server trusts at least one issuer key:

```yaml
licenses:
  keys:
    - |
      -----BEGIN PUBLIC KEY-----
      …the issuer's licence key, as it publishes it…
      -----END PUBLIC KEY-----
  refresh: 6h
```

The keys are configuration, never data: a tenant administrator cannot add one. Nothing defaults to
any issuer's keys; take them from your issuer. `kista admin license keys` and
`GET /api/v1/licenses/keys` (server administrators) list the trusted fingerprints, and the start-up
log names them. Without keys, imports and relays are refused, and no licence is served.

Trusting a key in kista does not make an extension trust it: the extension's SDK has its issuer's
keys compiled in. A licence signed by any other key is stored by kista and refused by the
extension.

## Licences in a tenant

A tenant holds one licence per extension name. It comes from one of two places.

**An administrator's import**, for an air-gapped site or a licence received by other means:

```bash
kista admin license import acme tresor.lic             # a new licence
kista admin license import acme tresor.lic -replace    # replaces the one stored for that extension
```

Over the API the token is the body of `POST /api/v1/tenants/{t}/licenses`; replacing a stored
licence needs `If-Match` with its ETag. The licence must verify and be in force (valid, or in its
grace) when it is imported.

**The relay from an upstream.** A `repository` upstream with a credential
that is not paused ([private upstreams](upstreams.md#private-upstreams)) is asked for the licences its credential holds, every
`licenses.refresh` (6 hours by default), at `<upstream prefix>/-/licenses`. For each licence:

- it must verify and be in force;
- the upstream must supply that extension: it brought a build of it into its channel since it was
  added, and a live release of that build is in this tenant (a promotion releases the same build).
  An upstream does not hand out licences for names it does not supply;
- it replaces the stored licence when it is newer from the same upstream, or lasts longer; a
  shorter licence from elsewhere leaves the stored one in place.

A licence the upstream stops listing is removed (revoked). A `404` from the upstream means it
serves no licences and changes nothing, and so does a failure (the credential refused with `401`
or `403`, a network error, an answer over 1,000 licences or 8 MiB). Removing the upstream removes its licences; clearing its
credential stops the relay, and its licences stay until their grace ends. A licence past its grace
is removed, whatever its source.

`kista admin upstream licenses <tenant> <upstream> [-sync]` shows the last relay run (received,
stored, replaced, unchanged, removed, refused with reasons) and with `-sync` asks for one now;
the API has `GET` and `POST …/upstreams/{name}/licenses[/sync]`.

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

- a **private** licence (the default) goes to a token holding `install` on the extension in that
  channel, where a live (active or deprecated) release of it exists;
- a **public** licence (`kista admin license public <tenant> <name>`) also goes to whoever is served a
  public release of the extension there: for a kista that serves its own users without tokens, on
  an internal network. Anyone who reaches the channel can then copy it;
- everyone else gets exactly the answer of a missing extension path: `401` without a valid token,
  `404` with one, byte for byte. The routes reveal nothing about extensions the caller may not see.

Over plain `http` a token is ignored, as on the extension routes, so only public licences are
served there, and the list route always answers `401`. A passthrough channel has no licences, and
while licensing is off every licence route answers as a missing path.

## Usage reports

An extension's SDK reports how it is used to the repository it was installed from, with the same
http secret:

```text
POST <prefix>/-/usage/<name>      Content-Type: application/json, at most 64 KiB → 204
```

```json
{"version": "1.4.0", "platform": "linux_amd64", "duckdb_version": "v2.0.0",
 "license": "lic-1", "status": "valid",
 "events": [{"name": "load", "count": 1},
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
  make no event.
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
       users: [{dimensions…, users}]}
```

Rows are always per event and counter (`counter` absent for the event's own count). `status` sets use
without a licence apart, and aggregates received from downstream kistas apart as `received`; `users` are summed over the stored rows (per channel, extension, version and
day), so a principal on two days, versions or channels counts twice when those are not grouped by;
users are never by `status`.

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

- **What is sent**: the versions the upstream brought into its channel since it was added, each with
  its counts summed over the channels that hold that build (a promotion releases the same build
  elsewhere; the same version from another source is not counted), all callers and licence
  statuses, local and received from downstream.
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
- received counts appear in `/stats/usage` with the status `received`, their users with the
  others, and are forwarded on with this kista's own usage.

`statistics.usage_accept: false` keeps no received aggregate either.

## What licensing does not do

Licensing protects honest users from accidents, not against deliberate acts. A patched binary, an
extension loaded with `allow_unsigned_extensions`, a rolled-back clock or a licence copied to
another organisation defeat it; the licence names its holder wherever it goes.

## Events

`license.import`, `license.change` (visibility), `license.remove` (with the reason: `admin`,
`revoked`, `expired`, `upstream removed`), `license.sync` (each relay run), `extension.use` (a
usage report by a principal holding `install`), `usage.forward` (each forwarding run), `usage.receive` (each aggregate taken from downstream) and
`upstream.change` with `change: usage`. See
[Observability](observability.md).
