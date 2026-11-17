# Spec 0045: Received reports: the `report` kind, the feed over catalogue builds, and documents handed downstream

- **Status**: implemented
- **Date**: 2026-11-16
- **Author**: vgsml, Claude

## Summary

An organisation's own kista shows, next to a version that waits for a decision, what an analyser
found in it: a **report** (the issuer's own page and a short summary, no verdict), its SBOM and
its known vulnerabilities. The analyser is elsewhere (Enterest, or anyone) and the own kista
usually cannot reach it: it mirrors a channel of the organisation's tenant on another kista. On
top of spec 0011's attachments and feed and spec 0014's `export`, this spec adds:

1. a kind **`report`**: one signed JSON document holding a few summary members kista reads and
   the issuer's page, semantic HTML kista never interprets, shown in an isolated frame under the
   console's own stylesheet;
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
| `subject.bodies` | 1 to 1,000 of `{hash: 64 lowercase hex, read: boolean}` | the body attached to must be listed; whether it was read |
| `attention` | at most 32 of `{kind: 1 to 32 bytes of [a-z0-9_.-], count: 0 to 2^31}` | points of attention by kind |
| `kinds_evaluated` | integer, 0 to 2^31, optional | "none" differs from "not evaluated" |
| `sections` | at most 16 of `{name, status}`, each 1 to 32 bytes of `[a-z0-9_.-]`, optional | coverage |
| `html` | string | the issuer's page; never parsed |

`envelope`, `report`, `issue`, `issued_at`, `depth`, `subject.bodies` and `html` are strict: a
document that breaks one is refused.

- **These members are the kind's floor, frozen for every envelope version**: name, type, bound and
  meaning. kista reads nothing else and interprets no version, so a newer envelope is stored and
  shown exactly as `v1` is, and nothing is recomputed after an upgrade. This is the floor spec
  0046 ("Statements") lets a kind declare: a report never waits on a `document version`. Adding
  the kind raises spec 0046's reader level.
- **Everything else is carried unread.** Members are read by exact name; a document that repeats a
  read member, in any letter case, is refused.
- **`html`** is semantic HTML only: headings, paragraphs, lists, tables, code, `details`; no
  style, no script, no external resource. kista checks only that it is a string. Its size is the
  document's, bounded by `attachments.max_size`.
- **A summary list that breaks its rule does not lose the report.** An `attention` or `sections`
  entry, name or count outside its grammar or bounds, or a `kinds_evaluated` that is not such an
  integer, drops that member from the summary and sets `incomplete`; the report is stored and
  shown, and the row says "summary incomplete: open the report".
- **Checks at intake** (the same code for an attach, a run and an import): spec 0011's JSON checks;
  the strict members by their types and bounds; the attachment's body hash is one of
  `subject.bodies[].hash`. A failure is `400`, or the refusal `document`.
- **The summary** is those members without `html`, with `bodies` (their number) and `read` (this
  body's) in place of `subject`. The bounds make its largest form fit 4,000 bytes; there is no cut rule.
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
  (spec 0014; spec 0046, "Export feeds and relays": a waiting statement is kept as a refusal is).
  Content is served only for documents of kinds an `export` feed of that issuer takes. Every other
  statement goes without content, with a `note`: for a statement the relay waits on, its kept
  reason, one of spec 0046's waiting reasons; for the rest (removed or refused here, or of a kind
  the export feed does not take) the fixed text "not held by the relay".
- Each statement is verified against the relaying feed's own pinned keys before it is sent; the
  relay applies no kind filter of its own and signs nothing.
- **Shared with the plan-driven export** (`exportfeeds.go`): the read of what a tenant holds from
  an issuer for a body and a range (documents, kept statements, refusals, their order at one
  `seq`), moved to `internal/attachment` (`internal/bundle` imports it, not the reverse). **New**:
  choosing the feed without a plan, verifying against its pins rather than a site's, and paging.
  An answer holds at most `attachments.feed_answer_max` bytes (new; 128 KiB to 1 MiB, 1 MiB by
  default: the keys file may be 64 KiB, and a client reads no more than 1 MiB), the keys file
  included, then `more`.
- **A suspended tenant serves nothing on the feed routes, as today**: the own kista's feed turns
  `unauthorized`, keeps what it holds, and no withdrawal arrives meanwhile. The end of an
  entitlement that removes the tenant's feed ends the relay the same way.

**The downstream's feed** gains `via` (the relaying tenant's URL), set at creation only, like
`mode`: never changed or cleared.

