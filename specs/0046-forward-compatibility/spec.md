# Spec 0046: Forward compatibility: what a kista does with what a newer peer sends, and the compatibility rules

- **Status**: draft
- **Date**: 2026-11-16
- **Author**: vgsml, Claude

## Summary

kista gets its first real release for on-prem installs before Enterest launches. An installed kista
is chained to a peer that will be newer than it: its organisation's tenant in Enterest, or another
kista. Today most of what one kista reads from another is read strictly: one unknown field, kind,
state or format either holds everything behind it or is refused for good, and the document is not
taken again after an upgrade. This spec fixes how a released kista treats what it does not know:
five rules, the same on every surface. A format has a major; within a major unknown members are
ignored; a higher major, or a value the reader does not know, makes **that one item wait**: it is
kept, not applied, shown to the operator, and taken again by itself once the kista is upgraded.
Nothing unverified is ever kept, and nothing unknown is ever applied. The spec also writes down
what may change in a minor release and what needs a major one, for every interface kista has.

The trial tags `v0.1.0` to `v0.1.4` are owed nothing. The rules hold from this release on.

## Problem

Verified in the code at `3ddc6e4`. "Refused for good" means the cursor moves past the statement
and only a manual cursor reset (`website/docs/attachments.md:507-512`) brings it back.

### Where one unknown thing blocks or is lost

1. **A signed statement with one unknown member holds its body for ever.**
   `internal/attachment/statement.go:199-202` decodes with `DisallowUnknownFields` before the
   signature is checked (`:204-211`), and `:212` compares the bytes with a re-marshal of the known
   struct. The failure is reported as `signature` (`internal/attachment/client.go:783-785`), which
   holds the body's cursor, or every body of the request when the body cannot be told (`:766-779`).
   The next run receives the same statement and holds again. A bundle does the same
   (`internal/attachment/bundleimport.go:186-189`). `statement.go:261-264` allows only the
   `audience` values `administrators` and `readers`, and the `origin` forms `tenant` and
   `publisher:<name>`.
2. **An unknown kind or media type is refused for good.** `client.go:960-961` is the feed's own
   kind filter; a kind the binary does not know fails `CanonicalMediaType`
   (`internal/attachment/kinds.go:56-73`) and is refused as `media type` (`client.go:963-964`). The
   cursor moves past it (`:818-822`; `attachments.md:453-458`).
3. **A document in a newer format is refused for good** as `document` (`client.go:1020-1023`):
   CycloneDX other than `1.x` (`kinds.go:203`), SPDX other than `SPDX-2.x` (`:241`), SARIF other
   than `2.1.0` (`:378`), a `kista.review.v1` document with one unknown member (`:476-479`). A
   review's version is also in its media type (`:29`), so a `v2` review is refused as `media type`.
4. **The kinds are closed in three places**: a `CHECK` on `attachments.kind`
   (`internal/store/migrations/{postgres,sqlserver,sqlite}/0017_attachments.sql:30`), a feed's
   settings (`client.go:124-131`), and a plan's feeds (`internal/bundle/feeds.go:89-96`). The last
   runs on the **exporter**: a plan from a newer site naming a new kind is refused whole.
5. **A bundle with one new member fails whole.** `internal/bundle/manifest.go:199-216` refuses
   unknown fields in a manifest (`bundle.go:144`), a plan (`internal/upstream/export.go:222`) and a
   batch (`feeds.go:224`). The formats are closed (`manifest.go:357`, `:425`). Validation also
   refuses the whole manifest for a source of an unknown package type (`:310-318`), a revocation
   with an unknown state (`:549`) and a cell with an unknown outcome (`:648`). Only the tar is
   tolerant: an entry the manifest does not name is read through (`bundle.go:240`).
6. **`-/names` and `-/revocations` never check `api`.** Both require the member and ignore its
   value (`internal/upstream/names.go:60-66`, `revocations.go:85-91`): an `"api": 2` answer is read
   as version 1. `-/names` carries no package type (`internal/serve/names.go:19-23`) and the reader
   assumes DuckDB extensions (`names.go:122`, `:129`), so a channel of another type would be
   mirrored as extensions. Forwarded usage carries no type (`internal/usage/daily.go:27-45`).
