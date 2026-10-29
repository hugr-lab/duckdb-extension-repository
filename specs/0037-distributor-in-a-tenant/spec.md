# Spec 0037: The distributor in a tenant

- **Status**: implemented
- **Changed by** [0036](../0036-entry-meta/spec.md) (phase 1): the metadata routes use the `catalogue-full` type PR 1 added, and an access rule of their own admitting any publisher of the tenant by a signed request, beside PR 1's signed branch, which admits only distributors' publishers.
- **Date**: 2026-10-29
- **Author**: vgsml, Claude

## Summary

Changes to how a distributor (Enterest) acts in a customer's tenant. All of them are needed before
production. They come from the owner's decisions of 2026-10-26 and 2026-10-29 and from Enterest's
reviews.

1. **A distributor's licences belong to the distributor.**
   - Only licence managers and server administrators write licences in a distributor's
     publisher's entry. Tenant administrators can only read them.
   - Separately, kista refuses a licence older (by `iat`) than a licence it already holds for the
     same name, issuer and holder in the tenant: one in a distributor's entry whatever its key, one
     in any other entry when verified by the same key.
2. **A narrower distribution admin.**
   - The role reaches only distributors' publishers and their catalogue entries. It no longer
     reserves names in tenants, creates ordinary publishers, writes grants, or touches the tenant's
     own publishers.
   - Tenant reservations (spec 0030) are removed.
   - Tenant administrators set a minimum hold for distributors' publishers, which a distribution
     admin cannot lower.
3. **A distribution admin can read a tenant's state and expected holder.** Enterest can then check
   that a tenant belongs to its customer before it writes there.
4. **Answers carry the publisher's id**, so a distributor can recognise its own publisher after the
   publisher is removed and created again.
5. **A distributor's publisher can read its own catalogue entries** with a signed request.
6. **Each kind of `429` has its own problem type.**

## Problem

- **A tenant administrator can undo the distributor's licence.** Spec 0035 lets tenant
  administrators write any entry, including a distributor's publisher's entry. They can:
  - remove the licence the reconciler stored;
  - replace it, with `If-Match`, by an older licence from the same distributor (for example, a
    longer or wider plan issued before a downgrade);
  - make it public. Its token is then served, without a token, to anyone who is served a public
    release of the name.

  The reconciler puts the licence back every 15 minutes. Until then, the tenant is served a licence
  the distributor did not intend. Spec 0035 phase 2 only warns about public serving.
- **Old licences come back.**
  - Spec 0012's relay rules let a licence that lasts longer replace the stored one, even when it was
    issued earlier.
  - So a remote restored from a backup, a bundle imported again, or a stale token can replace a
    newer licence.
  - An administrator can also import an old licence into another entry whose builds declare the
    same key, then switch a channel's rule to that entry.
- **The distribution admin has more power than Enterest needs, and has holes.** Spec 0030 gave it
  names reserved in tenants, the creation of ordinary publishers, and grants on reserved names.
  Enterest stopped using all of these as a distribution admin after its decisions 26 and 34. Since
  spec 0033 B, a distributor publishes only into the catalogue and holds no grants. Today a
  distribution admin can also act on the tenant's own publishers:
  - **Keys.** `marked()` (`internal/tenants/publishers.go`) admits a distribution admin to the keys
    of any publisher with the licence-key mark. That includes a tenant's CI publisher that a server
    administrator marked (spec 0021), which holds the tenant's `publish` grants. A distribution admin
    can add its own SSH key to it and publish into the tenant's channels as the tenant's CI.
    `ListPublisherKeys` and `TenantPublisherKeys` show it those keys.
  - **The mark.** `SetPublisherLicenseKeys` has no check for a distribution admin when it unmarks
    (`on=false`). When it marks (`on=true`), it accepts any tenant publisher without grants or
    keys. Once marked, that publisher's keys and grants are no longer the tenant administrators' to
    change.
  - **Grants.** Through `marked()`, `AddGrant` and `RemoveGrant` let a distribution admin add or
    remove grants of a marked tenant publisher.
- **A stolen distribution admin's token can publish at once.** With that token, someone can add
  their own key to Enterest's publisher, set its hold to 0, and publish. A channel's `auto` rule or a
  follow of that publisher, which the tenant wrote, then places the build immediately.
- **Enterest cannot check a tenant before it writes.** A distribution admin cannot read
  `GET /api/v1/tenants/{t}`. So Enterest cannot confirm that the tenant is active and that its
  expected holder (spec 0028) is the customer's organisation.
- **A distributor cannot recognise its own publisher.** Publisher answers carry no id. Enterest
  compares `created_at` to notice when a publisher was removed and created again under the same
  name. An id would say it directly.
- **Enterest's publishing worker cannot read what it published.** The catalogue reads admit only a
  distribution admin, so the worker needs a second credential to check its versions, holds and
  withdrawals.
- **Clients have to guess what a `429` means.**
  - Six refusals answer `too-many-requests`. Some carry a `detail` and some do not. One of them, a
    chunk refusal, is typed `unavailable`.
  - A client that retries every `429` keeps retrying a full catalogue. The developer kit's
    `kista_publish.py` does this, up to its retry limit.

## Design

### 1. A distributor's licences

**Who writes.** A distributor's publisher's entry is the entry of a publisher marked as a
distributor's, whether that publisher is live or removed. Writes on such an entry are allowed as
follows:

| Caller | import, replace | `DELETE` | `…/private` | `…/public` | reads |
| --- | --- | --- | --- | --- | --- |
| server administrator (server token, CLI) | yes | yes | yes | yes | yes |
| licence manager (spec 0029) | yes | yes | yes | no (spec 0029) | yes |
| tenant administrator | refused | refused | refused | refused | yes |

Other entries do not change. Tenant and server administrators write them. A licence manager is
refused.

- **The refusal.**
  - A tenant principal (a tenant administrator) gets `404` with the problem type
    `distributors-entry` and the detail "a distributor's entry: its licence manager or a server
    administrator writes it". This is the one `404` that carries a type of its own and a detail.
  - It reveals nothing new: the caller can already read the licence and see that the entry is a
    distributor's.
  - The console and the CLI show it as the reason.
  - The `authz.refused` event gains `name`, `source` and the operation (`import`, `remove`,
    `public`, `private`), the entry resolved (`entry`, its id form), and `name_unverified` when the
    name was read from an import's token before it was verified. The audit writer coalesces these
    refusals per operation, so each is told.
  - The check runs when the service resolves the entry, and again inside the write's transaction.
- **A copy made public is not prevented.** A tenant administrator reads the distributor's licence,
  so they can import a copy into another entry where its key is live and make it public there. That
  is covered by the distributor's terms and spec 0035 phase 2's warning, not refused (the owner's
  decision: an organisation serving a licence publicly need not be prevented now). A refusal keyed on
  the licence's issuer and id would be bypassed anyway: a replacement with `If-Match`, the relay and
  bundles keep a public row public, and a renewal changes the id.
