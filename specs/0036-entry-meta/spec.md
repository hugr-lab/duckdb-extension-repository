# Spec 0036: Entry icons and distributor statistics

- **Status**: draft
- **Date**: 2026-10-29
- **Author**: vgsml, Claude

## Summary

Two additions around the catalogue (spec 0033), both asked for by Enterest:

1. **Entry metadata.** A catalogue entry (tenant, package type, source, name) gets an optional
   **icon** (a small square PNG) and a **summary** (one line of plain text). A publisher sets them
   for its own entries with a signed request (spec 0023). A tenant's administrators set them for
   the tenant's own sources. A distributor's entries are set by the distributor's publisher or a
   distribution admin, never by the tenant. The icon is a blob in the tenant's storage domain. kista
   serves it from a route of its own, with headers it sets itself. The console shows icon and
   summary in the catalogue and on an entry's page.
2. **Distributor download statistics.** A distribution admin reads the distributor's downloads per
   extension per week, with installer-days beside them, summed over every tenant. Downloads now
   record whose entry they were. No tenant is named. Tenants listed as public in the configuration
   always count. Every other tenant's downloads are withheld from a row unless enough such tenants
   contribute. This is what Enterest's storefront shows. kista publishes no public statistics.

## Problem

- **An entry is a name and nothing more.** The catalogue (spec 0033), a channel's Sources tab and
  the release pages show `geo_tools` from `publisher:enterest`, and nothing that says what it is.
  A developer has an icon and a one-line description for the extension. Enterest has them in its
  storefront, but cannot hand them to the tenants it publishes into. A tenant's own CI cannot
  either.
- **A distributor cannot see adoption.** Enterest publishes a developer's extension into many
  tenants. Each tenant counts its downloads (spec 0010, `GET tenants/{t}/stats/downloads`), but only
  the tenant reads them. The storefront wants downloads per extension per week, as DuckDB's
  community-extensions page shows. Today only a server token reading every tenant could add them
  up. That reads far more than the figure, and shows which tenant installs what.
- **Download counts do not say whose build was downloaded.** A count is kept per (channel, name,
  version, platform, DuckDB version, day, authenticated). Since spec 0033 a channel may hold one
  version from two sources, and a rule may switch sources. So a stored count cannot be attributed to
  the distributor's entry later. Install events are fine: they carry the release id, and a release
  has a source. Only `download_counts` lacks it.

## Design

### Part 1: entry metadata

#### What an entry carries

An entry's **metadata** is a row of its own, `entry_meta`, keyed as the entry: (tenant, package
type, source, name). The source is stored as builds store it (`upstream:<id>`, `publisher:<id>`,
`tenant`), so renames do not matter. It holds:

- **summary**: plain text, at most 160 characters, or none.
- **icon**: the SHA-256 of a PNG in the tenant's storage domain, with its width and height, or none.
- **set_by, set_at**: who wrote it last, and when.
- **signed_at**: the `signed-at` of the last write made by a signed request (replay protection,
  below).

Metadata belongs to the entry, not to a version or a build. A publication never touches it, so a
new version keeps the entry's icon. The row is kept when every build of the entry is purged or
collected, so a re-publication after a purge finds its metadata again. It goes only with the
tenant: `entry_meta` joins the reaper's tables (`store.ReapTables`, which
`tenantdelete_test.go` checks).

#### The summary

- `""` after trimming spaces clears it, as `null` does.
- Normalised to NFC, then counted in code points: at most 160.
- Every character must be printable (`unicode.IsPrint`) or a space.
- Refused: categories Cf, Zl and Zp. This covers the bidirectional controls, LRM, RLM and ALM, the
  zero-width characters, U+2028 and U+2029, and tag characters.
- At most 4 combining marks after any base character.
- It is never markup. The console renders it as text, and the CLI prints it as is.

#### The icon

