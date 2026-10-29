# Spec 0029: Licence managers

- **Status**: implemented (2026-10-21)
- **Changed by** [0037](../0037-distributor-in-a-tenant/spec.md) (PR 1): a distributor's publisher's entry is written by licence managers and server administrators only (a tenant administrator reads it); a licence manager still never makes a licence public.
- **Date**: 2026-10-21
- **Author**: vgsml, Claude

## Summary

Licence managers are a server-wide role, configured like `auth.server_admins`. A licence manager may
manage the licences of every tenant and do nothing else. Enterest's licence reconciler then writes
licences without being a server administrator, who may also create, suspend and delete tenants,
add upstreams, register keys and mark publishers.

## Problem

Enterest's reconciler (Enterest spec 0002, part 2b) writes each customer's licences through
kista's licence API, which today only tenant and server administrators may call. Enterest has two
choices today:
- give the reconciler a server administrator's token, so a leak of it reaches everything kista
  manages;
- or write it a tenant administrator's grant in every tenant, which is also wider than licences (it
  covers publishers, upstreams, blocks and keys), and needs a token per tenant.

## Design

- **Configuration.** `auth.server_license_admins` is a list of principals of the server issuers,
  checked as `server_admins` is (`kind:issuer|value`; a whole issuer is refused). It is file-only,
  like the server administrators. For both lists, `publisher:` is refused by name. A `role:`,
  `group:` or `client:` principal of a server issuer without `roles_claim`, `groups_claim` or
  `client_claim` is refused too, since it could never match.
- **What a licence manager may do**, on every tenant that is not being deleted (nor suspended: a
  suspended tenant is unknown to it, as to the tenant's administrators):
  - `GET /api/v1/tenants/{t}/licenses` and `GET …/licenses/{name}`;
  - `POST …/licenses` (import, and replacement with `If-Match`);
  - `DELETE …/licenses/{name}`;
  - `POST …/licenses/{name}/private` (never `…/public`: see Security);
  - `GET …/license-keys` and `GET …/license-keys/{name}`, to see which keys the live releases
    declare (the reconciler's `kid_untrusted` case).
- **What it may not do:** anything else. That includes making a licence public, the tenant's other
  routes, the upstreams' licence relay (`…/upstreams/{u}/licenses[/sync]`, which belongs to the
  tenant's administrators), and every server route.
- **Implementation.**
  - A new access kind, `licenseAdmin`, is satisfied by `pathAdmin` or by a principal in
    `server_license_admins`.
  - The licence routes above use it.
  - `whoami` reports the role (`license_manager: true`).
  - The services see a licence manager as an actor of its own kind, `license-manager`. The API's
    authorizer allows it one verb, `license`, on an active tenant, and nothing else. The licence
    service asks for `license` on the routes above. It keeps `admin` for making a licence public
    and for the relay. A tenant-wide `admin` implies `license`. No grant can give it, since it is
    not a grant verb.
  - A principal in both lists is a server administrator.
- **Events.** A licence manager's acts carry its actor as every other actor's do (`license.import`,
  `license.remove`, `license.change`): `license-manager:<issuer>|<who>`. A tenant's readers see
  `license-manager` without the name, as they see `server` for a server administrator.

## Security

- **Narrower than today.** A leaked licence manager's token can import, replace and remove
  licences and make them private. It cannot publish, block, add upstreams or keys, or touch a
  tenant's records. A licence still has to verify against a key that a live release declares
  (spec 0021), and must name the tenant's expected holder (spec 0028). So the token cannot bring a
  licence that the extension's key did not sign.
- **Never public.** A public licence's token is served to anyone served a public release of the
  name (`<prefix>/-/licenses`, anonymous included). With `…/public`, a leaked token could publish a
  customer's paid licence to everyone. Enterest's reconciler only ever restores `private`, so making
  a licence public stays the tenant administrators' act.
- **No implied rights.** Being a licence manager implies nothing else, and server administrators
  keep everything they have.
- **Configuration only.** The role cannot be granted over the API. It is file-only so that an API
  caller cannot widen it.

## Testing

- The configuration: validation, and the refusal of a whole issuer, a publisher, and a principal
  whose issuer lacks its claim.
- `…/public` answers `404` to a licence manager; `…/private` works.
- A licence manager on two tenants: every licence route works; the upstreams' relay routes, the
  tenant's other routes and the server routes are refused. The answer is the API's `404`, which
  stands for forbidden (spec 0007).
- A tenant administrator and a server administrator are unchanged.
- `whoami`.

## Alternatives considered

- **A tenant verb `license`, given by grants.** It is finer, but Enterest would write a grant into
  every tenant and mint a token per tenant. It may come later for self-hosted tools.
- **Server administrator, as today.** It is too wide for a job that runs every 15 minutes.

## Follow-ups

- A tenant-level `license` verb, if someone needs it.
- ~~The cloud recipes (bicep, terraform, the marketplace UI) do not expose `server_license_admins`
  yet.~~ Implemented: `server_license_admins` in the AWS and GCP recipes, `serverLicenseAdmins` in
  the Azure recipe, and an optional licence managers' app role in the Marketplace UI; empty, the
  configuration leaves the key out.
