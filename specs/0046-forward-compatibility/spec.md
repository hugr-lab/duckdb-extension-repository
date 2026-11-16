# Spec 0046: Forward compatibility: what a kista does with what a newer peer sends, and the compatibility rules

- **Status**: accepted (2026-11-16; the owner's answers of 2026-11-16; three reviews and a re-review applied)
- **Date**: 2026-11-16
- **Author**: vgsml, Claude

## Summary

kista's first real release for on-prem installs is `v0.2.0`. An installed kista is chained to a
peer that will be newer than it: its organisation's tenant in Enterest, or another kista. Today
most of what one kista reads from another is read strictly: one unknown field, kind, state or
format holds everything behind it or is refused for good. This spec fixes how a released kista
treats what it does not know, with five rules that are the same on every surface. A wire format
has its own major; within it unknown members are ignored; a higher major, or a value the reader
does not know, makes **that one item wait**: passed over, not applied, counted for the operator,
and taken again by itself once the kista is upgraded. Nothing unknown is ever applied. The spec
also writes down what a later release may change.

The rules are in force from `v0.2.0` and hang on the formats' own majors, not on kista's release
number: kista 1.0 changes nothing here. The trial tags `v0.1.0` to `v0.1.4` are owed nothing.

## Problem

Verified in the code at `3ddc6e4`. "Refused for good" means the cursor moves past the statement.
An online feed has no cursor reset of its own: only toggling `export` resets it
(`internal/attachment/client.go:338`), and `bundle reset -feed` a bundle-mode feed's
(`internal/upstream/importfeeds.go:238`).

1. **A signed statement with one unknown member holds its body for ever.**
   `internal/attachment/statement.go:199-202` decodes with `DisallowUnknownFields` before the
   signature is checked (`:204-211`); `:212` compares the bytes with a re-marshal of the known
   struct. The failure is `signature` (`client.go:783-785`), which holds the body's cursor, or
   every body of the request when the body cannot be told (`:766-779`). Every run holds again. A
   bundle does the same (`internal/attachment/bundleimport.go:186-189`). Only two `audience`
   values and two `origin` forms pass (`statement.go:261-264`).
2. **An unknown kind or media type is refused for good.** `client.go:960-961` is the feed's own
   kind filter; a kind the binary does not know fails `CanonicalMediaType`
   (`internal/attachment/kinds.go:56-73`) and is refused as `media type` (`client.go:963-964`); the
   cursor moves past it (`:818-822`).
3. **A document in a newer format is refused for good** as `document` (`client.go:1020-1023`):
   CycloneDX other than `1.x` (`kinds.go:203`), SPDX other than `SPDX-2.x` (`:241`), SARIF other
   than `2.1.0` (`:378`), a review with one unknown member (`:476-479`).
4. **The kinds are closed in three places**: a `CHECK` on `attachments.kind`
   (`internal/store/migrations/*/0017_attachments.sql:30`), a feed's settings
   (`client.go:124-131`), a plan's feeds (`internal/bundle/feeds.go:89-96`). The last runs on the
   exporter: a newer site's plan naming a new kind is refused whole.
5. **A bundle with one new member fails whole.** `internal/bundle/manifest.go:199-216` refuses
   unknown fields in a manifest, a plan (`internal/upstream/export.go:222`) and a batch
   (`feeds.go:224`). The formats are closed (`manifest.go:357`, `:425`). The whole manifest is also
   refused for a source of an unknown package type (`:310-318`), a revocation with an unknown
   state (`:549`), a cell with an unknown outcome (`:648`). The tar is tolerant (`bundle.go:240`).
6. **`-/names` and `-/revocations` never check `api`**: the member is required, its value ignored
   (`internal/upstream/names.go:60-66`, `revocations.go:85-91`).
7. **A revocation with an unknown state is dropped, and can undo a stored one.**
   `revocations.go:234` skips the row; after a complete read the stored row of that build is "no
   longer listed" (`:249-251`): deleted, or marked unlisted. Unknown reads as "not revoked".
8. **A licence in a newer format removes the licence the kista holds.** `v` other than 1 is
   refused (`internal/license/license.go:273`). The relay parses each token before its transaction
   (`internal/licenses/relay.go:228-242`); a token that does not parse never marks its name as
   kept, and the stored licence of that name is deleted and reported `revoked` (`:351-365`).
9. **The feed route refuses a request with an unknown member** (`internal/api/feeds.go:126`,
   `internal/api/manage.go:64-65`), and **an SDK report with an unknown status is refused whole**
   (`internal/usage/report.go:94`).

**Tolerant today**: `.well-known` (`internal/upstream/upstream.go:1048-1053`, `import.go:493-497`,
`export.go:606-611`), the attestation keys file (`client.go:667-683`), the feed's answer (`:735`),
the licence relay's answer (`relay.go:202-207`), usage reports and daily aggregates, a licence's
payload members (spec 0012). Trust there comes from pinned fingerprints and signatures.

**What a mirror reads of a kista**: extension files (DuckDB's format), `.well-known`, `-/names`,
`-/revocations`. `internal/upstream/run.go` parses no JSON of a peer; the index API is never read
(spec 0039). Catalogue entry metadata and events are not exchanged between kistas.

**No written rule** exists for the config file (unknown keys refused,
`internal/config/config.go:370`), the admin API, the CLI, events or the console's `contract`
(`web/console/src/mfe.tsx:19`). The schema has one: `min_reader` (spec 0003).

## Design

### The rules

1. **A wire format has a major**: `api` on the dash routes, `format` in a plan and a manifest, `v`
   in a licence and (new) in a statement, a document's media type, a report's `envelope` (spec
   0045). Major 1 stays readable. It is retired only by the format's major 2, which is a new route
   or a new marker value, never a changed meaning under the old one.
2. **Within a major, unknown members are ignored.** A producer adds a member only when a reader
   that ignores it grants, serves and shows no more than a reader that understands it. A member
   that narrows, replaces or conditions a known one is a new major. A producer writes the lowest
   major that carries what it says.
3. **A higher major, or an unknown value of a known member, makes that one item wait.** It is not
   applied, not guessed at and not dropped; the rest goes on. Where the surface has a floor
   (below), the item falls to the floor instead.
4. **Verify first.** The signature is checked over the exact bytes before anything is read for
   meaning. A few members are **frozen**: the same name, type and meaning in every major, so a
   reader can file an item it cannot read.
5. **An unreadable item is not an absent one.** Where absence means "remove", an item the reader
   cannot read removes nothing it may name. What can only take away (a withdrawal, a revocation)
   is applied in its mildest known form.

### Statements

**The format.** A statement gains the optional `v`, an integer of at least 1; absent is 1. This
release writes none. Frozen in every major: `v`, `issuer`, `key`, `issuer_id`, `seq`, `body_hash`,
`withdrawn`, `audience` (`readers`, or anything else: not readers); RFC 8785 with integers only;
the digest's domain `kista-statement-v1`, which separates statements from kista's other signatures
and is not the format's version.

**Verification** (`attachment.Verify`), in order: size and UTF-8; every member name at every
depth is `[a-z0-9_]{1,64}` and none is twice in its object (the bundle manifest's
`noDuplicateKeys` and `keyRE`); the key is pinned; the signature over the received bytes; the
bytes are their own canonical form. A failure is `signature` and holds the body, as today.

- The name rule is why tolerance is safe: Go's decoder matches names case-insensitively and takes
  the last duplicate, so `Kind` beside `kind` would mean different things to two readers.
- A fraction or an exponent in any member, known or not, is not canonical here: `signature`. An
  unknown member carries integers, strings, booleans, arrays and objects only.
- A verified statement whose frozen or known member has the wrong type or is out of its bound is
  refused for good (`statement`), not held: it is authentic and will not get better.

**A verified statement**, after the feed's own kind filter (a kind the administrator left out is
refused `kind`, as today):

| What | Outcome |
| --- | --- |
| `v` 1, known kind and media type | as today; unknown members ignored |
| `v` above 1, `withdrawn: true` | applied as a withdrawal: it can only remove |
| `v` above 1, otherwise | waits (`newer`) |
| a kind the binary does not know | waits (`unknown kind`) |
| known kind, unknown `media_type` | waits (`media type`) |
| a document version the binary does not read (item 3) | waits (`document version`) |
| an `audience` other than `readers` | stored for `administrators` |
| an `origin` that passes `ValidTitle`'s predicate, at most 80 bytes | stored and shown as text |

- **The floor is the tenant's administrators**: they hold the feed's credential, so what the feed
  delivers they may already read. Widening stays their act.
- **A higher-major statement about a document already held** waits. When its `audience` is not
  `readers`, its `seq` is higher and its `body_hash` is the document's, the document is narrowed
  to `administrators` first. It is not widened back.
- **A kind may declare a floor for its document.** Spec 0045's `report` does: its identity and
  summary members and the issuer's HTML page are read, the rest is never interpreted, so a report
  of any `envelope` is stored and shown. Every other kind waits on a version it does not read.
- **`kista.review.v1`** ignores unknown members at feed and bundle intake only. The local attach
  route stays strict.
- **A feed's `kinds` setting** stays a closed vocabulary: a typo must fail. **A plan's `kinds`**
  are checked by grammar (`[a-z][a-z0-9-]{0,15}`), so an older exporter reads a newer site's plan.

### Waiting and taking again

No new table. `feed_cursors` gains `waiting_from` (the lowest `seq` passed over), `waiting` (a
count), `waiting_why` (the last reason, for display) and `reader_level`.

- **The reader level** is an integer constant in `internal/attachment`, raised whenever the kinds,
  the media types, a readable document version or `v` grow. A release that adds a kind (spec
  0045's `report`) raises it.
- **Waiting**: the cursor moves past the statement; `waiting_from` keeps the lowest, `waiting` is
  raised, `reader_level` is the binary's. A row has one level: while it has waiting state, a new
  wait keeps the lower one (an old replica waited at seq 12, the new one waits at seq 15; two
  waits of different reasons). No document is fetched except where it had to be read to find
  out. Nothing is stored that can be exhausted, so there is no cap.
- **Retake**, before a feed's run and when a plan is written (not at a bundle's import): a cursor
  whose `reader_level` is below the binary's goes back to `waiting_from - 1` and its waiting state
  is cleared. This is a new store operation: `SetFeedCursor` only moves forward. An older replica
  in a rolling upgrade retakes nothing.
- **A replay judges the rewound range again**, earlier refusals included: a document refused for
  `size`, `title` or `limits` then is taken now if it passes now. What is stored stays as it is: a
  statement not newer than the stored one is a no-op, a removed document stays removed.
- **A cursor reset** (`export` toggled, `bundle reset -feed`, the collector) deletes the waiting
  state with the cursor.
- **The count may overstate** until the upgrade: a waiting document withdrawn or re-stated since
  is still counted. It says "something waits", not how much will arrive.

### Export feeds and relays

A feed with `export` on keeps a waiting statement exactly as it keeps a refusal today: in
`feed_refusals`, bytes and signature, under `limits.ceilings.feed_kept` (unchanged in meaning).
A bundle, and spec 0045's relay, carry it without content. Its note is its kept reason, one of
this spec's waiting reasons (`newer`, `unknown kind`, `media type`, `document version`). Later
statements of the body flow.

The receiver verifies it as any statement. One it cannot read waits by its own rules. One it can
read waits too, but only when the note is one of those words, and at `reader_level` 0, so it is
asked for again until the exporter is upgraded and carries the content. With any other note (a
document removed or refused at the exporter, or of a kind its export feed does not take) it is
refused `no content` for good, as today.

A retake at the gateway deletes that feed's kept refusal rows of the body with `seq` at or above
`waiting_from`; the replay keeps again what is still refused. Otherwise a waiting statement
withdrawn or re-stated at the source before the upgrade would leave a kept row that every later
bundle carries as waiting: the source sends only what it still offers, and `KeepFeedRefusal` does
nothing on an existing row.

### The dash routes

- **`"api": 1` is frozen** on `-/names` and `-/revocations` for wire major 1; a next major is a new
  route. The value is still checked, as a guard: a higher one makes the upstream's run `failing`,
  reads nothing and forgets nothing.
- **A revocation with an unknown state is a yank.** At intake (`storeRevocations`, shared with the
  bundle import) a state other than the three is stored as `yanked`, with the source's word
  prefixed to `reason` when it is `[a-z][a-z0-9_-]{0,15}`, else under the fixed label `unknown`. It
  never lowers a stored stronger state, and it is listed, so it never forgets a stored row (rule
  5). No rank changes. After an upgrade the same row arrives with its real state and applies by
  the existing precedence.
- **Package type.** How a remote channel declares its type is its own release item. It will be
  declared in a way this release's reader already refuses or ignores safely; this spec adds no
  rule for it.
- **`POST …/feed`** reads `bodies` and `issuer` (spec 0045's relay selector, part of major 1 from
  the start) and ignores other members. Before the relay exists, a request with an `issuer`
  answers no statements. A request member that selects what is answered is a new
  route, never an added member: an older source would ignore it and answer something else.
- `-/licenses`, `-/usage` and `-/license` carry no marker: major 1.

### Licences

- A token with `v` above 1 is still refused (`v`), in the relay, an import and a bundle.
- **In the relay its name is kept**: a peek reads the payload's `v` and `ext` only, and the stored
  licence of that name stays until its own expiry and grace. The run counts it.
- The peek acts on an unverified `ext`. That is acceptable: it can only keep a licence this kista
  already verified and stored, for no longer than that licence's own `exp`, and the same upstream
  could keep it by listing it again. It adds and serves nothing.
- A payload from which `v` and `ext` cannot be read is refused as today and keeps nothing.
- Leaving a name out still revokes it.

### Usage and bundles

- An SDK report with a status this kista does not know is counted under `other`, not refused.
- **Bundles**: unknown members are ignored in a manifest, a plan and a batch, under the same name
  rule (`[a-z0-9_]{1,64}`, no duplicates), so a future map keyed by data is an array. A higher
  `format` is refused whole, saying which side to upgrade: the manifest binds every file. It
  should not arrive: a new major is written only for a plan that asks for it, as
  `kista-bundle/v2` is (spec 0039).
- **One unknown thing fails its own part**: a source of an unknown package type is left out with a
  problem; a cell with an unknown outcome is that cell's problem; an unknown revocation state is a
  yank; a licence of a higher `v` is refused by itself.

### Surfaces that need nothing

`.well-known`, the attestation keys file, the relay's and the feed's answers, extension files, a
bundle's tar, a licence's payload members, forwarded usage.

### What the operator sees

- `waiting` in a feed run's counts (`feed.run` carries them already). No new event or member.
- A line on the feed and on the upstream, to administrators only, in the console and
  `kista admin feed show` / `upstream show`: "This kista is older than its source: documents wait
  for an upgrade (`unknown kind`)." A feed that only waits stays `ok`.
- A higher `api` is the upstream's `failing` state with its reason.

### Data model

Migration `0054_forward_compat`, three dialects, `min_reader` unchanged.

```text
feed_cursors   + waiting_from bigint null, waiting int not null default 0,
                 waiting_why varchar(32) null, reader_level int not null default 0
attachments    ck_attachments_kind dropped (SQLite: a table rebuild)
```

Kinds are validated in `internal/attachment` only. This release reads an attachment row of any
kind as stored, so a later minor that adds a kind needs no migration and does not raise
`min_reader`.

### The compatibility rules

A docs page, **Compatibility** (`website/docs/compatibility.md`, linked from `upgrading.md` and
`development.md`): the five rules, the frozen members, and this table.

| Interface | A later release may | Never, within the major |
| --- | --- | --- |
| Wire and signed formats, bundles | add a member by rule 2; add a route; add a value where the reader waits or has a floor | change a frozen member, the canonical form or what is signed; narrow by an added member; drop major 1 |
| Admin API, CLI (`-json`, exit codes), events | add | remove, rename, change a meaning |
| Config file | add a key whose default keeps behaviour | rename or remove a key; accept an unknown one |
| Database schema | add migrations; raise `min_reader`, named in the upgrade notes | edit an applied migration |
| Console mount contract | add optional options and exports | change `contract` |

- A client of the API ignores unknown response members; the server refuses unknown request
  members, so a newer client fails loudly instead of dropping what was asked.
- Config stays strict: a misspelt security key must not start a server without it. No version
  field: there is nothing to switch on.
- Each row is held by a test: the wire fixtures, the strict config loader, `min_reader`, the
  `contract` constant.

## Security

- **Tolerance starts after verification.** An unknown member is signed like the others and cannot
  be added by a carrier. The member-name rule closes the one way two readers could read the same
  bytes differently.
- **Nothing unknown is applied or shown.** A waiting statement leaves a count and a reason word
  from a closed list, to administrators.
- **Fail closed where ignoring could over-grant**: an audience that is not `readers` is
  `administrators`, in any major; an unknown revocation state is a yank; a higher `api` is not read
  as 1.
- **Fail soft where it could only under-show**: an unknown kind, media type or document version
  waits.
- **Rule 2 is a promise, not a check.** A producer that narrowed by an added member in major 1
  would be over-applied by an older reader, and no reader can detect it. The frozen `audience`
  bounds what that could expose; the fixtures hold the producers to the rule.
- **A replay** can only re-judge by today's settings what this feed was already offered.
- **Licences**: keeping a stored licence grants nothing new (above). **Revocations**: an unknown
  state can only stop serving, and an administrator lifts it.
- **Config stays strict**, so tolerance never reaches the server's own security settings.

## Testing

- **`Verify`**: an extra member; `Kind` beside `kind`; the Kelvin-sign and long-s spellings of a
  known name; a duplicate; a fraction in an unknown member (each `signature`); `v: 2` waits, `v: 2`
  withdrawn applies, `v: 0` and a wrong type are refused for good.
- **The feed**: a waiting statement between two readable ones (both stored, the run `ok`); the
  feed's filter before waiting; the higher-major audience rule in its four cases; a review with an
  extra member at feed intake and at the attach route.
- **Retake**: a raised reader level stores the document with no reset and re-judges an earlier
  `size` refusal; a lower level retakes nothing; a second wait keeps the lower level; a reset
  with waiting state; a plan after a retake.
- **Export**: a waiting statement kept, carried through an export feed to a newer site, asked for
  again and stored once its content comes; a contentless statement with another note refused
  `no content` for good; a gateway's retake deleting a kept row whose statement was withdrawn.
- **Revocations**: an unknown state yanks, never lowers `blocked`, forgets nothing; after an
  upgrade the real state applies; `"api": 2` fails the run and keeps what is stored.
- **Licences**: a `v: 2` token beside a stored `v: 1` keeps it; a name left out is removed.
- **The store suite**, three dialects: the new columns, the rewind, a row of a sixth kind read.
- **One skew test**, `cmd/kista/skew_test.go`, on the existing fixtures (`bundleFixture`,
  `feedFixture`): a bundle from a stand-in newer exporter with an extra manifest member, a higher
  format, a source of an unknown type, an unknown revocation state, a `v: 2` licence, a statement
  of an unknown kind, one with an extra member and one with `v: 2`. That harness builds bundles
  and runs the CLI; it does not reach an online remote, so the online cases (the feed, `api`, the
  relay) stay in their packages' tests above.
- **Fixtures**: `testdata/wire/1/` holds the bytes this release writes for each format, signed.
  They are never edited; every later release reads them.

## Changes to earlier specs and docs

Each sentence below becomes false and is amended with the code.

- **0011**: `:195-197` (unknown fields refused: now ignored, under the name rule); `:264-266` (the
  cursor's outcomes gain "waits"). The spec does not state the `CHECK`; migration 0017's is dropped.
- **0014**: `:455-456` and `:1373` (strict manifest: unknown members ignored); `:726-730` (what an
  export feed keeps gains waiting statements); `:851` and `:1494` (content not carried: waits when
  the note is a waiting reason, else refused as before). The range rule (`:1490`, `:1496`) stands.
- **0021** `:229` and `website/docs/upstreams.md:970-973`: "decoded strictly" as the reason an
  older kista refuses a bundle with licences.
- **0034** `:228-230`: the same reason, and "a source of a type an importer does not know is
  refused" (left out). `:68-70` and `:456-457` stay the package-type item's.
- **0038** `:637`, `configuration.md:704`, `attachments.md:514`: unchanged; `feed_kept` means what
  it meant.
- **0039**: `:55-60` (the states: any other is a yank; the spec is silent, the code skipped it);
  `:99-100` and `:384` (`"api": 1` is frozen and its value checked).
- **0012** `:241-242` (left out is removed: not for a name with a newer-format token); `:329-331`
  (`status` no longer a `400`).
- **Docs**: `attachments.md:55` (review: unknown fields), `:269` (subscriber refuses unknown
  fields), `:453-464` (cursor outcomes; `signature` now also the name rule); `api.md:65` (the feed
  route ignores unknown members); `administration/upgrading.md` (schema 54).

## Alternatives considered

- **Hold the cursor until upgraded** (today): one statement blocks its body's documents.
- **A table of waiting statements, retried by kista version.** More to store, cap and compare; a
  cursor column and an integer level do the same.
- **End an export's range below a waiting statement.** A site behind an older gateway would block.
- **Negotiation** (the downstream sends its version or capabilities). Every producer carries every
  old form, and a bundle cannot negotiate.
- **A `crit` list** naming must-understand members: rule 3 at member grain; `v` is one integer.
- **Ignore unknown members with no majors.** A narrowing member would be over-applied.
- **Show an unknown revocation state without acting.** A revocation exists to be acted on.
- **Remove the stored licence when a newer one is listed** (today). The customer's extension
  stops because Enterest moved first.
- **Tolerant config, key aliases, a config version.** A silent typo, or machinery with no reader.

## What spec 0045 and Enterest must respect

- **Rule 2** on every format they write: a narrowing is a new major, never an added member.
- **The lowest major** that carries what is said, for statements, licences and bundles.
- **Frozen members**, integer-only RFC 8785, the digest's domain, member names
  `[a-z0-9_]{1,64}` at every depth; a withdrawal and a not-`readers` audience readable in any major.
- **0045**: a new kind is code only (no `CHECK`), and raises the reader level; `report` declares
  its floor and its `envelope`; the relay answers only a request that names `issuer`, keeps the
  issuer's exact bytes, and carries a waiting statement without content, with a note. A statement
  of another issuer or an unpinned key holds its body at any reader (`client.go:783-789`).
- **Enterest**: the feed at `…/api/v1/tenants/<t>` and the dash routes with `"api": 1`; a new
  revocation state means at least a yank downstream; a licence of a newer `v` only where the
  extension's live builds read it, and a real revocation leaves the name out; as a client and a
  host, unknown members, values, kinds and exports ignored and `contract` compared for equality.

## Follow-ups

- The C++ SDK (hugr-lab/extension-ci-tools, `kista/sdk/cpp`) reads `-/license` and the usage
  route's answer: the same rules, in its own spec there.
- `make skew-release`: the last released binary as the downstream of the working tree, once a
  release exists.
- A document refused as `size` is taken again only by a retake of its range, not when the site's
  maximum alone is raised.
