# Spec 0035: Licences per catalogue entry

- **Status**: draft
- **Date**: 2026-10-26
- **Author**: vgsml, Claude

## Summary

A licence belongs to a **catalogue entry**: a source's builds of a name in a tenant (spec 0033).
An import names the entry, and the licence must verify against the keys that entry's own builds
declare. A relay stores the licence into its upstream's entry. A channel serves the licence of the
entry it serves the name from. This replaces spec 0033 phase B part 2's **key owners**: a key's
first recorder, tombstones of removed owners, the rule that a distributor's records do not count
for names an upstream lists, the owner route and command, and the exceptions they needed. A copy
of a key under another source is another entry, and it has no licence until one is put there. A
small, unenforced rule comes with it: kista **warns** when an administrator makes a build of a
licensed entry public. Enforcing that rule is a follow-up.

## Problem

Spec 0033 phase B part 2 bound a licence to the source that **owns** its key for the name. The
owner was the first source whose counted build recorded the key. Ownership was meant to stop a
build carrying a copied public key from getting a licence or taking one away. To compute and keep
that owner, kista needs:

- a first recorder that accounts for holds, revocations, blocks and withdrawals, with ties broken by
  the source string. The migration also computes it in SQL in three dialects;
- claims on first need;
- tombstones for removed owners, updated by compare-and-set;
- a live rule that a distributor's publisher does not count for a name a `repository` upstream lists;
- an administrator's act that moves licences, with its own `409`s;
- licences of source `unknown`;
- and, in the relay and in bundles, an exception for a licence that is byte for byte the owner's,
  plus re-binding and adoption of `unknown` rows.

That is about 600 lines of store code and a documentation section that is hard to follow. It also
gives wrong answers:

- **A pull-through upstream blocks a distributor's licences.** A tenant with a `repository`
  upstream allowing `*` (another kista, pulled through) "lists" every name. So a distributor's
  publisher never owns a key in that tenant. Importing Enterest's licence for `geo_pro` fails, even
  when every channel serves `geo_pro` from Enterest. A tenant administrator then has to run
  `license owner` once per key and name, and again after every key rotation.
- **Two upstreams of one product depend on order.** A tenant that mirrors a remote's `prod` and
  `nightly` through two upstreams gets the licence on the second only when it is byte for byte the
  one the owner already holds. A renewal reaching the second upstream first is refused until the
  owner's relay has it.
- **History decides, not the administrator.** Where a licence lands depends on which source happened
  to record the key first. After an owner is removed, the key has no owner until a fresh record or an
  act gives it one: a second upstream already bringing the same builds gets no licence until a new
  version arrives or an administrator acts.

The owner's decision (2026-10-26): a licence belongs to a catalogue entry, the import names it,
and no other source's records matter. A copied key is simply another entry, with no licence.

## Design

### The model

- **A licence is per entry**: (tenant, package type, source, name). One tenant may hold two `geo_pro`
  entries, for example `publisher:enterest` with a licence that Enterest imports and `upstream:x`
  with none, or with its own licence relayed from `x`. Each entry holds at most one licence. As
  before, a licence names its holder (checked against the tenant's expected holder on every path,
  spec 0028) and its extension (`ext`).
