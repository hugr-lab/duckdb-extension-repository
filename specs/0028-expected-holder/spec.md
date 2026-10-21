# Spec 0028: An expected holder per tenant

- **Status**: accepted (2026-10-21)
- **Date**: 2026-10-21
- **Author**: vgsml, Claude

## Summary

A server administrator may set the holder a tenant's licences must name: the licence's `sub`. kista
then refuses, on every path, a licence that names another holder. Enterest needs this. It sets the
expected holder on each customer's tenant when it creates the tenant, so a licence signed for one
organisation can never be stored in another organisation's tenant. Spec 0012 lists this as a
follow-up, and Enterest's spec 0002 needs it before Enterest takes paying customers.

## Problem

kista checks a licence's signature, its extension and its time, but not its holder (spec 0012, "the
honest-user line"). That is right for the SDK, which cannot know who runs it. It is wrong for a
repository that knows whose tenant it is.

In Enterest each organisation has its own tenant, and Enterest writes licences into it (Enterest
spec 0002). A slip there, such as a mapping bug, a restore, or an operator's import into the wrong
tenant, would give an organisation a licence it did not pay for. Nothing in kista would stop it, and
the licence would then travel on to the customer's own kista and its sites.

Spec 0012's table also says that `sub` is "an Enterest tenant". It is an Enterest organisation.

## Design

### The setting

- **Data.** Each tenant gets a `holder_id` column: nullable, 1 to 128 bytes, the same rule as a
  licence's `sub`. The migration is additive and covers all three dialects. Null means no expected
  holder, which is today's behaviour.
- **Who sets it.** Only server administrators. A tenant administrator may read it but not change it:
  someone who could clear it would defeat it.
  - `POST /api/v1/tenants` takes an optional `holder_id`.
  - `PUT /api/v1/tenants/{t}/holder` with `{"holder_id": "…"}` sets it, and `DELETE` clears it. Both
    are for server administrators and take `If-Match` on the tenant.
  - The CLI: `kista admin tenant create <name> … -holder-id <id>`, and
    `kista admin tenant holder <name> <id>|-clear`.
  - The tenant's read (`GET /api/v1/tenants/{t}`, `tenant show`) includes `holder_id`.
- **Setting it over stored licences.** If the tenant already holds a licence that names another
  holder, setting the holder is refused (`409`, naming those licences). The administrator removes
  them first. Nothing is removed silently.
- **Events.** A `tenant.holder` event records the old and the new holder.

### Enforcement

When a tenant has an expected holder, every path that stores a licence in it refuses a licence whose
`sub` differs:

| Path | The refusal |
| --- | --- |
| an import (`POST …/licenses`, `kista admin license import`) | `400` `holder_mismatch`, naming the expected and the given holder |
| the relay from an upstream | refused with the reason `holder_mismatch`; counted in `license.sync` |
| a bundle's import | refused with the reason `holder_mismatch`; counted in the import's record and in `bundle.import` |

The check runs after the signature and before any write, on the same transaction as the write, so a
holder changed meanwhile is seen. Reading, serving and the expiry pass are unchanged: every licence
already stored names the expected holder, by the rule above.

A site's bundle plan may name an expected holder of its own (`license_holder`, spec 0021). The two
checks are independent: the plan's on the exporter's side, the tenant's on the importer's side.

### Spec 0012's wording

In spec 0012's payload table, `sub` becomes "the holder's id at the issuer (for Enterest, an
organisation)".

## Security

- **Fail closed.** With a holder set, no path stores a licence for another holder. The check sits in
  the one function that stores licences, so a new path cannot skip it.
- **Who may change it.** Only server administrators. In Enterest, that is the provisioning principal,
  which sets it when it creates the tenant. A customer, as a channel or extension administrator,
  cannot change it.
- **What it does not do.** It does not stop a holder from copying its own licence elsewhere. That is
  still the honest-user line.

## Testing

- The store suite on all three dialects: the column, set, clear, and the refusal while another
  holder's licence is stored.
- The API: set and clear by a server administrator; a tenant administrator refused; `If-Match`; the
  tenant's read.
- Enforcement on each path:
  - an import of another holder's licence: `400`;
  - a relay answer with one licence of another holder: refused, and the others stored;
  - a bundle with one: refused and counted;
  - with no holder set: everything as today.
- The CLI commands.

## Alternatives considered

- **A grant-level check** (the holder in each administrator's grant). This is finer than needed: a
  tenant belongs to one holder.
- **Enforcing it in Enterest only.** Enterest already maps by immutable ids. The setting in kista is
  the second line, which also covers an operator's manual import.

## Follow-ups

- None for Enterest. A customer's own kista may set the same holder on the tenant it relays into.
