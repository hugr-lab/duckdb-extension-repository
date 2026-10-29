# Spec 0036: Entry icons and distributor statistics

- **Status**: implemented (phases 1 and 2)
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
- **icon**: the SHA-256 of a PNG in the tenant's storage domain, with its width, height and size in
  bytes, or none.
- **set_by, set_at**: who wrote it last, and when.
- **signed_at, signed_body**: the `signed-at` and the body's SHA-256 of the last accepted signed
  write (replay protection, below).

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
- Refused too: the invisible fillers U+115F, U+1160, U+3164 and U+FFA0 (the Hangul fillers) and
  U+2800 (the blank Braille pattern). They are printable letters or symbols, so the rules above
  pass them, yet they show nothing. Enterest's summary rules (its spec 0010) refuse them too: the
  two must agree, so a summary Enterest accepts is one kista accepts.
- At most 4 combining marks after any base character.
- It is never markup. The console renders it as text, and the CLI prints it as is.

#### The icon

- **PNG only.** The checks, in order, all refusing with `400`:
  1. The PNG signature, then the chunks. Allowed:
     - the image's own: `IHDR`, `PLTE`, `tRNS`, `IDAT`, `IEND`;
     - the small fixed-size colour and physical chunks most exporters write, with their exact
       lengths checked: `sRGB` (1 byte), `gAMA` (4), `cHRM` (32), `pHYs` (9), `bKGD` (1, 2 or 6,
       as the colour type says).

     Each at most once, except `IDAT`, in the order the PNG specification requires: `cHRM`, `gAMA`
     and `sRGB` before `PLTE`, `bKGD` after it, all of them before the first `IDAT`. Refused, not
     stripped: `iCCP`, every text chunk (`tEXt`, `zTXt`, `iTXt`), `eXIf`, animation (`acTL`,
     `fcTL`, `fdAT`), `sPLT`, and any chunk not listed. No bytes after `IEND`. The `IDAT` chunks
     must be consecutive, and their data, concatenated, must decompress with zlib reaching its end
     exactly at the last `IDAT`'s last byte. Go's decoder ignores trailing `IDAT` data
     (`image/png`, `reader.go`), so kista checks this itself.
  2. The header (`png.DecodeConfig`): square, 64 to 512 pixels a side. This happens before
     decoding, so a decompression bomb never allocates pixels.
  3. A full decode with Go's `image/png`, and `img.Bounds()` must equal the header's size.
     Decodes run under a semaphore (4 per replica). The worst case is a 16-bit 512x512 image, 2 MiB,
     and about 4 MiB at a decode's peak when it is interlaced (its passes and the image).
- **Size**: at most 256 KiB. A 256x256 PNG fits.
- The declared `content_type` must be `image/png`.
- For a refused file, the website tells publishers how to strip chunks (`oxipng --strip all`,
  `pngcrush -rem allb`). Enterest re-encodes icons with Go's encoder, which writes none of the
  optional chunks.
- **Bytes kept as sent**: the checks refuse rather than rewrite, so the stored bytes are exactly
  what passed. The same icon has one hash wherever it goes.

#### Storage: a blob in the tenant's storage domain

- **The object** is `icons/<sha256>` in the tenant's storage domain (spec 0005), content-addressed.
  Tenants sharing a domain share the object.