- **The live keys of an entry** are the keys its source recorded (spec 0021: a marked publisher's
  publication, or a `repository` upstream's fetch or bundle) on **its own** builds of the name, while
  the source exists, from either:
  - a live build: an active or deprecated release in a signed channel whose rule serves this source
    for the name, its body not blocked and the entry not withdrawn; or
  - the source's newest available version of the name: not held, withdrawn or revoked, its body not
    blocked, by spec 0006's version order.

  This is phase B part 2's `LiveLicenseKeys`, unchanged and per source. Keys in
  `licenses.refuse_keys` count nowhere. A record that one source made on another source's build
  counts for neither. `tenant` and DuckDB's core and community upstreams record no keys, so their
  entries take no licence.
- **No other entry matters.** Importing, relaying or serving a licence reads only its own entry's
  keys and licence. Two entries that declare the same key are independent.

### Import

- `POST …/licenses?source=<source>` names the entry. The source is `upstream:<name>` or
  `publisher:<name>`, or the id forms `upstream-id:<id>` and `publisher-id:<id>`, as the licence
  routes take them today. **Without `?source=`**, the entry is the only one whose live keys for the
  licence's name include its key id. With none, the answer is `400 kid_untrusted`. With several, it
  is `409` naming them: "name one with `?source=`". The default only acts when the answer is
  unambiguous, so the SDK's tests, air-gapped sites and a name with one source keep working without
  a flag.
- **Checks**, unchanged except for the keys they read:
  - the licence is in force;
  - it verifies against the entry's live keys (`kid_untrusted`, naming the source);
  - its key is not refused;
  - it names the expected holder (`holder_mismatch`).
- **Storage.** The licence is stored in the entry as an import. It replaces the entry's licence
  only with `If-Match`, as today. `Location` names the source.
- **Who may import**, into any entry of the tenant, a distributor's publisher's entry included:
  - the tenant's administrators;
  - licence managers (spec 0029);
  - server administrators.

  Distribution admins (spec 0030) still have no licence routes.

### Relay and bundles

- **Relay.** An upstream's relay stores each licence into **its own entry** (`upstream:<id>`) when:
  - the upstream supplies the name (`not_supplied`, unchanged);
  - the licence verifies against that entry's live keys (`kid_untrusted`);
  - it names the expected holder.

  Spec 0012's and 0021's replacement rules stay: a newer licence from the same upstream, or one
  that lasts longer, replaces the stored one; an import in the entry is replaced only by a licence
  signed with the same key that lasts longer, or once the import's grace has ended or its key is
  dead. Two upstreams of one remote (Enterest's `prod` and `nightly` mirrored into a tenant's
  `prod` and `nightly`) each store their own copy, and each channel serves its own. A licence is the organisation's purchase, served by every channel that serves a genuine
  build of it (the owner's earlier decision stands); each entry that brings such a build gets it
  through its own relay. An upstream's relay governs only the licences it relayed into its entry:
  one it stops listing is removed, and an import into its entry is never removed by it. The
  `key_owner` refusal goes.
- **Bundles.** A bundle's licence goes into the entry of **each** bundle-mode upstream of its
  source that supplies the name and whose live keys verify it, checked as a relayed licence is (a
  bundle still never replaces an import). The export is unchanged: per name, the licence of the
  entry the files come from.

### Serving and usage

- **A channel serves, with the build, the licence of the entry it serves the name from**: the
  rule's source, as the channel's snapshot selects it. It serves the licence while its grace lasts
  and while a live build there that the caller sees declares its key (spec 0021), as B part 2
  built. **Switching the source switches the licence**: the next request reads the new snapshot.
  An entry without a licence answers `404`, and the SDK reads `missing`. A licence is never served
  through another entry, whatever keys that entry's builds declare.
- **Usage** is unchanged: it is forwarded by the name's selected source (B part 2).

### Removing a source

Its entries stay (spec 0033) and its keys go (spec 0021), so its entries have no live keys.
Removing an upstream removes the licences its relay stored, as today. Any other licence of a removed
source's entry (an import) stays: it is listed under the id form, served nowhere, removable, and
gone at the end of its grace. Nothing passes to another source. A source added again (spec 0033's
successor) is another entry, which gets its licence through its relay or an import.

### What goes

These go:

- the table `license_key_owners`;
- the first recorder, claims, tombstones and the "listed by an upstream" rule;
- `PUT …/license-keys/{name}/{kid}/owner` and `kista admin license owner`;
- the `owner` field of a licence and of a key;
- the events `license.key_owner` and `license.key_conflict`;
- the refusals `key_owner` and `ErrOwnerHolds`;
- the relay's byte-for-byte exception (`relayable`), `rebind` and `RebindLicense`;
- licences of source `unknown` and `adoptUnknown`.

`RecordLicenseKeys` and `DeleteLicenseKeysOf` lose their owner bookkeeping. kista has no release
yet, so there is no compatibility path for any of them.

### API, CLI, console

- **A licence** answers `source`: its entry's source (`upstream:<name>`, `publisher:<name>`, or a
  removed source's id form), the name that `?source=`, the events and the catalogue use. How it
  arrived (`import` or `upstream`) moves to `via`, and `upstream` still names the relaying upstream.
  The routes for one licence keep `?source=`: without it they act on the name's only licence and
  answer `409` when there are several.
- **Keys.** `GET …/license-keys[/{name}][?source=]` lists, per name and key, the entries whose live
  keys include it (`sources`, as today: kind and name) and the number of builds declaring it, with
  no `owner`. `?source=` keeps one entry's keys. This is how a licence manager tells whether a
  licence can land: its key id is listed for the entry.
- **CLI.**
  - `license import <t> <file> [-source <s>] [-replace]`;
  - `license keys <t> [<name>] [-source <s>]`, with the columns `NAME KEY BUILDS SOURCES`;
  - `license list` with the columns `NAME SOURCE PLAN HOLDER STATUS EXPIRES VIA VISIBILITY`;
  - `license owner` goes.
- **Console, Licences.**
  - The table's **Of** column becomes **Source**, and the arrival column becomes **Via**.
  - The import dialog reads the licence's key id from the JWS header and offers the entries that
    list it (one is chosen for you; with none, it says no available build of the extension declares
    the key yet). It replaces that entry's licence after a confirmation.
  - A licence is flagged "not served" unless its key is a live key of its own entry.
  - The keys table shows each key's entries.

### Data model

Migration `0043_license_entries` in the three dialects, `min_reader` 43, because an older binary
reads and writes `license_key_owners` and the relay's exceptions. It does the following:

- drops `license_key_owners`;
- deletes the licences of source `unknown` (served nowhere already);
- points each relayed licence (`source = 'upstream'`) whose entry is not `upstream:<upstream_id>`
  to that entry. Where that entry already holds a licence of the name, the moved one is dropped and
  the upstream's own copy stays. Of several licences moving into one entry, the latest issued is
  kept;
- renames `licenses.owner_source` to `entry_source`, still part of the primary key (tenant, type,
  name, entry_source). PostgreSQL uses `RENAME COLUMN`, SQL Server `sp_rename`, SQLite `RENAME
  COLUMN`. The store's `License.Owner` becomes `Entry`.

`ix_build_license_keys_kid` stays, because the import's default reads records by key id. Tenant
deletion and the store test harness drop the table from their lists.

### Public builds of licensed extensions (phase 2)

An organisation may mirror an extension with its licence into its own kista and serve it inside the
organisation as it likes. It must not serve it publicly with its licence unless the distributor
allows it. Nothing enforces this rule for now: the distributor's public terms state it, and kista
**warns**.

**How a build becomes public in kista.** Visibility belongs to a release; there is no public
channel. A release is public when one of these happens:

- an administrator makes it public (`POST …/releases/{id}/public`, `release public`);
- it is placed or promoted from an entry whose visibility is public (an upstream with `-public` or
  after its `public` action, or a publication with `visibility: public`), unless `private` is asked;
- a publication on the channel route asks for public.

A licence has its own visibility. A public licence is served to anyone served a public release of
the name from its entry, and only an administrator can make one public (spec 0029).

**A licensed entry** is one that holds a licence, or whose build being made public declares
licence keys recorded for its source.

- **Answers.** The administrator's routes that make something public answer `warnings: [{"code":
  "licensed_public", "name", "source", "message"}]`. The field is omitted when there is nothing to
  warn about. The routes are:
  - a release's `public`;
  - a placement or a promotion whose release is public;
  - an upstream's `public`, when one of its entries is licensed;
  - a licence's `public`, which always warns.

  The CLI prints each warning as `warning: …` on stderr and still exits 0. The console shows the
  warnings after the act; the licence's public dialog keeps its note.
- **Event** `license.public_warning` (`name`, `source`, `package_type`, `channel`, `releases` or
  `license`). It is written in the act's transaction whenever a release of a licensed entry is
  stored public or made public, on **any** path, including a publication or an automatic placement
  that answers nobody, and whenever a licence is made public. The check sits at the store's two
  writes of a public release (`InsertRelease` and `SetReleaseVisibility`), so no path is missed.
- **Nothing is refused.** Enforcement is a follow-up.

### Enterest and licence managers

Enterest's reconciler is a licence manager. In each customer tenant:

1. It reads `GET …/license-keys/{name}?source=publisher:<its publisher>`. Once the licence's key id
   is listed there (the version is available: its hold has passed), the licence can land.
2. It imports with `POST …/licenses?source=publisher:<its publisher>`, using `If-Match` to replace.
3. Before the key is listed, the import answers `400 kid_untrusted`, and the reconciler tries again.

A customer's own kista relays from Enterest through its upstreams, each into its own entry.

## Security

- **A copied public key** (an upstream's or a tenant publisher's build declaring Enterest's key) is
  another entry. It gets no licence unless one is put there: an administrator's or licence
  manager's import into that entry, or its own upstream's relay of a licence that verifies against
  its keys and names the tenant's holder. It takes nothing away from the genuine entry. A licence
  cannot be forged with a public key. A genuine licence for the organisation, served in a channel
  the organisation chose for builds that accept it, gives nothing the organisation did not already
  hold.
- **A distributor taking a tenant's licence**: a distributor's publisher reaches only its own entry.
  It cannot import, because distribution admins have no licence routes. Its entry's keys do not
  touch the upstream's licence. A pull-through `*` upstream and a distributor coexist: each entry
  has its own licence.
- **A removed source's licence**: its keys are gone, so it is served nowhere. The relayed licences
  go with the upstream. Nothing passes to a successor or to another entry with the same key.
- **Who writes into an entry**: tenant administrators, licence managers and server administrators
  (import); the entry's own upstream (relay, bundle). A licence manager may import into any entry of
  any tenant, as it may import any licence today. Each write is bounded by the entry's live keys,
  the refused keys and the expected holder.
- **Fail closed**: the import's default never guesses. With several candidate entries it answers
  `409`; a held, revoked, withdrawn or blocked build makes no key live; serving needs the channel's
  selected entry and a live build declaring the key.
- **Existence**: unchanged. The licence routes are for administrators and licence managers, and the
  DuckDB route answers as a missing path for a caller who may not have the licence.
- **The warning** changes no decision. An anonymous caller still gets a public licence only where
  an administrator made it public.

## Testing

- **Store suite, three dialects:**
  - the migration on a populated level-42 database: the owners table gone; relayed licences moved
    to their upstream's entry, with a collision dropped; `unknown` deleted; imports kept;
  - live keys per entry (own builds only, held, revoked, withdrawn, blocked, removed source);
  - the default entry's lookup by key id;
  - tenant deletion without the table.
- **The licence service** (`TestLicenseSources`, rewritten):
  - an import with `?source=` into a distributor's entry;
  - an import without a source: one entry, none (`kid_untrusted`) and two (`409` naming them);
  - an import into an entry whose build is still held (`kid_untrusted`, then accepted once the hold
    ends);
  - a copied key under another source getting nothing, with the genuine entry unaffected;
  - **the regression case**: a pull-through `*` repository upstream and a distributor's publisher
    of one name, the distributor's licence importing without any act;
  - the relay into the upstream's own entry, beside a distributor's licence for the same key;
  - two upstreams of one remote, each storing its copy, a renewal reaching either first, each
    revoking only its own;
  - an upstream's removal removing its relayed licences only, and a removed publisher's import
    served nowhere;
  - bundles into each supplying upstream.
- **Serving**: one name served from two entries in two channels, each with its own licence. Switching
  a channel's source switches the licence served, and an entry without a licence answers `404`.
- **The API and the CLI**:
  - `?source=` on import and its `409` and `400`;
  - the owner route gone;
  - `license-keys?source=`;
  - `source` and `via` in the answers;
  - the events registry (`license.key_owner` and `license.key_conflict` gone; phase 2's
    `license.public_warning`);
  - phase 2's warnings on each route and the event on a publication and on an automatic placement.
- **Console**: unit tests for the import's entry choice and the "not served" flag; the console's
  `licenses.spec.ts` adapted.
- **DuckDB end to end at the pin:**
  - `TestLicenseFromDistributor` adapted. The distributor's licence is imported with `-source`
    before placement and served from `prod`. The tenant's upstream brings `kista_demo` with the same
    key into `nightly`, which serves no licence (`missing`). **Two entries**: the same name from the
    distributor with a licence and from the upstream without. Switching `nightly`'s rule to the
    distributor's entry serves the distributor's licence there (`valid`). Phase 2 adds making
    `prod`'s release public, which answers the warning and writes the event.
  - `TestSDKLicenseRelay` extended: two upstreams of the remote, in two channels, each relay the
    licence into its entry, and DuckDB is licensed from both.
  - `TestSDKLicense` and `TestSDKLicenseBundle` pass with the import's default.

## Delivery

Two PRs:

- **Phase 1, `0035-licences-per-entry`: the model.** It covers:
  - the store: `licenseowners.go` goes, with `LiveLicenseKeys`, `SuppliedBy` and
    `TenantLicenseKeys` moved to `licensekeys.go` without owners; `licenses.go`; migration 0043 ×3;
    `tenantdelete.go`; `storetest`;
  - `internal/licenses` (import, relay, bundle);
  - `internal/serve` (field rename);
  - `internal/api` (route removed, `?source=` on import);
  - `internal/audit`;
  - `cmd/kista` (`admin.go`, `admin_license.go`);
  - the console's Licences screen and its tests;
  - the e2e tests above;
  - the website pages below.

  It mostly removes code.
- **Phase 2, `0035-licences-per-entry-warning`: the warning.** It covers the store check at the two
  public writes, the answers' `warnings` (release change, place, promote, upstream public, licence
  public), the event, the CLI and console display, tests and docs.

## Alternatives considered

- **Keep key owners and patch the `*` case** (for example, `*` not counting as listing a name):
  this fixes the pull-through tenant, but keeps the history-dependent first recorder, the
  tombstones, the act and the relay's exception, and each later case needs another rule.
- **One licence per name, served by every entry whose keys verify it**: simpler still, but a copied
  key in another source's build would be served the licence. The entry keeps the choice explicit.
- **`?source=` always required**: this breaks a name's only-entry case for every existing caller
  for no safety gain, because the default acts only when the answer is unambiguous.
- **Counting a held entry's keys** so a licence lands before the hold passes: the hold is the
  window for revoking a stolen publisher key, so the reconciler waits it out instead.
- **Enforcing the public rule now**: the owner deferred it. The warning records the rule without
  deciding its shape.

## Changes to earlier specs

- **0033** (phase B part 2, superseded where it concerns licences):
  - the Summary's "its source (the source that owns the key …)" becomes "its catalogue entry";
  - in the Design's **Licences**, the key's owner and the relay's owner rule go; licences are per
    entry, with live keys per entry as written;
  - in **The API and the CLI**, the owner route and the events `license.key_conflict` and
    `license.key_owner` go;
  - in the **Data model**, `license_key_owners` goes;
  - in **Existing data**, the (B) key owners bullet and the last bullet go;
  - the **Security** licences bullet changes;
  - in **Testing**, the key owners go;
  - **As built (B part 2)** keeps its history with a note pointing here: its items on key owners,
    distributors and upstreams, ownership changes, the relay's exceptions, `unknown`, the `owner`
    field and the migration's owners;
  - in **Alternatives**, "a key's owner by precedence" stands as history.

  The implementing PR adds a "superseded by spec 0035" line at the head of the Licences section
  and of As built (B part 2).
- **0012, 0021**: 0033's amendment "licences per (tenant, name, source); key owners per (name,
  key)" now reads "licences per catalogue entry; live keys per entry".
- **0014**: a bundle's licence goes into each bundle-mode upstream of its source that supplies the
  name and verifies it.
- **0028**: unchanged (the expected holder on every path).
- **0029**: a licence manager imports into a named entry (`?source=`) and reads
  `license-keys?source=`.
- **The website, in the implementing PRs**:
  - `licensing.md`: "A key's owner" is replaced by "A licence's entry", and "Licences in a tenant",
    the relay, bundles, the console and events change; phase 2 adds "Public builds of licensed
    extensions";
  - `api.md`;
  - `administration/commands.md`;
  - `administration/upgrading.md`: migration 0043 and its `min_reader`; the B part 2 section points
    here;
  - `observability.md`: the events;
  - `publishing.md`: the distributor's licences;
  - `upstreams.md`: a removed upstream's keys;
  - `access.md`: the licence manager;
  - `console.md`, if it describes Licences.

## Follow-ups

- **Enforcing the public rule**, for example a distributor's publisher marked "not public" that
  refuses public releases of its entries and public licences of them, or a tenant setting that
  allows it.
- **Copying a licence from one entry to another** as an administrator's act, if needed. Today the
  licence is imported again.
- **The catalogue's reads saying whether an entry is licensed** (spec 0033's follow-up). Phase 2's
  check can feed it.
