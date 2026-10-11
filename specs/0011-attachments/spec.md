# Spec 0011: Attachments and the feed

- **Status**: accepted (2026-10-11); 1a in progress
- **Date**: 2026-10-11
- **Author**: vgsml, Claude

## Summary

An **attachment** is a document about one body: an SBOM, a vulnerability report, a review, a
provenance attestation, notes. It is bound to the body hash (spec 0002), so it never applies to
another binary; it belongs to the tenant that holds the body; and it records who stands behind it
(the tenant's administrators, a publisher, or a source the tenant subscribes to). Attachments come
from the tenant itself (an administrator, or a publisher's CI next to its build) and, through a
**feed**, from another repository that offers documents about the bodies the tenant holds: an
organisation's kista subscribing to Enterest's reviews, or to the public kista it mirrors. Feed
documents are **signed statements**: whatever repository they pass through, the subscriber checks
them against the source's pinned key.

Delivery, each phase a pull request:

- **1a. Attachments in a tenant**: the model (with the statement's fields, so 1b needs no
  migration), storage and collection, the API, the CLI, events, and a Reports block on the console's
  extension and release pages.
- **1b. Signed statements and the feed**: a tenant's attestation keys, the feed a kista serves, and
  the feed client that pulls a source's statements for the bodies a tenant holds.
- Later: relaying a received feed onward, "available" entries for non-subscribers, policies that act
  on attachments (Follow-ups).

## Problem

- An organisation mirroring DuckDB's community extensions decides which its nodes may install. It
  wants what is known about each binary next to it: what it contains (an SBOM), what is known to be
  vulnerable in it, and an independent review of what it does (the capabilities it uses, its network
  and file access, its licences).
- A publisher's CI builds an extension and can produce its SBOM and a provenance attestation (GitHub
  Actions does); today there is nowhere to put them but the 4,000-byte provenance field (spec 0008).
- Enterest's business is the review. It must reach the organisations that subscribe, for the exact
  binaries they hold, wherever those were mirrored from (spec 0001: attachments flow by feed, not by
  mirror origin), without kista depending on Enterest: any repository can be a source.
- The node agent installs by version and channel; an operator deciding whether a version may be
  installed needs the gist (a review's verdict, a scan's critical findings and its age) without
  downloading documents.

## Design

### The model

```text
attachments  id (uuid), tenant_id, body_hash, kind, media_type, title, size, content_hash,
             summary (json, at most 4 KB, computed at intake),
             origin ('tenant' | 'publisher' | 'feed'), attached_by (an actor; a publisher's name;
             the feed's id), visibility ('administrators' | 'readers'), feedable (bool, 1b),
             version (visibility and feedable move it), removed_at null (a removed feed document's
             tombstone), created_at,
             -- the statement (1b fills them for feed documents; for the tenant's own, issuer is ''
             -- and issuer_id the attachment's id):
             issuer (the source's stable identity: its tenant URL), issuer_id, key (the signing
             key's fingerprint), source_origin ('tenant' | 'publisher:<name>' at the source),
             issued_at, valid_until null, supersedes null (an issuer_id), withdrawn_at null,
             statement null (the exact signed bytes), signature null
             unique (tenant_id, issuer, issuer_id); index (tenant_id, body_hash)
```

- **Bound to a body, in a tenant.** An attachment names a body hash, not a release: every release of
  that body in the tenant shows it (a promotion carries its documents along). Only a body the tenant
  holds (a Build of it exists) can be attached to. When the tenant stops holding it (its last Build
  collected, spec 0016), the collector removes its attachments too, unless the body is blocked
  (spec 0008): the report that justified a block stays with the block. Tenant deletion (spec 0001)
  removes them all. A body mirrored again later starts with none of the tenant's own documents; a
  feed sends its documents again (1b).
- **Kinds and media types** (a closed list, so summaries and the console know them):

| Kind | Media types | Summary (computed at intake) |
| --- | --- | --- |
| `sbom` | CycloneDX 1.4-1.6 JSON (`application/vnd.cyclonedx+json`), SPDX 2.3 JSON (`application/spdx+json`); CycloneDX 1.6 with cryptography assets is a CBOM | components, licences, crypto assets |
| `vulnerabilities` | CycloneDX VDR or VEX JSON, OpenVEX (`application/openvex+json`), SARIF 2.1.0 (`application/sarif+json`), osv-scanner JSON (`application/json; profile=osv-scanner`) | findings by severity, not-affected count, `scanned_at` |
| `review` | `application/vnd.kista.review.v1+json` (below) | verdict, `analysed_at`, analyser |
| `provenance` | a Sigstore bundle (`application/vnd.dev.sigstore.bundle.v0.3+json`), a DSSE envelope (`application/vnd.dsse.envelope.v1+json`) | predicate type, builder, source repository and ref, shown as claimed (unverified) |
| `notes` | `text/markdown` (UTF-8) | first line |

  A document must parse as its media type says (JSON within 32 levels and the size limit; Markdown
  valid UTF-8), and its summary is extracted then, once; the summary is what lists, the index and the
  console show. A summary that cannot be extracted refuses the document (`400`).
- **The review format** is kista's, open and versioned, so a review's producer (Enterest, or anyone)
  and its readers meet on a published schema:
  `{schema: "kista.review.v1", verdict: "approved"|"conditional"|"rejected", analysed_at, analyser,
  capabilities: [...], network: [...], files: [...], processes: [...], licences: [...],
  findings: [{severity, title, detail}], text: <Markdown>}`, every string bounded. kista renders only
  this schema.
- **A provenance document's subject** is the file CI built (the whole file, its last 256 bytes
  included). kista binds to the body hash (the file without them) and re-signs, so the attestation's
  subject digest is not the body hash; a later spec verifies attestations against the uploaded file.