7. **A revocation with an unknown state is dropped, and can undo a stored one.**
   `revocations.go:234` skips the row. After a complete read, a stored row of the same build is
   then "no longer listed" (`:249-251`): deleted when nothing was applied or lifted of it, else
   marked unlisted. A state the reader does not know reads as "not revoked". In a bundle the same
   row refuses the whole manifest (item 5).
8. **A licence in a newer format removes the licence the kista holds.** `v` other than 1 is
   refused (`internal/license/license.go:273`); so is a header with a fourth member (`:233`);
   unknown payload members are ignored (spec 0012). The inventory missed what follows: the relay
   parses each token before its transaction (`internal/licenses/relay.go:228-242`), a token that
   does not parse never marks its name as kept, and the stored licence of that name is deleted and
   reported `revoked` (`:351-365`). When an upstream lists a `v: 2` licence for a name, every older
   kista drops the `v: 1` licence it serves, and the extension loses its licence at the customer.
9. **The feed route refuses a request with an unknown member.** `internal/api/feeds.go:126` decodes
   with `DisallowUnknownFields` (`internal/api/manage.go:64-65`): a newer subscriber that adds a
   request member gets `400` from an older source.
10. **An SDK report with an unknown status is refused whole** (`internal/usage/report.go:94`).

### What is tolerant today

- `.well-known`: a plain decode, keys matched by pinned fingerprint
  (`internal/upstream/upstream.go:1048-1053`, `import.go:493-497`, `export.go:606-611`).
- The attestation keys file (`client.go:667-683`), the feed's answer (`:735`), the licence relay's
  answer (`relay.go:202-207`), usage reports and daily aggregates (`usage/report.go:76-83`,
  `daily.go:68-70`). In each, a key or token that does not parse is left out, not fatal.
- A licence's payload: unknown members ignored, "a change of meaning bumps `v`" (spec 0012).

### What a mirror reads of a kista

`internal/upstream` (`run.go`, `pull.go`, `import.go`, `export.go`) reads four things of a remote:
extension files (DuckDB's format, verified by signature and footer), `.well-known`, `-/names` and
`-/revocations`. `run.go` parses no JSON of a peer. It never reads the index API (spec 0039).
Catalogue entry metadata and events are not exchanged between kistas: no reader exists.

### What has no written rule

- The config file refuses unknown keys (`internal/config/config.go:370`) and has no version.
- The admin API, the CLI, events and the console's mount contract (`web/console/src/mfe.tsx:19`,
  `contract = 1`; spec 0015: "a shell checks each module's") have no rule for what a release may
  change. The schema has one: `min_reader` (spec 0003, `internal/store/migrate.go:193`, `:292`).

## Design

### The rules

1. **A format has a major.** `api` in the dash routes, `format` in a plan and a manifest, `v` in a
   licence and (new) in a statement, the media type of a document. The same major is readable. A
   route or file without a marker is major 1; its next major is a new route or file name.
2. **Within a major, unknown members are ignored.** A producer adds a member in a minor release
   only when a reader that ignores it grants, serves and shows no more than a reader that
   understands it. A member that narrows, replaces or conditions a known one is a new major.
   Rule 2 is the producer's promise; rules 3 and 4 are what the reader does when it is broken.
3. **A higher major, or an unknown value of a known member, makes that one item wait.** It is not
   applied, not guessed at and not dropped. Where the surface has a safe floor (below), the item
   falls to the floor instead.
4. **Verify first.** The signature is checked over the exact bytes before anything is read for
   meaning. What does not verify is never kept. A few members are **frozen**: they keep their name,
   type and meaning in every major, so a reader can file an item it cannot read.
5. **An unreadable item is not an absent one.** Where absence means "remove" (a licence left out, a
   revocation no longer listed), an answer holding an item the reader cannot read removes nothing
   that item may name. What can only take away (a withdrawal, a revocation) is applied in its
   mildest known form from any major.

