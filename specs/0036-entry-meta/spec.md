# Spec 0036: Entry icons and distributor statistics

- **Status**: draft
- **Date**: 2026-10-29
- **Author**: vgsml, Claude

## Summary

Two additions around the catalogue (spec 0033), both asked for by Enterest:

1. **Entry metadata.** A catalogue entry (tenant, package type, source, name) gets an optional
   **icon** (a small square PNG or WebP) and a **summary** (one line of plain text). A publisher
   sets them for its own entries with a signed request (spec 0023), next to its publications. A
   tenant's administrators set them for the tenant's own sources. For a distributor's publisher, the
   publisher or a distribution admin sets them. kista keeps them, serves the icon from a route of
   its own with headers it sets itself, and the console shows them where the catalogue shows a name.
2. **Distributor download statistics.** A distribution admin reads, for one distributor, the
   downloads of its publishers' entries per extension per week, with installers beside them, across
   every tenant. They are counted from the download counts kista already keeps (spec 0010), now
   recording whose entry was downloaded. No tenant is named. Anonymous downloads (without a token)
   always count; authenticated downloads are withheld from every row fewer than a minimum number of
   tenants contribute them to. This is what Enterest's storefront shows, like DuckDB's
   community-extensions page. kista publishes no public statistics.

## Problem

- **An entry is a name and nothing more.** The catalogue (spec 0033), a channel's Sources tab and
  the release pages show `geo_tools` from `publisher:enterest`, and nothing that tells a person what
  it is. A distributor's developer has an icon and a one-line description for the extension.
  Enterest has them in its own storefront, but cannot hand them to the tenants it publishes into.
  A tenant's own CI cannot either.
- **A distributor cannot see adoption.** Enterest publishes a developer's extension into many
  tenants' catalogues. Each tenant counts its downloads (spec 0010, `GET
  tenants/{t}/stats/downloads`), but only the tenant reads them. Enterest's storefront wants the
  community-extensions figure, downloads per extension per week. Today the only way is a server
  token reading every tenant's statistics and adding them up. That reads far more than the figure
  and would show which tenant installs what.
- **The counts do not say whose build was downloaded.** A download is counted per (channel, name,
  version, platform, DuckDB version, day, authenticated). Since spec 0033 a channel may hold one
  version from two sources (a slot is per source), and a rule may switch sources. So a stored count
  cannot be attributed to the distributor's entry afterwards, even by a server administrator.

## Design

### Part 1: entry metadata

#### What an entry carries

An entry's **metadata** is a row of its own, keyed as the entry: (tenant, package type, source,
name). The source is in its stored form (`upstream:<id>`, `publisher:<id>`, `tenant`), so it
survives renames, and the entries of a removed source keep theirs. It holds:

- **summary**: plain text, 1 to 160 characters (Unicode code points) of UTF-8. Refused: control
  characters (CR and LF included) and Unicode's bidirectional controls (U+202A to U+202E, U+2066 to
  U+2069), which could make the text read as something else. Surrounding spaces are trimmed. It is
  never interpreted as markup: the console renders it as text, and the CLI prints it as is.
- **icon**: a reference (the SHA-256 of its bytes) to an image in the icon store (below), with its
  content type, its size in bytes and in pixels.
- who set it last, and when.

Both are optional and independent. Metadata is the entry's, not a version's or a build's: the
latest write wins. A publication carries no metadata, so publishing a version without touching the
metadata keeps what the entry has.

#### The icon

- **Formats**: PNG and WebP, recognised by their signature bytes. The request's declared
  `content_type` must name the format found (`400` otherwise), and what is stored and served is
  kista's finding, never the declaration. Every other input is refused (`400`): SVG (it is a
  document and can carry script), GIF, JPEG, an animated image (APNG's `acTL` chunk, WebP's `ANIM`
  or the VP8X animation flag), anything else.
- **Bounds**: at most 256 KiB; square; 64 to 512 pixels a side (a 256x256 PNG of up to 256 KiB, the
  size Enterest assumes, fits). The dimensions are read from the header (`image.DecodeConfig`)
  before the image is decoded, so a decompression bomb is refused before any pixel is allocated. The whole image is then decoded once (Go's `image/png` and
  `golang.org/x/image/webp`, a new dependency of the Go project's own), so a truncated or corrupt
  file is refused at write time, not in a browser.
- **Bytes kept as sent**: kista stores the validated bytes unchanged, not re-encoded. The same icon
  sent to many tenants then has one hash, which the deduplication and an idempotent `PUT` rely on.
  What a browser does with them is fixed by the serving headers, not by the bytes (Security).
- **Storage: in the database, deduplicated server-wide.** `entry_icons` holds each distinct icon
  once, keyed by its SHA-256: content type, width, height, size, bytes, `used_at`. A metadata row
  references it. The reasons, against a body in the tenant's storage domain (spec 0005):
  - the write is one transaction (the icon and the reference together, no object written before
    its row, no two-phase cleanup);
  - no new blob prefix for the collector (spec 0016) or the blob verifier (spec 0018);
  - the reads are one query;
  - deduplication is server-wide, not per storage domain. Enterest sending one developer's icon to
    a thousand tenants stores it once, however their storage is split;
  - it is small: one icon of at most 256 KiB per distinct image, bounded by the entries, which the
    catalogue's caps already bound (spec 0033).

  Storage domains exist for build bodies, and the data an organisation must keep in its own bucket
  is those bodies. An entry's icon and summary are metadata, like the entry's name and versions,
  which live in the database already. Sharing an icon's row across tenants reveals nothing to
  them: every read goes through the reader's own metadata row, and only a tenant whose entry
  references the hash, because it was sent those bytes, ever reads it.

#### The summary and the icon in one request

```text
PUT …/meta   {"summary": "H3 hexagonal indexing for DuckDB",
              "icon": {"content_type": "image/png", "data": "<standard base64 of the bytes>"}}