- **Answers.** A licence's answer gains `distributor` when its entry is a distributor's
  publisher's. The keys listing already includes it.
- **Console.** For a caller who is not a server administrator, the Licences screen shows a
  distributor's entry as read-only:
  - its licence has no Remove, Make public or Make private actions;
  - the import dialog lists the entry but does not let the caller choose it. It shows the note "a
    distributor's: its licence manager writes it".

  A server administrator's console does not change.
- **CLI.** No change. `kista admin` acts as a server administrator.

**Older licences are refused**, with the rule `older`. This applies to every entry.

- **The rule.**
  - kista refuses to store a licence when the tenant holds another licence of the same package type
    and name, with the same `iss` and the same holder id, and a later `iat`. The stored licence can
    be in any entry of the tenant. It counts when it is in a distributor's publisher's entry (live
    or removed), whatever its key, and in any other entry only when it was verified by the same key
    as the incoming licence.
  - The same key outside a distributor's entry, because `iss` and the holder are the licence's own
    claims: a tenant administrator could add an upstream whose build declares a key of their own,
    sign a licence with the issuer's `iss` and holder and a later `iat`, and so freeze every write of
    the issuer's licences in the tenant. A stored licence's key (`key_fp`) is the fingerprint of the
    key material that verified it (the key set is built from the declared keys' material, never from
    a declared kid), so comparing keys compares key material.
  - Any key in a distributor's entry, because only its licence manager and server administrators
    write there, so no forged key can freeze anything; and after the issuer rotates its key, its new
    licence there still supersedes a stale licence of the previous key imported elsewhere.
  - An equal `iat` is accepted. The console relies on this when it replaces a licence with the same
    file.
  - Licences from other issuers are not compared.
  - A stored licence does not count once its grace has ended, or while the server refuses its key
    (`licenses.refuse_keys`). So a licence signed with a key that was later compromised cannot block
    newer licences with a high `iat`. A key no longer live in its entry still counts: a tenant
    administrator can make it so (removing the distributor's publisher, blocking its bodies), then
    import an older licence elsewhere.
- **Where it applies.**
  - **Import**, including a replacement with `If-Match`. The answer is `400` `invalid`, "the
    licence is refused (older): …", like every other licence refusal. It is checked after the key
    and holder checks and before the stored licence is compared. So an older licence sent without
    `If-Match` gets `older`, not `409` "a licence is stored". The refusal is a `license.refused`
    event.
  - **The relay.** The licence is refused with the rule `older` in the run's result, and the stored
    licence stays. The check comes after the "unchanged" test and before `replaces`.
  - **Bundles.** The licence is refused with the rule `older`, per target. As for the relay, the
    check comes after the "unchanged" test. It also comes before the bundle's "superseded" refusal
    of a licence over an import, so when both apply, the refusal names `older`.
- **Where it is checked.**
  - One store helper reads the tenant's licences of the type and name. Go compares `iss` and the
    holder byte for byte, because SQL Server compares strings ignoring trailing spaces.
  - The service drops the stored licences that do not count: past their grace, of a refused key, or
    of another key outside a distributor's entry (it reads the entry's distributor).
  - All three paths call it inside the write's transaction, under the tenant's licence lock. Every
    licence write takes that lock, so nothing can be stored between the check and the write.
    `PutLicense` does not repeat the check, because it does not know the refused keys.
- **Going back on purpose.** Remove the newer licence, then import the older one. Both acts are
  audited.
- **Why every entry, and the whole tenant.**
  - The rule is about licences, not entries: an issuer's later licence for a holder supersedes its
    earlier ones.
  - Comparing within one entry only would be weaker. A tenant administrator could import an old
    licence into an empty upstream entry where the distributor's key is live, then switch the
    channel's rule to that entry. The tenant-wide comparison refuses that import while the newer
    licence is stored. It does not undo a licence stored earlier (see Gaps).
  - The relay and bundle imports run without anyone removing anything, so they need the rule most.
    They feed upstream entries.
- **Compatibility.**
  - Enterest issues `iat = max(now, LastIAT+1)` (`internal/licence/licence.go`). Its reconciler
    therefore always issues above the `iat` kista holds.
  - Third-party issuers must make `iat` increase for each holder. The website says so.
- **What it protects, honestly.**
  - It protects the state kista serves against accidents.
  - It also protects against tenant administrators who are not server administrators, on a kista
    that someone else hosts.
  - A customer who is the server administrator can still roll back by removing the newer licence. So
    can a licence file placed by hand on a client.
  - The real control is short licence lifetimes that the reconciler renews. This matches
    licensing.md: licensing "protects honest users from accidents, not against deliberate acts".
- **Gaps.** kista forgets a licence's `iat` once nothing it serves holds it. That happens:
  - after the licence expires;
  - after a writer removes it;
  - after the relay revokes its row (`license.remove`, reason `revoked`).

  After that, an older licence that is still in force can be stored again. kista keeps no
  tombstone; see Alternatives.

  **Pre-staging.** The rule only refuses new stores. It does not touch licences already stored. An
  older licence stored in another entry while it was still the newest stays there after a newer one
  arrives elsewhere. A channel serves it once its rule is switched to that entry. Likewise, when the
  relay refuses a licence as `older`, it keeps its own stored licence, which may be older than
  another entry's. kista adds no serving-time comparison for this. Short lifetimes bound it.

  **Other package types (latent).** A licence token carries no package type: the same token stored
  under another type is another licence to the rule, which compares within a type. Only one type
  exists today.

  **Copies.** A copy of a distributor's licence in another entry, made public there by a tenant
  administrator, is not prevented (Design, "A copy made public is not prevented").

No migration is needed: the stored row already has `issuer`, `holder_id`, `key_fp` and `issued_at`.

### 2. A narrower distribution admin