"Wait" always means: one item, the rest goes on, the operator sees it, and it is taken again after
an upgrade without anyone resetting a cursor.

### Statements and the feed

**The format.** A statement gains the optional integer `v`; absent is 1. This release writes none.
Frozen in every major: `v`, `issuer`, `key`, `issuer_id`, `seq`, `body_hash`, `withdrawn`; RFC 8785
with integers only; the digest's domain `kista-statement-v1` (it separates statements from kista's
other signatures; it is not the format's version and does not move).

**Verification** (`attachment.Verify`), in this order: size and UTF-8; a plain parse for `key`; the
key is pinned; the signature over the received bytes; the bytes are their own canonical form
(`Canonicalize(stmt)` equals `stmt`, which also refuses a duplicate member); then `v`. A failure
before `v` is `signature` and holds its body as today: nothing of it is trusted.

**A verified statement:**

| What | Outcome |
| --- | --- |
| `v` 1, known kind and media type | as today; unknown members ignored |
| `v` above 1, `withdrawn: true` | applied as a withdrawal: it can only remove |
| `v` above 1, otherwise | waits (`newer`) |
| unknown `kind` | waits (`kind`) |
| known kind, unknown `media_type` | waits (`media type`) |
| a document whose own version marker is not one this binary reads (item 3) | waits (`document version`) |
| unknown `audience` | stored for `administrators` |
| any `origin` of at most 80 printable bytes | stored and shown as text |

- **`audience` has a floor.** The tenant's administrators hold the feed's credential: what the feed
  delivers, they may already read. So an unknown audience is the narrowest one, and widening stays
  the administrators' act (as today).
- **`origin` decides nothing.** It is shown beside the document
  (`internal/api/attachments.go:44`). A new form is a label, not a rule.
- **A feed's `kinds` setting and a plan's `kinds`** are checked by grammar
  (`[a-z][a-z0-9-]{0,15}`), not against the vocabulary: an administrator may name a kind the
  binary will learn, and an older exporter reads a newer site's plan.
- **A broken document** of a known version is refused for good, as today. `kista.review.v1` is
  read with unknown members ignored (rule 2); a change of meaning is `kista.review.v2`, a new
  media type.
- **A higher-major statement about a document already held** waits, and the document falls to
  `administrators` until the statement is read: the newest statement governs, and this kista
  cannot tell whether it narrows. The waiting row remembers the narrowing (`narrowed`).

**Waiting.** The statement's bytes and signature are kept in `feed_waiting`, the cursor moves past
it, and the run counts it (`waiting`). No document is fetched for it, except where the document
had to be read to find out. A later readable statement of the same `issuer_id`, or its withdrawal,
deletes the row. The rows count under `limits.ceilings.feed_kept` with what export feeds keep; at
the cap a statement that cannot be kept holds its body's cursor and the run ends `failing`
(`limits`), as today: nothing is passed over unseen.

**Taking it again.** Each row records the kista version that kept it (`reader`). Before a feed's
run, and before a plan is written or a bundle imported for a bundle-mode feed, the rows kept by
another version are retaken, unless that version compares as newer than this binary (an old
replica in a rolling upgrade retakes nothing): a `narrowed` document gets its visibility back, the
body's cursor goes to just below its lowest waiting statement, the rows are deleted. The run or
the next bundle then brings the statements again by the ordinary path. A replay is harmless by the
rules that already make a manual reset safe: a statement not newer than the stored one is a no-op,
a removed document stays removed. What is still unreadable waits again under the new version.

**An export feed** (spec 0014) carries nothing it is waiting on: a body's range ends just below its
first waiting statement, as it already ends below one that does not verify
(`internal/upstream/exportfeeds.go:334-335`). A site behind an older gateway loses nothing; that
body's later statements reach it once the gateway, the connected side, is upgraded.

