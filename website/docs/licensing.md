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
This page covers what kista does today: licences in a tenant, the relay from upstreams, and the
routes an extension's SDK reads. Usage reports and the C++ SDK come in later phases.

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

## What licensing does not do

Licensing protects honest users from accidents, not against deliberate acts. A patched binary, an
extension loaded with `allow_unsigned_extensions`, a rolled-back clock or a licence copied to
another organisation defeat it; the licence names its holder wherever it goes.

## Events

`license.import`, `license.change` (visibility), `license.remove` (with the reason: `admin`,
`revoked`, `expired`, `upstream removed`) and `license.sync` (each relay run). See
[Observability](observability.md).