- **PNG only.** The checks, in order, all refusing with `400`:
  1. The PNG signature, then the chunks. Allowed: `IHDR`, `PLTE`, `tRNS`, `IDAT`, `IEND`. Each at
     most once, except `IDAT`. Any other chunk (`tEXt`, `iCCP`, `sRGB`, `pHYs`, `acTL` and so on) is
     refused, not stripped. No bytes after `IEND`.
  2. The header (`png.DecodeConfig`): square, 64 to 512 pixels a side. This happens before
     decoding, so a decompression bomb never allocates pixels.
  3. A full decode with Go's `image/png`, and `img.Bounds()` must equal the header's size.
     Decodes run under a semaphore (4 per replica). The worst case is a 16-bit 512x512 image, 2 MiB.
- **Size**: at most 256 KiB. A 256x256 PNG fits.
- The declared `content_type` must be `image/png`.
- Publishers are told how to strip chunks (`oxipng --strip all`, `pngcrush -rem allb`). Most
  exporters add `pHYs` or `sRGB`.
- **Bytes kept as sent**: the checks refuse rather than rewrite, so the stored bytes are exactly
  what passed. The same icon has one hash wherever it goes.

#### Storage: a blob in the tenant's storage domain

- **The object** is `icons/<sha256>` in the tenant's storage domain (spec 0005), content-addressed.
  Tenants sharing a domain share the object.
- **It counts toward the byte cap** of the entry's source (`catalogue.max_bytes`, spec 0033): each
  entry's icon size counts once. A write beyond the cap is `429`, as an arrival.
- **Write order: object first, row second.** A write:
  1. checks the image;
  2. writes the object (always, even when it exists: this refreshes its modification time);
  3. in one transaction holding the lock `kista/icon/<domain>/<sha256>`, checks that the object
     exists, then upserts the row.

  If the object is gone at step 3 (the collector won a race), the write puts it again under the
  lock before committing. A row never points to a missing object.
- **Collection** is a new step of spec 0016's collector, per domain, under the domain's lease:
  - It lists `icons/` in the domain.
  - It skips an object modified within `gc.grace`. That is an upload whose row may not be
    committed yet.
  - For an older object, it takes the same lock and checks that no `entry_meta` row of any tenant
    in the domain references the hash. If none does, it deletes the object.
- **Tenant deletion**: the reaper deletes the tenant's rows. Its icon objects then go with the
  collector's next pass, or with the domain when the domain was the tenant's alone.

#### The API

```text
PUT /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
    {"summary": string | null,
     "icon": {"content_type": "image/png", "data": "<standard base64>"} | null}
    → 200 {"summary": string | null, "icon": {"sha256", "width", "height"} | null, "set_at",
           "set_by" (to tenant administrators, server administrators and distribution admins only)}
GET /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
    → 200, the same answer
GET /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/icon[?type=]
    → 200 image/png
```

- **Replace semantics.** A `PUT` states the whole metadata: a field left out or `null` clears it.
  Writing the same metadata again changes nothing and makes no event.
- **Errors**:
  - `400`, type `invalid`, for every bound (summary, format, chunks, size, dimensions,
    `content_type`), naming the bound.
  - `413` above the body limit.
  - `404` for an entry that does not exist, a source the caller may not write, or a name the
    distributor's publisher may not publish (reserved). Always the same answer.
  - `409`, type `stale`, when a signed request's `signed-at` is older than the entry's stored
    `signed_at`. A captured older request cannot undo a newer one within spec 0023's five minutes.
    Signed-at equal to the stored one is accepted (the same request again changes nothing).
- **Body limit.** `body()` hard-codes 64 KiB and its 413 message (`internal/api/manage.go`). It
  gains a limit parameter. This route passes 384 KiB (a 256 KiB icon is about 342 KiB in base64),
  with its own message, "the body is at most 384 KiB".
- **The catalogue's reads** (`GET …/catalogue`, `GET …/catalogue/{source}/extensions/{ext}`) gain
  `summary`, `icon` (`{sha256, width, height}`) and `distributor: true` on a distributor's
  publisher's entries.
- **Signing.** The `PUT` is signed as spec 0023 says: the statement binds the method, the path with
  the source, and the SHA-256 of the JSON body, as for a promotion. The format does not change.
  The icon travels in this request, not in the publication: the publication's body stays the file
  alone, on both publication routes.