**The feed's routes.** `POST …/feed` ignores unknown request members (it reads `bodies`). The feed
stays at `…/api/v1/tenants/<t>` with `/feed`, `/feed/documents/<id>` and `/attestation-keys`
(`client.go:78`): a later management API keeps these paths.

### The dash routes: `-/names`, `-/revocations`

- **`api` is checked.** 1 is read, unknown members ignored. A higher `api` is a kista this binary
  does not read: the pass records one problem (`newer`), stores nothing and forgets nothing.
  `remote_kista`, the discovered names and the stored revocations stay as they are.
- **`package_type`** is a new member of both answers, always written. A reader that does not
  mirror that type takes no names and no rows and records a problem. Absent is `duckdb_extension`.
- **A revocation with an unknown state is a yank.** Every row of the route is a revoked release;
  the mildest thing kista does about one is a yank of the upstream's release, in its home channel,
  liftable. The row is stored with the source's state as text when it is `[a-z][a-z0-9_-]{0,15}`
  (the column takes it; no `CHECK`), ranked as `yanked`, applied as one where the switch is on,
  and shown with its state. It is listed, so it never forgets a stored row (rule 5).
  `applyRevocations` treats only `withdrawn` and `blocked` as blocks
  (`revocations.go:289` reads "not yanked" today).
- `-/licenses`, `-/usage` and `-/license` carry no marker: major 1 (rule 1).

### Licences

- A licence's `v` and `ext`, and its three header members, are frozen.
- A token with `v` above 1 is still refused (`v`), in the relay, an import and a bundle. In the
  relay its `ext` marks the name as kept: the stored licence of that name stays, and ends with its
  own expiry and grace. The run counts it (`newer`) and the refusal names the extension.
- A token whose payload does not parse far enough to read `v` and `ext` is refused as today and
  keeps nothing.
- kista still serves only licences it verified. It does not relay one it cannot read.

### Usage

- A daily report gains the optional `package_type` (absent: `duckdb_extension`). A receiver drops
  the reports of a type it does not know and counts them; the rest of the aggregate is taken.
- An SDK report with a status this kista does not know is taken and counted under `other`.

### Bundles

- **Unknown members are ignored** in a manifest, a plan and a batch. Duplicate keys and trailing
  data are still refused: a signed document means one thing.
- **A higher major is refused whole**, saying which side to upgrade. It cannot be read in part: the
  manifest binds every file. It should not arrive: an exporter writes the lowest format that
  carries the bundle, and a new major only for a plan that asks for it (as `kista-bundle/v2` is
  written only for `accepts_revocations`, spec 0039).
- **One unknown thing fails its own part**: a source of an unknown package type is left out with a
  problem and the other sources import; a cell with an unknown outcome is a problem of that cell; a
  revocation with an unknown state is a yank; a licence of a higher `v` is refused by itself.
- Statements in a bundle follow the feed's rules: they wait, and the next plan asks again.

### Surfaces that need nothing