```

- **The request replaces both**: `summary` left out or `null` clears the summary, `icon` left out or
  `null` clears the icon. Each write states the whole metadata, so there is no question of which
  field a partial write meant. This is Enterest's assumed interface, adopted.
- The route takes a JSON body of at most 384 KiB: a 256 KiB icon in base64 is about 342 KiB, beyond
  the management routes' 64 KiB, so the route's rule gets a limit of its own.
- The answer, `200`, is the metadata as stored: `{"summary": …|null, "icon": {"content_type",
  "sha256", "size", "width", "height"}|null, "set_by", "set_at"}`. The `GET` of the same path
  answers the same, and `404` while the source has no entry of the name, as the `PUT` does.
- The same metadata again changes nothing and makes no event, so a CI's replay or a distributor's
  re-send is harmless (spec 0023's replay window).

The icon travels in a request of its own, after the publication, rather than in the publication's
body. Spec 0023's statement binds the method, the path and query, and the body's SHA-256: a JSON
body signs as a promotion's does, with no change to the signed format. A publication's body stays
the file alone: the channel route, the catalogue-only route and their spool are untouched, and a
publication never touches the metadata, so publishing a new version keeps the entry's icon. A CI
publishes, then sets the metadata. Enterest sends it into each tenant after its first publication
there, and again whenever the developer changes it.

#### Who sets it

| Source of the entry | Who writes its metadata |
| --- | --- |
| a tenant's publisher (not a distributor's) | that publisher, by a signed request, while it holds `publish` on the name somewhere in the tenant (tenant-wide or in any channel); the tenant's administrators |
| a distributor's publisher, live or removed (spec 0033 phase B) | that publisher while it exists, by a signed request, for a name it may publish into the catalogue (the same rules: not reserved server-wide, not reserved in the tenant for another holder); a distribution admin; a server administrator. Not the tenant's administrators: the entry is the distributor's, as its hold and its withdrawals are |
| an upstream (a mirror, a bundle's) | the tenant's administrators |
| `tenant` | the tenant's administrators |

A server administrator writes any. The entry must exist: its source has at least one build of the
name and type in the tenant's catalogue (held, available or withdrawn). Otherwise the answer is
`404`, as for any entry the caller may not address, so metadata never comes before a publication
and never names an entry that does not exist.

#### API

```text
PUT  /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
GET  /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
     a publisher's signed request when {source} names itself (publisher:<its name>), or a token of a
     tenant administrator, a distribution admin or a server administrator, each as the table above
     says (a distribution admin addresses a distributor's publisher as the catalogue answers it,
     publisher:<name> or publisher-id:<id>). A publisher naming any other source is 404.
GET  /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/icon[?type=][&v=<sha256>]
     the GET's readers, and whoever reads the entry (spec 0033: the tenant's administrators; a
     distribution admin, a distributor's publisher's entries; server administrators).
