---
title: Compatibility
---

# Compatibility

An installed kista is chained to a peer that will be newer than it: its organisation's tenant in a
hosted kista, a gateway, another kista it mirrors. This page says what a kista does with what it
does not know yet, and what a later release may change. The rules are in force from `v0.2.0`. They
hang on each format's own major, not on kista's release number. The design is in
[spec 0046](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0046-forward-compatibility/spec.md).

For the database schema between releases, see [Upgrading](administration/upgrading.md).

## The five rules

1. **A wire format has a major.** `api` on the dash routes (`-/names`, `-/revocations`), `format`
   in a bundle's plan and manifest, `v` in a licence and in a statement, a document's media type.
   Major 1 stays readable. It is retired only by the format's major 2, which is a new route or a
   new marker value, never a changed meaning under the old one.
2. **Within a major, unknown members are ignored.** A producer adds a member only when a reader
   that ignores it grants, serves and shows no more than a reader that understands it. A member
   that narrows, replaces or conditions a known one is a new major. A producer writes the lowest
   major that carries what it says.
3. **A higher major, or an unknown value of a known member, makes that one item wait.** It is not
   applied, not guessed at and not dropped; the rest goes on. Where the surface has a floor (below),
   the item falls to the floor instead.
4. **Verify first.** A signature is checked over the exact bytes before anything is read for
   meaning. A few members are frozen: the same name, type and meaning in every major, so a reader
   can file an item it cannot read.
5. **An unreadable item is not an absent one.** Where absence means "remove", an item the reader
   cannot read removes nothing it may name. What can only take away (a withdrawal, a revocation) is
   applied in its mildest known form.

Nothing unknown is ever applied or shown. What waits leaves a count and a reason word from a closed
list, to administrators.

## What a kista does, by surface

