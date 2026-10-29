# Spec 0035: Licences per catalogue entry

- **Status**: implemented (phase 1 2026-10-26, phase 2 2026-10-26)
- **Changed by** [0037](../0037-distributor-in-a-tenant/spec.md) (PR 1): "Who writes which entry": tenant administrators write every entry but distributors' publishers', which licence managers and server administrators write; the rule `older` applies to an import, the relay and bundles.
- **Date**: 2026-10-26
- **Author**: vgsml, Claude

## Summary

A licence belongs to a **catalogue entry**: a source's builds of a name in a tenant (spec 0033).
Every licence write names the entry (`?source=`). An import is accepted only if the licence
verifies against the keys that entry's own builds declare. A relay stores the licence into its own
upstream's entry. A channel serves the licence of the entry it serves the name from.

This replaces spec 0033 phase B part 2's **key owners**: a key's first recorder, tombstones of
removed owners, the rule that a distributor's records do not count for names an upstream lists, the
owner route and command, and the exceptions they needed. A copy of a key under another source is
another entry, with no licence until one is put there.

A small, advisory rule comes with it (phase 2). kista **warns**, without refusing, when an entry
that holds a licence is served publicly. Enforcement is a follow-up.

## Problem

Spec 0033 phase B part 2 bound a licence to the source that **owns** its key for the name: the first
source whose counted build recorded the key. The purpose was to stop a build carrying a copied
public key from getting a licence or taking one away. Computing and keeping that owner took:

- a first recorder that accounts for holds, revocations, blocks and withdrawals, with ties broken by
  the source string, computed again in SQL in three dialects by migration 0042;
- the owner written when it is first needed (the first record, import or relay that reads it), with
  insert-if-absent races;
- tombstones for removed owners, updated by compare-and-set;
- a live rule that a distributor's publisher does not count for a name a `repository` upstream lists;
- an administrator's act that moves licences, with its own `409`s;
- licences of source `unknown`;
- and, in the relay and in bundles, an exception for a licence that is byte for byte the owner's,
  plus re-binding and adoption of `unknown` rows.

Most of `internal/store/licenseowners.go` and a hard documentation section exist only for this. It
also gives wrong answers:

- **A pull-through upstream blocks a distributor's licences.** A tenant with a `repository`
  upstream allowing `*` (another kista, pulled through) "lists" every name, so a distributor's
  publisher never owns a key there. Importing Enterest's licence for `geo_pro` fails even when every
  channel serves `geo_pro` from Enterest. A tenant administrator then has to run `license owner` per
  key and name, and again after every key rotation.
- **Two upstreams of one product depend on order.** A tenant that mirrors a remote's `prod` and
  `nightly` through two upstreams gets the licence on the second only when it is byte for byte the
  one the owner already holds. A renewal reaching the second upstream first is refused until the
  owner's relay has it.
- **History decides, not the administrator.** Where a licence lands depends on which source recorded
  the key first. After an owner is removed, the key has no owner until a fresh record or an act
  gives it one. A second upstream already bringing the same builds gets no licence until a new
  version arrives or an administrator acts.

The owner's decision (2026-10-26): a licence belongs to a catalogue entry, a write names it, and no
other source's records matter. A copied key is simply another entry, with no licence.

## Design

### The model

- **A licence is per entry**: (tenant, package type, source, name). Each entry holds at most one.
  For example, one tenant may hold two `geo_pro` entries: `publisher:enterest` with a licence that
  Enterest imports, and `upstream:x` with none, or with its own licence relayed from `x`. As before,
  a licence names its holder and its extension (`ext`). The holder is checked against the tenant's
  expected holder **when one is set** (spec 0028).
- **The live keys of an entry**, one rule used by import, relay, bundles, serving and the keys
  listing. They are the keys the entry's source recorded (spec 0021: a marked publisher's
  publication, or a `repository` upstream's fetch or bundle) on **its own** builds of the name
  (`b.source` equals the recorder), while the source exists, from either:
  - a **live build**: an active or deprecated release in a signed channel whose rule serves this
    source for the name, its body not blocked and not withdrawn. A held build placed early (spec
    0033 A2) is live: this path does not read `available_at`; or
  - the source's **newest available version** of the name: not held, withdrawn or revoked, its body
    not blocked, by spec 0006's version order.

  This is phase B part 2's `LiveLicenseKeys`, unchanged. Keys in `licenses.refuse_keys` count
  nowhere.
  - A record that one source made on another source's build counts for neither. Today the keys
    listing (`tenantLicenseKeys`) and serving (`BuildKids`) accept such a record, and both are
    narrowed to own-source records.
  - `tenant` and DuckDB's core and community upstreams record no keys, so their entries never carry
    a licence.
  - Passthrough channels serve no licences, as today.
- **No other entry matters.** Importing, relaying or serving a licence reads only its own entry's
  keys and licence.

### Naming an entry

Every licence write (import, `DELETE`, `…/public`, `…/private`) **requires** `?source=`, and the
CLI's `-source`. Without it the answer is `400`; the CLI refuses before any call. On reads
(`GET …/licenses[/{name}]`, `license list|show|token`), `?source=` stays an optional filter: a
read of one name without it acts on the name's only licence, and answers `409` when there are
several.

The forms are `upstream:<name>`, `publisher:<name>`, `upstream-id:<id>` and `publisher-id:<id>`.
On import and visibility changes, the form must resolve to a live upstream or publisher of the
tenant, otherwise the answer is `400` naming why:

- unknown or malformed;
- removed;
- `tenant`, or a DuckDB core or community upstream, which never carry a licence because they record
  no keys.

`DELETE` also accepts the id form of a removed source whose entry still holds a licence. Without
that, such a licence could only expire.

### Import

`POST …/licenses?source=<source>`. The checks are unchanged except for the keys they read:

- the licence is in force;
- it verifies against **that entry's** live keys (`kid_untrusted`, naming the source);
- its key is not refused;
- it names the expected holder when one is set (`holder_mismatch`).

It is stored in the entry as an import, and replaces the entry's licence only with `If-Match`.
`Location` carries `?source=`.

**Who writes which entry.**

- **Tenant administrators and server administrators**: any entry of the tenant.
- **Licence managers** (spec 0029, `auth.server_license_admins`): only the entries of
  **distributors' publishers** (publishers with the `distributor` mark, spec 0033 phase B), for
  every write: import, `DELETE`, `…/private`. Any other entry answers `403`. They keep reading
  every entry and every key. (As built: they name only those entries, and any other source answers
  as a missing path does, `404`, audited: see As built (phase 1).)
- **Distribution admins**: still no licence routes.

### Relay and bundles

- **Relay.** An upstream's relay stores each licence into **its own entry** (`upstream:<id>`) when
  all of these hold:
  - the upstream supplies the name (`not_supplied`, unchanged);
  - the licence verifies against that entry's live keys (`kid_untrusted`; an eager upstream's
    builds still held make no key live, so the licence is refused and stored at a later run);
  - it names the expected holder when one is set.

  Spec 0012's and 0021's replacement rules stay:
  - a newer licence from the same upstream, or one that lasts longer, replaces the stored one;
  - an import in the entry is replaced only by a relayed licence signed with the same key that
    lasts longer, or once the import's grace has ended or its key is dead. The row is then the
    relay's, and the relay governs it.

  Each entry gets the organisation's licence through its own relay or an import. Two upstreams of
  one remote (Enterest's `prod` and `nightly` mirrored into a tenant's `prod` and `nightly`) each
  store their own copy. An upstream's relay governs only the relayed licences of its own entry: one
  it stops listing is removed, and an import is never removed by it. A refusal keeps a row the
  relay stored before, as today, except `not_supplied`: a name the upstream no longer supplies is
  one it stops handing out, and its row is removed (as before 0035). Revocation is per entry: a relay's removal never touches another
  entry's copy. The `key_owner` refusal goes.
- **Bundles**, in parity with the relay. A licence a bundle carries is read once, even when two
  sources carry it. It goes into **each** bundle-mode upstream of a carrying source that supplies
  the name and whose live keys verify it, checked as a relayed licence is. A bundle still never
  replaces an import. The import's counts (stored, replaced, unchanged, refused) are per target.
  The export is unchanged: per name, the licence of the entry the files come from.

### Serving and usage

- **A channel serves the licence of the entry it serves the name from**: the rule's source, as the
  channel's snapshot selects it. It does so while the licence's grace lasts and while a live build
  there that the caller sees declares its key (the live-key rule above), as B part 2 built.
  - **Switching the source switches kista's answer** at the next request. An entry without a
    licence answers `404`. The SDK keeps a licence it cached before on a `404`, so the switch
    applies to kista's answers, not to what a running client already holds.
  - A licence is never served through another entry, whatever keys that entry's builds declare.
- **Usage** is unchanged: a channel's counts are forwarded to the source it serves the name from
  (B part 2). A channel serving a copier's entry forwards to that entry's source.

### Removing a source, and its successor

- **Removal.** The source's entries stay (spec 0033) and its keys go (spec 0021), so its entries
  have no live keys. Removing an upstream removes the relayed licences of its entry, as today. An
  import in a removed source's entry stays: listed under the id form, served nowhere, removable,
  and gone at the end of its grace. Nothing passes to another source.
- **The successor.** Specs 0031 and 0033 define it: an upstream of the same kind and prefix, added
  again, which takes the removed one's rules and releases in its home channel by copying their
  builds. It is another entry. **Gap, stated, no new mechanism**: its relay supplies nothing until
  it has fetched builds itself. `SuppliedBy` needs a build created since the upstream was added; the
  copies keep the old arrival time and carry no key records until they are fetched again. Until
  then, its channel serves no licence for the name.
- **Distributors.** Removing a distributor's publisher and adding it again also makes a new entry.
  Its licence is missing until the reconciler imports it again.

### What goes

These go, in phase 1:

- **Store.**
  - the table `license_key_owners` and its index;
  - the first recorder, owner writes, tombstones and the "listed by an upstream" rule;
  - `ValidLicenseOwner` stops accepting `unknown` and moves next to the source labels;
  - `store.LicenseUnknown`;
  - `UpstreamLicenses`' `unknown` clause (`internal/store/licenses.go`);
  - `NameKey.Owner` and `OwnerRecorded`;
  - `RebindLicense`;
  - `RecordLicenseKeys` and `DeleteLicenseKeysOf` lose their owner bookkeeping.
- **Licence service** (`internal/licenses`):
  - `owned()`, `RuleKeyOwner` (`key_owner`), `ErrOwnerHolds`, `SetKeyOwner` and `KeyOwnerView`;
  - the relay's byte-for-byte exception (`relayable`), `rebind` and `adoptUnknown`;
  - `KeyView.Owner`.
- **API and CLI.**
  - `PUT …/license-keys/{name}/{kid}/owner` and `kista admin license owner`;
  - the `owner` field.