**The assumption: one distributor per server** (the owner's decision, 2026-10-29).

- A kista server has one distributor, Enterest. Other vendors reach a kista through upstreams.
- So distribution admins and licence managers are not bound to a distributor. They reach every
  distributor's publisher on the server.
- If a second distributor were added to one server, the first distributor's distribution admin
  could change the second one's publishers' keys and holds, withdraw its versions and purge its
  builds. The first distributor's licence manager could write the second one's licences.
- Binding a role to one distributor is a follow-up.

**What the role keeps**, on every active tenant:

- **Publishers** (distributors' publishers only):
  - create a distributor's publisher (`POST …/publishers` with `distributor`);
  - list and read distributors' publishers;
  - rename a publisher's distributor (`PUT …/publishers/{p}/distributor`);
  - set and clear the licence-key mark (`…/license-keys`);
  - set the hold (`…/hold`), which takes effect only above the tenant's minimum (below);
  - manage keys: list, read, add, and remove (with `purge_held`).
- **Catalogue.** Read distributors' entries, purge their builds, and withdraw their versions. This
  is unchanged.
- **The tenant's state** (item 3).
- **The server's reserved names**, read only. `GET /api/v1/reserved-names` answers a distribution
  admin with each name and its kind only. It leaves out `sources`, which name other tenants'
  upstreams and administrators. These are DuckDB's names, which a distributor's publisher may not
  publish.
- **Spec 0036's metadata writes and aggregated statistics** for distributors' entries. They are
  defined in spec 0036, not here.

**What the role loses:**

- **The tenant's own publishers (PR 1).** A distribution admin may act on a publisher only if it is
  a distributor's (`p.Distributor != ""`). On a tenant's own publisher, with or without the
  licence-key mark, these calls answer the same `404` as a missing publisher:
  - mark and unmark (both directions of `SetPublisherLicenseKeys`; the grants check for a
    distribution admin goes);
  - hold;
  - `PUT …/distributor`;
  - every key route (`ListPublisherKeys` and `TenantPublisherKeys` too);
  - the grant paths that go through `marked()`.

  A publisher with only the licence-key mark goes back to being a server administrator's
  (spec 0021).
- **Reads of the tenant's own publishers (PR 2).**
  - Lists leave them out, and reading one answers `404`.
  - `POST …/publishers` for a name that a tenant's own publisher holds answers `409` with the
    problem type `publisher-taken`. This tells a distribution admin that the name is taken, and
    nothing else. Enterest treats it as "foreign, the tenant's own". It is a refusal:
    `authz.refused` records it with `reason` `publisher-taken`.
  - Distributors' publishers are counted apart from the tenant's own: at most 10 per tenant, beside
    the tenant's 200, so a distribution admin never takes the tenant's room.
- **Ordinary publishers (PR 2).** `POST …/publishers` without `distributor` answers `404`.
- **Grants (PR 2).** `GET|POST …/grants` and `GET|DELETE …/grants/{id}` go back to `pathAdmin`.
  `distributorGrant` and the checks on the grants' `created_by` go.
- **Tenant reservations (PR 2)**, which are removed (below).

Grants and publishers that a distribution admin made before PR 2 stay. They become the tenant
administrators' to manage.

**Marking an existing publisher goes, for every caller (PR 2).**

- A distribution admin can no longer create an unmarked publisher. So spec 0033 B's path, which
  marks a fresh publisher that a distribution admin created, has nothing left to apply to.
- `PUT …/distributor` on an unmarked publisher answers `404`. A distributor's publisher is created
  marked.
- The freshness check goes. `PublisherUsed` stays for spec 0036 phase 2's refusal to rename a used
  publisher's distributor.

**The minimum hold for distributors' publishers (PR 2).** This answers the stolen-token problem.

- **Where it lives.** A new column, `tenants.distributor_hold_seconds`, `NOT NULL DEFAULT 86400`
  (one day), in the three dialects.
- **Its effect.** An entry that arrives from a distributor's publisher is held for the larger of the
  publisher's hold and the tenant's minimum. A distribution admin may set its publisher's hold to
  anything, but cannot go below the minimum.
- **Reading it at arrival.** For a distributor's publisher, `SourceHold` (`store/holds.go`) also
  reads `tenants.distributor_hold_seconds`, with a shared lock on the tenant's row held until
  commit, as it reads the publisher's row today. The `PUT` updates that row, so it and an arrival are
  ordered. If the arrival reads first, the raise waits for the arrival to commit, and its
  `RaiseHeld` then covers the new entry. If the raise commits first, the arrival reads the new
  minimum. Either way, no arrival misses the raise. A publication's build transaction and the `PUT`
  also both take the tenant's auth lock (`PublisherAuthLock`), which orders them as well; the row
  lock is what orders any other arrival.
- **The publisher answer** gains `effective_hold`: the larger of the two. A distribution admin cannot
  read the minimum otherwise.
- **Who sets it.** Tenant administrators and server administrators. A distribution admin is refused
  (`404`).
- **API.** `GET|PUT /api/v1/tenants/{t}/distributor-hold {"hold": "24h"}`, read as other holds are
  (spec 0033 A2, `0` for none).
- **CLI.** `kista admin tenant distributor-hold <tenant> [<duration>]`.
- **Console.** On the Publishers screen, a "Minimum hold for distributors' publishers" field for the
  tenant's administrators.
- **Changes.**
  - Raising the minimum applies to entries held now, as raising a publisher's hold does. The `PUT`
    calls `RaiseHeld` for the source of every distributor's publisher in the tenant, live or
    removed, in its transaction.
  - Lowering it applies to arrivals from then on.
- **Event.** `catalogue.hold` with `source: "distributors"`, `hold`, `before` and `held` (the total
  count raised). Its subject is `tenant:<name>`. The audit registry's subject list for
  `catalogue.hold` (`internal/audit/audit.go`) gains `tenant:<name>`.
- **Why a source hold and not a channel hold.**
  - A held entry is what revoking a key with `purge_held` purges (spec 0033 A2). A channel's hold
    only delays placements in that channel.
  - A channel administrator's `early` placement still skips the hold. That is the tenant's own
    choice.

**Tenant reservations are removed** (the owner's decision, 2026-10-29). kista is unreleased, so
there is no compatibility path. What goes:

- **Data.** The table `tenant_reserved_names`, and its entries in the tenant deletion's list
  (`store/tenantdelete.go`) and the test harness's list (`storetest.go`).
- **Routes and CLI.** `GET|PUT|DELETE /api/v1/tenants/{t}/reserved-names[/{name}]`, and
  `kista admin tenant-reserved add|remove|list`.
- **Events.** `reserved.tenant.add` and `reserved.tenant.remove`.
- **Console.** The "Names reserved for a distributor" card.
- **Store.**
  - `ReservedTenant` and the `tenant` step of `ReservedFor`, leaving core, community and upstream;
  - `TenantReserves`, `TenantReservedHolder`, `TenantReservedSet` and `BumpTenantChannels`;
  - `NameReservedFor`'s tenant step.
- **The channel snapshot.** `TenantReserved` goes, along with its uses in the index's `reserved`,
  `"*"`, and the statistics scope (`api/stats.go`).
- **Release.**
  - the re-check under the release insert's locks;
  - `anyTakesNow`'s tenant check;
  - `distributorMay` and `distributorMayNow`, which become "not reserved server-wide";
  - the follows' holder check.
- **Elsewhere.**
  - `allow_reserved` for a tenant's names on upstream entries (`upstream.go`);
  - the attachment service's comment on `ReservedFor`;
  - the services `ReserveInTenant`, `ReleaseInTenant` and `ListInTenant`.

After this, a distributor's publisher may publish into the catalogue any name kista does not
reserve server-wide. A follow of a distributor's publisher takes names the way any follow does.

**Migration `0046_distributor_holds`**, in the three dialects:

- It drops `tenant_reserved_names` and adds `tenants.distributor_hold_seconds`.
- `min_reader` is 46. An older binary reads the table on every channel snapshot, and would ignore
  the minimum hold.
- The number follows the order in Delivery: spec 0036's two phases take 0044 and 0045.

### 3. A distribution admin reads a tenant

- **Access.**
  - The rule for `GET /api/v1/tenants/{t}` changes from `pathAdmin` (with `closed`) to admit
    distribution admins too.
  - `decide` answers a suspended tenant with `404` for everyone but server administrators. It gets
    a deliberate exception for a distribution admin on this one route.
  - A deleting, deleted or unknown tenant answers `404`.
  - The tenants service reads the tenant itself. It does not use `activeTenant`.
- **Answer.** `{"name", "state", "holder_id"}`, with `holder_id` left out when none is set. There is
  no ETag, display name, dates or storage domain.
  - Enterest treats a missing `holder_id` as "not ours".
  - A token that is both a server administrator and a distribution admin gets the full answer.
- **Audit.** A distribution admin's read writes `tenant.read` (`state`) to the tenant's events,
  including for a suspended tenant. The service writes it directly, because the API's `record()`
  only records refusals and failures, and skips tenants that are not active.
- **Why suspended too.** Enterest needs to tell "suspended, wait" from "gone". The role is already
  server-wide.

### 4. Publisher ids in answers

- **Publisher answers.** Every publisher answer (`publisherJSON`) gains `id`: list, read, create,
  hold, distributor mark and licence-key mark.
- **whoami.** For each entry of `publishers`, a signed `whoami` gives its `id` and its
  `distributor`. `distributor` is left out for the tenant's own publishers.
- **Catalogue-only publication.** The answer gains `publisher_id` and `distributor` for the
  publisher that published.
  - `distributor` is left out for the tenant's own publisher.
  - Both are left out when a tenant principal publishes with a token, since no publisher is
    involved.
- **Events.** `publisher.add` and `publisher.remove` gain `id`, added to their fields in the audit
  registry (`internal/audit/audit.go`).
- **CLI.** `publisher list` gains an `ID` column, and `publisher show` prints `id:`.
- **Console.** The publisher detail shows the id.

The ids are opaque and grant nothing.

### 5. A distributor's publisher reads its catalogue entries

- **Access.**
  - `GET …/catalogue` and `GET …/catalogue/{source}/extensions/{ext}` admit a signed request from a
    distributor's publisher.
  - Today `decide` reads the `distributors` flag only for `pathVerbs` routes, and these reads are
    `distributionAdmin` routes. So `decide` gains a branch: on a route with `publishers` and
    `distributors`, a signed caller whose publisher is a distributor's is admitted.
  - A request signed by a tenant's own publisher still answers `404`.
  - The statement names the empty body, as a signed `whoami` does.
- **Service.** A publisher's view of the catalogue:
  - It finds the caller's own source by its publisher **id**. The source's label may be
    `publisher-id:<id>` when a removed publisher of the same name existed.
  - It answers that source only, in the distribution admin's view: no rules, no `placed`, and
    `publisher_id` on each entry. `?type=` is accepted.
  - `?source=` (on the list) or `{source}` (on the entry) may be `publisher:<its name>` or
    `publisher-id:<its id>`. Either always means the signer. On the list, it may also be omitted.
    Any other source answers `404`, not an empty `200`.
- **Removal.** A removed publisher's keys are gone, so it can read nothing.
- **Spec 0036.** Its signed metadata `PUT` has its own access rule (any publisher of the tenant,
  for its own source). The two branches exist side by side.

### 6. Problem types for `429`

| Cause (site) | Type | `Retry-After` |
| --- | --- | --- |
| the per-client rate limiter (`api.go`, `ServeHTTP`) | `rate-limited` | the limiter's |
| upload slots per actor, tenant or server: publications and attachments (`publish.go`, `slot`) | `upload-slots` | `5` |
| every ingest slot taken (`blob.ErrBusy`): a publication (`publish.go`, `publishErr`) or a bundle chunk (`uploads.go`) | `ingest-busy` | `5`; `1` for a chunk |
| too many chunks of one bundle upload in flight (`upstream.ErrUploadBusy`, `maxParallelChunks`) | `chunks-busy` | `1` |
| a source's catalogue full (`catalogue.max_entries`, `max_bytes`; `publish.go`) | `catalogue-full` | none |
| an issuer being added to the tenant (`manage.go`) | `issuer-busy` | `5` |

- `too-many-requests` leaves the set.
- `upstream.ErrUploadBusy` stands for two causes today. They are split, so the chunk route answers
  either `chunks-busy` or `ingest-busy`.
- The status stays `429`, and the existing `detail` texts stay.
- `catalogue-full` has no `Retry-After` because kista does not know when space will free up. It
  clears when the source's builds are placed, purged or trimmed. The wording says that.
- The console's upload retries keep treating `429` as transient. The console never publishes, so it
  never meets `catalogue-full`.

## Security

- **Licences (1).**
  - **What is closed.** A tenant administrator, or a stolen tenant administrator's token, can no
    longer remove, replace or make public a distributor's licence in its entry. Making it public would
    serve the distributor's paid licence to anonymous clients.
  - **What stays open.** They read the licence, so they can import a copy into another entry where
    its key is live and make that public. The distributor's terms and spec 0035 phase 2's warning
    cover it; kista does not prevent it.
  - **What tenant administrators lose.** They can no longer write those entries. This is intended:
    the licence is the distributor's contract with the organisation.
  - **What they keep.** The tenant still decides what its channels serve, by switching a rule
    (spec 0035). It can still remove the distributor's publisher.
  - **The `older` rule.** It protects against accidents, replays (a relay from a remote restored
    from a backup, a bundle imported again), and tenant administrators on a kista that someone else
    hosts. It does not stop a writer who may remove the newer licence first:
    - a server administrator;
    - a stolen licence manager's token, as before; both acts are audited.

    Short lifetimes, renewed by the reconciler, remain the real control.
- **Distribution admins (2).**
  - **What is closed.** The `marked()` hole, the unchecked unmark, and the grant paths. A
    distribution admin no longer reaches a publisher that holds the tenant's grants.
  - **A stolen distribution admin's token** reaches distributors' publishers, their keys, holds and
    catalogue entries. It cannot write grants or reservations.
  - **What the token can still do.** It can add its own key to Enterest's publisher and publish.
    Nothing it publishes reaches a channel by itself. But a channel's `auto` rule or follow of that
    publisher, which the tenant wrote, places the build once the tenant's minimum hold has passed.
    Through a follow, that is a new name too, not only a new version of a name the channel has.
    A follow of a distributor's publisher skips a name an upstream of the tenant lists, as grants
    reach such a name only by naming it.
  - **Keys are unique in the tenant.** Adding a key that one of the tenant's publishers already
    holds answers `409`. That tells a distribution admin that some publisher of the tenant holds
    that public key, and a key a distributor registers first cannot then be registered on the
    tenant's own publisher. When one of the tenant's own publishers holds the key, the `409` is a
    refusal for a distribution admin (`authz.refused`, `reason` `key-taken`); a key a distributor's
    publisher holds already is a plain `409`. Public keys are not secrets, so kista does not hide it.
  - **The defences.**
    - The minimum hold (a day by default). The distribution admin cannot lower it.
    - Enterest's alerts on `publisher.key.add`, and on `catalogue.hold`, when the actor is a
      distribution admin (Enterest's P7). These replace the earlier alert on `publisher.add`,
      grants and reservations.
    - Revoking the key with `purge_held` within the hold, which purges what was published.
  - **What no hold covers.** Spec 0036's metadata writes (`catalogue.meta`) take effect at once. A
    stolen token can rewrite a distributor's entries' icons and summaries immediately, through a
    distribution admin's call or a newly added key. Enterest also alerts on `catalogue.meta` written
    by a distribution admin, or signed with a key added since its last look.
  - **What the token keeps.** It can still withdraw versions. Withdrawal is the distributor's right
    over its own versions.
  - **One distributor per server** is assumed (item 2). A second distributor on the same server
    would be reachable by the first one's distribution admin and licence manager.
  - **Tenant administrators.** Without reservations, the tenant's own grants reach every name that
    kista does not reserve server-wide and that no upstream of the tenant lists (spec 0009).
    Reservations only narrowed the tenant's own grants.
- **Tenant read (3).**
  - A distribution admin learns a tenant's state and expected holder, and that a suspended tenant
    exists. The role is already server-wide and acts in every active tenant.
  - No other field is answered, and each read is an event.
- **`publisher-taken` (2).** It tells a distribution admin that a publisher name is taken in the
  tenant, and nothing else about that publisher. Each one is an `authz.refused` event (`reason`
  `publisher-taken`), so probing the tenant's names is audited.
- **Ids (4).** They are opaque and grant nothing.
- **Signed catalogue reads (5).**
  - The publisher reads its own source only. It learns nothing about channels.
  - Signed requests keep spec 0023's statement and clock checks.
  - A stolen key could already publish under this publisher. Reading its entries adds nothing.
- **`429` types (6).** These add no information. Each cause was already in a `detail`, except the
  rate limiter's, which a caller can infer anyway. `catalogue-full` is answered only to a caller
  allowed to publish there, as today.

## Testing

- **Store suite, three dialects.**
  - The `older` helper:
    - a later `iat` in another entry of the tenant is listed, whatever its key;
    - an equal `iat` is not;
    - another issuer is not;
    - a holder that differs only by a trailing space is not;
    - (a stored licence past its grace, and another key outside a distributor's entry, are dropped
      by the service: its tests below.)
  - Migration 0046:
    - the table is gone;
    - the column's default is one day;
    - a level-45 binary is refused.
  - Tenant deletion and the test harness without the table.
  - The minimum hold:
    - an arrival from a distributor's publisher is held for the larger of the two holds;
    - raising the minimum applies to entries held now, for every distributor's publisher of the
      tenant, live or removed; lowering it does not;
    - a raise that races an arrival is not missed (the shared lock on the tenant's row).
- **Licence service.**
  - The `older` rule:
    - import, and replacement with `If-Match`: `400 older`, before `409`;
    - the same file again is accepted;
    - an old licence into an empty upstream entry, with the distributor's key live, is refused;
    - a stored licence past its grace does not block;
    - another key does not count outside a distributor's entry, and counts in it (a K1 licence after
      a rotation to K2 is refused because of the K2 licence in the distributor's entry);
    - a removed distributor's publisher's licence still counts;
    - the relay refuses with `older` and keeps the stored licence;
    - bundles refuse with `older` per target, before the bundle's "superseded" refusal.
  - The writers on a distributor's entry, live and removed (id form):
    - a tenant administrator is refused with `404 distributors-entry` on import, `DELETE`,
      `…/public` and `…/private`, and the refusal event names the operation;
    - a tenant administrator's reads succeed;
    - a licence manager writes but cannot make the licence public;
    - a server administrator writes everything.
- **Tenants.**
  - On a tenant's own publisher, with and without the licence-key mark, a distribution admin's
    mark, unmark, hold, `PUT …/distributor`, key routes and grant paths all answer the same `404`
    as a missing publisher. This is the regression test for the hole.
  - After PR 2:
    - no grants and no ordinary publishers;
    - lists show only distributors' publishers;
    - a taken name answers `409 publisher-taken`;
    - `PUT …/distributor` on an unmarked publisher answers `404`;
    - a distribution admin cannot set the minimum hold, and its publisher's hold of 0 still holds
      builds for the minimum;
    - the publisher answer's `effective_hold`;
    - the `catalogue.hold` event with subject `tenant:<name>`.
- **API.**
  - `GET tenants/{t}` for a distribution admin:
    - the exact field set;
    - `holder_id` left out when unset;
    - no ETag;
    - on active and suspended tenants; a deleting tenant answers `404`;
    - the `tenant.read` event, a suspended tenant included;
    - a tenant administrator's answer is unchanged;
    - a token that is both admin and distribution admin gets the full answer.
  - `GET /api/v1/reserved-names` for a distribution admin answers without `sources`.
  - Ids:
    - `id` in publisher answers and in a signed `whoami`;
    - `publisher_id` and `distributor` in the catalogue-only answer, and both left out for a
      tenant principal's publication.
  - Signed catalogue reads:
    - its own source, with no rules and no `placed`;
    - `?type=`;
    - `publisher:<its name>` resolves to the signer even when a removed distributor's publisher had
      the same name;
    - another source answers `404`;
    - a tenant publisher's signed request answers `404`.
  - Each `429` site answers its type, with or without `Retry-After`. The chunk route answers both
    of its types.
  - The routes removed in PR 2 answer `404`. `TestDistributionAdmin`
    (`api/distributors_test.go`) is rewritten as the matrix of what the role keeps and loses.
- **Unit tests to rewrite or drop:**
  - in `internal/api`: `distributors_test.go`, `manage_test.go`, `types_test.go`;
  - in `internal/release`: `distributors_test.go`, `publish_test.go`, `withdraw_test.go`;
  - in `internal/upstream`: `reserved_test.go`, `holds_test.go`;
  - in `internal/store`: `reserved_test.go`, `packagetypes_test.go`, `tenantdelete_test.go`;
  - in `internal/audit`: `coverage_test.go` and `sinks/sinks_test.go`;
  - in `cmd/kista`: `admin_test.go`, `admin_types_test.go`.
- **CLI.**
  - `tenant-reserved` is gone.
  - `tenant distributor-hold` works.
  - `publisher list` and `show` print the id.
  - `license import` prints the `older` and `distributors-entry` refusals.
- **Console.**
  - Unit tests:
    - the Licences screen: a distributor's entry is read-only to a tenant administrator and
      writable by a server administrator;
    - the import dialog's disabled entry;
    - the Publishers screen: no reservations card, the id shown, the minimum hold field.
  - `make console-e2e`:
    - `licenses.spec.ts`: the case where `tom` writes `publisher:enterest-ci`'s entry now uses a
      non-distributor entry, and a new case shows `tom` refused on it. The `vault-dist` block that
      relies on a tenant administrator writing a distributor's entry goes.
    - `run.sh` and `access.spec.ts` lose `tenant-reserved`.
    - The minimum hold is set to 0 where a test places a distributor's build at once, including
      the fixtures spec 0036 adds (`catalogue.spec.ts`).
- **End to end at the pin** (`make e2e`).
  - `TestLicenseFromDistributor`:
    - the tenant administrator's import into the distributor's entry is refused;
    - the licence manager's import works;
    - an older licence from the same issuer is refused with `older`;
    - the distributor's publisher reads its entries with a signed request.
  - `reserved_test.go` loses its tenant-reserved part.
  - The e2e tenants set the minimum hold to 0 where they need placement at once, including the
    fixtures spec 0036 adds (`e2e/distributors_test.go`).

## Delivery

Two pull requests, in this order with spec 0036. Each runs `make test` on the three databases,
`make lint`, `make console-test`, `make console-e2e` and `make e2e` before it opens.

1. **kista 0037 PR 1, `0037-distributor-in-tenant-1`: answers, licences and the hole.**
   - It covers items 1, 3, 4, 5 and 6, the `marked()` narrowing, and the reserved-names read
     without `sources`.
   - It has no migration.
2. **kista 0036 phase 1** (migration 0044).
3. **kista 0036 phase 2** (migration 0045).
4. **kista 0037 PR 2, `0037-distributor-in-tenant-2`: the narrower role.**
   - It covers the read narrowing, `publisher-taken`, the removal of ordinary publishers and
     grants, tenant reservations removed, and the minimum hold.
   - Its migration is 0046 (`min_reader` 46). It mostly removes code.

**Enterest, audited against this spec.**

- It no longer uses tenant reservations.
- It writes grants only with a server administrator's token (`internal/worker/provision.go`),
  never as a distribution admin.
- Its reconciler is a licence manager, and is not affected by PR 1's licence rules.

**Enterest's follow-ups after PR 1:**

- read the ids from creation and from `whoami`, and identify its publisher by id;
- read the tenant before writing, and stop when `holder_id` is missing or not the organisation's;
- treat `429` by its type;
- optionally, check with a signed catalogue read that the publisher sees its own versions.

**Enterest's follow-ups after PR 2:**

- map `409 publisher-taken` to "foreign, the tenant's own" in `look`. Today, `look` sees `404`,
  then `409`, then `404` again;
- update its security text and its alerts: `publisher.key.add`, `catalogue.hold` and
  `catalogue.meta` by a distribution admin, and `catalogue.meta` signed with a newly added key;
- account for the one-day default minimum hold. Its builds reach a tenant's channels a day after
  publication unless the tenant lowers the minimum. That affects its storefront ("available in")
  and how long provisioning takes to show a first version.

**The developer kit.** `kista_publish.py` in extension-ci-tools retries every `429` up to its limit.
It should stop on `catalogue-full`, both from a publication and from spec 0036's metadata `PUT`. It
may keep retrying the other types.

### As built (PR 1)

Delivered on the branch `0037-distributor-in-tenant-1`, with no migration.

- **Licences.** The distributor's-entry check (`refuseDistributorsEntry` in `internal/licenses`)
  runs when the entry is resolved and again in the write's transaction, as part of `recheck`; it
  admits a licence manager (its entries are distributors' only) and a server administrator
  (`Allow(admin, server)`). On an import it runs before the token is parsed, so the refusal's event
  names the licence read from the token, marked `name_unverified` (`""` when the token names none).
  The API maps `*DistributorsEntry` to the typed `404` and records its fields on `authz.refused`
  through the status writer; the audit writer's coalescing key includes a refusal's `operation`.
  `older` is one service helper over `store.LaterLicenses` (issuer and holder compared in Go),
  called by the import, the relay and bundles; after review it compares the key outside a
  distributor's entry and counts any key in one (Design above), and exempts a stored licence only for
  its grace or a refused key (the first cut also exempted a key not live in its entry, the relay's
  `deadKey`). Its refusal names the entry holding the later licence; the API writes
  `license.refused` for it. A refusal from `recheck` on an import, before the token is verified,
  marks the name unverified too. A refusal of making a copy public, added by the first review, was
  removed after the second (bypassable; Design, "A copy made public is not prevented").
- **Signed catalogue reads.** Another source than the signer's answers `authz.ErrDenied` (the same
  `404` bytes), so it is audited as a refusal.
- **Both roles.** A server token whose principals are a licence manager's and a distribution admin's
  is refused (`401`, `auth.Server.Verify`, since spec 0030); the API would otherwise act as the licence
  manager. The access page says so.
- **The narrowing.** `marked()`, `SetPublisherLicenseKeys` (both directions), `ListPublisherKeys`,
  `TenantPublisherKeys` and `SetPublisherDistributor` refuse a distribution admin on a publisher
  without `Distributor`. So a distribution admin no longer marks a fresh publisher it created; a
  server administrator still does until PR 2 removes the path.
- **Reserved names.** A distribution admin's answer leaves out `added_at` as well as `sources`
  ("each name and its kind only").
- **CLI.** There was no `publisher show`: it is added, printing the publisher with its `id`.
- **Console.** The import dialog never preselects a distributor's entry for a caller who is not a
  server administrator, and "never one among several" still counts every entry listed.
- **429s.** The `ingest-busy` details name the ingest slots ("every ingest slot is taken; try
  again" for a publication, "…; send it again" for a chunk); the other details are unchanged. An
  attachment takes the tenant's upload slots by its id, shared with publications (it used the
  tenant's name before).
- **Tests.** The "old licence into another entry where the distributor's key is live" case uses a
  tenant publisher's entry rather than an upstream's: the rule is tenant-wide, whatever the entry's
  kind. A refused key's exemption is not tested on its own (outside a distributor's entry a stored
  licence of a refused key could only block an incoming licence of the same key, which is refused
  first; in one, it would take a second key declared there). A
  removed entry's import, `…/public` and `…/private` answer `400` (removed) to every caller, as in
  spec 0035; its `DELETE` is the one write refused as `distributors-entry`. `TestDistributionAdmin` and `TestDistributorRoutes` are adjusted to PR 1 (distributors'
  publishers created marked); their rewrite as the role's full matrix is PR 2's. The e2e server
  answers the licence routes when licensing is on. The console e2e fixture gains `plain-ci`, a
  tenant publisher whose builds declare no licence key.
- **Earlier specs.** 0007, 0008, 0012, 0021, 0023, 0029, 0033 and 0035 carry a "Changed by 0037
  (PR 1)" line; 0030, 0031, 0034 and the reservation parts of 0033 and 0036 change with PR 2.
- **Website.** The deploy variables' descriptions say distributors' publishers now (their
  reservations wording stays for PR 2); `upgrading.md` and `upstreams.md`'s `allow_reserved` change
  with PR 2.

### As built (PR 2)

Delivered on the branch `0037-distributor-in-tenant-2`, on spec 0036 phase 2, with migration
`0046_distributor_holds` (`min_reader` 46).

- **Tenant reservations removed.** The table, the routes, `kista admin tenant-reserved` (now an
  unknown command), the events, the console card and the store's and services' reservation calls
  are gone, as Design lists. `ReservedFor` answers core or community, then upstream.
  `NameReservedFor` became `Store.ServerReservedKind`, a plain read of the server's list that shares
  its query with `Tx.ReservedKind`.
  `Store.ReservedAgainst` (spec 0036's list of a distributor's publisher's names) became
  `Store.ReservedAmong`, the names reserved server-wide among a list. The statistics scope and the
  index's `reserved` read DuckDB's names and the upstreams' only. `allow_reserved` on an upstream
  entry concerns DuckDB's names only. `store.MaxHolder` is `store.MaxDistributor` now, the bound of
  a distributor's name.
- **The narrower role.**
  - `AddPublisher` admits tenant administrators and server administrators only, so a distribution
    admin's `POST …/publishers` without `distributor` answers `404`.
  - `AddDistributorPublisher` reads the name under the tenant's auth lock before its insert. For a
    distribution admin, a name a tenant's own publisher holds answers `409 publisher-taken`. A name a
    distributor's publisher holds answers the usual `409 conflict`, as it does to a server
    administrator for any name. The read and the insert are under one lock, so no test hooks a
    publisher in between.
  - Refusals with a `reason`: the API records `publisher-taken` and, for a distribution admin, a
    key's `409` when one of the tenant's own publishers holds it (`key-taken`: `AddPublisherKey`
    reads the key's holder under the auth lock, `Tx.PublisherKeyHolder`) as `authz.refused` events.
    The audit writer coalesces `authz.refused` per `reason` as it does per `operation`; other kinds'
    `reason` (`auth.failure`'s) does not split their limit.
  - `InsertPublisher` inserts a distributor's publisher marked (`Publisher.Distributor`) and counts
    the two kinds apart: `MaxPublishers` (200) for the tenant's own, `MaxDistributorPublishers` (10)
    for distributors'.
  - A follow of a distributor's publisher skips a name an upstream of the tenant lists, in
    `followLimit` and again in `takeName`, which then takes the tenant's upstream lock before the
    channel lock (spec 0009's order). Every change to an upstream's entries takes that lock, so no
    entry lands between the check and the rule.
  - `ListPublishers` keeps distributors' publishers only for a distribution admin. The API's read of
    one publisher goes through that list, so the tenant's own answers `404`.
  - The grant routes are `pathAdmin` again; `distributorGrant` and the `created_by` checks went.
- **Marking an existing publisher goes.** `PUT …/distributor` on a publisher without the mark
  answers `404` to every caller: an audited refusal (`authz.ErrDenied`) for a distribution admin, not
  found for anyone else. The call only renames a distributor's publisher's distributor, and so does
  the store's `Tx.SetPublisherDistributor`: it answers not found for a publisher without the mark.
  `PublisherUsed` stays, for spec 0036 phase 2's `409` on renaming a used publisher's distributor:
  only its use for the freshness check went. `Tx.PublisherGrants` went with that check.
- **The minimum hold.**
  - `tenants.distributor_hold_seconds` is read with the tenant (`Tenant.DistributorHold`).
  - `Tx.SourceHold` reads the tenant's row, with a shared lock, only for a distributor's publisher.
  - `Tx.SetDistributorHold` writes only a changed value. The service runs it in
    `InTenantTxForUpdate` (an update lock on the tenant's row, which an arrival's shared read waits
    for) with the tenant's auth lock, then calls `RaiseHeld` for every source of
    `Tx.DistributorPublisherSources` (live and removed distributors' publishers).
  - An unchanged value writes no event.
  - `effective_hold` is answered for distributors' publishers only, from the tenant as the request
    read it.
  - The console shows the field as a "Minimum hold for distributors' publishers" card on the
    Publishers screen, and reads the publishers again after a change. A caller who may not read it
    sees no card. The list's Hold column shows a distributor's publisher's effective hold.
  - `kista admin tenant distributor-hold <tenant>` prints the hold; with a duration, it sets it.
    `tenant show` prints the minimum; `publisher list` has a `HELD FOR` column and `publisher show`
    a `held for` line for a distributor's publisher.
- **Tests.** `TestDistributionAdmin` is the role's matrix:
  - what it keeps: publishers, keys, marks, holds, the rename, catalogue reads, purges, withdrawals
    and metadata, a distributor's statistics;
  - what it loses: the tenant's own publishers, ordinary publishers, publisher removal, grants and
    `grants/principals`;
  - the removed routes, for every caller;
  - the minimum hold: refused to a distribution admin at the API and at the service, one event per
    change, and no `effective_hold` on the tenant's own publishers;
  - the `publisher-taken` and `key-taken` refusals, audited.

  `TestDistributorRoutes` loses the freshness cases. It gains:
  - a distributor's build held for the minimum;
  - the minimum lowered (nothing released) and raised (held longer, `held: 1`).

  `TestDistributorHoldRaiseRemoved` raises the minimum over a removed distributor's publisher's held
  entry; a removal through the API purges held entries, so the store removes the publisher there.
  The store suite covers:
  - migration 0046, a level-45 binary refused included (`Store.checkLevel`);
  - the minimum at arrival;
  - the distributors' cap;
  - the race with a raise (`TestDistributorHoldReadLocked`). It reads the minimum alone and checks
    that the raise waits for the arrival. With `SourceHold`'s row lock removed, it fails on
    PostgreSQL and SQL Server. Tests that place a distributor's build at once set the minimum
  to 0: the release and upstream helpers, the e2e fixtures, the console e2e's `run.sh`. Tests that
  reserve a name server-wide remove it at cleanup, since the server's list outlives a test on a
  shared database. Server-wide names replace the reservations that some tests used:
  - "reserved meanwhile" (`TestDistributorMayNow` and spec 0036's metadata tests);
  - a follow's skip;
  - an eager mirror's `*` refused under the insert's locks.
- **Earlier specs.** 0007, 0019, 0030, 0031, 0033, 0034 and 0036 carry a "Changed by 0037 (PR 2)"
  line. 0030's listed sections open with a "superseded in part" note. 0031's sentence on a
  distributor's tenant reservations goes. 0033 and 0036 carry inline notes where their rules
  change. 0034 keeps its per-type reservation text as history.
- **Website.** The pages Website lists, the three deploy variable descriptions and the deployment
  pages follow PR 2. `upgrading.md` has a "schema 46" section: migration 0046, the reservations
  dropped, the narrower role and the one-day default.

## Alternatives considered

- **Keep tenant reservations for server administrators.** The owner decided to remove them.
- **Let tenant administrators write distributors' entries, with a warning**, as spec 0035 phase 2
  does for public serving. The reconciler would still be undone between its runs.
- **The `older` rule per entry, or for distributors' entries only.** A tenant administrator could
  load an old licence into another entry and switch the rule to it. The relay and bundle paths would
  stay open.
- **A tombstone that remembers the newest `iat` per (type, name, issuer, holder) after removal.**
  It needs a new table. Once nothing serves the newer licence, any writer of an entry can store an
  older one that is still in force. That means tenant administrators for their own entries, and the
  relay and bundles for upstream entries. Short lifetimes bound how long such a licence stays in
  force, and that is the control we rely on.
- **A constant-bytes `404` for a tenant administrator's write.** The console and CLI could not tell
  the caller why. The caller can already read the licence, so a type and a detail reveal nothing.
- **A channel hold instead of a minimum source hold.** A channel hold does not keep entries held,
  so `purge_held` would not purge what a stolen key published.
- **`409` for a full catalogue.** It is a quota. A `429` with its own type and without
  `Retry-After` says so, and clients act on the type.
- **Signed reads for every publisher** (the tenant's own CI). Nobody asked for them.

## Changes to earlier specs

- **0007**:
  - the problem types (item 6), and `distributors-entry` as a `404` with a type and a detail;
  - `GET tenants/{t}` for a distribution admin;
  - the routes PR 2 removes.
- **0008**: the types of the upload-slot and ingest refusals.
- **0012**: the relay's replacement rules gain the `older` rule. A licence that lasts longer no
  longer replaces one with a later `iat` from the same issuer for the same holder.
- **0019**: the reaper no longer removes tenant reservations.
- **0021, 0023**: a distribution admin manages keys and the licence-key mark of distributors'
  publishers only.
- **0029**: licence managers and server administrators are the only writers of distributors'
  entries. A licence manager still never makes a licence public.
- **0030**: tenant reservations are removed. This affects these sections:
  - Summary;
  - Problem's third bullet;
  - "Names reserved in a tenant";
  - the lookup's `tenant` step;
  - Data model's `tenant_reserved_names`;
  - As built (part 2);
  - Security's tenant isolation.

  The distribution admins' calls become the list in this spec. The implementing PR adds a
  "superseded in part by spec 0037" line at the head of those sections.
- **0031**:
  - line 119: "a distributor's tenant reservations (0030) remain available" goes;
  - line 183: the alternative that reserves names up front stays, as history.
- **0033**:
  - B's rule for a distributor's names becomes "not reserved server-wide"; the "for another holder"
    part goes;
  - follows of a distributor's publisher lose the holder check;
  - "Distribution admins (spec 0030) gain …" becomes the list here;
  - a distributor's publisher reads its own entries;
  - distributors' publishers get the tenant's minimum hold;
  - a full catalogue's `429` gets its own type;
  - marking an existing publisher goes.
- **0034**: `tenant_reserved_names` per type (lines 204 and 284) and `tenant-reserved` among the
  typed commands (line 341) stay, as history, with a note.
- **0035**:
  - "Who writes which entry": tenant administrators write every entry except distributors'
    publishers';
  - the `older` rule applies to import, relay and bundles.
- **0036**:
  - "nor in the tenant for another holder", in its rule for a distributor's publisher's signed
    metadata write (`distributorMay`), goes;
  - its metadata writes and statistics stay with the role; they are referenced here, not defined.

## Website

- `access.md`: the distribution admins' list; who writes a distributor's entry; the minimum hold.
- `licensing.md`:
  - lines 52-57: marking a publisher is a distribution admin's act only for distributors'
    publishers;
  - lines 191-195: the relay's rules gain `older`;
  - a distributor's entry;
  - the `older` rule and its gaps;
  - `iat` must increase per holder for third-party issuers.
- `api.md`:
  - line 36: signed routes include the catalogue reads;
  - the problem types table;
  - `GET tenants/{t}` for a distribution admin;
  - `distributor-hold`;
  - publisher `id`, `whoami`, and the catalogue-only answer;
  - `GET /api/v1/reserved-names` without `sources` for a distribution admin;
  - the reserved-names and grant routes.
- `publishing.md`:
  - lines 98-103: tenant reservations go;
  - lines 370-388: creation only marked, no marking of existing publishers, names not reserved
    server-wide;
  - a distributor's publisher reading its entries;
  - the ids;
  - the `429` types for CI scripts.
- `catalogue.md`: distributors' reads; the minimum hold.
- `upstreams.md` (lines 64-67): `allow_reserved` no longer applies to a tenant's names.
- `security.md` (lines 201-207): the distribution admin's reach, and the one-distributor
  assumption.
- `administration/index.md` (line 24): the role table.
- `administration/commands.md`:
  - `tenant-reserved` removed;
  - `tenant distributor-hold`;
  - the publisher id;
  - `older`.
- `administration/upgrading.md`: migration 0046 and its `min_reader`; reservations dropped; the
  minimum hold's default.
- `configuration.md` (line 437): `auth.server_distribution_admins`.
- `observability.md`:
  - `reserved.tenant.*` removed;
  - `id` on publisher events;
  - `tenant.read`;
  - the fields of `authz.refused`.
- `console.md`: read-only distributor entries; the Publishers card removed; the minimum hold field.
- Deploy variable descriptions: `deploy/azure-container-apps/main.bicep`,
  `deploy/gcp-gke/variables.tf`, `deploy/aws-ecs/variables.tf`, and `deployment/aws.md`,
  `gcp.md` and `azure.md`.
- Code comments and help text:
  - the CLI help for `publisher distributor` (`cmd/kista/admin.go`);
  - `config/auth.go`, `authz/authz.go` (`VerbDistribute`), `tenants/auth.go` (`distributor`),
    `tenants/publishers.go` (`SetPublisherDistributor`; and `SetPublisherHold`, whose "a tenant
    that wants more waits with a channel hold" becomes the tenant's minimum hold).

`getting-started.md`, `administration/tenants-and-channels.md` and `console.md` mention only
server-wide reserved names. They change only as listed above.

## Follow-ups

- **Binding** distribution admins and licence managers to one distributor, if a server ever
  serves more than one.
- **A licence manager's read** of a tenant's state and holder, if the reconciler needs it.
- **A distribution admin removing a distributor's publisher**, if Enterest needs it. Today a
  tenant or server administrator does it.
- **`early` placement** asking for recent authentication (Enterest's request), in another spec.
- **Binding a signed request to its server's host**, as defence in depth. Enterest uses one key per
  tenant, so a request replayed to another kista finds no key there today.