- `url` stays the issuer's identity and is never contacted. `via` passes the feed URL's checks,
  egress and `egress.hosts` (spec 0044); the credential is matched against it and sent only there.
- A run sends `issuer` and its bodies to `via`, keeps from `keys` only the keys it pins, checks
  every statement as spec 0046's `Verify` and spec 0011's client do, and fetches content from
  `via`. A statement with a note is verified, then **waits** (`no content`, spec 0046) only when
  the note is one of spec 0046's waiting reasons, and is refused `no content` for good otherwise,
  as a bundle's is today; the note is bounded and cleaned as a bundle's is (`bundle.Text`). A
  bundle import follows the same rule.
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
- On an export feed, a kept refusal replaces the earlier kept row of the same `issuer_id`
  (`feed_refusals` gains the column, below).
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
  - no line at all for a caller who cannot read documents.
- **The page.** The console takes `html` from the document it downloads and builds the frame's
  `srcdoc` as a fixed prefix followed by `html`, with no parsing. The prefix is a `meta`
  Content-Security-Policy (`default-src 'none'; style-src <the console's report stylesheet URL>;
  img-src data:; form-action 'none'; base-uri 'none'`) and a link to that one stylesheet of
  kista's. The `iframe` has an empty `sandbox` and a border labelled "<issuer host>'s page". The
  page so follows the console's light and dark theme. The console's own header
  (`web/console/console.go:183-194`) only gains `frame-src 'none'`.
- **Download** (not offered for a page whose `html` contains `http-equiv`: see the implementation
  notes) is the print path (a sandboxed frame cannot print itself): the same prefix with
  kista's stylesheet inlined and `style-src 'unsafe-inline'` in the `meta`, then `html`, saved
  through the existing `save()` (an octet-stream blob with the `download` attribute). kista never
  serves `text/html`. Other kinds are downloaded only, as before.
- **A host that embeds the console** (spec 0015) sets `frame-src 'none'` in its own policy, or one
  that admits no navigation of the report's frame: in the embedded mount the host's policy
  applies, not kista's.

### API, configuration, data model

- `kind=report` wherever a kind is given; in code it joins `attachment.Kinds`, the media-type
  table, the console's lists and the CLI's help. Nothing for kinds in the schema.
- `via` on `POST …/feeds` and `kista admin feed add -via <url>`; `feed show`. The relay's two
  routes. `head=true`, `source=` and `version=` on the attachments list.
- `attachments.feed_answer_max`. Events: `feed.add` carries `via`.
- Migration `0055` (spec 0046 owns `0054`), three dialects: `feeds.via` (`varchar(1000)` NULL)
  and `feed_refusals.issuer_id` (`varchar(64)` NULL, indexed with `feed_id`; written with every
  row kept from now on, so replacing by `issuer_id` is a store operation; a row kept before has
  none and is replaced by none);
  SQL Server `nvarchar`, BIN2. `min_reader` is raised: an older replica would run a `via` feed
  against the issuer's URL.
- Docs with each PR: `attachments.md`, `api.md`, `upstreams.md`, `console.md`, the own-kista page,
  `configuration.md` (the new key), `administration/upgrading.md` (schema 55, `min_reader`).

## What a source must do

1. `"envelope": "kista.report.v1"` and the floor's members within their bounds; `html` semantic,
   with no style, no script and no external resource, and without the text `http-equiv` anywhere
   in it (a page that contains it, in any letter case, is shown but cannot be downloaded, so not
   printed); every body of a statement in
   `subject.bodies`; `kinds_evaluated` as an integer (a producer whose own member of that name is
   a list maps it).
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
  issuers the tenant hands on (an issuer that is handed on answers with `keys`). Also: a report
  lists the hashes of its subject's other bodies, and a statement of a kind the export feed does
  not take still arrives with its signed title and summary. No statement or document about any
  other body, nothing of sources not handed on, nothing without `feed`.