GET  /api/v1/tenants/{t}/channels/{c}/extensions/{ext}/meta
GET  /api/v1/tenants/{t}/channels/{c}/extensions/{ext}/icon[?v=<sha256>]
     the entry the channel serves the name from (the source its rule names, spec 0031): the
     extension's release readers (admin, publish or promote on the name in the channel, its
     publishers' signed requests), as GET …/channels/{c}/extensions/{ext}/releases. No rule, or no
     metadata, is 404.
```

One route serves the publisher and the administrators: the path names the entry exactly, and a
publisher's signed statement binds it, so the publisher's source is checked against its own
identity rather than implied.

- **The catalogue's reads** (`GET …/catalogue`, `GET …/catalogue/{source}/extensions/{ext}`) gain
  `summary` and `icon` (`{content_type, sha256, size, width, height}`) per entry, without the bytes.
  A page shows the text at once and fetches only the icons it shows.
- **The channel routes** are for the screens that show a channel's names (the console's channel
  lists and the redesign's Report & licence panel). They answer what a channel reader may already
  know: the name it reads releases of, and the source the channel serves it from.
- **The icon route** answers the bytes with:
  - `Content-Type`: kista's, from the format it found (`image/png` or `image/webp`), never the
    uploader's declaration;
  - `X-Content-Type-Options: nosniff`;
  - `Content-Security-Policy: default-src 'none'; sandbox`;
  - `Content-Disposition: inline; filename="icon.png"` (or `.webp`);
  - `Cross-Origin-Resource-Policy: same-origin`;
  - `ETag: "<sha256>"`, with `If-None-Match` answered `304`;
  - `Cache-Control`: `private, max-age=31536000, immutable` when `?v=` names the stored hash (the
    URL then names those bytes forever), else `private, no-cache`. A `?v=` naming another hash is
    `404`. `private` because every read is authenticated.
- **Existence** follows spec 0007: a caller who may not read the entry, or address the source, gets
  `404`, the same answer as an entry that does not exist. A publisher gets `404` alike for a name
  it has no entry of, a name it may not publish, and another source.
- **Errors**: `400` with the problem type `invalid` for a summary or an icon out of bounds, naming
  the bound (the format, a declared type that is not the format, the size, the dimensions,
  square, animated). `413` for a body over 384 KiB.

#### CLI

- `kista admin catalogue meta <t> <source> <name> [-summary <text>] [-icon <file>] [-type t]`, a
  server administrator's (the operator's CLI on the database, as `catalogue withdraw`). It replaces
  both, as the API does: a flag left out clears that field. `-icon` reads a file and checks it as
  the API does. `catalogue meta <t> <source> <name> -show` prints the metadata.
- `kista admin catalogue list` gains `SUMMARY` (cut to 60 characters), and `catalogue show` prints
  `summary:` and `icon: <sha256> <content type> <w>x<h> <size>`.
- Publishers have no kista CLI (spec 0023: `curl` and `ssh-keygen`). The website's publishing page
  shows the signed `PUT`, and the developer kit's `kista_publish.py` gains `--summary` and `--icon`
  in `hugr-lab/extension-ci-tools` (a change there, pinned by `e2e/CI_TOOLS_PIN`; Follow-ups).

#### Events

`catalogue.meta`, the tenant's event, carries `source` (its label), `name`, the package type when
not the default, `summary` (the new text, or `""` when cleared), `icon` and `icon_before` (hashes,
`""` for none), and which fields changed. It is written in the same transaction, actor the writer
(a publisher, a distribution admin, an administrator). The icon's bytes are never in an event. The
event kinds registry (`GET /api/v1/event-kinds`) lists it. A distributor's publisher's write is told
to the tenant like its arrivals (`catalogue.arrival`).

#### Data model (migration 0044)

Additive only, in the three dialects, with no `min_reader` bump: an older replica ignores the
tables, and its catalogue answers simply lack the metadata.

```text
entry_icons    sha256 (char 64, pk), content_type (varchar 16), width, height, size (int),
               body (bytea | varbinary(max) | BLOB), created_at, used_at
entry_meta     tenant_id (fk tenants), package_type, source, name, summary (160 characters, NULL;
               nvarchar on SQL Server, as its text columns are), icon_sha256 (NULL, fk entry_icons),
               set_by (varchar 400), set_at; pk (tenant_id, package_type, source, name);
               index (icon_sha256)