#### Who may write and read

The route uses a new access rule: **any verified caller of the tenant** (a principal's token, a
server token, a distribution admin, or a publisher's signed request). The service decides, and every
refusal is `404`. The signed-publisher branch of `decide` is built by spec 0037's first PR, which
lands before this spec.

The service runs one of three checks, chosen by the entry's source:

1. **The tenant's own sources** (an upstream, a tenant's own publisher, `tenant`): a tenant-wide
   administrator, `mayAdminTenant` with `authz.Resource{Tenant}`. A server administrator passes it.
2. **A distributor's publisher's source**, live or removed: `mayDistribute` (a distribution admin or
   a server administrator). Tenant administrators never write a distributor's entry.
3. **A publisher's signed request**, for its own source only:
   - `publisher:<own name>` and `publisher-id:<own id>` both mean its own source, resolved by id.
     Any other source is `404`.
   - A distributor's publisher must also pass `distributorMay` for the name (not reserved
     server-wide, nor in the tenant for another holder).
   - A tenant's own publisher needs nothing more. Its entry exists only because it published the
     name.

For every caller, the entry must exist: its source has at least one build of the name and type in
the catalogue (held, available or withdrawn). The `GET`s take the same checks. Catalogue readers
(spec 0033) read too.

**Revocation.** The row's transaction takes the tenant's auth lock and checks the publisher and its
key, as `PublicationLive` does (`internal/store/holds.go`). A write in flight when the key is
revoked writes nothing.

**Rate.** One tenant's metadata writes are bounded by the API's per-client rate and by its byte
caps.

#### Serving the icon

- Authorization and the entry lookup come first. Nothing is read from the blob store before both
  pass.
- Headers:
  - `Content-Type: image/png`, kista's own;
  - `X-Content-Type-Options: nosniff`;
  - `Content-Security-Policy: default-src 'none'; sandbox`;
  - `Cross-Origin-Resource-Policy: same-origin`;
  - `Content-Disposition: inline; filename="icon.png"`;
  - `Cache-Control: private, no-store`.
- An object missing at read time is `404`, and logged as an error.

#### CLI

- `kista admin catalogue meta <t> <source> <name> [-summary <text>] [-icon <file.png>]
  [-clear-summary] [-clear-icon] [-type t]`, a server administrator's (the operator's CLI on the
  database, as `catalogue withdraw`). It reads the current metadata and writes both fields: a field
  not named keeps its value. Without flags it prints the metadata.
- `catalogue show` prints `summary:` and `icon: <sha256> <w>x<h>`.
- `kista_publish.py` in `hugr-lab/extension-ci-tools` gains `--summary` and `--icon` (Follow-ups).
  It always sends both from its flags: a CI states its metadata whole. Given neither flag, it sends
  nothing.

#### Events

`catalogue.meta`, the tenant's event, written in the same transaction:

- `source` (its label), `name`, the package type when not the default;
- `summary` (the new text, `""` when cleared), `icon` and `icon_before` (hashes, `""` for none);
- the actor is the writer. A distribution admin shows under spec 0030's generic `distribution-admin`
  label, unless unmasked.

The icon's bytes are never in an event. `GET /api/v1/event-kinds` lists the kind.

#### Data model (migration 0044)

Additive, in the three dialects, with no `min_reader` bump: an older replica ignores the table.

```text
entry_meta   tenant_id (fk tenants), package_type, source, name,
             summary (160 characters, NULL; nvarchar on SQL Server), icon_sha256 (char 64, NULL),
             icon_width, icon_height (int, NULL), set_by (varchar 400), set_at,
             signed_at (NULL); pk (tenant_id, package_type, source, name); index (icon_sha256)
```

#### The console

- **Catalogue**: a 24-pixel icon before the name, and the summary under it in muted text.
- **An entry's page**: a 64-pixel icon beside the title, the summary under it, and an **Edit…**
  dialog (summary, icon file, clear). The dialog is hidden for entries the catalogue marks
  `distributor: true`. The API decides anyway.