- **Content** is stored in the tenant's storage domain as `attachments/<attachment id>` (spec 0005's
  key grammar and list prefixes gain it), one object per attachment, no deduplication: the object is
  put, then the record inserted with its content hash and size; a read checks the content against
  the hash before returning anything (the object store is not trusted). The collector (spec 0016)
  deletes `attachments/` objects without a record that are older than its grace, like `tmp/`
  leftovers: no claims or tombstones are needed, since no two records share an object.
- **Attaching** follows spec 0016's intake rule: the Build's `used_at` is refreshed in the same
  transaction, so the collector cannot remove the body between the check and the insert.
- **Limits**: a document is at most 4 MiB (`attachments.max_size`, a deployment setting up to
  16 MiB); at most 20 documents per (body, kind, origin) and 100,000 per tenant; superseded versions
  older than a year are removed by the collector. Reads hold at most `attachments.max_reads` (8)
  documents in memory at once, the rest wait.
- **Visibility**: `administrators` (the default for every document) or `readers`. Only a tenant
  administrator makes a document `readers`, an explicit act per document: it is then shown, through
  the index, to whoever may see a release of the body in the channel asked about. The console says,
  before the change, which channels show the body publicly. Who attached a document is never shown to
  readers, and neither is what it supersedes.
- **Versions**: a document may name an earlier one of the **same body, kind, origin and issuer** in
  `supersedes`, forming a chain. Readers see a document only if it is the head of its chain, not
  withdrawn, and `readers`; otherwise they see nothing of that chain (never an older version in its
  place). Administrators see the whole chain. A withdrawn document (1b: a source withdraws it) is
  kept, marked, and never the head. A document past its `valid_until` is shown as expired. The
  limits below count heads only.
- **Who attaches and removes**:
  - a tenant administrator, or an administrator of the extension (spec 0006, an extension group's
    included, spec 0017) whose grant has no channel: any kind, origin `tenant`;
  - a principal or publisher holding `publish` on the extension with no channel scope (spec 0008):
    `sbom` and `provenance` only, origin `publisher`, attached by its name, and only for a body
    released in the tenant by a publication whose provenance names this publisher or principal. A
    publisher's document never supersedes a tenant's, nor the reverse;
  - a feed's documents are written by the feed client only (1b);
  - removing a document: a tenant administrator, or the extension's administrator with no channel
    scope. Removing a feed document leaves a local tombstone, so the feed does not bring it back.

A channel-scoped grant does not attach or remove: an attachment is the tenant's, shown in every
channel, so a right on one channel must not change what another shows.

### API

Readers see `readers` documents through the index, in their view; administrators manage all of
them under the extension:

| Route | Who |
| --- | --- |
| `GET …/channels/{c}/extensions/{ext}/attachments[?body_hash=&kind=&cursor=&limit=]` | the `readers` documents of the bodies of `{ext}` in the caller's view of `{c}`, latest per (body, kind, origin, issuer) |
| `GET …/channels/{c}/extensions/{ext}/attachments/{id}` | one of those, as a download |
| `GET …/extensions/{ext}/attachments[?body_hash=&kind=&origin=&cursor=&limit=]` | every document of `{ext}`'s bodies (management: fresh tokens): its administrators, and the tenant's auditors |
| `GET …/extensions/{ext}/attachments/{id}[/content]` | the record (ETag `"v<version>"`), its content as a download |
| `POST …/extensions/{ext}/attachments?body_hash=&kind=&supersedes=` (the document as the raw body, `Content-Type` its media type, the title in `Kista-Title`, kept out of access logs) | as above (administrators; publishers' credentials too, as publication takes them, spec 0008) |
| `POST …/extensions/{ext}/attachments/{id}/readers`, `/administrators`, `/feedable`, `/unfeedable` (`If-Match`) | tenant administrators |
| `DELETE …/extensions/{ext}/attachments/{id}` | as above |
| `GET, DELETE …/blocks/{hash}/attachments[/{id}]` | tenant administrators: the documents of a blocked body whose Builds are gone |

- `{ext}` must name a Build of the body (a body under two names is two Builds: its documents are
  each name's).
- Downloads: `Content-Type` the stored canonical media type, `Content-Disposition: attachment;
  filename="<id>.<ext>"` (never the title), `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: sandbox`, `Cache-Control: private, no-store` (a document's visibility can
  change, so nothing is cached).
- The index's extension and version rows (`extensions/{name}`, `…/versions/{v}`) gain
  `attachments`: per kind, the count and the latest summary the caller may read (a review's verdict,
  a scan's counts by severity and `scanned_at`), computed per request over the rows' body hashes.
  The paged `extensions` list does not carry them, and serving snapshots are untouched; the query
  joins the extension's Builds rather than listing body hashes.
- `title`: at most 200 bytes of printable text (no control or format characters).

### Signed statements and the feed (1b)

- **Attestation keys.** A tenant that serves a feed has attestation keys: RSA-2048, held in spec
  0004's key sources with a purpose marker of their own (`kista-purpose = attestation-signing`; the
  marker checked at open follows the key's role; a file key, which has no tags, relies on the
  fingerprint registry alone), in an `attestation_keys` table (trusted, active, retired, as channel
  keys; one active per tenant) with registry rows of their own. Like channel keys, a server
  administrator registers one by signer reference and a tenant administrator activates and retires
  it (`kista admin attestation-key add|list|activate|retire`, and the API). Their public keys are
  served at `GET /api/v1/tenants/{t}/attestation-keys` (public). Rotation is a new key that
  subscribers pin before it signs.
- **The statement** is a JSON object canonicalised by RFC 8785 (JCS), signed RSA PKCS#1 v1.5 over
  SHA-256 of `kista-statement-v1\0` followed by it, a typed input of its own (spec 0002 signs only
  body hashes), and sent as its exact bytes (base64) with the signature, so a subscriber verifies
  bytes and never re-canonicalises. Fields: `issuer` (the source tenant's URL: a stable identity
  across key rotations), `key` (the signing key's fingerprint), `issuer_id`, `seq` (per issuer,
  increasing), `body_hash`, `kind`, `media_type`, `content_hash`, `size`, `title`, `summary`,
  `issued_at`, `valid_until`, `supersedes`, `origin` (who stands behind it at the source: `tenant` or
  `publisher:<name>`), `audience` (`administrators`, or `readers` when the source lets subscribers
  show it to their readers), `withdrawn` (true for a withdrawal, which carries no content). Every
  field the subscriber stores or shows is signed; a received `summary` is still checked against its
  kind's shape and size before it is stored.
- **Issuing.** A statement is issued (numbered and signed) when a document is marked feedable, not
  when it is created, and its `seq` is taken from a per-issuer counter row locked until commit, so a
  subscriber that has seen `seq` n never misses a lower one committed later. Un-marking a document
  feedable issues its withdrawal.
- **The feed route**, on the source:

```text
POST /api/v1/tenants/{t}/feed {bodies: [{body_hash, after_seq}]}   →   {statements: [...]}
GET  /api/v1/tenants/{t}/feed/documents/{issuer_id}                →   the content
```

  - Authorised by a new verb **`feed`**, granted on the tenant or on a channel (spec 0006's verb
    list gains it; `admin` implies it; never to an `issuer:` grant or a publisher). The source answers
    only for bodies released in the channels the grant covers, and only documents its administrators
    marked **feedable** (a flag on a document, off by default: a document is offered only on
    purpose). A body it does not hold, or holds outside those channels, answers like one without
    documents.
  - A request names at most 500 bodies; an answer carries statements, never content, at most 1 MiB,
    each body's in `seq` order, with `more` when it stopped early; content is fetched by its issuer
    id from the fixed path under the configured feed URL, which applies the same feedable and
    channel checks and answers a document out of scope like a missing one.
  - Not a management route: a feed credential may be long-lived. Its events are counted, not one
    per request.
- **The feed client**, on the subscriber: `feeds` per tenant (API and CLI, like upstreams): name, the
  source's URL (`https://<host>/api/v1/tenants/<t>`, validated as spec 0009 validates prefixes), a
  credential (spec 0009 phase 3, matched by its `prefixes`, which may name feed URLs; its principal
  holds `feed` at the source), pinned attestation key fingerprints, the kinds accepted, the visibility
  of what arrives (`administrators`; `readers` only for statements whose signed `audience` is
  `readers`, and only if the feed's configuration allows it, since a paid review shown to readers of
  a public release is published to anyone), the bodies offered (by
  default those whose Builds came from upstreams, never the tenant's own publications; or named
  channels), and a schedule.
  - A run (one replica's, under a lease) sends the bodies with the last `seq` received per (feed,
    body): a body never sent goes with none, so it receives its whole history; then it fetches each
    new statement's content from the fixed path (through egress, no redirects, the credential sent
    only there), checks size and content hash against the statement and the signature against a
    pinned key, and inserts it only if its body is one it sent and still holds, under the same rule
    as an intake (spec 0016). A `supersedes` must name an earlier statement of the same issuer, body
    and kind; one the subscriber never received (not offered, refused) is accepted and kept as an
    unresolved link. A withdrawal marks its target.
  - The cursor advances past a statement that was stored or permanently refused (a bad signature, a
    limit), each refusal recorded; a transient failure (a fetch, egress) leaves it, so the next run
    tries again. A body's feed state goes with its attachments when the collector removes them, so a
    body held again receives its history again.
  - The same document reaching the tenant through two feeds is stored once (the first feed's); a
    feed's name is not reused while its documents remain.
  - A run records `feed.run` (received, superseded, withdrawn, refused with reasons) and the feed's
    status (`ok`, `unauthorized`, `failing`, the last success and error) shown on the console.
  - Removing a feed keeps its documents unless asked to remove them.
- **What a subscriber discloses**: the body hashes it offers, to each source it subscribes to, and
  only those; a multi-tenant source learns which of those hashes each subscriber holds. The default
  (mirrored bodies only) keeps the tenant's own builds out of it.
- Enterest implements the source side for its subscribers; kista's open server implements both, so
  any organisation can run a source. Spec 0001's open-core table is amended: kista has attachments,
  the feed and its client; Enterest has its analysis, its subscriptions and the review content.

### Console

- The extension and release pages show a **Reports** block: per document its kind, title, origin
  (the tenant, a publisher, or the feed and its verified issuer), date and age (a scan older than 30
  days is flagged), expiry, and its summary (a review's verdict and findings, a scan's counts by
  severity, an SBOM's components), rendered as text; Markdown with raw HTML off, links limited to
  `https` with `rel="noopener noreferrer"`, no images. A download is fetched with the token and saved
  as `application/octet-stream` with the `download` attribute, never opened.
- Administrators attach a document, mark it `readers` or feedable, and remove it. Feeds are managed
  on the Upstreams page.

### Events

`attachment.add`, `attachment.remove`, `attachment.change` (visibility, feedable), subject
`ext:<name>/attachment:<id>`, data `body_hash`, `kind`, `origin`, `size`, the change; `feed.add`,
`feed.remove`, `feed.change`, `feed.run` (`feed`, counts, `error`), subject `feed:<name>`. Spec
0010's catalogue and subject forms gain them; a source does not record one event per feed request,
only refusals (spec 0010's `authz.refused`).

### Package layout

```text
internal/store/migrations/*/0017_attachments.sql   attachments (1a); feeds, feed state (1b)
internal/attachment   kinds, media types, summaries, the review schema; the service; the feed
                      source and client (1b)
internal/blob         + attachments/<id> objects (put, read checked against the hash), the collector's
                      step for them
internal/keys         + attestation keys (1b): attestation_keys, registry rows, statements signed
internal/api          the routes; the index rows' attachments
cmd/kista             kista admin attachment add|list|show|visibility|remove; feed add|show|remove|sync
web/console           the Reports block; feeds on Upstreams
```

## Security

- **Bound to the body hash**: a document is shown only for the binary it names; a source's
  statement signs which binary it is about.
- **No existence leaks**: readers reach `readers` documents only through a release in their view;
  management routes are the extension's administrators'. A body the caller cannot see answers like a
  missing one. Making a document `readers` is an explicit act, defaulting to no one, and never shows
  who attached it.
- **Who stands behind a document is recorded and shown**: a publisher's self-assessment is labelled
  as such, and cannot supersede the tenant's or a feed's documents. Publishers attach only SBOMs and
  provenance, only with tenant- or extension-wide `publish`, only for bodies they published.
- **Untrusted content**: documents are data, never rendered as HTML; downloads carry `nosniff`, a
  sandboxing CSP and a fixed file name; summaries are computed once, at intake, under parse limits;
  the console renders text.
- **The store is not trusted**: content is checked against the record's hash on every read.
- **The feed**: statements are accepted only under a pinned key, for bodies the subscriber sent and
  holds; every displayed field is signed, so a relay or a compromised intermediary cannot retitle,
  re-date or reorder documents; content comes only from the fixed path under the configured URL,
  through egress, without redirects; the credential goes nowhere else. A source offers only
  documents marked feedable, only for bodies in the channels a subscriber's grant covers.
- **Tenant isolation**: records and objects are per tenant (no cross-tenant deduplication).

## Testing

- Store suite on three dialects: records, uniqueness, supersedes rules, tombstones, the limits,
  removal with the last Build and its exception for blocked bodies.
- Service: every kind and media type parsing and summarising (valid, malformed, too deep, too
  large), visibility per caller, publishers' rules, a body not held, a tampered object.
- Collector: objects without records collected after the grace; records of bodies no longer held.
- API: the routes and who may call them, `404` uniformity, index rows' summaries, download headers,
  `If-Match`.
- Feed (1b): two kista servers in one test (source and subscriber): pinned keys, an unsigned, a
  wrongly signed, an altered (title, supersedes) and an unrequested statement refused; a new body
  receiving its history; withdrawals; feedable and channel scope at the source; the fixed content
  path; egress.
- Console: the Reports block; e2e attaching an SBOM and reading its summary.

## Alternatives considered

- **Attachments on a release, not a body**: a promotion would lose them, and one binary in two
  channels would carry two stories.
- **Content-addressed objects shared across tenants**: they need reference counts and spec 0016's
  claims; per-attachment objects are collected like `tmp/` leftovers.
- **Ed25519 attestation keys**: not offered by every key source (Azure Key Vault has none); RSA-2048
  is what spec 0004's backends hold.
- **One cursor per feed**: a body acquired later would never receive older documents.
- **URLs for content in feed answers**: a source could point the client, and its credential,
  anywhere.
- **Rendering documents as HTML**: a document is untrusted; the console shows summaries and text.
- **A push feed**: subscribers are often behind firewalls; a pull goes through their egress.

## Follow-ups

- Relaying a received feed onward (an organisation's central kista feeding its regional mirrors),
  with the origin's statements intact and the origin's keys pinned downstream.
- "Available" entries: a source telling a non-subscriber that a review exists (kind, date, where).
- Policies acting on attachments (refuse a release whose scan has a critical finding, or whose
  review is rejected): the node agent's and the cluster profile's specs, reading the index's
  summaries.
- Verifying provenance attestations against the uploaded file (GitHub's attestations for trusted
  publishing).
- Air-gapped bundles (spec 0014) carrying attachments with their statements.
- Spec 0018's verify covering attachment objects.