- **It counts toward the byte cap** of the entry's source (`catalogue.max_bytes`, spec 0033).
  `entry_meta` keeps `icon_bytes`. A source's icons count as the sum of `icon_bytes` over its
  `entry_meta` rows, added to `SourceUsage`'s bytes. They count whether or not the entry still has
  builds, since metadata outlives purges. A write that would go beyond the cap is `429`, type
  `catalogue-full` (added by spec 0037's first PR). As `room()` exempts an administrator's add,
  tenant administrators and server administrators are not bounded. Publishers' signed writes and
  distribution admins are.
- **Writing and collecting reuse spec 0016's protocol for streams**, unchanged, in two tables of
  their own shaped as spec 0016's: `icon_claims` and `icon_tombstones`. The lock is
  `kista/blob/<domain>/icon/<sha256>`. A write:
  1. checks the image;
  2. under the lock, claims the hash (waiting while a tombstone says `deleting`);
  3. uploads the object, always, even when it exists;
  4. under the lock, checks its claim, upserts the row, and removes its claim and any tombstone.

  A row is never committed without its object in place.
- **Collection** is a new step of spec 0016's collector, per domain, under the domain's lease. It
  lists `icons/`, marks an object no `entry_meta` row of the domain's tenants references and no
  live claim holds, and deletes marked objects older than `gc.grace`. Deleting follows spec 0016's
  stamped `deleting` state: no network call under the lock, a delete of unknown outcome left
  `deleting` and finished by the resolver after `deleteStale`. It reuses `blob/gc.go`'s code paths.
- **Tenant deletion**: the reaper deletes the tenant's rows. Its icon objects then go with the
  collector's next pass, or with the domain when the domain was the tenant's alone.

#### The API

```text
PUT /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
    {"summary": string | null,
     "icon": {"content_type": "image/png", "data": "<standard base64>"}
           | {"sha256": "<the stored icon's hash>"} | null}
    → 200 {"summary": string | null, "icon": {"sha256", "width", "height"} | null, "set_at",
           "signed_at",
           "set_by" (to tenant administrators, server administrators and distribution admins only)}
GET /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/meta[?type=]
    → 200, the same answer
GET /api/v1/tenants/{t}/catalogue/{source}/extensions/{ext}/icon[?type=]
    → 200 image/png
```

- **Replace semantics.** A `PUT` states the whole metadata: a field left out or `null` clears it.
  Writing the same metadata again changes nothing and makes no event.
- **Keeping the icon.** `"icon": {"sha256": …}` naming the stored icon's hash keeps it. A hash that
  no longer matches is `409`, type `conflict` ("the icon changed"): someone else wrote meanwhile,
  and the client reads again. This is how a client changes the summary alone: the CLI,
  `kista_publish.py` and the console's summary-only edit read the metadata and send the icon back
  by its hash.
- **Errors**:
  - `400`, type `invalid`, for every bound (summary, format, chunks, size, dimensions,
    `content_type`), naming the bound.
  - `413` above the body limit.
  - `404` for an entry that does not exist, a source the caller may not write, or a name the
    distributor's publisher may not publish (reserved). Always the same answer.
  - `409`, type `stale`, for a signed request whose `signed-at` is older than the entry's stored
    `signed_at`, or equal to it with another body hash than `signed_body`. Every accepted signed
    write advances both, no-ops included. A write by a token that changes something sets
    `signed_at` to the later of the server's now and the stored `signed_at` (and clears
    `signed_body`), so no signed request made before it can undo it and the mark never goes back; a
    token's write that changes nothing writes nothing. A captured request
    therefore never undoes a newer write within spec 0023's five minutes, and the same request
    again changes nothing. A client answered `stale` reads again; if the stored `signed_at` is ahead
    of its clock, it waits until its clock passes it.
  - `429`, type `catalogue-full`, beyond the source's byte cap (above).
- **Body limit.** `body()` hard-codes 64 KiB and its 413 message (`internal/api/manage.go`). It
  gains a limit parameter. This route passes 384 KiB (a 256 KiB icon is about 342 KiB in base64),
  with its own message, "the body is at most 384 KiB".
- **The catalogue's reads** (`GET …/catalogue`, `GET …/catalogue/{source}/extensions/{ext}`) gain
  `summary`, `icon` (`{sha256, width, height}`) and, on a distributor's publisher's entries,
  `distributor` with the distributor's name, as the publishers' answers carry it.
- **Signing.** The `PUT` is signed as spec 0023 says: the statement binds the method, the path with
  the source, and the SHA-256 of the JSON body, as for a promotion. The format does not change.
  The icon travels in this request, not in the publication: the publication's body stays the file
  alone, on both publication routes.

#### Who may write and read

The three routes (`PUT …/meta`, `GET …/meta`, `GET …/icon`) use an access rule of their own: **any
verified caller of the tenant**. That is a principal's token, a server token, a distribution admin,
or a signed request from **any publisher of the tenant**, a tenant's own as well as a distributor's.
A signed `GET` names the empty body, as `whoami` does. The service decides, and every refusal is
`404`. Spec 0037's signed branch admits only distributors' publishers on its routes, so this rule is
separate from it.

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

**Rate.** One tenant's metadata writes are bounded by the API's per-client rate, by a rate per
caller and tenant on each replica (one a second, bursts of ten, spent once a write passed its
checks), and by its byte caps.

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
- The bytes read are hashed and must equal the row's `icon_sha256` (256 KiB at most, cheap). An
  object missing or not matching is `404`, and logged as an error.

#### CLI

- `kista admin catalogue meta <t> <source> <name> [-summary <text>] [-icon <file.png>]
  [-clear-summary] [-clear-icon] [-type t]`, a server administrator's (the operator's CLI on the
  database, as `catalogue withdraw`). It reads the current metadata and writes both fields: a field
  not named keeps its value, the icon by its hash. Without flags it prints the metadata.
- `catalogue show` prints `summary:` and `icon: <sha256> <w>x<h>`.
- `kista_publish.py` in `hugr-lab/extension-ci-tools` gains `--summary` and `--icon` (Follow-ups).
  Like the CLI, it reads the current metadata first (`GET …/meta`, signed) and sends the merged
  result, the icon kept by `{"sha256"}`, so `--summary` alone keeps an icon set in the console. On
  `409` it reads and merges again, once. On `catalogue-full` it stops with the error (a non-zero
  exit). Given neither flag, it sends nothing.

