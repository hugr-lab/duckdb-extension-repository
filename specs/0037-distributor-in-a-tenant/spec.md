# Spec 0037: The distributor in a tenant

- **Status**: draft
- **Date**: 2026-10-29
- **Author**: vgsml, Claude

## Summary

Six changes to how a distributor (Enterest) acts in a customer's tenant. All are needed before
production. They come from the owner's decisions of 2026-10-26 and from Enterest's reviews.

1. **A distributor's licences are the distributor's.** In a distributor's publisher's entry, only
   licence managers and server administrators write licences. Tenant administrators read them.
   Separately, kista refuses a licence older (by `iat`) than the one stored in its entry from the
   same issuer for the same holder, on every path and in every entry.
2. **A narrower distribution admin.** The role acts on distributors' publishers and their catalogue
   entries only. It no longer reserves names in tenants, creates ordinary publishers, writes grants,
   or manages a tenant publisher that carries only the licence-key mark. Tenant reservations
   (spec 0030) are removed.
3. **A distribution admin reads a tenant's state and expected holder**, so Enterest can check that a
   tenant is its customer's before it writes there.
4. **Publisher ids in answers.** A distributor recognises its own publisher after it is removed and
   created again.
5. **A distributor's publisher reads its own catalogue entries** with a signed request.
6. **Each kind of `429` has its own problem type**, so clients do not have to guess from `detail`.

## Problem

- **A tenant administrator can undo the distributor's licence.** Spec 0035 lets tenant
  administrators write any entry, a distributor's publisher's included. They can:
  - remove the licence the reconciler stored;
  - replace it, with `If-Match`, by an older licence of the same distributor (a longer or wider plan
    issued before a downgrade);
  - make it public. Its token is then served without authentication to anyone served a public
    release of the name.

  The reconciler restores the licence every 15 minutes. Until it does, the tenant is served a
  licence the distributor did not intend. Spec 0035 phase 2 only warns about public serving.
- **Old licences come back.** Spec 0012's replacement rules accept a relayed licence that lasts
  longer even when it was issued earlier. A remote restored from a backup, a bundle imported again,
  or a stale token a reconciler sends can therefore replace a newer licence.
- **The distribution admin is wider than Enterest needs.** Spec 0030 gave the role names reserved in
  tenants, the creation of ordinary publishers, and `publish`, `promote` and `admin` grants on
  reserved names. Enterest used these to publish into customers' channels. Since its decisions 26
  and 34 and spec 0033 B, a distributor publishes into the catalogue only and holds no grants, so
  none of these is used. One of them is a hole: `marked()` (`internal/tenants/publishers.go`) lets a
  distribution admin manage the keys of **any** marked publisher. That includes a tenant's own CI
  publisher that a server administrator marked for licence keys (spec 0021), which holds the
  tenant's `publish` grants. A distribution admin can add its own SSH key to that publisher and
  publish into the tenant's channels as the tenant's CI.
- **Enterest cannot check a tenant before writing.** A distribution admin cannot read
  `GET /api/v1/tenants/{t}`. Enterest therefore cannot confirm that the tenant is active and that
  its expected holder (spec 0028) is the customer's organisation before it creates a publisher or
  imports a licence there.
- **A distributor cannot recognise its publisher.** Publisher answers carry no id. If a publisher
  is removed and created again under the same name, it is a new entry with no licence (spec 0035).
  Enterest sees the same name and does not notice.
- **Enterest's publishing worker cannot read what it published.** The worker signs as the
  distributor's publisher. The catalogue reads admit only a distribution admin, so the worker needs
  a second credential to check its own versions, holds and withdrawals.
- **Clients guess what a `429` means.** The rate limiter, the upload slots, a full ingest spool and
  a full catalogue all answer `too-many-requests`. Some carry `detail`, some do not. A client that
  retries on any `429` retries a full catalogue forever.

## Design

### 1. A distributor's licences

**Who writes.** A distributor's publisher's entry is one named by `DistributorSources`, whether its
publisher is live or removed. The licence writes on such an entry are allowed as follows:

| Caller | import, replace | `DELETE` | `…/private` | `…/public` | reads |
| --- | --- | --- | --- | --- | --- |
| server administrator (server token, CLI) | yes | yes | yes | yes | yes |
| licence manager (spec 0029) | yes | yes | yes | no (spec 0029) | yes |
| tenant administrator | `404` | `404` | `404` | `404` | yes |

Other entries do not change: tenant and server administrators write them, and a licence manager
gets `404`.