```

- **A write** inserts the icon if absent and touches its `used_at` (PostgreSQL and SQLite `INSERT …
  ON CONFLICT DO UPDATE SET used_at`, SQL Server `MERGE … WITH (HOLDLOCK)`), then upserts the
  metadata row, in one transaction. The entry's existence and the writer's right are checked in
  that transaction too, under the tenant's auth lock (spec 0033's `PublisherAuthLock`), which
  removing the publisher or its key takes. So a write in flight when the key is revoked writes
  nothing.
- **Collection** (spec 0016's collector, its hourly pass): a metadata row whose entry has no build
  left (every build purged or collected) is deleted after the collector's grace. An icon no row
  references, whose `used_at` is older than the grace, is deleted by one statement that re-checks
  `NOT EXISTS` and `used_at`. A write touching `used_at` holds the icon's row, so the delete either
  waits and then sees the fresh `used_at`, or ran first and the write inserts the icon again. The
  foreign key from `entry_meta` to `entry_icons` guarantees that a reference never dangles, whatever
  the timing.
- **Tenant deletion** (spec 0019) reaps `entry_meta` with the tenant; its icons go with the next
  collection once nothing else references them.
- **Withdrawal and removal**: a withdrawn entry, and a removed source's entries, keep their
  metadata (they are kept, spec 0033).

#### The console

The smallest change that shows the metadata where the catalogue shows a name:

- **Catalogue**: a 24-pixel icon before the name, and the summary under it in the muted text style.
  An entry without an icon shows nothing in its place, not a placeholder.
- **An entry's page**: a 64-pixel icon beside the title and the summary under it. A tenant
  administrator gets an **Edit…** dialog (summary, icon file, clear) for the tenant's own sources.
  A server administrator gets it for every source. A distributor's entry shows no dialog to a
  tenant administrator.
- **Fetching icons**: the console calls the API with a Bearer token, so an `<img src>` cannot
  authenticate. It fetches an icon with its client (`?v=<sha256>`), shows it from a `blob:` URL,
  keeps it by hash for the session (one fetch per distinct icon), loads icons only for the rows in
  view, at most four at a time, and backs off on a `429` as told by `Retry-After`. The API's
  per-client rate (`serve.api_rate`, 20 a second) stays well clear.
- The channel lists and the Report & licence panel belong to the console redesign: it reads the
  channel routes above, and this spec changes nothing there.

### Part 2: distributor download statistics

#### Counting the source

- **A download records its build's source.** The serving path already has the release it serves,
  and the release's `source` (spec 0031: a promotion keeps it). The download key gains `source`, so
  a count says whose entry was downloaded whatever the channel's rule says later.
- **An install event** (spec 0010) gains `source` in its data. The daily installers pass counts
  distinct principals per (channel, source, name, version, platform), so `download_installers` gains
  `source` too. An event written before the change has none, and counts under `""`.
- **Rows before the migration** get the source `""` and count for no distributor. kista is not
  released, so no backfill is attempted. A backfill from releases would be ambiguous whenever a
  channel holds a version from two sources, or a release was purged since.
- **The tenant's statistics** (`GET …/stats/downloads`) gain `source` as a dimension, answered with
  its label (`upstream:<name>`, `publisher:<name>`, `tenant`, a removed source's as the catalogue
  labels it, `unknown` for counts from before). They are otherwise unchanged.

#### The route

```text
GET /api/v1/distributors/{name}/stats/downloads?from=&to=&type=&extension=&group=<d>[,<d>]
    d: week | extension (always grouped, naming them is allowed)
       | version | platform | duckdb_version
    → {from, to, min_tenants, group,
       rows: [{week, extension, [version], [platform], [duckdb_version],
               downloads, anonymous, installers?, tenants?, withheld, partial?}]}