#### Events

`catalogue.meta`, the tenant's event, written in the same transaction:

- `source` (its label), `name`, the package type when not the default;
- `summary` (the new text, `""` when cleared), `icon` and `icon_before` (hashes, `""` for none);
- the actor is the writer. A distribution admin shows under spec 0030's generic `distribution-admin`
  label, unless unmasked;
- for a signed write, `key`: the signing key's fingerprint, so a newly added key is visible.

The icon's bytes are never in an event. `GET /api/v1/event-kinds` lists the kind.

#### Data model (migration 0044)

Additive, in the three dialects, with no `min_reader` bump: an older replica ignores the table.

```text
entry_meta   tenant_id (fk tenants), package_type, source, name,
             summary (160 characters, NULL; nvarchar on SQL Server), icon_sha256 (char 64, NULL),
             icon_width, icon_height, icon_bytes (int, NULL), set_by (varchar 400), set_at,
             signed_at (NULL), signed_body (char 64, NULL);
             pk (tenant_id, package_type, source, name); index (icon_sha256)
icon_claims, icon_tombstones   as spec 0016's blob_claims and blob_tombstones, keyed by
             (domain, sha256)
```

#### The console

- **Catalogue**: a 24-pixel icon before the name, and the summary under it in muted text.
- **An entry's page**: a 64-pixel icon beside the title, the summary under it, and an **Edit…**
  dialog (summary, icon file, clear). Changing only the summary sends the icon back by its hash.
  The dialog is hidden for entries the catalogue answers with a `distributor`. The API decides
  anyway.
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
  with that name. So a removed publisher's entries keep counting. The mark is read when a week is
  computed (below). A used publisher's distributor is never renamed (`409`, type `conflict`): used
  as `PublisherUsed` says (builds, releases, slots, withdrawals, rules, follows, a hold), or with any
  download count or installers.
- **Grouping is fixed**: one row per (week, extension).
- **Weeks** are complete ISO weeks in UTC, Monday to Sunday, named by their Monday (`2026-10-19`).
  Only weeks that ended at least two days ago are answered, so the counts are flushed and the
  daily installers pass is done. `from` and `to` are days, widened to whole weeks. A `to` beyond the
  last complete week is clamped to it. The default is the 12 weeks up to it. At most 53 weeks. The
  answer's `from` and `to` are the first and the last week's Mondays.
- **Rate**: the API's per-client rate applies, as for every route.

#### Weekly snapshots

The owner's decision after review: an answer reads stored snapshots only.

- The daily installers pass, once its days are counted, computes each complete week without a
  snapshot: for every distributor (the distinct names of marked publishers, live and removed) and
  every package type, the week's rows per extension, with the public tenants and the minimum of
  that moment. It stores them in `distributor_weeks`, and the week in `distributor_weeks_done`, in
  one transaction under the week's lock.
- A week is computed once and never again. A later re-mark, a tenant's deletion, a late flush, the
  retention's prune or a changed setting leaves an answered week as it was.
- The first pass computes every complete week the statistics' retention still holds whole, at most
  106 weeks back (a one-time backfill). Snapshots older than 106 weeks are deleted.

#### The figures

- **`downloads`**: the week's downloads as spec 0010 counts them (not deduplicated; a range from
  byte 0 counts).
- **`public`**: the part from public tenants.
- **`installer_days`**: daily distinct installers (spec 0010), summed over the days and the tenants.
  A principal installing on two days, or in two tenants, counts twice. Weekly distinct installers
  cannot be computed: install events may be kept for only two days (`events.retention`).
- **`withheld`**: true when the downloads' non-public part was left out (below): `downloads` then
  equals `public`, and `installer_days` is omitted. This is how Enterest's spec 0010 reads it.

No tenant is named, nor any channel, nor any per-tenant figure.

#### Public tenants and the minimum

- **Public tenants** are listed by name in `statistics.public_tenants`, for example Enterest's
  tenant that serves free extensions to anyone. All their downloads count, with a token or
  without. They are outside the minimum and do not count toward it.
- **Every other tenant** is under the minimum, whether its downloads carry a token or not.
- **Downloads.** A row's non-public downloads count only when at least
  `statistics.distributor_min_tenants` distinct non-public tenants contributed downloads to it
  (default 3, at least 2). When at least one but fewer did, that part is left out: `downloads`
  equals `public`, `installer_days` is omitted, and the row is `withheld`.
- **Installer-days** take the minimum separately: their non-public part counts only when at least
  the minimum of distinct non-public tenants have installers in the row. When at least one but fewer
  do, `installer_days` is omitted, even when the downloads are answered (the row is not `withheld`
  then). It is never answered in part: when answered, it holds the public tenants' installer-days
  and the non-public part.
