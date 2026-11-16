# Spec 0045: Received reports: the `report` kind, the feed over catalogue builds, and documents handed downstream

- **Status**: draft
- **Date**: 2026-11-16
- **Author**: vgsml, Claude

## Summary

An organisation's own kista must be able to show, next to a version that waits for a decision, what
an analyser found in it: a **report** (what the extension does, with no verdict), its SBOM and its
known vulnerabilities. The analyser is somewhere else (Enterest, or anyone), and the own kista
often cannot reach it: it mirrors from the organisation's tenant on another kista and talks to
nothing else. This spec gives kista the four things that path needs, all on top of spec 0011's
attachments, statements and feed:

1. a kind **`report`**: an open, versioned envelope without a verdict, summarised at intake, whose
   issues are ordered (a lower issue is never shown over a higher one held);
2. a feed that offers a tenant's **catalogue builds** (spec 0033), released or not, so the documents
   are there when a version waits in "Needs a decision";
3. **handing received documents downstream**: a tenant passes the documents it received from a
   source to a kista that takes its feed, with the source's own statements, verified downstream
   against the source's key pinned there; online what spec 0014's bundles already do offline;
4. honest **vulnerabilities summaries** (informational advisories are not findings; components
   that were not checked are said to be not checked), the size a default kista takes, and the
   console: the summary on the waiting version's row, a report view, and a comparison of two
   reports held.

kista stays independent of Enterest: every piece is for any source. The first release carries all
of it, so an on-prem kista installed before Enterest launches later connects to its organisation's
tenant with configuration only. How a released kista treats kinds, fields and versions it does not
know is spec 0046's; this spec says what it needs from it (Follow-ups).

## Problem

The path is: an issuer (Enterest) → the organisation's tenant (a kista tenant, hosted by Enterest)
→ the organisation's own kista, which mirrors a channel of that tenant (specs 0009, 0032, 0039) and
never talks to the issuer. An inventory of the code against that path, each point checked:

1. **The feed asks only about released bodies.** A feed's offer is `upstreams` (bodies with a
   release whose origin is `upstream`, and none released by the tenant itself) or `channels`
   (bodies released in named channels): `internal/store/feeds.go:483-495`. A catalogue build no
   channel has taken is never offered, so its documents never arrive. That is exactly the build an
   administrator is asked about in "Needs a decision" (spec 0041). Worse than the inventory said:
   a placement of a publisher's build has origin `placement` (`internal/release/place.go:228-231`),
   so under `upstreams` a distributor's builds (Enterest's own, published into the tenant's
   catalogue, spec 0033) are never offered at all, released or not.
2. **Received documents stop at the first kista, online.** A feed's document cannot be marked
   feedable (`internal/attachment/service.go:280-281`) and the feed's content route serves only
   the tenant's own (`internal/attachment/feed.go:256-258`); spec 0011 left relaying as a follow-up
   (`specs/0011-attachments/spec.md:388-389`). Bundles are **not** in the same state: spec 0014
   phase 1b already carries what a gateway's feed received, with the issuer's statements and keys
   file, to a site whose bundle-mode feed pins the issuer's key (`internal/upstream/exportfeeds.go`,
   the feed's `export` setting). Only the online path is missing.