- **Loading an icon**: the console calls the API with a Bearer token, so `<img src>` cannot
  authenticate. It fetches the icon with its client, builds a `Blob` of type `image/png` (from a
  fixed allow-list, never from the response's header), and shows it from a `blob:` URL. Nothing
  more: no caching layer and no batching.
- Enterest's console, if it shows these icons the same way, needs exactly `blob:` in its CSP's
  `img-src`.
- Channel lists and the Report & licence panel come with the console redesign, which adds the
  channel-scoped reads it needs.

### Part 2: distributor download statistics

#### Counting the source

- **A download records its release's source.** The serving path already holds the release, and the
  release's `source` (spec 0031; a promotion keeps it). The download key gains `source`.
- **Installers.** The daily installers pass takes the source from the install event's release:
  `source` in the event's data from now on, else the release row by id. `download_installers` gains
  `source`. An install whose release is gone counts under `""`.
- **Rows before the migration** get the source `""` and count for no distributor. kista is not
  released, so there is no backfill.
- **The tenant's statistics** (`GET …/stats/downloads`) gain `source` as a dimension, answered as a
  label (`unknown` for `""`). Nothing else changes there.

#### The route

```text
GET /api/v1/distributor-stats/downloads?distributor=<name>&from=&to=[&type=]
    → {"distributor", "from", "to", "min_tenants",
       "rows": [{"week", "extension", "downloads", "public", "installer_days", "withheld"}]}
```

- **The distributor is a query parameter.** Its name may need escaping, and the API answers `404`
  for any `%` in a path (`internal/api/api.go`, `ServeHTTP`). `?type=` is the package type (spec
  0034); the route joins `takesType`.
- **Who**: distribution admins and server administrators, with a fresh token. kista assumes one
  distributor per server (Enterest). The role stays server-wide, as spec 0037 keeps it.
- **The distributor's sources** are every publisher of every tenant marked with that name
  (`publishers.distributor`), plus the removed ones remembered in `removed_distributor_publishers`
  with that name. So a removed publisher's entries keep counting. The mark is read at query time.
- **Grouping is fixed**: one row per (week, extension).
- **Weeks** are complete ISO weeks in UTC, Monday to Sunday, named by their Monday (`2026-10-19`).
  Only weeks that ended at least two days ago are answered, so the counts are flushed and the
  daily installers pass is done. `from` and `to` are days, widened to whole weeks. A `to` beyond the
  last complete week is clamped to it. The default is the 12 weeks up to it. At most 53 weeks.
- **Rate**: the API's per-client rate applies, as for every route.

#### The figures

- **`downloads`**: the week's downloads as spec 0010 counts them (not deduplicated; a range from
  byte 0 counts).
- **`public`**: the part from public tenants.
- **`installer_days`**: daily distinct installers (spec 0010), summed over the days and the tenants.
  A principal installing on two days, or in two tenants, counts twice. Weekly distinct installers
  cannot be computed: install events may be kept for only two days (`events.retention`).
- **`withheld`**: true when the non-public part was left out (below).

No tenant is named, nor any channel, nor any per-tenant figure.

#### Public tenants and the minimum

- **Public tenants** are listed by name in `statistics.public_tenants`, for example Enterest's
  tenant that serves free extensions to anyone. All their downloads count, with a token or
  without. They are outside the minimum and do not count toward it.
- **Every other tenant** is under the minimum, whether its downloads carry a token or not.
- **Downloads.** A row's non-public downloads count only when at least
  `statistics.distributor_min_tenants` distinct non-public tenants contributed downloads to it
  (default 3, at least 2). When at least one but fewer did, the row is `withheld`: `downloads`
  equals `public`, and `installer_days` is left out.
- **Installer-days** take the minimum separately: the non-public part counts only when at least the
  minimum of distinct non-public tenants have installers in the row. Otherwise `installer_days` is
  left out. A row with installers from public tenants only answers theirs.
- **Fail closed.** An empty list exempts no one. A listed name that matches no tenant exempts
  nothing, and is logged at start. The minimum cannot be set below 2.