- **Fail closed.** An empty list exempts no one. Names are matched against tenants when a week is
  computed, so a public tenant created after the server started is exempt from then on. A listed
  name that matches no tenant exempts nothing, and is logged at start. The minimum cannot be set
  below 2.
- **Which tenants count.** A suspended tenant counts: its downloads happened, and it still exists.
  A tenant whose deletion has started does not (spec 0019: its statistics go with it).

#### The query

- The daily pass runs it per week, distributor and package type.
- First, the distributor's sources, from `publishers` and `removed_distributor_publishers`, sorted
  and taken in chunks of at most 500 sources (a publisher's id is the server's, so a source is in
  one tenant only). With a week's bound per week and three more, a query stays under SQL Server's
  2,100 parameters.
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
distributor_weeks     distributor, package_type, week (its Monday), name, downloads,
                        public_downloads, installer_days (NULL: left out), withheld;
                        pk (distributor, package_type, week, name)
distributor_weeks_done   week (pk), min_tenants, done_at
```

The two snapshot tables are the server's (`store.GlobalTables`): sums over tenants, naming none.

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
  header before decoding, and decodes are limited to 4 at a time: about 4 MiB each at a decode's
  peak (an interlaced 16-bit 512x512 image). The summary
  is at most 160 characters, with no format or line-separator characters, no invisible fillers and
  few combining marks.
- **Who writes.** A publisher writes only its own entries, and only by a signed request. Tenant
  administrators write only the tenant's own sources. A distributor's entries belong to its
  publisher, the distribution admins and the server administrators. A tenant cannot restyle a
  distributor's product, and a distributor cannot restyle a tenant's entries.
- **Replay.** Within spec 0023's five minutes, a captured request can be sent again. The same
  request again changes nothing. An older one, or another body with the same `signed-at`, is
  `409 stale`, and a token's write that changes something moves the mark to the later of the
  server's now and the mark. A request signed ahead of the server's clock (within the five
  minutes) and never delivered can still land within its window, over a token's write made before
  its `signed-at`.
- **A stolen distribution admin's token**, or a publisher key it adds, can rewrite the icon and the
  summary of every distributor's entry in every tenant at once. Metadata has no hold, so this
  reaches every console immediately, and a summary can carry a phishing URL (as text, never a
  link). The byte caps bound the icons, not the reach. What limits it: every write is a
  `catalogue.meta` event in the tenant, naming the actor kind and the signing key; Enterest
  alerts on `catalogue.meta` written by a distribution admin or by a newly added key; the console
  never makes the summary a link.
- **Revocation.** The write checks the publisher and its key under the lock their removal takes
  (`PublicationLive`).
- **Existence.** Every refusal on the metadata and icon routes is `404`, the same as for an entry
  that does not exist. Install and anonymous callers learn nothing new: the DuckDB routes and the
  index do not change, and there is no public JSON.
- **Integrity at read.** The icon route hashes the bytes it serves against the row.
- **Tenant isolation.** Icons live in the tenant's own storage domain and count against its caps.
  Tenants sharing a domain share an object only when both were sent the same bytes. Every read goes
  through the reader's own row, so a hash proves nothing.
- **Aggregated statistics** read across tenants by design, for the distributor's own entries only
  (the owner's decision). What protects the tenants:
  - only the distributor's sources count: a tenant's own build of the same name, or an upstream's,
    never does;
  - no answer names a tenant or a channel, and every row is a sum;
  - grouping is fixed, and each week is a snapshot computed once: asking before and after a
    publisher's re-mark, a tenant's deletion, a late flush, the retention's prune or a changed
    setting answers the same week the same, so no two answers differ by one tenant's figures; and a
    used publisher's distributor is never renamed;
  - a non-public tenant's downloads appear only in a row with at least the minimum of non-public
    tenants (default 3), and installer-days the same.

  The residual risks:
  - A public tenant's figures are answered whatever their size. That is the point of listing it.
    Only the server's operator lists a tenant, by its configuration, and an empty list exempts no
    one.
  - The minimum protects against accidental disclosure, not against the server's operator (who
    reads the counts anyway), nor against a distributor that controls tenants of its own, nor
    against one organisation's several tenants: such tenants can make up the minimum, so a row may
    reflect one organisation alone. Documented, not prevented.
  - A `withheld` row is a yes/no signal: between one tenant and the minimum downloaded the
    extension that week (where one organisation alone chose it, this tells so), not which or how
    many times.
- **Retention.** Counts follow `statistics.retention` (spec 0010). Nothing new is kept about a
  principal.

## Testing

- **Store suite, three dialects**:
  - metadata write and replace, `""` and `null` clearing, the same write changing nothing, the icon
    kept by its hash and `409` when it changed;
  - `stale`: an older `signed-at`, an equal one with another body, the same request again
    accepted, a no-op advancing the mark, a token write moving it to now;
  - `icon_bytes` in `SourceUsage`, `catalogue-full` for a publisher and a distribution admin, an
    administrator not bounded;
  - the reaper (`ReapTables`), and a re-publication after a purge finding its metadata;
  - migration 0044 on a level-43 database; migration 0045 on a level-44 database: counts and
    installers kept with source `''`, and two sources of one (channel, name, version, platform)
    both stored;
  - the distributor query: a removed distributor's publisher, a tenant's own `h3` beside the
    distributor's, public tenants (with and without tokens) never withheld, the minimum on
    downloads and on installers separately, a deleting tenant left out and a suspended one counted,
    weeks across a year's end, the clamp to complete weeks, the type filter, chunking (a small
    chunk size in the test, a tenant holding two of the distributor's publishers);
  - the snapshots: stored once with the week's mark, read back, pruned; computed once by the daily
    pass (the first pass's backfill within the retention), a later re-mark, tenant deletion, late
    flush, changed minimum or retention prune changing no answered week;
  - a used publisher's distributor refused a rename.
- **Icon checks** (fixtures): a valid PNG accepted, with and without each optional chunk
  (`sRGB`, `gAMA`, `cHRM`, `pHYs`, `bKGD`); an optional chunk of the wrong length, out of order or
  after `IDAT`; each refused chunk (`iCCP`, `tEXt`, `zTXt`, `iTXt`, `eXIf`, `acTL`, `fcTL`,
  `fdAT`, `sPLT`, an unknown one), a repeated chunk, bytes
  after `IEND` (an HTML tail included), SVG, JPEG, GIF, WebP, a non-square image, under 64 and over
  512 pixels, over 256 KiB, a wrong `content_type`, truncated and corrupt files, a header claiming
  100000x100000 (refused before decoding), an `IDAT` whose image does not match the header,
  `IDAT` chunks split by another chunk, and `IDAT(image) IDAT(<html>) IEND` (zlib data past the
  stream's end) refused.
- **Summary checks**: NFC, 160 code points, each refused category, each invisible filler (U+115F,
  U+1160, U+3164, U+FFA0, U+2800), and the fifth combining mark after a base character refused.
- **Icon collection**: an unreferenced icon marked, then deleted after the grace; a claimed one
  kept; a write waiting for a `deleting` tombstone; a delete of unknown outcome finished by the
  resolver; a write racing the collector ending with its object present; a corrupted object refused
  at read.
- **API**:
  - each check of the three, and each refusal `404`; `publisher:<name>` and `publisher-id:<id>` for
    a signed request; a tenant's own publisher and a distributor's both admitted by the route's
    rule; a signed `GET` with the empty body; a tenant administrator refused a distributor's
    entry;
  - replace semantics, the icon kept by hash, the `400`s, the `413` with its message, `409 stale`
    and `conflict`, `429 catalogue-full`, `signed_at` in the answer;
  - `set_by` shown only to administrators and distribution admins;
  - the icon route's headers, authorization before the blob read;
  - the catalogue reads with `summary`, `icon` and `distributor`;
  - the event and its registry, the distribution admin's label;
  - a revoked key's write refused;
  - the statistics route: who may call it, `?distributor=`, `?type=`, the figures, `withheld`, and
    `installer_days` omitted whenever its non-public part is left out;
  - the tenant route's `source` dimension.
- **Serving**: a download records its release's source; a promoted release keeps the original.
  The daily pass attributes installers by the event's release.
- **CLI**: `catalogue meta` with each flag, keeping the field not named; `catalogue show`.
- **Console, unit**: the Edit dialog hidden for an entry with a `distributor`, a summary-only edit
  sending the icon's hash, the Blob type fixed, the summary rendered as text.
- **Console, e2e** (`web/console/e2e/catalogue.spec.ts`): the fixture's distributor's publisher sets
  an icon and a summary by a signed request; the catalogue and the entry page show them; a tenant
  administrator edits the tenant's own entry and gets no dialog for the distributor's.
- **DuckDB e2e** (`e2e/distributors_test.go`): a distributor's publisher in four tenants publishes
  `kista_demo` and sets its metadata. One tenant is listed as public. DuckDB at the pin installs
  from each with a token of the tenant's, plus a tenant's own build of the same name. The test sets
  the store's clock past the week, flushes the counts and runs the daily pass. The route answers
  one row without the tenant's own build, with exact installer-days. A pass with the minimum at 4
  leaves the snapshot; computed again at 4, the row is `withheld` and `downloads` equals `public`.
  The icon route serves the bytes with the headers above.

## Delivery

The order across specs: spec 0037 PR 1, then this spec's phase 1 (migration 0044), then its
phase 2 (migration 0045, `min_reader 45`), then spec 0037 PR 2 (migration 0046).

- **Phase 1: entry metadata.** Migration 0044, the PNG checks (`internal/icon`), the blob objects
  and their collection step, the store, the service's checks, the access rule, `body()`'s limit
  parameter, the routes, the CLI, the event, the console, the website. About 1,600 lines with tests.
- **Phase 2: distributor statistics.** Migration 0045, the source in the download key and the
  installers pass, the aggregate query, the weekly snapshots, the route, the two configuration
  keys, the rename refusal, the tenant route's `source` dimension, the website. About 800 lines
  with tests.

### As built (phase 1)

Delivered on the branch `0036-entry-meta-1`, on spec 0037's PR 1, with migration 0044; the review's
fixes followed in the same branch.

- **Spec addition.** The summary also refuses the invisible fillers U+115F, U+1160, U+3164, U+FFA0
  and U+2800 (The summary, above), as Enterest's spec 0010 does.
- **Migration 0044.** The hash columns are `varchar(64)` (`nvarchar(64)` BIN2 on SQL Server), as
  every hash column of the schema, not `char(64)`. SQL Server's `summary` is `nvarchar(320)`:
  `nvarchar` counts UTF-16 units, and 160 code points beyond the Basic Multilingual Plane take 320.
- **The icon checks** (`internal/icon`). Each chunk's CRC is checked too (a corrupt file is refused
  in step 1). The zlib end check runs after the header's dimensions are checked, not inside step 1,
  and inflates at most 4 MiB, so a bomb is never inflated past the bound. There is no separate
  "IDAT chunks consecutive" check: every allowed chunk but `IDAT` and `IEND` must come before the
  first `IDAT`, so whatever comes between two `IDAT`s is refused by that rule ("the chunk … comes
  after the image data"). The decoded image's size check is defence only (Go's decoder sizes the
  image by the header, checked before). A refusal names the rule; a refused chunk is named with how
  to strip it, an unknown one is not (its type is the sender's bytes). A decode holds about 4 MiB at
  its peak (an interlaced 16-bit 512x512 image), not 2 MiB.
- **The summary** is stored as checked: trimmed and in NFC. `CheckSummary` is in
  `internal/release`. The console renders it in `<bdi>`.
- **Spec 0016's protocol** is one implementation over object kinds (`store.Objects`: streams and
  icons, each with its tables, column, lock and referencing query); callers use `Store.Streams()`
  and `Store.Icons()`, and the collector's listing and tombstone steps take the kind. A tombstone's
  hash is `Tombstone.Hash`; `ErrStreamBusy` and `ErrClaimLost` keep their names for both kinds.
  `Tx.CommitIcon` checks and removes a write's claim in the row's transaction. The collector's
  `Marked` counts both kinds; `Result.Icons`, `storage.gc`'s `icons` and `blob gc`'s `ICONS` count
  the icons deleted. An icon's write waits on the collector at most 10 seconds (`iconClaimWait`),
  then answers busy (`503`, `Retry-After`); the wait is a package setting, changed in tests only.
- **The write.**
  - The API authorizes the caller (the service's own checks, `MayWriteEntryMeta`) before it reads
    the body. It rates a caller's metadata writes per tenant, on each replica, at one a second in
    bursts of ten (`429` `rate-limited`), beside the per-client rate; a write spends it through
    `MetaWrite.Admit`, once its body is the signed one and it passed its checks before any upload,
    so a captured request replayed (`stale`) or a refused write spends none.
  - Before anything is uploaded the write is checked against the entry (stale, a kept icon
    changed), a publisher's key against the store, and the byte cap. An icon the entry has already,
    sent as bytes, skips the cap and is not uploaded again while its object is there and matches;
    a missing or changed object is uploaded again (the write repairs it, so a row never stands
    without its object). Such a kept icon is the server's, not the client's: if the entry's icon
    changed before the transaction, the write uploads the bytes it was sent rather than answering
    `409 conflict`. The cap counts the growth only: a write whose icon grows the source's
    bytes past `catalogue.max_bytes` is refused, a smaller icon or the same one passes even over the
    cap. The cap is read outside any lock, as `room()`'s.
  - The row's transaction, under the tenant's auth lock, checks again: the tenant active (but for a
    server administrator), a publisher and its key (`PublicationLive`), a distributor's publisher's
    name (`distributorMayNow`), a tenant administrator never on a distributor's entry; then stale
    and a kept icon changed. A distribution admin is not held to `distributorMayNow`: it writes the
    metadata of a name reserved since for another holder, as it withdraws its versions. A tenant
    administrator's grants and a distribution admin's `distribute` are not checked again in the
    transaction (a grant revoked meanwhile lets one write in flight through, as for every other
    write of theirs); the cap is not either, a race bounded by the writes in flight.
  - A token's write that changes something moves `signed_at` to the later of the server's clock and
    the stored mark, to the microsecond, and clears `signed_body`; a token's write that changes
    nothing writes nothing. A signed write always advances the mark, no-ops included. A request
    signed ahead of the clock and never delivered can still land within its window (Security).
  - A kept icon's `sha256` must be 64 lowercase hex digits, and an icon both new and kept is `400`.
    A key revoked or a publisher removed in flight is `401`, as a publication's.
  - A stale write, and one that fails after its icon was uploaded (the object left to the
    collector), are `catalogue.meta` events with the outcome `refused` or `failed` and a `reason`
    (the asynchronous writer adds `count`); a write that is both is recorded once, as failed. A
    conflict, an invalid write and a full catalogue are logged.
- **Reads.** "Catalogue readers" are spec 0033's: the tenant-wide administrators, the distribution
  admins (a distributor's entries) and a distributor's publisher (its own, signed), so the reads
  take the same checks as the writes, a tenant's administrators also reading a distributor's
  entries. A distributor's publisher reads no metadata of a name reserved since for another holder:
  neither the entry's read nor the catalogue's list answers it (the list reads the reservations of
  its names in one query, `Store.ReservedAgainst`).
- **The answers** carry `source`, `name`, `distributor` and `package_type` beside the spec's fields;
  `set_by` is shown as other answers show an actor (a server's or a distribution admin's actor by
  its kind to a caller who is not a server administrator). `Allow` now lists `PUT`.
- **CLI.** `catalogue show` prints `summary:` and `icon:` after its table, so the table's header
  stays its first line; `catalogue meta` also prints who set it; `-icon ""` is a usage error.
- **Console.** Edit… is offered to tenant-wide administrators (a server administrator's console
  included) on the tenant's own, live sources: never a distributor's entry nor a removed source's.
  kista's own console CSP gains `blob:` in `img-src` (the spec named Enterest's). The e2e fixture
  writes the icons (`fixture -icon`), and `run.sh`'s signed-request helper takes the method.
- **Tests.** The DuckDB e2e case (`e2e/distributors_test.go`) is phase 2's, where its statistics
  are: phase 1's icon route and headers are covered by the API test and the console e2e. A test
  hook between a write's checks (and upload) and its transaction drives the re-checks (a name
  reserved, a key revoked, the tenant suspended meanwhile) and a lost claim's retry; a counting store
  shows a refused read reaches no object and a refused or unchanged write uploads none (at the
  service, where the order is decided).
- **Not here.** `kista_publish.py --summary --icon` is in `hugr-lab/extension-ci-tools`
  (Follow-ups).

### As built (phase 2)

Delivered on the branch `0036-entry-meta-2`, on phase 1 (merged to main, #144), with migration
0045. After review, the owner's decisions (weekly snapshots, no rename of a used publisher's
distributor, the residual risks documented, `withheld` and `installer_days` as Enterest's spec
0010 reads them, Mondays in the answer) are in the sections above.

- **Migration 0045** (`0045_download_sources`) is as the spec says. PostgreSQL's and SQL Server's
  new primary keys are named (`pk_download_counts`, `pk_download_installers`); SQLite's rebuild
  makes the old `(tenant_id, day)` indexes again beside the new `(source, day)` ones. SQL Server's
  key is 805 bytes, under the 900 a clustered key takes. The snapshot's column is `public_downloads`
  (`PUBLIC` is reserved in SQL Server) and its extension column `name`, as every table's.
- **The install event's `source`** is the source as builds store it (`upstream:<id>`,
  `publisher:<id>`, `tenant`), not a label, so the daily pass attributes it exactly; the event
  catalogue lists the field. The pass buffers the events without it by release id and looks their
  releases up in chunks of 500 (`Store.ReleaseSources`). A promotion keeps the source: release's
  `TestSources`.
- **`ReleaseStats`** orders its rows by release in SQL: the grouping no longer came out in that
  order once the primary key held the source (tested with rows of two sources).
- **The store** answers each tenant's part per (week, extension) (`Store.DistributorParts`); the
  minimum is applied in Go (`stats.Distributor`), the weeks computed there too
  (`stats.DistributorWeeks`, `stats.LastCompleteWeek`; a refusal is a `stats.WeeksError`). The `CASE`
  maps a day to the week's index by `day <= <Sunday>` branches in a derived table, so every dialect
  groups by it. Public tenants are matched by name when a week is computed
  (`Store.TenantIDsByName`), a tenant being deleted matching none. The chunk sizes are constants
  (`DistributorChunk`, `ReleaseSourcesChunk`), tested to stay under 2,100 parameters.
- **Snapshots.** `stats.Daily` computes them after counting the days and before pruning the
  counts, re-taking its lease after each week; a week's rows and its mark are one transaction, and a
  week marked already is left (`PutDistributorWeek`). The mark is per week, for every distributor
  and type at once: a distributor marked after a week was computed has no rows for it, and could
  have none (only fresh publishers are marked, and a used one is never renamed).
- **The route.** `distributor` must be given once, at most 128 bytes. A `from` after `to`, or after
  the last complete week, is `400` (rather than an empty answer), as are a day that is not one and
  more than 53 weeks. `min_tenants` is the configured minimum (`api.Options.DistributorMinTenants`,
  below 2 taken as 3); each week was computed with the minimum of its time.
- **The rename refusal** is `409` (`tenants.Refusal`), its reason without counts.
- **The tenant route's `source`** names a removed distributor's publisher `publisher-id:<id>`, as
  the catalogue does, and another removed publisher `publisher:(removed)`. Counts kept per source
  reach the route's 100,000 stored rows sooner (the website's upgrading notes say so).
- **Configuration.** `statistics.public_tenants` must hold tenant names (the configuration is
  refused otherwise); `statistics.distributor_min_tenants` `0` means 3, 1 or less is refused. The
  app binds the settings once (`newDaily`).
- **Tests.** The minimum, public tenants and installer-days are tested on `stats.Distributor`
  (pure) and through the API; the store suite covers the query (removed publishers, a tenant with
  two of the distributor's publishers, a tenant's own build and another distributor's left out, a
  deleting tenant left out and a suspended one counted, weeks across a year's end, the type filter,
  chunks of 1 and 3) and the snapshot tables; `stats` covers the snapshots' life (computed once,
  backfill, re-mark, deletion, late flush, prune). The DuckDB e2e case calls the route over https
  with a server issuer the harness takes (`startKistaOpts`), installs with tokens for exact
  installer-days, and moves the store's clock through the harness's guarded `testClock`.

## Alternatives considered

- **The icon in the publication's request**: it changes the body spec 0023 signs and the spool
  reads, on two routes. A separate signed `PUT` leaves publication alone.
- **Metadata per version**: no one wants an icon per version, and the catalogue would have to pick
  one.
- **Icons in the database, deduplicated server-wide** (the first draft): it grows the database with
  every tenant's icons and lets a shared row answer for another tenant's bytes. In the tenant's
  storage domain, icons count against its caps and stay with its data.
- **WebP**: one more decoder and dependency, for files PNG already covers at these sizes.
- **Stripping refused chunks**: it rewrites what the publisher sent, and the stored hash would
  differ from the file's. Refusing is simpler and tells the publisher why.
- **SVG with sanitising**: sanitisers have a long history of bypasses.
- **A partial `PUT`** (a field left out keeps its value): replacing both leaves no doubt about
  what a write meant. A client that changes one field (the CLI, `kista_publish.py`, the console)
  reads first and sends the other back, the icon by `{"sha256"}`, so it never re-uploads it.
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
- **Computing the figures at query time** (the first implementation): a re-mark, a tenant's
  deletion, a late flush or the retention's prune between two answers would change a past week, so
  two answers could differ by one tenant's figures. A snapshot computed once cannot.

## Changes to earlier specs

- **0005, 0016**: a new object kind, `icons/<sha256>`, in a tenant's storage domain, with claims and
  tombstones of its own; the collector gains a step that marks and deletes unreferenced icons by
  spec 0016's protocol.
- **0007**: new routes; the catalogue's reads gain `summary`, `icon` and `distributor`.
- **0010**: the download and installer keys gain the source; install events carry `source`; the
  tenant's statistics gain the `source` dimension; the daily pass stores weekly distributor
  snapshots, which the distributor route answers; `statistics.distributor_min_tenants` and
  `statistics.public_tenants`.
- **0015**: the catalogue and entry pages show icons and summaries, with an Edit dialog.
- **0019**: the reaper deletes `entry_meta`.
- **0023**: one more route open to publishers' signed requests (`PUT` and `GET
  …/catalogue/{source}/extensions/{ext}/meta` and `GET …/icon`, own source only). Its replay list
  gains: a signed metadata write older than the entry's last, or of the same second with another
  body, is refused `409 stale`; the same one again changes nothing.
- **0030**: distribution admins write a distributor's entries' metadata and read distributor
  statistics; a used publisher's distributor is never renamed.
- **0033**: an entry gains metadata, kept through withdrawal, purges and source removal; the
  catalogue answers `distributor`; event `catalogue.meta`; icons count toward `catalogue.max_bytes`
  through `icon_bytes`.
- **0034**: metadata is per package type, like the entry.
- **0037**: this spec uses the `catalogue-full` problem type its first PR adds. Its metadata routes
  have an access rule of their own, admitting any publisher of the tenant by a signed request,
  beside 0037's signed branch, which admits only distributors' publishers.

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