- **Events**: `license.key_owner` and `license.key_conflict`, with their registry entries and the
  coverage test (`internal/audit`).
- **Upstreams** (`internal/upstream`):
  - the removal's event reads the licence's entry rather than `l.Owner`, and its "unknown" comment
    goes (`upstream.go`);
  - the export ranks licences by `l.Entry` (`export.go`).

kista has no release yet, so there is no compatibility path for any of them.

### API, CLI, console

- **A licence** answers `entry`: its entry's source (`upstream:<name>`, `publisher:<name>`, or a
  removed source's id form). It replaces `owner`. `source` keeps its meaning, how the licence
  arrived (`import` or `upstream`), and `upstream` names the relaying upstream.
- **Keys.** `GET …/license-keys[/{name}][?source=]` lists exactly what an import's key check would
  accept at that moment: per name and key, `sources`, the entries for which the key is live (`kind`,
  `name`, and `builds`, the number of that entry's builds declaring it). Refused keys are left out.
  - A key with more than one entry in `sources` is declared by several entries. There is no event
    for this; the listing shows it.
  - With `?source=`, one entry's keys: `200` with an empty list when it has no live key, `400` for
    an unknown, removed or malformed source.
  - Being listed means the key check passes. Being in force, the holder and `If-Match` are checked
    separately.
- **CLI.**
  - `license import <t> <file> -source <s> [-replace]`;
  - `license remove|public|private <t> <name> -source <s>`;
  - `license keys <t> [<name>] [-source <s>]`: `NAME KEY SOURCES`, each source with its builds;
  - `license list` and `license show`: `ENTRY` replaces `OWNER`, and `entry:` replaces `owner:`;
  - the usage text in `cmd/kista/admin.go` changes accordingly;
  - `license owner` goes.
- **Console, Licences.**
  - The **Of** column becomes **Entry**.
  - The import dialog reads `ext` and `kid` from the licence, as `peek` does, and lists the
    entries whose live keys include the kid, each with its source kind and, for a distributor's
    publisher, the distributor. It preselects only when exactly one entry lists the kid.
  - With none listed, the user may still choose an entry and send. kista's `400 kid_untrusted` is
    shown.
  - The dialog always sends `?source=`, and replaces that entry's licence after a confirmation.
  - A licence is flagged "no live build of its entry declares its key".
  - The keys table shows each key's entries.

### Data model

Migration `0043_license_entries` runs in the three dialects with `min_reader` 43, because an older
binary reads and writes `license_key_owners` and relies on the relay's exceptions. In order:

1. **Re-keys relayed licences.** For `source = 'upstream'`:
   - delete the rows that would collide on the primary key: keep the row already in
     `upstream:<upstream_id>`; of several rows moving to one empty entry, keep the one that expires
     last, then by licence id;
   - then `UPDATE licenses SET owner_source = 'upstream:' || upstream_id` (SQL Server `+`).

   This keeps bundle-imported licences (bundle-mode upstreams never relay), the rows of paused or
   credential-less upstreams, refused-but-kept rows and their visibility, with no window where they
   are `missing`.
2. **Deletes the remaining licences of source `unknown`** (imports whose key had no owner, served
   nowhere already). The upgrade notes say to export them first.
3. **Requests a relay run** for every relaying upstream: a `license_syncs` row with `requested_at`
   set, inserted where none exists.
4. **Renames** `licenses.owner_source` to `entry_source`, still in the primary key (tenant, type,
   name, entry_source). PostgreSQL and SQLite use `RENAME COLUMN`, SQL Server `sp_rename`. The
   store's `License.Owner` becomes `Entry`. Imports keep their value as their entry.
5. **Drops** `license_key_owners` and `ix_build_license_keys_kid`, which only the first recorder and
   the count behind `license.key_conflict` read (SQL Server `DROP INDEX … ON build_license_keys`).

Tenant deletion and the store test harness drop the table from their lists.

### Phase 2: public serving of licensed entries, advisory

An organisation may mirror an extension with its licence into its own kista and serve it inside the
organisation as it likes. It must not serve it publicly with its licence unless the distributor
allows it. The distributor's public terms say so. kista only **warns**: the warning is advisory and
not complete. A broad `issuer:` grant, or lifting a block, can expose a build without passing any
hook below. Enforcement is a follow-up.

**Visibility in kista.** Visibility belongs to a release; there is no public channel. A release is
public when an administrator makes it public, or when it is placed, promoted or published from an
entry whose visibility is public, unless `private` is asked. A licence's own visibility is separate:
a public licence goes, without a token, to whoever is served a public release of the name from its
entry (spec 0012), and only an administrator may make one public (spec 0029).

**The trigger** is an entry that holds a licence and is served publicly: it has an active or
deprecated public release in a signed channel whose rule selects it. kista warns when either:

- a release of an entry that holds a licence becomes such a public release (written public, made
  public, taken by a successor, or selected by a source switch); or
- a licence is stored, or made public, in an entry that already has such a public release.

Declaring keys alone does not trigger it.

**Hooks**, all in the act's transaction:

- the release writes: `InsertRelease`, `SetReleaseVisibility`, and `TakeReleases`' raw update;
- a licence's import, relay or bundle store, and its `…/public`, in an entry with public
  releases;
- a `channel_sources` switch that selects such an entry.

**What kista does.** The hooks note what the act's transaction touched. The warnings are written
right after the act's commit, best effort: a failure is logged, and never refuses or rolls back the
act. kista writes **one** `license.public_warning` event per (act, source, name, channel), with
fields `name`, `source`, `package_type`, `channel` and `act`, however many platforms or releases the
act touched (under concurrency, a rare duplicate event is accepted). Routes return the collected
warnings as one generic `warnings` array: `[{"name", "source", "channel", "message"}]`, omitted when
empty, to a caller who administers the path. The CLI prints them on stderr and exits 0. The console
shows them in the act's result. Acts that answer nobody (a publication, an automatic placement) or
another caller (a publisher, a promote-only principal, a licence manager) write only the event.
Served publicly excludes a withdrawn build's release and a blocked body's. A warning is given at the
act that changes it; a no-op replay answers none. Nothing is refused.

### Enterest

Enterest's reconciler is a licence manager, so it writes only its distributor's publishers'
entries. Its kista client must change **before** kista 0035 is deployed; today's kista ignores
`?source=` on import, so the order is safe:

- `internal/kista/kista.go` (`ImportLicence`, `LicencePrivate`) sends `?source=publisher-id:<id>`
  (or its publisher's name) on every licence write;
- `internal/worker/licences.go` keys held licences by (name, entry), not by name.

It may read `GET …/license-keys/{name}?source=…` to see whether a licence's key is live for its
entry. How and when it tries again is Enterest's decision.

## Security

- **A copied public key** (an upstream's or a tenant publisher's build declaring Enterest's key) is
  another entry. It gets no licence unless one is put there: by its own upstream's relay of a
  licence that verifies against its keys, or by a tenant or server administrator's import. A licence
  cannot be forged with a public key, and the copy takes nothing from the genuine entry.
  - **The organisation's purchase.** A tenant administrator may import the purchase into a
    copier's entry. The channel serving that entry then forwards its usage to the copier's source,
    and revoking the genuine entry's licence does not reach the copy: revocation is per entry.
  - **The holder check** protects only when an expected holder is set. Set one when relaying from
    upstreams you do not fully trust.
  - Several entries declaring one kid are visible in the keys listing and in the import dialog,
    which never preselects among them.
- **A licence manager** writes only distributors' publishers' entries. Otherwise its server-wide
  token could copy a tenant upstream's licence into its own entry, or, using `If-Match`, pin an
  import into an upstream's entry where the relay's revocation never removes it. As built, it names
  them as distribution admins do, and any other source answers a write as a missing path does,
  before anything about it is checked. It still reads every licence and every key's entries; it
  writes, copies and pins only into distributors' publishers' entries.
- **A distributor taking a tenant's licence.** A distributor's publisher reaches only its own entry,
  and its keys do not touch the upstream's licence. A pull-through `*` upstream and a distributor
  coexist, each entry with its own licence.
- **A removed source's licence** is served nowhere, because its keys are gone. Relayed licences go
  with the upstream. Nothing passes to a successor or to another entry with the same key.
- **Fail closed.**
  - Every write names its entry, and it must resolve to a live source that can carry a licence
    (`400` otherwise).
  - A held (not placed early), revoked, withdrawn or blocked build makes no key live.
  - Serving needs the channel's selected entry and a live own-source build declaring the key.
- **Existence** is unchanged. The licence routes are for administrators and licence managers (a
  licence manager's refused entry is a missing path, `404`, recorded as `authz.refused`), and the
  DuckDB route answers as a missing path for a caller who may not have the licence.
- **The warning** changes no decision, and is written after the act commits: its failure cannot
  refuse or undo the act.
  - Its answer goes only to a caller who administers the path (a server administrator; a tenant,
    channel or extension administrator) and the CLI. A publisher, a promote-only principal, a
    licence manager and a distribution admin are not told which channels serve an entry publicly.
  - The event names the channels for the tenant's auditors, whoever caused it.

## Testing

**Store suite**, on all three dialects (`make test-db`, then `make test` with the dialect variables):

- **Migration 0043**, on a populated level-42 database:
  - the owners table and its index are gone;
  - relayed and bundle rows are re-keyed to their upstream's entry, a collision is dropped, and
    visibility is kept;
  - `unknown` imports are deleted and other imports kept;
  - relay runs are requested;
  - a level-42 binary is refused (`min_reader` 43).
- **Live keys per entry**: own builds only (a record on another source's build counts for
  neither); held; placed early (live); revoked; withdrawn; blocked; removed source.
- **Tenant deletion** without the table.

**The licence service** (`TestLicenseSources` rewritten; `internal/upstream`, `internal/tenants`):

- Import:
  - with `?source=` into a distributor's entry;
  - without `?source=` (`400`);
  - into an entry that does not declare the key (`kid_untrusted`);
  - into `tenant` or a removed source (`400`).
- **The listing matches import acceptance**: a held build's key is not listed and `kid_untrusted`;
  placed early, it is listed; available, it is listed and accepted.
- A copied key under another source gets nothing, and the genuine entry is unaffected.
- **The regression case**: a pull-through `*` repository upstream and a distributor's publisher of
  one name; the distributor's licence imports without any act.
- Relay:
  - into the upstream's own entry, beside a distributor's licence for the same key;
  - into a held upstream entry: refused, then stored at a later run;
  - two upstreams of one remote, each storing its copy, a renewal reaching either first, each
    revoking only its own;
  - an import replaced by a longer-lasting relayed licence of the same key, then governed by the
    relay.
- Removal and successor:
  - an upstream's removal removes its relayed licences only;
  - a removed publisher's import is kept and served nowhere, and deletable by its id form;
  - a successor gets nothing from the old entry.
- Bundles go into each supplying, verifying upstream, with counts per target.
- A licence manager is refused (`403`; as built `404`, a missing path) on a non-distributor entry.
- The tests that use `store.LicenseUnknown` change: `internal/tenants/holder_test.go`,
  `internal/store/holder_test.go` and `internal/api/licenses_test.go`.

**Serving:**

- One name from two entries that declare the **same** key: the licensed entry's channel serves it.
- Switching the other channel's rule to the licensed entry serves it there.
- Switching back answers `404` (`missing`).

**The API and the CLI:**

- `?source=` is missing on import, `DELETE`, `…/public` or `…/private`: `400`. The CLI refuses
  before any call.
- `license-keys?source=`: `200` empty, `400`, per-entry `builds`, several entries for one kid.
- The `entry` field.
- The owner route is gone.
- The events registry and `internal/audit/coverage_test.go`.

**Phase 2:**

- A warning and **one** event per act across N platforms, for:
  - a release made public;
  - a placement;
  - a promotion;
  - a successor taking releases;
  - a source switch;
  - a licence import, relay and bundle;
  - a licence made public.
- Negatives: a private release of a licensed entry warns nothing, and neither does a public release
  of an unlicensed entry.
- The CLI writes to stderr and exits 0.
- A console unit test.

**Console:**

- Unit tests: the dialog's entry choice (never preselected among several), the `?source=` sent, and
  the flag.
- `make console-e2e` with `licenses.spec.ts`: the dialog's entry choice, the `?source=` sent, the
  columns, two entries of one name, and the case with no entry (sent, kista's `400 kid_untrusted`
  shown).

**DuckDB end to end at the pin** (`make e2e`):

- **`TestLicenseFromDistributor`**, adapted.
  - The distributor's licence is imported with `?source=` before placement and served from `prod`.
  - The tenant's upstream brings `kista_demo` with the same key into `nightly`, which serves no
    licence (`missing`): **two entries**, one licensed.
  - Switching `nightly`'s rule to the distributor's entry serves its licence there (`valid`).
  - Phase 2 adds making `prod`'s release public, which answers the warning and writes the event.
- **`TestSDKLicenseRelay`**, extended: two upstreams of the remote, in two channels, each relay into
  its entry, and DuckDB is licensed from both.
- **`TestSDKLicense`** (`sdk_test.go`), and the imports at the source kista of
  `TestSDKLicenseRelay` (`relay_test.go`) and `TestSDKLicenseBundle` (`licensebundle_test.go`):
  their `Import` calls gain the source.

## Delivery

Two PRs. Each runs `make test` (on all three databases), `make lint`, `make console-test`,
`make console-e2e` and `make e2e` before it opens.

- **Phase 1, `0035-licences-per-entry`: the model.** It touches:
  - **Store**: `licenseowners.go` goes, with `LiveLicenseKeys`, `SuppliedBy` and
    `TenantLicenseKeys` moving to `licensekeys.go`; `licenses.go`; `BuildKids` in `licensekeys.go`;
    migration 0043 in three dialects; `tenantdelete.go`; `storetest`.
  - **Packages**: `internal/licenses` (import, relay, bundle, the role check), `internal/upstream`
    (removal, export), `internal/tenants` (tests), `internal/serve`, `internal/api` (route removed,
    `?source=` on writes), `internal/audit`.
  - **CLI**: `cmd/kista` (`admin.go`, `admin_license.go`).
  - **Console**: the Licences screen, its tests and `licenses.spec.ts`.
  - **The e2e tests above, and the website pages below.**

  It mostly removes code.
- **Phase 2, `0035-licences-per-entry-warning`: the warning.** It touches the hooks, the collection
  and the event, the routes' `warnings`, the CLI and console display, the tests above, and its
  docs.

### As built (phase 1)

Delivered on the branch `0035-licences-p1` (the Delivery's `0035-licences-per-entry` is the spec's
own branch), in commits of their own after three review passes.

- **Store.** `licenseowners.go` and its test are gone; `LiveLicenseKeys`, `SuppliedBy` and
  `TenantLicenseKeys` live in `licensekeys.go`. `ValidLicenseOwner` is `ValidLicenseEntry` (it no
  longer accepts `unknown`), next to the source labels in `sources.go`, with `LicenseEntrySource`,
  which reads whether a source is a live upstream or publisher of the tenant (and an upstream's
  kind) for the writes' `?source=`. `TenantLicenseKeys` takes the entry (`""`: every entry) and
  answers each key with its entries, each with its own `builds`; `BuildKids` and the listing keep a
  record only when it is the build's own source's (filtered in Go: the dialects share no
  concatenation).
- **Migration 0043** (after review). A relayed row in its relayer's entry stays relayed. A relayed
  row in another **relaying** upstream's own entry (a `repository` upstream with a credential, not in
  bundle mode, paused or not) is deleted: that upstream's relay is requested and stores its own copy
  there, which it governs, where an import would never be revoked by it (until that run, the entry's
  channels serve no licence for the name). A relayed row in any other entry (a publisher's, a
  bundle-mode or credential-less upstream's: migration 0042's owner, or an administrator's act, put
  it there), or whose relayer is gone or not recorded, stays in that entry and becomes an
  **import** (`source` `import`, no `upstream_id`), its visibility kept: the channels
  serving it keep serving it, and the relayer's own entry is filled by its next run (requested). A
  relayed row of source `unknown` (served nowhere) moves into its live relayer's entry unless a row
  is there, else it goes with the other `unknown` licences. There are no ties to break: a source
  holds one licence of a name, so at most one `unknown` row moves into a given entry, and the only
  collision is with a row already there, which stays. A relaying upstream is a `repository`
  upstream with a credential, not in bundle mode (a paused one included: its run waits until it is
  resumed); its existing `license_syncs` row gets `requested_at` too. Its test runs on a populated
  level-42 database on the three dialects (the conversions in place, scenarios of 0042's owner, an
  administrator's act and a removed relayer, rows in a relaying upstream's entry deleted and in a
  bundle-mode upstream's converted, the moves and deletions, the paused upstream's
  request, and a level-42 binary refusing the database), and `TestMigrationPackageTypes` (level 38)
  gives its `unknown` licence an entry before 0043.
- **Naming an entry.** The writes resolve `?source=` in the service: `""`, `tenant`, a malformed
  form, a name that is no upstream or publisher of the tenant, a removed source's id form (except
  on `DELETE`), and a DuckDB core or community upstream are `400` (`invalid`, the detail starting
  `?source=`); `?source=` given twice is `400`, as a repeated `?type=` is. `DELETE` with a removed
  source's id form whose entry holds no licence is `400`; a live entry without one is `404`. The
  order is the caller, licensing (`409` while off, on an import), the entry, the licence. The import
  and `…/public|private` read the entry again in their transaction, under the licence lock
  (`Tx.LicenseEntrySource`): a source removed meanwhile is `400`. `GET …/license-keys?source=`
  resolves as the writes do, so `tenant` and a DuckDB upstream are `400` there too.
- **Licence managers: a missing path, not `403`** (after review). A licence manager names entries
  as distribution admins name distributors' publishers (`DistributorSources`: a live one by
  `publisher:<name>` or `publisher-id:<id>`, a removed one by its id form on `DELETE` only). Any
  other `?source=`, on a write or on `license-keys`, answers `authz.ErrDenied`, which the API
  answers as a missing path (`404`) and records as `authz.refused`, as every refused caller is,
  before anything about the source is checked: unknown and non-distributor sources answer alike.
  The write's transaction checks again that the entry is a distributor's publisher's of the tenant
  (`SourceDistributor`, now scoped by `tenant_id`). No `forbidden` problem type is added. Reads stay
  open to it, as the spec says.
- **Bundles carry only licences whose key is live for their entry.** `Exportable` keeps a licence
  only while its key is live for its entry, so a removed source's licence does not travel in a
  bundle.
- **Events.** A relay replacing an administrator's import writes `license.change` (`replaced`, the
  import's id; `upstream`, the relaying upstream; `source`, the entry), as the import was told per
  licence. A bundle's import lists each target's counts in its result (`targets`, by upstream name),
  carried in `bundle.import` and the import's record; it writes no per-licence event, as the relay
  writes none for what it stores.
- **Keys listing.** Each entry in `sources` answers `kind`, `name`, `distributor` (a distributor's
  publisher only) and `builds`; the key's top-level `builds` and `owner` are gone. The CLI prints
  `NAME KEY SOURCES`, each source as `kind:name (builds N[, distributor D])`.
- **The relay into a held upstream entry** is refused `not_supplied`, not `kid_untrusted`: a held
  build supplies nothing (`SuppliedBy`, unchanged), and it makes no key live either. It is stored at
  the run after the hold ends, as the spec says. A refusal for `not_supplied` does not keep a row
  this relay stored before (as before 0035).
- **Bundles.** `received` counts each licence once; stored, replaced, unchanged and refused count
  per target, the holder's refusal included (checked per target, as the relay checks it), and
  `targets` gives each target's own. An upstream named twice among a licence's targets is taken
  once.
- **Reads.** A read of a name with licences in several entries answers `409` naming them sorted
  (`several entries: …`). The events keep the field name `source` for the licence's entry, as B part
  2 named it.
- **Console.** The import dialog lists the entries as radio buttons (a radiogroup labelled by its
  visible heading), each with its kind (`upstream`, `publisher`, or `<distributor>'s publisher (a
  distributor)`), and offers "another entry" with a text field; with none listed, a field labelled
  "Into the entry". The entry is computed at render (`importEntry`): the chosen one while it is
  listed (a choice no longer listed is none: the administrator chooses again); with no choice yet,
  the only one listed, never one among several. A typed entry must be the name
  form (`upstream:<name>`, `publisher:<name>`), as the list shows live entries, so the licence it
  replaces is found and `If-Match` sent (an id form is refused in the dialog with a hint; an import
  never targets a removed source). While the keys load or fail, the dialog says so instead of "none
  listed". The confirmations name the entry. `Import` takes its client as props and has component
  tests (what `?source=` and `If-Match` it sends). The console end to end gains a second
  distributor's publisher, `vault-dist`, publishing vault 1.1 with the same key into the catalogue
  (two entries of one name), and a licence for tresor whose key no entry lists; it covers the
  choice, the `?source=` sent, the columns, a licence in each of the two entries and which one
  stays after a removal, and `400 kid_untrusted` shown for an entry declaring no key, listed or
  not.
- **Serving** is tested in `internal/serve` (`TestLicenseEntrySwitch`) within one channel whose rule
  is switched between two entries declaring the same key (the unlicensed entry answering exactly as
  a missing path), and in `TestLicenseFromDistributor` across channels (including switching
  `nightly` back, `404`).
- **Earlier specs.** 0033 and 0034 carry the notes listed above; 0014 and 0029 are left as they
  were, this spec recording their change, as earlier specs' changes were recorded.

### As built (phase 2)

Delivered on the branch `0035-licences-p2` (the Delivery's `0035-licences-per-entry-warning`), after
phase 1 merged, and revised after three review passes.

- **Hooks, then a check after the commit.** The hooks only note what they touched:
  `InsertRelease` a release written public, `SetReleaseVisibility` a private release made public
  (not one public already), `TakeReleases` the (channel, name, successor) it moved releases to,
  `SetChannelRule` a rule whose source is new or changed, `PutLicense` a licence stored (an insert
  or a replacement, whatever its visibility: an import, a relay, a bundle) and
  `SetLicenseVisibility` one made public. **Right after the act's transaction commits**, and only
  when it noted something, a read outside any transaction finds which of them are entries holding
  a licence with an active or deprecated public release in a signed channel whose rule selects
  them, their build not withdrawn and their body not blocked. Only when it finds one the act has
  not written yet does one attempt of a short transaction write the events, so a transaction with
  nothing to warn about takes no second write lock (SQLite); another transaction of the act writing
  the same key between the read and the write may rarely make a duplicate. It is **best effort**,
  bounded to 2 seconds (`context.WithoutCancel`, then a timeout): a failure (a lock timeout, the
  database gone, the bound) is logged at warn, and a panic recovered and logged at error
  (`Store.Log`: the server's log, `kista admin`'s stderr); either is dropped, and never refuses,
  retries or rolls back the act, nor turns it into a failed answer. The noted releases are read by id, the rules by (channel, name,
  source), the entries by tenant and type through the tenant's channels (`c.tenant_id`), each kind
  batched into one query (a few hundred notes per query). On SQLite the entries' query reads
  `channels(tenant_id)` then the channel's releases, the rules' the releases' slot index (`channel_id,
  source, name`); PostgreSQL (empty tables) takes `channels(tenant_id, name)`, `channel_sources`' key
  and the slot index.
- **The act.** One collector per act (`store.WithLicenseWarnings` in the context): each management
  write of the API, each `kista admin` command, an upstream's run, a bundle's import, and the
  placement pass's look at a rule or a follow (each placing builds in transactions of their own;
  set in `placeRule`, which the pass calls). Across the act's transactions a (tenant, source, name,
  channel) whose event was written is not written again; two transactions of one act checking at
  the same moment may each write one, a rare duplicate accepted for an advisory event. Without a
  collector, once per transaction. The `act` field is the kind the service names
  (`Tx.SetWarningAct`: `release.add` or `release.publish`, `release.place` for a placement by hand or
  by a rule or a follow, `release.promote`, `upstream.release` for a mirror's arrival and a
  successor's, `release.public`, `source.set`, `license.import`, `license.change`, `license.sync`,
  `bundle.import`), else the first hook's default (`release.add`, `release.public`,
  `upstream.release`, `source.set`, `license.import` or `license.sync`, `license.change`). The
  actor is the first hook's (the act's actor), else `system:kista`. The subject is
  `channel:<channel>/ext:<name>`; `source` is the entry as answers name it; `package_type` is left
  out for the default type, as in the other events.
- **A replay warns nothing.** `release.Service.Apply` making a release public that is public already
  changes nothing: no version, no event, no warning. Making a private one private again keeps its
  old path (version, `release.private`, its build narrowed): only public needs the no-op. A relay's
  unchanged licence writes nothing either.
- **Who is answered.** Every management write collects, but its answer carries `warnings` only for
  a caller who administers the path (a server administrator, or a principal with `admin` on the
  path's tenant, channel or extension) and is no publisher (signed request), licence manager or
  distribution admin; the CLI is a server administrator. A promote-only principal, a publisher's
  signed promotion and a licence manager's import write only the events. The caller's rights are
  read only when the act has a warning to answer, not on every write. `reply` adds the member
  last in an object answer, after its ETag is taken, so status and ETag are unchanged; an answer
  with a top-level `warnings` member of its own keeps it; error answers carry none. Each warning is
  `{"name", "source", "channel", "message"}`.
- **The CLI** prints each warning after the command, `kista admin: warning: <tenant>: <message>` on
  stderr, also when the command then fails; the exit status is the command's.
- **The console.** The API client hands each change's `warnings` to `onWarnings` (`[]` for none,
  and for a failed change before its error is thrown, refused or never reaching kista); the
  standalone console and the
  micro-frontend show the last change's in a `status` region that is always there (empty without
  warnings), "Done, with a warning. Nothing was refused.", until dismissed (the focus then goes to
  the page's `main h1`) or the next change. The console end to end publishes vault-ci's vault
  publicly, so the import into its entry shows the warning, and the import into vault-dist's
  (catalogue only) does not.
- **Tests.**
  - Store suite, three dialects (`TestLicensePublicWarnings`): each hook and its negatives, a
    replay, once per act across transactions, the act named by the service and by the hook's
    default, a withdrawn build's and a blocked body's release not counted, and a failed and a
    panicking check logged while the act stands (`SetLicenseWarningFault`, for tests). In
    `internal/upstream`, a release made public again through `Apply` warns nothing and keeps its
    version; one made private again is still told.
  - `internal/upstream`, three dialects (`TestLicensePublicWarning`): one warning and one event per
    act for a release made public, a placement and a promotion of two platforms, a source switch, a
    licence made public and replaced, an auto rule's and a follow's placements of two platforms by
    the placement pass, a relay and a successor's run; none for a private placement, a mirror's run
    bringing a public release of an unlicensed entry, a licence into an entry not served or made
    private. `TestBundleLicensePublicWarning`: a bundle import into a two-platform target, once for
    the licence stored with the releases and once for a later version of both platforms.
    `TestBundleLicenses`: the bundle's licence into a public entry warns by `bundle.import`, the
    private target not at all.
  - API: `TestLicenseWarningAnswers` (a licence manager's import, a promote-only principal's and a
    publisher's signed promotion answer none, an administrator's placement and `…/public` answer
    theirs, each writing its events), `TestWithWarnings` (the member added last, an answer's own
    kept, nothing for a caller who does not see them), `TestLicenseRoutes`.
  - The events registry and `TestCoverage`; the CLI (`TestAdminLicense`: stderr with the tenant,
    exit 0); console unit tests (`warnings.test.tsx`: the client, the region, the focus) and
    `licenses.spec.ts`; `TestLicenseFromDistributor` at the pin (prod's release made public over the
    API: the warning answered, one event, the licence still served).
- **Not hooked**, as the spec says: a broad grant, a block lifted.
## Alternatives considered

- **Keep key owners and patch the `*` case** (`*` not counting as listing a name). It fixes the
  pull-through tenant but keeps the history-dependent first recorder, the tombstones, the act and
  the relay's exception, and each later case needs another rule.
- **One licence per name, served by every entry whose keys verify it.** Simpler still, but a copied
  key in another source's build would be served the licence; the entry keeps the choice explicit.
- **A default entry for an import without `?source=`.** It is an inference from the key; the entry
  is the caller's choice.
- **Deleting relayed licences in the migration and waiting for the next relay.** It loses
  bundle-imported licences (bundle-mode upstreams never relay), the rows of paused or
  credential-less upstreams, refused-but-kept rows and public visibility, and leaves a `missing`
  window of up to a relay interval.
- **Letting licence managers write every entry.** That is wider than Enterest needs, and it lets a
  server-wide token defeat an upstream's revocation.
- **Counting a held entry's keys**, so a licence lands before the hold passes. The hold is the
  window for revoking a stolen publisher key.
- **Enforcing the public rule now.** The owner deferred it.

## Changes to earlier specs

- **0033**, superseded where it concerns licences:
  - the **Summary**'s "its source (the source that owns the key …)" becomes "its catalogue entry";
  - the Design's **Licences**: key owners and the relay's owner rule go, and licences are per entry
    with live keys per entry;
  - **The API and the CLI**: the owner route, `license.key_conflict` and `license.key_owner` go;
  - the **Data model**: `license_key_owners` goes;
  - **Existing data**: the (B) owners bullet and the last bullet go;
  - **Security**: the licences bullet changes;
  - **Testing**: the key owners go;
  - **Delivery**: B's licence part points here;
  - **As built (B part 2)**: kept as history with a note. This covers the key owners, distributors
    and upstreams, ownership changes, the relay's and bundles' exceptions (including 0014's bundle
    target, "a bundle's licence goes to the first bundle-mode upstream … that owns the key", around
    L1030), `unknown`, the `owner` field and the migration's owners;
  - **Changes to earlier specs**: its 0012/0021 bullet ("licences per (tenant, name, source); key
    owners per (name, key)") becomes "licences per catalogue entry; live keys per entry; key owners
    removed by spec 0035";
  - **Alternatives**: "a key's owner by precedence" stays as history.

  The implementing PR adds a "superseded by spec 0035" line at the head of 0033's Licences section
  and of As built (B part 2).
- **0014**: a bundle's licence goes into each bundle-mode upstream of a carrying source that
  supplies the name and verifies it.
- **0029**: licence managers write only distributors' publishers' entries, and name the entry on
  every write.
- **0028**: unchanged.
- **0034**: L26, L58 and L434 mention key owners. They stay as history, and L58 gains "(key owners
  removed by spec 0035)".
- **The website, phase 1**:
  - `licensing.md`:
    - "The keys a licence verifies against" (L34-70): live keys per entry, own builds, early
      placement;
    - "A key's owner" (L71-110) is replaced by "A licence's entry";
    - "Licences in a tenant", the relay and bundles;
    - "What extensions read" (around L278-282);
    - the console and Events (L477-479);
  - `api.md` (L380-422);
  - `administration/commands.md` (L479-481);
  - `administration/upgrading.md`: migration 0043 and `min_reader` 43, exporting `unknown`
    licences first, relayed rows re-keyed and relays requested, and the B part 2 section pointing
    here;
  - `observability.md` (L93-95);
  - `concepts.md` (around L214);
  - `deployment/air-gapped.md` (L239, L244-245);
  - `upstreams.md` (L190-191, L913-928);
  - `publishing.md` (the distributor's licences, around L397-402);
  - `access.md`: the licence manager's entries;
  - `console.md` (L161).
- **The website, phase 2**: `licensing.md` ("Public serving of licensed entries"), `api.md`
  (`warnings`), `observability.md` (`license.public_warning`), `administration/commands.md`
  (stderr warnings) and `console.md`.

## Follow-ups

- **Enforcing the public rule**: for example, a distributor's publisher marked "not public" that
  refuses public releases of its entries and public licences in them, or a tenant setting that
  allows it.
- **A successor's licence**: its relay supplying again before it has fetched builds itself, if
  needed.
- **Copying a licence between entries** as an administrator's act, if needed. Today it is imported
  again.
- **The catalogue's reads saying whether an entry is licensed** (spec 0033's follow-up). Phase 2's
  check can feed it.
