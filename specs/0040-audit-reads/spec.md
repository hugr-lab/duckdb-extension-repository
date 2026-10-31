# Spec 0040: An auditor reads the tenant's issuers, publishers and grants

- **Status**: accepted
- **Date**: 2026-10-31
- **Author**: vgsml, Claude

## Summary

A principal holding `audit` on a tenant reads, read-only, the tenant's token issuers, its publishers
with their keys, and its grants, each with `created_by` and `created_at`: the answers its
administrators get, through the same routes. Enterest's tenant watch (Enterest spec 0012, P8) needs
it: its tenant reader holds `audit` in every customer tenant and compares what the tenant holds with
what Enterest made, without a server administrator's token. The owner approved it on 2026-10-31.

## Problem

`audit` (spec 0010) reads a tenant's events and statistics. The tenant's structure behind them, its
issuers, publishers and keys, and grants, is read only by its administrators and server
administrators (`pathAdmin` on `…/issuers` and `…/grants`; `pathAdmin` or a distribution admin, who
reads only distributors' publishers, on `…/publishers`). A watcher that should only observe a tenant
then needs `admin` there, which also lets it change everything, or a server administrator's token.

Enterest's drift check (its spec 0012) reads, each hour, every customer tenant's issuers, publishers
with their keys' fingerprints and `distributor` mark, and grants with `created_by`, to report
objects Enterest did not make, and those a member who has left made. Reading the events alone is not
enough: an object made before the watch started, or whose events were not read (a gap, retention),
is never seen.

## Design

### Who reads

A caller holding `audit` on the tenant. `audit` is only ever granted tenant-wide, of every package
type (spec 0010, `store.Grant` validation), and `admin` implies it, so the tenant's administrators
keep these reads. A server administrator reads them as today. Nothing else changes for anyone:

- a distribution admin keeps spec 0037's narrowing: the publishers routes as today (distributors'
  publishers only), never issuers or grants. It holds no `audit` (it holds `distribute`, which no
  grant gives).
- a licence manager, a publisher, an install holder or a feed holder reads none of them.

### The routes

Read-only `GET`s, the answers unchanged (the administrators' JSON, `created_by` masked as it is for a
tenant administrator: server, licence-manager and distribution-admin actors are shown as their kind):

| Route | Today | With this spec |
| --- | --- | --- |
| `GET tenants/{t}/issuers`, `…/issuers/{name}` | tenant admin | also `audit` |
| `GET tenants/{t}/grants[?…]`, `…/grants/{id}` | tenant admin | also `audit` |
| `GET tenants/{t}/publishers`, `…/publishers/{name}` | tenant admin; a distribution admin, its distributors' publishers | also `audit` (every publisher) |
| `GET tenants/{t}/publishers/{name}/keys`, `…/keys/{id}` | as the publishers | also `audit` |

Not opened: `GET …/grants/principals` (the console's principal picker), every write, the issuers'
console client (`POST …/issuers/{name}/console`), `…/publishers/{name}/license-keys`, `…/hold` and
`…/distributor` (writes), and server-wide routes.

### How

- **API rules.** The issuers and grants `GET` rules become `pathVerbs` with `auditors`
  (`audit`; `admin` implies it), the rule the events routes use. The publishers `GET` rules keep
  admitting a distribution admin and add `audit` holders: a new access kind, `auditOrDistributor`
  (`pathVerbs` with `auditors`, or a distribution admin as `distributionAdmin` admits one), used only
  by those four routes.
- **Services.** `tenants.AuthAdmin` reads them with one check, `mayRead`: `audit` on the tenant
  (`Authz.Allow(…, VerbAudit, Resource{Tenant})`; `admin` passes), or a distribution admin on the
  publishers' reads (`tenantOrDistributor`, unchanged). The reads behind the eight routes are
  `ListIssuers`, `ListGrants`, `ListPublishers`, `TenantPublisherKeys` and `ListPublisherKeys` (an
  item route filters its list); they call it. `ListPublisherKeys` looks its publisher up with
  `publisher`, which the writes share: the read gets its own lookup under `mayRead`, and every write
  keeps `tenant` / `publisher` (`admin`, or a distribution admin on a distributor's publisher).
- **Answers.** Each carries what it carries today: an issuer's URL, JWKS URI, algorithms and claim
  names (an issuer holds no secret); a grant's principal, verbs and scope; a publisher's id, name,
  `distributor`, hold and licence-key mark, and its keys' type, fingerprint, title, public key and
  dates (a public SSH key, spec 0023). The ETags are there too; they let no one write.
- **Suspended and closed tenants** answer `404` to an auditor, as to a tenant administrator.
- **No audit event** is written for these reads, as for the events' reads.

### Docs

`access.md` (what `audit` reads), `api.md` (the four rows), `publishing.md` (who reads publishers and
keys), `tenants-and-channels.md` (issuers and grants).

## Security

- **What an auditor learns.** The tenant's structure: its issuers, publishers and keys, and grants.
  The events an auditor already reads tell the same, one change at a time (`issuer.add` with its
  name and URL, `grant.add` with its principal, verbs and scope, `publisher.key.add` with the key's
  fingerprint and type, spec 0010); this reads the current state instead. Nothing secret is in the
  answers: no token, no client secret, no private key.
- **Nothing is widened for a distribution admin.** It holds no `audit`; its publishers' narrowing
  (distributors' publishers only) is unchanged, and it still reads no issuer or grant.
- **No existence leak.** A caller without `audit` gets the same `404` as for a missing tenant; an
  auditor gets `404` for a missing issuer, grant, publisher or key, as an administrator does.
- **Writes** keep `admin` in the API rule and in the service, both.

## Testing

- `internal/api`: a principal holding only `audit` (tenant-wide) reads each of the eight routes
  (`200`, the same JSON as a tenant administrator's, `created_by` masked the same way); every write
  on the same resources answers `404`; `…/grants/principals` answers `404`; a principal holding
  `install` only, a channel administrator and a licence manager get `404` on each read; a distribution
  admin still reads only distributors' publishers and keys, and gets `404` on issuers and grants; a
  suspended tenant answers `404` to the auditor.
- `internal/tenants`: `mayRead` admits `audit`, `admin` and a server administrator, refuses others;
  the writes still refuse an auditor.

## Alternatives considered

- **One new endpoint listing everything** (`GET tenants/{t}/inventory`). One request instead of
  four, but a second answer format for the same objects to keep in step with the first; the reads
  are hourly and paged, so the extra requests cost little.
- **A narrower answer for auditors** (fingerprints without public keys, no ETags). Nothing in the
  answers is secret, and two shapes of one object are a maintenance cost for no protection.
- **Enterest reads as a server administrator.** A token that can change every tenant, held by a
  watcher that only reads; refused by the owner (Enterest spec 0012, decision 2).

## Delivery

One PR, `0040-audit-reads`: the API rules, `mayRead` and its callers, the tests and the docs pages.

## Follow-ups

- The console shows these screens to an auditor only if it gains an auditor's view; not needed by
  Enterest.