| It receives | What it does |
| --- | --- |
| A [statement](attachments.md#signed-statements) with a member it does not know | reads it as before; the member is ignored |
| A statement of a higher `v` | leaves it **waiting** (`newer`). A withdrawal is applied: it can only remove. One about a document it holds, newer than what it holds, of the same body and not for `readers`, narrows the document to administrators first (`narrowed` in the run): its audience is then administrators, and no administrator widens it, until a statement this kista reads says who may see it |
| A statement of a kind it does not know | waiting (`unknown kind`), unless the feed's own `kinds` leave the kind out: then it is refused, as any kind the administrator left out |
| A known kind with a media type the kind does not take here | waiting (`media type`) |
| A known kind with another spelling of a media type it takes (a parameter, another case), or with what is no media type | refused for good (`media type spelling`): a statement names the canonical form |
| A document in a version it does not read (CycloneDX other than 1.x, SPDX other than 2.x, SPDX 3 as JSON-LD too, SARIF other than 2.1.0, a review with a `verdict` it does not know) | waiting (`document version`) |
| An `audience` other than `readers` | the document is stored for administrators |
| An `origin` it does not know | stored and shown as text (printable, at most 80 bytes) |
| A statement that does not verify, or breaks the member-name rule | the body is held, as before (`signature`) |
| An authentic statement with a known member of the wrong type or out of its bounds, or `null` in a member it does not know | refused for good (`statement`): its body's cursor moves past it when its `issuer`, `body_hash` and `seq` can be read; when they cannot, no cursor can move, and it is refused again whenever the source sends it |
| A `kista.review.v1` document with a member it does not know | read, at a feed and from a bundle; the attach route stays strict |
| A [revocation](upstreams.md#revocations-from-a-kista) in a state it does not know (an empty or missing one too) | a yank, the source's word before its reason; it never lowers a stronger state stored for a row the source still listed, and forgets nothing |
| `-/names` or `-/revocations` answering an `api` above 1 | the upstream's run fails with its reason; nothing is read, stored or forgotten. Switching the upstream's revocations on, or giving it `*`, is refused with the same reason |
| A [licence](licensing.md) of a higher `v` | refused (`v`). In the relay, the licence stored for its name stays until its own expiry and grace; a name left out is still removed |
| An SDK usage report with a `status` it does not know | counted under `other` |
| A [bundle](upstreams.md#bundle-mode)'s plan, manifest or batch with a member it does not know | reads it as before; the member is ignored |
| A plan or a manifest of a higher `format` | refused whole, saying that the kista reading it is the one to upgrade ("this kista is older than the bundle's writer"): the manifest binds every file |
| A bundle's source of a package type it does not know | left out with a problem, at an import and (a plan's) at an export; nothing of it is read, its entries are read through without counting toward the cap on entries read through (the bundle's whole size is bounded by `bundles.max_size`, for an upload and for what `bundle import` reads), and the rest is taken |
| A bundle's cell with an outcome it does not know | that cell's problem; what the cell holds stays. Of the cell only its file's name is read, to read that entry through |
| A bundle's licence of a higher `v`, at `kista bundle sign` | skipped by itself, named in a line; the bundle is signed |
| A request to `POST …/feed` with a member it does not know | answered as before; with `issuer` (a relay's selector) it answers no statements |

`.well-known`, the attestation keys file, the feed's and the licence relay's answers, extension
files, a bundle's tar, a licence's payload members and forwarded usage were tolerant before and
need nothing.

### Waiting, and taking again

A waiting statement is passed over: the feed's cursor moves on, later statements of the body flow,
and the feed stays `ok`. The feed shows it to administrators, in the console and in
`kista admin feed show`:

```text
This kista is older than its source: documents wait for an upgrade (`unknown kind`).
```

Every binary has a **reader level**, raised whenever it learns a kind, a media type, a document
version or a statement major. A feed's cursor remembers the lowest `seq` it passed over and the
level of the binary that did. Before a feed's run, and when a bundle's plan is written, a binary of
a higher level takes the cursor back to just below that `seq`, with no reset by the administrator.
The replay judges the whole range again by today's settings: a document refused for its size then
is taken now if it passes now. What is stored stays as it is.

- An older replica in a rolling upgrade retakes nothing.
- The count may overstate until the upgrade: a waiting document withdrawn at the source since is
  still counted. It says that something waits, not how much will arrive.
- A cursor reset (the feed's `export` switched on, `bundle reset -feed`) deletes the waiting state
  with the cursor.
- A plan takes the cursors back only once it is whole. A bundle import that was running when a
  plan took its feed's cursor back fails that feed's target: import a bundle of the new plan.
- The reasons shown are each body's last one.

An [export feed](attachments.md#feeds-for-air-gapped-sites) keeps a waiting statement as it keeps a
refusal, and a bundle carries it without content, its note the waiting reason. A site that can read
it leaves it waiting too, and asks for it again with every plan until the gateway is upgraded and
carries the content. With any other note a statement without content is refused for good, as
before.

Here it is the exporting kista that is older, and the site says so apart, in `feed show`, the
console and the feed's record (`waiting_without_content`):

```text
The exporting kista is older than its source: 2 statements arrived without content (`unknown kind`).
```

The cost is the gateway's to end: until it is upgraded, every plan asks each such body's range again
from its first waiting statement, and every bundle carries that range again.

A body's row has one level, the lower. A body with both kinds of wait (a statement this kista
cannot read, and one that arrived without content) is shown whole under the exporter's line until
the exporter carries the content; this kista's own line shows the bodies that have only its own.

### The member-name rule

In a statement, a plan, a manifest and a batch, every member name at every depth is
`[a-z0-9_]{1,64}`, and none is twice in its object. This is what makes ignoring unknown members
safe: a JSON reader that matches names without regard to case, or keeps the last of two, would
otherwise read `Kind` beside `kind` differently from another reader. A statement that breaks the
rule does not verify; a plan or a manifest that breaks it is refused.

A statement's numbers are integers, in every member, known or not: a fraction or an exponent is not
the canonical form, and the statement does not verify. A member a reader does not know carries
integers, strings, booleans, arrays and objects: never `null`.

### The frozen members of a statement

`v`, `issuer`, `key`, `issuer_id`, `seq`, `body_hash`, `withdrawn` and `audience` (`readers`, or
anything else: not readers) keep their name, type and meaning in every major, with RFC 8785's
canonical form and the signature's domain `kista-statement-v1` (which separates statements from
kista's other signatures, and is not the format's version). `v` is absent in major 1.

Within major 1, two more bounds are frozen: `kind` is `[a-z][a-z0-9-]{0,15}` and `media_type` is 1
to 200 bytes. A `v0.2.0` reader refuses a statement outside them for good, so a later kind or media
type stays within them.

## What a later release may change

| Interface | A later release may | Never, within the major |
| --- | --- | --- |
| Wire and signed formats, bundles | add a member by rule 2; add a route; add a value where the reader waits or has a floor | change a frozen member, the canonical form or what is signed; narrow by an added member; drop major 1 |
| Admin API, CLI (`-json`, exit codes), events | add | remove, rename, change a meaning |
| Config file | add a key whose default keeps behaviour | rename or remove a key; accept an unknown one |
| Database schema | add migrations; raise `min_reader`, named in the [upgrade notes](administration/upgrading.md) | edit an applied migration |
| Console mount contract | add optional options and exports | change `contract` |

- **A client of the API ignores unknown response members.** The server refuses unknown request
  members on its management routes, so a newer client fails loudly instead of dropping what was
  asked.
- **The config file stays strict.** A misspelt security key must not start a server without it.
  There is no version field: there is nothing to switch on.
- **Rule 2 is a promise, not a check.** A producer that narrowed by an added member in major 1
  would be over-applied by an older reader, and no reader can detect it. The frozen `audience`
  bounds what that could expose.
- Each row is held by a test: the wire fixtures (`testdata/wire/1/`: what `v0.2.0`'s own producers
  wrote on the dash routes, the feed and a bundle, which every later release reads and must still
  write every member of), the strict config loader, `min_reader`, the console's `contract`
  constant.

The trial tags `v0.1.0` to `v0.1.4` are outside these rules.