- **Implementation.** In `licenses.entry()`, a principal actor naming a distributor's publisher's
  entry, live or by the id form of a removed one, gets `authz.ErrDenied`. The API answers that as a
  missing path and records `authz.refused`, as it does for a licence manager's other entries.
  `recheck` repeats the test inside the transaction.
- **Answers.** A licence's answer gains `distributor` when its entry is a distributor's publisher's.
  The keys listing already names it.
- **Console.** For a caller who is not a server administrator, the Licences screen shows a
  distributor's entry read-only:
  - its licence has no Remove, Make public or Make private actions;
  - the import dialog lists the entry but does not let it be chosen, with the note "a distributor's:
    its licence manager writes it".

  A server administrator's console is unchanged.
- **CLI.** No change. `kista admin` acts as a server administrator.

**Older licences are refused** (rule `older`). This applies to every entry.

- **The rule.** kista refuses to store a licence when the entry already holds a licence for the
  name, both have the same `iss` and the same holder id, and the incoming `iat` is earlier than the
  stored one. An equal `iat` is not older. A licence from another issuer is not compared.
- **Where it applies.**
  - Import, including a replacement with `If-Match`. The answer is `400` `invalid`, "the licence is
    refused (older): …", as for every other licence refusal.
  - The relay. The licence is refused with rule `older` in the run's result, and the stored licence
    stays. This check comes before `replaces`, whose rules are otherwise unchanged.
  - Bundles. The licence is refused with rule `older`, counted per target.
- **Where it is checked.** Each service checks first, so its refusal can name the rule. `PutLicense`
  checks again on every path, as it does for spec 0028's holder, and returns a typed error.
- **Going back on purpose.** Remove the stored licence, then import the older one. Both acts are
  audited, and only the entry's writers may do them.
- **Why every entry.** The rule belongs to licences: an issuer's later licence for a holder
  supersedes its earlier ones, whoever writes. The paths it protects most have no removal power: an
  upstream's relay and a bundle import. Those feed upstream entries, not distributors' entries.
  Applying the rule to distributors' entries only would leave the paths that run without anyone
  acting unprotected. The check costs one comparison against a row the write reads already.
- **The gap.** Nothing remembers a licence after it is removed (expired past its grace, or removed
  by a writer). After that, an older licence can be imported again if it is still in force. See
  Alternatives.

No migration: the stored row already has `issuer`, `holder_id` and `issued_at`.

### 2. A narrower distribution admin

**What the role keeps**, on every active tenant:

- **Publishers.**
  - Create a distributor's publisher (`POST …/publishers` with `distributor`).
  - List and read the distributors' publishers. The tenant's own publishers are not listed, and
    reading one answers `404`.
  - Rename a publisher's distributor (`PUT …/publishers/{p}/distributor`).
  - Set and clear the licence-key mark (`…/license-keys`) and set the hold (`…/hold`).
  - Manage keys: list, read, add, remove, with `purge_held`.

  All of these apply to distributors' publishers only.
- **Catalogue.** Read distributors' entries, purge their builds, and withdraw their versions. This
  is unchanged.
- **The tenant's state** (item 3).
- **The server's reserved names**, read only (`GET /api/v1/reserved-names`). These are DuckDB's
  names, public anyway, and they are the names a distributor's publisher may not publish.
- **Spec 0036's aggregated statistics** of distributors' entries, as defined there. 0036 is being
  drafted on its own branch, and this spec does not define them.

**What the role loses:**

- **Grants.** `GET|POST …/grants` and `GET|DELETE …/grants/{id}` go back to `pathAdmin`.
  `distributorGrant` and the `created_by` tests go.
- **Ordinary publishers.** `POST …/publishers` without `distributor` answers `404`.
- **Licence-key-only publishers.** For a distribution admin, `marked()` (and `ListPublisherKeys`,
  `TenantPublisherKeys`) admits only distributors' publishers (`p.Distributor != ""`). A publisher
  with only the licence-key mark stays a server administrator's (spec 0021). The licence-key mark
  route admits a distribution admin for distributors' publishers only.
- **Tenant reservations**, removed below.

**Marking an existing publisher goes, for every caller.** A distribution admin can no longer create
an unmarked publisher, so spec 0033 B's "fresh, created by a distribution admin" path has nothing
left to apply to. `PUT …/distributor` on an unmarked publisher answers `404`, and a distributor's
publisher is created marked. The freshness check (`PublisherUsed`) goes.

**Tenant reservations are removed.**

- **Why.** They let a distributor that published into a tenant's channels keep its names out of the
  tenant's wide grants (spec 0030's problem). Since spec 0033 B, a distributor publishes into the
  catalogue, reaches no channel by itself and holds no grant. Enterest no longer uses reservations.
  Keeping them for server administrators only would leave a second, unrequested way to narrow wide
  grants, which every name check would still carry. kista is unreleased, so there is no
  compatibility path.
