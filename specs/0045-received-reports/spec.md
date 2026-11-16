# Spec 0045: Received reports: the `report` kind, the feed over catalogue builds, and documents handed downstream

- **Status**: draft (revised 2026-11-16 after three reviews and the owner's answers)
- **Date**: 2026-11-16
- **Author**: vgsml, Claude

## Summary

An organisation's own kista shows, next to a version that waits for a decision, what an analyser
found in it: a **report** (the issuer's own page and a short summary, no verdict), its SBOM and
its known vulnerabilities. The analyser is elsewhere (Enterest, or anyone) and the own kista
usually cannot reach it: it mirrors a channel of the organisation's tenant on another kista. On
top of spec 0011's attachments and feed and spec 0014's `export`, this spec adds:

1. a kind **`report`**: one signed JSON document holding a few summary members kista reads and an
   HTML page kista never interprets, shown in an isolated frame;
2. the default feed offer over **catalogue builds** (spec 0033), released or not;
3. **handing received documents downstream online**: what a bundle already carries (spec 0014),
   answered on the feed route, with the issuer's statements unchanged and the issuer's key pinned
   downstream;
4. vulnerabilities summaries that say what was not checked, and the summary on the waiting
   version's row.

Every piece is for any source; kista does not depend on Enterest. It is part of the first real
release, v0.2.0, so a kista installed before Enterest launches connects later by configuration
only. Unknown kinds, members and versions are spec 0046's.

## Problem

The path: an issuer → the organisation's tenant (on a kista the issuer's operator hosts) → the
organisation's own kista, which mirrors a channel of that tenant (specs 0009, 0032, 0039). Checked
in the code:

1. **The feed asks only about released bodies**: `internal/store/feeds.go:483-495`. A catalogue
   build no channel has taken, the one "Needs a decision" asks about (spec 0041), gets no
   documents. And a placement of a publisher's build has origin `placement`
   (`internal/release/place.go:228-231`), so the default offer never names a distributor's builds
   at all.
2. **Received documents stop at the first kista, online**: `internal/attachment/service.go:280-281`,
   `feed.go:256-258`, `specs/0011-attachments/spec.md:388-389`. Bundles already carry them (spec
   0014 phase 1b, the feed's `export`, `internal/upstream/exportfeeds.go`).
3. **No kind fits a report.** `review` requires a verdict (`internal/attachment/kinds.go:474-485`);
   a report has none. The kinds are listed in `kinds.go:40-49`, `client.go:124-131`,
   `internal/bundle/feeds.go:89-96`, `web/console/src/screens/Feeds.tsx:37` and `Reports.tsx:12-23`
   (the schema's list is spec 0046's to drop).
4. **The console shows nothing on the row**: a drawer with the name's generic Reports block
   (`web/console/src/screens/Decisions.tsx:116`, `249-256`), and a document is only downloaded.
5. **Informational advisories count as findings** (`info`, `none` → `low`, `kinds.go:257-272`),
   and a document that checked no component reads "no findings" (`kinds.go:293-329` reads no
   component; `Reports.tsx:41`).
6. **A full document quota loses documents for good**: a refusal for the tenant's limits moves the
   cursor (`internal/attachment/client.go:1047-1049`, `567`). Daily vulnerabilities documents per
   body make that reachable.
7. **No grant fits an own kista as provisioned today**: the feed needs `feed`
   (`internal/authz/authz.go:64`, `339-343`), and a credential that only installs must not get it.

## Design

### The `report` kind

Kind `report`, media type `application/vnd.kista.report+json`: one JSON object, attached and fed
as any document (spec 0011). It carries no verdict; kista computes none. `review` stays unchanged.

| Member | Type and bound | Read for |
| --- | --- | --- |
| `envelope` | `kista.report.v<N>`, N 1 to 2^31 | the version, shown |
| `report` | 1 to 128 bytes of `[A-Za-z0-9._:-]` | the report's id at its issuer, kept whole |
| `issue` | integer, 0 to 2^31 | shown; sorts heads a producer did not chain |
| `issued_at` | RFC 3339 | shown |
| `depth` | 1 to 32 bytes of `[a-z0-9_.-]` | how far the analysis went, shown on every row |
| `subject.bodies` | 1 to 1,000 of `{hash: 64 hex, read: boolean}` | the body attached to must be listed; whether it was read |
| `attention` | at most 32 of `{kind: 1 to 32 bytes of [a-z0-9_.-], count: 0 to 2^31}` | points of attention by kind |
| `kinds_evaluated` | integer, 0 to 2^31, optional | "none" differs from "not evaluated" |
| `sections` | at most 16 of `{name, status}`, each 1 to 32 bytes of `[a-z0-9_.-]`, optional | coverage |
| `html` | string | the issuer's page; never parsed |

- **These members are the kind's floor, frozen for every envelope version**: name, type, bound and
  meaning. kista reads nothing else and interprets no version, so a newer envelope is stored and
  shown exactly as `v1` is, and nothing is recomputed after an upgrade. (Spec 0046's "document
  version" wait never applies to this kind.)
- **Everything else is carried unread.** Members are read by exact name; a document that repeats a
  read member, in any letter case, is refused.
- **`html`** is a self-contained page: no scripts, no external resources. kista checks only that it
  is a string. Its size is the document's, bounded by `attachments.max_size`.
- **Checks at intake** (the same code for an attach, a run and an import): spec 0011's JSON checks;
  the members above by their types and bounds; the attachment's body hash is one of
  `subject.bodies[].hash`. A failure is `400`, or the refusal `document`.
- **The summary** is those members without `html`, with `bodies` (their number) and `read` (this
  body's) in place of `subject`. The bounds make its largest form fit 4,000 bytes; nothing is cut.
  Strings have a grammar, so none needs scrubbing.

### Issues

There is no ordering rule of its own. A report's issues are spec 0011's chain: each issue
`supersedes` the previous issue's document of the same body, and `attachment.Heads` gives what is
shown, hiding a withdrawn head and what it superseded. `issue` is shown; where a producer did not
chain, the console sorts a body's heads by it and shows the highest first.

The limit, stated: a kista that first receives a body's documents after its highest issue was
withdrawn never sees the withdrawn one's chain, and shows the lower issue as current. So a source
retracts with a **higher issue** (a correction), never with a bare withdrawal of the highest.

### The default offer

`upstreams` is redefined; there is no third offer, no schema change and no flag. It names the
bodies of the tenant's catalogue builds that **came from outside**: source `upstream:…`, or a
distributor's publisher (live, or among `removed_distributor_publishers`), not withdrawn, released
or not; and never a body that a build of the tenant's own has (`tenant`, or a publisher that is
not a distributor's, a removed one included). `revoked_at` changes nothing here: a revoked entry
that remains is one a channel still serves. `channels` is unchanged. A bundle plan takes its
bodies from the same query (spec 0014).

A kista source's scope is unchanged: bodies released in the channels the caller's `feed` grant
covers. An own kista mirrors only what its tenant released, so that is enough.

### Handing received documents downstream

**The switch is the feed's `export`** (spec 0014), which keeps what it keeps today. Its meaning
widens: this source's documents may leave in a bundle **and online, to every holder of `feed`**
(those who hold it already included). The console's row and form keep the control; its text
becomes "Hand on: to bundles and to kistas that take this tenant's feed", and setting it asks for
confirmation naming the source and saying that every holder of `feed` on a channel will receive
its documents.

**The relaying tenant**, two routes (`issuer`: a feed URL in spec 0011's normalised form, else
`400`):

```text
POST /api/v1/tenants/{t}/feed  {issuer, bodies: [{body_hash, after_seq}]}
                               →  {keys, statements: [{statement, signature, note?}], bodies, more}
GET  /api/v1/tenants/{t}/feed/documents/{issuer_id}?issuer=<url>   →  the content
```

- Authorised by `feed`, scoped to the grant's channels, as today. An `issuer` member is one an
  older subscriber never sends (spec 0046).
- Answered only for an issuer of which the tenant has an online `export` feed with a keys file.
  Any other issuer, and any body out of scope, answers as a body without documents (the content
  route: as a missing path).
- `keys` is that issuer's keys file as the feed last fetched it; there is no keys route.
- Per body, from `after_seq` to the feed's cursor, in `seq` order: the stored statement bytes and
  signature of each document held from the issuer, withdrawals, and what the export feed keeps
  (spec 0014). Content is served only for documents of kinds an `export` feed of that issuer
  takes; every other statement carries the one fixed `note`, "not held by the relay". Statements
  that wait are carried as spec 0046 says for an export feed.
- Each statement is verified against the relaying feed's own pinned keys before it is sent; the
  relay applies no kind filter of its own and signs nothing.
- **Shared with the plan-driven export** (`exportfeeds.go`): the read of what a tenant holds from
  an issuer for a body and a range (documents, kept statements, refusals, their order at one
  `seq`), moved to `internal/attachment` (`internal/bundle` imports it, not the reverse). **New**:
  choosing the feed without a plan, verifying against its pins rather than a site's, and paging.
  An answer holds at most `attachments.feed_answer_max` bytes (new; 1 MiB, the most a client
  reads, by default and at most), the keys file included, then `more`.
- **A suspended tenant serves nothing on the feed routes, as today**: the own kista's feed turns
  `unauthorized`, keeps what it holds, and no withdrawal arrives meanwhile. The end of an
  entitlement that removes the tenant's feed ends the relay the same way.

**The downstream's feed** gains `via` (the relaying tenant's URL), set at creation only, like
`mode`: never changed or cleared.

- `url` stays the issuer's identity and is never contacted. `via` passes the feed URL's checks,
  egress and `egress.hosts` (spec 0044); the credential is matched against it and sent only there.
- A run sends `issuer` and its bodies to `via`, keeps from `keys` only the keys it pins, checks
  every statement as spec 0046's `Verify` and spec 0011's client do, and fetches content from
  `via`. A statement with a note is verified, then refused `no content`; the note is bounded and
  cleaned as a bundle's is (`bundle.Text`).
- A body's cursor moves no further than **that body's own verified statements**, as a bundle
  import's does (`internal/attachment/bundleimport.go:290-300`); the relay's unsigned high-water
  moves nothing.
- `failing` when `keys` holds none of the pinned keys: "none of the pinned keys is in what the
  relay serves; take the issuer's current fingerprint from the issuer". **No fingerprint from the
  relay is ever shown**, in a status or in the form.
- With `export` it hands on in turn; a chain has any length.

**The grant** is the existing `feed` verb on the mirrored channel, given to the own kista's
credential beside `install`. Nothing changes in `internal/authz`; `install` never implies it.

**Storage** stays one record and one object per body (spec 0011).

### Limits

- **The document quota holds, it does not refuse.** A document that does not fit the tenant's
  quota holds its body's cursor and the run ends `failing` (`limits`), as `errHeldByQuota` does for
  what an export feed keeps. The quota is one pool per tenant; only the heads limit is per issuer.
- On an export feed, a kept refusal replaces the earlier kept row of the same `issuer_id`.
- **`attachments.max_size` stays 4 MiB by default**; the recipes for a tenant's and an own kista's
  report feeds set it higher (at most 16 MiB). A run buffers each document whole, outside
  `attachments.max_reads`: memory is about `max_size` × (`max_reads` + the feeds running at once),
  and more while a document is summarised. The quota counts documents, not bytes. A download has
  two minutes (`client.go:853`), so a 16 MiB document needs about 140 KiB/s.

### Vulnerabilities summaries

- `info` and `none` are `informational`, a count outside `findings`.
- In a CycloneDX document a component is **checked** only when it carries the property
  `kista:vulnerabilities:check` exactly once with the value `checked`. The summary gains
  `components` (all of them), `checked` and `not_checked` (marked so).
- Console, two rules: never "no findings" while a component is unchecked or unmarked, but
  "N of M components checked"; a count that reached the list bound says "at least".
- Freshness is the statement's `valid_until`: the 30-day flag shows only when there is no
  unexpired one.
- Mapping a producer's own marks (Enterest's `enterest:` properties, its informational advisories
  that carry ratings) onto these is the source's job, and lossy.

### The console

- **The row** of a waiting version in "Needs a decision", from one read, the existing list with
  `?source=&version=&head=true`:
  - it begins with who stands behind the report, from the record, never from the document:
    "Report from <issuer host>, signature verified" or "The tenant's own report, attached by
    <principal>"; a feed's and the tenant's are separate lines, never merged;
  - then issue, date, `depth`, attention kinds with counts, sections not `analysed`, whether this
    build was read; open strings are labelled and quoted;
  - no attention with `kinds_evaluated` 0 or absent reads "not evaluated", never "no points of
    attention";
  - the vulnerabilities line by the rules above;
  - "No report held" only after a successful empty read, with the feed's last success;
  - where a statement waits for the body (spec 0046's waiting count), the row says so;
  - no line at all for a caller who cannot read documents.
- **The page**: the console takes `html` from the document it downloads and shows it in an
  `iframe` with an empty `sandbox`, under a policy on the framed content of `default-src 'none'`
  with inline styles and `data:` images only, and with no frame source allowed by the console's
  own policy; inside a border labelled "<issuer host>'s page". "Download" saves it as `.html`, for
  printing. Other kinds are downloaded only, as before.

### API, configuration, data model

- `kind=report` wherever a kind is given; in code it joins `attachment.Kinds`, the media-type
  table, the console's lists and the CLI's help. Nothing for kinds in the schema.
- `via` on `POST …/feeds` and `kista admin feed add -via <url>`; `feed show`. The relay's two
  routes. `head=true`, `source=` and `version=` on the attachments list.
- `attachments.feed_answer_max`. Events: `feed.add` carries `via`.
- Migration `0055` (spec 0046 owns `0054`), three dialects: `feeds.via` (`varchar(1000)` NULL;
  SQL Server `nvarchar`, BIN2). `min_reader` is raised: an older replica would run a `via` feed
  against the issuer's URL.
- Docs with each PR: `attachments.md`, `api.md`, `upstreams.md`, `console.md`, the own-kista page.

## What a source must do

1. `"envelope": "kista.report.v1"` and the floor's members within their bounds; `html`
   self-contained (no scripts, no external resources); every body of a statement in
   `subject.bodies`.
2. One statement per document and body; each issue `supersedes` the previous one of the same body;
   a retraction is a higher issue; audience `administrators`; `valid_until` on what ages.
3. `kista:vulnerabilities:check` on every component; the whole document within the receiver's
   size, not only its findings.
4. A feed URL of the shape `…/api/v1/tenants/<name>`, which is the statements' `issuer`.
5. Key fingerprints published outside any relay, and rotations announced ahead.
6. A credential of its own for an own kista, with `feed` on the channel; and no `kinds` filter on
   the report feeds it provisions (an explicit filter refuses a later kind for good).

## Security

- **Integrity.** A relay passes the issuer's stored bytes; the downstream verifies each statement
  against keys its administrator pinned, the content against the statement, and a report against
  its own body list. A relay, a gateway or a bundle cannot alter, retitle or re-bind a document.
- **Keys.** Pins come from the issuer, out of band. kista shows no fingerprint a relay sent, so a
  relay cannot talk an administrator into pinning its key. A rotation stops documents until the
  new key is pinned.
- **A relay can withhold**, and what it omits is omitted for good: the cursor passes it. It comes
  back only by resetting the feed's cursors: remove the feed keeping its documents, add it again.
  kista shows the issue, its date, `valid_until` and the feed's last success, and promises no more.
- **A lower issue can show as current** in the case under "Issues".
- **What a `feed` holder learns**: the documents of bodies released in its channels, and which
  issuers the tenant hands on (an issuer that is handed on answers with `keys`). Nothing of other
  bodies, of sources not handed on, or without `feed`.
- **Leaving a tenant** takes `export` on the source and a `feed` grant. Setting `export` widens
  what every existing holder receives; the confirmation says so. Both are audited.
- **What a source learns** from the default offer: the hashes of every build the tenant received
  from outside, undecided ones and other upstreams' included; never its own builds.
- **The issuer's page.** It runs nothing and reaches nothing: an empty `sandbox` gives it a unique
  origin without scripts, forms or top navigation; the content policy loads nothing external; the
  console allows no frame source, so a link navigates nowhere; kista never parses it. A browser
  test proves each. Not promised: what the page says. It is the issuer's word, inside a border
  that names the issuer from the record.
- **Summaries**: read members have fixed names, grammars and bounds; duplicates are refused; the
  console shows them as quoted text.
- **Tenant isolation, SSRF, credentials**: as specs 0011 and 0044; `via` is the only host
  contacted.
- **Bounds**: an answer by `attachments.feed_answer_max`; documents by the tenant's one quota,
  which a single source can fill: the run then holds and says `limits`.

## Testing

- **Unit**: the envelope (each member's type and bound; a duplicate in another case; unknown
  members; a higher version stored alike; a body not listed; the largest summary under 4,000
  bytes); the vulnerabilities rules; the console's row texts and the frame (browser test: no
  script, no request, no navigation, no form).
- **Store suite, three dialects**: the offer (upstream, live and removed distributor's publisher,
  the tenant's own and a removed own publisher, withdrawn, revoked, unreleased); `feeds.via`; the
  list filters; a kept refusal replaced.
- **Feed**: the relay's scope and equal answers; content by kind; paging; a suspended tenant; a
  `via` client against a fake relay (altered statement or content, unpinned key, a high-water
  without statements, a note); the quota holding a cursor.
- **End to end**, two kistas and a stand-in issuer: a report reaches A for an unreleased build and
  shows on the row; with `export` and a `feed` grant it reaches B through A while B never contacts
  the issuer; a correction supersedes; A's stored bytes altered: B refuses; the issuer rotates:
  B is `failing`, and proceeds once the fingerprint taken from the stand-in issuer is pinned;
  A suspended: B `unauthorized`, documents kept; own kista → bundle → site.

## Changes to earlier specs and docs

| Where | What becomes false |
| --- | --- |
| 0011:79, `attachments.md`:34-44 | the list of five kinds |
| 0011:244-246, `attachments.md`:379-382 | the default offer is released bodies |
| 0011:212-213, 388-389, `attachments.md`:274-276 | a feed's documents are not relayed |
| 0011:250-256, `attachments.md`:370 | content and keys come from under the feed's URL |
| 0011:264-266 | a limit is a permanent refusal (the document quota now holds) |
| 0011:84, `Reports.tsx`:41 | `info` and `none` as `low`; "no findings" |
| 0011:292-297, `console.md`:183-184, `Reports.tsx`:7-9 | a document is never rendered (a report's page is shown, framed) |
| 0014:721-744, `attachments.md`:377, `upstreams.md`:926-929, `Feeds.tsx`:232 | `export` is only for air-gapped sites |
| 0044:36, 64, 73, 191 | the feed URL's host is checked at add and every run (a `via` feed checks `via`; its issuer URL is never contacted) |
| 0041:63, 539-540 | where reports are shown |

## Alternatives considered

- **A verdict-less `review`**: absence would read as approval somewhere.
- **kista rendering a structured report**: it would freeze a producer's model into kista; the
  issuer's page changes without a kista release.
- **Re-signing by the relay**: the tenant's key would vouch for the issuer's words.
- **A second switch beside `export`**, or **routes under the channel's prefix** (spec 0039): a
  setting to forget, or the feed's client built twice with `install` reading documents.
- **A third offer, an issue rule, a keys route**: removed in review; spec 0011's parts do the work.

## Follow-ups

- A comparison of two reports held.
- One stored copy for a report that covers many bodies.