3. **Kinds are a closed list, in six places**: `internal/attachment/kinds.go:40-49` (and the feed
   client's check over the same list, `internal/attachment/client.go:124-131`), the database
   (`ck_attachments_kind`, `internal/store/migrations/*/0017_attachments.sql:30`), bundle plans
   (`internal/bundle/feeds.go:89-96`), and the console (`web/console/src/screens/Feeds.tsx:37`,
   `Reports.tsx:12-23`). The one analysis kind, `review`, requires a verdict `approved`,
   `conditional` or `rejected` and refuses unknown fields (`kinds.go:474-485`). Enterest's report
   has no verdict by design (its spec 0018: "no verdict, no gate") and a format that grows.
4. **The console shows nothing on the row.** "Needs a decision" has a Report & licence drawer
   holding the generic Reports block of the whole name (`web/console/src/screens/Decisions.tsx:116`,
   `249-256`): no summary on the waiting version's row, no view of a report, no comparison.
5. **A document is stored per body** (spec 0011: bound to a body, one object per attachment); one
   report covers every build of a commit, so it arrives once per body.
6. **Informational advisories count as findings.** The vulnerabilities summary maps `info` and
   `none` to `low` (`kinds.go:257-272`). An advisory that names no vulnerability (an unmaintained
   crate) then reads "1 low".
7. **"No findings" for a document that checked nothing.** The summary counts vulnerabilities only
   and never reads the components (`kinds.go:293-329`); the console prints "no findings" for zero
   (`Reports.tsx:41`). A document in which every component is "not checked" reads as clean.
   Enterest marks each component checked or not, with the reason, in properties of its own
   namespace (its `internal/advisory/document.go`), which kista does not read.
8. **Sizes do not meet.** A document is at most 4 MiB by default and 16 MiB at most
   (`internal/attachment/service.go:32-36`, `internal/config/gc.go:33-34`, `50-52`; a bundle's
   document entry has the same 16 MiB bound, `internal/bundle/feeds.go:25`). Enterest bounds a
   vulnerabilities document's findings at 16 MiB, with the components on top. A default kista
   refuses such a document for good (`size`).
9. **No grant fits an own kista.** The feed needs the verb `feed`
   (`internal/authz/authz.go:64`); only a tenant-wide `admin` implies it, and a channel's is
   granted explicitly (`authz.go:339-343`). An own kista's credential at an Enterest tenant
   carries the group `install:<channel>` only (Enterest `internal/tokens/exchange.go:92`, `213`),
   and granting `feed` to that group would hand administrators' documents to every person who
   installs.

## Design

### Decisions, in short

| # | Decision |
| --- | --- |
| D1 | A new kind `report`, media type `application/vnd.kista.report+json` (no version in the media type), an envelope `kista.report.v1`. kista reads a fixed set of members, carries every other member unread, and never refuses an unknown member. `review` stays as it is. |
| D2 | A report's identity is (who stands behind it, body, report id); its `issue` orders it. The highest issue held is the current one; a lower or equal issue arriving later is refused; a withdrawn highest issue shows as withdrawn, never the one below. |
| D3 | A third feed offer, `catalogue`, the default for new feeds: every body the tenant received from outside, released or not. |
| D4 | Handing downstream is the feed's existing `export` setting (spec 0014): one switch per source, for bundles and online. Online, the tenant answers its feed route for a named issuer with what a bundle would carry. The downstream's feed keeps the issuer's URL as its identity and gains `via`, the tenant it fetches from. Statements are unchanged. |
| D5 | The downstream's grant is the existing `feed` verb on the channel it mirrors, given to a principal of its own; `install` never implies it. No change to authorization. |
| D6 | Storage stays one record and one object per body. Content shared between bodies is a follow-up that changes no wire format. |
| D7 | Vulnerabilities: `info` and `none` are `informational`, outside `findings`; components marked with the property `kista:vulnerabilities:check` are counted checked and not checked; the console never prints "no findings" over components that were not checked. |
| D8 | `attachments.max_size` defaults to 16 MiB, the bound of the feed's and the bundle's formats; the setting only lowers it. |
| D9 | The console computes the row's summary and the comparison from summaries; the report view shows the document's `text` as plain text. One list filter is the only new read. |

### The `report` kind

A **report** says what an analysis found in an extension's version, for a reader who decides. It
carries no verdict: kista computes none and shows none. `review` (spec 0011) stays: a verdict by
whoever stands behind it, for an organisation's own reviewers.

**The envelope** is a JSON object. Its version is the member `envelope`, the string
`kista.report.v<N>`; this spec defines `kista.report.v1`. Members kista reads:

| Member | Type | In every version (the floor) | Read for |
| --- | --- | --- | --- |
| `envelope` | string | yes | the version |
| `report` | string, 1 to 128 bytes of `[A-Za-z0-9._:-]` | yes | the report's id, at its issuer |
| `issue` | integer, at least 1 | yes | the ordering (below) |
| `issued_at` | RFC 3339 | yes | the summary |
| `subject.bodies[].hash` | 64 lowercase hex | yes | the document must list the body it is attached to |
| `text` | string, UTF-8 | yes | the human view, shown as plain text |
| `format` | string | v1 | the producer's name for the rest (`enterest.report.v1`), shown |
| `reason` | string | v1 | why this issue exists, shown |
| `depth` | string | v1 | how far the analysis went (`mechanical`, `read`, or any other), shown everywhere |
| `self_assessment` | boolean | v1 | the producer analysed its own extension, shown |
| `subject.extension`, `.version`, `.commit`, `.repository` | strings | v1 | shown; never matched against the Build |
| `subject.bodies[].platform`, `.duckdb`, `.read`, `.not_read` | strings, boolean | v1 | whether this body was read |
| `sections[].name`, `.status` | strings | v1 | coverage: sections by status |
| `attention[].kind`, `.count` | string, integer | v1 | points of attention by kind |
| `kinds_evaluated` | list of strings | v1 | "none" differs from "not evaluated" |

- **Everything else is carried.** Any other member, at any depth (claims, leads, evidence, inputs,
  a fact sheet, a notice), is the producer's: stored, served, signed through the content hash, and
  never read. An unknown member never refuses a document.
- **Open strings.** `depth`, `reason`, a section's `name` and `status`, an attention `kind` are
  text to kista: a value it has no word for is shown as it is written. kista keeps no registry of
  kinds: the producer publishes its own (Enterest: `GET /api/v1/reports/format`).
- **The floor** is what every envelope version keeps, with these meanings, for good. A document
  whose `envelope` is a version this kista does not know is checked against the floor only, stored,
  ordered and bound to its body as any report, and summarised as
  `{"envelope": N, "degraded": true, "report", "issue", "issued_at"}`. The console shows its text
  under "a newer report format than this kista reads: shown as its text only". So a producer moves
  to `v2` without waiting for every kista.
- **Checks at intake** (an attach, a feed run, a bundle import; the same code): one JSON value
  within 32 levels (spec 0011); the floor's members present and of their types; in `v1`, each `v1`
  member that is present is of its type; **the attachment's body hash is one of
  `subject.bodies[].hash`** (a report is shown only for a build it names; the statement signs the
  same binding from outside). A failure refuses the document: `400` on an attach, the refusal
  `document` in a feed run.
- **The summary** (computed at intake, at most 4,000 bytes, every string scrubbed of control and
  format characters and clipped at 64 bytes, as spec 0011's summaries are):

```json
{"envelope": 1, "format": "enterest.report.v1", "report": "r-7f3a", "issue": 2, "reason": "builds",
 "depth": "mechanical", "issued_at": "2026-11-10T09:00:00Z", "self_assessment": false,
 "extension": "h3", "version": "1.2.0", "commit": "9f2c…", "bodies": 12,
 "body": {"read": false, "not_read": "format"},
 "sections": {"analysed": 7, "partial": 2, "not_analysed": 1}, "not_analysed": ["build"],
 "attention": {"raw_network": 3, "process_or_loading": 1}, "attention_more": 0, "kinds_evaluated": 14}
```

  `body` is this attachment's entry of `subject.bodies`. `sections` counts by status (a status
  other than the three is counted under its own text); `not_analysed` names the sections that are
  not `analysed`, the first 20. `attention` holds the first 32 kinds with their counts, and
  `attention_more` how many kinds it left out. A summary is never refused for its size: it is cut,
  and says so.
- **Who attaches.** As any kind (spec 0011): a tenant administrator or the extension's, origin
  `tenant`; a feed. Not a publisher (a publisher attaches `sbom` and `provenance` only).

### Issues

A report is a chain of issues at its issuer. kista orders them by the number inside the document,
not by arrival:

- **The key** is (origin, issuer, body hash, `report`): two sources never share a report, and the
  tenant's own reports are apart from every feed's.
- **Current** is the document of a key with the highest `issue`, removed ones aside. Lists mark it
  (`current`), the console and the index show it, and readers see only it (and only when it is
  `readers` and the head of its chain, as spec 0011 says).
- **A withdrawn current issue stays current**: the report shows as "issue 3, withdrawn by its
  issuer", and issue 2 is listed under it as history. kista never falls back to a lower issue: a
  withdrawal is not a statement that the earlier issue holds.
- **A lower or equal issue arriving later is refused**: a document of a key whose `issue` is not
  above every issue held for it (withdrawn ones included) is refused, `issue`, permanently (the
  feed's cursor passes it; an attach answers `409`). An issue is immutable: the same number with
  other content is the same refusal. The check and the insert are under the tenant's attachments
  lock, as every attach is.
- **A newer statement about a document held** (the same `issuer_id`) updates it as spec 0011 says;
  it is the same issue.
- **`supersedes` is not what orders reports**, but a producer sets it (each issue names the
  previous issue's document for the same body), so the chain's heads limit counts one head per
  report and older issues age out with the superseded (below).
- **What this does not give**: a kista that never received issue 3 shows issue 2 as current and
  cannot know. It shows the issue's number and date, and the feed's last success; the report's own
  notice says where the current issue can be checked (Security).

### The feed over catalogue builds

`feeds.offer` gains **`catalogue`**, the default for a new feed:

- **`catalogue`**: every body of the tenant's catalogue builds (spec 0033) whose source is an
  upstream or a distributor's publisher, in state `held` or `available`, of a body that no build of
  the tenant's own (`tenant`, or a publisher that is not a distributor's) has. One query over
  `builds` joined to `publishers`; no release is needed.
- `upstreams` and `channels` stay as they are, for a tenant that tells a source only what it
  serves.
- A withdrawn entry's body is not offered (its body reference is gone, spec 0033). A body held
  again later is offered again and receives its history (spec 0011).
- A bundle plan's feed takes its bodies from the same query (spec 0014), and a `catalogue` feed
  names the plan's sources as an `upstreams` feed does.

The **source's** scope does not change: a kista source answers for bodies released in the channels
the caller's `feed` grant covers. That is what an own kista needs, because everything it mirrors
is released in the channel it mirrors, whatever its own channels have taken. An issuer that is not
a kista (Enterest) decides its own scope.

### Handing received documents downstream

**The switch** is the feed's `export` setting (spec 0014 phase 1b), a tenant administrator's, off
by default, per feed and so per source. Its meaning widens from "keep what a bundle needs" to
"this source's documents may leave for a downstream kista: in a bundle, and online to a caller
holding `feed`". What it keeps is unchanged (the source's keys file, permanent refusals, statements
about documents not held), and so is its reset of the feed's cursors. The console's feed row gains
the switch, "Hand on to kistas that take this tenant's feed and to bundles", with a confirmation
naming the source.

**The relaying tenant** answers three routes for a named issuer. `issuer` is a feed URL in kista's
normalised form (spec 0011); anything else is `400`.

```text
POST /api/v1/tenants/{t}/feed  {issuer, bodies: [{body_hash, after_seq}]}
                                               →  {statements: [{statement, signature, note?}], bodies, more}
GET  /api/v1/tenants/{t}/feed/attestation-keys?issuer=<url>   →  the issuer's keys file, as last fetched
GET  /api/v1/tenants/{t}/feed/documents/{issuer_id}?issuer=<url>   →  the content
```

- **Authorised** as the feed is: `feed` on the tenant or on a channel. Without `issuer` (or with
  the tenant's own issuer URL) the routes are today's.
- **Scope**: a body released in the channels the grant covers, and an issuer for which the tenant
  has an online feed with `export` set and a keys file (spec 0014's export candidates). Any other
  body or issuer answers as a body without documents (`/feed`) or as a missing path (the other
  two): whether the tenant subscribes to a source is not shown.
- **What is answered** is what spec 0014's export carries for the body, by the same code: for the
  range from the caller's `after_seq` to the tenant's cursor for the body, in `seq` order, each
  document held from the issuer with its latest statement and signature; a withdrawal; and each
  statement the tenant holds without content (a refusal, a document removed here, one never held)
  with an unsigned `note` saying why. `bodies` gives the tenant's cursor per body read through.
  The limits are the feed's (500 bodies, 1 MiB an answer, `more`).
- **Statements are the issuer's, byte for byte.** The tenant signs nothing and issues nothing:
  no statement of its own, no `seq` of its own, no attestation key needed.
- **The content** is the stored object, read against its hash (spec 0011), for a document of that
  issuer still held and in scope.

**The downstream's feed** gains `via`:

```text
feeds   + via   NULL   the tenant fetched from: https://<host>/api/v1/tenants/<t>
```

- `url` stays the **issuer's** identity: statements must name it, and a feed with `via` never
  contacts it (as a bundle-mode feed never does: https, its port not checked against egress).
- `via` is validated as a feed URL through egress and `egress.hosts` (spec 0044). The credential is
  matched against `via` (spec 0009's `prefixes`) and sent only there. `via` is set at creation and
  changed like the feed's other settings; a bundle-mode feed takes none.
- **A run** reads the keys file from `via` and keeps the keys the feed **pins**; sends its bodies
  with their cursors and `issuer`; checks each statement exactly as today (its signature under a
  pinned key, its issuer the feed's `url`, its body one it sent and holds, `seq` above what it
  holds); fetches content from `via`, checked against the statement's size and hash. A statement
  with a `note` is verified and then refused, `relay: no content (<note>)`, as a bundle's is. The
  code is the one path spec 0014 factored out: a statement applied with its content from a fetch,
  a bundle, or a relay.
- **Statuses**: a `404` from `via` is `unauthorized` ("no feed grant, no such tenant, or the tenant
  does not hand on this issuer"); a keys file without a pinned key is `failing`, naming the
  fingerprints the file holds, so a rotation at the issuer shows at once.
- **`export` on the downstream's feed** works as on any online feed: an own kista hands the same
  documents to its regional mirrors and its air-gapped sites. A chain has any length; every kista
  in it verifies the issuer's signature itself.
- **A document reaching a tenant twice** (through a relay and a bundle, or two relays) is stored
  once (spec 0011).

**Pinning the issuer's key** is the downstream administrator's act (`-key sha256:…` at
`feed add`), as for any feed. The fingerprint comes from the issuer out of band: its documentation,
or the organisation's page there. The console's form shows the fingerprints of the keys file `via`
serves, for comparison; it pins none by itself. A key the administrator did not pin verifies
nothing, whatever the relay sends.

**Bundles** need one change: a plan's and a feed's kinds accept `report`. A feed with `via` is an
online feed, so with `export` it is a candidate for a site's plan as any other.

### The grant

Nothing changes in `internal/authz`. An own kista takes its tenant's feed with a credential whose
principal holds **`feed` on the channel it mirrors**, granted explicitly by the tenant's
administrators (or its provisioning), beside the `install` its mirror uses. It may be the same
credential with both grants.

- `install` does not imply `feed`: people and nodes install; documents default to administrators.
- `feed` on a channel reaches the tenant's own feedable documents and, per source, what `export`
  hands on, for bodies released in that channel only.
- The own kista's credential names the feed URL in its `prefixes` beside the channel's prefix.

### Storage: per body

A report, its SBOM and its vulnerabilities document stay **one attachment per body**, each with its
own statement and its own object. Reasons:

- the binding is the security property: a statement signs one body, a record is reached through
  one body's Build, a collector removes it with that body. One record for many bodies would need a
  second binding and a second lifetime rule;
- sharing objects needs reference counts or spec 0016's claims (spec 0011 rejected it for that);
- the cost is bounded and known: a report is about 1 MiB at Enterest's defaults, a version has a
  few tens of builds, and the tenant's quotas (`limits.ceilings.attachments`, heads per body and
  kind) hold it.

The age at which the collector removes superseded documents becomes a setting,
`attachments.superseded_retention` (a year by default, at least a day): rescans supersede often.

### Vulnerabilities summaries

For every media type of the `vulnerabilities` kind:

- **`informational`.** A severity of `info` or `none` is `informational`, a count of its own,
  outside `findings` and outside the severity counts. For CycloneDX a vulnerability is
  informational when it has ratings and every one is `info` or `none`; one without ratings stays
  `unknown`, a finding. SARIF's `note` stays `low`: a tool's note is a finding.
- **Components checked.** A CycloneDX document may mark a component with the property
  `kista:vulnerabilities:check`, value `checked` or `not_checked`. kista counts them into the
  summary: `components` (those marked), `not_checked`. The name is kista's and documented, so any
  producer writes it; kista reads no other producer's namespace. Other values are counted as not
  checked.

```json
{"findings": 2, "high": 1, "medium": 1, "informational": 3, "components": 40, "not_checked": 12,
 "scanned_at": "2026-11-15T03:00:00Z"}
```

- **What the console says** (one line, from the summary):

| Summary | Text |
| --- | --- |
| findings | "2 findings: 1 high, 1 medium", then "3 informational", then "12 of 40 components not checked" |
| no findings, components marked, all checked | "no findings in 40 components checked" |
| no findings, some not checked | "no findings in 28 components checked; 12 not checked" |
| no findings, none checked | "not checked: none of 40 components was matched" |
| no findings, no component marked | "no findings listed; the document does not say what was checked" |

  A vulnerabilities document's date is its `scanned_at`; when its statement is later than the scan
  (the source confirmed it without a new document), the console adds "confirmed <date>". The
  30-day flag and `valid_until` ("expired") stay.

### Sizes

- `attachments.max_size` defaults to **16 MiB** and is at most 16 MiB: the bound of a document in
  the feed's and the bundle's formats (`bundle.MaxDocument`), the same at every kista, so a
  document one kista took can always travel on. The setting lowers it for a small deployment.
- A document over the size is refused for good (`size`), as today, and the refusal travels on with
  its note. The feed's last run names it (the reason and the body), as today; a version's row shows
  only what is held.
- Reads still hold at most `attachments.max_reads` documents in memory: 128 MiB at the defaults.

### API, CLI and configuration

| Change | Where |
| --- | --- |
| kind `report` | `POST …/extensions/{ext}/attachments?kind=report`, `Content-Type: application/vnd.kista.report+json`; feeds' and plans' `kinds`; `kista admin attachment add -kind report` |
| `current` in an attachment's answers | `true` on the current document of a report's key; absent for other kinds |
| list filters | `GET …/extensions/{ext}/attachments?source=&version=&current=true`: the documents of the builds of that source and version; `current=true` leaves, per body and kind, what is shown now (a report's current issue; else the heads of chains) |
| `offer: "catalogue"` | `POST …/feeds`, a change; `kista admin feed add|set -offer catalogue`; the default |
| `via` | `POST …/feeds`, a change; `kista admin feed add|set -via <url>`; `feed show` |
| the relay routes | `POST …/feed` with `issuer`; `GET …/feed/attestation-keys?issuer=`; `GET …/feed/documents/{id}?issuer=` |
| `attachments.max_size` | default 16 MiB |
| `attachments.superseded_retention` | new; default `8760h`, at least `24h` |

Events: `feed.add` and `feed.change` carry `via`; `feed.run` gains the refusal reasons `issue` and
`relay: no content`; `attachment.add` of a report carries `report` and `issue`. The relaying tenant
records no event per request, as the feed does not; a change of `export` is `feed.change`, as
today.

### Data model

One migration (the next free number), in the three dialects:

```text
feeds         + via   varchar(1000) NULL            (SQL Server: nvarchar, BIN2)
feeds         offer:  the CHECK gains 'catalogue'
attachments   kind:   ck_attachments_kind gains 'report'
```

- PostgreSQL and SQL Server drop and add the two constraints. SQLite rebuilds the two tables (it
  has no other way to change a CHECK). If spec 0046 takes the kind list out of the schema first,
  this migration only adds `via` and the offer.
- No column for a report's id or issue: they are in the summary, and a key's documents are read
  through the existing index (tenant, body hash), a handful of rows.
- `min_reader` is raised to this migration: an older replica would refuse or mis-show the kind and
  not know the offer.
- No new table: the relay reads `attachments`, `feed_cursors`, `feed_refusals`, `feed_kept` and the
  feed's keys file, which spec 0014 already keeps.

### The console

- **Needs a decision**, on each row of "New versions under manual rules" and "Holds ending": one
  line for the waiting version, from `…/attachments?source=&version=&current=true`:
  - "Report: mechanical, issue 2 of 10 Nov. 2 points of attention: raw_network (3),
    process_or_loading (1). 3 sections not fully analysed. 1 of 12 builds not read."; the depth is
    always there; a self-assessment is marked; builds whose current issues differ are said to
    differ;
  - "Vulnerabilities: …" as the table above, with the scan's age;
  - "No report held for this version." when there is none: never a word about the extension.
- **The report view** (from the row, the drawer and the Reports block): the summary as a header
  (issuer and "signature verified", depth, issue and reason, dates, subject, this build read or
  not, sections by status, points of attention), then the document's `text` as **plain text**:
  preformatted, control and format characters shown as placeholders, no Markdown, no links, no
  HTML. The console reads the document it already downloads and takes `text` from it. Older issues
  are listed under the current one.
- **Comparison**: "Compare with…" on a report offers the current reports of the name's other
  versions from the same issuer, and the report's earlier issues. It shows, from the two summaries:
  depth of each (and a warning when they differ), points of attention added, removed and changed
  in count, sections whose status changed, builds read, and the two vulnerabilities lines. It says
  "compared by their summaries; claim by claim, read the two documents".
- **The Reports block**'s texts follow the vulnerabilities table; a `report` shows depth, issue and
  attention kinds; the attach form and the feed form offer the kind.
- **Feeds** (on Upstreams): `via` in the form, with the relay's fingerprints shown for comparison;
  the `export` switch; the offer.

### Package layout

```text
internal/attachment   kinds.go: the report envelope and its summary, the vulnerabilities changes;
                      service.go: the issue rule, `current`; feed.go: the relay's three answers
                      (over the export's reader, moved from internal/upstream where shared);
                      client.go: `via`, the relay as a content source
internal/store        the catalogue offer; feeds.via; the list filters
internal/bundle       feeds.go: `report` among a plan's kinds
internal/api          the routes' parameters
cmd/kista             feed add|set -via, -offer catalogue; attachment add -kind report
web/console           Reports.tsx (texts, the view, the comparison), Decisions.tsx (the row), Feeds.tsx
website/docs          attachments.md, api.md, bundles.md, the own-kista page: updated with each PR
```

## Security

- **Who signed what.** A document handed on carries its **issuer's** statement, unchanged: the
  issuer signed the body hash, the kind, the content hash, the title, the audience and the dates.
  The relaying tenant signs nothing, so its key is never a root of trust for another's documents,
  and a compromised relay, gateway or bundle cannot alter, retitle, re-date or re-bind a document.
- **What the downstream verifies, and against what.** Every statement against a key its own
  administrator pinned by fingerprint; the issuer named in the statement against the feed's `url`;
  the content against the statement's hash and size; the report against its own body list. It
  needs no route to the issuer: public keys arrive in the relay's keys file, and a key that is not
  pinned verifies nothing. A statement under an unpinned key holds its body's cursor until the key
  is pinned (spec 0011), so a rotation delays documents and never admits one.
- **What a signature does not give.** It proves origin and integrity. A relay can withhold a
  document, a withdrawal or a newer issue, and a downstream without a route to the issuer cannot
  know; a statement names no recipient, so it does not say who was entitled to it. kista shows the
  issue and its date, the scan's age, `valid_until` when the issuer signed one, and the feed's last
  success; removals are in the relaying tenant's audit. A document once held is never replaced by
  an older statement (`seq`) nor a report by a lower issue.
- **No existence leaks.** The relay routes answer a caller without `feed` as a missing path; a
  caller with `feed` learns nothing of a body outside its grant's channels, nor whether the tenant
  subscribes to an issuer it does not hand on: both answer as "no documents". The keys route is
  not public (which sources a tenant takes is the tenant's). Readers see a report only through a
  release in their view, only when an administrator made it `readers` and its signed audience
  allows it (spec 0011).
- **Tenant isolation.** The relay reads the tenant's own records and objects; `issuer` selects
  among the tenant's feeds and reaches nothing else. Documents are per tenant; nothing is shared
  across tenants.
- **Two consents to leave a tenant**: the source's `export`, and a `feed` grant on a channel. Both
  are administrators' acts, both audited. `install` gives neither.
- **Hostile documents.** A report holds strings taken from third-party source code (paths, calls,
  names) and a model's prose. kista treats the whole document as data:
  - parsing is bounded (size, 32 levels, list counts), members are read into fixed types, and
    nothing is evaluated or followed (no URL in a document is fetched);
  - a summary's strings are scrubbed of control and format characters (bidi overrides) and
    clipped; the summary is bounded;
  - the console renders summaries and `text` as text nodes: no HTML, no Markdown, no links, format
    characters made visible; a download is saved, never opened (spec 0011);
  - kista computes no verdict from a report and no policy reads one, so a hostile document can
    mislead a reader at most as far as its issuer's signature lets it stand: the issuer and "depth"
    are always shown, and a tenant's own report is labelled the tenant's.
- **The subject is shown, not trusted.** `subject.extension` and `.version` are the producer's
  words; kista binds a report to a Build by the body hash only.
- **SSRF and credentials.** `via` goes through egress and `egress.hosts`; the issuer's URL is never
  contacted on a `via` feed; the credential is sent to `via` only, never forwarded.
- **Disclosure.** A `catalogue` feed tells its source the hashes of every build it received from
  outside, released or not. They came from outside; the tenant's own builds stay out, as before.
- **DoS.** The relay's answers have the feed's bounds; one document a request; documents count
  against the feed quotas per issuer (spec 0011), so one source's reports never use up another's
  room.

## Testing

- **Unit** (`internal/attachment`): the envelope (valid `v1`; each floor member missing or of
  another type; a `v1` member of another type; unknown members at every depth carried; open
  strings kept; a higher envelope version degraded; a body not in the subject; nesting and size;
  control and bidi characters in every read string; a summary cut at its bounds). The issue rule
  (higher accepted; lower and equal refused; equal with other content; a withdrawn highest stays
  current; two issuers and the tenant's own apart). Vulnerabilities (`info`, `none`, mixed
  ratings, no ratings; the check property: all, some, none, absent, other values; every media type
  unchanged otherwise). The console's texts by table (vitest).
- **Store suite, three dialects**: the `catalogue` offer (held, available, withdrawn; an upstream's,
  a distributor's publisher's, the tenant's own, a body under two sources); `feeds.via`; the two
  constraints after the migration (and SQLite's rebuilt tables keeping their rows and keys); the
  list filters and `current`; the issue rule under two concurrent writers.
- **Service and feed**: the relay's scope (the grant's channels, `export` off, an unknown issuer,
  each answering alike); its answer equal to a bundle's for the same plan;
  notes; the keys route; a `via` client against a fake relay: an altered statement, altered
  content, another issuer's statement, an unpinned key holding the cursor then taken once pinned,
  a replayed lower `seq`, a note refused without a fetch, a `404`.
- **Bundles**: a plan with `report`; a `via` feed as an export candidate; a round trip own kista →
  bundle → site.
- **End to end**, two kistas and a stand-in issuer (a test server that serves a feed and signs
  kista statements with its own RSA key; it is not Enterest):
  1. kista A's tenant subscribes to the issuer (`catalogue`), mirrors an extension and holds its
     build unreleased: the report, the SBOM and the vulnerabilities document arrive; "Needs a
     decision" shows the line.
  2. A releases the build and sets `export`; kista B mirrors A's channel with a credential holding
     `install` and `feed`, and a feed for the issuer `via` A with the issuer's key pinned: the same
     three documents arrive at B, for a build B has not released, verified against the issuer's
     key. B never contacts the issuer (the stand-in refuses B's address).
  3. The issuer releases issue 2, then withdraws it: B shows issue 2, then "withdrawn", never
     issue 1 as current.
  4. A's database is altered (a title, a content byte): B refuses. `export` off: B receives
     nothing more and keeps what it holds. The issuer rotates its key: B is `failing` naming the
     new fingerprint, then proceeds once it is pinned.
  5. The pinned DuckDB installs the mirrored extension from B as before: no DuckDB behaviour is
     relied on by this spec.

## Alternatives considered

- **Extending `review` with an optional verdict.** A kind whose main field is sometimes absent
  reads as "approved" by omission somewhere. A report without a verdict is another kind.
- **The version in the media type** (`…report.v2+json`). A released kista matches media types
  exactly and would refuse the next version outright; the version inside the envelope, with a
  floor, lets it store and show it.
- **A nested envelope** (kista's members at the top, the producer's document under `detail`).
  Cleaner on paper; it makes every producer restructure for no gain, since unread members are
  carried either way.
- **kista reading claims and leads** (a claim-by-claim comparison in kista). It would make the
  claim model part of kista's open format and its release. Summaries compare what a decision needs
  first; see the open questions.
- **Re-signing by the relaying tenant.** The downstream would trust the tenant's key for the
  issuer's words, and a tenant administrator could write a report. Statements pass unchanged.
- **A second switch for online relaying**, beside `export`. Two settings that both mean "this
  source's documents may leave" invite one being forgotten. The grant is the second consent.
- **Routes under the channel's prefix** (as spec 0039's revocations). The mirror's credential
  would reach them with no change, but the feed's client, cursors, quotas and bundle path would be
  built a second time; and `install` would then be what reads administrators' documents.
- **`install` implying the feed for mirrored bodies.** Every person's install token would read
  reports.
- **One record for all bodies of a report**, or content shared between records. See "Storage".
- **Fetching the issuer's keys from the issuer on the downstream.** The own kista has no route to
  it; and a route would not add trust beyond the pin.
- **Raising the document bound above 16 MiB.** It is the bundle format's; a source that needs more
  has a document nobody opens.

## Open questions for the owner

1. **How deep is the comparison on an own kista?** By summaries (depth, points of attention,
   coverage, vulnerabilities), as designed; or claim by claim, which means kista adopts the claim
   model as its own format before the release. *Recommendation*: summaries for the release.
   Enterest can put "changed against the previous version" into the report's `text`, which every
   kista shows, and its own screens keep the full comparison.
2. **Who may read a report downstream?** The issuer signs the audience. `administrators` keeps
   reports with the tenant's and the extensions' administrators and auditors; `readers` would let
   an organisation show them to everyone who installs, and on a public release to anyone.
   *Recommendation*: Enterest signs `administrators` for the report and its vulnerabilities
   document; revisit when an organisation asks.
3. **Does a vulnerabilities document expire?** If Enterest signs `valid_until` (a few rescan
   periods ahead), an own kista whose relay stopped shows "expired" by itself; without it, only
   the scan's age and the 30-day flag speak. *Recommendation*: sign `valid_until`; it is the only
   signal a cut-off kista has that does not rely on the relay.
4. **Does `review` (a verdict) stay in the first release?** Nothing of ours produces it once
   reports exist; it serves an organisation's own reviewers. *Recommendation*: keep it; it is
   built, small, and the one place a tenant records its own decision as a document.

## What Enterest must do (its P3)

1. **The report**: add `"envelope": "kista.report.v1"`; keep `format`, `report`, `issue`, `reason`,
   `depth`, `self_assessment`, `issued_at`, `subject`, `sections`, `attention`, `kinds_evaluated`,
   `text` under these names and types; list every body of a statement in `subject.bodies`; `text`
   readable as plain text; the whole document within 16 MiB and 32 levels. Kind `report`, media
   type `application/vnd.kista.report+json`.
2. **Statements**: one per document and body; audience per the owner's answer; each issue's
   `supersedes` naming the previous issue's document of the same body; an issue number never
   reused with other content; a `correction` is a higher issue.
3. **The vulnerabilities document**: the property `kista:vulnerabilities:check` (`checked`,
   `not_checked`) on every component, beside its own; an advisory that is not to count as a
   finding carries no rating but `info` (kista goes by ratings); the **whole** document within
   16 MiB, not only its findings; a rescan that changes nothing issues a new statement about the
   same document (kista updates it and shows "confirmed") rather than a new document per body;
   `valid_until` per the owner's answer.
4. **The source**: a feed URL of kista's shape, `https://<host>[/<path>]/api/v1/tenants/<name>`,
   which is also the statements' `issuer`; `GET <url>/attestation-keys` in kista's shape, within
   64 KiB; `POST <url>/feed` and `GET <url>/feed/documents/{issuer_id}` as spec 0011 has them;
   unknown, unreported and unentitled bodies answered alike.
5. **Provisioning a tenant**: the feed with `offer: catalogue`, kinds `report`, `sbom`,
   `vulnerabilities`, its key pinned; `export` left to the organisation's administrators.
6. **An own kista's credential**: a token group of its own (`feed:<channel>`) for the service
   account an own kista uses, and the grant `feed` on that channel for it; never on
   `install:<channel>`.
7. **Keys**: publish the attestation keys' fingerprints where an organisation finds them without
   its tenant; announce a rotation ahead, since own kistas pin by hand.
8. **The notice** says where the current issue can be checked.

## Follow-ups

- **From spec 0046** (unknown kinds, fields and versions in a released kista), this spec needs:
  1. the envelope's floor and "unknown members are carried" confirmed as the general rule for
     kista's own formats;
  2. what a kista does with a **kind** it does not know, in a feed, a plan and a relay: today it is
     a permanent refusal, a plan naming it is refused whole (`internal/bundle/feeds.go:89-96`), and
     a relay hands it on without content; an older relay between an issuer and a newer kista then
     drops every new kind;
  3. how a **statement** gains a field: the verifier refuses unknown fields
     (`internal/attachment/statement.go:200`), so none is added here;
  4. the kind list out of the schema, so a kind is not a migration;
  5. the console showing a summary whose keys it does not know (it lists them today).
- Content shared between a report's bodies in storage, and a feed run that reuses content it holds
  instead of fetching it per body.
- A claim-by-claim comparison, if the owner asks for it (question 1).
- Policies over reports (spec 0011's follow-up) stay out: a report has no verdict to act on.
- "Available" entries for a tenant that does not subscribe (spec 0011's follow-up).
- A relayed feed set up from the upstream's form in one step.