- **What goes.**
  - The table `tenant_reserved_names`.
  - The routes `GET|PUT|DELETE /api/v1/tenants/{t}/reserved-names[/{name}]`.
  - `kista admin tenant-reserved add|remove|list`.
  - The events `reserved.tenant.add` and `reserved.tenant.remove`.
  - The console's "Names reserved for a distributor" card.
  - In code:
    - the `tenant` step of `ReservedFor`, leaving core, community and upstream;
    - `TenantReserved` on the channel snapshot, and its uses in the index's `reserved`, the
      statistics scope and `"*"`;
    - `TenantReserves`, `TenantReservedHolder` and `TenantReservedSet`;
    - the re-check under the release insert's locks;
    - `allow_reserved` for a tenant's names on upstream entries;
    - the follows' holder check;
    - the reaper's step.
- **Distributors' publishers.** A distributor's publisher may publish into the catalogue any name
  kista does not reserve server-wide. A follow of a distributor's publisher takes names as any
  follow does.
- **Migration** `00NN_drop_tenant_reserved`, in the three dialects. It drops the table. `NN` is the
  next free number when this lands: 0044 unless spec 0036 takes it first. `min_reader` is `NN`,
  because an older binary reads the table on every channel snapshot.

### 3. A distribution admin reads a tenant

- **Access.** `GET /api/v1/tenants/{t}` admits a distribution admin on an **active or suspended**
  tenant. A deleting, deleted or unknown tenant answers `404`.
  - In `decide`, the rule "a suspended tenant is 404 for everyone but server administrators" admits
    a distribution admin on this one route.
  - The tenants service reads the tenant itself. It does not go through `activeTenant`.
- **Answer.** `{"name", "state", "holder_id"}`, with `holder_id` omitted when none is set. It has no
  ETag, display name, dates or storage domain.
- **Why suspended too.** Enterest needs to tell "suspended, wait" from "gone". The role is
  server-wide already.

### 4. Publisher ids in answers

- **Publisher answers.** Every publisher answer gains `id`: list, read, create, hold, distributor
  mark and licence-key mark (`publisherJSON`).