- **Which tenants count.** A suspended tenant counts: its downloads happened, and it still exists.
  A tenant whose deletion has started does not (spec 0019: its statistics go with it).

#### The query

- First, the distributor's sources, from `publishers` and `removed_distributor_publishers`, chunked
  by tenant (at most 500 sources a chunk, under SQL Server's parameter limit).
- Per chunk, one query over `download_counts` and one over `download_installers`:
  - filtered by `source IN (…)`, the day range, the type (`channel_id IN (SELECT id FROM channels
    WHERE package_type = ?)`), and tenants not being deleted;
  - days mapped to weeks by a parameterised `CASE` (one branch per week, plain comparison of
    `YYYY-MM-DD` text, the same in the three dialects);
  - grouped by week, extension and tenant, with `SUM`.
- Go adds the rows up per (week, extension), counts the distinct non-public tenants, and applies the
  minimum.
- New indexes `(source, day)` on both tables keep it a range scan of the distributor's rows.

#### Data model (migration 0045)

`min_reader 45`: an older replica would add counts by the old key, which the new primary key no
longer matches.

```text
download_counts       + source (varchar 64, NOT NULL, default ''), in the primary key
                        (tenant_id, channel_id, source, name, package_version, platform,
                        duckdb_version, day, authenticated); index (source, day)
download_installers   + source, in the primary key (tenant_id, channel_id, source, name,
                        package_version, platform, day); index (source, day)
```

- **PostgreSQL**: in place. `ADD COLUMN source … DEFAULT ''`, drop the primary key, add the new one,
  create the index.
- **SQL Server**: in place. Add the column with a default. Migration 0011 left the primary keys
  unnamed, so the migration finds each name in `sys.key_constraints` and drops it by dynamic SQL,
  then adds the new key and the index. BIN2, as every text column.
- **SQLite**: rebuild. Create the new table, copy with `source` `''`, drop the old one, rename, and
  create the indexes after the old table is gone. No other table references these, so the
  foreign-keys-off path is not needed.

New configuration: `statistics.distributor_min_tenants` (integer, default 3, at least 2) and
`statistics.public_tenants` (tenant names, default none).

## Security

- **The icon is never active content.** Only PNG passes: a strict chunk allow-list, nothing after
  `IEND`, and a full decode whose size must match the header. SVG and every other format are
  refused. The type served is kista's, with `nosniff`, `default-src 'none'; sandbox`, `inline` and
  `same-origin`. The console builds its `Blob` with a fixed type. So the bytes are only ever an
  image, even opened directly.
- **Bounded input.** The body is at most 384 KiB and the icon 256 KiB. Size is checked from the
  header before decoding, and decodes are limited to 4 at a time: at most 2 MiB each. The summary
  is at most 160 characters, with no format or line-separator characters and few combining marks.
- **Who writes.** A publisher writes only its own entries, and only by a signed request. Tenant
  administrators write only the tenant's own sources. A distributor's entries belong to its
  publisher, the distribution admins and the server administrators. A tenant cannot restyle a
  distributor's product, and a distributor cannot restyle a tenant's entries.
- **Replay.** Within spec 0023's five minutes, a captured request can be sent again. Sending the
  same request again changes nothing. An older request cannot overwrite a newer one (`409 stale`).
- **Revocation.** The write checks the publisher and its key under the lock their removal takes
  (`PublicationLive`).
- **Existence.** Every refusal on the metadata and icon routes is `404`, the same as for an entry
  that does not exist. Install and anonymous callers learn nothing new: the DuckDB routes and the
  index do not change, and there is no public JSON.
- **Tenant isolation.** Icons live in the tenant's own storage domain and count against its caps.
  Tenants sharing a domain share an object only when both were sent the same bytes. Every read goes
  through the reader's own row, so a hash proves nothing.
- **Aggregated statistics** read across tenants by design, for the distributor's own entries only
  (the owner's decision). What protects the tenants:
  - only the distributor's sources count: a tenant's own build of the same name, or an upstream's,
    never does;
  - no answer names a tenant or a channel, and every row is a sum;
  - grouping is fixed, so no two answers can be subtracted to isolate a smaller group;
  - a non-public tenant's downloads appear only in a row with at least the minimum of non-public
    tenants (default 3), and installer-days the same.

  The residual risk: a public tenant's figures are answered whatever their size. That is the point
  of listing it. Only the server's operator lists a tenant, by its configuration, and an empty list
  exempts no one. A `withheld` row tells that between one tenant and the minimum downloaded the
  extension, not which or how many times.
- **Retention.** Counts follow `statistics.retention` (spec 0010). Nothing new is kept about a
  principal.

## Testing

- **Store suite, three dialects**:
  - metadata write and replace, `""` and `null` clearing, the same write changing nothing,
    `signed_at` and `stale`;
  - the reaper (`ReapTables`), and a re-publication after a purge finding its metadata;
  - migration 0044 on a level-43 database; migration 0045 on a level-44 database: counts and
    installers kept with source `''`, and two sources of one (channel, name, version, platform)
    both stored;
  - the distributor query: a removed distributor's publisher, a tenant's own `h3` beside the
    distributor's, public tenants (with and without tokens) never withheld, the minimum on
    downloads and on installers separately, a deleting tenant left out and a suspended one counted,
    weeks across a year's end, the clamp to complete weeks, the type filter, chunking (a small
    chunk size in the test).
- **Icon checks** (fixtures): a valid PNG accepted; each disallowed chunk, a repeated chunk, bytes
  after `IEND` (an HTML tail included), SVG, JPEG, GIF, WebP, a non-square image, under 64 and over
  512 pixels, over 256 KiB, a wrong `content_type`, truncated and corrupt files, a header claiming
  100000x100000 (refused before decoding), and an `IDAT` whose image does not match the header.
- **Blob collection**: an unreferenced icon deleted after the grace; a fresh one kept; a write
  racing the collector ending with its object present.
- **API**:
  - each check of the three, and each refusal `404`; `publisher:<name>` and `publisher-id:<id>` for
    a signed request; a tenant administrator refused a distributor's entry;
  - replace semantics, the `400`s, the `413` with its message, `409 stale`;
  - `set_by` shown only to administrators and distribution admins;
  - the icon route's headers, authorization before the blob read;
  - the catalogue reads with `summary`, `icon` and `distributor`;
  - the event and its registry, the distribution admin's label;
  - a revoked key's write refused;
  - the statistics route: who may call it, `?distributor=`, `?type=`, the figures and `withheld`;
  - the tenant route's `source` dimension.
- **Serving**: a download records its release's source; a promoted release keeps the original.
  The daily pass attributes installers by the event's release.
- **CLI**: `catalogue meta` with each flag, keeping the field not named; `catalogue show`.
- **Console, unit**: the Edit dialog hidden for `distributor: true`, the Blob type fixed, the
  summary rendered as text.
- **Console, e2e** (`web/console/e2e/catalogue.spec.ts`): the fixture's distributor's publisher sets
  an icon and a summary by a signed request; the catalogue and the entry page show them; a tenant
  administrator edits the tenant's own entry and gets no dialog for the distributor's.
- **DuckDB e2e** (`e2e/distributors_test.go`): a distributor's publisher in four tenants publishes
  `kista_demo` and sets its metadata. One tenant is listed as public. DuckDB at the pin installs
  from each, plus a tenant's own build of the same name. The test sets the store's clock
  (`Store.Now`) past the week, flushes the counts and runs the daily pass. The route answers one
  row without the tenant's own build. With the minimum raised to 4, the row is `withheld` and
  `downloads` equals `public`. The icon route serves the bytes with the headers above.

## Delivery

The order across specs: spec 0037 PR 1, then this spec's phase 1 (migration 0044), then its
phase 2 (migration 0045, `min_reader 45`), then spec 0037 PR 2 (migration 0046).

- **Phase 1: entry metadata.** Migration 0044, the PNG checks (`internal/icon`), the blob objects
  and their collection step, the store, the service's checks, the access rule, `body()`'s limit
  parameter, the routes, the CLI, the event, the console, the website. About 1,600 lines with tests.
- **Phase 2: distributor statistics.** Migration 0045, the source in the download key and the
  installers pass, the aggregate query, the route, the two configuration keys, the tenant route's
  `source` dimension, the website. About 800 lines with tests.

## Alternatives considered

- **The icon in the publication's request**: it changes the body spec 0023 signs and the spool
  reads, on two routes. A separate signed `PUT` leaves publication alone.
- **Metadata per version**: no one wants an icon per version, and the catalogue would have to pick
  one.
- **Icons in the database, deduplicated server-wide** (the first draft): it grows the database with
  every tenant's icons and lets a shared row answer for another tenant's bytes. In the tenant's
  storage domain, icons count against its caps and stay with its data.
- **WebP**: one more decoder and dependency, for files PNG already covers at these sizes.
- **Stripping disallowed chunks**: it rewrites what the publisher sent, and the stored hash would
  differ from the file's. Refusing is simpler and tells the publisher why.
- **SVG with sanitising**: sanitisers have a long history of bypasses.
- **A partial `PUT`** (a field left out keeps its value): replacing both leaves no doubt about
  what a write meant.
- **A separate source-less route for publishers**: one route naming the source serves everyone, and
  the signed statement binds the source.
- **Channel-scoped metadata and icon reads** now: no screen needs them before the console redesign.
- **Public statistics JSON in kista**: the owner's decision is that figures appear only on
  Enterest's storefront.
- **Anonymity per download** (tokenless downloads always counted, the second draft): an
  organisation's tokenless downloads would escape the minimum. A tenant listed as public by the
  operator is what is meant, and the operator decides it.
- **Free grouping** (by version, platform, DuckDB version): subtracting one grouping from another
  could isolate a small group. A fixed grouping cannot be subtracted.
- **Attributing old counts by today's rules**: a rule switched or a release purged would attribute
  downloads wrongly. Recording the source at download is exact.

## Changes to earlier specs

- **0005, 0016**: a new object kind, `icons/<sha256>`, in a tenant's storage domain; the collector
  gains a step that deletes unreferenced icons after the grace.
- **0007**: new routes; the catalogue's reads gain `summary`, `icon` and `distributor`.
- **0010**: the download and installer keys gain the source; install events carry `source`; the
  tenant's statistics gain the `source` dimension; the distributor route,
  `statistics.distributor_min_tenants` and `statistics.public_tenants`.
- **0015**: the catalogue and entry pages show icons and summaries, with an Edit dialog.
- **0019**: the reaper deletes `entry_meta`.
- **0023**: one more route open to publishers' signed requests (`PUT` and `GET
  …/catalogue/{source}/extensions/{ext}/meta`, own source only). Its replay list gains: an older
  signed metadata write is refused `409 stale`, and the same one again changes nothing.
- **0030**: distribution admins write a distributor's entries' metadata and read distributor
  statistics.
- **0033**: an entry gains metadata, kept through withdrawal, purges and source removal; the
  catalogue answers `distributor`; event `catalogue.meta`; icons count toward `catalogue.max_bytes`.
- **0034**: metadata is per package type, like the entry.

The website: `catalogue.md` (icon and summary, who sets them, the console), `publishing.md` (the
signed `PUT` with `curl` and `ssh-keygen`, the PNG rules and how to strip chunks),
`observability.md` (the `source` dimension, distributor statistics, public tenants, the minimum, the
event), `api.md`, `console.md`, `configuration.md` (the two keys).

## Follow-ups

- `kista_publish.py --summary --icon` in `hugr-lab/extension-ci-tools`, the template's workflow
  passing them, and `e2e/CI_TOOLS_PIN` moved.
- Several distributors on one server: re-marking a publisher under another distributor moves its
  whole history to the new name, since the mark is read at query time. Fix this when a second
  distributor comes, together with a per-distributor role.
- Channel-scoped reads of metadata and icons, with the console redesign.
- Relaying metadata through a `repository` upstream.
- Metadata for DuckDB's core and community mirrors.
- More fields if asked: a homepage URL, keywords.