`.well-known`, the attestation keys file, the relay's and the feed's answers (tolerant; trust comes
from pinned fingerprints, so an unknown member adds none). Extension files (DuckDB's format). The
tar of a bundle. A licence's payload members.

### What the operator sees

- **Counts.** A feed run counts `waiting`; a feed's record gains `waiting`: the number of rows, by
  reason, with the kinds or versions they name. An upstream run and the licence relay count
  `newer`.
- **A console line** on the feed, the upstream and the tenant's home: "This kista is older than
  its source: 3 documents (kind `report`) wait for an upgrade." Nothing else changes colour: a
  waiting item is not a failure, and the feed stays `ok`.
- **Events.** No new kind. `feed.run`, `upstream.run`, `license.sync` and a bundle import's event
  gain `newer`: what waited or was not read, with counts. They reach the sinks as any event.
- **CLI.** `kista admin feed show` and `upstream show` print the same line.

kista does not show its upstream's version, and asks for none: spec 0039 keeps a kista's version
from anonymous callers, and what waits says more than a number.

### Data model

Migration `0054_forward_compat`, three dialects, `min_reader` unchanged (additive; an older replica
writes only the five kinds).

```text
feed_waiting   feed_id (fk feeds, cascade), body_hash varchar(64), seq bigint,
               issuer_id varchar(64), reason varchar(32), detail varchar(100),
               statement text, signature varchar(512), reader varchar(64),
               narrowed boolean, waiting_since timestamp
               primary key (feed_id, body_hash, seq); index (feed_id, issuer_id)
attachments    ck_attachments_kind dropped
```

- `detail` is the unknown kind, media type or version, cleaned to printable text, for display.
- PostgreSQL and SQL Server drop the constraint with `ALTER TABLE`. SQLite rebuilds the table
  (`-- +foreign_keys_off`, as migrations 0034, 0039, 0042 and 0053 do).
- The kinds are validated in `internal/attachment` only. A kind added later needs no migration.
- The collector deletes a body's waiting rows with its cursors; a feed's go with the feed (and so
  with its tenant, spec 0019).
- `upstream_revocations` needs nothing.

### The compatibility rules

A page of the docs site, **Compatibility** (`website/docs/compatibility.md`, linked from
`administration/upgrading.md` and `development.md`). "Major" and "minor" are kista's release
numbers. A contributor's change is one of the rows below, or it waits for a major.

| Interface | A minor release may | Needs a major |
| --- | --- | --- |
| Wire formats between kistas (dash routes, the feed, usage, the relay) | add a member by rule 2; add a route; add a value where the reader has a floor or waits | remove or rename a member; change its meaning; narrow by an added member; drop a route or a marker's major |
| Signed formats (statements, licences, bundle manifests, the review format) | the same, and nothing that changes the bytes a reader verifies | a new `v`, `format` or media type; never a change to the frozen members or to how bytes are canonicalised and signed |
| Bundles | add members; write a higher format only for a plan that asks | drop a format a supported site writes or reads |
| Admin API `/api/v1` | add routes, optional request members, response members, values, problem types | remove or change any of them: `/api/v2`, with v1 served through the next major |
| Config file | add keys with defaults that keep behaviour; deprecate a key | remove a deprecated key; change a default that changes behaviour for an unchanged file |
| CLI | add commands and flags; deprecate with a warning | remove or rename; change `-json` output other than by addition; change exit codes |
| Events | add kinds and members | remove or rename a kind or member; change the hash chain's encoding |
| Database schema | add migrations; raise `min_reader`, named in the upgrade notes | edit an applied migration (never); drop a column in the release that stops using it |
| Console mount contract | add optional options, events and exports | raise `contract` |

- **Both sides read tolerantly, each its own direction.** A client of the admin API ignores unknown
  response members and values. The server refuses unknown **request** members: a newer CLI or
  console against an older server fails loudly rather than dropping what was asked.
- **Config.** Unknown keys stay an error: a misspelt `egress.hosts_enforce` must not start a server
  without it. A renamed or removed key is still accepted for the rest of the major, with a
  start-up warning naming its replacement; the old and the new name together are an error. No
  version field: a binary knows its own keys, and there is nothing to switch on. A file written
  for a newer minor fails on an older binary by naming the key.
- **Schema.** A minor release upgrades from any earlier release of its major. A vocabulary that a
  peer or a newer replica can extend is validated in code, never by a `CHECK`.
- **The mount contract.** `contract` is a major and nothing else. A host compares for equality,
  which is right: a host built for 1 cannot drive 2. kista raises it only in a major release.
- **Human-readable CLI output** is not an interface.

## Security

- **Tolerance starts after verification.** A statement, a manifest and a licence are verified over
  their exact bytes before any member is read for meaning. An unknown member is signed like the
  others; ignoring it does not weaken the signature, and it cannot be added by a carrier.
- **Nothing unverified is kept.** A waiting row holds a statement signed by a pinned key, at most
  16 KiB, under the tenant's cap. A flood of unreadable statements costs what a flood of refused
  ones costs today.
- **Nothing unknown is applied.** A waiting statement shows nothing, to anyone. Its `detail` is
  cleaned and shown to administrators only.
- **Fail closed where ignoring could over-grant**: an unknown audience is `administrators`; a
  higher-major statement about a held document narrows it; an unknown revocation state is a yank;
  a higher `api` is not read as 1; an unknown package type mirrors nothing.
- **Fail soft where it could only under-show**: an unknown kind, media type or document version
  waits and is shown after an upgrade.
- **Rule 2 is a promise, not a check.** A producer that narrows by an added member in major 1
  would be over-applied by an older reader. No reader can detect that; the rule, the frozen
  members and the fixtures below are the defence. The exposure is bounded by the floors.
- **Licences.** Keeping a `v: 1` licence while its upstream lists a newer one for the name grants
  nothing new: the token is the issuer's, unexpired, and ends by its own `exp`. Leaving a name out
  still revokes it. Licensing is a compliance control, not DRM (spec 0001).
- **Revocations.** An unknown state can only stop serving, in one channel, and an administrator
  lifts it. It cannot be used to un-revoke.
- **No existence leaks.** Waiting rows are read through the feed's management routes only.
- **Config stays strict**, so tolerance never reaches the server's own security settings.

## Testing

- **Unit, per rule.** `Verify`: an extra member (stored), `v: 2` (waits), `v: 2` withdrawn
  (applied), a duplicate member and a non-canonical form (`signature`), an unknown audience
  (`administrators`). Kinds: an unknown kind and media type wait; CycloneDX 2, SPDX 3 and SARIF
  2.2 wait; a broken document is refused; a review with an extra member is stored.
- **The feed.** One body with a waiting statement between two readable ones: both are stored, the
  cursor passes all three, the run is `ok`. The cap. A withdrawal and a readable re-statement
  delete the row. A held `readers` document under a `v: 2` re-statement narrows, and comes back.
- **Taking it again.** The same store with the kind registered and a later version: the next run
  stores the document with no reset; a replay changes nothing else; an older version retakes
  nothing. The same over two bundles and a plan.
- **The store suite**, three dialects: `feed_waiting`, the dropped constraint (a sixth kind
  inserts), the collector, a tenant's deletion.
- **The version-skew test** (`internal/skew`, in `make test`): this build as the downstream of a
  stand-in newer upstream, an `httptest` server and a bundle writer with a test attestation key and
  bundle key. It serves, together: a statement of an unknown kind, one with an extra member, one
  with `v: 2`; a revocation with an unknown state; `-/names` with an unknown `package_type`;
  `"api": 2` on a second upstream; a licence with `v: 2` beside a stored `v: 1`; a bundle with an
  extra manifest member, one of a higher format, one with a source of an unknown type. It asserts
  after one run of everything: every readable item is stored, nothing readable is held, the
  unknown ones wait or fall to their floor as the tables say, the stored licence and revocations
  are still there, and the events carry `newer`.
- **Fixtures as the contract.** `testdata/wire/1/` holds the exact bytes this release writes for
  each format, with their signatures. They are never edited. A later release must read them: the
  backward half of the same promise.
- **After the release**: `make skew-release` runs the last released binary as the downstream of
  the working tree's server, locally and on release branches (not on every push).
- **Docs**: the Compatibility page; `attachments.md` (the waiting outcome, the reset no longer
  needed for it), `upstreams.md`, `licensing.md`, `upgrading.md`.

## Alternatives considered

- **Hold the cursor until the reader is upgraded** (today). One statement blocks its body's
  attachments, vulnerability documents among them, for as long as the customer does not upgrade.
- **Skip and reset the cursors on upgrade.** Nothing to show the operator in between, and every
  feed reads its whole history at every upgrade.
- **Try the waiting rows again at every run.** No version column, but a document version cannot be
  judged without fetching the document, so every run would fetch what it cannot read.
- **Keep the document of a waiting statement.** It saves one fetch and stores bytes nobody has
  validated, under a quota of their own. An export feed's range ending below it does the same job.
- **Negotiation**: the downstream sends its version or its capabilities and the upstream answers
  in kind. Every producer then carries every old form, each pair is a case to test, and a bundle
  cannot negotiate. Rule 2 needs no round trip. The one existing case, a plan's
  `accepts_revocations`, stays as the pattern for a manifest's major.
- **A `crit` list** (JWS, X.509): the members a reader must understand, named in the item. It is
  rule 3 at member grain. `v` does the same with one integer, and a producer signs `v: 1` whenever
  it uses nothing new.
- **Ignore unknown members everywhere, no majors.** A member that narrows would be over-applied.
- **Apply nothing for an unknown revocation state** (show only). A revocation is the one message
  whose point is that the reader acts.
- **Remove the stored licence when a newer one is listed** (today). Fail closed, and the customer's
  extension stops because Enterest moved first.
- **Tolerant config.** A typo in a security key would be silent.
- **A config version field, an event schema registry, a per-route capability document.** Machinery
  with no reader today.

## Open questions for the owner

1. **The release's number.** The rules speak of majors and minors; under `0.x` semver promises
   nothing. Recommendation: this release is `v1.0.0`.
2. **How far back Enterest stays readable.** Recommendation: every wire major 1 format is served
   for all of kista 1.x; a wire major is retired only by kista 2.0, with both served for at least
   twelve months. An installed 1.0 then never needs an upgrade to stay connected, only to see what
   waits.
3. **An unknown revocation state.** Recommendation: a yank, as designed (liftable, only where the
   organisation switched revocations on). The alternative is show-only.
4. **A licence in a newer format beside a stored one.** Recommendation: the stored licence stays
   until its own expiry and grace, as designed. The alternative, removal, is stricter and stops the
   customer's extension on the day Enterest issues the first `v: 2`.

## What spec 0045 and Enterest must respect

**Spec 0045** (a `report` kind, relay of received documents):

- A new kind is code only: no `CHECK`, no migration for the vocabulary. Whichever of the two specs
  lands first drops `ck_attachments_kind`.
- A `report` statement is `v` 1 unless it carries a member a reader must not ignore. A kista of
  this release waits on it (`kind`) and takes it after its upgrade.
- **Relayed statements go only to a subscriber that asks for them**, by a request member an older
  subscriber never sends, or on a route of their own. A statement whose issuer is not the feed's
  URL, or whose key the feed does not pin, holds its body at every reader of this release
  (`client.go:783-789`), by design: it is not trusted.
- A relayed statement keeps its issuer's exact bytes and signature. Nothing is re-canonicalised.
- A new `origin` form is a label (at most 80 printable bytes); a new `audience` value reads as
  `administrators` on an older kista.

**Enterest**, as the upstream that is always newer:

- Rule 2, on every format it writes. A narrowing is a new major, never an added member.
- The frozen members of a statement and a licence; RFC 8785 with integers only; the same digest
  domain; a withdrawal readable by its frozen members in every major.
- The feed at `…/api/v1/tenants/<t>` and the dash routes under each channel's prefix, with
  `"api": 1` while their shape is major 1. A channel of another package type says so in
  `package_type`.
- A new revocation state means at least a yank at an older kista.
- A licence of a newer `v` is issued for an extension only when its live builds' SDK reads it; an
  older kista keeps serving the licence it holds until that licence ends, so a real revocation
  leaves the name out.
- A bundle it exports is in the lowest format that carries it.
- As a client of kista's API and events, and as a host of the console: unknown members, values,
  kinds and exports are ignored; `contract` is compared for equality.
- Its CI runs the oldest supported kista release as its downstream.

## Follow-ups

- The C++ SDK (hugr-lab/extension-ci-tools, `kista/sdk/cpp`) is a reader too: of `-/license` and
  of the usage route's answer. The same rules, in its own spec there.
- A document refused as `size` under the site's configured maximum is not taken again when the
  maximum is raised. It is not a version skew; the waiting table could carry it.
- Spec 0032's open question (a downstream's retries on `503`) is unrelated and stays open.