- **whoami.** A signed `whoami` names, for each entry of `publishers`, its `id` and its
  `distributor` (omitted for the tenant's own).
- **Catalogue-only publication.** The answer gains `publisher_id` and `distributor` for the
  publishing publisher (`distributor` omitted for the tenant's own).
- **Events.** `publisher.add` and `publisher.remove` gain `id`, so a tenant's auditors can see a
  re-creation.
- **CLI.** `publisher list` gains an `ID` column, and `publisher show` prints `id:`.
- **Console.** The Publishers detail shows the id.

The ids are opaque, grant nothing, and are already listed for removed distributors' publishers.

### 5. A distributor's publisher reads its catalogue entries

- **Access.** `GET …/catalogue` and `GET …/catalogue/{source}/extensions/{ext}` admit a signed
  request from a distributor's publisher. The route rules gain `publishers` and `distributors`, as
  the catalogue-only publication's have.
  - As with `whoami`, the statement names the empty body.
  - A request signed by a tenant's own publisher stays `404`.
- **Answer.** The publisher's own source only, in the distribution admin's view: no rules, no
  `placed`, and `publisher_id` on each entry.
  - On the list, `?source=` may be omitted, which means its own source. If given, it must name
    that source (`publisher:<name>` or `publisher-id:<id>`). Any other value answers `404`.
  - On the entry route, `{source}` must name its own source in the same way. Any other value
    answers `404`.
- **Removal.** A removed publisher's keys are gone, so it reads nothing.

### 6. Problem types for `429`

| Cause | Type | `Retry-After` |
| --- | --- | --- |
| the per-client rate limiter | `rate-limited` | the limiter's |
| the upload slots per actor, tenant or server (`slot`) | `upload-slots` | `5` |
| every ingest slot taken (`blob.ErrBusy`: a publication, a bundle chunk) | `ingest-busy` | `5`, or `1` for a chunk |
| a source's catalogue full (`catalogue.max_entries`, `max_bytes`) | `catalogue-full` | none: waiting does not help |
| an issuer being added to the tenant | `issuer-busy` | `5` |

- `too-many-requests` leaves the set.
- The bundle chunk's `429`, typed `unavailable` today, becomes `ingest-busy`.
- The status stays `429`, and the `detail` texts stay as they are.
- The console's upload retries keep treating `429` as transient. A publication from the console
  never meets `catalogue-full`.

## Security

- **Licences (1).**
  - **Closed.** A tenant administrator, or a stolen tenant administrator's token, can no longer
    remove, replace or publish a distributor's licence. Making it public would serve the
    distributor's paid licence to anonymous clients.
  - **What tenant administrators lose.** They can no longer write those entries. This is intended:
    the licence is the distributor's contract with the organisation.
  - **What they keep.** The tenant still decides what its channels serve, by a rule switch (spec
    0035), and may still remove the distributor's publisher.
  - **The `older` rule** stops replays where nobody can remove the stored licence: a relay from a
    remote restored from a backup, a bundle imported again, a stale token imported by mistake. It
    does not stop a writer who may remove. A stolen licence manager's token can remove and then
    import, as before, and both acts are audited.
- **Distribution admins (2).**
  - **The `marked()` hole is closed.** A distribution admin no longer reaches a tenant publisher
    that holds the tenant's grants.
  - **What a stolen distribution admin's token reaches.** Only distributors' publishers and their
    catalogue entries. It cannot write grants or reservations. Nothing it publishes reaches a
    channel without the tenant's own rule.
  - **What it keeps.** Withdrawal, which is the distributor's right over its own versions.
  - **Tenant administrators** gain nothing and lose nothing here. Reservations only narrowed their
    wide grants. Without them, the tenant's own grants reach every name that kista does not
    reserve server-wide and that no upstream of the tenant lists (spec 0009).
- **Tenant read (3).** A distribution admin learns a tenant's state and expected holder, and that a
  suspended tenant exists. The role is server-wide and acts in every active tenant already. No
  other field is answered.
- **Ids (4).** They are opaque and grant nothing.
- **Signed catalogue reads (5).**
  - The publisher reads its own source only, and learns nothing about channels.
  - Signed requests keep spec 0023's statement and clock checks.
  - A stolen key could already publish under this publisher. Reading its entries adds nothing.
- **`429` types (6).** These give no new information. Each cause was already in `detail`, except the
  rate limiter's, which a caller can infer. `catalogue-full` is answered only to a caller allowed to
  publish there, as today.

## Testing

- **Store suite, three dialects.**
  - `PutLicense` and the `older` rule:
    - an older `iat` is refused;
    - an equal `iat` is accepted;
    - another issuer is accepted;
    - an older licence imported after the stored one is removed is accepted.
  - Migration `NN`:
    - the table is gone;
    - a level `NN-1` binary is refused;
    - tenant deletion and the harness no longer list the table.
- **Licence service.**
  - The `older` rule:
    - import, and replacement with `If-Match`: `400 older`;
    - the relay: refused `older`, the stored licence kept, ahead of `replaces`;
    - bundles: refused `older` per target.
  - The writers' matrix on a distributor's entry, live and removed (id form):
    - a tenant administrator is refused (`404`, `authz.refused`) on import, `DELETE`, `…/public`
      and `…/private`, and reads;
    - a licence manager writes but cannot make the licence public;
    - a server administrator writes everything.
- **Tenants.**
  - `marked()` refuses a distribution admin on a licence-key-only publisher. This is a regression
    test for the hole.
  - Distribution admins:
    - no grants;
    - no ordinary publisher;
    - reads list only distributors' publishers;
    - `PUT …/distributor` on an unmarked publisher answers `404`.
- **API.**
  - `TestDistributionAdmin` rewritten as the allowed and refused matrix of item 2.
  - `GET tenants/{t}` for a distribution admin on an active tenant, a suspended tenant, and a
    deleting tenant (`404`).
  - `id` in publisher answers and in a signed `whoami`; `publisher_id` and `distributor` in the
    catalogue-only answer.
  - Signed catalogue reads:
    - its own source;
    - another distributor's publisher's source: `404`;
    - a tenant publisher: `404`;
    - `?source=` checked.
  - Each `429` site answers its type, with or without `Retry-After`.
  - The removed routes answer `404`.
- **CLI.**
  - `tenant-reserved` is gone.
  - `publisher list` and `show` print the id.
  - `license import` prints the `older` refusal.
- **Console.**
  - Unit tests:
    - Licences shows a distributor's entry read-only to a tenant administrator and writable to a
      server administrator;
    - the import dialog's disabled entry;
    - Publishers without the reservations card, with the id.
  - `make console-e2e`:
    - `licenses.spec.ts`: the tenant administrator cannot write `vault-dist`'s entry;
    - `run.sh` and `access.spec.ts` lose `tenant-reserved`.
- **End to end at the pin** (`make e2e`).
  - `TestLicenseFromDistributor`:
    - the tenant administrator's import into the distributor's entry is refused;
    - the licence manager's import works;
    - an older licence of the same issuer is refused `older`;
    - the distributor's publisher reads its entries with a signed request.
  - `TestDistributorWithdrawal` works without grants or reservations.
  - `reserved_test.go` loses its tenant-reserved part.

## Delivery

Two pull requests. Each runs `make test` on the three databases, `make lint`, `make console-test`,
`make console-e2e` and `make e2e` before it opens.

1. **`0037-distributor-in-tenant-1`: answers and licences**, items 1 and 3 to 6. It is mostly
   additive and has no migration.
2. **`0037-distributor-in-tenant-2`: the narrower role**, item 2. It removes tenant reservations and
   adds the migration. It mostly removes code.

**Enterest.** Its kista client reads the `429` types and the ids once PR 1 is deployed. Until then
it treats any `429` as it does today. Since its decisions 26 and 34 it uses neither tenant
reservations nor grants. Before PR 2 is deployed, its client is checked for any call PR 2 removes.
Its reconciler is a licence manager, so it is unaffected.

## Alternatives considered

- **Keep tenant reservations for server administrators.** Nobody needs them, and every name check
  would carry them.
- **Let tenant administrators write distributors' entries, with a warning** (as spec 0035 phase 2
  does for public serving). The reconciler would still be undone between its runs.
- **The `older` rule for distributors' entries only.** The relay and bundle paths, which run
  without anyone acting, would stay open.
- **A high-water mark per (entry, issuer, holder)** that survives removal. It needs a new table to
  close a narrow gap: after removal, only the entry's writers may import.
- **`403` for a tenant administrator's write.** kista answers a refused caller as a missing path
  (spec 0007), as spec 0035 does for licence managers.
- **`409` for a full catalogue.** It is a quota, and `429` without `Retry-After` and with its own
  type says so. Clients key on the type.
- **Signed reads for every publisher** (the tenant's own CI). Nobody asked for them.

## Changes to earlier specs

- **0007**: the problem types (item 6); `GET tenants/{t}` for a distribution admin; the removed
  routes.
- **0008**: the upload-slot and ingest refusals' types.
- **0019**: the reaper no longer removes tenant reservations.
- **0021, 0023**: a distribution admin manages keys and the licence-key mark of distributors'
  publishers only.
- **0029**: licence managers and server administrators are the only writers of distributors'
  entries; a licence manager still never makes a licence public.
- **0030**: tenant reservations are removed, which affects:
  - Summary;
  - Problem's third bullet;
  - "Names reserved in a tenant";
  - the lookup's `tenant` step;
  - Data model's `tenant_reserved_names`;
  - As built (part 2);
  - Security's tenant isolation.

  The distribution admins' calls are this spec's list. The implementing PR adds a "superseded in
  part by spec 0037" line at the head of those sections.
- **0033**:
  - B's distributor's names: "not reserved server-wide", without "for another holder";
  - follows of a distributor's publisher: no holder check;
  - "Distribution admins (spec 0030) gain …": the list here;
  - a distributor's publisher reads its own entries;
  - the full catalogue's `429` type;
  - marking an existing publisher goes.
- **0035**: "Who writes which entry": tenant administrators write every entry except distributors'
  publishers'; the `older` rule applies to import, relay and bundles.

## Website

- `access.md`: the distribution admins' list; who writes a distributor's entry.
- `licensing.md`: a distributor's entry; the `older` rule (import, relay, bundles).
- `api.md`:
  - the problem types table;
  - `GET tenants/{t}` for a distribution admin;
  - publisher `id`, `whoami`, and the catalogue-only answer;
  - signed catalogue reads;
  - the reserved-names and grant routes for distribution admins.
- `publishing.md`: a distributor's publisher reading its entries; the ids; the `429` types for CI
  scripts.
- `catalogue.md`: distributors' reads.
- `configuration.md`: `auth.server_distribution_admins`.
- `administration/commands.md`: `tenant-reserved` removed; the publisher id; `older`.
- Pages that mention tenant reservations: `administration/tenants-and-channels.md`, `upstreams.md`
  (`allow_reserved`), `getting-started.md`, `security.md`, and the role's line in
  `deployment/aws.md`, `deployment/gcp.md` and `deployment/azure.md`.
- `administration/upgrading.md`: the migration and its `min_reader`; reservations dropped.
- `observability.md`: `reserved.tenant.*` removed; `id` on publisher events.
- `console.md`: read-only distributor entries; the Publishers card removed.

## Follow-ups

- A licence manager's read of a tenant's state and holder, if the reconciler needs it.
- A distribution admin removing a distributor's publisher, if Enterest needs it. Today a tenant or
  server administrator does it.