```

This is the interface Enterest assumed (`group=week,extension`; rows `{week, extension, downloads,
installers, withheld}`), with `anonymous`, `tenants`, `partial` and the optional dimensions added.

- **Who**: distribution admins and server administrators (the `distributionAdmin` access a
  server-level route like `reserved-names` takes), with a fresh token. Today's distribution admins
  are server-wide (spec 0030) and so read any distributor's. The narrower distribution admin
  Enterest plans, bound to one distributor, will be refused (`404`) any other `{name}`: the route
  takes the distributor's name in its path for that reason. A distributor with no publishers
  answers no rows, the same as one whose extensions nobody downloaded.
- **`{name}`** is the distributor's name as its publishers are marked (`publishers.distributor`,
  spec 0033 phase B), path-escaped. Its publishers are every publisher of every tenant marked with
  that name, live, plus the removed ones remembered in `removed_distributor_publishers` with that
  name, so a removed publisher's entries keep counting. Their sources are `publisher:<id>`. A
  publisher marked again under another name counts for the name it has now, its history included:
  the mark is read at query time.
- **Weeks** are ISO weeks in UTC, Monday to Sunday, named by their Monday (`2026-10-26`). `from` and
  `to` are days, widened to whole weeks; the default is the 12 weeks up to today's; at most 53
  weeks. The current week is answered with `partial: true`.
- **`type`** is the package type (spec 0034), the default when not named: the counts of channels of
  that type.
- **`downloads`**, the headline figure, is the week's downloads as spec 0010 counts them (not
  deduplicated, a range from byte 0 included): the anonymous ones always, plus the authenticated
  ones unless they are withheld (below). **`anonymous`** is the anonymous part alone.
- **`installers`**, shown beside it, sums the daily distinct installers (spec 0010) over the days,
  the tenants and the dimensions grouped: a principal installing on two days, or in two tenants,
  counts twice. Weekly distinct installers cannot be computed: the install events they would come
  from are kept for as little as two days (`events.retention`). An installer is a principal holding
  `install`, so anonymous downloads have none, and `installers` is withheld with the authenticated
  part. It is left out when grouping by `duckdb_version`, as on the tenant's route. The website says
  plainly that it counts installer-days, not people.
- **`tenants`** is how many tenants contributed authenticated downloads to the row, left out when
  withheld.
- **No tenant is named**, nor any channel, nor anything per tenant.

#### The minimum, and anonymous downloads

The owner's decision (2026-10-29): the minimum protects organisations' tenants; anonymous downloads,
such as those of Enterest's public tenant serving free extensions without a token, single no one
out and always count.

- **What is anonymous**: a download that came without a valid token, spec 0010's `authenticated`
  false, already a column of `download_counts`. kista identifies anonymous downloads per download,
  not per tenant: nothing about such a download names a principal or an organisation, and anyone on
  the internet could have made it. No tenant attribute is needed, and none can be set wrong.
- **Anonymous downloads are never withheld**: a row's `anonymous` figure is answered whatever the
  number of tenants behind it.
- **Authenticated downloads** (with a valid token, so of a principal of some organisation) and the
  installers count only when at least `statistics.distributor_min_tenants` tenants contribute
  authenticated downloads to the row (default 3, at least 2). Otherwise they are **withheld**: the
  row says `withheld: true`, `downloads` equals `anonymous`, and `installers` and `tenants` are left
  out. A row with only withheld downloads is still listed, so a storefront can say that figures are
  withheld, but it carries no figure of them.
- An organisation's tenant whose nodes download a public release without a token adds to the
  anonymous figure too, by the same rule: such a download carries nothing that identifies it, and is
  one anyone could make. The website says so for tenants' administrators.
- Each answer applies the minimum at the grouping asked for, so every authenticated figure answered
  aggregates that many tenants. Comparing two groupings can still yield an authenticated figure by
  difference (an extension's week total minus the platforms answered gives the platforms withheld).
  Such a figure is a count without a tenant. kista never answers which tenants contribute, nor any
  tenant's own count (Security).

#### The query

- The distributor's sources are read first, in one query over `publishers` and
  `removed_distributor_publishers` by `distributor`. They are chunked by tenant, at most 500 sources
  a chunk to stay under SQL Server's parameter limit.
- Per chunk, one query over `download_counts`, and one over `download_installers`, filtered by
  `source IN (…)`, the day range, the extension and the type (`channel_id IN (SELECT id FROM
  channels WHERE package_type = ?)`). Days are mapped to weeks in SQL by a parameterised `CASE`
  (one branch per week, plain text comparison of `YYYY-MM-DD`, portable across the three dialects).
  The query groups by extension, week, `authenticated` and the dimensions asked, with `SUM` and
  `COUNT(DISTINCT tenant_id)`.
- Chunks partition the tenants, so their distinct tenant counts add up. The minimum is applied to
  the authenticated part after the chunks are merged.
- New indexes `(source, day)` on both tables keep it a range scan of the distributor's rows, never
  a scan of every tenant's.
- More than 100,000 answered rows is `400` (narrow it), as the tenant's route.

#### Limits

The general API rate per client applies. On top of it, each replica runs at most two of these
queries at once: a third waits up to five seconds, then gets `429` with `Retry-After: 5`. The answer
is `Cache-Control: no-store`, like the tenant's statistics. The storefront caches what it shows, and
the counts change every minute.

#### Data model (migration 0045)

`min_reader 45`: an older replica would add counts by the old key, which the new primary key no
longer matches, so its writes would fail.

```text
download_counts       + source (varchar 64 NOT NULL, as releases.source), in the primary key
                        (tenant_id, channel_id, source, name, package_version, platform,
                        duckdb_version, day, authenticated); index (source, day)