- **Leaving a tenant** takes `export` on the source and a `feed` grant. Setting `export` widens
  what every existing holder receives; the confirmation says so. Both are audited.
- **What a source learns** from the default offer: the hashes of every build the tenant received
  from outside, undecided ones and other upstreams' included; never its own builds.
- **The issuer's page.** Closed by the empty `sandbox`: scripts, forms, `meta` refresh, a unique
  origin. Closed by the frame's `meta` policy: every external load the browser test covers (image,
  style sheet, CSS `url()`, font, media, frame, object), and every style sheet, style element and
  style attribute but kista's one stylesheet (the console's own `style-src 'self'` already blocks
  inline styles in the frame, which is why the page carries none). HTML's presentational
  attributes (`bgcolor`, `text`, a `font` element's `color`, `width`, `align`) are not styles to a
  policy and still apply; kista's stylesheet states the colours and backgrounds of the body, tables
  and `font` so that none of them hides or recolours text. Resource hints (`dns-prefetch`,
  `preconnect`) are not verified closed. Closed by `frame-src 'none'`: a link navigating the frame,
  standalone. **Not closed by kista in the embedded mount**: there the host's policy governs, and
  link navigation inside the frame is the host's `frame-src` to block. The download is a file the
  reader opens outside kista, under its own `meta` policy. kista never parses the page. A browser
  test proves each property, standalone and embedded. Not promised: what the page says. It is the
  issuer's word, inside a border that names the issuer from the record.
- **Summaries**: read members have fixed names, grammars and bounds; duplicates are refused; a
  list that breaks its rule is left out and flagged, never shown; the console shows the rest as
  quoted text.
- **Tenant isolation, SSRF, credentials**: as specs 0011 and 0044; `via` is the only host
  contacted.
- **Bounds**: an answer by `attachments.feed_answer_max`; documents by the tenant's one quota,
  which a single source can fill: the run then holds and says `limits`.

## Testing

- **Unit**: the envelope (each strict member's type and bound; a list out of its rule dropped and
  flagged; a duplicate in another case; unknown members; a higher version stored alike; a body not
  listed; the largest summary under 4,000 bytes); the vulnerabilities rules; the console's row
  texts.
- **Browser**, standalone and embedded, one test per property: no script runs, no form submits,
  no `meta` refresh, no external request (image, CSS `url()`, font), no inline style applies,
  kista's stylesheet applies in both themes, a link does not navigate (embedded: under a host
  policy with `frame-src 'none'`), the download opens styled and loads nothing.
- **Store suite, three dialects**: the offer (upstream, live and removed distributor's publisher,
  the tenant's own and a removed own publisher, withdrawn, revoked, unreleased); `feeds.via`; the
  list filters; a kept refusal replaced.
- **Feed**: the relay's scope and equal answers; content by kind; paging; a suspended tenant; a
  `via` client against a fake relay (altered statement or content, unpinned key, a high-water
  without statements, a note that is a waiting reason and one that is not); the quota holding a
  cursor.
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
- **A styled page from the issuer**: the console's policy blocks inline styles in any frame it
  builds, and loosening it for one frame loosens it for the console.

## Implementation notes

What the implementation found, where the text above says less or otherwise. None changes the design.

- **The frame's prefix also names the console's theme**, on the root element
  (`<html data-kista-theme="light|dark">`, one of two fixed words), before the `meta` policy and the
  link. A sandboxed frame does not see the console's theme (it is a toggle, not the system's
  preference), so "the page follows the console's light and dark theme" needs it. The stylesheet
  and the policy are as written.
- **No download for a page whose `html` contains `http-equiv`** (the owner's decision). A policy
  in a `meta` element cannot carry `sandbox`, and no directive of it stops a refresh: a downloaded
  file would follow a `meta` refresh in the issuer's html when opened. So the console offers no
  download, and builds no file, for a page whose raw `html` contains the bytes `http-equiv` in any
  letter case, anywhere (a substring check; the html is not parsed, so a page that only mentions
  the word loses its download too). It says why instead. The frame is unchanged: there the sandbox
  stops the refresh. Any other page downloads as written: the file loads nothing, runs nothing
  and submits nothing; a link in it is a link. `web/console/e2e/report-frame.spec.ts` holds both.
- **A link that navigates the frame** is refused by `frame-src 'none'`: its target is never
  requested, and the browser then shows its own blocked page in the frame until the report is opened
  again.
- **The value that marks a component not checked** is `not_checked`
  (`kista:vulnerabilities:check`): the text names only `checked`.
- **A relay's refusal for good is `no content`; a bundle's stays `bundle: no content`**, as merged.
- **`attachments.feed_answer_max` bounds every answer of the feed route**, the tenant's own too.
- **The offer's catalogue builds** are the builds the catalogue keeps (landed, or released), not
  withdrawn.
- **After the reviews of the implementation** (relay and feeds):
  - With several export feeds of one issuer, a statement the feed that read furthest refused for
    its `kinds` is not answered "not held by the relay" while another export feed of the issuer
    takes the kind and has not read that far: the body's answer ends just below it.
  - At the document quota a run fetches no new document; a held body's later statements that need
    no new record (a withdrawal, a newer statement about a document held) are applied without
    moving the cursor; a run that ended `limits` is tried again at the feed's interval; a bundle's
    dry run counts `would_hold`.
  - Of a relay's note only the fixed words are kept (the waiting reasons, "not held by the relay");
    any other text is recorded as "a note this kista does not know". This narrows "bounded and
    cleaned as a bundle's is" for the online path: the note is unsigned and reaches `last_run`.
  - `attachments.feed_answer_max` counts the keys file as the API writes it; a keys file that
    leaves no room for a statement is not handed on.
  - The relay checks its reads as the bundle export does: a body whose feed's `export` or cursor
    changed while the answer was read is answered nothing.
  - A relayed answer that ends at a statement the feed cannot verify is logged and counted on the
    feed's record (`relay_stopped`, `relay_stopped_at`; migration 0055 holds the two columns).
  - A kept refusal replaces the earlier kept row of the same `issuer_id` and body.
- **After the reviews** (the kind and its summaries):
  - A summary member spelled only in another letter case (`Attention` without `attention`) is not
    read and sets `incomplete`; a strict member spelled so is missing, and the report refused.
  - A document with a second JSON value after the first is refused, for every JSON kind.
  - A vulnerability is `informational` only when every rating it carries is `info` or `none`.
  - A CycloneDX property of another shape leaves its component unmarked, never refuses the
    document. `hash` is 64 lowercase hex.
  - At the list bound a document is never read as fully checked: "N of at least M components
    checked".
- **After the reviews** (the console):
  - A publisher's report is named as a publisher's, on the row and on the page's border; a feed's
    document whose `source_origin` is not `tenant` says who stands behind it at the source.
  - The row's coverage counts against the version's builds as the catalogue holds them: "report
    held for N of M builds; K read". Report lines show `valid_until` or `expired`.
  - "Show the page again" builds the frame anew after a link left the browser's blocked page in
    it. The Reports block orders a body's unchained report heads by `issue`. A feed added with
    `export` set asks for the same confirmation as "Hand on…".
  - The browser suite has a third mode, a page with no policy at all, so that scripts, forms,
    refresh, external requests and inline styles are proved closed by the frame's sandbox and
    `meta` policy alone. The suite builds the frame with the console's prefix code and stylesheet
    in a plain page: the real component, and its mount in a shadow root (the theme read from the
    shadow host), are pinned by unit tests only.
- **A via feed whose relay answers no `keys`** (it does not hand the issuer on) is `failing` too,
  with a text of its own; no fingerprint is shown in either.
- **Tests.** The end-to-end chain runs in `internal/api` (three servers over TLS: issuer, relaying
  tenant, own kista) and in `internal/attachment` (rotation, altered bytes, a chain of two relays);
  the own kista → bundle → site leg is covered by the shared read (`attachment.HeldStatements`) and
  the existing export tests, not by a test of its own. The browser tests run without kista (three
  local servers); the console's Keycloak suite has no report case.

## Follow-ups

- A comparison of two reports held.
- One stored copy for a report that covers many bodies.