download_installers   + source, in the primary key (tenant_id, channel_id, source, name,
                        package_version, platform, day); index (source, day)
```

Both tables are rebuilt in the three dialects (create, copy with `source` `''`, drop, rename:
`ALTER TABLE … RENAME TO`, SQL Server's `sp_rename`). Migration 0011 left SQL Server's primary keys
unnamed, so they are not altered in place. The existing `(tenant_id, day)` indexes are recreated.
SQL Server's text columns are BIN2, as everywhere.

`statistics.distributor_min_tenants` (an integer, default 3, at least 2) is the one new
configuration key.

## Security

- **The icon is never active content.** Only PNG and WebP pass, by their signature bytes and a full
  decode. SVG and every other format are refused. The type served is kista's, with `nosniff`, a
  sandboxing CSP, `default-src 'none'`, `inline` disposition and `same-origin` resource policy, so a
  polyglot file (an image that is also HTML or script) is never rendered as anything but an image,
  even opened directly. The console shows it through `<img>` from a `blob:` URL of the type served.
- **Bounded input.** The body is at most 384 KiB, the icon at most 256 KiB and 512 pixels a side,
  checked from the header before decoding, so the decode allocates at most about 1 MiB. The
  summary is at most 160 characters of text without controls or bidirectional overrides. The
  console renders it as text, never as HTML or Markdown.
- **Who writes**: a publisher only its own entries, and only with a signed request (spec 0023's
  statement binds the method, the path naming the source, and the body's hash), while it
  holds `publish` on the name (a distributor's publisher: its catalogue rules). A tenant
  administrator writes only the tenant's own sources. A distributor's entry is the distributor's
  publisher's, its distribution admins' and the server administrators'. A tenant's
  administrator cannot restyle a distributor's product, and a distributor cannot restyle a tenant's
  own entries.
- **Revocation**: the write checks the publisher and its key under the auth lock that removing them
  takes, as an intake does (spec 0033 phase A2). A revoked key's in-flight write writes nothing.
- **Existence**: the metadata and icon routes answer `404` alike for an entry the caller may not
  read and one that does not exist. The channel routes answer only the extension's release
  readers, who already know the name and the channel. Install and anonymous callers get nothing
  new: no DuckDB route changes, nor the index, and there is no public JSON. Deduplication is
  invisible: an icon is read only through the reader's own metadata row, so knowing a hash proves
  nothing.
- **Tenant isolation**: metadata rows are per tenant. A shared icon row holds bytes that every
  tenant referencing it was sent itself.
- **Aggregated statistics** read across tenants by design. That is the owner's decision, for the
  distributor's own publishers' entries only. What protects the tenants:
  - only the distributor's sources' counts are read: a tenant's own build of the same name, an
    upstream's, or a distributor build re-published by the tenant under its own source is never
    counted;
  - no answer names a tenant or a channel;
  - every authenticated figure answered (downloads with a token, installers, tenants) aggregates at
    least `statistics.distributor_min_tenants` tenants (at least 2, default 3);
  - anonymous downloads are answered without the minimum (the owner's decision): they carry no
    principal and no organisation, and are what anyone could download.

  The residual risks, stated plainly. Comparing groupings can isolate an authenticated figure
  smaller than the minimum, which is still a count without a tenant. An organisation's tokenless
  downloads of a public release count in the anonymous figure: a distributor that knows only one
  organisation uses its extension could read that organisation's tokenless activity there, which is
  what anyone serving the same public file would see. A withheld row says that some authenticated
  downloads happened, not how many nor where. The reader is a distribution admin, a role the
  server's operator grants (spec 0030), not a tenant or the public. A dominance rule and noise are
  alternatives the owner may choose instead (below).
- **Fail closed**: an unknown distributor answers no rows. A source whose mark cannot be read is not
  counted. The minimum is never configurable below 2.
- **Retention**: counts keep `statistics.retention` (spec 0010). Nothing new is kept about a
  principal: the installers pass already counted principals per day without storing them.

## Testing

- **Store suite, three dialects**:
  - metadata write replacing both fields (left out or `null` clears), the same write again
    changing nothing;
  - icon deduplication across tenants, and collection: an unreferenced icon after the grace, one
    referenced again during the grace kept, an orphaned metadata row after its entry's last build;
  - the foreign key;
  - tenant deletion reaping the rows;
  - migration 0044 on a populated level-43 database;
  - migration 0045 on a populated level-44 database: the counts and installers kept with source
    `''`, and the new primary keys taking two sources of one (channel, name, version, platform);
  - the distributor query: two distributors, a removed distributor's publisher, a publisher marked
    again, a tenant's own `h3` beside the distributor's, the week mapping across a year's end, the
    type filter, the minimum on authenticated downloads only (anonymous downloads of a single tenant
    answered, its authenticated ones withheld with `withheld`), chunking (a small chunk size in the
    test), and two sources in one channel.
- **Icon checks**: unit tests with fixtures: PNG and WebP accepted; SVG, GIF, JPEG, APNG, animated
  WebP, a non-square image, under 64 and over 512 pixels, over 256 KiB, a declared type that is not
  the format, truncated and corrupt files, a PNG claiming 100000x100000 (refused from its header), a
  file with HTML after the image (accepted as an image, served as one).
- **API**:
  - the publisher's signed `PUT` (statement, body hash, replay);
  - each writer allowed and refused per the table, a distributor's entry refused to a tenant
    administrator, `404` for a missing entry, for a name the publisher may not publish and for
    another source named by a publisher; the `GET` of the metadata;
  - the `413` and `400`s;
  - the icon route's headers, `?v=` and its `304`;
  - the channel routes following the rule's source and its switch;
  - the catalogue reads carrying `summary` and `icon`;
  - the events and their registry;
  - the revoked key's write refused;
  - the distributor route: who may call it, the minimum and `withheld`, `anonymous` always
    answered, `installers` without `duckdb_version`, `partial`, `group=week,extension`, the
    semaphore's `429`, the 100,000-row limit;
  - the tenant route's `source` dimension.
- **Serving**: a download records its release's source (a promoted release keeps the original
  source). An install event carries `source`, and the daily pass keys installers by it.
- **CLI**: `catalogue meta`, its flags and file check; `list` and `show` printing the metadata.
- **Console, unit**: the icon cache (one fetch per hash, the four-at-a-time limit, `429` back-off),
  the Edit dialog's offering per role and source, the summary rendered as text.
- **Console, e2e** (`web/console/e2e/catalogue.spec.ts`): the fixture's distributor's publisher sets
  an icon and a summary by a signed request. The catalogue and the entry page show them. A tenant
  administrator edits the tenant's own entry, and is not offered the distributor's.
- **DuckDB e2e** (`e2e/distributors_test.go`): a distributor's publisher in three tenants publishes
  `kista_demo`, sets its metadata in each, and each tenant's channel places it. DuckDB at the pin
  installs it from each tenant with a token, and once without one from a public channel, plus a
  tenant's own build of the same name in one of them. After a flush, the distributor route answers
  the three tenants' downloads in one week row, without the tenant's own build. With the minimum
  raised to 4 the row is `withheld` and answers only the anonymous download. The icon route serves
  the bytes with the headers above.

## Delivery

One spec, two phases, each a PR with its own reviews and its own migration:

- **Phase 1: entry metadata**: migration 0044, the icon checks (`internal/icon`), the store, the
  release service's writes and reads with their authorisation, the routes, the CLI, the event, the
  collector's two steps, the console (catalogue and entry page), the website. About 1,800 lines with
  tests: the store and the service each about 300, the routes 250, the icon checks 150, the console
  300, the rest tests and docs.
- **Phase 2: distributor statistics**: migration 0045, the source in the download and installer
  keys (serving, the counter, the daily pass), the store's aggregate query, the route with its
  semaphore, the configuration key, the tenant route's `source` dimension, the website. About 1,000
  lines with tests.

Enterest can take each phase as it lands: phase 1 for its publishing pipeline, phase 2 for its
storefront.

## Alternatives considered

- **The icon in the publication's request** (a multipart body, or query parameters with a
  base64 field): it changes the publication body that spec 0023 signs and the spool reads, on two
  routes, and a publication would have to say whether a missing icon clears or keeps one. A separate
  signed `PUT` keeps the publication as it is.
- **Metadata per version** (an icon in the build or its footer): nobody wants an icon per version,
  DuckDB's footer has no room for it, and the catalogue would have to choose which version's to
  show.
- **The icon as a body in the tenant's storage domain** (spec 0005), deduplicated per domain: it
  needs a blob prefix, two-phase writes, the collector and the verifier, for files of at most 256
  KiB; the database's server-wide deduplication is both simpler and smaller.
- **Inline bytes in each metadata row**: no collection at all, but a distributor's icon stored once
  per tenant.
- **Re-encoding every icon to PNG**: it would strip any trailing content. The serving headers
  already make the bytes inert, and re-encoding would change the hash a distributor's re-send relies
  on, and grow WebP files.
- **SVG with sanitising**: sanitisers have a long history of bypasses. Raster icons at up to 512
  pixels are enough for every place the console and a storefront show one.
- **A public icon route** (anyone, for entries with public releases): DuckDB clients never ask for
  icons, and a public route would tell anyone which extensions a tenant holds. The storefront has
  the developer's icon itself.
- **Public statistics JSON in kista**, like DuckDB's community-extensions: the owner's decision is
  that figures appear only on Enterest's storefront. A tenant's statistics are its own.
- **Attributing old counts by the current rules**: a rule switched or a release purged would
  attribute downloads to the wrong source. Recording the source at download is exact.
- **Weekly distinct installers**: they need the week's install events, which `events.retention` may
  keep for only two days. `installers` sums daily distinct installers, and the website says so.
  (A first draft named it `installer_days`; Enterest's interface names it `installers`, adopted.)
- **A "public tenant" mark** exempting a tenant's downloads from the minimum: a tenant attribute to
  set and keep right, and a public tenant's authenticated downloads would still be a principal's.
  Anonymity is a property of the download (no token), which kista records already.
- **A partial `PUT` of the metadata** (a field left out keeps its value): Enterest's interface
  replaces both, which leaves no doubt about what a write meant.
- **A separate own-source route for publishers** (`PUT …/catalogue/extensions/{ext}/meta`, as the
  catalogue-only publication): one route naming the source serves publishers and administrators
  alike, as Enterest assumed.
- **A dominance rule** (no row where one tenant contributes more than, say, 80%) **or noise added
  to small figures**: stronger against inference by difference, at the cost of suppressing or
  blurring figures a storefront shows. Left to the owner (Follow-ups).
- **Suppressing at a fixed finest grain** (every figure a sum of answered finest cells, so no
  difference reveals anything): a small distributor's figures would mostly vanish, split by
  version, platform and DuckDB version.

## Changes to earlier specs

- **0005, 0016**: icons live in the database, not the blob store. The collector's hourly pass gains
  orphaned metadata and unreferenced icons.
- **0007**: new routes; the catalogue's reads gain `summary` and `icon`.
- **0010**: the download and installer keys gain the source; `install` events carry `source`; the
  tenant's statistics gain the `source` dimension; the distributor route and
  `statistics.distributor_min_tenants`.
- **0015**: the catalogue and entry pages show icons and summaries, with an Edit dialog.
- **0019**: tenant deletion reaps `entry_meta`.
- **0023**: one more route open to publishers' signed requests (`PUT` and `GET
  …/catalogue/{source}/extensions/{ext}/meta`, for the publisher's own source), with a JSON body
  signed as a promotion's.
- **0030**: distribution admins write a distributor's entries' metadata and read distributor
  statistics.
- **0033**: an entry gains metadata, which a withdrawal and a removed source keep; event
  `catalogue.meta`.
- **0034**: metadata is per package type, like the entry.

The website: `catalogue.md` (an entry's icon and summary, who sets them, the console),
`publishing.md` (the signed metadata `PUT` with `curl` and `ssh-keygen`, the bounds),
`observability.md` (the source dimension, distributor statistics, the minimum, the new event),
`api.md` (the routes), `console.md` (what the catalogue pages show), `configuration.md`
(`statistics.distributor_min_tenants`).

## Follow-ups

- `kista_publish.py --summary --icon` in `hugr-lab/extension-ci-tools`, and the template's
  workflow passing them; `e2e/CI_TOOLS_PIN` moves.
- Relaying metadata through a `repository` upstream (an organisation's kista mirroring a tenant of
  another kista would show the source's icons); it needs a read the upstream's credential may call.
- Metadata for DuckDB's core and community mirrors from DuckDB's own descriptions.
- More fields if asked: a homepage URL, a licence name, keywords.
- Distributor statistics by usage report (spec 0012), and a CLI for them.
- A dominance rule or noise for distributor statistics, should the owner want more than the minimum.
- The narrower, per-distributor distribution admin (Enterest's), which the route is ready for.
